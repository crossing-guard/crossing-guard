package daemon

// GET /api/memory/search — the memory store's keyword search over HTTP
// (recall-mcp-v1-plan §3.4). Every call is appended to the recall log.

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"crossing-guard/memory"
	"crossing-guard/store"
)

// memorySearchHit is one search result: the record's frontmatter, why it
// matched, and a bounded excerpt of its body. Pending marks an unreviewed
// proposal, disclosed rather than filtered.
type memorySearchHit struct {
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	Category   string   `json:"category"`
	Repository string   `json:"repository"`
	Tags       []string `json:"tags"`
	Updated    string   `json:"updated"`
	VerifiedAt string   `json:"verified_at,omitempty"`
	Pending    bool     `json:"pending"`
	Score      int      `json:"score"`
	Why        []string `json:"why"`
	Excerpt    string   `json:"excerpt"`
}

type memorySearchResponse struct {
	Hits      []memorySearchHit `json:"hits"`
	Total     int               `json:"total"`
	Truncated bool              `json:"truncated"`
}

// memorySearchFilters are the facet filters the HTTP search and the MCP share.
type memorySearchFilters struct {
	Category   string
	Repository string
	Tag        string
	// Scope, when set, is the caller's resolved recall scope (it sent its cwd): the
	// search then returns what recall would — every organization and user record, and
	// repository records in this repository (team item 5 decision 12).
	Scope *recallScopeResponse
}

// memSearchHit is the internal hit type: the store record plus score/why.
type memSearchHit struct {
	store.MemoryRecord
	Score int
	Why   []string
}

// searchMemoryStore runs the keyword search over the STORE (plan §3.5: one read
// surface; the legacy file walk is gone). Scoring is the legacy contract —
// title/id > aliases > tags > body — with Go-side folding (RT-5: SQLite's
// lower() is ASCII-only, LIKE metacharacters never enter SQL because matching
// happens in Go over in-memory rows at this store's 10³ scale). Rejected
// records are never hits: a rejection is a curation verdict, and a hit carries
// no status beyond pending. A store failure is returned, never read as "no
// hits" — the incident that motivated the reads-through-daemon plan was a
// store that could not be opened.
func searchMemoryStore(query string, filters memorySearchFilters) ([]memSearchHit, error) {
	ix, err := openIndexForRead()
	if errors.Is(err, errIndexNotCreated) {
		return nil, nil // nothing recorded yet: no hits
	}
	if err != nil {
		return nil, err
	}
	defer ix.Close()
	recs, err := ix.ListMemory("")
	if err != nil {
		return nil, err
	}
	terms := memTokenize(query)
	var hits []memSearchHit
	for _, r := range recs {
		if r.Status == "rejected" {
			continue
		}
		if filters.Category != "" && r.Category != filters.Category {
			continue
		}
		if filters.Repository != "" && !memory.SameRepositoryScope(r.ScopeID, filters.Repository) {
			continue
		}
		if filters.Scope != nil && !memoryInRecallScope(r, *filters.Scope) {
			continue
		}
		if filters.Tag != "" && !memHasTag(r.Tags, filters.Tag) {
			continue
		}
		score := 0
		var why []string
		title := strings.ToLower(r.Title)
		id := strings.ToLower(r.ID)
		tags := strings.ToLower(strings.Join(r.Tags, " "))
		aliases := strings.ToLower(strings.Join(r.Aliases, " "))
		body := strings.ToLower(r.Body)
		for _, t := range terms {
			switch {
			case strings.Contains(title, t) || strings.Contains(id, t):
				score += 3
				why = append(why, "title:"+t)
			case strings.Contains(aliases, t):
				score += 3
				why = append(why, "alias:"+t)
			case strings.Contains(tags, t):
				score += 2
				why = append(why, "tag:"+t)
			case strings.Contains(body, t):
				score++
				why = append(why, "body:"+t)
			}
		}
		if len(terms) == 0 && filters.Tag != "" {
			why = []string{"tag:" + strings.ToLower(filters.Tag)}
		} else if score == 0 {
			continue
		}
		hits = append(hits, memSearchHit{r, score, why})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].UpdatedAt > hits[j].UpdatedAt
	})
	return hits, nil
}

func memTokenize(q string) []string {
	fields := strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
		return !('a' <= r && r <= 'z' || '0' <= r && r <= '9')
	})
	var out []string
	for _, f := range fields {
		if len(f) >= 2 {
			out = append(out, f)
		}
	}
	return out
}

func memHasTag(tags []string, want string) bool {
	for _, t := range tags {
		if strings.EqualFold(t, want) {
			return true
		}
	}
	return false
}

func handleMemorySearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	q, tag := strings.TrimSpace(query.Get("q")), strings.TrimSpace(query.Get("tag"))
	if q == "" && tag == "" {
		http.Error(w, "q or tag is required", http.StatusBadRequest)
		return
	}
	via, err := recallVia(query.Get("via"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	limits, _ := consoleConfig()
	limit := limits.Recall.MemorySearchLimitMax
	if raw := query.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > limits.Recall.MemorySearchLimitMax {
			http.Error(w, "limit must be between 1 and "+strconv.Itoa(limits.Recall.MemorySearchLimitMax), http.StatusBadRequest)
			return
		}
		limit = n
	}
	dir := memory.DefaultDir()
	filters := memorySearchFilters{Category: query.Get("category"), Repository: query.Get("repository"), Tag: tag}
	if cwd := query.Get("cwd"); cwd != "" {
		scope := resolveRecallScope(r.Context(), cwd)
		filters.Scope = &scope
	}
	hits, err := searchMemoryStore(q, filters)
	if err != nil {
		http.Error(w, "memory store unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	out := memorySearchResponse{Hits: []memorySearchHit{}, Total: len(hits)}
	if len(hits) > limit {
		hits, out.Truncated = hits[:limit], true
	}
	ids := make([]string, 0, len(hits))
	for _, hit := range hits {
		ids = append(ids, hit.ID)
		tags, why := hit.Tags, hit.Why
		if tags == nil {
			tags = []string{}
		}
		if why == nil {
			why = []string{}
		}
		out.Hits = append(out.Hits, memorySearchHit{ID: hit.ID, Title: hit.Title, Category: hit.Category,
			Repository: hit.ScopeID, Tags: tags,
			Updated: time.Unix(0, hit.UpdatedAt).UTC().Format(time.RFC3339), VerifiedAt: hit.VerifiedAt,
			Pending: hit.Status == "pending", Score: hit.Score, Why: why,
			Excerpt: memoryExcerpt(hit.Body, limits.Recall.MemoryExcerptBytes)})
	}
	logged := q
	if tag != "" {
		logged = strings.TrimSpace(q + " tag=" + tag)
	}
	memory.LogRecall(dir, recallLabel(via, "mcp-search"), logged, ids)
	writeJSON(w, out)
}

// memoryExcerpt cuts text to at most maxBytes on a rune boundary, the cut
// marker included.
func memoryExcerpt(text string, maxBytes int) string {
	const marker = "…"
	text = strings.TrimSpace(text)
	if len(text) <= maxBytes {
		return text
	}
	cut := max(maxBytes-len(marker), 0)
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + marker
}
