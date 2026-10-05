package daemon

import (
	"context"
	"log"
	"time"

	"crossing-guard/store"
)

// Understanding facts retention (understanding-facts-retention plan §4.3). The
// worker belongs to the scan coordinator and stops with it, but runs on its own
// goroutine: an attachment or pre-mutation scan is due at once, and a pass must
// never sit in front of one. It holds the store's write lock only inside one
// bounded transaction, and the store re-checks the keep rule inside the
// transaction that retires a generation, so the candidate list here is advice
// and never the decision.

// understandingRetentionSettings is one pass's reading of daemon.json.
type understandingRetentionSettings struct {
	keep       time.Duration
	grace      time.Duration
	startDelay time.Duration
	interval   time.Duration
	pause      time.Duration
	// deleteRows and deleteDescriptors bound one delete transaction.
	deleteRows        int
	deleteDescriptors int
}

// understandingRetentionSettingsNow re-reads the configuration for every pass, so
// turning retention off takes effect by the next pass without a restart. It
// fails closed: a file that did not load ("invalid") or stopped loading
// ("last-good") never prunes, because the value in force is then not what the
// owner wrote, and a delete cannot be taken back.
func understandingRetentionSettingsNow() (understandingRetentionSettings, bool) {
	config, origin := consoleConfig()
	retention := config.UnderstandingRetention
	settings := understandingRetentionSettings{
		keep:              time.Duration(retention.KeepDays) * 24 * time.Hour,
		grace:             time.Duration(retention.IntermediateGraceHours) * time.Hour,
		startDelay:        time.Duration(retention.StartDelaySeconds) * time.Second,
		interval:          time.Duration(retention.PassIntervalSeconds) * time.Second,
		pause:             time.Duration(retention.PauseMS) * time.Millisecond,
		deleteRows:        retention.DeleteRowsPerTransaction,
		deleteDescriptors: retention.DeleteDescriptorsPerTransaction,
	}
	if origin == "invalid" || origin == "last-good" {
		return settings, false
	}
	return settings, retention.Enabled && retention.validate() == nil
}

// retentionStillInForce reports whether the configuration that started the pass is
// still the one in force. The read is cached for the console's reload check, so
// asking before every write costs nothing.
func (c *understandingScanCoordinator) retentionStillInForce(pass understandingRetentionSettings) bool {
	current, enabled := c.retentionSettings()
	return enabled && current == pass
}

// runRetention waits the start delay before the first pass, so a daemon start never
// begins with a candidate query, then alternates passes and intervals until the
// coordinator stops. The start delay is its own setting: were the first wait the
// interval, a daemon restarted more often than that would never run a pass.
func (c *understandingScanCoordinator) runRetention(ctx context.Context) {
	settings, _ := c.retentionSettings()
	wait := settings.startDelay
	for {
		if !sleepContext(ctx, wait) {
			return
		}
		settings, enabled := c.retentionSettings()
		if enabled {
			c.retentionPass(ctx, settings)
		}
		wait = settings.interval
	}
}

// retentionPass first finishes deletes an earlier pass or a stopped daemon left
// behind, then lists the candidates once and retires them oldest first. Every write
// is one bounded transaction followed by the pause, and the configuration is read
// again before every write: turning retention off, changing a horizon, or a file
// that stops loading ends the pass at the next write, not hours later. A write that
// cannot take the lock is a counted skip left for the next pass; any other store
// error ends the pass.
func (c *understandingScanCoordinator) retentionPass(ctx context.Context, settings understandingRetentionSettings) {
	now := c.now()
	c.stats.retentionLastPass.Store(now.Unix())
	keepSinceSeconds := now.Add(-settings.keep).Unix()
	graceSinceNanos := now.Add(-settings.grace).UnixNano()
	leftovers, err := c.g.ix.PrunedUnderstandingGenerationsWithFacts()
	if err != nil {
		log.Printf("understanding retention: list unfinished deletes: %v", err)
		return
	}
	c.stats.pruneUnfinished.Store(int64(len(leftovers)))
	// Descriptors lose their last reference only when unit rows are deleted, so the
	// unreferenced ones are swept only by a pass that deleted some.
	deletedRows := len(leftovers) > 0
	defer func() {
		if deletedRows {
			c.deleteUnreferencedDescriptors(ctx, settings)
		}
	}()
	for _, generationID := range leftovers {
		if !c.retentionStillInForce(settings) {
			return
		}
		outcome := c.deletePrunedFacts(ctx, generationID, settings)
		if outcome == retentionDrained {
			c.stats.pruneUnfinished.Add(-1)
		}
		if outcome == retentionStop {
			return
		}
	}
	candidates, err := c.g.ix.PrunableUnderstandingGenerations(keepSinceSeconds, graceSinceNanos)
	if err != nil {
		log.Printf("understanding retention: list candidates: %v", err)
		return
	}
	remaining := int64(len(candidates))
	c.stats.pruneCandidates.Store(remaining)
	for _, generationID := range candidates {
		if ctx.Err() != nil || !c.retentionStillInForce(settings) {
			return
		}
		pruned, err := c.g.ix.PruneUnderstandingFacts(generationID, keepSinceSeconds, graceSinceNanos)
		switch {
		case store.IsBusyError(err):
			c.stats.pruneSkippedBusy.Add(1)
			continue
		case err != nil:
			log.Printf("understanding retention: prune generation %d: %v", generationID, err)
			return
		}
		remaining--
		c.stats.pruneCandidates.Store(remaining)
		if !pruned {
			continue
		}
		c.stats.factsPruned.Add(1)
		c.stats.pruneUnfinished.Add(1)
		deletedRows = true
		if !sleepContext(ctx, settings.pause) {
			return
		}
		outcome := c.deletePrunedFacts(ctx, generationID, settings)
		if outcome == retentionDrained {
			c.stats.pruneUnfinished.Add(-1)
		}
		if outcome == retentionStop {
			return
		}
	}
}

// retentionDeleteOutcome is how draining one pruned generation's rows ended.
type retentionDeleteOutcome int

const (
	// retentionDrained: the generation holds no rows any more.
	retentionDrained retentionDeleteOutcome = iota
	// retentionSkipped: the store was busy; rows remain for the next pass, and this
	// pass moves on.
	retentionSkipped
	// retentionStop: the pass must end (stopped, configuration changed, or a store error).
	retentionStop
)

// deletePrunedFacts drains one pruned generation's rows, a bounded transaction and a
// pause at a time.
func (c *understandingScanCoordinator) deletePrunedFacts(ctx context.Context, generationID int64, settings understandingRetentionSettings) retentionDeleteOutcome {
	for {
		if ctx.Err() != nil || !c.retentionStillInForce(settings) {
			return retentionStop
		}
		more, err := c.g.ix.DeletePrunedUnderstandingFacts(generationID, settings.deleteRows)
		switch {
		case store.IsBusyError(err):
			c.stats.pruneSkippedBusy.Add(1)
			if !sleepContext(ctx, settings.pause) {
				return retentionStop
			}
			return retentionSkipped
		case err != nil:
			log.Printf("understanding retention: delete facts of generation %d: %v", generationID, err)
			return retentionStop
		}
		if !sleepContext(ctx, settings.pause) {
			return retentionStop
		}
		if !more {
			return retentionDrained
		}
	}
}

// deleteUnreferencedDescriptors removes descriptors no unit references any more, a
// bounded transaction and a pause at a time, under the same rules as every other
// retention write. What it does not finish, the next pass that deletes rows does.
func (c *understandingScanCoordinator) deleteUnreferencedDescriptors(ctx context.Context, settings understandingRetentionSettings) {
	for {
		if ctx.Err() != nil || !c.retentionStillInForce(settings) {
			return
		}
		more, err := c.g.ix.DeleteUnreferencedUnderstandingDescriptors(settings.deleteDescriptors)
		switch {
		case store.IsBusyError(err):
			c.stats.pruneSkippedBusy.Add(1)
			return
		case err != nil:
			log.Printf("understanding retention: delete unreferenced descriptors: %v", err)
			return
		}
		if !more || !sleepContext(ctx, settings.pause) {
			return
		}
	}
}

// sleepContext waits for the duration and reports false when the context ended first.
func sleepContext(ctx context.Context, wait time.Duration) bool {
	if wait <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
