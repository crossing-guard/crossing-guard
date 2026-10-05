package daemon

// The fold gathers one session's facts from their owners — the task service,
// the approvals hub, the turn table, the lifecycle rows, the governance log —
// reduces them to the framework's vocabulary, and hands them to the decider.
// It is the only caller of the decider and the only writer of a session's
// status onto the activity snapshot. Every receiver that lands a fact about a
// session calls publishSessionStatus; the sampler decorates its presence
// items through the same function so the two paths cannot disagree.

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"crossing-guard/internal/sessionactivity"
	"crossing-guard/store"
)

func init() {
	// sessionStatusRefold is a package var only so tests can observe or
	// silence the publish; at runtime it is always this function.
	swapSessionStatusRefold(publishSessionStatus)
}

// wholeSecondUpperBoundMS places a whole-second stamp at the END of its
// second on the millisecond clock. A seconds stamp names an interval; reading
// it at the upper bound is the conservative choice for "the session is over":
// a Stop (ms) and a SessionEnd (s) from one process land in the same second
// (measured 910 ms apart) and the end must win that second — declared tie
// precedence then decides equal instants. Asymmetry, stated: a turn.started
// or tool call in the same second as an end also loses to it; a full second
// later it wins, which is correct (activity after the end).
const wholeSecondUpperBoundMS = 999

func lifecycleInstantMS(seconds int64) int64 { return seconds*1000 + wholeSecondUpperBoundMS }

// errSessionStatusOwner marks a fold that could not consult one of its owners.
// The frame is then honest silence — unknown/none — and the cause is logged
// once per session key, so a broken store looks broken instead of quiet.
var errSessionStatusOwner = errors.New("session status owner unavailable")

// foldSessionStatus computes the frame for one session from live owners. The
// identity it folds is the NATIVE id (the hooks' and the task service's key);
// the catalog id only widens the task lookup when the two differ.
func foldSessionStatus(now time.Time, runtime, catalogID, nativeID string) (sessionStatusFrame, error) {
	return foldSessionStatusWith(now, runtime, catalogID, nativeID, nil)
}

// foldSessionStatusWith folds with one sampler pass's owner-attention read;
// a nil batch reads this session's attention alone (an event refold).
func foldSessionStatusWith(now time.Time, runtime, catalogID, nativeID string, batch *ownerAttentionBatch) (sessionStatusFrame, error) {
	config := sessionStreamConfig()
	in := sessionStatusInputs{Now: now, Quiet: config.Quiet()}
	lookback := config.LookbackRows
	if nativeID == "" {
		nativeID = catalogID
	}
	var failures []error

	// Owned tasks: the one fully observed "working", and the owner of task
	// attention. An active task wins outright; otherwise the newest terminal.
	taskIDs := map[string]bool{}
	var sessionTasks []RuntimeTask
	var turns []store.SessionTurnObservation
	var turnsErr, tasksErr error
	if runtimeTasks != nil {
		tasks, err := runtimeTasks.List(runtime, nativeID, lookback)
		if err != nil {
			tasksErr = err
			failures = append(failures, fmt.Errorf("tasks: %w", err))
		}
		if catalogID != "" && catalogID != nativeID {
			more, err := runtimeTasks.List(runtime, catalogID, lookback)
			if err != nil {
				tasksErr = err
				failures = append(failures, fmt.Errorf("tasks by catalog id: %w", err))
			}
			tasks = append(tasks, more...)
		}
		sessionTasks = tasks
		var chosen *RuntimeTask
		for index := range tasks {
			task := &tasks[index]
			taskIDs[task.ID] = true
			if task.Lifecycle == TaskUnknown {
				continue // lost state is not an owned fact
			}
			if chosen == nil || isActiveLifecycle(task.Lifecycle) && !isActiveLifecycle(chosen.Lifecycle) ||
				(isActiveLifecycle(task.Lifecycle) == isActiveLifecycle(chosen.Lifecycle) && task.UpdatedAt > chosen.UpdatedAt) {
				chosen = task
			}
		}
		if chosen != nil {
			visible, err := taskHasVisibleOutput(chosen)
			if err != nil {
				failures = append(failures, fmt.Errorf("task output: %w", err))
			}
			in.Owned = &sessionStatusOwned{Lifecycle: chosen.Lifecycle, UpdatedAtMS: chosen.UpdatedAt,
				LastEventID: chosen.LastEventID, HasVisibleOutput: visible}
		}
	}
	if approvals != nil {
		in.PendingApprovals = approvals.pendingForSession(runtime, catalogID, nativeID, taskIDs)
	}
	if governor != nil && governor.ix != nil {
		// Turn boundaries from installed hooks — the observed facts.
		turns, turnsErr = governor.ix.SessionTurnsFor(runtime, nativeID, lookback)
		if turnsErr != nil {
			failures = append(failures, fmt.Errorf("turn rows: %w", turnsErr))
		}
		for _, turn := range turns {
			in.Facts = append(in.Facts, sessionStatusFact{Kind: turn.Kind, AtMS: turn.ReceivedAtMS, RowID: turn.RowID})
		}
		// Recorded session ends. One that landed within the attribution window
		// of OUR task's completion is that process ending, never the native
		// session's end — a console `--resume` shares the human's native id and
		// must not mark their terminal session idle underneath them. The hook
		// row carries no process identity, so proximity is the signal; the
		// window is configuration and deliberately tight.
		rows, err := governor.ix.SessionActivityObservations(runtime, nativeID, lookback)
		if err != nil {
			failures = append(failures, fmt.Errorf("lifecycle rows: %w", err))
		}
		for _, row := range rows {
			if row.EntryKind != "end" {
				continue
			}
			atMS := lifecycleInstantMS(row.ReceivedAt)
			if in.Owned != nil && isTerminalLifecycle(in.Owned.Lifecycle) &&
				absMS(in.Owned.UpdatedAtMS-atMS) <= config.OwnedEndAttribution().Milliseconds() {
				continue
			}
			in.Facts = append(in.Facts, sessionStatusFact{Kind: "session.ended", AtMS: atMS})
		}
		// The newest governed tool call is work in flight — the only boundary
		// a session without the turn hooks can offer. Runtime-qualified so a
		// native id shared across runtimes cannot cross wires.
		ts, ok, err := governor.ix.SessionNewestActionAt(runtime, nativeID)
		if err != nil {
			failures = append(failures, fmt.Errorf("newest action: %w", err))
		} else if ok {
			in.Facts = append(in.Facts, sessionStatusFact{Kind: "action.observed", AtMS: ts * 1000})
		}
	}
	// Owner attention is peripheral and isolated: its failure degrades only
	// the ask fields, and the ladders' failure does not hide an ask.
	readErr := turnsErr
	if readErr == nil {
		readErr = tasksErr
	}
	owner := foldOwnerAttention(now, runtime, catalogID, nativeID, batch, turns, readErr, sessionTasks)
	if len(failures) > 0 {
		return sessionStatusFrame{Execution: "unknown", Authority: "none", Attention: "none", Owner: owner},
			fmt.Errorf("%w: %w", errSessionStatusOwner, errors.Join(failures...))
	}
	frame := decideSessionStatus(in)
	frame.Owner = owner
	return frame, nil
}

func absMS(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// taskHasVisibleOutput mirrors the browser's former rule: a completed task
// counts as unread output only if the vendor actually said something. It
// reads the TAIL of the task's events — the final message is what matters.
func taskHasVisibleOutput(task *RuntimeTask) (bool, error) {
	if runtimeTasks == nil || task == nil {
		return false, nil
	}
	window := int64(sessionStreamConfig().LookbackRows)
	after := task.LastSequence - window
	if after < 0 {
		after = 0
	}
	events, err := runtimeTasks.Events(task.ID, after, int(window)+1)
	if err != nil {
		return false, err
	}
	for _, event := range events {
		if event.Kind != "message.completed" {
			continue
		}
		if text, _ := event.Payload["text"].(string); text != "" {
			return true, nil
		}
	}
	return false, nil
}

// applySessionStatus writes the frame onto an activity item.
func applySessionStatus(item sessionactivity.Item, frame sessionStatusFrame) sessionactivity.Item {
	item.Execution = frame.Execution
	if frame.Authority != "none" {
		item.Authority = frame.Authority
	}
	item.Attention = frame.Attention
	item.AttentionID = frame.AttentionID
	item.AttentionSource = frame.AttentionSource
	item.SinceMS = frame.SinceMS
	item.Progress = frame.Progress
	item.ProgressTool = frame.ProgressTool
	item.AskID, item.AskText, item.AskAgent = frame.Owner.AskID, frame.Owner.AskText, frame.Owner.AskAgent
	item.AskCount, item.DraftCount, item.DraftText = frame.Owner.AskCount, frame.Owner.DraftCount, frame.Owner.DraftText
	item.AskState = frame.Owner.State
	return item
}

// decorateSessionStatusWith folds every presence item the sampler produced, so a
// full sampler pass and an event-driven replace carry identical frames.
// (Recorded deviation: the plan said "only changed sessions"; the sampler
// rebuilds the whole snapshot, so every item is folded — bounded by the rail
// cap, and identical to the event path by construction.)
// Every item is folded against one owner-attention read for the whole pass
// (escalation-delivery plan RT-12).
func decorateSessionStatusWith(now time.Time, items []sessionactivity.Item, batch *ownerAttentionBatch) []sessionactivity.Item {
	for index := range items {
		item := items[index]
		frame, err := foldSessionStatusWith(now, item.Runtime, item.CatalogSessionID, item.NativeSessionID, batch)
		if err != nil {
			logSessionStatusFailure(item.Runtime, item.NativeSessionID, err)
		}
		items[index] = applySessionStatus(item, frame)
	}
	return items
}

var (
	sessionStatusLoggedMu sync.Mutex
	sessionStatusLogged   = map[string]bool{}
)

// logSessionStatusFailure reports an owner failure once per session key, so a
// broken store is visible in the log without flooding it every tick.
// It reports whether this call was the first for the key.
func logSessionStatusFailure(runtime, nativeID string, err error) bool {
	key := runtime + "\x00" + nativeID
	sessionStatusLoggedMu.Lock()
	seen := sessionStatusLogged[key]
	sessionStatusLogged[key] = true
	sessionStatusLoggedMu.Unlock()
	if !seen {
		log.Printf("session status for %s %s unavailable — rendering unknown: %v", runtime, nativeID, err)
	}
	return !seen
}

// Event-driven refold, coalesced per session so a burst of hook rows costs one
// fold. The coalesce window is configuration.
var (
	sessionStatusPendingMu sync.Mutex
	sessionStatusPending   = map[string]*time.Timer{}
)

func publishSessionStatus(runtime, nativeID string) {
	// Capture the service now: the timer fires later, and the global may have
	// been cleared by shutdown in between. The closure never touches the global.
	service := sessionActivityService()
	if service == nil || runtime == "" || nativeID == "" {
		return
	}
	key := runtime + "\x00" + nativeID
	delay := time.Duration(sessionStreamConfig().CoalesceMS) * time.Millisecond
	sessionStatusPendingMu.Lock()
	if timer, ok := sessionStatusPending[key]; ok {
		timer.Reset(delay)
		sessionStatusPendingMu.Unlock()
		return
	}
	sessionStatusPending[key] = time.AfterFunc(delay, func() {
		sessionStatusPendingMu.Lock()
		delete(sessionStatusPending, key)
		sessionStatusPendingMu.Unlock()
		refoldSessionStatusNow(service, runtime, nativeID)
	})
	sessionStatusPendingMu.Unlock()
}

// cancelPendingSessionStatus stops every coalesced refold; called on shutdown.
func cancelPendingSessionStatus() {
	sessionStatusPendingMu.Lock()
	for key, timer := range sessionStatusPending {
		timer.Stop()
		delete(sessionStatusPending, key)
	}
	sessionStatusPendingMu.Unlock()
}

func refoldSessionStatusNow(service *sessionactivity.Service, runtime, nativeID string) {
	now := time.Now().UTC()
	snapshot := service.Snapshot()
	var item *sessionactivity.Item
	for index := range snapshot.Items {
		candidate := &snapshot.Items[index]
		if candidate.Runtime == runtime && (candidate.NativeSessionID == nativeID || candidate.CatalogSessionID == nativeID) {
			item = candidate
			break
		}
	}
	if item == nil {
		// No presence lane holds this session; the hook row itself is the
		// evidence that it exists and acted. Presence stays honestly unknown.
		catalogID := resolveCatalogSessionID(runtime, nativeID)
		if catalogID == "" {
			catalogID = nativeID
		}
		fresh := sessionactivity.Item{Runtime: runtime, CatalogSessionID: catalogID, NativeSessionID: nativeID,
			Presence: "unknown", Execution: "unknown", Evidence: "native_protocol", Freshness: "live",
			Authority: "observed", ObservedAt: now, ExpiresAt: now.Add(sessionStreamConfig().Quiet())}
		item = &fresh
	}
	// Fold the item's OWN native identity — the id this refold was published
	// under may be a catalog id (approvals carry either), and the hooks' and
	// task service's rows are keyed by the native one.
	foldID := item.NativeSessionID
	if foldID == "" {
		foldID = nativeID
	}
	frame, err := foldSessionStatus(now, runtime, item.CatalogSessionID, foldID)
	if err != nil {
		logSessionStatusFailure(runtime, foldID, err)
	}
	service.Replace(applySessionStatus(*item, frame))
}
