package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"crossing-guard/engine"
)

func TestAuditRulesReportsActiveDefaultStatefulRules(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.Mkdir(filepath.Join(home, ".crossing-guard"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_RULES", filepath.Join(t.TempDir(), "absent-rules.json"))
	auditMigrationErr = nil
	t.Cleanup(func() { auditMigrationErr = nil })

	rec := httptest.NewRecorder()
	handleAuditRules(rec, httptest.NewRequest(http.MethodGet, "/api/audit/rules", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET audit rules: status %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Rules  []engine.Rule `json:"rules"`
		Path   string        `json:"path"`
		Source string        `json:"source"`
		Reach  string        `json:"reach"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Rules) != 3 {
		t.Fatalf("expected the three shipped stateful observe rules, got %d", len(body.Rules))
	}
	if body.Source != "embedded-catalog" {
		t.Fatalf("source = %q", body.Source)
	}
	if body.Path == "" || !strings.Contains(body.Reach, "post-hoc") {
		t.Fatalf("missing canonical path or reach: %+v", body)
	}
	for _, rule := range body.Rules {
		if rule.Action != "observe" || rule.Severity == "" {
			t.Fatalf("default audit rule is not report-only with presentation severity: %+v", rule)
		}
	}
}

func TestAuditRulesCustomFileReplacesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules.json")
	raw := `{"rules":[{"id":"custom-stateful","action":"observe","severity":"low","if":{"tag":"session:fs","value":"write"}}]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_RULES", path)
	auditMigrationErr = nil
	t.Cleanup(func() { auditMigrationErr = nil })

	rec := httptest.NewRecorder()
	handleAuditRules(rec, httptest.NewRequest(http.MethodGet, "/api/audit/rules", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET audit rules: status %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Rules  []engine.Rule `json:"rules"`
		Source string        `json:"source"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Rules) != 1 || body.Rules[0].ID != "custom-stateful" {
		t.Fatalf("custom file must replace, not receive, defaults: %+v", body.Rules)
	}
	if body.Source != "environment-file" {
		t.Fatalf("source = %q", body.Source)
	}
}

func TestAuditHandlersFailLoudlyWhileLegacyMigrationIsUnresolved(t *testing.T) {
	detectors, _ := engine.DefaultDetectors()
	if err := initAudit(detectors); err != nil {
		t.Fatal(err)
	}
	auditMigrationErr = errors.New("candidate is malformed")
	t.Cleanup(func() { auditMigrationErr = nil })

	for _, handler := range []http.HandlerFunc{handleAuditRules, handleAuditRun, handleAuditSession} {
		rec := httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodGet, "/api/audit/test", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("handler returned %d, want 503: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "migration unresolved") {
			t.Fatalf("handler hid the migration failure: %q", rec.Body.String())
		}
	}
}

func TestAuditFindingUsesRuleSeverityWithoutChangingAction(t *testing.T) {
	detectors, _ := engine.DefaultDetectors()
	if err := initAudit(detectors); err != nil {
		t.Fatal(err)
	}
	rules := []engine.Rule{{
		ID: "report-only", Action: "observe", Severity: "high",
		If: engine.Predicate{Tag: "session:fs", Value: "write"},
	}}
	findings := livePolicyFindings(rules, []engine.Tag{{Key: "session:fs", Value: "write"}}, SessionSummary{})
	if len(findings) != 1 || findings[0].Severity != "high" {
		t.Fatalf("severity metadata was not preserved: %+v", findings)
	}
	if len(findings[0].Fired) != 1 || findings[0].Fired[0].DetectorKind != "unknown" {
		t.Fatalf("unknown detector kind must be explicit, not guessed: %+v", findings[0].Fired)
	}
	if severityForAction("ask") != "medium" || severityForAction("redact") != "medium" {
		t.Fatal("fallback report severity must use the frontend's closed vocabulary")
	}
}

func TestAuditRunStopsBeforeScanningCancelledRequest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.Mkdir(filepath.Join(home, ".crossing-guard"), 0o755); err != nil {
		t.Fatal(err)
	}
	detectors, _ := engine.DefaultDetectors()
	if err := initAudit(detectors); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_RULES", filepath.Join(t.TempDir(), "absent-rules.json"))
	auditMigrationErr = nil
	t.Cleanup(func() { auditMigrationErr = nil })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/audit/run", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	handleAuditRun(rec, req)
	if rec.Body.Len() != 0 {
		t.Fatalf("cancelled request wrote a partial success response: %q", rec.Body.String())
	}
}
