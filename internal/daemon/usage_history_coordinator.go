package daemon

// The usage recorder's lifecycle (token-usage-analytics plan §3.4): one
// background loop that runs a recorder pass on the configured interval over
// its own store handle, like the transcript index coordinator, and publishes
// what the last pass covered for the usage routes.

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"crossing-guard/harvest"
	"crossing-guard/internal/usagehistory"
	"crossing-guard/store"
)

// Usage coverage states, as the usage routes report them. Only a route that
// cannot read the store says unavailable; a recorder that cannot write leaves
// the recorded figures readable, so it is incomplete (code red-team C-5).
const (
	usageCoverageCatchingUp  = "catching-up" // no pass yet, or sources still have content to read
	usageCoverageCurrent     = "current"     // every discovered source read to its end
	usageCoverageIncomplete  = "incomplete"  // a source or the recorder's own store write failed; retried each pass
	usageCoverageUnavailable = "unavailable" // the route could not read the store
)

// usageRecorderStatus is what the last pass covered.
type usageRecorderStatus struct {
	State         string
	Discovered    int
	Recorded      int
	Pending       int
	Failed        int
	RecorderError bool
	AsOf          time.Time
	// Present and ListingFailed are the last pass's listing (nil before one).
	Present       map[string]map[string]bool
	ListingFailed map[string]bool
}

type usageRecorderCoordinator struct {
	path      string
	status    atomic.Pointer[usageRecorderStatus]
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
}

func newUsageRecorderCoordinator(path string) *usageRecorderCoordinator {
	ctx, cancel := context.WithCancel(context.Background())
	coordinator := &usageRecorderCoordinator{path: path, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	coordinator.status.Store(&usageRecorderStatus{State: usageCoverageCatchingUp})
	return coordinator
}

// daemonUsageRecorder is the running coordinator, read by the usage routes.
var daemonUsageRecorder *usageRecorderCoordinator

func (c *usageRecorderCoordinator) Start() {
	if c == nil {
		return
	}
	c.startOnce.Do(func() { go c.run() })
}

func (c *usageRecorderCoordinator) Close() {
	if c == nil {
		return
	}
	c.closeOnce.Do(func() {
		c.cancel()
		c.Start()
		<-c.done
	})
}

// Status is what the last pass covered; incomplete when there is no recorder.
func (c *usageRecorderCoordinator) Status() usageRecorderStatus {
	if c == nil {
		return usageRecorderStatus{State: usageCoverageIncomplete, RecorderError: true}
	}
	return *c.status.Load()
}

func (c *usageRecorderCoordinator) run() {
	defer close(c.done)
	var index *store.Index
	defer func() {
		if index != nil {
			_ = index.Close()
		}
	}()
	var recorder *usagehistory.Recorder
	config := usageHistoryConfig()
	for c.ctx.Err() == nil {
		if index == nil {
			opened, err := store.Open(c.path)
			if err != nil {
				log.Printf("usage recorder: store unavailable: %v", err)
				c.status.Store(&usageRecorderStatus{State: usageCoverageIncomplete, RecorderError: true, AsOf: time.Now()})
				if !c.wait(config.RecorderInterval()) {
					return
				}
				continue
			}
			index = opened
			recorder = usagehistory.New(harvest.UsageSources(), index)
		}
		result, err := recorder.Pass(c.ctx, config.Recorder.ReadBudgetBytes)
		if c.ctx.Err() != nil {
			return
		}
		c.status.Store(keepUsagePresence(c.status.Load(), usageStatusFor(result, err), err))
		if err != nil {
			log.Printf("usage recorder: pass stopped: %v", err)
			_ = index.Close()
			index, recorder = nil, nil
		}
		if !c.wait(config.RecorderInterval()) {
			return
		}
	}
}

func usageStatusFor(result usagehistory.PassResult, err error) *usageRecorderStatus {
	status := &usageRecorderStatus{Discovered: result.Discovered, Recorded: result.Recorded,
		Pending: result.Pending, Failed: result.Failed, AsOf: result.Finished,
		Present: result.Present, ListingFailed: result.ListingFailed}
	switch {
	case err != nil:
		status.State, status.RecorderError = usageCoverageIncomplete, true
		status.AsOf = time.Now()
	case result.Failed > 0:
		status.State = usageCoverageIncomplete
	case result.Pending > 0:
		status.State = usageCoverageCatchingUp
	default:
		status.State = usageCoverageCurrent
	}
	return status
}

// keepUsagePresence keeps the previous pass's listing when this pass failed:
// a pass that stopped partway listed only some runtimes, and the rest would
// otherwise read as removed (code red-team C-12).
func keepUsagePresence(previous, next *usageRecorderStatus, err error) *usageRecorderStatus {
	if err != nil && previous != nil {
		next.Present, next.ListingFailed = previous.Present, previous.ListingFailed
	}
	return next
}

func (c *usageRecorderCoordinator) wait(delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-c.ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
