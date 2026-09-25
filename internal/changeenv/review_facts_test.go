package changeenv

import (
	"testing"
	"time"

	"crossing-guard/store"
)

func reviewTag(key string) string {
	return `[{"key":"` + key + `","value":"run","detector":"` + key +
		`.run","provenance":"observed","scope":"session"}]`
}

func TestStatementFactsRemainRebuildableAttributedText(t *testing.T) {
	ix := openIndex(t)
	_, err := ix.ReplaceTranscriptProjection(store.TranscriptProjection{
		Session:    store.SessionRow{Vendor: "runtime-a", ID: "statements"},
		Generation: "g1", IndexedAt: time.Unix(1, 0).UTC(), SourceCount: 1,
		Documents: []store.SearchDocument{
			{Order: 0, Kind: "title", Text: "Statements"},
			{Order: 1, Timestamp: "1", Kind: "user", Text: "continue"},
			{Order: 2, Timestamp: "2", Kind: "assistant", Text: "done"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	facts, err := BuildStatementFacts(ix, "runtime-a", "statements", 0, 25)
	if err != nil {
		t.Fatal(err)
	}
	if facts.State != "rebuildable" || facts.Source != "rebuildable-storage-sanitized-index" ||
		facts.Page.Total != 2 || len(facts.Statements) != 2 || facts.Statements[0].Role != "user" ||
		facts.Statements[0].Snippet != "continue" {
		t.Fatalf("statement facts = %+v", facts)
	}
	withoutRuntime, err := BuildStatementFacts(ix, "", "statements", 0, 25)
	if err != nil || withoutRuntime.State != "unavailable" || withoutRuntime.Reason == "" {
		t.Fatalf("runtime-less statement facts = %+v err=%v", withoutRuntime, err)
	}
}

func TestObservedChecksSeparateExactDeniedPendingAndMissing(t *testing.T) {
	ix := openIndex(t)
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	type fixture struct {
		name     string
		at       int64
		decision string
		callID   string
	}
	fixtures := []fixture{
		{name: "exact", at: 10, decision: "allow", callID: "call-exact"},
		{name: "denied", at: 20, decision: "deny"},
		{name: "pending", at: 30, decision: "allow"},
		{name: "missing", at: 50, decision: "allow"},
	}
	ids := map[string]int64{}
	for _, item := range fixtures {
		eventID, appendErr := tx.AppendEvent(store.EventRecord{TS: item.at, SessionID: "checks",
			Runtime: "runtime-a", Verb: "exec", Tool: "shell", Tags: reviewTag("test"),
			Decision: item.decision, Origin: "live"}, nil)
		if appendErr != nil {
			t.Fatal(appendErr)
		}
		ids[item.name] = eventID
		if item.callID != "" {
			if err := tx.AppendEventDelivery(store.EventDelivery{EventID: eventID,
				ObservationID: "obs-" + item.name, ObservationSchema: "fixture",
				EnvelopeDigest: "sha256-v1:" + item.name, CollectorID: "fixture",
				NativeCallID: item.callID, NativeCallKind: "call-id", ReceivedAt: item.at,
				DeliveryAttempts: 1, DeliveryMode: "direct"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.EnsureSessionRoot("runtime-a", "checks", "/tmp/checks", "/repo"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.AppendSessionActivity(store.SessionActivityObservation{ObservationID: "open-checks",
		Runtime: "runtime-a", SessionID: "checks", State: "open", ObservedAt: 25,
		ValidUntil: 40, EvidenceClass: "explicit-start", EntryKind: "start",
		EvidenceDigest: "sha256-v1:open", CollectorID: "fixture", ReceivedAt: 25,
		DeliveryAttempts: 1, DeliveryMode: "direct"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	resultID, _, err := ix.AppendResultObservation(store.ResultObservation{ObservationID: "res-exact",
		SessionID: "checks", Runtime: "runtime-a", Tool: "shell", NativeCallID: "call-exact",
		NativeCallKind: "call-id", SourceKind: "live-post-tool", SourceDigest: "sha256-v1:result",
		State: "success", CompletedAt: 11, RawBytes: 20, RetainedBytes: 0,
		Completeness: "metadata-only"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ix.ReconcileResultObservation(resultID, 12); err != nil || got.JoinClass != "exact" {
		t.Fatalf("reconcile = %+v err=%v", got, err)
	}
	facts, err := BuildObservedCheckFacts(ix, "checks", 0, 25)
	if err != nil {
		t.Fatal(err)
	}
	if facts.Page.Total != 4 || len(facts.Checks) != 4 || !facts.ResultLinksComplete {
		t.Fatalf("check facts = %+v", facts)
	}
	want := map[int64]string{ids["exact"]: "terminal_without_retained_body", ids["denied"]: "not_executed",
		ids["pending"]: "pending", ids["missing"]: "missing"}
	for _, check := range facts.Checks {
		if check.ResultState != want[check.EventID] {
			t.Errorf("event %d state=%q want %q: %+v", check.EventID, check.ResultState, want[check.EventID], check)
		}
	}
}
