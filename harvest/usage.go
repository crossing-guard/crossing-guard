package harvest

// The naive substring search over session files (FTS5 over the canonical store
// supersedes it — ADR 0008). Usage totals come from calls recorded in the
// store (token-usage-analytics plan §3.4).

import (
	"os"
	"strings"
	"sync"
)

type SearchHit struct {
	Runtime  string `json:"runtime"`
	ID       string `json:"id"`
	ResumeID string `json:"resume_id,omitempty"`
	Title    string `json:"title"`
	Project  string `json:"project"`
	Snippet  string `json:"snippet"`
}

// SearchSessions is the parallel substring scan over every session file;
// one hit per session, newest first, capped at 50.
func SearchSessions(q string) []SearchHit {
	q = strings.ToLower(strings.TrimSpace(q))
	if len(q) < 2 {
		return []SearchHit{}
	}
	sessions := ScanSessions() // newest first
	hits := make([]SearchHit, len(sessions))
	found := make([]bool, len(sessions))
	var wg sync.WaitGroup
	sem := make(chan struct{}, scanConcurrency)
	for i, s := range sessions {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, s SessionSummary) {
			defer wg.Done()
			defer func() { <-sem }()
			if s.SourceRef != "" {
				events, _, _, err := NormalizeSummary(s, false)
				if err != nil {
					return
				}
				for _, event := range events {
					idx := strings.Index(strings.ToLower(event.Text), q)
					if idx < 0 {
						continue
					}
					start, end := max(0, idx-60), min(len(event.Text), idx+120)
					hits[i] = SearchHit{Runtime: s.Runtime, ID: s.ID, ResumeID: s.ResumeID, Title: s.Title,
						Project: s.Project, Snippet: "…" + event.Text[start:end] + "…"}
					found[i] = true
					return
				}
				return
			}
			f, err := os.Open(s.Path)
			if err != nil {
				return
			}
			defer f.Close()
			sc := newLineScanner(f)
			for sc.Scan() {
				line := sc.Text()
				idx := strings.Index(strings.ToLower(line), q)
				if idx < 0 {
					continue
				}
				start := max(0, idx-60)
				end := min(len(line), idx+120)
				hits[i] = SearchHit{
					Runtime: s.Runtime, ID: s.ID, ResumeID: s.ResumeID, Title: s.Title, Project: s.Project,
					Snippet: "…" + line[start:end] + "…",
				}
				found[i] = true
				return // one hit per session is enough for the list
			}
		}(i, s)
	}
	wg.Wait()
	out := []SearchHit{}
	for i := range hits { // preserves newest-first order
		if found[i] {
			out = append(out, hits[i])
			if len(out) >= 50 {
				break
			}
		}
	}
	return out
}
