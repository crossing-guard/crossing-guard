package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"crossing-guard/internal/detectorselection"
)

func detectorPolicyFixture(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "fresh")
	ledger, err := detectorselection.ResolveDetectors(root, detectorselection.DetectorSurfaceLedger, "")
	if err != nil {
		t.Fatal(err)
	}
	governor, err := detectorselection.ResolveDetectors(root, detectorselection.DetectorSurfaceGovernor, "")
	if err != nil {
		t.Fatal(err)
	}
	configurePolicyDetectorRuntime(root, "", ledger, governor)
	return root
}

func TestDetectorPolicyStatusPreviewSelectConflictAndUnselect(t *testing.T) {
	detectorPolicyFixture(t)
	statusRec := httptest.NewRecorder()
	handlePolicyDetectorsGet(statusRec, httptest.NewRequest(http.MethodGet, "/api/policy/detectors", nil))
	if statusRec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", statusRec.Code, statusRec.Body.String())
	}
	var status struct {
		Available          bool                               `json:"available"`
		ConfigurationReady bool                               `json:"configuration_available"`
		Governor           detectorselection.LoadedDetectors  `json:"governor"`
		DurableGovernor    *detectorselection.LoadedDetectors `json:"durable_governor"`
		RuntimeGovernor    map[string]any                     `json:"runtime_governor"`
		RestartRequired    bool                               `json:"restart_required"`
	}
	if err := json.Unmarshal(statusRec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if !status.Available || !status.ConfigurationReady || status.Governor.Origin != "mechanism-floor" ||
		status.Governor.DetectorCount != 30 || status.DurableGovernor == nil ||
		status.DurableGovernor.Digest != status.Governor.Digest ||
		status.RuntimeGovernor["digest"] != status.Governor.Digest || status.RestartRequired {
		t.Fatalf("fresh status = %+v", status)
	}

	previewRec := httptest.NewRecorder()
	handlePolicyDetectorSelectionPreview(previewRec, httptest.NewRequest(http.MethodPost,
		"/api/policy/detectors/selection/preview", bytes.NewBufferString(`{"source":"starter"}`)))
	if previewRec.Code != http.StatusOK {
		t.Fatalf("preview=%d body=%s", previewRec.Code, previewRec.Body.String())
	}
	var preview detectorselection.DetectorPreview
	if err := json.Unmarshal(previewRec.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	request := detectorselection.DetectorSelectRequest{Source: "starter",
		ExpectedActiveDigest: preview.ExpectedActiveDigest, ExpectedStateToken: preview.ExpectedStateToken,
		AcknowledgedDigest: preview.ProposedDigest}
	body, _ := json.Marshal(request)
	selectRec := httptest.NewRecorder()
	handlePolicyDetectorSelection(selectRec, httptest.NewRequest(http.MethodPost,
		"/api/policy/detectors/selection", bytes.NewReader(body)))
	if selectRec.Code != http.StatusOK {
		t.Fatalf("select=%d body=%s", selectRec.Code, selectRec.Body.String())
	}

	staleRec := httptest.NewRecorder()
	handlePolicyDetectorSelection(staleRec, httptest.NewRequest(http.MethodPost,
		"/api/policy/detectors/selection", bytes.NewReader(body)))
	if staleRec.Code != http.StatusConflict {
		t.Fatalf("stale select=%d body=%s", staleRec.Code, staleRec.Body.String())
	}

	selectedRec := httptest.NewRecorder()
	handlePolicyDetectorsGet(selectedRec, httptest.NewRequest(http.MethodGet, "/api/policy/detectors", nil))
	if err := json.Unmarshal(selectedRec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if !status.Governor.Selected || status.Governor.DetectorCount != 70 || !status.RestartRequired {
		t.Fatalf("selected status = %+v", status)
	}
	if status.DurableGovernor == nil || !status.DurableGovernor.Selected ||
		status.RuntimeGovernor["detector_count"] != float64(30) {
		t.Fatalf("configured/runtime distinction lost after selection: %+v", status)
	}
	unselectBody, _ := json.Marshal(map[string]string{"expected_state_token": status.Governor.StateToken})
	unselectRec := httptest.NewRecorder()
	handlePolicyDetectorUnselect(unselectRec, httptest.NewRequest(http.MethodPost,
		"/api/policy/detectors/selection/unselect", bytes.NewReader(unselectBody)))
	if unselectRec.Code != http.StatusOK {
		t.Fatalf("unselect=%d body=%s", unselectRec.Code, unselectRec.Body.String())
	}
}

func TestDetectorPolicyInvocationDisplacementAndUnselectResponse(t *testing.T) {
	root := filepath.Join(t.TempDir(), "fresh")
	overlay := filepath.Join(t.TempDir(), "overlay.json")
	if err := os.WriteFile(overlay, []byte(`{"detectors":[{"id":"risk.rm-rf","disabled":true}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ledger, err := detectorselection.ResolveDetectors(root, detectorselection.DetectorSurfaceLedger, overlay)
	if err != nil {
		t.Fatal(err)
	}
	governor, err := detectorselection.ResolveDetectors(root, detectorselection.DetectorSurfaceGovernor, overlay)
	if err != nil {
		t.Fatal(err)
	}
	configurePolicyDetectorRuntime(root, overlay, ledger, governor)

	preview, err := detectorselection.PreviewDetectorSelection(root, detectorselection.DetectorSurfaceGovernor, "starter", "")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(detectorselection.DetectorSelectRequest{Source: "starter",
		ExpectedActiveDigest: preview.ExpectedActiveDigest, ExpectedStateToken: preview.ExpectedStateToken,
		AcknowledgedDigest: preview.ProposedDigest})
	selectedRec := httptest.NewRecorder()
	handlePolicyDetectorSelection(selectedRec, httptest.NewRequest(http.MethodPost,
		"/api/policy/detectors/selection", bytes.NewReader(body)))
	var selectedResponse struct {
		Displaced      bool   `json:"displaced_by_invocation"`
		InvocationPath string `json:"invocation_path"`
	}
	if selectedRec.Code != http.StatusOK || json.Unmarshal(selectedRec.Body.Bytes(), &selectedResponse) != nil ||
		!selectedResponse.Displaced || selectedResponse.InvocationPath != overlay {
		t.Fatalf("invocation selection response=%d %s", selectedRec.Code, selectedRec.Body.String())
	}

	statusRec := httptest.NewRecorder()
	handlePolicyDetectorsGet(statusRec, httptest.NewRequest(http.MethodGet, "/api/policy/detectors", nil))
	var status struct {
		InvocationOverride bool                               `json:"invocation_override"`
		Governor           detectorselection.LoadedDetectors  `json:"governor"`
		DurableGovernor    *detectorselection.LoadedDetectors `json:"durable_governor"`
	}
	if err := json.Unmarshal(statusRec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if !status.InvocationOverride || status.Governor.Selection != "invocation-path" ||
		status.Governor.Displaced == nil || status.DurableGovernor == nil || !status.DurableGovernor.Selected {
		t.Fatalf("invocation/durable status = %+v", status)
	}
	unselectBody, _ := json.Marshal(map[string]string{"expected_state_token": status.DurableGovernor.StateToken})
	unselectRec := httptest.NewRecorder()
	handlePolicyDetectorUnselect(unselectRec, httptest.NewRequest(http.MethodPost,
		"/api/policy/detectors/selection/unselect", bytes.NewReader(unselectBody)))
	var unselectResponse struct {
		Displaced      bool   `json:"displaced_by_invocation"`
		InvocationPath string `json:"invocation_path"`
	}
	if unselectRec.Code != http.StatusOK || json.Unmarshal(unselectRec.Body.Bytes(), &unselectResponse) != nil ||
		!unselectResponse.Displaced || unselectResponse.InvocationPath != overlay {
		t.Fatalf("invocation unselect response=%d %s", unselectRec.Code, unselectRec.Body.String())
	}
}

func TestDetectorPolicyUnavailableConfigurationRetainsRuntimeFactsAndRecovery(t *testing.T) {
	root := detectorPolicyFixture(t)
	preview, err := detectorselection.PreviewDetectorSelection(root, detectorselection.DetectorSurfaceGovernor, "starter", "")
	if err != nil {
		t.Fatal(err)
	}
	selected, err := detectorselection.SelectDetectors(detectorselection.DetectorSelectRequest{DataRoot: root,
		Surface: detectorselection.DetectorSurfaceGovernor, Source: "starter", Selector: "console",
		ExpectedActiveDigest: preview.ExpectedActiveDigest, ExpectedStateToken: preview.ExpectedStateToken,
		AcknowledgedDigest: preview.ProposedDigest})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(selected.Active.Path, []byte(`{"detectors":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	statusRec := httptest.NewRecorder()
	handlePolicyDetectorsGet(statusRec, httptest.NewRequest(http.MethodGet, "/api/policy/detectors", nil))
	var status struct {
		Available          bool                                     `json:"available"`
		RuntimeGovernor    map[string]any                           `json:"runtime_governor"`
		RecoveryStateToken string                                   `json:"recovery_state_token"`
		InvalidSelection   *detectorselection.DetectorSelectionInfo `json:"invalid_selection"`
	}
	if err := json.Unmarshal(statusRec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Available || status.RuntimeGovernor["detector_count"] != float64(30) ||
		status.RecoveryStateToken == "" || status.InvalidSelection == nil {
		t.Fatalf("failure/runtime/recovery facts = %+v body=%s", status, statusRec.Body.String())
	}
	unselectBody, _ := json.Marshal(map[string]string{"expected_state_token": status.RecoveryStateToken})
	unselectRec := httptest.NewRecorder()
	handlePolicyDetectorUnselect(unselectRec, httptest.NewRequest(http.MethodPost,
		"/api/policy/detectors/selection/unselect", bytes.NewReader(unselectBody)))
	if unselectRec.Code != http.StatusOK {
		t.Fatalf("invalid selection recovery=%d %s", unselectRec.Code, unselectRec.Body.String())
	}
}
