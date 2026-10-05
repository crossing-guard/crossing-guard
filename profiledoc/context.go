package profiledoc

// ContextSelection is inert profile configuration; its encoding is versioned.
type ContextSelection struct {
	Kind     string `json:"kind"`
	Selector string `json:"selector,omitempty"`
	Required bool   `json:"required"`
	MaxBytes int    `json:"max_bytes"`
}
