package ruledoc

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"crossing-guard/engine"
)

// RuleChange is a mechanical identity/action/predicate change. It is deliberately
// not called a semantic diff: arbitrary regex and state predicates are not enumerable,
// so a preview shows what changed in the document, never what will fire.
type RuleChange struct {
	ID              string `json:"id"`
	BeforeAction    string `json:"before_action,omitempty"`
	AfterAction     string `json:"after_action,omitempty"`
	BeforePredicate string `json:"before_predicate,omitempty"`
	AfterPredicate  string `json:"after_predicate,omitempty"`
}

// MechanicalDiff reports rule ids added, removed, and changed between two parsed
// documents. Output is sorted so a preview is stable across runs and machines.
func MechanicalDiff(before, after *engine.Policy) ([]string, []string, []RuleChange) {
	beforeByID := make(map[string]engine.Rule)
	afterByID := make(map[string]engine.Rule)
	for _, rule := range before.Rules {
		beforeByID[rule.ID] = rule
	}
	for _, rule := range after.Rules {
		afterByID[rule.ID] = rule
	}
	var added, removed []string
	var changed []RuleChange
	for id, oldRule := range beforeByID {
		newRule, ok := afterByID[id]
		if !ok {
			removed = append(removed, id)
			continue
		}
		oldAction, newAction := RuleAction(oldRule), RuleAction(newRule)
		oldPredicate, newPredicate := PredicateDigest(oldRule.If), PredicateDigest(newRule.If)
		if oldAction != newAction || oldPredicate != newPredicate {
			changed = append(changed, RuleChange{ID: id, BeforeAction: oldAction,
				AfterAction: newAction, BeforePredicate: oldPredicate, AfterPredicate: newPredicate})
		}
	}
	for id := range afterByID {
		if _, ok := beforeByID[id]; !ok {
			added = append(added, id)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Slice(changed, func(i, j int) bool { return changed[i].ID < changed[j].ID })
	return added, removed, changed
}

// RuleAction is the authored decision vocabulary a rule presents: Action when set,
// otherwise the ladder-native Mode.
func RuleAction(rule engine.Rule) string {
	if rule.Action != "" {
		return rule.Action
	}
	return string(rule.Mode)
}

// PredicateDigest identifies a predicate by its canonical JSON, so a preview can say
// "changed" without pretending to enumerate what a regex or state term matches.
func PredicateDigest(predicate engine.Predicate) string {
	raw, _ := json.Marshal(predicate)
	return ContentDigest(raw)
}

// PolicySummary counts rules per action, e.g. "ask=2, deny=3".
func PolicySummary(policy *engine.Policy) string {
	counts := map[string]int{}
	for _, rule := range policy.Rules {
		counts[RuleAction(rule)]++
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, counts[key]))
	}
	return strings.Join(parts, ", ")
}
