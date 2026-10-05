package guardcli

// Claude Code registration of the recall server (recall-mcp-v1-plan §3.7):
// a user-scope mcpServers entry in ~/.claude.json, which loads in every
// project, and a server-level allow rule in the settings file the hooks
// already live in, so -p and helper turns can call the read-only tools.
// CLAUDE_CONFIG_DIR is not supported, as for hooks.

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"

	"crossing-guard/internal/recallmcp"
	"crossing-guard/internal/vendorconfig"
)

// claudeRecallRule allows every tool of our server (the server-level rule
// form; measured on 2.1.280).
var claudeRecallRule = "mcp__" + recallmcp.ServerName

type claudeRecallEntry struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// RecallConfig derives <home>/.claude.json from <home>/.claude/settings.json.
func (claudeInstaller) RecallConfig(settingsPath string) string {
	return filepath.Join(filepath.Dir(filepath.Dir(settingsPath)), ".claude.json")
}

func claudeRecallServers(top map[string]json.RawMessage, path string) (map[string]json.RawMessage, error) {
	servers := map[string]json.RawMessage{}
	if raw, ok := top["mcpServers"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &servers); err != nil {
			return nil, fmt.Errorf("%s: mcpServers is not an object", path)
		}
	}
	return servers, nil
}

func (installer claudeInstaller) RecallStatus(settingsPath, self string) (string, error) {
	path := installer.RecallConfig(settingsPath)
	_, top, err := readJSONObject(path)
	if err != nil {
		return "", err
	}
	servers, err := claudeRecallServers(top, path)
	if err != nil {
		return "", err
	}
	raw, ok := servers[recallmcp.ServerName]
	if !ok {
		return RecallAbsent, nil
	}
	var entry claudeRecallEntry
	if json.Unmarshal(raw, &entry) != nil {
		return RecallForeign, nil
	}
	state := recallEntryState(entry.Command, entry.Args, claudeVendor, self)
	if state == RecallCurrent {
		allowed, err := claudeRecallRuleState(settingsPath)
		if err != nil {
			return "", err
		}
		if !allowed {
			return RecallStale, nil
		}
	}
	return state, nil
}

func (installer claudeInstaller) RegisterRecall(settingsPath, self string) error {
	path := installer.RecallConfig(settingsPath)
	snapshot, top, err := readJSONObject(path)
	if err != nil {
		return err
	}
	servers, err := claudeRecallServers(top, path)
	if err != nil {
		return err
	}
	want := claudeRecallEntry{Type: "stdio", Command: self, Args: recallArgs(claudeVendor)}
	var have claudeRecallEntry
	current := json.Unmarshal(servers[recallmcp.ServerName], &have) == nil &&
		have.Type == want.Type && have.Command == want.Command && slices.Equal(have.Args, want.Args)
	if !current {
		entry, _ := json.Marshal(want)
		servers[recallmcp.ServerName] = entry
		encoded, _ := json.Marshal(servers)
		top["mcpServers"] = encoded
		// A concurrent rewrite by Claude itself fails the compare-and-swap; it is
		// reported, never retried or forced (the next init or boot repairs it).
		if err := writeJSONObject(path, snapshot, top); err != nil {
			return err
		}
	}
	return setClaudeRecallRule(settingsPath, true)
}

func (installer claudeInstaller) UnregisterRecall(settingsPath, self string) (bool, error) {
	path := installer.RecallConfig(settingsPath)
	snapshot, top, err := readJSONObject(path)
	if err != nil {
		return false, err
	}
	servers, err := claudeRecallServers(top, path)
	if err != nil {
		return false, err
	}
	removed := false
	if raw, ok := servers[recallmcp.ServerName]; ok {
		var entry claudeRecallEntry
		if json.Unmarshal(raw, &entry) == nil && recallEntryState(entry.Command, entry.Args, claudeVendor, self) != RecallForeign {
			delete(servers, recallmcp.ServerName)
			if len(servers) == 0 {
				delete(top, "mcpServers")
			} else {
				encoded, _ := json.Marshal(servers)
				top["mcpServers"] = encoded
			}
			if err := writeJSONObject(path, snapshot, top); err != nil {
				return false, err
			}
			removed = true
		}
	}
	if err := setClaudeRecallRule(settingsPath, false); err != nil {
		return removed, err
	}
	return removed, nil
}

// claudeRecallRuleState says whether the allow rule is in the settings file.
func claudeRecallRuleState(settingsPath string) (bool, error) {
	snapshot, err := vendorconfig.Read(settingsPath)
	if err != nil || !snapshot.Exists {
		return false, err
	}
	var settings struct {
		Permissions struct {
			Allow []string `json:"allow"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(snapshot.Data, &settings); err != nil {
		return false, fmt.Errorf("%s is not valid JSON (%w)", settingsPath, err)
	}
	for _, rule := range settings.Permissions.Allow {
		if rule == claudeRecallRule {
			return true, nil
		}
	}
	return false, nil
}

// setClaudeRecallRule adds or removes the allow rule, leaving every other
// setting and rule as it was. It uses the hook installer's merge shape.
func setClaudeRecallRule(settingsPath string, present bool) error {
	snapshot, err := vendorconfig.Read(settingsPath)
	if err != nil {
		return err
	}
	settings := map[string]any{}
	if snapshot.Exists {
		if err := json.Unmarshal(snapshot.Data, &settings); err != nil {
			return fmt.Errorf("%s is not valid JSON (%w) — fix it first; refusing to overwrite it", settingsPath, err)
		}
	} else if !present {
		return nil
	}
	permissions, _ := settings["permissions"].(map[string]any)
	if permissions == nil {
		if _, exists := settings["permissions"]; exists {
			return fmt.Errorf("%s: permissions is not an object", settingsPath)
		}
		if !present {
			return nil
		}
		permissions = map[string]any{}
	}
	var allow []any
	if raw, exists := permissions["allow"]; exists {
		list, ok := raw.([]any)
		if !ok {
			return fmt.Errorf("%s: permissions.allow is not a list", settingsPath)
		}
		allow = list
	}
	kept, had := []any{}, false
	for _, rule := range allow {
		if rule == claudeRecallRule {
			had = true
			continue
		}
		kept = append(kept, rule)
	}
	if had == present {
		return nil
	}
	if present {
		kept = append(kept, claudeRecallRule)
	}
	if len(kept) == 0 && len(permissions) == 1 {
		delete(settings, "permissions")
	} else {
		if len(kept) == 0 {
			delete(permissions, "allow")
		} else {
			permissions["allow"] = kept
		}
		settings["permissions"] = permissions
	}
	b, _ := json.MarshalIndent(settings, "", "  ")
	_, err = vendorconfig.Replace(settingsPath, snapshot, append(b, '\n'))
	return err
}

// A registrar that stops satisfying the port would silently drop out of RecallRuntimes.
var _ RecallRegistrar = claudeInstaller{}
