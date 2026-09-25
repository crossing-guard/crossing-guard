package memcli

// doctor used to say "NOT INJECTED — memory is not reaching this agent" for a
// runtime where the memory hook had simply never been installed. That is the same
// not-installed-vs-not-working conflation the guard surface had: it reads as a
// broken system to someone who never asked for the feature, and it buries the
// case that IS broken among false alarms.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeMemoryHookIsFoundOrHonestlyAbsent(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")

	// A settings file with a guard hook but NO memory hook: attached for
	// enforcement, not attached for memory. The two are separate attachments and
	// must not be read off each other.
	guardOnly := `{"hooks":{"PreToolUse":[{"matcher":"*","hooks":[` +
		`{"type":"command","command":"\"/bin/crossing-guard\" hook --runtime claude"}]}]}}`
	if err := os.WriteFile(settings, []byte(guardOnly), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := (claudeAdapter{}).MemoryHookBinary(settings); got != "" {
		t.Errorf("MemoryHookBinary = %q, want empty: a guard hook is not a memory hook", got)
	}

	withMemory := `{"hooks":{"SessionStart":[{"matcher":"startup","hooks":[` +
		`{"type":"command","command":"/opt/crossing-guard memory index"}]}]}}`
	if err := os.WriteFile(settings, []byte(withMemory), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := (claudeAdapter{}).MemoryHookBinary(settings); got != "/opt/crossing-guard" {
		t.Errorf("MemoryHookBinary = %q, want the binary the hook runs", got)
	}
	// A config that does not exist is "not attached", never an error.
	if got := (claudeAdapter{}).MemoryHookBinary(filepath.Join(dir, "nope.json")); got != "" {
		t.Errorf("missing config = %q, want empty", got)
	}
}

// Codex's hook lives in TOML, and may have been written by an older build or by
// hand — reporting such a hook as absent would tell the user to attach what is
// already attached.
func TestCodexMemoryHookIsFoundOutsideOurMarkerBlock(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	body := "[[hooks.SessionStart]]\n[[hooks.SessionStart.hooks]]\n" +
		"type = \"command\"\ncommand = \"/some/old/path/crossing-guard memory index\"\n"
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := (codexAdapter{}).MemoryHookBinary(cfg); got != "/some/old/path/crossing-guard" {
		t.Errorf("MemoryHookBinary = %q, want the path even outside our marker block", got)
	}

	// A Stop-only sync hook is not an injection hook.
	sync := "[[hooks.Stop]]\n[[hooks.Stop.hooks]]\ntype = \"command\"\n" +
		"command = \"/opt/crossing-guard sync --quiet\"\n"
	if err := os.WriteFile(cfg, []byte(sync), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := (codexAdapter{}).MemoryHookBinary(cfg); got != "" {
		t.Errorf("MemoryHookBinary = %q, want empty: sync is not injection", got)
	}
}

func TestCodexMemoryHookRecognizesBothTOMLStringForms(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want string
	}{
		{"basic string", `command = "/opt/crossing-guard memory index"`, "/opt/crossing-guard"},
		{"literal string", `command = '/opt/crossing-guard memory index'`, "/opt/crossing-guard"},
		{"other command", `command = '/opt/crossing-guard sync --quiet'`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(cfg, []byte(tc.line+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := (codexAdapter{}).MemoryHookBinary(cfg); got != tc.want {
				t.Errorf("MemoryHookBinary = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMemoryHookCommandParsing(t *testing.T) {
	for _, tc := range []struct{ cmd, want string }{
		{"/opt/crossing-guard memory index", "/opt/crossing-guard"},
		{"/Applications/My Tools/crossing-guard memory index", "/Applications/My Tools/crossing-guard"},
		// Trailing flags must not break recognition. The first parser was
		// end-anchored and would have reported every attached vendor as absent the
		// moment a flag was appended — the guard parser's documented regression,
		// rebuilt here the same day. This case is the tripwire.
		{"/opt/crossing-guard memory index --quiet", "/opt/crossing-guard"},
		{"/opt/crossing-guard sync --quiet", ""},
		{"/opt/crossing-guard hook --runtime claude", ""},
		{"", ""},
	} {
		if got := memoryHookBinaryFromCommand(tc.cmd); got != tc.want {
			t.Errorf("memoryHookBinaryFromCommand(%q) = %q, want %q", tc.cmd, got, tc.want)
		}
	}
}

func TestDoctorUsesRecordedCustomConfigBeforeAnySessionExists(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CG_MEMORY_DIR", filepath.Join(home, ".crossing-guard", "memory"))
	custom := filepath.Join(home, "custom", "config.toml")
	if err := os.MkdirAll(filepath.Dir(custom), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(custom, []byte(`command = "/opt/crossing-guard memory index"`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recordAttachment("codex", custom); err != nil {
		t.Fatal(err)
	}
	report := doctorReport(0)
	for _, health := range report.Vendors {
		if health.Vendor == "codex" {
			if !health.Attached || health.ConfigPath != custom {
				t.Fatalf("codex health = %+v, want attached at recorded custom config", health)
			}
			return
		}
	}
	t.Fatal("codex health missing")
}

func TestDoctorDoesNotHideCorruptAttachmentEvidence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CG_MEMORY_DIR", filepath.Join(home, ".crossing-guard", "memory"))
	path := attachmentRecordPath("codex")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, health := range doctorReport(0).Vendors {
		if health.Vendor == "codex" {
			found = true
			if !strings.Contains(health.Status, "attachment evidence unreadable") {
				t.Fatalf("status = %q, want corrupt evidence reported", health.Status)
			}
			if health.ConfigPath != "" {
				t.Fatalf("config_path = %q beside corrupt evidence; it must not invent a claimed path", health.ConfigPath)
			}
		}
	}
	if !found {
		t.Fatal("codex health missing")
	}
}

func TestAttachmentRecordRoundTripIsOwnerOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CG_MEMORY_DIR", filepath.Join(home, ".crossing-guard", "memory"))
	const config = "/custom/config.toml"
	if err := recordAttachment("codex", config); err != nil {
		t.Fatal(err)
	}
	rec, found, err := readAttachmentRecord("codex")
	if err != nil || !found {
		t.Fatalf("readAttachmentRecord = %+v, %v, %v", rec, found, err)
	}
	if rec.Version != attachmentRecordVersion || rec.Vendor != "codex" || rec.ConfigPath != config {
		t.Fatalf("record = %+v, want versioned codex path %q", rec, config)
	}
	info, err := os.Stat(attachmentRecordPath("codex"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("record mode = %v, want 0600", info.Mode().Perm())
	}
}
