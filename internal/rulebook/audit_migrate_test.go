package rulebook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"crossing-guard/engine"
)

func TestImportLegacyAuditPreservesAndRetriesWithoutDuplication(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("CG_RULES", "")
	activePath := filepath.Join(dir, ".crossing-guard", "policy", "rules.json")
	legacyPath := filepath.Join(dir, "audit-rules.json")
	if err := os.MkdirAll(filepath.Dir(activePath), 0o700); err != nil {
		t.Fatal(err)
	}
	active := `{"custom":{"keep":true},"rules":[{"id":"credential-touch","action":"deny","if":{"tag":"command","matches":"never"}}]}`
	legacy := `{"rules":[{"id":"credential-touch","severity":"high","then":"stop","message":"report only","if":{"all":[{"tag":"data-class","value":"credential-material"},{"not":{"tag":"phase","value":"red-team"}}]}}]}`
	if err := os.WriteFile(activePath, []byte(active), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	// Force the post-save archive-warning path. The imported active document must
	// remain usable, and a retry must recognize its converted content.
	if err := os.WriteFile(legacyPath+".migrated", []byte("prior archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := ImportLegacyAudit(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if first.Imported != 1 || first.ArchiveWarning == "" {
		t.Fatalf("first import = %+v, want one import plus archive warning", first)
	}
	pol, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(pol.Rules) != 2 {
		t.Fatalf("active rules = %d, want prior + imported", len(pol.Rules))
	}
	migrated := pol.Rules[1]
	if migrated.ID != "legacy-audit-credential-touch" || migrated.Action != "observe" || migrated.Severity != "high" {
		t.Fatalf("migrated rule strengthened or lost metadata: %+v", migrated)
	}
	if got := migrated.If.All[0].Tag; got != "session:data-class" {
		t.Fatalf("whole-session tag not migrated, got %q", got)
	}
	if got := migrated.If.All[1].Not.Tag; got != "session:phase" {
		t.Fatalf("nested negated tag not migrated, got %q", got)
	}
	var raw map[string]json.RawMessage
	bytes, err := os.ReadFile(activePath)
	if err != nil || json.Unmarshal(bytes, &raw) != nil || raw["custom"] == nil {
		t.Fatalf("unknown active fields were lost: err=%v raw=%v", err, raw)
	}

	second, err := ImportLegacyAudit(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if second.Imported != 0 || second.Skipped != 1 {
		t.Fatalf("retry duplicated import: %+v", second)
	}
	pol, err = Load()
	if err != nil || len(pol.Rules) != 2 {
		t.Fatalf("retry changed active rules: len=%d err=%v", len(pol.Rules), err)
	}

	if err := os.Remove(legacyPath + ".migrated"); err != nil {
		t.Fatal(err)
	}
	third, err := ImportLegacyAudit(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if third.Imported != 0 || third.Skipped != 1 || third.Archive == "" {
		t.Fatalf("archive retry did not close migration: %+v", third)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("legacy source still active after archive: %v", err)
	}
}

func TestSaveValidatesBacksUpAndUsesPrivateMode(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "policy", "rules.json")
	t.Setenv("CG_RULES", path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	old := `{"rules":[{"id":"old","action":"observe","if":{"tag":"session:x"}}]}`
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	valid := `{"rules":[{"id":"new","action":"observe","if":{"tag":"session:y"}}]}`
	backup, err := Save([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if backup != path+".bak" {
		t.Fatalf("backup = %q", backup)
	}
	if bytes, err := os.ReadFile(backup); err != nil || string(bytes) != old {
		t.Fatalf("backup mismatch: %q err=%v", bytes, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("active mode = %o, want 600", info.Mode().Perm())
	}
	backupInfo, err := os.Stat(backup)
	if err != nil {
		t.Fatal(err)
	}
	if backupInfo.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode = %o, want 600", backupInfo.Mode().Perm())
	}
	if _, err := Save([]byte(`{"rules":[{"id":"bad","action":"dney","if":{}}]}`)); err == nil {
		t.Fatal("malformed candidate must fail before replacement")
	}
	pol, err := Load()
	if err != nil || len(pol.Rules) != 1 || pol.Rules[0].ID != "new" {
		t.Fatalf("failed save changed active rules: %+v err=%v", pol, err)
	}
}

func TestImportedAuditRuleRemainsReportOnlyToDecide(t *testing.T) {
	rule := engine.Rule{ID: "report", Action: "observe", Severity: "high",
		If: engine.Predicate{Tag: "session:data-class", Value: "credential-material"}}
	decision := engine.Decide([]engine.Tag{{Key: "session:data-class", Value: "credential-material"}},
		&engine.Policy{Rules: []engine.Rule{rule}})
	if decision.Mode != engine.SilentLog {
		t.Fatalf("report migration strengthened to %s", decision.Mode)
	}
}
