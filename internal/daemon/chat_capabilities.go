package daemon

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"crossing-guard/harvest"
	"crossing-guard/internal/taskinput"
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
	CatalogRequired  bool     `json:"catalog_required,omitempty"`
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
	Effort             *ChatEffortCapability `json:"thinking_effort,omitempty"`
	ID                 string                `json:"id"`
	Label              string                `json:"label"`
	Description        string                `json:"description,omitempty"`
	Custom             bool                  `json:"custom,omitempty"`
	VendorAuthRequired *bool                 `json:"vendor_auth_required,omitempty"`
	// The fields below are facts a runtime reports about a model it can run
	// (runtime-model-catalog-and-usage design §4.2). An adapter's declared
	// entries (Source "") never carry them; discovered entries have Source
	// "runtime". Every fact is optional: absent means unknown.
	Source       string           `json:"source,omitempty"`
	Group        string           `json:"group,omitempty"` // opaque grouping key
	GroupLabel   string           `json:"group_label,omitempty"`
	Limits       *ChatModelLimits `json:"limits,omitempty"`
	Inputs       []string         `json:"inputs,omitempty"` // task input kinds the adapter would admit for this id
	Price        *ChatModelPrice  `json:"price,omitempty"`
	PricePartial bool             `json:"price_partial,omitempty"` // the runtime states prices this vocabulary cannot hold
}

const chatModelSourceRuntime = "runtime"

// ChatModelLimits are token limits a runtime states for a model.
type ChatModelLimits struct {
	ContextTokens *int64 `json:"context_tokens,omitempty"`
	InputTokens   *int64 `json:"input_tokens,omitempty"`
	OutputTokens  *int64 `json:"output_tokens,omitempty"`
}

// ChatModelPrice is the rate a runtime states per PerTokens tokens, in an
// opaque unit code. It is information for choosing a model, not a bill.
type ChatModelPrice struct {
	Unit      string          `json:"unit"`
	PerTokens int64           `json:"per_tokens"`
	Rates     []ChatModelRate `json:"rates"`
}

// ChatModelRate prices one token class: a well-known class id, or a
// runtime-specific id that then needs a Label.
type ChatModelRate struct {
	Class              string  `json:"class"`
	Label              string  `json:"label,omitempty"`
	Amount             float64 `json:"amount"`
	AboveContextTokens *int64  `json:"above_context_tokens,omitempty"`
}

// chatTokenClasses are the well-known, disjoint token classes (design §4.1).
var chatTokenClasses = map[string]bool{
	"input": true, "cache-read": true, "cache-write": true, "output": true, "reasoning": true,
}

// chatModelBounds are validation bounds; daemon.json `models` supplies them.
type chatModelBounds struct {
	MaxIDBytes, MaxLabelBytes int
}

var defaultChatModelBounds = chatModelBounds{MaxIDBytes: 256, MaxLabelBytes: 200}

// chatCapabilityProvider is an optional focused port. ChatDriver deliberately
// stays the frozen subprocess/event interface used by existing callers.
type chatCapabilityProvider interface {
	ChatCapability() ChatCapability
}

// chatRouteLocality is an optional focused port: the adapter states whether a
// request would send its inference to a model server on this machine, and on
// what basis (managed-turn-profile-limits plan §4.1). It must be pure — no I/O,
// never blocking — because admission and the agents projection call it
// inline. A runtime without it, and a request it does not claim, is not local:
// unknown stays unsupported. "Local" is the endpoint class the runtime is
// configured to use; where that server forwards work, and what the harness
// sends besides inference, are outside it.
type chatRouteLocality interface {
	LocalRoute(req ChatRequest) (local bool, basis string)
}

// chatRouteIsLocal answers the locality question for one request through its
// runtime's adapter.
func chatRouteIsLocal(req ChatRequest) (bool, string) {
	adapter, ok := chatDrivers[req.Runtime].(chatRouteLocality)
	if !ok {
		return false, ""
	}
	return adapter.LocalRoute(req)
}

// chatRuntimeOffersLocalModel reports whether the runtime declares a model
// option its adapter treats as local — whether "choose a local route" is advice
// the owner can follow on this runtime.
func chatRuntimeOffersLocalModel(runtime string) bool {
	provider, ok := chatDrivers[runtime].(chatCapabilityProvider)
	if !ok {
		return false
	}
	for _, option := range provider.ChatCapability().Models {
		if option.Custom {
			continue
		}
		if local, _ := chatRouteIsLocal(ChatRequest{Runtime: runtime, Model: option.ID}); local {
			return true
		}
	}
	return false
}

// chatModelLabel names a model the way its runtime declares it (the declared
// option's label), falling back to the raw id for a custom model — generic
// copy never hard-codes one runtime's word for its default.
func chatModelLabel(runtime, model string) string {
	if provider, ok := chatDrivers[runtime].(chatCapabilityProvider); ok {
		for _, option := range provider.ChatCapability().Models {
			if option.ID == model && !option.Custom && option.Label != "" {
				return option.Label
			}
		}
	}
	if model == "" {
		return "default model"
	}
	return model
}

// chatRuntimeLabel is the runtime's declared display name, or its id.
func chatRuntimeLabel(runtime string) string {
	if provider, ok := chatDrivers[runtime].(chatCapabilityProvider); ok {
		if name := provider.ChatCapability().DisplayName; name != "" {
			return name
		}
	}
	return runtime
}

func chatCapabilities() ([]ChatCapability, error) {
	out := make([]ChatCapability, 0, len(chatDrivers))
	for runtimeName, driver := range chatDrivers {
		provider, ok := driver.(chatCapabilityProvider)
		if !ok {
			continue // dispatch-compatible, intentionally not advertised to the GUI
		}
		capability := provider.ChatCapability()
		if _, discovers := driver.(chatModelDiscoverer); !discovers {
			for _, input := range capability.Inputs {
				if input.CatalogRequired {
					return nil, fmt.Errorf("catalog-required input has no discovery provider for %q", runtimeName)
				}
			}
		}
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
		if seenModels[model.ID] {
			return fmt.Errorf("duplicate chat model %q for %q", model.ID, runtimeName)
		}
		if err := validateChatModelOption(model, false, defaultChatModelBounds); err != nil {
			return fmt.Errorf("invalid chat model %q for %q: %w", model.ID, runtimeName, err)
		}
		seenModels[model.ID] = true
	}
	seenInputs := map[string]bool{}
	for _, input := range c.Inputs {
		if (input.Kind != "text" && input.Kind != "image") || seenInputs[input.Kind] || len(input.MediaTypes) == 0 {
			return fmt.Errorf("invalid or duplicate chat input kind %q for %q", input.Kind, runtimeName)
		}
		if input.CatalogRequired && !input.ModelConditional {
			return fmt.Errorf("catalog-required input must be model-conditional for %q", runtimeName)
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

// validateChatModelOption is the one rule set for declared and discovered
// model entries. Declared entries may not carry runtime-reported facts;
// discovered ones must say they came from the runtime.
func validateChatModelOption(model ChatModelOption, discovered bool, bounds chatModelBounds) error {
	if err := validateEffortCapability(model.Effort); err != nil {
		return err
	}
	switch {
	case model.Label == "":
		return errors.New("label is required")
	case len(model.ID) > bounds.MaxIDBytes || len(model.Label) > bounds.MaxLabelBytes ||
		len(model.GroupLabel) > bounds.MaxLabelBytes || len(model.Group) > bounds.MaxIDBytes:
		return errors.New("id, label or group exceeds its bound")
	case !utf8.ValidString(model.ID) || strings.ContainsFunc(model.ID, unicode.IsControl):
		return errors.New("id is not printable UTF-8")
	}
	reported := model.Group != "" || model.GroupLabel != "" || model.Limits != nil ||
		len(model.Inputs) > 0 || model.Price != nil || model.PricePartial
	if !discovered {
		if reported || model.Source != "" {
			return errors.New("declared entries cannot carry runtime-reported facts")
		}
		return nil
	}
	if model.Source != chatModelSourceRuntime || model.ID == "" || model.Custom || model.VendorAuthRequired != nil {
		return errors.New("a discovered entry needs a concrete id and source runtime")
	}
	if limits := model.Limits; limits != nil {
		for _, value := range []*int64{limits.ContextTokens, limits.InputTokens, limits.OutputTokens} {
			if value != nil && *value <= 0 {
				return errors.New("a stated limit must be positive")
			}
		}
	}
	seenInputs := map[string]bool{}
	for _, kind := range model.Inputs {
		if (taskinput.Kind(kind) != taskinput.KindText && taskinput.Kind(kind) != taskinput.KindImage) || seenInputs[kind] {
			return fmt.Errorf("input kind %q has no transport or repeats", kind)
		}
		seenInputs[kind] = true
	}
	if price := model.Price; price != nil {
		if price.Unit == "" || len(price.Unit) > 16 || price.PerTokens <= 0 || len(price.Rates) == 0 {
			return errors.New("a price needs a unit, a positive per_tokens and at least one rate")
		}
		seenRates := map[string]bool{}
		for _, rate := range price.Rates {
			key := rate.Class
			if rate.AboveContextTokens != nil {
				key += "@" + strconv.FormatInt(*rate.AboveContextTokens, 10)
			}
			if rate.Class == "" || seenRates[key] || rate.Amount < 0 || math.IsNaN(rate.Amount) ||
				math.IsInf(rate.Amount, 0) || (!chatTokenClasses[rate.Class] && rate.Label == "") {
				return fmt.Errorf("rate for class %q is invalid or repeats", rate.Class)
			}
			seenRates[key] = true
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
