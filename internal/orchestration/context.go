package orchestration

import "context"

// ContextSelection is inert profile configuration; its encoding is versioned.
type ContextSelection struct {
	Kind     string `json:"kind"`
	Selector string `json:"selector,omitempty"`
	Required bool   `json:"required"`
	MaxBytes int    `json:"max_bytes"`
}

type ContextRequest struct {
	Source     ManagedSource
	GroupID    string
	SessionID  string
	Selections []ContextSelection
}

type ContextCoverage struct {
	Kind   string `json:"kind"`
	State  string `json:"state"` // supplied | empty | unavailable | truncated
	Detail string `json:"detail,omitempty"`
	ReadAt int64  `json:"read_at"`
}

type ContextEnvelope struct {
	Source   ManagedSource
	Items    []PromptContext
	Coverage []ContextCoverage
	// TranscriptSeq is the highest transcript sequence session.messages
	// supplied, recorded on the run so a retry pins the same cutoff.
	TranscriptSeq int64
}

// ContextReader keeps resource access outside the orchestration consumer.
type ContextReader interface {
	ReadContext(context.Context, ContextRequest) (ContextEnvelope, error)
}
