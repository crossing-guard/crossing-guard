package daemon

import (
	"encoding/json"
	"errors"
	"net/http"

	"crossing-guard/internal/detectorselection"
)

var policyDetectorRuntime struct {
	dataRoot, invocationPath string
	ledger, governor         *detectorselection.LoadedDetectors
}

func configurePolicyDetectorRuntime(dataRoot, invocationPath string, ledger, governor *detectorselection.LoadedDetectors) {
	policyDetectorRuntime.dataRoot = dataRoot
	policyDetectorRuntime.invocationPath = invocationPath
	policyDetectorRuntime.ledger = ledger
	policyDetectorRuntime.governor = governor
}

func policyDetectorRoot() string {
	if policyDetectorRuntime.dataRoot != "" {
		return policyDetectorRuntime.dataRoot
	}
	return defaultDataDir()
}

func detectorRuntimeFact(loaded *detectorselection.LoadedDetectors) any {
	if loaded == nil {
		return nil
	}
	return map[string]any{"surface": loaded.Surface, "digest": loaded.Digest,
		"origin": loaded.Origin, "selection": loaded.Selection, "path": loaded.Path,
		"detector_count": len(loaded.Detectors)}
}

func handlePolicyDetectorsGet(w http.ResponseWriter, r *http.Request) {
	root := policyDetectorRoot()
	invocation := policyDetectorRuntime.invocationPath
	ledger, ledgerErr := detectorselection.ResolveDetectors(root, detectorselection.DetectorSurfaceLedger, invocation)
	governor, governorErr := detectorselection.ResolveDetectors(root, detectorselection.DetectorSurfaceGovernor, invocation)
	durableLedger, durableLedgerErr := detectorselection.ResolveDetectors(root, detectorselection.DetectorSurfaceLedger, "")
	durableGovernor, durableErr := detectorselection.ResolveDetectors(root, detectorselection.DetectorSurfaceGovernor, "")
	var recoveryToken string
	var recoveryInfo *detectorselection.DetectorSelectionInfo
	var recoveryErr error
	if durableLedgerErr != nil || durableErr != nil {
		recoveryToken, recoveryInfo, recoveryErr = detectorselection.DetectorSelectionStateToken(root, detectorselection.DetectorSurfaceGovernor)
	}
	if ledgerErr != nil || governorErr != nil {
		writeJSON(w, map[string]any{"available": false, "ledger_error": errorText(ledgerErr),
			"governor_error": errorText(governorErr), "runtime_ledger": detectorRuntimeFact(policyDetectorRuntime.ledger),
			"runtime_governor":     detectorRuntimeFact(policyDetectorRuntime.governor),
			"recovery_state_token": recoveryToken, "invalid_selection": recoveryInfo,
			"recovery_error": errorText(recoveryErr), "durable_ledger_error": errorText(durableLedgerErr),
			"durable_error":       errorText(durableErr),
			"invocation_override": invocation != "", "invocation_path": invocation})
		return
	}
	writeJSON(w, map[string]any{"available": true, "ledger": ledger, "governor": governor,
		"durable_ledger": durableLedger, "durable_ledger_error": errorText(durableLedgerErr),
		"durable_governor": durableGovernor, "durable_error": errorText(durableErr),
		"configuration_available": durableLedgerErr == nil && durableErr == nil,
		"recovery_state_token":    recoveryToken, "invalid_selection": recoveryInfo,
		"recovery_error":        errorText(recoveryErr),
		"configured_divergence": durableLedgerErr == nil && durableErr == nil && durableLedger.Digest != durableGovernor.Digest,
		"runtime_ledger":        detectorRuntimeFact(policyDetectorRuntime.ledger),
		"runtime_governor":      detectorRuntimeFact(policyDetectorRuntime.governor),
		"restart_required":      runtimeDetectorRestartRequired(ledger, governor),
		"invocation_override":   invocation != "", "invocation_path": invocation})
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func runtimeDetectorRestartRequired(ledger, governor *detectorselection.LoadedDetectors) bool {
	return policyDetectorRuntime.ledger == nil || policyDetectorRuntime.governor == nil ||
		policyDetectorRuntime.ledger.Digest != ledger.Digest || policyDetectorRuntime.governor.Digest != governor.Digest
}

func handlePolicyDetectorSelectionPreview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source string `json:"source"`
		Path   string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	preview, err := detectorselection.PreviewDetectorSelection(policyDetectorRoot(), detectorselection.DetectorSurfaceGovernor, req.Source, req.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	writeJSON(w, preview)
}

func handlePolicyDetectorSelection(w http.ResponseWriter, r *http.Request) {
	var req detectorselection.DetectorSelectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.DataRoot = policyDetectorRoot()
	req.Surface = detectorselection.DetectorSurfaceGovernor
	req.Selector = "console"
	result, err := detectorselection.SelectDetectors(req)
	if err != nil {
		var conflict *detectorselection.DetectorConflictError
		if errors.As(err, &conflict) {
			http.Error(w, err.Error(), http.StatusConflict)
		} else {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		}
		return
	}
	displaced := policyDetectorRuntime.invocationPath != ""
	note := "selection is durable; restart the daemon so every cached detector surface activates the same digest"
	if displaced {
		note = "selection is durable but remains displaced by the invocation overlay; restart without --detectors/CG_DETECTORS to activate it"
	}
	writeJSON(w, map[string]any{"selection": result, "restart_required": true,
		"displaced_by_invocation": displaced, "invocation_path": policyDetectorRuntime.invocationPath,
		"note": note})
}

func handlePolicyDetectorUnselect(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ExpectedStateToken string `json:"expected_state_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	active, archive, err := detectorselection.UnselectDetectors(policyDetectorRoot(), detectorselection.DetectorSurfaceGovernor,
		req.ExpectedStateToken, "console")
	if err != nil {
		var conflict *detectorselection.DetectorConflictError
		if errors.As(err, &conflict) {
			http.Error(w, err.Error(), http.StatusConflict)
		} else {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		}
		return
	}
	displaced := policyDetectorRuntime.invocationPath != ""
	note := "selection is archived; restart the daemon to activate the cohort baseline"
	if displaced {
		note = "selection is archived; restart without --detectors/CG_DETECTORS to activate the cohort baseline"
	}
	writeJSON(w, map[string]any{"active": active, "archive": archive, "restart_required": true,
		"displaced_by_invocation": displaced, "invocation_path": policyDetectorRuntime.invocationPath,
		"note": note})
}
