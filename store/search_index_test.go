package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ncruces/go-sqlite3/driver"
	"github.com/ncruces/go-sqlite3/ext/fts5"
)

var projectionTestTime = time.Date(2026, 8, 28, 18, 0, 0, 0, time.UTC)

func projectionFixture(runtime, sessionID, generation, title, text string,
	indexedAt time.Time) TranscriptProjection {
	return TranscriptProjection{
		Session: SessionRow{Vendor: runtime, ID: sessionID, CatalogID: "catalog-" + sessionID,
			ResumeID: "resume-" + sessionID, Path: "/opaque/" + sessionID,
			CWD: "/repo", Project: "repo", Title: title, Modified: indexedAt.Unix(), Turns: 2},
		Generation: generation, IndexedAt: indexedAt, SourceCount: 1,
		Documents: []SearchDocument{
			{Order: 0, Timestamp: indexedAt.Format(time.RFC3339Nano), Kind: "title",
				Text: title, Lineage: "title:" + runtime + ":" + sessionID},
			{Order: 1, Timestamp: indexedAt.Format(time.RFC3339Nano), Kind: "user",
				Text: text, Lineage: "event:user"},
		},
	}
}

func TestTranscriptProjectionForceAndConcurrentSameGenerationAreDistinct(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()

	first := projectionFixture("claude", "session", "g1", "First title", "banana first", projectionTestTime)
	result, err := ix.ReplaceTranscriptProjection(first)
	if err != nil || !result.Replaced || result.State.Generation != "g1" {
		t.Fatalf("first=%+v err=%v", result, err)
	}
	hits, err := ix.SearchEvents("banana", 10)
	if err != nil || len(hits) != 1 || hits[0].Title != "First title" ||
		hits[0].CatalogID != "catalog-session" || hits[0].ResumeID != "resume-session" ||
		hits[0].Project != "repo" || hits[0].Path == "" {
		t.Fatalf("enriched hits=%+v err=%v", hits, err)
	}

	forced := projectionFixture("claude", "session", "g1", "Forced title", "banana forced",
		projectionTestTime.Add(time.Minute))
	forced.ExpectedGeneration = result.State.Generation
	forced.ExpectedIndexedAt = result.State.IndexedAt
	forcedResult, err := ix.ReplaceTranscriptProjection(forced)
	if err != nil || !forcedResult.Replaced || forcedResult.State.IndexedAt != forced.IndexedAt {
		t.Fatalf("forced=%+v err=%v", forcedResult, err)
	}

	staleConcurrent := projectionFixture("claude", "session", "g1", "Stale writer",
		"banana stale", projectionTestTime.Add(2*time.Minute))
	staleConcurrent.ExpectedGeneration = result.State.Generation
	staleConcurrent.ExpectedIndexedAt = result.State.IndexedAt
	skipped, err := ix.ReplaceTranscriptProjection(staleConcurrent)
	if err != nil || skipped.Replaced || skipped.State.IndexedAt != forcedResult.State.IndexedAt {
		t.Fatalf("concurrent skip=%+v err=%v", skipped, err)
	}
	hits, err = ix.SearchEvents("forced", 10)
	if err != nil || len(hits) == 0 {
		t.Fatalf("concurrent writer changed documents: %+v err=%v", hits, err)
	}
	for _, hit := range hits {
		if hit.Title != "Forced title" || strings.Contains(hit.Text, "stale") {
			t.Fatalf("concurrent writer changed documents: %+v", hits)
		}
	}
	concurrentState, err := ix.RecordTranscriptProjectionLimitation(
		TranscriptProjectionKey{Runtime: "claude", SessionID: "session"},
		result.State.Generation, result.State.IndexedAt, projectionTestTime.Add(4*time.Minute),
		TranscriptProjectionLimitation{Kind: "source-unreadable"})
	if err != nil || concurrentState.IndexedAt != forcedResult.State.IndexedAt ||
		concurrentState.Limitation != nil {
		t.Fatalf("stale failure tainted concurrent success: state=%+v err=%v", concurrentState, err)
	}

	conflict := projectionFixture("claude", "session", "g2", "Conflicting title",
		"banana conflict", projectionTestTime.Add(3*time.Minute))
	conflict.ExpectedGeneration = result.State.Generation
	conflict.ExpectedIndexedAt = result.State.IndexedAt
	if _, err := ix.ReplaceTranscriptProjection(conflict); !errors.Is(err, ErrTranscriptProjectionChanged) {
		t.Fatalf("conflicting generation err=%v", err)
	}
}

func TestTranscriptProjectionFaultRollsBackDocumentsSessionAndMarker(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	first := projectionFixture("codex", "rollback", "g1", "Old title", "old-good phrase",
		projectionTestTime)
	created, err := ix.ReplaceTranscriptProjection(first)
	if err != nil {
		t.Fatal(err)
	}

	searchIndexReplacementTestHook = func(searchDocumentExecutor) error {
		return errors.New("injected before marker")
	}
	t.Cleanup(func() { searchIndexReplacementTestHook = nil })
	next := projectionFixture("codex", "rollback", "g2", "New title", "new-partial phrase",
		projectionTestTime.Add(time.Minute))
	next.ExpectedGeneration = created.State.Generation
	next.ExpectedIndexedAt = created.State.IndexedAt
	if _, err := ix.ReplaceTranscriptProjection(next); err == nil {
		t.Fatal("injected replacement unexpectedly succeeded")
	}
	searchIndexReplacementTestHook = nil

	oldHits, err := ix.SearchEvents("old-good", 10)
	if err != nil || len(oldHits) != 1 || oldHits[0].Title != "Old title" {
		t.Fatalf("old-good projection lost: %+v err=%v", oldHits, err)
	}
	newHits, err := ix.SearchEvents("new-partial", 10)
	if err != nil || len(newHits) != 0 {
		t.Fatalf("partial documents committed: %+v err=%v", newHits, err)
	}
	state, err := ix.transcriptProjectionState(TranscriptProjectionKey{Runtime: "codex", SessionID: "rollback"})
	if err != nil || state.Generation != "g1" || state.IndexedAt != created.State.IndexedAt {
		t.Fatalf("marker advanced: %+v err=%v", state, err)
	}
}

func TestSchemaV22MigratesPopulatedTranscriptAndMemorySearchContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.AppendEvent(EventRecord{TS: 7, SessionID: "primary", Runtime: "claude",
		Verb: "read", Tool: "Read", Origin: "live"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.db.Exec(`DROP TRIGGER search_document_ai;
		DROP TRIGGER search_document_ad;
		DROP TRIGGER search_document_au;
		DROP TABLE events_fts;
		DROP TABLE search_document;
		DROP TABLE transcript_index_projection;
		CREATE VIRTUAL TABLE events_fts USING fts5(vendor,session_id,ts,kind,text);
		INSERT INTO sessions(vendor,id,path,cwd,project,title,modified,turns)
			VALUES('claude','legacy','/legacy','/repo','repo','Legacy title',1,1);
		INSERT INTO events_fts VALUES('claude','legacy','1','user','sprint retro legacy');
		INSERT INTO events_fts VALUES('memory','memory-1','2','memory','remember migration token');
		CREATE TABLE unrelated_fixture(value TEXT);
		INSERT INTO unrelated_fixture VALUES('preserve me');
		PRAGMA user_version=21;`); err != nil {
		t.Fatal(err)
	}
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	for query, wantVendor := range map[string]string{"retro": "claude", "migration token": "memory"} {
		hits, searchErr := migrated.SearchEvents(query, 10)
		if searchErr != nil || len(hits) != 1 || hits[0].Vendor != wantVendor {
			t.Fatalf("query %q hits=%+v err=%v", query, hits, searchErr)
		}
	}
	var documents, indexed, primary, unrelated, version int
	if err := migrated.db.QueryRow(`SELECT COUNT(*) FROM search_document`).Scan(&documents); err != nil {
		t.Fatal(err)
	}
	if err := migrated.db.QueryRow(`SELECT COUNT(*) FROM events_fts`).Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	_ = migrated.db.QueryRow(`SELECT COUNT(*) FROM event`).Scan(&primary)
	_ = migrated.db.QueryRow(`SELECT COUNT(*) FROM unrelated_fixture WHERE value='preserve me'`).Scan(&unrelated)
	_ = migrated.db.QueryRow(`PRAGMA user_version`).Scan(&version)
	if documents != 2 || indexed != documents || primary != 1 || unrelated != 1 || version != SchemaVersion {
		t.Fatalf("documents=%d indexed=%d primary=%d unrelated=%d version=%d",
			documents, indexed, primary, unrelated, version)
	}
	states, err := migrated.TranscriptProjectionStates()
	if err != nil || len(states) != 1 || states[0].Generation != "" || !states[0].IndexedAt.IsZero() {
		t.Fatalf("legacy projection state=%+v err=%v", states, err)
	}
	var ftsSQL string
	if err := migrated.db.QueryRow(`SELECT sql FROM sqlite_schema WHERE name='events_fts'`).Scan(&ftsSQL); err != nil ||
		!strings.Contains(strings.ToLower(ftsSQL), "content='search_document'") {
		t.Fatalf("external FTS sql=%q err=%v", ftsSQL, err)
	}
	columns, err := columnSet(migrated.db, "sessions")
	if err != nil || !columns["catalog_id"] || !columns["resume_id"] {
		t.Fatalf("migrated session identity columns=%v err=%v", columns, err)
	}
}

func TestSchemaV22AddsExactIdentityColumnsToLegacySessionRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	raw, err := driver.Open("file:"+path, fts5.Register)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE sessions(
		vendor TEXT,id TEXT,path TEXT,cwd TEXT,project TEXT,title TEXT,
		modified INTEGER,turns INTEGER,PRIMARY KEY(vendor,id));
		INSERT INTO sessions VALUES('codex','native-thread','/rollout','/repo','repo','Legacy',1,1);
		CREATE VIRTUAL TABLE events_fts USING fts5(vendor,session_id,ts,kind,text);
		INSERT INTO events_fts VALUES('codex','native-thread','1','user','legacy identity token');
		PRAGMA user_version=21;`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	columns, err := columnSet(migrated.db, "sessions")
	if err != nil || !columns["catalog_id"] || !columns["resume_id"] {
		t.Fatalf("identity columns=%v err=%v", columns, err)
	}
	row, found, err := migrated.SessionByID("codex", "native-thread")
	if err != nil || !found || row.CatalogID != "" || row.ResumeID != "" || row.Title != "Legacy" {
		t.Fatalf("legacy row=%+v found=%v err=%v", row, found, err)
	}
	hits, err := migrated.SearchEvents("identity token", 10)
	if err != nil || len(hits) != 1 || hits[0].CatalogID != "" || hits[0].ResumeID != "" {
		t.Fatalf("legacy hits=%+v err=%v", hits, err)
	}
}

func TestSchemaV22MigrationFailureRollsBackLegacyContentAndVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ix.db.Exec(`DROP TRIGGER search_document_ai;
		DROP TRIGGER search_document_ad;
		DROP TRIGGER search_document_au;
		DROP TABLE events_fts;
		DROP TABLE search_document;
		DROP TABLE transcript_index_projection;
		CREATE VIRTUAL TABLE events_fts USING fts5(vendor,session_id,ts,kind,text);
		INSERT INTO events_fts VALUES('claude','legacy','1','user','rollback legacy token');
		PRAGMA user_version=21;`); err != nil {
		t.Fatal(err)
	}
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}
	migrationTestHook = func(schemaDB) error { return errors.New("injected v22 migration failure") }
	if _, err := Open(path); err == nil {
		t.Fatal("injected migration unexpectedly succeeded")
	}
	migrationTestHook = nil
	t.Cleanup(func() { migrationTestHook = nil })

	raw, err := driver.Open("file:"+path, fts5.Register)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var version, legacyHits, newTable int
	_ = raw.QueryRow(`PRAGMA user_version`).Scan(&version)
	_ = raw.QueryRow(`SELECT COUNT(*) FROM events_fts WHERE events_fts MATCH 'rollback'`).Scan(&legacyHits)
	_ = raw.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type='table' AND name='search_document'`).Scan(&newTable)
	if version != 21 || legacyHits != 1 || newTable != 0 {
		t.Fatalf("rollback version=%d legacy_hits=%d search_document=%d", version, legacyHits, newTable)
	}
}

func TestProjectionLimitationAndOrphansPreserveOldGoodAndMemory(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	keep := projectionFixture("claude", "keep", "g1", "Keep", "keep transcript", projectionTestTime)
	orphan := projectionFixture("claude", "orphan", "g1", "Orphan", "orphan transcript", projectionTestTime)
	keepResult, err := ix.ReplaceTranscriptProjection(keep)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ix.ReplaceTranscriptProjection(orphan); err != nil {
		t.Fatal(err)
	}
	primary, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := primary.AppendEvent(EventRecord{TS: 1, Runtime: "claude", SessionID: "orphan",
		Verb: "read", Tool: "Read", Origin: "live"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := primary.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := ix.IndexMemoryText("memory-keep", 1, "orphan memory survives"); err != nil {
		t.Fatal(err)
	}
	limited, err := ix.RecordTranscriptProjectionLimitation(
		TranscriptProjectionKey{Runtime: "claude", SessionID: "keep"},
		keepResult.State.Generation, keepResult.State.IndexedAt, projectionTestTime.Add(time.Minute),
		TranscriptProjectionLimitation{Kind: "source-too-large", ObservedBytes: 100, LimitBytes: 50})
	if err != nil || limited.Generation != keepResult.State.Generation || limited.Limitation == nil ||
		limited.Limitation.Kind != "source-too-large" {
		t.Fatalf("limited state=%+v err=%v", limited, err)
	}
	removed, err := ix.RemoveTranscriptProjectionOrphans([]TranscriptProjectionKey{{Runtime: "claude", SessionID: "keep"}})
	if err != nil || removed != 1 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	transcriptHits, _ := ix.SearchEvents("orphan transcript", 10)
	memoryHits, _ := ix.SearchEvents("orphan memory", 10)
	if len(transcriptHits) != 0 || len(memoryHits) != 1 || memoryHits[0].Vendor != "memory" {
		t.Fatalf("transcript=%+v memory=%+v", transcriptHits, memoryHits)
	}
	if row, found, err := ix.SessionByID("claude", "orphan"); err != nil || !found ||
		row.Title != "" || row.CatalogID != "" || row.ResumeID != "" {
		t.Fatalf("primary-referenced session root lost: row=%+v found=%v err=%v", row, found, err)
	}
	var plan string
	rows, err := ix.db.Query(`EXPLAIN QUERY PLAN DELETE FROM search_document
		WHERE owner_kind='transcript' AND vendor='claude' AND session_id='keep'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail
	}
	if !strings.Contains(plan, "search_document_owner") {
		t.Fatalf("owner delete is not indexed: %s", plan)
	}
}

func TestBusyReplacementPreservesOldGoodProjection(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	first := projectionFixture("opencode", "busy", "g1", "Busy old", "busy old-good",
		projectionTestTime)
	created, err := ix.ReplaceTranscriptProjection(first)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := ix.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Rollback() }()
	if _, err := lock.Exec(`UPDATE sessions SET title=title WHERE vendor='opencode' AND id='busy'`); err != nil {
		t.Fatal(err)
	}
	next := projectionFixture("opencode", "busy", "g2", "Busy new", "busy new-partial",
		projectionTestTime.Add(time.Minute))
	next.ExpectedGeneration = created.State.Generation
	next.ExpectedIndexedAt = created.State.IndexedAt
	if _, err := ix.ReplaceTranscriptProjection(next); err == nil {
		t.Fatal("contended replacement unexpectedly succeeded")
	}
	if err := lock.Rollback(); err != nil {
		t.Fatal(err)
	}
	oldHits, err := ix.SearchEvents("old-good", 10)
	if err != nil || len(oldHits) != 1 {
		t.Fatalf("old-good projection lost under busy: %+v err=%v", oldHits, err)
	}
	newHits, err := ix.SearchEvents("new-partial", 10)
	if err != nil || len(newHits) != 0 {
		t.Fatalf("busy committed partial documents: %+v err=%v", newHits, err)
	}
}
