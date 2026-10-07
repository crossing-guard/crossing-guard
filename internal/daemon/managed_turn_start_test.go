package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"crossing-guard/store"
)

// A profile selecting the three session kinds that also reads the source
// transcript, so the run's coverage shows whether it had a session to read.
func transcriptFollowerProfileSource() []byte {
	return []byte(strings.Replace(string(sessionKindsFollowerProfileSource()),
		"  - kind: session.tags\n", "  - kind: session.tags\n  - kind: session.messages\n", 1))
}

func turnStartRuns(runs []store.ManagedRun) []store.ManagedRun {
	out := []store.ManagedRun{}
	for _, run := range runs {
		if signal, _ := run.Detail["signal"].(string); signal == "session.turn-started" {
			out = append(out, run)
		}
	}
	return out
}

func sessionMessagesCoverage(run store.ManagedRun) map[string]any {
	coverage, _ := run.Detail["context_coverage"].([]any)
	for _, item := range coverage {
		if entry, _ := item.(map[string]any); entry != nil && entry["kind"] == "session.messages" {
			return entry
		}
	}
	return nil
}

// A managed turn starts at the task's first session frame (managed turn start
// identity plan D1), not at spawn where a new session has no identity:
//   - the new session's run carries the frame's id as its group root and reads
//     that session's transcript;
//   - a runtime that repeats the frame every step (OpenCode) fires once;
//   - a resume of the same session joins the same group;
//   - a task that never reports a session fires no turn start;
//   - a resume answered with another session keeps the id the task addressed,
//     like every other session kind of that task;
//   - replaying the frame admits nothing new.
func TestManagedTurnStartFiresAtFirstSessionFrameWithIdentity(t *testing.T) {
	defer swapOrchestrationConfig(settledConfig())()
	const sessionID = "7c2f0a9e-1b3d-4e5f-8a6b-0c1d2e3f4a5b"
	claim := `{"action":"no_action","message":"Noted.","citations":[]}`
	driver := &helperSessionFixtureDriver{}
	driver.command = func(request ChatRequest) string {
		if strings.Contains(request.Prompt, "Crossing Guard") {
			return helperTurnCommand("", claim, "")
		}
		frame := `'{"session_id":"` + sessionID + `"}' `
		switch request.Prompt {
		case "no session":
			return jsonTextCommand("The runtime failed before its session.")
		case "forked resume":
			frame = `'{"session_id":"ses-forked"}' `
		}
		// Two frames, as OpenCode reports one per step, then the answer.
		return "printf '%s\\n' " + frame + frame + `'{"text":"Source turn text"}'`
	}
	root := t.TempDir()
	fixture := newAgentHostFixtureAt(t, driver, root)
	chatDrivers["claude"] = driver
	transcript := filepath.Join(root, ".claude", "projects", "-source", sessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, []byte(`{"type":"user","uuid":"u1","sessionId":"`+sessionID+
		`","timestamp":"2026-09-25T15:00:00Z","message":{"role":"user","content":"Source prompt"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	preview := selectManagedProfile(t, fixture.owner, transcriptFollowerProfileSource())
	binding, err := fixture.host.putBinding(managedBindingCommand{BindingID: "agent-turn-start", ProfileID: preview.ProfileID,
		ProfileSourceDigest: preview.SourceDigest, ProfileBundleDigest: preview.BundleDigest, ProjectRoot: fixture.root,
		RouteID: testRouteID(fixture.host, "managed-fixture", "", nil), GrantedAuthority: []string{}, ExpectedStateToken: store.ManagedBindingAbsentToken("agent-turn-start")})
	if err != nil {
		t.Fatal(err)
	}
	turnEnds := func(want int) []store.ManagedRun {
		t.Helper()
		// turn-ended routes after every frame of its task, so once it is
		// admitted the task's turn start has been decided.
		waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
			ended := 0
			for _, run := range runs {
				if signal, _ := run.Detail["signal"].(string); run.BindingID == binding.BindingID && signal == "session.turn-ended" {
					ended++
				}
			}
			return ended >= want
		})
		return runsFor(t, fixture, binding.BindingID)
	}
	// The helper session takes one turn at a time and coalesces the rest into
	// one pending slot, where a later signal replaces an earlier one. Start
	// the next source task only once no helper turn is running and no signal
	// waits, so every turn start is admitted as its own run.
	settle := func() {
		t.Helper()
		runs := waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
			for _, run := range runs {
				if run.BindingID == binding.BindingID && (run.State == "admitted" || run.State == "running" || run.State == "parked") {
					return false
				}
			}
			return true
		})
		for _, run := range runs {
			if run.BindingID == binding.BindingID {
				waitForGroup(t, fixture, run.GroupID, func(group store.ManagedGroup) bool {
					occupying, _ := fixture.host.ix.OccupyingManagedRuns(group.GroupID)
					return group.PendingEventID == 0 && occupying == 0
				})
			}
		}
	}

	source, _, err := fixture.tasks.Create(ChatRequest{Runtime: "claude", Prompt: "new session", Cwd: fixture.root}, "turn-start-new")
	if err != nil {
		t.Fatal(err)
	}
	starts := turnStartRuns(turnEnds(1))
	if len(starts) != 1 {
		t.Fatalf("one turn start per task, repeated frames included: %+v", starts)
	}
	first := starts[0]
	events, err := fixture.tasks.Events(source.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	var frameEvent TaskEvent
	for _, event := range events {
		if sessionFrameID(event) != "" {
			frameEvent = event
			break
		}
	}
	if frameEvent.EventID == 0 || first.SourceEventID != frameEvent.EventID {
		t.Fatalf("the turn starts at the first session frame %d, got event %d", frameEvent.EventID, first.SourceEventID)
	}
	// The decision itself, not the admissions: a persistent helper's
	// coalesce slot would absorb a second turn start, so count answers.
	firsts := 0
	for _, event := range events {
		isFirst, err := fixture.host.firstSessionFrame(event)
		if err != nil {
			t.Fatal(err)
		}
		if isFirst {
			firsts++
			if event.EventID != frameEvent.EventID {
				t.Fatalf("only the first frame starts the turn, also event %d", event.EventID)
			}
		}
	}
	if firsts != 1 {
		t.Fatalf("exactly one event starts the turn, got %d", firsts)
	}
	group, found, err := fixture.host.ix.ManagedGroup(first.GroupID)
	if err != nil || !found || group.RootNativeSessionID != sessionID || group.RootRuntime != "claude" {
		t.Fatalf("the group root must be the reported session: %+v %v %v", group, found, err)
	}
	if coverage := sessionMessagesCoverage(first); coverage == nil || coverage["state"] != "supplied" {
		t.Fatalf("session.messages must read the reported session's transcript: %+v", coverage)
	}

	settle()
	if _, _, err := fixture.tasks.Create(ChatRequest{Runtime: "claude", Prompt: "resume", SessionID: sessionID, Cwd: fixture.root}, "turn-start-resume"); err != nil {
		t.Fatal(err)
	}
	starts = turnStartRuns(turnEnds(2))
	if len(starts) != 2 || starts[0].GroupID != starts[1].GroupID {
		t.Fatalf("a resumed turn of the same session joins its group: %+v", starts)
	}

	settle()
	if _, _, err := fixture.tasks.Create(ChatRequest{Runtime: "claude", Prompt: "no session", Cwd: fixture.root}, "turn-start-none"); err != nil {
		t.Fatal(err)
	}
	if starts = turnStartRuns(turnEnds(3)); len(starts) != 2 {
		t.Fatalf("a task that never reports a session has no turn start: %+v", starts)
	}

	settle()
	forked, _, err := fixture.tasks.Create(ChatRequest{Runtime: "claude", Prompt: "forked resume", SessionID: sessionID, Cwd: fixture.root}, "turn-start-fork")
	if err != nil {
		t.Fatal(err)
	}
	starts = turnStartRuns(turnEnds(4))
	var forkStart store.ManagedRun
	for _, run := range starts {
		if run.SourceTaskID == forked.ID {
			forkStart = run
		}
	}
	if len(starts) != 3 || forkStart.GroupID != first.GroupID {
		t.Fatalf("a forked resume fires once and keeps the addressed session's group: %+v", starts)
	}
	forkedTask, _, err := fixture.tasks.Task(forked.ID)
	if err != nil {
		t.Fatal(err)
	}
	forkEvents, err := fixture.tasks.Events(forked.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	recorded := false
	for _, event := range forkEvents {
		recorded = recorded || event.Kind == "session.forked"
	}
	if !recorded || forkedTask.NativeSessionID != sessionID {
		t.Fatalf("the fork is recorded and the task keeps the id it addressed: forked=%v id=%q", recorded, forkedTask.NativeSessionID)
	}

	task, _, err := fixture.tasks.Task(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	frameEvent.Producer = managedTaskStreamKind
	if err := fixture.host.routeSignal(task, frameEvent); err != nil {
		t.Fatal(err)
	}
	if again := turnStartRuns(runsFor(t, fixture, binding.BindingID)); len(again) != 3 {
		t.Fatalf("a replayed frame admits nothing new: %+v", again)
	}
}
