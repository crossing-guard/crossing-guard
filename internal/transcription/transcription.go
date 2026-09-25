// Package transcription owns the speech-to-text port, the backend registry, and
// the dictation lifecycle behind the console composer's microphone control. It
// knows nothing about HTTP, the daemon, or any provider: adapters register a
// factory here by name and the daemon composition root selects one from
// configuration.
package transcription

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// EndState is the one terminal outcome of a dictation. Every dictation ends in
// exactly one of these, and every one of them deletes the clip.
type EndState string

const (
	EndFinal         EndState = "final"
	EndNoSpeech      EndState = "no_speech"
	EndLowConfidence EndState = "low_confidence"
	EndTimeout       EndState = "timeout"
	EndEngineError   EndState = "engine_error"
	EndProviderError EndState = "provider_error"
	EndFrameError    EndState = "frame_error"
	EndCancelled     EndState = "cancelled"
	EndAbandoned     EndState = "abandoned"
)

// Capabilities are the per-backend facts the service and the browser branch on.
// Nothing outside an adapter may branch on a backend name. Facts are display
// rows for Settings (for example the model file and the sandbox contract) that
// only the adapter can state.
type Capabilities struct {
	Backend             string `json:"backend"`
	DisplayName         string `json:"display_name"`
	EmitsPartials       bool   `json:"emits_partials"`
	ReportsConfidence   bool   `json:"reports_confidence"`
	RequiresDisclosure  bool   `json:"requires_disclosure"`
	PreferredSampleRate int    `json:"preferred_sample_rate"`
	Facts               []Fact `json:"facts,omitempty"`
}

// Fact is one label/value row an adapter publishes for Settings.
type Fact struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Note  string `json:"note,omitempty"`
}

// Hints are the only context that ever accompanies audio to a backend.
type Hints struct {
	ProjectName string
	BranchName  string
	Keywords    []string
}

// Empty reports whether no hint is set.
func (h Hints) Empty() bool {
	return h.ProjectName == "" && h.BranchName == "" && len(h.Keywords) == 0
}

// DecodeRequest asks a backend to transcribe one WAV file. Window is true for a
// provisional decode of a trailing window and false for the authoritative final
// pass over the whole clip. WorkDir is a private directory the backend may
// write into; it is deleted with the dictation.
type DecodeRequest struct {
	WAVPath      string
	WorkDir      string
	Hints        Hints
	Window       bool
	CaptureLimit int64
}

// Word is one recognized word with its time span inside the decoded audio.
type Word struct {
	Text    string
	StartMS int64
	EndMS   int64
}

// Decoded is a backend's answer for one DecodeRequest. HasConfidence is false
// for backends that do not report token probabilities.
type Decoded struct {
	Text          string
	Words         []Word
	Confidence    float64
	HasConfidence bool
}

// PartialFunc receives progressive text from a backend that streams its final
// pass (for example an after-release cloud upload). It may be nil.
type PartialFunc func(text string)

// Backend is the port every speech-to-text adapter implements.
type Backend interface {
	Name() string
	Capabilities() Capabilities
	Decode(ctx context.Context, request DecodeRequest, onPartial PartialFunc) (Decoded, error)
}

// Factory builds a backend from the validated transcription configuration. It
// decodes its own section strictly and performs its readiness checks; an error
// here is reported as a Settings problem, never as a file-load failure.
type Factory func(config Config) (Backend, error)

// Error is a typed dictation failure: the end state the service records, a
// message safe to show, and a detail that may reach the daemon log only.
type Error struct {
	State   EndState
	Message string
	Detail  string
}

func (err *Error) Error() string { return err.Message }

// Fail builds a typed failure with no log detail.
func Fail(state EndState, format string, values ...any) error {
	return &Error{State: state, Message: fmt.Sprintf(format, values...)}
}

// FailWithDetail builds a typed failure carrying a log-only detail.
func FailWithDetail(state EndState, message, detail string) error {
	return &Error{State: state, Message: message, Detail: detail}
}

// StateOf returns the end state carried by err, or EndEngineError for an
// untyped error and EndTimeout for a context deadline.
func StateOf(err error) EndState {
	var typed *Error
	if errors.As(err, &typed) {
		return typed.State
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return EndTimeout
	}
	if errors.Is(err, context.Canceled) {
		return EndCancelled
	}
	return EndEngineError
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// Register adds a backend factory under its name. Adapters call it from init;
// a duplicate name is a programming error, exactly as in infer.Register.
func Register(name string, factory Factory) {
	if name == "" || factory == nil {
		panic("transcription backend registration requires a name and a factory")
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[name]; exists {
		panic("duplicate transcription backend registration: " + name)
	}
	registry[name] = factory
}

// New builds the backend selected by config.Backend.
func New(config Config) (Backend, error) {
	registryMu.RLock()
	factory, ok := registry[config.Backend]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("backend %q is not registered; available: %v", config.Backend, Names())
	}
	return factory(config)
}

// Names lists registered backends in a stable order for configuration errors
// and the Settings page.
func Names() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
