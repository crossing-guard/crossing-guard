package recallmcp

import "encoding/json"

// OpenCode shares one server across its sessions and says nothing about which
// session calls (measured on 1.18.0): no id, and its process folder may belong
// to another of its sessions.
type openCodeIdentity struct{}

func init() { registerIdentity("opencode", openCodeIdentity{}) }

func (openCodeIdentity) SessionID(json.RawMessage) string { return "" }

func (openCodeIdentity) Place() (string, string) { return "", "" }
