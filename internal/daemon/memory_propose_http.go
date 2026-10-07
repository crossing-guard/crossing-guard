package daemon

// memory_propose_http.go — POST /api/memory/propose: the recall MCP's one
// write-shaped door (memory-first-class-records plan §5.2, RT-6 folds). An
// agent session may propose a memory; a HUMAN promotes it (ADR 0013 D4). The
// store is the enforcement point — this route can only ever create a
// status='pending' record.

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"crossing-guard/harvest"
	"crossing-guard/store"
)

// proposeWindow is the rate window; one session may propose at most
// recall.propose_per_session_max records inside it (queue-flood is the
// documented residual; the bound makes it explicit).
const proposeWindow = time.Hour

type proposeLimiter struct {
	mu     sync.Mutex
	counts map[string]int // session key -> proposals in window
	since  time.Time
}

var memoryProposeLimits = &proposeLimiter{counts: map[string]int{}, since: time.Now()}

func (p *proposeLimiter) allow(session string, max int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if time.Since(p.since) > proposeWindow {
		p.counts, p.since = map[string]int{}, time.Now()
	}
	if p.counts[session] >= max {
		return false
	}
	p.counts[session]++
	return true
}

type memoryProposeRequest struct {
	Title      string   `json:"title"`
	Category   string   `json:"category"`
	Body       string   `json:"body"`
	Tags       []string `json:"tags"`
	Aliases    []string `json:"aliases"`
	Repository string   `json:"repository"`
	Session    string   `json:"session"` // caller identity for the rate bound
	// Cwd is the proposing session's folder, when its runtime reports one: the door
	// mints the repository identity from it (team item 5 decision 18).
	Cwd string `json:"cwd"`
}

// handleMemoryPropose validates the consent flag, bounds the rate, and writes
// one pending record through the one store owner. Sources are attached from
// the caller's claim — a proposal without evidence is refused (plan §4.2).
func handleMemoryPropose(w http.ResponseWriter, r *http.Request) {
	// The consent is the memory owner's (memory.json propose section), read
	// LIVE per request (config-ownership plan Fix A / RT-C1): a flip in the
	// file is obeyed by the next call, no restart.
	config, _ := memoryConfig()
	if !config.Propose.Enabled {
		http.Error(w, "memory proposals are not enabled (propose.enabled in memory.json)", http.StatusNotFound)
		return
	}
	var req memoryProposeRequest
	if err := decodeJSONBody(r, &req, 32<<10); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Title) == "" || strings.TrimSpace(req.Body) == "" {
		http.Error(w, "title and body are required", http.StatusBadRequest)
		return
	}
	if req.Category == "" {
		req.Category = "note"
	}
	// Labels are checked before the rate bound, so a malformed label list does
	// not spend a slot.
	if err := store.ValidateMemoryLabels(req.Tags, req.Aliases); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	session := strings.TrimSpace(req.Session)
	if session == "" {
		session = callerIPKey(r)
	}
	if !memoryProposeLimits.allow(session, config.Propose.PerSessionMax) {
		http.Error(w, "proposal rate reached for this session; wait or ask the owner", http.StatusTooManyRequests)
		return
	}

	ix, err := store.Open(indexPath())
	if err != nil {
		http.Error(w, "memory store unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer ix.Close()
	rec := store.MemoryRecord{
		ID: store.NewMemoryID("proposal"), Title: req.Title, Category: req.Category, Body: req.Body,
		Tags: req.Tags, Aliases: req.Aliases,
		Source: "agent", Status: "pending",
		ScopeType: store.MemoryScopeUser, AuthorType: "agent", AuthorID: "session:" + session,
		Origin: "proposed by an agent session via propose_memory",
	}
	if req.Repository != "" {
		rec.ScopeType = store.MemoryScopeRepository
		if req.Cwd != "" && filepath.IsAbs(req.Cwd) && len(req.Cwd) <= maxPeerPlaceBytes {
			rec.ScopeID, rec.RepositoryIdentity, rec.IdentityNote = mintMemoryScope(req.Cwd, req.Repository)
		} else {
			rec.ScopeID, rec.RepositoryIdentity = req.Repository, "weak"
			rec.IdentityNote = "the proposing session reported no folder, so the repository is known only by name"
		}
	}
	var sources []store.MemorySource
	if session != "" {
		// The proposal's evidence: the proposing session (record-level, anchor
		// kind none). A runtime-named session becomes a real citation.
		for _, vendor := range harvest.RuntimeNames() {
			if rest, ok := strings.CutPrefix(session, vendor+"/"); ok && rest != "" {
				sources = append(sources, store.MemorySource{Vendor: vendor, SessionID: rest, AnchorKind: "none"})
				break
			}
		}
	}
	if len(sources) == 0 {
		// Plan §4.2: proposals MUST cite. An unidentified caller is refused
		// rather than landing an unattributable record in a human's queue.
		http.Error(w, "proposal must name its session (session: \"<runtime>/<session-id>\")", http.StatusBadRequest)
		return
	}
	// Create-only: a proposal never lands on an existing record.
	saved, err := ix.CreateMemory(rec, sources, nil,
		store.MemoryActor{AuthorType: "agent", AuthorID: "session:" + session, ActorSource: "mcp"})
	switch {
	case errors.Is(err, store.ErrMemoryExists):
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case errors.Is(err, store.ErrMemoryInvalid):
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	case err != nil:
		// Not the proposer's fault: the store failed (as at open, above).
		http.Error(w, "memory store unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, memoryProposeResponse{
		ID: saved.ID, Status: saved.Status,
		Result: "proposed into pending; a human promotes — this is not stored yet",
	})
}

// memoryProposeResponse is the propose door's answer: what was proposed and
// the honest words for it.
type memoryProposeResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Result string `json:"result"`
}

// decodeJSONBody decodes a size-bounded JSON request body.
func decodeJSONBody(r *http.Request, into any, maxBytes int64) error {
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBytes)).Decode(into)
}

// callerIPKey is the fallback bound key when a caller is not identified.
func callerIPKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return "unknown"
	}
	return host
}
