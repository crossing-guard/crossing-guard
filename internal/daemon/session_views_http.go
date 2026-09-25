package daemon

// HTTP for the owner's saved views. Every route is a thin wrapper over the
// views file owner (session_views_config.go): Save in the console writes
// session-views.json, and nothing else does.

import (
	"errors"
	"net/http"

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
	respondSessionViews(w, request.StateToken, func(views []SavedSessionView) ([]SavedSessionView, error) {
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
	respondSessionViews(w, request.StateToken, func(views []SavedSessionView) ([]SavedSessionView, error) {
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
	respondSessionViews(w, token, func(views []SavedSessionView) ([]SavedSessionView, error) {
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
	respondSessionViews(w, request.StateToken, func(views []SavedSessionView) ([]SavedSessionView, error) {
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

func respondSessionViews(w http.ResponseWriter, token string, change func([]SavedSessionView) ([]SavedSessionView, error)) {
	document, err := mutateSessionViews(sessionViewsDataDir(), sessionOrganizationConfig(), token, change)
	switch {
	case err == nil:
		writeJSON(w, document)
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
