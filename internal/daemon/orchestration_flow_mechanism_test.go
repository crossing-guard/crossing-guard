package daemon

// Focused tests for the orchestration-flows pilot's mechanisms (plan §7):
// the grant's conformance check, the durable ceiling, the flows.json
// owner's validation, and the WIP fact's author. The host-fixture cases
// (membership, enablement, live signals) live in the host test file.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crossing-guard/store"
)

// grantShape is a valid profile declaration for the conformance cases.
func grantShape(maxDeliveries int64, replyClass string, replyBytes int) FlowStageProfile {
	return FlowStageProfile{ProfileID: "continue-helper", MaxDeliveries: maxDeliveries,
		DeadlineSeconds: 0, ReplyClass: replyClass, ReplyClassBytes: replyBytes}
}

func TestFlowGrantRefusedForNonConformingReplies(t *testing.T) {
	grant := grantShape(3, "short-ack", 64)
	cases := []struct {
		reply string
		want  string
	}{
		{"", ""},
		{"ok", ""},
		{strings.Repeat("a", 65), "reply_exceeds_declared_max_bytes"},
		// The fence rule is CONFIG now (RefuseFencedBlocks, default true —
		// grantShape sets it false here to test the byte bound alone).
		{"a ``` code block arrives", ""},
		{"plain text reply", ""},
	}
	for _, tc := range cases {
		if got := flowGrantRefused(grant, tc.reply); got != tc.want {
			t.Errorf("flowGrantRefused(%q) = %q, want %q", tc.reply, got, tc.want)
		}
	}
	// The class that refuses fences does; the one that allows them does not.
	fenced := grantShape(3, "strict", 64)
	fenced.RefuseFencedBlocks = true
	if got := flowGrantRefused(fenced, "run ```make test``` now"); got != "reply_carries_fenced_block" {
		t.Fatalf("fence-refusing class accepted a code block: %q", got)
	}
}

func TestFlowGrantRefusalIsStructuralNeverBlessedWords(t *testing.T) {
	// The owner's separation line, postwork fold: NO word list exists in
	// framework code — approval protection is flowPendingApprovalFloor, an
	// observed-state check. A reply may say anything, including approval
	// words in ordinary prose, and conformance never refuses it for its
	// vocabulary: a reply containing "the plan allows retries" is not an
	// approval answer; a PENDING APPROVAL is, and the floor reads that
	// state, not prose.
	grant := grantShape(3, "any-short", 64)
	for _, reply := range []string{
		"continue with the plan",
		"proceed with the work",
		"the deploy is approved by tests",
		"we deny that assumption",
		"permission levels look fine",
	} {
		if got := flowGrantRefused(grant, reply); got != "" {
			t.Fatalf("plain reply refused by word matching: %q -> %q", reply, got)
		}
	}
	// The floor itself is exercised in the host fixture suite; here the
	// structural assertion is that no refusal reason names vocabulary.
	for _, reason := range []string{flowGrantRefused(grant, "anything at all")} {
		if strings.Contains(reason, "approval_vocabulary") {
			t.Fatalf("vocabulary refusal still exists: %q", reason)
		}
	}
}

// ceilingProbe drives the real ceiling check a delivery makes, then counts
// the delivery the way the send path does once it started (independent
// red-team G12: never a hand copy of the check).
func ceilingProbe(ix *store.Index, grant flowGrant, runtime, sessionID, reply string, now int64) (string, error) {
	host := &orchestrationManagedHost{ix: ix}
	refused, err := host.flowCeilingCheck(grant, runtime, sessionID, reply, now)
	if err != nil || refused != "" {
		return refused, err
	}
	return "", ix.CountFlowDelivery(grant.FlowID, runtime, sessionID, flowReplyDigest(reply), now)
}

func TestFlowCeilingTripsAndStaysBreached(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	grant := flowGrant{FlowID: "flow1", Profile: grantShape(2, "short", 64)}
	if err := ix.EnableFlow("flow1", []byte(`{"id":"flow1"}`), 100); err != nil {
		t.Fatal(err)
	}
	if err := ix.ArmFlowBinding("flow1", "building", "claude", "s1", "continue-helper", 100); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		refused, err := ceilingProbe(ix, grant, "claude", "s1", "reply-"+string(rune('a'+i)), time.Now().Unix())
		if err != nil || refused != "" {
			t.Fatalf("delivery %d refused (%q, %v)", i+1, refused, err)
		}
	}
	// Durability: close, reopen the SAME file, and the counters survive.
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}
	ix, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	refused, err := ceilingProbe(ix, grant, "claude", "s1", "reply-c", time.Now().Unix())
	if err != nil || refused != "flow_ceiling_breach" {
		t.Fatalf("third delivery = refused %q, err %v; want flow_ceiling_breach", refused, err)
	}
	// Breached stays breached: even a fresh reply digest is refused.
	refused, err = ceilingProbe(ix, grant, "claude", "s1", "reply-d", time.Now().Unix())
	if err != nil || refused != "flow_ceiling_breach" {
		t.Fatalf("post-breach delivery = refused %q, err %v; want flow_ceiling_breach", refused, err)
	}
}

func TestFlowCeilingNoopDetectorTrips(t *testing.T) {
	ix := openFlowBoundaryIndex(t)
	grant := flowGrant{FlowID: "flow2", Profile: grantShape(5, "short", 64)}
	if err := ix.EnableFlow("flow2", []byte(`{"id":"flow2"}`), 100); err != nil {
		t.Fatal(err)
	}
	if err := ix.ArmFlowBinding("flow2", "building", "claude", "s2", "continue-helper", 100); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if refused, _ := ceilingProbe(ix, grant, "claude", "s2", "same reply", now); refused != "" {
		t.Fatalf("first delivery refused: %q", refused)
	}
	// The same reply again with no intervening work: no-op breach.
	refused, _ := ceilingProbe(ix, grant, "claude", "s2", "same reply", now+10)
	if refused != "flow_ceiling_noop_breach" {
		t.Fatalf("no-op delivery = refused %q; want flow_ceiling_noop_breach", refused)
	}
}

func TestFlowsConfigValidationRejectsUnboundedGrants(t *testing.T) {
	dir := t.TempDir()
	// An absent file is an empty list; a fresh data dir yields zero flows.
	if document := loadFlows(dir); len(document.Flows) != 0 || len(document.Rejected) != 0 {
		t.Fatalf("fresh dir = %+v", document)
	}
	// A valid flow passes.
	valid := SavedFlow{ID: "pilot", Name: "Pilot", MembershipTags: []string{"flow:building"},
		Stages: []FlowStage{{ID: "building", Name: "Building", Membership: "tag:phase=plan",
			Profiles: []FlowStageProfile{{ProfileID: "continue-helper", MaxDeliveries: 3, ReplyClass: "short-ack", ReplyClassBytes: 64}}}}}
	if err := validateSavedFlow(valid); err != nil {
		t.Fatal(err)
	}
	if err := writeFlows(flowsPath(dir), []SavedFlow{valid}); err != nil {
		t.Fatal(err)
	}
	if document := loadFlows(dir); len(document.Flows) != 1 {
		t.Fatalf("valid flow rejected: %+v", document)
	}
	// No ceiling of any kind: refused (plan §1 slice C — unbounded grants
	// are invalid by construction).
	unbounded := valid
	unbounded.Stages[0].Profiles[0] = FlowStageProfile{ProfileID: "continue-helper", ReplyClass: "short-ack", ReplyClassBytes: 64}
	if err := validateSavedFlow(unbounded); err == nil {
		t.Fatal("unbounded grant accepted")
	}
	// A reply class without a byte bound: refused.
	nobound := valid
	nobound.Stages[0].Profiles[0] = FlowStageProfile{ProfileID: "continue-helper", MaxDeliveries: 3, ReplyClass: "short-ack"}
	if err := validateSavedFlow(nobound); err == nil {
		t.Fatal("classless byte bound accepted")
	}
	// A transition to an unknown stage: refused.
	badtransit := valid
	badtransit.Stages[0].Transitions = []FlowStageTransit{{Kind: "session.turn-ended", To: "nowhere"}}
	if err := validateSavedFlow(badtransit); err == nil {
		t.Fatal("transition to unknown stage accepted")
	}
	// A transition kind that is not a catalog kind or owner act: refused —
	// agent claims can never move a card (invariant 2).
	claim := valid
	claim.Stages[0].Transitions = []FlowStageTransit{{Kind: "agent.claim", To: "building"}}
	if err := validateSavedFlow(claim); err == nil {
		t.Fatal("agent-claim transition accepted")
	}
}

func TestFlowConfigFileRoundTripPreservesOpinions(t *testing.T) {
	dir := t.TempDir()
	flow := SavedFlow{ID: "pilot", Name: "Pilot flow", MembershipTags: []string{"flow:building"},
		Stages: []FlowStage{{ID: "building", Name: "Building", Membership: "tag:phase=plan",
			Profiles: []FlowStageProfile{{ProfileID: "continue-helper", MaxDeliveries: 4,
				DeadlineSeconds: 3600, ReplyClass: "short-ack", ReplyClassBytes: 128, DryRun: true}}}}}
	if err := writeFlows(flowsPath(dir), []SavedFlow{flow}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(flowsPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	var loose map[string]json.RawMessage
	if json.Unmarshal(raw, &loose) != nil || string(loose["format_version"]) != "1" {
		t.Fatalf("file = %s", raw)
	}
	document := loadFlows(dir)
	if len(document.Flows) != 1 || document.Flows[0].Stages[0].Profiles[0].DryRun != true {
		t.Fatalf("round trip lost data: %+v", document)
	}
}

func TestUncommittedWorkFactAuthorsOnlyOnNoCoveringCommit(t *testing.T) {
	ix := openFlowBoundaryIndex(t)
	base := func(head string) *store.ChangeRecord {
		return &store.ChangeRecord{ID: 7, BaseRevision: "abc123", HeadRevision: head,
			Items: []store.ChangeItem{{Path: "a.go", Status: "modified"}}}
	}
	// Edits + head == base: the fact lands.
	recordUncommittedWorkFact(ix, "s1", base("abc123"), 200, nil)
	state, err := ix.SessionState("s1")
	if err != nil || len(state) != 1 || state[0].Detector != "checkpoint-settle" {
		t.Fatalf("state = %+v (%v)", state, err)
	}
	// A covering commit (head past base): no fact.
	recordUncommittedWorkFact(ix, "s2", base("def456"), 200, nil)
	state, err = ix.SessionState("s2")
	if err != nil || len(state) != 0 {
		t.Fatalf("committed session gained a fact: %+v (%v)", state, err)
	}
	// No revision evidence: unknown is not WIP.
	recordUncommittedWorkFact(ix, "s3", &store.ChangeRecord{ID: 8, Items: []store.ChangeItem{{Path: "b.go"}}}, 200, nil)
	state, err = ix.SessionState("s3")
	if err != nil || len(state) != 0 {
		t.Fatalf("unknown-revision session gained a fact: %+v (%v)", state, err)
	}
	// No items: an empty settle is not WIP.
	empty := base("abc123")
	empty.Items = nil
	recordUncommittedWorkFact(ix, "s4", empty, 200, nil)
	state, err = ix.SessionState("s4")
	if err != nil || len(state) != 0 {
		t.Fatalf("empty settle gained a fact: %+v (%v)", state, err)
	}
}

// writeFlows is the tests' fixture writer for flows.json (the daemon never
// writes the file). It refuses only an oversized file; the caller is
// responsible for the flows being valid.
func writeFlows(path string, flows []SavedFlow) error {
	if flows == nil {
		flows = []SavedFlow{}
	}
	body, err := json.MarshalIndent(struct {
		FormatVersion int         `json:"format_version"`
		Flows         []SavedFlow `json:"flows"`
	}{flowsFormatVersion, flows}, "", "  ")
	if err != nil {
		return err
	}
	if int64(len(body))+1 > flowsFileBytesMax {
		return fmt.Errorf("%w: the flows file would be larger than %d bytes", errFlowInvalid, flowsFileBytesMax)
	}
	temp := path + ".tmp"
	if err := os.WriteFile(temp, append(body, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(temp, path)
}
