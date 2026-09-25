package store

import "fmt"

// workspaceSchemaV26 stores operator workspace bindings and the lifecycle facts
// consumed by worktrees, live review, mutations, tasks, and terminals. It is
// deliberately outside the governance event fold: these rows are runtime authority
// and recovery state, not evidence that an agent action occurred.
const workspaceSchemaV26 = `
CREATE TABLE IF NOT EXISTS workspace_worktree(
  id TEXT PRIMARY KEY,
  repository_id TEXT NOT NULL,
  checkout_id TEXT NOT NULL DEFAULT '',
  root TEXT NOT NULL UNIQUE,
  source_checkout_id TEXT NOT NULL,
  source_root TEXT NOT NULL,
  starting_state TEXT NOT NULL CHECK(starting_state IN ('branch','working-tree')),
  start_ref TEXT NOT NULL DEFAULT '',
  branch TEXT NOT NULL DEFAULT '',
  head TEXT NOT NULL DEFAULT '',
  lifecycle TEXT NOT NULL CHECK(lifecycle IN
    ('creating','ready_unbound','bound','cleanup_pending','removed','recovery_required')),
  version INTEGER NOT NULL CHECK(version > 0),
  idempotency_key TEXT NOT NULL,
  request_digest TEXT NOT NULL,
  source_snapshot_digest TEXT NOT NULL,
  last_verified_snapshot TEXT NOT NULL DEFAULT '',
  recovery_reason TEXT NOT NULL DEFAULT '' CHECK(length(recovery_reason) <= 2000),
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE(repository_id,idempotency_key)
);
CREATE INDEX IF NOT EXISTS workspace_worktree_lifecycle
  ON workspace_worktree(lifecycle,updated_at,id);
CREATE INDEX IF NOT EXISTS workspace_worktree_checkout
  ON workspace_worktree(repository_id,checkout_id,id);

-- Selection rows are immutable revisions. workspace_binding is the one current
-- pointer per subject, which makes a compare-and-set rebind atomic without erasing
-- old idempotency results.
CREATE TABLE IF NOT EXISTS workspace_selection(
  id TEXT PRIMARY KEY,
  subject_kind TEXT NOT NULL CHECK(subject_kind IN ('task','session')),
  subject_id TEXT NOT NULL,
  binding_version INTEGER NOT NULL CHECK(binding_version > 0),
  idempotency_key TEXT NOT NULL,
  request_digest TEXT NOT NULL,
  repository_id TEXT NOT NULL,
  checkout_id TEXT NOT NULL,
  root TEXT NOT NULL,
  kind TEXT NOT NULL CHECK(kind IN ('local','worktree')),
  worktree_id TEXT REFERENCES workspace_worktree(id) ON DELETE RESTRICT,
  source_checkout_id TEXT NOT NULL DEFAULT '',
  starting_state TEXT NOT NULL DEFAULT '' CHECK(starting_state IN ('','branch','working-tree')),
  branch TEXT NOT NULL DEFAULT '',
  head TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK(status IN ('active','superseded','released','stale')),
  snapshot_digest TEXT NOT NULL,
  observed_at INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE(subject_kind,subject_id,binding_version),
  UNIQUE(subject_kind,subject_id,idempotency_key),
  CHECK((kind='local' AND worktree_id IS NULL) OR (kind='worktree' AND worktree_id IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS workspace_selection_checkout
  ON workspace_selection(repository_id,checkout_id,updated_at DESC,id);
CREATE INDEX IF NOT EXISTS workspace_selection_status
  ON workspace_selection(status,updated_at,id);

CREATE TABLE IF NOT EXISTS workspace_binding(
  subject_kind TEXT NOT NULL CHECK(subject_kind IN ('task','session')),
  subject_id TEXT NOT NULL,
  version INTEGER NOT NULL CHECK(version > 0),
  selection_id TEXT NOT NULL UNIQUE REFERENCES workspace_selection(id) ON DELETE RESTRICT,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY(subject_kind,subject_id)
);

CREATE TABLE IF NOT EXISTS workspace_selection_lease(
  selection_id TEXT NOT NULL REFERENCES workspace_selection(id) ON DELETE RESTRICT,
  selection_version INTEGER NOT NULL CHECK(selection_version > 0),
  owner_kind TEXT NOT NULL CHECK(owner_kind IN ('task','terminal','mutation')),
  owner_id TEXT NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('active','released')),
  acquired_at INTEGER NOT NULL,
  released_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(selection_id,owner_kind,owner_id),
  CHECK((status='active' AND released_at=0) OR (status='released' AND released_at>=acquired_at))
);
CREATE UNIQUE INDEX IF NOT EXISTS workspace_selection_lease_owner_active
  ON workspace_selection_lease(owner_kind,owner_id) WHERE status='active';
CREATE INDEX IF NOT EXISTS workspace_selection_lease_active
  ON workspace_selection_lease(selection_id,status,owner_kind,owner_id);

CREATE TABLE IF NOT EXISTS workspace_mutation(
  id TEXT PRIMARY KEY,
  selection_id TEXT NOT NULL REFERENCES workspace_selection(id) ON DELETE RESTRICT,
  selection_version INTEGER NOT NULL CHECK(selection_version > 0),
  idempotency_key TEXT NOT NULL,
  request_digest TEXT NOT NULL,
  action TEXT NOT NULL CHECK(action IN
    ('stage_files','unstage_files','stage_hunks','unstage_hunks','apply_patch',
     'discard_worktree_changes','delete_untracked_files')),
  state TEXT NOT NULL CHECK(state IN
    ('previewed','pending_approval','executing','succeeded','failed','denied','cancelled',
     'recovery_required','expired_repreview_required')),
  source_snapshot_digest TEXT NOT NULL,
  preview_digest TEXT NOT NULL,
  approval_id TEXT NOT NULL DEFAULT '',
  pre_checkpoint_id INTEGER NOT NULL DEFAULT 0,
  post_checkpoint_id INTEGER NOT NULL DEFAULT 0,
  recovery_ref TEXT NOT NULL DEFAULT '',
  error_text TEXT NOT NULL DEFAULT '' CHECK(length(error_text) <= 2000),
  preview_expires_at INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE(selection_id,idempotency_key)
);
CREATE UNIQUE INDEX IF NOT EXISTS workspace_mutation_one_active
  ON workspace_mutation(selection_id)
  WHERE state IN ('pending_approval','executing','recovery_required');
CREATE INDEX IF NOT EXISTS workspace_mutation_state
  ON workspace_mutation(state,updated_at,id);
`

func migrateWorkspacesV26(db schemaDB) error {
	columns, err := columnSet(db, "runtime_task")
	if err != nil {
		return fmt.Errorf("inspect runtime task workspace columns v26: %w", err)
	}
	if len(columns) > 0 && !columns["workspace_selection_id"] {
		if _, err := db.Exec(`ALTER TABLE runtime_task ADD COLUMN workspace_selection_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("migrate runtime task workspace selection v26: %w", err)
		}
	}
	if len(columns) > 0 && !columns["workspace_selection_version"] {
		if _, err := db.Exec(`ALTER TABLE runtime_task ADD COLUMN workspace_selection_version INTEGER NOT NULL DEFAULT 0 CHECK(workspace_selection_version >= 0)`); err != nil {
			return fmt.Errorf("migrate runtime task workspace version v26: %w", err)
		}
	}
	if _, err := db.Exec(workspaceSchemaV26); err != nil {
		return fmt.Errorf("migrate workspaces v26: %w", err)
	}
	return nil
}
