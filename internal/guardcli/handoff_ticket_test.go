package guardcli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"crossing-guard/internal/observation"
)

// §6.4: the lifecycle hook copies CG_HANDOFF_TICKET, when present, into the session
// entry it already posts — one optional field, absent from the entry's JSON when the
// process carried none, so a session the person started in a terminal claims nothing.
func TestSessionEntryCopiesTheHandoffTicketWhenPresent(t *testing.T) {
	in := hookInput{Runtime: "codex", SessionID: "native", HookEventName: "SessionStart", Source: "startup", Cwd: "/repo"}
	t.Setenv(observation.HandoffTicketEnv, "")
	plain, err := buildSessionEntryEnvelope(in)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(plain)
	if plain.HandoffTicket != "" || strings.Contains(string(raw), "handoff_ticket") {
		t.Fatalf("an entry with no ticket carries no field: %s", raw)
	}

	t.Setenv(observation.HandoffTicketEnv, "tkt_01JB9ZK6M3Q0V7W8X9Y0Z1A2B3")
	ticketed, err := buildSessionEntryEnvelope(in)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(ticketed)
	if ticketed.HandoffTicket != "tkt_01JB9ZK6M3Q0V7W8X9Y0Z1A2B3" || !strings.Contains(string(raw), `"handoff_ticket":"tkt_01JB9ZK6M3Q0V7W8X9Y0Z1A2B3"`) {
		t.Fatalf("the ticket is copied as it is: %s", raw)
	}

	t.Setenv(observation.HandoffTicketEnv, strings.Repeat("x", observation.MaxHandoffTicketBytes+1))
	if oversize, _ := buildSessionEntryEnvelope(in); oversize.HandoffTicket != "" {
		t.Fatal("a value longer than a ticket id is not copied")
	}
}

// §6.4 rule 4: a session run with CG_OBSERVE=0 posts no entry and no prompt row, so it
// claims nothing and receives nothing — even when its process carries a ticket.
func TestObserveOffPostsNoEntryAndClaimsNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var requests atomic.Int32
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer daemon.Close()
	t.Setenv("CG_GOVERN", strings.TrimPrefix(daemon.URL, "http://"))
	t.Setenv(observation.HandoffTicketEnv, "tkt_01JB9ZK6M3Q0V7W8X9Y0Z1A2B3")
	t.Setenv("CG_OBSERVE", "0")

	in := hookInput{Runtime: "claude", SessionID: "native", HookEventName: "SessionStart", Source: "startup", Cwd: home, Carrier: true}
	emitSessionEntry(in)
	if deliveries := emitSessionTurn(in, "turn.started"); len(deliveries) != 0 {
		t.Fatalf("a session with observation off receives nothing: %+v", deliveries)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("a session with observation off posted %d requests", got)
	}
	spooled, _ := filepath.Glob(filepath.Join(home, ".crossing-guard", "*", "*"))
	for _, path := range spooled {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			t.Fatalf("a session with observation off spooled %s", path)
		}
	}
}

// Criterion 72: neither hook's installed command line changed. The lifecycle hook's is
// pinned here, byte for byte; the memory hook's is pinned in internal/memcli.
func TestLifecycleHookInstalledCommandLinesAreUnchanged(t *testing.T) {
	exe := "/opt/crossing-guard/crossing-guard"
	for event, want := range map[string]string{
		"SessionStart":     `"/opt/crossing-guard/crossing-guard" hook --runtime claude`,
		"PreToolUse":       `"/opt/crossing-guard/crossing-guard" hook --runtime claude`,
		"UserPromptSubmit": `"/opt/crossing-guard/crossing-guard" hook --runtime claude --observe turn.started`,
	} {
		if got := claudeHookCommandFor(exe, event); got != want {
			t.Fatalf("claude %s: %q, want %q", event, got, want)
		}
	}
	for event, want := range map[string]string{
		"SessionStart":     `"/opt/crossing-guard/crossing-guard" hook --runtime codex`,
		"PreToolUse":       `"/opt/crossing-guard/crossing-guard" hook --runtime codex`,
		"UserPromptSubmit": `"/opt/crossing-guard/crossing-guard" hook --runtime codex --observe turn.started`,
	} {
		if got := codexHookCommandFor(exe, event); got != want {
			t.Fatalf("codex %s: %q, want %q", event, got, want)
		}
	}
}

// OD-24: a daemon may edit a runtime's settings for another feature only when that
// home's lifecycle-hook entry for the runtime names its own executable.
func TestLifecycleHookOwnerNamesTheInstallation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	ForgetResolvedConfigs()
	t.Cleanup(ForgetResolvedConfigs)
	self := filepath.Join(home, "bin", "crossing-guard")
	if err := os.MkdirAll(filepath.Dir(self), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(self, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if owner, _ := LifecycleHookOwner(codexVendor, self); owner != "none" {
		t.Fatalf("a home with no hook entry is nobody's: %s", owner)
	}
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := (codexInstaller{}).Install(filepath.Join(home, ".codex"), self); err != nil {
		t.Fatal(err)
	}
	if owner, binary := LifecycleHookOwner(codexVendor, self); owner != "self" || binary != self {
		t.Fatalf("the hook names this executable: %s %s", owner, binary)
	}
	other := filepath.Join(home, "bin", "development-build")
	if err := os.WriteFile(other, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if owner, _ := LifecycleHookOwner(codexVendor, other); owner != "foreign" {
		t.Fatalf("another build pointed at this home does not own it: %s", owner)
	}
	if owner, _ := LifecycleHookOwner("no-such-runtime", self); owner != "none" {
		t.Fatalf("an unknown runtime: %s", owner)
	}
}
