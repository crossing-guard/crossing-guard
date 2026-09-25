package guardcli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withDetectedCursor(t *testing.T) string {
	t.Helper()
	home := withTempHome(t)
	oldLookPath, oldStat := cursorClientLookPath, cursorClientStat
	t.Cleanup(func() { cursorClientLookPath, cursorClientStat = oldLookPath, oldStat })
	cursorClientLookPath = func(binary string) (string, error) {
		if binary == "cursor" {
			return "/Applications/Cursor.app/Contents/Resources/app/bin/cursor", nil
		}
		return "", errors.New("absent")
	}
	cursorClientStat = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	return home
}

func connectionErrorKind(err error) string {
	var connectionErr *RuntimeConnectionError
	if errors.As(err, &connectionErr) {
		return connectionErr.Kind
	}
	return ""
}

func TestCursorConnectionPreviewIsReadOnlyAndProviderOwned(t *testing.T) {
	home := withDetectedCursor(t)
	preview, err := PreviewRuntimeConnection(cursorVendor, ConnectionOperationConnect, "/opt/crossing-guard")
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(home, ".cursor", "hooks.json")
	if preview.ConfigPath != wantPath || preview.DisplayName != "Cursor" || !preview.WillCreateFile || !preview.WillChange {
		t.Fatalf("preview = %+v", preview)
	}
	if preview.StateDigest == "" || len(preview.HookPhases) != len(cursorLifecycleHookEvents) {
		t.Fatalf("preview lacks server state/phases: %+v", preview)
	}
	if _, err := os.Stat(filepath.Join(home, ".cursor")); !os.IsNotExist(err) {
		t.Fatalf("preview created Cursor state: %v", err)
	}
	statuses := RuntimeConnections("/opt/crossing-guard")
	if len(statuses) != 1 || statuses[0].Descriptor.Runtime != cursorVendor || statuses[0].State != "detected" {
		t.Fatalf("connection statuses = %+v", statuses)
	}
	statuses[0].Descriptor.Limitations[0] = "mutated"
	again := RuntimeConnections("/opt/crossing-guard")
	if again[0].Descriptor.Limitations[0] == "mutated" {
		t.Fatal("descriptor slices were not defensively copied")
	}
}

func TestCursorConnectionConnectAndDisconnectRoundTrip(t *testing.T) {
	home := withDetectedCursor(t)
	path := filepath.Join(home, ".cursor", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := cursorSeed(`"/old/crossing-guard" hook --runtime cursor`)
	if err := os.WriteFile(path, seed, 0o640); err != nil {
		t.Fatal(err)
	}
	connectPreview, err := PreviewRuntimeConnection(cursorVendor, ConnectionOperationConnect, "/opt/crossing-guard")
	if err != nil {
		t.Fatal(err)
	}
	if connectPreview.ForeignHandlersPreserved != 3 || !connectPreview.WillCreateBackup {
		t.Fatalf("connect preview = %+v", connectPreview)
	}
	connected, err := ConnectRuntime(cursorVendor, "/opt/crossing-guard", connectPreview.StateDigest)
	if err != nil {
		t.Fatal(err)
	}
	if !connected.ConfigChanged || !connected.ConsentChanged || !IsConsented(cursorVendor) {
		t.Fatalf("connect result = %+v consent=%t", connected, IsConsented(cursorVendor))
	}
	if !(cursorInstaller{}).IsCurrent(path, "/opt/crossing-guard") {
		t.Fatal("connected hooks are not current")
	}
	if got, _ := os.ReadFile(path + ".crossing-guard.bak"); string(got) != string(seed) {
		t.Fatalf("connect backup mismatch: %q", got)
	}

	disconnectPreview, err := PreviewRuntimeConnection(cursorVendor, ConnectionOperationDisconnect, "/opt/crossing-guard")
	if err != nil {
		t.Fatal(err)
	}
	if disconnectPreview.OwnedHandlersRemoved != len(cursorLifecycleHookEvents) || !disconnectPreview.ConsentPresent {
		t.Fatalf("disconnect preview = %+v", disconnectPreview)
	}
	disconnected, err := DisconnectRuntime(cursorVendor, "/opt/crossing-guard", disconnectPreview.StateDigest)
	if err != nil {
		t.Fatal(err)
	}
	if !disconnected.ConfigChanged || !disconnected.ConsentChanged || IsConsented(cursorVendor) {
		t.Fatalf("disconnect result = %+v consent=%t", disconnected, IsConsented(cursorVendor))
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "crossing-guard") {
		t.Fatalf("owned hook survived disconnect: %s", raw)
	}
	for _, foreign := range []string{"/foreign/pre", "/foreign/post", "/foreign/workspace"} {
		if !strings.Contains(string(raw), foreign) {
			t.Fatalf("disconnect lost foreign handler %q: %s", foreign, raw)
		}
	}
}

func TestCursorCurrentHookWithoutConsentNeedsAttention(t *testing.T) {
	home := withDetectedCursor(t)
	path := filepath.Join(home, ".cursor", "hooks.json")
	if err := (cursorInstaller{}).Install(path, "/opt/crossing-guard"); err != nil {
		t.Fatal(err)
	}
	status, err := RuntimeConnection(cursorVendor, "/opt/crossing-guard")
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "needs_attention" || status.Consented || !status.Current {
		t.Fatalf("unconsented current hook status = %+v", status)
	}
}

func TestCursorConnectionRejectsChangedPreviewWithoutOwnedWrite(t *testing.T) {
	home := withDetectedCursor(t)
	preview, err := PreviewRuntimeConnection(cursorVendor, ConnectionOperationConnect, "/opt/crossing-guard")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".cursor", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := []byte(`{"version":1,"foreign":"arrived-after-preview"}`)
	if err := os.WriteFile(path, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectRuntime(cursorVendor, "/opt/crossing-guard", preview.StateDigest); connectionErrorKind(err) != "preview_changed" {
		t.Fatalf("connect error = %v kind=%q", err, connectionErrorKind(err))
	}
	if got, _ := os.ReadFile(path); string(got) != string(foreign) {
		t.Fatalf("stale preview changed config: %s", got)
	}
	if IsConsented(cursorVendor) {
		t.Fatal("stale preview recorded consent")
	}
}

func TestCursorConnectionCompensatesConsentFailure(t *testing.T) {
	home := withDetectedCursor(t)
	oldRecord := recordRuntimeConnectionConsent
	recordRuntimeConnectionConsent = func(string, string, string, bool) error { return errors.New("disk unavailable") }
	t.Cleanup(func() { recordRuntimeConnectionConsent = oldRecord })

	preview, err := PreviewRuntimeConnection(cursorVendor, ConnectionOperationConnect, "/opt/crossing-guard")
	if err != nil {
		t.Fatal(err)
	}
	result, err := ConnectRuntime(cursorVendor, "/opt/crossing-guard", preview.StateDigest)
	if connectionErrorKind(err) != "consent_failed" || result.ResidualHook {
		t.Fatalf("connect result=%+v err=%v kind=%q", result, err, connectionErrorKind(err))
	}
	if IsConsented(cursorVendor) {
		t.Fatal("failed consent was recorded")
	}
	path := filepath.Join(home, ".cursor", "hooks.json")
	config := readCursorConfigForTest(t, path)
	owned, _ := cursorHandlerCounts(config)
	if owned != 0 {
		t.Fatalf("compensation left %d owned hooks: %+v", owned, config)
	}
}

func TestCursorDisconnectStopsBeforeConfigMutationWhenConsentRemovalFails(t *testing.T) {
	home := withDetectedCursor(t)
	path := filepath.Join(home, ".cursor", "hooks.json")
	if err := (cursorInstaller{}).Install(path, "/opt/crossing-guard"); err != nil {
		t.Fatal(err)
	}
	if err := RecordConsent(cursorVendor, path, "/opt/crossing-guard", false); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	oldForget := forgetRuntimeConnectionConsent
	forgetRuntimeConnectionConsent = func(string) error { return errors.New("locked") }
	t.Cleanup(func() { forgetRuntimeConnectionConsent = oldForget })

	preview, err := PreviewRuntimeConnection(cursorVendor, ConnectionOperationDisconnect, "/opt/crossing-guard")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DisconnectRuntime(cursorVendor, "/opt/crossing-guard", preview.StateDigest); connectionErrorKind(err) != "consent_failed" {
		t.Fatalf("disconnect error = %v kind=%q", err, connectionErrorKind(err))
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) || !IsConsented(cursorVendor) {
		t.Fatalf("failed consent removal changed state: consent=%t before=%q after=%q", IsConsented(cursorVendor), before, after)
	}
}

func TestCursorConnectionUnsafeBinaryAndMalformedConfigAreReadOnly(t *testing.T) {
	home := withDetectedCursor(t)
	path := filepath.Join(home, ".cursor", "hooks.json")
	if _, err := PreviewRuntimeConnection(cursorVendor, ConnectionOperationConnect,
		filepath.Join(os.TempDir(), "crossing-guard")); connectionErrorKind(err) != "unsafe_binary" {
		t.Fatalf("unsafe binary error = %v kind=%q", err, connectionErrorKind(err))
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("unsafe preview created Cursor path: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	broken := []byte(`{"version":2,"secret":"do-not-return"}`)
	if err := os.WriteFile(path, broken, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := PreviewRuntimeConnection(cursorVendor, ConnectionOperationConnect, "/opt/crossing-guard")
	if connectionErrorKind(err) != "preview_blocked" || strings.Contains(err.Error(), "do-not-return") {
		t.Fatalf("malformed preview error = %v kind=%q", err, connectionErrorKind(err))
	}
	if got, _ := os.ReadFile(path); string(got) != string(broken) {
		t.Fatalf("malformed preview changed config: %s", got)
	}
	if _, err := os.Stat(path + ".crossing-guard.bak"); !os.IsNotExist(err) {
		t.Fatalf("malformed preview created backup: %v", err)
	}
}

func TestRuntimeConnectionPreviewJSONOmitsStateAndConfigBytes(t *testing.T) {
	withDetectedCursor(t)
	preview, err := PreviewRuntimeConnection(cursorVendor, ConnectionOperationConnect, "/opt/crossing-guard")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(preview)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), preview.StateDigest) || strings.Contains(string(raw), "snapshot") {
		t.Fatalf("server-only preview state leaked: %s", raw)
	}
}
