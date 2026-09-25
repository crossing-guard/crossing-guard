package daemon

import (
	"os"
	"strings"
	"testing"
	"time"

	"crossing-guard/harvest"
)

func stamp(base time.Time, offset time.Duration) string {
	return base.Add(offset).UTC().Format(time.RFC3339Nano)
}

func TestTurnProgressRules(t *testing.T) {
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	now := base.Add(20 * time.Second)
	quiet := 5 * time.Minute
	ev := func(kind, name string, offset time.Duration) harvest.CanonicalEvent {
		return harvest.CanonicalEvent{Kind: kind, Name: name, Ts: stamp(base, offset)}
	}
	cases := []struct {
		name      string
		execution string
		attention string
		events    []harvest.CanonicalEvent
		want      turnProgress
	}{
		{"prompt only → thinking", "running", "none",
			[]harvest.CanonicalEvent{ev("user", "", 0)}, turnProgress{Progress: "thinking"}},
		{"thinking landed → writing", "running", "none",
			[]harvest.CanonicalEvent{ev("user", "", 0), ev("thinking", "", 12*time.Second)}, turnProgress{Progress: "writing"}},
		{"text landed → writing", "running", "none",
			[]harvest.CanonicalEvent{ev("user", "", 0), ev("assistant", "", 13*time.Second)}, turnProgress{Progress: "writing"}},
		{"tool call open → running that tool", "running", "none",
			[]harvest.CanonicalEvent{ev("user", "", 0), ev("tool_call", "Bash", 14*time.Second)}, turnProgress{Progress: "tool", Tool: "Bash"}},
		{"tool result → thinking", "running", "none",
			[]harvest.CanonicalEvent{ev("user", "", 0), ev("tool_call", "Bash", 14*time.Second), ev("tool_result", "", 15*time.Second)}, turnProgress{Progress: "thinking"}},
		{"parallel calls: oldest outstanding names the tool", "running", "none",
			[]harvest.CanonicalEvent{ev("user", "", 0), ev("tool_call", "Grep", 14*time.Second), ev("tool_call", "Read", 14*time.Second), ev("tool_result", "", 15*time.Second)},
			turnProgress{Progress: "tool", Tool: "Read"}},
		{"not running → absent", "waiting", "none",
			[]harvest.CanonicalEvent{ev("user", "", 0)}, turnProgress{}},
		{"approval pending → absent", "running", "approval",
			[]harvest.CanonicalEvent{ev("user", "", 0), ev("tool_call", "Bash", 14*time.Second)}, turnProgress{}},
		{"older than quiet → absent", "running", "none",
			[]harvest.CanonicalEvent{ev("user", "", -10*time.Minute)}, turnProgress{}},
		{"compaction resets the turn", "running", "none",
			[]harvest.CanonicalEvent{ev("user", "", 0), ev("tool_call", "Bash", 1*time.Second), ev("summary", "", 2*time.Second)}, turnProgress{}},
		{"a new prompt closes the previous turn's open call", "running", "none",
			[]harvest.CanonicalEvent{ev("user", "", 0), ev("tool_call", "Bash", 1*time.Second), ev("user", "", 3*time.Second)}, turnProgress{Progress: "thinking"}},
		{"no events → absent", "running", "none", nil, turnProgress{}},
		{"no clock at all → absent, never a claim", "running", "none",
			[]harvest.CanonicalEvent{{Kind: "user"}, {Kind: "tool_call", Name: "Bash"}}, turnProgress{}},
		{"unstamped newest, fresh stamped earlier → still decided", "running", "none",
			[]harvest.CanonicalEvent{ev("user", "", 0), {Kind: "assistant"}}, turnProgress{Progress: "writing"}},
	}
	for _, tc := range cases {
		got := decideTurnProgress(tc.execution, tc.attention, turnBoundaries(tc.events, 64), now, quiet)
		if got != tc.want {
			t.Errorf("%s: got %+v want %+v", tc.name, got, tc.want)
		}
	}
}

func TestTurnProgressWindowIsHonoured(t *testing.T) {
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	events := []harvest.CanonicalEvent{
		{Kind: "user", Ts: stamp(base, 0)},
		{Kind: "tool_call", Name: "Bash", Ts: stamp(base, time.Second)},
		{Kind: "assistant", Ts: stamp(base, 2*time.Second)},
	}
	// A window of 1 sees only the assistant block: no prompt, no open call.
	got := decideTurnProgress("running", "none", turnBoundaries(events, 1), base.Add(3*time.Second), time.Minute)
	if got.Progress != "writing" {
		t.Fatalf("window must bound what is read: %+v", got)
	}
}

func TestThoughtDurationsAttachWhereTheThoughtSat(t *testing.T) {
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	events := []harvest.CanonicalEvent{
		{Seq: 1, Kind: "user", Ts: stamp(base, 0)},
		{Seq: 2, Kind: "thinking", Ts: stamp(base, 12*time.Second)}, // signature-only: no text
		{Seq: 3, Kind: "assistant", Ts: stamp(base, 13*time.Second)},
		{Seq: 4, Kind: "tool_result", Ts: stamp(base, 14*time.Second)},
		{Seq: 5, Kind: "thinking", Text: "checked the primes", Ts: stamp(base, 20*time.Second)}, // vendor summary
		{Seq: 6, Kind: "assistant", Ts: stamp(base, 21*time.Second)},
		{Seq: 7, Kind: "thinking", Ts: stamp(base, 21*time.Second+300*time.Millisecond)},
		{Seq: 8, Kind: "assistant", Ts: stamp(base, 22*time.Second)},
	}
	out := annotateThoughts(events, 1000)
	if out[2].ThoughtMS != 12000 {
		t.Fatalf("an empty thought's duration must land on the next content block: %+v", out[2])
	}
	if out[4].ThoughtMS != 6000 {
		t.Fatalf("a thought with text carries its own duration: %+v", out[4])
	}
	if out[7].ThoughtMS != 0 {
		t.Fatalf("a thought under the minimum must not render as 0s: %+v", out[7])
	}
	if out[1].ThoughtMS != 0 || out[0].ThoughtMS != 0 {
		t.Fatal("durations must not appear on the prompt or the empty thought itself")
	}
}

// An interrupted thought (no content block followed it) must not be charged
// to the next turn's first reply.
func TestThoughtDurationNeverCrossesATurn(t *testing.T) {
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	events := []harvest.CanonicalEvent{
		{Seq: 1, Kind: "user", Ts: stamp(base, 0)},
		{Seq: 2, Kind: "thinking", Ts: stamp(base, 12*time.Second)}, // then Esc
		{Seq: 3, Kind: "user", Ts: stamp(base, 30*time.Second)},
		{Seq: 4, Kind: "assistant", Ts: stamp(base, 31*time.Second)},
	}
	out := annotateThoughts(events, 1000)
	if out[3].ThoughtMS != 0 {
		t.Fatalf("the next turn's reply inherited a stale thought: %+v", out[3])
	}
}

// The concern stores nothing and reaches nothing that stores. Pinned by
// reading the file: an import or identifier that reaches persistence fails.
func TestTurnProgressReachesNoPersistence(t *testing.T) {
	body, err := os.ReadFile("turn_progress.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"crossing-guard/store", "governor", "runtimeTasks", "nativeSessionActivity", "sql.", "AppendSessionTurn"} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("turn progress must stay transient; found %q", forbidden)
		}
	}
	for _, vendor := range []string{"claude", "codex", "opencode"} {
		if strings.Contains(strings.ToLower(string(body)), vendor) {
			t.Fatalf("vendor name %q in the progress rules", vendor)
		}
	}
}
