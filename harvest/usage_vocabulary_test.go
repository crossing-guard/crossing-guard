package harvest

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ncruces/go-sqlite3/driver"
)

func i64(v int64) *int64 { return &v }

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// fakegamma: calls with an other class, reasoning on one call only, and cost in
// two units (design §10). The fold must sum without interpreting any of it.
func TestFoldCallsIsNeutral(t *testing.T) {
	at := func(minute int) time.Time { return time.Date(2026, 9, 24, 10, minute, 0, 0, time.UTC) }
	calls := []UsageCall{
		{ID: "c2", At: at(2), Model: "gamma-b", TokenClasses: TokenClasses{Output: i64(7), Reasoning: i64(2)},
			Cost: &Cost{Amount: 0.5, Unit: "USD", Basis: CostBasisRuntime}},
		{ID: "c1", At: at(1), Model: "gamma-a", ContextWindow: i64(1000),
			TokenClasses: TokenClasses{Input: i64(10), CacheRead: i64(30)},
			Other:        []TokenCount{{ID: "audio-in", Label: "Audio in", Side: TokenSideInput, Count: 5}},
			Cost:         &Cost{Amount: 3, Unit: "credits", Basis: CostBasisRuntime}},
		{ID: "c3", At: at(3), TokenClasses: TokenClasses{Input: i64(1), CacheWrite: i64(4)}},
	}
	usage := FoldCalls(calls)
	if usage.Turns != 3 || usage.InputTokens != 11 || usage.CacheRead != 30 || usage.CacheCreate != 4 ||
		usage.OutputTokens != 7 {
		t.Fatalf("classes: %+v", usage)
	}
	if usage.Reasoning == nil || *usage.Reasoning != 2 || usage.ReasoningStatedCalls != 1 {
		t.Fatalf("reasoning is summed only where stated, and says how often: %+v", usage)
	}
	if usage.Cost != nil {
		t.Fatalf("two units cannot become one cost: %+v", usage.Cost)
	}
	if len(usage.Other) != 1 || usage.Other[0].Count != 5 {
		t.Fatalf("other classes are summed by id: %+v", usage.Other)
	}
	// Levels come from the latest call by time, not the slice order.
	if usage.Model != "gamma-b" || usage.Context != 5 || usage.ContextWindow != 1000 {
		t.Fatalf("levels: model=%q context=%d window=%d", usage.Model, usage.Context, usage.ContextWindow)
	}
	if FoldCalls(nil) != nil {
		t.Fatal("no calls is no telemetry, not zeros")
	}
}

func TestAdmitUsageCallRules(t *testing.T) {
	stamped := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	if _, ok := AdmitUsageCall(UsageCall{ID: "untimed", TokenClasses: TokenClasses{Output: i64(3)}}); ok {
		t.Fatal("a call with no time cannot be placed or ordered")
	}
	if _, ok := AdmitUsageCall(UsageCall{ID: "zero", At: stamped, TokenClasses: TokenClasses{Input: i64(0), Output: i64(0)}}); ok {
		t.Fatal("an all-zero placeholder is not a call")
	}
	if _, ok := AdmitUsageCall(UsageCall{ID: "unstated", At: stamped}); ok {
		t.Fatal("a call that states no class is not a call")
	}
	call, ok := AdmitUsageCall(UsageCall{ID: "odd", At: stamped, TokenClasses: TokenClasses{Output: i64(3), Reasoning: i64(9)}})
	if !ok || call.Reasoning != nil {
		t.Fatalf("reasoning over output cannot be a part of it: %+v ok=%v", call, ok)
	}
}

func TestProjectionMergeKeepsMixedCostUnknown(t *testing.T) {
	merged := mergeProjectionUsage(nil, &SessionUsage{InputTokens: 1, Cost: &Cost{Amount: 1, Unit: "USD", Basis: CostBasisRuntime}})
	merged = mergeProjectionUsage(merged, &SessionUsage{InputTokens: 2, Reasoning: i64(4),
		Cost: &Cost{Amount: 2, Unit: "USD", Basis: CostBasisRuntime}})
	if merged.InputTokens != 3 || *merged.Reasoning != 4 || merged.Cost.Amount != 3 {
		t.Fatalf("merged: %+v", merged)
	}
	merged = mergeProjectionUsage(merged, &SessionUsage{Cost: &Cost{Amount: 9, Unit: "credits", Basis: CostBasisRuntime}})
	if merged.Cost != nil {
		t.Fatalf("mixed units must become unknown, not a sum: %+v", merged.Cost)
	}
}

// A dedicated store (not the shared single-session fixture; NULL-model plan
// RT3): cost, reasoning apart from output (measured on 1.18.0), two UTC dates.
func TestOpenCodeHistoryStatesCostReasoningAndContext(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode.db")
	db, err := driver.Open("file:" + path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	msg := func(id string, created int64, input, output, reasoning, read, write int64) string {
		return `('` + id + `','ses_cost',` + itoa(created) + `,` + itoa(created) + `,'{"role":"assistant","time":{"created":` +
			itoa(created) + `},"tokens":{"total":` + itoa(input+output+reasoning+read+write) + `,"input":` + itoa(input) +
			`,"output":` + itoa(output) + `,"reasoning":` + itoa(reasoning) + `,"cache":{"read":` + itoa(read) +
			`,"write":` + itoa(write) + `}},"cost":0.01,"providerID":"p","modelID":"m"}')`
	}
	_, err = db.Exec(`
CREATE TABLE session(id TEXT PRIMARY KEY,project_id TEXT NOT NULL,workspace_id TEXT,parent_id TEXT,
 slug TEXT NOT NULL,directory TEXT NOT NULL,path TEXT,title TEXT NOT NULL,version TEXT NOT NULL,
 cost REAL NOT NULL DEFAULT 0,tokens_input INTEGER NOT NULL DEFAULT 0,tokens_output INTEGER NOT NULL DEFAULT 0,
 tokens_reasoning INTEGER NOT NULL DEFAULT 0,tokens_cache_read INTEGER NOT NULL DEFAULT 0,
 tokens_cache_write INTEGER NOT NULL DEFAULT 0,model TEXT,time_created INTEGER NOT NULL,time_updated INTEGER NOT NULL);
CREATE TABLE message(id TEXT PRIMARY KEY,session_id TEXT NOT NULL,time_created INTEGER NOT NULL,
 time_updated INTEGER NOT NULL,data TEXT NOT NULL);
CREATE TABLE part(id TEXT PRIMARY KEY,message_id TEXT NOT NULL,session_id TEXT NOT NULL,
 time_created INTEGER NOT NULL,time_updated INTEGER NOT NULL,data TEXT NOT NULL);
INSERT INTO session(id,project_id,slug,directory,title,version,model,cost,tokens_input,tokens_output,
 tokens_reasoning,tokens_cache_read,tokens_cache_write,time_created,time_updated) VALUES
 ('ses_cost','p','c','/work/repo','Costed','1.18.0','{"id":"m","providerID":"p"}',0.03,300,30,9,700,5,1000,90000000);
INSERT INTO session(id,project_id,slug,directory,title,version,model,time_created,time_updated) VALUES
 ('ses_empty','p','e','/work/repo','Never answered','1.18.0',NULL,1000,2000);
INSERT INTO message VALUES ` + msg("m1", 86399000, 100, 10, 3, 200, 5) + `,` + msg("m2", 86401000, 150, 10, 3, 250, 0) +
		`,` + msg("m3", 86402000, 50, 10, 3, 250, 0) + `,` + msg("m4", 86403000, 0, 0, 0, 0, 0) + `;`)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENCODE_DATA_HOME", dir)
	records, err := listOpenCodeProjectionRecords(context.Background(), path, "", false)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]SessionRecord{}
	for _, record := range records {
		byID[record.Record.Summary.ID] = record.Record
	}
	if context := byID["ses_cost"].Summary.Context; context != 50+250 {
		t.Fatalf("context is the last call's input plus cache, not a sum: %d", context)
	}
	_, _, readUsage, err := (opencodeRuntime{}).NormalizeSession(byID["ses_cost"].Ref, false)
	if err != nil || readUsage.Cost == nil || readUsage.Context != 300 || *readUsage.Reasoning != 9 {
		t.Fatalf("read path: %+v err=%v", readUsage, err)
	}
	if readUsage.OutputTokens != 39 {
		t.Fatalf("output must include reasoning, which OpenCode counts apart: %+v", readUsage)
	}
	refs, err := (opencodeRuntime{}).UsageSources(context.Background())
	if err != nil || len(refs) != 2 {
		t.Fatalf("usage sources: %+v err=%v", refs, err)
	}
	var source UsageSourceRef
	for _, ref := range refs {
		if ref.Session == "ses_cost" {
			source = ref
		}
	}
	batch, err := (opencodeRuntime{}).ReadUsage(context.Background(), UsageReadRequest{Source: source})
	// m4 is still in flight (all zeros): not a call yet, recorded on a later read.
	if err != nil || !batch.Complete || len(batch.Calls) != 3 {
		t.Fatalf("calls: %+v err=%v", batch, err)
	}
	first := batch.Calls[0]
	if first.ID != "m1" || *first.Output != 13 || *first.Reasoning != 3 || *first.Input != 100 ||
		*first.CacheRead != 200 || *first.CacheWrite != 5 || first.Model != "p/m" || first.Client != "1.18.0" ||
		first.Cost == nil || first.Cost.Amount != 0.01 {
		t.Fatalf("a call maps one message: %+v", first)
	}
}
