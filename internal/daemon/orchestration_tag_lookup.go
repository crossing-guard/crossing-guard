package daemon

// Reverse tag lookups (recall-mcp-v1-plan §3.4): which sessions carry a
// follower/helper tag, and how many sessions carry each active tag. Tags are
// written under a session's catalog-else-native id, so one session can hold
// rows under two identities; every result is folded through the session
// catalog's identity alternates before it is listed or counted.

import (
	"net/http"
	"sort"

	"crossing-guard/store"
)

// taggedSession is one session carrying a tag, with the catalog facts that
// name it. Catalog is false when no catalog row matched the tagged id.
type taggedSession struct {
	Runtime   string   `json:"runtime,omitempty"`
	ID        string   `json:"id"`
	Title     string   `json:"title,omitempty"`
	Cwd       string   `json:"cwd,omitempty"`
	Modified  string   `json:"modified,omitempty"`
	Catalog   bool     `json:"catalog"`
	Tags      []string `json:"tags"`
	AgentKeys []string `json:"agent_keys"`
}

// tagLookupResponse answers GET /api/orchestration/tags?tag= or ?agent_key=.
type tagLookupResponse struct {
	Tags      []store.OrchestrationTag `json:"tags"`
	Sessions  []taggedSession          `json:"sessions"`
	Truncated bool                     `json:"truncated"`
}

// tagSummaryResponse answers GET /api/orchestration/tags/summary.
type tagSummaryResponse struct {
	Tags      []tagSummaryRow `json:"tags"`
	Truncated bool            `json:"truncated"`
}

type tagSummaryRow struct {
	Tag      string `json:"tag"`
	AgentKey string `json:"agent_key"`
	Sessions int    `json:"sessions"`
}

// sessionIndex looks up catalog rows by any identity candidate; a row is
// credited with a tag only when belongsTo (the runtime's MatchID) agrees, so
// a child rollout carrying its parent's thread id never takes the parent's tag.
type sessionIndex map[string][]SessionSummary

func sessionsByIdentity(sessions []SessionSummary) sessionIndex {
	out := sessionIndex{}
	for _, session := range sessions {
		for _, id := range candidateIDs(session) {
			out[id] = append(out[id], session)
		}
	}
	return out
}

// foldedSessionKey names the session a tag row belongs to after the fold.
func foldedSessionKey(tag store.OrchestrationTag, catalog sessionIndex) (string, SessionSummary, bool) {
	for _, row := range catalog[tag.SessionID] {
		if belongsTo(row, tag.Runtime, tag.SessionID) {
			return row.Runtime + "\x00" + row.ID, row, true
		}
	}
	return tag.Runtime + "\x00" + tag.SessionID, SessionSummary{}, false
}

// taggedSessions folds tag rows into one entry per session, newest tag first.
func taggedSessions(tags []store.OrchestrationTag, catalog sessionIndex) []taggedSession {
	out := []taggedSession{}
	index := map[string]int{}
	for _, tag := range tags {
		key, row, found := foldedSessionKey(tag, catalog)
		at, seen := index[key]
		if !seen {
			entry := taggedSession{Runtime: tag.Runtime, ID: tag.SessionID, Catalog: found, Tags: []string{}, AgentKeys: []string{}}
			if found {
				entry.Runtime, entry.ID, entry.Title, entry.Cwd = row.Runtime, row.ID, row.Title, row.Cwd
				if !row.Modified.IsZero() {
					entry.Modified = row.Modified.UTC().Format("2006-01-02T15:04:05Z")
				}
			}
			index[key], at = len(out), len(out)
			out = append(out, entry)
		}
		out[at].Tags = appendUnique(out[at].Tags, tag.Tag)
		out[at].AgentKeys = appendUnique(out[at].AgentKeys, tag.AgentKey)
	}
	return out
}

// tagSummary counts distinct folded sessions per (tag, agent_key).
func tagSummary(tags []store.OrchestrationTag, catalog sessionIndex) []tagSummaryRow {
	sessions := map[[2]string]map[string]bool{}
	for _, tag := range tags {
		key, _, _ := foldedSessionKey(tag, catalog)
		pair := [2]string{tag.Tag, tag.AgentKey}
		if sessions[pair] == nil {
			sessions[pair] = map[string]bool{}
		}
		sessions[pair][key] = true
	}
	out := make([]tagSummaryRow, 0, len(sessions))
	for pair, set := range sessions {
		out = append(out, tagSummaryRow{Tag: pair[0], AgentKey: pair[1], Sessions: len(set)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Sessions != out[j].Sessions {
			return out[i].Sessions > out[j].Sessions
		}
		if out[i].Tag != out[j].Tag {
			return out[i].Tag < out[j].Tag
		}
		return out[i].AgentKey < out[j].AgentKey
	})
	return out
}

func appendUnique(list []string, value string) []string {
	if value == "" {
		return list
	}
	for _, have := range list {
		if have == value {
			return list
		}
	}
	return append(list, value)
}

// handleTagLookup answers GET /api/orchestration/tags?tag= or ?agent_key=.
func handleTagLookup(w http.ResponseWriter, ix *store.Index, field, value string) {
	limits, _ := consoleConfig()
	limit := limits.Recall.TagLookupLimit
	rows, err := ix.ActiveOrchestrationTagsWith(field, value, timeNowUnix(), limit+1)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	truncated := len(rows) > limit
	if truncated {
		rows = rows[:limit]
	}
	writeJSON(w, tagLookupResponse{Tags: rows, Sessions: taggedSessions(rows, sessionsByIdentity(ScanSessions())),
		Truncated: truncated})
}

func handleTagSummary(w http.ResponseWriter, ix *store.Index) {
	limits, _ := consoleConfig()
	// Counts are over the newest tag_lookup_limit distinct (session identity,
	// agent tag) rows; truncated says when older ones were not counted.
	rows, truncated, err := ix.AllActiveOrchestrationTags(timeNowUnix(), limits.Recall.TagLookupLimit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, tagSummaryResponse{Tags: tagSummary(rows, sessionsByIdentity(ScanSessions())), Truncated: truncated})
}
