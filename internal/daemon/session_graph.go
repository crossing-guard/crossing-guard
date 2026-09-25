package daemon

import (
	"net/http"
	"path/filepath"
	"sort"

	"crossing-guard/harvest"
	"crossing-guard/store"
)

// The session graph read model: ONE related-sessions presentation serving both
// provenance classes — observed native edges derived per scan through the
// harvest capabilities, and caused agent edges from the orchestration store.
// Provenance is a field on every row; no code path promotes one class into the
// other (graph design review §3.1, adopted O1=B / derived-first).

type relatedSession struct {
	Kind       string `json:"kind"`       // spawned | messaged | waited_on | read_context_of | reviewed-by | prompted-by
	Provenance string `json:"provenance"` // observed | caused
	Direction  string `json:"direction"`  // parent | child | peer | self
	Runtime    string `json:"runtime,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	Label      string `json:"label,omitempty"`
	Role       string `json:"role,omitempty"`
	Anchor     string `json:"anchor,omitempty"`
	Cycle      int64  `json:"cycle,omitempty"`
	State      string `json:"state,omitempty"`
	Unresolved bool   `json:"unresolved,omitempty"`
	RunID      string `json:"run_id,omitempty"`
	TaskID     string `json:"task_id,omitempty"`
}

type relatedSessionsResponse struct {
	Runtime  string           `json:"runtime"`
	ID       string           `json:"id"`
	Related  []relatedSession `json:"related"`
	Coverage string           `json:"coverage"`
}

func registerSessionGraphRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/session/related", handleSessionRelated)
}

func handleSessionRelated(w http.ResponseWriter, r *http.Request) {
	runtime, id := r.URL.Query().Get("runtime"), r.URL.Query().Get("id")
	if runtime == "" || id == "" {
		http.Error(w, "runtime and id are required", 400)
		return
	}
	response := relatedSessionsResponse{Runtime: runtime, ID: id, Related: []relatedSession{}}
	summary, found := harvest.Find(runtime, id)
	coverageNotes := ""
	if found {
		// Observed lineage: this session's own child-side facts.
		if facts, ok := harvest.Lineage(summary); ok {
			if facts.Parent.ID != "" {
				response.Related = append(response.Related, relatedSession{
					Kind: harvest.EdgeKindSpawned, Provenance: harvest.EdgeProvenanceObserved,
					Direction: "parent", Runtime: facts.Parent.Runtime, SessionID: facts.Parent.ID,
					Role: facts.Role, Label: lineageLabel(facts.Kind, facts.Nickname)})
			}
			for _, child := range facts.Children {
				response.Related = append(response.Related, relatedSession{
					Kind: harvest.EdgeKindSpawned, Provenance: harvest.EdgeProvenanceObserved,
					Direction: "child", Runtime: summary.Runtime, SessionID: child.ID,
					Role: child.Role, Label: lineageLabel(child.Kind, child.Nickname)})
			}
		}
		// Observed edges: lazy full-transcript derivation for the selected session
		// only (never in the rail path).
		if edges, ok, err := harvest.Edges(summary); err != nil {
			coverageNotes = "observed edge derivation failed: " + err.Error()
		} else if ok {
			for _, edge := range edges {
				row := relatedSession{Kind: edge.Kind, Provenance: edge.Provenance, Anchor: edge.Anchor}
				other := edge.To
				row.Direction = "child"
				if edge.To.ID == summary.ID || (edge.To.ID == "" && edge.From.ID != summary.ID) {
					other, row.Direction = edge.From, "parent"
				}
				if edge.Kind == harvest.EdgeKindMessaged || edge.Kind == harvest.EdgeKindReadContextOf {
					row.Direction = "peer"
				}
				if edge.Kind == harvest.EdgeKindWaitedOn && other.ID == "" {
					row.Direction = "self"
				}
				row.Runtime, row.SessionID, row.Unresolved = other.Runtime, other.ID, other.Unresolved
				if other.ID == "" && other.Raw != "" {
					row.Label, row.Unresolved = other.Raw, true
				}
				response.Related = append(response.Related, row)
			}
		}
	} else {
		coverageNotes = "session not found in the current scan; observed edges unavailable"
	}
	// Caused: the orchestration ledger, resolved through task-owned native ids.
	if governor != nil && governor.ix != nil {
		// Every identity alternate is asked (ID, MetaID, ThreadID): the hook
		// path names a session by its native id, the catalog by its stem, and
		// which one the ledger recorded depends on the runtime.
		nativeID := id
		if found && summary.MetaID != "" {
			nativeID = summary.MetaID
		}
		selfIDs := map[string]bool{id: true, nativeID: true}
		alternates := []string{nativeID}
		if found {
			selfIDs[summary.ID] = true
			for _, alternate := range []string{summary.ID, summary.MetaID, summary.ThreadID} {
				if alternate != "" && !containsString(alternates, alternate) {
					alternates = append(alternates, alternate)
				}
			}
		}
		relations := []store.CausedRelation{}
		seen := map[string]bool{}
		var err error
		for _, alternate := range alternates {
			var batch []store.CausedRelation
			batch, err = governor.ix.CausedRelationsForSession(alternate, 100)
			if err != nil {
				break
			}
			for _, rel := range batch {
				if !seen[rel.RelationshipID] {
					seen[rel.RelationshipID] = true
					relations = append(relations, rel)
				}
			}
		}
		if err == nil {
			for _, rel := range relations {
				row := relatedSession{Provenance: "caused", Role: rel.Role, State: rel.State,
					Cycle: rel.Cycle, RunID: rel.RunID, Anchor: rel.SourceTurnAnchor}
				if rel.ChildSessionID != "" && !selfIDs[rel.ChildSessionID] {
					row.Kind, row.Direction = "reviewed-by", "child"
					row.Runtime, row.SessionID, row.TaskID = rel.ChildRuntime, rel.ChildSessionID, rel.ChildTaskID
				} else {
					row.Kind, row.Direction = "reviews", "parent"
					row.Runtime, row.SessionID, row.TaskID = rel.ParentRuntime, rel.ParentSessionID, rel.ParentTaskID
					if rel.ReplyTaskID != "" {
						row.Kind = "prompted-by"
						row.Anchor = rel.ReplyTurnAnchor
					}
				}
				response.Related = append(response.Related, row)
			}
		} else if coverageNotes == "" {
			coverageNotes = "caused relations unavailable: " + err.Error()
		}
	}
	sort.SliceStable(response.Related, func(i, j int) bool {
		return response.Related[i].Provenance < response.Related[j].Provenance
	})
	response.Coverage = coverageNotes
	writeJSON(w, response)
}

func lineageLabel(kind, nickname string) string {
	if nickname != "" {
		return kind + " · " + nickname
	}
	return kind
}

// agentWatch is the "who is watching this session" fact: an enabled binding
// whose scope covers the session, shown BEFORE any run exists (redesign §3.3 —
// invisible coverage was a recorded trust failure).
type agentWatch struct {
	BindingID string `json:"binding_id"`
	Role      string `json:"role"`
	Priority  int64  `json:"priority"`
	ProfileID string `json:"profile_id"`
}

type sessionDetailWithAgents struct {
	*SessionDetail
	WatchedBy         []agentWatch `json:"watched_by,omitempty"`
	OrchestrationRole string       `json:"orchestration_role,omitempty"`
	AgentRunID        string       `json:"agent_run_id,omitempty"`
}

// decorateSessionAgents wraps a session detail with agent coverage and, when
// the session IS an agent's own run, its role — without touching the harvest
// detail owner. Failures degrade to the undecorated detail; coverage is a
// convenience read, never a gate.
func decorateSessionAgents(detail *SessionDetail) any {
	out := sessionDetailWithAgents{SessionDetail: detail}
	if governor == nil || governor.ix == nil {
		return out
	}
	if bindings, err := governor.ix.ManagedBindings(true); err == nil {
		for _, binding := range bindings {
			if binding.ScopeRuntime != "" && binding.ScopeRuntime != detail.Runtime {
				continue
			}
			// One "watching" semantic, mirroring the host's trigger-time
			// bindingScopeMatches (g4 plan §5.9): a session-scoped binding
			// matches ANY exact identity alternate (artifact id, vendor meta
			// id, vendor thread id — the task row's native id is the thread
			// id for codex), and the project root matches by exact cleaned
			// equality — a subdirectory or cwd-less session never triggers,
			// so calling it "watched" would be a lie.
			if binding.ScopeSession != "" && binding.ScopeSession != detail.ID &&
				(detail.MetaID == "" || binding.ScopeSession != detail.MetaID) &&
				(detail.ThreadID == "" || binding.ScopeSession != detail.ThreadID) {
				continue
			}
			if binding.ProjectRoot != "" &&
				(detail.Cwd == "" || filepath.Clean(binding.ProjectRoot) != filepath.Clean(detail.Cwd)) {
				continue
			}
			out.WatchedBy = append(out.WatchedBy, agentWatch{BindingID: binding.BindingID,
				Role: binding.Role, Priority: binding.Priority, ProfileID: binding.ProfileID})
		}
	}
	nativeID := detail.ID
	if detail.MetaID != "" {
		nativeID = detail.MetaID
	}
	if runID, _, role, ok, err := governor.ix.AgentSessionRun(nativeID); err == nil && ok {
		out.OrchestrationRole, out.AgentRunID = role, runID
	}
	return out
}
