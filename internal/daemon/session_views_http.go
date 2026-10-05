package daemon

// HTTP for the owner's saved views. Every route is a thin wrapper over the
// views file owner (session_views_config.go): Save in the console writes
// session-views.json, and nothing else does.

import (
	"errors"
	"net/http"
	"time"

	"crossing-guard/internal/sessionquery"
)

func handleSessionViewsGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, loadSessionViews(sessionViewsDataDir(), sessionOrganizationConfig()))
}

func handleSessionViewCreate(w http.ResponseWriter, r *http.Request) {
	var request sessionViewWriteRequest
	if !decodeSessionOrganizationBody(w, r, &request) {
		return
	}
	id, err := newSessionViewID()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	request.View.ID = id
	respondSessionViews(w, request.StateToken, id, func(views []SavedSessionView) ([]SavedSessionView, error) {
		return append(views, request.View), nil
	})
}

func handleSessionViewUpdate(w http.ResponseWriter, r *http.Request) {
	var request sessionViewWriteRequest
	if !decodeSessionOrganizationBody(w, r, &request) {
		return
	}
	id := r.PathValue("view")
	request.View.ID = id
	respondSessionViews(w, request.StateToken, id, func(views []SavedSessionView) ([]SavedSessionView, error) {
		for index := range views {
			if views[index].ID == id {
				views[index] = request.View
				return views, nil
			}
		}
		return nil, errSessionViewNotFound
	})
}

func handleSessionViewDelete(w http.ResponseWriter, r *http.Request) {
	id, token := r.PathValue("view"), r.URL.Query().Get("state_token")
	respondSessionViews(w, token, "", func(views []SavedSessionView) ([]SavedSessionView, error) {
		for index := range views {
			if views[index].ID == id {
				return append(views[:index], views[index+1:]...), nil
			}
		}
		return nil, errSessionViewNotFound
	})
}

func handleSessionViewOrder(w http.ResponseWriter, r *http.Request) {
	var request sessionViewOrderRequest
	if !decodeSessionOrganizationBody(w, r, &request) {
		return
	}
	respondSessionViews(w, request.StateToken, "", func(views []SavedSessionView) ([]SavedSessionView, error) {
		return reorderSessionViews(views, request.Order)
	})
}

// reorderSessionViews requires the order to name exactly the views that exist:
// a stale browser must not be able to drop a view by leaving it out.
func reorderSessionViews(views []SavedSessionView, order []string) ([]SavedSessionView, error) {
	byID := map[string]SavedSessionView{}
	for _, view := range views {
		byID[view.ID] = view
	}
	if len(order) != len(views) {
		return nil, errSessionViewsStale
	}
	out := make([]SavedSessionView, 0, len(views))
	for _, id := range order {
		view, found := byID[id]
		if !found {
			return nil, errSessionViewsStale
		}
		delete(byID, id)
		out = append(out, view)
	}
	return out, nil
}

// sessionViewWriteResponse answers a view write: the views document, and for
// a created or updated view the notes on its query. A note never rejects a
// write (validateSessionView alone decides that); it tells a writer — the
// console or a script — that the query it just saved probably selects nothing.
type sessionViewWriteResponse struct {
	sessionViewsDocument
	ViewNotes map[string][]sessionquery.Note `json:"view_notes,omitempty"`
	// PlacementNotes are the notes on the written board view's placement
	// rules, as a board read reports them.
	PlacementNotes []boardRuleNotes `json:"placement_notes,omitempty"`
}

// respondSessionViews applies one write. written names the view a create or
// update wrote, whose query is noted; it is empty for a delete or a reorder,
// whose answer is the document alone.
func respondSessionViews(w http.ResponseWriter, token, written string, change func([]SavedSessionView) ([]SavedSessionView, error)) {
	limits := sessionOrganizationConfig()
	document, err := mutateSessionViews(sessionViewsDataDir(), limits, token, change)
	switch {
	case err == nil && written == "":
		writeJSON(w, document)
	case err == nil:
		writeJSON(w, sessionViewWriteResponse{sessionViewsDocument: document,
			ViewNotes:      writtenViewNotes(document.Views, written, sessionQueryLimits(limits), time.Now()),
			PlacementNotes: writtenPlacementNotes(document.Views, written, sessionQueryLimits(limits), time.Now())})
	case errors.Is(err, errSessionViewsStale), errors.Is(err, errSessionViewsRejected):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, errSessionViewNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, errSessionViewInvalid), errors.Is(err, sessionquery.ErrQuery):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// writtenViewNotes notes the written view's query over the coalesced tag
// snapshot (sessionTagSnapshots: the snapshot a recent read took, else a fresh
// read of the three tag tables) — never a session scan, and no tag read at all
// for a query with nothing Notes could say. The snapshot holds every tag,
// including those of folded children and vanished sessions, so a write-time
// note may differ from the read-time one, which stays authoritative. A
// snapshot that is unavailable or cut short yields no note, as it yields no
// count.
func writtenViewNotes(views []SavedSessionView, written string, limits sessionquery.Limits, now time.Time) map[string][]sessionquery.Note {
	for _, view := range views {
		if view.ID != written || view.RecordKind == sessionViewRecordKindMemory {
			continue
		}
		query, err := sessionquery.Parse(view.Query, limits)
		if err != nil || !query.MayHaveNotes() {
			return nil
		}
		snapshot := sessionTagSnapshots.get(now)
		if !snapshot.countable() {
			return nil
		}
		if notes := query.Notes(snapshotVocabulary(snapshot)); len(notes) > 0 {
			return map[string][]sessionquery.Note{written: notes}
		}
	}
	return nil
}

// writtenPlacementNotes notes the written board view's placement rules over
// the same snapshot writtenViewNotes reads, under the same gate: no note is
// better than one taken from tags read in part.
func writtenPlacementNotes(views []SavedSessionView, written string, limits sessionquery.Limits, now time.Time) []boardRuleNotes {
	for _, view := range views {
		if view.ID != written || view.Board == nil || len(view.Board.Placement) == 0 {
			continue
		}
		var vocabulary *sessionquery.Vocabulary
		var out []boardRuleNotes
		for index, rule := range view.Board.Placement {
			query, err := sessionquery.Parse(rule.Query, limits)
			if err != nil || !query.MayHaveNotes() {
				continue
			}
			if vocabulary == nil {
				snapshot := sessionTagSnapshots.get(now)
				if !snapshot.countable() {
					return nil
				}
				read := snapshotVocabulary(snapshot)
				vocabulary = &read
			}
			if notes := query.Notes(*vocabulary); len(notes) > 0 {
				out = append(out, boardRuleNotes{Rule: index, Column: rule.Column, Notes: notes})
			}
		}
		return out
	}
	return nil
}
