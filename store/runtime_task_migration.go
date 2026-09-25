package store

import "fmt"

// migrateRuntimeTasksV20 adds the bounded UI lifecycle projection for one vendor turn.
// It deliberately does not reference the governance event ledger: task replay is
// transient operator state, not policy truth. Runtime/native session identity is kept
// as a correlation key because the canonical session may not exist until harvesting
// observes the newly started turn. CatalogSessionID references an existing session row
// when the turn was resumed from history; it is not a second identity registry.
func migrateRuntimeTasksV20(db schemaDB, version int) error {
	if version >= 20 {
		return nil
	}
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS runtime_task(
  id TEXT PRIMARY KEY,
  console_scope TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  request_digest TEXT NOT NULL,
  runtime TEXT NOT NULL,
  catalog_session_id TEXT NOT NULL DEFAULT '',
  native_session_id TEXT NOT NULL DEFAULT '',
  working_directory TEXT NOT NULL,
  workspace_selection_id TEXT NOT NULL DEFAULT '',
  workspace_selection_version INTEGER NOT NULL DEFAULT 0 CHECK(workspace_selection_version >= 0),
  lifecycle TEXT NOT NULL CHECK(lifecycle IN ('queued','starting','running','completed','interrupted','failed','unknown')),
  ownership TEXT NOT NULL CHECK(ownership IN ('crossing-guard','native')),
  observation_mode TEXT NOT NULL CHECK(observation_mode IN ('stream','incremental','snapshot')),
  freshness TEXT NOT NULL CHECK(freshness IN ('live','updating','stale','unknown')),
  controllable INTEGER NOT NULL CHECK(controllable IN (0,1)),
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  last_sequence INTEGER NOT NULL DEFAULT 0 CHECK(last_sequence >= 0),
  last_event_id INTEGER NOT NULL DEFAULT 0 CHECK(last_event_id >= 0),
  error_text TEXT NOT NULL DEFAULT '' CHECK(length(error_text) <= 2000),
  retention_deadline INTEGER NOT NULL,
  UNIQUE(console_scope,idempotency_key)
);
CREATE INDEX IF NOT EXISTS runtime_task_session
  ON runtime_task(runtime,native_session_id,updated_at DESC,id);
CREATE INDEX IF NOT EXISTS runtime_task_catalog_session
  ON runtime_task(runtime,catalog_session_id,updated_at DESC,id);
CREATE INDEX IF NOT EXISTS runtime_task_lifecycle
  ON runtime_task(lifecycle,updated_at,id);

CREATE TABLE IF NOT EXISTS runtime_task_event(
  event_id INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id TEXT NOT NULL REFERENCES runtime_task(id) ON DELETE CASCADE,
  sequence INTEGER NOT NULL CHECK(sequence > 0),
  schema_version INTEGER NOT NULL CHECK(schema_version = 1),
  occurred_at INTEGER NOT NULL,
  observed_at INTEGER NOT NULL,
  kind TEXT NOT NULL,
  source_kind TEXT NOT NULL,
  evidence_class TEXT NOT NULL CHECK(evidence_class IN ('observed','derived','reported','unknown')),
  freshness TEXT NOT NULL CHECK(freshness IN ('live','updating','stale','unknown')),
  payload BLOB NOT NULL CHECK(length(payload) <= 65536),
  UNIQUE(task_id,sequence)
);
CREATE INDEX IF NOT EXISTS runtime_task_event_task
  ON runtime_task_event(task_id,sequence);
CREATE INDEX IF NOT EXISTS runtime_task_event_observed
  ON runtime_task_event(observed_at,event_id);
`); err != nil {
		return fmt.Errorf("migrate runtime tasks v20: %w", err)
	}
	return nil
}
