package daemon

// The session footprint (the Facts card): what the session touched, derived
// from our own governance event log by liveFootprint. The sessions.db
// enrichment this file once read (branch, commits, issues/PRs, memory writes,
// written by the retired sessionindex experiment) is gone since 2026-07-20;
// the branch/commits/PR fields below have no writer today, and the memory
// read/write fields were retired (audit-memory-write-fact plan).

import (
	"strings"

	"crossing-guard/harvest"
)

type Commit struct {
	SHA string `json:"sha,omitempty"`
	Msg string `json:"msg,omitempty"`
}

type Enrichment struct {
	Branch    string   `json:"branch,omitempty"`
	Commits   int      `json:"commits,omitempty"`
	PRs       []string `json:"prs,omitempty"`
	ToolCalls int      `json:"tool_calls,omitempty"`
	// full detail (session facts card)
	CommitList   []Commit `json:"commit_list,omitempty"`
	Pushes       []string `json:"pushes,omitempty"`
	Files        []string `json:"files,omitempty"`
	FilesCreated []string `json:"files_created,omitempty"`
	// Source says WHERE this footprint came from. Only "live" (our own governance
	// event log, current by construction) is set today; "legacy-index" named the
	// retired sessions.db pipeline, which stopped running on 2026-07-16.
	Source string `json:"source,omitempty"`
	AsOf   int64  `json:"as_of,omitempty"` // newest observation backing this record
}

// FactsFor returns the session's footprint, derived from our own governance log.
//
// This was the last production dependency on an experiment: the panel read
// sessions.db, produced by experiments/sessionindex/sessionindex.py, which has
// not run since 2026-07-16 — so every session since showed "no recorded
// footprint" while our own event log held the answer. Production code may not
// depend on experiment output; the experiment stays, the dependency goes.
func FactsFor(s *SessionSummary) *Enrichment {
	return liveFootprint(s)
}

// liveFootprint derives the footprint from the governance event log — what this
// session actually touched, as we observed it. nil when the governor is not up or
// the session predates live capture.
func liveFootprint(s *SessionSummary) *Enrichment {
	if governor == nil || s == nil {
		return nil
	}
	sessionID := harvest.CanonicalID(*s)
	evs, err := governor.SessionEvents(sessionID, 0)
	if err != nil || len(evs) == 0 {
		return nil
	}
	e := &Enrichment{Source: "live"}
	seenRead, seenWrite := map[string]bool{}, map[string]bool{}
	for _, ev := range evs {
		e.ToolCalls++
		if ev.TS > e.AsOf {
			e.AsOf = ev.TS
		}
		id := ev.TargetEntityID
		if id == "" || !strings.HasPrefix(id, "file:") {
			continue
		}
		path := strings.TrimPrefix(id, "file:")
		switch {
		case ev.Verb == "write" && ev.Tool != "apply_patch":
			if !seenWrite[path] {
				seenWrite[path] = true
				e.FilesCreated = append(e.FilesCreated, path)
			}
		default:
			if !seenRead[path] {
				seenRead[path] = true
				e.Files = append(e.Files, path)
			}
		}
	}
	// Multi-target actions keep their resource set outside the legacy one-target
	// event column. Reuse the store's one touch projection so the Facts card and
	// Change map cannot disagree about which exact files were observed.
	touches, touchErr := governor.ix.SessionFileTouches(sessionID, 10000)
	if touchErr == nil {
		e.ToolCalls = touches.EventTotal
		e.AsOf = touches.LastEventTS
		for _, touch := range touches.Touches {
			if !seenRead[touch.Identity] && !seenWrite[touch.Identity] {
				seenRead[touch.Identity] = true
				e.Files = append(e.Files, touch.Identity)
			}
		}
	}
	return e
}
