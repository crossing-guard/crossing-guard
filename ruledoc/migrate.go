package ruledoc

import (
	"encoding/json"
	"fmt"
	"regexp"
)

type legacyGuard struct {
	Name    string `json:"name"`
	Pattern string `json:"pattern"`
}

type legacyRule struct {
	ID     string        `json:"id"`
	Intent string        `json:"intent"`
	Action string        `json:"action"`
	Guards []legacyGuard `json:"guards"`
}

func isLegacy(raw []byte) bool {
	var probe struct {
		Rules []struct {
			Guards []legacyGuard `json:"guards"`
		} `json:"rules"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return false
	}
	for _, rule := range probe.Rules {
		if len(rule.Guards) > 0 {
			return true
		}
	}
	return false
}

// migrateLegacy converts old guard arrays in flight while preserving new-format
// rules and top-level fields byte-semantically. Files remain untouched for downgrade
// compatibility until a user explicitly saves through the console.
func migrateLegacy(raw []byte) ([]byte, error) {
	if !isLegacy(raw) {
		return raw, nil
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("rules look like the old format but will not parse: %w", err)
	}
	var rawRules []json.RawMessage
	if value, ok := document["rules"]; ok {
		if err := json.Unmarshal(value, &rawRules); err != nil {
			return nil, fmt.Errorf("rules array will not parse: %w", err)
		}
	}
	converted := make([]json.RawMessage, 0, len(rawRules))
	for _, rawRule := range rawRules {
		var old legacyRule
		if err := json.Unmarshal(rawRule, &old); err != nil || len(old.Guards) == 0 {
			converted = append(converted, rawRule)
			continue
		}
		newRule, err := convertLegacyRule(old)
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(newRule)
		if err != nil {
			return nil, err
		}
		converted = append(converted, encoded)
	}
	rulesJSON, err := json.Marshal(converted)
	if err != nil {
		return nil, err
	}
	document["rules"] = rulesJSON
	return json.Marshal(document)
}

type currentRule struct {
	ID      string         `json:"id"`
	Intent  string         `json:"intent,omitempty"`
	Action  string         `json:"action"`
	Message string         `json:"message,omitempty"`
	If      map[string]any `json:"if"`
}

func convertLegacyRule(rule legacyRule) (currentRule, error) {
	action := rule.Action
	if action == "" {
		action = "ask"
	}
	terms := make([]map[string]any, 0, len(rule.Guards))
	for _, guard := range rule.Guards {
		if _, err := regexp.Compile(guard.Pattern); err != nil {
			return currentRule{}, fmt.Errorf("rule %s guard %q: bad regex %q: %w",
				rule.ID, guard.Name, guard.Pattern, err)
		}
		terms = append(terms, map[string]any{"tag": "command", "matches": guard.Pattern})
	}
	var predicate map[string]any
	switch len(terms) {
	case 0:
		predicate = map[string]any{}
	case 1:
		predicate = terms[0]
	default:
		anyTerms := make([]any, len(terms))
		for index, term := range terms {
			anyTerms[index] = term
		}
		predicate = map[string]any{"any": anyTerms}
	}
	return currentRule{ID: rule.ID, Intent: rule.Intent, Action: action,
		Message: rule.Intent, If: predicate}, nil
}
