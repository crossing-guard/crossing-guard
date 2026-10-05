// Package store owns the episodic index (ADR 0013 D1): SQLite sessions +
// events FTS5, embedded via the CGo-free ncruces driver (ADR 0008/0014
// decision — validated in experiments/sqlite-driver-probe; FTS5 is not in
// the default build and must be registered per connection).
//
// Two classes of data live here (governance-model.md rev. 3):
//   - legacy source markers, `search_document`/`events_fts`, and the presentation
//     fields on `sessions` — a
//     REBUILDABLE PROJECTION over runtime session files. The session identity root and
//     its activity children are primary collection facts and survive refresh.
//   - `event`, `entity`, `entity_state`, `session_state` — the governance store
//     for the live path. The `event` log is PRIMARY TRUTH: append-only, written as
//     actions happen, NOT reconstructable from vendor files (a blocked action never
//     appears in a transcript). Current state is a fold over the event log's frozen
//     tags. Deleting the event log loses real data.
//
// Placement note: public crossing-guard/store (not internal/) until the
// experiment modules merge into the root module at M4 — same visibility
// reasoning as crossing-guard/harvest.
package store

import (
	"crossing-guard/engine"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ncruces/go-sqlite3/driver"
	"github.com/ncruces/go-sqlite3/ext/fts5"
)

const schema = `
CREATE TABLE IF NOT EXISTS files(path TEXT PRIMARY KEY, mtime INTEGER, size INTEGER, session_id TEXT, vendor TEXT);
CREATE TABLE IF NOT EXISTS session_source_watermark(
  runtime TEXT NOT NULL,
  source_ref TEXT NOT NULL,
  source_segment_id TEXT NOT NULL,
  update_marker TEXT NOT NULL,
  session_id TEXT NOT NULL,
  PRIMARY KEY(runtime,source_ref,source_segment_id)
);
CREATE TABLE IF NOT EXISTS sessions(vendor TEXT, id TEXT, path TEXT, cwd TEXT, project TEXT, title TEXT, modified INTEGER, turns INTEGER, catalog_id TEXT NOT NULL DEFAULT '', resume_id TEXT NOT NULL DEFAULT '', PRIMARY KEY(vendor,id));
CREATE TABLE IF NOT EXISTS session_activity_observation(
  observation_id TEXT PRIMARY KEY,
  runtime TEXT NOT NULL,
  session_id TEXT NOT NULL,
  state TEXT NOT NULL CHECK(state IN ('open','stopped')),
  observed_at INTEGER NOT NULL,
  valid_until INTEGER NOT NULL,
  evidence_class TEXT NOT NULL CHECK(evidence_class IN ('explicit-start','positive-open','explicit-end')),
  entry_kind TEXT NOT NULL CHECK(entry_kind IN ('start','resume','context-reset','context-compact','first-action','end','unknown')),
  native_source TEXT NOT NULL DEFAULT '',
  source_ref TEXT NOT NULL DEFAULT '',
  evidence_digest TEXT NOT NULL,
  collector_id TEXT NOT NULL,
  collector_version TEXT NOT NULL DEFAULT '',
  received_at INTEGER NOT NULL,
  delivery_attempts INTEGER NOT NULL CHECK(delivery_attempts >= 1),
  delivery_mode TEXT NOT NULL CHECK(delivery_mode IN ('direct','replay','transcript')),
  FOREIGN KEY(runtime,session_id) REFERENCES sessions(vendor,id) ON DELETE RESTRICT,
  CHECK((state='open' AND valid_until>observed_at AND evidence_class!='explicit-end') OR
        (state='stopped' AND valid_until=0 AND evidence_class='explicit-end' AND entry_kind='end'))
);
CREATE INDEX IF NOT EXISTS session_activity_current
  ON session_activity_observation(runtime,session_id,observed_at DESC,observation_id DESC);

-- session_turn_observation: turn-boundary evidence from installed hooks — the
-- agent began a turn, handed back, or is blocked asking the human. Its own
-- table, not rows in session_activity_observation: the natural-session signal
-- emitter reads that table by rowid with no kind filter, and one busy session's
-- turn rows would starve agent routing. received_at_ms is the DAEMON clock and
-- is the only comparator the status decider uses; observed_at is the hook's
-- own clock, kept for inspection.
CREATE TABLE IF NOT EXISTS session_turn_observation(
  observation_id TEXT PRIMARY KEY,
  runtime TEXT NOT NULL,
  session_id TEXT NOT NULL,
  catalog_session_id TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL CHECK(kind IN ('turn.started','turn.ended','input.requested',
    'subagent.started','subagent.ended','context.compacted')),
  observed_at INTEGER NOT NULL,
  received_at_ms INTEGER NOT NULL,
  native_source TEXT NOT NULL DEFAULT '',
  source_ref TEXT NOT NULL DEFAULT '',
  evidence_digest TEXT NOT NULL,
  collector_id TEXT NOT NULL,
  collector_version TEXT NOT NULL DEFAULT '',
  delivery_attempts INTEGER NOT NULL CHECK(delivery_attempts >= 1),
  delivery_mode TEXT NOT NULL CHECK(delivery_mode IN ('direct','replay')),
  FOREIGN KEY(runtime,session_id) REFERENCES sessions(vendor,id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS session_turn_current
  ON session_turn_observation(runtime,session_id,received_at_ms DESC);
` + searchIndexSchemaV22 + `

-- Governance store (governance-model.md rev. 3). PRIMARY, not a projection.
-- event: the append-only truth of the live path. tags are FROZEN at observe time
-- (JSON), so state is a pure re-fold and replay is deterministic across detector edits.
CREATE TABLE IF NOT EXISTS event(
  id INTEGER PRIMARY KEY,
  ts INTEGER NOT NULL,
  session_id TEXT NOT NULL,
  runtime TEXT NOT NULL DEFAULT '',
  verb TEXT,
  tool TEXT,
  target_entity_id TEXT,
  tags TEXT,
  decision TEXT,
  reason TEXT,
  origin TEXT
);
CREATE INDEX IF NOT EXISTS event_session ON event(session_id);
CREATE INDEX IF NOT EXISTS event_target ON event(target_entity_id);

CREATE TABLE IF NOT EXISTS event_delivery(
  event_id INTEGER PRIMARY KEY REFERENCES event(id) ON DELETE RESTRICT,
  observation_id TEXT NOT NULL UNIQUE,
  observation_schema TEXT NOT NULL,
  action_id TEXT NOT NULL DEFAULT '',
  envelope_digest TEXT NOT NULL,
  collector_id TEXT NOT NULL,
  collector_version TEXT NOT NULL DEFAULT '',
  native_call_id TEXT NOT NULL DEFAULT '',
  native_call_kind TEXT NOT NULL DEFAULT '',
  queued_at INTEGER NOT NULL DEFAULT 0,
  received_at INTEGER NOT NULL,
  delivery_attempts INTEGER NOT NULL CHECK(delivery_attempts >= 1),
  delivery_mode TEXT NOT NULL CHECK(delivery_mode IN ('direct','replay','transcript'))
);
CREATE INDEX IF NOT EXISTS event_delivery_native
  ON event_delivery(native_call_kind,native_call_id,event_id);

CREATE TABLE IF NOT EXISTS event_input(
  event_id INTEGER PRIMARY KEY REFERENCES event(id) ON DELETE RESTRICT,
  media_type TEXT NOT NULL,
  raw_bytes INTEGER NOT NULL CHECK(raw_bytes >= 0),
  captured_bytes INTEGER NOT NULL CHECK(captured_bytes >= 0),
  digest TEXT NOT NULL DEFAULT '',
  completeness TEXT NOT NULL CHECK(completeness IN ('complete','metadata-only','unavailable')),
  payload BLOB,
  source_ref TEXT NOT NULL DEFAULT '',
  CHECK((payload IS NULL AND captured_bytes=0) OR (payload IS NOT NULL AND captured_bytes=length(payload))),
  CHECK((completeness='complete' AND payload IS NOT NULL AND captured_bytes=raw_bytes AND digest!='') OR
        (completeness='metadata-only' AND payload IS NULL AND digest!='') OR
        (completeness='unavailable' AND payload IS NULL))
);
CREATE INDEX IF NOT EXISTS event_input_source ON event_input(source_ref,event_id);

-- Optional upper-layer report reviews. These records are attributed claims and
-- operational configuration; they do not participate in the governance fold.
CREATE TABLE IF NOT EXISTS orchestration_review_binding(
  binding_id TEXT PRIMARY KEY,
  state TEXT NOT NULL CHECK(state IN ('enabled','disabled')),
  effect TEXT NOT NULL DEFAULT 'report-only' CHECK(effect IN ('report-only','delegated-first')),
  runtime_filter TEXT NOT NULL DEFAULT '',
  profile_id TEXT NOT NULL,
  profile_source_digest TEXT NOT NULL,
  profile_bundle_digest TEXT NOT NULL,
  instruction_digest TEXT NOT NULL,
  request_path_kind TEXT NOT NULL,
  request_path_digest TEXT NOT NULL,
  endpoint TEXT NOT NULL,
  model TEXT NOT NULL,
  timeout_ms INTEGER NOT NULL CHECK(timeout_ms BETWEEN 1 AND 120000),
  approval_subdeadline_ms INTEGER NOT NULL DEFAULT 0 CHECK(approval_subdeadline_ms BETWEEN 0 AND 120000),
  answer_choice_prompts INTEGER NOT NULL DEFAULT 1 CHECK(answer_choice_prompts IN (0,1)),
  max_input_bytes INTEGER NOT NULL CHECK(max_input_bytes > 0),
  max_output_bytes INTEGER NOT NULL CHECK(max_output_bytes > 0),
  max_tokens INTEGER NOT NULL CHECK(max_tokens > 0),
  max_concurrency INTEGER NOT NULL CHECK(max_concurrency BETWEEN 1 AND 64),
  state_token TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS orchestration_review_invocation(
  invocation_id TEXT PRIMARY KEY,
  action_id TEXT NOT NULL UNIQUE,
  action_digest TEXT NOT NULL,
  observation_id TEXT NOT NULL,
  event_id INTEGER NOT NULL,
  runtime TEXT NOT NULL DEFAULT '',
  session_id TEXT NOT NULL,
  native_call_id TEXT NOT NULL DEFAULT '',
  native_call_kind TEXT NOT NULL DEFAULT '',
  tool TEXT NOT NULL,
  binding_id TEXT NOT NULL,
  binding_state_token TEXT NOT NULL,
  profile_id TEXT NOT NULL,
  profile_source_digest TEXT NOT NULL,
  profile_bundle_digest TEXT NOT NULL,
  instruction_digest TEXT NOT NULL,
  request_path_kind TEXT NOT NULL,
  request_path_digest TEXT NOT NULL,
  endpoint TEXT NOT NULL,
  model TEXT NOT NULL,
  timeout_ms INTEGER NOT NULL CHECK(timeout_ms > 0),
  max_input_bytes INTEGER NOT NULL CHECK(max_input_bytes > 0),
  max_output_bytes INTEGER NOT NULL CHECK(max_output_bytes > 0),
  max_tokens INTEGER NOT NULL CHECK(max_tokens > 0),
  max_concurrency INTEGER NOT NULL CHECK(max_concurrency > 0),
  state TEXT NOT NULL CHECK(state IN ('admitted','running','completed','unavailable','timed_out','malformed','unknown','suppressed')),
  admitted_at INTEGER NOT NULL,
  started_at INTEGER NOT NULL DEFAULT 0,
  completed_at INTEGER NOT NULL DEFAULT 0,
  duration_ms INTEGER NOT NULL DEFAULT 0 CHECK(duration_ms >= 0),
  decision TEXT NOT NULL DEFAULT '' CHECK(decision IN ('','allow','deny','abstain')),
  message TEXT NOT NULL DEFAULT '' CHECK(length(message) <= 262144),
  citations_json TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(citations_json)),
  request_bytes INTEGER NOT NULL DEFAULT 0 CHECK(request_bytes >= 0),
  response_bytes INTEGER NOT NULL DEFAULT 0 CHECK(response_bytes >= 0),
  prompt_tokens INTEGER NOT NULL DEFAULT 0 CHECK(prompt_tokens >= 0),
  completion_tokens INTEGER NOT NULL DEFAULT 0 CHECK(completion_tokens >= 0),
  error_class TEXT NOT NULL DEFAULT '',
  recovery TEXT NOT NULL DEFAULT '' CHECK(length(recovery) <= 2000),
  timing_class TEXT NOT NULL DEFAULT 'result_not_observed_at_completion' CHECK(timing_class IN
    ('result_not_observed_at_completion','completed_before_exact_result_observation',
     'completed_after_exact_result_observation','exact_result_timing_unknown')),
  approval_id TEXT NOT NULL DEFAULT '',
  approval_response_id TEXT NOT NULL DEFAULT '',
  approval_outcome TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS orchestration_review_history
  ON orchestration_review_invocation(admitted_at DESC,invocation_id DESC);
CREATE INDEX IF NOT EXISTS orchestration_review_session
  ON orchestration_review_invocation(runtime,session_id,admitted_at DESC,invocation_id DESC);
CREATE INDEX IF NOT EXISTS orchestration_review_active
  ON orchestration_review_invocation(binding_id,profile_id,profile_bundle_digest,state);

` + orchestrationManagedSchemaV25 + sessionDeliverySchemaV29 + sessionOwnerTagSchema + usageCallSchema + `

` + workspaceSchemaV26 + `

-- Runtime/transcript completion evidence. One source observation is retained per row;
-- logical completion and event selection are append-only reconciliation facts below.
CREATE TABLE IF NOT EXISTS result_observation(
  id INTEGER PRIMARY KEY,
  observation_id TEXT NOT NULL UNIQUE,
  session_id TEXT NOT NULL,
  supplied_session_id TEXT NOT NULL DEFAULT '',
  runtime TEXT NOT NULL,
  tool TEXT NOT NULL DEFAULT '',
  native_call_id TEXT NOT NULL DEFAULT '',
  native_call_kind TEXT NOT NULL DEFAULT '',
  source_kind TEXT NOT NULL CHECK(source_kind IN ('live-post-tool','vendor-transcript','vendor-patch-result')),
  source_ref TEXT NOT NULL DEFAULT '',
  source_sequence TEXT NOT NULL DEFAULT '',
  source_digest TEXT NOT NULL,
  source_segment_id TEXT NOT NULL DEFAULT '',
  collector_id TEXT NOT NULL DEFAULT '',
  collector_version TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL CHECK(state IN ('success','failure','cancelled','timeout','unknown')),
  error_class TEXT NOT NULL DEFAULT '',
  started_at INTEGER NOT NULL DEFAULT 0,
  returned_at INTEGER NOT NULL DEFAULT 0,
  completed_at INTEGER NOT NULL DEFAULT 0,
  duration_ms INTEGER NOT NULL DEFAULT 0 CHECK(duration_ms >= 0),
  media_type TEXT NOT NULL DEFAULT '',
  raw_bytes INTEGER NOT NULL DEFAULT 0 CHECK(raw_bytes >= 0),
  raw_field_bytes INTEGER NOT NULL DEFAULT 0 CHECK(raw_field_bytes >= 0),
  decoded_bytes INTEGER NOT NULL DEFAULT 0 CHECK(decoded_bytes >= 0),
  retained_bytes INTEGER NOT NULL DEFAULT 0 CHECK(retained_bytes >= 0),
  external_locator_bytes INTEGER NOT NULL DEFAULT 0 CHECK(external_locator_bytes >= 0),
  payload_digest TEXT NOT NULL DEFAULT '',
  completeness TEXT NOT NULL CHECK(completeness IN ('complete','metadata-only','unavailable','redacted')),
  payload BLOB,
  stdout_bytes INTEGER NOT NULL DEFAULT 0 CHECK(stdout_bytes >= 0),
  stdout_digest TEXT NOT NULL DEFAULT '',
  stderr_bytes INTEGER NOT NULL DEFAULT 0 CHECK(stderr_bytes >= 0),
  stderr_digest TEXT NOT NULL DEFAULT '',
  queued_at INTEGER NOT NULL DEFAULT 0,
  received_at INTEGER NOT NULL DEFAULT 0,
  delivery_attempts INTEGER NOT NULL DEFAULT 1 CHECK(delivery_attempts >= 1),
  delivery_mode TEXT NOT NULL DEFAULT 'transcript' CHECK(delivery_mode IN ('direct','replay','transcript')),
  CHECK((payload IS NULL AND retained_bytes=0) OR (payload IS NOT NULL AND retained_bytes=length(payload))),
  CHECK((completeness='complete' AND payload IS NOT NULL AND payload_digest!='') OR
        (completeness IN ('metadata-only','redacted') AND payload IS NULL AND payload_digest!='') OR
        (completeness='unavailable' AND payload IS NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS result_observation_source
  ON result_observation(runtime,session_id,source_segment_id,source_kind,source_sequence,source_digest);
CREATE INDEX IF NOT EXISTS result_observation_session
  ON result_observation(session_id,completed_at,id);
CREATE INDEX IF NOT EXISTS result_observation_native
  ON result_observation(runtime,session_id,native_call_kind,native_call_id);
CREATE INDEX IF NOT EXISTS result_observation_identity_repair
  ON result_observation(runtime,source_kind,native_call_kind,id);

-- Adapter-supplied alternate native identity for a retained result. The raw identity
-- remains on result_observation; aliases are append-only evidence consumed by the one
-- generic reconciler, not a mutable canonical-ID registry.
CREATE TABLE IF NOT EXISTS result_native_call_alias(
  result_id INTEGER NOT NULL REFERENCES result_observation(id) ON DELETE RESTRICT,
  native_call_kind TEXT NOT NULL,
  native_call_id TEXT NOT NULL,
  algorithm TEXT NOT NULL,
  PRIMARY KEY(result_id,native_call_kind,native_call_id)
);
CREATE INDEX IF NOT EXISTS result_native_call_alias_lookup
  ON result_native_call_alias(native_call_kind,native_call_id,result_id);

CREATE TABLE IF NOT EXISTS result_effect(
  result_id INTEGER NOT NULL REFERENCES result_observation(id) ON DELETE RESTRICT,
  ordinal INTEGER NOT NULL,
  entity_id TEXT NOT NULL DEFAULT '',
  raw_identity TEXT NOT NULL DEFAULT '',
  operation TEXT NOT NULL CHECK(operation IN ('create','update','delete','move','write','unknown')),
  move_target TEXT NOT NULL DEFAULT '',
  evidence_source TEXT NOT NULL CHECK(evidence_source IN ('runtime-result','derived-input')),
  source_field TEXT NOT NULL DEFAULT '',
  completeness TEXT NOT NULL CHECK(completeness IN ('complete','partial','metadata-only','unresolved','unknown')),
  replace_all INTEGER NOT NULL DEFAULT 0 CHECK(replace_all IN (0,1)),
  replacement_before_bytes INTEGER NOT NULL DEFAULT 0 CHECK(replacement_before_bytes >= 0),
  replacement_before_digest TEXT NOT NULL DEFAULT '',
  replacement_before_payload BLOB,
  replacement_after_bytes INTEGER NOT NULL DEFAULT 0 CHECK(replacement_after_bytes >= 0),
  replacement_after_digest TEXT NOT NULL DEFAULT '',
  replacement_after_payload BLOB,
  content_bytes INTEGER NOT NULL DEFAULT 0 CHECK(content_bytes >= 0),
  content_digest TEXT NOT NULL DEFAULT '',
  content_payload BLOB,
  diff_bytes INTEGER NOT NULL DEFAULT 0 CHECK(diff_bytes >= 0),
  diff_digest TEXT NOT NULL DEFAULT '',
  diff_completeness TEXT NOT NULL CHECK(diff_completeness IN ('complete','metadata-only','unavailable')),
  diff_payload BLOB,
  PRIMARY KEY(result_id,ordinal),
  CHECK((replacement_before_payload IS NULL) OR length(replacement_before_payload)=replacement_before_bytes),
  CHECK((replacement_after_payload IS NULL) OR length(replacement_after_payload)=replacement_after_bytes),
  CHECK((content_payload IS NULL) OR length(content_payload)=content_bytes),
  CHECK((diff_payload IS NULL) OR length(diff_payload)=diff_bytes)
);

CREATE TABLE IF NOT EXISTS result_reconciliation(
  id INTEGER PRIMARY KEY,
  result_id INTEGER NOT NULL REFERENCES result_observation(id) ON DELETE RESTRICT,
  join_class TEXT NOT NULL CHECK(join_class IN ('exact','indirect','ambiguous','unjoined','same-logical-result')),
  algorithm TEXT NOT NULL,
  reconciled_at INTEGER NOT NULL,
  supersedes_id INTEGER REFERENCES result_reconciliation(id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS result_reconciliation_current
  ON result_reconciliation(result_id,reconciled_at,id);
CREATE INDEX IF NOT EXISTS result_reconciliation_ambiguous
  ON result_reconciliation(join_class,result_id,id) WHERE join_class='ambiguous';
CREATE TABLE IF NOT EXISTS result_reconciliation_candidate(
  reconciliation_id INTEGER NOT NULL REFERENCES result_reconciliation(id) ON DELETE RESTRICT,
  event_id INTEGER NOT NULL REFERENCES event(id) ON DELETE RESTRICT,
  candidate_reason TEXT NOT NULL,
  selected INTEGER NOT NULL CHECK(selected IN (0,1)),
  PRIMARY KEY(reconciliation_id,event_id)
);
CREATE INDEX IF NOT EXISTS result_reconciliation_candidate_event
  ON result_reconciliation_candidate(event_id,reconciliation_id);
CREATE TABLE IF NOT EXISTS logical_result_relation(
  result_id INTEGER NOT NULL REFERENCES result_observation(id) ON DELETE RESTRICT,
  peer_result_id INTEGER NOT NULL REFERENCES result_observation(id) ON DELETE RESTRICT,
  relation TEXT NOT NULL CHECK(relation='same-logical-result'),
  algorithm TEXT NOT NULL,
  related_at INTEGER NOT NULL,
  PRIMARY KEY(result_id,peer_result_id),
  CHECK(result_id < peer_result_id)
);
CREATE INDEX IF NOT EXISTS logical_result_relation_peer
  ON logical_result_relation(peer_result_id,result_id);

CREATE TABLE IF NOT EXISTS transcript_cursor(
  runtime TEXT NOT NULL,
  source_ref TEXT NOT NULL,
  source_segment_id TEXT NOT NULL,
  session_id TEXT NOT NULL DEFAULT '',
  working_directory TEXT NOT NULL DEFAULT '',
  file_size INTEGER NOT NULL CHECK(file_size >= 0),
  file_mtime INTEGER NOT NULL DEFAULT 0,
  generation_digest TEXT NOT NULL,
  source_sequence TEXT NOT NULL DEFAULT '',
  committed_offset INTEGER NOT NULL DEFAULT 0 CHECK(committed_offset >= 0),
  source_line INTEGER NOT NULL DEFAULT 0 CHECK(source_line >= 0),
  parser_state BLOB,
  parser_state_digest TEXT NOT NULL DEFAULT '',
  rescan_needed INTEGER NOT NULL DEFAULT 0 CHECK(rescan_needed IN (0,1)),
  continuation_needed INTEGER NOT NULL DEFAULT 0 CHECK(continuation_needed IN (0,1)),
  action_parser_version INTEGER NOT NULL DEFAULT 0 CHECK(action_parser_version >= -2),
  updated_at INTEGER NOT NULL,
  CHECK(committed_offset <= file_size),
  PRIMARY KEY(runtime,source_ref,source_segment_id)
);
-- One governed action may name several exact resources (for example one
-- apply_patch call). Keep action cardinality in event and target cardinality here.
CREATE TABLE IF NOT EXISTS event_resource(
  event_id INTEGER NOT NULL REFERENCES event(id) ON DELETE RESTRICT,
  ordinal INTEGER NOT NULL,
  entity_id TEXT NOT NULL REFERENCES entity(id) ON DELETE RESTRICT,
  source TEXT NOT NULL,
  raw_identity TEXT NOT NULL DEFAULT '',
  operation TEXT NOT NULL DEFAULT 'unknown' CHECK(operation IN ('read','search','write','patch','execute','connect','use','unknown')),
  evidence_class TEXT NOT NULL DEFAULT 'unknown' CHECK(evidence_class IN ('declared','observed','unknown')),
  source_field TEXT NOT NULL DEFAULT '',
  completeness TEXT NOT NULL DEFAULT 'unknown' CHECK(completeness IN ('complete','partial','unresolved','unknown')),
  PRIMARY KEY(event_id, ordinal)
);
CREATE INDEX IF NOT EXISTS event_resource_entity ON event_resource(entity_id,event_id);

-- entity: a governed resource. State accretes on it, vendor-neutral.
CREATE TABLE IF NOT EXISTS entity(
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  identity TEXT NOT NULL,
  first_seen INTEGER,
  last_seen INTEGER
);

-- entity_state / session_state: the materialized fold. One row per distinct
-- (subject, key, value, detector); first/last_seen bound its observation window.
CREATE TABLE IF NOT EXISTS entity_state(
  entity_id TEXT NOT NULL,
  key TEXT NOT NULL,
  value TEXT NOT NULL,
  detector TEXT NOT NULL,
  provenance TEXT,
  evidence TEXT,
  first_seen INTEGER,
  last_seen INTEGER,
  PRIMARY KEY(entity_id, key, value, detector)
);
CREATE TABLE IF NOT EXISTS session_state(
  session_id TEXT NOT NULL,
  key TEXT NOT NULL,
  value TEXT NOT NULL,
  detector TEXT NOT NULL,
  provenance TEXT,
  evidence TEXT,
  first_seen INTEGER,
  last_seen INTEGER,
  PRIMARY KEY(session_id, key, value, detector)
);

-- C6 portable change envelope. This is typed evidence in the ONE store, not
-- configuration and not a second action log.
CREATE TABLE IF NOT EXISTS change_record(
  id INTEGER PRIMARY KEY,
  session_id TEXT NOT NULL,
  repository_id TEXT NOT NULL,
  checkout_id TEXT NOT NULL,
  repository_identity_kind TEXT NOT NULL CHECK(repository_identity_kind IN ('remote-sha256','local-sha256')),
  checkout_root TEXT NOT NULL,
  session_runtime_claim TEXT NOT NULL DEFAULT '',
  session_title_claim TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL CHECK(kind IN ('declaration','revision','implementation','verification')),
  evidence_class TEXT NOT NULL CHECK(evidence_class IN ('claimed','observed')),
  source_kind TEXT NOT NULL CHECK(source_kind IN ('file','git','process')),
  source_ref TEXT NOT NULL,
  source_display TEXT NOT NULL,
  source_digest TEXT NOT NULL,
  recorded_at INTEGER NOT NULL,
  capture_started_at INTEGER,
  capture_ended_at INTEGER,
  base_revision TEXT,
  head_revision TEXT,
  git_version TEXT,
  snapshot_digest TEXT,
  capture_attempts INTEGER,
  included_layers TEXT,
  ignored_included INTEGER,
  sparse_checkout INTEGER,
  common_dir_digest TEXT,
  non_atomic INTEGER,
  intent_label TEXT,
  verification_name TEXT,
  verification_boundary TEXT,
  verification_name_class TEXT,
  verification_result TEXT,
  executable_path TEXT,
  executable_digest TEXT,
  argv_digest TEXT,
  exit_code INTEGER,
  signal TEXT,
  termination TEXT,
  environment_state TEXT,
  child_cleanup TEXT,
  limitation TEXT,
  CHECK(
    (kind='declaration' AND evidence_class='claimed' AND source_kind='file' AND intent_label IS NOT NULL
      AND base_revision IS NULL AND executable_path IS NULL) OR
    (kind='revision' AND evidence_class='observed' AND source_kind='git' AND base_revision IS NOT NULL
      AND head_revision IS NOT NULL AND snapshot_digest IS NOT NULL AND capture_started_at IS NOT NULL
      AND capture_ended_at IS NOT NULL AND intent_label IS NULL AND executable_path IS NULL) OR
    (kind='implementation' AND evidence_class='claimed' AND source_kind='file'
      AND intent_label IS NULL AND base_revision IS NULL AND executable_path IS NULL) OR
    (kind='verification' AND evidence_class='claimed' AND source_kind='file'
      AND verification_name IS NOT NULL AND verification_name_class='claimed' AND base_revision IS NULL) OR
    (kind='verification' AND evidence_class='observed' AND source_kind='process'
      AND verification_name IS NOT NULL AND verification_name_class='claimed'
      AND capture_started_at IS NOT NULL AND capture_ended_at IS NOT NULL AND termination IS NOT NULL
      AND base_revision IS NULL AND intent_label IS NULL)
  )
);
CREATE INDEX IF NOT EXISTS change_record_latest
  ON change_record(session_id,repository_id,checkout_id,kind,recorded_at DESC,id DESC);
CREATE INDEX IF NOT EXISTS change_record_session ON change_record(session_id,recorded_at,id);
CREATE TABLE IF NOT EXISTS change_item(
  record_id INTEGER NOT NULL REFERENCES change_record(id) ON DELETE RESTRICT,
  ordinal INTEGER NOT NULL,
  path TEXT NOT NULL,
  old_path TEXT NOT NULL DEFAULT '',
  symbol TEXT NOT NULL DEFAULT '',
  layer TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(record_id, ordinal),
  UNIQUE(record_id,path,old_path,symbol,layer,status)
);
CREATE TRIGGER IF NOT EXISTS change_item_kind_insert BEFORE INSERT ON change_item
BEGIN
  SELECT CASE
    WHEN (SELECT kind FROM change_record WHERE id=NEW.record_id)='verification'
      THEN RAISE(ABORT,'verification records cannot have items')
    WHEN (SELECT kind FROM change_record WHERE id=NEW.record_id) IN ('declaration','implementation')
      AND (NEW.old_path!='' OR NEW.layer!='' OR NEW.status!='')
      THEN RAISE(ABORT,'claim items cannot have revision fields')
    WHEN (SELECT kind FROM change_record WHERE id=NEW.record_id)='revision'
      AND (NEW.layer NOT IN ('committed','index','worktree','untracked') OR NEW.status='')
      THEN RAISE(ABORT,'revision items require a known layer and status')
  END;
END;

CREATE TABLE IF NOT EXISTS session_checkpoint(
  id INTEGER PRIMARY KEY,
  runtime TEXT NOT NULL DEFAULT '',
  session_id TEXT NOT NULL,
  scope_key TEXT NOT NULL,
  kind TEXT NOT NULL CHECK(kind IN ('attachment','pre-mutation','settled','pre-verification','pre-commit','closing','timeout')),
  request_id TEXT NOT NULL UNIQUE,
  trigger_event_id INTEGER REFERENCES event(id) ON DELETE SET NULL,
  trigger_result_id INTEGER REFERENCES result_observation(id) ON DELETE SET NULL,
  trigger_observation_id TEXT NOT NULL DEFAULT '',
  predecessor_checkpoint_id INTEGER REFERENCES session_checkpoint(id) ON DELETE SET NULL,
  working_directory TEXT NOT NULL,
  repository_id TEXT NOT NULL DEFAULT '',
  checkout_id TEXT NOT NULL DEFAULT '',
  checkout_root TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK(status IN ('pending','capturing','complete','failed','unavailable')),
  boundary_class TEXT NOT NULL CHECK(boundary_class IN ('direct-pre-release','late-replay','unconfirmed','settled','point-in-time','exact-close')),
  requested_at INTEGER NOT NULL,
  capture_started_at INTEGER NOT NULL DEFAULT 0,
  capture_ended_at INTEGER NOT NULL DEFAULT 0,
  capture_attempts INTEGER NOT NULL DEFAULT 0 CHECK(capture_attempts >= 0),
  change_record_id INTEGER REFERENCES change_record(id) ON DELETE RESTRICT,
  failure_kind TEXT NOT NULL DEFAULT '',
  detail_digest TEXT NOT NULL DEFAULT '',
  CHECK((status='complete' AND change_record_id IS NOT NULL AND failure_kind='') OR
        (status IN ('failed','unavailable') AND change_record_id IS NULL AND failure_kind!='') OR
        (status IN ('pending','capturing') AND change_record_id IS NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS session_checkpoint_single_boundary
  ON session_checkpoint(session_id,scope_key,kind) WHERE kind IN ('attachment','pre-mutation');
CREATE INDEX IF NOT EXISTS session_checkpoint_status
  ON session_checkpoint(status,requested_at,id);

CREATE TABLE IF NOT EXISTS session_checkpoint_trigger(
  checkpoint_id INTEGER NOT NULL REFERENCES session_checkpoint(id) ON DELETE RESTRICT,
  result_id INTEGER NOT NULL REFERENCES result_observation(id) ON DELETE RESTRICT,
  observation_id TEXT NOT NULL,
  accepted_at INTEGER NOT NULL,
  PRIMARY KEY(checkpoint_id,result_id)
);

CREATE TABLE IF NOT EXISTS checkpoint_payload(
  checkpoint_id INTEGER NOT NULL REFERENCES session_checkpoint(id) ON DELETE RESTRICT,
  ordinal INTEGER NOT NULL,
  path TEXT NOT NULL,
  layer TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT '',
  content_bytes INTEGER NOT NULL DEFAULT 0 CHECK(content_bytes >= 0),
  content_digest TEXT NOT NULL DEFAULT '',
  content_completeness TEXT NOT NULL CHECK(content_completeness IN ('complete','metadata-only','unavailable')),
  content_payload BLOB,
  patch_bytes INTEGER NOT NULL DEFAULT 0 CHECK(patch_bytes >= 0),
  patch_digest TEXT NOT NULL DEFAULT '',
  patch_completeness TEXT NOT NULL CHECK(patch_completeness IN ('complete','metadata-only','unavailable')),
  patch_payload BLOB,
  source_identity TEXT NOT NULL DEFAULT '',
  limitation TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(checkpoint_id,ordinal),
  CHECK(content_payload IS NULL OR length(content_payload)=content_bytes),
  CHECK(patch_payload IS NULL OR length(patch_payload)=patch_bytes)
);

CREATE TABLE IF NOT EXISTS evidence_body(
  digest TEXT PRIMARY KEY,
  media_type TEXT NOT NULL,
  body_bytes INTEGER NOT NULL CHECK(body_bytes >= 0),
  payload BLOB NOT NULL,
  CHECK(length(payload)=body_bytes)
);

CREATE TABLE IF NOT EXISTS checkpoint_payload_body(
  checkpoint_id INTEGER NOT NULL,
  ordinal INTEGER NOT NULL,
  kind TEXT NOT NULL CHECK(kind IN ('content','patch')),
  body_digest TEXT NOT NULL REFERENCES evidence_body(digest) ON DELETE RESTRICT,
  PRIMARY KEY(checkpoint_id,ordinal,kind),
  FOREIGN KEY(checkpoint_id,ordinal) REFERENCES checkpoint_payload(checkpoint_id,ordinal) ON DELETE RESTRICT
);

CREATE TABLE IF NOT EXISTS path_reconciliation(
  id INTEGER PRIMARY KEY,
  session_id TEXT NOT NULL,
  checkout_id TEXT NOT NULL,
  predecessor_checkpoint_id INTEGER REFERENCES session_checkpoint(id) ON DELETE RESTRICT,
  current_checkpoint_id INTEGER NOT NULL REFERENCES session_checkpoint(id) ON DELETE RESTRICT,
  event_id INTEGER REFERENCES event(id) ON DELETE SET NULL,
  result_id INTEGER REFERENCES result_observation(id) ON DELETE SET NULL,
  path TEXT NOT NULL,
  old_path TEXT NOT NULL DEFAULT '',
  classification TEXT NOT NULL CHECK(classification IN ('inherited','native-effect-git-matched','action-reported','interval-associated','session-associated','no-net-change-at-checkpoint','unattributed','ambiguous','unavailable')),
  competing_actor TEXT NOT NULL DEFAULT '',
  runtime_effect_digest TEXT NOT NULL DEFAULT '',
  git_effect_digest TEXT NOT NULL DEFAULT '',
  algorithm TEXT NOT NULL,
  reconciled_at INTEGER NOT NULL,
  supersedes_id INTEGER REFERENCES path_reconciliation(id) ON DELETE RESTRICT
);

CREATE TABLE IF NOT EXISTS collection_issue(
  id INTEGER PRIMARY KEY,
  issue_id TEXT NOT NULL UNIQUE,
  session_id TEXT NOT NULL DEFAULT '',
  runtime TEXT NOT NULL DEFAULT '',
  observation_id TEXT NOT NULL DEFAULT '',
  native_call_id TEXT NOT NULL DEFAULT '',
  source_ref TEXT NOT NULL DEFAULT '',
  source_segment_id TEXT NOT NULL DEFAULT '',
  collector_id TEXT NOT NULL,
  kind TEXT NOT NULL CHECK(kind IN ('collision','malformed','checkpoint-failed','result-collision','result-missing','result-unjoined','result-payload-bounded','transcript-unavailable','transcript-changed','checkpoint-payload-bounded','attribution-ambiguous','reconciliation-failed','parser-state-evicted')),
  affected_count INTEGER NOT NULL CHECK(affected_count >= 1),
  first_seen INTEGER NOT NULL,
  last_seen INTEGER NOT NULL,
  detail_digest TEXT NOT NULL DEFAULT '',
  resolved_at INTEGER NOT NULL DEFAULT 0,
  resolution_class TEXT NOT NULL DEFAULT '' CHECK(resolution_class IN ('','recovered','outside-observation')),
  CHECK(kind!='parser-state-evicted' OR
    (native_call_id!='' AND source_ref!='' AND source_segment_id!=''))
);
CREATE INDEX IF NOT EXISTS collection_issue_active
  ON collection_issue(kind,resolved_at,id);

-- C7 durable understanding evidence. These are append-only analyzer generations,
-- deliberately separate from entity_state: that table is the governed-event fold
-- and is directly visible to stateful policy. A generation row is never rewritten
-- except for facts_state, which retention flips to 'pruned' once when it removes
-- the row's unit, edge and coverage children (understanding-facts-retention plan).
CREATE TABLE IF NOT EXISTS understanding_generation(
  id INTEGER PRIMARY KEY,
  repository_id TEXT NOT NULL,
  checkout_id TEXT NOT NULL,
  checkout_root TEXT NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('complete','failed')),
  snapshot_protocol TEXT NOT NULL DEFAULT '',
  snapshot_digest TEXT NOT NULL DEFAULT '',
  base_revision TEXT NOT NULL DEFAULT '',
  head_revision TEXT NOT NULL DEFAULT '',
  structural_schema TEXT NOT NULL DEFAULT '',
  analyzer_bundle_digest TEXT NOT NULL DEFAULT '',
  convention_state TEXT NOT NULL CHECK(convention_state IN ('none','explicit')),
  convention_source_ref TEXT NOT NULL DEFAULT '',
  convention_source_digest TEXT NOT NULL DEFAULT '',
  started_at INTEGER NOT NULL,
  ended_at INTEGER NOT NULL,
  path_total INTEGER NOT NULL DEFAULT 0 CHECK(path_total >= 0),
  unit_total INTEGER NOT NULL DEFAULT 0 CHECK(unit_total >= 0),
  edge_total INTEGER NOT NULL DEFAULT 0 CHECK(edge_total >= 0),
  error_total INTEGER NOT NULL DEFAULT 0 CHECK(error_total >= 0),
  limitation_code TEXT NOT NULL DEFAULT '',
  limitation TEXT NOT NULL DEFAULT '',
  facts_state TEXT NOT NULL DEFAULT 'present' CHECK(facts_state IN ('present','pruned')),
  CHECK(ended_at >= started_at),
  CHECK(
    (convention_state='none' AND convention_source_ref='' AND convention_source_digest='') OR
    (convention_state='explicit' AND convention_source_ref!='' AND convention_source_digest!='')
  ),
  CHECK(
    (status='complete' AND snapshot_protocol='git-tree-v2' AND snapshot_digest LIKE 'git-tree-v2-sha256:%'
      AND structural_schema!='' AND analyzer_bundle_digest!='' AND limitation_code='') OR
    (status='failed' AND limitation_code!='')
  )
);
CREATE INDEX IF NOT EXISTS understanding_generation_latest
  ON understanding_generation(repository_id,checkout_id,started_at DESC,id DESC);
CREATE INDEX IF NOT EXISTS understanding_generation_snapshot
  ON understanding_generation(repository_id,checkout_id,snapshot_digest,analyzer_bundle_digest,
    convention_state,started_at DESC,id DESC);

-- A unit's descriptor is stored once per distinct text (v45): scans of an unchanged
-- file repeat the same descriptor, and 2M unit rows held 51K distinct ones. The
-- digest is of the descriptor text itself. The index on descriptor_digest is created
-- by the v45 migration step, because this DDL runs before migrate() on a store whose
-- unit table does not have the column yet.
CREATE TABLE IF NOT EXISTS understanding_descriptor(
  digest TEXT PRIMARY KEY CHECK(digest LIKE 'sha256-v1:%'),
  descriptor_json TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS understanding_unit(
  generation_id INTEGER NOT NULL REFERENCES understanding_generation(id) ON DELETE RESTRICT,
  path TEXT NOT NULL,
  source_hash TEXT NOT NULL CHECK(source_hash LIKE 'sha256-v1:%'),
  language TEXT NOT NULL DEFAULT '',
  namespace TEXT NOT NULL DEFAULT '',
  descriptor_digest TEXT NOT NULL REFERENCES understanding_descriptor(digest) ON DELETE RESTRICT,
  PRIMARY KEY(generation_id,path)
);

-- WITHOUT ROWID (v45): the primary key spans nine of the eleven columns, so a rowid
-- table stored nearly every edge twice. Lookups by (generation_id,from_kind,from_ref)
-- use the primary key itself; there is no separate "from" index.
CREATE TABLE IF NOT EXISTS understanding_edge(
  generation_id INTEGER NOT NULL REFERENCES understanding_generation(id) ON DELETE RESTRICT,
  from_kind TEXT NOT NULL CHECK(from_kind IN ('file','package','symbol','document','item')),
  from_ref TEXT NOT NULL,
  relation TEXT NOT NULL CHECK(relation IN (
    'file_in_package','package_depends_on','file_declares_symbol',
    'symbol_calls_symbol','file_references_symbol',
    'document_references_file','document_defines_item','item_references_file'
  )),
  to_kind TEXT NOT NULL CHECK(to_kind IN ('file','package','symbol','document','item')),
  to_ref TEXT NOT NULL,
  source_path TEXT NOT NULL DEFAULT '',
  source_line INTEGER NOT NULL DEFAULT 0 CHECK(source_line >= 0),
  provenance TEXT NOT NULL CHECK(provenance='measured'),
  analyzer_id TEXT NOT NULL,
  evidence_digest TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(generation_id,from_kind,from_ref,relation,to_kind,to_ref,source_path,source_line,analyzer_id),
  CHECK(
    (relation='file_in_package' AND from_kind='file' AND to_kind='package') OR
    (relation='package_depends_on' AND from_kind='package' AND to_kind='package') OR
    (relation='file_declares_symbol' AND from_kind='file' AND to_kind='symbol') OR
    (relation='symbol_calls_symbol' AND from_kind='symbol' AND to_kind='symbol') OR
    (relation='file_references_symbol' AND from_kind='file' AND to_kind='symbol') OR
    (relation='document_references_file' AND from_kind='document' AND to_kind='file') OR
    (relation='document_defines_item' AND from_kind='document' AND to_kind='item') OR
    (relation='item_references_file' AND from_kind='item' AND to_kind='file')
  )
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS understanding_edge_to
  ON understanding_edge(generation_id,to_kind,to_ref);

CREATE TABLE IF NOT EXISTS understanding_coverage(
  generation_id INTEGER NOT NULL REFERENCES understanding_generation(id) ON DELETE RESTRICT,
  family TEXT NOT NULL CHECK(family IN (
    'file_inventory','package_dependency','symbol_declaration','responsibility_fingerprint','symbol_call',
    'document_reference','configuration_effect','data_effect','policy_effect','journey_effect'
  )),
  state TEXT NOT NULL CHECK(state IN ('complete','partial','unsupported','failed')),
  analyzer_id TEXT NOT NULL,
  attempted INTEGER NOT NULL DEFAULT 0 CHECK(attempted >= 0),
  produced INTEGER NOT NULL DEFAULT 0 CHECK(produced >= 0),
  errors INTEGER NOT NULL DEFAULT 0 CHECK(errors >= 0),
  unresolved INTEGER NOT NULL DEFAULT 0 CHECK(unresolved >= 0),
  ambiguous INTEGER NOT NULL DEFAULT 0 CHECK(ambiguous >= 0),
  reason TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(generation_id,family,analyzer_id),
  CHECK((state='complete' AND reason='') OR state!='complete')
);

CREATE TRIGGER IF NOT EXISTS understanding_unit_complete BEFORE INSERT ON understanding_unit
BEGIN
  SELECT CASE WHEN COALESCE((SELECT status FROM understanding_generation WHERE id=NEW.generation_id),'')!='complete'
    THEN RAISE(ABORT,'understanding units require a complete generation') END;
END;
CREATE TRIGGER IF NOT EXISTS understanding_edge_complete BEFORE INSERT ON understanding_edge
BEGIN
  SELECT CASE WHEN COALESCE((SELECT status FROM understanding_generation WHERE id=NEW.generation_id),'')!='complete'
    THEN RAISE(ABORT,'understanding edges require a complete generation') END;
END;
CREATE TRIGGER IF NOT EXISTS understanding_coverage_complete BEFORE INSERT ON understanding_coverage
BEGIN
  SELECT CASE WHEN COALESCE((SELECT status FROM understanding_generation WHERE id=NEW.generation_id),'')!='complete'
    THEN RAISE(ABORT,'understanding coverage requires a complete generation') END;
END;

-- Memory as first-class records (v36; memory-first-class-records plan §3.1).
-- Store-canonical dossiers with WIRE identity: global_id mem_… (RT-4), author
-- {type,id} always present (RT-3 — an empty author is invalid on the wire),
-- per-record revision with content_hash over the wire shape. The files directory
-- is a write-through mirror, never truth.
CREATE TABLE IF NOT EXISTS memory_record(
  id TEXT PRIMARY KEY,
  global_id TEXT NOT NULL UNIQUE,
  status TEXT NOT NULL CHECK(status IN ('active','pending','rejected')),
  scope_type TEXT NOT NULL DEFAULT 'user' CHECK(scope_type IN ('user','repository','organization')),
  scope_id TEXT NOT NULL DEFAULT '',
  repository_identity TEXT NOT NULL DEFAULT 'weak' CHECK(repository_identity IN ('weak','remote-sha256')),
  title TEXT NOT NULL,
  category TEXT NOT NULL,
  body TEXT NOT NULL,
  tags TEXT NOT NULL DEFAULT '[]',
  aliases TEXT NOT NULL DEFAULT '[]',
  source TEXT NOT NULL CHECK(source IN ('human','agent','import','harvest')),
  origin TEXT NOT NULL DEFAULT '',
  superseded_by TEXT NOT NULL DEFAULT '',
  verified_at TEXT NOT NULL DEFAULT '',
  verified_by TEXT NOT NULL DEFAULT '',
  author_type TEXT NOT NULL DEFAULT 'user',
  author_id TEXT NOT NULL DEFAULT '',
  revision INTEGER NOT NULL DEFAULT 1 CHECK(revision >= 1),
  content_hash TEXT NOT NULL,
  reject_reason TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  -- Team sync state (v44, team item 5 decisions 13/14/15). share_state: only a shared
  -- record leaves the device. pushed_hash / server_revision / synced_projection_hash:
  -- the base the next push names, its server number, and the content projection at that
  -- moment (in sync = the current projection equals it). held_*: a pulled revision
  -- waiting for this record's in-flight push to settle. wire_slug: the team's slug when
  -- the local id is a collision alias. collision: alias | shadowed (by a user record).
  share_state TEXT NOT NULL DEFAULT 'unshared' CHECK(share_state IN ('unshared','shared')),
  sync_origin TEXT NOT NULL DEFAULT 'local' CHECK(sync_origin IN ('local','pulled')),
  pushed_hash TEXT NOT NULL DEFAULT '',
  server_revision INTEGER NOT NULL DEFAULT 0,
  synced_projection_hash TEXT NOT NULL DEFAULT '',
  held_wire_record TEXT NOT NULL DEFAULT '',
  held_server_revision INTEGER NOT NULL DEFAULT 0,
  wire_slug TEXT NOT NULL DEFAULT '',
  collision TEXT NOT NULL DEFAULT '' CHECK(collision IN ('','alias','shadowed')),
  team_author TEXT NOT NULL DEFAULT '',
  identity_note TEXT NOT NULL DEFAULT '',
  -- The team record's global id a user-scope copy was detached from (O-11); '' otherwise.
  detached_from TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS memory_record_updated ON memory_record(updated_at);
CREATE INDEX IF NOT EXISTS memory_record_status ON memory_record(status);
-- Session citations (ADR 0013 D2): the contested-memory evidence path.
-- anchor_kind closes O2: event-uuid | turn-id | line-offset | none.
-- Sources cascade with the record row (a resurrected id starts fresh);
-- REVISIONS do not (see below).
CREATE TABLE IF NOT EXISTS memory_source(
  record_id TEXT NOT NULL REFERENCES memory_record(id) ON DELETE CASCADE,
  vendor TEXT NOT NULL,
  session_id TEXT NOT NULL,
  anchor TEXT NOT NULL DEFAULT '',
  anchor_kind TEXT NOT NULL DEFAULT 'none' CHECK(anchor_kind IN ('none','event-uuid','turn-id','line-offset')),
  captured_at INTEGER NOT NULL,
  PRIMARY KEY(record_id,vendor,session_id,anchor)
);
-- Per-record hash-chained revision snapshots — the git-history replacement:
-- blame/bisect/undo are queries, not filesystem archaeology. Revisions are
-- deliberately NOT cascade-deleted: honest deletion (ADR 0013 D7) removes the
-- record from store/index/mirror while the revision history keeps the content,
-- exactly as the archived git history did before it — EXCEPT for team records
-- (shared or pulled), whose deletion erases their revisions too (team item 5, O-7).
-- Keyed by the record's global id (v44), so a re-created slug never overwrites
-- another record's history; revision is the LOCAL counter and server_revision the
-- team server's number when the revision was accepted or landed from a pull.
` + memoryRevisionDDL + `
-- Deletion that syncs (ADR 0013 D7 honest deletion, made shareable). Keyed by the
-- record's global id (v44): a slug deleted, re-created and deleted again is two
-- tombstones. A slug deleted before v44 is keyed 'slug:<id>'. origin says whether this
-- device deleted it or a teammate's deletion arrived by pull.
` + memoryTombstoneDDL + `
CREATE INDEX IF NOT EXISTS memory_tombstone_slug ON memory_tombstone(id);
-- Conflict copies (team item 5, decision 13b): a local version the team's revision
-- displaced — a stale push, a pulled revision over an unsent edit, or a deletion over an
-- edit. Disclosed, never merged (invariant 3).
CREATE TABLE IF NOT EXISTS memory_conflict(
  conflict_id INTEGER PRIMARY KEY,
  global_id TEXT NOT NULL,
  record_id TEXT NOT NULL,
  reason TEXT NOT NULL CHECK(reason IN ('stale_base','pulled_over_edit','deleted')),
  local_revision INTEGER NOT NULL,
  body TEXT NOT NULL,
  by_author TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS memory_conflict_record ON memory_conflict(global_id);
-- Owner tags on memory records (plan §6): the SAME grammar, folding and
-- validation as session owner tags (session_owner_tag.go), one more record
-- kind — not a second tag system. The row is separate because the session
-- table's CHECKs encode session identity; the TAG DISCIPLINE is shared code.
CREATE TABLE IF NOT EXISTS memory_owner_tag(
  tag_id TEXT PRIMARY KEY,
  record_id TEXT NOT NULL CHECK(length(record_id) > 0),
  key TEXT NOT NULL DEFAULT '' CHECK(length(key) <= 64),
  value TEXT NOT NULL CHECK(length(value) BETWEEN 1 AND 64),
  key_fold TEXT NOT NULL DEFAULT '',
  value_fold TEXT NOT NULL,
  applied_at INTEGER NOT NULL,
  retracted_at INTEGER NOT NULL DEFAULT 0,
  UNIQUE(record_id,key_fold,value_fold,applied_at)
);
CREATE UNIQUE INDEX IF NOT EXISTS memory_owner_tag_active
  ON memory_owner_tag(record_id,key_fold,value_fold) WHERE retracted_at=0;
CREATE INDEX IF NOT EXISTS memory_owner_tag_fold ON memory_owner_tag(key_fold,value_fold);
-- Agent-initiated cross-vendor sends (v42; planned as v37, renumbered at merge; session-message-cross-vendor-plan §4).
-- One record per MCP send request: admission facts (digest), resolved tier, and
-- the terminal outcome. The boundary carrier claims from session_delivery; the
-- by-target index here serves the per-target budget and the console display.
-- Additive; rolling back is dropping the table and stamping 41.
CREATE TABLE IF NOT EXISTS session_message_invocation(
  invocation_id TEXT PRIMARY KEY,
  caller_runtime TEXT NOT NULL,
  caller_native_id TEXT NOT NULL DEFAULT '',
  target_runtime TEXT NOT NULL,
  target_catalog_id TEXT NOT NULL DEFAULT '',
  target_native_id TEXT NOT NULL DEFAULT '',
  message TEXT NOT NULL,
  state TEXT NOT NULL CHECK(state IN ('pending','refused','accepted','delivered','expired','unavailable','unknown')),
  receipt TEXT NOT NULL DEFAULT '',
  digest TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  settled_at INTEGER NOT NULL DEFAULT 0,
  detail TEXT NOT NULL DEFAULT '',
  delivery_run_id TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS session_message_invocation_target
  ON session_message_invocation(state,target_runtime,target_native_id,target_catalog_id,created_at);
CREATE INDEX IF NOT EXISTS session_message_invocation_caller
  ON session_message_invocation(caller_runtime,caller_native_id,created_at);
CREATE INDEX IF NOT EXISTS session_message_invocation_digest
  ON session_message_invocation(digest,state,created_at);
CREATE INDEX IF NOT EXISTS session_message_invocation_settle
  ON session_message_invocation(state,created_at);
`

type Index struct {
	db *sql.DB
	// ownerKeptSessionsLimit bounds how many owner-tagged sessions the transcript
	// orphan sweep spares (SetOwnerKeptSessionsLimit). Zero spares them all.
	ownerKeptSessionsLimit atomic.Int64
}

// migrationTestHook is nil in production. Focused store tests use it to prove a
// failure after DDL but before the stamp rolls the whole migration back.
var migrationTestHook func(schemaDB) error

// migrate applies changes the schema DDL above CANNOT: `CREATE TABLE IF NOT EXISTS`
// silently does nothing to an existing table, so a renamed column leaves an upgraded
// binary writing to a column that isn't there — every observation failing with a 500
// while the daemon looks healthy. The event log is PRIMARY TRUTH and is never
// rebuilt, so we migrate it in place rather than dropping it. Each step is
// idempotent: safe to run on every open, on any age of database.
type schemaDB interface {
	Exec(string, ...any) (sql.Result, error)
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
}

func migrate(db schemaDB) error {
	version, err := schemaVersion(db)
	if err != nil {
		return err
	}
	cols, err := columnSet(db, "event")
	if err != nil {
		return err
	}
	// event.provenance -> event.origin: the column carries data LINEAGE
	// ("live"|"imported"), and sharing the name `provenance` with the state tables'
	// TRUST enum was a trust-name collision.
	if cols["provenance"] && !cols["origin"] {
		if _, err := db.Exec(`ALTER TABLE event RENAME COLUMN provenance TO origin`); err != nil {
			return fmt.Errorf("migrate event.provenance->origin: %w", err)
		}
	}
	// event.runtime: WHICH agent produced the action. Added as a nullable column, so
	// every event already in the log keeps its row and reads back as UNKNOWN rather
	// than being back-filled with a guess. There is no honest way to infer the
	// runtime of an event recorded before the hook carried it, and a plausible guess
	// in PRIMARY TRUTH is worse than an admitted blank.
	//
	// NOT NULL DEFAULT '' — identical to the CREATE TABLE above, deliberately: a
	// migrated store and a fresh store must have the SAME layout, or v2 means two
	// different things depending on the store's history. (The first version added
	// the column NOT NULL here while the DDL created it nullable — caught in
	// review; both now agree.) The DEFAULT is load-bearing, not tidiness: a bare
	// `ADD COLUMN runtime TEXT` leaves every pre-existing row NULL, and scanning
	// NULL into a Go string fails — every READ of a pre-upgrade session erroring
	// while the daemon reports healthy, the provenance→origin bug moved to the
	// read path.
	if len(cols) > 0 && !cols["runtime"] {
		if _, err := db.Exec(`ALTER TABLE event ADD COLUMN runtime TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("migrate event: add runtime: %w", err)
		}
	} else if cols["runtime"] {
		// Repair a column added nullable by an intermediate build. Probed first so the
		// common case (no NULLs) does not pay for a write on every daemon start.
		var hasNull int
		if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM event WHERE runtime IS NULL)`).Scan(&hasNull); err != nil {
			return fmt.Errorf("migrate event: probe runtime nulls: %w", err)
		}
		if hasNull == 1 {
			if _, err := db.Exec(`UPDATE event SET runtime='' WHERE runtime IS NULL`); err != nil {
				return fmt.Errorf("migrate event: normalise runtime nulls: %w", err)
			}
		}
	}
	if version < 13 {
		if _, err := db.Exec(`
DROP INDEX IF EXISTS event_delivery_native;
ALTER TABLE event_delivery RENAME TO event_delivery_v12;
CREATE TABLE event_delivery(
  event_id INTEGER PRIMARY KEY REFERENCES event(id) ON DELETE RESTRICT,
  observation_id TEXT NOT NULL UNIQUE,
  observation_schema TEXT NOT NULL,
  envelope_digest TEXT NOT NULL,
  collector_id TEXT NOT NULL,
  collector_version TEXT NOT NULL DEFAULT '',
  native_call_id TEXT NOT NULL DEFAULT '',
  native_call_kind TEXT NOT NULL DEFAULT '',
  queued_at INTEGER NOT NULL DEFAULT 0,
  received_at INTEGER NOT NULL,
  delivery_attempts INTEGER NOT NULL CHECK(delivery_attempts >= 1),
  delivery_mode TEXT NOT NULL CHECK(delivery_mode IN ('direct','replay','transcript'))
);
INSERT INTO event_delivery SELECT event_id,observation_id,observation_schema,envelope_digest,
  collector_id,collector_version,native_call_id,native_call_kind,queued_at,received_at,
  delivery_attempts,delivery_mode FROM event_delivery_v12;
DROP TABLE event_delivery_v12;
CREATE INDEX event_delivery_native ON event_delivery(native_call_kind,native_call_id,event_id);`); err != nil {
			return fmt.Errorf("migrate event delivery transcript lineage v13: %w", err)
		}
	}
	if version == 4 {
		if err := migrateUnderstandingCoverageV5(db); err != nil {
			return err
		}
	}
	if err := migrateUnderstandingAnalyzerFactsV18(db, version); err != nil {
		return err
	}
	if err := migrateUnderstandingIdentityV16(db, version); err != nil {
		return err
	}
	resourceCols, err := columnSet(db, "event_resource")
	if err != nil {
		return err
	}
	for _, addition := range []struct {
		name string
		ddl  string
	}{
		{"raw_identity", `ALTER TABLE event_resource ADD COLUMN raw_identity TEXT NOT NULL DEFAULT ''`},
		{"operation", `ALTER TABLE event_resource ADD COLUMN operation TEXT NOT NULL DEFAULT 'unknown' CHECK(operation IN ('read','search','write','patch','execute','connect','use','unknown'))`},
		{"evidence_class", `ALTER TABLE event_resource ADD COLUMN evidence_class TEXT NOT NULL DEFAULT 'unknown' CHECK(evidence_class IN ('declared','observed','unknown'))`},
		{"source_field", `ALTER TABLE event_resource ADD COLUMN source_field TEXT NOT NULL DEFAULT ''`},
		{"completeness", `ALTER TABLE event_resource ADD COLUMN completeness TEXT NOT NULL DEFAULT 'unknown' CHECK(completeness IN ('complete','partial','unresolved','unknown'))`},
	} {
		if len(resourceCols) > 0 && !resourceCols[addition.name] {
			if _, err := db.Exec(addition.ddl); err != nil {
				return fmt.Errorf("migrate event_resource: add %s: %w", addition.name, err)
			}
		}
	}
	// v6 allowed only one association per (event, entity). Exact v1 evidence may
	// legitimately declare the same resource more than once with a different source
	// field or operation, so retain every ordinal by rebuilding only this join table.
	// The event log and entity registry themselves remain untouched.
	if version < 7 {
		if _, err := db.Exec(`
ALTER TABLE event_resource RENAME TO event_resource_v6;
CREATE TABLE event_resource(
  event_id INTEGER NOT NULL REFERENCES event(id) ON DELETE RESTRICT,
  ordinal INTEGER NOT NULL,
  entity_id TEXT NOT NULL REFERENCES entity(id) ON DELETE RESTRICT,
  source TEXT NOT NULL,
  raw_identity TEXT NOT NULL DEFAULT '',
  operation TEXT NOT NULL DEFAULT 'unknown' CHECK(operation IN ('read','search','write','patch','execute','connect','use','unknown')),
  evidence_class TEXT NOT NULL DEFAULT 'unknown' CHECK(evidence_class IN ('declared','observed','unknown')),
  source_field TEXT NOT NULL DEFAULT '',
  completeness TEXT NOT NULL DEFAULT 'unknown' CHECK(completeness IN ('complete','partial','unresolved','unknown')),
  PRIMARY KEY(event_id, ordinal)
);
INSERT INTO event_resource SELECT event_id,ordinal,entity_id,source,raw_identity,operation,evidence_class,source_field,completeness FROM event_resource_v6;
DROP TABLE event_resource_v6;
CREATE INDEX event_resource_entity ON event_resource(entity_id,event_id);`); err != nil {
			return fmt.Errorf("migrate event_resource: retain repeated exact claims: %w", err)
		}
	}
	checkpointCols, err := columnSet(db, "session_checkpoint")
	if err != nil {
		return err
	}
	if len(checkpointCols) > 0 && !checkpointCols["request_id"] {
		if _, err := db.Exec(`
ALTER TABLE session_checkpoint RENAME TO session_checkpoint_v7;
DROP INDEX IF EXISTS session_checkpoint_status;
CREATE TABLE session_checkpoint(
  id INTEGER PRIMARY KEY,
  runtime TEXT NOT NULL DEFAULT '',
  session_id TEXT NOT NULL,
  scope_key TEXT NOT NULL,
  kind TEXT NOT NULL CHECK(kind IN ('attachment','pre-mutation','settled','pre-verification','pre-commit','closing','timeout')),
  request_id TEXT NOT NULL UNIQUE,
  trigger_event_id INTEGER REFERENCES event(id) ON DELETE SET NULL,
  trigger_result_id INTEGER REFERENCES result_observation(id) ON DELETE SET NULL,
  trigger_observation_id TEXT NOT NULL DEFAULT '',
  predecessor_checkpoint_id INTEGER REFERENCES session_checkpoint(id) ON DELETE SET NULL,
  working_directory TEXT NOT NULL,
  repository_id TEXT NOT NULL DEFAULT '',
  checkout_id TEXT NOT NULL DEFAULT '',
  checkout_root TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK(status IN ('pending','capturing','complete','failed','unavailable')),
  boundary_class TEXT NOT NULL CHECK(boundary_class IN ('direct-pre-release','late-replay','unconfirmed','settled','point-in-time','exact-close')),
  requested_at INTEGER NOT NULL,
  capture_started_at INTEGER NOT NULL DEFAULT 0,
  capture_ended_at INTEGER NOT NULL DEFAULT 0,
  capture_attempts INTEGER NOT NULL DEFAULT 0 CHECK(capture_attempts >= 0),
  change_record_id INTEGER REFERENCES change_record(id) ON DELETE RESTRICT,
  failure_kind TEXT NOT NULL DEFAULT '',
  detail_digest TEXT NOT NULL DEFAULT '',
  CHECK((status='complete' AND change_record_id IS NOT NULL AND failure_kind='') OR
        (status IN ('failed','unavailable') AND change_record_id IS NULL AND failure_kind!='') OR
        (status IN ('pending','capturing') AND change_record_id IS NULL))
);
INSERT INTO session_checkpoint(id,runtime,session_id,scope_key,kind,request_id,trigger_event_id,
  trigger_observation_id,working_directory,repository_id,checkout_id,checkout_root,status,
  boundary_class,requested_at,capture_started_at,capture_ended_at,capture_attempts,
  change_record_id,failure_kind,detail_digest)
SELECT id,'',session_id,scope_key,kind,'legacy-attachment-'||id,trigger_event_id,
  trigger_observation_id,working_directory,repository_id,checkout_id,checkout_root,status,
  boundary_class,requested_at,capture_started_at,capture_ended_at,capture_attempts,
  change_record_id,failure_kind,detail_digest FROM session_checkpoint_v7;
DROP TABLE session_checkpoint_v7;
CREATE UNIQUE INDEX session_checkpoint_single_boundary
  ON session_checkpoint(runtime,session_id,scope_key,kind) WHERE kind IN ('attachment','pre-mutation');
CREATE INDEX session_checkpoint_status ON session_checkpoint(status,requested_at,id);`); err != nil {
			return fmt.Errorf("migrate session checkpoints v8: %w", err)
		}
	}
	if err := repairCheckpointDependentForeignKeysV8(db); err != nil {
		return err
	}
	if err := repairCheckpointTriggerForeignKeyV14(db); err != nil {
		return err
	}
	if err := repairCheckpointPayloadBodyForeignKeysV9(db); err != nil {
		return err
	}
	issueCols, err := columnSet(db, "collection_issue")
	if err != nil {
		return err
	}
	if version < 8 && len(issueCols) > 0 {
		if _, err := db.Exec(`
ALTER TABLE collection_issue RENAME TO collection_issue_v7;
CREATE TABLE collection_issue(
  id INTEGER PRIMARY KEY,
  issue_id TEXT NOT NULL UNIQUE,
  session_id TEXT NOT NULL DEFAULT '',
  runtime TEXT NOT NULL DEFAULT '',
  observation_id TEXT NOT NULL DEFAULT '',
  collector_id TEXT NOT NULL,
  kind TEXT NOT NULL CHECK(kind IN ('collision','malformed','checkpoint-failed','result-collision','result-missing','result-unjoined','result-payload-bounded','transcript-unavailable','transcript-changed','checkpoint-payload-bounded','attribution-ambiguous','reconciliation-failed')),
  affected_count INTEGER NOT NULL CHECK(affected_count >= 1),
  first_seen INTEGER NOT NULL,
  last_seen INTEGER NOT NULL,
  detail_digest TEXT NOT NULL DEFAULT '',
  resolved_at INTEGER NOT NULL DEFAULT 0
);
INSERT INTO collection_issue(id,issue_id,session_id,runtime,observation_id,collector_id,
  kind,affected_count,first_seen,last_seen,detail_digest,resolved_at)
SELECT id,issue_id,session_id,runtime,observation_id,collector_id,
  kind,affected_count,first_seen,last_seen,detail_digest,resolved_at FROM collection_issue_v7;
DROP TABLE collection_issue_v7;`); err != nil {
			return fmt.Errorf("migrate collection issues v8: %w", err)
		}
	}
	issueCols, err = columnSet(db, "collection_issue")
	if err != nil {
		return err
	}
	if len(issueCols) > 0 && !issueCols["resolution_class"] {
		if _, err := db.Exec(`ALTER TABLE collection_issue ADD COLUMN resolution_class TEXT NOT NULL DEFAULT ''
			CHECK(resolution_class IN ('','recovered','outside-observation'))`); err != nil {
			return fmt.Errorf("migrate collection issues v11: add resolution class: %w", err)
		}
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS collection_issue_active
		ON collection_issue(kind,resolved_at,id)`); err != nil {
		return fmt.Errorf("migrate collection issues v11: active index: %w", err)
	}
	if version < 11 {
		if _, err := db.Exec(`UPDATE collection_issue SET resolution_class='recovered'
			WHERE resolved_at>0 AND resolution_class='';
		UPDATE collection_issue AS issue
		SET resolved_at=CAST(strftime('%s','now') AS INTEGER),
			resolution_class='outside-observation'
		WHERE issue.kind='result-unjoined' AND issue.resolved_at=0
		AND EXISTS (
			SELECT 1 FROM result_observation result
			JOIN result_reconciliation reconciliation ON reconciliation.result_id=result.id
			WHERE result.observation_id=issue.observation_id
			AND reconciliation.id=(SELECT MAX(current.id) FROM result_reconciliation current
				WHERE current.result_id=result.id)
			AND reconciliation.join_class='unjoined'
			AND result.source_kind!='live-post-tool'
			AND result.completed_at>0
			AND NOT EXISTS (
				SELECT 1 FROM event action JOIN event_delivery delivery ON delivery.event_id=action.id
				WHERE action.origin='live' AND action.session_id=result.session_id
				AND action.runtime=result.runtime AND action.ts<=result.completed_at
			)
		)`); err != nil {
			return fmt.Errorf("migrate collection issues v11: classify outside observation: %w", err)
		}
	}
	if version < 14 && len(issueCols) > 0 {
		if _, err := db.Exec(`
DROP INDEX IF EXISTS collection_issue_active;
ALTER TABLE collection_issue RENAME TO collection_issue_v13;
CREATE TABLE collection_issue(
  id INTEGER PRIMARY KEY,
  issue_id TEXT NOT NULL UNIQUE,
  session_id TEXT NOT NULL DEFAULT '',
  runtime TEXT NOT NULL DEFAULT '',
  observation_id TEXT NOT NULL DEFAULT '',
  native_call_id TEXT NOT NULL DEFAULT '',
  source_ref TEXT NOT NULL DEFAULT '',
  source_segment_id TEXT NOT NULL DEFAULT '',
  collector_id TEXT NOT NULL,
  kind TEXT NOT NULL CHECK(kind IN ('collision','malformed','checkpoint-failed','result-collision','result-missing','result-unjoined','result-payload-bounded','transcript-unavailable','transcript-changed','checkpoint-payload-bounded','attribution-ambiguous','reconciliation-failed','parser-state-evicted')),
  affected_count INTEGER NOT NULL CHECK(affected_count >= 1),
  first_seen INTEGER NOT NULL,
  last_seen INTEGER NOT NULL,
  detail_digest TEXT NOT NULL DEFAULT '',
  resolved_at INTEGER NOT NULL DEFAULT 0,
  resolution_class TEXT NOT NULL DEFAULT '' CHECK(resolution_class IN ('','recovered','outside-observation')),
  CHECK(kind!='parser-state-evicted' OR
    (native_call_id!='' AND source_ref!='' AND source_segment_id!=''))
);
INSERT INTO collection_issue(id,issue_id,session_id,runtime,observation_id,collector_id,
  kind,affected_count,first_seen,last_seen,detail_digest,resolved_at,resolution_class)
SELECT id,issue_id,session_id,runtime,observation_id,collector_id,
  kind,affected_count,first_seen,last_seen,detail_digest,resolved_at,resolution_class
FROM collection_issue_v13;
DROP TABLE collection_issue_v13;
CREATE INDEX collection_issue_active ON collection_issue(kind,resolved_at,id);`); err != nil {
			return fmt.Errorf("migrate collection issue source identity v14: %w", err)
		}
	}
	if version < 15 && len(issueCols) > 0 {
		var invalidPairings int
		if err := db.QueryRow(`SELECT COUNT(*) FROM collection_issue
			WHERE kind='parser-state-evicted'
			AND (native_call_id='' OR source_ref='' OR source_segment_id='')`).Scan(&invalidPairings); err != nil {
			return fmt.Errorf("migrate collection issue pairing v15: inspect rows: %w", err)
		}
		if invalidPairings > 0 {
			if _, err := db.Exec(`UPDATE collection_issue SET kind='malformed'
				WHERE kind='parser-state-evicted'
				AND (native_call_id='' OR source_ref='' OR source_segment_id='')`); err != nil {
				return fmt.Errorf("migrate collection issue pairing v15: classify %d unlinked rows: %w",
					invalidPairings, err)
			}
		}
		if _, err := db.Exec(`
DROP INDEX IF EXISTS collection_issue_active;
ALTER TABLE collection_issue RENAME TO collection_issue_v14;
CREATE TABLE collection_issue(
  id INTEGER PRIMARY KEY,
  issue_id TEXT NOT NULL UNIQUE,
  session_id TEXT NOT NULL DEFAULT '',
  runtime TEXT NOT NULL DEFAULT '',
  observation_id TEXT NOT NULL DEFAULT '',
  native_call_id TEXT NOT NULL DEFAULT '',
  source_ref TEXT NOT NULL DEFAULT '',
  source_segment_id TEXT NOT NULL DEFAULT '',
  collector_id TEXT NOT NULL,
  kind TEXT NOT NULL CHECK(kind IN ('collision','malformed','checkpoint-failed','result-collision','result-missing','result-unjoined','result-payload-bounded','transcript-unavailable','transcript-changed','checkpoint-payload-bounded','attribution-ambiguous','reconciliation-failed','parser-state-evicted')),
  affected_count INTEGER NOT NULL CHECK(affected_count >= 1),
  first_seen INTEGER NOT NULL,
  last_seen INTEGER NOT NULL,
  detail_digest TEXT NOT NULL DEFAULT '',
  resolved_at INTEGER NOT NULL DEFAULT 0,
  resolution_class TEXT NOT NULL DEFAULT '' CHECK(resolution_class IN ('','recovered','outside-observation')),
  CHECK(kind!='parser-state-evicted' OR
    (native_call_id!='' AND source_ref!='' AND source_segment_id!=''))
);
INSERT INTO collection_issue SELECT * FROM collection_issue_v14;
DROP TABLE collection_issue_v14;
CREATE INDEX collection_issue_active ON collection_issue(kind,resolved_at,id);`); err != nil {
			return fmt.Errorf("migrate collection issue pairing v15: %w", err)
		}
	}
	cursorCols, err := columnSet(db, "transcript_cursor")
	if err != nil {
		return err
	}
	for _, addition := range []struct {
		name string
		ddl  string
	}{
		{"session_id", `ALTER TABLE transcript_cursor ADD COLUMN session_id TEXT NOT NULL DEFAULT ''`},
		{"working_directory", `ALTER TABLE transcript_cursor ADD COLUMN working_directory TEXT NOT NULL DEFAULT ''`},
		{"committed_offset", `ALTER TABLE transcript_cursor ADD COLUMN committed_offset INTEGER NOT NULL DEFAULT 0 CHECK(committed_offset >= 0)`},
		{"source_line", `ALTER TABLE transcript_cursor ADD COLUMN source_line INTEGER NOT NULL DEFAULT 0 CHECK(source_line >= 0)`},
		{"parser_state", `ALTER TABLE transcript_cursor ADD COLUMN parser_state BLOB`},
		{"parser_state_digest", `ALTER TABLE transcript_cursor ADD COLUMN parser_state_digest TEXT NOT NULL DEFAULT ''`},
		{"rescan_needed", `ALTER TABLE transcript_cursor ADD COLUMN rescan_needed INTEGER NOT NULL DEFAULT 0 CHECK(rescan_needed IN (0,1))`},
		{"continuation_needed", `ALTER TABLE transcript_cursor ADD COLUMN continuation_needed INTEGER NOT NULL DEFAULT 0 CHECK(continuation_needed IN (0,1))`},
		{"action_parser_version", `ALTER TABLE transcript_cursor ADD COLUMN action_parser_version INTEGER NOT NULL DEFAULT 0 CHECK(action_parser_version >= -2)`},
	} {
		if len(cursorCols) > 0 && !cursorCols[addition.name] {
			if _, err := db.Exec(addition.ddl); err != nil {
				return fmt.Errorf("migrate transcript cursor: add %s: %w", addition.name, err)
			}
		}
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS transcript_cursor_rescan
		ON transcript_cursor(rescan_needed,updated_at,runtime,source_ref)`); err != nil {
		return fmt.Errorf("migrate transcript cursor rescan index: %w", err)
	}
	if version < 13 {
		if _, err := db.Exec(`DROP INDEX IF EXISTS transcript_cursor_action_backfill;
			CREATE INDEX transcript_cursor_action_backfill
			ON transcript_cursor(runtime,action_parser_version,updated_at DESC,source_ref);
			CREATE INDEX IF NOT EXISTS transcript_cursor_action_active
			ON transcript_cursor(updated_at) WHERE action_parser_version=-1;
			CREATE INDEX IF NOT EXISTS result_observation_action_backfill
			ON result_observation(runtime,source_kind,native_call_kind,session_id,source_ref)`); err != nil {
			return fmt.Errorf("migrate transcript action backfill indexes v13: %w", err)
		}
	} else {
		if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS transcript_cursor_action_backfill
			ON transcript_cursor(runtime,action_parser_version,updated_at DESC,source_ref);
			CREATE INDEX IF NOT EXISTS transcript_cursor_action_active
			ON transcript_cursor(updated_at) WHERE action_parser_version=-1;
			CREATE INDEX IF NOT EXISTS result_observation_action_backfill
			ON result_observation(runtime,source_kind,native_call_kind,session_id,source_ref)`); err != nil {
			return fmt.Errorf("ensure transcript action backfill indexes v13: %w", err)
		}
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS event_live_session_runtime_ts
		ON event(origin,session_id,runtime,ts)`); err != nil {
		return fmt.Errorf("migrate live session event index: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS result_reconciliation_ambiguous
		ON result_reconciliation(join_class,result_id,id) WHERE join_class='ambiguous'`); err != nil {
		return fmt.Errorf("migrate ambiguous result index v12: %w", err)
	}
	if err := migrateSessionEntryV17(db); err != nil {
		return err
	}
	if err := migrateRuntimeTasksV20(db, version); err != nil {
		return err
	}
	if err := migrateOrchestrationReviewsV21(db); err != nil {
		return err
	}
	if err := migrateSearchIndexV22(db, version); err != nil {
		return err
	}
	if err := migrateOrchestrationDelegationV23(db); err != nil {
		return err
	}
	if err := migrateOrchestrationAgentsV24(db, version); err != nil {
		return err
	}
	if err := migrateProviderOutageV25(db); err != nil {
		return err
	}
	if err := migrateWorkspacesV26(db); err != nil {
		return err
	}
	if err := migrateHelperSessionV30(db); err != nil {
		return err
	}
	if err := migrateSyncV31(db); err != nil {
		return err
	}
	if err := migrateRuleV33(db); err != nil {
		return err
	}
	if err := migrateUsageRoleV35(db); err != nil {
		return err
	}
	if err := migrateThinkingEffortV37(db); err != nil {
		return err
	}
	if err := migrateLayerV38(db); err != nil {
		return err
	}
	if err := migrateFlowsV39(db); err != nil {
		return err
	}
	if err := migrateOwnerAttentionV40(db); err != nil {
		return err
	}
	if err := migrateFlowMemberExclusionV41(db); err != nil {
		return err
	}
	if err := migrateSessionMessageInvocationV42(db); err != nil {
		return err
	}
	if err := migrateUnderstandingFactsStateV43(db); err != nil {
		return err
	}
	if err := migrateTeamMemoryV44(db); err != nil {
		return err
	}
	if err := migrateUnderstandingShapeV45(db); err != nil {
		return err
	}
	if err := migrateTeamRestOfReleaseV46(db); err != nil {
		return err
	}
	return ensurePathReconciliationIndexes(db)
}

// migrateUnderstandingFactsStateV43 gives understanding_generation the facts_state
// column and narrows the complete-identity unique index to rows that still hold
// their facts, so a re-scan of a pruned snapshot appends a new row instead of
// colliding with history (understanding-facts-retention plan §4.2). Both halves
// probe the layout, so the step runs once: the column by name, the index by its
// stored definition. The v16 step above keeps creating the un-narrowed index on a
// fresh store (IF NOT EXISTS never narrows an existing one); this step owns the swap.
func migrateUnderstandingFactsStateV43(db schemaDB) error {
	cols, err := columnSet(db, "understanding_generation")
	if err != nil {
		return err
	}
	if len(cols) == 0 {
		return nil
	}
	if !cols["facts_state"] {
		if _, err := db.Exec(`ALTER TABLE understanding_generation ADD COLUMN facts_state TEXT NOT NULL DEFAULT 'present' CHECK(facts_state IN ('present','pruned'))`); err != nil {
			return fmt.Errorf("migrate understanding facts state v43: add column: %w", err)
		}
	}
	var indexSQL string
	err = db.QueryRow(`SELECT COALESCE(sql,'') FROM sqlite_master WHERE type='index' AND name='understanding_generation_complete_identity'`).Scan(&indexSQL)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("migrate understanding facts state v43: inspect identity index: %w", err)
	}
	if strings.Contains(indexSQL, "facts_state") {
		return nil
	}
	if _, err := db.Exec(`DROP INDEX IF EXISTS understanding_generation_complete_identity;
CREATE UNIQUE INDEX understanding_generation_complete_identity
  ON understanding_generation(repository_id,checkout_id,snapshot_digest,structural_schema,
    analyzer_bundle_digest,convention_state,convention_source_digest)
  WHERE status='complete' AND facts_state='present'`); err != nil {
		return fmt.Errorf("migrate understanding facts state v43: narrow identity index: %w", err)
	}
	return nil
}

// ensurePathReconciliationIndexes gives path_reconciliation its two lookup
// indexes: the per-path supersede lookup, and the per-session console reads.
// It is the last step of migrate, not part of the schema string, because
// repairCheckpointDependentForeignKeysV8 rebuilds the table and drops whatever
// indexes it had. Additive and idempotent, so it runs on every Open and does not
// bump SchemaVersion (observe-hook-latency plan §15).
func ensurePathReconciliationIndexes(db schemaDB) error {
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS path_reconciliation_current
		ON path_reconciliation(current_checkpoint_id,path,algorithm,id);
CREATE INDEX IF NOT EXISTS path_reconciliation_session
		ON path_reconciliation(session_id,reconciled_at,id)`); err != nil {
		return fmt.Errorf("ensure path reconciliation indexes: %w", err)
	}
	return nil
}

// migrateHelperSessionV30 gives an existing orchestration_group table the
// helper-session and pending-slot columns (helper-persistent-session plan
// D2). Additive ADD COLUMN with defaults — SQLite never rebuilds, the FK
// children are untouched — guarded by the probe-the-column discipline so it
// is idempotent; a store without the table (never ran the orchestration
// layer) takes the base DDL instead.
func migrateHelperSessionV30(db schemaDB) error {
	cols, err := columnSet(db, "orchestration_group")
	if err != nil {
		return err
	}
	if len(cols) == 0 {
		return nil
	}
	for _, addition := range []struct {
		name string
		ddl  string
	}{
		{"helper_runtime", `ALTER TABLE orchestration_group ADD COLUMN helper_runtime TEXT NOT NULL DEFAULT ''`},
		{"helper_native_session_id", `ALTER TABLE orchestration_group ADD COLUMN helper_native_session_id TEXT NOT NULL DEFAULT ''`},
		{"helper_turns", `ALTER TABLE orchestration_group ADD COLUMN helper_turns INTEGER NOT NULL DEFAULT 0`},
		{"helper_session_replaced", `ALTER TABLE orchestration_group ADD COLUMN helper_session_replaced INTEGER NOT NULL DEFAULT 0`},
		{"helper_transcript_seq", `ALTER TABLE orchestration_group ADD COLUMN helper_transcript_seq INTEGER NOT NULL DEFAULT 0`},
		{"pending_signal_json", `ALTER TABLE orchestration_group ADD COLUMN pending_signal_json TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(pending_signal_json))`},
		{"pending_producer", `ALTER TABLE orchestration_group ADD COLUMN pending_producer TEXT NOT NULL DEFAULT ''`},
		{"pending_event_id", `ALTER TABLE orchestration_group ADD COLUMN pending_event_id INTEGER NOT NULL DEFAULT 0`},
		{"pending_coalesced", `ALTER TABLE orchestration_group ADD COLUMN pending_coalesced INTEGER NOT NULL DEFAULT 0`},
		{"pending_at", `ALTER TABLE orchestration_group ADD COLUMN pending_at INTEGER NOT NULL DEFAULT 0`},
		{"pending_dropped", `ALTER TABLE orchestration_group ADD COLUMN pending_dropped INTEGER NOT NULL DEFAULT 0`},
		{"pending_dropped_reason", `ALTER TABLE orchestration_group ADD COLUMN pending_dropped_reason TEXT NOT NULL DEFAULT ''`},
	} {
		if cols[addition.name] {
			continue
		}
		if _, err := db.Exec(addition.ddl); err != nil {
			return fmt.Errorf("migrate helper session v30: %s: %w", addition.name, err)
		}
	}
	// The helper-id index lives here, not in the base DDL: the base DDL runs
	// before this migration on every open, and on a v29 store the column it
	// indexes does not exist yet.
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS orchestration_group_identity
  ON orchestration_group(binding_id,root_native_session_id,root_catalog_session_id);
CREATE INDEX IF NOT EXISTS orchestration_group_helper ON orchestration_group(helper_native_session_id)`); err != nil {
		return fmt.Errorf("migrate helper session v30: index: %w", err)
	}
	return nil
}

// migrateProviderOutageV25 gives existing stores the provider-outage shape
// (provider-outage plan): `parked` joins the run-state CHECK — a DDL change,
// so the run table and its FK children (relationship, control, tag) are
// rebuilt with the accepted rename/recreate/copy pattern; renaming the parent
// drags the children's stored FK clauses onto the _v24 name (the v23→v24
// repair lesson), which is why the children rebuild in the same pass — and
// bindings gain the routes_json fallback chain via the probe-the-column
// repair discipline. Probe-based and unconditional, so it is idempotent and
// harmless on fresh stores whose DDL already carries both.
func migrateProviderOutageV25(db schemaDB) error {
	var hasParked int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master
		WHERE type='table' AND name='orchestration_managed_run'
		AND instr(sql,'parked') > 0`).Scan(&hasParked); err != nil {
		return fmt.Errorf("probe orchestration run v25: %w", err)
	}
	if hasParked == 0 {
		if _, err := db.Exec(`
ALTER TABLE orchestration_managed_run RENAME TO orchestration_managed_run_v24;
ALTER TABLE orchestration_relationship RENAME TO orchestration_relationship_v24;
ALTER TABLE orchestration_control RENAME TO orchestration_control_v24;
ALTER TABLE orchestration_tag RENAME TO orchestration_tag_v24;
DROP INDEX IF EXISTS orchestration_managed_run_group;
DROP INDEX IF EXISTS orchestration_managed_run_child;
DROP INDEX IF EXISTS orchestration_tag_session;`); err != nil {
			return fmt.Errorf("migrate provider outage v25: rename: %w", err)
		}
		if _, err := db.Exec(orchestrationManagedSchemaV25); err != nil {
			return fmt.Errorf("migrate provider outage v25: recreate: %w", err)
		}
		if _, err := db.Exec(`
INSERT INTO orchestration_managed_run(run_id,idempotency_key,group_id,binding_id,binding_state_token,role,kind,
  profile_id,profile_source_digest,profile_bundle_digest,source_task_id,source_event_id,child_task_id,
  state,action,message,citations_json,detail_json,error_class,recovery,admitted_at,started_at,completed_at)
SELECT run_id,idempotency_key,group_id,binding_id,binding_state_token,role,kind,
  profile_id,profile_source_digest,profile_bundle_digest,source_task_id,source_event_id,child_task_id,
  state,action,message,citations_json,detail_json,error_class,recovery,admitted_at,started_at,completed_at
FROM orchestration_managed_run_v24;
INSERT INTO orchestration_relationship SELECT * FROM orchestration_relationship_v24;
INSERT INTO orchestration_control SELECT * FROM orchestration_control_v24;
INSERT INTO orchestration_tag SELECT * FROM orchestration_tag_v24;
DROP TABLE orchestration_tag_v24;
DROP TABLE orchestration_control_v24;
DROP TABLE orchestration_relationship_v24;
DROP TABLE orchestration_managed_run_v24;`); err != nil {
			return fmt.Errorf("migrate provider outage v25: copy: %w", err)
		}
	}
	bindingCols, err := columnSet(db, "orchestration_managed_binding")
	if err != nil {
		return err
	}
	if len(bindingCols) > 0 && !bindingCols["routes_json"] {
		if _, err := db.Exec(`ALTER TABLE orchestration_managed_binding ADD COLUMN routes_json TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(routes_json));`); err != nil {
			return fmt.Errorf("repair orchestration binding routes_json: %w", err)
		}
	}
	return nil
}

func migrateOrchestrationDelegationV23(db schemaDB) error {
	cols, err := columnSet(db, "orchestration_review_binding")
	if err != nil {
		return err
	}
	if len(cols) > 0 && !cols["effect"] {
		if _, err := db.Exec(`ALTER TABLE orchestration_review_binding ADD COLUMN effect TEXT NOT NULL DEFAULT 'report-only' CHECK(effect IN ('report-only','delegated-first'))`); err != nil {
			return fmt.Errorf("migrate orchestration delegation v23: add effect: %w", err)
		}
	}
	if len(cols) > 0 && !cols["approval_subdeadline_ms"] {
		if _, err := db.Exec(`ALTER TABLE orchestration_review_binding ADD COLUMN approval_subdeadline_ms INTEGER NOT NULL DEFAULT 0 CHECK(approval_subdeadline_ms BETWEEN 0 AND 120000)`); err != nil {
			return fmt.Errorf("migrate orchestration delegation v23: add subdeadline: %w", err)
		}
	}
	if len(cols) > 0 && !cols["answer_choice_prompts"] {
		if _, err := db.Exec(`ALTER TABLE orchestration_review_binding ADD COLUMN answer_choice_prompts INTEGER NOT NULL DEFAULT 1 CHECK(answer_choice_prompts IN (0,1))`); err != nil {
			return fmt.Errorf("migrate orchestration choice selection v28: add answer_choice_prompts: %w", err)
		}
	}
	invocationCols, err := columnSet(db, "orchestration_review_invocation")
	if err != nil {
		return err
	}
	for _, addition := range []struct{ name, ddl string }{
		{"approval_id", `ALTER TABLE orchestration_review_invocation ADD COLUMN approval_id TEXT NOT NULL DEFAULT ''`},
		{"approval_response_id", `ALTER TABLE orchestration_review_invocation ADD COLUMN approval_response_id TEXT NOT NULL DEFAULT ''`},
		{"approval_outcome", `ALTER TABLE orchestration_review_invocation ADD COLUMN approval_outcome TEXT NOT NULL DEFAULT ''`},
		{"selections_json", `ALTER TABLE orchestration_review_invocation ADD COLUMN selections_json TEXT NOT NULL DEFAULT ''`},
	} {
		if len(invocationCols) > 0 && !invocationCols[addition.name] {
			if _, err := db.Exec(addition.ddl); err != nil {
				return fmt.Errorf("migrate orchestration delegation v23: add %s: %w", addition.name, err)
			}
		}
	}
	if _, err := db.Exec(orchestrationManagedSchemaV25); err != nil {
		return fmt.Errorf("migrate orchestration managed work v23: %w", err)
	}
	return nil
}

// migrateOrchestrationAgentsV24 rebuilds the managed family for the agents
// redesign: the binding role vocabulary becomes reviewer/follower/helper (a
// CHECK change, so the FK-parent tables are rebuilt with the accepted
// rename/recreate/copy pattern and children dropped first — the v7->v8 FK
// rewrite lesson), the run states gain `deferred`, relationships gain reply/turn
// anchors and a cycle counter, and the ambiguous orchestration_cursor table
// becomes orchestration_stream_position. Historical rows stay, but their role
// labels map onto the new taxonomy so no pre-redesign vocabulary survives on a
// live surface: bindings map deterministically (the legacy coordinator and
// course-corrector labels become helper; a follower granted `reply` becomes
// helper), and run/relationship rows map every legacy acting label to helper
// while follower maps to itself. Probe-based so it is idempotent and harmless
// on fresh stores.
func migrateOrchestrationAgentsV24(db schemaDB, version int) error {
	if version < 24 {
		if err := migrateOrchestrationAgentsFamilyV24(db); err != nil {
			return err
		}
	}
	// Repair pass (runs regardless of version, like the v8 FK repair): the v23
	// step pre-creates orchestration_tag/orchestration_group_note from the v24
	// DDL, so the family rename above drags their FK clauses onto the _v23
	// names that are then dropped — every insert fails with "no such table
	// main.orchestration_managed_run_v23" (found live 2026-08-29). Rebuild any
	// aux table whose stored SQL references a _v23 name.
	for _, repair := range []struct{ table, ddl string }{
		{"orchestration_tag", `CREATE TABLE orchestration_tag(
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
)`},
		{"orchestration_group_note", `CREATE TABLE orchestration_group_note(
  note_id TEXT PRIMARY KEY,
  group_id TEXT NOT NULL REFERENCES orchestration_group(group_id) ON DELETE RESTRICT,
  body TEXT NOT NULL CHECK(length(body) BETWEEN 1 AND 4000),
  created_at INTEGER NOT NULL,
  retracted_at INTEGER NOT NULL DEFAULT 0
)`},
	} {
		var stale int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=? AND instr(sql,'_v23') > 0`, repair.table).Scan(&stale); err != nil {
			return fmt.Errorf("probe %s fk repair: %w", repair.table, err)
		}
		if stale == 0 {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE ` + repair.table + ` RENAME TO ` + repair.table + `_fkstale;
` + repair.ddl + `;
INSERT INTO ` + repair.table + ` SELECT * FROM ` + repair.table + `_fkstale;
DROP TABLE ` + repair.table + `_fkstale;`); err != nil {
			return fmt.Errorf("repair %s foreign keys: %w", repair.table, err)
		}
	}
	// Kind repair (unconditional — it MUST live here, outside the version
	// guard: the installed store was stamped 24 before the kind column
	// existed, and a guarded repair skips exactly that store; found live
	// 2026-08-30, the v7→v8/_v23 lesson a third time). The redesign briefly
	// overloaded run.role with synthetic kinds ('reply'/'correction'/
	// 'delegate') — the vocabulary the migration promised to erase. The typed
	// kind column carries that axis; existing rows backfill and their roles
	// normalize to the taxonomy. Fresh v24 rebuilds create the column in the
	// DDL, so the probe no-ops there.
	runCols, err := columnSet(db, "orchestration_managed_run")
	if err != nil {
		return err
	}
	if len(runCols) > 0 && !runCols["kind"] {
		if _, err := db.Exec(`ALTER TABLE orchestration_managed_run ADD COLUMN kind TEXT NOT NULL DEFAULT '' CHECK(kind IN ('','reply','correction','delegate'));
UPDATE orchestration_managed_run SET kind = role WHERE role IN ('reply','correction','delegate');
UPDATE orchestration_managed_run SET role = 'helper' WHERE role IN ('reply','correction','delegate');
UPDATE orchestration_relationship SET role = 'helper' WHERE role IN ('reply','correction','delegate');`); err != nil {
			return fmt.Errorf("repair orchestration run kind: %w", err)
		}
	}
	// WatchNatural repair (unconditional, same probe-the-column discipline —
	// the kind lesson; natural-session plan Slice B): stores stamped 24
	// before this column exists must gain it on open, defaulting every
	// existing binding to consent OFF. Fresh DDL creates the column, so the
	// probe no-ops there.
	bindingCols, err := columnSet(db, "orchestration_managed_binding")
	if err != nil {
		return err
	}
	if len(bindingCols) > 0 && !bindingCols["watch_natural"] {
		if _, err := db.Exec(`ALTER TABLE orchestration_managed_binding ADD COLUMN watch_natural INTEGER NOT NULL DEFAULT 0 CHECK(watch_natural IN (0,1));`); err != nil {
			return fmt.Errorf("repair orchestration binding watch_natural: %w", err)
		}
	}
	var oldCursor int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='orchestration_cursor'`).Scan(&oldCursor); err != nil {
		return fmt.Errorf("probe orchestration cursor v24: %w", err)
	}
	if oldCursor > 0 {
		if _, err := db.Exec(`
INSERT INTO orchestration_stream_position(source_kind,position,updated_at)
SELECT source_kind,cursor,updated_at FROM orchestration_cursor WHERE true
ON CONFLICT(source_kind) DO NOTHING;
DROP TABLE orchestration_cursor;`); err != nil {
			return fmt.Errorf("migrate orchestration cursor v24: %w", err)
		}
	}
	return nil
}

func migrateOrchestrationAgentsFamilyV24(db schemaDB) error {
	var oldShape int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master
		WHERE type='table' AND name='orchestration_managed_binding'
		AND instr(sql,'coordinator') > 0`).Scan(&oldShape); err != nil {
		return fmt.Errorf("probe orchestration agents v24: %w", err)
	}
	if oldShape > 0 {
		if _, err := db.Exec(`
ALTER TABLE orchestration_managed_binding RENAME TO orchestration_managed_binding_v23;
ALTER TABLE orchestration_group RENAME TO orchestration_group_v23;
ALTER TABLE orchestration_managed_run RENAME TO orchestration_managed_run_v23;
ALTER TABLE orchestration_relationship RENAME TO orchestration_relationship_v23;
ALTER TABLE orchestration_control RENAME TO orchestration_control_v23;
DROP INDEX IF EXISTS orchestration_managed_binding_scope;
DROP INDEX IF EXISTS orchestration_group_root;
DROP INDEX IF EXISTS orchestration_managed_run_group;
DROP INDEX IF EXISTS orchestration_managed_run_child;`); err != nil {
			return fmt.Errorf("migrate orchestration agents v24: rename: %w", err)
		}
		if _, err := db.Exec(orchestrationManagedSchemaV25); err != nil {
			return fmt.Errorf("migrate orchestration agents v24: recreate: %w", err)
		}
		if _, err := db.Exec(`
INSERT INTO orchestration_managed_binding(binding_id,state,role,priority,scope_runtime,scope_session,project_root,
  profile_id,profile_source_digest,profile_bundle_digest,runtime,model,mode,authority_json,allowed_profiles_json,
  declared_tags_json,limits_json,auto_action,state_token,created_at,updated_at)
SELECT binding_id,state,
  CASE WHEN role IN ('coordinator','course-corrector') THEN 'helper'
       WHEN role='follower' AND instr(authority_json,'"reply"') > 0 THEN 'helper'
       ELSE 'follower' END,
  0,scope_runtime,scope_session,project_root,profile_id,profile_source_digest,profile_bundle_digest,
  runtime,model,mode,authority_json,allowed_profiles_json,'[]','{}',auto_action,state_token,created_at,updated_at
FROM orchestration_managed_binding_v23;
INSERT INTO orchestration_group(group_id,binding_id,state,root_task_id,root_runtime,root_catalog_session_id,root_native_session_id,project_root,created_at,updated_at) SELECT group_id,binding_id,state,root_task_id,root_runtime,root_catalog_session_id,root_native_session_id,project_root,created_at,updated_at FROM orchestration_group_v23;
INSERT INTO orchestration_managed_run(run_id,idempotency_key,group_id,binding_id,binding_state_token,role,
  profile_id,profile_source_digest,profile_bundle_digest,source_task_id,source_event_id,child_task_id,
  state,action,message,citations_json,detail_json,error_class,recovery,admitted_at,started_at,completed_at)
SELECT run_id,idempotency_key,group_id,binding_id,binding_state_token,
  CASE role WHEN 'coordinator' THEN 'helper' WHEN 'course-corrector' THEN 'helper' WHEN 'reply' THEN 'helper'
       WHEN 'delegate' THEN 'helper' WHEN 'correction' THEN 'helper' ELSE role END,
  profile_id,profile_source_digest,profile_bundle_digest,source_task_id,source_event_id,child_task_id,
  state,action,message,citations_json,detail_json,error_class,recovery,admitted_at,started_at,completed_at
FROM orchestration_managed_run_v23;
INSERT INTO orchestration_relationship(relationship_id,group_id,run_id,parent_task_id,child_task_id,
  reply_task_id,source_turn_anchor,reply_turn_anchor,cycle,role,depth,hops,state,created_at,updated_at)
SELECT relationship_id,group_id,run_id,parent_task_id,child_task_id,'','','',0,
  CASE role WHEN 'coordinator' THEN 'helper' WHEN 'course-corrector' THEN 'helper' WHEN 'reply' THEN 'helper'
       WHEN 'delegate' THEN 'helper' WHEN 'correction' THEN 'helper' ELSE role END,
  depth,hops,state,created_at,updated_at
FROM orchestration_relationship_v23;
INSERT INTO orchestration_control SELECT * FROM orchestration_control_v23;
DROP TABLE orchestration_control_v23;
DROP TABLE orchestration_relationship_v23;
DROP TABLE orchestration_managed_run_v23;
DROP TABLE orchestration_group_v23;
DROP TABLE orchestration_managed_binding_v23;`); err != nil {
			return fmt.Errorf("migrate orchestration agents v24: copy: %w", err)
		}
	}
	return nil
}

func migrateOrchestrationReviewsV21(db schemaDB) error {
	cols, err := columnSet(db, "event_delivery")
	if err != nil {
		return err
	}
	if len(cols) > 0 && !cols["action_id"] {
		if _, err := db.Exec(`ALTER TABLE event_delivery ADD COLUMN action_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("migrate action identity v21: add event delivery action id: %w", err)
		}
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS event_delivery_action
		ON event_delivery(action_id,event_id) WHERE action_id!=''`); err != nil {
		return fmt.Errorf("migrate action identity v21: create action index: %w", err)
	}
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS orchestration_review_binding(
  binding_id TEXT PRIMARY KEY,
  state TEXT NOT NULL CHECK(state IN ('enabled','disabled')),
  effect TEXT NOT NULL DEFAULT 'report-only' CHECK(effect IN ('report-only','delegated-first')),
  runtime_filter TEXT NOT NULL DEFAULT '',
  profile_id TEXT NOT NULL,
  profile_source_digest TEXT NOT NULL,
  profile_bundle_digest TEXT NOT NULL,
  instruction_digest TEXT NOT NULL,
  request_path_kind TEXT NOT NULL,
  request_path_digest TEXT NOT NULL,
  endpoint TEXT NOT NULL,
  model TEXT NOT NULL,
  timeout_ms INTEGER NOT NULL CHECK(timeout_ms BETWEEN 1 AND 120000),
  approval_subdeadline_ms INTEGER NOT NULL DEFAULT 0 CHECK(approval_subdeadline_ms BETWEEN 0 AND 120000),
  answer_choice_prompts INTEGER NOT NULL DEFAULT 1 CHECK(answer_choice_prompts IN (0,1)),
  max_input_bytes INTEGER NOT NULL CHECK(max_input_bytes > 0),
  max_output_bytes INTEGER NOT NULL CHECK(max_output_bytes > 0),
  max_tokens INTEGER NOT NULL CHECK(max_tokens > 0),
  max_concurrency INTEGER NOT NULL CHECK(max_concurrency BETWEEN 1 AND 64),
  state_token TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS orchestration_review_invocation(
  invocation_id TEXT PRIMARY KEY,
  action_id TEXT NOT NULL UNIQUE,
  action_digest TEXT NOT NULL,
  observation_id TEXT NOT NULL,
  event_id INTEGER NOT NULL,
  runtime TEXT NOT NULL DEFAULT '',
  session_id TEXT NOT NULL,
  native_call_id TEXT NOT NULL DEFAULT '',
  native_call_kind TEXT NOT NULL DEFAULT '',
  tool TEXT NOT NULL,
  binding_id TEXT NOT NULL,
  binding_state_token TEXT NOT NULL,
  profile_id TEXT NOT NULL,
  profile_source_digest TEXT NOT NULL,
  profile_bundle_digest TEXT NOT NULL,
  instruction_digest TEXT NOT NULL,
  request_path_kind TEXT NOT NULL,
  request_path_digest TEXT NOT NULL,
  endpoint TEXT NOT NULL,
  model TEXT NOT NULL,
  timeout_ms INTEGER NOT NULL CHECK(timeout_ms > 0),
  max_input_bytes INTEGER NOT NULL CHECK(max_input_bytes > 0),
  max_output_bytes INTEGER NOT NULL CHECK(max_output_bytes > 0),
  max_tokens INTEGER NOT NULL CHECK(max_tokens > 0),
  max_concurrency INTEGER NOT NULL CHECK(max_concurrency > 0),
  state TEXT NOT NULL CHECK(state IN ('admitted','running','completed','unavailable','timed_out','malformed','unknown','suppressed')),
  admitted_at INTEGER NOT NULL,
  started_at INTEGER NOT NULL DEFAULT 0,
  completed_at INTEGER NOT NULL DEFAULT 0,
  duration_ms INTEGER NOT NULL DEFAULT 0 CHECK(duration_ms >= 0),
  decision TEXT NOT NULL DEFAULT '' CHECK(decision IN ('','allow','deny','abstain')),
  message TEXT NOT NULL DEFAULT '' CHECK(length(message) <= 262144),
  citations_json TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(citations_json)),
  request_bytes INTEGER NOT NULL DEFAULT 0 CHECK(request_bytes >= 0),
  response_bytes INTEGER NOT NULL DEFAULT 0 CHECK(response_bytes >= 0),
  prompt_tokens INTEGER NOT NULL DEFAULT 0 CHECK(prompt_tokens >= 0),
  completion_tokens INTEGER NOT NULL DEFAULT 0 CHECK(completion_tokens >= 0),
  error_class TEXT NOT NULL DEFAULT '',
  recovery TEXT NOT NULL DEFAULT '' CHECK(length(recovery) <= 2000),
  timing_class TEXT NOT NULL DEFAULT 'result_not_observed_at_completion' CHECK(timing_class IN
    ('result_not_observed_at_completion','completed_before_exact_result_observation',
     'completed_after_exact_result_observation','exact_result_timing_unknown')),
  approval_id TEXT NOT NULL DEFAULT '',
  approval_response_id TEXT NOT NULL DEFAULT '',
  approval_outcome TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS orchestration_review_history
  ON orchestration_review_invocation(admitted_at DESC,invocation_id DESC);
CREATE INDEX IF NOT EXISTS orchestration_review_session
  ON orchestration_review_invocation(runtime,session_id,admitted_at DESC,invocation_id DESC);
CREATE INDEX IF NOT EXISTS orchestration_review_active
  ON orchestration_review_invocation(binding_id,profile_id,profile_bundle_digest,state);`); err != nil {
		return fmt.Errorf("migrate orchestration reviews v21: %w", err)
	}
	return nil
}

func migrateSessionEntryV17(db schemaDB) error {
	checkpointCols, err := columnSet(db, "session_checkpoint")
	if err != nil {
		return err
	}
	if len(checkpointCols) > 0 && !checkpointCols["runtime"] {
		if _, err := db.Exec(`ALTER TABLE session_checkpoint ADD COLUMN runtime TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("migrate session entry v17: add checkpoint runtime: %w", err)
		}
	}
	if _, err := db.Exec(`UPDATE session_checkpoint SET runtime=COALESCE(
		NULLIF((SELECT runtime FROM event WHERE id=session_checkpoint.trigger_event_id),''),
		NULLIF((SELECT runtime FROM result_observation WHERE id=session_checkpoint.trigger_result_id),''),
		NULLIF((SELECT session_runtime_claim FROM change_record WHERE id=session_checkpoint.change_record_id),''),
		'') WHERE runtime=''`); err != nil {
		return fmt.Errorf("migrate session entry v17: derive checkpoint runtime: %w", err)
	}
	if _, err := db.Exec(`DROP INDEX IF EXISTS session_checkpoint_single_boundary;
		CREATE UNIQUE INDEX session_checkpoint_single_boundary
		ON session_checkpoint(runtime,session_id,scope_key,kind)
		WHERE kind IN ('attachment','pre-mutation')`); err != nil {
		return fmt.Errorf("migrate session entry v17: key checkpoint runtime: %w", err)
	}
	return nil
}

// repairCheckpointDependentForeignKeysV8 fixes the migration-order edge case where
// SQLite followed the v7 parent-table rename and rewrote new dependent foreign keys to
// the temporary session_checkpoint_v7 name. It also repairs stores already stamped v8.
// Rows are copied before the stale definitions are removed; Open's migration transaction
// makes the replacement atomic.
func repairCheckpointDependentForeignKeysV8(db schemaDB) error {
	var stale int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master
		WHERE type='table' AND name IN ('checkpoint_payload','path_reconciliation')
		AND instr(sql,'session_checkpoint_v7') > 0`).Scan(&stale); err != nil {
		return fmt.Errorf("probe checkpoint dependent foreign keys v8: %w", err)
	}
	if stale == 0 {
		return nil
	}
	if _, err := db.Exec(`
ALTER TABLE checkpoint_payload RENAME TO checkpoint_payload_v8_stale;
CREATE TABLE checkpoint_payload(
  checkpoint_id INTEGER NOT NULL REFERENCES session_checkpoint(id) ON DELETE RESTRICT,
  ordinal INTEGER NOT NULL,
  path TEXT NOT NULL,
  layer TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT '',
  content_bytes INTEGER NOT NULL DEFAULT 0 CHECK(content_bytes >= 0),
  content_digest TEXT NOT NULL DEFAULT '',
  content_completeness TEXT NOT NULL CHECK(content_completeness IN ('complete','metadata-only','unavailable')),
  content_payload BLOB,
  patch_bytes INTEGER NOT NULL DEFAULT 0 CHECK(patch_bytes >= 0),
  patch_digest TEXT NOT NULL DEFAULT '',
  patch_completeness TEXT NOT NULL CHECK(patch_completeness IN ('complete','metadata-only','unavailable')),
  patch_payload BLOB,
  source_identity TEXT NOT NULL DEFAULT '',
  limitation TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(checkpoint_id,ordinal),
  CHECK(content_payload IS NULL OR length(content_payload)=content_bytes),
  CHECK(patch_payload IS NULL OR length(patch_payload)=patch_bytes)
);
INSERT INTO checkpoint_payload SELECT * FROM checkpoint_payload_v8_stale;
DROP TABLE checkpoint_payload_v8_stale;

ALTER TABLE path_reconciliation RENAME TO path_reconciliation_v8_stale;
CREATE TABLE path_reconciliation(
  id INTEGER PRIMARY KEY,
  session_id TEXT NOT NULL,
  checkout_id TEXT NOT NULL,
  predecessor_checkpoint_id INTEGER REFERENCES session_checkpoint(id) ON DELETE RESTRICT,
  current_checkpoint_id INTEGER NOT NULL REFERENCES session_checkpoint(id) ON DELETE RESTRICT,
  event_id INTEGER REFERENCES event(id) ON DELETE SET NULL,
  result_id INTEGER REFERENCES result_observation(id) ON DELETE SET NULL,
  path TEXT NOT NULL,
  old_path TEXT NOT NULL DEFAULT '',
  classification TEXT NOT NULL CHECK(classification IN ('inherited','native-effect-git-matched','action-reported','interval-associated','session-associated','no-net-change-at-checkpoint','unattributed','ambiguous','unavailable')),
  competing_actor TEXT NOT NULL DEFAULT '',
  runtime_effect_digest TEXT NOT NULL DEFAULT '',
  git_effect_digest TEXT NOT NULL DEFAULT '',
  algorithm TEXT NOT NULL,
  reconciled_at INTEGER NOT NULL,
  supersedes_id INTEGER REFERENCES path_reconciliation(id) ON DELETE RESTRICT
);
INSERT INTO path_reconciliation SELECT * FROM path_reconciliation_v8_stale;
DROP TABLE path_reconciliation_v8_stale;`); err != nil {
		return fmt.Errorf("repair checkpoint dependent foreign keys v8: %w", err)
	}
	return nil
}

func repairCheckpointPayloadBodyForeignKeysV9(db schemaDB) error {
	var stale int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table'
		AND name='checkpoint_payload_body'
		AND instr(sql,'checkpoint_payload_v8_stale') > 0`).Scan(&stale); err != nil {
		return fmt.Errorf("probe checkpoint body foreign keys v9: %w", err)
	}
	if stale == 0 {
		return nil
	}
	if _, err := db.Exec(`
ALTER TABLE checkpoint_payload_body RENAME TO checkpoint_payload_body_v9_stale;
CREATE TABLE checkpoint_payload_body(
  checkpoint_id INTEGER NOT NULL,
  ordinal INTEGER NOT NULL,
  kind TEXT NOT NULL CHECK(kind IN ('content','patch')),
  body_digest TEXT NOT NULL REFERENCES evidence_body(digest) ON DELETE RESTRICT,
  PRIMARY KEY(checkpoint_id,ordinal,kind),
  FOREIGN KEY(checkpoint_id,ordinal) REFERENCES checkpoint_payload(checkpoint_id,ordinal) ON DELETE RESTRICT
);
INSERT INTO checkpoint_payload_body SELECT * FROM checkpoint_payload_body_v9_stale;
DROP TABLE checkpoint_payload_body_v9_stale;`); err != nil {
		return fmt.Errorf("repair checkpoint body foreign keys v9: %w", err)
	}
	return nil
}

// repairCheckpointTriggerForeignKeyV14 closes the one v8 dependent-table gap: the
// parent checkpoint table was rebuilt, SQLite followed its temporary rename, and
// this trigger relation could be left pointing at the dropped temporary table.
func repairCheckpointTriggerForeignKeyV14(db schemaDB) error {
	var stale int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table'
		AND name='session_checkpoint_trigger'
		AND instr(sql,'session_checkpoint_v7') > 0`).Scan(&stale); err != nil {
		return fmt.Errorf("probe checkpoint trigger foreign key v14: %w", err)
	}
	if stale == 0 {
		return nil
	}
	if _, err := db.Exec(`
ALTER TABLE session_checkpoint_trigger RENAME TO session_checkpoint_trigger_v14_stale;
CREATE TABLE session_checkpoint_trigger(
  checkpoint_id INTEGER NOT NULL REFERENCES session_checkpoint(id) ON DELETE RESTRICT,
  result_id INTEGER NOT NULL REFERENCES result_observation(id) ON DELETE RESTRICT,
  observation_id TEXT NOT NULL,
  accepted_at INTEGER NOT NULL,
  PRIMARY KEY(checkpoint_id,result_id)
);
INSERT INTO session_checkpoint_trigger SELECT * FROM session_checkpoint_trigger_v14_stale;
DROP TABLE session_checkpoint_trigger_v14_stale;`); err != nil {
		return fmt.Errorf("repair checkpoint trigger foreign key v14: %w", err)
	}
	return nil
}

// migrateUnderstandingCoverageV5 expands a checked framework vocabulary. SQLite
// cannot alter a CHECK constraint in place, so the v4 table is rebuilt inside
// Open's surrounding transaction. Rows and the complete-generation trigger are
// copied/recreated before the version stamp is allowed to advance.
func migrateUnderstandingCoverageV5(db schemaDB) error {
	_, err := db.Exec(`
DROP TRIGGER IF EXISTS understanding_coverage_complete;
ALTER TABLE understanding_coverage RENAME TO understanding_coverage_v4;
CREATE TABLE understanding_coverage(
  generation_id INTEGER NOT NULL REFERENCES understanding_generation(id) ON DELETE RESTRICT,
  family TEXT NOT NULL CHECK(family IN (
    'file_inventory','package_dependency','symbol_declaration','responsibility_fingerprint','symbol_call',
    'document_reference','configuration_effect','data_effect','policy_effect','journey_effect'
  )),
  state TEXT NOT NULL CHECK(state IN ('complete','partial','unsupported','failed')),
  analyzer_id TEXT NOT NULL,
  attempted INTEGER NOT NULL DEFAULT 0 CHECK(attempted >= 0),
  produced INTEGER NOT NULL DEFAULT 0 CHECK(produced >= 0),
  errors INTEGER NOT NULL DEFAULT 0 CHECK(errors >= 0),
  reason TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(generation_id,family,analyzer_id),
  CHECK((state='complete' AND reason='') OR state!='complete')
);
INSERT INTO understanding_coverage SELECT * FROM understanding_coverage_v4;
DROP TABLE understanding_coverage_v4;
CREATE TRIGGER understanding_coverage_complete BEFORE INSERT ON understanding_coverage
BEGIN
  SELECT CASE WHEN COALESCE((SELECT status FROM understanding_generation WHERE id=NEW.generation_id),'')!='complete'
    THEN RAISE(ABORT,'understanding coverage requires a complete generation') END;
END;`)
	if err != nil {
		return fmt.Errorf("migrate understanding coverage v5: %w", err)
	}
	return nil
}

// migrateUnderstandingAnalyzerFactsV18 widens only the neutral understanding fact
// vocabulary. Existing generations, units, edges, and coverage rows are preserved;
// language-specific tables or columns are deliberately not introduced.
func migrateUnderstandingAnalyzerFactsV18(db schemaDB, version int) error {
	if version >= 18 {
		return nil
	}
	coverageColumns, err := columnSet(db, "understanding_coverage")
	if err != nil {
		return err
	}
	if len(coverageColumns) > 0 && (!coverageColumns["unresolved"] || !coverageColumns["ambiguous"]) {
		if _, err := db.Exec(`
DROP TRIGGER IF EXISTS understanding_coverage_complete;
ALTER TABLE understanding_coverage RENAME TO understanding_coverage_v17;
CREATE TABLE understanding_coverage(
  generation_id INTEGER NOT NULL REFERENCES understanding_generation(id) ON DELETE RESTRICT,
  family TEXT NOT NULL CHECK(family IN (
    'file_inventory','package_dependency','symbol_declaration','responsibility_fingerprint','symbol_call',
    'document_reference','configuration_effect','data_effect','policy_effect','journey_effect'
  )),
  state TEXT NOT NULL CHECK(state IN ('complete','partial','unsupported','failed')),
  analyzer_id TEXT NOT NULL,
  attempted INTEGER NOT NULL DEFAULT 0 CHECK(attempted >= 0),
  produced INTEGER NOT NULL DEFAULT 0 CHECK(produced >= 0),
  errors INTEGER NOT NULL DEFAULT 0 CHECK(errors >= 0),
  unresolved INTEGER NOT NULL DEFAULT 0 CHECK(unresolved >= 0),
  ambiguous INTEGER NOT NULL DEFAULT 0 CHECK(ambiguous >= 0),
  reason TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(generation_id,family,analyzer_id),
  CHECK((state='complete' AND reason='') OR state!='complete')
);
INSERT INTO understanding_coverage(generation_id,family,state,analyzer_id,attempted,produced,errors,reason)
  SELECT generation_id,family,state,analyzer_id,attempted,produced,errors,reason
  FROM understanding_coverage_v17;
DROP TABLE understanding_coverage_v17;
CREATE TRIGGER understanding_coverage_complete BEFORE INSERT ON understanding_coverage
BEGIN
  SELECT CASE WHEN COALESCE((SELECT status FROM understanding_generation WHERE id=NEW.generation_id),'')!='complete'
    THEN RAISE(ABORT,'understanding coverage requires a complete generation') END;
END;`); err != nil {
			return fmt.Errorf("migrate understanding coverage v18: %w", err)
		}
	}
	var edgeSQL string
	if err := db.QueryRow(`SELECT COALESCE(sql,'') FROM sqlite_master WHERE type='table' AND name='understanding_edge'`).Scan(&edgeSQL); err != nil {
		return fmt.Errorf("inspect understanding edge v18: %w", err)
	}
	if edgeSQL != "" && !strings.Contains(edgeSQL, "symbol_calls_symbol") {
		if _, err := db.Exec(`
DROP TRIGGER IF EXISTS understanding_edge_complete;
DROP INDEX IF EXISTS understanding_edge_from;
DROP INDEX IF EXISTS understanding_edge_to;
ALTER TABLE understanding_edge RENAME TO understanding_edge_v17;
CREATE TABLE understanding_edge(
  generation_id INTEGER NOT NULL REFERENCES understanding_generation(id) ON DELETE RESTRICT,
  from_kind TEXT NOT NULL CHECK(from_kind IN ('file','package','symbol','document','item')),
  from_ref TEXT NOT NULL,
  relation TEXT NOT NULL CHECK(relation IN (
    'file_in_package','package_depends_on','file_declares_symbol',
    'symbol_calls_symbol','file_references_symbol',
    'document_references_file','document_defines_item','item_references_file'
  )),
  to_kind TEXT NOT NULL CHECK(to_kind IN ('file','package','symbol','document','item')),
  to_ref TEXT NOT NULL,
  source_path TEXT NOT NULL DEFAULT '',
  source_line INTEGER NOT NULL DEFAULT 0 CHECK(source_line >= 0),
  provenance TEXT NOT NULL CHECK(provenance='measured'),
  analyzer_id TEXT NOT NULL,
  evidence_digest TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(generation_id,from_kind,from_ref,relation,to_kind,to_ref,source_path,source_line,analyzer_id),
  CHECK(
    (relation='file_in_package' AND from_kind='file' AND to_kind='package') OR
    (relation='package_depends_on' AND from_kind='package' AND to_kind='package') OR
    (relation='file_declares_symbol' AND from_kind='file' AND to_kind='symbol') OR
    (relation='symbol_calls_symbol' AND from_kind='symbol' AND to_kind='symbol') OR
    (relation='file_references_symbol' AND from_kind='file' AND to_kind='symbol') OR
    (relation='document_references_file' AND from_kind='document' AND to_kind='file') OR
    (relation='document_defines_item' AND from_kind='document' AND to_kind='item') OR
    (relation='item_references_file' AND from_kind='item' AND to_kind='file')
  )
);
INSERT INTO understanding_edge SELECT * FROM understanding_edge_v17;
DROP TABLE understanding_edge_v17;
CREATE INDEX understanding_edge_from ON understanding_edge(generation_id,from_kind,from_ref);
CREATE INDEX understanding_edge_to ON understanding_edge(generation_id,to_kind,to_ref);
CREATE TRIGGER understanding_edge_complete BEFORE INSERT ON understanding_edge
BEGIN
  SELECT CASE WHEN COALESCE((SELECT status FROM understanding_generation WHERE id=NEW.generation_id),'')!='complete'
    THEN RAISE(ABORT,'understanding edges require a complete generation') END;
END;`); err != nil {
			return fmt.Errorf("migrate understanding edges v18: %w", err)
		}
	}
	return nil
}

// migrateUnderstandingIdentityV16 makes complete generation identity a storage
// invariant. Failed attempts remain repeatable so retry history is preserved.
func migrateUnderstandingIdentityV16(db schemaDB, version int) error {
	if version < 16 {
		var duplicates int
		if err := db.QueryRow(`SELECT COUNT(*) FROM (
			SELECT 1 FROM understanding_generation WHERE status='complete'
			GROUP BY repository_id,checkout_id,snapshot_digest,structural_schema,
				analyzer_bundle_digest,convention_state,convention_source_digest
			HAVING COUNT(*)>1)`).Scan(&duplicates); err != nil {
			return fmt.Errorf("migrate understanding identity v16: inspect duplicates: %w", err)
		}
		if duplicates > 0 {
			return fmt.Errorf("migrate understanding identity v16: %d duplicate complete generation identities require explicit repair", duplicates)
		}
	}
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS understanding_generation_complete_identity
		ON understanding_generation(repository_id,checkout_id,snapshot_digest,structural_schema,
			analyzer_bundle_digest,convention_state,convention_source_digest)
		WHERE status='complete'`); err != nil {
		return fmt.Errorf("migrate understanding identity v16: create unique index: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS session_checkpoint_understanding_v16
		ON session_checkpoint(status,repository_id,checkout_id,capture_ended_at DESC,id DESC)`); err != nil {
		return fmt.Errorf("migrate understanding identity v16: create checkpoint index: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS session_checkpoint_recovery_v16
		ON session_checkpoint(status,capture_ended_at DESC,id DESC,repository_id,checkout_id,change_record_id)`); err != nil {
		return fmt.Errorf("migrate understanding identity v16: create recovery index: %w", err)
	}
	return nil
}

// SchemaVersion is the store layout this binary understands. Bump it when a
// migration step is added, so an OLDER binary opening a NEWER database can refuse
// rather than silently write against a layout it does not know (D12). A release is
// N binaries × M databases; "works at 1×1" is not the shipping condition.
// An additive CREATE INDEX IF NOT EXISTS that every binary can read need not
// bump it (ensurePathReconciliationIndexes).
// v2 adds event.runtime; v3 adds C6 change evidence; v4 adds C7 typed
// understanding generations outside the governed-event fold; v5 adds the C8
// responsibility-fingerprint coverage family without adding a second fact store;
// v6 adds one-action-to-many-resource associations for exact structured targets;
// v7 adds replay-safe observation/input evidence and first-attachment checkpoints;
// v8 adds result/effect observations, append-only reconciliation, lifecycle cursors,
// later checkpoint kinds and bounded checkpoint/path facts; v9 adds complete-record
// transcript progress, bounded rescan recovery, and coalesced checkpoint triggers;
// v10 adds result-scoped native-call aliases for exact cross-surface reconciliation;
// v11 separates active result/action collection gaps from evidence retained outside
// the delivered-action observation denominator; v12 indexes strict duplicate-delivery
// reconciliation repair; v13 adds durable, bounded Codex transcript-action parser
// progress without adding another work/cursor table; v14 adds exact collection-issue
// source identity and repairs the checkpoint-trigger foreign key left by the v8 rebuild;
// v15 enforces the parser-eviction source-identity tuple in SQLite as well as Go;
// v16 enforces exact complete-understanding identity across processes and restarts;
// v17 records canonical session-entry activity and keys Git boundaries by runtime;
// v18 adds generic symbol-call edges and unresolved/ambiguous analyzer coverage;
// v20 adds bounded daemon-owned runtime tasks and their replay projection; v21 adds
// optional collector-owned action correlation for committed observation evidence; v22
// makes one relational table the search-content owner and adds canonical transcript
// projection state; v23 adds exact delegated-first approval linkage to the optional
// upper-layer reviewer without changing governance or approval ownership; v24 is
// the orchestration agents redesign (reviewer/follower/helper binding taxonomy,
// deferred run state, reply/turn anchors and cycle counters on relationships,
// declared tags, owner limits, group notes, model-claimed tag rows, and the
// orchestration_cursor -> orchestration_stream_position rename); v25 adds provider
// outage parking and route fallback; v26 adds durable workspace selection revisions,
// consumer leases, worktree lifecycle, and mutation recovery authority outside the
// governance evidence fold.
// Version 19 was reserved by the preceding installed build and intentionally remains
// unstated.
// v27 (2026-09-01): session_turn_observation — turn-boundary evidence from
// installed hooks (turn.started / turn.ended / input.requested / subagent.* /
// context.compacted), its own table so the presence emitter's unfiltered
// rowid read is untouched; received_at_ms is the status decider's comparator.
// v28 (2026-09-05): choice prompts on held approvals — orchestration_review_binding
// gains answer_choice_prompts (whether this reviewer may answer a held question) and
// orchestration_review_invocation gains selections_json (what the reviewer CLAIMED to
// pick, which is not the same fact as what the approval owner made operative).
// v29 (2026-09-12): session_delivery — one helper message pending for a session's
// own next hook/plugin boundary (helper-session-attachment plan D5); additive,
// created by the base schema on open.
// v32 (2026-09-17): session_owner_tag + session_owner_note — text the owner puts
// on a session to organize his work (session-organization plan). User content:
// never a detector fact, a rule input or agent context. Additive, created by the
// base schema on open; no backfill. (Written as v31; the team plan's sync schema
// merged first and holds that number.)
// v33 (2026-09-24): event.rule_id — the rule that produced or asked for a decision, as a
// field instead of prose inside reason (team plan §5.15); additive, no backfill.
// v34 (2026-09-25): usage_call, usage_call_copy, usage_source, usage_prune — model calls
// the usage recorder reads from vendor sources, kept past vendor pruning
// (token-usage-analytics plan §3.6). Additive, created by the base schema on open; the
// recorder backfills from the sources still on disk. Rolling back is dropping the four
// tables and stamping 33.
// v35 (2026-09-26): usage_source.role — the vendor-published agent type of a delegated
// source, for the Usage page's subagents-by-type card (session usage breakdown plan
// §5.2). Additive, no backfill here: the recorder fills it from the listing (Claude) or
// a re-read (Codex). Rolling back is dropping the column and stamping 34.
// v36 (2026-09-26): memory as first-class records (memory-first-class-records plan §3):
// memory_record (store-canonical dossiers with wire identity: mem_… global_id,
// scope_type/scope_id, author {type,id}, revision, content_hash), memory_source
// (session citations — ADR 0013 D2 landed), memory_revision (per-record hash-chained
// snapshots — the git-history replacement), memory_tombstone (deletion that syncs).
// Additive tables; the file import itself runs as the daemon's one-time migration
// pass, not here (it touches the filesystem, which DDL may not). Rolling back is
// dropping the four tables and stamping 35. The number is provisional per the
// plan's RT-1 fold: re-derived from origin/main at merge time; renumbering is
// mechanical because the change is additive-only.
// v37 (2026-09-26): session/model effort defaults, immutable task requested settings,
// and managed primary effort. Additive; old rows remain unrecorded/inherited.
// v38 (2026-09-27): event.layer — the distribution tier (user|repository|organization)
// the deciding rule arrived by (team plan §5.16, item 3a). Additive ADD COLUMN with a
// partial index, the rule_id pattern exactly; no backfill — a row's layer is a fact
// about the moment of decision, and rows decided before layered bundles existed carry
// none, never a guessed user. Rolling back is dropping the index and column and
// stamping 37.
// v39 (2026-09-27): orchestration-flows pilot — the append-only owner-tag
// change journal and the flow tables (journal, flow records, stage members,
// flow-scoped bindings, folded-state ceiling counters, dry-run receipts).
// Additive; no existing table changes.
// v40 (2026-09-29): owner attention (escalation-delivery plan §6.7, §13). One
// column, orchestration_managed_run.settled_seq (claim-settle order, the agent-ask
// id space; runs settled before v40 keep 0), plus indexes: orchestration_group by
// (root_runtime, root_native_session_id) and (root_runtime, root_catalog_session_id);
// completed runs by completed_at, by receipt state + completed_at, and by
// settled_seq; runs by (profile_id, error_class). Rolling back: a v39 binary refuses
// a v40 store, so restore the pre-install copy (index.pre-schema-40-*), or drop the
// six indexes, rebuild the table without settled_seq, and stamp 39.
//
// v41 (2026-09-28; planned as 40 and renumbered at merge, because owner attention took
// 40 first): orchestration_flow_member.excluded's CHECK becomes the class
// vocabulary the daemon writes — no-stage added (the postwork fold wrote it and every
// such INSERT failed), the never-written no-root dropped (owner D-1). NOT additive: a
// table REBUILD (rename, create, named-column copy, drop), probed by the generated
// CHECK clause so it runs once; a row in a retired class refuses the open instead of
// being rewritten (flow-member-exclusion-class plan D-3). Any later change to this
// CHECK must extend this rebuild, never add a sibling one. Rolling back is restoring
// the pre-install copy, or — with no no-stage rows — rebuilding with the v39 CHECK
// and stamping 40.
//
// v42 (2026-09-27; planned as 37 and renumbered at merge, because main reached 41
// first): session_message_invocation (session-message-cross-vendor-plan §4) — one
// ledger record per agent-initiated cross-vendor send, minted by the send route:
// admission digest, state vocabulary pending→terminal, receipt, and the synthetic
// delivery-run link for hook targets. Additive table + four indexes, plus two
// canonical-id columns added to a table an earlier build created; rolling back is
// dropping the table and stamping 41.
//
// v43 (2026-10-03): understanding_generation.facts_state ('present'|'pruned') and the
// complete-identity unique index narrowed to present rows (understanding-facts-retention
// plan §4.2). Additive ADD COLUMN plus an index swap, both layout-probed. A v42 binary
// refuses a v43 store. Rolling back, daemon stopped, needs no spare disk and does NOT
// rebuild the table (it is the foreign-key parent of three tables; the v42 binary names
// its columns, so the extra column is harmless):
//
//	(the statements are understandingFactsStateV43RollbackSQL, below)
//
// The pruned rows must go: a v42 binary would read them as "measured, empty" and never
// re-scan. The three child deletes clear rows a stopped daemon had not finished
// removing. Remove the understanding_retention section from daemon.json first; the
// older binary decodes that file strictly.
//
// v44 (2026-10-03, built 2026-10-02 as 43 and renumbered when main took 43): team item 5 — memory by scope across the team
// (team-plane-item5-plan.md decision 2). memory_record gains the sync state and
// share_state (additive columns); memory_revision is REBUILT keyed UNIQUE(global_id,
// revision) with global_id backfilled from its body and a nullable server_revision;
// memory_tombstone is REBUILT keyed by global_id (legacy slug rows become
// 'slug:<id>') with prior hash, scope, reason and origin; memory_conflict holds conflict
// copies; sync_outbox gains revision, ack_code, sent_body_hash and the frozen
// sent_wire_body / sent_wire_hash. The pre-44 memory backlog is NOT touched here: the
// drain acks each such row not_shareable on its first tick (C-11). Each rebuild is
// probed by its new column so it runs once. Rolling back is restoring the pre-install
// copy (index.pre-schema-44-*): a v43 binary refuses a v44 store. The step probes the layout,
// so it is correct from a v42 store and from a store already at main's v43.
//
// v45 (2026-10-03; built as 44 and renumbered at merge, because team memory took 44
// first): understanding storage shape (understanding-facts-retention plan §5).
// understanding_unit.descriptor_json becomes descriptor_digest referencing the new
// understanding_descriptor table (each distinct descriptor text stored once), and
// understanding_edge becomes WITHOUT ROWID with understanding_edge_from dropped (a
// prefix of the primary key). NOT additive: both tables are rebuilt by rename, create,
// copy, drop, probed by layout so it runs once, and refused before it starts when the
// volume lacks room (store/understanding_shape.go). The copy is proportional to the
// rows left, so run it after retention has drained. Rolling back is restoring the
// pre-install copy; a v44 binary refuses a v45 store.
//
// v46 (2026-10-04): the rest of the team release (team-rest-of-release-plan.md §5.3,
// §6.7). Additive only: route_id, route_revision_digest, route_problem and adoption_key
// on orchestration_managed_binding and orchestration_review_binding (the existing
// runtime/model/endpoint columns become the resolved copy of a named model route);
// the handoff, team_member, handoff_open and handoff_runtime_readiness tables. No row
// is rewritten here: bindings get their routes from a pure, idempotent pass after the
// store opens (plan §5.6). Rolling back is restoring the pre-install copy and the
// profile directory copy and removing <dataDir>/models/routes/ (plan §5.6 step 5); a
// v45 binary refuses a v46 store.
const SchemaVersion = 46

// understandingFactsStateV43RollbackSQL is the v43 rollback procedure named in the
// schema note above. It is a constant so the test that proves it runs the very text
// an operator would.
const understandingFactsStateV43RollbackSQL = `
DELETE FROM understanding_edge WHERE generation_id IN (SELECT id FROM understanding_generation WHERE facts_state='pruned');
DELETE FROM understanding_unit WHERE generation_id IN (SELECT id FROM understanding_generation WHERE facts_state='pruned');
DELETE FROM understanding_coverage WHERE generation_id IN (SELECT id FROM understanding_generation WHERE facts_state='pruned');
DELETE FROM understanding_generation WHERE facts_state='pruned';
DROP INDEX understanding_generation_complete_identity;
CREATE UNIQUE INDEX understanding_generation_complete_identity
  ON understanding_generation(repository_id,checkout_id,snapshot_digest,structural_schema,
    analyzer_bundle_digest,convention_state,convention_source_digest)
  WHERE status='complete';
PRAGMA user_version = 42;`

// checkSchemaVersion is the version half of D12. `migrate` already brings an older
// database's COLUMNS forward idempotently; this adds the forward-compat guard the
// column checks cannot give: a database stamped NEWER than this binary is refused,
// because we cannot know what a future layout changed and a wrong guess corrupts
// PRIMARY TRUTH. A database at or below our version is migrated (above) and stamped.
func schemaVersion(db schemaDB) (int, error) {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return v, nil
}

func checkSchemaVersion(db schemaDB) error {
	v, err := schemaVersion(db)
	if err != nil {
		return err
	}
	if v > SchemaVersion {
		return fmt.Errorf("store schema is v%d but this crossing-guard understands only v%d — "+
			"upgrade the binary; refusing to open a newer store to avoid corrupting it", v, SchemaVersion)
	}
	return nil
}

// columnSet returns the column names of a table, empty when the table is absent.
func columnSet(db schemaDB, table string) (map[string]bool, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}

// Open opens (creating if needed) the index at path and ensures the schema.
func Open(path string) (*Index, error) { return open(path, false) }

// OpenExclusive opens the store the way Open does, but holds it exclusively from the
// first byte, migration included: locking_mode(EXCLUSIVE) is in the connection string
// ahead of the journal mode, on a pool of one connection (a second pooled connection
// would be locked out by the first). It is for whole-file maintenance — the schema-45
// rebuild and Compact — and fails busy when any other process has the store open for
// writing. The pool of one means a method that holds a cursor while issuing another
// statement would wait on itself; use the returned Index for maintenance only.
func OpenExclusive(path string) (*Index, error) { return open(path, true) }

func open(path string, exclusive bool) (*Index, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// Only in a test binary that asked for it (SeedFreshStores): a path with nothing
	// at it then starts from a copy of a migrated, empty store, and everything below
	// runs over the copy as over any existing store. A production binary never reads
	// the seed.
	if testing.Testing() {
		if seed := freshStoreSeed.Load(); seed != nil {
			placeFreshStoreSeed(path, *seed)
		}
	}
	return openAt(path, exclusive)
}

// openAt opens the store at path as it is: it creates the schema in a new file and
// migrates an older one.
func openAt(path string, exclusive bool) (*Index, error) {
	// Pragmas in the DSN so they apply to EVERY pooled connection, not just the one
	// that would serve a post-open Exec (that was the bug: busy_timeout is
	// per-connection). WAL lets the live governor (writer) run without blocking
	// console/query readers; the busy timeout bounds a contended observe write
	// (governance-model.md rev. 3, R3).
	//
	// _txlock=immediate: every Begin() here is a write transaction, and several
	// read before their first write. A DEFERRED transaction that upgrades its
	// read snapshot under WAL gets SQLITE_BUSY_SNAPSHOT the instant any other
	// connection has committed — the busy handler is never consulted, so the
	// 2000ms timeout silently did not apply and concurrent writers failed with
	// "database is locked" after 0ms. Taking the
	// write lock at BEGIN puts the contention where busy_timeout genuinely
	// works; plain queries open no transaction and keep WAL's readers-never-
	// block property.
	locking := ""
	if exclusive {
		locking = "&_pragma=locking_mode(EXCLUSIVE)"
	}
	db, err := driver.Open("file:"+path+"?_txlock=immediate"+locking+"&_pragma=journal_mode(WAL)&_pragma=busy_timeout(2000)&_pragma=foreign_keys(1)", fts5.Register)
	if err != nil {
		return nil, err
	}
	if exclusive {
		db.SetMaxOpenConns(1)
	}
	// Version gate FIRST, before any write. A store stamped NEWER than this binary
	// understands must be refused before DDL or migration touches it — a `CREATE TABLE
	// IF NOT EXISTS` would otherwise re-create a table a future layout dropped, mutating
	// the very store the guard swore not to corrupt (D12). user_version is a db-level
	// pragma, so it reads/stamps fine before the tables exist.
	if err := checkSchemaVersion(db); err != nil {
		db.Close()
		return nil, err
	}
	// The schema-45 rebuild copies every unit and edge row. An ordinary open never
	// starts that under whoever else has the store open: only the exclusive open does
	// (the daemon at boot, or crossing-guard compact).
	if !exclusive {
		pending, pendingErr := understandingShapeRebuildPending(db)
		if pendingErr != nil {
			db.Close()
			return nil, pendingErr
		}
		if pending {
			db.Close()
			return nil, ErrUnderstandingRebuildPending
		}
	}
	// DDL, legacy repair and the version stamp are ONE transaction. The old order
	// stamped first, so a failed migration could leave a v3 label on a v2 layout.
	tx, err := db.Begin()
	if err != nil {
		db.Close()
		return nil, err
	}
	if _, err = tx.Exec(schema); err == nil {
		err = migrate(tx)
	}
	if err == nil && migrationTestHook != nil {
		err = migrationTestHook(tx)
	}
	if err == nil {
		_, err = tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, SchemaVersion))
	}
	if err == nil {
		err = tx.Commit()
	} else {
		_ = tx.Rollback()
	}
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate store: %w", err)
	}
	var fk int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		db.Close()
		return nil, fmt.Errorf("store: foreign_keys is %d, expected 1: %v", fk, err)
	}
	// Confirm WAL actually engaged — a silent fallback (e.g. an unsupported fs) would
	// otherwise leave the "readers never block" guarantee false while the code claims it.
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		db.Close()
		return nil, err
	}
	if mode != "wal" {
		db.Close()
		return nil, fmt.Errorf("store: journal_mode is %q, expected wal", mode)
	}
	restrictPerms(path)
	return &Index{db: db}, nil
}

// restrictPerms makes the store owner-only. It holds every file path, URL and
// command an agent touched; it was created 0644 (world-readable) while the API
// token beside it was correctly 0600. Applied on every Open, not just creation, so
// an index made by an older build is fixed in place rather than staying exposed
// until someone recreates it.
//
// The -wal and -shm siblings carry the same content and are chmod'd too. Errors are
// ignored deliberately: a store we cannot chmod is still a store we must serve, and
// on Windows chmod does not restrict access at all (that is D18's ACL work, not a
// reason to fail here).
func restrictPerms(path string) {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		_ = os.Chmod(p, 0o600)
	}
}

// OpenRO opens an existing index read-only for query paths: no schema DDL
// (Open's DDL takes a write lock on every request) and a short busy timeout
// instead of the driver's one-minute default, so a concurrent harvest writer
// makes readers fail fast to their fallbacks rather than stall (red-team
// finding).
func OpenRO(path string) (*Index, error) {
	db, err := driver.Open("file:"+path+"?mode=ro&_pragma=busy_timeout(500)&_pragma=foreign_keys(1)", fts5.Register)
	if err != nil {
		return nil, err
	}
	if err := checkSchemaVersion(db); err != nil {
		db.Close()
		return nil, err
	}
	// A read-only open never migrates. A store still awaiting the schema-45 rebuild
	// would answer this binary's unit and edge reads with "no such table"; say what
	// is actually wrong instead.
	if pending, err := understandingShapeRebuildPending(db); err != nil || pending {
		db.Close()
		if err == nil {
			err = ErrUnderstandingRebuildPending
		}
		return nil, err
	}
	return &Index{db: db}, nil
}

func (ix *Index) Close() error { return ix.db.Close() }

// Export writes a consistent, self-contained snapshot of the store to destPath
// (D15). The store is declared PRIMARY TRUTH and is never rebuildable from vendor
// files — the event log is the only record that a blocked action ever happened — so
// a backup verb is a deployability requirement (install-flow-v1 promises "exportable"),
// not a nicety.
//
// VACUUM INTO takes a read transaction, so it is consistent even while the live
// governor is writing under WAL, and it produces ONE plain file (no -wal/-shm
// siblings to carry) that is itself a valid store. destPath must not already exist —
// refusing to overwrite is deliberate: a backup that clobbers a prior backup is a
// footgun on the one artifact you reach for after losing the original.
func (ix *Index) Export(destPath string) error {
	if _, err := os.Stat(destPath); err == nil {
		return fmt.Errorf("export target already exists: %s (refusing to overwrite a backup)", destPath)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return err
	}
	// VACUUM INTO cannot bind a parameter; quote the path for the SQL string.
	if _, err := ix.db.Exec(`VACUUM INTO '` + strings.ReplaceAll(destPath, "'", "''") + `'`); err != nil {
		return fmt.Errorf("export: %w", err)
	}
	// The snapshot inherits nothing from the source's perms; it holds the same
	// sensitive record, so lock it down the same way (best-effort, as restrictPerms).
	_ = os.Chmod(destPath, 0o600)
	return nil
}

// migrateRuleV33 makes the deciding rule a field (team plan §5.15). Until now a rule id
// reached storage only as prose inside reason, so nothing durable could say "the canary
// was blocked": the fleet's core claim had no source. Additive and idempotent. There is
// deliberately NO backfill — a rule id parsed out of a reason string would be inference
// stored as fact, which EventRecord.Runtime's own comment forbids for that column too.
// The index is partial because almost every row has no rule. SQLite qualifies a partial
// index only when the query repeats its WHERE term literally — `rule_id = ?` does not
// imply `rule_id != ”` to the planner — so RuntimeCanaries carries that term, and a test
// reads the query plan.
func migrateRuleV33(db schemaDB) error {
	cols, err := columnSet(db, "event")
	if err != nil {
		return err
	}
	if !cols["rule_id"] {
		if _, err := db.Exec(`ALTER TABLE event ADD COLUMN rule_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add event.rule_id: %w", err)
		}
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS event_rule ON event(rule_id, runtime, id) WHERE rule_id != ''`); err != nil {
		return fmt.Errorf("index event.rule_id: %w", err)
	}
	return nil
}

// migrateLayerV38 gives the event table the layer column (team plan §5.16, schema 38):
// the distribution tier — user, repository, organization — whose rule decided, the same
// shape as rule_id's v33 migration. Additive and idempotent by the probe-the-column
// discipline; the partial index exists for the sessions view's per-layer filtering and
// is partial because most rows carry no layer, exactly as most carry no rule.
func migrateLayerV38(db schemaDB) error {
	cols, err := columnSet(db, "event")
	if err != nil {
		return err
	}
	if !cols["layer"] {
		if _, err := db.Exec(`ALTER TABLE event ADD COLUMN layer TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add event.layer: %w", err)
		}
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS event_layer ON event(layer, runtime, id) WHERE layer != ''`); err != nil {
		return fmt.Errorf("index event.layer: %w", err)
	}
	return nil
}

// migrateUsageRoleV35 gives an existing usage_source table the role column
// (session usage breakdown plan §5.2). Additive; the recorder fills it.
func migrateUsageRoleV35(db schemaDB) error {
	cols, err := columnSet(db, "usage_source")
	if err != nil {
		return err
	}
	if len(cols) == 0 || cols["role"] {
		return nil
	}
	if _, err := db.Exec(`ALTER TABLE usage_source ADD COLUMN role TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("add usage_source.role: %w", err)
	}
	return nil
}

// migrateSyncV31 adds the client's push side (team plan §5.12): a device identity, the
// outbox and cursors, and global ids plus the per-session event chain on event rows.
// Additive and idempotent — tables by IF NOT EXISTS, columns guarded by columnSet, and
// the backfill touches only rows still carrying the empty default. A legacy row gets a
// deterministic global id and NO chain columns: it is `chain: none`, never a fabricated
// hash. The indexes are created here, not in the base DDL, because the base DDL runs
// before this migration on every open and on a pre-31 store the columns do not exist.
func migrateSyncV31(db schemaDB) error {
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS sync_device(
  id TEXT PRIMARY KEY,
  created_at INTEGER NOT NULL,
  linked INTEGER NOT NULL DEFAULT 0 CHECK(linked IN (0,1))
);
CREATE TABLE IF NOT EXISTS sync_outbox(
  seq INTEGER PRIMARY KEY,
  record_kind TEXT NOT NULL,
  global_id TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  scope TEXT NOT NULL,
  enqueued_at INTEGER NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0 CHECK(attempts >= 0),
  acked_at INTEGER
);
CREATE INDEX IF NOT EXISTS sync_outbox_pending ON sync_outbox(acked_at, seq);
CREATE TABLE IF NOT EXISTS sync_cursor(
  scope TEXT PRIMARY KEY,
  cursor TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sync_content_optin(
  session_id TEXT PRIMARY KEY,
  since INTEGER NOT NULL
);`); err != nil {
		return fmt.Errorf("migrate sync v31: tables: %w", err)
	}
	var devices int
	if err := db.QueryRow(`SELECT count(*) FROM sync_device`).Scan(&devices); err != nil {
		return fmt.Errorf("migrate sync v31: device probe: %w", err)
	}
	if devices == 0 {
		if _, err := db.Exec(`INSERT INTO sync_device(id,created_at,linked) VALUES(?,?,0)`,
			engine.NewTypedID(engine.DeviceIDPrefix), time.Now().UnixNano()); err != nil {
			return fmt.Errorf("migrate sync v31: mint device: %w", err)
		}
	}
	cols, err := columnSet(db, "event")
	if err != nil {
		return err
	}
	for _, addition := range []struct {
		name string
		ddl  string
	}{
		{"global_id", `ALTER TABLE event ADD COLUMN global_id TEXT NOT NULL DEFAULT ''`},
		{"chain_seq", `ALTER TABLE event ADD COLUMN chain_seq INTEGER`},
		{"prev_hash", `ALTER TABLE event ADD COLUMN prev_hash TEXT`},
		{"hash", `ALTER TABLE event ADD COLUMN hash TEXT`},
	} {
		if cols[addition.name] {
			continue
		}
		if _, err := db.Exec(addition.ddl); err != nil {
			return fmt.Errorf("migrate sync v31: %s: %w", addition.name, err)
		}
	}
	// Backfill: a deterministic id per legacy row, keyed by this device, so a restored
	// backup migrates to the same ids. Read every id first; SQLite's one writer must not
	// have an open cursor while it updates.
	var deviceID string
	if err := db.QueryRow(`SELECT id FROM sync_device LIMIT 1`).Scan(&deviceID); err != nil {
		return fmt.Errorf("migrate sync v31: device id: %w", err)
	}
	rows, err := db.Query(`SELECT id FROM event WHERE global_id = ''`)
	if err != nil {
		return fmt.Errorf("migrate sync v31: legacy scan: %w", err)
	}
	var legacy []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		legacy = append(legacy, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, id := range legacy {
		gid := engine.DeterministicTypedID("evt", deviceID+":"+strconv.FormatInt(id, 10))
		if _, err := db.Exec(`UPDATE event SET global_id = ? WHERE id = ? AND global_id = ''`, gid, id); err != nil {
			return fmt.Errorf("migrate sync v31: backfill %d: %w", id, err)
		}
	}
	if _, err := db.Exec(`
CREATE UNIQUE INDEX IF NOT EXISTS event_global_id ON event(global_id) WHERE global_id != '';
CREATE INDEX IF NOT EXISTS event_chain ON event(session_id, chain_seq);`); err != nil {
		return fmt.Errorf("migrate sync v31: indexes: %w", err)
	}
	return nil
}

// migrateSessionMessageInvocationV42 gives an existing session_message_invocation
// table the canonical-id columns the postwork red-team fold added (postwork
// PW-3: the loop bound forms edges over canonical ids, so one session under
// its rollout/meta/thread id forms is one node). Additive ADD COLUMN with
// defaults, guarded by the probe-the-column discipline; a store without the
// table (a store the DDL created fresh on open) takes the base DDL.
func migrateSessionMessageInvocationV42(db schemaDB) error {
	cols, err := columnSet(db, "session_message_invocation")
	if err != nil {
		return err
	}
	if len(cols) == 0 {
		return nil
	}
	for _, addition := range []struct {
		name string
		ddl  string
	}{
		{"caller_canonical_id", `ALTER TABLE session_message_invocation ADD COLUMN caller_canonical_id TEXT NOT NULL DEFAULT ''`},
		{"target_canonical_id", `ALTER TABLE session_message_invocation ADD COLUMN target_canonical_id TEXT NOT NULL DEFAULT ''`},
	} {
		if cols[addition.name] {
			continue
		}
		if _, err := db.Exec(addition.ddl); err != nil {
			return fmt.Errorf("migrate session_message_invocation: add %s: %w", addition.name, err)
		}
	}
	return nil
}

// memoryRevisionDDL and memoryTombstoneDDL are the v44 shapes, shared by the base DDL
// (a fresh store) and migrateTeamMemoryV44's rebuild (an older store), so the two can
// never disagree.
const memoryRevisionDDL = `CREATE TABLE IF NOT EXISTS memory_revision(
  revision_id INTEGER PRIMARY KEY,
  record_id TEXT NOT NULL,
  global_id TEXT NOT NULL,
  revision INTEGER NOT NULL,
  server_revision INTEGER,
  author TEXT NOT NULL,
  actor_source TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  prev_hash TEXT NOT NULL DEFAULT '',
  changed TEXT NOT NULL DEFAULT '',
  body TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  UNIQUE(global_id,revision)
);`

const memoryTombstoneDDL = `CREATE TABLE IF NOT EXISTS memory_tombstone(
  global_id TEXT PRIMARY KEY,
  id TEXT NOT NULL,
  deleted_at INTEGER NOT NULL,
  deleted_by TEXT NOT NULL DEFAULT '',
  prior_content_hash TEXT NOT NULL DEFAULT '',
  scope_type TEXT NOT NULL DEFAULT '',
  scope_id TEXT NOT NULL DEFAULT '',
  reason_class TEXT NOT NULL DEFAULT 'user-request',
  origin TEXT NOT NULL DEFAULT 'local' CHECK(origin IN ('local','pulled'))
);`

// migrateTeamMemoryV44 brings an older store to the team item 5 layout
// (team-plane-item5-plan.md decision 2). Each step is probed by a column it adds, so it
// runs once and is safe on every open:
//   - memory_record: the sync-state and share_state columns (additive);
//   - memory_revision: rebuilt UNIQUE(global_id, revision), global_id from the body;
//   - memory_tombstone: rebuilt keyed by global_id, legacy slug rows as 'slug:<id>';
//   - sync_outbox: revision, ack_code, sent_body_hash, sent_wire_body, sent_wire_hash.
//
// Pending pre-44 memory outbox rows name no revision; the drain acks them
// not_shareable on its first tick (C-11, K-6), never encoding them from current state.
func migrateTeamMemoryV44(db schemaDB) error {
	cols, err := columnSet(db, "memory_record")
	if err != nil {
		return err
	}
	for _, addition := range []struct{ name, ddl string }{
		{"share_state", `ALTER TABLE memory_record ADD COLUMN share_state TEXT NOT NULL DEFAULT 'unshared' CHECK(share_state IN ('unshared','shared'))`},
		{"sync_origin", `ALTER TABLE memory_record ADD COLUMN sync_origin TEXT NOT NULL DEFAULT 'local' CHECK(sync_origin IN ('local','pulled'))`},
		{"pushed_hash", `ALTER TABLE memory_record ADD COLUMN pushed_hash TEXT NOT NULL DEFAULT ''`},
		{"server_revision", `ALTER TABLE memory_record ADD COLUMN server_revision INTEGER NOT NULL DEFAULT 0`},
		{"synced_projection_hash", `ALTER TABLE memory_record ADD COLUMN synced_projection_hash TEXT NOT NULL DEFAULT ''`},
		{"held_wire_record", `ALTER TABLE memory_record ADD COLUMN held_wire_record TEXT NOT NULL DEFAULT ''`},
		{"held_server_revision", `ALTER TABLE memory_record ADD COLUMN held_server_revision INTEGER NOT NULL DEFAULT 0`},
		{"wire_slug", `ALTER TABLE memory_record ADD COLUMN wire_slug TEXT NOT NULL DEFAULT ''`},
		{"collision", `ALTER TABLE memory_record ADD COLUMN collision TEXT NOT NULL DEFAULT '' CHECK(collision IN ('','alias','shadowed'))`},
		{"team_author", `ALTER TABLE memory_record ADD COLUMN team_author TEXT NOT NULL DEFAULT ''`},
		{"identity_note", `ALTER TABLE memory_record ADD COLUMN identity_note TEXT NOT NULL DEFAULT ''`},
		{"detached_from", `ALTER TABLE memory_record ADD COLUMN detached_from TEXT NOT NULL DEFAULT ''`},
	} {
		if len(cols) == 0 || cols[addition.name] {
			continue
		}
		if _, err := db.Exec(addition.ddl); err != nil {
			return fmt.Errorf("migrate team memory v44: memory_record.%s: %w", addition.name, err)
		}
	}
	if cols, err = columnSet(db, "memory_revision"); err != nil {
		return err
	}
	if len(cols) > 0 && !cols["global_id"] {
		for _, q := range []string{
			`ALTER TABLE memory_revision RENAME TO memory_revision_pre44`,
			memoryRevisionDDL,
			`INSERT OR IGNORE INTO memory_revision(revision_id, record_id, global_id, revision, server_revision, author, actor_source, content_hash, prev_hash, changed, body, created_at)
			 SELECT revision_id, record_id, COALESCE(NULLIF(json_extract(body, '$.id'), ''), 'slug:' || record_id), revision, NULL,
			        author, actor_source, content_hash, prev_hash, changed, body, created_at FROM memory_revision_pre44`,
			`DROP TABLE memory_revision_pre44`,
		} {
			if _, err := db.Exec(q); err != nil {
				return fmt.Errorf("migrate team memory v44: memory_revision rebuild: %w", err)
			}
		}
	}
	if cols, err = columnSet(db, "memory_tombstone"); err != nil {
		return err
	}
	if len(cols) > 0 && !cols["global_id"] {
		for _, q := range []string{
			`DROP INDEX IF EXISTS memory_tombstone_slug`,
			`ALTER TABLE memory_tombstone RENAME TO memory_tombstone_pre44`,
			memoryTombstoneDDL,
			`INSERT OR IGNORE INTO memory_tombstone(global_id, id, deleted_at, deleted_by)
			 SELECT 'slug:' || id, id, deleted_at, deleted_by FROM memory_tombstone_pre44`,
			`DROP TABLE memory_tombstone_pre44`,
			`CREATE INDEX IF NOT EXISTS memory_tombstone_slug ON memory_tombstone(id)`,
		} {
			if _, err := db.Exec(q); err != nil {
				return fmt.Errorf("migrate team memory v44: memory_tombstone rebuild: %w", err)
			}
		}
	}
	if cols, err = columnSet(db, "sync_outbox"); err != nil {
		return err
	}
	for _, addition := range []struct{ name, ddl string }{
		{"revision", `ALTER TABLE sync_outbox ADD COLUMN revision INTEGER NOT NULL DEFAULT 0`},
		{"ack_code", `ALTER TABLE sync_outbox ADD COLUMN ack_code TEXT NOT NULL DEFAULT ''`},
		{"sent_body_hash", `ALTER TABLE sync_outbox ADD COLUMN sent_body_hash TEXT NOT NULL DEFAULT ''`},
		{"sent_wire_body", `ALTER TABLE sync_outbox ADD COLUMN sent_wire_body TEXT NOT NULL DEFAULT ''`},
		{"sent_wire_hash", `ALTER TABLE sync_outbox ADD COLUMN sent_wire_hash TEXT NOT NULL DEFAULT ''`},
	} {
		if len(cols) == 0 || cols[addition.name] {
			continue
		}
		if _, err := db.Exec(addition.ddl); err != nil {
			return fmt.Errorf("migrate team memory v44: sync_outbox.%s: %w", addition.name, err)
		}
	}
	// Two indexes over the outbox, each built once over the whole table at first open (the
	// one step of this migration whose cost grows with the store). sync_outbox_record
	// serves the per-record reads (one in flight, the hold rule). sync_outbox_kind_scope
	// serves the status reads GET /api/team and the session footer make on every call:
	// measured 2026-10-02 on 1,000,000 acknowledged rows, the status read took 316 ms
	// and the footer's count 237 ms scanning the table, 48 ms and 18 ms with the index,
	// which took 1.5 s to build (CR-12).
	for _, q := range []string{
		`CREATE INDEX IF NOT EXISTS sync_outbox_record ON sync_outbox(global_id, acked_at, seq)`,
		`CREATE INDEX IF NOT EXISTS sync_outbox_kind_scope ON sync_outbox(record_kind, scope, ack_code)`,
	} {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("migrate team memory v44: outbox index: %w", err)
		}
	}
	return nil
}
