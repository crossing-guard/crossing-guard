package daemon

// memory_tags_http.go — owner tags over memory records (memory-first-class-
// records plan §6): the SAME grammar and validation as session tags, applied
// to memory records; the tag vocabulary endpoint find_by_tag reads; and the
// memory rows the saved-view grammar sees. Owner content: never a detector
// fact, a rule input, or agent context.

import (
	"errors"
	"net/http"
	"time"

	"crossing-guard/store"
)

type memoryTagsRequest struct {
	Records []string                     `json:"records"`
	Apply   []store.SessionOwnerTagValue `json:"apply,omitempty"`
	Retract []store.SessionOwnerTagValue `json:"retract,omitempty"`
}

func handleMemoryTags(w http.ResponseWriter, r *http.Request) {
	var req memoryTagsRequest
	if err := decodeJSONBody(r, &req, 64<<10); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Records) == 0 {
		http.Error(w, "records is required", http.StatusBadRequest)
		return
	}
	if len(req.Apply) == 0 && len(req.Retract) == 0 {
		http.Error(w, "apply or retract is required", http.StatusBadRequest)
		return
	}
	ix, err := store.OpenRO(indexPath())
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	// Validate every record exists before writing anything.
	for _, id := range req.Records {
		if _, err := ix.MemoryByID(id); err != nil {
			ix.Close()
			http.Error(w, "no such memory record: "+id, http.StatusBadRequest)
			return
		}
	}
	ix.Close()
	ix, err = store.Open(indexPath())
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer ix.Close()
	now := time.Now().Unix()
	for _, id := range req.Records {
		if err := ix.ChangeMemoryOwnerTags(id, req.Apply, req.Retract, now); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	writeJSON(w, memoryTagSummary(ix, req.Records))
}

// memoryTagSummary returns each addressed record's active tags, so the
// browser patches rows in place.
func memoryTagSummary(ix *store.Index, records []string) map[string]any {
	out := map[string]any{"records": []map[string]any{}}
	rows := out["records"].([]map[string]any)
	for _, id := range records {
		tags, err := ix.MemoryOwnerTags(id)
		if err != nil {
			continue
		}
		entry := map[string]any{"id": id, "tags": []map[string]any{}}
		tagRows := entry["tags"].([]map[string]any)
		for _, t := range tags {
			tagRows = append(tagRows, map[string]any{"key": t.Key, "value": t.Value, "applied_at": t.AppliedAt})
		}
		entry["tags"] = tagRows
		rows = append(rows, entry)
	}
	out["records"] = rows
	return out
}

// handleMemoryTagUses lists the memory owner-tag vocabulary find_by_tag shows.
func handleMemoryTagUses(w http.ResponseWriter, _ *http.Request) {
	ix, err := openIndexForRead()
	if errors.Is(err, errIndexNotCreated) {
		writeJSON(w, memoryTagUsesResponse{Tags: []store.MemoryOwnerTagUse{}})
		return
	}
	if err != nil {
		http.Error(w, "memory store unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer ix.Close()
	uses, err := ix.MemoryOwnerTagUses()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if uses == nil {
		uses = []store.MemoryOwnerTagUse{}
	}
	writeJSON(w, memoryTagUsesResponse{Tags: uses})
}

// memoryTagUsesResponse is the vocabulary answer find_by_tag shows.
type memoryTagUsesResponse struct {
	Tags []store.MemoryOwnerTagUse `json:"tags"`
}

// handleMemoryByTag lists records carrying one owner tag — find_by_tag's
// memory leg.
func handleMemoryByTag(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	value := r.URL.Query().Get("value")
	if value == "" {
		http.Error(w, "value is required", http.StatusBadRequest)
		return
	}
	ix, err := openIndexForRead()
	if errors.Is(err, errIndexNotCreated) {
		writeJSON(w, memoryByTagResponse{Records: []memoryByTagRecord{}})
		return
	}
	if err != nil {
		http.Error(w, "memory store unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer ix.Close()
	ids, err := ix.RecordsByMemoryOwnerTag(key, value)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	hits := []memoryByTagRecord{}
	for _, id := range ids {
		rec, err := ix.MemoryByID(id)
		if err != nil {
			continue // a tombstoned-after-tag row is skipped, not fatal
		}
		hits = append(hits, memoryByTagRecord{
			ID: rec.ID, Title: rec.Title, Category: rec.Category,
			ScopeType: string(rec.ScopeType), Updated: rec.UpdatedAt,
			Pending: rec.Status == "pending",
		})
	}
	writeJSON(w, memoryByTagResponse{Records: hits, Total: len(hits)})
}

// memoryByTagResponse is find_by_tag's memory leg.
type memoryByTagResponse struct {
	Records []memoryByTagRecord `json:"records"`
	Total   int                 `json:"total"`
}

type memoryByTagRecord struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Category  string `json:"category"`
	ScopeType string `json:"scope_type"`
	Updated   int64  `json:"updated"`
	Pending   bool   `json:"pending"`
}
