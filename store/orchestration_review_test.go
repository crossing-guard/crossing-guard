package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func testReviewBinding() ReviewBinding {
	return ReviewBinding{BindingID: ReviewBindingID, State: "enabled", RuntimeFilter: "claude",
		ProfileID: "command-reviewer", ProfileSourceDigest: "sha256-v1:source",
		ProfileBundleDigest: "sha256-v1:bundle", InstructionDigest: "sha256-v1:instructions",
		RequestPathKind: "local-ollama-v1", RequestPathDigest: "sha256-v1:path",
		Endpoint: "http://127.0.0.1:11434", Model: "local-model", TimeoutMS: 5000,
		MaxInputBytes: 8192, MaxOutputBytes: 1024, MaxTokens: 128, MaxConcurrency: 1}
}

func testReviewInvocation(id, actionID, digest string, binding ReviewBinding, admittedAt int64) ReviewInvocation {
	return ReviewInvocation{InvocationID: id, ActionID: actionID, ActionDigest: digest,
		ObservationID: "obs_" + id, EventID: admittedAt, Runtime: "claude", SessionID: "session", Tool: "Bash",
		BindingID: binding.BindingID, BindingStateToken: binding.StateToken, ProfileID: binding.ProfileID,
		ProfileSourceDigest: binding.ProfileSourceDigest, ProfileBundleDigest: binding.ProfileBundleDigest,
		InstructionDigest: binding.InstructionDigest, RequestPathKind: binding.RequestPathKind,
		RequestPathDigest: binding.RequestPathDigest, Endpoint: binding.Endpoint, Model: binding.Model,
		TimeoutMS: binding.TimeoutMS, MaxInputBytes: binding.MaxInputBytes, MaxOutputBytes: binding.MaxOutputBytes,
		MaxTokens: binding.MaxTokens, MaxConcurrency: binding.MaxConcurrency, AdmittedAt: admittedAt,
		TimingClass: "result_not_observed_at_completion"}
}

func TestReviewBindingCASAndDisablePreservePinnedState(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	binding, err := ix.PutReviewBinding(testReviewBinding(), ReviewBindingAbsentToken(), 10)
	if err != nil || binding.StateToken == "" || binding.CreatedAt != 10 || binding.UpdatedAt != 10 {
		t.Fatalf("binding=%+v err=%v", binding, err)
	}
	if _, err := ix.PutReviewBinding(testReviewBinding(), ReviewBindingAbsentToken(), 11); !errors.Is(err, ErrReviewBindingConflict) {
		t.Fatalf("stale create err=%v", err)
	}
	updated := binding
	updated.Model = "new-local-model"
	updated, err = ix.PutReviewBinding(updated, binding.StateToken, 12)
	if err != nil || updated.StateToken == binding.StateToken || updated.CreatedAt != binding.CreatedAt {
		t.Fatalf("updated=%+v err=%v", updated, err)
	}
	disabled, err := ix.DisableReviewBinding(updated.StateToken, 13)
	if err != nil || disabled.State != "disabled" || disabled.StateToken == updated.StateToken {
		t.Fatalf("disabled=%+v err=%v", disabled, err)
	}
	if _, err := ix.DisableReviewBinding(updated.StateToken, 14); !errors.Is(err, ErrReviewBindingConflict) {
		t.Fatalf("stale disable err=%v", err)
	}
}

func TestReviewAdmissionIsIdempotentConflictSafeAndConcurrencyBounded(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	binding, err := ix.PutReviewBinding(testReviewBinding(), ReviewBindingAbsentToken(), 1)
	if err != nil {
		t.Fatal(err)
	}
	first := testReviewInvocation("rev_1", "act_1", "sha256-v1:a", binding, 10)
	first, created, err := ix.AdmitReview(first)
	if err != nil || !created || first.State != "admitted" {
		t.Fatalf("first=%+v created=%v err=%v", first, created, err)
	}
	duplicate, created, err := ix.AdmitReview(testReviewInvocation("rev_other", "act_1", "sha256-v1:a", binding, 11))
	if err != nil || created || duplicate.InvocationID != first.InvocationID {
		t.Fatalf("duplicate=%+v created=%v err=%v", duplicate, created, err)
	}
	if _, _, err := ix.AdmitReview(testReviewInvocation("rev_bad", "act_1", "sha256-v1:different", binding, 12)); !errors.Is(err, ErrReviewActionConflict) {
		t.Fatalf("action conflict err=%v", err)
	}
	suppressed, created, err := ix.AdmitReview(testReviewInvocation("rev_2", "act_2", "sha256-v1:b", binding, 13))
	if err != nil || !created || suppressed.State != "suppressed" || suppressed.ErrorClass != "concurrency_limit" {
		t.Fatalf("suppressed=%+v created=%v err=%v", suppressed, created, err)
	}
	if err := ix.MarkReviewRunning(first.InvocationID, 14); err != nil {
		t.Fatal(err)
	}
	if err := ix.CompleteReview(first.InvocationID, ReviewCompletion{State: "completed", Action: "deny",
		Message: "Independent concern.", Citations: []string{"action.tool"}, CompletedAt: 15, DurationMS: 100}); err != nil {
		t.Fatal(err)
	}
	if err := ix.LinkReviewApprovalOutcome(first.InvocationID, "ap_exact", "apr_ABCDEFGHIJKLMNOPQRSTUVWXYZ", "accepted"); err != nil {
		t.Fatal(err)
	}
	items, err := ix.ReviewInvocationsForSession("claude", "session", 10)
	if err != nil || len(items) != 2 || items[1].State != "completed" || items[1].Action != "deny" || len(items[1].Citations) != 1 ||
		items[1].ApprovalID != "ap_exact" || items[1].ApprovalOutcome != "accepted" {
		t.Fatalf("items=%+v err=%v", items, err)
	}
}

func TestReviewRecoveryMarksUnfinishedUnknownWithoutReplay(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	binding, _ := ix.PutReviewBinding(testReviewBinding(), ReviewBindingAbsentToken(), 1)
	invocation, _, _ := ix.AdmitReview(testReviewInvocation("rev_restart", "act_restart", "sha256-v1:r", binding, 2))
	if err := ix.MarkReviewRunning(invocation.InvocationID, 3); err != nil {
		t.Fatal(err)
	}
	count, err := ix.RecoverReviewInvocationsUnknown(4)
	if err != nil || count != 1 {
		t.Fatalf("recovered=%d err=%v", count, err)
	}
	items, _ := ix.ReviewInvocations(10, "")
	if len(items) != 1 || items[0].State != "unknown" || items[0].ErrorClass != "restart_unknown" {
		t.Fatalf("items=%+v", items)
	}
}

func TestReviewCompletionAndProjectionUseOnlyExactResultEvidence(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()

	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	eventID, err := tx.AppendEvent(EventRecord{TS: 10, SessionID: "session", Runtime: "claude",
		Verb: "exec", Tool: "Bash", Decision: "allow", Origin: "live"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.AppendEventDelivery(EventDelivery{EventID: eventID, ObservationID: "obs_review_exact",
		ActionID: "act_review_exact", ObservationSchema: "fixture", EnvelopeDigest: "sha256-v1:action",
		CollectorID: "fixture", NativeCallID: "call_review_exact", NativeCallKind: "call-id",
		ReceivedAt: 10, DeliveryAttempts: 1, DeliveryMode: "direct"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	binding, err := ix.PutReviewBinding(testReviewBinding(), ReviewBindingAbsentToken(), 11)
	if err != nil {
		t.Fatal(err)
	}
	invocation := testReviewInvocation("rev_exact", "act_review_exact", "sha256-v1:exact", binding, 12)
	invocation.EventID = eventID
	invocation.ObservationID = "obs_review_exact"
	invocation, created, err := ix.AdmitReview(invocation)
	if err != nil || !created {
		t.Fatalf("invocation=%+v created=%v err=%v", invocation, created, err)
	}
	if err := ix.MarkReviewRunning(invocation.InvocationID, 13); err != nil {
		t.Fatal(err)
	}

	resultID, _, err := ix.AppendResultObservation(ResultObservation{ObservationID: "res_review_exact",
		SessionID: "session", Runtime: "claude", Tool: "Bash", NativeCallID: "call_review_exact",
		NativeCallKind: "call-id", SourceKind: "live-post-tool", SourceDigest: "sha256-v1:result",
		State: "success", CompletedAt: 14, ReceivedAt: 20, RawBytes: 0,
		Completeness: "metadata-only", DeliveryAttempts: 1, DeliveryMode: "direct"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reconciliation, err := ix.ReconcileResultObservation(resultID, 21); err != nil || reconciliation.JoinClass != "exact" {
		t.Fatalf("reconciliation=%+v err=%v", reconciliation, err)
	}
	if err := ix.CompleteReview(invocation.InvocationID, ReviewCompletion{State: "completed", Action: "allow",
		Message: "No independent concern.", CompletedAt: 19, DurationMS: 6}); err != nil {
		t.Fatal(err)
	}

	items, err := ix.ReviewInvocations(10, "")
	if err != nil || len(items) != 1 || items[0].TimingClass != "completed_before_exact_result_observation" {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	outcome, err := ix.ReviewActualOutcome(eventID)
	if err != nil || !outcome.Observed || outcome.State != "success" || outcome.ObservedAt != 20 {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
}

func TestSchemaV21MigratesActionIdentityAndReviewTablesTransactionally(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ix.db.Exec(`DROP INDEX event_delivery_action;
		DROP INDEX orchestration_review_active;
		DROP INDEX orchestration_review_session;
		DROP INDEX orchestration_review_history;
		DROP TABLE orchestration_review_invocation;
		DROP TABLE orchestration_review_binding;
		ALTER TABLE event_delivery DROP COLUMN action_id;
		PRAGMA user_version=20`); err != nil {
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
	cols, err := columnSet(migrated.db, "event_delivery")
	if err != nil || !cols["action_id"] {
		t.Fatalf("action_id=%v err=%v", cols["action_id"], err)
	}
	for _, table := range []string{"orchestration_review_binding", "orchestration_review_invocation"} {
		var found int
		if err := migrated.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&found); err != nil || found != 1 {
			t.Fatalf("table %s found=%d err=%v", table, found, err)
		}
	}
	var version int
	if err := migrated.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
}
