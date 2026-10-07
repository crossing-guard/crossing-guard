package daemon

import (
	"net/http"
	"sort"
	"strconv"
	"strings"

	"crossing-guard/harvest"
	"crossing-guard/store"
)

// The session graph read model: ONE related-sessions presentation serving both
// provenance classes — observed native edges derived per scan through the
// harvest capabilities, and caused agent edges from the orchestration store.
// Provenance is a field on every row; no code path promotes one class into the
// other (graph design review §3.1, adopted O1=B / derived-first).

type relatedSession struct {
	Kind       string `json:"kind"`       // spawned | messaged | waited_on | read_context_of | reviewed-by | reviews | resumed-by
	Provenance string `json:"provenance"` // observed | caused
	Direction  string `json:"direction"`  // parent | child | descendant | peer | self
	Runtime    string `json:"runtime,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	// Openable: SessionID can name a session the session route could open
	// (the runtime's IDShape does not reject it). A client links only these.
	Openable    bool   `json:"openable"`
	Label       string `json:"label,omitempty"`
	Role        string `json:"role,omitempty"`
	Description string `json:"description,omitempty"`
	// Via names the native child that spawned a descendant row.
	Via        string `json:"via,omitempty"`
	Anchor     string `json:"anchor,omitempty"`
	Cycle      int64  `json:"cycle,omitempty"`
	State      string `json:"state,omitempty"`
	Unresolved bool   `json:"unresolved,omitempty"`
	RunID      string `json:"run_id,omitempty"`
	TaskID     string `json:"task_id,omitempty"`
	// Runs and Replies count the caused runs collapsed into one row; the
	// other caused fields come from the newest of them.
	Runs    int `json:"runs,omitempty"`
	Replies int `json:"replies,omitempty"`
}

type relatedSessionsResponse struct {
	Runtime  string           `json:"runtime"`
	ID       string           `json:"id"`
	Related  []relatedSession `json:"related"`
	Coverage string           `json:"coverage"`
}

// causedRelationsReadLimit bounds one identity's caused read.
const causedRelationsReadLimit = 100

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
	alternates := relatedIdentityAlternates(id, summary, found)
	self := relatedSelfIDs(alternates, summary, found)
	var notes []string
	if found {
		facts, _ := harvest.Lineage(summary)
		// Observed edges: lazy full-transcript derivation for the selected
		// session only (never in the rail path).
		edges, _, err := harvest.Edges(summary)
		if err != nil {
			notes = append(notes, "observed edge derivation failed: "+err.Error())
			edges = nil
		}
		response.Related = append(response.Related, observedRelatedRows(summary.Runtime, self, facts, edges)...)
	} else {
		notes = append(notes, "session not found in the current scan; observed edges unavailable")
	}
	relations, causedNotes := readCausedRelations(alternates)
	notes = append(notes, causedNotes...)
	response.Related = append(response.Related, causedRelatedRows(relations, self)...)
	sort.SliceStable(response.Related, func(i, j int) bool {
		return response.Related[i].Provenance < response.Related[j].Provenance
	})
	response.Coverage = strings.Join(notes, "; ")
	writeJSON(w, response)
}

// relatedIdentityAlternates lists every identity the session may be recorded
// under: the hook path names a session by its native id, the catalog by its
// stem, and which one the ledger recorded depends on the runtime. Only ids
// the runtime itself says name THIS session count: a codex subagent carries
// its parent's thread id, which names the parent, never the child.
func relatedIdentityAlternates(id string, summary harvest.SessionSummary, found bool) []string {
	alternates := []string{id}
	if !found {
		return alternates
	}
	for _, alternate := range []string{summary.MetaID, summary.ID, summary.ThreadID} {
		if alternate != "" && !containsString(alternates, alternate) && harvest.MatchID(summary, alternate) {
			alternates = append(alternates, alternate)
		}
	}
	return alternates
}

// relatedSelfIDs is every identity the viewed session answers to — its
// alternates plus the canonical id its own edges are written under — so no
// row can name the open session as its own counterpart.
func relatedSelfIDs(alternates []string, summary harvest.SessionSummary, found bool) map[string]bool {
	self := map[string]bool{}
	for _, alternate := range alternates {
		self[alternate] = true
	}
	if found {
		self[harvest.CanonicalID(summary)] = true
	}
	delete(self, "")
	return self
}

// openableSession: a row links only when its id could name a session of its
// runtime. The runtime's IDShape rejects ids that cannot (a Claude subagent's
// agent id); an accepted id may still fail to open, which the console's
// open-failure path reports.
func openableSession(runtime, id string) bool {
	return runtime != "" && id != "" && harvest.CouldMatchID(runtime, id)
}

// observedRelatedRows is the observed half: the lineage capability's parent and
// children, then the edge capability's edges. The two capabilities can state
// one relationship twice (a Claude background agent is both a subagents file
// and an async-launch record; an OpenCode child's parent is both lineage and a
// spawned edge), and one relationship is one row: a repeat folds into the
// first row, lending it an anchor it lacked.
func observedRelatedRows(runtime string, self map[string]bool, facts harvest.LineageFacts, edges []harvest.Edge) []relatedSession {
	rows := []relatedSession{}
	index := map[string]int{}
	add := func(row relatedSession) {
		row.Provenance = harvest.EdgeProvenanceObserved
		row.Openable = openableSession(row.Runtime, row.SessionID)
		if row.SessionID != "" {
			key := row.Kind + "\x00" + row.Direction + "\x00" + row.Runtime + "\x00" + row.SessionID
			if i, ok := index[key]; ok {
				if rows[i].Anchor == "" {
					rows[i].Anchor = row.Anchor
				}
				return
			}
			index[key] = len(rows)
		}
		rows = append(rows, row)
	}
	if facts.Parent.ID != "" {
		add(relatedSession{Kind: harvest.EdgeKindSpawned, Direction: "parent",
			Runtime: facts.Parent.Runtime, SessionID: facts.Parent.ID,
			Role: facts.Role, Label: lineageLabel(facts.Kind, facts.Nickname)})
	}
	for _, child := range facts.Children {
		direction := "child"
		if child.Via != "" {
			direction = "descendant"
		}
		add(relatedSession{Kind: harvest.EdgeKindSpawned, Direction: direction,
			Runtime: runtime, SessionID: child.ID, Via: child.Via,
			Role: child.Role, Description: child.Description, Label: lineageLabel(child.Kind, child.Nickname)})
	}
	for _, edge := range edges {
		add(observedEdgeRow(self, edge))
	}
	return rows
}

// observedEdgeRow orients one edge from the viewed session's side. Self is
// every identity the session answers to: a codex edge names its session by
// the thread id while the catalog knows it by the rollout stem.
func observedEdgeRow(self map[string]bool, edge harvest.Edge) relatedSession {
	row := relatedSession{Kind: edge.Kind, Anchor: edge.Anchor, Direction: "child"}
	other := edge.To
	if self[edge.To.ID] || (edge.To.ID == "" && edge.From.ID != "" && !self[edge.From.ID]) {
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
	return row
}

// readCausedRelations asks the orchestration ledger under every identity
// alternate. Every failure or limit is a note: a read that could not answer
// must not look like "no related sessions".
func readCausedRelations(alternates []string) ([]store.CausedRelation, []string) {
	if governor == nil || governor.ix == nil {
		return nil, []string{"caused relations unavailable: the orchestration store is not open"}
	}
	return collectCausedRelations(governor.ix.CausedRelationsForSession, alternates)
}

// collectCausedRelations keeps each relationship once however many identities
// returned it (so a run is never counted twice) and says when any identity's
// read stopped at its limit. A failed read returns no rows at all: partial
// rows under an "unavailable" note would present counts nobody can trust.
func collectCausedRelations(read func(string, int) ([]store.CausedRelation, error), alternates []string) ([]store.CausedRelation, []string) {
	relations := []store.CausedRelation{}
	seen := map[string]bool{}
	truncated := false
	for _, alternate := range alternates {
		batch, err := read(alternate, causedRelationsReadLimit)
		if err != nil {
			return nil, []string{"caused relations unavailable: " + err.Error()}
		}
		truncated = truncated || len(batch) >= causedRelationsReadLimit
		for _, rel := range batch {
			if !seen[rel.RelationshipID] {
				seen[rel.RelationshipID] = true
				relations = append(relations, rel)
			}
		}
	}
	if truncated {
		return relations, []string{"caused relations: only the newest " + strconv.Itoa(causedRelationsReadLimit) +
			" per identity were read; run counts may be low and older sessions may be missing"}
	}
	return relations, nil
}

// causedRelatedRows is the caused half: one row per counterpart session,
// direction and role, carrying the newest run's state and how many runs and
// replies it stands for. A run whose child task resumed this same session is
// a self row, never a link to the session already open.
func causedRelatedRows(relations []store.CausedRelation, self map[string]bool) []relatedSession {
	sorted := append([]store.CausedRelation(nil), relations...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].UpdatedAt != sorted[j].UpdatedAt {
			return sorted[i].UpdatedAt > sorted[j].UpdatedAt
		}
		return sorted[i].RelationshipID > sorted[j].RelationshipID
	})
	rows := []relatedSession{}
	index := map[string]int{}
	for _, rel := range sorted {
		row := causedRow(rel, self)
		reply := 0
		if rel.ReplyTaskID != "" {
			reply = 1
		}
		// A run whose counterpart is not known yet has no identity to
		// collapse on; it stays its own row.
		if row.SessionID != "" || row.Direction == "self" {
			key := row.Direction + "\x00" + row.Runtime + "\x00" + row.SessionID + "\x00" + row.Role
			if i, ok := index[key]; ok {
				rows[i].Runs++
				rows[i].Replies += reply
				continue
			}
			index[key] = len(rows)
		}
		row.Runs, row.Replies = 1, reply
		rows = append(rows, row)
	}
	return rows
}

func causedRow(rel store.CausedRelation, self map[string]bool) relatedSession {
	row := relatedSession{Provenance: "caused", Role: rel.Role, State: rel.State,
		Cycle: rel.Cycle, RunID: rel.RunID, Anchor: rel.SourceTurnAnchor}
	childSelf, parentSelf := self[rel.ChildSessionID], self[rel.ParentSessionID]
	switch {
	case childSelf && parentSelf:
		row.Kind, row.Direction = "resumed-by", "self"
		row.Runtime, row.TaskID = rel.ChildRuntime, rel.ChildTaskID
		return row
	case childSelf:
		row.Kind, row.Direction = "reviews", "parent"
	case parentSelf:
		row.Kind, row.Direction = "reviewed-by", "child"
	case rel.ChildSessionID != "":
		// Neither endpoint answers to an identity this session holds: the
		// ledger matched it anyway, so keep the historical orientation.
		row.Kind, row.Direction = "reviewed-by", "child"
	default:
		row.Kind, row.Direction = "reviews", "parent"
	}
	if row.Direction == "child" {
		row.Runtime, row.SessionID, row.TaskID = rel.ChildRuntime, rel.ChildSessionID, rel.ChildTaskID
	} else {
		row.Runtime, row.SessionID, row.TaskID = rel.ParentRuntime, rel.ParentSessionID, rel.ParentTaskID
	}
	row.Openable = openableSession(row.Runtime, row.SessionID)
	return row
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
	// Events shadows the detail's rows with the transcript wire shape: each
	// tool_call row carries the detector facts it classifies as, so a view
	// profile can match exec:run rather than a vendor tool name.
	Events []liveEvent `json:"events"`
	// Identities are the ids that name this session (sessionIdentities): a
	// client reads its runs and tags under these and never derives them from
	// meta_id or thread_id, since a subagent's thread id names its parent.
	Identities []string `json:"identities"`
	// CwdKey is the folder identity of Cwd (projectRootKey): an opaque value
	// the browser compares with each agent's project_root_key, never a path.
	CwdKey            string       `json:"cwd_key,omitempty"`
	WatchedBy         []agentWatch `json:"watched_by,omitempty"`
	OrchestrationRole string       `json:"orchestration_role,omitempty"`
	AgentRunID        string       `json:"agent_run_id,omitempty"`
}

// decorateSessionAgents wraps a session detail with agent coverage and, when
// the session IS an agent's own run, its role — without touching the harvest
// detail owner. Failures degrade to the undecorated detail; coverage is a
// convenience read, never a gate.
func decorateSessionAgents(detail *SessionDetail) sessionDetailWithAgents {
	out := sessionDetailWithAgents{SessionDetail: detail, Events: withTranscriptFacts(detail.Events),
		Identities: sessionIdentities(detail.SessionSummary)}
	if governor == nil || governor.ix == nil {
		return out
	}
	if bindings, err := governor.ix.ManagedBindings(true); err == nil {
		// The folder key is for comparing with places, so the session's cwd
		// is resolved only when an enabled place exists: opening a session
		// never touches its folder otherwise (plan §5).
		folder := newFolderScope(detail.Cwd)
		if len(bindings) > 0 {
			out.CwdKey = folder.Key()
		}
		for _, binding := range bindings {
			if binding.ScopeRuntime != "" && binding.ScopeRuntime != detail.Runtime {
				continue
			}
			// One "watching" semantic, mirroring the host's trigger-time
			// bindingScopeMatches (g4 plan §5.9): a session-scoped binding
			// matches ANY exact identity alternate (artifact id, vendor meta
			// id, vendor thread id — the task row's native id is the thread
			// id for codex), and the project root is the session's own folder
			// under any spelling (folderScope) — a subdirectory, a cwd-less
			// session or a rootless binding never triggers, so calling it
			// "watched" would be a lie. A subagent keeps its parent's thread
			// here, unlike in Identities: its activity is recorded under that
			// thread, so the thread's bindings fire on it
			// (child-thread-identity plan D-1).
			if binding.ScopeSession != "" && binding.ScopeSession != detail.ID &&
				(detail.MetaID == "" || binding.ScopeSession != detail.MetaID) &&
				(detail.ThreadID == "" || binding.ScopeSession != detail.ThreadID) {
				continue
			}
			if !folder.matchesRoot(binding.ProjectRoot) {
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
