package guardcli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAntigravityInstallPreservesRepairsAndRemoves(t *testing.T) {
	home := withTempHome(t)
	self := fakeSelf(t)
	installer := antigravityInstaller{}
	path := filepath.Join(home, ".gemini", "config", "hooks.json")
	before := `{"foreign":{"Stop":[{"command":"echo foreign"}]}}`
	writeFile(t, path, before)
	if err := InstallFor(antigravityVendor, path, self); err != nil {
		t.Fatal(err)
	}
	if !installer.IsCurrent(path, self) || installer.HookBinary(path) != self {
		t.Fatal("installation not current")
	}
	if readFile(t, path+".crossing-guard.bak") != before {
		t.Fatal("prior config backup changed")
	}
	installed := readFile(t, path)
	if !strings.Contains(installed, "foreign") {
		t.Fatal("foreign config lost")
	}
	if err := installer.Install(path, self); err != nil {
		t.Fatal(err)
	}
	if installed != readFile(t, path) || readFile(t, path+".crossing-guard.bak") != before {
		t.Fatal("idempotent install wrote config or backup")
	}
	for phase, current := range installer.HookPhaseStatus(path, self) {
		if !current {
			t.Errorf("phase %s not current", phase)
		}
	}
	old := strings.ReplaceAll(installed, self, "/missing/crossing-guard")
	writeFile(t, path, old)
	if installer.IsCurrent(path, self) {
		t.Fatal("stale executable called current")
	}
	if err := installer.Install(path, self); err != nil {
		t.Fatal(err)
	}
	if !installer.IsCurrent(path, self) {
		t.Fatal("repair failed")
	}
	if removed, err := installer.Uninstall(path, self); err != nil || !removed {
		t.Fatalf("uninstall: %v %v", removed, err)
	}
	_, hooks, err := readAntigravityHooks(path)
	if err != nil || hooks["foreign"] == nil || hooks[antigravityHookGroup] != nil {
		t.Fatalf("foreign preservation: %s %v", readFile(t, path), err)
	}
	if removed, err := installer.Uninstall(path, self); err != nil || removed {
		t.Fatalf("second uninstall: %v %v", removed, err)
	}
}

func TestAntigravityRefusesForeignAndMalformedSettings(t *testing.T) {
	for _, body := range []string{"null", "[]", "", "{", `{"crossing-guard":null}`, `{"crossing-guard":[]}`, `{"crossing-guard":{"Stop":[{"command":"echo foreign"}]}}`} {
		t.Run(body, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			writeFile(t, path, body)
			installer := antigravityInstaller{}
			if err := installer.Install(path, "/bin/crossing-guard"); err == nil {
				t.Fatal("expected refusal")
			}
			if removed, err := installer.Uninstall(path, "/bin/crossing-guard"); err == nil || removed {
				t.Fatal("expected uninstall refusal")
			}
			if got := readFile(t, path); got != body {
				t.Fatal("failed operation modified config")
			}
			if _, err := os.Stat(path + ".crossing-guard.bak"); !os.IsNotExist(err) {
				t.Fatal("failed operation wrote backup")
			}
		})
	}
}

func TestAntigravityNativeHookProjection(t *testing.T) {
	installer := antigravityInstaller{}
	raw := `{"conversationId":"session-a","workspacePaths":["/work"],"transcriptPath":"/home/.gemini/antigravity-cli/brain/session-a/.system_generated/logs/transcript_full.jsonl","stepIdx":0,"toolCall":{"name":"run_command","args":{"CommandLine":"cat input.txt","Cwd":"/work","vendor_future":true}},"error":""}`
	in, err := installer.DecodeHookInput(strings.NewReader(raw), "PreToolUse")
	if err != nil {
		t.Fatal(err)
	}
	if in.SessionID != "session-a" || in.CallID != "step:0" || in.Cwd != "/work" || in.ToolName != "Bash" || string(in.ToolInput.Command) != "cat input.txt" {
		t.Fatalf("projection: %+v", in)
	}
	if !bytes.Contains(in.RawToolInput, []byte(`"vendor_future":true`)) {
		t.Fatal("raw vendor argument lost")
	}
	result, err := installer.DecodeHookInput(strings.NewReader(strings.Replace(raw, `"error":""`, `"error":"exit status 1"`, 1)), "PostToolUse")
	if err != nil || !result.ToolIsError || result.CallID != in.CallID || len(result.RawToolResponse) != 0 {
		t.Fatalf("result: %+v %v", result, err)
	}
	for _, event := range []string{"PreInvocation", "SessionStart", ""} {
		if _, err := installer.DecodeHookInput(strings.NewReader(raw), event); err == nil {
			t.Errorf("accepted unsupported event %q", event)
		}
	}
	for _, body := range []string{`{}`, strings.Replace(raw, `"stepIdx":0,`, "", 1), strings.Replace(raw, `"stepIdx":0`, `"stepIdx":-1`, 1), strings.Replace(raw, `"conversationId":"session-a"`, `"conversationId":""`, 1), raw + "{}"} {
		if _, err := installer.DecodeHookInput(strings.NewReader(body), "PreToolUse"); err == nil {
			t.Errorf("accepted invalid payload: %s", body)
		}
	}
	if _, err := installer.DecodeHookInput(strings.NewReader(strings.Repeat(" ", antigravityMaxHookBytes+1)), "PreToolUse"); err == nil {
		t.Fatal("unbounded payload")
	}
}

func TestAntigravityStopAndWorkspaceAreNotGuessed(t *testing.T) {
	installer := antigravityInstaller{}
	for _, idle := range []bool{true, false} {
		payload, _ := json.Marshal(map[string]any{"conversationId": "a", "transcriptPath": "/home/.gemini/antigravity-cli/brain/a/.system_generated/logs/transcript_full.jsonl", "workspacePaths": []string{"/a", "/b"}, "fullyIdle": idle})
		in, err := installer.DecodeHookInput(bytes.NewReader(payload), "Stop")
		if err != nil || in.Cwd != "" || in.HookEventName != "Stop" || (in.TurnKind == "turn.ended") != idle {
			t.Fatalf("stop: %+v %v", in, err)
		}
		if idle {
			turn, err := buildSessionTurnEnvelope(in, in.TurnKind)
			if err != nil || turn.NativeSource != "Stop" {
				t.Fatalf("native Stop did not enter the shared turn contract: %+v %v", turn, err)
			}
		}
	}
	deny := installer.EncodeHookDeny("PreToolUse", "fixture denied")
	var response map[string]string
	if json.Unmarshal(deny, &response) != nil || response["decision"] != "deny" || response["reason"] != "fixture denied" {
		t.Fatalf("deny: %s", deny)
	}
	if installer.HookAskBudget() != 0 {
		t.Fatal("unmeasured approval budget")
	}
	if _, ok := any(installer).(HookContextEncoder); ok {
		t.Fatal("unmeasured context injection")
	}
}

func TestAntigravityToolMappingRetainsUnknowns(t *testing.T) {
	cases := []struct{ name, raw, want, path string }{
		{"view_file", `{"AbsolutePath":"/a"}`, "Read", "/a"},
		{"write_to_file", `{"TargetFile":"/a","CodeContent":"body"}`, "Write", "/a"},
		{"replace_file_content", `{"TargetFile":"/a","TargetContent":"old","ReplacementContent":"new"}`, "Edit", "/a"},
		{"multi_replace_file_content", `{"TargetFile":"/a"}`, "multi_replace_file_content", ""},
		{"future_tool", `{"future":true}`, "future_tool", ""},
	}
	for _, tc := range cases {
		name, input, err := antigravityToolInput(tc.name, json.RawMessage(tc.raw))
		if err != nil || name != tc.want || input.FilePath != tc.path {
			t.Errorf("%s: %s %+v %v", tc.name, name, input, err)
		}
	}
}

func TestAntigravityRecallUsesExistingConsentAndSharedConfig(t *testing.T) {
	home := withTempHome(t)
	self := fakeSelf(t)
	installer := antigravityInstaller{}
	settings := filepath.Join(home, ".gemini", "config", "hooks.json")
	path := installer.RecallConfig(settings)
	if path != filepath.Join(home, ".gemini", "config", "mcp_config.json") {
		t.Fatal(path)
	}
	writeFile(t, path, `{"mcpServers":{"other":{"command":"foreign"}},"note":"preserve"}`)
	if err := RegisterRecallFor(antigravityVendor, settings, self); err == nil {
		t.Fatal("recall without consent")
	}
	if err := RecordConsent(antigravityVendor, settings, self, false); err != nil {
		t.Fatal(err)
	}
	if err := RegisterRecallFor(antigravityVendor, settings, self); err != nil {
		t.Fatal(err)
	}
	if state, err := installer.RecallStatus(settings, self); err != nil || state != RecallCurrent {
		t.Fatalf("state %s %v", state, err)
	}
	if !strings.Contains(readFile(t, path), "other") || !strings.Contains(readFile(t, path), "preserve") {
		t.Fatal("foreign fields lost")
	}
	if removed, err := installer.UnregisterRecall(settings, self); err != nil || !removed {
		t.Fatalf("remove %v %v", removed, err)
	}
	foreign := `{"mcpServers":{"crossing-guard":{"command":"foreign","args":["serve"]}}}`
	writeFile(t, path, foreign)
	if err := installer.RegisterRecall(settings, self); err == nil {
		t.Fatal("foreign overwritten")
	}
	if removed, err := installer.UnregisterRecall(settings, self); err != nil || removed {
		t.Fatal("foreign removed")
	}
	if readFile(t, path) != foreign {
		t.Fatal("foreign mutated")
	}
}

func TestAntigravitySharedHooksIgnoreOtherSurfaces(t *testing.T) {
	for _, path := range []string{"", "/tmp/transcript.jsonl", "/home/.gemini/antigravity/brain/a/.system_generated/logs/transcript_full.jsonl", "/home/.gemini/antigravity-ide/brain/a/.system_generated/logs/transcript_full.jsonl", "/home/.gemini/antigravity-cli/brain/another/.system_generated/logs/transcript_full.jsonl"} {
		raw, _ := json.Marshal(map[string]any{"conversationId": "a", "transcriptPath": path, "fullyIdle": true})
		in, err := (antigravityInstaller{}).DecodeHookInput(bytes.NewReader(raw), "Stop")
		if err != nil || in.HookEventName != "" || in.TurnKind != "" || in.SessionID != "" {
			t.Fatalf("non-CLI event entered lane: %+v %v", in, err)
		}
	}
}
