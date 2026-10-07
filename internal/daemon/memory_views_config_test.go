package daemon

// memory_views_config_test.go — a saved view may now be a memory view
// (memory-first-class-records plan §6): record_kind sessions|memory,
// additive; the old file is valid untouched.

import (
	"os"
	"path/filepath"
	"testing"

	"crossing-guard/internal/sessionquery"
)

func TestSavedViewRecordKindValidation(t *testing.T) {
	limits := sessionQueryLimitsForTest()

	// The default (empty) is sessions; "memory" is accepted; anything else is
	// refused with words.
	if err := validateSessionView(SavedSessionView{ID: "v1", Name: "One", Query: "", RecordKind: ""}, limits, sessionViewBoardBudgets(defaultConsoleConfig().SessionOrganization)); err != nil {
		t.Fatalf("empty record_kind must mean sessions: %v", err)
	}
	if err := validateSessionView(SavedSessionView{ID: "v2", Name: "Mem", Query: "tag:plan", RecordKind: "memory"}, limits, sessionViewBoardBudgets(defaultConsoleConfig().SessionOrganization)); err != nil {
		t.Fatalf("memory record_kind must be valid: %v", err)
	}
	if err := validateSessionView(SavedSessionView{ID: "v3", Name: "Odd", Query: "", RecordKind: "notes"}, limits, sessionViewBoardBudgets(defaultConsoleConfig().SessionOrganization)); err == nil {
		t.Fatal("an unknown record_kind must be refused")
	}
}

// sessionQueryLimitsForTest builds the grammar limits the way the views owner
// does (session-organization's bound mapping).
func sessionQueryLimitsForTest() sessionquery.Limits {
	org := defaultConsoleConfig().SessionOrganization
	return sessionquery.Limits{QueryBytes: int(org.QueryBytesMax), Terms: int(org.QueryTermsMax),
		GlobExpansion: int(org.GlobExpansionMax)}
}

func TestSavedViewRecordKindRoundTrips(t *testing.T) {
	dir := t.TempDir()
	limits := defaultConsoleConfig().SessionOrganization
	body := `{"format_version":1,"views":[` +
		`{"id":"m1","name":"Mem","query":"tag:plan","record_kind":"memory"},` +
		`{"id":"s1","name":"Sess","query":""}]}`
	if err := os.WriteFile(sessionViewsPath(dir), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	doc := loadSessionViews(dir, limits)
	if len(doc.Rejected) != 0 {
		t.Fatalf("both entries must read cleanly, got rejections: %+v", doc.Rejected)
	}
	if len(doc.Views) != 2 || doc.Views[0].RecordKind != "memory" || doc.Views[1].RecordKind != "" {
		t.Fatalf("round-trip mismatch: %+v", doc.Views)
	}
	_ = filepath.Join(dir, "session-views.json")
}
