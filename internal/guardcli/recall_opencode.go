package guardcli

// OpenCode registration of the recall server (recall-mcp-v1-plan §3.7): an
// mcp["crossing-guard"] local server in <config dir>/opencode.json. OpenCode
// merges opencode.json with opencode.jsonc, so the .jsonc is never touched; an
// opencode.json that is not plain JSON (comments) is reported, never rewritten.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"

	"crossing-guard/internal/recallmcp"
	"crossing-guard/internal/vendorconfig"
)

type openCodeRecallEntry struct {
	Type    string   `json:"type"`
	Command []string `json:"command"`
	Enabled bool     `json:"enabled"`
}

func (openCodeInstaller) RecallConfig(configDir string) string {
	return filepath.Join(configDir, "opencode.json")
}

func readOpenCodeConfig(path string) (vendorconfig.Snapshot, map[string]json.RawMessage, map[string]json.RawMessage, error) {
	snapshot, top, err := readJSONObject(path)
	if err != nil {
		return snapshot, nil, nil, fmt.Errorf("%w (a file with comments belongs in opencode.jsonc)", err)
	}
	servers := map[string]json.RawMessage{}
	if raw, ok := top["mcp"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &servers); err != nil {
			return snapshot, nil, nil, fmt.Errorf("%s: mcp is not an object", path)
		}
	}
	return snapshot, top, servers, nil
}

func openCodeEntryState(raw json.RawMessage, self string) string {
	var entry openCodeRecallEntry
	if json.Unmarshal(raw, &entry) != nil || len(entry.Command) == 0 {
		return RecallForeign
	}
	state := recallEntryState(entry.Command[0], entry.Command[1:], openCodeVendor, self)
	if state == RecallCurrent && (entry.Type != "local" || !entry.Enabled) {
		return RecallStale
	}
	return state
}

func (installer openCodeInstaller) RecallStatus(configDir, self string) (string, error) {
	_, _, servers, err := readOpenCodeConfig(installer.RecallConfig(configDir))
	if err != nil {
		return "", err
	}
	raw, ok := servers[recallmcp.ServerName]
	if !ok {
		return RecallAbsent, nil
	}
	return openCodeEntryState(raw, self), nil
}

func (installer openCodeInstaller) RegisterRecall(configDir, self string) error {
	path := installer.RecallConfig(configDir)
	snapshot, top, servers, err := readOpenCodeConfig(path)
	if err != nil {
		return err
	}
	entry, _ := json.Marshal(openCodeRecallEntry{Type: "local", Command: append([]string{self}, recallArgs(openCodeVendor)...), Enabled: true})
	if bytes.Equal(servers[recallmcp.ServerName], entry) {
		return nil
	}
	servers[recallmcp.ServerName] = entry
	encoded, _ := json.Marshal(servers)
	top["mcp"] = encoded
	return writeJSONObject(path, snapshot, top)
}

func (installer openCodeInstaller) UnregisterRecall(configDir, self string) (bool, error) {
	path := installer.RecallConfig(configDir)
	snapshot, top, servers, err := readOpenCodeConfig(path)
	if err != nil || !snapshot.Exists {
		return false, err
	}
	raw, ok := servers[recallmcp.ServerName]
	if !ok || openCodeEntryState(raw, self) == RecallForeign {
		return false, nil
	}
	delete(servers, recallmcp.ServerName)
	if len(servers) == 0 {
		delete(top, "mcp")
	} else {
		encoded, _ := json.Marshal(servers)
		top["mcp"] = encoded
	}
	return true, writeJSONObject(path, snapshot, top)
}

// A registrar that stops satisfying the port would silently drop out of RecallRuntimes.
var _ RecallRegistrar = openCodeInstaller{}
