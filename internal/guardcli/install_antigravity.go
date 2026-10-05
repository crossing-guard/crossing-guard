package guardcli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"

	"crossing-guard/internal/vendorconfig"
	"crossing-guard/internal/vendorpaths"
)

const antigravityVendor = "antigravity"
const antigravityHookGroup = "crossing-guard"

var antigravityHookEvents = []string{"PreToolUse", "PostToolUse", "Stop"}

type antigravityInstaller struct{}

func init()                                      { registerHookInstaller(antigravityInstaller{}) }
func (antigravityInstaller) Name() string        { return antigravityVendor }
func (antigravityInstaller) DisplayName() string { return "Antigravity" }
func (antigravityInstaller) SelectFlag() string  { return "--antigravity-hooks" }
func (antigravityInstaller) DefaultConfig() string {
	return homeJoin(filepath.FromSlash(vendorpaths.AntigravityHooksRelative))
}
func (antigravityInstaller) IsDefault() bool { return false }
func (installer antigravityInstaller) ResolveConfig() string {
	path := installer.DefaultConfig()
	if installer.RuntimePresent() || fileExists(path) {
		return path
	}
	return ""
}
func (antigravityInstaller) RuntimePresent() bool {
	if _, err := exec.LookPath("agy"); err == nil {
		return true
	}
	info, err := os.Stat(homeJoin(".local", "bin", "agy"))
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}
func (antigravityInstaller) ManualStep() string {
	return "Shared Antigravity hooks, filtered to CLI transcript paths; candidate: verify a harmless native tool action and denial; transcript history, live presence and console chat are not supported"
}

// The named group is wholly ours or untouched. An existing foreign command under
// this name is a collision, not permission to replace another integration.
func readAntigravityHooks(path string) (vendorconfig.Snapshot, map[string]json.RawMessage, error) {
	snapshot, top, err := readJSONObject(path)
	if err != nil {
		return snapshot, nil, err
	}
	if top == nil || (snapshot.Exists && len(strings.TrimSpace(string(snapshot.Data))) == 0) {
		return snapshot, nil, fmt.Errorf("%s must contain a JSON object", path)
	}
	hooks := top
	if raw, exists := hooks[antigravityHookGroup]; exists {
		if _, err := antigravityOwnedCommands(raw); err != nil {
			return snapshot, nil, err
		}
	}
	return snapshot, hooks, nil
}

func antigravityOwnedCommands(raw json.RawMessage) ([]string, error) {
	var group map[string]json.RawMessage
	if json.Unmarshal(raw, &group) != nil || len(group) == 0 {
		return nil, fmt.Errorf("invalid or foreign %s hook group", antigravityHookGroup)
	}
	commands := []string{}
	for phase, rawEntries := range group {
		if phase == "enabled" {
			var enabled bool
			if json.Unmarshal(rawEntries, &enabled) != nil || string(rawEntries) == "null" {
				return nil, fmt.Errorf("invalid hook enabled field")
			}
			continue
		}
		if phase != "PreToolUse" && phase != "PostToolUse" && phase != "Stop" {
			return nil, fmt.Errorf("unknown phase in owned hook group: %s", phase)
		}
		var entries []map[string]json.RawMessage
		if json.Unmarshal(rawEntries, &entries) != nil || entries == nil {
			return nil, fmt.Errorf("invalid hook entries for %s", phase)
		}
		for _, entry := range entries {
			handlers := []map[string]json.RawMessage{entry}
			if phase != "Stop" {
				if json.Unmarshal(entry["hooks"], &handlers) != nil || len(handlers) == 0 {
					return nil, fmt.Errorf("invalid tool hook handlers")
				}
			}
			for _, handler := range handlers {
				var command string
				if json.Unmarshal(handler["command"], &command) != nil || ourHookBinaryFromCommand(command) == "" || !strings.Contains(command, "--runtime "+antigravityVendor) {
					return nil, fmt.Errorf("foreign command in %s hook group; refusing to replace it", antigravityHookGroup)
				}
				commands = append(commands, command)
			}
		}
	}
	if len(commands) == 0 {
		return nil, fmt.Errorf("hook group contains no owned command")
	}
	return commands, nil
}

func antigravityHooks(executable string) json.RawMessage {
	group := map[string]any{}
	for _, event := range antigravityHookEvents {
		handler := map[string]any{"type": "command", "command": hookCommand(executable, antigravityVendor) + " --event " + event}
		if event == "Stop" {
			group[event] = []any{handler}
		} else {
			group[event] = []any{map[string]any{"matcher": "*", "hooks": []any{handler}}}
		}
	}
	body, _ := json.Marshal(group)
	return body
}

func antigravityJSONEqual(a, b json.RawMessage) bool {
	var first, second any
	return json.Unmarshal(a, &first) == nil && json.Unmarshal(b, &second) == nil && reflect.DeepEqual(first, second)
}

func (antigravityInstaller) HookBinary(path string) string {
	_, hooks, err := readAntigravityHooks(path)
	if err != nil {
		return ""
	}
	commands, err := antigravityOwnedCommands(hooks[antigravityHookGroup])
	if err != nil {
		return ""
	}
	for _, command := range commands {
		if strings.Contains(command, "--event PreToolUse") {
			return ourHookBinaryFromCommand(command)
		}
	}
	return ourHookBinaryFromCommand(commands[0])
}
func (antigravityInstaller) IsCurrent(path, executable string) bool {
	_, hooks, err := readAntigravityHooks(path)
	return err == nil && antigravityJSONEqual(hooks[antigravityHookGroup], antigravityHooks(executable))
}
func (installer antigravityInstaller) HookPhaseStatus(path, executable string) map[string]bool {
	result := map[string]bool{}
	_, hooks, err := readAntigravityHooks(path)
	var actual, expected map[string]json.RawMessage
	valid := err == nil && json.Unmarshal(hooks[antigravityHookGroup], &actual) == nil
	_ = json.Unmarshal(antigravityHooks(executable), &expected)
	var enabled bool
	if raw, exists := actual["enabled"]; exists && (json.Unmarshal(raw, &enabled) != nil || !enabled) {
		valid = false
	}
	for _, phase := range antigravityHookEvents {
		result[phase] = valid && antigravityJSONEqual(actual[phase], expected[phase])
	}
	return result
}
func (antigravityInstaller) Install(path, executable string) error {
	snapshot, hooks, err := readAntigravityHooks(path)
	if err != nil {
		return err
	}
	want := antigravityHooks(executable)
	if antigravityJSONEqual(hooks[antigravityHookGroup], want) {
		return nil
	}
	hooks[antigravityHookGroup] = want
	if err := writeJSONObject(path, snapshot, hooks); err != nil {
		return err
	}
	fmt.Println("installed Antigravity CLI hooks →", path)
	return nil
}
func (antigravityInstaller) Uninstall(path, _ string) (bool, error) {
	snapshot, hooks, err := readAntigravityHooks(path)
	if err != nil {
		return false, err
	}
	if _, exists := hooks[antigravityHookGroup]; !exists {
		return false, nil
	}
	delete(hooks, antigravityHookGroup)
	return true, writeJSONObject(path, snapshot, hooks)
}
