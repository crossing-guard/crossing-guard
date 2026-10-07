package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crossing-guard/engine"
	"crossing-guard/internal/guardcli"
	"crossing-guard/internal/observation"
	"crossing-guard/store"
	"crossing-guard/teamwire"
)

// Red-team M9 (b5): §6.4 rule 3 at the daemon. The launched turn is still running —
// its session frame is known and its ticket is still waiting, so it is claimable — and
// another session's entry carrying the inherited ticket is refused because it is not
// the launched session, not because the ticket ended. TestOnlyTheLaunchedSession…
// reaches the same entry only after the turn ended, where the ticket is cancelled and
// the refusal is ticket_ended whatever rule 3 does.
func TestAnotherSessionIsRefusedOnceTheFrameIsKnownWhileTheTicketIsStillClaimable(t *testing.T) {
	rig := newOpenRig(t)
	rig.ready("codex")
	rig.drivers(handoffFixtureDriver{session: "ses-launched", hold: true})
	ticket := rig.open("codex").Ticket.TicketID
	code, task, refusal := rig.send("codex", ticket)
	if code != http.StatusAccepted {
		t.Fatalf("send: %d %+v", code, refusal)
	}
	t.Cleanup(func() { _, _ = rig.tasks.Interrupt(task.ID) })
	deadline := time.Now().Add(5 * time.Second)
	for {
		handoffOpens.reconcile(rig.tasks)
		if got, _, _ := governor.ix.HandoffOpenByTicket(ticket); got.LaunchedNativeID != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the launched task's session frame was never recorded")
		}
		time.Sleep(20 * time.Millisecond)
	}
	before, _, _ := governor.ix.HandoffOpenByTicket(ticket)
	if before.LaunchedNativeID != "ses-launched" || before.State != store.HandoffOpenWaiting || before.Claimed() {
		t.Fatalf("the frame is known and the ticket is still claimable: %+v", before)
	}

	logged := captureLog(t)
	rig.entry("codex", "ses-spawned", "start", ticket)
	after, _, _ := governor.ix.HandoffOpenByTicket(ticket)
	if after.Claimed() || after.State != store.HandoffOpenWaiting {
		t.Fatalf("a session other than the launched one claimed a waiting ticket: %+v", after)
	}
	if !strings.Contains(logged.String(), store.HandoffClaimNotLaunched) {
		t.Fatalf("the refusal must be rule 3's (%s): %q", store.HandoffClaimNotLaunched, logged.String())
	}
	if handed := rig.prompt("codex", "ses-spawned", true); len(handed) != 0 {
		t.Fatalf("the refused session is handed nothing: %+v", handed)
	}

	rig.entry("codex", "ses-launched", "start", ticket)
	if got, _, _ := governor.ix.HandoffOpenByTicket(ticket); got.ClaimedNativeID != "ses-launched" {
		t.Fatalf("the launched session claims its own ticket: %+v", got)
	}
}

// codexRecordedPrompt is the envelope the installed Codex hook posts for one payload
// the 2026-10-03 probe recorded (a parent that spawned one child thread): decoded and
// judged by the hook's own code, then sent from this rig's folder.
func codexRecordedPrompt(t *testing.T, who, repo string) observation.SessionTurnEnvelope {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "guardcli", "testdata", "codex_0_159_2_user_prompt_"+who+".json"))
	if err != nil {
		t.Fatal(err)
	}
	turn, err := guardcli.HookTurnEnvelope("codex", bytes.NewReader(raw), handoffPromptKind)
	if err != nil {
		t.Fatalf("%s: %v", who, err)
	}
	turn.Cwd, turn.TranscriptPath = repo, ""
	return turn
}

// Red-team M9 (b3): K-1 through the daemon. A Codex child thread fires its prompt
// event under the PARENT's session id; only the hook can tell them apart. The
// recorded payloads go through the hook's own decoder and judgement and then through
// the daemon's handler: the child's prompt is handed nothing while the brief waits,
// and the parent's own prompt carries it. Every other daemon test sets the envelope's
// carrier by hand, so none failed when the hook stopped reporting a nested call.
func TestCodexChildPromptIsHandedNothingThroughTheDaemon(t *testing.T) {
	rig := newOpenRig(t)
	rig.ready("codex")
	parent, child := codexRecordedPrompt(t, "parent", rig.repo), codexRecordedPrompt(t, "child", rig.repo)
	if parent.SessionID == "" || parent.SessionID != child.SessionID {
		t.Fatalf("the recorded child runs under its parent's session id: %q %q", parent.SessionID, child.SessionID)
	}
	rig.drivers(handoffFixtureDriver{session: parent.SessionID})
	ticket := rig.open("codex").Ticket.TicketID
	rig.launch("codex", ticket, TaskCompleted)
	rig.entry("codex", parent.SessionID, "start", ticket)
	if got, _, _ := governor.ix.HandoffOpenByTicket(ticket); got.ClaimedNativeID != parent.SessionID {
		t.Fatalf("the launched session holds the claim: %+v", got)
	}

	post := func(turn observation.SessionTurnEnvelope) []observation.Delivery {
		t.Helper()
		recorder := httptest.NewRecorder()
		rig.postTurn(turn, recorder)
		if recorder.Code != http.StatusOK {
			t.Fatalf("prompt: %d %s", recorder.Code, recorder.Body.String())
		}
		var receipt observation.SessionTurnReceipt
		if err := json.Unmarshal(recorder.Body.Bytes(), &receipt); err != nil {
			t.Fatal(err)
		}
		return receipt.Deliveries
	}
	if handed := post(child); len(handed) != 0 {
		t.Fatalf("a Codex child prompt under the parent's session id was handed the brief: %+v", handed)
	}
	if got, _, _ := governor.ix.HandoffOpenByTicket(ticket); got.BriefConfirmedAt != 0 {
		t.Fatalf("the child's prompt confirmed the brief: %+v", got)
	}
	if handed := post(parent); len(handed) != 1 {
		t.Fatalf("the parent's own prompt carries the brief: %+v", handed)
	}
}

// Red-team Low 7: a claim whose document cannot be read used to claim the ticket with
// an empty brief, which nothing can arm afterwards. It now claims nothing and leaves
// the ticket claimable.
func TestAClaimWhoseDocumentCannotBeReadClaimsNothingAndLeavesTheTicketClaimable(t *testing.T) {
	rig := newOpenRig(t)
	// A local handoff whose stored document does not decode.
	broken := teamwire.HandoffRecord{SchemaVersion: teamwire.HandoffSchemaVersion, ID: engine.NewTypedID(teamwire.HandoffIDPrefix),
		Session: teamwire.SessionIdentity{ID: "ses_x", Runtime: "claude", NativeID: "native-1"}, Title: "Broken"}
	if err := governor.ix.SendHandoff(store.HandoffSend{Record: broken, Body: []byte(`{"schema_version":`), At: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	ticket, err := governor.ix.CreateHandoffOpen(store.HandoffOpenCreate{HandoffID: broken.ID, Runtime: "claude", CheckoutRoot: rig.repo, At: time.Now().Unix()})
	if err != nil {
		t.Fatal(err)
	}
	logged := captureLog(t)
	rig.entry("claude", "ses-reader", "start", ticket.TicketID)
	got, _, _ := governor.ix.HandoffOpenByTicket(ticket.TicketID)
	if got.Claimed() || got.State != store.HandoffOpenWaiting {
		t.Fatalf("a claim whose document could not be read took the ticket: %+v", got)
	}
	if !strings.Contains(logged.String(), "could not be read") || !strings.Contains(logged.String(), ticket.TicketID) {
		t.Fatalf("the refusal is logged with the ticket's id: %q", logged.String())
	}
	if handed := rig.prompt("claude", "ses-reader", true); len(handed) != 0 {
		t.Fatalf("nothing is handed over: %+v", handed)
	}
}

// Red-team Low 19: the two values that were compiled in are read from their config
// owners, with defaults in the embedded documents.
func TestHandoffFollowRetryAndTheRepositoryResolveLimitAreConfiguration(t *testing.T) {
	defaults, err := defaultHandoffConfig()
	if err != nil || defaults.FollowRetry.Duration != time.Second {
		t.Fatalf("the embedded default for handoff.follow_retry: %+v %v", defaults.FollowRetry, err)
	}
	zero := defaults
	zero.FollowRetry.Duration = 0
	if err := validateHandoffConfig(zero); err == nil || !strings.Contains(err.Error(), "follow_retry") {
		t.Fatalf("a follow_retry that is not positive must be refused by name: %v", err)
	}
	if config, _ := consoleConfig(); handoffFollowRetry() != config.Handoff.FollowRetry.Duration || handoffFollowRetry() <= 0 {
		t.Fatalf("the pause is daemon.json's handoff.follow_retry: %s", handoffFollowRetry())
	}
	tl, _, _ := teamTestLinker(t)
	tl.mu.Lock()
	tl.doc.IdentityUpgradeRoots = 7
	tl.mu.Unlock()
	if got := tl.repositoryResolveLimit(); got != 7 {
		t.Fatalf("the resolve pass reads team.json's identity_upgrade_roots: %d", got)
	}
}

// Journey W-3: Team Details showed "Approved by usr_…". The status carries the
// approver's display name when the member directory this device holds lists them, and
// the id alone when it does not.
func TestTeamStatusNamesTheApproverFromTheMemberDirectory(t *testing.T) {
	rig := newHandoffRig(t, true)
	rig.tl.mu.Lock()
	rig.tl.doc.ApprovedBy = handoffTeammate
	rig.tl.mu.Unlock()
	status := rig.tl.status()
	if status.ApprovedBy != handoffTeammate || status.ApprovedByName != "Teammate" {
		t.Fatalf("the approver is named from the directory: id=%q name=%q", status.ApprovedBy, status.ApprovedByName)
	}
	rig.tl.mu.Lock()
	rig.tl.doc.ApprovedBy = "usr_not_in_the_directory"
	rig.tl.mu.Unlock()
	if unknown := rig.tl.status(); unknown.ApprovedByName != "" || unknown.ApprovedBy != "usr_not_in_the_directory" {
		t.Fatalf("an approver the directory does not list has no name: %+v", unknown.ApprovedByName)
	}
}
