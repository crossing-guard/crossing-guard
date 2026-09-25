package harvest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ncruces/go-sqlite3/driver"
)

func TestOpenCodeNaturalStoreSmoke(t *testing.T) {
	database := os.Getenv("CG_OPENCODE_SMOKE_DB")
	if database == "" {
		t.Skip("set CG_OPENCODE_SMOKE_DB for an installed-store smoke test")
	}
	t.Setenv("OPENCODE_DATA_HOME", filepath.Dir(database))
	records, err := (opencodeRuntime{}).ListSessionRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) == 0 {
		t.Fatal("installed OpenCode store returned no sessions")
	}
	if _, _, _, err := (opencodeRuntime{}).NormalizeSession(records[0].Ref, false); err != nil {
		t.Fatal(err)
	}
}

func openCodeFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode.db")
	db, err := driver.Open("file:" + path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`
CREATE TABLE session(id TEXT PRIMARY KEY,project_id TEXT NOT NULL,workspace_id TEXT,parent_id TEXT,
 slug TEXT NOT NULL,directory TEXT NOT NULL,path TEXT,title TEXT NOT NULL,version TEXT NOT NULL,
 share_url TEXT,summary_additions INTEGER,summary_deletions INTEGER,summary_files INTEGER,
 summary_diffs TEXT,metadata TEXT,cost REAL NOT NULL DEFAULT 0,tokens_input INTEGER NOT NULL DEFAULT 0,
 tokens_output INTEGER NOT NULL DEFAULT 0,tokens_reasoning INTEGER NOT NULL DEFAULT 0,
 tokens_cache_read INTEGER NOT NULL DEFAULT 0,tokens_cache_write INTEGER NOT NULL DEFAULT 0,
 revert TEXT,permission TEXT,agent TEXT,model TEXT,time_created INTEGER NOT NULL,time_updated INTEGER NOT NULL,
 time_compacting INTEGER,time_archived INTEGER);
CREATE TABLE message(id TEXT PRIMARY KEY,session_id TEXT NOT NULL,time_created INTEGER NOT NULL,
 time_updated INTEGER NOT NULL,data TEXT NOT NULL);
CREATE TABLE part(id TEXT PRIMARY KEY,message_id TEXT NOT NULL,session_id TEXT NOT NULL,
 time_created INTEGER NOT NULL,time_updated INTEGER NOT NULL,data TEXT NOT NULL);
INSERT INTO session(id,project_id,parent_id,slug,directory,title,version,model,tokens_input,tokens_output,
 tokens_cache_read,time_created,time_updated) VALUES
 ('ses_child','p','ses_parent','child','/work/repo','Fixture session','1.18.0',
  '{"id":"qwen2.5-coder:7b","providerID":"ollama"}',12,3,4,1000,5000);
INSERT INTO message VALUES('msg_user','ses_child',1100,1100,
 '{"role":"user","time":{"created":1100}}');
INSERT INTO message VALUES('msg_assistant','ses_child',1200,4900,
 '{"role":"assistant","time":{"created":1200,"completed":4900}}');
INSERT INTO part VALUES('prt_text','msg_user','ses_child',1100,1100,
 '{"type":"text","text":"hello fixture"}');
INSERT INTO part VALUES('prt_tool_1','msg_assistant','ses_child',2000,3000,
 '{"type":"tool","tool":"write","callID":"call_1","state":{"status":"completed","input":{"filePath":"src/a.go","content":"package a"},"output":"written","time":{"start":2000,"end":3000}}}');
INSERT INTO part VALUES('prt_tool_2','msg_assistant','ses_child',4000,4900,
 '{"type":"tool","tool":"bash","callID":"call_2","state":{"status":"error","input":{"command":"false"},"error":"exit 1","time":{"start":4000,"end":4900}}}');`)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENCODE_DATA_HOME", dir)
	return path, "ses_child"
}

// openCodeNullModelFixture builds a store with one healthy session and one
// session whose model is NULL (OpenCode writes this when its task server errors
// before a model is assigned). It is a dedicated fixture: the shared
// openCodeFixture asserts exactly one session, so this must not reuse it.
func openCodeNullModelFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode.db")
	db, err := driver.Open("file:" + path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`
CREATE TABLE session(id TEXT PRIMARY KEY,project_id TEXT NOT NULL,workspace_id TEXT,parent_id TEXT,
 slug TEXT NOT NULL,directory TEXT NOT NULL,path TEXT,title TEXT NOT NULL,version TEXT NOT NULL,
 share_url TEXT,summary_additions INTEGER,summary_deletions INTEGER,summary_files INTEGER,
 summary_diffs TEXT,metadata TEXT,cost REAL NOT NULL DEFAULT 0,tokens_input INTEGER NOT NULL DEFAULT 0,
 tokens_output INTEGER NOT NULL DEFAULT 0,tokens_reasoning INTEGER NOT NULL DEFAULT 0,
 tokens_cache_read INTEGER NOT NULL DEFAULT 0,tokens_cache_write INTEGER NOT NULL DEFAULT 0,
 revert TEXT,permission TEXT,agent TEXT,model TEXT,time_created INTEGER NOT NULL,time_updated INTEGER NOT NULL,
 time_compacting INTEGER,time_archived INTEGER);
CREATE TABLE message(id TEXT PRIMARY KEY,session_id TEXT NOT NULL,time_created INTEGER NOT NULL,
 time_updated INTEGER NOT NULL,data TEXT NOT NULL);
CREATE TABLE part(id TEXT PRIMARY KEY,message_id TEXT NOT NULL,session_id TEXT NOT NULL,
 time_created INTEGER NOT NULL,time_updated INTEGER NOT NULL,data TEXT NOT NULL);
INSERT INTO session(id,project_id,parent_id,slug,directory,title,version,model,tokens_input,tokens_output,
 tokens_cache_read,time_created,time_updated) VALUES
 ('ses_good','p',NULL,'good','/work/repo','Healthy session','1.18.0',
  '{"id":"qwen2.5-coder:7b","providerID":"ollama"}',12,3,4,1000,5000);
INSERT INTO session(id,project_id,parent_id,slug,directory,title,version,model,tokens_input,tokens_output,
 tokens_cache_read,time_created,time_updated) VALUES
 ('ses_null_model','p',NULL,'null-model','/work/repo','Task server error','1.18.0',
  NULL,0,0,0,6000,7000);
INSERT INTO message VALUES('msg_null','ses_null_model',6100,6100,'{"role":"user","time":{"created":6100}}');
INSERT INTO part VALUES('prt_null','msg_null','ses_null_model',6100,6100,'{"type":"text","text":"hi"}');`)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENCODE_DATA_HOME", dir)
	return path, "ses_null_model"
}

func TestOpenCodeNullModelDoesNotAbortDiscoveryOrRead(t *testing.T) {
	openCodeNullModelFixture(t)
	records, err := (opencodeRuntime{}).ListSessionRecords()
	if err != nil {
		t.Fatalf("discovery aborted on NULL model: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("expected healthy + NULL-model rows, got %d records", len(records))
	}
	var nullModel *SessionRecord
	for i := range records {
		if records[i].Summary.ID == "ses_null_model" {
			nullModel = &records[i]
		}
	}
	if nullModel == nil {
		t.Fatal("NULL-model session was not discovered")
	}
	if nullModel.Summary.Model != "" || nullModel.Summary.Provider != "" {
		t.Fatalf("NULL model must degrade to empty, got model=%q provider=%q",
			nullModel.Summary.Model, nullModel.Summary.Provider)
	}
	discovery, err := (opencodeRuntime{}).DiscoverTranscriptProjections(context.Background(), ProjectionReadLimits{})
	if err != nil {
		t.Fatalf("projection discovery aborted on NULL model: %v", err)
	}
	if !discovery.Complete || len(discovery.Sessions) != 2 || len(discovery.Limitations) != 0 {
		t.Fatalf("discovery should be complete with 2 sessions and no limitations: complete=%v sessions=%d limits=%+v",
			discovery.Complete, len(discovery.Sessions), discovery.Limitations)
	}
	events, unparsed, _, err := (opencodeRuntime{}).NormalizeSession(nullModel.Ref, false)
	if err != nil {
		t.Fatalf("read aborted on NULL model: %v", err)
	}
	if len(events) == 0 || unparsed != 0 {
		t.Fatalf("NULL-model session should normalize cleanly: events=%d unparsed=%d", len(events), unparsed)
	}
}

func TestOpenCodePatchEffectsRetainDeclaredPaths(t *testing.T) {
	input := json.RawMessage(`{"patchText":"*** Begin Patch\n*** Update File: src/a.go\n*** Add File: src/b.go\n*** End Patch"}`)
	effects := openCodeEffects("apply_patch", input, false)
	if len(effects) != 2 || effects[0].RawIdentity != "src/a.go" || effects[0].Operation != "update" ||
		effects[1].RawIdentity != "src/b.go" || effects[1].Operation != "write" {
		t.Fatalf("patch effects=%+v", effects)
	}
}

func TestOpenCodeLogicalSessionSummaryAndTranscript(t *testing.T) {
	path, id := openCodeFixture(t)
	records, err := (opencodeRuntime{}).ListSessionRecords()
	if err != nil || len(records) != 1 {
		t.Fatalf("records=%+v err=%v", records, err)
	}
	record := records[0]
	if record.Ref.Source != path || record.Ref.Segment != id || record.Summary.Provider != "ollama" ||
		record.Summary.Model != "qwen2.5-coder:7b" || record.Summary.ParentID != "ses_parent" {
		t.Fatalf("unexpected record: %+v", record)
	}
	events, unparsed, usage, err := (opencodeRuntime{}).NormalizeSession(record.Ref, false)
	if err != nil || unparsed != 0 || len(events) != 5 {
		t.Fatalf("events=%+v unparsed=%d usage=%+v err=%v", events, unparsed, usage, err)
	}
	if events[1].Kind != "tool_call" || events[2].Kind != "tool_result" || usage.InputTokens != 12 {
		t.Fatalf("canonical transcript mismatch: events=%+v usage=%+v", events, usage)
	}
}

func TestOpenCodeLifecyclePagesTerminalToolsWithoutFakePaths(t *testing.T) {
	path, id := openCodeFixture(t)
	first, err := (opencodeRuntime{}).NormalizeLogicalLifecycle(LogicalLifecycleReadRequest{
		Source: path, Segment: id, SessionID: id, MaxRecords: 1, RetainEffectBodies: true})
	if err != nil || !first.Continuation || len(first.Batch.Actions) != 1 || len(first.Batch.Results) != 1 {
		t.Fatalf("first page=%+v err=%v", first, err)
	}
	if first.Batch.Actions[0].NativeCallID != "call_1" || len(first.Batch.Results[0].Effects) != 1 ||
		first.Batch.Results[0].Effects[0].RawIdentity != "src/a.go" {
		t.Fatalf("first canonical facts=%+v", first.Batch)
	}
	var cursor openCodeLifecycleCursor
	if json.Unmarshal(first.Cursor, &cursor) != nil || cursor.ID != "prt_tool_1" {
		t.Fatalf("cursor=%s", first.Cursor)
	}
	second, err := (opencodeRuntime{}).NormalizeLogicalLifecycle(LogicalLifecycleReadRequest{
		Source: path, Segment: id, SessionID: id, Cursor: first.Cursor, MaxRecords: 1})
	if err != nil || second.Continuation || len(second.Batch.Results) != 1 || second.Batch.Results[0].State != "failure" {
		t.Fatalf("second page=%+v err=%v", second, err)
	}
}

func TestLogicalFourthRuntimeDoesNotWidenRuntime(t *testing.T) {
	var _ Runtime = opencodeRuntime{}
	var _ SessionSource = opencodeRuntime{}
	var _ LogicalLifecycleSource = opencodeRuntime{}
}
