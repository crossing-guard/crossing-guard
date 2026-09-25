package daemon

import (
	"fmt"
	"net/http"
	"strconv"
	"time"
)

func handleSessionActivityList(w http.ResponseWriter, _ *http.Request) {
	if sessionActivityService() == nil {
		http.Error(w, "native session activity unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, sessionActivityService().Snapshot())
}

func handleSessionActivityStream(w http.ResponseWriter, r *http.Request) {
	if sessionActivityService() == nil {
		http.Error(w, "native session activity unavailable", http.StatusServiceUnavailable)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	after, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if err != nil || after < 0 {
		http.Error(w, "after must be a non-negative generation", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	reset, subscription, _ := activityFeedOpen(after) // one feed per response: nothing waits on its opening
	if reset != nil {
		writeJSONSSE(w, flusher, "reset", reset.Generation, reset)
		return
	}
	defer sessionActivityService().Unsubscribe(subscription.ID)
	keepalive := time.NewTicker(sessionActivityConfig().RailKeepalive())
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case snapshot := <-subscription.Updates:
			writeJSONSSE(w, flusher, "activity", snapshot.Generation, snapshot)
		}
	}
}
