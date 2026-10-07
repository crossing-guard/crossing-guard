package daemon

// The adoption routes (team plan §5.16.3; team rest-of-release plan §4.1 decision 6):
// GET /api/team/layers reads the offer; POST /api/team/layers/adopt and /unadopt are
// the explicit local actions after a shown diff — availability is not activation.
// Both update the offer in the same call, so it tells the truth at once. Typed
// responses only (ADR 0022); documented in loopback-api.md.

import (
	"encoding/json"
	"net/http"

	"crossing-guard/internal/orchestration/profilefs"
	"crossing-guard/internal/rulebook"
	"crossing-guard/store"
)

// teamAdoptedBundle is one adopted-bundle record as the routes show it: the record,
// the agents its profile adoption lists, and what its expiry means right now. Records
// of an organization this device is no longer linked to stay listed, to their expiry
// and until Un-adopt.
type teamAdoptedBundle struct {
	rulebook.AdoptedBundle
	// Linked is true for a record of the organization this device is linked to.
	Linked bool `json:"linked"`
	// Expired is true past expires_at: fail-open rules have stopped applying and the
	// bundle's places are held. Blocking is true when the bundle is also fail-closed:
	// governed tool calls in its scope are denied until a refresh or Un-adopt.
	Expired  bool                         `json:"expired"`
	Blocking bool                         `json:"blocking"`
	Agents   []profilefs.AdoptionDocument `json:"agents"`
	// AgentsBundleID is the bundle id the profile adoption record names. It differs
	// from bundle_id only for an adoption that stopped part-way; Adopt completes it.
	AgentsBundleID string `json:"agents_bundle_id,omitempty"`
	// RuleCount is how many rules the record's staged rulebook holds; 0 for a bundle
	// with no rulebook or one whose staged document cannot be read. It is read from
	// this device's own copy, so it is known when the device is unlinked and nothing
	// is offered.
	RuleCount int `json:"rule_count"`
}

// teamLayersState is GET /api/team/layers' body.
type teamLayersState struct {
	Available []teamLayerEntry `json:"available"`
	// Adopted maps each scope to its record — the linked organization's when two
	// organizations hold one scope. AdoptedBundles lists every record.
	Adopted        map[string]teamAdoptedBundle `json:"adopted"`
	AdoptedBundles []teamAdoptedBundle          `json:"adopted_bundles"`
	Unusable       []teamUnusableBundle         `json:"unusable"`
	KeyMismatches  []teamKeyMismatch            `json:"key_mismatches"`
	PinnedKey      orgKeyPin                    `json:"pinned_org_key"`
	Reasons        []string                     `json:"reasons,omitempty"`
}

// layersState reads the offer and every adoption record.
func (t *teamLinker) layersState() (teamLayersState, error) {
	t.mu.Lock()
	out := teamLayersState{Available: append([]teamLayerEntry{}, t.available...),
		Unusable:      append([]teamUnusableBundle{}, t.unusable...),
		KeyMismatches: append([]teamKeyMismatch{}, t.keyMismatches...),
		PinnedKey:     t.pinnedKey, Reasons: t.layerReasons,
		Adopted: map[string]teamAdoptedBundle{}, AdoptedBundles: []teamAdoptedBundle{}}
	linked := t.doc.Organization.ID
	now := t.now()
	t.mu.Unlock()
	layers, err := rulebook.LoadLayers(t.storeDir)
	if err != nil {
		return teamLayersState{}, err
	}
	owner, err := t.profileOwner()
	if err != nil {
		return teamLayersState{}, err
	}
	for _, record := range layers.Adopted {
		expired := now.After(record.ExpiresAt)
		item := teamAdoptedBundle{AdoptedBundle: record, Linked: linked != "" && record.OrganizationID == linked,
			Expired: expired, Blocking: expired && record.FailureMode == rulebook.FailClosed,
			Agents: []profilefs.AdoptionDocument{}}
		if record.RulebookDigest != "" {
			if staged, stagedErr := readStagedRulebook(t.storeDir, record.RulebookDigest); stagedErr == nil {
				item.RuleCount = len(staged.Rules)
			}
		}
		if record.OrganizationID != "" {
			if adoption, found, adoptionErr := owner.Adoption(record.OrganizationID, record.Scope); adoptionErr == nil && found {
				item.Agents, item.AgentsBundleID = adoption.Documents, adoption.BundleID
			}
		}
		out.AdoptedBundles = append(out.AdoptedBundles, item)
		if existing, taken := out.Adopted[record.Scope]; !taken || !existing.Linked {
			out.Adopted[record.Scope] = item
		}
	}
	return out, nil
}

func handleTeamLayers(w http.ResponseWriter, _ *http.Request) {
	if team == nil {
		writeGovernorUnavailable(w)
		return
	}
	state, err := team.layersState()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, teamErrorResponse{Error: "the adoption records could not be read"})
		return
	}
	writeJSON(w, state)
}

// teamAdoptResponse is POST /api/team/layers/adopt's body: the adopted-bundle record
// as stored, the adopted bundle's own id and revision, and the offer as it reads now.
type teamAdoptResponse struct {
	Adopted  rulebook.AdoptedBundle `json:"adopted"`
	BundleID string                 `json:"bundle_id"`
	Revision int64                  `json:"revision"`
	Offer    teamLayerEntry         `json:"offer"`
}

// teamUnadoptResponse is POST /api/team/layers/unadopt's body: the scope whose
// bundle no longer applies, the bundle that was adopted, the places turned off, and
// the scope's offers as they read now.
type teamUnadoptResponse struct {
	Unadopted      string                  `json:"unadopted"`
	OrganizationID string                  `json:"organization_id,omitempty"`
	BundleID       string                  `json:"bundle_id,omitempty"`
	Revision       int64                   `json:"revision,omitempty"`
	WasAdopted     bool                    `json:"was_adopted"`
	PlacesOff      store.AdoptionPlacesOff `json:"places_off"`
	Offers         []teamLayerEntry        `json:"offers"`
}

type adoptLayerRequest struct {
	Scope  string `json:"scope"`
	Digest string `json:"digest"` // the bundle id whose signed bytes were verified
	// StateToken is the offer's state_token as the caller was shown it. Required: an
	// adoption is an act on an offer someone read, and an offer that moved since is
	// refused. A caller reads GET /api/team/layers first, as the command does.
	StateToken string `json:"state_token"`
	Surface    string `json:"surface,omitempty"`
}

type unadoptLayerRequest struct {
	Scope          string `json:"scope"`
	OrganizationID string `json:"organization_id,omitempty"`
	Surface        string `json:"surface,omitempty"`
}

// requestSurface is the surface an adoption act came from; a caller that does not
// say is the console.
func requestSurface(surface string) (string, bool) {
	switch surface {
	case "", teamSurfaceConsole:
		return teamSurfaceConsole, true
	case teamSurfaceCommand:
		return teamSurfaceCommand, true
	}
	return "", false
}

func writeAdoptionRefusal(w http.ResponseWriter, err error) {
	status, message := http.StatusInternalServerError, err.Error()
	if refusal, ok := asAdoptionProblem(err); ok {
		status, message = refusal.status, refusal.message
	}
	w.WriteHeader(status)
	writeJSON(w, teamErrorResponse{Error: message})
}

// handleTeamAdopt adopts a SPECIFIC verified bundle id: each document goes to its
// owner in the crash order (bytes, selection, profile adoption record, adopted-bundle
// record), and the answer carries the offer as it reads after the adoption.
func handleTeamAdopt(w http.ResponseWriter, r *http.Request) {
	if team == nil {
		writeGovernorUnavailable(w)
		return
	}
	var req adoptLayerRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	surface, surfaceOK := "", false
	if err := dec.Decode(&req); err == nil {
		surface, surfaceOK = requestSurface(req.Surface)
	}
	if !surfaceOK || req.Scope == "" || req.Digest == "" || req.StateToken == "" {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, teamErrorResponse{Error: "body must be {\"scope\": \"organization|repository:<id>\", \"digest\": \"<verified bundle id>\", \"state_token\": \"<the offer's state_token, from GET /api/team/layers>\"}"})
		return
	}
	offer, record, err := team.adoptBundle(req.Scope, req.Digest, req.StateToken, surface)
	if err != nil {
		writeAdoptionRefusal(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
	writeJSON(w, teamAdoptResponse{Adopted: record, BundleID: record.BundleID, Revision: record.Revision, Offer: offer})
}

func handleTeamUnadopt(w http.ResponseWriter, r *http.Request) {
	if team == nil {
		writeGovernorUnavailable(w)
		return
	}
	var req unadoptLayerRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	surface, surfaceOK := "", false
	if err := dec.Decode(&req); err == nil {
		surface, surfaceOK = requestSurface(req.Surface)
	}
	if !surfaceOK || req.Scope == "" {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, teamErrorResponse{Error: "body must be {\"scope\": \"organization|repository:<id>\", \"organization_id\": \"<optional>\"}"})
		return
	}
	result, err := team.unadoptBundle(req.OrganizationID, req.Scope, surface)
	if err != nil {
		writeAdoptionRefusal(w, err)
		return
	}
	out := teamUnadoptResponse{Unadopted: req.Scope, OrganizationID: result.OrganizationID, BundleID: result.BundleID,
		Revision: result.Revision, WasAdopted: result.Found, PlacesOff: result.PlacesOff, Offers: []teamLayerEntry{}}
	team.mu.Lock()
	for _, entry := range team.available {
		if entry.Scope == req.Scope {
			out.Offers = append(out.Offers, entry)
		}
	}
	team.mu.Unlock()
	w.WriteHeader(http.StatusOK)
	writeJSON(w, out)
}
