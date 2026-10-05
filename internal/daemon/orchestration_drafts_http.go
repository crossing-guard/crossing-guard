package daemon

// Draft and revision routes (agents-settings-redesign plan §5). The console
// edits an agent through named fields; profilefs applies them to the authored
// PROFILE.md and validates the result. A draft never runs and never changes a
// binding; publishing selects its exact bytes and refuses to evict a version
// a place still runs.

import (
	"encoding/base64"
	"net/http"
	"strings"

	"crossing-guard/internal/orchestration/profilefs"
)

type draftPutRequest struct {
	ExpectedStateToken string `json:"expected_state_token"`
	// ExpectedSelectionToken is the published version the author was looking
	// at. It is checked only when this save starts the draft, so edits made
	// against a version that has since been replaced are refused, not applied
	// on top of the newer one.
	ExpectedSelectionToken string                 `json:"expected_selection_token,omitempty"`
	Edit                   *profilefs.ProfileEdit `json:"edit,omitempty"`
	SourceBase64           string                 `json:"source_base64,omitempty"`
}

type draftTokenRequest struct {
	ExpectedStateToken string `json:"expected_state_token"`
}

type draftPublishRequest struct {
	ExpectedDraftToken     string `json:"expected_draft_token"`
	ExpectedSelectionToken string `json:"expected_selection_token"`
	Confirmed              bool   `json:"confirmed"`
}

type newDraftRequest struct {
	Start         string `json:"start"`
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	Type          string `json:"type"`
	FromProfileID string `json:"from_profile_id,omitempty"`
}

type draftResponse struct {
	Draft profilefs.Draft `json:"draft"`
}

type draftDiscardResponse struct {
	Discarded bool `json:"discarded"`
}

type draftPublishResponse struct {
	Profile profilefs.Detail `json:"profile"`
}

// reviewerEditable is what a reviewer draft may change: its trigger, context
// and output are fixed by review-lane compatibility.
func reviewerEditable(edit profilefs.ProfileEdit) bool {
	return edit.TriggerEvent == nil && edit.Context == nil && edit.Stages == nil && edit.Authority == nil &&
		edit.MayTag == nil && edit.ReplyShape == nil && edit.Locality == nil
}

func registerOrchestrationDraftRoutes(mux *http.ServeMux, owner *profilefs.Owner, pinned func(string) profilefs.PinSource) {
	mux.HandleFunc("GET /api/orchestration/profiles/{id}/revision", func(w http.ResponseWriter, r *http.Request) {
		detail, err := owner.GetRevision(r.PathValue("id"), r.URL.Query().Get("source_digest"), r.URL.Query().Get("bundle_digest"))
		if err != nil {
			writeOrchestrationProfileError(w, err)
			return
		}
		writeJSON(w, detail)
	})
	mux.HandleFunc("GET /api/orchestration/profiles/{id}/draft", func(w http.ResponseWriter, r *http.Request) {
		draft, found, err := owner.Draft(r.PathValue("id"))
		if err != nil {
			writeOrchestrationProfileError(w, err)
			return
		}
		if !found {
			writeOrchestrationProfileError(w, &profilefs.Problem{Code: "not_found", Field: "draft",
				Message: "This agent has no draft.", Recovery: "Edit the agent to start one."})
			return
		}
		writeJSON(w, draftResponse{Draft: draft})
	})
	mux.HandleFunc("PUT /api/orchestration/profiles/{id}/draft", func(w http.ResponseWriter, r *http.Request) {
		var request draftPutRequest
		if err := decodeBoundedProfileJSON(w, r, &request); err != nil {
			writeOrchestrationProfileError(w, err)
			return
		}
		draft, err := putDraft(owner, r.PathValue("id"), request)
		if err != nil {
			writeOrchestrationProfileError(w, err)
			return
		}
		writeJSON(w, draftResponse{Draft: draft})
	})
	mux.HandleFunc("DELETE /api/orchestration/profiles/{id}/draft", func(w http.ResponseWriter, r *http.Request) {
		var request draftTokenRequest
		if err := decodeBoundedProfileJSON(w, r, &request); err != nil {
			writeOrchestrationProfileError(w, err)
			return
		}
		if err := owner.DiscardDraft(r.PathValue("id"), request.ExpectedStateToken); err != nil {
			writeOrchestrationProfileError(w, err)
			return
		}
		writeJSON(w, draftDiscardResponse{Discarded: true})
	})
	mux.HandleFunc("POST /api/orchestration/profiles/{id}/draft/publish", func(w http.ResponseWriter, r *http.Request) {
		var request draftPublishRequest
		if err := decodeBoundedProfileJSON(w, r, &request); err != nil {
			writeOrchestrationProfileError(w, err)
			return
		}
		if !request.Confirmed {
			writeOrchestrationProfileError(w, profileTransportProblem("confirmed", "Explicit publish confirmation is required.",
				"Review the draft and publish it explicitly."))
			return
		}
		id := r.PathValue("id")
		result, err := owner.PublishDraft(id, request.ExpectedDraftToken, request.ExpectedSelectionToken, pinned(id))
		if err != nil {
			writeOrchestrationProfileError(w, err)
			return
		}
		writeJSON(w, draftPublishResponse{Profile: result.Detail})
	})
	mux.HandleFunc("POST /api/orchestration/drafts", func(w http.ResponseWriter, r *http.Request) {
		var request newDraftRequest
		if err := decodeBoundedProfileJSON(w, r, &request); err != nil {
			writeOrchestrationProfileError(w, err)
			return
		}
		draft, err := newDraft(owner, request)
		if err != nil {
			writeOrchestrationProfileError(w, err)
			return
		}
		writeJSON(w, draftResponse{Draft: draft})
	})
}

// putDraft saves exact bytes, or applies a structured edit to the draft (or,
// with no draft yet, to the current version, bumping its version to the next
// minor unless the edit names one).
func putDraft(owner *profilefs.Owner, id string, request draftPutRequest) (profilefs.Draft, error) {
	existing, found, err := owner.Draft(id)
	if err != nil {
		return profilefs.Draft{}, err
	}
	command := profilefs.DraftCommand{ProfileID: id, ExpectedStateToken: request.ExpectedStateToken}
	if request.SourceBase64 != "" {
		source, decodeErr := base64.StdEncoding.Strict().DecodeString(request.SourceBase64)
		if decodeErr != nil {
			return profilefs.Draft{}, profileTransportProblem("source_base64", "The encoded draft source is invalid.", "Retry the save.")
		}
		command.Source = source
	}
	var base []byte
	var baseType string
	if found {
		base = []byte(existing.Source)
		if existing.Normalized != nil {
			baseType = existing.Normalized.AgentType()
		}
	} else if current, getErr := owner.Get(id); getErr == nil {
		if request.ExpectedSelectionToken != current.StateToken {
			return profilefs.Draft{}, &profilefs.Problem{Code: "state_conflict", Field: "expected_selection_token",
				Message: "This agent was published elsewhere since you opened it.", Recovery: "Reload to edit the newest version."}
		}
		base = []byte(current.Source)
		command.BaseSourceDigest, command.BaseBundleDigest = current.Current.SourceDigest, current.Current.BundleDigest
		if current.Normalized != nil {
			baseType = current.Normalized.AgentType()
		}
		if request.Edit != nil && request.Edit.Version == nil {
			next := profilefs.NextMinorVersion(current.Current.Version)
			request.Edit.Version = &next
		}
	}
	if request.Edit != nil {
		if len(base) == 0 {
			return profilefs.Draft{}, &profilefs.Problem{Code: "not_found", Field: "draft",
				Message: "There is nothing to edit for this agent.", Recovery: "Create the agent first."}
		}
		if baseType == "reviewer" && !reviewerEditable(*request.Edit) {
			return profilefs.Draft{}, profileTransportProblem("edit", "A reviewer's trigger, context and output are fixed.",
				"Edit its name, description, version or instructions.")
		}
		edited, editErr := profilefs.ApplyEdit(base, *request.Edit)
		if edited == nil {
			return profilefs.Draft{}, editErr
		}
		command.Source = edited
	}
	if len(command.Source) == 0 {
		return profilefs.Draft{}, profileTransportProblem("edit", "Send an edit or the draft's source.", "Retry the save.")
	}
	return owner.PutDraft(command)
}

// newDraft starts an agent that does not exist yet: blank from the shipped
// template for its kind, or a duplicate of another agent's current version.
func newDraft(owner *profilefs.Owner, request newDraftRequest) (profilefs.Draft, error) {
	id := strings.TrimSpace(request.ID)
	if _, err := owner.Get(id); err == nil {
		return profilefs.Draft{}, profileTransportProblem("id", "An agent with this ID already exists.", "Choose another name.")
	}
	if _, found, err := owner.Draft(id); err != nil || found {
		if err != nil {
			return profilefs.Draft{}, err
		}
		return profilefs.Draft{}, profileTransportProblem("id", "A draft with this ID already exists.", "Choose another name.")
	}
	var source []byte
	var err error
	switch request.Start {
	case "blank":
		source, err = profilefs.NewSource(profilefs.NewProfile{ID: id, Name: request.Name,
			Description: request.Description, Type: request.Type})
	case "duplicate":
		from, getErr := owner.Get(request.FromProfileID)
		if getErr != nil {
			return profilefs.Draft{}, getErr
		}
		source, err = profilefs.Duplicate([]byte(from.Source), id, request.Name, request.Description)
	default:
		return profilefs.Draft{}, profileTransportProblem("start", "Start blank or from another agent.", "Choose how to start.")
	}
	if err != nil {
		return profilefs.Draft{}, err
	}
	return owner.PutDraft(profilefs.DraftCommand{ProfileID: id, Source: source, ExpectedStateToken: profilefs.DraftAbsentToken(id)})
}
