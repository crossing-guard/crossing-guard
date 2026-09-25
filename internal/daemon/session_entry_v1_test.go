package daemon

import (
	"context"
	"testing"
	"time"

	"crossing-guard/internal/observation"
	"crossing-guard/store"
)

func testSessionEntry(repo string) observation.SessionEntryEnvelope {
	now := time.Now().Unix()
	return observation.SessionEntryEnvelope{
		Schema: observation.SessionEntrySchemaV1, ObservationID: "ent_0123456789abcdef0123456789abcdef",
		CollectorID: observation.CollectorSessionEntry, Runtime: "claude", SessionID: "native-session",
		HookEventName: "SessionStart", EntryKind: "start", NativeSource: "startup",
		TranscriptPath: repo + "/transcript.jsonl", Cwd: repo, ObservedAt: now, QueuedAt: now,
		DeliveryAttempts: 1, DeliveryMode: "direct",
	}
}

func TestSessionStartStoresActivityAndImmediateGitCheckpointWithoutAction(t *testing.T) {
	repo := v1TestRepo(t)
	ix, err := store.Open(t.TempDir() + "/index.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	g := NewGovernor(ix, nil)
	entry := testSessionEntry(repo)
	receipt, err := ingestSessionEntryV1(context.Background(), g, entry)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Duplicate || receipt.EvidenceClass != "explicit-start" || receipt.Checkpoint.Status != "complete" {
		t.Fatalf("receipt=%+v", receipt)
	}
	activities, err := ix.SessionActivityObservations(entry.Runtime, entry.SessionID, 10)
	if err != nil || len(activities) != 1 || activities[0].EntryKind != "start" || activities[0].NativeSource != "startup" {
		t.Fatalf("activities=%+v err=%v", activities, err)
	}
	report, err := g.SessionReport(entry.SessionID, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Events) != 0 {
		t.Fatalf("SessionStart manufactured actions: %+v", report.Events)
	}
	if len(report.Checkpoints) != 1 || report.Checkpoints[0].Runtime != "claude" {
		t.Fatalf("checkpoints=%+v", report.Checkpoints)
	}
}

func TestSessionStartReplayChangesDeliveryNotFactIdentity(t *testing.T) {
	repo := v1TestRepo(t)
	ix, err := store.Open(t.TempDir() + "/index.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	g := NewGovernor(ix, nil)
	entry := testSessionEntry(repo)
	if _, err := ingestSessionEntryV1(context.Background(), g, entry); err != nil {
		t.Fatal(err)
	}
	entry.DeliveryMode = "replay"
	entry.DeliveryAttempts = 2
	receipt, err := ingestSessionEntryV1(context.Background(), g, entry)
	if err != nil || !receipt.Duplicate {
		t.Fatalf("replay receipt=%+v err=%v", receipt, err)
	}
	activity, found, err := ix.SessionActivityByID(entry.ObservationID)
	if err != nil || !found || activity.DeliveryAttempts != 2 || activity.DeliveryMode != "direct" {
		t.Fatalf("activity=%+v found=%t err=%v", activity, found, err)
	}
}

func TestNativeStartSuppressesFirstActionFallbackAndClosureStoresEnd(t *testing.T) {
	repo := v1TestRepo(t)
	ix, err := store.Open(t.TempDir() + "/index.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	g := NewGovernor(ix, nil)
	entry := testSessionEntry(repo)
	if _, err := ingestSessionEntryV1(context.Background(), g, entry); err != nil {
		t.Fatal(err)
	}
	action := testV1Envelope(t, repo)
	action.SessionID = entry.SessionID
	if _, err := ingestObservationV1(context.Background(), g, action); err != nil {
		t.Fatal(err)
	}
	activities, err := ix.SessionActivityObservations(entry.Runtime, entry.SessionID, 10)
	if err != nil || len(activities) != 1 || activities[0].EntryKind != "start" {
		t.Fatalf("action duplicated native start: %+v err=%v", activities, err)
	}
	closure := observation.ClosureEnvelope{Schema: observation.ClosureSchemaV1,
		ObservationID: "cls_0123456789abcdef0123456789abcdef", CollectorID: observation.CollectorClosure,
		Runtime: entry.Runtime, SessionID: entry.SessionID, HookEventName: "SessionEnd",
		TranscriptPath: entry.TranscriptPath, Cwd: repo, ObservedAt: time.Now().Unix(),
		QueuedAt: time.Now().Unix(), DeliveryAttempts: 1, DeliveryMode: "direct"}
	if _, err := ingestClosureV1(g, closure); err != nil {
		t.Fatal(err)
	}
	activities, err = ix.SessionActivityObservations(entry.Runtime, entry.SessionID, 10)
	if err != nil || len(activities) != 2 || activities[0].EntryKind != "end" || activities[0].State != "stopped" {
		t.Fatalf("closure activity=%+v err=%v", activities, err)
	}
	action.DeliveryAttempts = 2
	action.DeliveryMode = "replay"
	if receipt, err := ingestObservationV1(context.Background(), g, action); err != nil || !receipt.Duplicate {
		t.Fatalf("late action retry receipt=%+v err=%v", receipt, err)
	}
	activities, err = ix.SessionActivityObservations(entry.Runtime, entry.SessionID, 10)
	if err != nil || len(activities) != 2 || activities[0].State != "stopped" {
		t.Fatalf("late action retry reopened session: %+v err=%v", activities, err)
	}
}

func TestSessionAttachmentIdentityIncludesRuntime(t *testing.T) {
	repo := v1TestRepo(t)
	ix, err := store.Open(t.TempDir() + "/index.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	g := NewGovernor(ix, nil)
	claude := testSessionEntry(repo)
	if _, err := ingestSessionEntryV1(context.Background(), g, claude); err != nil {
		t.Fatal(err)
	}
	codex := claude
	codex.Runtime = "codex"
	codex.ObservationID = "ent_fedcba9876543210fedcba9876543210"
	if _, err := ingestSessionEntryV1(context.Background(), g, codex); err != nil {
		t.Fatal(err)
	}
	claudeCheckpoint, claudeFound, err := ix.SessionCheckpointForWorkingDirectory("claude", claude.SessionID, repo)
	if err != nil || !claudeFound {
		t.Fatalf("claude checkpoint=%+v found=%t err=%v", claudeCheckpoint, claudeFound, err)
	}
	codexCheckpoint, codexFound, err := ix.SessionCheckpointForWorkingDirectory("codex", codex.SessionID, repo)
	if err != nil || !codexFound || codexCheckpoint.ID == claudeCheckpoint.ID {
		t.Fatalf("codex checkpoint=%+v found=%t err=%v claude=%+v", codexCheckpoint, codexFound, err, claudeCheckpoint)
	}
}
