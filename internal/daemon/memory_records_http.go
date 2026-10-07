package daemon

// GET /api/memory/record and GET /api/memory/records — one record, and one
// status's records, in the same map (memory-reads-through-daemon plan §4.2) —
// and the console's GET /api/memory and /api/memory/pending lists.
// These are the reads the CLI's memory verbs make; the daemon is the store's
// only reader. A store failure answers 503 with its reason, never "not found"
// and never an empty list.

import (
	"errors"
	"fmt"
	"net/http"
)

// recallVia validates the optional caller label a memory read carries into the
// recall log. Absent keeps each route's historical label; "cli" marks the
// CLI's reads. Nothing else is accepted, so the log's vocabulary stays closed.
func recallVia(raw string) (string, error) {
	switch raw {
	case "", "cli":
		return raw, nil
	}
	return "", fmt.Errorf("via must be absent or %q", "cli")
}

// recallLabel is the recall-log label for a read: the route's own label when
// no caller is named, or the caller's with the route's verb.
func recallLabel(via, defaultLabel string) string {
	switch {
	case via == "":
		return defaultLabel
	case defaultLabel == "mcp-search":
		return via + "-search"
	default:
		return via + "-" + defaultLabel
	}
}

func handleMemoryRecord(w http.ResponseWriter, r *http.Request) {
	via, err := recallVia(r.URL.Query().Get("via"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rec, err := GetCpmemMemory(r.URL.Query().Get("id"), via)
	switch {
	case errors.Is(err, errMemoryNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case err != nil:
		http.Error(w, "memory store unavailable: "+err.Error(), http.StatusServiceUnavailable)
	default:
		writeJSON(w, rec)
	}
}

func handleMemoryRecords(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	switch status {
	case "active", "pending", "rejected":
	default:
		http.Error(w, "status must be active, pending or rejected", http.StatusBadRequest)
		return
	}
	recs, err := ListMemoryRecordMaps(status)
	if err != nil {
		http.Error(w, "memory store unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	out := memoryRecordsResponse{Records: recs}
	// The SessionStart hook sends its cwd; the daemon answers the session's recall scope
	// from its own place resolution, so the hook forks no git (team item 5 decision 12).
	if cwd := r.URL.Query().Get("cwd"); cwd != "" {
		scope := resolveRecallScope(r.Context(), cwd)
		out.RecallScope = &scope
	}
	writeJSON(w, out)
}

// handleConsoleMemories answers the console's two list reads, GET /api/memory
// and GET /api/memory/pending, in the console vocabulary. A store that cannot be
// read is a 503 with its reason like the reads above, never [] shown as "no
// memories" (memory-store-unavailable-reads plan §2.2); one not created yet is [].
func handleConsoleMemories(w http.ResponseWriter, list func() ([]Memory, error)) {
	out, err := list()
	if err != nil {
		http.Error(w, "memory store unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, out)
}

// memoryRecordsResponse is GET /api/memory/records' answer: one status's
// records in the /api/memory/record shape, without sources.
type memoryRecordsResponse struct {
	Records []map[string]any `json:"records"`
	// RecallScope is present when the request carried a cwd: the label and, when the
	// checkout resolves to one remote, the repository id recall filters by.
	RecallScope *recallScopeResponse `json:"recall_scope,omitempty"`
}
