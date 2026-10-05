package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"crossing-guard/harvest"
	"crossing-guard/store"
)

const (
	lifecycleQueueCapacity = 64
	lifecycleFlushDelay    = 750 * time.Millisecond
	lifecycleBackfillDelay = 750 * time.Millisecond
	lifecycleSweepInterval = 30 * time.Second
	lifecycleAliasBatch    = 256
)

type lifecycleCoordinatorStats struct {
	queued                    atomic.Int64
	coalesced                 atomic.Int64
	overflow                  atomic.Int64
	started                   atomic.Int64
	completed                 atomic.Int64
	failed                    atomic.Int64
	active                    atomic.Int64
	bytesInspected            atomic.Int64
	recordsDecoded            atomic.Int64
	aliasesRepaired           atomic.Int64
	equivalentActionsRepaired atomic.Int64
}

type delayedLifecycleWork struct {
	work store.LiveSessionRuntime
	due  time.Time
}

type lifecycleCoordinator struct {
	g       *Governor
	queue   chan store.LiveSessionRuntime
	mu      sync.Mutex
	queued  map[string]bool
	delayed map[string]delayedLifecycleWork
	stats   lifecycleCoordinatorStats
}

func newLifecycleCoordinator(g *Governor) *lifecycleCoordinator {
	return &lifecycleCoordinator{g: g, queue: make(chan store.LiveSessionRuntime, lifecycleQueueCapacity),
		queued: map[string]bool{}, delayed: map[string]delayedLifecycleWork{}}
}

func lifecycleWorkKey(work store.LiveSessionRuntime) string {
	return work.Runtime + "\x00" + work.SessionID + "\x00" + filepath.Clean(work.TranscriptPath)
}

func lifecycleSegment(path string) string {
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}

func (c *lifecycleCoordinator) enqueue(work store.LiveSessionRuntime) error {
	if work.Runtime == "" || work.SessionID == "" || work.TranscriptPath == "" {
		return nil
	}
	key := lifecycleWorkKey(work)
	c.mu.Lock()
	if c.queued[key] {
		c.stats.coalesced.Add(1)
		c.mu.Unlock()
		return nil
	}
	c.queued[key] = true
	select {
	case c.queue <- work:
		c.stats.queued.Add(1)
		c.mu.Unlock()
		return nil
	default:
		delete(c.queued, key)
		c.stats.overflow.Add(1)
		c.mu.Unlock()
	}
	// Queue pressure never loses the source identity. The periodic safety sweep
	// selects this durable rescan bit after capacity becomes available. If that
	// durable write fails, the caller receives the error and its existing delivery
	// boundary must retry instead of treating admission as successful.
	if err := c.markRescan(work); err != nil {
		return fmt.Errorf("persist lifecycle queue-overflow rescan: %w", err)
	}
	return nil
}

func (c *lifecycleCoordinator) markRescan(work store.LiveSessionRuntime) error {
	c.g.writeMu.Lock()
	err := c.g.ix.MarkTranscriptRescan(work.SessionID, work.Runtime, work.TranscriptPath,
		lifecycleSegment(work.TranscriptPath), work.WorkingDirectory, time.Now().Unix())
	c.g.writeMu.Unlock()
	return err
}

func (c *lifecycleCoordinator) hint(work store.LiveSessionRuntime) error {
	enqueueErr := c.enqueue(work)
	if work.TranscriptPath == "" {
		if enqueueErr != nil {
			c.stats.failed.Add(1)
		}
		return enqueueErr
	}
	key := lifecycleWorkKey(work)
	c.mu.Lock()
	_, exists := c.delayed[key]
	overflow := !exists && len(c.delayed) >= lifecycleQueueCapacity
	if !overflow {
		c.delayed[key] = delayedLifecycleWork{work: work, due: time.Now().Add(lifecycleFlushDelay)}
	}
	c.mu.Unlock()
	if overflow {
		c.stats.overflow.Add(1)
		if err := c.markRescan(work); err != nil {
			enqueueErr = errors.Join(enqueueErr,
				fmt.Errorf("persist lifecycle delayed-overflow rescan: %w", err))
		}
	}
	if enqueueErr != nil {
		c.stats.failed.Add(1)
	}
	return enqueueErr
}

func (c *lifecycleCoordinator) runWorker() {
	for work := range c.queue {
		key := lifecycleWorkKey(work)
		c.stats.started.Add(1)
		c.stats.active.Add(1)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err := withLifecycleSlot(ctx, c.g, func() error {
			return reconcileLifecycleSessions(ctx, c.g, []store.LiveSessionRuntime{work})
		})
		cancel()
		c.stats.active.Add(-1)
		if err != nil {
			c.stats.failed.Add(1)
		} else {
			c.stats.completed.Add(1)
		}
		c.mu.Lock()
		delete(c.queued, key)
		c.mu.Unlock()
		cursor, found, cursorErr := c.g.ix.TranscriptCursor(work.Runtime, work.TranscriptPath,
			lifecycleSegment(work.TranscriptPath))
		if cursorErr == nil && found && cursor.ContinuationNeeded {
			if cursor.ActionParserVersion == -1 {
				if err := c.delay(work, lifecycleBackfillDelay); err != nil {
					c.stats.failed.Add(1)
				}
			} else {
				if err := c.enqueue(work); err != nil {
					c.stats.failed.Add(1)
				}
			}
		}
	}
}

func (c *lifecycleCoordinator) delay(work store.LiveSessionRuntime, delay time.Duration) error {
	if work.Runtime == "" || work.SessionID == "" || work.TranscriptPath == "" {
		return nil
	}
	key := lifecycleWorkKey(work)
	c.mu.Lock()
	_, exists := c.delayed[key]
	overflow := !exists && len(c.delayed) >= lifecycleQueueCapacity
	if !overflow {
		c.delayed[key] = delayedLifecycleWork{work: work, due: time.Now().Add(delay)}
	}
	c.mu.Unlock()
	if overflow {
		c.stats.overflow.Add(1)
		if err := c.markRescan(work); err != nil {
			return fmt.Errorf("persist lifecycle delay-overflow rescan: %w", err)
		}
	}
	return nil
}

func (c *lifecycleCoordinator) admitDelayed(now time.Time) {
	ready := make([]store.LiveSessionRuntime, 0, len(c.delayed))
	c.mu.Lock()
	for key, delayed := range c.delayed {
		if !delayed.due.After(now) {
			ready = append(ready, delayed.work)
			delete(c.delayed, key)
		}
	}
	c.mu.Unlock()
	for _, work := range ready {
		if err := c.enqueue(work); err != nil {
			c.stats.failed.Add(1)
			// The item just left this bounded map, so put it back for a later
			// scheduler tick. Failure accounting happened above and this
			// in-memory retry remains bounded by the same cap.
			if retryErr := c.delay(work, lifecycleFlushDelay); retryErr != nil {
				c.stats.failed.Add(1)
			}
		}
	}
}

func (c *lifecycleCoordinator) safetySweep() {
	c.repairResultAliases()
	c.repairEquivalentActions()
	c.startActionBackfill()
	rescans, err := c.g.ix.TranscriptRescanSources(lifecycleQueueCapacity)
	if err == nil {
		for _, source := range rescans {
			if enqueueErr := c.enqueue(source); enqueueErr != nil {
				c.stats.failed.Add(1)
				continue
			}
		}
	} else {
		c.stats.failed.Add(1)
	}
	recent, err := c.g.ix.LiveSessionRuntimes(lifecycleReconcileSessionLimit)
	if err == nil {
		for _, source := range recent {
			if enqueueErr := c.enqueue(source); enqueueErr != nil {
				c.stats.failed.Add(1)
				continue
			}
		}
	} else {
		c.stats.failed.Add(1)
	}
}

func (c *lifecycleCoordinator) startActionBackfill() {
	active, found, err := c.g.ix.TranscriptActionBackfillActiveSource()
	if err != nil {
		c.stats.failed.Add(1)
		return
	}
	if found {
		if info, statErr := os.Stat(active.TranscriptPath); statErr == nil && info.Mode().IsRegular() {
			return
		} else {
			c.g.writeMu.Lock()
			parkErr := c.g.ix.BlockTranscriptActionBackfill(active.Runtime, active.TranscriptPath,
				lifecycleSegment(active.TranscriptPath), time.Now().Unix())
			c.g.writeMu.Unlock()
			issueErr := recordLifecycleIssue(c.g, active.SessionID, active.Runtime,
				active.TranscriptPath, "transcript-unavailable", statErr)
			if parkErr != nil || issueErr != nil {
				c.stats.failed.Add(1)
				return
			}
		}
	}
	for _, contract := range harvest.LifecycleActionCollectionContracts() {
		candidates, err := c.g.ix.TranscriptActionBackfillSources(contract.Runtime,
			contract.ResultSourceKind, contract.ResultNativeCallKind, contract.ParserVersion, 8)
		if err != nil {
			c.stats.failed.Add(1)
			return
		}
		for _, candidate := range candidates {
			if info, statErr := os.Stat(candidate.TranscriptPath); statErr != nil || !info.Mode().IsRegular() {
				if err := recordLifecycleIssue(c.g, candidate.SessionID, candidate.Runtime,
					candidate.TranscriptPath, "transcript-unavailable", statErr); err != nil {
					c.stats.failed.Add(1)
					return
				}
				continue
			}
			c.g.writeMu.Lock()
			claimed, claimErr := c.g.ix.BeginTranscriptActionBackfill(candidate.Runtime,
				candidate.TranscriptPath, lifecycleSegment(candidate.TranscriptPath),
				contract.ParserVersion, time.Now().Unix())
			c.g.writeMu.Unlock()
			if claimErr != nil {
				c.stats.failed.Add(1)
				return
			}
			if claimed {
				if err := c.enqueue(candidate); err != nil {
					c.stats.failed.Add(1)
					return
				}
				return
			}
		}
	}
}

func (c *lifecycleCoordinator) repairEquivalentActions() {
	candidates, err := c.g.ix.AmbiguousResultRepairCandidates(lifecycleAliasBatch)
	if err != nil {
		c.stats.failed.Add(1)
		return
	}
	for _, candidate := range candidates {
		c.g.writeMu.Lock()
		reconciliation, repairErr := c.g.ix.ReconcileResultObservation(candidate.ResultID, time.Now().Unix())
		if repairErr == nil && reconciliation.JoinClass == "exact" {
			repairErr = c.g.ix.ResolveCollectionIssue(observationIssueID("result-unjoined",
				candidate.ObservationID), time.Now().Unix())
		}
		c.g.writeMu.Unlock()
		if repairErr != nil {
			c.stats.failed.Add(1)
			continue
		}
		if reconciliation.Algorithm == "native-id-duplicate-delivery-v1" {
			c.stats.equivalentActionsRepaired.Add(1)
		}
	}
}

func (c *lifecycleCoordinator) repairResultAliases() {
	for _, contract := range harvest.LifecycleAliasRepairContracts() {
		candidates, err := c.g.ix.ResultAliasRepairCandidates(store.ResultAliasRepairContract{
			Runtime: contract.Runtime, ResultSourceKind: contract.ResultSourceKind,
			ResultNativeCallKind: contract.ResultNativeCallKind,
			AliasNativeCallKind:  contract.AliasNativeCallKind, Tool: contract.Tool}, lifecycleAliasBatch)
		if err != nil {
			c.stats.failed.Add(1)
			continue
		}
		for _, candidate := range candidates {
			c.g.writeMu.Lock()
			duplicate, repairErr := c.g.ix.AppendResultNativeCallAlias(store.ResultNativeCallAlias{
				ResultID: candidate.ResultID, NativeCallKind: contract.AliasNativeCallKind,
				NativeCallID: candidate.NativeCallID, Algorithm: contract.Algorithm})
			if repairErr == nil {
				repairErr = c.g.ix.RelateLogicalResults(candidate.ResultID, time.Now().Unix())
			}
			var reconciliation store.ResultReconciliation
			if repairErr == nil {
				reconciliation, repairErr = c.g.ix.ReconcileResultObservation(candidate.ResultID, time.Now().Unix())
			}
			if repairErr == nil && reconciliation.JoinClass == "exact" {
				repairErr = c.g.ix.ResolveCollectionIssue(observationIssueID("result-unjoined",
					candidate.ObservationID), time.Now().Unix())
			}
			c.g.writeMu.Unlock()
			if repairErr != nil {
				c.stats.failed.Add(1)
				continue
			}
			if !duplicate {
				c.stats.aliasesRepaired.Add(1)
			}
		}
	}
}

func (c *lifecycleCoordinator) runScheduler() {
	flushTicker := time.NewTicker(250 * time.Millisecond)
	sweepTicker := time.NewTicker(lifecycleSweepInterval)
	defer flushTicker.Stop()
	defer sweepTicker.Stop()
	c.safetySweep()
	// The handoff owner's sweep rides this one, not the managed host's, so an armed
	// brief is re-armed with no agent turned on (team rest-of-release plan §6.5).
	// The pass at start-up re-arms a brief handed over by a daemon that died
	// before confirming it.
	handoffOpens.sweep(true)
	for {
		select {
		case now := <-flushTicker.C:
			c.admitDelayed(now)
		case <-sweepTicker.C:
			c.safetySweep()
			handoffOpens.sweep(false)
			// Parked-run relaunches ride the same sweep cadence
			// (provider-outage plan Slice B): one bounded ladder pass —
			// retry, chain advance, or leave parked — per sweep. Natural
			// signal emission has its own configured loop (helper-session-
			// attachment plan D2/D8).
			if managedHost != nil {
				managedHost.relaunchParkedRunsOnce()
			}
			// The memory import's safety net and pending drain ride the same
			// sweep (daemon-memory-import plan D1c): the first import after
			// boot, then only what a session end left pending.
			runMemoryImportIfDue("sweep")
		}
	}
}

func requestLifecycleHint(g *Governor, work store.LiveSessionRuntime) error {
	if g == nil || g.lifecycle == nil {
		return nil
	}
	return g.lifecycle.hint(work)
}
