package guardcli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"crossing-guard/engine"
	"crossing-guard/internal/platform"
	"crossing-guard/internal/rulebook"
)

// coverageHome isolates a `coverage` run: a scratch HOME (rulebook, store, detector
// selection) and no invocation file unless a test names one.
func coverageHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, k := range []string{"CG_INDEX", "CPMEM_INDEX", "CG_RULES", "CG_DETECTORS"} {
		t.Setenv(k, "")
	}
	t.Setenv("CG_POLICY", filepath.Join(home, "absent-policy.json"))
	return home
}

func writeRules(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runCoverage(t *testing.T, cwd string) string {
	t.Helper()
	var out bytes.Buffer
	if err := writeCoverage(&out, cwd); err != nil {
		t.Fatalf("coverage: %v", err)
	}
	return out.String()
}

// ruleBlock is one rule's printed block: its header line through its last term row.
func ruleBlock(t *testing.T, out, id string) string {
	t.Helper()
	i := strings.Index(out, "\n"+id+" ")
	if i < 0 && strings.HasPrefix(out, id+" ") {
		i = -1
	} else if i < 0 {
		t.Fatalf("rule %s not labeled:\n%s", id, out)
	}
	rest := out[i+1:]
	lines := strings.SplitAfter(rest, "\n")
	block := lines[0]
	for _, l := range lines[1:] {
		if !strings.HasPrefix(l, "    ") && !strings.HasPrefix(l, "      ") {
			break
		}
		block += l
	}
	return block
}

// An install with no invocation file is the normal case. `coverage` used to exit 1 there
// ("load policy: <nil>") and never label the rulebook the hook and the stateful tier
// load (stateful-tier reach plan RT-3).
func TestCoverageLabelsTheRulebookWithoutAnInvocationFile(t *testing.T) {
	home := coverageHome(t)
	out := runCoverage(t, home)
	if !strings.HasPrefix(out, "rulebook ") || !strings.Contains(out, "stateful tier: ") || !strings.Contains(out, "  no rules\n") {
		t.Fatalf("fresh install's empty rulebook not reported:\n%s", out)
	}
	if strings.Contains(out, "\ninvocation file") || strings.Contains(out, "[legacy]") {
		t.Fatalf("no invocation section and no legacy block expected:\n%s", out)
	}
}

// The user rulebook's gating state rules label at the stateful tier's reach on this
// host; a CG_POLICY file's state rules label at the not-loaded reach, where a positive
// state term cannot fire (RT-3 + RT-6).
func TestCoverageReachFollowsTheTierThatLoadsTheRule(t *testing.T) {
	home := coverageHome(t)
	rulesPath := filepath.Join(home, ".crossing-guard", "policy", "rules.json")
	writeRules(t, rulesPath, `{"rules":[
	 {"id":"user-positive","action":"deny","if":{"tag":"session:vcs","value":"push-force"}},
	 {"id":"user-negated","action":"ask","if":{"not":{"tag":"agent:b1:reviewed"}}}]}`)
	invocation := filepath.Join(home, "invocation.json")
	writeRules(t, invocation, `{"rules":[
	 {"id":"inv-positive","action":"deny","if":{"tag":"session:vcs","value":"push-force"}},
	 {"id":"inv-command","action":"deny","if":{"tag":"command","matches":"some-command"}}]}`)
	t.Setenv("CG_POLICY", invocation)
	out := runCoverage(t, home)

	userReach := engine.ReachStopUnarmed
	if platform.Current().StatefulEnforcementReady() {
		userReach = engine.ReachStop
	}
	if b := ruleBlock(t, out, "user-positive"); !strings.Contains(b, userReach) {
		t.Errorf("user state rule not at %q:\n%s", userReach, b)
	}
	// Armed, the stateful tier evaluates the negated rule and it can fire; unarmed, no
	// tier evaluates a state rule (the static tiers skip it), so it is INERT.
	armed := platform.Current().StatefulEnforcementReady()
	if b := ruleBlock(t, out, "user-negated"); !strings.Contains(b, userReach) || strings.Contains(b, "INERT") == armed {
		t.Errorf("negated user state rule at %q (armed=%v):\n%s", userReach, armed, b)
	}
	if !strings.Contains(out, "invocation file "+invocation) {
		t.Fatalf("invocation section missing:\n%s", out)
	}
	b := ruleBlock(t, out, "inv-positive")
	if !strings.Contains(b, "INERT") || !strings.Contains(b, engine.ReachStopNotLoaded) || !strings.Contains(b, "state is never read at this reach") {
		t.Errorf("invocation state rule must be INERT at the not-loaded reach:\n%s", b)
	}
	// Only the engine tier loads the invocation file, and it decides over the whole
	// action — the raw command included — so a command rule there can fire.
	if b := ruleBlock(t, out, "inv-command"); !strings.Contains(b, engine.ReachStop) || strings.Contains(b, "INERT") ||
		!strings.Contains(b, "(tiers: hook-engine)") {
		t.Errorf("invocation command rule can fire at the engine tier:\n%s", b)
	}
}

// CG_POLICY naming the rulebook itself is labeled once (red-team RT-9); an unreadable
// invocation file is a note, not a failure — no tier evaluates it (RT-11).
func TestCoverageInvocationFileEdgeCases(t *testing.T) {
	home := coverageHome(t)
	rulesPath := filepath.Join(home, ".crossing-guard", "policy", "rules.json")
	writeRules(t, rulesPath, `{"rules":[{"id":"only","action":"deny","if":{"tag":"command","matches":"x"}}]}`)
	t.Setenv("CG_POLICY", rulesPath)
	out := runCoverage(t, home)
	if !strings.Contains(out, "is the rulebook itself") || strings.Count(out, "\nonly ") != 1 {
		t.Fatalf("same file must be labeled once:\n%s", out)
	}
	broken := filepath.Join(home, "broken.json")
	writeRules(t, broken, `{not json`)
	t.Setenv("CG_POLICY", broken)
	out = runCoverage(t, home)
	if !strings.Contains(out, "invocation file "+broken+" unreadable") || !strings.Contains(out, "only ") {
		t.Fatalf("broken invocation file must be a note:\n%s", out)
	}
}

// A rulebook that cannot load is an error: the hook fails closed on the same file.
func TestCoverageFailsOnAMalformedRulebook(t *testing.T) {
	home := coverageHome(t)
	writeRules(t, filepath.Join(home, ".crossing-guard", "policy", "rules.json"), `{not json`)
	var out bytes.Buffer
	if err := writeCoverage(&out, home); err == nil || !strings.Contains(err.Error(), "load rulebook") {
		t.Fatalf("malformed rulebook: err=%v out=%s", err, out.String())
	}
}

// On the compiled path: a detector-only user rule can fire wherever the hook's engine
// tier stands, because the standalone tier decides over the same detector tags. Only
// the engine row follows the invocation file: it loads the rulebook when that file has
// no rules or is the rulebook itself. A tool ∧
// state rule is the stateful tier's alone, whatever the engine tier loads: that tier has
// both facts, so the rule can fire exactly where the stateful tier is armed.
func TestCoverageFollowsTheHookEngineTier(t *testing.T) {
	home := coverageHome(t)
	rulesPath := filepath.Join(home, ".crossing-guard", "policy", "rules.json")
	writeRules(t, rulesPath, `{"rules":[
	 {"id":"cred","action":"deny","if":{"tag":"data-class","value":"credential-material"}},
	 {"id":"cross","action":"deny","if":{"all":[{"tag":"tool","value":"Bash"},{"tag":"session:vcs","value":"push-force"}]}}]}`)
	check := func(label string, engineLoads bool, header string) {
		t.Helper()
		out := runCoverage(t, home)
		if !strings.Contains(out, "  hook engine tier: "+header) {
			t.Errorf("%s: header %q missing:\n%s", label, header, out)
		}
		cred := ruleBlock(t, out, "cred")
		producedBy := "(tiers: hook-standalone)"
		if engineLoads {
			producedBy = "(tiers: hook-engine, hook-standalone)"
		}
		if strings.Contains(cred, "INERT") || !strings.Contains(cred, producedBy) ||
			engineLoads == strings.Contains(cred, "(loads: not here)") {
			t.Errorf("%s: cred must fire, engine tier loads=%v:\n%s", label, engineLoads, cred)
		}
		armed := platform.Current().StatefulEnforcementReady()
		if cross := ruleBlock(t, out, "cross"); strings.Contains(cross, "INERT") == armed {
			t.Errorf("%s: tool ∧ state fires only in an armed stateful tier (armed=%v):\n%s", label, armed, cross)
		}
	}
	check("no invocation file", false, "does not run here — no invocation file")
	empty := filepath.Join(home, "empty-policy.json")
	writeRules(t, empty, `{"rules":[]}`)
	t.Setenv("CG_POLICY", empty)
	check("empty invocation file", true, "loads this rulebook here")
	own := filepath.Join(home, "own-policy.json")
	writeRules(t, own, `{"rules":[{"id":"inv-tool","action":"deny","if":{"tag":"tool","value":"Bash"}}]}`)
	t.Setenv("CG_POLICY", own)
	check("invocation file with rules", false, "does not load this rulebook here")
	t.Setenv("CG_POLICY", rulesPath)
	check("invocation file is the rulebook", true, "loads this rulebook here")
}

// hookEngineRules is the one statement of what the engine tier loads (RT-2).
func TestHookEngineRules(t *testing.T) {
	dir := t.TempDir()
	rulesPath := filepath.Join(dir, "rules.json")
	writeRules(t, rulesPath, `{"rules":[]}`)
	user := engine.Rule{ID: "u"}
	team := engine.Rule{ID: "t"}
	inv := engine.Rule{ID: "i"}
	layered := rulebook.LayeredPolicy{Policy: &engine.Policy{Rules: []engine.Rule{user, team}}, UserRuleCount: 1, UserPath: rulesPath, UserDigest: "sha256:file"}
	withRules := &rulebook.InvocationPolicy{Available: true, Path: filepath.Join(dir, "other.json"), Policy: &engine.Policy{Rules: []engine.Rule{inv}}}
	sameAsRulebook := &rulebook.InvocationPolicy{Available: true, Path: rulesPath, Digest: "sha256:file", Policy: &engine.Policy{Rules: []engine.Rule{inv}}}
	// An unselected install reports the legacy path but decides over another document.
	samePathOtherRules := &rulebook.InvocationPolicy{Available: true, Path: rulesPath, Digest: "sha256:other", Policy: &engine.Policy{Rules: []engine.Rule{inv}}}
	noRules := &rulebook.InvocationPolicy{Available: true, Path: filepath.Join(dir, "empty.json"), Policy: &engine.Policy{}}
	ids := func(p *engine.Policy) string {
		if p == nil {
			return "<none>"
		}
		var out []string
		for _, r := range p.Rules {
			out = append(out, r.ID)
		}
		return strings.Join(out, ",")
	}
	cases := []struct {
		name           string
		inv            *rulebook.InvocationPolicy
		invErr, layErr error
		rules          string
		user, team, in bool
	}{
		{"unavailable", &rulebook.InvocationPolicy{}, nil, nil, "<none>", false, false, false},
		{"unreadable", nil, os.ErrPermission, nil, "<none>", false, false, false},
		{"layered error", withRules, nil, os.ErrPermission, "i", false, false, true},
		{"zero rules", noRules, nil, nil, "u,t", true, true, false},
		{"own rules", withRules, nil, nil, "i,t", false, true, true},
		{"is the rulebook", sameAsRulebook, nil, nil, "i,t", true, true, true},
		{"same path, other rules (PW-5)", samePathOtherRules, nil, nil, "i,t", false, true, true},
	}
	for _, c := range cases {
		lay := layered
		if c.layErr != nil {
			lay = rulebook.LayeredPolicy{}
		}
		got := hookEngineRules(c.inv, c.invErr, lay, c.layErr)
		if ids(got.Policy) != c.rules || got.User != c.user || got.Team != c.team || got.Invocation != c.in {
			t.Errorf("%s: %+v rules %s", c.name, got, ids(got.Policy))
		}
	}
}
