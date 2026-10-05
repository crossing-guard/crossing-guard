package main

// The first doctor shipped with zero tests and three ladder bugs the review had
// to find by reading: a missing rung (consented-but-detached fell through to
// "ENFORCING" off stale events), a garbled status when HookBinary was empty, and
// severity ordered below verifiability. The ladder is pure decision logic over a
// Runtime struct — exactly what a table test pins for free.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"crossing-guard/internal/guardcli"
)

// ladderFor drives runtimesFromConfig with one synthetic runtime by pointing
// detection at a scratch HOME holding exactly the config the case needs.
// DetectRuntimes reads real vendor resolvers, so the table builds real states
// (claude only — one vendor is enough to pin ordering). Returns the HOME so a
// case can drop a real binary into it (the hook recognizer matches on OUR
// basename, so a fixture cannot borrow /bin/sh as a stand-in).
func ladderFor(t *testing.T, settings func(home string) string, consent bool, evidence func(string) (int, int64)) runtimeHealth {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	// These cases require an installed client independently of any host tools.
	// Detection checks executable presence without running this inert fixture.
	if err := os.WriteFile(filepath.Join(home, "claude"), []byte("#!/bin/sh\nexit 97\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", home)
	// A present binary with our name, for fixtures that want BinaryPresent=true.
	if err := os.WriteFile(filepath.Join(home, "crossing-guard"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if body := settings(home); body != "" {
		p := filepath.Join(home, ".claude", "settings.json")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if consent {
		if err := guardcli.RecordConsent("claude", filepath.Join(home, ".claude", "settings.json"),
			"/bin/crossing-guard", false); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range runtimesFromConfig(evidence) {
		if h.Name == "claude" {
			return h
		}
	}
	t.Fatal("claude missing from the ladder")
	return runtimeHealth{}
}

func hookJSON(bin string) string {
	return `{"hooks":{"PreToolUse":[{"matcher":"*","hooks":[` +
		`{"type":"command","command":"\"` + bin + `\" hook --runtime claude"}]}]}}`
}

func TestLadderConsentedButDetachedNeverReadsEnforcing(t *testing.T) {
	// Consent recorded, hook GONE from the config, and stale historical events in
	// the log: the state that used to fall all the way through to "ENFORCING —
	// proven by events" plus a garbled "(via , …)". Past events prove only that it
	// used to work.
	h := ladderFor(t, func(string) string { return `{"model":"opus"}` }, true,
		func(string) (int, int64) { return 500, 1 })
	if !strings.Contains(h.Status, "GONE") {
		t.Errorf("status = %q, want the hook-gone diagnosis", h.Status)
	}
	if strings.Contains(h.Status, "ENFORCING") {
		t.Errorf("status = %q — stale events must never outrank a missing hook", h.Status)
	}
	if strings.Contains(h.Status, "(via ,") {
		t.Errorf("status = %q — the garbled empty-binary suffix is back", h.Status)
	}
}

func TestLadderMissingBinaryOutranksNoDaemon(t *testing.T) {
	// The hook's binary does not exist AND the daemon is down (evidence nil).
	// Whether the binary exists is a filesystem fact needing no daemon; ordering
	// it below "cannot verify" hid the one diagnosis that needs no verifying.
	h := ladderFor(t, func(string) string { return hookJSON("/gone/crossing-guard") }, true, nil)
	if !strings.Contains(h.Status, "BROKEN") {
		t.Errorf("status = %q, want BROKEN — a filesystem fact must not hide behind a daemon outage", h.Status)
	}
}

func TestLadderNeverClaimsEnforcingWithoutEvidence(t *testing.T) {
	// The hook's binary exists (ladderFor plants one named like ours); daemon down.
	// The builder receives the SCRATCH home — reading $HOME at call time raced the
	// fixture and pointed the hook at the developer's real home.
	h := ladderFor(t, func(home string) string { return hookJSON(home + "/crossing-guard") }, true, nil)
	if strings.Contains(h.Status, "ENFORCING") {
		t.Errorf("status = %q — no daemon means no evidence, and no evidence must never read as enforcing", h.Status)
	}
	if !strings.Contains(h.Status, "cannot verify") {
		t.Errorf("status = %q, want the honest cannot-verify wording", h.Status)
	}
}

func TestLadderEnforcingRequiresEvents(t *testing.T) {
	zero := func(string) (int, int64) { return 0, 0 }
	h := ladderFor(t, func(home string) string { return hookJSON(home + "/crossing-guard") }, true, zero)
	if !strings.Contains(h.Status, "NEVER VERIFIED") {
		t.Errorf("status = %q, want NEVER VERIFIED at zero events", h.Status)
	}
	some := func(string) (int, int64) { return 3, 1 }
	h = ladderFor(t, func(home string) string { return hookJSON(home + "/crossing-guard") }, true, some)
	if !strings.Contains(h.Status, "ENFORCING") {
		t.Errorf("status = %q, want ENFORCING once events exist", h.Status)
	}
}

func TestEphemeralBinaryFlagsTempAndDownloads(t *testing.T) {
	// Classification is lexical; this fictional home need not exist.
	t.Setenv("HOME", "/Users/crossingguard-test")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/tmp/x/crossing-guard", true},
		{"/private/var/folders/ab/scratch/cgtest", true},
		{filepath.Join(home, "Downloads", "crossing-guard"), true},
		{"/usr/local/bin/crossing-guard", false},
		{filepath.Join(home, "go", "bin", "crossing-guard"), false},
	} {
		got, _ := ephemeralBinary(tc.path)
		if got != tc.want {
			t.Errorf("ephemeralBinary(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestHumanAge(t *testing.T) {
	for _, tc := range []struct {
		sec  int64
		want string
	}{{-5, "0s"}, {30, "30s"}, {90, "1m"}, {7200, "2h"}, {172800, "2d"}} {
		if got := humanAge(tc.sec); got != tc.want {
			t.Errorf("humanAge(%d) = %q, want %q", tc.sec, got, tc.want)
		}
	}
}
