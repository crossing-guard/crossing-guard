package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"crossing-guard/internal/changeenv"
	"crossing-guard/internal/observation"
	"crossing-guard/store"
)

func v1TestRepo(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.name", "Crossing Guard"}, {"config", "user.email", "crossing-guard@example.invalid"}} {
		if out, err := exec.Command("git", append([]string{"-C", d}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(d, "a.go"), []byte("package a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "a.go"}, {"commit", "-q", "-m", "base"}} {
		if out, err := exec.Command("git", append([]string{"-C", d}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return d
}

func testV1Envelope(t *testing.T, repo string) observation.Envelope {
	t.Helper()
	raw := json.RawMessage(`{"file_path":"a.go","new_string":"package a\n// changed\n","future":{"x":1}}`)
	e := observation.Envelope{Schema: observation.SchemaV1,
		ObservationID: "obs_0123456789abcdef0123456789abcdef",
		ActionID:      "act_0123456789abcdef0123456789abcdef", CollectorID: observation.CollectorPreTool,
		SessionID: "claude/s-v1", Runtime: "claude", Tool: "Edit", FilePath: filepath.Join(repo, "a.go"),
		FilePaths: []string{filepath.Join(repo, "a.go")}, Cwd: repo, Decision: "allow", TS: time.Now().Unix(),
		ToolInput: raw, ToolInputBytes: len(raw), ToolInputDigest: observation.DigestBytes(raw),
		ToolInputCompleteness: "complete", QueuedAt: time.Now().Unix(), DeliveryAttempts: 1, DeliveryMode: "direct",
		ResourceClaims: []observation.ResourceClaim{{Ordinal: 0, Kind: "file", RawIdentity: "a.go",
			Identity: filepath.Join(repo, "a.go"), Operation: "write", EvidenceClass: "declared",
			SourceField: "tool_input.file_path", Completeness: "complete"}}}
	return e
}

func postV1(t *testing.T, e observation.Envelope) (*httptest.ResponseRecorder, observation.Receipt) {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/govern/observe/v1", bytes.NewReader(b))
	w := httptest.NewRecorder()
	handleGovernObserveV1(w, r)
	var receipt observation.Receipt
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil {
			t.Fatal(err)
		}
	}
	return w, receipt
}

func TestGovernObserveV1StoresInputResourcesAndFirstSnapshotIdempotently(t *testing.T) {
	repo := v1TestRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "dirty.txt"), []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	old := governor
	governor = NewGovernor(ix, nil)
	t.Cleanup(func() { governor = old; _ = ix.Close() })

	e := testV1Envelope(t, repo)
	e.ResourceClaims = append(e.ResourceClaims, observation.ResourceClaim{Ordinal: 1, Kind: "file",
		RawIdentity: "a.go", Identity: filepath.Join(repo, "a.go"), Operation: "read",
		EvidenceClass: "declared", SourceField: "tool_input.secondary_path", Completeness: "complete"})
	w, receipt := postV1(t, e)
	if w.Code != http.StatusOK || receipt.ObservationID != e.ObservationID || receipt.ActionID != e.ActionID || receipt.EventID == 0 || receipt.Duplicate {
		t.Fatalf("first response=%d %s receipt=%+v", w.Code, w.Body.String(), receipt)
	}
	activities, err := ix.SessionActivityObservations(e.Runtime, e.SessionID, 10)
	if err != nil || len(activities) != 1 || activities[0].EntryKind != "first-action" {
		t.Fatalf("first-action fallback=%+v err=%v", activities, err)
	}
	if receipt.Checkpoint.Status != "complete" || receipt.Checkpoint.ChangeRecordID == 0 || receipt.Checkpoint.BoundaryClass != "direct-pre-release" {
		t.Fatalf("checkpoint=%+v", receipt.Checkpoint)
	}
	changes, total, err := ix.ChangeRecordsForSession(e.SessionID, 10)
	if err != nil || total != 1 || len(changes) != 1 {
		t.Fatalf("changes=%+v total=%d err=%v", changes, total, err)
	}
	foundDirty := false
	for _, item := range changes[0].Items {
		if item.Path == "dirty.txt" && item.Layer == "untracked" {
			foundDirty = true
		}
	}
	if !foundDirty {
		t.Fatalf("starting dirty state missing from baseline: %+v", changes[0].Items)
	}
	events, err := ix.EventsForSession(e.SessionID, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("events=%d err=%v", len(events), err)
	}
	resources, _, err := ix.EventResourcesForSession(e.SessionID, 10, 10)
	if err != nil || len(resources) != 2 || resources[0].SourceField != "tool_input.file_path" || resources[0].Operation != "write" ||
		resources[1].SourceField != "tool_input.secondary_path" || resources[1].Operation != "read" {
		t.Fatalf("resources=%+v err=%v", resources, err)
	}
	delivery, input, found, err := ix.ObservationEvidence(e.ObservationID)
	if err != nil || !found || delivery.ActionID != e.ActionID || delivery.NativeCallID != e.NativeCallID || input.RawBytes != len(e.ToolInput) || string(input.Payload) != string(e.ToolInput) {
		t.Fatalf("delivery=%+v input=%+v found=%v err=%v", delivery, input, found, err)
	}

	w, duplicate := postV1(t, e)
	if w.Code != http.StatusOK || !duplicate.Duplicate || duplicate.EventID != receipt.EventID || duplicate.Checkpoint.ChangeRecordID != receipt.Checkpoint.ChangeRecordID {
		t.Fatalf("duplicate response=%d %s receipt=%+v", w.Code, w.Body.String(), duplicate)
	}
	events, _ = ix.EventsForSession(e.SessionID, 10)
	if len(events) != 1 {
		t.Fatalf("duplicate created %d events", len(events))
	}
	second := e
	second.ObservationID = "obs_2123456789abcdef0123456789abcdef"
	second.Tool = "Read"
	w, secondReceipt := postV1(t, second)
	if w.Code != http.StatusOK || secondReceipt.Duplicate || secondReceipt.Checkpoint.Status != "complete" ||
		secondReceipt.Checkpoint.ChangeRecordID != receipt.Checkpoint.ChangeRecordID {
		t.Fatalf("second action response=%d %s receipt=%+v", w.Code, w.Body.String(), secondReceipt)
	}
	events, _ = ix.EventsForSession(e.SessionID, 10)
	if len(events) != 2 {
		t.Fatalf("second action event count=%d", len(events))
	}
	changes, total, err = ix.ChangeRecordsForSession(e.SessionID, 10)
	if err != nil || total != 1 || len(changes) != 1 {
		t.Fatalf("second action duplicated checkpoint changes=%+v total=%d err=%v", changes, total, err)
	}
}

// TestObserveV1PublishesStatusRefoldAfterCommittedAction pins plan D1: a
// newly committed session-scoped action publishes the refold seam, a
// duplicate delivery does not.
func TestObserveV1PublishesStatusRefoldAfterCommittedAction(t *testing.T) {
	repo := v1TestRepo(t)
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	old := governor
	governor = NewGovernor(ix, nil)
	t.Cleanup(func() { governor = old; _ = ix.Close() })

	var published []string
	var pubMu sync.Mutex
	defer swapSessionStatusRefold(func(runtime, sessionID string) {
		pubMu.Lock()
		published = append(published, runtime+"/"+sessionID)
		pubMu.Unlock()
	})()

	e := testV1Envelope(t, repo)
	w, receipt := postV1(t, e)
	if w.Code != http.StatusOK || receipt.Duplicate {
		t.Fatalf("first response=%d %s receipt=%+v", w.Code, w.Body.String(), receipt)
	}
	pubMu.Lock()
	if len(published) != 1 || published[0] != "claude/claude/s-v1" {
		t.Fatalf("a committed action must publish one refold, got %v", published)
	}
	pubMu.Unlock()

	// A duplicate delivery must not publish again.
	pubMu.Lock()
	published = nil
	pubMu.Unlock()
	w, dup := postV1(t, e)
	if w.Code != http.StatusOK || !dup.Duplicate {
		t.Fatalf("duplicate response=%d %s receipt=%+v", w.Code, w.Body.String(), dup)
	}
	pubMu.Lock()
	if len(published) != 0 {
		t.Fatalf("a duplicate delivery must not publish a refold, got %v", published)
	}
	pubMu.Unlock()

	// A second distinct action publishes again.
	second := e
	second.ObservationID = "obs_2123456789abcdef0123456789abcdef"
	second.Tool = "Read"
	w, secondReceipt := postV1(t, second)
	if w.Code != http.StatusOK || secondReceipt.Duplicate {
		t.Fatalf("second action response=%d %s receipt=%+v", w.Code, w.Body.String(), secondReceipt)
	}
	pubMu.Lock()
	if len(published) != 1 || published[0] != "claude/claude/s-v1" {
		t.Fatalf("a second action must publish one refold, got %v", published)
	}
	pubMu.Unlock()
}

func TestGovernObserveV1RejectsObservationCollision(t *testing.T) {
	repo := v1TestRepo(t)
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	old := governor
	governor = NewGovernor(ix, nil)
	t.Cleanup(func() { governor = old; _ = ix.Close() })
	e := testV1Envelope(t, repo)
	if w, _ := postV1(t, e); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	e.Tool = "Write"
	if w, _ := postV1(t, e); w.Code != http.StatusConflict {
		t.Fatalf("collision response=%d %s", w.Code, w.Body.String())
	}
}

func TestGovernObserveV1RejectsTrailingJSON(t *testing.T) {
	repo := v1TestRepo(t)
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	old := governor
	governor = NewGovernor(ix, nil)
	t.Cleanup(func() { governor = old; _ = ix.Close() })
	b, err := json.Marshal(testV1Envelope(t, repo))
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/govern/observe/v1", bytes.NewReader(append(b, []byte(` {}`)...)))
	w := httptest.NewRecorder()
	handleGovernObserveV1(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("response=%d %s", w.Code, w.Body.String())
	}
}

func TestValidateObservationV1AcceptsLegacyMissingActionAndRejectsMalformedAction(t *testing.T) {
	e := testV1Envelope(t, t.TempDir())
	e.ActionID = ""
	if err := validateObservationV1(e); err != nil {
		t.Fatalf("legacy missing action rejected: %v", err)
	}
	for _, invalid := range []string{"act_short", "obs_0123456789abcdef0123456789abcdef",
		"act_0123456789abcdef0123456789abcdeG"} {
		e.ActionID = invalid
		if err := validateObservationV1(e); err == nil {
			t.Fatalf("invalid action accepted: %q", invalid)
		}
	}
}

func TestObservationSpoolReplayIsIdempotentAndLate(t *testing.T) {
	repo := v1TestRepo(t)
	data := t.TempDir()
	ix, err := store.Open(filepath.Join(data, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	g := NewGovernor(ix, nil)
	t.Cleanup(func() { _ = ix.Close() })
	e := testV1Envelope(t, repo)
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(data, "observation-spool")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, e.ObservationID+".json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := replayObservationSpoolOnce(t.Context(), data, g); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("acknowledged spool remains: %v", err)
	}
	events, err := ix.EventsForSession(e.SessionID, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("events=%d err=%v", len(events), err)
	}
	checkpoint, found, err := ix.SessionCheckpointForObservation(e.ObservationID)
	if err != nil || !found || checkpoint.Status != "complete" || checkpoint.BoundaryClass != "late-replay" {
		t.Fatalf("checkpoint=%+v found=%v err=%v", checkpoint, found, err)
	}
	delivery, _, found, err := ix.ObservationEvidence(e.ObservationID)
	if err != nil || !found || delivery.DeliveryAttempts != 2 || delivery.DeliveryMode != "replay" {
		t.Fatalf("delivery=%+v found=%v err=%v", delivery, found, err)
	}
}

// TestLayeredReplayOfAnUnlayeredObservationIsADuplicate is schema 38's upgrade window:
// a new hook stages a layer on an action whose observation an older daemon committed
// without one, and the acknowledgement was lost. The replay's digest differs ONLY by
// the layer — the same observation, never a collision that quarantines the file.
func TestLayeredReplayOfAnUnlayeredObservationIsADuplicate(t *testing.T) {
	repo := v1TestRepo(t)
	data := t.TempDir()
	ix, err := store.Open(filepath.Join(data, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	old := governor
	governor = NewGovernor(ix, nil)
	t.Cleanup(func() { governor = old; _ = ix.Close() })

	// The old hook's envelope: a deny with a rule, no layer.
	e := testV1Envelope(t, repo)
	e.Decision, e.Reason, e.Rule = "deny", "blocked by the org bundle", "deny-alpha"
	if w, rec := postV1(t, e); w.Code != http.StatusOK || rec.Duplicate {
		t.Fatalf("first post: status=%d receipt=%+v", w.Code, rec)
	}

	// The upgraded hook retries with the layer staged.
	layered := e
	layered.Layer = "organization"
	w, rec := postV1(t, layered)
	if w.Code != http.StatusOK {
		t.Fatalf("layered replay refused: %d %s", w.Code, w.Body.String())
	}
	if !rec.Duplicate {
		t.Fatal("a digest differing only by the layer is the same observation — a duplicate, not a collision")
	}
	events, err := ix.EventsForSession(e.SessionID, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("the replay must not append a second event: events=%d err=%v", len(events), err)
	}
	if events[0].Layer != "" || events[0].RuleID != "deny-alpha" {
		t.Fatalf("the committed row keeps what the FIRST delivery said — rule %q layer %q", events[0].RuleID, events[0].Layer)
	}
}

func TestMalformedClosureReplayIsRecordedOnceAndQuarantinedWithoutRewrite(t *testing.T) {
	data := t.TempDir()
	ix, err := store.Open(filepath.Join(data, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	g := NewGovernor(ix, nil)
	t.Cleanup(func() { _ = ix.Close() })
	dir := filepath.Join(data, "observation-spool")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	envelope := observation.ClosureEnvelope{Schema: "wrong-schema",
		ObservationID: "cls_0123456789abcdef0123456789abcdef", SessionID: "bad-closure",
		Runtime: "runtime", HookEventName: "SessionEnd", DeliveryAttempts: 1, DeliveryMode: "direct"}
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, envelope.ObservationID+".json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := replayObservationSpoolOnce(t.Context(), data, g); err == nil {
		t.Fatal("malformed closure replay unexpectedly succeeded")
	}
	rejectedBody, err := os.ReadFile(quarantineObservationPath(path, observation.DigestBytes(body)))
	if err != nil || !bytes.Equal(rejectedBody, body) {
		t.Fatalf("quarantined body changed: err=%v got=%q want=%q", err, rejectedBody, body)
	}
	issues, err := ix.CollectionIssuesForSession("bad-closure", 10)
	if err != nil || len(issues) != 1 || issues[0].Kind != "malformed" || issues[0].AffectedCount != 1 {
		t.Fatalf("issues=%+v err=%v", issues, err)
	}
	if err := replayObservationSpoolOnce(t.Context(), data, g); err != nil {
		t.Fatalf("rejected record was rescheduled: %v", err)
	}
	issues, err = ix.CollectionIssuesForSession("bad-closure", 10)
	if err != nil || len(issues) != 1 || issues[0].AffectedCount != 1 {
		t.Fatalf("issues after second pass=%+v err=%v", issues, err)
	}
}

func TestReplayQuarantineFailureLeavesOriginalPendingAndIssueIdempotent(t *testing.T) {
	data := t.TempDir()
	ix, err := store.Open(filepath.Join(data, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	g := NewGovernor(ix, nil)
	t.Cleanup(func() { _ = ix.Close() })
	dir := filepath.Join(data, "observation-spool")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	envelope := observation.ClosureEnvelope{Schema: "wrong-schema",
		ObservationID: "cls_fedcba9876543210fedcba9876543210", SessionID: "quarantine-retry",
		Runtime: "runtime", HookEventName: "SessionEnd", DeliveryAttempts: 1, DeliveryMode: "direct"}
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, envelope.ObservationID+".json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	priorRename := quarantineObservationRename
	quarantineObservationRename = func(_, _ string) error { return errors.New("forced rename failure") }
	if err := replayObservationSpoolOnce(t.Context(), data, g); err == nil || !strings.Contains(err.Error(), "forced rename failure") {
		t.Fatalf("quarantine failure=%v", err)
	}
	quarantineObservationRename = priorRename
	t.Cleanup(func() { quarantineObservationRename = priorRename })
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("pending body changed: err=%v got=%q want=%q", err, got, body)
	}
	if err := replayObservationSpoolOnce(t.Context(), data, g); err == nil {
		t.Fatal("second malformed replay unexpectedly succeeded")
	}
	issues, err := ix.CollectionIssuesForSession("quarantine-retry", 10)
	if err != nil || len(issues) != 1 || issues[0].AffectedCount != 1 {
		t.Fatalf("issues after retry=%+v err=%v", issues, err)
	}
}

type failingQuarantineDirectory struct {
	syncErrors []error
}

func (d *failingQuarantineDirectory) Sync() error {
	if len(d.syncErrors) == 0 {
		return nil
	}
	err := d.syncErrors[0]
	d.syncErrors = d.syncErrors[1:]
	return err
}

func (*failingQuarantineDirectory) Close() error { return nil }

func TestReplayQuarantineReportsSyncAndRollbackFailures(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "obs_failure.json")
	if err := os.WriteFile(path, []byte("body"), 0o600); err != nil {
		t.Fatal(err)
	}
	priorOpen, priorRename := quarantineObservationOpenDir, quarantineObservationRename
	quarantineObservationOpenDir = func(string) (quarantineSyncDirectory, error) {
		return &failingQuarantineDirectory{syncErrors: []error{errors.New("forced directory sync failure")}}, nil
	}
	renames := 0
	quarantineObservationRename = func(from, to string) error {
		renames++
		if renames == 2 {
			return errors.New("forced rollback failure")
		}
		return os.Rename(from, to)
	}
	t.Cleanup(func() {
		quarantineObservationOpenDir, quarantineObservationRename = priorOpen, priorRename
	})
	digest := observation.DigestBytes([]byte("body"))
	err := quarantineObservation(path, digest)
	if err == nil || !strings.Contains(err.Error(), "forced directory sync failure") ||
		!strings.Contains(err.Error(), "forced rollback failure") {
		t.Fatalf("combined quarantine error=%v", err)
	}
	if _, err := os.Stat(quarantineObservationPath(path, digest)); err != nil {
		t.Fatalf("rejected body was lost after rollback failure: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "body" {
		t.Fatalf("pending body was not recreated: got=%q err=%v", got, err)
	}
	quarantineObservationOpenDir, quarantineObservationRename = priorOpen, priorRename
	if err := quarantineObservation(path, digest); err != nil {
		t.Fatalf("later quarantine retry failed: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("successful retry left pending record: %v", err)
	}
}

func TestReplayQuarantineRestoresOriginalWhenDirectorySyncFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "obs_retry.json")
	body := []byte("body")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	priorOpen := quarantineObservationOpenDir
	quarantineObservationOpenDir = func(string) (quarantineSyncDirectory, error) {
		return &failingQuarantineDirectory{syncErrors: []error{errors.New("forced directory sync failure"), nil}}, nil
	}
	t.Cleanup(func() { quarantineObservationOpenDir = priorOpen })
	if err := quarantineObservation(path, observation.DigestBytes(body)); err == nil ||
		!strings.Contains(err.Error(), "forced directory sync failure") {
		t.Fatalf("quarantine sync failure=%v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("original was not restored: got=%q err=%v", got, err)
	}
}

func TestReplayQuarantinePreservesDistinctBodiesForOneEntryName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "obs_same.json")
	bodies := [][]byte{[]byte("first"), []byte("second")}
	for _, body := range bodies {
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := quarantineObservation(path, observation.DigestBytes(body)); err != nil {
			t.Fatal(err)
		}
	}
	for _, body := range bodies {
		got, err := os.ReadFile(quarantineObservationPath(path, observation.DigestBytes(body)))
		if err != nil || !bytes.Equal(got, body) {
			t.Fatalf("quarantined body got=%q want=%q err=%v", got, body, err)
		}
	}
}

func TestConcurrentSameObservationCreatesOneEventAndCheckpoint(t *testing.T) {
	repo := v1TestRepo(t)
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	g := NewGovernor(ix, nil)
	t.Cleanup(func() { _ = ix.Close() })
	e := testV1Envelope(t, repo)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := ingestObservationV1(t.Context(), g, e)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	events, err := ix.EventsForSession(e.SessionID, 20)
	if err != nil || len(events) != 1 {
		t.Fatalf("events=%d err=%v", len(events), err)
	}
	checkpoint, found, err := ix.SessionCheckpointForObservation(e.ObservationID)
	if err != nil || !found || checkpoint.Status != "complete" || checkpoint.CaptureAttempts != 1 {
		t.Fatalf("checkpoint=%+v found=%v err=%v", checkpoint, found, err)
	}
}

func TestCancelledDirectCaptureIsNeverClassifiedPreRelease(t *testing.T) {
	repo := v1TestRepo(t)
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	g := NewGovernor(ix, nil)
	t.Cleanup(func() { _ = ix.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	receipt, err := ingestObservationV1(ctx, g, testV1Envelope(t, repo))
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Checkpoint.Status != "failed" || receipt.Checkpoint.FailureKind != "capture-cancelled" || receipt.Checkpoint.BoundaryClass != "unconfirmed" {
		t.Fatalf("checkpoint=%+v", receipt.Checkpoint)
	}
	events, err := ix.EventsForSession("claude/s-v1", 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("cancelled capture erased action: events=%d err=%v", len(events), err)
	}
}

func TestSecureCheckpointReadFailureCreatesFailedCheckpointFact(t *testing.T) {
	repo := v1TestRepo(t)
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	g := NewGovernor(ix, nil)
	t.Cleanup(func() { _ = ix.Close() })
	prior := captureCheckpointEvidence
	captureCheckpointEvidence = func(context.Context, changeenv.SnapshotInput) (*store.ChangeRecord,
		[]store.CheckpointPayload, error) {
		return nil, nil, errors.New("secure checkpoint content nested/file.go: symbolic link")
	}
	t.Cleanup(func() { captureCheckpointEvidence = prior })
	receipt, err := ingestObservationV1(t.Context(), g, testV1Envelope(t, repo))
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Checkpoint.Status != "failed" || receipt.Checkpoint.FailureKind != "git-capture-failed" {
		t.Fatalf("checkpoint=%+v", receipt.Checkpoint)
	}
	issues, err := ix.CollectionIssuesForSession("claude/s-v1", 10)
	if err != nil || len(issues) != 1 || issues[0].Kind != "checkpoint-failed" {
		t.Fatalf("issues=%+v err=%v", issues, err)
	}
}

func TestNonGitFirstObservationKeepsActionAndRecoveryIsUnconfirmed(t *testing.T) {
	cwd := t.TempDir()
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	old := governor
	governor = NewGovernor(ix, nil)
	t.Cleanup(func() { governor = old; _ = ix.Close() })
	e := testV1Envelope(t, cwd)
	w, first := postV1(t, e)
	if w.Code != http.StatusOK || first.Checkpoint.Status != "unavailable" || first.Checkpoint.FailureKind != "repository-unavailable" {
		t.Fatalf("first=%d %s receipt=%+v", w.Code, w.Body.String(), first)
	}
	events, _ := ix.EventsForSession(e.SessionID, 10)
	if len(events) != 1 {
		t.Fatalf("Git failure erased action: %d events", len(events))
	}
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.name", "Crossing Guard"}, {"config", "user.email", "crossing-guard@example.invalid"}} {
		if out, err := exec.Command("git", append([]string{"-C", cwd}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(cwd, "a.go"), []byte("package a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "a.go"}, {"commit", "-q", "-m", "base"}} {
		if out, err := exec.Command("git", append([]string{"-C", cwd}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	e.ObservationID = "obs_1123456789abcdef0123456789abcdef"
	w, recovered := postV1(t, e)
	if w.Code != http.StatusOK || recovered.Checkpoint.Status != "complete" || recovered.Checkpoint.BoundaryClass != "unconfirmed" {
		t.Fatalf("recovery=%d %s receipt=%+v", w.Code, w.Body.String(), recovered)
	}
	issues, err := ix.CollectionIssuesForSession(e.SessionID, 10)
	if err != nil || len(issues) != 1 || issues[0].Kind != "checkpoint-failed" || issues[0].ResolvedAt == 0 {
		t.Fatalf("reconciled issues=%+v err=%v", issues, err)
	}
}

// A checkpoint minted by observation A and recaptured during a LATER
// observation records its capture failures under A's issue identity — the key
// success resolution uses — so the eventual recovery resolves the same ledger
// row. The asymmetric keying this pins down stranded a later-observation-keyed
// checkpoint-failed issue forever when a recapture was cancelled at the direct
// capture budget under load (2026-08-31 flake).
func TestRecaptureFailureKeysIssueToCheckpointTriggerAndRecoveryResolvesIt(t *testing.T) {
	cwd := t.TempDir()
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	old := governor
	governor = NewGovernor(ix, nil)
	t.Cleanup(func() { governor = old; _ = ix.Close() })
	e := testV1Envelope(t, cwd)
	if w, first := postV1(t, e); w.Code != http.StatusOK || first.Checkpoint.Status != "unavailable" {
		t.Fatalf("first=%d receipt=%+v", w.Code, first)
	}
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.name", "Crossing Guard"}, {"config", "user.email", "crossing-guard@example.invalid"}} {
		if out, err := exec.Command("git", append([]string{"-C", cwd}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(cwd, "a.go"), []byte("package a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "a.go"}, {"commit", "-q", "-m", "base"}} {
		if out, err := exec.Command("git", append([]string{"-C", cwd}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	prior := captureCheckpointEvidence
	t.Cleanup(func() { captureCheckpointEvidence = prior })
	captureCheckpointEvidence = func(context.Context, changeenv.SnapshotInput) (*store.ChangeRecord,
		[]store.CheckpointPayload, error) {
		return nil, nil, errors.New("transient capture failure under load")
	}
	e.ObservationID = "obs_1123456789abcdef0123456789abcdef"
	if w, failed := postV1(t, e); w.Code != http.StatusOK || failed.Checkpoint.Status != "failed" ||
		failed.Checkpoint.FailureKind != "git-capture-failed" {
		t.Fatalf("failed recapture=%d receipt=%+v", w.Code, failed)
	}
	captureCheckpointEvidence = prior
	e.ObservationID = "obs_2123456789abcdef0123456789abcdef"
	if w, recovered := postV1(t, e); w.Code != http.StatusOK || recovered.Checkpoint.Status != "complete" {
		t.Fatalf("recovery=%d receipt=%+v", w.Code, recovered)
	}
	issues, err := ix.CollectionIssuesForSession(e.SessionID, 10)
	if err != nil || len(issues) != 1 || issues[0].Kind != "checkpoint-failed" ||
		issues[0].ObservationID != "obs_0123456789abcdef0123456789abcdef" || issues[0].ResolvedAt == 0 {
		t.Fatalf("issues=%+v err=%v", issues, err)
	}
}
