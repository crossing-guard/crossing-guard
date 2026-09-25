package ruledoc

import (
	"strings"
	"testing"

	"crossing-guard/engine"
)

func decide(policy *engine.Policy, command string) engine.Decision {
	return engine.Decide([]engine.Tag{{Key: engine.CommandTagKey, Value: command}}, policy)
}

func TestLegacyAndMixedRulesMigratePerRule(t *testing.T) {
	raw := []byte(`{"rules":[
      {"id":"new-deny","action":"deny","if":{"tag":"command","matches":"alpha"}},
      {"id":"old-ask","action":"ask","guards":[{"name":"b","pattern":"bravo"}]}
    ]}`)
	policy, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := decide(policy, "alpha").Mode; got != engine.HardBlock {
		t.Fatalf("new-format rule mode = %s, want hard block", got)
	}
	if got := decide(policy, "bravo").Mode; got != engine.ConfirmAndRecord {
		t.Fatalf("legacy rule mode = %s, want confirm", got)
	}
	if _, err := Parse([]byte(`{"rules":[{"id":"x","action":"deny","guards":[{"name":"n","pattern":"a(b"}]}]}`)); err == nil {
		t.Fatal("bad legacy regex did not fail loudly")
	}
}

func TestPublicCatalogDocumentsAreBoundedAndValid(t *testing.T) {
	safety, err := Parse(SafetyStarterRules())
	if err != nil {
		t.Fatal(err)
	}
	security, err := Parse(SecurityObserveRules())
	if err != nil {
		t.Fatal(err)
	}
	if len(safety.Rules) != 5 || len(security.Rules) != 6 {
		t.Fatalf("catalog counts safety=%d security=%d, want 5 and 6", len(safety.Rules), len(security.Rules))
	}
	for _, rule := range safety.Rules {
		if strings.HasPrefix(rule.ID, "observe-") {
			t.Fatalf("safety starter contains observation %q", rule.ID)
		}
	}
	if got := decide(safety, "echo "+CanaryMarker).Rule; got != "canary-deny" {
		t.Fatalf("safety canary fired %q", got)
	}
	credentialRule := security.Rules[len(security.Rules)-1]
	if credentialRule.ID != "observe-credential-external-egress" || credentialRule.Action != "observe" {
		t.Fatalf("security-only rule = %+v", credentialRule)
	}
	if !engine.Match(credentialRule.If, []engine.Tag{
		{Key: "session:data-class", Value: "credential-material"},
		{Key: "session:destination-class", Value: "external"},
	}) {
		t.Fatal("security observation missed its exact positive facts")
	}
}

// ContentDigest is the identity every selection, adoption, and bundle keys on; it must
// be stable and byte-sensitive.
func TestContentDigestIsStableAndByteSensitive(t *testing.T) {
	a := ContentDigest([]byte(`{"rules":[]}`))
	if a != ContentDigest([]byte(`{"rules":[]}`)) || !strings.HasPrefix(a, "sha256:") || len(a) != len("sha256:")+64 {
		t.Fatalf("digest shape/stability: %q", a)
	}
	if a == ContentDigest([]byte(`{"rules": []}`)) {
		t.Fatal("digest ignored a byte change")
	}
}

func TestMechanicalDiffIsSortedAndMechanical(t *testing.T) {
	before, _ := Parse([]byte(`{"rules":[
      {"id":"b","action":"deny","if":{"tag":"command","matches":"x"}},
      {"id":"a","action":"ask","if":{"tag":"command","matches":"y"}},
      {"id":"gone","action":"deny","if":{"tag":"command","matches":"z"}}]}`))
	after, _ := Parse([]byte(`{"rules":[
      {"id":"b","action":"deny","if":{"tag":"command","matches":"x"}},
      {"id":"a","action":"deny","if":{"tag":"command","matches":"y"}},
      {"id":"new","action":"observe","if":{"tag":"command","matches":"w"}}]}`))
	added, removed, changed := MechanicalDiff(before, after)
	if len(added) != 1 || added[0] != "new" || len(removed) != 1 || removed[0] != "gone" {
		t.Fatalf("added=%v removed=%v", added, removed)
	}
	if len(changed) != 1 || changed[0].ID != "a" || changed[0].BeforeAction != "ask" || changed[0].AfterAction != "deny" ||
		changed[0].BeforePredicate != changed[0].AfterPredicate {
		t.Fatalf("changed=%+v", changed)
	}
	if got := PolicySummary(after); got != "deny=2, observe=1" {
		t.Fatalf("summary %q", got)
	}
}
