package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// Schema 24: the agents redesign. Binding roles are the redesign taxonomy
// (reviewer/follower/helper); historical run and relationship rows stay but
// their legacy role labels map onto that taxonomy at migration (a comment on
// the mapping lives with migrateOrchestrationAgentsV24). `deferred` joins the
// run states for priority arbitration. The stream-position table replaces the ambiguous
// orchestration_cursor name (this repository also ships a `cursor` runtime).
// Turn/reply anchors, declared tags, priority, owner limits, group notes, and
// model-claimed tag rows are new. Owner budget POLICY lives with the host; the
// store enforces shape only.
//
// Schema 25 (provider-outage plan): `parked` joins the run states — a run
// whose PROVIDER died (quota/unreachable) parks non-terminally with its
// idempotency and signal anchor intact, awaiting a cadence retry or a
// user-directed reroute; bindings gain `routes_json`, the user-authored
// ordered fallback chain (empty = today's single route, byte-identical
// behavior).
const orchestrationManagedSchemaV25 = `
CREATE TABLE IF NOT EXISTS orchestration_managed_binding(
  binding_id TEXT PRIMARY KEY,
  state TEXT NOT NULL CHECK(state IN ('enabled','disabled')),
  role TEXT NOT NULL CHECK(role IN ('reviewer','follower','helper')),
  priority INTEGER NOT NULL DEFAULT 0 CHECK(priority BETWEEN -1000 AND 1000),
  scope_runtime TEXT NOT NULL DEFAULT '',
  scope_session TEXT NOT NULL DEFAULT '',
  project_root TEXT NOT NULL,
  profile_id TEXT NOT NULL,
  profile_source_digest TEXT NOT NULL,
  profile_bundle_digest TEXT NOT NULL,
  runtime TEXT NOT NULL,
  model TEXT NOT NULL DEFAULT '',
  mode TEXT NOT NULL,
  authority_json TEXT NOT NULL CHECK(json_valid(authority_json)),
  allowed_profiles_json TEXT NOT NULL CHECK(json_valid(allowed_profiles_json)),
  declared_tags_json TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(declared_tags_json)),
  limits_json TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(limits_json)),
  auto_action INTEGER NOT NULL DEFAULT 0 CHECK(auto_action IN (0,1)),
  watch_natural INTEGER NOT NULL DEFAULT 0 CHECK(watch_natural IN (0,1)),
  routes_json TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(routes_json)),
  state_token TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS orchestration_managed_binding_scope
  ON orchestration_managed_binding(state,role,scope_runtime,scope_session,project_root);

CREATE TABLE IF NOT EXISTS orchestration_group(
  group_id TEXT PRIMARY KEY,
  binding_id TEXT NOT NULL REFERENCES orchestration_managed_binding(binding_id) ON DELETE RESTRICT,
  state TEXT NOT NULL CHECK(state IN ('active','completed','disabled','unknown')),
  root_task_id TEXT NOT NULL,
  root_runtime TEXT NOT NULL,
  root_catalog_session_id TEXT NOT NULL DEFAULT '',
  root_native_session_id TEXT NOT NULL DEFAULT '',
  project_root TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  helper_runtime TEXT NOT NULL DEFAULT '',
  helper_native_session_id TEXT NOT NULL DEFAULT '',
  helper_turns INTEGER NOT NULL DEFAULT 0,
  helper_session_replaced INTEGER NOT NULL DEFAULT 0,
  helper_transcript_seq INTEGER NOT NULL DEFAULT 0,
  pending_signal_json TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(pending_signal_json)),
  pending_producer TEXT NOT NULL DEFAULT '',
  pending_event_id INTEGER NOT NULL DEFAULT 0,
  pending_coalesced INTEGER NOT NULL DEFAULT 0,
  pending_at INTEGER NOT NULL DEFAULT 0,
  pending_dropped INTEGER NOT NULL DEFAULT 0,
  pending_dropped_reason TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS orchestration_group_root ON orchestration_group(root_task_id,updated_at DESC);
CREATE INDEX IF NOT EXISTS orchestration_group_identity
  ON orchestration_group(binding_id,root_native_session_id,root_catalog_session_id);

CREATE TABLE IF NOT EXISTS orchestration_managed_run(
  run_id TEXT PRIMARY KEY,
  idempotency_key TEXT NOT NULL UNIQUE,
  group_id TEXT NOT NULL REFERENCES orchestration_group(group_id) ON DELETE RESTRICT,
  binding_id TEXT NOT NULL REFERENCES orchestration_managed_binding(binding_id) ON DELETE RESTRICT,
  binding_state_token TEXT NOT NULL,
  role TEXT NOT NULL,
  kind TEXT NOT NULL DEFAULT '' CHECK(kind IN ('','reply','correction','delegate')),
  profile_id TEXT NOT NULL,
  profile_source_digest TEXT NOT NULL,
  profile_bundle_digest TEXT NOT NULL,
  source_task_id TEXT NOT NULL,
  source_event_id INTEGER NOT NULL,
  child_task_id TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL CHECK(state IN ('admitted','running','completed','failed','suppressed','deferred','parked','unknown')),
  action TEXT NOT NULL DEFAULT '',
  message TEXT NOT NULL DEFAULT '' CHECK(length(message)<=262144),
  citations_json TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(citations_json)),
  detail_json TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(detail_json)),
  error_class TEXT NOT NULL DEFAULT '',
  recovery TEXT NOT NULL DEFAULT '' CHECK(length(recovery)<=2000),
  admitted_at INTEGER NOT NULL,
  started_at INTEGER NOT NULL DEFAULT 0,
  completed_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS orchestration_managed_run_group
  ON orchestration_managed_run(group_id,admitted_at DESC,run_id DESC);
CREATE INDEX IF NOT EXISTS orchestration_managed_run_child
  ON orchestration_managed_run(child_task_id) WHERE child_task_id!='';

CREATE TABLE IF NOT EXISTS orchestration_relationship(
  relationship_id TEXT PRIMARY KEY,
  group_id TEXT NOT NULL REFERENCES orchestration_group(group_id) ON DELETE RESTRICT,
  run_id TEXT NOT NULL UNIQUE REFERENCES orchestration_managed_run(run_id) ON DELETE RESTRICT,
  parent_task_id TEXT NOT NULL,
  child_task_id TEXT NOT NULL,
  reply_task_id TEXT NOT NULL DEFAULT '',
  source_turn_anchor TEXT NOT NULL DEFAULT '',
  reply_turn_anchor TEXT NOT NULL DEFAULT '',
  cycle INTEGER NOT NULL DEFAULT 0 CHECK(cycle>=0),
  role TEXT NOT NULL,
  depth INTEGER NOT NULL CHECK(depth BETWEEN 0 AND 8),
  hops INTEGER NOT NULL CHECK(hops BETWEEN 0 AND 8),
  state TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS orchestration_control(
  control_id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL UNIQUE REFERENCES orchestration_managed_run(run_id) ON DELETE RESTRICT,
  task_id TEXT NOT NULL,
  requested_action TEXT NOT NULL CHECK(requested_action IN ('interrupt')),
  request_state TEXT NOT NULL CHECK(request_state IN ('requested','cancelled')),
  outcome TEXT NOT NULL DEFAULT '',
  error_text TEXT NOT NULL DEFAULT '',
  requested_at INTEGER NOT NULL,
  completed_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS orchestration_stream_position(
  source_kind TEXT PRIMARY KEY,
  position INTEGER NOT NULL CHECK(position>=0),
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS orchestration_group_note(
  note_id TEXT PRIMARY KEY,
  group_id TEXT NOT NULL REFERENCES orchestration_group(group_id) ON DELETE RESTRICT,
  body TEXT NOT NULL CHECK(length(body) BETWEEN 1 AND 4000),
  created_at INTEGER NOT NULL,
  retracted_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS orchestration_group_note_group
  ON orchestration_group_note(group_id,created_at);

CREATE TABLE IF NOT EXISTS orchestration_tag(
  tag_id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL REFERENCES orchestration_managed_run(run_id) ON DELETE RESTRICT,
  binding_id TEXT NOT NULL,
  agent_key TEXT NOT NULL,
  tag TEXT NOT NULL CHECK(length(tag) BETWEEN 1 AND 64),
  provenance TEXT NOT NULL CHECK(provenance='model-claimed'),
  runtime TEXT NOT NULL DEFAULT '',
  session_id TEXT NOT NULL DEFAULT '',
  anchor TEXT NOT NULL DEFAULT '',
  applied_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL DEFAULT 0,
  retracted_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS orchestration_tag_session
  ON orchestration_tag(session_id,tag,applied_at DESC);`

var ErrManagedBindingConflict = errors.New("managed orchestration binding changed")

// ManagedLimits carries the owner-set budgets a binding runs under. Zero means
// "use the shipped configuration default" resolved by the host; the store never
// hardcodes a budget.
type ManagedLimits struct {
	MaxTotal       int64 `json:"max_total,omitempty"`
	MaxActive      int64 `json:"max_active,omitempty"`
	MaxHops        int64 `json:"max_hops,omitempty"`
	LoopBudget     int64 `json:"loop_budget,omitempty"`
	MaxGroupTokens int64 `json:"max_group_tokens,omitempty"`
	MaxAgentTokens int64 `json:"max_agent_tokens,omitempty"`
}

type ManagedBinding struct {
	BindingID           string              `json:"binding_id"`
	State               string              `json:"state"`
	Role                string              `json:"role"`
	Priority            int64               `json:"priority"`
	ScopeRuntime        string              `json:"scope_runtime,omitempty"`
	ScopeSession        string              `json:"scope_session,omitempty"`
	ProjectRoot         string              `json:"project_root"`
	ProfileID           string              `json:"profile_id"`
	ProfileSourceDigest string              `json:"profile_source_digest"`
	ProfileBundleDigest string              `json:"profile_bundle_digest"`
	Runtime             string              `json:"runtime"`
	Model               string              `json:"model,omitempty"`
	Mode                string              `json:"mode"`
	Authority           []string            `json:"granted_authority"`
	AllowedProfiles     []ManagedProfileRef `json:"allowed_profiles"`
	DeclaredTags        []string            `json:"declared_tags"`
	Limits              ManagedLimits       `json:"limits"`
	AutoAction          bool                `json:"auto_action"`
	// WatchNatural admits natural-session activity signals (consent flag,
	// natural-session plan Slice B). Default false: a binding fires only on
	// console-owned task events unless the owner opted in.
	WatchNatural bool `json:"watch_natural,omitempty"`
	// Routes is the user-authored ordered fallback chain (provider-outage
	// plan, Slice C). Empty means exactly the single Runtime/Model/Mode above
	// — zero behavior change. Advancing along the chain is authorized because
	// the user authored it; entries may never carry a riskier mode than the
	// primary (red-team R1, validated at the binding surface).
	Routes     []ManagedRoute `json:"routes,omitempty"`
	StateToken string         `json:"state_token"`
	CreatedAt  int64          `json:"created_at"`
	UpdatedAt  int64          `json:"updated_at"`
}

// ManagedRoute is one fallback chain entry: where a parked run may relaunch.
type ManagedRoute struct {
	Runtime string `json:"runtime"`
	Model   string `json:"model,omitempty"`
	Mode    string `json:"mode,omitempty"`
}

type ManagedProfileRef struct {
	ProfileID    string `json:"profile_id"`
	SourceDigest string `json:"source_digest"`
	BundleDigest string `json:"bundle_digest"`
}

type ManagedGroup struct {
	GroupID              string `json:"group_id"`
	BindingID            string `json:"binding_id"`
	State                string `json:"state"`
	RootTaskID           string `json:"root_task_id"`
	RootRuntime          string `json:"root_runtime"`
	RootCatalogSessionID string `json:"root_catalog_session_id,omitempty"`
	RootNativeSessionID  string `json:"root_native_session_id,omitempty"`
	ProjectRoot          string `json:"project_root"`
	CreatedAt            int64  `json:"created_at"`
	UpdatedAt            int64  `json:"updated_at"`
	// Helper session (schema 30): the ONE vendor session this (binding,
	// source session) pair owns; every turn after the first resumes it. The
	// identity is only ever a REPORTED vendor id (task row or session.forked
	// event), never guessed. Replaced counts framework-side re-creation; the
	// helper never sees any of it.
	HelperRuntime         string `json:"helper_runtime,omitempty"`
	HelperNativeSessionID string `json:"helper_native_session_id,omitempty"`
	HelperTurns           int64  `json:"helper_turns"`
	HelperSessionReplaced int64  `json:"helper_session_replaced"`
	HelperTranscriptSeq   int64  `json:"helper_transcript_seq"`
	// Pending slot: the newest signal that arrived while a turn occupied the
	// helper session. Replaced, never queued; drained when the session frees.
	PendingProducer  string `json:"pending_producer,omitempty"`
	PendingEventID   int64  `json:"pending_event_id,omitempty"`
	PendingCoalesced int64  `json:"pending_coalesced"`
	PendingAt        int64  `json:"pending_at,omitempty"`
	// Dropped counts pending signals the drain could not launch (binding gone,
	// no longer selects, session became task-owned); the last reason is kept.
	PendingDropped       int64  `json:"pending_dropped"`
	PendingDroppedReason string `json:"pending_dropped_reason,omitempty"`
}

type ManagedRun struct {
	RunID               string         `json:"run_id"`
	IdempotencyKey      string         `json:"-"`
	GroupID             string         `json:"group_id"`
	BindingID           string         `json:"binding_id"`
	BindingStateToken   string         `json:"binding_state_token"`
	Role                string         `json:"role"`
	ProfileID           string         `json:"profile_id"`
	ProfileSourceDigest string         `json:"profile_source_digest"`
	ProfileBundleDigest string         `json:"profile_bundle_digest"`
	Kind                string         `json:"kind,omitempty"`
	SourceTaskID        string         `json:"source_task_id"`
	SourceEventID       int64          `json:"source_event_id"`
	ChildTaskID         string         `json:"child_task_id,omitempty"`
	State               string         `json:"state"`
	Action              string         `json:"action,omitempty"`
	Message             string         `json:"message,omitempty"`
	Citations           []string       `json:"citations"`
	Detail              map[string]any `json:"detail"`
	ErrorClass          string         `json:"error_class,omitempty"`
	Recovery            string         `json:"recovery,omitempty"`
	AdmittedAt          int64          `json:"admitted_at"`
	StartedAt           int64          `json:"started_at,omitempty"`
	CompletedAt         int64          `json:"completed_at,omitempty"`
}

type ManagedControl struct {
	ControlID       string `json:"control_id"`
	RunID           string `json:"run_id"`
	TaskID          string `json:"task_id"`
	RequestedAction string `json:"requested_action"`
	RequestState    string `json:"request_state"`
	Outcome         string `json:"outcome,omitempty"`
	ErrorText       string `json:"error,omitempty"`
	RequestedAt     int64  `json:"requested_at"`
	CompletedAt     int64  `json:"completed_at,omitempty"`
}

type ManagedGroupNote struct {
	NoteID      string `json:"note_id"`
	GroupID     string `json:"group_id"`
	Body        string `json:"body"`
	CreatedAt   int64  `json:"created_at"`
	RetractedAt int64  `json:"retracted_at,omitempty"`
}

type OrchestrationTag struct {
	TagID       string `json:"tag_id"`
	RunID       string `json:"run_id"`
	BindingID   string `json:"binding_id"`
	AgentKey    string `json:"agent_key"`
	Tag         string `json:"tag"`
	Provenance  string `json:"provenance"`
	Runtime     string `json:"runtime,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
	Anchor      string `json:"anchor,omitempty"`
	AppliedAt   int64  `json:"applied_at"`
	ExpiresAt   int64  `json:"expires_at,omitempty"`
	RetractedAt int64  `json:"retracted_at,omitempty"`
}

// ManagedGroupBudget is the resolved budget the caller (host policy) supplies
// to admission. The store applies it; it never invents one.
type ManagedGroupBudget struct {
	MaxTotal  int64
	MaxActive int64
	// CoalesceWhenOccupied is host policy for persistent helper sessions: when
	// a run occupies the group (admitted, running, or parked — a parked turn
	// still owns a relaunch on the same vendor session), admission writes no
	// run row and replaces the group's pending slot instead (one store
	// transaction — a check outside it races the pump, the natural emitter
	// and the relaunch sweep).
	CoalesceWhenOccupied bool
}

func ManagedBindingAbsentToken(id string) string {
	return managedDigest("crossing-guard-managed-binding-absent-v1\x00", []byte(id))
}

func ManagedBindingStateToken(binding ManagedBinding) string {
	binding.StateToken, binding.UpdatedAt = "", 0
	body, _ := json.Marshal(binding)
	return managedDigest("crossing-guard-managed-binding-state-v2\x00", body)
}

func managedDigest(frame string, body []byte) string {
	digest := sha256.Sum256(append([]byte(frame), body...))
	return "sha256-v1:" + hex.EncodeToString(digest[:])
}

const managedBindingColumns = `binding_id,state,role,priority,scope_runtime,scope_session,project_root,
	profile_id,profile_source_digest,profile_bundle_digest,runtime,model,mode,authority_json,
	allowed_profiles_json,declared_tags_json,limits_json,auto_action,watch_natural,routes_json,state_token,created_at,updated_at`

func scanManagedBinding(row interface{ Scan(...any) error }) (ManagedBinding, error) {
	var out ManagedBinding
	var authorityJSON, allowedJSON, tagsJSON, limitsJSON, routesJSON string
	err := row.Scan(&out.BindingID, &out.State, &out.Role, &out.Priority, &out.ScopeRuntime, &out.ScopeSession,
		&out.ProjectRoot, &out.ProfileID, &out.ProfileSourceDigest, &out.ProfileBundleDigest,
		&out.Runtime, &out.Model, &out.Mode, &authorityJSON, &allowedJSON, &tagsJSON, &limitsJSON,
		&out.AutoAction, &out.WatchNatural, &routesJSON, &out.StateToken, &out.CreatedAt, &out.UpdatedAt)
	if err == nil {
		err = json.Unmarshal([]byte(routesJSON), &out.Routes)
	}
	if err == nil {
		err = json.Unmarshal([]byte(authorityJSON), &out.Authority)
	}
	if err == nil {
		err = json.Unmarshal([]byte(allowedJSON), &out.AllowedProfiles)
	}
	if err == nil {
		err = json.Unmarshal([]byte(tagsJSON), &out.DeclaredTags)
	}
	if err == nil {
		err = json.Unmarshal([]byte(limitsJSON), &out.Limits)
	}
	if out.Authority == nil {
		out.Authority = []string{}
	}
	if out.AllowedProfiles == nil {
		out.AllowedProfiles = []ManagedProfileRef{}
	}
	if out.DeclaredTags == nil {
		out.DeclaredTags = []string{}
	}
	return out, err
}

func (ix *Index) ManagedBinding(id string) (ManagedBinding, bool, error) {
	out, err := scanManagedBinding(ix.db.QueryRow(`SELECT `+managedBindingColumns+` FROM orchestration_managed_binding WHERE binding_id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ManagedBinding{}, false, nil
	}
	return out, err == nil, err
}

func (ix *Index) PutManagedBinding(binding ManagedBinding, expected string, now int64) (ManagedBinding, error) {
	if binding.BindingID == "" || binding.State != "enabled" || expected == "" {
		return ManagedBinding{}, ErrManagedBindingConflict
	}
	authority, err := json.Marshal(binding.Authority)
	if err != nil {
		return ManagedBinding{}, err
	}
	allowed, err := json.Marshal(binding.AllowedProfiles)
	if err != nil {
		return ManagedBinding{}, err
	}
	if binding.DeclaredTags == nil {
		binding.DeclaredTags = []string{}
	}
	declaredTags, err := json.Marshal(binding.DeclaredTags)
	if err != nil {
		return ManagedBinding{}, err
	}
	limits, err := json.Marshal(binding.Limits)
	if err != nil {
		return ManagedBinding{}, err
	}
	if binding.Routes == nil {
		binding.Routes = []ManagedRoute{}
	}
	routes, err := json.Marshal(binding.Routes)
	if err != nil {
		return ManagedBinding{}, err
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return ManagedBinding{}, err
	}
	defer func() { _ = tx.Rollback() }()
	current, readErr := scanManagedBinding(tx.QueryRow(`SELECT `+managedBindingColumns+` FROM orchestration_managed_binding WHERE binding_id=?`, binding.BindingID))
	if errors.Is(readErr, sql.ErrNoRows) {
		if expected != ManagedBindingAbsentToken(binding.BindingID) {
			return ManagedBinding{}, ErrManagedBindingConflict
		}
		binding.CreatedAt = now
	} else if readErr != nil {
		return ManagedBinding{}, readErr
	} else if expected != current.StateToken {
		return ManagedBinding{}, ErrManagedBindingConflict
	} else {
		binding.CreatedAt = current.CreatedAt
	}
	binding.UpdatedAt = now
	binding.StateToken = ManagedBindingStateToken(binding)
	_, err = tx.Exec(`INSERT INTO orchestration_managed_binding(`+managedBindingColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(binding_id) DO UPDATE SET state=excluded.state,role=excluded.role,priority=excluded.priority,
		scope_runtime=excluded.scope_runtime,scope_session=excluded.scope_session,project_root=excluded.project_root,
		profile_id=excluded.profile_id,profile_source_digest=excluded.profile_source_digest,
		profile_bundle_digest=excluded.profile_bundle_digest,runtime=excluded.runtime,model=excluded.model,
		mode=excluded.mode,authority_json=excluded.authority_json,allowed_profiles_json=excluded.allowed_profiles_json,
		declared_tags_json=excluded.declared_tags_json,limits_json=excluded.limits_json,
		auto_action=excluded.auto_action,watch_natural=excluded.watch_natural,routes_json=excluded.routes_json,state_token=excluded.state_token,updated_at=excluded.updated_at`,
		binding.BindingID, binding.State, binding.Role, binding.Priority, binding.ScopeRuntime, binding.ScopeSession, binding.ProjectRoot,
		binding.ProfileID, binding.ProfileSourceDigest, binding.ProfileBundleDigest, binding.Runtime, binding.Model,
		binding.Mode, string(authority), string(allowed), string(declaredTags), string(limits),
		binding.AutoAction, binding.WatchNatural, string(routes), binding.StateToken, binding.CreatedAt, binding.UpdatedAt)
	if err != nil {
		return ManagedBinding{}, err
	}
	if err := tx.Commit(); err != nil {
		return ManagedBinding{}, err
	}
	return binding, nil
}

func (ix *Index) DisableManagedBinding(id, expected string, now int64) (ManagedBinding, error) {
	current, found, err := ix.ManagedBinding(id)
	if err != nil || !found {
		return ManagedBinding{}, ErrManagedBindingConflict
	}
	if current.StateToken != expected {
		return ManagedBinding{}, ErrManagedBindingConflict
	}
	previous := current.StateToken
	current.State = "disabled"
	current.UpdatedAt = now
	current.StateToken = ManagedBindingStateToken(current)
	result, err := ix.db.Exec(`UPDATE orchestration_managed_binding SET state='disabled',state_token=?,updated_at=? WHERE binding_id=? AND state_token=?`, current.StateToken, now, id, previous)
	if err != nil {
		return ManagedBinding{}, err
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return ManagedBinding{}, ErrManagedBindingConflict
	}
	return current, nil
}

func (ix *Index) ManagedBindings(enabledOnly bool) ([]ManagedBinding, error) {
	query := `SELECT ` + managedBindingColumns + ` FROM orchestration_managed_binding`
	if enabledOnly {
		query += ` WHERE state='enabled'`
	}
	query += ` ORDER BY created_at,binding_id`
	rows, err := ix.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ManagedBinding{}
	for rows.Next() {
		item, err := scanManagedBinding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// AdmitManagedRun admits one run idempotently under the caller-resolved budget.
// A run pre-marked `deferred` (priority arbitration) is recorded without
// consuming the group budget. The store never invents budget numbers.
func (ix *Index) AdmitManagedRun(group ManagedGroup, run ManagedRun, budget ManagedGroupBudget) (ManagedRun, bool, error) {
	return ix.admitManagedRun(group, run, budget, nil)
}

// AdmitManagedTurn admits one turn of a persistent helper session. When the
// budget's CoalesceWhenOccupied policy is set and a run occupies the group,
// the signal is coalesced: the returned run carries State "coalesced" with
// created=false, no run row exists, and the group's pending slot holds the
// supplied signal (schema 30). Otherwise identical to AdmitManagedRun.
func (ix *Index) AdmitManagedTurn(group ManagedGroup, run ManagedRun, budget ManagedGroupBudget, pending ManagedPendingSignal) (ManagedRun, bool, error) {
	return ix.admitManagedRun(group, run, budget, &pending)
}

func (ix *Index) admitManagedRun(group ManagedGroup, run ManagedRun, budget ManagedGroupBudget, pending *ManagedPendingSignal) (ManagedRun, bool, error) {
	if budget.MaxTotal < 1 || budget.MaxActive < 1 {
		return ManagedRun{}, false, errors.New("managed admission requires a resolved budget")
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return ManagedRun{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	existing, readErr := scanManagedRun(tx.QueryRow(`SELECT `+managedRunColumns+` FROM orchestration_managed_run WHERE idempotency_key=?`, run.IdempotencyKey))
	if readErr == nil {
		return existing, false, nil
	}
	if !errors.Is(readErr, sql.ErrNoRows) {
		return ManagedRun{}, false, readErr
	}
	// Consent is re-validated INSIDE the admission transaction (COMPLETE-RT-24):
	// a disable or edit racing an in-flight match must not admit against the
	// stale token — found live 2026-08-29 when a reply cycle admitted after
	// Disable. The refusal is recorded as a suppressed run, not silence.
	var bindingState, bindingToken string
	if err = tx.QueryRow(`SELECT state, state_token FROM orchestration_managed_binding WHERE binding_id=?`, run.BindingID).Scan(&bindingState, &bindingToken); err != nil {
		return ManagedRun{}, false, err
	}
	staleConsent := bindingState != "enabled" || bindingToken != run.BindingStateToken
	if staleConsent {
		group.State = "disabled"
	}
	if _, err = tx.Exec(`INSERT OR IGNORE INTO orchestration_group(group_id,binding_id,state,root_task_id,root_runtime,root_catalog_session_id,root_native_session_id,project_root,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		group.GroupID, group.BindingID, group.State, group.RootTaskID, group.RootRuntime, group.RootCatalogSessionID, group.RootNativeSessionID, group.ProjectRoot, group.CreatedAt, group.UpdatedAt); err != nil {
		return ManagedRun{}, false, err
	}
	if staleConsent {
		run.State = "suppressed"
		run.ErrorClass = "binding_changed"
		run.Recovery = "The agent binding was disabled or edited after this signal matched; re-enable or update the agent to act on future signals."
		run.CompletedAt = run.AdmittedAt
	}
	if run.State == "deferred" || run.State == "suppressed" {
		run.CompletedAt = run.AdmittedAt
	} else {
		if budget.CoalesceWhenOccupied && pending != nil {
			var occupying int64
			if err = tx.QueryRow(`SELECT COUNT(*) FROM orchestration_managed_run WHERE group_id=? AND state IN ('admitted','running','parked')`, run.GroupID).Scan(&occupying); err != nil {
				return ManagedRun{}, false, err
			}
			if occupying >= budget.MaxActive {
				payload, marshalErr := json.Marshal(pending)
				if marshalErr != nil {
					return ManagedRun{}, false, marshalErr
				}
				// The SAME signal re-coalescing (a drain that lost the race to a
				// live admission) keeps its count and wait clock; a new signal
				// replaces the slot and counts.
				if _, err = tx.Exec(`UPDATE orchestration_group SET pending_signal_json=?,
					pending_coalesced=CASE WHEN pending_producer=? AND pending_event_id=? THEN pending_coalesced ELSE pending_coalesced+1 END,
					pending_at=CASE WHEN pending_producer=? AND pending_event_id=? THEN pending_at ELSE ? END,
					pending_producer=?,pending_event_id=?,updated_at=? WHERE group_id=?`,
					string(payload), pending.Producer, pending.EventID, pending.Producer, pending.EventID, pending.At,
					pending.Producer, pending.EventID, run.AdmittedAt, run.GroupID); err != nil {
					return ManagedRun{}, false, err
				}
				if err = tx.Commit(); err != nil {
					return ManagedRun{}, false, err
				}
				run.State = "coalesced"
				return run, false, nil
			}
		}
		var total, active int64
		// Only runs that actually executed count toward MaxTotal — suppressed and
		// deferred refusals must not starve a group of its budget (red-team).
		if err = tx.QueryRow(`SELECT COALESCE(SUM(state IN ('admitted','running','completed','failed','unknown')),0),COALESCE(SUM(state IN ('admitted','running')),0) FROM orchestration_managed_run WHERE group_id=?`, run.GroupID).Scan(&total, &active); err != nil {
			return ManagedRun{}, false, err
		}
		if total >= budget.MaxTotal || active >= budget.MaxActive {
			run.State = "suppressed"
			run.ErrorClass = "group_budget"
			run.Recovery = "End or wait for existing managed work, or raise this agent's budget in its settings."
			run.CompletedAt = run.AdmittedAt
		} else {
			run.State = "admitted"
		}
	}
	citations, _ := json.Marshal(run.Citations)
	detail, _ := json.Marshal(run.Detail)
	if run.Detail == nil {
		detail = []byte(`{}`)
	}
	_, err = tx.Exec(`INSERT INTO orchestration_managed_run(`+managedRunColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, run.RunID, run.IdempotencyKey, run.GroupID, run.BindingID, run.BindingStateToken, run.Role, run.Kind, run.ProfileID, run.ProfileSourceDigest, run.ProfileBundleDigest, run.SourceTaskID, run.SourceEventID, run.ChildTaskID, run.State, run.Action, run.Message, string(citations), string(detail), run.ErrorClass, run.Recovery, run.AdmittedAt, run.StartedAt, run.CompletedAt)
	if err != nil {
		return ManagedRun{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return ManagedRun{}, false, err
	}
	return run, true, nil
}

const managedRunColumns = `run_id,idempotency_key,group_id,binding_id,binding_state_token,role,kind,profile_id,profile_source_digest,profile_bundle_digest,source_task_id,source_event_id,child_task_id,state,action,message,citations_json,detail_json,error_class,recovery,admitted_at,started_at,completed_at`

func scanManagedRun(row interface{ Scan(...any) error }) (ManagedRun, error) {
	var out ManagedRun
	var citations, detail string
	err := row.Scan(&out.RunID, &out.IdempotencyKey, &out.GroupID, &out.BindingID, &out.BindingStateToken, &out.Role, &out.Kind, &out.ProfileID, &out.ProfileSourceDigest, &out.ProfileBundleDigest, &out.SourceTaskID, &out.SourceEventID, &out.ChildTaskID, &out.State, &out.Action, &out.Message, &citations, &detail, &out.ErrorClass, &out.Recovery, &out.AdmittedAt, &out.StartedAt, &out.CompletedAt)
	if err == nil {
		err = json.Unmarshal([]byte(citations), &out.Citations)
	}
	if err == nil {
		err = json.Unmarshal([]byte(detail), &out.Detail)
	}
	return out, err
}

func (ix *Index) StartManagedRun(runID, childTaskID, relationshipID string, at int64) error {
	tx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var groupID, sourceTaskID, role string
	if err = tx.QueryRow(`SELECT group_id,source_task_id,role FROM orchestration_managed_run WHERE run_id=? AND state='admitted'`, runID).Scan(&groupID, &sourceTaskID, &role); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE orchestration_managed_run SET state='running',child_task_id=?,started_at=? WHERE run_id=? AND state='admitted'`, childTaskID, at, runID); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO orchestration_relationship(relationship_id,group_id,run_id,parent_task_id,child_task_id,role,depth,hops,state,created_at,updated_at) VALUES(?,?,?,?,?,?,1,0,'running',?,?)`, relationshipID, groupID, runID, sourceTaskID, childTaskID, role, at, at); err != nil {
		return err
	}
	return tx.Commit()
}

func (ix *Index) CompleteManagedRun(runID, state, action, message string, citations []string, detail map[string]any, errorClass, recovery string, at int64) error {
	citationsJSON, _ := json.Marshal(citations)
	detailJSON, _ := json.Marshal(detail)
	if detail == nil {
		detailJSON = []byte(`{}`)
	}
	// 'parked' settles too (provider-outage plan): the window-lapse and
	// relaunch-error paths complete runs that never got a live child back.
	result, err := ix.db.Exec(`UPDATE orchestration_managed_run SET state=?,action=?,message=?,citations_json=?,detail_json=?,error_class=?,recovery=?,completed_at=? WHERE run_id=? AND state IN ('admitted','running','parked')`, state, action, message, string(citationsJSON), string(detailJSON), errorClass, recovery, at, runID)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return errors.New("managed run is not active")
	}
	_, _ = ix.db.Exec(`UPDATE orchestration_relationship SET state=?,updated_at=? WHERE run_id=?`, state, at, runID)
	return nil
}

// ParkManagedRun moves an active run to the non-terminal `parked` state
// (provider-outage plan, Slice B): its PROVIDER died before the agent could
// work, so the run keeps its idempotency key and signal anchor for a cadence
// retry or a user-directed reroute. detail replaces the stored detail and
// carries the attempt ledger; errorClass is one of the provider_* classes and
// recovery carries the vendor's own recovery text. A parked follower stops
// counting as an active annotator (ActiveAnnotatorRuns), so held actors
// release exactly as they do on failure.
func (ix *Index) ParkManagedRun(runID string, detail map[string]any, errorClass, recovery string, at int64) error {
	detailJSON, _ := json.Marshal(detail)
	if detail == nil {
		detailJSON = []byte(`{}`)
	}
	result, err := ix.db.Exec(`UPDATE orchestration_managed_run SET state='parked',detail_json=?,error_class=?,recovery=? WHERE run_id=? AND state IN ('admitted','running')`,
		string(detailJSON), errorClass, recovery, runID)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return errors.New("managed run is not active")
	}
	_, _ = ix.db.Exec(`UPDATE orchestration_relationship SET state='parked',updated_at=? WHERE run_id=?`, at, runID)
	return nil
}

// ParkedManagedRuns lists parked runs oldest-first for the bounded cadence
// relaunch pass. Eligibility timing lives in each run's detail ledger; the
// store answers state only.
func (ix *Index) ParkedManagedRuns(limit int) ([]ManagedRun, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	rows, err := ix.db.Query(`SELECT `+managedRunColumns+` FROM orchestration_managed_run WHERE state='parked' ORDER BY admitted_at ASC,run_id ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ManagedRun{}
	for rows.Next() {
		item, err := scanManagedRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// RelaunchManagedRun moves a parked run back to running on a FRESH child task
// (red-team R2: the run's idempotency key is stable; every attempt gets its
// own task). The relationship row — UNIQUE per run — follows the run onto the
// new child; a run parked before any launch (breaker-parked admission) gains
// its relationship here. detail replaces the stored detail (attempt ledger).
func (ix *Index) RelaunchManagedRun(runID, childTaskID, relationshipID string, detail map[string]any, at int64) error {
	detailJSON, _ := json.Marshal(detail)
	if detail == nil {
		detailJSON = []byte(`{}`)
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var groupID, sourceTaskID, role string
	if err = tx.QueryRow(`SELECT group_id,source_task_id,role FROM orchestration_managed_run WHERE run_id=? AND state='parked'`, runID).Scan(&groupID, &sourceTaskID, &role); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE orchestration_managed_run SET state='running',child_task_id=?,detail_json=?,started_at=? WHERE run_id=? AND state='parked'`,
		childTaskID, string(detailJSON), at, runID); err != nil {
		return err
	}
	result, err := tx.Exec(`UPDATE orchestration_relationship SET child_task_id=?,state='running',updated_at=? WHERE run_id=?`, childTaskID, at, runID)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		if _, err = tx.Exec(`INSERT INTO orchestration_relationship(relationship_id,group_id,run_id,parent_task_id,child_task_id,role,depth,hops,state,created_at,updated_at) VALUES(?,?,?,?,?,?,1,0,'running',?,?)`,
			relationshipID, groupID, runID, sourceTaskID, childTaskID, role, at, at); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ActiveAnnotatorRuns reports whether any follower-type run for the exact
// source event is still working — the annotators-before-actors release check.
// A targeted query, not a recency window: the 200-newest scan could silently
// release an actor early on a busy store (red-team).
func (ix *Index) ActiveAnnotatorRuns(sourceTaskID string, sourceEventID int64) (bool, error) {
	var active int
	err := ix.db.QueryRow(`SELECT COUNT(*) FROM orchestration_managed_run
		WHERE source_task_id=? AND source_event_id=? AND role='follower' AND state IN ('admitted','running')`,
		sourceTaskID, sourceEventID).Scan(&active)
	if err != nil || active > 0 {
		return active > 0, err
	}
	// A follower whose turn coalesced has no run row yet (schema 30): its
	// group's pending slot names the event, and the actor keeps waiting for
	// the annotations that turn will produce.
	err = ix.db.QueryRow(`SELECT COUNT(*) FROM orchestration_group grp
		JOIN orchestration_managed_binding b ON b.binding_id = grp.binding_id
		WHERE grp.pending_event_id=? AND grp.pending_event_id<>0 AND b.role='follower'
		AND json_extract(grp.pending_signal_json,'$.task.id')=?`, sourceEventID, sourceTaskID).Scan(&active)
	return active > 0, err
}

// SetRelationshipReply records the reply half of the hand-back arc: the resumed
// task id, the durable cycle count, and — when the anchor capability supplies
// them — the opaque source/reply turn anchors. Anchors are equality-only and
// never parsed here.
func (ix *Index) SetRelationshipReply(runID, replyTaskID string, cycle int64, sourceAnchor, replyAnchor string, at int64) error {
	result, err := ix.db.Exec(`UPDATE orchestration_relationship SET reply_task_id=?,cycle=?,
		source_turn_anchor=CASE WHEN ?!='' THEN ? ELSE source_turn_anchor END,
		reply_turn_anchor=CASE WHEN ?!='' THEN ? ELSE reply_turn_anchor END,
		updated_at=? WHERE run_id=?`,
		replyTaskID, cycle, sourceAnchor, sourceAnchor, replyAnchor, replyAnchor, at, runID)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return errors.New("managed relationship not found for reply anchor")
	}
	return nil
}

// ManagedRelationships returns the durable arcs for a group, newest first.
func (ix *Index) ManagedRelationships(groupID string, limit int) ([]map[string]any, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	rows, err := ix.db.Query(`SELECT relationship_id,group_id,run_id,parent_task_id,child_task_id,reply_task_id,source_turn_anchor,reply_turn_anchor,cycle,role,depth,hops,state,created_at,updated_at FROM orchestration_relationship WHERE group_id=? ORDER BY created_at DESC,relationship_id DESC LIMIT ?`, groupID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var relationshipID, gID, runID, parent, child, reply, sourceAnchor, replyAnchor, role, state string
		var cycle, depth, hops, createdAt, updatedAt int64
		if err := rows.Scan(&relationshipID, &gID, &runID, &parent, &child, &reply, &sourceAnchor, &replyAnchor, &cycle, &role, &depth, &hops, &state, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"relationship_id": relationshipID, "group_id": gID, "run_id": runID,
			"parent_task_id": parent, "child_task_id": child, "reply_task_id": reply,
			"source_turn_anchor": sourceAnchor, "reply_turn_anchor": replyAnchor, "cycle": cycle,
			"role": role, "depth": depth, "hops": hops, "state": state,
			"created_at": createdAt, "updated_at": updatedAt})
	}
	return out, rows.Err()
}

func (ix *Index) ManagedRunByChildTask(taskID string) (ManagedRun, bool, error) {
	out, err := scanManagedRun(ix.db.QueryRow(`SELECT `+managedRunColumns+` FROM orchestration_managed_run WHERE child_task_id=?`, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return ManagedRun{}, false, nil
	}
	return out, err == nil, err
}

func (ix *Index) ManagedRun(id string) (ManagedRun, bool, error) {
	out, err := scanManagedRun(ix.db.QueryRow(`SELECT `+managedRunColumns+` FROM orchestration_managed_run WHERE run_id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ManagedRun{}, false, nil
	}
	return out, err == nil, err
}
func (ix *Index) ManagedRuns(limit int) ([]ManagedRun, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	rows, err := ix.db.Query(`SELECT `+managedRunColumns+` FROM orchestration_managed_run ORDER BY admitted_at DESC,run_id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ManagedRun{}
	for rows.Next() {
		item, err := scanManagedRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (ix *Index) RecoverManagedRunsUnknown(now int64) (int64, error) {
	result, err := ix.db.Exec(`UPDATE orchestration_managed_run SET state='unknown',completed_at=?,
		error_class='restart_unknown',recovery='The daemon restarted; no managed action was replayed.'
		WHERE state IN ('admitted','running')`, now)
	if err != nil {
		return 0, err
	}
	_, _ = ix.db.Exec(`UPDATE orchestration_relationship SET state='unknown',updated_at=? WHERE state='running'`, now)
	return result.RowsAffected()
}

// OrchestrationStreamPosition reads the durable consumer position for one
// source stream (previously named orchestration_cursor; renamed because this
// repository also ships a `cursor` runtime).
func (ix *Index) OrchestrationStreamPosition(kind string) (int64, error) {
	var position int64
	err := ix.db.QueryRow(`SELECT position FROM orchestration_stream_position WHERE source_kind=?`, kind).Scan(&position)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return position, err
}

// EnsureOrchestrationStreamPosition creates a stream's position row at the
// given head when none exists and reports whether it did. A durable consumer
// that meets an existing table for the first time starts at the head: the
// rows already there were written before anyone subscribed, and replaying
// them as fresh signals fires agents on history (installed defect, 2026-09-12).
func (ix *Index) EnsureOrchestrationStreamPosition(kind string, head, at int64) (bool, error) {
	result, err := ix.db.Exec(`INSERT INTO orchestration_stream_position(source_kind,position,updated_at) VALUES(?,?,?) ON CONFLICT(source_kind) DO NOTHING`, kind, head, at)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

func (ix *Index) PutOrchestrationStreamPosition(kind string, position, at int64) error {
	result, err := ix.db.Exec(`INSERT INTO orchestration_stream_position(source_kind,position,updated_at) VALUES(?,?,?) ON CONFLICT(source_kind) DO UPDATE SET position=excluded.position,updated_at=excluded.updated_at WHERE excluded.position>=orchestration_stream_position.position`, kind, position, at)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return fmt.Errorf("orchestration stream position regressed")
	}
	return nil
}

func (ix *Index) PutManagedControl(control ManagedControl) error {
	_, err := ix.db.Exec(`INSERT INTO orchestration_control(control_id,run_id,task_id,requested_action,request_state,outcome,error_text,requested_at,completed_at) VALUES(?,?,?,?,?,?,?,?,?)`, control.ControlID, control.RunID, control.TaskID, control.RequestedAction, control.RequestState, control.Outcome, control.ErrorText, control.RequestedAt, control.CompletedAt)
	return err
}

func (ix *Index) CompleteManagedControl(controlID, outcome, errorText string, at int64) error {
	result, err := ix.db.Exec(`UPDATE orchestration_control SET outcome=?,error_text=?,completed_at=? WHERE control_id=? AND outcome=''`, outcome, errorText, at, controlID)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return errors.New("managed control is not pending")
	}
	return nil
}

func (ix *Index) ManagedControls(limit int) ([]ManagedControl, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	rows, err := ix.db.Query(`SELECT control_id,run_id,task_id,requested_action,request_state,outcome,error_text,requested_at,completed_at FROM orchestration_control ORDER BY requested_at DESC,control_id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ManagedControl{}
	for rows.Next() {
		var item ManagedControl
		if err := rows.Scan(&item.ControlID, &item.RunID, &item.TaskID, &item.RequestedAction, &item.RequestState, &item.Outcome, &item.ErrorText, &item.RequestedAt, &item.CompletedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// ManagedGroup returns one durable group row; tag identity and reply anchors
// read session/runtime truth from here rather than from the volatile task row.
func (ix *Index) ManagedGroup(id string) (ManagedGroup, bool, error) {
	out, err := scanManagedGroup(ix.db.QueryRow(`SELECT `+managedGroupColumns+` FROM orchestration_group WHERE group_id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ManagedGroup{}, false, nil
	}
	return out, err == nil, err
}

// MergeManagedRunDetail patches keys into one run's detail_json after the run
// is terminal. The off-pump claim-ref resolver records ref resolution outcomes
// here (plan §5: resolution rides detail_json, no DDL); it never changes run
// state, action, or message.
func (ix *Index) MergeManagedRunDetail(runID string, patch map[string]any) error {
	if len(patch) == 0 {
		return nil
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var detailJSON string
	if err := tx.QueryRow(`SELECT detail_json FROM orchestration_managed_run WHERE run_id=?`, runID).Scan(&detailJSON); err != nil {
		return err
	}
	detail := map[string]any{}
	if err := json.Unmarshal([]byte(detailJSON), &detail); err != nil {
		return err
	}
	for key, value := range patch {
		detail[key] = value
	}
	merged, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE orchestration_managed_run SET detail_json=? WHERE run_id=?`, string(merged), runID); err != nil {
		return err
	}
	return tx.Commit()
}

func (ix *Index) ManagedGroups(limit int) ([]ManagedGroup, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	rows, err := ix.db.Query(`SELECT `+managedGroupColumns+` FROM orchestration_group ORDER BY updated_at DESC,group_id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ManagedGroup{}
	for rows.Next() {
		item, err := scanManagedGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (ix *Index) PutManagedGroupNote(note ManagedGroupNote) error {
	_, err := ix.db.Exec(`INSERT INTO orchestration_group_note(note_id,group_id,body,created_at,retracted_at) VALUES(?,?,?,?,0)`,
		note.NoteID, note.GroupID, note.Body, note.CreatedAt)
	return err
}

func (ix *Index) RetractManagedGroupNote(noteID string, at int64) error {
	result, err := ix.db.Exec(`UPDATE orchestration_group_note SET retracted_at=? WHERE note_id=? AND retracted_at=0`, at, noteID)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return errors.New("group note is not active")
	}
	return nil
}

func (ix *Index) ManagedGroupNotes(groupID string, includeRetracted bool) ([]ManagedGroupNote, error) {
	query := `SELECT note_id,group_id,body,created_at,retracted_at FROM orchestration_group_note WHERE group_id=?`
	if !includeRetracted {
		query += ` AND retracted_at=0`
	}
	query += ` ORDER BY created_at,note_id`
	rows, err := ix.db.Query(query, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ManagedGroupNote{}
	for rows.Next() {
		var item ManagedGroupNote
		if err := rows.Scan(&item.NoteID, &item.GroupID, &item.Body, &item.CreatedAt, &item.RetractedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// PutOrchestrationTags records the model-claimed tags of one run. Tag names
// were already validated against the profile's declared vocabulary by the claim
// owner; the agent key embeds the binding identity so fired rules can show
// whose claim acted.
func (ix *Index) PutOrchestrationTags(tags []OrchestrationTag) error {
	if len(tags) == 0 {
		return nil
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, tag := range tags {
		if _, err := tx.Exec(`INSERT INTO orchestration_tag(tag_id,run_id,binding_id,agent_key,tag,provenance,runtime,session_id,anchor,applied_at,expires_at,retracted_at) VALUES(?,?,?,?,?,'model-claimed',?,?,?,?,?,0)`,
			tag.TagID, tag.RunID, tag.BindingID, tag.AgentKey, tag.Tag, tag.Runtime, tag.SessionID, tag.Anchor, tag.AppliedAt, tag.ExpiresAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AllActiveOrchestrationTags is ActiveOrchestrationTags for every session at
// once, newest first, for callers that decorate a whole session list: one read
// instead of one per session. The bool reports that limit cut the read short.
func (ix *Index) AllActiveOrchestrationTags(now int64, limit int) ([]OrchestrationTag, bool, error) {
	rows, err := ix.db.Query(`SELECT tag_id,run_id,binding_id,agent_key,tag,provenance,runtime,session_id,anchor,applied_at,expires_at,retracted_at
		FROM orchestration_tag WHERE retracted_at=0 AND (expires_at=0 OR expires_at>?) ORDER BY applied_at DESC,tag_id DESC LIMIT ?`, now, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []OrchestrationTag{}
	for rows.Next() {
		var item OrchestrationTag
		if err := rows.Scan(&item.TagID, &item.RunID, &item.BindingID, &item.AgentKey, &item.Tag, &item.Provenance, &item.Runtime, &item.SessionID, &item.Anchor, &item.AppliedAt, &item.ExpiresAt, &item.RetractedAt); err != nil {
			return nil, false, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

// ActiveOrchestrationTags returns the unexpired, unretracted model-claimed tags
// for one session at the supplied instant.
func (ix *Index) ActiveOrchestrationTags(sessionID string, now int64) ([]OrchestrationTag, error) {
	rows, err := ix.db.Query(`SELECT tag_id,run_id,binding_id,agent_key,tag,provenance,runtime,session_id,anchor,applied_at,expires_at,retracted_at
		FROM orchestration_tag WHERE session_id=? AND retracted_at=0 AND (expires_at=0 OR expires_at>?) ORDER BY applied_at DESC,tag_id DESC LIMIT 200`, sessionID, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OrchestrationTag{}
	for rows.Next() {
		var item OrchestrationTag
		if err := rows.Scan(&item.TagID, &item.RunID, &item.BindingID, &item.AgentKey, &item.Tag, &item.Provenance, &item.Runtime, &item.SessionID, &item.Anchor, &item.AppliedAt, &item.ExpiresAt, &item.RetractedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (ix *Index) RetractOrchestrationTag(tagID string, at int64) error {
	result, err := ix.db.Exec(`UPDATE orchestration_tag SET retracted_at=? WHERE tag_id=? AND retracted_at=0`, at, tagID)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return errors.New("orchestration tag is not active")
	}
	return nil
}
