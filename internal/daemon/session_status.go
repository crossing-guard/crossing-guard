package daemon

// The session status decider — the ONE place that answers "what is this
// session doing, and does it need the reader?" for every row the console
// shows, whoever launched the session and whichever provider it runs on.
//
// It is a pure function over facts already reduced to the framework's own
// vocabulary. It never sees a provider's event name, never reads an adapter
// declaration, and never infers a state from transcript shape: every value it
// emits is backed by a dated row somebody observed. Where nothing was
// observed, the answer is `unknown` — a legitimate, speakable state.
//
// Two ladders, because they answer two questions. Execution: working, waiting,
// idle, unknown. Attention: what, if anything, the reader has not yet seen.
// They are independent — a session can be idle AND hold an unread hand-back.

import (
	"sort"
	"time"
)

// sessionStatusFact is one dated boundary fact. AtMS is the daemon clock in
// milliseconds — the only comparator (Stop and SessionEnd from one process
// land in the same second; vendor clocks are never compared with ours).
type sessionStatusFact struct {
	Kind  string // session.ended | input.requested | turn.ended | turn.started | action.observed
	AtMS  int64
	RowID int64 // monotonic within its table; the attention id for turn facts
}

// sessionStatusOwned is the newest task Crossing Guard itself launched for
// the session — the only fully observed "working": we hold the process.
type sessionStatusOwned struct {
	Lifecycle        TaskLifecycle
	UpdatedAtMS      int64
	LastEventID      int64
	HasVisibleOutput bool
}

type sessionStatusInputs struct {
	Now              time.Time
	Quiet            time.Duration
	PendingApprovals int
	Owned            *sessionStatusOwned
	Facts            []sessionStatusFact
}

// sessionStatusFrame is what the decider publishes. The browser renders it and
// applies its own "seen" ledger; it computes nothing else.
type sessionStatusFrame struct {
	Execution       string
	Authority       string
	Attention       string
	AttentionID     int64
	AttentionSource string
	SinceMS         int64
	// Progress refines a running Execution for the OPEN session only (turn
	// progress, turn_progress.go). The rail never carries it.
	Progress     string
	ProgressTool string
}

// Equal instants resolve by this precedence: a recorded end outranks a
// question, which outranks a hand-back, which outranks a start.
var sessionFactPrecedence = map[string]int{
	"session.ended": 5, "input.requested": 4, "turn.ended": 3, "turn.started": 2, "action.observed": 1,
}

func sortSessionFacts(facts []sessionStatusFact) []sessionStatusFact {
	sorted := append([]sessionStatusFact(nil), facts...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].AtMS != sorted[j].AtMS {
			return sorted[i].AtMS > sorted[j].AtMS
		}
		if sessionFactPrecedence[sorted[i].Kind] != sessionFactPrecedence[sorted[j].Kind] {
			return sessionFactPrecedence[sorted[i].Kind] > sessionFactPrecedence[sorted[j].Kind]
		}
		return sorted[i].RowID > sorted[j].RowID
	})
	return sorted
}

func isTerminalLifecycle(lifecycle TaskLifecycle) bool {
	switch lifecycle {
	case TaskCompleted, TaskFailed, TaskInterrupted:
		return true
	}
	return false
}

func isActiveLifecycle(lifecycle TaskLifecycle) bool {
	switch lifecycle {
	case TaskQueued, TaskStarting, TaskRunning:
		return true
	}
	return false
}

// decideSessionStatus applies both ladders. Read top to bottom: that is the
// specification, and the table test mirrors it row for row.
func decideSessionStatus(in sessionStatusInputs) sessionStatusFrame {
	nowMS := in.Now.UnixMilli()
	quietMS := in.Quiet.Milliseconds()
	facts := sortSessionFacts(in.Facts)
	var newest *sessionStatusFact
	if len(facts) > 0 {
		newest = &facts[0]
	}
	age := func(atMS int64) int64 { return nowMS - atMS }

	frame := sessionStatusFrame{Execution: "unknown", Authority: "none", Attention: "none"}

	// ---- execution ladder ----
	switch {
	case in.Owned != nil && isActiveLifecycle(in.Owned.Lifecycle):
		// 1. We launched it and it has not finished.
		frame.Execution, frame.Authority, frame.SinceMS = string(in.Owned.Lifecycle), "owned", in.Owned.UpdatedAtMS
	case in.Owned != nil && isTerminalLifecycle(in.Owned.Lifecycle) && (newest == nil || in.Owned.UpdatedAtMS > newest.AtMS):
		// 2. Our task ended (completed / failed / interrupted) and nothing has
		//    happened since. A task whose state we LOST (unknown) is not an
		//    owned fact — it falls through to whatever was observed.
		frame.Execution, frame.Authority, frame.SinceMS = "terminal", "owned", in.Owned.UpdatedAtMS
	case newest == nil:
		// 8. Nothing observed at all.
	case newest.Kind == "session.ended":
		// 3. The runtime said the session ended, and that is the last word.
		frame.Execution, frame.Authority, frame.SinceMS = "idle", "observed", newest.AtMS
	case newest.Kind == "input.requested" && age(newest.AtMS) <= quietMS:
		// 4. Blocked asking the human.
		frame.Execution, frame.Authority, frame.SinceMS = "waiting", "observed", newest.AtMS
	case newest.Kind == "turn.ended":
		// 5. Handed back. Does not decay: "handed back at 14:02" stays true.
		frame.Execution, frame.Authority, frame.SinceMS = "waiting", "observed", newest.AtMS
	case (newest.Kind == "turn.started" || newest.Kind == "action.observed") && age(newest.AtMS) <= quietMS:
		// 6. A turn began or a tool ran, recently.
		frame.Execution, frame.Authority, frame.SinceMS = "running", "observed", newest.AtMS
	default:
		// 7. A start, a tool, or a question with nothing after it past the
		//    quiet window: silence reported as silence, never as completion.
		frame.Execution, frame.Authority, frame.SinceMS = "unknown", "observed", newest.AtMS
	}

	// ---- attention ladder ----
	// Live asks first, by priority: they are states, not unread markers.
	if in.PendingApprovals > 0 {
		frame.Attention, frame.AttentionSource = "approval", "task"
		return frame
	}
	if newest != nil && newest.Kind == "input.requested" && age(newest.AtMS) <= quietMS {
		frame.Attention, frame.AttentionSource, frame.AttentionID = "approval", "turn", newest.RowID
		return frame
	}
	// Then unread markers. The decider cannot know what the reader has
	// acknowledged, so it reports the NEWEST marker — an old, long-seen failure
	// must not outrank a fresh hand-back (that would be today's presence bug in
	// a new place). The browser's ledger decides whether it is still unread.
	type marker struct {
		attention, source string
		id, atMS          int64
	}
	var best *marker
	consider := func(m marker) {
		if best == nil || m.atMS > best.atMS {
			best = &m
		}
	}
	if in.Owned != nil {
		switch in.Owned.Lifecycle {
		case TaskFailed:
			consider(marker{"new_failure", "task", in.Owned.LastEventID, in.Owned.UpdatedAtMS})
		case TaskInterrupted:
			consider(marker{"interrupted", "task", in.Owned.LastEventID, in.Owned.UpdatedAtMS})
		case TaskCompleted:
			if in.Owned.HasVisibleOutput {
				consider(marker{"new_result", "task", in.Owned.LastEventID, in.Owned.UpdatedAtMS})
			}
		}
	}
	for _, fact := range facts {
		if fact.Kind == "turn.ended" {
			consider(marker{"new_result", "turn", fact.RowID, fact.AtMS})
			break // facts are newest-first; the first hand-back is the newest
		}
	}
	if best != nil {
		frame.Attention, frame.AttentionSource, frame.AttentionID = best.attention, best.source, best.id
	}
	return frame
}
