package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"crossing-guard/internal/observation"
	"crossing-guard/internal/sessionactivity"
	"crossing-guard/store"
)

// foldHarness wires the fold's owners to a fresh store, with tasks and
// approvals absent, and restores the package globals afterwards.
func foldHarness(t *testing.T) *Governor {
	t.Helper()
	// Fixture session resolution must not scan the developer's vendor history.
	t.Setenv("HOME", t.TempDir())
	ix, err := store.Open(t.TempDir() + "/index.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	g := NewGovernor(ix, nil)
	prevGov, prevTasks, prevApprovals := governor, runtimeTasks, approvals
	governor, runtimeTasks, approvals = g, nil, nil
	t.Cleanup(func() { governor, runtimeTasks, approvals = prevGov, prevTasks, prevApprovals; ix.Close() })
	return g
}

func landTurn(t *testing.T, g *Governor, session, kind string, offset int) {
	t.Helper()
	envelope := observation.SessionTurnEnvelope{
		Schema:        observation.SessionTurnSchemaV1,
		ObservationID: "trn_" + strings.Repeat("0", 31) + string(rune('a'+offset)),
		CollectorID:   observation.CollectorSessionTurn, Runtime: "claude", SessionID: session,
		Kind: kind, ObservedAt: 1_800_000_000, QueuedAt: 1_800_000_000, DeliveryAttempts: 1, DeliveryMode: "direct",
	}
	if _, err := ingestSessionTurnV1(g, envelope); err != nil {
		t.Fatal(err)
	}
}

func TestFoldReadsObservedTurnRows(t *testing.T) {
	g := foldHarness(t)
	defer swapSessionStatusRefold(func(string, string) {})()
	landTurn(t, g, "ses-fold", "turn.started", 0)
	landTurn(t, g, "ses-fold", "turn.ended", 1)
	frame, err := foldSessionStatus(time.Now().UTC(), "claude", "ses-fold", "ses-fold")
	if err != nil {
		t.Fatal(err)
	}
	if frame.Execution != "waiting" || frame.Authority != "observed" || frame.Attention != "new_result" || frame.AttentionSource != "turn" {
		t.Fatalf("frame=%+v", frame)
	}
}

// I7: a session end within the attribution window of an owned task's
// completion is that process ending; a human's end outside it is the
// session's end. Both directions are pinned.
func TestFoldOwnedEndAttribution(t *testing.T) {
	now := time.Now().UTC()
	window := sessionStreamConfig().OwnedEndAttribution()
	endRow := sessionStatusFact{Kind: "session.ended", AtMS: lifecycleInstantMS(now.Add(-time.Minute).Unix())}
	within := &sessionStatusOwned{Lifecycle: TaskCompleted, UpdatedAtMS: endRow.AtMS - window.Milliseconds()/2, HasVisibleOutput: true}
	outside := &sessionStatusOwned{Lifecycle: TaskCompleted, UpdatedAtMS: endRow.AtMS - 3*window.Milliseconds(), HasVisibleOutput: true}
	// The fold's exclusion is applied while gathering; replicate its rule
	// here exactly as written so the decision is pinned, not the plumbing.
	excluded := absMS(within.UpdatedAtMS-endRow.AtMS) <= window.Milliseconds()
	kept := absMS(outside.UpdatedAtMS-endRow.AtMS) <= window.Milliseconds()
	if !excluded || kept {
		t.Fatalf("attribution window misapplied: excluded=%v kept=%v", excluded, kept)
	}
	// Excluded end → owned terminal stands (the end was our process).
	f := decideSessionStatus(sessionStatusInputs{Now: now, Quiet: 5 * time.Minute, Owned: within})
	if f.Execution != "terminal" {
		t.Fatalf("owned process end must leave the task terminal: %+v", f)
	}
	// Kept end → the human's session ended; the unread output survives.
	f = decideSessionStatus(sessionStatusInputs{Now: now, Quiet: 5 * time.Minute, Owned: outside, Facts: []sessionStatusFact{endRow}})
	if f.Execution != "idle" || f.Attention != "new_result" {
		t.Fatalf("a human's own end must read idle and keep the unread marker: %+v", f)
	}
}

func TestFoldIgnoresLostTaskState(t *testing.T) {
	now := time.Now().UTC()
	f := decideSessionStatus(sessionStatusInputs{Now: now, Quiet: 5 * time.Minute,
		Owned: &sessionStatusOwned{Lifecycle: TaskUnknown, UpdatedAtMS: now.UnixMilli()}})
	if f.Execution != "unknown" || f.Authority != "none" {
		t.Fatalf("lost task state must not read as terminal/owned: %+v", f)
	}
}

// The event-driven refold must fold the item's OWN native identity even when
// the publish arrived under its catalog id.
func TestRefoldUsesItemNativeIdentity(t *testing.T) {
	g := foldHarness(t)
	defer swapSessionStatusRefold(func(string, string) {})()
	service := sessionactivity.NewService(func(context.Context, time.Time) (sessionactivity.Capability, []sessionactivity.Item) {
		return sessionactivity.Capability{Status: "available"}, nil
	}, time.Hour, time.Second)
	service.Replace(sessionactivity.Item{Runtime: "claude", CatalogSessionID: "rollout-catalog", NativeSessionID: "thread-native",
		Presence: "open", Execution: "unknown", Evidence: "file_open", Freshness: "live", Authority: "observed"})
	landTurn(t, g, "thread-native", "turn.ended", 2)
	refoldSessionStatusNow(service, "claude", "rollout-catalog")
	items := service.Snapshot().Items
	if len(items) != 1 || items[0].Execution != "waiting" || items[0].AttentionSource != "turn" {
		t.Fatalf("refold by catalog id must fold the native identity: %+v", items)
	}
}

// A broken owner renders honest silence and is logged, never a stated state
// built from half the evidence.
func TestFoldSurfacesOwnerFailure(t *testing.T) {
	g := foldHarness(t)
	g.ix.Close() // every store read now fails
	frame, err := foldSessionStatus(time.Now().UTC(), "claude", "ses-broken", "ses-broken")
	if err == nil || frame.Execution != "unknown" || frame.Authority != "none" {
		t.Fatalf("frame=%+v err=%v", frame, err)
	}
}

// The DDL's kind CHECK and the framework vocabulary must be the same set.
func TestTurnTableAdmitsExactlyTheVocabulary(t *testing.T) {
	g := foldHarness(t)
	for kind := range observation.SessionTurnKinds {
		tx, _ := g.ix.BeginGov()
		if err := tx.EnsureSessionRoot("claude", "ses-vocab", "/tmp/v.jsonl", "/tmp"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := tx.AppendSessionTurn(store.SessionTurnObservation{ObservationID: "trn_v_" + kind, Runtime: "claude",
			SessionID: "ses-vocab", Kind: kind, ObservedAt: 1, ReceivedAtMS: 1000, EvidenceDigest: "d", CollectorID: "t",
			DeliveryAttempts: 1, DeliveryMode: "direct"}); err != nil {
			t.Fatalf("vocabulary kind %q rejected by the table: %v", kind, err)
		}
		_ = tx.Commit()
	}
	tx, _ := g.ix.BeginGov()
	_ = tx.EnsureSessionRoot("claude", "ses-vocab", "/tmp/v.jsonl", "/tmp")
	if _, _, err := tx.AppendSessionTurn(store.SessionTurnObservation{ObservationID: "trn_v_foreign", Runtime: "claude",
		SessionID: "ses-vocab", Kind: "Stop", ObservedAt: 1, ReceivedAtMS: 1000, EvidenceDigest: "d", CollectorID: "t",
		DeliveryAttempts: 1, DeliveryMode: "direct"}); err == nil {
		t.Fatal("a provider event name must not be storable as a kind")
	}
	_ = tx.Rollback()
}
