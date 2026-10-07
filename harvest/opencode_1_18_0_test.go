package harvest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// Conversation text is the transcript: a long prompt and a long reply come back
// whole in both read modes, as the Claude and Codex adapters return them. Text
// OpenCode generated itself (a synthetic attached-file part) and text of an
// unknown role keep the bookkeeping budget; step rows are unchanged. Only
// lengths change: one event per part, sequence numbers contiguous, so pins
// taken on Seq are unaffected.
func TestOpenCodeConversationTextIsWholeAndBookkeepingStaysClipped(t *testing.T) {
	path, id := openCodeFixture(t)
	db, err := driver.Open("file:" + path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	prompt := "  " + strings.Repeat("Please compare the providers. ", 40) + "\n"
	reply := strings.Repeat("Profile pricing is per account, not per profile. ", 60)
	attached := strings.Repeat("1: line of an attached file\n", 60)
	partJSON := func(fields map[string]any) string {
		encoded, _ := json.Marshal(fields)
		return string(encoded)
	}
	for _, row := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO message VALUES('msg_user_2','ses_child',5000,5000,'{"role":"user","time":{"created":5000}}')`, nil},
		{`INSERT INTO message VALUES('msg_assistant_2','ses_child',5100,5900,'{"role":"assistant","time":{"created":5100}}')`, nil},
		{`INSERT INTO message VALUES('msg_system','ses_child',6000,6000,'{"role":"system","time":{"created":6000}}')`, nil},
		{`INSERT INTO part VALUES('prt_prompt','msg_user_2','ses_child',5000,5000,?)`, []any{partJSON(map[string]any{"type": "text", "text": prompt})}},
		{`INSERT INTO part VALUES('prt_attached','msg_user_2','ses_child',5001,5001,?)`, []any{partJSON(map[string]any{"type": "text", "text": attached, "synthetic": true})}},
		{`INSERT INTO part VALUES('prt_step','msg_assistant_2','ses_child',5100,5100,?)`, []any{partJSON(map[string]any{"type": "step-start"})}},
		{`INSERT INTO part VALUES('prt_reply','msg_assistant_2','ses_child',5200,5200,?)`, []any{partJSON(map[string]any{"type": "text", "text": reply})}},
		{`INSERT INTO part VALUES('prt_system','msg_system','ses_child',6000,6000,?)`, []any{partJSON(map[string]any{"type": "text", "text": strings.Repeat("system note ", 60)})}},
	} {
		if _, err := db.Exec(row.query, row.args...); err != nil {
			t.Fatal(err)
		}
	}
	ref := SessionRef{Runtime: opencodeRuntimeName, ID: id, Source: path, Segment: id}
	for _, full := range []bool{false, true} {
		events, unparsed, _, err := (opencodeRuntime{}).NormalizeSession(ref, full)
		if err != nil || unparsed != 0 || len(events) != 10 {
			t.Fatalf("full=%v: events=%d unparsed=%d err=%v", full, len(events), unparsed, err)
		}
		for index, event := range events {
			if event.Seq != index {
				t.Fatalf("full=%v: sequence must stay contiguous, event %d has Seq %d", full, index, event.Seq)
			}
		}
		byText := func(prefix string) CanonicalEvent {
			for _, event := range events {
				if strings.HasPrefix(event.Text, prefix) {
					return event
				}
			}
			t.Fatalf("full=%v: no event starts with %q", full, prefix)
			return CanonicalEvent{}
		}
		if got := byText("Please compare"); got.Kind != "user" || got.Text != strings.TrimSpace(prompt) || got.FullLen != 0 {
			t.Fatalf("full=%v: the prompt must come back whole: kind=%s len=%d full_len=%d", full, got.Kind, len(got.Text), got.FullLen)
		}
		if got := byText("Profile pricing"); got.Kind != "assistant" || got.Text != strings.TrimSpace(reply) || got.FullLen != 0 {
			t.Fatalf("full=%v: the reply must come back whole: kind=%s len=%d full_len=%d", full, got.Kind, len(got.Text), got.FullLen)
		}
		if full {
			continue // the deep read lifts every budget; bookkeeping clipping is a whole-transcript rule
		}
		if got := byText("1: line of an attached file"); got.Kind != "user" || got.FullLen != len(strings.TrimSpace(attached)) || len(got.Text) > transcriptCaps.meta+len("…") {
			t.Fatalf("a synthetic part keeps the bookkeeping budget: kind=%s len=%d full_len=%d", got.Kind, len(got.Text), got.FullLen)
		}
		if got := byText("system note"); got.Kind != "other" || got.FullLen == 0 {
			t.Fatalf("an unknown role keeps the bookkeeping budget: kind=%s full_len=%d", got.Kind, got.FullLen)
		}
		steps := 0
		for _, event := range events {
			if event.Name == "step-start" && event.Kind == "other" && event.Text == "prt_step" {
				steps++
			}
		}
		if steps != 1 {
			t.Fatalf("the step row must be unchanged: %d matching rows", steps)
		}
	}
}

// Stored rows the console can draw live carry their part id as TurnAnchor, the
// same identity OpenCode's live stream sends, so the console skips the stored
// copy of what it drew. A tool call and its result share their part's id.
// Rows the console never draws live (user text, synthetic text, steps) carry
// none, so no stored row can be hidden by a mark it was never meant to match.
func TestOpenCodeDrawableRowsAnchorOnTheirPartID(t *testing.T) {
	path, id := openCodeFixture(t)
	db, err := driver.Open("file:" + path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, query := range []string{
		`INSERT INTO part VALUES('prt_reply','msg_assistant','ses_child',4950,4950,'{"type":"text","text":"done"}')`,
		`INSERT INTO part VALUES('prt_reason','msg_assistant','ses_child',1900,1900,'{"type":"reasoning","text":"think"}')`,
		`INSERT INTO part VALUES('prt_step','msg_assistant','ses_child',1950,1950,'{"type":"step-start"}')`,
		`INSERT INTO part VALUES('prt_synth','msg_user','ses_child',1101,1101,'{"type":"text","text":"file body","synthetic":true}')`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	events, _, _, err := (opencodeRuntime{}).NormalizeSession(SessionRef{Runtime: opencodeRuntimeName, ID: id, Source: path, Segment: id}, false)
	if err != nil {
		t.Fatal(err)
	}
	anchors := map[string][]string{}
	for _, event := range events {
		key := event.Kind + ":" + event.Name
		anchors[key] = append(anchors[key], event.TurnAnchor)
	}
	want := map[string][]string{
		"user:":             {"", ""},
		"thinking:":         {"prt_reason"},
		"other:step-start":  {""},
		"tool_call:write":   {"prt_tool_1"},
		"tool_result:write": {"prt_tool_1"},
		"tool_call:bash":    {"prt_tool_2"},
		"tool_result:bash":  {"prt_tool_2"},
		"assistant:":        {"prt_reply"},
	}
	for key, values := range want {
		if strings.Join(anchors[key], ",") != strings.Join(values, ",") {
			t.Fatalf("%s anchors = %q, want %q (all: %v)", key, anchors[key], values, anchors)
		}
	}
}
