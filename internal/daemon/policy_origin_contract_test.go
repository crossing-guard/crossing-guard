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
)

func TestPolicyRulesGetReportsEmbeddedActiveDocument(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.Mkdir(filepath.Join(home, ".crossing-guard"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "absent-rules.json")
	t.Setenv("CG_RULES", path)
	rec := httptest.NewRecorder()
	handlePolicyRulesGet(rec, httptest.NewRequest(http.MethodGet, "/api/policy/rules", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET rules: %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Path      string          `json:"path"`
		Doc       json.RawMessage `json:"doc"`
		Origin    string          `json:"origin"`
		Selection string          `json:"selection"`
		Digest    string          `json:"digest"`
		Active    bool            `json:"active"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Path != path || body.Origin != "embedded-catalog" || body.Selection != "legacy-implicit" || !body.Active {
		t.Fatalf("wrong active origin: %+v", body)
	}
	if len(body.Doc) == 0 || !strings.HasPrefix(body.Digest, "sha256:") {
		t.Fatalf("missing inspectable content evidence: %+v", body)
	}
}

func TestPolicyRulesRejectsMalformedCandidateWithoutReplacingActiveFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules.json")
	original := []byte(`{"rules":[{"id":"keep","action":"deny","if":{"tag":"command","matches":"keep"}}]}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_RULES", path)
	req := httptest.NewRequest(http.MethodPut, "/api/policy/rules",
		strings.NewReader(`{"rules":[{"id":"bad","action":"dney","if":{}}]}`))
	rec := httptest.NewRecorder()
	handlePolicyRulesPut(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PUT malformed rules: got %d, want 422: %s", rec.Code, rec.Body.String())
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatalf("rejected candidate replaced active config: got %q want %q", after, original)
	}
}

func TestRetiredPolicyGridIsGoneThroughBothAPIAliases(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/policy", handleRetiredPolicyGrid)
	mux.HandleFunc("PUT /api/policy", handleRetiredPolicyGrid)
	guarded := securityMiddleware(mux, "127.0.0.1:7788", "sekret", "/data/api-token")
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		for _, path := range []string{"/api/policy", "/api/v1/policy"} {
			req := httptest.NewRequest(method, path, nil)
			req.Header.Set("Authorization", "Bearer sekret")
			rec := httptest.NewRecorder()
			guarded.ServeHTTP(rec, req)
			if rec.Code != http.StatusGone {
				t.Fatalf("%s %s: got %d, want 410: %s", method, path, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "/api/v1/policy/rules") {
				t.Fatalf("retirement response does not name the live owner: %q", rec.Body.String())
			}
		}
	}
}

func TestPolicySelectionPreviewSelectSaveAndRollbackContract(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CG_RULES", "")
	// This C5b regression exercises rollback to the historical implicit starter,
	// so make the fixture an explicit pre-C5c compatibility root. Fresh C5c roots
	// already have an empty unselected baseline and selecting none removes no IDs.
	if err := os.Mkdir(filepath.Join(home, ".crossing-guard"), 0o755); err != nil {
		t.Fatal(err)
	}

	previewRec := httptest.NewRecorder()
	handlePolicySelectionPreview(previewRec, httptest.NewRequest(http.MethodPost,
		"/api/policy/rules/selection/preview", strings.NewReader(`{"source":"none"}`)))
	if previewRec.Code != http.StatusOK {
		t.Fatalf("preview: %d: %s", previewRec.Code, previewRec.Body.String())
	}
	var preview struct {
		ExpectedActiveDigest string   `json:"expected_active_digest"`
		ExpectedStateToken   string   `json:"expected_state_token"`
		ProposedDigest       string   `json:"proposed_digest"`
		Removed              []string `json:"removed"`
		SemanticReach        string   `json:"semantic_reach"`
	}
	if err := json.Unmarshal(previewRec.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.ExpectedStateToken == "" || preview.ProposedDigest == "" || len(preview.Removed) == 0 ||
		!strings.Contains(preview.SemanticReach, "not enumerable") {
		t.Fatalf("preview omitted acknowledgement facts: %+v", preview)
	}

	selectBody, _ := json.Marshal(map[string]string{
		"source": "none", "expected_active_digest": preview.ExpectedActiveDigest,
		"expected_state_token": preview.ExpectedStateToken, "acknowledged_digest": preview.ProposedDigest,
	})
	selectRec := httptest.NewRecorder()
	handlePolicySelection(selectRec, httptest.NewRequest(http.MethodPost,
		"/api/policy/rules/selection", bytes.NewReader(selectBody)))
	if selectRec.Code != http.StatusOK {
		t.Fatalf("select: %d: %s", selectRec.Code, selectRec.Body.String())
	}

	getRec := httptest.NewRecorder()
	handlePolicyRulesGet(getRec, httptest.NewRequest(http.MethodGet, "/api/policy/rules", nil))
	var active struct {
		Selection         string          `json:"selection"`
		Selected          bool            `json:"selected"`
		SelectedSource    string          `json:"selected_source"`
		SelectedSourceRef string          `json:"selected_source_ref"`
		Digest            string          `json:"digest"`
		StateToken        string          `json:"state_token"`
		Doc               json.RawMessage `json:"doc"`
	}
	if err := json.Unmarshal(getRec.Body.Bytes(), &active); err != nil {
		t.Fatal(err)
	}
	if active.Selection != "explicit-user" || !active.Selected || active.Digest != preview.ProposedDigest ||
		active.SelectedSource != "none" || active.SelectedSourceRef != "explicit empty rulebook" ||
		!strings.Contains(string(active.Doc), `"rules":[]`) {
		t.Fatalf("selected state mismatch: %+v doc=%s", active, active.Doc)
	}

	saveReq := httptest.NewRequest(http.MethodPut, "/api/policy/rules",
		strings.NewReader(`{"rules":[{"id":"api-save","action":"deny","if":{"tag":"command","matches":"api-smoke"}}]}`))
	saveReq.Header.Set("If-Match", active.StateToken)
	saveRec := httptest.NewRecorder()
	handlePolicyRulesPut(saveRec, saveReq)
	if saveRec.Code != http.StatusOK {
		t.Fatalf("selected save: %d: %s", saveRec.Code, saveRec.Body.String())
	}
	var saved struct {
		Digest string `json:"digest"`
	}
	if err := json.Unmarshal(saveRec.Body.Bytes(), &saved); err != nil || saved.Digest == active.Digest {
		t.Fatalf("selected save did not advance digest: %+v err=%v", saved, err)
	}

	staleReq := httptest.NewRequest(http.MethodPut, "/api/policy/rules", strings.NewReader(`{"rules":[]}`))
	staleReq.Header.Set("If-Match", active.StateToken)
	staleRec := httptest.NewRecorder()
	handlePolicyRulesPut(staleRec, staleReq)
	if staleRec.Code != http.StatusConflict || !strings.Contains(staleRec.Body.String(), "reload and review") {
		t.Fatalf("stale save: %d: %s", staleRec.Code, staleRec.Body.String())
	}

	latestRec := httptest.NewRecorder()
	handlePolicyRulesGet(latestRec, httptest.NewRequest(http.MethodGet, "/api/policy/rules", nil))
	if err := json.Unmarshal(latestRec.Body.Bytes(), &active); err != nil {
		t.Fatal(err)
	}
	if active.SelectedSource != "edited" || active.SelectedSourceRef != preview.ProposedDigest {
		t.Fatalf("selected edit source facts = %+v", active)
	}
	rollbackBody, _ := json.Marshal(map[string]string{"expected_state_token": active.StateToken})
	rollbackRec := httptest.NewRecorder()
	handlePolicySelectionRollback(rollbackRec, httptest.NewRequest(http.MethodPost,
		"/api/policy/rules/selection/rollback", bytes.NewReader(rollbackBody)))
	if rollbackRec.Code != http.StatusOK || !strings.Contains(rollbackRec.Body.String(), `"archive"`) {
		t.Fatalf("rollback: %d: %s", rollbackRec.Code, rollbackRec.Body.String())
	}
	afterRec := httptest.NewRecorder()
	handlePolicyRulesGet(afterRec, httptest.NewRequest(http.MethodGet, "/api/policy/rules", nil))
	if !strings.Contains(afterRec.Body.String(), `"selection":"legacy-implicit"`) {
		t.Fatalf("rollback did not restore legacy compatibility: %s", afterRec.Body.String())
	}
}

func TestPolicyConsoleMutationsRefuseInvocationOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, "invocation.json")
	if err := os.WriteFile(path, []byte(`{"rules":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_RULES", path)
	getRec := httptest.NewRecorder()
	handlePolicyRulesGet(getRec, httptest.NewRequest(http.MethodGet, "/api/policy/rules", nil))
	var active struct {
		Digest     string `json:"digest"`
		StateToken string `json:"state_token"`
	}
	if err := json.Unmarshal(getRec.Body.Bytes(), &active); err != nil {
		t.Fatal(err)
	}

	previewRec := httptest.NewRecorder()
	handlePolicySelectionPreview(previewRec, httptest.NewRequest(http.MethodPost,
		"/api/policy/rules/selection/preview", strings.NewReader(`{"source":"starter"}`)))
	var preview struct {
		ProposedDigest string `json:"proposed_digest"`
	}
	if err := json.Unmarshal(previewRec.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	selectBody, _ := json.Marshal(map[string]string{
		"source": "starter", "expected_active_digest": active.Digest,
		"expected_state_token": active.StateToken, "acknowledged_digest": preview.ProposedDigest,
	})
	selectRec := httptest.NewRecorder()
	handlePolicySelection(selectRec, httptest.NewRequest(http.MethodPost,
		"/api/policy/rules/selection", bytes.NewReader(selectBody)))
	if selectRec.Code != http.StatusConflict || !strings.Contains(selectRec.Body.String(), "CG_RULES invocation override") {
		t.Fatalf("console selection under invocation: %d: %s", selectRec.Code, selectRec.Body.String())
	}

	saveReq := httptest.NewRequest(http.MethodPut, "/api/policy/rules", strings.NewReader(`{"rules":[]}`))
	saveReq.Header.Set("If-Match", active.StateToken)
	saveRec := httptest.NewRecorder()
	handlePolicyRulesPut(saveRec, saveReq)
	if saveRec.Code != http.StatusConflict || !strings.Contains(saveRec.Body.String(), "CG_RULES invocation override") {
		t.Fatalf("console save under invocation: %d: %s", saveRec.Code, saveRec.Body.String())
	}
}
