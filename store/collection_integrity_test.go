package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func collectionIntegrityCheckpoint(t *testing.T, ix *Index, requestID string) SessionCheckpoint {
	t.Helper()
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, _, err := tx.EnsureSessionCheckpoint(SessionCheckpoint{SessionID: requestID,
		ScopeKey: "checkout:" + requestID, Kind: "settled", RequestID: requestID,
		WorkingDirectory: "/repo", RepositoryID: "repo", CheckoutID: "checkout", CheckoutRoot: "/repo",
		Status: "pending", BoundaryClass: "settled", RequestedAt: 1})
	if err == nil {
		err = tx.Commit()
	} else {
		_ = tx.Rollback()
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, won, err := ix.ClaimSessionCheckpoint(checkpoint.ID, 2, 0, "settled"); err != nil || !won {
		t.Fatalf("claim checkpoint: won=%v err=%v", won, err)
	}
	return checkpoint
}

func collectionIntegrityChange(sessionID string) *ChangeRecord {
	return &ChangeRecord{SessionID: sessionID, RepositoryID: "repo", CheckoutID: "checkout",
		RepositoryIdentityKind: "local-sha256", CheckoutRoot: "/repo", Kind: "revision",
		EvidenceClass: "observed", SourceKind: "git", SourceDigest: "snapshot", RecordedAt: 2,
		CaptureStartedAt: 1, CaptureEndedAt: 2, BaseRevision: "base", HeadRevision: "head",
		SnapshotDigest: "snapshot"}
}

func TestResultRetainedBodyDigestIsValidatedAtStoreBoundary(t *testing.T) {
	ix := openResultTestIndex(t)
	body := []byte("measured body")
	result := ResultObservation{ObservationID: "res-integrity", SessionID: "integrity",
		Runtime: "runtime", SourceKind: "vendor-transcript", SourceDigest: "sha256-v1:source",
		State: "success", Completeness: "metadata-only", PayloadDigest: "sha256-v1:metadata"}
	effect := ResultEffect{RawIdentity: "src/a.go", Operation: "update",
		EvidenceSource: "runtime-result", Completeness: "complete", ContentBytes: len(body),
		ContentDigest: "sha256-v1:producer-claim", ContentPayload: body,
		DiffCompleteness: "unavailable"}
	if _, _, err := ix.AppendResultObservation(result, []ResultEffect{effect}); err == nil {
		t.Fatal("accepted a producer digest that did not match the retained result body")
	}
	var results int
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM result_observation`).Scan(&results); err != nil || results != 0 {
		t.Fatalf("rejected result partially persisted: count=%d err=%v", results, err)
	}
	effect.ContentDigest = retainedBodyDigest(body)
	if _, _, err := ix.AppendResultObservation(result, []ResultEffect{effect}); err != nil {
		t.Fatalf("append measured result: %v", err)
	}
}

func TestZeroByteResultAndEffectBodiesRemainMeasured(t *testing.T) {
	ix := openResultTestIndex(t)
	empty := []byte{}
	digest := retainedBodyDigest(empty)
	result := ResultObservation{ObservationID: "res-empty", SessionID: "empty-result",
		Runtime: "runtime", SourceKind: "vendor-transcript", SourceDigest: "sha256-v1:source",
		State: "success", Completeness: "complete", Payload: empty, PayloadDigest: digest}
	effect := ResultEffect{RawIdentity: "empty.go", Operation: "create",
		EvidenceSource: "runtime-result", Completeness: "complete", ContentPayload: empty,
		ContentDigest: digest, DiffCompleteness: "unavailable"}
	resultID, _, err := ix.AppendResultObservation(result, []ResultEffect{effect})
	if err != nil {
		t.Fatal(err)
	}
	var resultNull, effectNull, resultBytes, effectBytes int
	if err := ix.db.QueryRow(`SELECT payload IS NULL,retained_bytes FROM result_observation WHERE id=?`,
		resultID).Scan(&resultNull, &resultBytes); err != nil {
		t.Fatal(err)
	}
	if err := ix.db.QueryRow(`SELECT content_payload IS NULL,content_bytes FROM result_effect
		WHERE result_id=?`, resultID).Scan(&effectNull, &effectBytes); err != nil {
		t.Fatal(err)
	}
	if resultNull != 0 || effectNull != 0 || resultBytes != 0 || effectBytes != 0 {
		t.Fatalf("empty bodies result(null=%d bytes=%d) effect(null=%d bytes=%d)",
			resultNull, resultBytes, effectNull, effectBytes)
	}
}

func TestCheckpointPayloadDigestRejectionIsAtomic(t *testing.T) {
	ix := openResultTestIndex(t)
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, _, err := tx.EnsureSessionCheckpoint(SessionCheckpoint{SessionID: "checkpoint-integrity",
		ScopeKey: "checkout:one", Kind: "settled", RequestID: "checkpoint-integrity",
		WorkingDirectory: "/repo", RepositoryID: "repo", CheckoutID: "checkout", CheckoutRoot: "/repo",
		Status: "pending", BoundaryClass: "settled", RequestedAt: 1})
	if err == nil {
		err = tx.Commit()
	} else {
		_ = tx.Rollback()
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, won, err := ix.ClaimSessionCheckpoint(checkpoint.ID, 2, 0, "settled"); err != nil || !won {
		t.Fatalf("claim checkpoint: won=%v err=%v", won, err)
	}
	record := &ChangeRecord{SessionID: "checkpoint-integrity", RepositoryID: "repo",
		CheckoutID: "checkout", RepositoryIdentityKind: "local-sha256", CheckoutRoot: "/repo",
		Kind: "revision", EvidenceClass: "observed", SourceKind: "git", SourceDigest: "snapshot",
		RecordedAt: 2, CaptureStartedAt: 1, CaptureEndedAt: 2, BaseRevision: "base",
		HeadRevision: "head", SnapshotDigest: "snapshot"}
	body := []byte("checkpoint body")
	payload := CheckpointPayload{Path: "src/a.go", Layer: "worktree", Status: "M",
		ContentBytes: len(body), ContentDigest: "sha256-v1:false", ContentCompleteness: "complete",
		ContentPayload: body, PatchCompleteness: "unavailable"}
	if err := ix.CompleteSessionCheckpointWithPayload(checkpoint.ID, record,
		[]CheckpointPayload{payload}, 3); err == nil {
		t.Fatal("accepted a checkpoint digest that did not match the retained body")
	}
	for table, want := range map[string]int{"change_record": 0, "checkpoint_payload": 0, "evidence_body": 0} {
		var count int
		if err := ix.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != want {
			t.Fatalf("%s count=%d want=%d err=%v", table, count, want, err)
		}
	}
}

func TestZeroByteCheckpointBodyIsValidatedStoredAndLinked(t *testing.T) {
	ix := openResultTestIndex(t)
	checkpoint := collectionIntegrityCheckpoint(t, ix, "checkpoint-empty")
	empty := []byte{}
	digest := retainedBodyDigest(empty)
	payload := CheckpointPayload{Path: "empty.go", Layer: "worktree", Status: "M",
		ContentDigest: digest, ContentCompleteness: "complete", ContentPayload: empty,
		PatchCompleteness: "unavailable"}
	if err := ix.CompleteSessionCheckpointWithPayload(checkpoint.ID,
		collectionIntegrityChange("checkpoint-empty"), []CheckpointPayload{payload}, 3); err != nil {
		t.Fatal(err)
	}
	var bodies, links, nullPayload, bodyBytes int
	if err := ix.db.QueryRow(`SELECT COUNT(*),COALESCE(MAX(payload IS NULL),1),
		COALESCE(MAX(body_bytes),-1) FROM evidence_body`).Scan(&bodies, &nullPayload, &bodyBytes); err != nil {
		t.Fatal(err)
	}
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM checkpoint_payload_body WHERE checkpoint_id=?`,
		checkpoint.ID).Scan(&links); err != nil {
		t.Fatal(err)
	}
	if bodies != 1 || links != 1 || nullPayload != 0 || bodyBytes != 0 {
		t.Fatalf("empty checkpoint evidence bodies=%d links=%d null=%d bytes=%d",
			bodies, links, nullPayload, bodyBytes)
	}
}

func TestCheckpointCompletionReplayRequiresSameFacts(t *testing.T) {
	ix := openResultTestIndex(t)
	checkpoint := collectionIntegrityCheckpoint(t, ix, "checkpoint-replay")
	body := []byte("body")
	payload := CheckpointPayload{Path: "file.go", Layer: "worktree", Status: "M",
		ContentBytes: len(body), ContentDigest: retainedBodyDigest(body),
		ContentCompleteness: "complete", ContentPayload: body, PatchCompleteness: "unavailable"}
	record := collectionIntegrityChange("checkpoint-replay")
	if err := ix.CompleteSessionCheckpointWithPayload(checkpoint.ID, record,
		[]CheckpointPayload{payload}, 3); err != nil {
		t.Fatal(err)
	}
	if err := ix.CompleteSessionCheckpointWithPayload(checkpoint.ID, record,
		[]CheckpointPayload{payload}, 4); err != nil {
		t.Fatalf("same completion replay: %v", err)
	}
	conflict := *record
	conflict.SourceDigest = "different-snapshot"
	if err := ix.CompleteSessionCheckpointWithPayload(checkpoint.ID, &conflict,
		[]CheckpointPayload{payload}, 5); !errors.Is(err, ErrCheckpointCompletionCollision) {
		t.Fatalf("conflicting completion error=%v", err)
	}
	for table, want := range map[string]int{"change_record": 1, "checkpoint_payload": 1,
		"checkpoint_payload_body": 1} {
		var count int
		if err := ix.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != want {
			t.Fatalf("%s count=%d want=%d err=%v", table, count, want, err)
		}
	}
}

func TestCheckpointCompleteBodyRejectsMissingDigestAndCountMismatch(t *testing.T) {
	for _, test := range []struct {
		name    string
		bytes   int
		digest  string
		payload []byte
	}{
		{name: "missing empty digest", payload: []byte{}},
		{name: "zero body wrong count", bytes: 1, digest: retainedBodyDigest([]byte{}), payload: []byte{}},
		{name: "missing body", digest: retainedBodyDigest([]byte{}), payload: nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			ix := openResultTestIndex(t)
			checkpoint := collectionIntegrityCheckpoint(t, ix, "invalid-"+test.name)
			payload := CheckpointPayload{Path: "empty.go", Layer: "worktree", Status: "M",
				ContentBytes: test.bytes, ContentDigest: test.digest,
				ContentCompleteness: "complete", ContentPayload: test.payload,
				PatchCompleteness: "unavailable"}
			if err := ix.CompleteSessionCheckpointWithPayload(checkpoint.ID,
				collectionIntegrityChange("invalid-"+test.name), []CheckpointPayload{payload}, 3); err == nil {
				t.Fatal("invalid complete checkpoint body was accepted")
			}
		})
	}
}

func TestTranscriptCursorAndParserIssuesCommitAtomicallyAndIdempotently(t *testing.T) {
	ix := openResultTestIndex(t)
	issue := CollectionIssue{IssueID: "evicted-one", SessionID: "session", Runtime: "runtime",
		NativeCallID: "call-1", SourceRef: "/transcript", SourceSegmentID: "segment",
		CollectorID: "harvest-lifecycle", Kind: "parser-state-evicted", AffectedCount: 1,
		FirstSeen: 10, LastSeen: 10, DetailDigest: "sha256-v1:reason"}
	cursor := TranscriptCursor{Runtime: "runtime", SourceRef: "/transcript",
		SourceSegmentID: "segment", SessionID: "session", FileSize: 100, CommittedOffset: 100,
		ActionParserVersion: 1, UpdatedAt: 10}
	if err := ix.CommitTranscriptCursor(cursor, []CollectionIssue{issue}); err != nil {
		t.Fatal(err)
	}
	if err := ix.CommitTranscriptCursor(cursor, []CollectionIssue{issue}); err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	issues, err := ix.CollectionIssuesForSession("session", 10)
	if err != nil || len(issues) != 1 || issues[0].AffectedCount != 1 {
		t.Fatalf("issues=%+v err=%v", issues, err)
	}
	collision := issue
	collision.NativeCallID = "different-call"
	cursor.FileSize = 200
	cursor.CommittedOffset = 200
	if err := ix.CommitTranscriptCursor(cursor, []CollectionIssue{collision}); !errors.Is(err, ErrCollectionIssueCollision) {
		t.Fatalf("collision error=%v", err)
	}
	got, found, err := ix.TranscriptCursor("runtime", "/transcript", "segment")
	if err != nil || !found || got.CommittedOffset != 100 {
		t.Fatalf("cursor advanced through issue collision: %+v found=%v err=%v", got, found, err)
	}
}

func TestParserEvictionRequiresPairedSourceIdentity(t *testing.T) {
	ix := openResultTestIndex(t)
	issue := CollectionIssue{IssueID: "evicted-missing-source", SessionID: "session",
		Runtime: "runtime", NativeCallID: "call", CollectorID: "collector",
		Kind: "parser-state-evicted", AffectedCount: 1, FirstSeen: 1, LastSeen: 1}
	if err := ix.EnsureCollectionIssue(issue); err == nil {
		t.Fatal("store boundary accepted an unpaired parser eviction")
	}
	if _, err := ix.db.Exec(`INSERT INTO collection_issue(issue_id,session_id,runtime,
		native_call_id,collector_id,kind,affected_count,first_seen,last_seen)
		VALUES('raw-eviction','session','runtime','call','collector','parser-state-evicted',1,1,1)`); err == nil {
		t.Fatal("schema accepted an unpaired parser eviction")
	}
}

func TestSchemaV14MigratesCollectionIssueSourceIdentityWithoutLosingRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ix.db.Exec(`
DROP INDEX collection_issue_active;
ALTER TABLE collection_issue RENAME TO collection_issue_v14_fixture;
CREATE TABLE collection_issue(
 id INTEGER PRIMARY KEY, issue_id TEXT NOT NULL UNIQUE, session_id TEXT NOT NULL DEFAULT '',
 runtime TEXT NOT NULL DEFAULT '', observation_id TEXT NOT NULL DEFAULT '', collector_id TEXT NOT NULL,
 kind TEXT NOT NULL, affected_count INTEGER NOT NULL, first_seen INTEGER NOT NULL,
 last_seen INTEGER NOT NULL, detail_digest TEXT NOT NULL DEFAULT '', resolved_at INTEGER NOT NULL DEFAULT 0,
 resolution_class TEXT NOT NULL DEFAULT '');
INSERT INTO collection_issue(id,issue_id,session_id,runtime,observation_id,collector_id,kind,
 affected_count,first_seen,last_seen,detail_digest,resolved_at,resolution_class)
VALUES(1,'legacy-issue','session','runtime','obs','collector','malformed',1,1,2,'digest',2,'recovered');
DROP TABLE collection_issue_v14_fixture;
PRAGMA user_version=13;`); err != nil {
		t.Fatal(err)
	}
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}
	ix, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	issues, err := ix.CollectionIssuesForSession("session", 10)
	if err != nil || len(issues) != 1 || issues[0].IssueID != "legacy-issue" ||
		issues[0].ResolutionClass != "recovered" || issues[0].SourceRef != "" {
		t.Fatalf("migrated issues=%+v err=%v", issues, err)
	}
	if err := ix.EnsureCollectionIssue(CollectionIssue{IssueID: "evicted-after-upgrade",
		SessionID: "session", Runtime: "runtime", NativeCallID: "call", SourceRef: "/source",
		SourceSegmentID: "segment", CollectorID: "collector", Kind: "parser-state-evicted",
		AffectedCount: 1, FirstSeen: 3, LastSeen: 3}); err != nil {
		t.Fatalf("new v14 issue after migration: %v", err)
	}
}

func TestSchemaV15AddsParserEvictionPairingWithoutLosingRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ix.db.Exec(`INSERT INTO collection_issue(issue_id,session_id,runtime,
		native_call_id,source_ref,source_segment_id,collector_id,kind,affected_count,first_seen,last_seen)
		VALUES('valid-v14','session','runtime','call','/source','segment','collector',
		'parser-state-evicted',1,1,1);
		PRAGMA user_version=14;`); err != nil {
		t.Fatal(err)
	}
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}
	ix, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	var count int
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM collection_issue WHERE issue_id='valid-v14'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("migrated row count=%d err=%v", count, err)
	}
	if _, err := ix.db.Exec(`INSERT INTO collection_issue(issue_id,collector_id,kind,
		affected_count,first_seen,last_seen) VALUES('invalid-v15','collector',
		'parser-state-evicted',1,1,1)`); err == nil {
		t.Fatal("v15 schema accepted an unpaired parser eviction")
	}
}

func TestSchemaV15ClassifiesUnlinkedV14EvictionWithoutInventingIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ix.db.Exec(`
DROP INDEX collection_issue_active;
ALTER TABLE collection_issue RENAME TO collection_issue_current;
CREATE TABLE collection_issue(
 id INTEGER PRIMARY KEY, issue_id TEXT NOT NULL UNIQUE, session_id TEXT NOT NULL DEFAULT '',
 runtime TEXT NOT NULL DEFAULT '', observation_id TEXT NOT NULL DEFAULT '',
 native_call_id TEXT NOT NULL DEFAULT '', source_ref TEXT NOT NULL DEFAULT '',
 source_segment_id TEXT NOT NULL DEFAULT '', collector_id TEXT NOT NULL,
 kind TEXT NOT NULL, affected_count INTEGER NOT NULL, first_seen INTEGER NOT NULL,
 last_seen INTEGER NOT NULL, detail_digest TEXT NOT NULL DEFAULT '',
 resolved_at INTEGER NOT NULL DEFAULT 0, resolution_class TEXT NOT NULL DEFAULT '');
INSERT INTO collection_issue(issue_id,session_id,runtime,collector_id,kind,
 affected_count,first_seen,last_seen,detail_digest)
VALUES('unlinked-v14','session','runtime','collector','parser-state-evicted',1,1,2,'detail');
DROP TABLE collection_issue_current;
PRAGMA user_version=14;`); err != nil {
		t.Fatal(err)
	}
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}
	ix, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	issues, err := ix.CollectionIssuesForSession("session", 10)
	if err != nil || len(issues) != 1 {
		t.Fatalf("issues=%+v err=%v", issues, err)
	}
	if issues[0].Kind != "malformed" || issues[0].NativeCallID != "" ||
		issues[0].SourceRef != "" || issues[0].SourceSegmentID != "" ||
		issues[0].DetailDigest != "detail" {
		t.Fatalf("unlinked v14 issue was fabricated or lost: %+v", issues[0])
	}
}

func TestSchemaV14RepairsCheckpointTriggerForeignKeyAndAcceptsRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, _, err := tx.EnsureSessionCheckpoint(SessionCheckpoint{SessionID: "trigger",
		ScopeKey: "checkout:trigger", Kind: "settled", RequestID: "trigger-checkpoint",
		WorkingDirectory: "/repo", Status: "pending", BoundaryClass: "settled", RequestedAt: 1})
	if err == nil {
		err = tx.Commit()
	} else {
		_ = tx.Rollback()
	}
	if err != nil {
		t.Fatal(err)
	}
	resultID, _, err := ix.AppendResultObservation(ResultObservation{ObservationID: "res-trigger",
		SessionID: "trigger", Runtime: "runtime", SourceKind: "vendor-transcript",
		SourceDigest: "sha256-v1:trigger", State: "success", Completeness: "metadata-only"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ix.db.Exec(`DROP TABLE session_checkpoint_trigger;
CREATE TABLE session_checkpoint_trigger(
 checkpoint_id INTEGER NOT NULL REFERENCES session_checkpoint_v7(id) ON DELETE RESTRICT,
 result_id INTEGER NOT NULL REFERENCES result_observation(id) ON DELETE RESTRICT,
 observation_id TEXT NOT NULL, accepted_at INTEGER NOT NULL,
 PRIMARY KEY(checkpoint_id,result_id));
PRAGMA user_version=13;`); err != nil {
		t.Fatal(err)
	}
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}
	ix, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	var canonical, stale int
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_list('session_checkpoint_trigger')
		WHERE "table"='session_checkpoint'`).Scan(&canonical); err != nil {
		t.Fatal(err)
	}
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_list('session_checkpoint_trigger')
		WHERE "table"='session_checkpoint_v7'`).Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if canonical != 1 || stale != 0 {
		t.Fatalf("checkpoint trigger foreign keys canonical=%d stale=%d", canonical, stale)
	}
	if err := ix.AddSessionCheckpointTrigger(checkpoint.ID, resultID, "res-trigger", 2); err != nil {
		t.Fatalf("insert repaired trigger: %v", err)
	}
}
