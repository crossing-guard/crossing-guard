package daemon

// Feed producers for the console's multiplexed event stream (workspace-panes
// implementation plan, Slice T; fit-and-finish Slice 1). Each producer is the
// ONE owner of its feed's subscription and framing rules: the legacy per-feed
// handlers call the same functions, so the bytes a projection store reads are
// identical over either route.

import (
	"context"
	"encoding/json"
	"errors"

	"crossing-guard/internal/sessionactivity"
)

// feedFrame is one envelope on the multiplexed stream. Payload carries the
// feed's legacy body verbatim.
type feedFrame struct {
	Feed    string          `json:"feed"`
	Event   string          `json:"event"`
	Cursor  int64           `json:"cursor,omitempty"`
	Payload json.RawMessage `json:"payload"`
}

const (
	feedStream    = "stream"
	feedApprovals = "approvals"
	feedTasks     = "tasks"
	feedActivity  = "activity"
	feedSession   = "session"
)

// errFeedRestart tells the multiplexer that a feed reached a point where the
// legacy handler would have ended its connection (a reset, or a stalled
// approval client). The whole connection ends — after every other feed has
// opened, so the client never loses an opening picture to the close — and the
// client reconnects with its cursors; that reconnect is the price of one
// socket per tab (event-stream-reset-opening-barrier-plan §2).
var errFeedRestart = errors.New("feed requires a reconnect")

type feedEmitter func(frame feedFrame) error

// feedProducer runs one feed until ctx ends. It calls opened once its opening
// frames are in the emitter — or once it knows it has none, which is why the
// multiplexer cannot infer "open" from a first frame: a caught-up activity
// cursor and an empty task replay both open without one and then block.
// opened is idempotent; the multiplexer also treats the producer's return as
// opened, so a quiet end or a restart can never hold a close back.
type feedProducer func(ctx context.Context, emit feedEmitter, opened func()) error

func emitJSON(emit feedEmitter, feed, event string, cursor int64, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return emit(feedFrame{Feed: feed, Event: event, Cursor: cursor, Payload: encoded})
}

func emitUnavailable(emit feedEmitter, feed, reason string) error {
	return emitJSON(emit, feed, "unavailable", 0, map[string]string{"error": reason})
}

// ---- approvals -----------------------------------------------------------

// approvalStreamBuffer is how many broadcast frames one client may lag before
// the hub disconnects it; its reconnect receives a fresh snapshot.
const approvalStreamBuffer = 8

// subscribe registers one stream client and returns the authoritative
// pending/history snapshot bytes that client must see first.
func (h *approvalsHub) subscribe() (snapshot []byte, ch chan []byte, unsubscribe func()) {
	ch = make(chan []byte, approvalStreamBuffer)
	h.mu.Lock()
	h.streams[ch] = true
	pending := make([]Approval, 0, len(h.pending))
	for _, a := range h.pending {
		pending = append(pending, cloneApproval(a))
	}
	history := make([]Approval, 0, len(h.history))
	for _, a := range h.history {
		history = append(history, cloneApproval(a))
	}
	snapshot, _ = json.Marshal(map[string]any{"pending": pending, "history": history})
	h.mu.Unlock()
	return snapshot, ch, func() {
		h.mu.Lock()
		delete(h.streams, ch)
		h.mu.Unlock()
	}
}

func approvalFeed(ctx context.Context, emit feedEmitter, opened func()) error {
	snapshot, ch, unsubscribe := approvals.subscribe()
	defer unsubscribe()
	if err := emit(feedFrame{Feed: feedApprovals, Event: "snapshot", Payload: snapshot}); err != nil {
		return err
	}
	opened()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg, ok := <-ch:
			if !ok {
				return errFeedRestart
			}
			if err := emit(feedFrame{Feed: feedApprovals, Event: "approval", Payload: msg}); err != nil {
				return err
			}
		}
	}
}

// ---- runtime tasks -------------------------------------------------------

// taskFeedStart is what opening the task feed after a cursor yields: either a
// live subscription with its replay, or the reset the client must apply first.
type taskFeedStart struct {
	replay       []TaskEvent
	subscription taskSubscription
	reset        map[string]any
	resetCursor  int64
}

// taskFeedOpen applies the replay and retention rules once for both routes.
// On a reset the subscription is already released.
func taskFeedOpen(after int64) (taskFeedStart, error) {
	subscription, replay, truncated, err := runtimeTasks.SubscribeAll(after)
	if err != nil {
		return taskFeedStart{}, err
	}
	minimum, maximum, err := runtimeTasks.EventRange()
	if err != nil {
		runtimeTasks.Unsubscribe(subscription.ID)
		return taskFeedStart{}, err
	}
	reset := func(reason string) taskFeedStart {
		runtimeTasks.Unsubscribe(subscription.ID)
		return taskFeedStart{resetCursor: maximum, reset: map[string]any{
			"type": "reset", "reason": reason, "through_event_id": maximum}}
	}
	if after > maximum || (after > 0 && minimum > 0 && after+1 < minimum) {
		return reset("runtime task replay cursor is outside retention"), nil
	}
	if truncated {
		return reset("runtime task replay exceeds the bounded window"), nil
	}
	return taskFeedStart{replay: replay, subscription: subscription}, nil
}

// taskForwarder deduplicates the replay/live overlap: an event at or below the
// last forwarded id is dropped.
func taskForwarder(after int64) func(TaskEvent) bool {
	last := after
	return func(event TaskEvent) bool {
		if event.EventID <= last {
			return false
		}
		last = event.EventID
		return true
	}
}

func taskFeed(ctx context.Context, after int64, emit feedEmitter, opened func()) error {
	if runtimeTasks == nil {
		return emitUnavailable(emit, feedTasks, runtimeTasksUnavailable("runtime task service unavailable"))
	}
	start, err := taskFeedOpen(after)
	if err != nil {
		return emitUnavailable(emit, feedTasks, err.Error())
	}
	if start.reset != nil {
		if err := emitJSON(emit, feedTasks, "reset", start.resetCursor, start.reset); err != nil {
			return err
		}
		return errFeedRestart
	}
	defer runtimeTasks.Unsubscribe(start.subscription.ID)
	forward := taskForwarder(after)
	for _, event := range start.replay {
		if !forward(event) {
			continue
		}
		if err := emitJSON(emit, feedTasks, "task", event.EventID, event); err != nil {
			return err
		}
		// The first replay frame is the opening picture: the client's cursor is
		// the last frame it applied, so a replay cut by a close resumes exactly
		// there. Waiting for the whole replay (up to 2,000 frames) would only
		// slow another feed's restart.
		opened()
	}
	opened() // an empty replay opens with no frame at all
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-start.subscription.Events:
			if !ok {
				return errFeedRestart
			}
			if forward(event) {
				if err := emitJSON(emit, feedTasks, "task", event.EventID, event); err != nil {
					return err
				}
			}
		}
	}
}

// ---- native session activity ----------------------------------------------

// activityFeedOpen returns the reset snapshot when the cursor is ahead of the
// service, otherwise a live subscription. queued says whether that
// subscription starts with a frame: it is decided from the snapshot read
// here, never by probing the channel (which races the service's refresh).
// Generation is monotonic, so a cursor behind this snapshot is behind the one
// Subscribe saw, and Subscribe queued exactly one frame.
func activityFeedOpen(after int64) (reset *sessionactivity.Snapshot, subscription sessionactivity.Subscription, queued bool) {
	current := sessionActivityService().Snapshot()
	if after > current.Generation {
		return &current, sessionactivity.Subscription{}, false
	}
	return nil, sessionActivityService().Subscribe(after), current.Generation > after
}

func activityFeed(ctx context.Context, after int64, emit feedEmitter, opened func()) error {
	if sessionActivityService() == nil {
		return emitUnavailable(emit, feedActivity, "native session activity unavailable")
	}
	reset, subscription, queued := activityFeedOpen(after)
	if reset != nil {
		if err := emitJSON(emit, feedActivity, "reset", reset.Generation, reset); err != nil {
			return err
		}
		return errFeedRestart
	}
	defer sessionActivityService().Unsubscribe(subscription.ID)
	if !queued {
		opened() // a caught-up cursor has no opening frame; the next change is live
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case snapshot := <-subscription.Updates:
			if err := emitJSON(emit, feedActivity, "activity", snapshot.Generation, snapshot); err != nil {
				return err
			}
			opened() // the queued generation frame was the opening picture
		}
	}
}

// ---- live session ----------------------------------------------------------

// sessionFeed follows one session. A finished feed (the session became
// unresolvable, or the budget refused it) does not end the connection: the
// other feeds keep flowing and the client reads the honest terminal frame.
func sessionFeed(ctx context.Context, runtime, id string, emit feedEmitter, opened func()) error {
	if runtime == "" || id == "" {
		return nil
	}
	release, ok := acquireLiveSessionSlot()
	if !ok {
		return emitUnavailable(emit, feedSession, "live session stream budget exhausted")
	}
	defer release()
	// The live producer's first frame (snapshot or unavailable) is its opening
	// picture; the producer itself stays shared with the legacy live route.
	return sessionLiveProducer(ctx, runtime, id, func(event string, payload any) error {
		if err := emitJSON(emit, feedSession, event, 0, payload); err != nil {
			return err
		}
		opened()
		return nil
	})
}
