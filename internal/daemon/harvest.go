package daemon

// Thin daemon-side layer over the shared harvest package
// (crossing-guard/harvest — the one parser set, code-organization-v1 M2).
// This file owns only what is daemon-specific: the governance-index
// enrichment merge, the Facts card on session detail, and the FTS5-index
// search preference (queried directly via the store package since M4
// slice C — the cpmem subprocess coupling is gone). All vendor-file
// parsing lives in the shared package.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"crossing-guard/harvest"
	"crossing-guard/internal/changeenv"
	"crossing-guard/internal/transcriptindex"
	"crossing-guard/store"
)

type SessionSummary = harvest.SessionSummary
type CanonicalEvent = harvest.CanonicalEvent
type SessionUsage = harvest.SessionUsage

type SessionDetail struct {
	harvest.SessionDetail
	Facts *Enrichment `json:"facts,omitempty"` // governor index detail (session facts card)
	// Segments lists the OTHER files that resolve to this same conversation. A codex
	// thread resumed across rollouts is one conversation in several files (~9% of
	// codex sessions), and MatchID matches all of them. Returning the first silently
	// showed one segment as if it were the whole thread — undetectable loss, which is
	// worse than double-counting. Declared here so a reader can see the rest (INV-21).
	Segments       []SessionRef `json:"segments,omitempty"`
	TranscriptNote string       `json:"transcript_note,omitempty"`
}

// SessionRef points at another file belonging to the same conversation.
type SessionRef struct {
	Runtime  string `json:"runtime"`
	ID       string `json:"id"`
	Modified int64  `json:"modified"`
	Lines    int    `json:"lines"`
}

// scanCoalescer collapses concurrent ScanSessions calls into one underlying scan
// and lets callers within a short window share the completed result. It is a burst
// coalescer, not a cache: nothing is served beyond scanReuseWindow, so freshness
// honesty is bounded by ~1.5 s. Measured trigger: the console boot fires the rail
// plus every expanded repository page at once, and each request re-ran the full
// vendor-store scan — ~6 concurrent scans, minutes of aggregate CPU after a restart.
type scanCoalescer struct {
	mu      sync.Mutex
	waiting chan struct{}
	result  []SessionSummary
	done    time.Time
}

const scanReuseWindow = 1500 * time.Millisecond

func (c *scanCoalescer) get(scan func() []SessionSummary) []SessionSummary {
	c.mu.Lock()
	if c.result != nil && time.Since(c.done) < scanReuseWindow {
		out := c.result
		c.mu.Unlock()
		return append([]SessionSummary(nil), out...)
	}
	if c.waiting != nil {
		ch := c.waiting
		c.mu.Unlock()
		<-ch
		c.mu.Lock()
		out := c.result
		c.mu.Unlock()
		return append([]SessionSummary(nil), out...)
	}
	ch := make(chan struct{})
	c.waiting = ch
	c.mu.Unlock()
	out := scan()
	c.mu.Lock()
	c.result, c.done, c.waiting = out, time.Now(), nil
	c.mu.Unlock()
	close(ch)
	return append([]SessionSummary(nil), out...)
}

var sessionScanCoalescer = &scanCoalescer{}

// ScanSessions lists sessions newest-first with governance-index tags
// (branch/commits/PRs/memory-writes) merged from the engine index. Concurrent
// callers share one scan; each receives its own top-level slice copy.
func ScanSessions() []SessionSummary {
	return sessionScanCoalescer.get(scanSessionsUncoalesced)
}

func scanSessionsUncoalesced() []SessionSummary {
	// No enrichment merge. The branch/commits/PR chips came from sessions.db, an
	// EXPERIMENT's output that stopped being written on 2026-07-16 — so they were
	// frozen decoration rendered as current fact. Deriving them from our own store
	// is Phase 5's work-artifact extraction; until then the honest state is absent,
	// not stale (INV-22).
	out := harvest.ScanSessions()
	if _, err := os.Stat(indexPath()); err != nil {
		return out
	}
	ix, err := store.OpenRO(indexPath())
	if err != nil {
		return out
	}
	defer ix.Close()
	ids, err := ix.EnvelopeSessionIDs(500)
	if err != nil {
		return out
	}
	seen := map[string]int{}
	for i, s := range out {
		seen[s.ID] = i
		if s.ThreadID != "" {
			seen[s.ThreadID] = i
		}
	}
	for _, id := range ids {
		if i, ok := seen[id]; ok {
			out[i].HasChangeEvidence = true
			continue
		}
		runtime, title, _, e := changeenv.Metadata(ix, id)
		if e != nil {
			continue
		}
		if runtime == "" {
			runtime = "unknown"
		}
		if title == "" {
			title = "untitled envelope"
		}
		recs, _, e := ix.ChangeRecordsForSession(id, 1)
		if e != nil || len(recs) == 0 {
			continue
		}
		out = append(out, SessionSummary{Runtime: runtime, ID: id, Title: title, TitleSource: "envelope-claim", Modified: time.Unix(0, recs[0].RecordedAt), Cwd: recs[0].CheckoutRoot, Project: filepath.Base(recs[0].CheckoutRoot), HasChangeEvidence: true})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out
}

// catalogSessionExists validates the exact row identity already owned by the harvest
// catalog. Vendor resume matching is deliberately not used here: several harvested
// records may share one resume handle while remaining distinct console rows.
func catalogSessionExists(runtime, id string) (bool, error) {
	for _, session := range ScanSessions() {
		if session.Runtime == runtime && session.ID == id {
			return true, nil
		}
	}
	return false, nil
}

// LoadSession reads and normalizes one session, with the enriched summary
// and the facts card. Codex ids match by suffix (rollout filename stem vs
// bare thread uuid), same as harvest.Find.
func LoadSession(runtime, id string) (*SessionDetail, error) {
	// Collect EVERY match, not the first. MatchID resolves a codex thread id to all
	// of its rollout segments; picking whichever the scan happened to yield first was
	// silent and non-deterministic.
	matches := harvest.FindAll(runtime, id)
	if len(matches) > 0 {
		// Newest wins — the segment a human asking about a thread almost always means.
		primary := 0
		for i := range matches {
			if matches[i].Modified.After(matches[primary].Modified) {
				primary = i
			}
		}
		s := matches[primary]
		{
			d := &SessionDetail{SessionDetail: harvest.SessionDetail{SessionSummary: s}}
			for i, m := range matches {
				if i != primary {
					d.Segments = append(d.Segments, SessionRef{Runtime: m.Runtime, ID: m.ID,
						Modified: m.Modified.Unix(), Lines: m.Lines})
				}
			}
			var err error
			d.Events, d.Unparsed, d.Usage, err = harvest.NormalizeSummary(s, false)
			// zero turns = no telemetry observed; show "unknown", not zeros
			if d.Usage != nil && d.Usage.Turns == 0 {
				d.Usage = nil
			}
			d.Facts = FactsFor(&d.SessionSummary)
			return d, err
		}
	}
	if _, err := os.Stat(indexPath()); err == nil {
		ix, e := store.OpenRO(indexPath())
		if e == nil {
			defer ix.Close()
			if detail := sessionWithoutTranscript(ix, runtime, id); detail != nil {
				return detail, nil
			}
		}
	}
	return nil, fmt.Errorf("session not found: %s/%s", runtime, id)
}

// sessionWithoutTranscript answers for a session no transcript file matches.
// Two things may still be known: recorded change evidence, and — for a session
// the owner tagged, which is therefore kept — the text that was indexed for
// search. Either alone is enough; with neither the session is not found.
func sessionWithoutTranscript(ix *store.Index, runtime, id string) *SessionDetail {
	var detail *SessionDetail
	if recs, _, err := ix.ChangeRecordsForSession(id, 1); err == nil && len(recs) > 0 {
		claimedRuntime, title, _, _ := changeenv.Metadata(ix, id)
		if title == "" {
			title = "untitled envelope"
		}
		if claimedRuntime == "" {
			claimedRuntime = "unknown"
		}
		s := SessionSummary{Runtime: claimedRuntime, ID: id, Title: title, TitleSource: "envelope-claim", Modified: time.Unix(0, recs[0].RecordedAt), Cwd: recs[0].CheckoutRoot, Project: filepath.Base(recs[0].CheckoutRoot), HasChangeEvidence: true}
		detail = &SessionDetail{SessionDetail: harvest.SessionDetail{SessionSummary: s, Events: []harvest.CanonicalEvent{}}, TranscriptNote: "Transcript unavailable — this session is known only from recorded change evidence."}
	}
	remembered, tagged := ownerRememberedSession(ix, runtime, id)
	if !tagged {
		// Known only from change evidence, or not known at all: unchanged.
		return detail
	}
	if detail == nil {
		detail = &SessionDetail{SessionDetail: harvest.SessionDetail{SessionSummary: remembered, Events: []harvest.CanonicalEvent{}}}
	}
	// What was observed is that no transcript file was found — not that one was
	// deleted: a store that could not be read during this scan looks the same.
	detail.TranscriptNote = "No transcript file was found for this session."
	if kept := keptTranscriptEvents(ix, runtime, id); len(kept) > 0 {
		detail.Events = kept
		detail.TranscriptNote = "No transcript file was found for this session. Showing the text that was kept."
	}
	return detail
}

func BuildUsageReport() *harvest.UsageReport { return harvest.BuildUsageReport() }

// --- search (the bounded relational projection only) ---

type SearchHit struct {
	Runtime  string `json:"runtime"`
	ID       string `json:"id"`
	ResumeID string `json:"resume_id,omitempty"`
	Title    string `json:"title"`
	Project  string `json:"project"`
	Kind     string `json:"kind"`
	Snippet  string `json:"snippet"`
}

// SearchResult wraps indexed hits with the latest completed reconciliation coverage.
type SearchResult struct {
	Source   string                   `json:"source"`
	Coverage transcriptindex.Coverage `json:"coverage"`
	Hits     []SearchHit              `json:"hits"`
}

// SearchAll queries only the maintained FTS projection. Coverage, including an
// incomplete zero, remains explicit; request-time raw transcript scans are forbidden.
func SearchAll(q string, coverage transcriptindex.Coverage) SearchResult {
	hits, err := searchViaIndex(q)
	if err != nil {
		return SearchResult{Source: "fts5 [index]",
			Coverage: transcriptindex.UnavailableCoverage(err), Hits: []SearchHit{}}
	}
	return SearchResult{Source: "fts5 [index]", Coverage: coverage, Hits: hits}
}

func searchMetadataByIdentity(sessions []SessionSummary) map[string]SessionSummary {
	metadata := make(map[string]SessionSummary, len(sessions)*2)
	for _, session := range sessions { // ScanSessions is newest-first; first identity wins.
		for _, id := range []string{session.ID, session.ResumeID, session.ThreadID} {
			key := session.Runtime + "/" + id
			if session.Runtime == "" || id == "" {
				continue
			}
			if _, exists := metadata[key]; !exists {
				metadata[key] = session
			}
		}
	}
	return metadata
}

func enrichSearchHitIdentity(hit SearchHit, session SessionSummary) SearchHit {
	hit.ID = session.ID
	hit.ResumeID = session.ResumeID
	hit.Title = session.Title
	hit.Project = session.Project
	return hit
}

// resolvedIndexPath is computed ONCE at startup by setIndexPath and read everywhere
// after. Recomputing it per call from $HOME is what let the daemon honor --data for
// its token, address file and logs while writing the database somewhere else.
var resolvedIndexPath string

// setIndexPath fixes the index location for the process. explicitDataDir is the
// --data value ONLY if the operator actually passed it (see store.IndexPath).
func setIndexPath(explicitDataDir string) string {
	resolvedIndexPath = store.IndexPath(explicitDataDir, homeDir())
	// The scan coalescer memoizes against one index/home environment; repointing
	// the index (daemon start, tests switching HOME) invalidates that burst memo.
	sessionScanCoalescer = &scanCoalescer{}
	return resolvedIndexPath
}

func indexPath() string {
	if resolvedIndexPath == "" { // CLI paths that never call setIndexPath (crossing-guard sessions, etc.)
		return store.IndexPath("", homeDir())
	}
	return resolvedIndexPath
}

func searchViaIndex(q string) ([]SearchHit, error) {
	if len(strings.TrimSpace(q)) < 2 {
		return []SearchHit{}, nil
	}
	p := indexPath()
	if _, err := os.Stat(p); err != nil {
		return nil, err
	}
	ix, err := store.OpenRO(p)
	if err != nil {
		return nil, err
	}
	defer ix.Close()
	events, err := ix.SearchEvents(q, 20) // the CLI's default event-hit budget
	if err != nil {
		return nil, err
	}
	// SearchEvents already joins the indexed session row. Deduplicate by canonical
	// projection identity, while returning the exact catalog/resume identity used by
	// the Sessions rail and resume path.
	seen := map[string]bool{}
	hits := []SearchHit{}
	for _, ev := range events {
		// The shared FTS also serves the Memory surface. This endpoint owns the
		// Sessions rail, where memory documents have no session navigation identity.
		if ev.Kind == "memory" {
			continue
		}
		key := ev.Vendor + "/" + ev.SessionID
		if seen[key] {
			continue
		}
		seen[key] = true
		catalogID, resumeID := ev.CatalogID, ev.ResumeID
		if catalogID == "" {
			catalogID = ev.SessionID
		}
		if resumeID == "" {
			resumeID = ev.SessionID
		}
		h := enrichSearchHitIdentity(SearchHit{
			Runtime: ev.Vendor, ID: ev.SessionID, Kind: ev.Kind,
			Snippet: truncate(ev.Text, 180),
		}, SessionSummary{ID: catalogID, ResumeID: resumeID,
			Title: ev.Title, Project: ev.Project})
		hits = append(hits, h)
		if len(hits) >= 50 {
			break
		}
	}
	return hits, nil
}

// --- local helpers still used by the daemon's other files ---

func anyString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case nil:
		return ""
	case []any, map[string]any:
		return compactJSON(s)
	default:
		return fmt.Sprintf("%v", s)
	}
}

func compactJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// SessionEventText is one event's untruncated payload.
type SessionEventText struct {
	Seq  int    `json:"seq"`
	Text string `json:"text"`
	// Available is false when this runtime cannot re-read its own file at full
	// fidelity. The console must then keep showing the clipped text and SAY it
	// is clipped, rather than implying the fragment is the whole payload.
	Available bool   `json:"available"`
	Note      string `json:"note,omitempty"`
}

// handleSessionEvent serves the full text of one canonical event.
//
// The transcript deliberately clips tool payloads — a session can hold 1,300
// events and hundreds of whole tool results would be tens of megabytes on every
// open. But the clip landed where a code review needs to look: for
// agent-written code the surviving bytes are the doc comment and the discarded
// bytes are the code. The bytes are still on disk, so a reader who expands one
// chip gets that one chip in full.
func handleSessionEvent(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	runtime, id := q.Get("runtime"), q.Get("id")
	seq, err := strconv.Atoi(q.Get("seq"))
	if err != nil {
		http.Error(w, "seq must be an integer", http.StatusBadRequest)
		return
	}
	text, ok, err := harvest.EventText(runtime, id, seq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	out := SessionEventText{Seq: seq, Text: text, Available: ok}
	if !ok {
		out.Note = "runtime " + runtime + " cannot re-read events at full fidelity; " +
			"the transcript is showing the clipped payload"
	}
	writeJSON(w, out)
}
