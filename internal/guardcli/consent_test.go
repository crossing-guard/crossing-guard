package guardcli

// The consent gate's whole risk is that it changes what EnsureHooks does on a
// machine that is ALREADY working. These tests pin both directions: an
// unconsented runtime must never be edited, and a runtime that was governed
// before the record existed must not silently stop being repaired.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// withTempHome points dataDir() (and the vendor config resolvers) at a scratch
// HOME, so a test never reads or writes the developer's real install.
func withTempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func TestConsentRoundTrips(t *testing.T) {
	withTempHome(t)
	if IsConsented("claude") {
		t.Fatal("a fresh machine must consent to nothing")
	}
	if err := RecordConsent("claude", "/cfg/settings.json", "/bin/crossing-guard", false); err != nil {
		t.Fatal(err)
	}
	if !IsConsented("claude") {
		t.Error("recorded consent must be readable")
	}
	if IsConsented("codex") {
		t.Error("consent is PER VENDOR — attaching claude must never imply codex")
	}
	if err := ForgetConsent("claude"); err != nil {
		t.Fatal(err)
	}
	if IsConsented("claude") {
		t.Error("uninstall must forget the yes, so a reinstall asks again")
	}
}

func TestConcurrentConsentUpdatesLoseNoVendor(t *testing.T) {
	withTempHome(t)
	const writers = 24
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			vendor := fmt.Sprintf("vendor-%02d", i)
			if err := RecordConsent(vendor, "/cfg/"+vendor, "/bin/crossing-guard", false); err != nil {
				t.Errorf("RecordConsent(%s): %v", vendor, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if got := len(LoadConsent().Vendors); got != writers {
		t.Fatalf("stored vendors = %d, want %d: a read-modify-write update was lost", got, writers)
	}
}

func TestConsentLockSerializesTransactions(t *testing.T) {
	home := withTempHome(t)
	var active atomic.Int32
	var overlap atomic.Bool
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := withConsentLock(func() error {
				if active.Add(1) != 1 {
					overlap.Store(true)
				}
				time.Sleep(time.Millisecond)
				active.Add(-1)
				return nil
			}); err != nil {
				t.Errorf("withConsentLock: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if overlap.Load() {
		t.Error("two consent transactions entered the critical section together")
	}
	info, err := os.Stat(filepath.Join(home, ".crossing-guard", "attached.json.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("lock mode = %v, want 0600", info.Mode().Perm())
	}
}

// A corrupt record must fail toward asking, never toward "consent to everything".
func TestCorruptConsentRecordIsNotConsent(t *testing.T) {
	home := withTempHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".crossing-guard"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".crossing-guard", "attached.json"),
		[]byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if IsConsented("claude") {
		t.Error("a corrupt record must read as NO consent")
	}
}

// The gate's reason for existing: a runtime the user never said yes to is
// reported and left alone, however obviously installed it is.
func TestEnsureHooksDoesNotAttachAnUnconsentedRuntime(t *testing.T) {
	home := withTempHome(t)
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"model":"opus"}` + "\n")
	if err := os.WriteFile(settings, original, 0o644); err != nil {
		t.Fatal(err)
	}

	var claude HookStatus
	for _, st := range EnsureHooks() {
		if st.Vendor == "claude" {
			claude = st
		}
	}
	if claude.Action != "unconsented" {
		t.Fatalf("action = %q, want \"unconsented\" — an unasked runtime must not be attached", claude.Action)
	}
	got, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Errorf("the config was EDITED without consent:\n%s", got)
	}
	if claude.Manual == "" {
		t.Error("an ungoverned runtime must say how to govern it, or the report is a dead end")
	}
}

// The regression this could have shipped: introducing the gate must not stop the
// daemon repairing a hook it has been maintaining for weeks. An install that
// predates the record is adopted, once, and repaired as normal.
func TestGrandfatheringKeepsAWorkingInstallGoverned(t *testing.T) {
	home := withTempHome(t)
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	// A hook pointing at a DIFFERENT (older) binary path: exactly the install that
	// most needs repairing, and the one an IsCurrent-based check would refuse to
	// adopt.
	prior := `{"hooks":{"PreToolUse":[{"matcher":"*","hooks":[` +
		`{"type":"command","command":"\"/old/path/crossing-guard\" hook"}]}]}}` + "\n"
	if err := os.WriteFile(settings, []byte(prior), 0o644); err != nil {
		t.Fatal(err)
	}

	adopted := GrandfatherExistingAttachments()
	if len(adopted) != 1 || adopted[0] != "claude" {
		t.Fatalf("adopted = %v, want [claude] — a pre-existing attachment is evidence of a past yes", adopted)
	}
	if !IsConsented("claude") {
		t.Fatal("adoption must be durable, not per-process")
	}
	rec := LoadConsent()
	if !rec.Vendors["claude"].Grandfathered {
		t.Error("an INFERRED yes must be marked as such — it is weaker evidence than a typed one")
	}

	// And now the daemon repairs it, as it did before the gate existed.
	var claude HookStatus
	for _, st := range EnsureHooks() {
		if st.Vendor == "claude" {
			claude = st
		}
	}
	if claude.Action != "installed" {
		t.Fatalf("action = %q, want \"installed\" — an adopted runtime must still be repaired", claude.Action)
	}
}

// The historical `cg` spelling must be recognised (name-lint: historical): installs made before the
// rename point at the old symlink, and refusing to recognise our own past work
// would ask a long-time user to consent to what they already consented to.
func TestOurHookIsRecognisedUnderTheOldName(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		want bool
	}{
		{`"/Users/x/go/bin/crossing-guard" hook`, true},
		{`"/Users/x/go/bin/cg" hook`, true}, // name-lint: historical
		{`/usr/local/bin/crossing-guard hook`, true},
		{`"/Users/x/go/bin/crossing-guard" serve`, false},
		{`/usr/bin/someone-elses-tool hook`, false},
		{``, false},
	} {
		if got := isOurHookCommand(tc.cmd); got != tc.want {
			t.Errorf("isOurHookCommand(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}
}

func TestOurHookRecognisesModuleIdentifiedAlternateExecutable(t *testing.T) {
	modulePath := func(path string) string {
		switch path {
		case "/private/tmp/crossing-guard-integrity":
			return crossingGuardCommandModule
		case "/opt/foreign/crossing-guard-helper":
			return "example.com/foreign/cmd/helper"
		default:
			return ""
		}
	}
	for _, tc := range []struct {
		command string
		want    string
	}{
		{`"/private/tmp/crossing-guard-integrity" hook --runtime claude`, "/private/tmp/crossing-guard-integrity"},
		{`"/opt/foreign/crossing-guard-helper" hook --runtime claude`, ""},
		{`"/gone/crossing-guard-integrity" hook --runtime claude`, ""},
		{`"/private/tmp/crossing-guard-integrity" serve`, ""},
	} {
		if got := ourHookBinaryFromCommandWithBuildPath(tc.command, modulePath); got != tc.want {
			t.Errorf("alternate hook binary from %q = %q, want %q", tc.command, got, tc.want)
		}
	}
}

func TestConsentRecordIsOwnerOnly(t *testing.T) {
	home := withTempHome(t)
	if err := RecordConsent("claude", "/cfg", "/bin", false); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(home, ".crossing-guard", "attached.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600 — it records which agents this machine governs", fi.Mode().Perm())
	}
	// And it must be readable as JSON by anything else that needs it (doctor).
	b, err := os.ReadFile(filepath.Join(home, ".crossing-guard", "attached.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rec ConsentRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatalf("record is not valid JSON: %v", err)
	}
}

// The hook command must carry --runtime, because the event log cannot otherwise
// say WHICH agent produced an action: one binary serves every vendor, and a live
// observation arrives with only a runtime session id. This pins the wiring end of
// that (the storage end is store's migration test).
func TestHookCommandCarriesTheRuntime(t *testing.T) {
	cmd := hookCommand("/usr/local/bin/crossing-guard", "claude")
	if !strings.Contains(cmd, "--runtime claude") {
		t.Errorf("hook command %q must name the runtime it was installed for", cmd)
	}
	// ...and the recogniser must still see it as ours. It used to anchor on the
	// command ENDING in "hook", which stopped being true the moment a flag was
	// appended — silently un-recognising every hook we had just written.
	if !isOurHookCommand(cmd) {
		t.Error("adding a flag must not stop us recognising our own hook")
	}
	if flagValue(strings.Fields(cmd)[1:], "--runtime") != "claude" {
		t.Error("the hook must be able to read back the runtime it was installed with")
	}
}

// Uninstall must remove hooks we wrote in the PAST, not only the one this binary
// would write today. A leftover hook pointing at a binary that is gone does not
// fail loudly — it stops guarding, silently, which is the failure this product
// exists to make visible. Bar 2 says uninstall is complete.
func TestUninstallRemovesOlderSpellingsOfOurHook(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	// The pre---runtime spelling, at a stale binary path, under the old name.
	legacy := `{"hooks":{"PreToolUse":[{"matcher":"*","hooks":[` +
		`{"type":"command","command":"\"/old/bin/cg\" hook"}]}]}}` + "\n" // name-lint: historical
	if err := os.WriteFile(settings, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	removed, err := removeOurPreToolUseHook(settings, "/new/bin/crossing-guard")
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("an older spelling of our own hook must still be removable")
	}
	b, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "hook") {
		t.Errorf("our hook survived uninstall:\n%s", b)
	}
}

// A hook whose binary is GONE is the silent failure: the config still looks
// installed, the agent keeps running, and nothing is guarded. It cannot be
// expressed as "attached: true/false", which is why HookBinary returns a path —
// health has to be able to tell a MOVED binary (ordinary) from a MISSING one
// (unguarded).
func TestDetectSeparatesAMovedBinaryFromAMissingOne(t *testing.T) {
	home := withTempHome(t)
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	// A hook pointing at a binary that does not exist.
	gone := `{"hooks":{"PreToolUse":[{"matcher":"*","hooks":[` +
		`{"type":"command","command":"\"/gone/crossing-guard\" hook --runtime claude"}]}]}}` + "\n"
	if err := os.WriteFile(settings, []byte(gone), 0o644); err != nil {
		t.Fatal(err)
	}
	var r Runtime
	for _, d := range DetectRuntimes() {
		if d.Name == "claude" {
			r = d
		}
	}
	if !r.Attached {
		t.Fatal("a hook IS registered — attached must stay true, or the config reads as clean")
	}
	if r.HookBinary != "/gone/crossing-guard" {
		t.Errorf("HookBinary = %q, want the path the config actually invokes", r.HookBinary)
	}
	if r.BinaryPresent {
		t.Error("the binary does not exist — reporting it present is the silent-unguarded failure")
	}

	// Now point it at a real binary that is simply not the one running the check.
	real := filepath.Join(home, "crossing-guard")
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	moved := `{"hooks":{"PreToolUse":[{"matcher":"*","hooks":[` +
		`{"type":"command","command":"\"` + real + `\" hook --runtime claude"}]}]}}` + "\n"
	if err := os.WriteFile(settings, []byte(moved), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, d := range DetectRuntimes() {
		if d.Name == "claude" {
			r = d
		}
	}
	if !r.BinaryPresent {
		t.Error("a different-but-present binary is another install, not a fault")
	}
	if r.Current {
		t.Error("Current must still mean THIS binary — it is what init uses to decide on repair")
	}
}
