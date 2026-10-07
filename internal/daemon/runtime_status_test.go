package daemon

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"crossing-guard/internal/guardcli"
	"crossing-guard/internal/rulebook"
	"crossing-guard/internal/teamlink"
	"crossing-guard/store"
)

// isolateRuntimeDetection points every runtime's configuration lookup at an
// empty home, so detection finds nothing, runs no runtime binary, and leaves no
// remembered path behind for the next test.
func isolateRuntimeDetection(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("OPENCODE_CONFIG_DIR", filepath.Join(home, ".config", "opencode"))
	guardcli.ForgetResolvedConfigs()
	t.Cleanup(guardcli.ForgetResolvedConfigs)
}

// runtimeFactsFixture opens a store with two live claude events — one ordinary, one
// the proof rule's deny — under an empty HOME, so no runtime is attached.
func runtimeFactsFixture(t *testing.T) {
	t.Helper()
	isolateRuntimeDetection(t)
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	prior := governor
	governor = NewGovernor(ix, nil)
	t.Cleanup(func() { governor = prior; _ = ix.Close() })
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []store.EventRecord{
		{TS: 20, SessionID: "a", Runtime: "claude", Verb: "exec", Tool: "Bash", Decision: "deny", RuleID: rulebook.CanaryRuleID, Origin: "live"},
		{TS: 30, SessionID: "a", Runtime: "claude", Verb: "exec", Tool: "Bash", Decision: "allow", Origin: "live"},
	} {
		if _, err := tx.AppendEvent(e, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// The team report's runtime facts, pinned before the gather was shared with
// GET /api/runtime-status (settings-restructure plan invariant 10): attached is
// the folded fact, firing and the canary come from stored live events.
func TestTeamReportRuntimeFactsArePinned(t *testing.T) {
	runtimeFactsFixture(t)
	at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	linker := &teamLinker{deviceID: "dev_fixture", now: func() time.Time { return at }}
	facts, err := linker.gatherFacts()
	if err != nil {
		t.Fatal(err)
	}
	live, canary := time.Unix(30, 0), time.Unix(20, 0)
	var want []teamlink.RuntimeFacts
	for _, state := range guardcli.RuntimeAttachStates() {
		fact := teamlink.RuntimeFacts{Name: state.Name}
		if state.Name == "claude" {
			fact.LastLive, fact.Canary = &live, &canary
		}
		want = append(want, fact)
	}
	if !reflect.DeepEqual(facts.Runtimes, want) {
		t.Fatalf("runtime facts = %+v, want %+v", facts.Runtimes, want)
	}
	if facts.DeviceID != "dev_fixture" || !facts.ReportedAt.Equal(at) || facts.StoreSchema != store.SchemaVersion {
		t.Fatalf("report header changed: %+v", facts)
	}
	// The canary rule and the rulebook layer, each worked out here the way the
	// report always did, independently of the shared function.
	wantActive := false
	if verdict, err := guardcli.CheckCommand("echo "+rulebook.CanaryMarker, nil); err == nil {
		wantActive = canaryProvable(verdict)
	}
	wantDigest, wantLayer := "", ""
	if doc, err := rulebook.LoadDocumentPreview(); err == nil && doc.Active {
		wantDigest, wantLayer = doc.Digest, "user"
	}
	if facts.CanaryRuleActive != wantActive || facts.RulebookDigest != wantDigest || facts.RulebookLayer != wantLayer {
		t.Fatalf("canary %v, rulebook %q/%q; want %v, %q/%q", facts.CanaryRuleActive, facts.RulebookDigest, facts.RulebookLayer, wantActive, wantDigest, wantLayer)
	}
}

// GET /api/runtime-status (settings-restructure plan §3.3): every registered
// runtime has a row, its observations come from stored live events, and the
// one attention code is decided here.
func TestRuntimeStatusListsEveryRuntimeWithItsObservations(t *testing.T) {
	runtimeFactsFixture(t)
	dropRuntimeObservationCache()
	t.Cleanup(dropRuntimeObservationCache)
	status := buildRuntimeStatus(time.Unix(1000, 0))
	names := guardcli.RuntimeAttachStates()
	if len(status.Runtimes) != len(names) || status.LiveUnavailable || status.ObservedAt == "" || !status.CanaryRuleChecked {
		t.Fatalf("status = %+v, want %d rows with observations", status, len(names))
	}
	for index, row := range status.Runtimes {
		if row.Name != names[index].Name || row.DisplayName == "" {
			t.Fatalf("row %d = %+v, want runtime %s with a display name", index, row, names[index].Name)
		}
		// A registered identifier is not a name for people: some owner in Go
		// (chat capability, connection descriptor, or the installer) names it.
		if row.DisplayName == row.Name {
			t.Fatalf("runtime %s shows its registered identifier as its name", row.Name)
		}
		if row.HookConfigured {
			t.Fatalf("an empty home has no hook configured: %+v", row)
		}
		if row.Name == "claude" && (row.LastLiveAt != "1970-01-01T00:00:30Z" || row.CanaryAt != "1970-01-01T00:00:20Z") {
			t.Fatalf("claude observations = %q / %q", row.LastLiveAt, row.CanaryAt)
		}
		if row.Name != "claude" && (row.LastLiveAt != "" || row.CanaryAt != "") {
			t.Fatalf("%s has no stored event: %+v", row.Name, row)
		}
	}
}

func TestRuntimeStatusAnswersWithoutAStore(t *testing.T) {
	isolateRuntimeDetection(t)
	prior := governor
	governor = nil
	dropRuntimeObservationCache()
	t.Cleanup(func() { governor = prior; dropRuntimeObservationCache() })
	status := buildRuntimeStatus(time.Unix(1000, 0))
	if !status.LiveUnavailable || status.ObservedAt != "" || status.CanaryRuleChecked || len(status.Runtimes) == 0 {
		t.Fatalf("without a store the local facts still answer, marked: %+v", status)
	}
	for _, row := range status.Runtimes {
		if row.Attention == "never_fired" {
			t.Fatalf("an unreadable store proves nothing about firing: %+v", row)
		}
	}
}

func TestRuntimeObservationsAreReusedUntilDropped(t *testing.T) {
	runtimeFactsFixture(t)
	dropRuntimeObservationCache()
	t.Cleanup(dropRuntimeObservationCache)
	first := buildRuntimeStatus(time.Unix(1000, 0))
	tx, err := governor.ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.AppendEvent(store.EventRecord{TS: 90, SessionID: "b", Runtime: "claude", Verb: "exec", Tool: "Bash", Decision: "allow", Origin: "live"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	live := func(status runtimeStatusResponse) string {
		for _, row := range status.Runtimes {
			if row.Name == "claude" {
				return row.LastLiveAt
			}
		}
		return ""
	}
	if reused := buildRuntimeStatus(time.Unix(1001, 0)); live(reused) != live(first) || reused.ObservedAt != first.ObservedAt {
		t.Fatalf("a read one second later must reuse the observations: %q vs %q", live(reused), live(first))
	}
	dropRuntimeObservationCache()
	if fresh := buildRuntimeStatus(time.Unix(1002, 0)); live(fresh) != "1970-01-01T00:01:30Z" {
		t.Fatalf("after a drop the newest event shows: %q", live(fresh))
	}
	if stale := buildRuntimeStatus(time.Unix(1002, 0).Add(runtimeObservationMaxAge())); stale.ObservedAt == first.ObservedAt {
		t.Fatal("observations older than the report interval must be read again")
	}
}

func TestRuntimeAttentionIsDecidedOnce(t *testing.T) {
	guided := guardcli.RuntimeConnectionStatus{State: "needs_attention", Problem: "the configured Crossing Guard hook binary no longer exists"}
	for _, c := range []struct {
		name     string
		row      runtimeStatusRow
		guided   bool
		unread   bool
		want     string
		sentence string
	}{
		{"a guided connection's judgement wins", runtimeStatusRow{HookConfigured: true, HookBinaryPresent: true, HookCurrent: true, LastLiveAt: "x"}, true, false, "needs_attention", guided.Problem},
		{"hook binary gone", runtimeStatusRow{HookConfigured: true}, false, false, "hook_binary_missing", ""},
		{"hook outdated", runtimeStatusRow{HookConfigured: true, HookBinaryPresent: true}, false, false, "hook_outdated", ""},
		{"connected, never fired", runtimeStatusRow{HookConfigured: true, HookBinaryPresent: true, HookCurrent: true}, false, false, "never_fired", ""},
		{"firing unknown is not never", runtimeStatusRow{HookConfigured: true, HookBinaryPresent: true, HookCurrent: true}, false, true, "", ""},
		{"healthy", runtimeStatusRow{HookConfigured: true, HookBinaryPresent: true, HookCurrent: true, LastLiveAt: "x"}, false, false, "", ""},
		{"not connected", runtimeStatusRow{}, false, false, "", ""},
	} {
		code, sentence := runtimeAttention(c.row, guided, c.guided, c.unread)
		if code != c.want || sentence != c.sentence {
			t.Fatalf("%s: got %q %q, want %q %q", c.name, code, sentence, c.want, c.sentence)
		}
	}
}

func TestDaemonVersionNamesTheRunningDaemon(t *testing.T) {
	dir := t.TempDir()
	version := daemonVersion(dir, "127.0.0.1:7788")
	if version.Version != apiVersion || version.BuildVersion == "" || version.StoreSchema != store.SchemaVersion ||
		version.DataDir != dir || version.ListenAddr != "127.0.0.1:7788" || version.LogPath != "" {
		t.Fatalf("version = %+v", version)
	}
	if err := os.WriteFile(filepath.Join(dir, "daemon.log"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := daemonVersion(dir, "").LogPath; got != filepath.Join(dir, "daemon.log") {
		t.Fatalf("log path = %q", got)
	}
}

// The Settings pages' strings the owner reads in each state (settings-restructure
// plan §5.2): a page that loses one has lost a state, a banner or a recovery path.
// Each is pinned to the file that owns it after the split.
func TestSettingsPagesKeepTheirStates(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		body, err := staticFS.ReadFile("static/js/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	for file, pins := range map[string][]string{
		"views/settings-runtimes.js": {
			"Checking registered runtime connections…", "Runtime connections unavailable: ", "Detection is read-only.",
			"Install the provider first; Crossing Guard will not install or launch it.", "Watch unavailable: ", "Watch stopped: ",
			"Confirmation failed: ", "Current connection status could not be refreshed: ", "Preparing a read-only preview…",
			"Preview unavailable: ", "setTimeout(poll, 1500)", "setTimeout(poll, 700)", "Loading registered chat runtimes…",
			"Chat defaults unavailable: ", "Saved ✓", "runtimes[runtime] = { ...(runtimes[runtime] || {}), ...account }",
			"api('/api/runtime-status')", "status.canary_rule_checked && !status.canary_rule_active",
		},
		// Settings → Team was rebuilt (team rest-of-release plan §4.2). Each state the old
		// page pinned is pinned where it lives now; the verification record's inventory
		// says where every old control and fact went.
		//   "Team link unavailable: "            → "Team could not be read: "
		//   "Team layers unavailable: "          → "What the team shares could not be read."
		//   "this device sends nothing…"         → model: "Leaves this machine" / "Nothing"
		//   "Link refused: ", "Cancel failed: ", "Unlink failed: ", "Adopt failed: ",
		//   "Un-adopt failed: "                  → each act's `failed:` words, said beside it
		//   "nothing available — … published nothing" → model: "None shared yet"
		//   "no rule documents parsed"           → removed by §4.1 (refused at offer)
		//   the content-policy sentence          → model: "Session content" rows
		//   the 5 s pending recheck              → PENDING_RECHECK_MS
		//   the unlink route's note              → model: unlink outcome words
		"views/settings-team.js": {
			"Team could not be read: ", "What the team shares could not be read.", "failed: 'Not un-adopted'",
			"failed: 'Not trusted'", "failed: 'Not unlinked'", "errorText(error, 'Not adopted')", "errorText(error, 'Not linked')", "setTimeout(recheck, PENDING_RECHECK_MS)",
			"const PENDING_RECHECK_MS = 5000;", "await paintTeam(page, ctx);",
		},
		"views/settings-team-model.js": {
			"'Leaves this machine'", "'None shared yet'", "'Session content'", "'Waiting to send'",
			"new Set(['rejected', 'revoked'])", "'Unlinked here; the team server could not be told.'",
			"'Required: tool inputs of every session go to the team server'",
		},
		"views/settings-team-memory.js": {"failed: 'Not shared'"},
		"views/settings-team-dialog.js": {"dialog.showProblem(errorText(error, failed))", "await view.repaint();"},
		"views/settings-system.js": {
			"Dictation settings unavailable: ", "localStorage.removeItem('cg_token')", "localStorage.removeItem('cp_token')",
			"setDefaults(forgetProviderTokens(getDefaults()))", "api('/api/version')",
		},
		"views/settings-models.js":                    {"Refreshing…", "list.state !== 'unsupported'", "entries were not listed"},
		"views/settings-appearance.js":                {"Appearance could not be loaded: ", "previewTokens(null)", "Save theme as…"},
		"views/settings-transcript-modes.js":          {"Transcript modes could not be loaded: ", "The first rule that matches a row decides it"},
		"session-organization/settings-views.js":      {"Views unavailable: ", "document.addEventListener('cg:view-restored', reread)"},
		"session-organization/settings-view-index.js": {"No views yet.", "cannot be read; fix or remove it before saving"},
		"session-organization/settings-view-edit.js":  {"This view changed since you started editing.", "Not counted"},
		"views/settings.js": {
			"document.addEventListener('cg:view-restored'", "document.addEventListener('cg:settings-subpage'",
			"REREAD_ON_RESTORE", "return () => previewTokens(null);", "shell.pageHost.dataset.edited",
			// What to show on entry and on restore is decided by one pure function
			// (settings-model.js settleDecision, tested there); the shell dispatches
			// to the Agents page under the guard that keeps its own dispatch from
			// being read back as a request.
			"settleDecision({ requested, mounted, drawn, restored, reread })", "if (page === 'agents' && tellAgents) {",
			"detail: target ? { id: target } : {}", "if (dispatching) return;",
		},
		"app.js":        {"void refreshAttention();"},
		"views/chat.js": {"new CustomEvent('cg:settings-subpage', { detail: 'runtimes' })"},
	} {
		source := read(file)
		for _, pin := range pins {
			if !strings.Contains(source, pin) {
				t.Errorf("%s lost %q", file, pin)
			}
		}
	}
	// The Team page repaints into its own element; it no longer finds its place
	// by a heading's text, which a re-laid page would break without an error.
	if strings.Contains(read("views/settings-team.js"), "querySelectorAll('h2')") {
		t.Error("the Team page must not locate itself by heading text")
	}
}
