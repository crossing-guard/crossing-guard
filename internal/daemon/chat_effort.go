package daemon

// Effort metadata is model-scoped. IDs are native adapter values; labels only
// unify presentation and never authorize a value on another model.
import (
	"crossing-guard/store"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

type ChatEffortChoice struct {
	MappingDigest string `json:"mapping_digest,omitempty"`
	ID            string `json:"id"`
	Label         string `json:"label"`
	Description   string `json:"description,omitempty"`
}

type ChatEffortCapability struct {
	State     string             `json:"state"`
	Choices   []ChatEffortChoice `json:"choices,omitempty"`
	Default   string             `json:"default,omitempty"`
	CanStart  bool               `json:"can_start"`
	CanResume bool               `json:"can_resume"`
}

// effortLabel is a display vocabulary, not a native-value allowlist.
func effortLabel(level string) string {
	switch level {
	case "none":
		return "None"
	case "minimal":
		return "Minimal"
	case "low":
		return "Low"
	case "medium":
		return "Medium"
	case "high":
		return "High"
	case "xhigh":
		return "Extra high"
	case "max":
		return "Max"
	case "ultra":
		return "Ultra"
	default:
		return ""
	}
}

func nativeEffortCapability(levels []string, defaultLevel string) *ChatEffortCapability {
	capability := &ChatEffortCapability{State: "unknown"}
	for _, level := range levels {
		label := effortLabel(level)
		if label == "" {
			continue
		}
		capability.Choices = append(capability.Choices, ChatEffortChoice{ID: level, Label: label})
		if level == defaultLevel {
			capability.Default = level
		}
	}
	if len(capability.Choices) > 0 {
		capability.State = "supported"
		capability.CanStart = true
		capability.CanResume = true
	}
	return capability
}

func validateEffortCapability(capability *ChatEffortCapability) error {
	if capability == nil {
		return nil
	}
	if capability.State != "supported" && capability.State != "unsupported" && capability.State != "unknown" {
		return errors.New("invalid effort capability state")
	}
	if len(capability.Choices) > 32 || (capability.State == "supported") != (len(capability.Choices) > 0) {
		return errors.New("invalid effort capability choices")
	}
	seen := map[string]bool{}
	for _, choice := range capability.Choices {
		if choice.ID == "" || len(choice.ID) > 256 || !utf8.ValidString(choice.ID) || strings.ContainsFunc(choice.ID, unicode.IsControl) || choice.Label == "" || len(choice.Label) > 200 || len(choice.Description) > 2000 || seen[choice.ID] {
			return errors.New("invalid effort choice")
		}
		seen[choice.ID] = true
	}
	if capability.Default != "" && !seen[capability.Default] {
		return errors.New("invalid default effort")
	}
	return nil
}

func cloneEffortCapability(capability *ChatEffortCapability) *ChatEffortCapability {
	if capability == nil {
		return nil
	}
	out := *capability
	out.Choices = append([]ChatEffortChoice(nil), capability.Choices...)
	return &out
}

// EffortError is safe for the browser: no native flags or secret configuration.
type EffortError struct {
	Code    string `json:"code"`
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (e *EffortError) Error() string { return e.Message }
func effortError(code, message string) error {
	return &EffortError{Code: code, Field: "thinking_effort", Message: message}
}

func normalizeEffort(req ChatRequest) (ChatRequest, error) {
	if req.ThinkingEffort != nil {
		copy := *req.ThinkingEffort
		req.ThinkingEffort = &copy
		if err := copy.Validate(); err != nil {
			return req, effortError("invalid_choice", err.Error())
		}
	}
	if len(req.SessionEffortToken) > 32 || (req.SessionEffortToken != "" && req.ThinkingEffort == nil) {
		return req, effortError("stale_session_default", "Session effort token requires an explicit selection")
	}
	return req, nil
}

func mergeLegacyEffort(req ChatRequest, level string) (ChatRequest, error) {
	if level == "" {
		return req, nil
	}
	if req.ThinkingEffort != nil {
		if req.ThinkingEffort.Kind != "level" || req.ThinkingEffort.Value != level {
			return req, effortError("legacy_conflict", "Thinking effort conflicts with Settings extra args. Remove the extra-args effort setting.")
		}
	} else {
		req.ThinkingEffort = &store.ThinkingEffort{Kind: "level", Value: level}
		req.effortLegacy = true
	}
	return req, nil
}

func resolveRequestEffort(req ChatRequest) (ChatRequest, error) {
	if req.SessionEffortToken != "" {
		id := req.CatalogSessionID
		if id == "" {
			id = req.SessionID
		}
		canonical, err := effortSessionID(req.Runtime, id)
		if err != nil {
			return req, err
		}
		native, nativeErr := effortSessionID(req.Runtime, req.SessionID)
		if nativeErr != nil || native != canonical {
			return req, effortError("stale_session_default", "Saved effort does not belong to this session destination")
		}
		req.effortSessionID = canonical
	}
	if req.ThinkingEffort == nil || req.ThinkingEffort.Kind == "inherit" || req.effortLegacy {
		return req, nil
	}
	if req.Model == "" {
		return req, effortError("unavailable_evidence", "Choose a concrete model before setting thinking effort")
	}
	if req.Binary != "" || req.BaseURL != "" || req.AuthToken != "" || req.OSS || req.LocalProvider != "" {
		return req, effortError("unavailable_evidence", "Effort capabilities are unavailable for overridden runtime configuration")
	}
	list := chatModels.List(req.Runtime)
	if list.State != modelStateFresh {
		list = chatModels.Refresh(req.Runtime)
	}
	if list.State != modelStateFresh {
		return req, effortError("unavailable_evidence", "Model capabilities are unavailable. Refresh models or use runtime setting.")
	}
	for _, model := range list.Models {
		if model.ID != req.Model {
			continue
		}
		return validateModelEffort(req, model, list.Digest)
	}
	return req, effortError("unavailable_evidence", "Effort capabilities are unavailable for this model")
}

func validateModelEffort(req ChatRequest, model ChatModelOption, digest string) (ChatRequest, error) {
	capability := model.Effort
	if capability == nil || capability.State == "unknown" {
		return req, effortError("unavailable_evidence", "This model has no verified effort choices")
	}
	if capability.State != "supported" || (req.SessionID != "" && !capability.CanResume) || (req.SessionID == "" && !capability.CanStart) {
		return req, effortError("unsupported", "This model does not support effort for this turn")
	}
	for _, choice := range capability.Choices {
		if choice.ID == req.ThinkingEffort.Value {
			req.effortMapping = choice.MappingDigest
			req.effortDisplayLabel = choice.Label
			req.effortCatalogDigest = digest
			return req, nil
		}
	}
	return req, effortError("invalid_choice", "Selected effort is no longer supported. Choose an available level or use runtime setting.")
}

func requestedSettings(req ChatRequest) *store.TaskRequestedSettings {
	if req.ThinkingEffort == nil {
		return nil
	}
	source := req.effortSource
	if source == "" {
		source = "turn"
	}
	if req.effortLegacy {
		source = "legacy"
	}
	if req.SessionEffortToken != "" {
		source = "session"
	}
	label := req.effortDisplayLabel
	if req.effortLegacy {
		label = effortLabel(req.ThinkingEffort.Value)
	}
	if req.ThinkingEffort.Kind == "inherit" {
		label = "Default"
	}
	return &store.TaskRequestedSettings{EffortLabel: label, Model: req.Model, Effort: *req.ThinkingEffort, Source: source, CatalogDigest: req.effortCatalogDigest}
}
