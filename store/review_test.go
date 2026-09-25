package store

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionStatementsAreCompositeBoundedAndDisplayOnly(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	long := strings.Repeat("界", 200)
	if _, err := ix.ReplaceTranscriptProjection(TranscriptProjection{
		Session: SessionRow{Vendor: "runtime-a", ID: "same"}, Generation: "g-a",
		IndexedAt: projectionTestTime, SourceCount: 1, Documents: []SearchDocument{
			{Order: 0, Kind: "title", Text: "Review A"},
			{Order: 1, Timestamp: "1", Kind: "user", Text: "continue"},
			{Order: 2, Timestamp: "2", Kind: "tool_call", Text: "not a statement"},
			{Order: 3, Timestamp: "3", Kind: "assistant", Text: long},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.ReplaceTranscriptProjection(TranscriptProjection{
		Session: SessionRow{Vendor: "runtime-b", ID: "same"}, Generation: "g-b",
		IndexedAt: projectionTestTime, SourceCount: 1, Documents: []SearchDocument{
			{Order: 0, Kind: "title", Text: "Review B"},
			{Order: 1, Timestamp: "4", Kind: "user", Text: "other runtime"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	page, err := ix.SessionStatements("runtime-a", "same", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || len(page.Statements) != 1 || page.Statements[0].Role != "assistant" ||
		!page.Statements[0].SnippetTruncated || page.Statements[0].SourceState != "rebuildable-storage-sanitized-index" {
		t.Fatalf("statement page = %+v", page)
	}
	if !strings.HasSuffix(page.Statements[0].Snippet, "…") || len(page.Statements[0].Snippet) > maxStatementSnippet+len("…") {
		t.Fatalf("snippet was not UTF-8 bounded: %q (%d bytes)", page.Statements[0].Snippet, len(page.Statements[0].Snippet))
	}
	missingRuntime, err := ix.SessionStatements("", "same", 0, 25)
	if err != nil || missingRuntime.Total != 0 || len(missingRuntime.Statements) != 0 {
		t.Fatalf("runtime-less composite lookup leaked rows: %+v err=%v", missingRuntime, err)
	}
}

func TestTaggedEventsAndResultLinksUseFrozenFacts(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	tags := `[{"key":"test","value":"run","detector":"test.run","provenance":"observed","scope":"session"}]`
	eventID, err := tx.AppendEvent(EventRecord{TS: 1, SessionID: "checks", Runtime: "runtime-a",
		Verb: "exec", Tool: "shell", Tags: tags, Decision: "allow", Origin: "live"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.AppendEventDelivery(EventDelivery{EventID: eventID, ObservationID: "obs-check",
		ObservationSchema: "fixture", EnvelopeDigest: "sha256-v1:action", CollectorID: "fixture",
		NativeCallID: "call-check", NativeCallKind: "call-id", ReceivedAt: 1,
		DeliveryAttempts: 1, DeliveryMode: "direct"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.AppendEvent(EventRecord{TS: 2, SessionID: "checks", Runtime: "runtime-a",
		Verb: "exec", Tool: "shell", Tags: "not-json", Decision: "allow", Origin: "live"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	page, err := ix.TaggedEventsForSession("checks", []EventTagValue{{Key: "test", Value: "run"}, {Key: "build", Value: "run"}}, 0, 25)
	if err != nil || page.Total != 1 || len(page.Events) != 1 || page.Events[0].ID != eventID {
		t.Fatalf("tagged page = %+v err=%v", page, err)
	}
	resultID, _, err := ix.AppendResultObservation(ResultObservation{ObservationID: "res-check",
		SessionID: "checks", Runtime: "runtime-a", Tool: "shell", NativeCallID: "call-check",
		NativeCallKind: "call-id", SourceKind: "live-post-tool", SourceDigest: "sha256-v1:result",
		State: "success", CompletedAt: 3, DurationMS: 25, RawBytes: 20, RetainedBytes: 0,
		StdoutBytes: 10, Completeness: "metadata-only"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ix.ReconcileResultObservation(resultID, 4); err != nil || got.JoinClass != "exact" {
		t.Fatalf("reconciliation = %+v err=%v", got, err)
	}
	links, total, err := ix.ResultLinksForEvents([]int64{eventID})
	if err != nil || total != 1 || len(links) != 1 || !links[0].Selected ||
		links[0].JoinClass != "exact" || links[0].DurationMS != 25 || links[0].StdoutBytes != 10 {
		t.Fatalf("result links = %+v total=%d err=%v", links, total, err)
	}
}
