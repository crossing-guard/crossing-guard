package daemon

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"crossing-guard/internal/transcriptindex"
	"crossing-guard/store"
)

const (
	transcriptIndexSweepInterval = 30 * time.Second
	transcriptIndexBusyBackoff   = 2 * time.Second
	transcriptIndexMaxAdmissions = 100
)

// transcriptIndexRefreshOwner is the daemon's narrow lifecycle boundary around the
// shared application use case and its long-lived store handle.
type transcriptIndexRefreshOwner interface {
	Refresh(context.Context, transcriptindex.RefreshRequest) (transcriptindex.RefreshResult, error)
	Close() error
}

type transcriptIndexRefreshFactory func() (transcriptIndexRefreshOwner, error)

type transcriptIndexTimer interface {
	C() <-chan time.Time
	Stop()
}

type transcriptIndexClock interface {
	Now() time.Time
	NewTimer(time.Duration) transcriptIndexTimer
}

type realTranscriptIndexClock struct{}

func (realTranscriptIndexClock) Now() time.Time { return time.Now() }
func (realTranscriptIndexClock) NewTimer(delay time.Duration) transcriptIndexTimer {
	return realTranscriptIndexTimer{Timer: time.NewTimer(delay)}
}

type realTranscriptIndexTimer struct{ *time.Timer }

func (t realTranscriptIndexTimer) C() <-chan time.Time { return t.Timer.C }
func (t realTranscriptIndexTimer) Stop()               { _ = t.Timer.Stop() }

// transcriptIndexCoverageStore is the one immutable handoff from the background
// reconciler to HTTP handlers. Coverage contains slices, so both directions clone.
type transcriptIndexCoverageStore struct {
	value atomic.Pointer[transcriptindex.Coverage]
}

func newTranscriptIndexCoverageStore(now time.Time) *transcriptIndexCoverageStore {
	store := &transcriptIndexCoverageStore{}
	store.Store(transcriptindex.Coverage{
		State: transcriptindex.CoverageCatchingUp, CoverageAsOf: now.UTC(),
	})
	return store
}

func (s *transcriptIndexCoverageStore) Store(coverage transcriptindex.Coverage) {
	if s == nil {
		return
	}
	copy := cloneTranscriptIndexCoverage(coverage)
	s.value.Store(&copy)
}

func (s *transcriptIndexCoverageStore) Load() transcriptindex.Coverage {
	if s == nil {
		return transcriptindex.UnavailableCoverage(errors.New("coverage snapshot is unavailable"))
	}
	coverage := s.value.Load()
	if coverage == nil {
		return transcriptindex.UnavailableCoverage(errors.New("coverage snapshot is unavailable"))
	}
	return cloneTranscriptIndexCoverage(*coverage)
}

func cloneTranscriptIndexCoverage(coverage transcriptindex.Coverage) transcriptindex.Coverage {
	out := coverage
	out.Limitations = append([]transcriptindex.Limitation(nil), coverage.Limitations...)
	out.Sessions = append([]transcriptindex.SessionCoverage(nil), coverage.Sessions...)
	for index := range out.Sessions {
		out.Sessions[index].Limitations = append([]transcriptindex.Limitation(nil),
			coverage.Sessions[index].Limitations...)
	}
	return out
}

var daemonTranscriptIndexCoverage = newTranscriptIndexCoverageStore(time.Now())

// transcriptIndexCoordinator schedules one completion-relative refresh loop. It is
// deliberately independent of Governor and the lifecycle reconciliation queue.
type transcriptIndexCoordinator struct {
	factory  transcriptIndexRefreshFactory
	clock    transcriptIndexClock
	coverage *transcriptIndexCoverageStore

	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
}

func newTranscriptIndexCoordinator(factory transcriptIndexRefreshFactory,
	clock transcriptIndexClock, coverage *transcriptIndexCoverageStore) *transcriptIndexCoordinator {
	if clock == nil {
		clock = realTranscriptIndexClock{}
	}
	if coverage == nil {
		coverage = newTranscriptIndexCoverageStore(clock.Now())
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &transcriptIndexCoordinator{
		factory: factory, clock: clock, coverage: coverage,
		ctx: ctx, cancel: cancel, done: make(chan struct{}),
	}
}

func newDaemonTranscriptIndexCoordinator(path string) *transcriptIndexCoordinator {
	factory := func() (transcriptIndexRefreshOwner, error) {
		ix, err := store.Open(path)
		if err != nil {
			kind := transcriptindex.LimitationRepositoryUnavailable
			if store.IsBusyError(err) {
				kind = transcriptindex.LimitationRepositoryBusy
			}
			return nil, transcriptindex.NewLimitedError(
				transcriptindex.Limitation{Kind: kind}, err)
		}
		// The orphan sweep runs on this handle. It spares sessions the owner
		// tagged, up to the configured bound.
		ix.SetOwnerKeptSessionsLimit(sessionOrganizationConfig().KeptSessionsMax)
		return &daemonTranscriptIndexRefresher{
			Indexer: transcriptindex.New(transcriptindex.NewRegisteredHarvestCatalog(),
				transcriptindex.NewStoreRepository(ix)),
			index: ix,
		}, nil
	}
	return newTranscriptIndexCoordinator(factory, realTranscriptIndexClock{},
		daemonTranscriptIndexCoverage)
}

type daemonTranscriptIndexRefresher struct {
	*transcriptindex.Indexer
	index *store.Index
}

func (r *daemonTranscriptIndexRefresher) Close() error {
	if r == nil || r.index == nil {
		return nil
	}
	return r.index.Close()
}

func (c *transcriptIndexCoordinator) Start() {
	if c == nil {
		return
	}
	c.startOnce.Do(func() { go c.run() })
}

func (c *transcriptIndexCoordinator) Close() {
	if c == nil {
		return
	}
	c.closeOnce.Do(func() {
		c.cancel()
		c.Start()
		<-c.done
	})
}

func (c *transcriptIndexCoordinator) Coverage() transcriptindex.Coverage {
	if c == nil {
		return transcriptindex.UnavailableCoverage(errors.New("transcript index coordinator is unavailable"))
	}
	return c.coverage.Load()
}

func (c *transcriptIndexCoordinator) run() {
	defer close(c.done)
	var owner transcriptIndexRefreshOwner
	defer func() {
		if owner != nil {
			_ = owner.Close()
		}
	}()

	for {
		if err := c.ctx.Err(); err != nil {
			return
		}
		if owner == nil {
			var opened transcriptIndexRefreshOwner
			var err error
			if c.factory == nil {
				err = errors.New("transcript index refresh factory is unavailable")
			} else {
				opened, err = c.factory()
			}
			if err == nil && opened == nil {
				err = errors.New("transcript index refresh owner is unavailable")
			}
			if err != nil {
				coverage := transcriptindex.UnavailableCoverage(err)
				c.coverage.Store(coverage)
				if !c.wait(nextTranscriptIndexDelay(coverage)) {
					return
				}
				continue
			}
			owner = opened
		}

		result, err := owner.Refresh(c.ctx, transcriptindex.RefreshRequest{
			Mode: transcriptindex.RefreshModeIncremental, MaxSessions: transcriptIndexMaxAdmissions,
		})
		if c.ctx.Err() != nil {
			return
		}
		coverage := result.Coverage
		if coverage.State == "" {
			if err == nil {
				err = errors.New("transcript index refresh returned no coverage state")
			}
			coverage = transcriptindex.UnavailableCoverage(err)
		}
		c.coverage.Store(coverage)
		if hasTranscriptIndexLimitation(coverage, transcriptindex.LimitationRepositoryUnavailable) {
			_ = owner.Close()
			owner = nil
		}
		if !c.wait(nextTranscriptIndexDelay(coverage)) {
			return
		}
	}
}

func (c *transcriptIndexCoordinator) wait(delay time.Duration) bool {
	timer := c.clock.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-c.ctx.Done():
		return false
	case <-timer.C():
		return true
	}
}

func nextTranscriptIndexDelay(coverage transcriptindex.Coverage) time.Duration {
	if hasTranscriptIndexLimitation(coverage, transcriptindex.LimitationRepositoryBusy) {
		return transcriptIndexBusyBackoff
	}
	return transcriptIndexSweepInterval
}

func hasTranscriptIndexLimitation(coverage transcriptindex.Coverage,
	kind transcriptindex.LimitationKind) bool {
	for _, limitation := range coverage.Limitations {
		if limitation.Kind == kind {
			return true
		}
	}
	for _, session := range coverage.Sessions {
		for _, limitation := range session.Limitations {
			if limitation.Kind == kind {
				return true
			}
		}
	}
	return false
}
