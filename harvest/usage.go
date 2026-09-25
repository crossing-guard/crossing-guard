package harvest

// Usage aggregation over the summary cache, and the naive substring search
// (FTS5 over the canonical store supersedes the latter — ADR 0008).

import (
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type UsageSessionRow struct {
	Runtime  string    `json:"runtime"`
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Project  string    `json:"project"`
	Modified time.Time `json:"modified"`
	Total    int64     `json:"total"` // in+out+cache_read+cache_create
	Context  int64     `json:"context"`
	HitRate  float64   `json:"hit_rate"`
	Model    string    `json:"model,omitempty"`
}

type UsageReport struct {
	Totals     SessionUsage                     `json:"totals"`
	Sessions   int                              `json:"sessions"` // sessions WITH telemetry
	NoData     int                              `json:"no_data"`  // sessions without (honest count)
	ByRuntime  map[string]*SessionUsage         `json:"by_runtime"`
	Days       map[string]*DayBucket            `json:"days"` // date -> bucket, ALL runtimes
	DaysByRt   map[string]map[string]*DayBucket `json:"days_by_runtime"`
	TopContext []UsageSessionRow                `json:"top_context"` // by last-turn context
	TopTotal   []UsageSessionRow                `json:"top_total"`   // by total tokens
}

// BuildUsageReport aggregates cached per-file telemetry. ScanSessions() first
// so the cache is warm/fresh; after that this is pure in-memory folding.
func BuildUsageReport() *UsageReport {
	sums := ScanSessions()
	r := &UsageReport{
		ByRuntime: map[string]*SessionUsage{},
		Days:      map[string]*DayBucket{},
		DaysByRt:  map[string]map[string]*DayBucket{},
	}
	var rows []UsageSessionRow
	for _, s := range sums {
		c, ok := summaryCache.Load(summaryCacheKey(s))
		if !ok {
			continue
		}
		e := c.(cacheEntry)
		if e.usage == nil {
			r.NoData++
			continue
		}
		r.Sessions++
		u := e.usage
		r.Totals.Turns += u.Turns
		r.Totals.InputTokens += u.InputTokens
		r.Totals.OutputTokens += u.OutputTokens
		r.Totals.CacheRead += u.CacheRead
		r.Totals.CacheCreate += u.CacheCreate
		rt := r.ByRuntime[s.Runtime]
		if rt == nil {
			rt = &SessionUsage{}
			r.ByRuntime[s.Runtime] = rt
		}
		rt.Turns += u.Turns
		rt.InputTokens += u.InputTokens
		rt.OutputTokens += u.OutputTokens
		rt.CacheRead += u.CacheRead
		rt.CacheCreate += u.CacheCreate
		for day, b := range e.days {
			tb := r.Days[day]
			if tb == nil {
				tb = &DayBucket{}
				r.Days[day] = tb
			}
			tb.Input += b.Input
			tb.Output += b.Output
			tb.CacheRead += b.CacheRead
			tb.CacheCreate += b.CacheCreate
			tb.Turns += b.Turns
			rm := r.DaysByRt[s.Runtime]
			if rm == nil {
				rm = map[string]*DayBucket{}
				r.DaysByRt[s.Runtime] = rm
			}
			rb := rm[day]
			if rb == nil {
				rb = &DayBucket{}
				rm[day] = rb
			}
			rb.Input += b.Input
			rb.Output += b.Output
			rb.CacheRead += b.CacheRead
			rb.CacheCreate += b.CacheCreate
			rb.Turns += b.Turns
		}
		rows = append(rows, UsageSessionRow{
			Runtime: s.Runtime, ID: s.ID, Title: s.Title, Project: s.Project,
			Modified: s.Modified, Model: u.Model,
			Total:   u.InputTokens + u.OutputTokens + u.CacheRead + u.CacheCreate,
			Context: u.Context, HitRate: u.CacheHitRate,
		})
	}
	finishUsage(&r.Totals)
	for _, rt := range r.ByRuntime {
		finishUsage(rt)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Context > rows[j].Context })
	r.TopContext = append([]UsageSessionRow{}, rows[:min(12, len(rows))]...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].Total > rows[j].Total })
	r.TopTotal = append([]UsageSessionRow{}, rows[:min(12, len(rows))]...)
	return r
}

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
