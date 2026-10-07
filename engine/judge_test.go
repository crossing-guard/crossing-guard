package engine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func term(key, value string) Predicate { return Predicate{Tag: key, Value: value} }
func not(p Predicate) Predicate        { return Predicate{Not: &p} }

// The three answers through each connective. `known` is a term the site can answer and
// nothing satisfies (No); `blind` is one it cannot answer (Undecided); `held` is
// satisfied (Yes).
func TestJudgeTruthTables(t *testing.T) {
	tags := []Tag{{Key: "held", Value: "x"}}
	unknown := func(p Predicate) bool { return p.Tag == "blind" }
	held, known, blind := term("held", "x"), term("known", ""), term("blind", "")
	cases := []struct {
		name string
		p    Predicate
		want Truth
	}{
		{"term yes", held, Yes},
		{"term no", known, No},
		{"term undecided", blind, Undecided},
		{"not yes", not(held), No},
		{"not no", not(known), Yes},
		{"not undecided", not(blind), Undecided},
		{"double negation undecided", not(not(blind)), Undecided},
		{"double negation yes", not(not(held)), Yes},
		{"all yes+undecided", Predicate{All: []Predicate{held, blind}}, Undecided},
		{"all no+undecided", Predicate{All: []Predicate{known, blind}}, No},
		{"all undecided+no", Predicate{All: []Predicate{blind, known}}, No},
		{"all yes+yes", Predicate{All: []Predicate{held, not(known)}}, Yes},
		{"any yes+undecided", Predicate{Any: []Predicate{blind, held}}, Yes},
		{"any no+undecided", Predicate{Any: []Predicate{known, blind}}, Undecided},
		{"any no+no", Predicate{Any: []Predicate{known, not(held)}}, No},
		{"not any undecided", not(Predicate{Any: []Predicate{known, blind}}), Undecided},
		{"not all no", not(Predicate{All: []Predicate{known, blind}}), Yes},
		{"empty node", Predicate{}, No},
	}
	for _, c := range cases {
		if got := Judge(c.p, tags, unknown); got != c.want {
			t.Errorf("%s: got %d want %d", c.name, got, c.want)
		}
	}
}

// A satisfied term is Yes even where the site calls its key unknown: presence is a
// fact, only absence is unanswerable.
func TestJudgeSatisfiedTermBeatsUnknown(t *testing.T) {
	tags := []Tag{{Key: "exec", Value: "sudo"}}
	all := func(Predicate) bool { return true }
	if Judge(term("exec", "sudo"), tags, all) != Yes {
		t.Fatal("a tag that satisfies the term must answer it")
	}
	if Judge(term("exec", "run"), tags, all) != Undecided {
		t.Fatal("another value of the same key stays undecided")
	}
}

// Match is Judge at a site that can answer everything: the old two-valued evaluator,
// including its first-shape order for predicates built in Go.
func TestMatchIsJudgeWithNoBlindSpot(t *testing.T) {
	tags := []Tag{{Key: "a", Value: "1"}, {Key: CommandTagKey, Value: "git push --force"}}
	cases := []struct {
		p    Predicate
		want bool
	}{
		{term("a", "1"), true},
		{term("a", "2"), false},
		{term("a", ""), true},
		{Predicate{Tag: CommandTagKey, Matches: `push\s+--force`}, true},
		{Predicate{Tag: CommandTagKey, Matches: `(`}, false}, // a bad pattern matches nothing
		{not(term("b", "")), true},
		{not(Predicate{}), true}, // Go-built only: a loaded document cannot carry it
		{Predicate{All: []Predicate{term("a", "1"), not(term("b", ""))}}, true},
		{Predicate{Any: []Predicate{term("b", ""), term("c", "")}}, false},
		// first shape wins: all is read, the tag beside it is not
		{Predicate{All: []Predicate{term("a", "1")}, Tag: "b"}, true},
		{Predicate{Any: []Predicate{term("b", "")}, Not: &Predicate{Tag: "b"}}, false},
	}
	for i, c := range cases {
		if got := Match(c.p, tags); got != c.want {
			t.Errorf("case %d: Match=%v want %v", i, got, c.want)
		}
		if got := Judge(c.p, tags, nil) == Yes; got != c.want {
			t.Errorf("case %d: Judge=%v want %v", i, got, c.want)
		}
	}
}

func TestDecideSeeingFiresOnlyYes(t *testing.T) {
	pol := &Policy{Rules: []Rule{
		{ID: "blind-deny", Action: "deny", If: not(term(ToolTagKey, "Bash"))},
		{ID: "blind-observe", Action: "observe", If: term(ToolTagKey, "Bash")},
		{ID: "ask-a", Action: "ask", If: Predicate{Tag: CommandTagKey, Matches: "push"}},
		{ID: "ask-a", Action: "ask", If: Predicate{Tag: CommandTagKey, Matches: "git"}},
		{ID: "ask-b", Action: "ask", If: Predicate{Any: []Predicate{term(ToolTagKey, "Bash"), {Tag: CommandTagKey, Matches: "git"}}}},
	}}
	tags := []Tag{{Key: CommandTagKey, Value: "git push"}}
	blindTool := func(p Predicate) bool { return p.Tag == ToolTagKey }
	d, seen := DecideSeeing(tags, pol, blindTool)
	if d.Rule != "ask-a" || d.Mode != ConfirmAndRecord || strings.Join(d.AllFired, ",") != "ask-a,ask-a,ask-b" {
		t.Fatalf("decision: %+v", d)
	}
	if len(seen.Undecided) != 2 || seen.GatingUndecided() != 1 {
		t.Fatalf("undecided: %+v", seen.Undecided)
	}
	if asking := seen.Asking(); len(asking) != 2 || asking[0].ID != "ask-a" || asking[1].ID != "ask-b" {
		t.Fatalf("asking: %+v", asking)
	}
	// With no blind spot the negated tool rule is vacuously true — the defect Judge's
	// third answer removes at a site that names no tool.
	if d := Decide(tags, pol); d.Rule != "blind-deny" {
		t.Fatalf("two-valued decide: %+v", d)
	}
}

func TestAnyUnknownIsTheUnion(t *testing.T) {
	u := AnyUnknown(nil, UnknownWithoutDetectors, UnknownInPreview(nil))
	for _, key := range []string{ToolTagKey, "secret"} {
		if !u(term(key, "")) {
			t.Errorf("%s must be unknown", key)
		}
	}
	if u(term(CommandTagKey, "")) {
		t.Error("the raw command is known to a preview with no detectors")
	}
}

func TestCompilePredicatesRefusesMalformedNodes(t *testing.T) {
	a, b := term("a", ""), term("b", "")
	bad := map[string]Predicate{
		"all+tag":       {All: []Predicate{a}, Tag: "b"},
		"all+any":       {All: []Predicate{a}, Any: []Predicate{b}},
		"any+not":       {Any: []Predicate{a}, Not: &b},
		"not+tag":       {Not: &a, Tag: "b"},
		"all+not":       {All: []Predicate{a}, Not: &b},
		"any+tag":       {Any: []Predicate{a}, Tag: "b"},
		"empty in not":  {Not: &Predicate{}},
		"empty in all":  {All: []Predicate{a, {}}},
		"empty in any":  {Any: []Predicate{{}, a}},
		"nested mixed":  {All: []Predicate{a, {Not: &Predicate{Any: []Predicate{b}, Tag: "c"}}}},
		"value no tag":  {All: []Predicate{a}, Value: "x"},
		"match no tag":  {Not: &a, Matches: "x"},
		"root value":    {Value: "x"},
		"value+matches": {Tag: "a", Value: "x", Matches: "y"},
	}
	for name, p := range bad {
		err := CompilePredicates(&Policy{Rules: []Rule{{ID: "r1", Action: "deny", If: p}}})
		var shape *PredicateShapeError
		if !errors.As(err, &shape) || shape.RuleID != "r1" {
			t.Errorf("%s: want a PredicateShapeError naming r1, got %v", name, err)
		}
	}
	good := map[string]Predicate{
		"empty root": {}, // never fires; legacy migration writes it
		"term":       a,
		"term value": term("a", "x"),
		"pattern":    {Tag: "a", Matches: "x+"},
		"nested":     {All: []Predicate{a, {Not: &Predicate{Any: []Predicate{b, a}}}}},
	}
	for name, p := range good {
		if err := CompilePredicates(&Policy{Rules: []Rule{{ID: "r1", Action: "deny", If: p}}}); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
}

// Every rule document shipped in the repository loads under the node checks.
func TestShippedRuleDocumentsPassTheNodeChecks(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "ruledoc", "rules.*.json"))
	if err != nil || len(paths) < 3 {
		t.Fatalf("shipped rule documents not found: %v %v", paths, err)
	}
	more, _ := filepath.Glob(filepath.Join("testdata", "*.json"))
	for _, path := range append(paths, more...) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"rules"`) {
			continue
		}
		if _, err := LoadPolicy(path); err != nil {
			var shape *PredicateShapeError
			if errors.As(err, &shape) {
				t.Errorf("%s: %v", path, err)
			}
		}
	}
}

func TestDetectorCannotEmitAnInvocationKey(t *testing.T) {
	for _, key := range []string{CommandTagKey, ToolTagKey} {
		err := CompileDetectors([]Detector{{ID: "d", Kind: "content", Keywords: []string{"x"}, Tag: tagSpec{Key: key, Value: "Bash"}}})
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Errorf("%s: want a reserved-key refusal, got %v", key, err)
		}
	}
	dets, err := DefaultDetectors()
	if err != nil {
		t.Fatalf("the shipped library must still load: %v", err)
	}
	for _, d := range dets {
		if isInvocationKey(d.Tag.Key) {
			t.Errorf("shipped detector %s emits %s", d.ID, d.Tag.Key)
		}
	}
}
