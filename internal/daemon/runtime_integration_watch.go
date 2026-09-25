package daemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"crossing-guard/internal/guardcli"
	"crossing-guard/internal/rulebook"
)

func handleRuntimeIntegrationWatchStart(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var request struct {
		Surface string `json:"surface"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request.Surface == "" {
		writeRuntimeIntegrationError(w, http.StatusBadRequest, "surface_required",
			"choose a provider verification surface", nil)
		return
	}
	executable, err := runtimeIntegrationExecutable()
	if err != nil {
		writeRuntimeIntegrationError(w, http.StatusInternalServerError, "binary_unavailable",
			"Crossing Guard could not resolve its installed binary", nil)
		return
	}
	status, err := guardcli.RuntimeConnection(r.PathValue("runtime"), executable)
	if err != nil {
		writeGuardConnectionError(w, err, nil)
		return
	}
	if !status.Consented || !status.Attached || !status.Current || !status.BinaryPresent {
		writeRuntimeIntegrationError(w, http.StatusConflict, "connection_not_current",
			"the runtime must have explicit connection consent, a current hook, and a live hook binary before watching", nil)
		return
	}
	surfaceLabel := runtimeSurfaceLabel(status.Descriptor, request.Surface)
	if surfaceLabel == "" {
		writeRuntimeIntegrationError(w, http.StatusBadRequest, "unknown_surface",
			"the provider did not advertise that verification surface", nil)
		return
	}
	if governor == nil {
		writeRuntimeIntegrationError(w, http.StatusConflict, "capture_unavailable",
			"the local capture store is unavailable; start the daemon before watching", nil)
		return
	}
	afterEventID, err := governor.ix.RuntimeEventCursor(status.Descriptor.Runtime)
	if err != nil {
		writeRuntimeIntegrationError(w, http.StatusInternalServerError, "capture_unavailable",
			"Crossing Guard could not read the local event boundary", nil)
		return
	}
	watchToken, expiresAt, err := runtimeIntegrationPreviews.issueWatch(runtimeIntegrationWatchEntry{
		Runtime: status.Descriptor.Runtime, DisplayName: status.Descriptor.DisplayName, Surface: request.Surface,
		SurfaceLabel: surfaceLabel, AfterEventID: afterEventID, ScanEventID: afterEventID,
		ConnectionRevision: status.Revision,
	})
	if err != nil {
		writeRuntimeIntegrationError(w, http.StatusInternalServerError, "token_unavailable",
			"Crossing Guard could not create a watch token", nil)
		return
	}
	canaryCommand := "echo " + rulebook.CanaryMarker
	canaryVerdict, canaryErr := guardcli.CheckCommand(canaryCommand)
	canaryAvailable := canaryErr == nil && canaryVerdict.Decision == "deny"
	if !canaryAvailable {
		canaryCommand = ""
	}
	writeJSON(w, map[string]any{
		"watch_token": watchToken, "expires_at": expiresAt.UTC().Format(time.RFC3339),
		"status": "waiting", "surface": request.Surface, "surface_label": surfaceLabel,
		"surface_evidence": "user-selected; the native provider payload has not proved this label",
		"after_event_id":   afterEventID, "verification_steps": status.Descriptor.VerificationSteps,
		"canary_available": canaryAvailable, "canary_command": canaryCommand,
		"note": "Crossing Guard is only watching its local event log. Open and use the provider yourself when ready.",
	})
}

func runtimeSurfaceLabel(descriptor guardcli.RuntimeConnectionDescriptor, surfaceID string) string {
	for _, surface := range descriptor.Surfaces {
		if surface.ID == surfaceID {
			return surface.Label
		}
	}
	return ""
}

func handleRuntimeIntegrationWatchGet(w http.ResponseWriter, r *http.Request) {
	watch, err := runtimeIntegrationPreviews.watch(r.PathValue("token"), r.PathValue("runtime"))
	if err != nil {
		writeRuntimeIntegrationError(w, http.StatusGone, "watch_unavailable", err.Error(), nil)
		return
	}
	result, err := runtimeIntegrationWatchResult(r.PathValue("token"), watch)
	if err != nil {
		writeRuntimeIntegrationError(w, http.StatusInternalServerError, "capture_unavailable",
			"Crossing Guard could not read events for this watch", nil)
		return
	}
	writeJSON(w, result)
}

func handleRuntimeIntegrationWatchConfirmVisible(w http.ResponseWriter, r *http.Request) {
	watch, err := runtimeIntegrationPreviews.watch(r.PathValue("token"), r.PathValue("runtime"))
	if err != nil {
		writeRuntimeIntegrationError(w, http.StatusGone, "watch_unavailable", err.Error(), nil)
		return
	}
	result, err := runtimeIntegrationWatchResult(r.PathValue("token"), watch)
	if err != nil {
		writeRuntimeIntegrationError(w, http.StatusInternalServerError, "capture_unavailable",
			"Crossing Guard could not read events for this watch", nil)
		return
	}
	if result["status"] != "denial_returned" && result["status"] != "enforcement_verified" {
		writeRuntimeIntegrationError(w, http.StatusConflict, "denial_not_observed",
			"a matching harmless denial must be returned before visible blocking can be confirmed", nil)
		return
	}
	watch, err = runtimeIntegrationPreviews.confirmVisible(r.PathValue("token"), r.PathValue("runtime"))
	if err != nil {
		writeRuntimeIntegrationError(w, http.StatusGone, "watch_unavailable", err.Error(), nil)
		return
	}
	result, err = runtimeIntegrationWatchResult(r.PathValue("token"), watch)
	if err != nil {
		writeRuntimeIntegrationError(w, http.StatusInternalServerError, "capture_unavailable",
			"Crossing Guard could not read events for this watch", nil)
		return
	}
	writeJSON(w, result)
}

func runtimeIntegrationWatchResult(token string, watch runtimeIntegrationWatchEntry) (map[string]any, error) {
	response := map[string]any{
		"status": "waiting", "surface": watch.Surface, "surface_label": watch.SurfaceLabel,
		"surface_evidence": "user-selected; the native provider payload has not proved this label",
		"after_event_id":   watch.AfterEventID,
	}
	if governor == nil {
		return nil, errors.New("capture unavailable")
	}
	executable, err := runtimeIntegrationExecutable()
	if err != nil {
		return nil, err
	}
	connection, err := guardcli.RuntimeConnection(watch.Runtime, executable)
	if err != nil {
		return nil, err
	}
	if connection.Revision != watch.ConnectionRevision || !connection.Current || !connection.BinaryPresent {
		response["status"] = "needs_attention"
		response["note"] = "The runtime connection changed after this watch started; start a new preview/watch before relying on later events."
		return response, nil
	}
	events, err := governor.ix.RuntimeEventsAfter(watch.Runtime, watch.ScanEventID, 100)
	if err != nil {
		return nil, err
	}
	if len(events) > 0 {
		observations := make([]runtimeIntegrationWatchObservation, 0, len(events))
		for _, event := range events {
			observations = append(observations, runtimeIntegrationWatchObservation{
				EventID: event.ID, SessionID: event.SessionID, Tool: event.Tool, Decision: event.Decision,
				Denied: event.Decision == "deny" && runtimeEventHasExactCanary(event.Tags),
			})
		}
		watch, err = runtimeIntegrationPreviews.recordWatchEvents(token, watch.Runtime, watch.ScanEventID, observations)
		if err != nil {
			return nil, err
		}
	}
	if watch.ObservedEventID > 0 {
		response["status"] = "decision_firing_observed"
		response["event_id"] = watch.ObservedEventID
		response["session_id"] = watch.ObservedSessionID
		response["tool"] = watch.ObservedTool
		response["decision"] = watch.ObservedDecision
	}
	if watch.DeniedEventID > 0 {
		response["status"] = "denial_returned"
		response["event_id"] = watch.DeniedEventID
		response["session_id"] = watch.DeniedSessionID
		response["tool"] = watch.DeniedTool
		response["decision"] = watch.DeniedDecision
		response["note"] = "Crossing Guard returned a hard denial. Confirm visible blocking only if you personally saw " + watch.DisplayName + " honor it."
	}
	if watch.VisibleConfirmed && response["status"] == "denial_returned" {
		response["status"] = "enforcement_verified"
		response["visible_blocking"] = "owner-confirmed"
		response["note"] = "The local denial is recorded and visible blocking was confirmed by the owner for the selected surface."
	}
	return response, nil
}

func runtimeEventHasExactCanary(tagsJSON string) bool {
	var tags []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if json.Unmarshal([]byte(tagsJSON), &tags) != nil {
		return false
	}
	want := "echo " + rulebook.CanaryMarker
	for _, tag := range tags {
		if strings.EqualFold(tag.Key, "command") && strings.TrimSpace(tag.Value) == want {
			return true
		}
	}
	return false
}
