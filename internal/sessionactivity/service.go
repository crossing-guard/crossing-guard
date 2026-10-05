package sessionactivity

import (
	"context"
	"sync"
	"time"
)

type Sampler func(context.Context, time.Time) (Capability, []Item)

type Subscription struct {
	ID      uint64
	Updates <-chan Snapshot
}

// Service owns exactly one bounded sampling schedule and one latest-value
// subscriber projection. Full snapshots make reconnect repair deterministic;
// native observations are transient and are never persisted as task truth.
//
// Writer serialization (opencode-session-visibility-and-status plan D6): a
// writerMu mutex serializes full refresh sample-plus-commit and event-driven
// replacement so a stale in-flight refresh cannot overwrite a newer event
// replacement. If an event arrives during a refresh it waits, then its
// replacement lands after the older refresh.
type Service struct {
	mu       sync.RWMutex
	writerMu sync.Mutex
	sampler  Sampler
	interval time.Duration
	timeout  time.Duration
	now      func() time.Time
	snapshot Snapshot
	subs     map[uint64]chan Snapshot
	nextSub  uint64
	cancel   context.CancelFunc
	done     chan struct{}
}

func NewService(sampler Sampler, interval, timeout time.Duration) *Service {
	return &Service{sampler: sampler, interval: interval, timeout: timeout, now: time.Now,
		subs: map[uint64]chan Snapshot{}}
}

func (s *Service) Start(parent context.Context) {
	s.mu.Lock()
	if s.cancel != nil {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.done = make(chan struct{})
	s.mu.Unlock()
	go s.run(ctx)
}

func (s *Service) run(ctx context.Context) {
	defer close(s.done)
	s.refresh(ctx)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.refresh(ctx)
		}
	}
}

func (s *Service) refresh(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, s.timeout)
	defer cancel()
	// writerMu serializes the full sample-plus-commit with event-driven
	// replacement so a newer event cannot be overwritten by a stale refresh.
	s.writerMu.Lock()
	defer s.writerMu.Unlock()
	now := s.now().UTC()
	capability, items := s.sampler(ctx, now)
	s.mu.Lock()
	s.snapshot = Snapshot{SchemaVersion: SchemaVersion, Generation: s.snapshot.Generation + 1,
		ObservedAt: now, Capability: capability, Items: append([]Item(nil), items...)}
	snapshot := cloneSnapshot(s.snapshot)
	for _, subscriber := range s.subs {
		select {
		case subscriber <- snapshot:
		default:
			select {
			case <-subscriber:
			default:
			}
			select {
			case subscriber <- snapshot:
			default:
			}
		}
	}
	s.mu.Unlock()
}

// Replace swaps (or appends) one session's item and publishes a new
// generation. This is the event-driven path: a turn row, a task event, or an
// approval decision refolds ONE session instead of waiting for the sampler's
// next full pass. Identity is runtime + catalog session id, the rail's key.
//
// writerMu ensures this lands after any in-flight refresh, not before it
// (plan D6): a stale refresh cannot overwrite a newer event replacement.
func (s *Service) Replace(item Item) {
	s.writerMu.Lock()
	defer s.writerMu.Unlock()
	s.mu.Lock()
	items := append([]Item(nil), s.snapshot.Items...)
	replaced := false
	for index := range items {
		if items[index].Runtime == item.Runtime && items[index].CatalogSessionID == item.CatalogSessionID {
			items[index] = item
			replaced = true
			break
		}
	}
	if !replaced {
		items = append(items, item)
	}
	s.snapshot = Snapshot{SchemaVersion: SchemaVersion, Generation: s.snapshot.Generation + 1,
		ObservedAt: s.now().UTC(), Capability: s.snapshot.Capability, Items: items}
	snapshot := cloneSnapshot(s.snapshot)
	for _, subscriber := range s.subs {
		select {
		case subscriber <- snapshot:
		default:
			select {
			case <-subscriber:
			default:
			}
			select {
			case subscriber <- snapshot:
			default:
			}
		}
	}
	s.mu.Unlock()
}

func (s *Service) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneSnapshot(s.snapshot)
}

func (s *Service) Subscribe(after int64) Subscription {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextSub++
	updates := make(chan Snapshot, 1)
	if s.snapshot.Generation > after {
		updates <- cloneSnapshot(s.snapshot)
	}
	s.subs[s.nextSub] = updates
	return Subscription{ID: s.nextSub, Updates: updates}
}

func (s *Service) Unsubscribe(id uint64) {
	s.mu.Lock()
	delete(s.subs, id)
	s.mu.Unlock()
}

func (s *Service) Close() {
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.cancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	snapshot.Items = append([]Item(nil), snapshot.Items...)
	return snapshot
}
