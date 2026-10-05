package guardcli

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"crossing-guard/internal/recallmcp"
	"crossing-guard/internal/vendorconfig"
	"crossing-guard/internal/vendorpaths"
)

// Google shares this MCP file across its CLI and desktop apps. Registration is
// separately consented by the existing recall flow; it never alters permissions.
func (antigravityInstaller) RecallConfig(settings string) string {
	return filepath.Join(filepath.Dir(settings), filepath.FromSlash(vendorpaths.AntigravityMCPFromHooksDir))
}

type antigravityRecallEntry struct {
	Command  string   `json:"command"`
	Args     []string `json:"args"`
	Disabled bool     `json:"disabled,omitempty"`
}

func readAntigravityRecall(path string) (vendorconfig.Snapshot, map[string]json.RawMessage, map[string]json.RawMessage, error) {
	snapshot, top, err := readJSONObject(path)
	if err != nil {
		return snapshot, nil, nil, err
	}
	if top == nil {
		return snapshot, nil, nil, fmt.Errorf("MCP config must be an object")
	}
	servers := map[string]json.RawMessage{}
	if raw, ok := top["mcpServers"]; ok {
		if json.Unmarshal(raw, &servers) != nil || servers == nil {
			return snapshot, nil, nil, fmt.Errorf("mcpServers must be an object")
		}
	}
	return snapshot, top, servers, nil
}
func antigravityRecallState(raw json.RawMessage, self string) string {
	var entry antigravityRecallEntry
	if json.Unmarshal(raw, &entry) != nil {
		return RecallForeign
	}
	state := recallEntryState(entry.Command, entry.Args, antigravityVendor, self)
	if state == RecallCurrent && entry.Disabled {
		return RecallStale
	}
	return state
}
func (installer antigravityInstaller) RecallStatus(settings, self string) (string, error) {
	_, _, servers, err := readAntigravityRecall(installer.RecallConfig(settings))
	if err != nil {
		return "", err
	}
	raw, ok := servers[recallmcp.ServerName]
	if !ok {
		return RecallAbsent, nil
	}
	return antigravityRecallState(raw, self), nil
}
func (installer antigravityInstaller) RegisterRecall(settings, self string) error {
	path := installer.RecallConfig(settings)
	snapshot, top, servers, err := readAntigravityRecall(path)
	if err != nil {
		return err
	}
	if raw, ok := servers[recallmcp.ServerName]; ok {
		state := antigravityRecallState(raw, self)
		if state == RecallCurrent {
			return nil
		}
		if state == RecallForeign {
			return fmt.Errorf("foreign Crossing Guard MCP entry; refusing to overwrite")
		}
	}
	servers[recallmcp.ServerName], _ = json.Marshal(antigravityRecallEntry{Command: self, Args: recallArgs(antigravityVendor)})
	top["mcpServers"], _ = json.Marshal(servers)
	return writeJSONObject(path, snapshot, top)
}
func (installer antigravityInstaller) UnregisterRecall(settings, self string) (bool, error) {
	path := installer.RecallConfig(settings)
	snapshot, top, servers, err := readAntigravityRecall(path)
	if err != nil {
		return false, err
	}
	raw, ok := servers[recallmcp.ServerName]
	if !ok || antigravityRecallState(raw, self) == RecallForeign {
		return false, nil
	}
	delete(servers, recallmcp.ServerName)
	if len(servers) == 0 {
		delete(top, "mcpServers")
	} else {
		top["mcpServers"], _ = json.Marshal(servers)
	}
	return true, writeJSONObject(path, snapshot, top)
}

var _ RecallRegistrar = antigravityInstaller{}
