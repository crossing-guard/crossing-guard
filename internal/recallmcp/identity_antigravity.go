package recallmcp

import "encoding/json"

// Native CLI 1.2.14 has no measured per-call MCP session identity. Recall can
// use the process folder; identified-session operations must remain unavailable.
type antigravityIdentity struct{}

func init()                                                  { registerIdentity("antigravity", antigravityIdentity{}) }
func (antigravityIdentity) SessionID(json.RawMessage) string { return "" }
func (antigravityIdentity) Place() (string, string)          { return "", "" }
