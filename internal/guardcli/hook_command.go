package guardcli

import (
	"debug/buildinfo"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"crossing-guard/internal/vendorconfig"
)

const crossingGuardCommandModule = "crossing-guard/cmd/crossing-guard"

// hookCommand is the one installed command shape. Runtime attribution is carried
// in the command because the incoming vendor hook payload does not identify it.
func hookCommand(executable, vendor string) string {
	return fmt.Sprintf("%q hook --runtime %s", executable, vendor)
}

func hookEventStatus(path, event, want string, matcherOK func(map[string]any) bool) (owned, current int, ok bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, false
	}
	var config map[string]any
	if json.Unmarshal(raw, &config) != nil {
		return 0, 0, false
	}
	owned, current = hookEventCounts(config, event, want, matcherOK)
	return owned, current, true
}

func ourHookBinary(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var config struct {
		Hooks map[string][]map[string]any `json:"hooks"`
	}
	if json.Unmarshal(raw, &config) != nil {
		return ""
	}
	for _, entry := range config.Hooks["PreToolUse"] {
		inner, _ := entry["hooks"].([]any)
		for _, hook := range inner {
			item, _ := hook.(map[string]any)
			command, _ := item["command"].(string)
			if binary := ourHookBinaryFromCommand(command); binary != "" {
				return binary
			}
		}
	}
	return ""
}

// ourHookBinaryFromCommand accepts current and historical hook spellings. It
// anchors on the verb so later flags cannot make an installed hook disappear.
func ourHookBinaryFromCommand(command string) string {
	return ourHookBinaryFromCommandWithBuildPath(command, hookExecutableBuildPath)
}

func ourHookBinaryFromCommandWithBuildPath(command string, buildPath func(string) string) string {
	fields := strings.Fields(command)
	for index, field := range fields {
		if field != "hook" || index == 0 {
			continue
		}
		path := strings.Trim(strings.Join(fields[:index], " "), `"`)
		if name := filepath.Base(path); name == "crossing-guard" || name == "cg" { // name-lint: historical
			return path
		}
		// Developer/integration builds can have an arbitrary filename. A prefix
		// match would claim foreign commands such as crossing-guard-helper; use the
		// Go command's module identity instead. This is local config ownership
		// evidence only, never executable trust or a policy authorization boundary.
		if buildPath(path) == crossingGuardCommandModule {
			return path
		}
	}
	return ""
}

func hookExecutableBuildPath(path string) string {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return ""
	}
	return info.Path
}

func isOurHookCommand(command string) bool { return ourHookBinaryFromCommand(command) != "" }

func validateHookEventShapes(config map[string]any, events ...string) error {
	rawHooks, present := config["hooks"]
	if !present {
		return nil
	}
	hooks, ok := rawHooks.(map[string]any)
	if !ok {
		return fmt.Errorf("hooks must be a JSON object; refusing to replace it")
	}
	for _, event := range events {
		rawEntries, present := hooks[event]
		if !present {
			continue
		}
		if _, ok := rawEntries.([]any); !ok {
			return fmt.Errorf("hooks.%s must be a JSON array; refusing to replace it", event)
		}
	}
	return nil
}

// preToolUseHookCounts distinguishes a healthy single owned handler from a
// config that merely contains the current command alongside stale or duplicate
// Crossing Guard handlers. The latter must be normalized by Install.
func preToolUseHookCounts(config map[string]any, want string, matcherOK func(map[string]any) bool) (owned, current int) {
	return hookEventCounts(config, "PreToolUse", want, matcherOK)
}

func hookEventCounts(config map[string]any, event, want string, matcherOK func(map[string]any) bool) (owned, current int) {
	hooks, _ := config["hooks"].(map[string]any)
	entries, _ := hooks[event].([]any)
	for _, rawEntry := range entries {
		entry, _ := rawEntry.(map[string]any)
		inner, _ := entry["hooks"].([]any)
		for _, rawHook := range inner {
			hook, _ := rawHook.(map[string]any)
			command, _ := hook["command"].(string)
			if !isOurHookCommand(command) {
				continue
			}
			owned++
			if command == want && matcherOK(entry) {
				current++
			}
		}
	}
	return owned, current
}

// removeOurPreToolUseHook removes only our commands from the shared vendor JSON
// shape, preserving foreign entries and other top-level configuration.
func removeOurPreToolUseHook(path, _ string) (bool, error) {
	return removeOurHookEvents(path, "PreToolUse")
}

func removeOurHookEvents(path string, events ...string) (bool, error) {
	snapshot, err := vendorconfig.Read(path)
	if err != nil {
		return false, err
	}
	if !snapshot.Exists {
		return false, nil
	}
	var config map[string]any
	if err := json.Unmarshal(snapshot.Data, &config); err != nil {
		return false, err
	}
	if err := validateHookEventShapes(config, events...); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	changed := removeOurHooks(&config, events...)
	if !changed {
		return false, nil
	}
	output, _ := json.MarshalIndent(config, "", "  ")
	if _, err := vendorconfig.Replace(path, snapshot, append(output, '\n')); err != nil {
		return false, err
	}
	return true, nil
}

func removeOurHooks(config *map[string]any, events ...string) bool {
	hooks, _ := (*config)["hooks"].(map[string]any)
	if hooks == nil {
		return false
	}
	changed := false
	for _, event := range events {
		entries, _ := hooks[event].([]any)
		if entries == nil {
			continue
		}
		kept := make([]any, 0, len(entries))
		for _, rawEntry := range entries {
			entry, _ := rawEntry.(map[string]any)
			if entry == nil {
				kept = append(kept, rawEntry)
				continue
			}
			inner, ok := entry["hooks"].([]any)
			if !ok {
				kept = append(kept, rawEntry)
				continue
			}
			keptInner := make([]any, 0, len(inner))
			hadOurs := false
			for _, rawHook := range inner {
				hook, _ := rawHook.(map[string]any)
				if command, _ := hook["command"].(string); isOurHookCommand(command) {
					changed, hadOurs = true, true
					continue
				}
				keptInner = append(keptInner, rawHook)
			}
			if hadOurs && len(keptInner) == 0 {
				continue
			}
			entry["hooks"] = keptInner
			kept = append(kept, entry)
		}
		if len(kept) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}
	if !changed {
		return false
	}
	if len(hooks) == 0 {
		delete(*config, "hooks")
	}
	return true
}

func appendHookEvent(config map[string]any, event, matcher, command string) {
	hooks, _ := config["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		config["hooks"] = hooks
	}
	entries, _ := hooks[event].([]any)
	entry := map[string]any{
		"hooks": []any{map[string]any{"type": "command", "command": command}},
	}
	if matcher != "" {
		entry["matcher"] = matcher
	}
	hooks[event] = append(entries, entry)
}
