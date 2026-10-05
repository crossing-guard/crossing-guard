package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"crossing-guard/internal/guardcli"
	"crossing-guard/internal/memcli"
)

// recallRig is a temp home — never the real one — with a Claude settings file. owner
// is the executable the home's lifecycle hook names: this test binary (the daemon owns
// the installation), another path (a development build pointed at someone's home), or
// "" (no lifecycle hook at all).
type recallRig struct {
	t        *testing.T
	mux      *http.ServeMux
	home     string
	settings string
	self     string
}

func newRecallRig(t *testing.T, owner string) *recallRig {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CG_MEMORY_DIR", filepath.Join(home, ".crossing-guard", "memory"))
	t.Setenv("CPMEM_DIR", "")
	guardcli.ForgetResolvedConfigs()
	t.Cleanup(guardcli.ForgetResolvedConfigs)
	// The daemon's executable for the test: a file named as the product is, so the
	// installers recognise a hook that names it.
	self := filepath.Join(home, "bin", "crossing-guard")
	if err := os.MkdirAll(filepath.Dir(self), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(self, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	previous := daemonExecutable
	daemonExecutable = func() (string, error) { return self, nil }
	t.Cleanup(func() { daemonExecutable = previous })
	rig := &recallRig{t: t, mux: http.NewServeMux(), home: home, settings: filepath.Join(home, ".claude", "settings.json"), self: self}
	if err := os.MkdirAll(filepath.Dir(rig.settings), 0o700); err != nil {
		t.Fatal(err)
	}
	switch owner {
	case "":
		if err := os.WriteFile(rig.settings, []byte("{\"model\":\"opus\"}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "self":
		owner = self
		fallthrough
	default:
		if owner != self {
			if err := os.MkdirAll(filepath.Dir(owner), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(owner, []byte("#!/bin/sh\n"), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if err := guardcli.InstallFor("claude", rig.settings, owner); err != nil {
			t.Fatal(err)
		}
	}
	registerMemoryRecallRoutes(rig.mux)
	return rig
}

func (r *recallRig) post(req memoryRecallRequest) (int, memoryRecallResponse, teamHandoffError) {
	r.t.Helper()
	body, _ := json.Marshal(req)
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/memory/attach", bytes.NewReader(body)))
	var out memoryRecallResponse
	var refusal teamHandoffError
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			r.t.Fatalf("response: %v: %s", err, rec.Body.String())
		}
	} else {
		_ = json.Unmarshal(rec.Body.Bytes(), &refusal)
	}
	return rec.Code, out, refusal
}

func (r *recallRig) state(runtime string) teamHandoffRecall {
	r.t.Helper()
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/memory/attach", nil))
	var out memoryRecallStateResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		r.t.Fatalf("state: %d %s", rec.Code, rec.Body.String())
	}
	for _, state := range out.Runtimes {
		if state.Runtime == runtime {
			return state
		}
	}
	r.t.Fatalf("no state for %s: %+v", runtime, out)
	return teamHandoffRecall{}
}

func (r *recallRig) file() string {
	r.t.Helper()
	raw, err := os.ReadFile(r.settings)
	if err != nil {
		r.t.Fatal(err)
	}
	return string(raw)
}

// Criteria 87 and 92 (OD-22): on a device whose Claude settings have no memory hook the
// action is offered; declining — a request without consent — leaves the settings file
// byte-identical and recall still off; consenting installs the existing memory hook
// for this daemon's own executable; Turn off removes exactly that entry.
func TestMemoryRecallActionNeedsConsentAndHasAWayBack(t *testing.T) {
	rig := newRecallRig(t, "self")
	before := rig.file()
	off := rig.state("claude")
	if off.EntryPresent || off.ObservedInjecting || off.AddedByAction || len(off.Offers) != 1 || off.Offers[0] != memoryRecallAttach || off.WithheldCode != "" {
		t.Fatalf("recall is off and Turn on is offered: %+v", off)
	}

	code, _, refusal := rig.post(memoryRecallRequest{Runtime: "claude", Action: memoryRecallAttach})
	if code != http.StatusBadRequest || refusal.Code != memoryRecallConsentRequired {
		t.Fatalf("without consent: %d %+v", code, refusal)
	}
	if got := rig.file(); got != before {
		t.Fatalf("declining leaves the settings file byte-identical:\n%s", got)
	}
	if still := rig.state("claude"); still.EntryPresent {
		t.Fatalf("the sheet still says recall is off: %+v", still)
	}

	code, on, refusal := rig.post(memoryRecallRequest{Runtime: "claude", Action: memoryRecallAttach, Consent: true})
	if code != http.StatusOK || !on.Changed || !on.State.EntryPresent || !on.State.AddedByAction ||
		len(on.State.Offers) != 1 || on.State.Offers[0] != memoryRecallDetach {
		t.Fatalf("turn on: %d %+v %+v", code, on, refusal)
	}
	if got := rig.file(); !strings.Contains(got, rig.self+" memory index") {
		t.Fatalf("the memory hook names this daemon's own executable:\n%s", got)
	}
	if code, again, _ := rig.post(memoryRecallRequest{Runtime: "claude", Action: memoryRecallAttach, Consent: true}); code != http.StatusOK ||
		again.Changed || again.Code != memoryRecallAlreadyPresent {
		t.Fatalf("a second turn on writes nothing: %+v", again)
	}

	code, turnedOff, _ := rig.post(memoryRecallRequest{Runtime: "claude", Action: memoryRecallDetach, Consent: true})
	if code != http.StatusOK || !turnedOff.Changed || turnedOff.State.EntryPresent {
		t.Fatalf("turn off: %d %+v", code, turnedOff)
	}
	after := rig.file()
	if strings.Contains(after, "memory index") || !strings.Contains(after, "hook --runtime claude") {
		t.Fatalf("exactly the entry the action added is removed; the lifecycle hook stays:\n%s", after)
	}
	var beforeJSON, afterJSON any
	if json.Unmarshal([]byte(before), &beforeJSON) != nil || json.Unmarshal([]byte(after), &afterJSON) != nil {
		t.Fatal("the settings file is JSON before and after")
	}
	beforeText, _ := json.Marshal(beforeJSON)
	afterText, _ := json.Marshal(afterJSON)
	if string(beforeText) != string(afterText) {
		t.Fatalf("the settings are what they were before Turn on:\n%s\n%s", beforeText, afterText)
	}
}

// Criterion 92's fail path at the route: an entry the person added by hand is left
// alone — Turn off is not offered for it, and a detach that is asked anyway changes
// nothing.
func TestMemoryRecallLeavesAHandAddedEntryAlone(t *testing.T) {
	rig := newRecallRig(t, "self")
	if _, err := memcli.AttachRuntime("claude", rig.self); err != nil { // stands in for an entry made outside the action…
		t.Fatal(err)
	}
	// …so forget that an action made it: the record says nothing about who added it.
	if err := os.RemoveAll(filepath.Join(rig.home, ".crossing-guard", "memory-attachments")); err != nil {
		t.Fatal(err)
	}
	withEntry := rig.file()
	state := rig.state("claude")
	if !state.EntryPresent || state.AddedByAction || len(state.Offers) != 0 {
		t.Fatalf("an entry the action did not add offers neither action: %+v", state)
	}
	code, out, _ := rig.post(memoryRecallRequest{Runtime: "claude", Action: memoryRecallDetach, Consent: true})
	if code != http.StatusOK || out.Changed || out.Code != memcli.RecallNotAddedByAction {
		t.Fatalf("a hand-added entry is left alone: %d %+v", code, out)
	}
	if got := rig.file(); got != withEntry {
		t.Fatal("the file is byte-identical")
	}
}

// OD-24: a daemon may attach or detach for a runtime only when that home's
// lifecycle-hook entry for the runtime names this daemon's own executable. A
// development build pointed at someone's real home is refused and says why; nothing
// is written.
func TestMemoryRecallIsRefusedForAnInstallationThisDaemonDoesNotOwn(t *testing.T) {
	foreign := newRecallRig(t, filepath.Join(t.TempDir(), "installed", "crossing-guard"))
	before := foreign.file()
	state := foreign.state("claude")
	if state.WithheldCode != memoryRecallNotThisInstallation || len(state.Offers) != 0 || !strings.Contains(state.Withheld, "installed/crossing-guard") {
		t.Fatalf("the card says why it offers nothing: %+v", state)
	}
	for _, action := range []string{memoryRecallAttach, memoryRecallDetach} {
		code, _, refusal := foreign.post(memoryRecallRequest{Runtime: "claude", Action: action, Consent: true})
		if code != http.StatusConflict || refusal.Code != memoryRecallNotThisInstallation || !strings.Contains(refusal.Error, "installed/crossing-guard") {
			t.Fatalf("%s for another installation: %d %+v", action, code, refusal)
		}
	}
	if got := foreign.file(); got != before {
		t.Fatal("a refused action writes nothing")
	}

	none := newRecallRig(t, "")
	code, _, refusal := none.post(memoryRecallRequest{Runtime: "claude", Action: memoryRecallAttach, Consent: true})
	if code != http.StatusConflict || refusal.Code != memoryRecallNoLifecycleHook {
		t.Fatalf("a home with no lifecycle hook is nobody's installation: %d %+v", code, refusal)
	}
	if got := none.file(); got != "{\"model\":\"opus\"}\n" {
		t.Fatalf("nothing is written: %s", got)
	}
	if code, _, refusal := none.post(memoryRecallRequest{Runtime: "no-such-runtime", Action: memoryRecallAttach, Consent: true}); code != http.StatusNotFound || refusal.Code != memoryRecallUnknownRuntime {
		t.Fatalf("an unknown runtime: %d %+v", code, refusal)
	}
	if code, _, refusal := none.post(memoryRecallRequest{Runtime: "claude", Action: "reinstall", Consent: true}); code != http.StatusBadRequest || refusal.Code != memoryRecallInvalid {
		t.Fatalf("an unknown action: %d %+v", code, refusal)
	}
}

// Red-team M8: ownership is proved for the home's own settings; the write goes to the
// path an earlier attach recorded, which may be a file anywhere. When the two are not
// the same settings, both actions are refused and neither file is touched.
func TestMemoryRecallIsRefusedWhenTheRecordedFileIsNotTheOneChecked(t *testing.T) {
	rig := newRecallRig(t, "self")
	elsewhere := filepath.Join(t.TempDir(), "other-home", ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(elsewhere), 0o700); err != nil {
		t.Fatal(err)
	}
	const other = "{\"hooks\":{\"SessionStart\":[{\"hooks\":[{\"type\":\"command\",\"command\":\"SELF memory index\"}]}]}}\n"
	otherBody := strings.ReplaceAll(other, "SELF", rig.self)
	if err := os.WriteFile(elsewhere, []byte(otherBody), 0o600); err != nil {
		t.Fatal(err)
	}
	record, _ := json.Marshal(map[string]any{"version": 1, "vendor": "claude", "config_path": elsewhere, "added_by_action": true, "binary": rig.self})
	recordDir := filepath.Join(rig.home, ".crossing-guard", "memory-attachments")
	if err := os.MkdirAll(recordDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(recordDir, "claude.json"), record, 0o600); err != nil {
		t.Fatal(err)
	}
	before := rig.file()

	state := rig.state("claude")
	if state.WithheldCode != memoryRecallOtherSettings || len(state.Offers) != 0 || !strings.Contains(state.Withheld, elsewhere) {
		t.Fatalf("the card offers nothing for a file the check did not read: %+v", state)
	}
	for _, action := range []string{memoryRecallAttach, memoryRecallDetach} {
		code, _, refusal := rig.post(memoryRecallRequest{Runtime: "claude", Action: action, Consent: true})
		if code != http.StatusConflict || refusal.Code != memoryRecallOtherSettings {
			t.Fatalf("%s into a file the ownership check did not read: %d %+v", action, code, refusal)
		}
	}
	if raw, err := os.ReadFile(elsewhere); err != nil || string(raw) != otherBody {
		t.Fatalf("the other file was written: %s %v", raw, err)
	}
	if got := rig.file(); got != before {
		t.Fatal("the checked file was written")
	}
}
