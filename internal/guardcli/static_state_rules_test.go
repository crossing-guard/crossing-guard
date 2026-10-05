package guardcli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A rule whose predicate NEGATES a state term. Over the static tier's stateless tag set
// that term is vacuously true, so before the fix this blocked every push whatever the
// review state (state-producer red-team RT-2). Only the daemon's stateful tier may
// judge it. The push command is assembled so this file does not trip a live hook.
const negatedStateRules = `{"rules":[
  {"id":"push-needs-review","action":"deny","message":"push requires a review claim",
   "if":{"all":[{"tag":"command","matches":"\\bgit\\s+pu` + `sh\\b"},{"not":{"tag":"agent:b1:reviewed"}}]}},
  {"id":"deny-alpha","action":"deny","message":"alpha is restricted","if":{"tag":"command","matches":"alpha"}}
]}`

const negatedStatePolicy = `{"rules":[
  {"id":"any-needs-session-review","action":"deny","message":"session not reviewed",
   "if":{"not":{"tag":"session:reviewed"}}}
]}`

var pushCommand = "git pu" + "sh origin main"

func writeTemp(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCheckCommandDoesNotFireNegatedStateTerm(t *testing.T) {
	governTestHome(t, negatedStateRules)
	v, excluded, err := CheckCommandStatic(pushCommand, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v.Decision != "allow" {
		t.Fatalf("static dry run fired a negated state term: %+v", v)
	}
	if excluded.StateRules != 1 {
		t.Fatalf("excluded state rules = %d, want 1", excluded.StateRules)
	}
	if v, _ := CheckCommand("run alpha now", nil); v.Decision != "deny" || v.Rule != "deny-alpha" {
		t.Fatalf("a pure command rule beside the state rule stopped enforcing: %+v", v)
	}
}

func TestGovernHookDoesNotFireNegatedStateTerm(t *testing.T) {
	governTestHome(t, negatedStateRules)
	if d := governHookPreToolUse(governPreToolCall(pushCommand)); d.Decision != "allow" {
		t.Fatalf("command-guard tier fired a negated state term: %+v", d)
	}
	if d := governHookPreToolUse(governPreToolCall("run alpha now")); d.Decision != "deny" {
		t.Fatalf("pure command rule stopped enforcing: %+v", d)
	}
}

func TestEngineTierDoesNotFireNegatedStateTerm(t *testing.T) {
	governTestHome(t, `{"rules":[]}`)
	t.Setenv("CG_POLICY", writeTemp(t, "policy.json", negatedStatePolicy))
	in := governPreToolCall("ls")
	engineSet, ok := loadEngineTier(in)
	if !ok {
		t.Fatal("engine tier did not run with an invocation policy present")
	}
	st, boundary := engineDecision(engineSet, actionOf(in, engineSet.Policy))
	if st.Decision.Decision != "allow" || boundary != nil {
		t.Fatalf("engine tier fired a negated state term: %+v boundary=%v", st.Decision, boundary)
	}
	if gd := governHookPreToolUse(in); gd.Decision != "allow" {
		t.Fatalf("govern hook blocked via the engine tier: %+v", gd)
	}
}

// TestHookSubprocessHelper is not a test on its own: TestHookDoesNotFireNegatedStateTerm
// re-executes the test binary into it, because cmdHook ends the process.
func TestHookSubprocessHelper(t *testing.T) {
	if os.Getenv("CG_TEST_HOOK_SUBPROCESS") != "1" {
		t.Skip("subprocess helper")
	}
	// A test can age the hook, so what is left of its one approval budget is small
	// without the test waiting out the real budget.
	if ms, err := strconv.Atoi(os.Getenv("CG_TEST_HOOK_AGE_MS")); err == nil {
		hookStarted = time.Now().Add(-time.Duration(ms) * time.Millisecond)
	}
	cmdHook([]string{"--runtime", "claude"})
	os.Exit(0)
}

func runHookSubprocess(t *testing.T, home, rules, policy, command string) string {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "session_id": "s1",
		"tool_name": "Bash", "cwd": home, "tool_input": map[string]any{"command": command}})
	cmd := exec.Command(os.Args[0], "-test.run=^TestHookSubprocessHelper$")
	cmd.Env = []string{"CG_TEST_HOOK_SUBPROCESS=1", "HOME=" + home, "PATH=/usr/bin:/bin",
		"CG_RULES=" + rules, "CG_LOG=" + filepath.Join(home, "decisions.jsonl")}
	if policy != "" {
		cmd.Env = append(cmd.Env, "CG_POLICY="+policy)
	}
	cmd.Stdin = bytes.NewReader(payload)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("hook subprocess: %v\n%s", err, out.String())
	}
	return out.String()
}

// TestHookDoesNotFireNegatedStateTerm drives the real `hook` verb — the command-guard
// tier and the engine tier — with no daemon: the negated state rule must not deny, and
// a pure command rule beside it still must.
func TestHookDoesNotFireNegatedStateTerm(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".crossing-guard"), 0o755); err != nil {
		t.Fatal(err)
	}
	rules := writeTemp(t, "rules.json", negatedStateRules)
	policy := writeTemp(t, "policy.json", negatedStatePolicy)
	if out := runHookSubprocess(t, home, rules, policy, pushCommand); strings.Contains(out, `"deny"`) {
		t.Fatalf("hook denied over a negated state term:\n%s", out)
	}
	if out := runHookSubprocess(t, home, rules, policy, "run alpha now"); !strings.Contains(out, `"deny"`) ||
		!strings.Contains(out, "deny-alpha") {
		t.Fatalf("pure command rule did not deny through the hook:\n%s", out)
	}
}

// TestStaticDecideIsTheOnlyStaticEvaluator keeps the next lane from reintroducing the
// defect: across the whole module, the engine's evaluators (Decide, DecideSeeing, Match,
// Judge) are called only by the
// recorded owners — StaticDecide (the static tier), DecideStateful (the stateful tier),
// the audit's report-only match (a recorded limitation) and the session-query view
// filter. The engine package itself (Decide's definition, the Ledger over StaticTier)
// is the owner and is not scanned. The engine import must not be aliased, or the scan
// would be blind to it.
func TestStaticDecideIsTheOnlyStaticEvaluator(t *testing.T) {
	// routeEvaluator is the third recorded evaluator (team rest-of-release plan §5.4,
	// OD-25): route admission, which decides ONLY rules that read a route: fact, at
	// bind and at run start. It is named exactly — file and function — so a second
	// route evaluator, or one that moves, fails this test.
	const routeEvaluator = "internal/modelroute/admit.go:Admit"
	allowed := map[string]bool{
		"internal/guardcli/main.go:StaticDecide":   true,
		"internal/daemon/decide.go:DecideStateful": true,
		"internal/daemon/audit.go:*":               true,
		"internal/sessionquery/match.go:*":         true,
		routeEvaluator:                             true,
	}
	routeFound := 0
	call := regexp.MustCompile(`\bengine\.(Decide|DecideSeeing|Match|Judge)\(`)
	aliased := regexp.MustCompile(`(?m)^\s*(?:import\s+)?[A-Za-z_.]+\s+"crossing-guard/engine"`)
	root := filepath.Join("..", "..")
	found := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if path == root {
				return nil
			}
			if rel == "engine" || strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if aliased.Match(src) {
			t.Errorf("%s: aliased import of crossing-guard/engine hides evaluator calls from this scan", rel)
		}
		for _, loc := range call.FindAllIndex(src, -1) {
			line := string(src[:loc[0]])
			if i := strings.LastIndexByte(line, '\n'); i >= 0 {
				line = line[i+1:]
			}
			if strings.Contains(line, "//") {
				continue // a mention in a comment, not a call
			}
			fn := enclosingFunc(string(src[:loc[0]]))
			if allowed[rel+":"+fn] || allowed[rel+":*"] {
				if fn == "StaticDecide" {
					found++
				}
				if rel+":"+fn == routeEvaluator {
					routeFound++
					// Admission may decide only the route tier: the call must be over
					// engine.RouteTier's selection, never the whole policy.
					if !strings.Contains(string(src), "engine.RouteTier(") {
						t.Errorf("%s: route admission must select its rules with engine.RouteTier", rel)
					}
				}
				continue
			}
			t.Errorf("%s: %s…) in %s — static evaluation must go through StaticDecide", rel, src[loc[0]:loc[1]], fn)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found != 1 {
		t.Fatalf("StaticDecide's own engine.DecideSeeing call not found exactly once (found %d)", found)
	}
	if routeFound != 1 {
		t.Fatalf("route admission's own evaluator call not found exactly once in %s (found %d)", routeEvaluator, routeFound)
	}
}

// A rule that NEGATES a route: fact. No hook call carries a route fact, so over a hook's
// tag set the term is true by absence: left in a hook tier it would deny every tool
// call. route: is the third key class (OD-25): the rule is decided only by route
// admission, at bind and at run start, and never by a hook (plan criterion 59).
const negatedRouteRules = `{"rules":[
  {"id":"cloud-route-refused","action":"deny","message":"this route leaves the machine",
   "if":{"not":{"tag":"route:local","value":"true"}}},
  {"id":"deny-alpha","action":"deny","message":"alpha is restricted","if":{"tag":"command","matches":"alpha"}}
]}`

func TestCheckCommandNeverEvaluatesARouteRule(t *testing.T) {
	governTestHome(t, negatedRouteRules)
	v, _, err := CheckCommandStatic("ls", nil)
	if err != nil {
		t.Fatal(err)
	}
	if v.Decision != "allow" {
		t.Fatalf("static dry run evaluated a route rule: %+v", v)
	}
	if v, _ := CheckCommand("run alpha now", nil); v.Decision != "deny" || v.Rule != "deny-alpha" {
		t.Fatalf("a pure command rule beside the route rule stopped enforcing: %+v", v)
	}
}

func TestGovernHookNeverEvaluatesARouteRule(t *testing.T) {
	governTestHome(t, negatedRouteRules)
	if d := governHookPreToolUse(governPreToolCall("ls")); d.Decision != "allow" {
		t.Fatalf("command-guard tier evaluated a route rule on a hook call: %+v", d)
	}
	if d := governHookPreToolUse(governPreToolCall("run alpha now")); d.Decision != "deny" {
		t.Fatalf("pure command rule stopped enforcing: %+v", d)
	}
}

func TestEngineTierNeverEvaluatesARouteRule(t *testing.T) {
	governTestHome(t, `{"rules":[]}`)
	t.Setenv("CG_POLICY", writeTemp(t, "policy.json", negatedRouteRules))
	in := governPreToolCall("ls")
	engineSet, ok := loadEngineTier(in)
	if !ok {
		t.Fatal("engine tier did not run with an invocation policy present")
	}
	st, boundary := engineDecision(engineSet, actionOf(in, engineSet.Policy))
	if st.Decision.Decision != "allow" || boundary != nil {
		t.Fatalf("engine tier evaluated a route rule on a hook call: %+v boundary=%v", st.Decision, boundary)
	}
	for _, rule := range st.Policy.Rules {
		if rule.ID == "cloud-route-refused" {
			t.Fatal("the hook's engine tier still holds the route rule")
		}
	}
}

// TestHookNeverEvaluatesARouteRule drives the real `hook` verb with no daemon, with the
// negated route rule in both the rulebook and the invocation policy: a tool call is
// allowed, and a pure command rule beside it still denies.
func TestHookNeverEvaluatesARouteRule(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".crossing-guard"), 0o755); err != nil {
		t.Fatal(err)
	}
	rules := writeTemp(t, "rules.json", negatedRouteRules)
	policy := writeTemp(t, "policy.json", negatedRouteRules)
	if out := runHookSubprocess(t, home, rules, policy, "ls"); strings.Contains(out, `"deny"`) {
		t.Fatalf("hook denied a tool call over a route rule:\n%s", out)
	}
	if out := runHookSubprocess(t, home, rules, policy, "run alpha now"); !strings.Contains(out, `"deny"`) ||
		!strings.Contains(out, "deny-alpha") {
		t.Fatalf("pure command rule did not deny through the hook:\n%s", out)
	}
}

// enclosingFunc names the last top-level func declared before a source offset.
func enclosingFunc(before string) string {
	i := strings.LastIndex(before, "\nfunc ")
	if i < 0 {
		return "(file scope)"
	}
	decl := before[i+len("\nfunc "):]
	if strings.HasPrefix(decl, "(") { // method: skip the receiver
		if j := strings.Index(decl, ") "); j >= 0 {
			decl = decl[j+2:]
		}
	}
	if j := strings.IndexAny(decl, "([ "); j >= 0 {
		decl = decl[:j]
	}
	return decl
}
