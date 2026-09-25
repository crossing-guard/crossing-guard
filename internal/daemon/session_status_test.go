package daemon

import (
	"testing"
	"time"
)

func statusNow() time.Time { return time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC) }

func fact(kind string, ago time.Duration, rowID int64) sessionStatusFact {
	return sessionStatusFact{Kind: kind, AtMS: statusNow().Add(-ago).UnixMilli(), RowID: rowID}
}

func decide(facts []sessionStatusFact, owned *sessionStatusOwned, approvals int) sessionStatusFrame {
	return decideSessionStatus(sessionStatusInputs{Now: statusNow(), Quiet: 5 * time.Minute,
		PendingApprovals: approvals, Owned: owned, Facts: facts})
}

// The five traces the rev-2 red-team ran by hand, now pinned. Each is a real
// journey on this machine, not an edge case.
func TestSessionStatusTraces(t *testing.T) {
	t.Run("turn.started → Stop → SessionEnd: idle, unread hand-back kept", func(t *testing.T) {
		frame := decide([]sessionStatusFact{
			fact("turn.started", 3*time.Minute, 1), fact("turn.ended", 2*time.Minute, 2),
			fact("session.ended", time.Minute, 0),
		}, nil, 0)
		if frame.Execution != "idle" {
			t.Fatalf("execution = %q, want idle (session end outranks the hand-back)", frame.Execution)
		}
		if frame.Attention != "new_result" || frame.AttentionSource != "turn" || frame.AttentionID != 2 {
			t.Fatalf("the unread hand-back must survive the session ending: %+v", frame)
		}
	})
	t.Run("Notification → answered → tool runs: amber clears into working", func(t *testing.T) {
		blocked := decide([]sessionStatusFact{fact("turn.started", 2*time.Minute, 1), fact("input.requested", time.Minute, 2)}, nil, 0)
		if blocked.Execution != "waiting" || blocked.Attention != "approval" || blocked.AttentionSource != "turn" {
			t.Fatalf("blocked on a prompt must read waiting/approval: %+v", blocked)
		}
		answered := decide([]sessionStatusFact{fact("turn.started", 2*time.Minute, 1), fact("input.requested", time.Minute, 2),
			fact("action.observed", 10*time.Second, 0)}, nil, 0)
		if answered.Execution != "running" || answered.Attention != "none" {
			t.Fatalf("the tool running after the answer must clear the ask: %+v", answered)
		}
	})
	t.Run("long tool-less reply: running, then silence, then handed back", func(t *testing.T) {
		if f := decide([]sessionStatusFact{fact("turn.started", 2*time.Minute, 1)}, nil, 0); f.Execution != "running" {
			t.Fatalf("2m after start = %q, want running", f.Execution)
		}
		if f := decide([]sessionStatusFact{fact("turn.started", 8*time.Minute, 1)}, nil, 0); f.Execution != "unknown" || f.SinceMS == 0 {
			t.Fatalf("8m of silence = %+v, want unknown with age", f)
		}
		if f := decide([]sessionStatusFact{fact("turn.started", 8*time.Minute, 1), fact("turn.ended", time.Second, 2)}, nil, 0); f.Execution != "waiting" {
			t.Fatalf("Stop after silence = %q, want waiting", f.Execution)
		}
	})
	t.Run("owned task running while a native Stop lands: we hold the process", func(t *testing.T) {
		owned := &sessionStatusOwned{Lifecycle: TaskRunning, UpdatedAtMS: statusNow().Add(-time.Minute).UnixMilli()}
		if f := decide([]sessionStatusFact{fact("turn.ended", time.Second, 9)}, owned, 0); f.Execution != "running" || f.Authority != "owned" {
			t.Fatalf("%+v", f)
		}
	})
	t.Run("pre-hook session with only tool calls: pulses, never blue", func(t *testing.T) {
		f := decide([]sessionStatusFact{fact("action.observed", 30*time.Second, 0)}, nil, 0)
		if f.Execution != "running" || f.Attention != "none" {
			t.Fatalf("%+v", f)
		}
	})
}

func TestSessionStatusOwnedTerminalKeepsItsAttention(t *testing.T) {
	completed := &sessionStatusOwned{Lifecycle: TaskCompleted, UpdatedAtMS: statusNow().Add(-time.Minute).UnixMilli(),
		LastEventID: 41000, HasVisibleOutput: true}
	f := decide(nil, completed, 0)
	if f.Execution != "terminal" || f.Attention != "new_result" || f.AttentionSource != "task" || f.AttentionID != 41000 {
		t.Fatalf("owned completion must keep today's blue: %+v", f)
	}
	silent := &sessionStatusOwned{Lifecycle: TaskCompleted, UpdatedAtMS: statusNow().Add(-time.Minute).UnixMilli(), LastEventID: 41001}
	if f := decide(nil, silent, 0); f.Attention != "none" {
		t.Fatalf("a completion that said nothing raises nothing: %+v", f)
	}
	failed := &sessionStatusOwned{Lifecycle: TaskFailed, UpdatedAtMS: statusNow().Add(-time.Minute).UnixMilli(), LastEventID: 7}
	if f := decide(nil, failed, 0); f.Attention != "new_failure" {
		t.Fatalf("%+v", f)
	}
}

// The decider cannot know what was acknowledged, so it must surface the
// NEWEST unread marker — an old failure must not hide a fresh hand-back.
func TestSessionStatusNewestMarkerWins(t *testing.T) {
	oldFailure := &sessionStatusOwned{Lifecycle: TaskFailed, UpdatedAtMS: statusNow().Add(-48 * time.Hour).UnixMilli(), LastEventID: 100}
	f := decide([]sessionStatusFact{fact("turn.ended", time.Minute, 500)}, oldFailure, 0)
	if f.Attention != "new_result" || f.AttentionSource != "turn" || f.AttentionID != 500 {
		t.Fatalf("a two-day-old failure outranked a fresh hand-back: %+v", f)
	}
}

func TestSessionStatusApprovalsOutrankEverything(t *testing.T) {
	f := decide([]sessionStatusFact{fact("turn.ended", time.Minute, 5)}, nil, 2)
	if f.Attention != "approval" || f.AttentionSource != "task" {
		t.Fatalf("%+v", f)
	}
}

// Equal instants resolve by declared precedence. (Stop and SessionEnd from
// one -p process land in the same SECOND; the fold's upper-bound conversion
// makes that an equal instant, and this precedence then decides it.)
func TestSessionStatusTiePrecedence(t *testing.T) {
	at := statusNow().Add(-time.Minute).UnixMilli()
	f := decide([]sessionStatusFact{
		{Kind: "turn.ended", AtMS: at, RowID: 1}, {Kind: "session.ended", AtMS: at, RowID: 0},
	}, nil, 0)
	if f.Execution != "idle" {
		t.Fatalf("tie must go to session.ended: %+v", f)
	}
}

// Totality by brute force: every kind × age × ownership lands on exactly one
// legal execution value and one legal attention value. A gap here is a blank
// header in the product.
func TestSessionStatusIsTotal(t *testing.T) {
	kinds := []string{"session.ended", "input.requested", "turn.ended", "turn.started", "action.observed"}
	ages := []time.Duration{0, time.Second, time.Minute, 4 * time.Minute, 6 * time.Minute, 48 * time.Hour}
	owneds := []*sessionStatusOwned{nil,
		{Lifecycle: TaskRunning, UpdatedAtMS: statusNow().Add(-time.Minute).UnixMilli()},
		{Lifecycle: TaskCompleted, UpdatedAtMS: statusNow().Add(-time.Minute).UnixMilli(), HasVisibleOutput: true},
		{Lifecycle: TaskFailed, UpdatedAtMS: statusNow().Add(-10 * time.Minute).UnixMilli()},
	}
	legalExecution := map[string]bool{"queued": true, "starting": true, "running": true, "terminal": true,
		"idle": true, "waiting": true, "unknown": true}
	legalAttention := map[string]bool{"none": true, "approval": true, "new_result": true, "new_failure": true, "interrupted": true}
	for _, kind := range kinds {
		for _, age := range ages {
			for _, owned := range owneds {
				for _, approvals := range []int{0, 1} {
					f := decide([]sessionStatusFact{fact(kind, age, 1)}, owned, approvals)
					if !legalExecution[f.Execution] || !legalAttention[f.Attention] {
						t.Fatalf("kind=%s age=%s owned=%v approvals=%d → %+v", kind, age, owned, approvals, f)
					}
					if (f.Execution == "running" || f.Execution == "waiting" || f.Execution == "idle") && f.Authority == "none" {
						t.Fatalf("a stated execution must carry authority: kind=%s age=%s → %+v", kind, age, f)
					}
				}
			}
		}
	}
	if f := decide(nil, nil, 0); f.Execution != "unknown" || f.Authority != "none" || f.Attention != "none" {
		t.Fatalf("nothing observed must be unknown/none: %+v", f)
	}
}

// No derived state exists: a stated running or waiting always traces to a row.
func TestSessionStatusNeverDerivesFromSilence(t *testing.T) {
	f := decide(nil, nil, 0)
	if f.Execution != "unknown" {
		t.Fatalf("with no facts the only honest answer is unknown, got %q", f.Execution)
	}
}

// A whole-second SessionEnd must not lose to a millisecond Stop from the same
// second. Measured live 2026-09-01: 910 ms apart, same second, and the fold
// read the -p session as "waiting" instead of "idle".
func TestLifecycleInstantUpperBoundBeatsSameSecondStop(t *testing.T) {
	stopMS := int64(1788304312910)
	endSeconds := int64(1788304312)
	if lifecycleInstantMS(endSeconds) < stopMS {
		t.Fatalf("end at %d must not read as earlier than a Stop at %d in the same second", lifecycleInstantMS(endSeconds), stopMS)
	}
	f := decideSessionStatus(sessionStatusInputs{Now: time.UnixMilli(stopMS + 5000), Quiet: 5 * time.Minute,
		Facts: []sessionStatusFact{{Kind: "turn.ended", AtMS: stopMS, RowID: 2},
			{Kind: "session.ended", AtMS: lifecycleInstantMS(endSeconds)}}})
	if f.Execution != "idle" || f.Attention != "new_result" {
		t.Fatalf("Stop then SessionEnd in one second must read idle with the unread hand-back kept: %+v", f)
	}
}
