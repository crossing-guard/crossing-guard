package engine

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestReferencesStateFindsEveryStateNamespaceAtAnyDepth(t *testing.T) {
	cases := []struct {
		name string
		p    Predicate
		want bool
	}{
		{"session term", Predicate{Tag: "session:phase", Value: "plan"}, true},
		{"target term", Predicate{Tag: "target:data-class"}, true},
		{"agent term", Predicate{Tag: "agent:b1:reviewed"}, true},
		{"negated agent inside all", Predicate{All: []Predicate{
			{Tag: CommandTagKey, Matches: "git push"},
			{Not: &Predicate{Tag: "agent:b1:reviewed"}},
		}}, true},
		{"session deep in any/not", Predicate{Any: []Predicate{
			{Tag: ToolTagKey, Value: "Read"},
			{Not: &Predicate{All: []Predicate{{Tag: "session:reviewed"}}}},
		}}, true},
		{"pure command", Predicate{Tag: CommandTagKey, Matches: "rm"}, false},
		{"pure negated tool", Predicate{Not: &Predicate{Tag: ToolTagKey, Value: "Read"}}, false},
		{"detector key that merely contains a colon-free prefix word", Predicate{Tag: "agent"}, false},
	}
	for _, c := range cases {
		if got := ReferencesState(c.p); got != c.want {
			t.Errorf("%s: ReferencesState = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestStaticTierDropsStateRulesAndKeepsTheRest(t *testing.T) {
	in := &Policy{
		Capabilities: Capabilities{},
		Rules: []Rule{
			{ID: "pure-1", If: Predicate{Tag: CommandTagKey, Matches: "alpha"}},
			{ID: "neg-state", If: Predicate{All: []Predicate{
				{Tag: CommandTagKey, Matches: "git push"},
				{Not: &Predicate{Tag: "session:reviewed"}},
			}}},
			{ID: "pure-2", If: Predicate{Tag: ToolTagKey, Value: "Bash"}},
			{ID: "agent", If: Predicate{Tag: "agent:b1:reviewed"}},
		},
	}
	before := append([]Rule{}, in.Rules...)
	out := StaticTier(in)
	var ids []string
	for _, r := range out.Rules {
		ids = append(ids, r.ID)
	}
	if want := []string{"pure-1", "pure-2"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("StaticTier rules = %v, want %v (order preserved, state rules dropped)", ids, want)
	}
	if !reflect.DeepEqual(out.Capabilities, in.Capabilities) {
		t.Error("StaticTier dropped the policy capabilities")
	}
	if !reflect.DeepEqual(in.Rules, before) {
		t.Error("StaticTier mutated its input")
	}
	if StaticTier(nil) != nil {
		t.Error("StaticTier(nil) must be nil")
	}
}

// TestStaticTierNegatedStateTermCannotFire is the defect itself: over a stateless tag
// set, `not: session:reviewed` is vacuously true, so the whole policy blocks a push
// whatever the review state; the static tier must not.
func TestStaticTierNegatedStateTermCannotFire(t *testing.T) {
	pol := &Policy{Rules: []Rule{{ID: "push-needs-review", Action: "deny",
		If: Predicate{All: []Predicate{
			{Tag: CommandTagKey, Matches: `\bgit\s+push\b`},
			{Not: &Predicate{Tag: "session:reviewed"}},
		}}}}}
	tags := InvocationTags("Bash", "git push origin main")
	if d := Decide(tags, pol); d.Decision == "allow" {
		t.Fatal("precondition: the whole policy should (wrongly) fire statically")
	}
	if d := Decide(tags, StaticTier(pol)); d.Decision != "allow" {
		t.Fatalf("static tier fired a negated state term: %+v", d)
	}
}

func TestGatesMirrorsTheStatefulVerdictLadder(t *testing.T) {
	for action, want := range map[string]bool{"deny": true, "ask": true, "redact": true,
		"observe": false, "allow": false, "": false} {
		if got := (Rule{Action: action}).Gates(); got != want {
			t.Errorf("action %q: Gates = %v, want %v", action, got, want)
		}
	}
	if (Rule{Mode: WarnAndProceed, Action: "deny"}).Gates() {
		t.Error("an authored warn mode must win over the action")
	}
}

func TestInvocationTags(t *testing.T) {
	if got := InvocationTags("", "ls"); !reflect.DeepEqual(got, []Tag{{Key: CommandTagKey, Value: "ls"}}) {
		t.Errorf("no tool: %+v", got)
	}
	got := InvocationTags(" mcp__provider__Artifact ", "")
	want := []Tag{{Key: CommandTagKey, Value: ""}, {Key: ToolTagKey, Value: "Artifact"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bare tool: got %+v want %+v", got, want)
	}
}

// TestLedgerDoesNotDecideStateRules: the ledger folds UNPREFIXED session tags, so a
// state term can never be present there and a negated one would be vacuous.
func TestLedgerDoesNotDecideStateRules(t *testing.T) {
	d, err := LoadDetectors("testdata/detectors.json")
	if err != nil {
		t.Fatal(err)
	}
	pol := &Policy{Rules: []Rule{{ID: "unreviewed", Action: "deny",
		If: Predicate{Not: &Predicate{Tag: "session:reviewed"}}}}}
	l, err := NewLedger(filepath.Join(t.TempDir(), "ledger.jsonl"), d, pol)
	if err != nil {
		t.Fatal(err)
	}
	if obs := l.Observe("s1", Event{Tool: "getSupplierPrice"}); obs.Decision.Decision != "allow" {
		t.Fatalf("ledger decided a state rule over stateless tags: %+v", obs.Decision)
	}
}

// previewFixture: one detector of each kind that reads something a command preview
// does not have (the tool, the destination), and one that reads only the text.
func previewFixture(t *testing.T) []Detector {
	t.Helper()
	run := Detector{ID: "exec.run", Kind: "source", Tag: tagSpec{Key: "exec", Value: "run"}, Coverage: Coverage{Enumerable: true}}
	run.Match.Tool = []string{"Bash"}
	dets := []Detector{run,
		{ID: "exec.sudo", Kind: "pattern", Regex: `\bsudo\b`, Tag: tagSpec{Key: "exec", Value: "sudo"}},
		{ID: "dest", Kind: "destination", Coverage: Coverage{Enumerable: true}},
		{ID: "secret.aws", Kind: "pattern", Regex: "AKIA[0-9A-Z]{4}", Tag: tagSpec{Key: "secret", Value: "aws-key"}},
	}
	if err := CompileDetectors(dets); err != nil {
		t.Fatal(err)
	}
	return dets
}

func TestActionEventTextIsTheCommandElseTheBody(t *testing.T) {
	ev := ActionEvent("mcp__x__Bash", "ls", "body", "/p", "https://h/")
	if ev.Tool != BareTool("mcp__x__Bash") || ev.Text != "ls" || ev.Path != "/p" || ev.Destination != "https://h/" || ev.Role != LiveEventRole {
		t.Fatalf("%+v", ev)
	}
	if ev := ActionEvent("Write", "", "body", "/p", ""); ev.Text != "body" {
		t.Fatalf("a write body is the text when there is no command: %+v", ev)
	}
}

// One action, one tag set: the detector tags and the invocation facts together.
func TestActionTagsCarryDetectorAndInvocationFacts(t *testing.T) {
	dets := previewFixture(t)
	tags := ActionTags(ActionEvent("Bash", "sudo cat AKIA1234", "", "", ""), dets, "sudo cat AKIA1234")
	want := map[string]string{"exec=run": "", "exec=sudo": "", "secret=aws-key": "", "tool=Bash": "", "command=sudo cat AKIA1234": ""}
	for _, tag := range tags {
		delete(want, tag.Key+"="+tag.Value)
	}
	if len(want) != 0 {
		t.Fatalf("missing %v in %+v", want, tags)
	}
	// A rule spanning both halves fires over them; it could fire over neither half alone.
	mixed := Predicate{All: []Predicate{{Tag: CommandTagKey, Matches: "cat"}, {Tag: "secret", Value: "aws-key"}}}
	if !Match(mixed, tags) || Match(mixed, InvocationTags("Bash", "sudo cat AKIA1234")) ||
		Match(mixed, Classify(ActionEvent("Bash", "sudo cat AKIA1234", "", "", ""), dets)) {
		t.Fatal("a mixed rule must fire over the action tags and over neither half")
	}
	// A write body is classified too.
	if !Match(Predicate{Tag: "secret", Value: "aws-key"}, ActionTags(ActionEvent("Write", "", "key AKIA9999", "/f", ""), dets, "")) {
		t.Fatal("a secret in a write body must be tagged")
	}
}

func TestUnknownInPreview(t *testing.T) {
	u := UnknownInPreview(previewFixture(t))
	for name, c := range map[string]struct {
		p    Predicate
		want bool
	}{
		"tool":                {Predicate{Tag: ToolTagKey, Value: "Bash"}, true},
		"tool-derived fact":   {Predicate{Tag: "exec", Value: "run"}, true},
		"same key, text fact": {Predicate{Tag: "exec", Value: "sudo"}, false},
		"destination":         {Predicate{Tag: "destination-class", Value: "external"}, true},
		"text fact":           {Predicate{Tag: "secret", Value: "aws-key"}, false},
		"command":             {Predicate{Tag: CommandTagKey, Matches: "x"}, false},
		"no producer":         {Predicate{Tag: "nothing", Value: "x"}, false},
	} {
		if got := u(c.p); got != c.want {
			t.Errorf("%s: unknown=%v want %v", name, got, c.want)
		}
	}
	// A preview names no tool: the tool-dependent rule is undecided, not allowed, and a
	// negated one does not fire by absence.
	tags := ActionTags(ActionEvent("", "sudo ls", "", "", ""), previewFixture(t), "sudo ls")
	shell := Predicate{All: []Predicate{{Tag: ToolTagKey, Value: "Bash"}, {Tag: CommandTagKey, Matches: "ls"}}}
	if Judge(shell, tags, u) != Undecided || Judge(Predicate{Not: &Predicate{Tag: "exec", Value: "run"}}, tags, u) != Undecided {
		t.Fatal("tool-dependent terms must be undecided in a preview")
	}
	if Judge(Predicate{Tag: "exec", Value: "sudo"}, tags, u) != Yes {
		t.Fatal("a text fact is decided in a preview")
	}
}

func TestUnknownAtHarvest(t *testing.T) {
	sp := stateProducerFixture()
	sp.Producers = append(sp.Producers, StateProducer{Tag: "agent:b2:seen", Values: []string{"model-claimed"}, Live: true, Harvest: true})
	u := UnknownAtHarvest(sp)
	for name, c := range map[string]struct {
		p    Predicate
		want bool
	}{
		"command":            {Predicate{Tag: CommandTagKey, Matches: "x"}, true},
		"tool":               {Predicate{Tag: ToolTagKey, Value: "Bash"}, true},
		"target":             {Predicate{Tag: "target:secret", Value: "aws-key"}, true},
		"live-only fact":     {Predicate{Tag: "session:work", Value: "uncommitted"}, true},
		"live-only, other v": {Predicate{Tag: "session:work", Value: "clean"}, false},
		"harvest claim":      {Predicate{Tag: "agent:b2:seen"}, false},
		"detector state":     {Predicate{Tag: "session:vcs", Value: "push-force"}, false},
	} {
		if got := u(c.p); got != c.want {
			t.Errorf("%s: unknown=%v want %v", name, got, c.want)
		}
	}
}

// The Unknown constructors restate the producer-channel table from the evaluator's side.
// A term a site calls unknown has no producer on that site's channel, and the facts
// every live tier carries are unknown to none of them.
func TestUnknownsMatchTheChannelTable(t *testing.T) {
	dets := append(stateChannelFixture(), previewFixture(t)...)
	sp := stateProducerFixture()
	probes := []Predicate{
		{Tag: CommandTagKey, Matches: "x"}, {Tag: ToolTagKey, Value: "Bash"},
		{Tag: "secret", Value: "aws-key"}, {Tag: "exec", Value: "run"},
		{Tag: "target:secret", Value: "aws-key"}, {Tag: "session:work", Value: "uncommitted"},
		{Tag: "session:vcs", Value: "push-force"}, {Tag: "agent:b1:reviewed"},
	}
	harvest, tagsOnly := UnknownAtHarvest(sp), Unknown(UnknownInvocation)
	for _, p := range probes {
		onHarvest := producersFor(p, producerSources{dets: dets, state: sp, ch: channelHarvest})
		if harvest(p) && len(onHarvest) != 0 {
			t.Errorf("%s: unknown at harvest but the harvest channel has a producer", p.Tag)
		}
		if tagsOnly(p) && len(onHarvest) != 0 {
			t.Errorf("%s: unknown to the tags dry run but the harvest channel has a producer", p.Tag)
		}
		if isInvocationKey(p.Tag) {
			if !harvest(p) || !tagsOnly(p) {
				t.Errorf("%s: no dry run has the invocation", p.Tag)
			}
			for _, ch := range []producerChannel{channelEngine, channelStandalone, channelStateful} {
				if len(producersFor(p, producerSources{dets: dets, state: sp, ch: ch})) == 0 {
					t.Errorf("%s: every live tier carries the invocation facts (channel %d)", p.Tag, ch)
				}
			}
		}
	}
	// The hook's two tiers carry the same facts.
	for _, p := range probes {
		e := producersFor(p, producerSources{dets: dets, state: sp, ch: channelEngine})
		s := producersFor(p, producerSources{dets: dets, state: sp, ch: channelStandalone})
		if len(e) != len(s) {
			t.Errorf("%s: engine tier has %d producers, standalone %d", p.Tag, len(e), len(s))
		}
	}
}

// At harvest a term the dry run cannot read is inert in both polarities: the evaluator
// leaves it undecided, so it never fires the rule — it is not VACUOUS.
func TestHarvestBlindTermIsInertInBothPolarities(t *testing.T) {
	dets, sp := stateChannelFixture(), stateProducerFixture()
	for _, tg := range []Predicate{{Tag: "target:secret", Value: "aws-key"}, {Tag: CommandTagKey, Matches: "x"}, {Tag: "session:work", Value: "uncommitted"}} {
		for _, p := range []Predicate{tg, {Not: &tg}} {
			b := Boundary(Rule{ID: "r", Action: "observe", If: p}, dets, sp, ReachHarvest)
			if b.CanFire || !strings.HasPrefix(b.Label, "INERT") || b.Detection[0].Note != harvestBlindNote {
				t.Errorf("%s negated=%v: %+v", tg.Tag, p.Not != nil, b)
			}
		}
	}
	// A decidable branch beside it still fires.
	either := Predicate{Any: []Predicate{{Tag: "target:secret", Value: "aws-key"}, {Tag: "session:vcs", Value: "push-force"}}}
	if b := Boundary(Rule{ID: "r", Action: "observe", If: either}, dets, sp, ReachHarvest); !b.CanFire {
		t.Errorf("a readable branch must still fire: %+v", b)
	}
}

// The ledger decides over a session's accumulated tags: it has no single command or
// tool, so a rule reading one is undecided there and named, never fired by absence.
func TestLedgerLeavesInvocationRulesUndecided(t *testing.T) {
	pol := &Policy{Rules: []Rule{
		{ID: "not-git", Action: "deny", If: Predicate{Not: &Predicate{Tag: CommandTagKey, Matches: "git"}}},
		{ID: "not-bash", Action: "deny", If: Predicate{Not: &Predicate{Tag: ToolTagKey, Value: "Bash"}}},
	}}
	l, err := NewLedger(filepath.Join(t.TempDir(), "ledger.jsonl"), nil, pol)
	if err != nil {
		t.Fatal(err)
	}
	obs := l.Observe("s1", Event{Tool: "Bash", Text: "ls"})
	if obs.Decision.Decision != "allow" || strings.Join(obs.UndecidedRules, ",") != "not-git,not-bash" {
		t.Fatalf("%+v", obs)
	}
}

// A tier classifies only with the detectors whose facts its rules read: nothing else
// could change a decision, and each pattern is a full scan of a write body.
func TestDetectorsReadFollowsTheRules(t *testing.T) {
	dets := previewFixture(t)
	ids := func(ds []Detector) string {
		var out []string
		for _, d := range ds {
			out = append(out, d.ID)
		}
		return strings.Join(out, ",")
	}
	command := &Policy{Rules: []Rule{{ID: "c", If: Predicate{Tag: CommandTagKey, Matches: "x"}}, {ID: "t", If: Predicate{Not: &Predicate{Tag: ToolTagKey, Value: "Bash"}}},
		{ID: "s", If: Predicate{Tag: "session:secret", Value: "aws-key"}}}}
	if ReadsDetectorFacts(command, nil) || len(DetectorsRead(dets, command, nil)) != 0 {
		t.Fatal("command, tool and state rules read no detector fact")
	}
	secret := &Policy{Rules: []Rule{{ID: "x", If: Predicate{All: []Predicate{{Tag: CommandTagKey, Matches: "x"}, {Not: &Predicate{Tag: "secret", Value: "aws-key"}}}}}}}
	if !ReadsDetectorFacts(command, secret) || ids(DetectorsRead(dets, command, secret)) != "secret.aws" {
		t.Fatalf("secret rule: %s", ids(DetectorsRead(dets, command, secret)))
	}
	exec := &Policy{Rules: []Rule{{ID: "e", If: Predicate{Tag: "exec", Value: "run"}}, {ID: "d", If: Predicate{Tag: "destination-class", Value: "external"}}}}
	if got := ids(DetectorsRead(dets, exec)); got != "exec.run,exec.sudo,dest" {
		t.Fatalf("every detector of a read key is kept, and the destination detector by its fact: %s", got)
	}
}

// Narrowing the detectors to the ones the rules read never changes a decision: a
// detector's tag can only answer a term on its own key.
func TestNarrowedDetectorsDecideTheSame(t *testing.T) {
	dets, err := DefaultDetectors()
	if err != nil {
		t.Fatal(err)
	}
	token := "gh" + "p_" + strings.Repeat("a1B2", 9)
	events := []struct{ tool, command, content, path, url string }{
		{"Bash", "sudo cat /etc/hosts", "", "", ""},
		{"Bash", "curl -H 'x: " + token + "' https://example.test", "", "", ""},
		{"Write", "", "mail me at someone@example.test " + token, "/tmp/notes.txt", ""},
		{"WebFetch", "", "", "", "https://example.test/x"},
		{"Read", "", "", "/Users/x/.ssh/id_rsa", ""},
	}
	policies := []*Policy{
		{Rules: []Rule{{ID: "a", Action: "deny", If: Predicate{Tag: "secret", Value: "gh-token"}}}},
		{Rules: []Rule{{ID: "b", Action: "ask", If: Predicate{All: []Predicate{{Tag: ToolTagKey, Value: "Bash"}, {Not: &Predicate{Tag: "exec", Value: "sudo"}}}}}}},
		{Rules: []Rule{{ID: "c", Action: "deny", If: Predicate{Any: []Predicate{{Tag: "data-class", Value: "personal-data"}, {Tag: "destination-class", Value: "external"}}}},
			{ID: "d", Action: "observe", If: Predicate{Not: &Predicate{Tag: "nothing-emits-this"}}}}},
	}
	for i, pol := range policies {
		narrowed := DetectorsRead(dets, pol)
		if len(narrowed) == 0 || len(narrowed) >= len(dets) {
			t.Fatalf("policy %d: narrowed to %d of %d detectors", i, len(narrowed), len(dets))
		}
		for j, e := range events {
			ev := ActionEvent(e.tool, e.command, e.content, e.path, e.url)
			full := Decide(ActionTags(ev, dets, e.command), pol)
			lean := Decide(ActionTags(ev, narrowed, e.command), pol)
			if !reflect.DeepEqual(full, lean) {
				t.Errorf("policy %d event %d: full %+v, narrowed %+v", i, j, full, lean)
			}
		}
	}
}
