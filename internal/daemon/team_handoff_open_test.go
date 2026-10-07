package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"crossing-guard/engine"
	"crossing-guard/internal/changeenv"
	"crossing-guard/internal/guardcli"
	"crossing-guard/internal/observation"
	"crossing-guard/internal/recallmcp"
	"crossing-guard/store"
	"crossing-guard/teamwire"
)

// handoffFixtureDriver stands in for a vendor CLI (criterion 63: everything here is
// tested without one). Its process prints, as one JSON line, the session it "started"
// and the ticket it found in its own environment — which is how the tests see what a
// launched process was given. An empty session prints no session frame at all.
type handoffFixtureDriver struct {
	session string
	fail    bool
	// hold keeps the process alive after it printed its session frame: the launched
	// turn is still running, so its ticket is still claimable.
	hold bool
}

func (d handoffFixtureDriver) BuildCmd(req ChatRequest, _ ChatLaunchContext) (*exec.Cmd, error) {
	script := `printf '{"session":"%s","ticket":"%s"}\n' "$FIXTURE_SESSION" "$CG_HANDOFF_TICKET"`
	if d.fail {
		script += `; echo "fixture runtime could not start" >&2; exit 3`
	}
	if d.hold {
		script += `; sleep 30`
	}
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Dir = req.Cwd
	cmd.Env = chatLaunchEnv(req)
	cmd.Env = append(cmd.Env, "FIXTURE_SESSION="+d.session)
	return cmd, nil
}

func (handoffFixtureDriver) ProjectEvent(object map[string]any) []ChatEvent {
	events := []ChatEvent{}
	if session := anyString(object["session"]); session != "" {
		events = append(events, ChatEvent{"type": "session", "id": session})
	}
	return append(events, ChatEvent{"type": "text", "text": "ticket=" + anyString(object["ticket"])})
}

// openRig is a linked device that received one handoff from a teammate, with a task
// service and a fixture runtime under each name a handoff opens in.
type openRig struct {
	*handoffRig
	tasks *TaskApplicationService
	repo  string
	doc   teamwire.HandoffRecord
}

func newOpenRig(t *testing.T) *openRig {
	t.Helper()
	t.Cleanup(swapOrchestrationConfig(defaultOrchestrationConfig()))
	t.Cleanup(swapSessionStatusRefold(func(string, string) {}))
	rig := &openRig{handoffRig: newHandoffRig(t, true), repo: v1TestRepo(t)}
	rig.tasks = installTestRuntimeTasks(t)
	rig.drivers(handoffFixtureDriver{session: "ses-launched"})
	previous := handoffOpens
	handoffOpens = &handoffOpenOwner{watched: map[string]string{}}
	t.Cleanup(func() { handoffOpens = previous })
	row, doc := pulledDocument(t, 1, "Finish the outbox drain", "the full handoff text", teamwire.HandoffSent)
	rig.fake.serve(row)
	rig.tl.pullHandoffsOnce()
	rig.doc = doc
	return rig
}

// drivers installs the fixture as both runtimes a handoff opens in, and OpenCode
// beside them so "not offered" is tested against a runtime the console can launch.
func (r *openRig) drivers(driver ChatDriver) {
	r.t.Helper()
	original := chatDrivers
	chatDrivers = map[string]ChatDriver{"claude": driver, "codex": driver, "opencode": driver}
	r.t.Cleanup(func() { chatDrivers = original })
}

// openTestObservation numbers the observations the tests post.
var openTestObservation atomic.Int64

func openTestEntry(runtime, session, repo, kind, ticket string, at int64) observation.SessionEntryEnvelope {
	source := map[string]string{"start": "startup", "context-compact": "compact", "resume": "resume"}[kind]
	return observation.SessionEntryEnvelope{Schema: observation.SessionEntrySchemaV1,
		ObservationID: "ent_" + fmt.Sprintf("%032x", openTestObservation.Add(1)),
		CollectorID:   observation.CollectorSessionEntry, Runtime: runtime, SessionID: session, HookEventName: "SessionStart",
		EntryKind: kind, NativeSource: source, TranscriptPath: repo + "/" + session + ".jsonl", Cwd: repo,
		ObservedAt: at, QueuedAt: at, DeliveryAttempts: 1, DeliveryMode: "direct", HandoffTicket: ticket}
}

// entry posts a session entry as the lifecycle hook would.
func (r *openRig) entry(runtime, session, kind, ticket string) observation.SessionEntryEnvelope {
	r.t.Helper()
	entry := openTestEntry(runtime, session, r.repo, kind, ticket, time.Now().Unix())
	if _, err := ingestSessionEntryV1(context.Background(), governor, entry); err != nil {
		r.t.Fatalf("session entry: %v", err)
	}
	return entry
}

func openTestTurn(runtime, session, repo, kind string, carrier bool) observation.SessionTurnEnvelope {
	now := time.Now().Unix()
	return observation.SessionTurnEnvelope{Schema: observation.SessionTurnSchemaV1,
		ObservationID: "trn_" + fmt.Sprintf("%032x", openTestObservation.Add(1)),
		CollectorID:   observation.CollectorSessionTurn, Runtime: runtime, SessionID: session, Kind: kind,
		NativeSource: "UserPromptSubmit", Cwd: repo, ObservedAt: now, QueuedAt: now,
		DeliveryAttempts: 1, DeliveryMode: "direct", Carrier: carrier}
}

// prompt posts the prompt event of a session through the HTTP handler — the only path
// that hands anything over — and returns what the hook would print.
func (r *openRig) prompt(runtime, session string, carrier bool) []observation.Delivery {
	r.t.Helper()
	recorder := httptest.NewRecorder()
	r.postTurn(openTestTurn(runtime, session, r.repo, handoffPromptKind, carrier), recorder)
	if recorder.Code != http.StatusOK {
		r.t.Fatalf("prompt: %d %s", recorder.Code, recorder.Body.String())
	}
	var receipt observation.SessionTurnReceipt
	if err := json.Unmarshal(recorder.Body.Bytes(), &receipt); err != nil {
		r.t.Fatalf("prompt receipt: %v", err)
	}
	return receipt.Deliveries
}

// postTurn drives one turn boundary through the HTTP handler with a live hook
// deadline. The rig's governor is already the package's: unlike postSessionTurnWith it
// does not swap that global, which the link's own push goroutine reads.
func (r *openRig) postTurn(turn observation.SessionTurnEnvelope, w http.ResponseWriter) {
	r.t.Helper()
	body, err := json.Marshal(turn)
	if err != nil {
		r.t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/govern/session-turn/v1", bytes.NewReader(body))
	request.Header.Set(observation.HookDeadlineHeader, fmt.Sprint(time.Now().Add(observation.HookDeliveryBudget).UnixMilli()))
	handleGovernSessionTurnV1(w, request)
}

// ready makes a runtime's hook observed at both events, by another session of it.
func (r *openRig) ready(runtime string) {
	r.t.Helper()
	r.entry(runtime, "ses-earlier-"+runtime, "start", "")
	r.prompt(runtime, "ses-earlier-"+runtime, true)
}

func (r *openRig) open(runtime string) teamHandoffOpenResponse {
	r.t.Helper()
	var out teamHandoffOpenResponse
	if code, refusal := r.call("POST", "/api/team/handoffs/"+r.doc.ID+"/open", teamHandoffOpenRequest{Runtime: runtime, CheckoutRoot: r.repo}, &out); code != http.StatusOK {
		r.t.Fatalf("open: %d %+v", code, refusal)
	}
	return out
}

// send posts the person's first prompt with the ticket, as the composer does.
func (r *openRig) send(runtime, ticket string) (int, RuntimeTask, runtimeTaskRefusal) {
	r.t.Helper()
	body, _ := json.Marshal(ChatRequest{Runtime: runtime, Prompt: "carry on from the handoff", HandoffTicket: ticket,
		IdempotencyKey: "open-" + ticket + fmt.Sprint(time.Now().UnixNano())})
	rec := httptest.NewRecorder()
	handleRuntimeTaskCreate(rec, httptest.NewRequest(http.MethodPost, "/api/runtime-tasks", bytes.NewReader(body)))
	var task RuntimeTask
	var refusal runtimeTaskRefusal
	if rec.Code == http.StatusOK || rec.Code == http.StatusAccepted {
		if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil {
			r.t.Fatalf("task: %v: %s", err, rec.Body.String())
		}
	} else {
		_ = json.Unmarshal(rec.Body.Bytes(), &refusal)
	}
	return rec.Code, task, refusal
}

// launch sends the first prompt and waits for the launched turn to end.
func (r *openRig) launch(runtime, ticket string, want TaskLifecycle) RuntimeTask {
	r.t.Helper()
	code, task, refusal := r.send(runtime, ticket)
	if code != http.StatusAccepted {
		r.t.Fatalf("send: %d %+v", code, refusal)
	}
	wantTaskState(r.t, r.tasks, task.ID, want, 5*time.Second)
	return task
}

func (r *openRig) taskText(taskID string) string {
	r.t.Helper()
	events, err := r.tasks.Events(taskID, 0, 100)
	if err != nil {
		r.t.Fatal(err)
	}
	raw, _ := json.Marshal(events)
	return string(raw)
}

func (r *openRig) detail() teamHandoffDetailResponse {
	r.t.Helper()
	var out teamHandoffDetailResponse
	if code, refusal := r.call("GET", "/api/team/handoffs/"+r.doc.ID, nil, &out); code != http.StatusOK {
		r.t.Fatalf("detail: %d %+v", code, refusal)
	}
	return out
}

func (r *openRig) options() teamHandoffOpenOptionsResponse {
	r.t.Helper()
	var out teamHandoffOpenOptionsResponse
	if code, refusal := r.call("GET", "/api/team/handoffs/"+r.doc.ID+"/open-options", nil, &out); code != http.StatusOK {
		r.t.Fatalf("open-options: %d %+v", code, refusal)
	}
	return out
}

func (o teamHandoffOpenOptionsResponse) runtime(name string) (teamHandoffOpenRuntime, bool) {
	for _, runtime := range o.Runtimes {
		if runtime.Runtime == name {
			return runtime, true
		}
	}
	return teamHandoffOpenRuntime{}, false
}

// chained counts the team events of one kind in the device's event chain.
func (r *openRig) chained(kind string) int {
	r.t.Helper()
	rows, err := governor.ix.EventChainRows(teamSessionID)
	if err != nil {
		r.t.Fatal(err)
	}
	n := 0
	for _, row := range rows {
		if row.Body.Tool == kind {
			n++
		}
	}
	return n
}

func (r *openRig) receipts(transition string) int {
	r.t.Helper()
	n := 0
	for _, record := range r.fake.records(teamwire.KindHandoffReceipt) {
		var receipt teamwire.HandoffReceipt
		if json.Unmarshal(record.Body, &receipt) == nil && receipt.Transition == transition {
			n++
		}
	}
	return n
}

// Criterion 63, without a vendor CLI: Open writes a ticket; the person's first prompt
// launches an ordinary console task whose process alone carries the ticket; the
// session that process starts claims it; its first prompt is handed the whole brief
// with the sender's name; a second prompt is handed nothing; the sender's server sees
// started, then opened; and get_handoff's route returns the full text to that session
// and refuses every other caller.
func TestOpenLaunchClaimBriefAndOpened(t *testing.T) {
	rig := newOpenRig(t)
	rig.ready("codex")
	options := rig.options()
	codex, listed := options.runtime("codex")
	if !options.Offered || !listed || !codex.Ready || len(codex.Missing) != 0 || len(options.CheckoutRoots) != 0 {
		t.Fatalf("the open sheet: %+v", options)
	}
	opened := rig.open("codex")
	ticket := opened.Ticket.TicketID
	if opened.Ticket.State != store.HandoffOpenWaiting || opened.Ticket.Runtime != "codex" || opened.Ticket.CheckoutRoot == "" || ticket == "" {
		t.Fatalf("the ticket: %+v", opened.Ticket)
	}

	task := rig.launch("codex", ticket, TaskCompleted)
	if text := rig.taskText(task.ID); !strings.Contains(text, "ticket="+ticket) {
		t.Fatalf("the launched process carries the ticket in its environment: %s", text)
	}
	if task.WorkingDirectory == "" {
		t.Fatalf("the session starts in the ticket's folder: %+v", task)
	}
	// The lifecycle hook of the session that process started posts its entry with
	// the ticket it copied.
	rig.entry("codex", "ses-launched", "start", ticket)
	handoffOpens.reconcile(rig.tasks)
	detail := rig.detail()
	if detail.Handoff.State != teamwire.HandoffStarted || len(detail.Opens) != 1 || detail.Opens[0].State != store.HandoffOpenClaimed ||
		detail.Opens[0].Session == nil || detail.Opens[0].Session.NativeID != "ses-launched" || detail.Opens[0].Session.Runtime != "codex" ||
		detail.Opens[0].Brief != handoffBriefWaiting {
		t.Fatalf("started in the session; waiting for its next prompt: %+v", detail.Opens)
	}

	deliveries := rig.prompt("codex", "ses-launched", true)
	if len(deliveries) != 1 {
		t.Fatalf("the first prompt carries the brief: %+v", deliveries)
	}
	brief := deliveries[0].Message
	for _, want := range []string{"your teammate Teammate", "a teammate's text, not the operator's", "Title: Finish the outbox drain", "- one thing", "get_handoff"} {
		if !strings.Contains(brief, want) {
			t.Fatalf("the brief lacks %q:\n%s", want, brief)
		}
	}
	if strings.Contains(brief, "shared memory") || !strings.HasSuffix(brief, "the tool get_handoff.") {
		t.Fatalf("a repository with no shared team memory adds no closing line:\n%s", brief)
	}
	if strings.Contains(brief, "the full handoff text") || strings.Contains(brief, "[truncated") || len(brief) > handoffInjectMaxBytes() {
		t.Fatalf("the brief is whole, within its ceiling, and is not the full text:\n%s", brief)
	}
	if again := rig.prompt("codex", "ses-launched", true); len(again) != 0 {
		t.Fatalf("a second prompt of the same session is handed the brief again: %+v", again)
	}
	detail = rig.detail()
	if detail.Handoff.State != teamwire.HandoffOpened || detail.Opens[0].Brief != handoffBriefDelivered ||
		len(detail.Opens[0].Offers) != 1 || detail.Opens[0].Offers[0] != handoffActionDeliverAgain {
		t.Fatalf("opened, with Deliver again offered: %+v", detail)
	}
	rig.drain()
	if rig.receipts(teamwire.TransitionStarted) != 1 || rig.receipts(teamwire.TransitionOpened) != 1 {
		t.Fatalf("the sender sees started, then opened: started=%d opened=%d", rig.receipts(teamwire.TransitionStarted), rig.receipts(teamwire.TransitionOpened))
	}

	// get_handoff: the full text to the claimed session, and to no other caller.
	var claimed teamHandoffClaimedResponse
	if code, refusal := rig.call("GET", "/api/team/handoffs/claimed?caller_runtime=codex&caller_id=ses-launched", nil, &claimed); code != http.StatusOK ||
		claimed.Document.BodyMarkdown != "the full handoff text" || claimed.From.DisplayName != "Teammate" || claimed.HandoffID != rig.doc.ID {
		t.Fatalf("the claimed session reads the full document: %d %+v %+v", code, refusal, claimed)
	}
	for path, wantCode := range map[string]string{
		"/api/team/handoffs/claimed?caller_runtime=codex&caller_id=ses-other":     handoffCodeNotTheOpenedCaller,
		"/api/team/handoffs/claimed?caller_runtime=claude&caller_id=ses-launched": handoffCodeNotTheOpenedCaller,
		"/api/team/handoffs/claimed?caller_runtime=codex":                         handoffCodeCallerUnidentified,
		"/api/team/handoffs/claimed":                                              handoffCodeCallerUnidentified,
	} {
		if code, refusal := rig.call("GET", path, nil, nil); code != http.StatusForbidden || refusal.Code != wantCode {
			t.Fatalf("%s: %d %+v, want %s", path, code, refusal, wantCode)
		}
	}
	// The opened session's own header: "continues <title> from <sender>".
	var session teamHandoffSessionResponse
	if code, _ := rig.call("GET", "/api/team/handoffs/session?runtime=codex&native_id=ses-launched", nil, &session); code != http.StatusOK ||
		!session.Opened || session.Title != "Finish the outbox drain" || session.From == nil || session.From.DisplayName != "Teammate" || session.HandoffID != rig.doc.ID {
		t.Fatalf("the opened session's facts: %+v", session)
	}
	if code, _ := rig.call("GET", "/api/team/handoffs/session?runtime=codex&native_id=ses-earlier-codex", nil, &session); code != http.StatusOK || session.Opened {
		t.Fatalf("a session that was not opened for a handoff: %+v", session)
	}
	// One ticket, one launch.
	if code, _, refusal := rig.send("codex", ticket); code != http.StatusConflict || refusal.Code != store.HandoffCodeTicketUsed {
		t.Fatalf("a second send with the same ticket: %d %+v", code, refusal)
	}
}

// Criteria 64 and 88: a session the person starts in a terminal, in the right folder
// and runtime, receives nothing and the handoff stays received; a follower's or a
// spawned process's entry that carries the inherited ticket is refused once the
// launched session is known; a duplicate entry changes nothing; a Codex child prompt
// under the parent's id is handed nothing; a Codex tool event never carries the
// brief; later turns receive nothing; a compaction of the claimed session re-arms it
// once.
func TestOnlyTheLaunchedSessionClaimsAndReceives(t *testing.T) {
	rig := newOpenRig(t)
	rig.ready("codex")
	ticket := rig.open("codex").Ticket.TicketID

	// No terminal pickup: no ticket in the entry, nothing claimed, nothing handed.
	rig.entry("codex", "ses-terminal", "start", "")
	if handed := rig.prompt("codex", "ses-terminal", true); len(handed) != 0 {
		t.Fatalf("a terminal session receives nothing: %+v", handed)
	}
	if state := rig.detail().Handoff.State; state != teamwire.HandoffReceived && state != teamwire.HandoffSent {
		t.Fatalf("the handoff stays received: %s", state)
	}

	rig.launch("codex", ticket, TaskCompleted)
	handoffOpens.reconcile(rig.tasks) // the launched task's session frame is known
	rig.entry("codex", "ses-spawned", "start", ticket)
	if got, _, _ := governor.ix.HandoffOpenByTicket(ticket); got.Claimed() {
		t.Fatalf("a process that inherited the ticket is refused once the launched session is known: %+v", got)
	}
	first := rig.entry("codex", "ses-launched", "start", ticket)
	first.DeliveryMode, first.DeliveryAttempts = "replay", 2
	if _, err := ingestSessionEntryV1(context.Background(), governor, first); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := governor.ix.HandoffOpenByTicket(ticket); got.ClaimedNativeID != "ses-launched" {
		t.Fatalf("the launched session holds the claim: %+v", got)
	}
	rig.drain()
	if rig.receipts(teamwire.TransitionStarted) != 1 {
		t.Fatalf("a duplicate and a replayed entry change nothing: started=%d", rig.receipts(teamwire.TransitionStarted))
	}

	// Never to a child (K-1): the hook reports a nested call as unable to carry.
	if handed := rig.prompt("codex", "ses-launched", false); len(handed) != 0 {
		t.Fatalf("a Codex child prompt under the parent's session id is handed the brief: %+v", handed)
	}
	// On Codex a tool event never carries the brief, from a child or the parent.
	for _, kind := range []string{"tool.started", "tool.completed"} {
		request := httptest.NewRequest(http.MethodPost, "/api/govern/observe/v1", nil)
		if claimed := claimForReply(governor, request, carrierBoundary{Runtime: "codex", SessionID: "ses-launched", Kind: kind,
			ObservationID: "obs_" + kind, DeliveryMode: "direct", Carrier: true}); len(claimed) != 0 {
			t.Fatalf("a Codex %s event carried the handoff row: %+v", kind, claimed)
		}
	}
	if handed := rig.prompt("codex", "ses-other", true); len(handed) != 0 {
		t.Fatalf("another session of the same runtime and folder receives nothing: %+v", handed)
	}
	if handed := rig.prompt("codex", "ses-launched", true); len(handed) != 1 {
		t.Fatalf("the parent's own prompt carries it: %+v", handed)
	}
	if handed := rig.prompt("codex", "ses-launched", true); len(handed) != 0 {
		t.Fatalf("later console turns receive nothing again: %+v", handed)
	}

	// A compaction of the claimed session re-arms it once; a new id reaches nothing.
	rig.entry("codex", "ses-cleared", "context-compact", "")
	rig.entry("codex", "ses-launched", "context-compact", "")
	if handed := rig.prompt("codex", "ses-cleared", true); len(handed) != 0 {
		t.Fatalf("a cleared or forked session is a new id: %+v", handed)
	}
	if handed := rig.prompt("codex", "ses-launched", true); len(handed) != 1 {
		t.Fatalf("a compaction re-arms the brief: %+v", handed)
	}
	if handed := rig.prompt("codex", "ses-launched", true); len(handed) != 0 {
		t.Fatalf("once: %+v", handed)
	}
	rig.drain()
	if rig.receipts(teamwire.TransitionOpened) != 1 {
		t.Fatalf("a compaction re-delivery sends no second opened: %d", rig.receipts(teamwire.TransitionOpened))
	}
}

// §6.5: on Claude a nested call is told apart at every kind that carries context, so
// a Claude tool boundary of the session itself may carry the brief; the gate is the
// installer's published kinds, not a vendor name.
func TestBriefRidesOnlyTheKindsTheInstallerPublishes(t *testing.T) {
	if !handoffCarrierKind("claude", "tool.started") || !handoffCarrierKind("claude", handoffPromptKind) {
		t.Fatal("claude publishes the tool kinds and the prompt kind")
	}
	if handoffCarrierKind("codex", "tool.started") || handoffCarrierKind("codex", "tool.completed") || !handoffCarrierKind("codex", handoffPromptKind) {
		t.Fatal("codex publishes the prompt kind only")
	}
	if handoffCarrierKind("opencode", handoffPromptKind) || handoffCarrierKind("no-such-runtime", handoffPromptKind) {
		t.Fatal("a runtime that reports no nested calls carries no brief at any kind")
	}
	for _, kinds := range [][]string{guardcli.NestedCallKinds("claude"), guardcli.NestedCallKinds("codex")} {
		for _, kind := range kinds {
			if kind != "tool.started" && kind != "tool.completed" && !observation.SessionTurnKinds[kind] {
				t.Fatalf("%q is not a kind of the framework's vocabulary", kind)
			}
		}
	}
}

// OD-7b: a handoff cannot be opened in OpenCode in this release — it is not listed and
// an open that names it is refused.
func TestOpenIsNotOfferedInARuntimeThatCannotTellANestedCall(t *testing.T) {
	rig := newOpenRig(t)
	rig.ready("opencode")
	if _, listed := rig.options().runtime("opencode"); listed {
		t.Fatal("OpenCode is not offered")
	}
	if got := handoffOpenRuntimes(); len(got) != 2 || got[0] != "claude" || got[1] != "codex" {
		t.Fatalf("the runtimes a handoff opens in: %v", got)
	}
	code, refusal := rig.call("POST", "/api/team/handoffs/"+rig.doc.ID+"/open", teamHandoffOpenRequest{Runtime: "opencode", CheckoutRoot: rig.repo}, nil)
	if code != http.StatusConflict || refusal.Code != handoffCodeRuntimeNotOff {
		t.Fatalf("an open in OpenCode: %d %+v", code, refusal)
	}
}

// Criterion 84: Open in a runtime is offered only when this device has observed, from
// that runtime, a session-entry row and a prompt row inside the window, and the prompt
// kind is a carrier kind; the sheet names exactly what is missing.
func TestReadinessIsObservedAndNamesWhatIsMissing(t *testing.T) {
	rig := newOpenRig(t)
	missing := func(runtime string) []string {
		t.Helper()
		got, listed := rig.options().runtime(runtime)
		if !listed || got.Ready != (len(got.Missing) == 0) {
			t.Fatalf("%s: %+v", runtime, got)
		}
		return got.Missing
	}
	if got := missing("claude"); len(got) != 2 || got[0] != handoffMissingSessionEntry || got[1] != handoffMissingPrompt {
		t.Fatalf("nothing observed: %v", got)
	}
	code, refusal := rig.call("POST", "/api/team/handoffs/"+rig.doc.ID+"/open", teamHandoffOpenRequest{Runtime: "claude", CheckoutRoot: rig.repo}, nil)
	if code != http.StatusConflict || refusal.Code != handoffCodeNotReady {
		t.Fatalf("an open in a runtime that is not ready: %d %+v", code, refusal)
	}
	rig.entry("claude", "ses-seen", "start", "")
	if got := missing("claude"); len(got) != 1 || got[0] != handoffMissingPrompt {
		t.Fatalf("an entry and no prompt (an untrusted hook fires neither): %v", got)
	}
	rig.prompt("claude", "ses-seen", true)
	if got := missing("claude"); len(got) != 0 {
		t.Fatalf("both observed: %v", got)
	}
	if got := missing("codex"); len(got) != 2 {
		t.Fatalf("another runtime's rows prove nothing for this one: %v", got)
	}
	// The prompt kind taken out of the carrier kinds: nothing could deliver.
	config := defaultOrchestrationConfig()
	config.Delivery.CarrierKinds = []string{"tool.started"}
	restore := swapOrchestrationConfig(config)
	if got := missing("claude"); len(got) != 1 || got[0] != handoffMissingCarrierKind {
		restore()
		t.Fatalf("the prompt kind is not a carrier kind: %v", got)
	}
	restore()
	// Outside the window, an observation no longer counts.
	if firing, err := governor.ix.HandoffRuntimeFiring("claude", handoffPromptKind, time.Now().Unix()+10); err != nil || firing.SessionEntryAt != 0 || firing.PromptAt != 0 {
		t.Fatalf("rows older than the window are not read: %+v %v", firing, err)
	}
}

// Criteria 84 and 94: a launched turn that never started a session cancels the ticket
// with the task's reason and offers Retry — the only case; one whose session started
// without a claim names the hook, offers no Retry, and clears the runtime's readiness
// until rows newer than it are seen.
func TestLaunchedTurnWithoutAClaimIsToldApartByTheSessionFrame(t *testing.T) {
	rig := newOpenRig(t)
	rig.ready("codex")

	rig.drivers(handoffFixtureDriver{fail: true})
	notStarted := rig.open("codex").Ticket.TicketID
	rig.launch("codex", notStarted, TaskFailed)
	handoffOpens.reconcile(rig.tasks)
	opens := rig.detail().Opens
	if len(opens) != 1 || opens[0].State != store.HandoffOpenCancelled || opens[0].EndedCode != store.HandoffOpenRuntimeNotStarted ||
		opens[0].EndedDetail == "" || len(opens[0].Offers) != 1 || opens[0].Offers[0] != handoffActionRetry {
		t.Fatalf("could not start the runtime, with the task's reason and Retry: %+v", opens)
	}
	if codex, _ := rig.options().runtime("codex"); !codex.Ready {
		t.Fatalf("a runtime that never started a session stays ready: %+v", codex)
	}

	rig.drivers(handoffFixtureDriver{session: "ses-no-hook"})
	noHook := rig.open("codex").Ticket.TicketID
	rig.launch("codex", noHook, TaskCompleted)
	handoffOpens.reconcile(rig.tasks)
	var silent teamHandoffOpen
	for _, open := range rig.detail().Opens {
		if open.TicketID == noHook {
			silent = open
		}
	}
	if silent.State != store.HandoffOpenCancelled || silent.EndedCode != store.HandoffOpenHookDidNotRun || len(silent.Offers) != 0 {
		t.Fatalf("a session started and the hook did not run: named, with no Retry: %+v", silent)
	}
	codex, _ := rig.options().runtime("codex")
	if codex.Ready || len(codex.Missing) != 1 || codex.Missing[0] != handoffMissingAfterLaunch || codex.UnclaimedLaunchAt == "" {
		t.Fatalf("the launched turn that made no claim clears the runtime's readiness: %+v", codex)
	}
	code, refusal := rig.call("POST", "/api/team/handoffs/"+rig.doc.ID+"/open", teamHandoffOpenRequest{Runtime: "codex", CheckoutRoot: rig.repo}, nil)
	if code != http.StatusConflict || refusal.Code != handoffCodeNotReady {
		t.Fatalf("Open is not offered until the hook is seen again: %d %+v", code, refusal)
	}
	// Rows newer than the unclaimed launch make it ready again.
	time.Sleep(1100 * time.Millisecond)
	rig.entry("codex", "ses-after-fix", "start", "")
	rig.prompt("codex", "ses-after-fix", true)
	if codex, _ := rig.options().runtime("codex"); !codex.Ready {
		t.Fatalf("newer rows restore readiness: %+v", codex)
	}
}

// Criterion 67: withdrawing after started and before opened cancels the armed brief —
// it is never delivered — and get_handoff refuses; a send in a composer whose ticket
// the withdrawal cancelled is refused with "the sender withdrew this handoff".
func TestWithdrawalAfterStartedCancelsTheBriefAndGetHandoffRefuses(t *testing.T) {
	rig := newOpenRig(t)
	rig.ready("claude")
	ticket := rig.open("claude").Ticket.TicketID
	rig.launch("claude", ticket, TaskCompleted)
	rig.entry("claude", "ses-launched", "start", ticket)
	waiting := rig.open("claude").Ticket.TicketID

	rig.fake.serve(stateOnly(2, rig.doc.ID, teamwire.HandoffWithdrawn))
	rig.tl.pullHandoffsOnce()
	if handed := rig.prompt("claude", "ses-launched", true); len(handed) != 0 {
		t.Fatalf("the armed brief is never delivered after the withdrawal: %+v", handed)
	}
	if code, refusal := rig.call("GET", "/api/team/handoffs/claimed?caller_runtime=claude&caller_id=ses-launched", nil, nil); code != http.StatusConflict || refusal.Code != teamwire.CodeHandoffWithdrawn {
		t.Fatalf("get_handoff refuses: %d %+v", code, refusal)
	}
	code, _, refusal := rig.send("claude", waiting)
	if code != http.StatusConflict || refusal.Code != teamwire.CodeHandoffWithdrawn || refusal.Message != "the sender withdrew this handoff" {
		t.Fatalf("a send in the composer: %d %+v", code, refusal)
	}
	if code, refusal := rig.call("POST", "/api/team/handoffs/"+rig.doc.ID+"/deliver-again", nil, nil); code != http.StatusConflict || refusal.Code != teamwire.CodeHandoffWithdrawn {
		t.Fatalf("deliver again on a withdrawn handoff is refused: %d %+v", code, refusal)
	}
	detail := rig.detail()
	if detail.Document != nil || detail.Handoff.OpenOffered || detail.Handoff.OpenWithheldCode != teamwire.CodeHandoffWithdrawn {
		t.Fatalf("the unopened copy is erased and Open is withheld: %+v", detail.Handoff)
	}
	for _, open := range detail.Opens {
		if open.State != store.HandoffOpenCancelled || open.EndedCode != teamwire.CodeHandoffWithdrawn || len(open.Offers) != 0 {
			t.Fatalf("every ticket is cancelled by the withdrawal and offers nothing: %+v", open)
		}
	}
	rig.drain()
	if rig.receipts(teamwire.TransitionOpened) != 0 {
		t.Fatal("nothing was sent as opened")
	}
}

// Criterion 77: Deliver again is offered on a started and on an opened item, re-arms
// the brief for the claimed session, and never produces a second opened.
func TestDeliverAgainRearmsAndNeverSendsASecondOpened(t *testing.T) {
	rig := newOpenRig(t)
	rig.ready("claude")
	if code, refusal := rig.call("POST", "/api/team/handoffs/"+rig.doc.ID+"/deliver-again", nil, nil); code != http.StatusConflict || refusal.Code != store.HandoffCodeNotClaimed {
		t.Fatalf("no session started yet: %d %+v", code, refusal)
	}
	ticket := rig.open("claude").Ticket.TicketID
	rig.launch("claude", ticket, TaskCompleted)
	rig.entry("claude", "ses-launched", "start", ticket)

	var again teamHandoffDeliverAgainResponse
	if code, refusal := rig.call("POST", "/api/team/handoffs/"+rig.doc.ID+"/deliver-again", nil, &again); code != http.StatusOK || !again.Armed ||
		again.Ticket.Brief != handoffBriefWaiting {
		t.Fatalf("deliver again on a started item: %d %+v %+v", code, refusal, again)
	}
	if handed := rig.prompt("claude", "ses-launched", true); len(handed) != 1 {
		t.Fatalf("one brief, not two, when it was already armed: %+v", handed)
	}
	if code, _ := rig.call("POST", "/api/team/handoffs/"+rig.doc.ID+"/deliver-again", teamHandoffDeliverAgainRequest{Ticket: ticket}, &again); code != http.StatusOK ||
		!again.Armed || again.Ticket.Brief != handoffBriefDelivered || !again.Ticket.BriefDueAgain {
		t.Fatalf("deliver again on an opened item: %+v", again)
	}
	if handed := rig.prompt("claude", "ses-launched", true); len(handed) != 1 {
		t.Fatalf("the next prompt carries it again: %+v", handed)
	}
	rig.drain()
	if rig.receipts(teamwire.TransitionOpened) != 1 {
		t.Fatalf("never a second opened: %d", rig.receipts(teamwire.TransitionOpened))
	}
	// The receipt's id is the same for both confirmations, so the outbox alone would
	// hide a second "first confirmation". The chained event does not: only the first
	// confirmation of a ticket reports the handoff opened (red-team M9 b1).
	if opened := rig.chained("team.handoff.opened"); opened != 1 {
		t.Fatalf("the second delivery was reported as a first confirmation: %d opened events", opened)
	}
	if got, _, _ := governor.ix.HandoffOpenByTicket(ticket); got.BriefConfirmedAt == 0 || got.BriefDue {
		t.Fatalf("the ticket is confirmed and owes nothing: %+v", got)
	}
	if code, refusal := rig.call("POST", "/api/team/handoffs/"+rig.doc.ID+"/deliver-again", teamHandoffDeliverAgainRequest{Ticket: "tkt_unknown"}, nil); code != http.StatusNotFound || refusal.Code != store.HandoffCodeTicketNotFound {
		t.Fatalf("an unknown ticket: %d %+v", code, refusal)
	}
}

// Criteria 63 (fail path) and 86: a reply that fails to reach the hook releases the
// brief and leaves the handoff at started; a daemon killed after the hand-over and
// before the confirmation re-arms it at start-up; each ends with the brief delivered
// and exactly one opened.
func TestBriefSurvivesAFailedReplyAndADeadDaemon(t *testing.T) {
	rig := newOpenRig(t)
	rig.ready("claude")
	ticket := rig.open("claude").Ticket.TicketID
	rig.launch("claude", ticket, TaskCompleted)
	rig.entry("claude", "ses-launched", "start", ticket)

	// The hook was killed before its reply: the write fails.
	rig.postTurn(openTestTurn("claude", "ses-launched", rig.repo, handoffPromptKind, true), &failingReplyWriter{})
	if detail := rig.detail(); detail.Handoff.State != teamwire.HandoffStarted || detail.Opens[0].Brief != handoffBriefWaiting {
		t.Fatalf("a failed reply leaves the handoff at started: %+v", detail.Opens)
	}

	// A daemon killed between the hand-over and the confirmation: the row is
	// delivered and the ticket is not confirmed.
	handed, err := governor.ix.ClaimSessionDeliveriesAt(store.SessionDeliveryClaim{Runtime: "claude", SessionID: "ses-launched",
		Kind: handoffPromptKind, ObservationID: "trn_dead", Now: time.Now().Unix(), MaxBytes: 7000, CarriesHandoff: true})
	if err != nil || len(handed) != 1 {
		t.Fatalf("the released brief was handed over: %+v %v", handed, err)
	}
	if again := rig.prompt("claude", "ses-launched", true); len(again) != 0 {
		t.Fatalf("until the sweep, nothing is pending: %+v", again)
	}
	handoffOpens.sweep(false)
	if again := rig.prompt("claude", "ses-launched", true); len(again) != 0 {
		t.Fatalf("a periodic sweep does not take a row handed over a moment ago for lost: %+v", again)
	}
	handoffOpens.sweep(true) // the daemon starts again
	delivered := rig.prompt("claude", "ses-launched", true)
	if len(delivered) != 1 || delivered[0].Message != handed[0].Message {
		t.Fatalf("re-armed with the same text and delivered at the next prompt: %+v", delivered)
	}
	rig.drain()
	if rig.receipts(teamwire.TransitionOpened) != 1 || rig.detail().Handoff.State != teamwire.HandoffOpened {
		t.Fatalf("one opened: %d", rig.receipts(teamwire.TransitionOpened))
	}
}

// Criterion 68: a ticket waiting at unlink is cancelled and shown as cancelled, never
// claimed later; a received handoff stays openable locally and nothing is sent.
func TestUnlinkCancelsAWaitingTicketAndTheHandoffOpensLocally(t *testing.T) {
	rig := newOpenRig(t)
	rig.ready("claude")
	waiting := rig.open("claude").Ticket.TicketID
	rig.drain()
	pushedBefore := len(rig.fake.records(teamwire.KindHandoffReceipt))
	if _, err := rig.tl.unlink(); err != nil {
		t.Fatal(err)
	}
	detail := rig.detail()
	if len(detail.Opens) != 1 || detail.Opens[0].State != store.HandoffOpenCancelled || detail.Opens[0].EndedCode != store.HandoffCodeLinkEnded {
		t.Fatalf("shown as cancelled: %+v", detail.Opens)
	}
	if code, _, refusal := rig.send("claude", waiting); code != http.StatusConflict || refusal.Code != store.HandoffCodeTicketCancelled {
		t.Fatalf("a send with the cancelled ticket: %d %+v", code, refusal)
	}
	rig.entry("claude", "ses-late", "start", waiting)
	if got, _, _ := governor.ix.HandoffOpenByTicket(waiting); got.Claimed() {
		t.Fatal("never claimed later")
	}
	if !detail.Handoff.OpenOffered || !detail.Handoff.LinkEnded {
		t.Fatalf("after unlink a received handoff stays openable locally: %+v", detail.Handoff)
	}
	local := rig.open("claude").Ticket.TicketID
	rig.launch("claude", local, TaskCompleted)
	rig.entry("claude", "ses-launched", "start", local)
	if handed := rig.prompt("claude", "ses-launched", true); len(handed) != 1 {
		t.Fatalf("the same ticket, claim and delivery, with no server: %+v", handed)
	}
	if state := rig.detail().Handoff.State; state != teamwire.HandoffOpened {
		t.Fatalf("opened locally: %s", state)
	}
	rig.tl.pushOnce()
	if got := len(rig.fake.records(teamwire.KindHandoffReceipt)); got != pushedBefore {
		t.Fatalf("nothing is sent after unlink: %d receipts, %d before", got, pushedBefore)
	}
	if rows, err := governor.ix.OutboxBatch(100, nil); err != nil || len(rows) != 0 {
		t.Fatalf("nothing is queued after unlink: %+v %v", rows, err)
	}
}

// A ticket has no timer: it is cancelled when its composer is closed without sending,
// and a closing composer changes nothing once the session was launched.
func TestClosingTheComposerCancelsItsTicket(t *testing.T) {
	rig := newOpenRig(t)
	rig.ready("claude")
	ticket := rig.open("claude").Ticket.TicketID
	var cancelled teamHandoffOpenCancelResponse
	if code, refusal := rig.call("POST", "/api/team/handoffs/"+rig.doc.ID+"/open/"+ticket+"/cancel", nil, &cancelled); code != http.StatusOK ||
		!cancelled.Cancelled || cancelled.Ticket.EndedCode != store.HandoffOpenComposerClosed {
		t.Fatalf("cancel: %d %+v %+v", code, refusal, cancelled)
	}
	if code, _, refusal := rig.send("claude", ticket); code != http.StatusConflict || refusal.Code != store.HandoffCodeTicketCancelled {
		t.Fatalf("a send with a cancelled ticket launches nothing: %d %+v", code, refusal)
	}
	launched := rig.open("claude").Ticket.TicketID
	rig.launch("claude", launched, TaskCompleted)
	if code, _ := rig.call("POST", "/api/team/handoffs/"+rig.doc.ID+"/open/"+launched+"/cancel", nil, &cancelled); code != http.StatusOK || cancelled.Cancelled {
		t.Fatalf("a launched ticket is not cancelled by its composer closing: %+v", cancelled)
	}
	if code, refusal := rig.call("POST", "/api/team/handoffs/"+rig.doc.ID+"/open/tkt_unknown/cancel", nil, nil); code != http.StatusNotFound || refusal.Code != store.HandoffCodeTicketNotFound {
		t.Fatalf("an unknown ticket: %d %+v", code, refusal)
	}
	// A ticketed send needs a new session and a known ticket.
	for name, req := range map[string]ChatRequest{
		handoffCodeNewSession:           {Runtime: "claude", Prompt: "p", HandoffTicket: launched, SessionID: "ses-existing", Cwd: rig.repo},
		store.HandoffCodeTicketNotFound: {Runtime: "claude", Prompt: "p", HandoffTicket: "tkt_unknown", Cwd: rig.repo},
	} {
		_, _, err := rig.tasks.Create(req, "ticket-refusal-"+name)
		var refusal *handoffLaunchError
		if !errors.As(err, &refusal) || refusal.Code != name {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// Criteria 64 and 72, F-6: chatLaunchEnv is the one place a runtime process's
// environment is built. It removes an inherited ticket and adds one only for the
// request that carries it; the daemon also clears its own environment at start-up, so
// a daemon whose environment holds a ticket stamps none on any launch.
func TestChatLaunchEnvStampsOnlyTheFirstTurnOfAnOpen(t *testing.T) {
	t.Setenv(observation.HandoffTicketEnv, "tkt_inherited")
	ticketsIn := func(env []string) []string {
		var out []string
		for _, entry := range env {
			if strings.HasPrefix(entry, observation.HandoffTicketEnv+"=") {
				out = append(out, entry)
			}
		}
		return out
	}
	if got := ticketsIn(chatLaunchEnv(ChatRequest{Runtime: "claude", Prompt: "p"})); len(got) != 0 {
		t.Fatalf("a daemon whose own environment holds a ticket stamps none: %v", got)
	}
	if got := ticketsIn(chatLaunchEnv(ChatRequest{HandoffTicket: "tkt_mine"})); len(got) != 1 || got[0] != observation.HandoffTicketEnv+"=tkt_mine" {
		t.Fatalf("the first turn of an Open carries its own ticket and only that: %v", got)
	}
	if err := dropInheritedHandoffTicket(); err != nil {
		t.Fatal(err)
	}
	if value, set := os.LookupEnv(observation.HandoffTicketEnv); set {
		t.Fatalf("start-up clears the daemon's own environment: %q", value)
	}
	// A launch that assigns no environment inherits the daemon's — now clean.
	out, err := exec.Command("/bin/sh", "-c", `printf '%s' "$CG_HANDOFF_TICKET"`).Output()
	if err != nil || len(out) != 0 {
		t.Fatalf("a child that inherits the daemon's environment carries no ticket: %q %v", out, err)
	}
}

// Criterion 72: a test fails any chat_*.go that assigns a process environment other
// than through chatLaunchEnv; os.Environ is read in chat.go's chatLaunchEnv and
// nowhere else in those files; and Main clears the daemon's own environment.
func TestEveryChatProcessEnvironmentGoesThroughChatLaunchEnv(t *testing.T) {
	files, err := filepath.Glob("chat*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("chat files: %v %v", files, err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.FuncDecl:
				if n.Name.Name == "chatLaunchEnv" {
					return false // the one place the daemon's environment is read
				}
			case *ast.SelectorExpr:
				if pkg, ok := n.X.(*ast.Ident); ok && pkg.Name == "os" && (n.Sel.Name == "Environ" || n.Sel.Name == "Setenv") {
					t.Errorf("%s: os.%s outside chatLaunchEnv", fset.Position(n.Pos()), n.Sel.Name)
				}
			case *ast.AssignStmt:
				for index, left := range n.Lhs {
					selector, ok := left.(*ast.SelectorExpr)
					if !ok || selector.Sel.Name != "Env" || index >= len(n.Rhs) {
						continue
					}
					if !envBuiltByChatLaunchEnv(n.Rhs[index]) {
						t.Errorf("%s: a process environment is assigned other than through chatLaunchEnv", fset.Position(n.Pos()))
					}
				}
			}
			return true
		})
	}
	main, err := os.ReadFile("main.go")
	if err != nil || !strings.Contains(string(main), "dropInheritedHandoffTicket()") {
		t.Fatalf("Main must clear the daemon's own environment of a ticket at start-up: %v", err)
	}
}

// envBuiltByChatLaunchEnv accepts the three shapes that keep an environment the one
// chatLaunchEnv built: the call itself (or a filter over it), an append to an
// environment already assigned, and a copy of another command's environment.
func envBuiltByChatLaunchEnv(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.SelectorExpr:
		return e.Sel.Name == "Env"
	case *ast.CallExpr:
		name, ok := e.Fun.(*ast.Ident)
		if !ok || len(e.Args) == 0 {
			return false
		}
		switch name.Name {
		case "chatLaunchEnv":
			return true
		case "append", "withoutEnv":
			return envBuiltByChatLaunchEnv(e.Args[0])
		}
	}
	return false
}

// The checker itself: it must refuse the shapes it exists to refuse.
func TestTheEnvironmentCheckRefusesADirectAssignment(t *testing.T) {
	for source, want := range map[string]bool{
		`cmd.Env = chatLaunchEnv(req)`:                             true,
		`cmd.Env = append(cmd.Env, "A=b")`:                         true,
		`cmd.Env = withoutEnv(chatLaunchEnv(ChatRequest{}), "A")`:  true,
		`bounded.Env = cmd.Env`:                                    true,
		`cmd.Env = os.Environ()`:                                   false,
		`cmd.Env = append(os.Environ(), "A=b")`:                    false,
		`cmd.Env = []string{"A=b"}`:                                false,
		`cmd.Env = buildEnv(req)`:                                  false,
		`cmd.Env = append([]string{"CG_HANDOFF_TICKET=x"}, "A=b")`: false,
	} {
		statement, err := parser.ParseExpr("func() { " + source + " }")
		if err != nil {
			t.Fatal(err)
		}
		assign := statement.(*ast.FuncLit).Body.List[0].(*ast.AssignStmt)
		if got := envBuiltByChatLaunchEnv(assign.Rhs[0]); got != want {
			t.Errorf("%s: accepted=%v, want %v", source, got, want)
		}
	}
}

// Criterion 72 and §6.5 "Budget": handoff.inject_max_bytes is validated at load to be
// no more than the smallest encoder cap and no more than delivery.claim_bytes less the
// join bytes for the pending cap; an invalid value refuses to load, naming the key.
func TestInvalidInjectMaxBytesRefusesToLoad(t *testing.T) {
	defer swapOrchestrationConfig(defaultOrchestrationConfig())()
	delivery := defaultOrchestrationConfig().Delivery
	ceiling, _ := handoffInjectCeiling(delivery)
	joins := (delivery.MaxPendingPerSession - 1) * len(guardcli.HookContextJoin)
	if ceiling != delivery.ClaimBytes-joins || ceiling != 6996 {
		t.Fatalf("the default ceiling is delivery.claim_bytes less the join bytes: %d", ceiling)
	}
	if err := validateHandoffInjectBudget(ceiling, delivery); err != nil {
		t.Fatalf("the ceiling itself fits: %v", err)
	}
	err := validateHandoffInjectBudget(ceiling+1, delivery)
	if err == nil || !strings.Contains(err.Error(), "handoff.inject_max_bytes") || !strings.Contains(err.Error(), "delivery.claim_bytes") {
		t.Fatalf("over the claim budget, by name: %v", err)
	}
	roomy := delivery
	roomy.ClaimBytes = 20000
	err = validateHandoffInjectBudget(7001, roomy)
	if err == nil || !strings.Contains(err.Error(), "handoff.inject_max_bytes") || !strings.Contains(err.Error(), "codex hook's context cap") {
		t.Fatalf("over the smallest encoder cap, by name: %v", err)
	}
	if err := validateHandoffInjectBudget(40, delivery); err == nil || !strings.Contains(err.Error(), "handoff.inject_max_bytes") {
		t.Fatalf("too small to hold the brief's fixed lines: %v", err)
	}

	defaults, err := defaultHandoffConfig()
	if err != nil || defaults.InjectMaxBytes != 4900 || validateHandoffInjectBudget(defaults.InjectMaxBytes, delivery) != nil {
		t.Fatalf("the embedded default loads: %+v %v", defaults, err)
	}
	config, problems, err := parseConsoleConfig([]byte(`{"handoff":{"inject_max_bytes":9000}}`), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if config.Handoff.InjectMaxBytes != defaults.InjectMaxBytes || len(problems) != 1 || !strings.Contains(problems[0], "handoff.inject_max_bytes") {
		t.Fatalf("an invalid value is refused by name and never loaded: %d %v", config.Handoff.InjectMaxBytes, problems)
	}
	if config, problems, err := parseConsoleConfig([]byte(`{"handoff":{"inject_max_bytes":6000}}`), t.TempDir()); err != nil || len(problems) != 0 || config.Handoff.InjectMaxBytes != 6000 {
		t.Fatalf("a valid value loads: %d %v %v", config.Handoff.InjectMaxBytes, problems, err)
	}
}

// The budget arithmetic, per runtime: a brief at its ceiling, joined to as many other
// messages as the pending cap allows within the claim budget, is inside every
// runtime's encoder cap — so no encoder and no vendor ever cuts the brief.
func TestBriefBudgetFitsEveryRuntimesEncoderCap(t *testing.T) {
	delivery := defaultOrchestrationConfig().Delivery
	defaults, err := defaultHandoffConfig()
	if err != nil {
		t.Fatal(err)
	}
	caps := guardcli.HookContextCaps()
	if len(caps) == 0 {
		t.Fatal("no runtime publishes an encoder cap")
	}
	for runtime, encoderCap := range caps {
		joins := (delivery.MaxPendingPerSession - 1) * len(guardcli.HookContextJoin)
		if defaults.InjectMaxBytes > encoderCap {
			t.Fatalf("%s: the brief's ceiling %d is over the encoder cap %d", runtime, defaults.InjectMaxBytes, encoderCap)
		}
		if defaults.InjectMaxBytes+joins > delivery.ClaimBytes {
			t.Fatalf("%s: the brief and its joins exceed the claim budget", runtime)
		}
		// The brief sorts first in the claim, so an encoder that cuts at its cap can
		// only cut what follows the brief: the brief ends before the cap.
		if defaults.InjectMaxBytes+len(guardcli.HookContextJoin) > encoderCap {
			t.Fatalf("%s: the encoder's cut could reach into the brief", runtime)
		}
	}
}

// §6.5 "What the brief contains, in order", written to be whole within its ceiling:
// the remaining items are cut with a count when they do not fit, never split.
func TestBriefContentOrderAndCut(t *testing.T) {
	h := store.Handoff{OrganizationID: "org_1", PeerName: "Dana"}
	rec := teamwire.HandoffRecord{Title: "Finish the outbox drain", Remaining: []string{"Drain handoff kinds first", "Add the parked-kind retry"}}
	brief := buildHandoffBrief(h, rec, 0, 4900)
	order := []string{"Crossing Guard handoff from your teammate Dana", "a teammate's text, not the operator's", "Title: Finish the outbox drain",
		"What remains (2):", "- Drain handoff kinds first", "- Add the parked-kind retry", "returned by the tool get_handoff"}
	last := -1
	for _, part := range order {
		at := strings.Index(brief, part)
		if at <= last {
			t.Fatalf("%q is missing or out of order:\n%s", part, brief)
		}
		last = at
	}
	many := teamwire.HandoffRecord{Title: "T"}
	for index := 0; index < 400; index++ {
		many.Remaining = append(many.Remaining, fmt.Sprintf("item %03d with some words to fill the line", index))
	}
	for _, ceiling := range []int{600, 1200, 4900} {
		cut := buildHandoffBrief(h, many, 0, ceiling)
		if len(cut) > ceiling {
			t.Fatalf("a brief of %d bytes is over its ceiling %d", len(cut), ceiling)
		}
		if !strings.Contains(cut, "more not shown here") || !strings.Contains(cut, "What remains (400):") || !strings.HasSuffix(cut, "the tool get_handoff.") {
			t.Fatalf("cut with a count, and the tool's name survives:\n%s", cut)
		}
		for _, line := range strings.Split(cut, "\n") {
			if strings.HasPrefix(line, "- item") && !strings.HasSuffix(line, "fill the line") {
				t.Fatalf("an item was split: %q", line)
			}
		}
	}
	if local := buildHandoffBrief(store.Handoff{}, rec, 0, 4900); !strings.Contains(local, "this device's own earlier session") {
		t.Fatalf("a local handoff names no teammate:\n%s", local)
	}
	long := teamwire.HandoffRecord{Title: strings.Repeat("é", 4000)}
	if cut := buildHandoffBrief(h, long, 0, 700); len(cut) > 700 || !strings.HasSuffix(cut, "the tool get_handoff.") || strings.ContainsRune(cut, '\uFFFD') {
		t.Fatalf("a long title is cut on a rune boundary inside the ceiling: %d", len(cut))
	}
}

// §6.5's fifth element (owner decision 2026-10-04, journey finding F-1): when the
// repository has shared team memory the brief closes with ONE line stating the count
// and the recall tools by their registered names — nothing of the records' content —
// and that line's bytes are set aside before the items are fitted, so the items are
// what is cut, never the line. No records is no line, and the brief is as it was.
func TestBriefClosingLineStatesSharedMemoryAndIsNeverCut(t *testing.T) {
	h := store.Handoff{OrganizationID: "org_1", PeerName: "Dana"}
	rec := teamwire.HandoffRecord{Title: "Finish the outbox drain", Remaining: []string{"Drain handoff kinds first"}}
	closing := "The team's shared memory for this repository holds 3 records; the tools " +
		recallmcp.ToolSearchMemories + " and " + recallmcp.ToolGetMemory + " read them."
	if recallmcp.ToolSearchMemories != "search_memories" || recallmcp.ToolGetMemory != "get_memory" {
		t.Fatalf("the registered tool names moved: %q %q", recallmcp.ToolSearchMemories, recallmcp.ToolGetMemory)
	}
	brief := buildHandoffBrief(h, rec, 3, 4900)
	lines := strings.Split(brief, "\n")
	if lines[len(lines)-1] != closing || !strings.HasSuffix(lines[len(lines)-2], "the tool get_handoff.") {
		t.Fatalf("the closing line follows the tool that returns the rest:\n%s", brief)
	}
	if strings.Count(brief, "shared memory") != 1 {
		t.Fatalf("one closing line, not more:\n%s", brief)
	}
	if one := buildHandoffBrief(h, rec, 1, 4900); !strings.HasSuffix(one, "holds 1 record; the tools search_memories and get_memory read it.") {
		t.Fatalf("one record reads as one:\n%s", one)
	}
	for _, none := range []int{0, -1} {
		without := buildHandoffBrief(h, rec, none, 4900)
		if strings.Contains(without, "shared memory") || !strings.HasSuffix(without, "the tool get_handoff.") {
			t.Fatalf("no records, or an undetermined count, is no line:\n%s", without)
		}
	}
	// A local handoff has no organization; the line depends only on what this device
	// holds for the checkout.
	if local := buildHandoffBrief(store.Handoff{}, rec, 3, 4900); !strings.Contains(local, "this device's own earlier session") || !strings.HasSuffix(local, closing) {
		t.Fatalf("a local handoff still closes with the line:\n%s", local)
	}

	many := teamwire.HandoffRecord{Title: "T"}
	for index := 0; index < 400; index++ {
		many.Remaining = append(many.Remaining, fmt.Sprintf("item %03d with some words to fill the line", index))
	}
	for _, ceiling := range []int{600, 1200, 4900} {
		cut := buildHandoffBrief(h, many, 3, ceiling)
		if len(cut) > ceiling {
			t.Fatalf("a brief of %d bytes is over its ceiling %d", len(cut), ceiling)
		}
		if !strings.HasSuffix(cut, "the tool get_handoff.\n"+closing) || !strings.Contains(cut, "more not shown here") || !strings.Contains(cut, "What remains (400):") {
			t.Fatalf("the items are cut with a count and the closing line is whole at %d:\n%s", ceiling, cut)
		}
		// The line costs items, and only items: the same handoff without it shows more.
		if shownWith, shownWithout := strings.Count(cut, "\n- item"), strings.Count(buildHandoffBrief(h, many, 0, ceiling), "\n- item"); shownWith >= shownWithout {
			t.Fatalf("the closing line is budgeted before the items at %d: %d shown with it, %d without", ceiling, shownWith, shownWithout)
		}
	}
	long := teamwire.HandoffRecord{Title: strings.Repeat("é", 4000)}
	if cut := buildHandoffBrief(h, long, 3, 700); len(cut) > 700 || !strings.HasSuffix(cut, closing) || strings.ContainsRune(cut, '\uFFFD') {
		t.Fatalf("a long title is cut, never the closing line: %d", len(cut))
	}

	// The budget, at the widest count the floor allows for: a brief at the default
	// ceiling still fits, so the per-runtime arithmetic above covers the line too.
	defaults, err := defaultHandoffConfig()
	if err != nil {
		t.Fatal(err)
	}
	widest := buildHandoffBrief(store.Handoff{OrganizationID: "org", PeerName: strings.Repeat("n", handoffBriefNameAllowance)}, many, handoffBriefCountAllowance, defaults.InjectMaxBytes)
	if len(widest) > defaults.InjectMaxBytes || !strings.Contains(widest, fmt.Sprintf("holds %d records", handoffBriefCountAllowance)) {
		t.Fatalf("the widest closing line is whole inside handoff.inject_max_bytes: %d", len(widest))
	}
	floor := len(buildHandoffBrief(store.Handoff{OrganizationID: "org", PeerName: strings.Repeat("n", handoffBriefNameAllowance)}, teamwire.HandoffRecord{}, handoffBriefCountAllowance, 1<<20))
	delivery := defaultOrchestrationConfig().Delivery
	if validateHandoffInjectBudget(floor, delivery) != nil || validateHandoffInjectBudget(floor-1, delivery) == nil {
		t.Fatalf("the smallest valid ceiling is the one that holds the first line and both closing lines (%d)", floor)
	}
}

// A display name of any length cannot cost the brief its closing lines: the first line
// carries at most handoffBriefNameAllowance bytes of it, cut on a rune boundary and
// marked, so the tool's name and the shared-memory line are whole at the default
// ceiling. Under a ceiling too small for both fixed lines — one validation refuses —
// the line naming the tool is what is kept.
func TestBriefBoundsTheSendersNameAndKeepsTheToolLine(t *testing.T) {
	defaults, err := defaultHandoffConfig()
	if err != nil {
		t.Fatal(err)
	}
	rec := teamwire.HandoffRecord{Title: "Rate limiter", Remaining: []string{"rule 4", "rule 5"}}
	closing := "returned by the tool get_handoff.\n" + handoffSharedMemoryLine(3)
	for _, name := range []string{strings.Repeat("n", 3*defaults.InjectMaxBytes), strings.Repeat("名", defaults.InjectMaxBytes)} {
		brief := buildHandoffBrief(store.Handoff{OrganizationID: "org", PeerName: name}, rec, 3, defaults.InjectMaxBytes)
		if len(brief) > defaults.InjectMaxBytes || !utf8.ValidString(brief) || !strings.HasSuffix(brief, closing) {
			t.Fatalf("a very long name leaves the brief whole to its closing lines (%d bytes):\n%s", len(brief), brief)
		}
		if !strings.Contains(brief, "- rule 5") || !strings.Contains(brief, handoffBriefNameCut+":") {
			t.Fatalf("the items are shown and the name is marked as cut:\n%s", brief)
		}
	}
	if got := handoffBriefName(strings.Repeat("名", 100)); len(got) > handoffBriefNameAllowance || !utf8.ValidString(got) || !strings.HasSuffix(got, handoffBriefNameCut) {
		t.Fatalf("a cut name is within the allowance, mark included: %d bytes %q", len(got), got)
	}
	exact := strings.Repeat("n", handoffBriefNameAllowance)
	if handoffBriefName(exact) != exact {
		t.Fatal("a name at the allowance is carried whole")
	}
	for _, tooSmall := range []int{120, 40} {
		brief := buildHandoffBrief(store.Handoff{OrganizationID: "org", PeerName: "Daniel"}, rec, 3, tooSmall)
		if len(brief) > tooSmall || !strings.HasSuffix(brief, cutText("The full handoff text is returned by the tool get_handoff.", tooSmall)) {
			t.Fatalf("under a ceiling of %d the tool's line is what survives: %q", tooSmall, brief)
		}
	}
}

// The count the closing line states: the repository-scope records the team holds that
// a session in the ticket's checkout would recall on this device — not organization
// records, not another repository's, not a record that never left this device, not a
// pending one. A folder with no single origin remote has no team scope and no line, and
// neither has a device that is not linked.
func TestHandoffSharedMemoryCountIsThisRepositorysTeamRecords(t *testing.T) {
	_, ix, _ := teamTestLinker(t)
	if err := ix.SetLinked(true); err != nil { // the team's records are a linked device's
		t.Fatal(err)
	}
	here := gitRepo(t, "service", "https://git.example.com/acme/service.git")
	elsewhere := gitRepo(t, "service", "https://git.example.com/acme/other.git")
	noRemote := gitRepo(t, "scratch", "")
	repo, err := changeenv.ResolveRepository(here)
	if err != nil {
		t.Fatal(err)
	}
	other, err := changeenv.ResolveRepository(elsewhere)
	if err != nil {
		t.Fatal(err)
	}
	if got := handoffSharedMemoryCount(context.Background(), ix, here); got != 0 {
		t.Fatalf("an empty store counts nothing: %d", got)
	}
	rows := []teamwire.PullRow{
		pulledRow(1, engine.NewTypedID("mem"), "here-one", "repository", repo.ID, "a", 1),
		pulledRow(2, engine.NewTypedID("mem"), "here-two", "repository", repo.ID, "b", 1),
		pulledRow(3, engine.NewTypedID("mem"), "there", "repository", other.ID, "c", 1),
		pulledRow(4, engine.NewTypedID("mem"), "org-wide", "organization", "org_1", "d", 1),
	}
	if eff, err := ix.ApplyPulledRows(rows, 1, ""); err != nil || len(eff.Landed) != len(rows) {
		t.Fatalf("the team's records land: %+v %v", eff, err)
	}
	for _, r := range []store.MemoryRecord{
		{ID: "mine-only", Title: "never shared", Category: "note", Body: "e", Source: "human", Status: "active", ScopeType: store.MemoryScopeRepository, ScopeID: repo.ID, RepositoryIdentity: "remote-sha256", AuthorType: "user", AuthorID: "t"},
		{ID: "proposed", Title: "not accepted", Category: "note", Body: "f", Source: "agent", Status: "pending", ScopeType: store.MemoryScopeRepository, ScopeID: repo.ID, RepositoryIdentity: "remote-sha256", AuthorType: "agent", AuthorID: "t"},
	} {
		if _, err := ix.CreateMemory(r, nil, nil, teamHuman()); err != nil {
			t.Fatal(err)
		}
	}
	if got := handoffSharedMemoryCount(context.Background(), ix, here); got != 2 {
		t.Fatalf("this repository's team records: %d, want 2", got)
	}
	// The count is recall's predicate and the team-record rule, not a second rule.
	active, err := ix.ListMemory("active")
	if err != nil {
		t.Fatal(err)
	}
	scope, byRule := resolveRecallScope(context.Background(), here), 0
	for _, r := range active {
		if r.ScopeType == store.MemoryScopeRepository && store.MemoryIsTeamRecord(r) && memoryInRecallScope(r, scope) {
			byRule++
		}
	}
	if byRule != 2 || scope.RepositoryID != repo.ID {
		t.Fatalf("the store's count and recall's predicate agree: %d by the rule, scope %+v", byRule, scope)
	}
	if got := handoffSharedMemoryCount(context.Background(), ix, elsewhere); got != 1 {
		t.Fatalf("another repository counts its own: %d", got)
	}
	if got := handoffSharedMemoryCount(context.Background(), ix, noRemote); got != 0 {
		t.Fatalf("a checkout with no origin remote has no team scope: %d", got)
	}
	if got := handoffSharedMemoryCount(context.Background(), ix, ""); got != 0 {
		t.Fatalf("no folder counts nothing: %d", got)
	}
	// With no link there is no team: the records received from it stay, uncounted.
	if err := ix.SetLinked(false); err != nil {
		t.Fatal(err)
	}
	if got := handoffSharedMemoryCount(context.Background(), ix, here); got != 0 {
		t.Fatalf("an unlinked device's brief has no line about a team's memory: %d", got)
	}
}

// Criterion 63's closing line, through the claim: a session opened for a handoff in a
// checkout whose repository has shared team memory is handed a brief that ends with the
// count and the tools; a compaction's re-arm hands over the SAME text though the count
// has moved since (the count is as of the first arming).
func TestOpenedBriefClosesWithTheRepositorysSharedMemory(t *testing.T) {
	rig := newOpenRig(t)
	rig.repo = gitRepo(t, "service", "https://git.example.com/acme/service.git")
	repo, err := changeenv.ResolveRepository(rig.repo)
	if err != nil {
		t.Fatal(err)
	}
	rows := []teamwire.PullRow{
		pulledRow(1, engine.NewTypedID("mem"), "runbook", "repository", repo.ID, "a", 1),
		pulledRow(2, engine.NewTypedID("mem"), "gotcha", "repository", repo.ID, "b", 1),
		pulledRow(3, engine.NewTypedID("mem"), "org-wide", "organization", "org_1", "c", 1),
	}
	if eff, err := rig.ix.ApplyPulledRows(rows, 1, ""); err != nil || len(eff.Landed) != len(rows) {
		t.Fatalf("the team's records land: %+v %v", eff, err)
	}
	rig.ready("claude")
	ticket := rig.open("claude").Ticket.TicketID
	rig.launch("claude", ticket, TaskCompleted)
	rig.entry("claude", "ses-launched", "start", ticket)
	deliveries := rig.prompt("claude", "ses-launched", true)
	closing := "The team's shared memory for this repository holds 2 records; the tools search_memories and get_memory read them."
	if len(deliveries) != 1 || !strings.HasSuffix(deliveries[0].Message, "the tool get_handoff.\n"+closing) {
		t.Fatalf("the first prompt's brief closes with the count and the tools: %+v", deliveries)
	}
	first := deliveries[0].Message
	if len(first) > handoffInjectMaxBytes() || strings.Contains(first, "runbook") || strings.Contains(first, "T gotcha") {
		t.Fatalf("the brief is within its ceiling and says nothing of the records' content:\n%s", first)
	}
	if eff, err := rig.ix.ApplyPulledRows([]teamwire.PullRow{pulledRow(4, engine.NewTypedID("mem"), "later", "repository", repo.ID, "d", 1)}, 1, ""); err != nil || len(eff.Landed) != 1 {
		t.Fatalf("a third record lands: %+v %v", eff, err)
	}
	rig.entry("claude", "ses-launched", "context-compact", "")
	if again := rig.prompt("claude", "ses-launched", true); len(again) != 1 || again[0].Message != first {
		t.Fatalf("a re-arm hands over the stored text, count and all: %+v", again)
	}
}

// A place resolution that does not come back cannot hold the claim past the hook's
// wait: the count runs on the ingest's context, so the claim is prepared — with a brief
// that has no closing line — as soon as that context ends, not at the longer
// recall.peer_git_timeout_ms. The slow answer is not remembered: the next entry counts.
func TestBlockedPlaceResolutionDoesNotDelayTheClaim(t *testing.T) {
	rig := newOpenRig(t)
	rig.repo = gitRepo(t, "service", "https://git.example.com/acme/service.git")
	repo, err := changeenv.ResolveRepository(rig.repo)
	if err != nil {
		t.Fatal(err)
	}
	rows := []teamwire.PullRow{pulledRow(1, engine.NewTypedID("mem"), "runbook", "repository", repo.ID, "a", 1)}
	if eff, err := rig.ix.ApplyPulledRows(rows, 1, ""); err != nil || len(eff.Landed) != 1 {
		t.Fatalf("the team's record lands: %+v %v", eff, err)
	}
	rig.ready("claude")
	ticket := rig.open("claude").Ticket.TicketID
	rig.launch("claude", ticket, TaskCompleted)
	recallScopes.Lock()
	recallScopes.byFolder = map[string]cachedRecallScope{} // nothing resolved earlier answers for git
	recallScopes.Unlock()
	if config, _ := consoleConfig(); time.Duration(config.Recall.PeerGitTimeoutMS)*time.Millisecond <= observation.HookDeliveryBudget {
		t.Fatalf("the test needs a resolution timeout longer than the hook's wait: %d ms", config.Recall.PeerGitTimeoutMS)
	}

	// A git that never answers, first on the path, for this test only.
	stuck := t.TempDir()
	if err := os.WriteFile(filepath.Join(stuck, "git"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	realPath := os.Getenv("PATH")
	t.Setenv("PATH", stuck+string(os.PathListSeparator)+realPath)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	step := prepareHandoffEntry(ctx, governor, openTestEntry("claude", "ses-launched", rig.repo, "start", ticket, time.Now().Unix()))
	if waited := time.Since(started); waited >= observation.HookDeliveryBudget {
		t.Fatalf("the claim waited %s for a count, past the hook's wait of %s", waited, observation.HookDeliveryBudget)
	}
	if step.claim == nil || !strings.HasSuffix(step.claim.BriefText, "returned by the tool get_handoff.") {
		t.Fatalf("the claim is prepared, and its brief ends at the tool's name with no line about memory: %+v", step.claim)
	}

	t.Setenv("PATH", realPath)
	if got := handoffSharedMemoryCount(context.Background(), rig.ix, rig.repo); got != 1 {
		t.Fatalf("a resolution that ran out of time is not cached: the next count is %d, want 1", got)
	}
}

// The open owner follows the task service's events — there is no end callback — so the
// same outcome is reached from the events themselves as from re-reading the task: the
// first session frame is recorded, and the turn's end settles the ticket.
func TestTaskEventsRecordTheFrameAndSettleTheTicket(t *testing.T) {
	rig := newOpenRig(t)
	rig.ready("codex")
	rig.drivers(handoffFixtureDriver{session: "ses-from-frame"})
	ticket := rig.open("codex").Ticket.TicketID
	task := rig.launch("codex", ticket, TaskCompleted)
	if _, watched := handoffOpens.ticketOf(task.ID); !watched {
		t.Fatal("a launched task is watched from the moment its ticket names it")
	}
	events, err := rig.tasks.Events(task.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		handoffOpens.taskEvent(rig.tasks, event)
		if event.Kind != "task.completed" {
			if got, _, _ := governor.ix.HandoffOpenByTicket(ticket); got.State != store.HandoffOpenWaiting {
				t.Fatalf("the ticket waits until the turn ends: %+v after %s", got, event.Kind)
			}
		}
	}
	got, _, _ := governor.ix.HandoffOpenByTicket(ticket)
	if got.LaunchedNativeID != "ses-from-frame" || got.State != store.HandoffOpenCancelled || got.CancelReason != store.HandoffOpenHookDidNotRun {
		t.Fatalf("the frame is recorded and the unclaimed turn settles the ticket: %+v", got)
	}
	if _, watched := handoffOpens.ticketOf(task.ID); watched {
		t.Fatal("an ended task is watched no longer")
	}
	// An event of a task no Open launched is nobody's.
	handoffOpens.taskEvent(rig.tasks, TaskEvent{TaskID: "task_other", Kind: "task.completed", Payload: map[string]any{"type": "done"}})
}

// What an open shows, and what it offers: the brief's three states, Deliver again on a
// claimed open of a live handoff, Retry only when the runtime never started a session.
func TestOpenViewStatesAndOffers(t *testing.T) {
	claimed := store.HandoffOpen{TicketID: "tkt_1", Runtime: "codex", State: store.HandoffOpenClaimed, ClaimedRuntime: "codex",
		ClaimedNativeID: "ses-1", ClaimedAt: 100, CreatedAt: 90, BriefDue: true}
	for name, c := range map[string]struct {
		ticket store.HandoffOpen
		live   bool
		brief  string
		offers []string
	}{
		"started, waiting":       {claimed, true, handoffBriefWaiting, []string{handoffActionDeliverAgain}},
		"brief not delivered":    {withTicket(claimed, func(t *store.HandoffOpen) { t.BriefGivenUpAt = 500 }), true, handoffBriefNotDelivered, []string{handoffActionDeliverAgain}},
		"delivered":              {withTicket(claimed, func(t *store.HandoffOpen) { t.BriefConfirmedAt, t.BriefDue = 200, false }), true, handoffBriefDelivered, []string{handoffActionDeliverAgain}},
		"handoff ended":          {claimed, false, handoffBriefWaiting, []string{}},
		"waiting composer":       {store.HandoffOpen{TicketID: "tkt_2", State: store.HandoffOpenWaiting}, true, "", []string{}},
		"could not start":        {store.HandoffOpen{TicketID: "tkt_3", State: store.HandoffOpenCancelled, CancelReason: store.HandoffOpenRuntimeNotStarted}, true, "", []string{handoffActionRetry}},
		"could not start, ended": {store.HandoffOpen{TicketID: "tkt_3", State: store.HandoffOpenCancelled, CancelReason: store.HandoffOpenRuntimeNotStarted}, false, "", []string{}},
		"hook did not run":       {store.HandoffOpen{TicketID: "tkt_4", State: store.HandoffOpenCancelled, CancelReason: store.HandoffOpenHookDidNotRun}, true, "", []string{}},
		"composer closed":        {store.HandoffOpen{TicketID: "tkt_5", State: store.HandoffOpenCancelled, CancelReason: store.HandoffOpenComposerClosed}, true, "", []string{}},
		"claimed, then handoff end": {withTicket(claimed, func(t *store.HandoffOpen) {
			t.State, t.CancelReason = store.HandoffOpenCancelled, teamwire.CodeHandoffClosed
		}), false, handoffBriefWaiting, []string{}},
	} {
		view := handoffOpenView(c.ticket, c.live)
		if view.Brief != c.brief || strings.Join(view.Offers, ",") != strings.Join(c.offers, ",") {
			t.Errorf("%s: brief=%q offers=%v, want %q %v", name, view.Brief, view.Offers, c.brief, c.offers)
		}
		if (view.Session != nil) != c.ticket.Claimed() {
			t.Errorf("%s: the session is shown exactly when one claimed", name)
		}
	}
}

func withTicket(ticket store.HandoffOpen, change func(*store.HandoffOpen)) store.HandoffOpen {
	change(&ticket)
	return ticket
}
