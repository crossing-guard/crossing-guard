package daemon

// Re-pin (team rest-of-release plan §4.3, OD-9): a pinned device that meets a bundle
// signed by another key stops and shows both fingerprints. A person compares the
// presented one with the team console's Policy page and confirms; only then is the
// pin replaced. The request must carry the presented fingerprint, so a caller cannot
// trust a key it has not been shown, and the chained event records which surface
// asked. Until a device re-pins, what it adopted keeps working to its expiry.

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// teamRepinRequest is POST /api/team/org-key/repin's body.
type teamRepinRequest struct {
	// Fingerprint is the PRESENTED key's fingerprint, as the mismatch state shows it.
	Fingerprint string `json:"fingerprint"`
	Surface     string `json:"surface"`
}

// teamRepinResponse is the route's answer: the new pin and the key it replaced.
type teamRepinResponse struct {
	PinnedKey           orgKeyPin `json:"pinned_org_key"`
	ReplacedKeyID       string    `json:"replaced_key_id"`
	ReplacedFingerprint string    `json:"replaced_fingerprint"`
}

// repinOrgKey replaces the pin with the presented key whose fingerprint the caller
// names. It refuses when no other key is presented, and when the fingerprint is not
// the presented one — the pin is then unchanged.
func (t *teamLinker) repinOrgKey(fingerprint, surface string) (teamRepinResponse, error) {
	t.adoptMu.Lock()
	defer t.adoptMu.Unlock()
	t.mu.Lock()
	linked := t.state == teamLinked
	mismatches := append([]teamKeyMismatch(nil), t.keyMismatches...)
	gen := t.gen
	t.mu.Unlock()
	if !linked {
		return teamRepinResponse{}, adoptionRefusal(http.StatusConflict, "the device is not linked; there is no organization key to trust")
	}
	if len(mismatches) == 0 {
		return teamRepinResponse{}, adoptionRefusal(http.StatusConflict, "the server presents no key other than the pinned one; there is nothing to trust")
	}
	wanted := strings.ToUpper(strings.TrimSpace(fingerprint))
	var presented *teamKeyMismatch
	for index := range mismatches {
		if mismatches[index].PresentedFingerprint == wanted {
			presented = &mismatches[index]
		}
	}
	if presented == nil {
		return teamRepinResponse{}, adoptionRefusal(http.StatusConflict,
			"that is not the fingerprint of a key the server presents; the pin is unchanged — compare the presented fingerprint in Settings → Team with the team console's Policy page")
	}
	// The disk record is what verification reads: the pin on disk must still be the
	// one the mismatch was found against.
	previous := loadPinnedOrgKey(t.storeDir)
	if previous.KeyID != presented.PinnedKeyID {
		return teamRepinResponse{}, adoptionRefusal(http.StatusConflict, "the pinned key changed since the mismatch was found; pull again")
	}
	pin := orgKeyPin{KeyID: presented.PresentedKeyID, PublicKey: presented.presentedPublicKey,
		Fingerprint: presented.PresentedFingerprint, PinnedAt: t.now().UTC().Format(time.RFC3339)}
	if err := savePinnedOrgKey(t.storeDir, pin, previous.KeyID); err != nil {
		return teamRepinResponse{}, adoptionRefusal(http.StatusInternalServerError, "the pin could not be recorded: %v", err)
	}
	if !t.keepRecordedPin(pin, gen) {
		// The link ended while the pin was being recorded: the pin went with it, as a
		// first pin does (recordFirstPin).
		return teamRepinResponse{}, adoptionRefusal(http.StatusConflict, "the device was unlinked while the key was being trusted; nothing is pinned")
	}
	t.mu.Lock()
	t.keyMismatches = nil
	t.mu.Unlock()
	t.linkEvent("team.org-key.repinned", "organization key re-pinned from "+previous.KeyID+" ("+previous.Fingerprint+") to "+
		pin.KeyID+" ("+pin.Fingerprint+") after a person confirmed the presented fingerprint; asked from the "+surface)
	return teamRepinResponse{PinnedKey: pin, ReplacedKeyID: previous.KeyID, ReplacedFingerprint: previous.Fingerprint}, nil
}

// handleTeamRepin is POST /api/team/org-key/repin.
func handleTeamRepin(w http.ResponseWriter, r *http.Request) {
	if team == nil {
		writeGovernorUnavailable(w)
		return
	}
	var req teamRepinRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	err := dec.Decode(&req)
	surfaceOK := req.Surface == teamSurfaceConsole || req.Surface == teamSurfaceCommand
	if err != nil || strings.TrimSpace(req.Fingerprint) == "" || !surfaceOK {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, teamErrorResponse{Error: "body must be {\"fingerprint\": \"<the presented key's fingerprint>\", \"surface\": \"console|command\"}"})
		return
	}
	out, err := team.repinOrgKey(req.Fingerprint, req.Surface)
	if err != nil {
		writeAdoptionRefusal(w, err)
		return
	}
	// The re-signed bundles can refresh now; a failed pull is the next tick's business.
	team.pullLayersOnce()
	writeJSON(w, out)
}

// registerTeamPublishingRoutes registers the re-pin and bundle build routes.
func registerTeamPublishingRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/team/org-key/repin", handleTeamRepin)
	mux.HandleFunc("POST /api/team/bundles/build", handleTeamBundleBuild)
}
