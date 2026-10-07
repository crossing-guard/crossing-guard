package memcli

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recallSelf is the executable the tests' attaches write.
const recallSelf = "/opt/crossing-guard/crossing-guard"

// recallHome points HOME and the memory directory at a temp home — never the real one.
func recallHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CG_MEMORY_DIR", filepath.Join(home, ".crossing-guard", "memory"))
	t.Setenv("CPMEM_DIR", "")
	return home
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// Criterion 72: the memory hook's installed command line is unchanged.
func TestMemoryHookInstalledCommandLineIsUnchanged(t *testing.T) {
	if got := claudeMemoryHookCommand("/opt/crossing-guard/crossing-guard"); got != "/opt/crossing-guard/crossing-guard memory index" {
		t.Fatalf("claude: %q", got)
	}
	want := "# >>> crossing-guard memory (cpmem attach codex) >>>\n[[hooks.SessionStart]]\n[[hooks.SessionStart.hooks]]\n" +
		"type = \"command\"\ncommand = \"/opt/crossing-guard/crossing-guard memory index\"\n# <<< crossing-guard memory <<<"
	if got := codexMemoryHookBlock("/opt/crossing-guard/crossing-guard"); got != want {
		t.Fatalf("codex:\n%s", got)
	}
}

// Criteria 87 and 92 on Claude: Turn on writes the entry and records that it did;
// Turn off removes exactly that entry — the settings file comes back to what it held
// before, with everything else in it untouched.
func TestClaudeRecallAttachThenDetachRemovesExactlyWhatWasAdded(t *testing.T) {
	home := recallHome(t)
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	seed := `{"model":"opus","hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"/foreign/guard"}]}],` +
		`"SessionStart":[{"matcher":"startup","hooks":[{"type":"command","command":"/usr/local/bin/other-tool start"}]}]}}`
	if err := os.WriteFile(settings, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := RuntimeRecallAttachment("claude")
	if err != nil || before.EntryPresent || before.AddedByAction || before.ConfigPath != settings {
		t.Fatalf("no memory hook yet: %+v %v", before, err)
	}

	attached, err := AttachRuntime("claude", recallSelf)
	if err != nil || !attached.Wrote || !attached.EntryPresent || !attached.AddedByAction || attached.HookBinary != "/opt/crossing-guard/crossing-guard" {
		t.Fatalf("attach wrote the entry and recorded that it did: %+v %v", attached, err)
	}
	if got := mustRead(t, settings); !strings.Contains(got, "/opt/crossing-guard/crossing-guard memory index") || !strings.Contains(got, "/usr/local/bin/other-tool start") {
		t.Fatalf("the entry is added beside what was there:\n%s", got)
	}
	again, err := AttachRuntime("claude", recallSelf)
	if err != nil || again.Wrote || !again.AddedByAction {
		t.Fatalf("a second attach writes nothing and the entry is still this action's: %+v %v", again, err)
	}

	detached, err := DetachRuntime("claude")
	if err != nil || !detached.Removed || detached.EntryPresent || detached.LeftAlone != "" {
		t.Fatalf("detach removes the entry: %+v %v", detached, err)
	}
	after := mustRead(t, settings)
	for _, kept := range []string{`"model": "opus"`, "/foreign/guard", "/usr/local/bin/other-tool start", `"matcher": "startup"`} {
		if !strings.Contains(after, kept) {
			t.Fatalf("detach lost %q:\n%s", kept, after)
		}
	}
	if strings.Contains(after, "memory index") {
		t.Fatalf("the entry is gone:\n%s", after)
	}
	if _, found, _ := readAttachmentRecord("claude"); found {
		t.Fatal("the attachment record goes with the entry")
	}
	if none, err := DetachRuntime("claude"); err != nil || none.Removed || none.LeftAlone != RecallNotAttached {
		t.Fatalf("nothing left to remove: %+v %v", none, err)
	}
}

// Criterion 92's fail path, and F-2: an entry the person added by hand — or that was
// there before the action — is found already present, recorded as not this action's,
// and left alone by Turn off. The file is byte-identical afterwards.
func TestRecallDetachLeavesAHandAddedEntryAlone(t *testing.T) {
	home := recallHome(t)
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	seed := `{"hooks":{"SessionStart":[{"matcher":"startup|resume|clear|compact","hooks":[{"type":"command","command":"/opt/crossing-guard/crossing-guard memory index"}]}]}}` + "\n"
	if err := os.WriteFile(settings, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	attached, err := AttachRuntime("claude", recallSelf)
	if err != nil || attached.Wrote || !attached.EntryPresent || attached.AddedByAction {
		t.Fatalf("an entry already present is left alone and reported as such: %+v %v", attached, err)
	}
	if got := mustRead(t, settings); got != seed {
		t.Fatalf("attach changed a file that already had the entry:\n%s", got)
	}
	detached, err := DetachRuntime("claude")
	if err != nil || detached.Removed || detached.LeftAlone != RecallNotAddedByAction || !detached.EntryPresent {
		t.Fatalf("a hand-added entry is left alone: %+v %v", detached, err)
	}
	if got := mustRead(t, settings); got != seed {
		t.Fatalf("detach changed a hand-added entry:\n%s", got)
	}

	// With no record at all, a present entry is equally not this action's.
	if err := forgetAttachment("claude"); err != nil {
		t.Fatal(err)
	}
	if detached, _ := DetachRuntime("claude"); detached.Removed || detached.LeftAlone != RecallNotAddedByAction {
		t.Fatalf("no record, entry present: %+v", detached)
	}
}

// An entry the action added and someone then edited no longer matches exactly: it is
// left alone, and the record is kept so the state stays readable.
func TestRecallDetachLeavesAChangedEntryAlone(t *testing.T) {
	home := recallHome(t)
	settings := filepath.Join(home, ".claude", "settings.json")
	if _, err := AttachRuntime("claude", recallSelf); err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(mustRead(t, settings), "memory index", "memory index --quiet", 1)
	if err := os.WriteFile(settings, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	detached, err := DetachRuntime("claude")
	if err != nil || detached.Removed || detached.LeftAlone != RecallEntryChanged {
		t.Fatalf("an edited entry is not removed: %+v %v", detached, err)
	}
	if got := mustRead(t, settings); got != edited {
		t.Fatal("an edited entry is left as it is")
	}
}

// Codex: the action's entry is its marked block. Detach removes that block and gives
// the file back byte for byte; a hook written outside the markers is never touched.
func TestCodexRecallAttachThenDetachRemovesOnlyTheMarkedBlock(t *testing.T) {
	home := recallHome(t)
	config := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(config), 0o700); err != nil {
		t.Fatal(err)
	}
	seed := "model = \"gpt\"\n\n[[hooks.SessionStart]]\n[[hooks.SessionStart.hooks]]\ntype = \"command\"\ncommand = \"/usr/local/bin/other-tool start\"\n"
	if err := os.WriteFile(config, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	attached, err := AttachRuntime("codex", recallSelf)
	if err != nil || !attached.Wrote || !attached.AddedByAction {
		t.Fatalf("attach: %+v %v", attached, err)
	}
	if !strings.Contains(mustRead(t, config), codexMarkBegin) {
		t.Fatal("the marked block is written")
	}
	detached, err := DetachRuntime("codex")
	if err != nil || !detached.Removed || detached.EntryPresent {
		t.Fatalf("detach: %+v %v", detached, err)
	}
	if got := mustRead(t, config); got != seed {
		t.Fatalf("the file comes back byte for byte:\n%q\nwant\n%q", got, seed)
	}

	// A block another build wrote is not this action's.
	other := seed + "\n" + codexMemoryHookBlock("/old/crossing-guard") + "\n"
	if err := os.WriteFile(config, []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recordAttachment("codex", config, "/opt/crossing-guard/crossing-guard", true, false); err != nil {
		t.Fatal(err)
	}
	if left, err := DetachRuntime("codex"); err != nil || left.Removed || left.LeftAlone != RecallEntryChanged {
		t.Fatalf("another build's block is left alone: %+v %v", left, err)
	}
	if got := mustRead(t, config); got != other {
		t.Fatal("another build's block is unchanged")
	}
}

func TestRecallActionsRefuseAnUnknownRuntime(t *testing.T) {
	recallHome(t)
	if _, err := AttachRuntime("no-such-runtime", recallSelf); err != ErrUnknownRecallRuntime {
		t.Fatalf("attach: %v", err)
	}
	if _, err := DetachRuntime("no-such-runtime"); err != ErrUnknownRecallRuntime {
		t.Fatalf("detach: %v", err)
	}
	if got := RecallRuntimes(); len(got) != 2 || got[0] != "claude" || got[1] != "codex" {
		t.Fatalf("the runtimes with a memory adapter: %v", got)
	}
	if checked, injected := RecallObserved("claude", 5); checked != 0 || injected != 0 {
		t.Fatalf("a home with no sessions has observed nothing: %d %d", checked, injected)
	}
}

// Journey finding D-5: on a home with no Codex config.toml, Turn on created the file
// and Turn off left it behind with nothing in it. The attach records that it created
// the file, and a detach that leaves such a file empty removes it. A file that was
// there before, or that holds anything else by then, stays.
func TestCodexRecallDetachRemovesTheSettingsFileItsAttachCreated(t *testing.T) {
	home := recallHome(t)
	config := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(config), 0o700); err != nil {
		t.Fatal(err)
	}
	if attached, err := AttachRuntime("codex", recallSelf); err != nil || !attached.Wrote {
		t.Fatalf("attach on a home with no settings file: %+v %v", attached, err)
	}
	if detached, err := DetachRuntime("codex"); err != nil || !detached.Removed {
		t.Fatalf("detach: %+v %v", detached, err)
	}
	if _, err := os.Lstat(config); !os.IsNotExist(err) {
		t.Fatalf("the settings file the attach created was left behind: %q (%v)", mustRead(t, config), err)
	}

	// Something else was written to the created file since: it stays, with that in it.
	if _, err := AttachRuntime("codex", recallSelf); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte(mustRead(t, config)+"\nmodel = \"other\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if detached, err := DetachRuntime("codex"); err != nil || !detached.Removed {
		t.Fatalf("detach: %+v %v", detached, err)
	}
	if got := mustRead(t, config); !strings.Contains(got, `model = "other"`) || strings.Contains(got, codexMarkBegin) {
		t.Fatalf("a created file that holds something else stays, without the block: %q", got)
	}

	// A file that was there before the attach, even an empty one, is not this action's to remove.
	if err := os.WriteFile(config, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := AttachRuntime("codex", recallSelf); err != nil {
		t.Fatal(err)
	}
	if detached, err := DetachRuntime("codex"); err != nil || !detached.Removed {
		t.Fatalf("detach: %+v %v", detached, err)
	}
	if _, err := os.Lstat(config); err != nil {
		t.Fatalf("a settings file that existed before the attach was removed: %v", err)
	}
}

// Journey finding: `memory upsert --help` printed a "panic calling String method on
// zero …" line: the flag package builds a zero value of each flag to print the usage.
func TestRepeatableFlagUsagePrintsNoPanicLine(t *testing.T) {
	set := flag.NewFlagSet("upsert", flag.ContinueOnError)
	var usage strings.Builder
	set.SetOutput(&usage)
	multiFlag(set, "source-citation", "evidence, repeatable")
	set.PrintDefaults()
	if strings.Contains(usage.String(), "panic") || !strings.Contains(usage.String(), "source-citation") {
		t.Fatalf("the usage of a repeatable flag:\n%s", usage.String())
	}
}
