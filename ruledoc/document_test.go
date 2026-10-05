package ruledoc

import (
	"errors"
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
	if len(safety.Rules) != 6 || len(security.Rules) != 7 {
		t.Fatalf("catalog counts safety=%d security=%d, want 6 and 7 (team-link-change joined both, 2026-09-25)", len(safety.Rules), len(security.Rules))
	}
	for _, rule := range safety.Rules {
		if strings.HasPrefix(rule.ID, "observe-") {
			t.Fatalf("safety starter contains observation %q", rule.ID)
		}
	}
	if got := decide(safety, "echo "+CanaryMarker).Rule; got != "canary-deny" {
		t.Fatalf("safety canary fired %q", got)
	}
	// The team-link tripwire: the verbs it names are asked about, and the cost is a
	// recorded fact — prose that quotes the verb (a commit message) also fires.
	// That false positive is accepted: the rule is an ask, a person answers "yes"
	// once, and the alternative (prose-shaped exceptions) is a bypassable parser.
	if got := decide(safety, "crossing-guard link https://team.example.com").Rule; got != "team-link-change" {
		t.Fatalf("team-link tripwire did not fire on the link verb: %q", got)
	}
	if got := decide(safety, "crossing-guard layers --adopt organization").Rule; got != "team-link-change" {
		t.Fatalf("team-link tripwire did not fire on the layers verb (3c): %q", got)
	}
	if got := decide(safety, "curl -X POST http://127.0.0.1:7788/api/team/layers/adopt").Rule; got != "team-link-change" {
		t.Fatalf("team-link tripwire did not fire on the adopt route (3c): %q", got)
	}
	if got := decide(safety, "git commit -m \"add crossing-guard link verb docs\"").Rule; got != "team-link-change" {
		t.Fatal("team-link tripwire no longer fires on quoted prose — if the cost was narrowed away, this pin must change WITH a recorded decision, not silently")
	}
	// The rest of the team release (criterion 83): publishing, the organization key,
	// re-pin and handoff verbs and routes are asked about in all three catalogs.
	defaults, err := Parse(DefaultRules())
	if err != nil {
		t.Fatal(err)
	}
	asked := []string{
		"crossing-guard bundle build --scope organization --agent reviewer",
		"crossing-guard bundle sign --key /keys/acme.key /data/bundles/organization-r2.json",
		"crossing-guard org-key init --out /keys/acme.key",
		"crossing-guard org-key show --key /keys/acme.key",
		"crossing-guard layers --repin ABCD-EFGH-JKMN-PQRS-TVWX",
		"crossing-guard layers --unadopt organization",
		"crossing-guard handoff send --to usr_1",
		"crossing-guard handoff  withdraw hnd_1",
		"curl -X POST http://127.0.0.1:7788/api/team/org-key/repin -d '{}'",
		"curl -X POST http://127.0.0.1:7788/api/v1/team/org-key/repin -d '{}'",
		"curl -X POST http://127.0.0.1:7788/api/team/bundles/build -d '{}'",
		"curl -X POST http://127.0.0.1:7788/api/v1/team/bundles/build -d '{}'",
		"curl -X POST http://127.0.0.1:7788/api/team/handoffs/send -d '{}'",
		"curl -X POST http://127.0.0.1:7788/api/v1/team/handoffs/send -d '{}'",
		"curl -X POST http://127.0.0.1:7788/api/team/handoffs/hnd_01ABC/withdraw",
		"curl -X POST http://127.0.0.1:7788/api/v1/team/handoffs/hnd_01ABC/withdraw",
		"curl -X POST http://127.0.0.1:7788/api/team/layers/unadopt",
		"curl -X POST http://127.0.0.1:7788/api/team/unlink",
		// Memory recall on or off in a runtime's settings (owner decision 2026-10-04):
		// the one route carries both actions, so attach and detach are both asked
		// about, in both spellings, and so is the command-line verb that does the
		// same write.
		`curl -X POST http://127.0.0.1:7788/api/memory/attach -d '{"runtime":"codex","action":"attach","consent":true}'`,
		`curl -X POST http://127.0.0.1:7788/api/memory/attach -d '{"runtime":"codex","action":"detach","consent":true}'`,
		`curl -X POST http://127.0.0.1:7788/api/v1/memory/attach -d '{"runtime":"claude","action":"attach","consent":true}'`,
		`curl -X POST http://127.0.0.1:7788/api/v1/memory/attach -d '{"runtime":"claude","action":"detach","consent":true}'`,
		"crossing-guard attach claude",
		"crossing-guard  attach codex --config /tmp/config.toml",
		// What may follow the verb or the route and still be it: the end, a space,
		// a query, a quote, or a shell operator. Leaving these out would be a way
		// around the ask.
		"crossing-guard attach",
		"sh -c 'crossing-guard attach'",
		"crossing-guard attach; true",
		"curl -X POST -d '{}' http://127.0.0.1:7788/api/memory/attach?x=1",
		"curl -X POST -d '{}' 'http://127.0.0.1:7788/api/memory/attach'",
		`curl -X POST -d '{}' "http://127.0.0.1:7788/api/v1/memory/attach"`,
		"curl -X POST -d '{}' http://127.0.0.1:7788/api/memory/attach;true",
		"echo $(curl -X POST -d '{}' http://127.0.0.1:7788/api/memory/attach)",
	}
	for name, catalog := range map[string]*engine.Policy{"default": defaults, "safety starter": safety, "security observe": security} {
		for _, command := range asked {
			if got := decide(catalog, command); got.Rule != "team-link-change" || got.Mode != engine.ConfirmAndRecord {
				t.Fatalf("%s catalog: the tripwire did not ask on %q: %+v", name, command, got)
			}
		}
		// Reads and unrelated verbs are not asked about.
		for _, command := range []string{
			"curl http://127.0.0.1:7788/api/team",
			"curl http://127.0.0.1:7788/api/team/handoffs",
			"curl http://127.0.0.1:7788/api/team/handoffs/hnd_01ABC",
			"crossing-guard handoff list",
			"crossing-guard doctor",
			"crossing-guard rules",
			// The memory routes that write no runtime's settings, and the memory
			// verbs, are not the attach route or the attach verb.
			"curl http://127.0.0.1:7788/api/memory/search?q=attach",
			"curl http://127.0.0.1:7788/api/memory/records?status=active",
			"curl -X POST http://127.0.0.1:7788/api/memory/attachments",
			"curl -X POST http://127.0.0.1:7788/api/team/memory/share -d '{}'",
			"crossing-guard memory search attach",
			"crossing-guard memory list",
			"crossing-guard attachments",
			// Something longer that merely starts with the verb or the route, and a
			// file that happens to sit under a directory named like the route.
			"crossing-guard attach-anything",
			"crossing-guard attach/x",
			"crossing-guard attach.md",
			"curl -X POST http://127.0.0.1:7788/api/memory/attach-all",
			"curl -X POST http://127.0.0.1:7788/api/memory/attach/x",
			"curl -X POST http://127.0.0.1:7788/api/v1/memory/attach/x",
			"curl http://127.0.0.1:7788/api/memory/attach.md",
			"cat docs/api/memory/attach.md",
			"cat docs/api/memory/attach/notes.txt",
		} {
			if got := decide(catalog, command).Rule; got == "team-link-change" {
				t.Fatalf("%s catalog: the tripwire fired on %q", name, command)
			}
		}
	}
	// The cost of matching a path: reading the recall state (GET on the same path)
	// is asked about too. A command line does not say its method reliably, the rule
	// is an ask, and a narrower pattern would be one an agent could step around.
	if got := decide(safety, "curl http://127.0.0.1:7788/api/memory/attach").Rule; got != "team-link-change" {
		t.Fatalf("the attach path's read stopped asking — if that was narrowed, record the decision with it: %q", got)
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

// A rule that reads a route: fact beside a command, tool or state term is refused when
// the document is parsed — the one path every written document takes (OD-25).
func TestParseRefusesARuleThatMixesRouteAndOtherTerms(t *testing.T) {
	mixed := `{"rules":[{"id":"mixed","action":"deny","if":{"all":[{"tag":"route:local","value":"false"},{"tag":"command","matches":"push"}]}}]}`
	_, err := Parse([]byte(mixed))
	var refused *engine.MixedRouteRuleError
	if !errors.As(err, &refused) || refused.RuleID != "mixed" || refused.Key != "command" {
		t.Fatalf("mixed route rule was not refused by name: %v", err)
	}
	pure := `{"rules":[{"id":"route-only","action":"deny","if":{"all":[{"tag":"route:local","value":"false"},{"tag":"profile:locality","value":"local-only"}]}}]}`
	if _, err := Parse([]byte(pure)); err != nil {
		t.Fatalf("a pure route rule must parse: %v", err)
	}
}
