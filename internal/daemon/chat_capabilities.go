package daemon

import (
	"fmt"
	"net/http"
	"sort"

	"crossing-guard/harvest"
)

// ChatCapability is presentation-safe metadata for one registered fallback-chat
// adapter. It contains no argv, environment, credentials, HTML, or callbacks.
type ChatCapability struct {
	MessageDelivery SessionMessageCapability `json:"message_delivery"`
	Runtime         string                   `json:"runtime"`
	DisplayName     string                   `json:"display_name"`
	// InterfaceRevision names the provider CLI revision this product's chat
	// interface was verified against (natural-session plan, Slice A) — NOT
	// the currently installed version; installed-version evidence belongs to
	// the runtime-discovery owner. Empty when the adapter publishes none.
	InterfaceRevision string `json:"interface_revision,omitempty"`
	// GovernanceLane/GovernanceNote surface the installed governance mode for
	// runtimes whose hook attachment is version-gated (natural-session plan,
	// Slice C). The fact is read from the installed artifact by the
	// runtime-discovery owner (guardcli) and carried here verbatim — Settings
	// must show a collection-only fallback instead of implying enforcement.
	GovernanceLane string `json:"governance_lane,omitempty"`
	GovernanceNote string `json:"governance_note,omitempty"`
	CanStart       bool   `json:"can_start"`
	CanResume      bool   `json:"can_resume"`
	// ConcurrentTurns answers whether two turns may run on ONE of this
	// runtime's sessions at the same time. False on every shipped adapter
	// (2026-09-12): claude/codex/opencode each append one session file, and
	// two writers on it is the hazard the ownership gate exists for. The
	// managed host bounds a helper session's concurrency with it.
	ConcurrentTurns    bool                  `json:"concurrent_turns"`
	CanSignIn          bool                  `json:"can_sign_in"`
	VendorAuthDefault  bool                  `json:"vendor_auth_default"`
	SupportsBaseURL    bool                  `json:"supports_base_url,omitempty"`
	SupportsAuthToken  bool                  `json:"supports_auth_token,omitempty"`
	AcceptsCustomModel bool                  `json:"accepts_custom_model"`
	ModelHint          string                `json:"model_hint,omitempty"`
	Modes              []ChatMode            `json:"modes"`
	Models             []ChatModelOption     `json:"models"`
	Inputs             []ChatInputCapability `json:"inputs,omitempty"`
}

// SessionMessageCapability describes transport support, not runtime availability.
type SessionMessageCapability struct {
	Supported bool   `json:"supported"`
	Boundary  string `json:"boundary,omitempty"`
	Detail    string `json:"detail"`
}

func sourceMessageCapability(runtime string) SessionMessageCapability {
	driver := chatDrivers[runtime]
	provider, ok := driver.(chatCapabilityProvider)
	if !ok {
		return SessionMessageCapability{Detail: "Runtime does not publish session message delivery support."}
	}
	capability := provider.ChatCapability().MessageDelivery
	if capability.Supported {
		if _, ok := driver.(sessionMessageDeliverer); !ok {
			return SessionMessageCapability{Detail: "Runtime has no session message transport."}
		}
	}
	return capability
}

type ChatInputCapability struct {
	Kind             string   `json:"kind"`
	MediaTypes       []string `json:"media_types"`
	CanStart         bool     `json:"can_start"`
	CanResume        bool     `json:"can_resume"`
	ModelConditional bool     `json:"model_conditional,omitempty"`
	ModeConditional  bool     `json:"mode_conditional,omitempty"`
	Note             string   `json:"note,omitempty"`
}

type ChatMode struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	Risk        string `json:"risk"` // normal | elevated | dangerous
}

type ChatModelOption struct {
	ID                 string `json:"id"`
	Label              string `json:"label"`
	Description        string `json:"description,omitempty"`
	Custom             bool   `json:"custom,omitempty"`
	VendorAuthRequired *bool  `json:"vendor_auth_required,omitempty"`
}

// chatCapabilityProvider is an optional focused port. ChatDriver deliberately
// stays the frozen subprocess/event interface used by existing callers.
type chatCapabilityProvider interface {
	ChatCapability() ChatCapability
}

func chatCapabilities() ([]ChatCapability, error) {
	out := make([]ChatCapability, 0, len(chatDrivers))
	for runtimeName, driver := range chatDrivers {
		provider, ok := driver.(chatCapabilityProvider)
		if !ok {
			continue // dispatch-compatible, intentionally not advertised to the GUI
		}
		capability := provider.ChatCapability()
		if err := validateChatCapability(runtimeName, capability); err != nil {
			return nil, err
		}
		// The measured interface revision rides the harvest adapter (one
		// owner per fact); the chat capability publishes it verbatim.
		capability.InterfaceRevision = harvest.CLIRevision(runtimeName)
		capability.Modes = append([]ChatMode(nil), capability.Modes...)
		capability.Models = append([]ChatModelOption(nil), capability.Models...)
		capability.Inputs = append([]ChatInputCapability(nil), capability.Inputs...)
		for index := range capability.Inputs {
			capability.Inputs[index].MediaTypes = append([]string(nil), capability.Inputs[index].MediaTypes...)
		}
		out = append(out, capability)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Runtime < out[j].Runtime })
	return out, nil
}

func validateChatCapability(runtimeName string, c ChatCapability) error {
	if c.Runtime != runtimeName || c.DisplayName == "" {
		return fmt.Errorf("invalid chat capability for %q", runtimeName)
	}
	seenModes := map[string]bool{}
	defaultModes := 0
	for _, mode := range c.Modes {
		if mode.Label == "" || seenModes[mode.ID] {
			return fmt.Errorf("invalid or duplicate chat mode %q for %q", mode.ID, runtimeName)
		}
		seenModes[mode.ID] = true
		if mode.ID == "" {
			defaultModes++
		}
		if mode.Risk != "normal" && mode.Risk != "elevated" && mode.Risk != "dangerous" {
			return fmt.Errorf("invalid chat mode risk %q for %q", mode.Risk, runtimeName)
		}
	}
	if defaultModes != 1 {
		return fmt.Errorf("chat capability %q must declare exactly one default mode", runtimeName)
	}
	seenModels := map[string]bool{}
	for _, model := range c.Models {
		if model.Label == "" || seenModels[model.ID] {
			return fmt.Errorf("invalid or duplicate chat model %q for %q", model.ID, runtimeName)
		}
		seenModels[model.ID] = true
	}
	seenInputs := map[string]bool{}
	for _, input := range c.Inputs {
		if (input.Kind != "text" && input.Kind != "image") || seenInputs[input.Kind] || len(input.MediaTypes) == 0 {
			return fmt.Errorf("invalid or duplicate chat input kind %q for %q", input.Kind, runtimeName)
		}
		seenInputs[input.Kind] = true
		seenMedia := map[string]bool{}
		for _, mediaType := range input.MediaTypes {
			if mediaType == "" || seenMedia[mediaType] {
				return fmt.Errorf("invalid chat input media type for %q", runtimeName)
			}
			seenMedia[mediaType] = true
		}
	}
	return nil
}

func handleChatCapabilities(w http.ResponseWriter, _ *http.Request) {
	capabilities, err := chatCapabilities()
	if err != nil {
		http.Error(w, "chat capabilities are invalid", http.StatusInternalServerError)
		return
	}
	writeJSON(w, capabilities)
}

func boolPointer(value bool) *bool { return &value }
