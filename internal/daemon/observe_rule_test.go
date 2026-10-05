package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"crossing-guard/internal/guardcli"
	"crossing-guard/internal/rulebook"
	"crossing-guard/store"
)

func ruleTestGovernor(t *testing.T) (*Governor, *store.Index) {
	t.Helper()
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	return NewGovernor(ix, nil), ix
}

func storedRule(t *testing.T, ix *store.Index, runtime string, afterID int64) string {
	t.Helper()
	events, err := ix.RuntimeEventsAfter(runtime, afterID, 10)
	if err != nil || len(events) == 0 {
		t.Fatalf("no stored event: %v", err)
	}
	return events[len(events)-1].RuleID
}

// The whole point of the field: what the hook decided with reaches the row as data.
func TestIngestStoresTheDecidingRule(t *testing.T) {
	g, ix := ruleTestGovernor(t)
	e := testV1Envelope(t, v1TestRepo(t))
	e.Decision, e.Reason, e.Rule = "deny", "Enforced by rule deny-alpha: alpha is restricted — denied.", "deny-alpha"
	if _, err := ingestObservationV1(t.Context(), g, e); err != nil {
		t.Fatal(err)
	}
	if got := storedRule(t, ix, "claude", 0); got != "deny-alpha" {
		t.Fatalf("rule_id = %q", got)
	}
	canaries, err := ix.RuntimeCanaries("deny-alpha")
	if err != nil || canaries["claude"].TS != e.TS {
		t.Fatalf("a live deny by a rule is queryable per runtime: %+v err=%v", canaries, err)
	}
}

// Upgrade window: the new hook binary is in place before the daemon reloads. The old
// daemon ignores the unknown field and commits with a digest that excludes it. If that
// acknowledgement is lost, the spool file replays into the NEW daemon, whose digest
// includes the rule. Same observation — it must be a duplicate, never a collision that
// quarantines a governed action's evidence.
func TestReplayAcrossTheUpgradeIsADuplicateNotACollision(t *testing.T) {
	g, _ := ruleTestGovernor(t)
	repo := v1TestRepo(t)
	asOldDaemonSawIt := testV1Envelope(t, repo)
	asOldDaemonSawIt.Decision = "deny"
	first, err := ingestObservationV1(t.Context(), g, asOldDaemonSawIt)
	if err != nil {
		t.Fatal(err)
	}
	replayed := asOldDaemonSawIt
	replayed.Rule = "deny-alpha"
	replayed.DeliveryAttempts, replayed.DeliveryMode = 2, "replay"
	second, err := ingestObservationV1(t.Context(), g, replayed)
	if err != nil {
		t.Fatalf("a digest that differs only by the rule is the same observation: %v", err)
	}
	if !second.Duplicate || second.EventID != first.EventID {
		t.Fatalf("want a duplicate of event %d: %+v", first.EventID, second)
	}

	// Any OTHER difference under the same observation id is still a collision.
	forged := replayed
	forged.Decision = "allow"
	if _, err := ingestObservationV1(t.Context(), g, forged); !errors.Is(err, ErrObservationCollision) {
		t.Fatalf("a changed decision must still collide, got %v", err)
	}
}

// The relaxation is one-way. Once a rule is stored, a replay WITHOUT it is not the same
// observation: a replay must never be able to strip attribution from a recorded block.
func TestAReplayCannotStripARecordedRule(t *testing.T) {
	g, _ := ruleTestGovernor(t)
	withRule := testV1Envelope(t, v1TestRepo(t))
	withRule.Decision, withRule.Rule = "deny", "deny-alpha"
	if _, err := ingestObservationV1(t.Context(), g, withRule); err != nil {
		t.Fatal(err)
	}
	stripped := withRule
	stripped.Rule = ""
	stripped.DeliveryAttempts, stripped.DeliveryMode = 2, "replay"
	if _, err := ingestObservationV1(t.Context(), g, stripped); !errors.Is(err, ErrObservationCollision) {
		t.Fatalf("stripping the rule must collide, got %v", err)
	}
}

// maxStoredRuleID mirrors the wire schema's bound; this pins the two to each other so the
// schema cannot move without this constant.
func TestStoredRuleBoundIsTheWireSchemaBound(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "schemas", "event.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	node := schema
	for _, key := range []string{"properties", "payload", "properties", "rule"} {
		next, ok := node[key].(map[string]any)
		if !ok {
			t.Fatalf("event.schema.json has no %s under payload.rule's path", key)
		}
		node = next
	}
	want, _ := node["maxLength"].(float64)
	if want == 0 || int(want) != maxStoredRuleID {
		t.Fatalf("event.schema.json payload.rule maxLength = %v, maxStoredRuleID = %d", want, maxStoredRuleID)
	}
}

// A user rule placed before canary-deny that also matches the marker denies the proof
// command — by the wrong rule. The watch could never recognize that in a stored event, so
// it must not advertise a canary.
func TestAShadowedCanaryRuleIsNotProvable(t *testing.T) {
	if canaryProvable(guardcli.Verdict{Decision: "deny", Rule: "my-own-deny"}) {
		t.Fatal("a deny by another rule is not a provable canary")
	}
	if canaryProvable(guardcli.Verdict{Decision: "allow", Rule: ""}) {
		t.Fatal("an allow is not a canary")
	}
	if !canaryProvable(guardcli.Verdict{Decision: "deny", Rule: rulebook.CanaryRuleID}) {
		t.Fatal("the proof rule denying is exactly the provable case")
	}
}

// The wire bounds a rule id at 255. A longer one is stored as unknown: a truncated id
// would name a rule that does not exist.
func TestAnOverlongRuleIsStoredAsUnknownNeverTruncated(t *testing.T) {
	g, ix := ruleTestGovernor(t)
	e := testV1Envelope(t, v1TestRepo(t))
	e.Decision, e.Rule = "deny", strings.Repeat("r", maxStoredRuleID+1)
	if _, err := ingestObservationV1(t.Context(), g, e); err != nil {
		t.Fatalf("an over-long rule must not cost the governed event itself: %v", err)
	}
	if got := storedRule(t, ix, "claude", 0); got != "" {
		t.Fatalf("rule_id = %q, want empty", got)
	}
}
