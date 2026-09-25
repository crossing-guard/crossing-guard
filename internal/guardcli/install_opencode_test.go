package guardcli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// pinOpenCodeVersion pins the measured-CLI probe so no test depends on the
// host machine's opencode binary. "" models an unmeasurable CLI.
func pinOpenCodeVersion(t *testing.T, version string) {
	t.Helper()
	previous := openCodeVersionProbe
	openCodeVersionProbe = func() string { return version }
	t.Cleanup(func() { openCodeVersionProbe = previous })
}

func TestOpenCodeInstallIsAdditiveCollectionOnlyAndReversible(t *testing.T) {
	pinOpenCodeVersion(t, "") // unmeasurable CLI → the gate installs collection-only
	config := t.TempDir()
	executable := filepath.Join(t.TempDir(), "crossing-guard")
	installer := openCodeInstaller{}
	if err := installer.Install(config, executable); err != nil {
		t.Fatal(err)
	}
	path := openCodePluginPath(config)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{"collect-hook", "tool.execute.before", "tool.execute.after", openCodePluginMarker,
		"canonicalTool", `["patchText", "command"]`, openCodeGatePrefix + "lane=collection-only"} {
		if !strings.Contains(text, want) {
			t.Fatalf("plugin missing %q:\n%s", want, text)
		}
	}
	for _, forbidden := range []string{"\"hook\"", "permissionDecision", "govern/decide", "govern-hook", "throw new Error"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("collection plugin contains decision-path token %q", forbidden)
		}
	}
	if !installer.CollectionOnly(config) {
		t.Fatal("collection-only plugin reported as governed")
	}
	if !installer.IsCurrent(config, executable) || installer.HookBinary(config) != executable {
		t.Fatal("installed plugin was not detected as current")
	}
	status := installer.HookPhaseStatus(config, executable)
	if !status["PreToolUse"] || !status["PostToolUse"] || status["SessionStart"] || status["SessionEnd"] {
		t.Fatalf("unexpected OpenCode hook capability: %+v", status)
	}
	removed, err := installer.Uninstall(config, executable)
	if err != nil || !removed {
		t.Fatalf("uninstall removed=%t err=%v", removed, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("owned plugin remains after uninstall: %v", err)
	}
	if backup, err := os.ReadFile(path + ".crossing-guard.bak"); err != nil || string(backup) != text {
		t.Fatalf("recovery backup mismatch err=%v", err)
	}
}

func TestOpenCodeVersionGateSelectsLane(t *testing.T) {
	interfaceRevision := resolveOpenCodeGate().Interface
	if interfaceRevision == "" {
		t.Fatal("opencode adapter publishes no interface revision — the gate has nothing to compare")
	}

	t.Run("matching revision installs the decision lane", func(t *testing.T) {
		pinOpenCodeVersion(t, interfaceRevision)
		config := t.TempDir()
		executable := filepath.Join(t.TempDir(), "crossing-guard")
		installer := openCodeInstaller{}
		if err := installer.Install(config, executable); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(openCodePluginPath(config))
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		for _, want := range []string{openCodeDecisionMarker, "govern-hook", "throw new Error",
			openCodeGatePrefix + "lane=decision installed=" + interfaceRevision + " interface=" + interfaceRevision,
			"tool.execute.after", "collect-hook"} {
			if !strings.Contains(text, want) {
				t.Fatalf("decision plugin missing %q:\n%s", want, text)
			}
		}
		// One recorder per attempt: the before phase must not also run collect-hook.
		before := text[strings.Index(text, "tool.execute.before"):strings.Index(text, "tool.execute.after")]
		if strings.Contains(before, "collect(") {
			t.Fatalf("decision lane before-hook double-records via collect-hook:\n%s", before)
		}
		if installer.CollectionOnly(config) {
			t.Fatal("decision-lane plugin reported as collection-only")
		}
		if !installer.IsCurrent(config, executable) {
			t.Fatal("decision plugin was not detected as current")
		}
		gate, ok := openCodeGateFromPlugin(raw)
		if !ok || gate.Lane != "decision" || gate.Installed != interfaceRevision || gate.Interface != interfaceRevision {
			t.Fatalf("gate round-trip mismatch: ok=%t %+v", ok, gate)
		}
	})

	t.Run("mismatched revision installs collection-only with the recorded reason", func(t *testing.T) {
		pinOpenCodeVersion(t, "9.9.9")
		config := t.TempDir()
		executable := filepath.Join(t.TempDir(), "crossing-guard")
		installer := openCodeInstaller{}
		if err := installer.Install(config, executable); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(openCodePluginPath(config))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), openCodeDecisionMarker) || strings.Contains(string(raw), "govern-hook") {
			t.Fatal("mismatched CLI still received the decision lane")
		}
		gate, ok := openCodeGateFromPlugin(raw)
		if !ok || gate.Lane != "collection-only" || gate.Installed != "9.9.9" ||
			!strings.Contains(gate.Reason, "does not match") {
			t.Fatalf("gate verdict not recorded in artifact: ok=%t %+v", ok, gate)
		}
		if !installer.CollectionOnly(config) {
			t.Fatal("mismatched install reported as governed")
		}
	})

	t.Run("a CLI upgrade flips IsCurrent so the stale lane is visible", func(t *testing.T) {
		pinOpenCodeVersion(t, interfaceRevision)
		config := t.TempDir()
		executable := filepath.Join(t.TempDir(), "crossing-guard")
		installer := openCodeInstaller{}
		if err := installer.Install(config, executable); err != nil {
			t.Fatal(err)
		}
		if !installer.IsCurrent(config, executable) {
			t.Fatal("fresh decision install not current")
		}
		pinOpenCodeVersion(t, "9.9.9")
		if installer.IsCurrent(config, executable) {
			t.Fatal("decision plugin still reads current after the measured CLI diverged from the interface")
		}
	})
}

func TestOpenCodeGovernanceStatusReadsArtifact(t *testing.T) {
	interfaceRevision := resolveOpenCodeGate().Interface
	pinOpenCodeVersion(t, interfaceRevision)
	config := t.TempDir()
	t.Setenv("OPENCODE_CONFIG_DIR", config)
	executable := filepath.Join(t.TempDir(), "crossing-guard")
	if status := OpenCodeGovernance(); status.Attached || status.Lane != "" {
		t.Fatalf("unattached status should carry no lane: %+v", status)
	}
	if err := (openCodeInstaller{}).Install(config, executable); err != nil {
		t.Fatal(err)
	}
	status := OpenCodeGovernance()
	if !status.Attached || status.Lane != "decision" || status.Installed != interfaceRevision ||
		status.Interface != interfaceRevision {
		t.Fatalf("decision status mismatch: %+v", status)
	}
	// A pre-gate v2 artifact reports collection-only with the reconnect note.
	v2 := []byte(openCodePluginMarker + "\nconst crossingGuardBinary = " + strconv.Quote(executable) + "\n")
	if err := os.WriteFile(openCodePluginPath(config), v2, 0o600); err != nil {
		t.Fatal(err)
	}
	status = OpenCodeGovernance()
	if !status.Attached || status.Lane != "collection-only" || !strings.Contains(status.Note, "reconnect") {
		t.Fatalf("pre-gate artifact status mismatch: %+v", status)
	}
}

func TestOpenCodeInstallRefusesForeignPluginCollision(t *testing.T) {
	pinOpenCodeVersion(t, "")
	config := t.TempDir()
	path := openCodePluginPath(config)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("export const Foreign = {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (openCodeInstaller{}).Install(config, "/opt/crossing-guard"); err == nil {
		t.Fatal("foreign plugin collision was overwritten")
	}
	if raw, _ := os.ReadFile(path); string(raw) != "export const Foreign = {}\n" {
		t.Fatalf("foreign plugin changed: %s", raw)
	}
}

// writeOpenCodePluginProbe prepares the POSIX fake collector + node harness the
// execution-probe tests share. The fake records argv and stdin, and when asked
// as govern-opencode answers $CG_FAKE_DECISION (or exits $CG_FAKE_GOVERN_EXIT).
func writeOpenCodePluginProbe(t *testing.T, plugin []byte) (node, harness, pluginPath, collectorLog, collectorArgs, resultLog, diagnosticLog string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the callback execution probe uses a temporary POSIX collector; Windows compilation remains covered")
	}
	found, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable for the generated-plugin execution probe")
	}
	node = found
	dir := t.TempDir()
	collectorLog = filepath.Join(dir, "collector.jsonl")
	collectorArgs = filepath.Join(dir, "collector.args")
	resultLog = filepath.Join(dir, "result.jsonl")
	diagnosticLog = filepath.Join(dir, "diagnostic.jsonl")
	collector := filepath.Join(dir, "collector")
	collectorScript := `#!/bin/sh
printf '%s\n' "$*" >> "$CG_FAKE_ARGS"
cat >> "$CG_FAKE_LOG"
printf '\n' >> "$CG_FAKE_LOG"
if [ "$1" = "govern-hook" ]; then
  if [ -n "$CG_FAKE_GOVERN_EXIT" ]; then exit "$CG_FAKE_GOVERN_EXIT"; fi
  printf '%s\n' "$CG_FAKE_DECISION"
fi
if [ -n "$CG_FAKE_EXIT" ]; then exit "$CG_FAKE_EXIT"; fi
`
	if err := os.WriteFile(collector, []byte(collectorScript), 0o700); err != nil {
		t.Fatal(err)
	}
	pluginPath = filepath.Join(dir, "plugin.mjs")
	harness = filepath.Join(dir, "harness.mjs")
	harnessSource := `import { appendFileSync } from "node:fs"
import { pathToFileURL } from "node:url"
const plugin = await import(pathToFileURL(process.argv[2]))
const factory = Object.values(plugin).find((value) => typeof value === "function")
const hooks = await factory({
  directory: "/tmp/worktree",
  client: { app: { log: async (entry) => appendFileSync(process.argv[3], JSON.stringify(entry) + "\n") } },
})
let blocked = ""
try {
  await hooks["tool.execute.before"](
    { sessionID: "ses_native", callID: "call_native", tool: "bash" },
    { args: { command: "pwd" } },
  )
} catch (error) {
  blocked = String(error && error.message || error)
}
appendFileSync(process.argv[4], JSON.stringify({ blocked }) + "\n")
await hooks["tool.execute.after"](
  { sessionID: "ses_native", callID: "call_native", tool: "bash", args: { command: "pwd" } },
  { title: "pwd", output: "/tmp/worktree", metadata: { exit: 0 } },
)
`
	if err := os.WriteFile(harness, []byte(harnessSource), 0o600); err != nil {
		t.Fatal(err)
	}
	// The plugin embeds its collector path at render time; rewrite it to the fake.
	quoted, _ := json.Marshal(collector)
	lines := strings.Split(string(plugin), "\n")
	for index, line := range lines {
		if strings.HasPrefix(line, "const crossingGuardBinary = ") {
			lines[index] = "const crossingGuardBinary = " + string(quoted)
		}
	}
	if err := os.WriteFile(pluginPath, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	return node, harness, pluginPath, collectorLog, collectorArgs, resultLog, diagnosticLog
}

func TestGeneratedOpenCodePluginExecutesBeforeAndAfterCallbacks(t *testing.T) {
	plugin := openCodePlugin("placeholder", openCodeGate{Lane: "collection-only", Interface: "1.18.0",
		Reason: "the installed OpenCode CLI revision could not be measured"})
	node, harness, pluginPath, collectorLog, collectorArgs, resultLog, diagnosticLog :=
		writeOpenCodePluginProbe(t, plugin)
	cmd := exec.Command(node, harness, pluginPath, diagnosticLog, resultLog)
	cmd.Env = append(os.Environ(), "CG_FAKE_LOG="+collectorLog, "CG_FAKE_ARGS="+collectorArgs)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("execute generated plugin: %v\n%s", err, output)
	}
	argsRaw, err := os.ReadFile(collectorArgs)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Split(strings.TrimSpace(string(argsRaw)), "\n"); len(got) != 2 || got[0] != "collect-hook --runtime opencode --carrier" || got[1] != got[0] {
		t.Fatalf("collector argv mismatch: %q", argsRaw)
	}
	payloadRaw, err := os.ReadFile(collectorLog)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(payloadRaw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("collector payload count=%d: %q", len(lines), payloadRaw)
	}
	var before, after map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &after); err != nil {
		t.Fatal(err)
	}
	if before["hook_event_name"] != "PreToolUse" || after["hook_event_name"] != "PostToolUse" ||
		before["session_id"] != "ses_native" || after["call_id"] != "call_native" || before["tool_name"] != "Bash" {
		t.Fatalf("unexpected callback payloads before=%v after=%v", before, after)
	}
	diagnosticRaw, err := os.ReadFile(diagnosticLog)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(diagnosticRaw)); got != `{"body":{"service":"crossing-guard","level":"info","message":"OpenCode collection firing"}}` {
		t.Fatalf("unexpected success diagnostic: %q", got)
	}

	if err := os.WriteFile(diagnosticLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command(node, harness, pluginPath, diagnosticLog, resultLog)
	cmd.Env = append(os.Environ(), "CG_FAKE_LOG="+collectorLog, "CG_FAKE_ARGS="+collectorArgs, "CG_FAKE_EXIT=7")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("collector failure blocked generated plugin: %v\n%s", err, output)
	}
	diagnosticRaw, err = os.ReadFile(diagnosticLog)
	if err != nil {
		t.Fatal(err)
	}
	diagnostics := string(diagnosticRaw)
	for _, want := range []string{"OpenCode collection before.exit", "OpenCode collection after.exit"} {
		if !strings.Contains(diagnostics, want) {
			t.Fatalf("missing bounded failure diagnostic %q in %q", want, diagnostics)
		}
	}
	for _, forbidden := range []string{"pwd", "ses_native", "call_native", "/tmp/worktree"} {
		if strings.Contains(diagnostics, forbidden) {
			t.Fatalf("diagnostic reflected payload %q: %q", forbidden, diagnostics)
		}
	}
}

func TestGeneratedOpenCodeDecisionPluginBlocksOnDenyAndFailsOpen(t *testing.T) {
	plugin := openCodePlugin("placeholder", openCodeGate{Lane: "decision", Installed: "1.18.0", Interface: "1.18.0"})
	node, harness, pluginPath, collectorLog, collectorArgs, resultLog, diagnosticLog :=
		writeOpenCodePluginProbe(t, plugin)

	run := func(extraEnv ...string) {
		t.Helper()
		cmd := exec.Command(node, harness, pluginPath, diagnosticLog, resultLog)
		cmd.Env = append(append(os.Environ(), "CG_FAKE_LOG="+collectorLog, "CG_FAKE_ARGS="+collectorArgs), extraEnv...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("execute decision plugin: %v\n%s", err, output)
		}
	}
	lastResult := func() map[string]any {
		t.Helper()
		raw, err := os.ReadFile(resultLog)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		var result map[string]any
		if err := json.Unmarshal([]byte(lines[len(lines)-1]), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}

	// Deny blocks by throw, carrying the recorded reason to the agent.
	run(`CG_FAKE_DECISION={"decision":"deny","reason":"rule protect-main fired"}`)
	if blocked := lastResult()["blocked"]; !strings.Contains(anyToString(blocked), "Crossing Guard denied this tool call: rule protect-main fired") {
		t.Fatalf("deny did not block: %v", blocked)
	}
	// The before phase spawns govern-opencode alone; collect-hook records only the after phase.
	argsRaw, err := os.ReadFile(collectorArgs)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(argsRaw)), "\n")
	if len(got) != 2 || got[0] != "govern-hook --runtime opencode --carrier" || got[1] != "collect-hook --runtime opencode --carrier" {
		t.Fatalf("decision-lane argv mismatch: %q", argsRaw)
	}

	// Allow proceeds without a throw.
	run(`CG_FAKE_DECISION={"decision":"allow","reason":"no rule matched"}`)
	if blocked := lastResult()["blocked"]; anyToString(blocked) != "" {
		t.Fatalf("allow blocked the call: %v", blocked)
	}

	// An evaluator failure fails OPEN with a visible diagnostic — never a silent block.
	if err := os.WriteFile(diagnosticLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	run("CG_FAKE_GOVERN_EXIT=7")
	if blocked := lastResult()["blocked"]; anyToString(blocked) != "" {
		t.Fatalf("evaluator failure blocked the call: %v", blocked)
	}
	diagnostics, err := os.ReadFile(diagnosticLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(diagnostics), "OpenCode collection govern.exit") {
		t.Fatalf("missing govern failure diagnostic: %q", diagnostics)
	}
}

func anyToString(value any) string {
	text, _ := value.(string)
	return text
}

func TestOpenCodeInstallUpgradesAndUninstallsOwnedV1Plugin(t *testing.T) {
	pinOpenCodeVersion(t, "")
	config := t.TempDir()
	executable := filepath.Join(t.TempDir(), "crossing-guard")
	path := openCodePluginPath(config)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	v1 := []byte(openCodePluginMarkerV1 + "\nconst crossingGuardBinary = " + strconv.Quote(executable) + "\n")
	if err := os.WriteFile(path, v1, 0o600); err != nil {
		t.Fatal(err)
	}
	installer := openCodeInstaller{}
	if err := installer.Install(config, executable); err != nil {
		t.Fatal(err)
	}
	if !installer.IsCurrent(config, executable) {
		t.Fatal("v1 plugin did not upgrade to current bytes")
	}
	backup, err := os.ReadFile(path + ".crossing-guard.bak")
	if err != nil || string(backup) != string(v1) {
		t.Fatalf("v1 recovery backup mismatch err=%v data=%q", err, backup)
	}
	removed, err := installer.Uninstall(config, executable)
	if err != nil || !removed {
		t.Fatalf("upgraded plugin uninstall removed=%t err=%v", removed, err)
	}
}

// The generated plugin is the OpenCode carrier (helper-session-attachment
// plan D5): every lane declares --carrier, captures the binary's stdout, and
// appends handed messages through the session's own API as context only.
func TestGeneratedOpenCodePluginCarriesHelperDeliveries(t *testing.T) {
	for _, lane := range []string{"collection-only", "decision"} {
		plugin := string(openCodePlugin("placeholder", openCodeGate{Lane: lane, Installed: "1.18.0", Interface: "1.18.0"}))
		for _, want := range []string{`"collect-hook", "--runtime", "opencode", "--carrier"`, `stdio: ["pipe", "pipe", "pipe"]`, `maxBuffer: 65536`,
			`deliver(payload.session_id, child.stdout)`, `context.client.session.prompt({`, `noReply: true`, `crossing_guard_deliveries`} {
			if !strings.Contains(plugin, want) {
				t.Fatalf("%s lane plugin lacks %q", lane, want)
			}
		}
		if lane == "decision" {
			for _, want := range []string{`"govern-hook", "--runtime", "opencode", "--carrier"`, `deliver(payload.session_id, decision.crossing_guard_deliveries)`} {
				if !strings.Contains(plugin, want) {
					t.Fatalf("decision lane plugin lacks %q", want)
				}
			}
		}
	}
}
