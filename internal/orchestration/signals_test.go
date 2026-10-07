package orchestration

import "testing"

// The session-scoped kinds (helper-session-attachment plan D1) are published
// beside the task-stream kinds; a superseded task kind names its successor so
// a wildcard fires once per fact, and the task-stream translation maps both.
func TestSessionScopedKindsPublishedAndSuperseding(t *testing.T) {
	for _, kind := range []string{"session.turn-started", "session.tool-completed", "session.turn-ended"} {
		if !KnownSignal(kind) {
			t.Fatalf("%s not published", kind)
		}
	}
	if !TerminalSignal("session.turn-ended") || TerminalSignal("session.tool-completed") || TerminalSignal("session.ended") {
		t.Fatal("terminality: turn-ended is terminal; tool-completed and session.ended are not")
	}
	if SupersededSignal("task.completed") != "session.turn-ended" || SupersededSignal("task.tool-completed") != "session.tool-completed" {
		t.Fatal("task.completed and task.tool-completed must name their session successors")
	}
	if SupersededSignal("task.failed") != "" || SupersededSignal("task.message-completed") != "" || SupersededSignal("session.turn-ended") != "" {
		t.Fatal("only kinds a session kind reports for every session are superseded")
	}
	// Spawn is not a turn start: a new session has no identity there. The
	// task's first session frame is (the caller decides which event that is).
	cases := map[string]string{"task.started": "", "tool.completed": "session.tool-completed",
		"task.completed": "session.turn-ended", "completed": "session.turn-ended",
		"message.completed": "", "task.failed": "", "task.activity": ""}
	for event, want := range cases {
		if got := SessionSignalForTaskEvent(event, false); got != want {
			t.Fatalf("SessionSignalForTaskEvent(%q)=%q want %q", event, got, want)
		}
	}
	if got := SessionSignalForTaskEvent("task.activity", true); got != "session.turn-started" {
		t.Fatalf("the first session frame must start the turn, got %q", got)
	}
	// The task-stream translation is unchanged: a task fact stays a task fact.
	if SignalForTaskEvent("tool.completed") != "task.tool-completed" || SignalForTaskEvent("completed") != "task.completed" {
		t.Fatal("task-stream translation regressed")
	}
}
