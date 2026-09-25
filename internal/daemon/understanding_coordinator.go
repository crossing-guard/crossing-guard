package daemon

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"crossing-guard/codemap"
	"crossing-guard/internal/changeenv"
	"crossing-guard/internal/understanding"
	"crossing-guard/store"
)

const (
	understandingPendingCapacity = 64
	understandingQuietWindow     = 15 * time.Second
	understandingScanTimeout     = 60 * time.Second
	understandingFailureBackoff  = 5 * time.Minute
	understandingRecoveryWindow  = 24 * time.Hour
	understandingRecoveryLimit   = 10
	understandingRecoveryPace    = time.Minute
)

type understandingCoordinatorStats struct {
	offered     atomic.Int64
	coalesced   atomic.Int64
	overflow    atomic.Int64
	started     atomic.Int64
	completed   atomic.Int64
	failed      atomic.Int64
	superseded  atomic.Int64
	active      atomic.Int64
	recovered   atomic.Int64
	missingRoot atomic.Int64
}

type understandingWork struct {
	source store.UnderstandingCheckpoint
	due    time.Time
}

type understandingScanFunc func(context.Context, *store.Index, understanding.ScanInput) (understanding.ScanResult, error)

// understandingScanCoordinator admits full repository analysis separately from Git
// checkpoint capture. It keeps only the latest pending source per checkout and runs one
// scan globally.
type understandingScanCoordinator struct {
	g      *Governor
	mu     sync.Mutex
	items  map[string]understandingWork
	active understandingWork
	wake   chan struct{}
	stop   context.CancelFunc
	now    func() time.Time
	scan   understandingScanFunc
	quiet  time.Duration
	stats  understandingCoordinatorStats
	closed bool
}

func newUnderstandingScanCoordinator(g *Governor) *understandingScanCoordinator {
	return &understandingScanCoordinator{g: g, items: map[string]understandingWork{},
		wake: make(chan struct{}, 1), now: time.Now, scan: understanding.Scan,
		quiet: understandingQuietWindow}
}

func (c *understandingScanCoordinator) offerCheckpoint(checkpointID int64) {
	if c == nil || checkpointID == 0 {
		return
	}
	source, err := c.g.ix.UnderstandingCheckpointByID(checkpointID)
	if err != nil {
		c.stats.failed.Add(1)
		return
	}
	c.offer(source)
}

func (c *understandingScanCoordinator) offer(source store.UnderstandingCheckpoint) {
	change := source.Change
	checkpoint := source.Checkpoint
	if checkpoint.Status != "complete" || change.SnapshotDigest == "" ||
		change.RepositoryID == "" || change.CheckoutID == "" || change.CheckoutRoot == "" {
		c.stats.failed.Add(1)
		return
	}
	existing, found, err := c.g.ix.UnderstandingForSnapshot(change.RepositoryID,
		change.CheckoutID, change.SnapshotDigest, codemap.StructuralSchema,
		c.g.analyzerAssembly.Digest(), "none", "")
	if err != nil {
		c.stats.failed.Add(1)
		return
	}
	if found {
		if existing.Status == "complete" || (existing.Status == "failed" &&
			existing.EndedAt > c.now().Add(-understandingFailureBackoff).UnixNano()) {
			c.stats.coalesced.Add(1)
			return
		}
	}
	due := c.now().Add(c.quiet)
	if checkpoint.Kind == "attachment" || checkpoint.Kind == "pre-mutation" ||
		checkpoint.Kind == "closing" || checkpoint.Kind == "timeout" {
		due = c.now()
	}
	key := understandingCheckoutKey(change.RepositoryID, change.CheckoutID)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	if current, ok := c.items[key]; ok {
		if newerUnderstandingSource(current.source.Checkpoint, checkpoint) {
			c.items[key] = understandingWork{source: source, due: due}
		}
		c.stats.coalesced.Add(1)
		c.mu.Unlock()
		c.signal()
		return
	}
	if len(c.items) >= understandingPendingCapacity {
		c.stats.overflow.Add(1)
		c.mu.Unlock()
		return
	}
	c.items[key] = understandingWork{source: source, due: due}
	c.stats.offered.Add(1)
	c.mu.Unlock()
	c.signal()
}

func newerUnderstandingSource(current, offered store.SessionCheckpoint) bool {
	return offered.CaptureEndedAt > current.CaptureEndedAt ||
		(offered.CaptureEndedAt == current.CaptureEndedAt && offered.ID > current.ID)
}

func (c *understandingScanCoordinator) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *understandingScanCoordinator) nextReady(now time.Time) (understandingWork, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var selectedKey string
	var selected understandingWork
	for key, work := range c.items {
		if work.due.After(now) {
			continue
		}
		if selectedKey == "" || work.due.Before(selected.due) ||
			(work.due.Equal(selected.due) && newerUnderstandingSource(selected.source.Checkpoint, work.source.Checkpoint)) {
			selectedKey, selected = key, work
		}
	}
	if selectedKey == "" {
		return understandingWork{}, false
	}
	delete(c.items, selectedKey)
	return selected, true
}

func (c *understandingScanCoordinator) runWork(parent context.Context, work understandingWork) {
	c.mu.Lock()
	c.active = work
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.active = understandingWork{}
		c.mu.Unlock()
	}()
	change := work.source.Change
	c.stats.started.Add(1)
	c.stats.active.Add(1)
	defer c.stats.active.Add(-1)
	ctx, cancel := context.WithTimeout(parent, understandingScanTimeout)
	defer cancel()
	result, err := c.scan(ctx, c.g.ix, understanding.ScanInput{RepoDir: change.CheckoutRoot,
		Assembly: c.g.analyzerAssembly,
		Base:     change.BaseRevision, Expected: &understanding.SnapshotExpectation{
			RepositoryID: change.RepositoryID, CheckoutID: change.CheckoutID,
			CheckoutRoot: change.CheckoutRoot, SnapshotProtocol: changeenv.GitTreeProtocol,
			SnapshotDigest: change.SnapshotDigest, BaseRevision: change.BaseRevision,
			HeadRevision: change.HeadRevision,
		}})
	if errors.Is(err, understanding.ErrSnapshotSuperseded) || result.Outcome == "superseded" {
		c.stats.superseded.Add(1)
		return
	}
	if err != nil {
		c.stats.failed.Add(1)
		return
	}
	c.stats.completed.Add(1)
}

// pending reports only an exact queued or active snapshot. It performs no admission
// and is safe for read-only HTTP status projection.
func understandingCheckoutKey(repositoryID, checkoutID string) string {
	return repositoryID + "\x00" + checkoutID
}

func (c *understandingScanCoordinator) pending(repositoryID, checkoutID, snapshotDigest string) bool {
	if c == nil || repositoryID == "" || checkoutID == "" || snapshotDigest == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := understandingCheckoutKey(repositoryID, checkoutID)
	if work, found := c.items[key]; found && work.source.Change.SnapshotDigest == snapshotDigest {
		return true
	}
	return c.active.source.Change.RepositoryID == repositoryID &&
		c.active.source.Change.CheckoutID == checkoutID &&
		c.active.source.Change.SnapshotDigest == snapshotDigest
}

func (c *understandingScanCoordinator) recoverOne(now time.Time) bool {
	checkpoints, err := c.g.ix.RecentCheckpointsNeedingUnderstanding(
		now.Add(-understandingRecoveryWindow).Unix(),
		now.Add(-understandingFailureBackoff).UnixNano(), codemap.StructuralSchema,
		c.g.analyzerAssembly.Digest(), 1)
	if err != nil {
		c.stats.failed.Add(1)
		return false
	}
	if len(checkpoints) == 0 {
		return false
	}
	source, err := c.g.ix.UnderstandingCheckpointByID(checkpoints[0].ID)
	if err != nil {
		c.stats.missingRoot.Add(1)
		return true
	}
	c.stats.recovered.Add(1)
	c.offer(source)
	return true
}

func (c *understandingScanCoordinator) run(ctx context.Context) {
	workTicker := time.NewTicker(500 * time.Millisecond)
	recoveryTicker := time.NewTicker(understandingRecoveryPace)
	defer workTicker.Stop()
	defer recoveryTicker.Stop()
	recoveryAdmissions := 0
	if c.recoverOne(c.now()) {
		recoveryAdmissions++
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.wake:
		case <-workTicker.C:
		case <-recoveryTicker.C:
			if recoveryAdmissions < understandingRecoveryLimit && c.recoverOne(c.now()) {
				recoveryAdmissions++
			}
		}
		if work, ok := c.nextReady(c.now()); ok {
			c.runWork(ctx, work)
		}
	}
}

func startUnderstandingScheduling(g *Governor) {
	coordinator := newUnderstandingScanCoordinator(g)
	ctx, cancel := context.WithCancel(context.Background())
	coordinator.stop = cancel
	g.understanding = coordinator
	go coordinator.run(ctx)
}

func stopUnderstandingScheduling(g *Governor) {
	if g == nil || g.understanding == nil {
		return
	}
	coordinator := g.understanding
	coordinator.mu.Lock()
	coordinator.closed = true
	coordinator.items = map[string]understandingWork{}
	coordinator.mu.Unlock()
	if coordinator.stop != nil {
		coordinator.stop()
	}
}

func requestUnderstandingCheckpoint(g *Governor, checkpoint store.SessionCheckpoint) {
	if g == nil || g.understanding == nil || checkpoint.Status != "complete" {
		return
	}
	g.understanding.offerCheckpoint(checkpoint.ID)
}
