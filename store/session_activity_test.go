package store

import (
	"path/filepath"
	"testing"
)

// seedLifecycle appends one lifecycle row through the owning writer.
func seedLifecycle(t *testing.T, ix *Index, id, runtime, session, kind string, observedAt int64) {
	t.Helper()
	gov, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	if err := gov.EnsureSessionRoot(runtime, session, "/tmp/"+session+".jsonl", "/tmp"); err != nil {
		t.Fatal(err)
	}
	observation := SessionActivityObservation{
		ObservationID: id, Runtime: runtime, SessionID: session,
		State: "open", ObservedAt: observedAt, ValidUntil: observedAt + 3600,
		EvidenceClass: "explicit-start", EntryKind: kind,
		EvidenceDigest: "sha256-v1:" + id, CollectorID: "test",
		ReceivedAt: observedAt, DeliveryAttempts: 1, DeliveryMode: "direct",
	}
	if kind == "end" {
		observation.State, observation.ValidUntil, observation.EvidenceClass = "stopped", 0, "explicit-end"
	}
	if _, err := gov.AppendSessionActivity(observation); err != nil {
		t.Fatal(err)
	}
	if err := gov.Commit(); err != nil {
		t.Fatal(err)
	}
}

func seedEvent(t *testing.T, ix *Index, session string, ts int64) {
	t.Helper()
	if _, err := ix.db.Exec(`INSERT INTO event(ts,session_id,runtime,verb,tool,target_entity_id,tags,decision,reason,origin)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, ts, session, "claude", "exec", "Bash", "", "", "allow", "", "hook"); err != nil {
		t.Fatal(err)
	}
}

// TestHookLiveSessionsHonorsAppendOrderAndWindows pins the
// session-presence-honesty query: start-with-no-end sessions with a recent
// event appear with their newest event time; an END row wins by APPEND order
// even when its wall-clock observed_at is EARLIER than the last tool event
// (red-team P1 — hook clocks skew); sessions without a recent event are
// absent (absence of evidence reported as absence).
func TestHookLiveSessionsHonorsAppendOrderAndWindows(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	now := int64(1_800_000_000)
	horizon, cutoff := now-7*24*3600, now-3600

	// live: started, evented 30s ago.
	seedLifecycle(t, ix, "obs-live-start", "claude", "ses-live", "start", now-1800)
	seedEvent(t, ix, "ses-live", now-300)
	seedEvent(t, ix, "ses-live", now-30)
	// ended: started, evented, then END appended LAST with an EARLIER
	// observed_at than the final event (clock skew) — must not appear.
	seedLifecycle(t, ix, "obs-skew-start", "claude", "ses-skew", "start", now-1800)
	seedEvent(t, ix, "ses-skew", now-60)
	seedLifecycle(t, ix, "obs-skew-end", "claude", "ses-skew", "end", now-120)
	// idle: started inside the horizon, no event inside the window.
	seedLifecycle(t, ix, "obs-idle-start", "claude", "ses-idle", "start", now-7200)
	seedEvent(t, ix, "ses-idle", now-7000)
	// resumed counts as open.
	seedLifecycle(t, ix, "obs-resume", "claude", "ses-resume", "resume", now-900)
	seedEvent(t, ix, "ses-resume", now-600)

	rows, err := ix.HookLiveSessions(horizon, cutoff, 50)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]int64{}
	for _, row := range rows {
		byID[row.SessionID] = row.LastEventAt
	}
	if len(rows) != 2 {
		t.Fatalf("live set wrong: %+v", rows)
	}
	if byID["ses-live"] != now-30 {
		t.Fatalf("newest event time lost: %+v", rows)
	}
	if _, ok := byID["ses-skew"]; ok {
		t.Fatal("append-order end row did not close the session (P1)")
	}
	if _, ok := byID["ses-idle"]; ok {
		t.Fatal("idle session leaked past the event window")
	}
	if _, ok := byID["ses-resume"]; !ok {
		t.Fatal("resumed session missing from the live set")
	}
}
