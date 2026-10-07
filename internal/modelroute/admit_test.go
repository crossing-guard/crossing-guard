package modelroute

import (
	"errors"
	"testing"

	"crossing-guard/engine"
)

func remoteRoute() Route {
	return Route{RouteID: "rte_01J00000000000000000000000", Family: FamilyRuntimeModel,
		Fields: Fields{Runtime: "alpha", Model: "model-a"}}
}

func denyRemote() *engine.Policy {
	return &engine.Policy{Rules: []engine.Rule{
		{ID: "command-rule", Action: "deny", If: engine.Predicate{Tag: engine.CommandTagKey, Matches: ".*"}},
		{ID: "no-remote-routes", Action: "deny", Message: "remote routes are not admitted",
			If: engine.Predicate{Tag: engine.RouteFactPrefix + "local", Value: "false"}},
	}}
}

// Criterion 59: with no route: rule selected, every bind and run proceeds.
func TestAdmitAllowsWhenNoRouteRuleIsSelected(t *testing.T) {
	facts := FactsFor(remoteRoute(), false, "explicit-local-or-remote", "/repo")
	for name, policy := range map[string]*engine.Policy{
		"nil policy": nil,
		"no rules":   {},
		"only a command rule that would match anything": {Rules: []engine.Rule{
			{ID: "command-rule", Action: "deny", If: engine.Predicate{Not: &engine.Predicate{Tag: engine.CommandTagKey, Matches: "never"}}}}},
		"only a negated state rule": {Rules: []engine.Rule{
			{ID: "state-rule", Action: "deny", If: engine.Predicate{Not: &engine.Predicate{Tag: engine.SessionStatePrefix + "reviewed"}}}}},
	} {
		if admission := Admit(policy, facts); !admission.Allowed || len(admission.Fired) != 0 {
			t.Errorf("%s: admission = %+v", name, admission)
		}
	}
}

func TestAdmitRefusesDenyAndAskAndRecordsObserve(t *testing.T) {
	remote := FactsFor(remoteRoute(), false, "explicit-local-or-remote", "/repo")
	local := FactsFor(remoteRoute(), true, "local-only", "/repo")

	denied := Admit(denyRemote(), remote)
	if denied.Allowed || denied.Rule != "no-remote-routes" || denied.Action != "deny" {
		t.Fatalf("deny admission = %+v", denied)
	}
	var refused *AdmissionRefused
	if err := denied.Refusal(); !errors.As(err, &refused) || refused.Code() != AdmissionRefusedCode {
		t.Fatalf("refusal is not typed: %v", err)
	}
	if admitted := Admit(denyRemote(), local); !admitted.Allowed || admitted.Refusal() != nil {
		t.Fatalf("a local route was refused: %+v", admitted)
	}

	ask := &engine.Policy{Rules: []engine.Rule{{ID: "ask-remote", Action: "ask",
		If: engine.Predicate{Tag: engine.RouteFactPrefix + "local", Value: "false"}}}}
	if asked := Admit(ask, remote); asked.Allowed || asked.Action != "ask" {
		t.Fatalf("an ask rule did not refuse: %+v", asked)
	}

	observe := &engine.Policy{Rules: []engine.Rule{{ID: "watch-remote", Action: "observe",
		If: engine.Predicate{Tag: engine.RouteFactPrefix + "local", Value: "false"}}}}
	observed := Admit(observe, remote)
	if !observed.Allowed || len(observed.Fired) != 1 || observed.Fired[0] != "watch-remote" || observed.Action != "observe" {
		t.Fatalf("an observe rule must proceed with the rule recorded: %+v", observed)
	}
}

func TestAdmissionFactsCoverThePlanTable(t *testing.T) {
	inference := Route{RouteID: "rte_01J00000000000000000000001", Family: FamilyInference,
		Fields: Fields{Endpoint: "http://127.0.0.1:11434", Model: "review-model"}}
	tags := map[string]string{}
	for _, tag := range FactsFor(inference, false, "local-only", "/repo").Tags() {
		tags[tag.Key] = tag.Value
	}
	want := map[string]string{"route:id": inference.RouteID, "route:family": FamilyInference, "route:model": "review-model",
		"route:local": "true", "route:destination-class": "loopback", "route:destination-host": "127.0.0.1",
		"profile:locality": "local-only", "project:root": "/repo"}
	for key, value := range want {
		if tags[key] != value {
			t.Errorf("%s = %q, want %q", key, tags[key], value)
		}
	}
	if _, has := tags["route:runtime"]; has {
		t.Error("an inference route carries no runtime fact")
	}
	managed := map[string]string{}
	for _, tag := range FactsFor(remoteRoute(), false, "explicit-local-or-remote", "/repo").Tags() {
		managed[tag.Key] = tag.Value
	}
	if managed["route:runtime"] != "alpha" || managed["route:local"] != "false" {
		t.Fatalf("runtime-model facts = %v", managed)
	}
	if _, has := managed["route:destination-class"]; has {
		t.Error("destination facts are inference-only")
	}
}

// Every admission fact key is one a route rule may read: a rule mixing in anything else
// is refused when written, so nothing admission supplies is unreadable and nothing a
// rule may read is unsupplied.
func TestEveryAdmissionFactIsAnAdmissionKey(t *testing.T) {
	inference := Route{RouteID: "rte_01J00000000000000000000001", Family: FamilyInference,
		Fields: Fields{Endpoint: "http://127.0.0.1:11434", Model: "m"}}
	for _, route := range []Route{remoteRoute(), inference} {
		for _, tag := range FactsFor(route, true, "local-only", "/repo").Tags() {
			if !engine.IsAdmissionKey(tag.Key) {
				t.Errorf("admission supplies %s, which a route rule may not read", tag.Key)
			}
		}
	}
}
