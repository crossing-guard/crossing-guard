package daemon

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"crossing-guard/internal/transcriptindex"
)

type fakeTranscriptIndexTimer struct {
	delay time.Duration
	ch    chan time.Time
}

func (t *fakeTranscriptIndexTimer) C() <-chan time.Time { return t.ch }
func (*fakeTranscriptIndexTimer) Stop()                 {}
func (t *fakeTranscriptIndexTimer) Fire()               { t.ch <- time.Unix(1, 0) }

type fakeTranscriptIndexClock struct {
	now    time.Time
	timers chan *fakeTranscriptIndexTimer
}

func newFakeTranscriptIndexClock() *fakeTranscriptIndexClock {
	return &fakeTranscriptIndexClock{now: time.Unix(1, 0).UTC(),
		timers: make(chan *fakeTranscriptIndexTimer, 16)}
}

func (c *fakeTranscriptIndexClock) Now() time.Time { return c.now }
func (c *fakeTranscriptIndexClock) NewTimer(delay time.Duration) transcriptIndexTimer {
	timer := &fakeTranscriptIndexTimer{delay: delay, ch: make(chan time.Time, 1)}
	c.timers <- timer
	return timer
}

func awaitTranscriptIndexTimer(t *testing.T, clock *fakeTranscriptIndexClock) *fakeTranscriptIndexTimer {
	t.Helper()
	select {
	case timer := <-clock.timers:
		return timer
	case <-time.After(time.Second):
		t.Fatal("coordinator did not schedule its next attempt")
		return nil
	}
}

type fakeTranscriptIndexOwner struct {
	refresh func(context.Context, transcriptindex.RefreshRequest) (transcriptindex.RefreshResult, error)
	closed  atomic.Int64
}

func (o *fakeTranscriptIndexOwner) Refresh(ctx context.Context,
	request transcriptindex.RefreshRequest) (transcriptindex.RefreshResult, error) {
	return o.refresh(ctx, request)
}
func (o *fakeTranscriptIndexOwner) Close() error { o.closed.Add(1); return nil }

func TestTranscriptIndexCoordinatorStartsNonBlockingAndCancelsOneWorker(t *testing.T) {
	clock := newFakeTranscriptIndexClock()
	started := make(chan transcriptindex.RefreshRequest, 1)
	cancelled := make(chan struct{})
	var active, maximum atomic.Int64
	owner := &fakeTranscriptIndexOwner{refresh: func(ctx context.Context,
		request transcriptindex.RefreshRequest) (transcriptindex.RefreshResult, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			prior := maximum.Load()
			if current <= prior || maximum.CompareAndSwap(prior, current) {
				break
			}
		}
		started <- request
		<-ctx.Done()
		close(cancelled)
		return transcriptindex.RefreshResult{}, ctx.Err()
	}}
	coverage := newTranscriptIndexCoverageStore(clock.Now())
	coordinator := newTranscriptIndexCoordinator(
		func() (transcriptIndexRefreshOwner, error) { return owner, nil }, clock, coverage)

	returned := make(chan struct{})
	go func() { coordinator.Start(); close(returned) }()
	select {
	case <-returned:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Start blocked on initial catch-up")
	}
	var request transcriptindex.RefreshRequest
	select {
	case request = <-started:
	case <-time.After(time.Second):
		t.Fatal("initial catch-up did not start")
	}
	if request.Mode != transcriptindex.RefreshModeIncremental ||
		request.MaxSessions != transcriptIndexMaxAdmissions {
		t.Fatalf("refresh request=%+v", request)
	}
	coordinator.Close()
	select {
	case <-cancelled:
	default:
		t.Fatal("Close did not cancel the in-flight refresh")
	}
	if maximum.Load() != 1 || owner.closed.Load() != 1 {
		t.Fatalf("max workers=%d closes=%d", maximum.Load(), owner.closed.Load())
	}
	if got := coverage.Load(); got.State != transcriptindex.CoverageCatchingUp {
		t.Fatalf("shutdown published a cancelled result: %+v", got)
	}
}

func TestTranscriptIndexCoordinatorBusyBackoffCadenceAndSnapshotIsolation(t *testing.T) {
	clock := newFakeTranscriptIndexClock()
	var calls atomic.Int64
	completed := make(chan int64, 4)
	owner := &fakeTranscriptIndexOwner{refresh: func(context.Context,
		transcriptindex.RefreshRequest) (transcriptindex.RefreshResult, error) {
		call := calls.Add(1)
		completed <- call
		if call == 1 {
			return transcriptindex.RefreshResult{Coverage: transcriptindex.Coverage{
				State: transcriptindex.CoverageIncomplete, CoverageAsOf: clock.Now(),
				Limitations: []transcriptindex.Limitation{{
					Kind: transcriptindex.LimitationRepositoryBusy,
				}},
			}}, nil
		}
		return transcriptindex.RefreshResult{Coverage: transcriptindex.Coverage{
			State: transcriptindex.CoverageCurrent, CoverageAsOf: clock.Now(),
			Sessions: []transcriptindex.SessionCoverage{{
				Key:   transcriptindex.SessionKey{Runtime: "codex", SessionID: "native"},
				State: transcriptindex.CoverageCurrent,
			}},
		}}, nil
	}}
	snapshot := newTranscriptIndexCoverageStore(clock.Now())
	coordinator := newTranscriptIndexCoordinator(
		func() (transcriptIndexRefreshOwner, error) { return owner, nil }, clock, snapshot)
	coordinator.Start()
	defer coordinator.Close()

	if call := <-completed; call != 1 {
		t.Fatalf("first call=%d", call)
	}
	busyTimer := awaitTranscriptIndexTimer(t, clock)
	if busyTimer.delay != transcriptIndexBusyBackoff {
		t.Fatalf("busy delay=%s", busyTimer.delay)
	}
	busyTimer.Fire()
	if call := <-completed; call != 2 {
		t.Fatalf("second call=%d", call)
	}
	regularTimer := awaitTranscriptIndexTimer(t, clock)
	if regularTimer.delay != transcriptIndexSweepInterval {
		t.Fatalf("regular delay=%s", regularTimer.delay)
	}

	read := coordinator.Coverage()
	read.Sessions[0].State = transcriptindex.CoverageUnavailable
	read.Sessions = append(read.Sessions, transcriptindex.SessionCoverage{})
	again := coordinator.Coverage()
	if again.State != transcriptindex.CoverageCurrent || len(again.Sessions) != 1 ||
		again.Sessions[0].State != transcriptindex.CoverageCurrent {
		t.Fatalf("caller mutated shared snapshot: %+v", again)
	}
}

func TestTranscriptIndexCoordinatorFactoryRecoveryAndRestart(t *testing.T) {
	clock := newFakeTranscriptIndexClock()
	var factories atomic.Int64
	completed := make(chan struct{}, 1)
	owner := &fakeTranscriptIndexOwner{refresh: func(context.Context,
		transcriptindex.RefreshRequest) (transcriptindex.RefreshResult, error) {
		completed <- struct{}{}
		return transcriptindex.RefreshResult{Coverage: transcriptindex.Coverage{
			State: transcriptindex.CoverageCurrent, CoverageAsOf: clock.Now(),
		}}, nil
	}}
	factory := func() (transcriptIndexRefreshOwner, error) {
		if factories.Add(1) == 1 {
			return nil, transcriptindex.NewLimitedError(transcriptindex.Limitation{
				Kind: transcriptindex.LimitationRepositoryUnavailable,
			}, errors.New("fixture unavailable"))
		}
		return owner, nil
	}
	coordinator := newTranscriptIndexCoordinator(factory, clock,
		newTranscriptIndexCoverageStore(clock.Now()))
	coordinator.Start()
	retry := awaitTranscriptIndexTimer(t, clock)
	if retry.delay != transcriptIndexSweepInterval {
		t.Fatalf("unavailable retry=%s", retry.delay)
	}
	retry.Fire()
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("coordinator did not recover after reopening its owner")
	}
	coordinator.Close()

	// A fresh coordinator can reconcile the same durable owner after the prior
	// process-level object was interrupted; no singleton start state is retained.
	restartedClock := newFakeTranscriptIndexClock()
	restarted := newTranscriptIndexCoordinator(
		func() (transcriptIndexRefreshOwner, error) { return owner, nil }, restartedClock,
		newTranscriptIndexCoverageStore(restartedClock.Now()))
	restarted.Start()
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("fresh coordinator did not restart reconciliation")
	}
	restarted.Close()
}

func TestTranscriptIndexCoordinatorRejectsEmptyOwnerAndCoverage(t *testing.T) {
	t.Run("empty owner", func(t *testing.T) {
		clock := newFakeTranscriptIndexClock()
		snapshot := newTranscriptIndexCoverageStore(clock.Now())
		coordinator := newTranscriptIndexCoordinator(
			func() (transcriptIndexRefreshOwner, error) { return nil, nil }, clock, snapshot)
		coordinator.Start()
		_ = awaitTranscriptIndexTimer(t, clock)
		if got := coordinator.Coverage(); got.State != transcriptindex.CoverageUnavailable {
			t.Fatalf("empty owner coverage=%+v", got)
		}
		coordinator.Close()
	})

	t.Run("empty coverage", func(t *testing.T) {
		clock := newFakeTranscriptIndexClock()
		snapshot := newTranscriptIndexCoverageStore(clock.Now())
		owner := &fakeTranscriptIndexOwner{refresh: func(context.Context,
			transcriptindex.RefreshRequest) (transcriptindex.RefreshResult, error) {
			return transcriptindex.RefreshResult{}, nil
		}}
		coordinator := newTranscriptIndexCoordinator(
			func() (transcriptIndexRefreshOwner, error) { return owner, nil }, clock, snapshot)
		coordinator.Start()
		_ = awaitTranscriptIndexTimer(t, clock)
		if got := coordinator.Coverage(); got.State != transcriptindex.CoverageUnavailable {
			t.Fatalf("empty refresh coverage=%+v", got)
		}
		coordinator.Close()
	})
}

func TestTranscriptIndexCoverageSnapshotConcurrentReaders(t *testing.T) {
	snapshot := newTranscriptIndexCoverageStore(time.Unix(1, 0))
	var writers sync.WaitGroup
	writers.Add(1)
	go func() {
		defer writers.Done()
		for index := 0; index < 500; index++ {
			snapshot.Store(transcriptindex.Coverage{
				State: transcriptindex.CoverageCurrent, CoverageAsOf: time.Unix(int64(index+1), 0),
				Limitations: []transcriptindex.Limitation{{Kind: transcriptindex.LimitationSourceUnreadable}},
			})
		}
	}()
	for index := 0; index < 500; index++ {
		coverage := snapshot.Load()
		if coverage.State == "" || coverage.CoverageAsOf.IsZero() {
			t.Fatalf("torn coverage snapshot: %+v", coverage)
		}
	}
	writers.Wait()
}
