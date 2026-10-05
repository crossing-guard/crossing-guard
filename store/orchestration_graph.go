package store

import (
	"database/sql"
	"slices"
)

// Session-graph reads over the caused (orchestration) side. Kept in their own
// file so the managed-store owner and this read model can evolve without
// overlapping edits.

// CausedRelation is one caused-edge row resolved to session identities where
// the task rows carry them.
type CausedRelation struct {
	RelationshipID   string `json:"relationship_id"`
	GroupID          string `json:"group_id"`
	RunID            string `json:"run_id"`
	Role             string `json:"role"`
	State            string `json:"state"`
	Cycle            int64  `json:"cycle"`
	ParentTaskID     string `json:"parent_task_id"`
	ChildTaskID      string `json:"child_task_id"`
	ReplyTaskID      string `json:"reply_task_id,omitempty"`
	SourceTurnAnchor string `json:"source_turn_anchor,omitempty"`
	ReplyTurnAnchor  string `json:"reply_turn_anchor,omitempty"`
	ParentSessionID  string `json:"parent_session_id,omitempty"`
	ChildSessionID   string `json:"child_session_id,omitempty"`
	ParentRuntime    string `json:"parent_runtime,omitempty"`
	ChildRuntime     string `json:"child_runtime,omitempty"`
	BindingID        string `json:"binding_id"`
	UpdatedAt        int64  `json:"updated_at"`
}

// CausedRelationsForSession returns the caused edges touching one native
// session id, from either side, resolved through the runtime_task rows the
// task owner already keeps. No inference: a task without a recorded native id
// simply leaves that endpoint blank.
func (ix *Index) CausedRelationsForSession(nativeSessionID string, limit int) ([]CausedRelation, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	// The parent endpoint resolves through the parent task row where one
	// exists, and through the run's group root otherwise: a helper attached
	// to a terminal session has no parent runtime_task (helper-session-
	// attachment plan D7), so the group's recorded root identity is the fact.
	rows, err := ix.db.Query(`
SELECT rel.relationship_id, rel.group_id, rel.run_id, rel.role, rel.state, rel.cycle,
       rel.parent_task_id, rel.child_task_id, rel.reply_task_id,
       rel.source_turn_anchor, rel.reply_turn_anchor,
       COALESCE(NULLIF(pt.native_session_id,''), NULLIF(grp.root_native_session_id,''), grp.root_catalog_session_id, ''),
       COALESCE(ct.native_session_id,''),
       COALESCE(NULLIF(pt.runtime,''), grp.root_runtime, ''), COALESCE(ct.runtime,''),
       run.binding_id, rel.updated_at
FROM orchestration_relationship rel
JOIN orchestration_managed_run run ON run.run_id = rel.run_id
JOIN orchestration_group grp ON grp.group_id = rel.group_id
LEFT JOIN runtime_task pt ON pt.id = rel.parent_task_id
LEFT JOIN runtime_task ct ON ct.id = rel.child_task_id
WHERE pt.native_session_id = ? OR ct.native_session_id = ?
   OR (pt.id IS NULL AND (grp.root_native_session_id = ? OR grp.root_catalog_session_id = ?))
ORDER BY rel.updated_at DESC, rel.relationship_id DESC LIMIT ?`,
		nativeSessionID, nativeSessionID, nativeSessionID, nativeSessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CausedRelation{}
	for rows.Next() {
		var item CausedRelation
		if err := rows.Scan(&item.RelationshipID, &item.GroupID, &item.RunID, &item.Role, &item.State,
			&item.Cycle, &item.ParentTaskID, &item.ChildTaskID, &item.ReplyTaskID,
			&item.SourceTurnAnchor, &item.ReplyTurnAnchor,
			&item.ParentSessionID, &item.ChildSessionID, &item.ParentRuntime, &item.ChildRuntime,
			&item.BindingID, &item.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// AgentSessionRun reports whether a native session id is the child session of a
// managed run — i.e. an agent session — and returns that run's identity.
func (ix *Index) AgentSessionRun(nativeSessionID string) (runID, bindingID, role string, ok bool, err error) {
	err = ix.db.QueryRow(`
SELECT run.run_id, run.binding_id, run.role
FROM orchestration_managed_run run
JOIN runtime_task ct ON ct.id = run.child_task_id
WHERE ct.native_session_id = ? AND run.kind = ''
ORDER BY run.admitted_at DESC LIMIT 1`, nativeSessionID).Scan(&runID, &bindingID, &role)
	if err == sql.ErrNoRows {
		return "", "", "", false, nil
	}
	if err != nil {
		return "", "", "", false, err
	}
	return runID, bindingID, role, true, nil
}

// AgentSessionParent identifies, for one agent child session, the run role
// and the group's root (parent) session identities — everything the rail
// needs to fold the child under its parent (g4 plan §3a).
type AgentSessionParent struct {
	// Runtime is the agent session's own runtime; set only by
	// AllAgentSessionParents.
	Runtime              string `json:"runtime,omitempty"`
	Role                 string `json:"role"`
	RootRuntime          string `json:"root_runtime"`
	RootCatalogSessionID string `json:"root_catalog_session_id"`
	RootNativeSessionID  string `json:"root_native_session_id"`
	// Profiles are the run profiles seen for this agent session, sorted; set
	// only by AllAgentSessionParents.
	Profiles []string `json:"profiles,omitempty"`
}

// AllAgentSessionParents is AgentSessionParents with no id set: every agent
// child session and its parent, in one read. A caller that partitions the whole
// session list would otherwise pay one chunked query per 200 ids.
//
// Rows are ordered (id, root, role, profile), so a cut drops the same tail
// every time, and an agent listed with several roots keeps the first; its
// profiles are the union (session usage breakdown plan R-7, S-7). The limit
// counts rows, and one row per distinct profile.
func (ix *Index) AllAgentSessionParents(limit int) (map[string]AgentSessionParent, bool, error) {
	rows, err := ix.db.Query(`
SELECT ct.native_session_id, run.role,
       COALESCE(grp.root_runtime,''), COALESCE(grp.root_catalog_session_id,''), COALESCE(grp.root_native_session_id,''),
       run.profile_id, ct.runtime
FROM orchestration_managed_run run
JOIN runtime_task ct ON ct.id = run.child_task_id
JOIN orchestration_group grp ON grp.group_id = run.group_id
WHERE run.kind = '' AND ct.native_session_id <> ''
UNION
SELECT grp.helper_native_session_id, run.role,
       COALESCE(grp.root_runtime,''), COALESCE(grp.root_catalog_session_id,''), COALESCE(grp.root_native_session_id,''),
       run.profile_id, grp.helper_runtime
FROM orchestration_group grp
JOIN orchestration_managed_run run ON run.group_id = grp.group_id AND run.kind = '' AND run.child_task_id <> ''
WHERE grp.helper_native_session_id <> ''
ORDER BY 1, 3, 4, 5, 2, 6, 7
LIMIT ?`, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := map[string]AgentSessionParent{}
	count := 0
	for rows.Next() {
		var id, profile string
		var parent AgentSessionParent
		if err := rows.Scan(&id, &parent.Role, &parent.RootRuntime, &parent.RootCatalogSessionID,
			&parent.RootNativeSessionID, &profile, &parent.Runtime); err != nil {
			return nil, false, err
		}
		count++
		if count > limit {
			return out, true, rows.Err()
		}
		kept, seen := out[id]
		if !seen {
			kept = parent
		}
		if profile != "" && !slices.Contains(kept.Profiles, profile) {
			kept.Profiles = append(kept.Profiles, profile) // rows arrive sorted, so the set stays sorted per root
		}
		out[id] = kept
	}
	for id, parent := range out {
		slices.Sort(parent.Profiles)
		out[id] = parent
	}
	return out, false, rows.Err()
}

// AgentSessionParents reports, for a set of native session ids, which are
// agent child sessions and who their parent is, resolved through the run's
// group. Chunk-looped: every supplied id is checked — no silent truncation
// (g4 plan §3a; the AgentSessionRoles 200-cap lesson). kind=” keeps reply/
// correction/delegate tasks — which resume the PARENT session — from marking
// the parent as an agent session.
func (ix *Index) AgentSessionParents(nativeSessionIDs []string) (map[string]AgentSessionParent, error) {
	out := map[string]AgentSessionParent{}
	const chunk = 200
	for start := 0; start < len(nativeSessionIDs); start += chunk {
		end := start + chunk
		if end > len(nativeSessionIDs) {
			end = len(nativeSessionIDs)
		}
		ids := nativeSessionIDs[start:end]
		placeholders := ""
		args := make([]any, 0, len(ids))
		for index, id := range ids {
			if index > 0 {
				placeholders += ","
			}
			placeholders += "?"
			args = append(args, id)
		}
		rows, err := ix.db.Query(`
SELECT ct.native_session_id, run.role,
       COALESCE(grp.root_runtime,''), COALESCE(grp.root_catalog_session_id,''), COALESCE(grp.root_native_session_id,'')
FROM orchestration_managed_run run
JOIN runtime_task ct ON ct.id = run.child_task_id
JOIN orchestration_group grp ON grp.group_id = run.group_id
WHERE run.kind = '' AND ct.native_session_id IN (`+placeholders+`)
UNION
SELECT grp.helper_native_session_id, run.role,
       COALESCE(grp.root_runtime,''), COALESCE(grp.root_catalog_session_id,''), COALESCE(grp.root_native_session_id,'')
FROM orchestration_group grp
JOIN orchestration_managed_run run ON run.group_id = grp.group_id AND run.kind = '' AND run.child_task_id <> ''
WHERE grp.helper_native_session_id IN (`+placeholders+`)`, append(append([]any{}, args...), args...)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			var parent AgentSessionParent
			if err := rows.Scan(&id, &parent.Role, &parent.RootRuntime, &parent.RootCatalogSessionID, &parent.RootNativeSessionID); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = parent
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}
