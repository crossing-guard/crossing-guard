package store

import (
	"path/filepath"
	"testing"
)

func TestSchemaV8ObservationEvidenceAndCheckpoint(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	var version int
	if err := ix.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("schema version=%d err=%v", version, err)
	}
	for _, table := range []string{"event_delivery", "event_input", "result_observation", "result_effect",
		"result_reconciliation", "result_reconciliation_candidate", "transcript_cursor",
		"session_checkpoint", "session_checkpoint_trigger", "checkpoint_payload", "evidence_body",
		"checkpoint_payload_body", "path_reconciliation", "collection_issue"} {
		var found int
		if err := ix.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&found); err != nil || found != 1 {
			t.Fatalf("table %s found=%d err=%v", table, found, err)
		}
	}
	cols, err := columnSet(ix.db, "event_resource")
	if err != nil {
		t.Fatal(err)
	}
	deliveryCols, err := columnSet(ix.db, "event_delivery")
	if err != nil || !deliveryCols["action_id"] {
		t.Fatalf("event_delivery action_id present=%v err=%v", deliveryCols["action_id"], err)
	}
	for _, col := range []string{"raw_identity", "operation", "evidence_class", "source_field", "completeness"} {
		if !cols[col] {
			t.Fatalf("event_resource missing %s", col)
		}
	}
	issueCols, err := columnSet(ix.db, "collection_issue")
	if err != nil || !issueCols["resolution_class"] {
		t.Fatalf("collection_issue resolution_class present=%v err=%v", issueCols["resolution_class"], err)
	}
}

func TestObservationEvidenceIdempotenceAndCollision(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	eventID, err := tx.AppendEvent(EventRecord{TS: 1, SessionID: "s", Tool: "Edit", Origin: "live"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.AppendEventDelivery(EventDelivery{EventID: eventID, ObservationID: "obs_a", ObservationSchema: "pretool-observation-v1", EnvelopeDigest: "sha256-v1:a", CollectorID: "guardcli-pretool", ReceivedAt: 1, DeliveryAttempts: 1, DeliveryMode: "direct"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.AppendEventInput(EventInput{EventID: eventID, MediaType: "application/json; charset=utf-8", RawBytes: 2, CapturedBytes: 2, Digest: "sha256-v1:x", Completeness: "complete", Payload: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	gotID, gotDigest, found, err := ix.ObservationIdentity("obs_a")
	if err != nil || !found || gotID != eventID || gotDigest != "sha256-v1:a" {
		t.Fatalf("identity=(%d,%q,%v) err=%v", gotID, gotDigest, found, err)
	}
	dup, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	defer dup.Rollback()
	if err := dup.AppendEventDelivery(EventDelivery{EventID: eventID, ObservationID: "obs_a", ObservationSchema: "pretool-observation-v1", EnvelopeDigest: "sha256-v1:b", CollectorID: "guardcli-pretool", ReceivedAt: 2, DeliveryAttempts: 1, DeliveryMode: "direct"}); err == nil {
		t.Fatal("observation id collision accepted")
	}
}

func TestOneAttachmentCheckpointPerSessionScope(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	tx, _ := ix.BeginGov()
	eventID, _ := tx.AppendEvent(EventRecord{TS: 1, SessionID: "s", Tool: "Read", Origin: "live"}, nil)
	first, created, err := tx.EnsureSessionCheckpoint(SessionCheckpoint{SessionID: "s", ScopeKey: "cwd:/repo", Kind: "attachment", TriggerEventID: eventID, TriggerObservationID: "obs_a", WorkingDirectory: "/repo", Status: "pending", BoundaryClass: "unconfirmed", RequestedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	second, createdAgain, err := tx.EnsureSessionCheckpoint(SessionCheckpoint{SessionID: "s", ScopeKey: "cwd:/repo", Kind: "attachment", TriggerEventID: eventID, TriggerObservationID: "obs_b", WorkingDirectory: "/repo", Status: "pending", BoundaryClass: "unconfirmed", RequestedAt: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !created || createdAgain || first.ID != second.ID || second.TriggerObservationID != "obs_a" {
		t.Fatalf("checkpoint first=%+v/%v second=%+v/%v", first, created, second, createdAgain)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestCollectionIssueCoalescesWithoutRawDetail(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	issue := CollectionIssue{IssueID: "issue_a", SessionID: "s", ObservationID: "obs_a",
		CollectorID: "daemon-replay", Kind: "checkpoint-failed", AffectedCount: 1,
		FirstSeen: 1, LastSeen: 1, DetailDigest: "sha256-v1:x"}
	if err := ix.RecordCollectionIssue(issue); err != nil {
		t.Fatal(err)
	}
	issue.LastSeen = 2
	if err := ix.RecordCollectionIssue(issue); err != nil {
		t.Fatal(err)
	}
	issues, err := ix.CollectionIssuesForSession("s", 10)
	if err != nil || len(issues) != 1 || issues[0].AffectedCount != 2 || issues[0].LastSeen != 2 {
		t.Fatalf("issues=%+v err=%v", issues, err)
	}
	if err := ix.ResolveCollectionIssue(issue.IssueID, 30); err != nil {
		t.Fatal(err)
	}
	if err := ix.ResolveCollectionIssue(issue.IssueID, 40); err != nil {
		t.Fatal(err)
	}
	issues, err = ix.CollectionIssuesForSession("s", 10)
	if err != nil || issues[0].ResolvedAt != 30 || issues[0].ResolutionClass != "recovered" {
		t.Fatalf("resolved issues=%+v err=%v", issues, err)
	}
}

func TestV6ToV7MigrationPreservesLegacyResourceWithoutInventingEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	tx, _ := ix.BeginGov()
	if err := tx.UpsertEntity("file:/repo/a.go", "file", "/repo/a.go", 1); err != nil {
		t.Fatal(err)
	}
	eventID, err := tx.AppendEvent(EventRecord{TS: 1, SessionID: "legacy", Tool: "Edit", Origin: "live"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.AppendEventResource(eventID, 0, "file:/repo/a.go", "tool_input.command"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, ddl := range []string{
		`DROP TABLE collection_issue`, `DROP TABLE session_checkpoint`, `DROP TABLE event_input`, `DROP TABLE event_delivery`,
		`ALTER TABLE event_resource DROP COLUMN completeness`, `ALTER TABLE event_resource DROP COLUMN source_field`,
		`ALTER TABLE event_resource DROP COLUMN evidence_class`, `ALTER TABLE event_resource DROP COLUMN operation`,
		`ALTER TABLE event_resource DROP COLUMN raw_identity`, `PRAGMA user_version=6`,
	} {
		if _, err := ix.db.Exec(ddl); err != nil {
			t.Fatalf("downgrade fixture %q: %v", ddl, err)
		}
	}
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}
	ix, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	resources, _, err := ix.EventResourcesForSession("legacy", 10, 10)
	if err != nil || len(resources) != 1 {
		t.Fatalf("resources=%+v err=%v", resources, err)
	}
	got := resources[0]
	if got.RawIdentity != "" || got.Operation != "unknown" || got.EvidenceClass != "unknown" || got.SourceField != "" || got.Completeness != "unknown" {
		t.Fatalf("legacy evidence was invented: %+v", got)
	}
}

func TestPruneDeletesEventChildrenButRetainsAttachmentCheckpoint(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	tx, _ := ix.BeginGov()
	eventID, err := tx.AppendEvent(EventRecord{TS: 1, SessionID: "s", Runtime: "codex", Tool: "Read", Origin: "live"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.AppendEventDelivery(EventDelivery{EventID: eventID, ObservationID: "obs_prune", ObservationSchema: "pretool-observation-v1", EnvelopeDigest: "sha256-v1:a", CollectorID: "guardcli-pretool", NativeCallID: "call-prune", NativeCallKind: "call_id", ReceivedAt: 1, DeliveryAttempts: 1, DeliveryMode: "direct"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.AppendEventInput(EventInput{EventID: eventID, MediaType: "application/json; charset=utf-8", Completeness: "unavailable"}); err != nil {
		t.Fatal(err)
	}
	checkpoint, _, err := tx.EnsureSessionCheckpoint(SessionCheckpoint{SessionID: "s", ScopeKey: "cwd:/repo", Kind: "attachment", TriggerEventID: eventID, TriggerObservationID: "obs_prune", WorkingDirectory: "/repo", Status: "pending", BoundaryClass: "unconfirmed", RequestedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	resultID, _, err := ix.AppendResultObservation(ResultObservation{
		ObservationID: "res_prune", SessionID: "s", Runtime: "codex", Tool: "Read",
		NativeCallID: "call-prune", NativeCallKind: "call_id", SourceKind: "live-post-tool",
		SourceDigest: "sha256-v1:result", State: "success", Completeness: "metadata-only",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if joined, err := ix.ReconcileResultObservation(resultID, 1); err != nil || joined.JoinClass != "exact" {
		t.Fatalf("initial result=%+v err=%v", joined, err)
	}
	if n, err := ix.PruneEventsBefore(2); err != nil || n != 1 {
		t.Fatalf("prune=%d err=%v", n, err)
	}
	got, err := ix.SessionCheckpointByID(checkpoint.ID)
	if err != nil || got.TriggerEventID != 0 || got.TriggerObservationID != "obs_prune" {
		t.Fatalf("checkpoint=%+v err=%v", got, err)
	}
	joined, err := ix.ReconcileResultObservation(resultID, 2)
	if err != nil || joined.JoinClass != "unjoined" {
		t.Fatalf("result after prune=%+v err=%v", joined, err)
	}
}

func TestDeleteImportedRemovesObservationChildrenAndPreservesLive(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	for _, fixture := range []struct {
		origin, observationID string
	}{{"imported", "obs_imported"}, {"live", "obs_live"}} {
		tx, err := ix.BeginGov()
		if err != nil {
			t.Fatal(err)
		}
		eventID, err := tx.AppendEvent(EventRecord{TS: 1, SessionID: "s", Tool: "Read", Origin: fixture.origin}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.AppendEventDelivery(EventDelivery{EventID: eventID, ObservationID: fixture.observationID,
			ObservationSchema: "pretool-observation-v1", EnvelopeDigest: "sha256-v1:a",
			CollectorID: "test", ReceivedAt: 1, DeliveryAttempts: 1, DeliveryMode: "direct"}); err != nil {
			t.Fatal(err)
		}
		if err := tx.AppendEventInput(EventInput{EventID: eventID, MediaType: "application/json; charset=utf-8", Completeness: "unavailable"}); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := ix.DeleteImportedForSession("s"); err != nil || n != 1 {
		t.Fatalf("deleted=%d err=%v", n, err)
	}
	if _, _, found, err := ix.ObservationEvidence("obs_imported"); err != nil || found {
		t.Fatalf("imported child found=%v err=%v", found, err)
	}
	if _, _, found, err := ix.ObservationEvidence("obs_live"); err != nil || !found {
		t.Fatalf("live child found=%v err=%v", found, err)
	}
}

func TestObservationEvidenceSurvivesExportAndReadOnlyReopen(t *testing.T) {
	dir := t.TempDir()
	ix, err := Open(filepath.Join(dir, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	tx, _ := ix.BeginGov()
	eventID, err := tx.AppendEvent(EventRecord{TS: 1, SessionID: "export", Tool: "Edit", Origin: "live"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.AppendEventDelivery(EventDelivery{EventID: eventID, ObservationID: "obs_export", ObservationSchema: "pretool-observation-v1", EnvelopeDigest: "sha256-v1:a", CollectorID: "guardcli-pretool", ReceivedAt: 1, DeliveryAttempts: 1, DeliveryMode: "direct"}); err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"future":true}`)
	if err := tx.AppendEventInput(EventInput{EventID: eventID, MediaType: "application/json; charset=utf-8", RawBytes: len(payload), CapturedBytes: len(payload), Digest: "sha256-v1:x", Completeness: "complete", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "export.sqlite")
	if err := ix.Export(dest); err != nil {
		t.Fatal(err)
	}
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}
	copy, err := OpenRO(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer copy.Close()
	_, input, found, err := copy.ObservationEvidence("obs_export")
	if err != nil || !found || string(input.Payload) != string(payload) {
		t.Fatalf("input=%+v found=%v err=%v", input, found, err)
	}
}
