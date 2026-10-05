package daemon

// Owner attention (escalation-delivery plan §6–§7, AC-5…AC-9): classes by
// structure, resolution by owner motion, isolation, the candidate source,
// the one notification, and the projection flags.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"crossing-guard/internal/orchestration"
	"crossing-guard/internal/sessionactivity"
	"crossing-guard/store"
)

// attentionFixture is the escalation fixture with the host published to the
// status fold and a counting notifier behind the presence-gated router.
func attentionFixture(t *testing.T, owned bool) (*escalationFixture, *[]string) {
	t.Helper()
	fx := newEscalationFixture(t, owned, nil, []string{"reply"})
	previousHost := orchestrationManagedHostService()
	setOrchestrationManagedHostService(fx.host)
	notified := &[]string{}
	var mu sync.Mutex
	previousRouter := approvalAttention
	approvalAttention = newApprovalAttentionRouter(func(message string) {
		mu.Lock()
		*notified = append(*notified, message)
		mu.Unlock()
	})
	t.Cleanup(func() {
		setOrchestrationManagedHostService(previousHost)
		approvalAttention = previousRouter
	})
	return fx, notified
}

func (fx *escalationFixture) attentionNow(t *testing.T, now time.Time, turns []store.SessionTurnObservation, tasks []RuntimeTask) ownerAttention {
	t.Helper()
	return foldOwnerAttention(now, "managed-fixture", fx.sessionID, fx.sessionID, readOwnerAttentionBatch(now), turns, nil, tasks)
}

// AC-5: an ask_owner line is an ask; a reply that was not sent is a draft;
// advise_user and a still-pending receipt are neither.
func TestOwnerAttentionClassesByStructure(t *testing.T) {
	fx, _ := attentionFixture(t, false)
	if _, err := fx.settleClaim(t, "session.turn-ended", `{"action":"advise_user","message":"The response contains a plan.","citations":[]}`); err != nil {
		t.Fatal(err)
	}
	fx.completeWithPendingReceipt(t, time.Now().Unix())
	now := time.Now().Add(time.Second)
	if got := fx.attentionNow(t, now, nil, nil); got.AskCount != 0 || got.DraftCount != 0 {
		t.Fatalf("advice or a pending receipt raised attention: %+v", got)
	}
	if _, err := fx.settleClaim(t, "session.turn-ended", `{"action":"reply","message":"Go ahead and run the tests.","citations":[]}`); err != nil {
		t.Fatal(err)
	}
	ask, err := fx.settleClaim(t, "session.turn-ended", `{"action":"ask_owner","message":"The owner needs to pick <b>A</b> or B.","citations":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	got := fx.attentionNow(t, time.Now().Add(time.Second), nil, nil)
	if got.AskCount != 1 || got.AskText != "The owner needs to pick <b>A</b> or B." || got.AskAgent != "Design helper" || got.AskID <= 0 {
		t.Fatalf("ask = %+v", got)
	}
	if got.DraftCount != 1 || got.DraftText != "Go ahead and run the tests." {
		t.Fatalf("draft = %+v", got)
	}
	if ask.Detail["delivery"] != nil {
		t.Fatalf("an ask is not an acting claim and carries no receipt: %+v", ask.Detail)
	}
}

// §6.3 / AC-6: an owner turn resolves; a sub-agent re-entry and a turn the
// daemon caused do not.
func TestOwnerAttentionResolvesOnOwnerMotionOnly(t *testing.T) {
	fx, _ := attentionFixture(t, false)
	ask, err := fx.settleClaim(t, "session.turn-ended", `{"action":"ask_owner","message":"Decide the schema number.","citations":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	settle := lifecycleInstantMS(ask.CompletedAt)
	now := time.UnixMilli(settle + 60_000)
	started := func(atMS int64) store.SessionTurnObservation {
		return store.SessionTurnObservation{Kind: "turn.started", ReceivedAtMS: atMS}
	}
	// A background sub-agent's result re-entered right after it ended.
	reentry := []store.SessionTurnObservation{started(settle + 5_000), {Kind: "subagent.ended", ReceivedAtMS: settle + 4_000}}
	if got := fx.attentionNow(t, now, reentry, nil); got.AskCount != 1 {
		t.Fatalf("a sub-agent re-entry resolved the ask: %+v", got)
	}
	// A turn inside a helper-launched task's window: the daemon's own.
	child := RuntimeTask{ID: ask.ChildTaskID, Lifecycle: TaskCompleted, CreatedAt: settle + 1_000, UpdatedAt: settle + 9_000}
	if got := fx.attentionNow(t, now, []store.SessionTurnObservation{started(settle + 5_000)}, []RuntimeTask{child}); got.AskCount != 1 {
		t.Fatalf("a daemon-caused turn resolved the ask: %+v", got)
	}
	// A task the owner admitted is not a managed child: his turn resolves.
	owner := RuntimeTask{ID: "task-owner-admitted", Lifecycle: TaskCompleted, CreatedAt: settle + 1_000, UpdatedAt: settle + 9_000}
	if got := fx.attentionNow(t, now, []store.SessionTurnObservation{started(settle + 5_000)}, []RuntimeTask{owner}); got.AskCount != 0 {
		t.Fatalf("an owner-admitted turn did not resolve the ask: %+v", got)
	}
	// A turn in the same second as the settle does not resolve it (the settle
	// second is read at its upper bound); a later owner turn does.
	if got := fx.attentionNow(t, now, []store.SessionTurnObservation{started(settle)}, nil); got.AskCount != 1 {
		t.Fatalf("a same-second turn resolved the ask: %+v", got)
	}
	if got := fx.attentionNow(t, now, []store.SessionTurnObservation{started(settle + 5_000)}, nil); got.AskCount != 0 {
		t.Fatalf("the owner's turn did not resolve the ask: %+v", got)
	}
}

// RT-4 / AC-6: a failing ask read degrades only the ask fields; a pending
// approval still shows.
func TestFailingAskReadKeepsTheApproval(t *testing.T) {
	foldHarness(t)
	defer swapSessionStatusRefold(func(string, string) {})()
	approvals = newApprovalsHub()
	approvals.pending["appr-1"] = &Approval{ID: "appr-1", Status: "pending", Runtime: "claude", NativeSessionID: "ses-ask"}
	frame, err := foldSessionStatusWith(time.Now().UTC(), "claude", "ses-ask", "ses-ask", &ownerAttentionBatch{err: errors.New("attention read failed")})
	if err != nil {
		t.Fatal(err)
	}
	if frame.Attention != "approval" || frame.Owner.State != "unknown" || frame.Owner.AskCount != 0 {
		t.Fatalf("frame = %+v", frame)
	}
	item := applySessionStatus(sessionactivity.Item{Runtime: "claude", CatalogSessionID: "ses-ask"}, frame)
	if item.AskState != "unknown" || item.Attention != "approval" {
		t.Fatalf("item = %+v", item)
	}
	// No host at all: no ask source, and nothing unknown about it.
	frame, _ = foldSessionStatusWith(time.Now().UTC(), "claude", "ses-ask", "ses-ask", &ownerAttentionBatch{absent: true})
	if frame.Owner != (ownerAttention{}) || frame.Attention != "approval" {
		t.Fatalf("absent source frame = %+v", frame)
	}
}

// RT-3 / AC: a closed session quiet past quiet_seconds keeps its ask on the
// rail: the candidate source adds it, pass after pass, until it resolves.
func TestOwnerAttentionCandidatesKeepParentAndGuardianDistinct(t *testing.T) {
	now := time.Now()
	parent := SessionSummary{Runtime: "codex", ID: "parent-rollout", MetaID: "parent", ThreadID: "parent", ResumeID: "parent"}
	child := SessionSummary{Runtime: "codex", ID: "child-rollout", MetaID: "child", ThreadID: "parent", ResumeID: "parent", LineageKind: "native-guardian"}
	for _, sessions := range [][]SessionSummary{{child, parent}, {parent, child}} {
		batch := &ownerAttentionBatch{runs: []store.OwnerAttentionRun{{Runtime: "codex", NativeID: "parent", Action: "ask_owner"}, {Runtime: "codex", NativeID: "child", Action: "ask_owner"}}}
		openChild := []sessionactivity.Item{{Runtime: "codex", CatalogSessionID: child.ID, NativeSessionID: child.MetaID, Presence: "open"}}
		items := mergeOwnerAttentionCandidates(now, sessions, openChild, batch)
		if len(items) != 2 || items[1].CatalogSessionID != parent.ID || items[1].NativeSessionID != parent.MetaID || items[1].Presence != "unknown" {
			t.Fatalf("candidate identity or deduplication: %+v", items)
		}
	}
}

func TestUnresolvedAskKeepsAQuietSessionOnTheRail(t *testing.T) {
	fx, _ := attentionFixture(t, false)
	if _, err := fx.settleClaim(t, "session.turn-ended", `{"action":"ask_owner","message":"Pick one.","citations":[]}`); err != nil {
		t.Fatal(err)
	}
	sessions := []SessionSummary{{Runtime: "managed-fixture", ID: fx.sessionID, ResumeID: fx.sessionID}}
	for pass := 0; pass < 2; pass++ {
		now := time.Now().Add(time.Duration(pass+1) * 10 * time.Minute)
		items := mergeOwnerAttentionCandidates(now, sessions, nil, readOwnerAttentionBatch(now))
		if len(items) != 1 || items[0].CatalogSessionID != fx.sessionID {
			t.Fatalf("pass %d: items = %+v", pass+1, items)
		}
		decorated := decorateSessionStatusWith(now, items, readOwnerAttentionBatch(now))
		if decorated[0].AskCount != 1 || decorated[0].AskText != "Pick one." {
			t.Fatalf("pass %d: frame = %+v", pass+1, decorated[0])
		}
	}
	// An item already on the rail is not added twice.
	present := []sessionactivity.Item{{Runtime: "managed-fixture", CatalogSessionID: fx.sessionID, NativeSessionID: fx.sessionID}}
	if items := mergeOwnerAttentionCandidates(time.Now(), sessions, present, readOwnerAttentionBatch(time.Now())); len(items) != 1 {
		t.Fatalf("duplicate candidate: %+v", items)
	}
}

// §7 / AC-7: an ask notifies once from the settle path; a replay, a restart
// and every fold send nothing; ask_notify=false and a draft send nothing.
func TestAskNotifiesOnceFromTheSettlePath(t *testing.T) {
	fx, notified := attentionFixture(t, false)
	ask, err := fx.settleClaim(t, "session.turn-ended", `{"action":"ask_owner","message":"The owner needs to choose the deploy window.","citations":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(*notified) != 1 || !strings.Contains((*notified)[0], "The owner needs to choose the deploy window.") ||
		!strings.HasPrefix((*notified)[0], "Design helper: ") {
		t.Fatalf("notifications = %q", *notified)
	}
	// Replay of the terminal event: the run is settled, nothing re-sends.
	if err := fx.host.finishManagedChild(ask, TaskEvent{TaskID: ask.ChildTaskID, Kind: "task.completed"}); err != nil {
		t.Fatal(err)
	}
	// A new process meets the store, and the sampler folds it twice.
	restarted, err := newOrchestrationManagedHost(fx.host.ix, fx.owner, nil)
	if err != nil {
		t.Fatal(err)
	}
	restarted.close()
	for i := 0; i < 2; i++ {
		fx.attentionNow(t, time.Now(), nil, nil)
	}
	if len(*notified) != 1 {
		t.Fatalf("replay, restart or fold notified: %q", *notified)
	}
	// A draft never notifies.
	if _, err := fx.settleClaim(t, "session.turn-ended", `{"action":"reply","message":"Go ahead.","citations":[]}`); err != nil {
		t.Fatal(err)
	}
	if len(*notified) != 1 {
		t.Fatalf("a draft notified: %q", *notified)
	}
	// The kill switch for asks.
	config := sessionStreamConfig()
	sessionStreamConfigValue.Attention.AskNotify = false
	t.Cleanup(func() { sessionStreamConfigValue = config })
	if _, err := fx.settleClaim(t, "session.turn-ended", `{"action":"ask_owner","message":"Second ask.","citations":[]}`); err != nil {
		t.Fatal(err)
	}
	if len(*notified) != 1 {
		t.Fatalf("ask_notify=false notified: %q", *notified)
	}
}

// RT-11 / AC-5: the projection carries the contract's flags; "to review"
// keeps launch_profile and request_interrupt proposals and draft_reply.
func TestClaimAttentionFlagsComeFromTheContract(t *testing.T) {
	cases := []struct {
		action, receipt, class string
		awaits                 bool
	}{
		{"ask_owner", "", orchestration.AttentionAsk, false},
		{"draft_reply", "", orchestration.AttentionDraft, true},
		{"draft_reply", "started", "", false},
		{"reply", "unavailable", orchestration.AttentionDraft, false},
		{"reply", "not_requested", orchestration.AttentionDraft, false},
		{"reply", "started", "", false},
		{"reply", "pending", "", false},
		{"reply", "", "", false},
		{"launch_profile", "not_requested", "", true},
		{"launch_profile", "started", "", false},
		{"request_interrupt", "not_requested", "", true},
		{"send_message", "unavailable", "", false},
		{"advise_user", "", "", false},
		{"no_action", "", "", false},
	}
	for _, tc := range cases {
		class, awaits := orchestration.ClaimAttention(tc.action, tc.receipt)
		if class != tc.class || awaits != tc.awaits {
			t.Errorf("ClaimAttention(%s, %q) = %q/%v, want %q/%v", tc.action, tc.receipt, class, awaits, tc.class, tc.awaits)
		}
	}
	run := store.ManagedRun{State: "completed", Action: "reply", Detail: map[string]any{"delivery": map[string]any{"state": "unavailable"}}}
	if class, _ := runAttention(run); class != orchestration.AttentionDraft {
		t.Fatalf("projection class = %q", class)
	}
	run.Kind = "reply"
	if class, _ := runAttention(run); class != "" {
		t.Fatalf("a resume child run was classified: %q", class)
	}
}

// AC-9: the action vocabulary has one home. No browser module but the named
// display-label map, and no store SQL, names an attention-bearing action;
// the attention code names no flow, stage or tag.
func TestOwnerAttentionVocabularyHasOneHome(t *testing.T) {
	// "reply" is also a run kind and an authority name, so the scan names the
	// actions no other vocabulary shares.
	actions := []string{}
	for _, action := range append(orchestration.OwnerAttentionActions(), "launch_profile", "request_interrupt") {
		if action != "reply" {
			actions = append(actions, action)
		}
	}
	allowed := map[string]bool{"orchestration/agents/roster-model.js": true}
	// Pre-existing operator-confirmation controls dispatch by action (the
	// panel's Send/Act buttons); they are not attention classification.
	dispatch := map[string]bool{"orchestration/session-agents-panel.js": true}
	root := filepath.Join("static", "js")
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".js") {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if allowed[filepath.ToSlash(rel)] || dispatch[filepath.ToSlash(rel)] {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, action := range actions {
			if strings.Contains(string(raw), "'"+action+"'") || strings.Contains(string(raw), `"`+action+`"`) {
				t.Errorf("%s names the action %q; read the projection's flags instead", rel, action)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	storeFiles, _ := filepath.Glob(filepath.Join("..", "..", "store", "*.go"))
	for _, path := range storeFiles {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		raw, _ := os.ReadFile(path)
		for _, action := range actions {
			if strings.Contains(string(raw), "'"+action+"'") {
				t.Errorf("%s compiles the action %q into SQL", path, action)
			}
		}
	}
	workflow := regexp.MustCompile(`(?i)\b(flow|stage|continue-helper|in-stock|building)\b`)
	for _, path := range []string{"session_owner_attention.go", filepath.Join("static", "js", "task", "session-status.js")} {
		raw, _ := os.ReadFile(path)
		for _, line := range strings.Split(string(raw), "\n") {
			code := strings.TrimSpace(line)
			if strings.HasPrefix(code, "//") {
				continue
			}
			if workflow.MatchString(code) {
				t.Errorf("%s carries workflow vocabulary: %s", path, code)
			}
		}
	}
}

// The pass's batch is indexed once by runtime + root id: an item matches its
// runs by catalog or native id, newest first, never another runtime's.
func TestOwnerAttentionBatchMatchesEitherIdentity(t *testing.T) {
	batch := &ownerAttentionBatch{runs: []store.OwnerAttentionRun{
		{SettledSeq: 9, Runtime: "rt-a", CatalogID: "cat-1", NativeID: "nat-1"},
		{SettledSeq: 8, Runtime: "rt-a", CatalogID: "", NativeID: "nat-1"},
		{SettledSeq: 7, Runtime: "rt-b", CatalogID: "cat-1", NativeID: "nat-1"},
		{SettledSeq: 6, Runtime: "rt-a", CatalogID: "cat-2", NativeID: "cat-2"},
	}}
	ids := func(runs []store.OwnerAttentionRun) []int64 {
		out := []int64{}
		for _, run := range runs {
			out = append(out, run.SettledSeq)
		}
		return out
	}
	if got := ids(batch.runsFor("rt-a", "cat-1", "nat-1")); len(got) != 2 || got[0] != 9 || got[1] != 8 {
		t.Fatalf("catalog+native match = %v", got)
	}
	if got := ids(batch.runsFor("rt-a", "", "nat-1")); len(got) != 2 {
		t.Fatalf("native-only match = %v", got)
	}
	if got := ids(batch.runsFor("rt-a", "cat-2", "cat-2")); len(got) != 1 || got[0] != 6 {
		t.Fatalf("same catalog and native id = %v", got)
	}
	if got := ids(batch.runsFor("rt-c", "cat-1", "nat-1")); len(got) != 0 {
		t.Fatalf("another runtime matched: %v", got)
	}
}

// Code red-team: the ask id is the settle order, not admission order — an
// ask admitted first but settled second is newer, so acknowledging the
// first-settled ask never hides it.
func TestAskIDFollowsSettleOrder(t *testing.T) {
	fx, _ := attentionFixture(t, false)
	first := fx.admitClaim(t, "session.turn-ended", `{"action":"ask_owner","message":"Admitted first.","citations":[]}`)
	second := fx.admitClaim(t, "session.turn-ended", `{"action":"ask_owner","message":"Admitted second.","citations":[]}`)
	if _, err := fx.finish(t, second); err != nil {
		t.Fatal(err)
	}
	early := fx.attentionNow(t, time.Now().Add(time.Second), nil, nil)
	if _, err := fx.finish(t, first); err != nil {
		t.Fatal(err)
	}
	late := fx.attentionNow(t, time.Now().Add(time.Second), nil, nil)
	if late.AskID <= early.AskID || late.AskText != "Admitted first." || late.AskCount != 2 {
		t.Fatalf("early=%+v late=%+v", early, late)
	}
}

// Code red-team: without the task read, a turn start cannot be proven the
// owner's, so the ask stays unknown instead of resolving.
func TestAskStaysWhenTheTaskReadFails(t *testing.T) {
	fx, _ := attentionFixture(t, false)
	ask, err := fx.settleClaim(t, "session.turn-ended", `{"action":"ask_owner","message":"Decide.","citations":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(time.Minute)
	turn := []store.SessionTurnObservation{{Kind: "turn.started", ReceivedAtMS: lifecycleInstantMS(ask.CompletedAt) + 5_000}}
	got := foldOwnerAttention(now, "managed-fixture", fx.sessionID, fx.sessionID, readOwnerAttentionBatch(now), turn, errors.New("task list failed"), nil)
	if got.State != "unknown" || got.AskCount != 0 {
		t.Fatalf("a failed task read resolved or hid the ask as a fact: %+v", got)
	}
}

// G9/G10: candidates are bounded on their own (a rail full of presence items
// never crowds an ask out), and a candidate whose lines the owner already
// resolved is pruned after the pass's single fold.
func TestAttentionCandidatesHavePriorityAndResolvedOnesArePruned(t *testing.T) {
	fx, _ := attentionFixture(t, false)
	ask, err := fx.settleClaim(t, "session.turn-ended", `{"action":"ask_owner","message":"Pick one.","citations":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	sessions := []SessionSummary{{Runtime: "managed-fixture", ID: fx.sessionID, ResumeID: fx.sessionID}}
	full := []sessionactivity.Item{}
	for i := 0; i < sessionActivityConfig().MaxRailSessions; i++ {
		full = append(full, sessionactivity.Item{Runtime: "managed-fixture", CatalogSessionID: fmt.Sprintf("presence-%d", i)})
	}
	now := time.Now().Add(time.Minute)
	batch := readOwnerAttentionBatch(now)
	items := mergeOwnerAttentionCandidates(now, sessions, full, batch)
	if len(items) != len(full)+1 || items[len(items)-1].CatalogSessionID != fx.sessionID {
		t.Fatalf("a full rail crowded the ask out: %d items", len(items))
	}
	// The owner answers: the next pass adds the candidate, folds it once and prunes it.
	motion := store.SessionTurnObservation{Kind: "turn.started", ReceivedAtMS: lifecycleInstantMS(ask.CompletedAt) + 5_000}
	decorated := []sessionactivity.Item{{Runtime: "managed-fixture", CatalogSessionID: fx.sessionID, NativeSessionID: fx.sessionID}}
	decorated[0] = applySessionStatus(decorated[0], sessionStatusFrame{Owner: foldOwnerAttention(now, "managed-fixture", fx.sessionID, fx.sessionID, batch, []store.SessionTurnObservation{motion}, nil, nil)})
	if kept := pruneResolvedAttentionCandidates(decorated, batch); len(kept) != 0 {
		t.Fatalf("a resolved candidate stayed on the rail: %+v", kept)
	}
	// A presence item is never pruned, whatever its attention.
	presence := []sessionactivity.Item{{Runtime: "managed-fixture", CatalogSessionID: "presence-0"}}
	if kept := pruneResolvedAttentionCandidates(presence, batch); len(kept) != 1 {
		t.Fatalf("a presence item was pruned: %+v", kept)
	}
}

// G15: before the daemon finished opening the managed host, asks are
// unknown for the pass, not "none", and nothing latches as a failure.
func TestAsksAreUnknownUntilTheHostOpened(t *testing.T) {
	previousHost, previousResolved := orchestrationManagedHostService(), managedHostResolved.Load()
	managedHostRef.Store(nil)
	managedHostResolved.Store(false)
	t.Cleanup(func() { managedHostRef.Store(previousHost); managedHostResolved.Store(previousResolved) })
	now := time.Now()
	if got := foldOwnerAttention(now, "rt", "s", "s", readOwnerAttentionBatch(now), nil, nil, nil); got.State != "unknown" {
		t.Fatalf("not-ready batch = %+v", got)
	}
	if got := foldOwnerAttention(now, "rt", "s", "s", nil, nil, nil, nil); got.State != "unknown" {
		t.Fatalf("not-ready single read = %+v", got)
	}
	setOrchestrationManagedHostService(nil)
	if got := foldOwnerAttention(now, "rt", "s", "s", readOwnerAttentionBatch(now), nil, nil, nil); got != (ownerAttention{}) {
		t.Fatalf("an unavailable host must read as no ask source: %+v", got)
	}
}

// G18: a turn start filed under the parent session by a child the daemon
// launched — a Codex helper thread shares its parent's thread id, a Claude
// sub-agent's hooks carry the parent's session id — falls inside a managed
// child task's window and is never owner motion. A group root recorded under
// the native (thread) id alone still joins an item that also carries its
// catalog (rollout) id.
func TestChildTurnUnderTheParentIdIsNotOwnerMotion(t *testing.T) {
	fx, _ := attentionFixture(t, false)
	ask, err := fx.settleClaim(t, "session.turn-ended", `{"action":"ask_owner","message":"Decide.","citations":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	settle := lifecycleInstantMS(ask.CompletedAt)
	now := time.UnixMilli(settle + 60_000)
	// The helper's own child task, listed under the parent's id while it ran.
	child := RuntimeTask{ID: ask.ChildTaskID, NativeSessionID: fx.sessionID, Lifecycle: TaskRunning, CreatedAt: settle + 1_000}
	turn := []store.SessionTurnObservation{{Kind: "turn.started", SessionID: fx.sessionID, ReceivedAtMS: settle + 5_000}}
	if got := fx.attentionNow(t, now, turn, []RuntimeTask{child}); got.AskCount != 1 {
		t.Fatalf("a child's turn under the parent id resolved the ask: %+v", got)
	}
	batch := &ownerAttentionBatch{runs: []store.OwnerAttentionRun{{SettledSeq: 1, Runtime: "codex-like", NativeID: "thread-1"}}}
	if runs := batch.runsFor("codex-like", "rollout-stem", "thread-1"); len(runs) != 1 {
		t.Fatalf("a thread-rooted group did not join its rollout item: %+v", runs)
	}
}
