package guardcli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"crossing-guard/harvest"
	"crossing-guard/internal/vendorconfig"
)

func init() { registerHookInstaller(openCodeInstaller{}) }

const (
	openCodeVendor         = "opencode"
	openCodePluginFile     = "crossing-guard-collection.js"
	openCodePluginMarker   = "// crossing-guard-owned: opencode-collection-v2"
	openCodePluginMarkerV1 = "// crossing-guard-owned: opencode-collection-v1"
	// openCodeDecisionMarker names the governed plugin shape (natural-session
	// plan, Slice C): tool.execute.before evaluates through govern-opencode and
	// blocks by throw — OpenCode's documented prevention mechanism.
	openCodeDecisionMarker = "// crossing-guard-owned: opencode-decision-v1"
	// openCodeGatePrefix starts the artifact line that records the version-gate
	// verdict at install time. Status surfaces read the fact from the artifact
	// instead of re-measuring, so the displayed lane cannot drift from what was
	// actually installed (plan deviation 4).
	openCodeGatePrefix = "// crossing-guard-gate: "
)

type openCodeInstaller struct{}

func (openCodeInstaller) Name() string          { return openCodeVendor }
func (openCodeInstaller) SelectFlag() string    { return "--opencode-config" }
func (openCodeInstaller) DefaultConfig() string { return "" }
func (openCodeInstaller) IsDefault() bool       { return false }

// CollectionOnly answers from the installed artifact when one exists (the gate
// verdict was decided at install time); otherwise from the gate a connect would
// apply right now. A decision-lane plugin is governed, not collection-only.
func (openCodeInstaller) CollectionOnly(configPath string) bool {
	if configPath != "" {
		if raw, err := os.ReadFile(openCodePluginPath(configPath)); err == nil && isOwnedOpenCodePlugin(raw) {
			return !bytes.Contains(raw, []byte(openCodeDecisionMarker))
		}
	}
	return resolveOpenCodeGate().Lane != "decision"
}

func (openCodeInstaller) ResolveConfig() string {
	if configured := os.Getenv("OPENCODE_CONFIG_DIR"); configured != "" {
		if directoryExists(configured) {
			return configured
		}
		return ""
	}
	if binary, err := exec.LookPath("opencode"); err == nil {
		ctx, cancel := contextWithShortTimeout()
		defer cancel()
		if output, err := exec.CommandContext(ctx, binary, "debug", "paths").Output(); err == nil {
			for _, line := range strings.Split(string(output), "\n") {
				if key, value, ok := strings.Cut(strings.TrimSpace(line), " "); ok && key == "config" {
					value = strings.TrimSpace(value)
					if value != "" {
						return value
					}
				}
			}
		}
	}
	var candidate string
	if runtime.GOOS == "windows" {
		candidate = filepath.Join(os.Getenv("APPDATA"), "opencode")
	} else if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		candidate = filepath.Join(base, "opencode")
	} else {
		candidate = homeJoin(".config", "opencode")
	}
	if directoryExists(candidate) {
		return candidate
	}
	return ""
}

func contextWithShortTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 2*time.Second)
}

func directoryExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func openCodePluginPath(config string) string {
	return filepath.Join(config, "plugins", openCodePluginFile)
}

// openCodeGate is the version-gate verdict (natural-session plan, Slice C):
// the decision lane installs only when the measured OpenCode CLI revision
// matches the interface revision the adapter was verified against; any
// mismatch or unmeasurable CLI installs collection-only with the reason
// visible — never a silent governance gap.
type openCodeGate struct {
	Lane      string // "decision" | "collection-only"
	Installed string // measured CLI revision, "" when unmeasurable
	Interface string // harvest adapter interface revision
	Reason    string // why collection-only, when it is
}

// openCodeVersionProbe measures the installed OpenCode CLI revision.
// Injectable so tests never depend on the host machine's binary.
var openCodeVersionProbe = measuredOpenCodeCLIVersion

var openCodeMeasuredVersion struct {
	once  sync.Once
	value string
}

func measuredOpenCodeCLIVersion() string {
	openCodeMeasuredVersion.once.Do(func() {
		binary, err := exec.LookPath(openCodeVendor)
		if err != nil {
			return
		}
		ctx, cancel := contextWithShortTimeout()
		defer cancel()
		output, err := exec.CommandContext(ctx, binary, "--version").Output()
		if err != nil {
			return
		}
		fields := strings.Fields(string(output))
		if len(fields) > 0 {
			openCodeMeasuredVersion.value = fields[len(fields)-1]
		}
	})
	return openCodeMeasuredVersion.value
}

func resolveOpenCodeGate() openCodeGate {
	gate := openCodeGate{Interface: harvest.CLIRevision(openCodeVendor), Installed: openCodeVersionProbe()}
	switch {
	case gate.Installed == "":
		gate.Lane = "collection-only"
		gate.Reason = "the installed OpenCode CLI revision could not be measured"
	case gate.Installed != gate.Interface:
		gate.Lane = "collection-only"
		gate.Reason = "installed OpenCode CLI " + gate.Installed +
			" does not match the verified interface " + gate.Interface
	default:
		gate.Lane = "decision"
	}
	return gate
}

func (g openCodeGate) artifactLine() string {
	installed := g.Installed
	if installed == "" {
		installed = "unmeasured"
	}
	line := openCodeGatePrefix + "lane=" + g.Lane + " installed=" + installed + " interface=" + g.Interface
	if g.Reason != "" {
		line += " reason=" + strings.ReplaceAll(g.Reason, "\n", " ")
	}
	return line
}

// openCodeGateFromPlugin recovers the install-time gate verdict from the
// artifact. ok is false for plugins that predate the gate line.
func openCodeGateFromPlugin(data []byte) (openCodeGate, bool) {
	for _, line := range strings.Split(string(data), "\n") {
		rest, found := strings.CutPrefix(line, openCodeGatePrefix)
		if !found {
			continue
		}
		gate := openCodeGate{}
		for _, field := range strings.SplitN(rest, " ", 4) {
			key, value, ok := strings.Cut(field, "=")
			if !ok {
				continue
			}
			switch key {
			case "lane":
				gate.Lane = value
			case "installed":
				if value != "unmeasured" {
					gate.Installed = value
				}
			case "interface":
				gate.Interface = value
			case "reason":
				gate.Reason = value
			}
		}
		return gate, gate.Lane != ""
	}
	return openCodeGate{}, false
}

// openCodePlugin renders the owned plugin for the given gate verdict. Both
// lanes share the collection prelude; the decision lane's tool.execute.before
// spawns govern-opencode alone (it stages and flushes the observation with its
// decision — running collect-hook too would double-record every attempt, plan
// deviation 5) and blocks a deny by throwing, OpenCode's documented prevention
// mechanism. Evaluation failures are reported and fail open — never silent.
func openCodePlugin(executable string, gate openCodeGate) []byte {
	quoted, _ := json.Marshal(executable)
	marker := openCodePluginMarker
	governHelper := ""
	before := `  "tool.execute.before": async (input, output) => {
    collect({
      hook_event_name: "PreToolUse",
      session_id: input.sessionID,
      call_id: input.callID,
      tool_name: canonicalTool(input.tool),
      cwd: context.directory,
      tool_input: canonicalArgs(output.args),
    })
  },`
	if gate.Lane == "decision" {
		marker = openCodeDecisionMarker
		governHelper = `  const govern = (payload) => {
    try {
      const child = spawnSync(crossingGuardBinary, ["govern-hook", "--runtime", "opencode", "--carrier"], {
        input: JSON.stringify(payload),
        encoding: "utf8",
        stdio: ["pipe", "pipe", "pipe"],
        timeout: 5000,
        maxBuffer: 65536,
        windowsHide: true,
      })
      if (child.error) { report(child.error.code === "ETIMEDOUT" ? "govern.timeout" : "govern.spawn-error"); return null }
      if (child.signal) { report("govern.signal"); return null }
      if (child.status !== 0) { report("govern.exit"); return null }
      const decision = JSON.parse(String(child.stdout || "").trim())
      if (decision && decision.error) report("govern.error-recorded")
      if (decision && decision.decision === "allow") {
        report("firing", "info")
        deliver(payload.session_id, decision.crossing_guard_deliveries)
      }
      return decision
    } catch (_) {
      report("govern.exception")
      return null // evaluation failed: reported and fail-open — never a silent block
    }
  }
`
		before = `  "tool.execute.before": async (input, output) => {
    const decision = govern({
      hook_event_name: "PreToolUse",
      session_id: input.sessionID,
      call_id: input.callID,
      tool_name: canonicalTool(input.tool),
      cwd: context.directory,
      tool_input: canonicalArgs(output.args),
    })
    if (decision && decision.decision === "deny") {
      // OpenCode's documented prevention mechanism: throwing here blocks the tool call.
      throw new Error("Crossing Guard denied this tool call: " + (decision.reason || "blocked by policy"))
    }
  },`
	}
	return []byte(fmt.Sprintf(`%s
%s
import { spawnSync } from "node:child_process"

const crossingGuardBinary = %s

function canonicalTool(name) {
  return ({read:"Read", edit:"Edit", write:"Write", glob:"Glob", grep:"Grep", bash:"Bash"})[name] || name
}

function canonicalArgs(args) {
  const value = {...(args || {})}
  for (const [native, canonical] of [
    ["filePath", "file_path"], ["notebookPath", "notebook_path"],
    ["oldString", "old_string"], ["newString", "new_string"],
    ["replaceAll", "replace_all"], ["patchText", "command"],
  ]) if (value[canonical] === undefined && value[native] !== undefined) value[canonical] = value[native]
  return value
}

export const CrossingGuardCollection = async (context) => {
  const reported = new Set()
  const report = (code, level = "warn") => {
    if (reported.has(code) || reported.size >= 4) return
    reported.add(code)
    try {
      Promise.resolve(context.client.app.log({
        body: {
          service: "crossing-guard",
          level,
          message: "OpenCode collection " + code,
        },
      })).catch(() => {})
    } catch (_) {}
  }
  // The collection-hook delivery channel (helper-session-attachment plan
  // D5): when the daemon handed this boundary a pending helper message, the
  // binary prints one JSON line and this plugin appends it to the session
  // through the instance's own API as a context-only message. The client is
  // bound to this very instance, so a plainly launched TUI with no network
  // listener is reached all the same. Failures are reported, never thrown.
  const deliver = (sessionID, handed) => {
    if (typeof handed === "string") {
      try {
        const line = handed.split("\n").find(l => l.startsWith("{"))
        handed = line ? JSON.parse(line).crossing_guard_deliveries : null
      } catch (_) { report("delivery.parse-error"); return }
    }
    if (!Array.isArray(handed) || !handed.length || !sessionID) return
    for (const item of handed) {
      try {
        Promise.resolve(context.client.session.prompt({
          path: { id: sessionID },
          body: { noReply: true, parts: [{ type: "text", text: String(item.message || "") }] },
        })).catch(() => report("delivery.append-error"))
      } catch (_) { report("delivery.append-exception") }
    }
  }
  const collect = (payload, observe) => {
    const phase = payload.hook_event_name === "PreToolUse" ? "before" : "after"
    // A turn-boundary event carries OUR kind on the command line; the binary
    // never has to know OpenCode's event name for it.
    const args = ["collect-hook", "--runtime", "opencode", "--carrier"]
    if (observe) args.push("--observe", observe)
    try {
      const child = spawnSync(crossingGuardBinary, args, {
        input: JSON.stringify(payload),
        encoding: "utf8",
        stdio: ["pipe", "pipe", "pipe"],
        timeout: 2500,
        maxBuffer: 65536,
        windowsHide: true,
      })
      if (child.error) {
        report(child.error.code === "ETIMEDOUT" ? phase + ".timeout" : phase + ".spawn-error")
      } else if (child.signal) {
        report(phase + ".signal")
      } else if (child.status !== 0) {
        report(phase + ".exit")
      } else {
        report("firing", "info")
        deliver(payload.session_id, child.stdout)
      }
    } catch (_) {
      report(phase + ".exception")
      // Collection is intentionally fail-open and must never interrupt OpenCode.
    }
  }
%s  return ({
%s
  "tool.execute.after": async (input, output) => {
    collect({
      hook_event_name: "PostToolUse",
      session_id: input.sessionID,
      call_id: input.callID,
      tool_name: canonicalTool(input.tool),
      cwd: context.directory,
      tool_input: canonicalArgs(input.args),
      tool_response: output,
    })
  },
  // Bus events. session.idle is OpenCode saying the agent handed the
  // conversation back — NOT the session ending (opencode-collection-plan).
  // Installed on the owner's decision; PROBE-GATED until a canary has seen
  // the row on this machine, so no adapter declares it yet.
  "event": async ({ event }) => {
    if (!event || event.type !== "session.idle") return
    const sessionID = event.properties && event.properties.sessionID
    if (!sessionID) return
    collect({
      hook_event_name: "session.idle",
      session_id: sessionID,
      cwd: context.directory,
    }, "turn.ended")
  },
  })
}
`, marker, gate.artifactLine(), quoted, governHelper, before))
}

func isOwnedOpenCodePlugin(data []byte) bool {
	return bytes.Contains(data, []byte(openCodePluginMarker)) ||
		bytes.Contains(data, []byte(openCodePluginMarkerV1)) ||
		bytes.Contains(data, []byte(openCodeDecisionMarker))
}

func (openCodeInstaller) Install(config, executable string) error {
	path := openCodePluginPath(config)
	snapshot, err := vendorconfig.Read(path)
	if err != nil {
		return err
	}
	if snapshot.Exists && !isOwnedOpenCodePlugin(snapshot.Data) {
		return fmt.Errorf("%s already exists and is not owned by Crossing Guard; refusing to overwrite it", path)
	}
	gate := resolveOpenCodeGate()
	want := openCodePlugin(executable, gate)
	if snapshot.Exists && bytes.Equal(snapshot.Data, want) {
		fmt.Println("already installed:", path)
		return nil
	}
	backup, err := vendorconfig.Replace(path, snapshot, want)
	if err != nil {
		return err
	}
	if gate.Lane == "decision" {
		fmt.Printf("installed governed OpenCode plugin (decision lane) -> %s\n", path)
		fmt.Printf("compatibility: installed OpenCode CLI %s matches the verified interface %s\n",
			gate.Installed, gate.Interface)
		fmt.Println("NOTE: tool calls are evaluated by the one evaluator; a deny blocks the call, and an internal evaluation failure records a governance error and fails open. Unproven until the firing canary passes.")
	} else {
		fmt.Printf("installed collection-only OpenCode plugin -> %s\n", path)
		fmt.Printf("compatibility: %s — the decision lane was NOT installed\n", gate.Reason)
		fmt.Println("NOTE: OpenCode actions/results are collected; this plugin does not evaluate or enforce policy.")
	}
	if backup != "" {
		fmt.Printf("backup (immediate prior) -> %s\n", backup)
	}
	return nil
}

func (openCodeInstaller) IsCurrent(config, executable string) bool {
	raw, err := os.ReadFile(openCodePluginPath(config))
	return err == nil && bytes.Equal(raw, openCodePlugin(executable, resolveOpenCodeGate()))
}

func (openCodeInstaller) HookBinary(config string) string {
	raw, err := os.ReadFile(openCodePluginPath(config))
	if err != nil || !isOwnedOpenCodePlugin(raw) {
		return ""
	}
	const prefix = "const crossingGuardBinary = "
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, prefix) {
			var binary string
			if json.Unmarshal([]byte(strings.TrimPrefix(line, prefix)), &binary) == nil {
				return binary
			}
		}
	}
	return ""
}

func (openCodeInstaller) HookPhaseStatus(config, executable string) map[string]bool {
	current := (openCodeInstaller{}).IsCurrent(config, executable)
	return map[string]bool{"SessionStart": false, "PreToolUse": current, "PostToolUse": current, "SessionEnd": false,
		"turn.ended": current}
}

func (openCodeInstaller) ManualStep() string {
	return "restart OpenCode, run a harmless tool canary, and confirm its action and result appear in Crossing Guard"
}

func (openCodeInstaller) Uninstall(config, _ string) (bool, error) {
	path := openCodePluginPath(config)
	snapshot, err := vendorconfig.Read(path)
	if err != nil || !snapshot.Exists {
		return false, err
	}
	if !isOwnedOpenCodePlugin(snapshot.Data) {
		return false, nil
	}
	// Replace first provides the standard immediate-prior recovery copy and verifies
	// that the file did not race between inspection and mutation.
	if _, err := vendorconfig.Replace(path, snapshot, nil); err != nil {
		return false, err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return false, err
	}
	return true, nil
}

// OpenCodeGovernanceStatus is the presentation-safe governance-lane fact for
// status surfaces (Settings, doctor). It reads the installed artifact's gate
// verdict when one exists; otherwise it reports the gate a connect would apply
// now. Lane is "" when no owned plugin is attached.
type OpenCodeGovernanceStatus struct {
	Attached  bool   `json:"attached"`
	Lane      string `json:"lane,omitempty"` // "decision" | "collection-only" | ""
	Installed string `json:"installed_cli,omitempty"`
	Interface string `json:"interface_revision,omitempty"`
	Note      string `json:"note,omitempty"`
}

func OpenCodeGovernance() OpenCodeGovernanceStatus {
	status := OpenCodeGovernanceStatus{Interface: harvest.CLIRevision(openCodeVendor)}
	config := openCodeInstaller{}.ResolveConfig()
	if config == "" {
		return status
	}
	raw, err := os.ReadFile(openCodePluginPath(config))
	if err != nil || !isOwnedOpenCodePlugin(raw) {
		return status
	}
	status.Attached = true
	if gate, ok := openCodeGateFromPlugin(raw); ok {
		status.Lane, status.Installed, status.Note = gate.Lane, gate.Installed, gate.Reason
		return status
	}
	// Pre-gate artifact (v1/v2 markers carry no gate line): collection-only by construction.
	status.Lane = "collection-only"
	status.Note = "installed plugin predates the decision lane; reconnect to apply the version gate"
	return status
}
