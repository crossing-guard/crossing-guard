package orchestration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"crossing-guard/internal/approvalchoice"
)

// One claim owner. Both lanes (stateless review and managed agents) decode
// through this module: the redesign taxonomy (reviewer/follower/helper) is the
// only claim vocabulary, with typed findings, contained references, and
// declared tags.

const (
	maxFindings          = 16
	maxRefsPerFinding    = 8
	maxRefPathBytes      = 512
	maxTagsPerClaim      = 8
	maxVerdictBytes      = 200
	maxClaimCitations    = 8
	maxDeclaredTagLength = 64
)

// Ref is a model-authored reference. It is a CONTAINED input class: shape is
// validated here; resolution happens in a sandboxed resolver (project-root jail
// for file/doc, catalog for session, exact session+event for event) and its
// outcome is recorded with the claim, never retried into a different target.
type Ref struct {
	Kind    string `json:"kind"` // file | doc | event | session
	Path    string `json:"path,omitempty"`
	Line    int    `json:"line,omitempty"`
	Session string `json:"session,omitempty"` // required for kind=event
	Anchor  string `json:"anchor,omitempty"`  // opaque; equality-only
}

type Finding struct {
	Severity  string `json:"severity"` // info | warn | error
	Statement string `json:"statement"`
	Refs      []Ref  `json:"refs,omitempty"`
}

// AgentClaim is the one wire contract for agent output, v1 fields included.
type AgentClaim struct {
	Action         string    `json:"action"`
	Message        string    `json:"message"`
	Citations      []string  `json:"citations"`
	Verdict        string    `json:"verdict,omitempty"`
	Findings       []Finding `json:"findings,omitempty"`
	Tags           []string  `json:"tags,omitempty"`
	StageID        string    `json:"stage_id,omitempty"`
	ChildProfileID string    `json:"child_profile_id,omitempty"`
	// Selections answers the questions a held call was asking. Only a reviewer
	// may carry them, and only the caller that holds the exact action can check
	// the values; this decoder enforces the type contract.
	Selections []approvalchoice.ChoiceSelection `json:"selections,omitempty"`
}

// agentTypeContract is the data-driven per-type validation table. Adding an
// action or type edits data, not control flow.
type agentTypeContract struct {
	actions          map[string]bool
	allowsChild      bool // child_profile_id permitted (helpers choosing launch_profile)
	allowsSelections bool // selections permitted (reviewers answering a held question)
}

var agentContracts = map[string]agentTypeContract{
	"reviewer": {actions: map[string]bool{"allow": true, "deny": true, "abstain": true},
		allowsSelections: true},
	"follower": {actions: map[string]bool{"no_action": true, "advise_user": true}},
	"helper": {actions: map[string]bool{"no_action": true, "advise_user": true, "draft_reply": true,
		"reply": true, "launch_profile": true, "request_interrupt": true, "send_message": true},
		allowsChild: true},
}

// RequiredActionGrant distinguishes known nonacting claims from unknown actions.
func RequiredActionGrant(action string) (string, bool) {
	grant, known := actionGrants[action]
	return grant, known
}

var actionGrants = map[string]string{
	"allow": "", "deny": "", "abstain": "", "no_action": "", "advise_user": "", "draft_reply": "",
	"reply": "reply", "launch_profile": "launch-profile", "request_interrupt": "request-interrupt", "send_message": "send-message",
}

func HasAutomaticActionGrant(grants []string) bool {
	for _, grant := range actionGrants {
		if grant != "" && containsValue(grants, grant) {
			return true
		}
	}
	return false
}

var findingSeverities = map[string]bool{"info": true, "warn": true, "error": true}
var refKinds = map[string]bool{"file": true, "doc": true, "event": true, "session": true}

// ClaimSchemaDescription generates the prompt-facing contract text from the
// data table, so prompt and validator cannot drift.
func ClaimSchemaDescription(agentType string) string {
	contract, ok := agentContracts[agentType]
	if !ok {
		return "Required fields: action, message, citations."
	}
	actions := make([]string, 0, len(contract.actions))
	for action := range contract.actions {
		actions = append(actions, action)
	}
	sortStrings(actions)
	selectionNote := ""
	if contract.allowsSelections {
		selectionNote = " When the action carries choice prompts and you allow, return selections:" +
			" one entry per prompt as {prompt_id, values}, each value an offered option label."
	}
	description := "Required fields: action, message, citations. citations is an array of at most " +
		fmt.Sprint(maxClaimCitations) + " supplied fact labels. action must be one of: " +
		strings.Join(actions, ", ") + "." +
		" Optional: verdict (one short line), findings (array, each {severity: info|warn|error, statement, refs}), tags (only declared tags)." +
		" A ref is {kind: file|doc|event|session, path, line, session, anchor}; kind event requires session."
	if contract.allowsChild {
		description += " For launch_profile also return child_profile_id (stage_id is optional)."
	}
	return description + selectionNote
}

// DecodeAgentClaim validates one strict JSON object against the type contract,
// the supplied fact labels, the allowlisted child profiles, and the profile's
// declared tag vocabulary. declaredTags nil means the profile declared none, so
// any tag is refused.
func DecodeAgentClaim(raw []byte, agentType string, maxOutputBytes int, labels, allowedProfiles, declaredTags []string) (AgentClaim, error) {
	contract, ok := agentContracts[agentType]
	if !ok {
		return AgentClaim{}, errors.New("unknown agent type")
	}
	if len(raw) < 2 || len(raw) > maxOutputBytes || !utf8.Valid(raw) {
		return AgentClaim{}, errors.New("agent output is empty, oversized, or invalid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var claim AgentClaim
	if err := decoder.Decode(&claim); err != nil {
		return AgentClaim{}, errors.New("agent output JSON is malformed")
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return AgentClaim{}, errors.New("agent output contains more than one JSON value")
	} else if !errors.Is(err, io.EOF) {
		return AgentClaim{}, errors.New("agent output has trailing invalid data")
	}
	if !contract.actions[claim.Action] || strings.TrimSpace(claim.Message) == "" ||
		len(claim.Message) > maxOutputBytes || len(claim.Citations) > maxClaimCitations {
		return AgentClaim{}, errors.New("agent output fields are invalid")
	}
	allowedLabels := map[string]bool{}
	for _, label := range labels {
		allowedLabels[label] = true
	}
	seen := map[string]bool{}
	for _, label := range claim.Citations {
		if !allowedLabels[label] || seen[label] {
			return AgentClaim{}, errors.New("agent citation is not a supplied fact")
		}
		seen[label] = true
	}
	if err := validateStageFields(contract, claim, allowedProfiles); err != nil {
		return AgentClaim{}, err
	}
	if len(claim.Selections) != 0 && !contract.allowsSelections {
		return AgentClaim{}, errors.New("this agent type cannot answer a held question")
	}
	if err := validateFindings(claim); err != nil {
		return AgentClaim{}, err
	}
	if err := validateTags(claim.Tags, declaredTags); err != nil {
		return AgentClaim{}, err
	}
	return claim, nil
}

func validateStageFields(contract agentTypeContract, claim AgentClaim, allowedProfiles []string) error {
	if !contract.allowsChild && (claim.StageID != "" || claim.ChildProfileID != "") {
		return errors.New("agent output includes type-incompatible fields")
	}
	if claim.Action == "launch_profile" {
		if !containsValue(allowedProfiles, claim.ChildProfileID) {
			return errors.New("agent selected a profile outside its allowlist")
		}
	} else if claim.ChildProfileID != "" && claim.Action == "no_action" {
		return errors.New("no-action output cannot select a child")
	}
	return nil
}

func containsValue(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func validateFindings(claim AgentClaim) error {
	if len(claim.Findings) == 0 {
		if len(claim.Verdict) > maxVerdictBytes {
			return errors.New("agent verdict is oversized")
		}
		return nil
	}
	if len(claim.Findings) > maxFindings {
		return errors.New("agent findings exceed the bound")
	}
	if strings.TrimSpace(claim.Verdict) == "" || len(claim.Verdict) > maxVerdictBytes {
		return errors.New("findings require a bounded one-line verdict")
	}
	for _, finding := range claim.Findings {
		if !findingSeverities[finding.Severity] || strings.TrimSpace(finding.Statement) == "" {
			return errors.New("agent finding severity or statement is invalid")
		}
		if len(finding.Refs) > maxRefsPerFinding {
			return errors.New("agent finding refs exceed the bound")
		}
		for _, ref := range finding.Refs {
			if err := validateRefShape(ref); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateRefShape(ref Ref) error {
	if !refKinds[ref.Kind] {
		return errors.New("agent ref kind is invalid")
	}
	if ref.Kind == "event" && strings.TrimSpace(ref.Session) == "" {
		return errors.New("event ref requires session identity")
	}
	if ref.Kind != "session" && strings.TrimSpace(ref.Path) == "" && ref.Kind != "event" {
		return errors.New("ref path is required")
	}
	if len(ref.Path) > maxRefPathBytes || len(ref.Session) > maxRefPathBytes || len(ref.Anchor) > maxRefPathBytes {
		return errors.New("ref field is oversized")
	}
	if ref.Line < 0 {
		return errors.New("ref line is invalid")
	}
	return nil
}

func validateTags(tags, declared []string) error {
	if len(tags) == 0 {
		return nil
	}
	if len(tags) > maxTagsPerClaim {
		return errors.New("agent tags exceed the bound")
	}
	allowed := map[string]bool{}
	for _, tag := range declared {
		allowed[tag] = true
	}
	seen := map[string]bool{}
	for _, tag := range tags {
		if tag == "" || len(tag) > maxDeclaredTagLength || !allowed[tag] || seen[tag] {
			return errors.New("agent tag is not in the profile's declared vocabulary")
		}
		seen[tag] = true
	}
	return nil
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
