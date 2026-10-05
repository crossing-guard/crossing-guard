// Package modelroute owns named model routes: a named, device-local choice of where a
// model call goes (team rest-of-release plan §5). A place — a managed binding, a
// fallback chain entry, or the reviewer — references a route by id; the route owner is
// the one place a runtime, a model id or an inference endpoint is typed.
//
// Storage is laid out the way internal/orchestration/profilefs lays out profiles, under
// <dataDir>/models/routes/: mutation.lock; selections/<route_id>.json (the current
// revision, a bounded history and a state token); documents/<revision_digest>/route.json,
// immutable. A route is not link state (it survives unlink), is minted on the device and
// never travels, and never holds a credential: neither family has a field for one.
//
// The package also holds route admission (admit.go): the one evaluator of rules that
// read a route: fact (plan §5.4, OD-25).
package modelroute

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"crossing-guard/infer"
)

// The two route families and the one kind each has in this release (plan §5.1, §5.2).
// The inference kind is fixed because the reviewer host builds its request path only
// through infer.NewLocalOllamaPath; more kinds arrive with the model-routes draft.
const (
	FamilyRuntimeModel = "runtime-model"
	FamilyInference    = "inference"

	KindRuntimeModel = "runtime-model"
	KindLocalOllama  = "local-ollama"
)

const (
	revisionFormat  = "crossing-guard-model-route-revision-v1"
	selectionFormat = "crossing-guard-model-route-selection-v1"
	maxJSONBytes    = 512 * 1024

	revisionDigestFrame = "crossing-guard-model-route-revision-v1\x00"
	previewDigestFrame  = "crossing-guard-model-route-preview-v1\x00"
	selectionTokenFrame = "crossing-guard-model-route-selection-state-v1\x00"
	absentTokenFrame    = "crossing-guard-model-route-selection-absent-v1\x00"

	// IDPrefix is the typed-id prefix of a route: rte_<ulid>, minted on the device.
	IDPrefix = "rte"

	maxModelBytes   = 128
	maxRuntimeBytes = 64
	maxEffortBytes  = 256
)

var routeIDPattern = regexp.MustCompile(`^rte_[0-9A-HJKMNP-TV-Z]{10,32}$`)

// ValidID reports whether id has the shape of a route id.
func ValidID(id string) bool { return routeIDPattern.MatchString(id) }

// Effort is a runtime-model route's optional thinking effort. It has the JSON shape of
// the store's thinking-effort value, so a binding's resolved copy is the same bytes.
type Effort struct {
	Kind  string `json:"kind"`
	Value string `json:"value,omitempty"`
}

// Fields are a route's family-specific facts. A runtime-model route has a runtime, a
// model (empty means the runtime's own default selection, as a binding stored it before
// routes existed) and an optional thinking effort. An inference route has a literal
// loopback endpoint and a model. No field holds a credential.
type Fields struct {
	Runtime        string  `json:"runtime,omitempty"`
	Model          string  `json:"model"`
	ThinkingEffort *Effort `json:"thinking_effort,omitempty"`
	Endpoint       string  `json:"endpoint,omitempty"`
}

// Revision is one immutable version of a route.
type Revision struct {
	FormatVersion string `json:"format_version"`
	RouteID       string `json:"route_id"`
	Name          string `json:"name"`
	Family        string `json:"family"`
	Kind          string `json:"kind"`
	Fields        Fields `json:"fields"`
	CreatedAt     string `json:"created_at"`
}

// Draft is the authored part of a revision: what Preview and Select take. RouteID is
// empty for a route that does not exist yet.
type Draft struct {
	RouteID string `json:"route_id,omitempty"`
	Name    string `json:"name"`
	Family  string `json:"family"`
	Fields  Fields `json:"fields"`
}

// KindFor returns the one kind a family has in this release, or "".
func KindFor(family string) string {
	switch family {
	case FamilyRuntimeModel:
		return KindRuntimeModel
	case FamilyInference:
		return KindLocalOllama
	}
	return ""
}

// NameKey is the identity a name is unique under: trimmed and case-folded.
func NameKey(name string) string {
	return strings.ToLower(strings.Join(strings.Fields(name), " "))
}

// normalizeDraft trims the name and canonicalises the fields, refusing a draft that is
// not a valid route. It never reads storage.
func normalizeDraft(draft Draft, config Config) (Draft, error) {
	draft.Name = strings.Join(strings.Fields(draft.Name), " ")
	if draft.Name == "" {
		return Draft{}, invalid("name", "Give the route a name.")
	}
	if utf8.RuneCountInString(draft.Name) > config.NameMaxRunes || !printable(draft.Name) {
		return Draft{}, invalid("name", "Use a shorter name with no control characters.")
	}
	if draft.RouteID != "" && !ValidID(draft.RouteID) {
		return Draft{}, invalid("route_id", "Use an existing route id, or none to create a route.")
	}
	fields, err := normalizeFields(draft.Family, draft.Fields)
	if err != nil {
		return Draft{}, err
	}
	draft.Fields = fields
	return draft, nil
}

func normalizeFields(family string, fields Fields) (Fields, error) {
	switch family {
	case FamilyRuntimeModel:
		if fields.Endpoint != "" {
			return Fields{}, invalid("fields.endpoint", "A runtime-model route has no endpoint.")
		}
		if fields.Runtime == "" || len(fields.Runtime) > maxRuntimeBytes || !runtimeName(fields.Runtime) {
			return Fields{}, invalid("fields.runtime", "Choose a runtime.")
		}
		if len(fields.Model) > maxModelBytes || !printable(fields.Model) || strings.TrimSpace(fields.Model) != fields.Model {
			return Fields{}, invalid("fields.model", "Use a model id with no surrounding space or control characters.")
		}
		if fields.ThinkingEffort != nil {
			if err := fields.ThinkingEffort.validate(); err != nil {
				return Fields{}, err
			}
			effort := *fields.ThinkingEffort
			fields.ThinkingEffort = &effort
		}
		return fields, nil
	case FamilyInference:
		if fields.Runtime != "" || fields.ThinkingEffort != nil {
			return Fields{}, invalid("fields.runtime", "An inference route has an endpoint and a model only.")
		}
		canonical, err := infer.CanonicalLoopbackEndpoint(fields.Endpoint)
		if err != nil {
			return Fields{}, invalid("fields.endpoint", "Use a literal loopback endpoint such as http://127.0.0.1:11434.")
		}
		if fields.Model == "" || len(fields.Model) > maxModelBytes || !printable(fields.Model) || strings.TrimSpace(fields.Model) == "" {
			return Fields{}, invalid("fields.model", "Name the model the endpoint serves.")
		}
		return Fields{Endpoint: canonical, Model: fields.Model}, nil
	}
	return Fields{}, invalid("family", "Choose runtime-model or inference.")
}

func (effort Effort) validate() error {
	if effort.Kind == "inherit" && effort.Value == "" {
		return nil
	}
	if effort.Kind == "level" && effort.Value != "" && len(effort.Value) <= maxEffortBytes && printable(effort.Value) {
		return nil
	}
	return invalid("fields.thinking_effort", "Thinking effort is inherit, or a level with a value.")
}

func printable(value string) bool {
	return utf8.ValidString(value) && !strings.ContainsFunc(value, unicode.IsControl)
}

func runtimeName(value string) bool {
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// SameFields reports whether two routes carry the same family-specific facts.
func SameFields(a, b Fields) bool {
	if a.Runtime != b.Runtime || a.Model != b.Model || a.Endpoint != b.Endpoint {
		return false
	}
	if (a.ThinkingEffort == nil) != (b.ThinkingEffort == nil) {
		return false
	}
	return a.ThinkingEffort == nil || *a.ThinkingEffort == *b.ThinkingEffort
}

// RevisionDigest is the identity of one immutable revision: every field of it,
// including when it was created.
func RevisionDigest(revision Revision) (string, error) {
	body, err := json.Marshal(revision)
	if err != nil {
		return "", err
	}
	return framedDigest(revisionDigestFrame, body), nil
}

// PreviewDigest is the identity of what a person previewed: the authored draft after
// normalisation. Select refuses a draft whose digest is not the one previewed.
func PreviewDigest(draft Draft) (string, error) {
	body, err := json.Marshal(draft)
	if err != nil {
		return "", err
	}
	return framedDigest(previewDigestFrame, body), nil
}

func framedDigest(frame string, body []byte) string {
	sum := sha256.Sum256(append([]byte(frame), body...))
	return "sha256-v1:" + hex.EncodeToString(sum[:])
}

func validDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256-v1:") || len(value) != len("sha256-v1:")+64 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256-v1:"))
	return err == nil
}

func validateRevision(revision Revision, config Config) error {
	if revision.FormatVersion != revisionFormat || !ValidID(revision.RouteID) ||
		revision.Kind == "" || revision.Kind != KindFor(revision.Family) {
		return errors.New("invalid model route revision")
	}
	normalized, err := normalizeDraft(Draft{RouteID: revision.RouteID, Name: revision.Name,
		Family: revision.Family, Fields: revision.Fields}, Config{NameMaxRunes: nameMaxRunesCeiling, HistoryMax: config.HistoryMax})
	if err != nil || normalized.Name != revision.Name || !SameFields(normalized.Fields, revision.Fields) {
		return errors.New("model route revision is not in canonical form")
	}
	if _, err := time.Parse(time.RFC3339Nano, revision.CreatedAt); err != nil {
		return errors.New("invalid model route revision time")
	}
	return nil
}

// Problem is a typed refusal a caller can show: a code, the field it is about, what is
// wrong and what to do.
type Problem struct {
	Code     string `json:"code"`
	Field    string `json:"field,omitempty"`
	Message  string `json:"message"`
	Recovery string `json:"recovery,omitempty"`
	cause    error
}

func (problem *Problem) Error() string { return problem.Message }

// Unwrap exposes the storage error a storage or integrity problem wraps.
func (problem *Problem) Unwrap() error { return problem.cause }

// Problem codes.
const (
	CodeInvalid       = "invalid_route"
	CodeNotFound      = "route_not_found"
	CodeNameTaken     = "route_name_taken"
	CodeStateConflict = "state_conflict"
	CodeInUse         = "route_in_use"
	CodeIntegrity     = "integrity_conflict"
	CodeStorage       = "storage_error"
	CodeUnconfirmed   = "confirmation_required"
)

func invalid(field, message string) *Problem {
	return &Problem{Code: CodeInvalid, Field: field, Message: message}
}

func problem(code, field, message, recovery string) *Problem {
	return &Problem{Code: code, Field: field, Message: message, Recovery: recovery}
}

func storageProblem(cause error) *Problem {
	return &Problem{Code: CodeStorage, Message: "Model route storage is unavailable.",
		Recovery: "Review the local model route storage and retry.", cause: cause}
}

func integrityConflict(cause error) error {
	var existing *Problem
	if errors.As(cause, &existing) && existing.Code == CodeIntegrity {
		return cause
	}
	return &Problem{Code: CodeIntegrity, Message: "Stored model route state failed integrity checks.",
		Recovery: "Preserve the local model route data for review; do not overwrite it as a normal update.", cause: cause}
}

// IsCode reports whether err is a route Problem with this code.
func IsCode(err error, code string) bool {
	var found *Problem
	return errors.As(err, &found) && found.Code == code
}
