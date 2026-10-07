package daemon

// memory_upsert_http.go — POST /api/memory: the console's create/edit door
// (memory-create-identity plan §3.2). The status says which refusal it was, so
// a client can tell a bad request from a clash or a store outage.

import (
	"errors"
	"net/http"

	"crossing-guard/store"
)

// memoryUpsertBodyMax is a generous bound on the request body (a record
// body's wire maximum is 200,000 characters; the bound is not that check).
const memoryUpsertBodyMax = 1 << 20

func handleMemoryUpsert(w http.ResponseWriter, r *http.Request, st *Store) {
	var m Memory
	if err := decodeJSONBody(r, &m, memoryUpsertBodyMax); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	saved, err := st.UpsertMemory(m)
	switch {
	case errors.Is(err, store.ErrMemoryInvalid):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, errMemoryNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, store.ErrMemoryExists), errors.Is(err, store.ErrMemoryStale):
		http.Error(w, err.Error(), http.StatusConflict)
	case err != nil:
		// Same convention as GET /api/memory/record: anything that is not the
		// request's fault is the store's.
		http.Error(w, "memory store unavailable: "+err.Error(), http.StatusServiceUnavailable)
	default:
		writeJSON(w, saved)
	}
}
