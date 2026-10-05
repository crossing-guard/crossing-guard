package store

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/ncruces/go-sqlite3/driver"
	"github.com/ncruces/go-sqlite3/ext/fts5"
)

// The v23 managed family, verbatim, so the v24 rebuild is exercised against the
// exact shapes a real pre-redesign store carries.
const managedFamilyV23ForTest = `
CREATE TABLE orchestration_managed_binding(
  binding_id TEXT PRIMARY KEY,
  state TEXT NOT NULL CHECK(state IN ('enabled','disabled')),
  role TEXT NOT NULL CHECK(role IN ('follower','coordinator','course-corrector')),
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
  auto_action INTEGER NOT NULL DEFAULT 0 CHECK(auto_action IN (0,1)),
  state_token TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE orchestration_group(
  group_id TEXT PRIMARY KEY,
  binding_id TEXT NOT NULL REFERENCES orchestration_managed_binding(binding_id) ON DELETE RESTRICT,
  state TEXT NOT NULL CHECK(state IN ('active','completed','disabled','unknown')),
  root_task_id TEXT NOT NULL,
  root_runtime TEXT NOT NULL,
  root_catalog_session_id TEXT NOT NULL DEFAULT '',
  root_native_session_id TEXT NOT NULL DEFAULT '',
  project_root TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE orchestration_managed_run(
  run_id TEXT PRIMARY KEY,
  idempotency_key TEXT NOT NULL UNIQUE,
  group_id TEXT NOT NULL REFERENCES orchestration_group(group_id) ON DELETE RESTRICT,
  binding_id TEXT NOT NULL REFERENCES orchestration_managed_binding(binding_id) ON DELETE RESTRICT,
  binding_state_token TEXT NOT NULL,
  role TEXT NOT NULL,
  profile_id TEXT NOT NULL,
  profile_source_digest TEXT NOT NULL,
  profile_bundle_digest TEXT NOT NULL,
  source_task_id TEXT NOT NULL,
  source_event_id INTEGER NOT NULL,
  child_task_id TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL CHECK(state IN ('admitted','running','completed','failed','suppressed','unknown')),
  action TEXT NOT NULL DEFAULT '',
  message TEXT NOT NULL DEFAULT '',
  citations_json TEXT NOT NULL DEFAULT '[]',
  detail_json TEXT NOT NULL DEFAULT '{}',
  error_class TEXT NOT NULL DEFAULT '',
  recovery TEXT NOT NULL DEFAULT '',
  admitted_at INTEGER NOT NULL,
  started_at INTEGER NOT NULL DEFAULT 0,
  completed_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE orchestration_relationship(
  relationship_id TEXT PRIMARY KEY,
  group_id TEXT NOT NULL REFERENCES orchestration_group(group_id) ON DELETE RESTRICT,
  run_id TEXT NOT NULL UNIQUE REFERENCES orchestration_managed_run(run_id) ON DELETE RESTRICT,
  parent_task_id TEXT NOT NULL,
  child_task_id TEXT NOT NULL,
  role TEXT NOT NULL,
  depth INTEGER NOT NULL CHECK(depth BETWEEN 0 AND 8),
  hops INTEGER NOT NULL CHECK(hops BETWEEN 0 AND 8),
  state TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE orchestration_control(
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
CREATE TABLE orchestration_cursor(
  source_kind TEXT PRIMARY KEY,
  cursor INTEGER NOT NULL CHECK(cursor>=0),
  updated_at INTEGER NOT NULL
);`

func TestOrchestrationAgentsV24MigrationMapsRolesAnchorsAndStreamPosition(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	raw, err := driver.Open("file:"+path+"?_pragma=foreign_keys(1)", fts5.Register)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(managedFamilyV23ForTest); err != nil {
		raw.Close()
		t.Fatalf("seed v23 shapes: %v", err)
	}
	seed := `
INSERT INTO orchestration_managed_binding VALUES
 ('b-follower','disabled','follower','','','/repo','p1','s1','d1','codex','','','["draft-reply"]','[]',0,'tok1',1,1),
 ('b-replier','enabled','follower','','','/repo','p2','s2','d2','codex','','','["draft-reply","reply"]','[]',1,'tok2',1,1),
 ('b-coord','enabled','coordinator','','','/repo','p3','s3','d3','codex','','','[]','[]',0,'tok3',1,1),
 ('b-course','disabled','course-corrector','','','/repo','p4','s4','d4','codex','','','[]','[]',0,'tok4',1,1);
INSERT INTO orchestration_group(group_id,binding_id,state,root_task_id,root_runtime,root_catalog_session_id,root_native_session_id,project_root,created_at,updated_at) VALUES ('g1','b-follower','active','t-root','codex','','', '/repo',2,2);
INSERT INTO orchestration_managed_run VALUES
 ('r1','k1','g1','b-follower','tok1','follower','p1','s1','d1','t-root',9,'t-child','completed','no_action','ok','[]','{}','','',3,4,5),
 ('r2','k2','g1','b-coord','tok3','coordinator','p3','s3','d3','t-root',10,'t-child-2','completed','launch_profile','go','[]','{}','','',3,4,5);
INSERT INTO orchestration_relationship VALUES
 ('rel1','g1','r1','t-root','t-child','follower',1,0,'completed',4,5),
 ('rel2','g1','r2','t-root','t-child-2','coordinator',1,0,'completed',4,5);
INSERT INTO orchestration_control VALUES ('c1','r1','t-root','interrupt','requested','confirmed','',6,7);
INSERT INTO orchestration_cursor VALUES ('runtime-task-events-v1',42,8);
PRAGMA user_version = 23;`
	if _, err := raw.Exec(seed); err != nil {
		raw.Close()
		t.Fatalf("seed v23 rows: %v", err)
	}
	raw.Close()

	ix, err := Open(path)
	if err != nil {
		t.Fatalf("open migrated store: %v", err)
	}
	defer ix.Close()

	wantRoles := map[string]string{"b-follower": "follower", "b-replier": "helper", "b-coord": "helper", "b-course": "helper"}
	for id, want := range wantRoles {
		binding, found, err := ix.ManagedBinding(id)
		if err != nil || !found {
			t.Fatalf("binding %s: found=%v err=%v", id, found, err)
		}
		if binding.Role != want {
			t.Fatalf("binding %s role=%q want %q", id, binding.Role, want)
		}
	}
	// Historical run and relationship rows stay, but their legacy role labels
	// map onto the taxonomy: follower maps to itself, every legacy acting
	// label (coordinator here) maps to helper.
	run, found, err := ix.ManagedRun("r1")
	if err != nil || !found || run.Role != "follower" || run.State != "completed" {
		t.Fatalf("follower run must map to itself: %+v found=%v err=%v", run, found, err)
	}
	coordRun, found, err := ix.ManagedRun("r2")
	if err != nil || !found || coordRun.Role != "helper" || coordRun.Action != "launch_profile" {
		t.Fatalf("coordinator run must read back as helper: %+v found=%v err=%v", coordRun, found, err)
	}
	rels, err := ix.ManagedRelationships("g1", 10)
	if err != nil || len(rels) != 2 {
		t.Fatalf("relationships=%v err=%v", rels, err)
	}
	for _, rel := range rels {
		if rel["reply_task_id"] != "" || rel["cycle"].(int64) != 0 {
			t.Fatalf("migrated relationship should default reply anchors: %+v", rel)
		}
		if role := rel["role"]; role != "follower" && role != "helper" {
			t.Fatalf("relationship role did not map to the taxonomy: %+v", rel)
		}
	}
	position, err := ix.OrchestrationStreamPosition("runtime-task-events-v1")
	if err != nil || position != 42 {
		t.Fatalf("stream position=%d err=%v", position, err)
	}
	var stale int
	if err := ix.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name LIKE '%_v23' OR name='orchestration_cursor'`).Scan(&stale); err != nil || stale != 0 {
		t.Fatalf("stale migration tables remain: %d err=%v", stale, err)
	}
	var fkViolations int
	_ = ix.db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&fkViolations)
	if fkViolations != 0 {
		t.Fatalf("foreign key violations after migration: %d", fkViolations)
	}
	if err := ix.SetRelationshipReply("r1", "t-reply", 1, "anchor-src", "anchor-reply", 9); err != nil {
		t.Fatalf("reply anchor write on migrated row: %v", err)
	}
	// The FK-rename repair: tag and note INSERTS must work on a migrated store
	// (the live store failed here 2026-08-29 with a dangling _v23 FK).
	if err := ix.PutOrchestrationTags([]OrchestrationTag{{TagID: "tag1", RunID: "r1", BindingID: "b-follower",
		AgentKey: "agent:b-follower:plan", Tag: "plan", SessionID: "native-x", AppliedAt: 10}}); err != nil {
		t.Fatalf("tag insert on migrated store: %v", err)
	}
	if err := ix.PutManagedGroupNote(ManagedGroupNote{NoteID: "n1", GroupID: "g1", Body: "note", CreatedAt: 11}); err != nil {
		t.Fatalf("group note insert on migrated store: %v", err)
	}
	tags, err := ix.ActiveOrchestrationTags("native-x", 12)
	if err != nil || len(tags) != 1 || tags[0].AgentKey != "agent:b-follower:plan" {
		t.Fatalf("active tags=%+v err=%v", tags, err)
	}
}

// TestKindRepairRunsOnStampedV24StoreMissingKindColumn reproduces the installed
// store's exact 2026-08-30 live failure: stamped user_version=24 BEFORE the
// kind column existed (a development iteration briefly overloaded run.role
// with 'reply'/'correction'/'delegate'). The repair must run at every open —
// probe the column, never trust the version stamp (the v7→v8/_v23 lesson).
func TestKindRepairRunsOnStampedV24StoreMissingKindColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	raw, err := driver.Open("file:"+path+"?_pragma=foreign_keys(1)", fts5.Register)
	if err != nil {
		t.Fatal(err)
	}
	family := strings.Replace(orchestrationManagedSchemaV25,
		"  kind TEXT NOT NULL DEFAULT '' CHECK(kind IN ('','reply','correction','delegate')),\n", "", 1)
	family = strings.Replace(family, "  thinking_effort TEXT NOT NULL DEFAULT 'null',\n", "", 1)
	family = strings.Replace(family,
		"  watch_natural INTEGER NOT NULL DEFAULT 0 CHECK(watch_natural IN (0,1)),\n", "", 1)
	// Historical v24 stores predate the v25 provider-outage shape too: no
	// routes_json column and no 'parked' run state — stripping both makes
	// this fixture exercise the v25 rebuild alongside the kind repair.
	family = strings.Replace(family,
		"  routes_json TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(routes_json)),\n", "", 1)
	family = strings.Replace(family,
		"'admitted','running','completed','failed','suppressed','deferred','parked','unknown'",
		"'admitted','running','completed','failed','suppressed','deferred','unknown'", 1)
	if strings.Contains(family, "\n  kind TEXT") {
		raw.Close()
		t.Fatal("fixture failed to strip the kind column")
	}
	if strings.Contains(family, "\n  watch_natural") {
		raw.Close()
		t.Fatal("fixture failed to strip the watch_natural column")
	}
	if strings.Contains(family, "routes_json") || strings.Contains(family, "parked") {
		raw.Close()
		t.Fatal("fixture failed to strip the v25 provider-outage shape")
	}
	if _, err := raw.Exec(family); err != nil {
		raw.Close()
		t.Fatalf("seed stamped-24 shapes: %v", err)
	}
	seed := `
INSERT INTO orchestration_managed_binding VALUES
 ('b-helper','enabled','helper',10,'','','/repo','p1','s1','d1','codex','','','["reply"]','[]','[]','{}',1,'tok1',1,1);
INSERT INTO orchestration_group(group_id,binding_id,state,root_task_id,root_runtime,root_catalog_session_id,root_native_session_id,project_root,created_at,updated_at) VALUES ('g1','b-helper','active','t-root','codex','cat-1','native-1','/repo',2,2);
INSERT INTO orchestration_managed_run(run_id,idempotency_key,group_id,binding_id,binding_state_token,role,
  profile_id,profile_source_digest,profile_bundle_digest,source_task_id,source_event_id,child_task_id,
  state,action,message,citations_json,detail_json,error_class,recovery,admitted_at,started_at,completed_at) VALUES
 ('r-agent','k1','g1','b-helper','tok1','helper','p1','s1','d1','t-root',9,'t-child','completed','reply','ok','[]','{}','','',3,4,5),
 ('r-reply','k2','g1','b-helper','tok1','reply','p1','s1','d1','t-root',10,'t-reply','completed','child_completed','done','[]','{}','','',3,4,5);
INSERT INTO orchestration_relationship VALUES
 ('rel1','g1','r-agent','t-root','t-child','','','',1,'reply',1,0,'completed',4,5);
PRAGMA user_version = 24;`
	if _, err := raw.Exec(seed); err != nil {
		raw.Close()
		t.Fatalf("seed stamped-24 rows: %v", err)
	}
	raw.Close()

	ix, err := Open(path)
	if err != nil {
		t.Fatalf("open stamped store: %v", err)
	}
	defer ix.Close()
	// The projection read that failed live with "no such column: kind".
	runs, err := ix.ManagedRuns(10)
	if err != nil {
		t.Fatalf("managed runs after repair: %v", err)
	}
	byID := map[string]ManagedRun{}
	for _, run := range runs {
		byID[run.RunID] = run
	}
	if run := byID["r-reply"]; run.Role != "helper" || run.Kind != "reply" {
		t.Fatalf("reply run not repaired: %+v", run)
	}
	if run := byID["r-agent"]; run.Role != "helper" || run.Kind != "" {
		t.Fatalf("agent run mislabeled: %+v", run)
	}
	// The relationship's synthetic role label normalizes too.
	var relationshipRole string
	if err := ix.db.QueryRow(`SELECT role FROM orchestration_relationship WHERE relationship_id='rel1'`).Scan(&relationshipRole); err != nil || relationshipRole != "helper" {
		t.Fatalf("relationship role=%q err=%v", relationshipRole, err)
	}
}
