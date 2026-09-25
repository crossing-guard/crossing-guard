package taskinput

import "fmt"

type Source string

const (
	SourcePicker Source = "picker"
	SourcePaste  Source = "paste"
	SourceDrop   Source = "drop"
)

func (source Source) Valid() bool {
	return source == SourcePicker || source == SourcePaste || source == SourceDrop
}

type Kind string

const (
	KindText  Kind = "text"
	KindImage Kind = "image"
)

type ScopeState string

const (
	ScopeOpen             ScopeState = "open"
	ScopeClaimed          ScopeState = "claimed"
	ScopeRecoveryRequired ScopeState = "recovery_required"
)

type Input struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Kind             Kind   `json:"kind"`
	MediaType        string `json:"media_type"`
	SourceBytes      int64  `json:"source_bytes"`
	PreparedBytes    int64  `json:"prepared_bytes"`
	PreviewAvailable bool   `json:"preview_available"`
	ExpiresAt        int64  `json:"expires_at"`
}

type Scope struct {
	ID        string     `json:"id"`
	State     ScopeState `json:"state"`
	Inputs    []Input    `json:"inputs"`
	ExpiresAt int64      `json:"expires_at"`
}

type ResolvedInput struct {
	Input
	Path string `json:"-"`
}

type Claim struct {
	ScopeID string
	TaskID  string
	Inputs  []ResolvedInput
}

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (err *Error) Error() string { return err.Message }

func inputError(code, format string, values ...any) error {
	return &Error{Code: code, Message: fmt.Sprintf(format, values...)}
}
