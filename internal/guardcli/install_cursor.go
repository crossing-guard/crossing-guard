package guardcli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"crossing-guard/internal/rulebook"
	"crossing-guard/internal/vendorconfig"
	"crossing-guard/internal/vendorpaths"
)

func init() { registerHookInstaller(cursorInstaller{}) }

const cursorVendor = "cursor"

var cursorLifecycleHookEvents = []string{
	"sessionStart", "preToolUse", "postToolUse", "postToolUseFailure", "sessionEnd",
}

var (
	cursorClientLookPath = exec.LookPath
	cursorClientStat     = os.Stat
)

type cursorInstaller struct{}

func (cursorInstaller) Name() string          { return cursorVendor }
func (cursorInstaller) SelectFlag() string    { return "--cursor-config" }
func (cursorInstaller) DefaultConfig() string { return "" }
func (cursorInstaller) IsDefault() bool       { return false }

// ResolveConfig keeps config discovery separate from client presence. A stale config
// remains discoverable for surgical uninstall, while RuntimePresent prevents it from
// being reported as an installed Cursor client.
func (cursorInstaller) ResolveConfig() string {
	path := homeJoin(filepath.FromSlash(vendorpaths.CursorHooksRelative))
	if path == "" {
		return ""
	}
	if cursorClientPresent() || fileExists(path) {
		return path
	}
	return ""
}

func (cursorInstaller) RuntimePresent() bool { return cursorClientPresent() }

func (cursorInstaller) RuntimeConnectionDescriptor() RuntimeConnectionDescriptor {
	return RuntimeConnectionDescriptor{
		Runtime: cursorVendor, DisplayName: "Cursor",
		ConfigPath:  homeJoin(filepath.FromSlash(vendorpaths.CursorHooksRelative)),
		ConfigLabel: "Cursor user hooks",
		HookPhases:  append([]string(nil), cursorLifecycleHookEvents...),
		Surfaces: []RuntimeConnectionSurface{
			{ID: "agent-chat", Label: "Agent Chat", Description: "User-selected surface; native hook payloads do not yet prove this label."},
			{ID: "cmd-k", Label: "Cmd+K", Description: "User-selected surface; verify separately from Agent Chat."},
		},
		Limitations: []string{
			"Crossing Guard never opens or controls Cursor; you perform verification when ready.",
			"Confirm-and-record approval timing is unverified and therefore fails closed immediately.",
			"Cursor CLI, Tab, and Cloud Agents are not covered by this connection.",
		},
		VerificationSteps: []string{
			"Choose Agent Chat or Cmd+K; the choice is a user label until native payloads can prove the surface.",
			"Open Cursor yourself in a disposable folder containing no private project material.",
			"Any Cursor session you start follows Cursor's own authentication, network, and data behavior; Crossing Guard sends no prompt or repository to Cursor.",
			"Ask Cursor to perform one harmless local tool action while Crossing Guard watches.",
			"For a denial check, use only the harmless canary shown after the active rulebook confirms it will deny.",
		},
	}
}

func (cursorInstaller) PreviewRuntimeConnection(configPath, executable, operation string) (RuntimeConnectionPreview, error) {
	snapshot, err := vendorconfig.Read(configPath)
	if err != nil {
		return RuntimeConnectionPreview{}, err
	}
	config := map[string]any{}
	if snapshot.Exists {
		if err := json.Unmarshal(snapshot.Data, &config); err != nil {
			return RuntimeConnectionPreview{}, fmt.Errorf("cursor hook config is not valid JSON; fix it before continuing")
		}
	}
	if err := validateCursorHookConfig(config); err != nil {
		return RuntimeConnectionPreview{}, err
	}
	owned, foreign := cursorHandlerCounts(config)
	current := cursorConfigIsCurrent(config, executable)
	preview := RuntimeConnectionPreview{
		Runtime: cursorVendor, Operation: operation, ConfigPath: configPath,
		HookBinary: executable, ForeignHandlersPreserved: foreign,
		HookPhases: append([]string(nil), cursorLifecycleHookEvents...), snapshot: snapshot,
	}
	switch operation {
	case ConnectionOperationConnect:
		preview.WillChange = !current
		if current {
			preview.Action = "already_connected"
			preview.Summary = []string{"Cursor already has exactly one current Crossing Guard command in every supported hook phase."}
		} else {
			preview.Action = "connect_or_repair"
			preview.OwnedHandlersRemoved = owned
			preview.OwnedHandlersAdded = len(cursorLifecycleHookEvents)
			preview.Summary = []string{
				"Add exactly one Crossing Guard command to each supported Cursor hook phase.",
				fmt.Sprintf("Preserve %d foreign hook handler(s) and all unrelated top-level settings.", foreign),
				"Do not launch Cursor; verification remains a separate user-driven action.",
			}
		}
	case ConnectionOperationDisconnect:
		preview.WillChange = owned > 0
		preview.OwnedHandlersRemoved = owned
		if owned > 0 {
			preview.Action = "disconnect"
			preview.Summary = []string{
				fmt.Sprintf("Remove %d Crossing Guard Cursor hook handler(s).", owned),
				fmt.Sprintf("Preserve %d foreign hook handler(s) and all unrelated Cursor settings.", foreign),
				"Do not stop, update, sign out of, or uninstall Cursor.",
			}
		} else {
			preview.Action = "already_disconnected"
			preview.Summary = []string{"No Crossing Guard Cursor hook handler is present; disconnect only clears any surviving consent record."}
		}
	default:
		return RuntimeConnectionPreview{}, fmt.Errorf("unsupported Cursor connection operation")
	}
	preview.WillCreateFile = !snapshot.Exists && preview.WillChange
	preview.WillCreateBackup = snapshot.Exists && preview.WillChange
	return preview, nil
}

func cursorClientPresent() bool {
	for _, binary := range []string{"cursor", "cursor-agent"} {
		if _, err := cursorClientLookPath(binary); err == nil {
			return true
		}
	}
	if runtime.GOOS != "darwin" {
		return false
	}
	applications := []string{"/Applications/Cursor.app"}
	if home, err := os.UserHomeDir(); err == nil {
		applications = append(applications, filepath.Join(home, "Applications", "Cursor.app"))
	}
	for _, application := range applications {
		if info, err := cursorClientStat(application); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

func (cursorInstaller) ManualStep() string {
	return "run a harmless Cursor Agent tool canary and confirm its action and result appear in Crossing Guard; configuration is not proof of firing"
}

// Cursor's hook deadline has not been measured in a natural installed client. An ask
// therefore fails closed immediately instead of inheriting another runtime's budget.
func (cursorInstaller) HookAskBudget() time.Duration { return 0 }

func (cursorInstaller) HookBinary(configPath string) string {
	config, ok := readCursorHookConfig(configPath)
	if !ok {
		return ""
	}
	hooks, _ := config["hooks"].(map[string]any)
	entries, _ := hooks["preToolUse"].([]any)
	for _, rawEntry := range entries {
		entry, _ := rawEntry.(map[string]any)
		command, _ := entry["command"].(string)
		if binary := ourHookBinaryFromCommand(command); binary != "" {
			return binary
		}
	}
	return ""
}

func (cursorInstaller) IsCurrent(configPath, executable string) bool {
	config, ok := readCursorHookConfig(configPath)
	return ok && cursorConfigIsCurrent(config, executable)
}

func cursorConfigIsCurrent(config map[string]any, executable string) bool {
	if !cursorVersionIsCurrent(config) {
		return false
	}
	want := hookCommand(executable, cursorVendor)
	totalOwned, _ := cursorHandlerCounts(config)
	if totalOwned != len(cursorLifecycleHookEvents) {
		return false
	}
	for _, event := range cursorLifecycleHookEvents {
		owned, current := cursorHookEventCounts(config, event, want)
		if owned != 1 || current != 1 {
			return false
		}
	}
	return true
}

func (cursorInstaller) HookPhaseStatus(configPath, executable string) map[string]bool {
	want := hookCommand(executable, cursorVendor)
	config, ok := readCursorHookConfig(configPath)
	status := make(map[string]bool, len(cursorLifecycleHookEvents))
	for _, event := range cursorLifecycleHookEvents {
		owned, current := cursorHookEventCounts(config, event, want)
		status[canonicalHookEvent(event)] = ok && owned == 1 && current == 1
	}
	return status
}

func (cursorInstaller) Install(configPath, executable string) error {
	snapshot, err := vendorconfig.Read(configPath)
	if err != nil {
		return err
	}
	config := map[string]any{}
	if snapshot.Exists {
		if err := json.Unmarshal(snapshot.Data, &config); err != nil {
			return fmt.Errorf("%s is not valid JSON (%w) — fix it first; refusing to overwrite it", configPath, err)
		}
	}
	if err := validateCursorHookConfig(config); err != nil {
		return fmt.Errorf("%s: %w", configPath, err)
	}
	want := hookCommand(executable, cursorVendor)
	if cursorConfigIsCurrent(config, executable) {
		fmt.Println("already installed:", configPath)
		return nil
	}
	removeCursorHooks(&config, cursorHookEventNames(config)...)
	config["version"] = float64(1)
	for _, event := range cursorLifecycleHookEvents {
		appendCursorHook(config, event, want)
	}
	output, _ := json.MarshalIndent(config, "", "  ")
	backup, err := vendorconfig.Replace(configPath, snapshot, append(output, '\n'))
	if err != nil {
		return err
	}
	fmt.Printf("installed Cursor sessionStart/preToolUse/postToolUse/postToolUseFailure/sessionEnd hooks -> %s\n", configPath)
	if backup != "" {
		fmt.Printf("backup (immediate prior) -> %s\n", backup)
	}
	if loaded, loadErr := rulebook.LoadDocument(); loadErr == nil {
		fmt.Printf("rules: %s [%s]\n", loaded.Path, loaded.Selection)
	} else {
		fmt.Printf("rules: unavailable (%v)\n", loadErr)
	}
	fmt.Println("NOTE: configuration is not firing; verify with a natural Cursor Agent canary.")
	fmt.Println("NOTE: Cursor approval timing is unverified; confirm-and-record fails closed until measured.")
	return nil
}

func (cursorInstaller) Uninstall(configPath, _ string) (bool, error) {
	snapshot, err := vendorconfig.Read(configPath)
	if err != nil || !snapshot.Exists {
		return false, err
	}
	var config map[string]any
	if err := json.Unmarshal(snapshot.Data, &config); err != nil {
		return false, err
	}
	if err := validateCursorHookConfig(config); err != nil {
		return false, fmt.Errorf("%s: %w", configPath, err)
	}
	if !removeCursorHooks(&config, cursorHookEventNames(config)...) {
		return false, nil
	}
	output, _ := json.MarshalIndent(config, "", "  ")
	if _, err := vendorconfig.Replace(configPath, snapshot, append(output, '\n')); err != nil {
		return false, err
	}
	return true, nil
}

func (cursorInstaller) NormalizeSessionEntrySource(source string) string {
	switch source {
	case "", "startup":
		return "start"
	case "resume":
		return "resume"
	case "clear":
		return "context-reset"
	case "compact":
		return "context-compact"
	default:
		return "unknown"
	}
}

func cursorVersionIsCurrent(config map[string]any) bool {
	version, ok := config["version"].(float64)
	return ok && version == 1
}

func readCursorHookConfig(path string) (map[string]any, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var config map[string]any
	if json.Unmarshal(raw, &config) != nil || validateCursorHookConfig(config) != nil {
		return nil, false
	}
	return config, true
}

func validateCursorHookConfig(config map[string]any) error {
	if rawVersion, present := config["version"]; present {
		version, ok := rawVersion.(float64)
		if !ok || version != 1 {
			return fmt.Errorf("version must be the number 1; refusing to replace an unknown Cursor hook schema")
		}
	}
	rawHooks, present := config["hooks"]
	if !present {
		return nil
	}
	hooks, ok := rawHooks.(map[string]any)
	if !ok {
		return fmt.Errorf("hooks must be a JSON object; refusing to replace it")
	}
	for _, event := range cursorLifecycleHookEvents {
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

func cursorHookEventCounts(config map[string]any, event, want string) (owned, current int) {
	hooks, _ := config["hooks"].(map[string]any)
	entries, _ := hooks[event].([]any)
	for _, rawEntry := range entries {
		entry, _ := rawEntry.(map[string]any)
		command, _ := entry["command"].(string)
		if !isOurHookCommand(command) {
			continue
		}
		owned++
		if command == want {
			current++
		}
	}
	return owned, current
}

func cursorHandlerCounts(config map[string]any) (owned, foreign int) {
	hooks, _ := config["hooks"].(map[string]any)
	for _, rawEntries := range hooks {
		entries, ok := rawEntries.([]any)
		if !ok {
			continue
		}
		for _, rawEntry := range entries {
			entry, _ := rawEntry.(map[string]any)
			command, _ := entry["command"].(string)
			if isOurHookCommand(command) {
				owned++
			} else {
				foreign++
			}
		}
	}
	return owned, foreign
}

func cursorHookEventNames(config map[string]any) []string {
	hooks, _ := config["hooks"].(map[string]any)
	events := make([]string, 0, len(hooks))
	for event := range hooks {
		events = append(events, event)
	}
	return events
}

func removeCursorHooks(config *map[string]any, events ...string) bool {
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
			command, _ := entry["command"].(string)
			if isOurHookCommand(command) {
				changed = true
				continue
			}
			kept = append(kept, rawEntry)
		}
		if len(kept) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}
	if changed && len(hooks) == 0 {
		delete(*config, "hooks")
	}
	return changed
}

func appendCursorHook(config map[string]any, event, command string) {
	hooks, _ := config["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		config["hooks"] = hooks
	}
	entries, _ := hooks[event].([]any)
	hooks[event] = append(entries, map[string]any{"command": command})
}
