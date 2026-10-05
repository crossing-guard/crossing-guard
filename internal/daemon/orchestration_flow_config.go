package daemon

// The typed owner of flows.json (orchestration-flows pilot, slice B; ADR
// 0026; pass-2 B2/RT-11). A flow is the owner's authored WORKFLOW
// OPINION — stages, membership predicates, transition rules, ceiling
// numbers, reply-class shapes — held in one file in the data directory:
//
//	<dataDir>/flows.json
//
// The framework contributes the mechanism and no opinion: there is no
// built-in flow, no embedded flow file and no example flow. An absent file
// is an empty list. A fresh data dir yields zero flows and no flow, stage
// or tag-name literal exists in shipped code or script
// (orchestration_flows_boundary_test.go fails the build if that changes —
// the same gate session-organization §3.5 uses for views).
//
// The file shape follows session-views.json (ADR 0026's typed owner): one
// ordered document, format_version, strict decoding (DisallowUnknownFields).
// The owner authors and edits the file by hand; the daemon only reads it and
// has no write route for it.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"crossing-guard/internal/orchestration"
	"crossing-guard/internal/sessionquery"
)

const (
	flowsFormatVersion = 1
	flowNameMax        = 120
	flowsOriginNone    = "none"
	flowsFileBytesMax  = 262144
	flowsMax           = 16
	flowStagesMax      = 16
)

// FlowReplyClass is the config-declared shape a continue grant may deliver
// (pass-2 B1): the profile's helper reply must conform to this shape —
// bounded bytes, no tool calls, no approval vocabulary — and delivery code
// checks CONFORMANCE to this declaration only. No blessed word exists in
// framework code; the class name is the owner's label for his own rule.
type FlowReplyClass struct {
	Name     string `json:"name"`
	MaxBytes int    `json:"max_bytes"`
}

// FlowStageProfile is one profile bound to a stage's members, with the
// ceiling numbers the stage's continue grant runs under. The reply class
// declares the SHAPE a delivered reply must conform to (pass-2 B1): the
// class name is the owner's label for his own rule, and the byte bound is
// part of the declaration. Zero ceilings are refused by validation — a
// continue grant without a loop ceiling is unbounded (plan §1 slice C).
type FlowStageProfile struct {
	ProfileID       string `json:"profile_id"`
	MaxDeliveries   int64  `json:"max_deliveries"`
	DeadlineSeconds int64  `json:"deadline_seconds"`
	ReplyClass      string `json:"reply_class"`
	ReplyClassBytes int    `json:"reply_class_bytes"`
	// RefuseFencedBlocks makes the class refuse replies containing fenced
	// code blocks (``` markers) — a CONFIG choice now, not a compiled rule
	// (postwork finding: the marker refusal was an opinion smuggled into
	// code; the default stays true because a continue reply carrying code
	// fences is what a tool-injection attempt looks like).
	RefuseFencedBlocks bool `json:"refuse_fenced_blocks,omitempty"`
	DryRun             bool `json:"dry_run,omitempty"`
}

// FlowStage is one stage: its membership query (the existing sessionquery
// grammar — no second evaluator), the profiles bound on entry, and the
// transitions OUT of the stage, keyed by the evidence class that fires
// them. Transitions select published catalog kinds or owner acts only
// (plan §5 invariant 2).
type FlowStage struct {
	ID          string             `json:"id"`
	Name        string             `json:"name"`
	Membership  string             `json:"membership"`
	Profiles    []FlowStageProfile `json:"profiles"`
	Transitions []FlowStageTransit `json:"transitions,omitempty"`
}

// FlowStageTransit moves a member to the next stage when the declared
// evidence fires. Kind is a published catalog kind (session.*) or
// "owner.move"; never an agent claim. When is a sessionquery TAG TERM
// (the owner's grammar, e.g. "phase=red-teamed") evaluated against the
// member's current tag/fact vocabulary: the transition fires only when the
// signal arrives AND the tag condition holds — the owner's "until a
// red-teamed tag appears, then pop it to the next column" shape. An empty
// When fires on the signal alone.
type FlowStageTransit struct {
	Kind  string `json:"kind"`
	When  string `json:"when,omitempty"`
	To    string `json:"to"`
	Label string `json:"label,omitempty"`
}

// SavedFlow is one flow as written in the file.
type SavedFlow struct {
	ID     string      `json:"id"`
	Name   string      `json:"name"`
	Stages []FlowStage `json:"stages"`
	// MembershipTags is the tag grammar (session-organization §3.5's
	// key:value form) a session must carry to be considered for this flow.
	MembershipTags []string `json:"membership_tags"`
}

// SavedFlowRejection names an entry that could not be used, in words fit to
// show the owner.
type SavedFlowRejection struct {
	Index   int    `json:"index"`
	Name    string `json:"name,omitempty"`
	Problem string `json:"problem"`
}

// flowsDocument is what one read of the file yields.
type flowsDocument struct {
	Flows    []SavedFlow          `json:"flows"`
	Rejected []SavedFlowRejection `json:"rejected"`
	Origin   string               `json:"origin"`
}

var (
	errFlowInvalid = errors.New("flow is invalid")
)

func flowsPath(dataDir string) string { return filepath.Join(dataDir, "flows.json") }

// loadFlows reads the file on every call, the views discipline.
func loadFlows(dataDir string) flowsDocument {
	document := flowsDocument{Flows: []SavedFlow{}, Rejected: []SavedFlowRejection{}, Origin: flowsOriginNone}
	path := flowsPath(dataDir)
	raw, err := readBoundedFile(path, flowsFileBytesMax)
	if errors.Is(err, os.ErrNotExist) {
		return document
	}
	document.Origin = path
	if err != nil {
		document.Rejected = append(document.Rejected, SavedFlowRejection{Index: -1, Problem: err.Error()})
		return document
	}
	var file struct {
		FormatVersion int               `json:"format_version"`
		Flows         []json.RawMessage `json:"flows"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		document.Rejected = append(document.Rejected, SavedFlowRejection{Index: -1, Problem: "the file cannot be read: " + err.Error()})
		return document
	}
	if decoder.More() {
		document.Rejected = append(document.Rejected, SavedFlowRejection{Index: -1, Problem: "the file has text after its closing brace"})
		return document
	}
	if file.FormatVersion != flowsFormatVersion {
		document.Rejected = append(document.Rejected, SavedFlowRejection{Index: -1,
			Problem: fmt.Sprintf("format_version %d is not supported", file.FormatVersion)})
		return document
	}
	seen := map[string]bool{}
	for index, entry := range file.Flows {
		flow, err := decodeSavedFlow(entry)
		if err == nil && seen[flow.ID] {
			err = fmt.Errorf("the id %q is used twice", flow.ID)
		}
		if err == nil && len(document.Flows) >= flowsMax {
			err = fmt.Errorf("more than %d flows", flowsMax)
		}
		if err != nil {
			document.Rejected = append(document.Rejected, SavedFlowRejection{Index: index, Name: flow.Name, Problem: err.Error()})
			continue
		}
		seen[flow.ID] = true
		document.Flows = append(document.Flows, flow)
	}
	return document
}

func decodeSavedFlow(entry json.RawMessage) (SavedFlow, error) {
	var flow SavedFlow
	decoder := json.NewDecoder(bytes.NewReader(entry))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&flow); err != nil {
		var loose struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(entry, &loose)
		return SavedFlow{Name: loose.Name}, errors.New("the entry has a field this version does not know, or the wrong type")
	}
	return flow, validateSavedFlow(flow)
}

func validateSavedFlow(flow SavedFlow) error {
	if flow.ID == "" || strings.ContainsAny(flow.ID, "/\\ \t\n") {
		return fmt.Errorf("%w: it needs an id without spaces or slashes", errFlowInvalid)
	}
	name := strings.TrimSpace(flow.Name)
	if name == "" || len(name) > flowNameMax {
		return fmt.Errorf("%w: it needs a name of at most %d bytes", errFlowInvalid, flowNameMax)
	}
	if len(flow.Stages) == 0 || len(flow.Stages) > flowStagesMax {
		return fmt.Errorf("%w: a flow needs between one and %d stages", errFlowInvalid, flowStagesMax)
	}
	stageIDs := map[string]bool{}
	for _, stage := range flow.Stages {
		if stage.ID == "" || strings.ContainsAny(stage.ID, "/\\ \t\n") {
			return fmt.Errorf("%w: stage %q needs an id without spaces or slashes", errFlowInvalid, stage.Name)
		}
		if stageIDs[stage.ID] {
			return fmt.Errorf("%w: stage id %q is used twice", errFlowInvalid, stage.ID)
		}
		stageIDs[stage.ID] = true
	}
	// Transitions may name any stage, so the id set is complete before any
	// transition is checked (forward references are legal — a flow's stages
	// form a graph, not a list).
	for _, stage := range flow.Stages {
		if err := validateFlowQuery(stage.Membership); err != nil {
			return fmt.Errorf("%w: stage %q membership: %v", errFlowInvalid, stage.Name, err)
		}
		// A stage MAY bind zero profiles: a pure waiting column (the
		// postwork arming review stage — "waiting to approve
		// implementation" — has no helper; it exists to receive
		// transitions). Profiles are still validated when present.
		for _, profile := range stage.Profiles {
			if profile.ProfileID == "" {
				return fmt.Errorf("%w: stage %q has a binding without a profile id", errFlowInvalid, stage.Name)
			}
			// The loop ceiling is mandatory (plan §1 slice C): a continue
			// grant without any bound is unbounded.
			if profile.MaxDeliveries <= 0 && profile.DeadlineSeconds <= 0 {
				return fmt.Errorf("%w: stage %q profile %s needs a max_deliveries or deadline ceiling", errFlowInvalid, stage.Name, profile.ProfileID)
			}
			if profile.ReplyClass == "" {
				return fmt.Errorf("%w: stage %q profile %s needs a reply class (the declared shape a reply must conform to)", errFlowInvalid, stage.Name, profile.ProfileID)
			}
			if profile.ReplyClassBytes <= 0 {
				return fmt.Errorf("%w: stage %q profile %s reply class %q needs a reply_class_bytes bound", errFlowInvalid, stage.Name, profile.ProfileID, profile.ReplyClass)
			}
		}
		for _, transit := range stage.Transitions {
			if transit.Kind == "" || transit.To == "" {
				return fmt.Errorf("%w: stage %q has an incomplete transition", errFlowInvalid, stage.Name)
			}
			if !flowTransitionKindKnown(transit.Kind) {
				return fmt.Errorf("%w: stage %q transition kind %q is not a published signal or owner act", errFlowInvalid, stage.Name, transit.Kind)
			}
			if !stageIDs[transit.To] {
				return fmt.Errorf("%w: stage %q transitions to unknown stage %q", errFlowInvalid, stage.Name, transit.To)
			}
			// The When clause is a TAG TERM in the owner's grammar: the
			// same query language membership uses, evaluated against the
			// member's tag/fact vocabulary at fire time. Validation parses
			// it here so a typo surfaces at author time, not mid-flight.
			if transit.When != "" {
				if err := validateFlowQuery(transit.When); err != nil {
					return fmt.Errorf("%w: stage %q transition when %q: %v", errFlowInvalid, stage.Name, transit.When, err)
				}
			}
		}
	}
	return nil
}

// flowQueryLimits are the grammar limits flow validation uses — the same
// shape the console organization config supplies; the shipped defaults are
// the values the views owner uses when nothing configured a tighter bound.
func flowQueryLimits() sessionquery.Limits {
	return sessionquery.Limits{QueryBytes: 1024, Terms: 24, GlobExpansion: 200}
}

// flowTransitionKindKnown is the closed rule of plan §5 invariant 2: a
// transition selects a published catalog kind or the owner act; agent
// claims are refused here by construction.
func flowTransitionKindKnown(kind string) bool {
	if kind == "owner.move" {
		return true
	}
	return orchestration.KnownSignal(kind)
}

// LoadFlowsHealth is doctor's read of the flows document: the file's origin,
// every flow the daemon could read, and every rejection in the owner's
// words. An absent file is an empty list with origin "none" — a
// hand-authored file must never fail silently (postwork PO-8).
func LoadFlowsHealth(dataDir string) (origin string, flows []SavedFlow, rejected []SavedFlowRejection) {
	document := loadFlows(dataDir)
	return document.Origin, document.Flows, document.Rejected
}

// validateFlowQuery parses a stage's query and refuses what a flow cannot
// decide. A flow evaluates its queries over a row it builds from a member's
// tags and facts alone (flowCandidateRow): that row has no call or line
// count, so calls: or lines: would read zero for every member.
func validateFlowQuery(text string) error {
	query, err := sessionquery.Parse(text, flowQueryLimits())
	if err != nil {
		return err
	}
	if query.HasNumberTerm() {
		return errors.New("a flow cannot use calls: or lines: — it sees a member's tags and facts, not its size")
	}
	return nil
}
