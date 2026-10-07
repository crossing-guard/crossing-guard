// Package ruledoc is the portable rule document: the shipped catalog, parsing with
// legacy migration and predicate compilation, content digests, and mechanical diffs.
//
// It knows nothing about where a document lives, who selected it, or install cohorts —
// those belong to internal/rulebook, the local owner. A separate server module imports
// this package so that bundle validation and preview run the same code the hook
// enforces (ADR 0025: one rule format, one evaluator). Evaluation itself stays in
// engine.Decide; this package deliberately has no verdict API.
package ruledoc

import (
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"crossing-guard/engine"
)

// CanaryMarker is the string the shipped proof rule fires on. No genuine command
// should contain it; demo and verification use it to prove the real hook path.
const CanaryMarker = "crossing-guard-canary"

// CanaryRuleID is the shipped proof rule's id. A live deny event carrying it is the only
// durable evidence that a block reaches a runtime's agent, so it is what verification and
// the device report look for. A rulebook that renames or deletes the rule, or puts an
// earlier deny rule in front of it, forfeits that evidence — reported as "no canary rule
// active", which is a different fact from "never observed".
const CanaryRuleID = "canary-deny"

// DemoRuntime is the runtime name `crossing-guard demo` drives the hook under. It never
// credits a real runtime: a canary the demo sends proves the hook path, not an agent's.
const DemoRuntime = "demo"

//go:embed rules.default.json
var defaultRules []byte

// defaultRules is retained byte-for-byte as the compatibility baseline for pre-C5c
// installations. New users must explicitly select one of the public catalog documents.
//
//go:embed rules.safety-starter.json
var safetyStarterRules []byte

//go:embed rules.security-observe.json
var securityObserveRules []byte

// DefaultRules is the pre-C5c compatibility baseline document. Each call returns a
// copy: the embedded bytes are the identity every digest keys on, and an importer —
// this module or the server — must not be able to change them in memory.
func DefaultRules() []byte { return append([]byte(nil), defaultRules...) }

// SafetyStarterRules is the public safety-starter catalog document (a copy; see DefaultRules).
func SafetyStarterRules() []byte { return append([]byte(nil), safetyStarterRules...) }

// SecurityObserveRules is the public security-observe catalog document (a copy; see DefaultRules).
func SecurityObserveRules() []byte { return append([]byte(nil), securityObserveRules...) }

// Parse runs legacy migration, decoding, and predicate compilation — the one path every
// document takes before it can decide anything, on a device or on a server.
func Parse(raw []byte) (*engine.Policy, error) {
	raw, err := migrateLegacy(raw)
	if err != nil {
		return nil, err
	}
	var policy engine.Policy
	if err := json.Unmarshal(raw, &policy); err != nil {
		return nil, err
	}
	if err := engine.CompilePredicates(&policy); err != nil {
		return nil, err
	}
	// A rule that reads a route: fact beside a term route admission cannot answer has
	// no evaluation site that holds both, so it is refused here, where every document
	// is written and loaded (team rest-of-release plan §5.4, OD-25).
	if err := engine.ValidateRouteRules(&policy); err != nil {
		return nil, err
	}
	return &policy, nil
}

// Validate checks a candidate through the same migration/parser/compiler as a load.
func Validate(raw []byte) error {
	_, err := Parse(raw)
	return err
}

// ShippedSummary derives a concise action-grouped description from the embedded
// default document so onboarding and doctor cannot drift from what actually ships.
func ShippedSummary() string {
	policy, err := Parse(defaultRules)
	if err != nil {
		return "(shipped ruleset unreadable: " + err.Error() + ")"
	}
	byAction := map[string][]string{}
	var order []string
	for _, rule := range policy.Rules {
		action := rule.Action
		if action == "" {
			action = string(rule.Mode)
		}
		if len(byAction[action]) == 0 {
			order = append(order, action)
		}
		byAction[action] = append(byAction[action], rule.ID)
	}
	parts := make([]string, 0, len(order))
	for _, action := range order {
		parts = append(parts, action+": "+strings.Join(byAction[action], ", "))
	}
	return strings.Join(parts, " · ")
}

// ContentDigest is the document identity every selection, adoption, and bundle keys on.
// It must hash identically wherever it runs, which is why it lives here and nowhere else.
func ContentDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("sha256:%x", sum)
}

// InvalidDocumentError distinguishes authored-policy rejection from persistence
// failure so an HTTP owner can preserve its 422 versus 500 contract.
type InvalidDocumentError struct{ Err error }

func (e *InvalidDocumentError) Error() string { return e.Err.Error() }
func (e *InvalidDocumentError) Unwrap() error { return e.Err }
