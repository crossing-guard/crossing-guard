package daemon

// Contract for the session edit set (workspace-panes plan §4.2): section=edits
// lists only reconciled edits that carry a body, collapses the live and
// transcript copies of one logical result to the live copy, and releases each
// replacement side through body_kind=effect_before / effect_after.

import (
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"crossing-guard/internal/changeenv"
	"crossing-guard/internal/observation"
	"crossing-guard/store"
)

// seedAttempt records the pre-tool attempt a result reconciles against.
func seedAttempt(t *testing.T, g *Governor, session, nativeID string) string {
	t.Helper()
	hex := observation.DigestBytes([]byte(session + "/" + nativeID))[len("sha256-v1:"):][:32]
	if _, err := g.ObserveV1(Observation{SessionID: session, Runtime: "claude", Tool: "Edit",
		Decision: "allow", TS: time.Now().Unix(), Origin: "live"}, ObservationEvidence{
		Delivery: store.EventDelivery{ObservationID: "obs_" + hex, ObservationSchema: observation.SchemaV1,
			EnvelopeDigest: "sha256-v1:attempt-" + nativeID, CollectorID: observation.CollectorPreTool,
			NativeCallID: nativeID, NativeCallKind: "tool_use_id", QueuedAt: time.Now().Unix(),
			ReceivedAt: time.Now().Unix(), DeliveryAttempts: 1, DeliveryMode: "direct"},
		Input: store.EventInput{Completeness: "unavailable"}}); err != nil {
		t.Fatal(err)
	}
	return hex
}

func seedLinkedEdit(t *testing.T, g *Governor, session, nativeID string) int64 {
	t.Helper()
	hex := seedAttempt(t, g, session, nativeID)
	envelope := validResultEnvelope(session, nativeID)
	envelope.ObservationID = "res_" + hex
	receipt, err := ingestResultV1(g, envelope)
	if err != nil || receipt.JoinClass != "exact" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	if _, err := g.ix.ReconcileResultObservation(receipt.ResultID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	return receipt.ResultID
}

func requestEdits(t *testing.T, query string) changeenv.SessionEdits {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/govern/session?"+query, nil)
	w := httptest.NewRecorder()
	handleGovernSession(w, req)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var got sessionEditsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Section != "edits" {
		t.Fatalf("section=%q", got.Section)
	}
	return got.Edits
}

func TestSessionEditsListsLinkedBodiedEffectsAndReleasesReplacementSides(t *testing.T) {
	g := resultTestGovernor(t)
	prior := governor
	governor = g
	t.Cleanup(func() { governor = prior })
	live := seedLinkedEdit(t, g, "edits-1", "call-1")
	// An unjoined result (no attempt to reconcile against) is not an edit of record.
	orphan := validResultEnvelope("edits-1", "call-orphan")
	orphan.ObservationID = "res_" + observation.DigestBytes([]byte("orphan"))[len("sha256-v1:"):][:32]
	if receipt, err := ingestResultV1(g, orphan); err != nil || receipt.JoinClass == "exact" {
		t.Fatalf("orphan receipt=%+v err=%v", receipt, err)
	}
	// A transcript copy of the same logical result collapses to the live copy.
	copyID, _, err := g.ix.AppendResultObservation(store.ResultObservation{ObservationID: "res_" + observation.DigestBytes([]byte("copy"))[len("sha256-v1:"):][:32],
		SessionID: "edits-1", Runtime: "claude", Tool: "Edit", NativeCallID: "call-1", NativeCallKind: "tool_use_id",
		SourceKind: "vendor-transcript", SourceRef: "/tmp/t.jsonl", SourceDigest: "sha256-v1:copy", State: "success",
		CompletedAt: time.Now().Unix(), Completeness: "metadata-only", DeliveryMode: "direct"},
		[]store.ResultEffect{{Ordinal: 0, RawIdentity: "src/a.go", Operation: "update", EvidenceSource: "derived-input",
			Completeness: "partial", ReplacementAfterBytes: 3, ReplacementAfterDigest: observation.DigestBytes([]byte("new")),
			ReplacementAfterPayload: []byte("new"), DiffCompleteness: "unavailable"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.ix.ReconcileResultObservation(copyID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if err := g.ix.RelateLogicalResults(copyID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	edits := requestEdits(t, "id=edits-1&section=edits")
	if edits.Total != 1 || edits.Returned != 1 || len(edits.Files) != 1 || edits.Files[0].Path != "src/a.go" {
		t.Fatalf("edits=%+v", edits)
	}
	edit := edits.Files[0].Edits[0]
	if edit.ResultID != live || edit.Kind != "replacement" || edit.BeforeBytes != 3 || edit.AfterBytes != 3 || edit.Operation != "update" || edit.Tool != "Edit" {
		t.Fatalf("edit=%+v (live=%d copy=%d)", edit, live, copyID)
	}
	// A live copy that retained no body never hides the transcript copy that did:
	// only copies inside the bodied population displace one another.
	bare := validResultEnvelope("edits-1", "call-bare")
	bare.ObservationID = "res_" + observation.DigestBytes([]byte("bare"))[len("sha256-v1:"):][:32]
	bare.Effects[0].BeforeBytes, bare.Effects[0].BeforeDigest, bare.Effects[0].BeforePayload = 0, "", nil
	bare.Effects[0].AfterBytes, bare.Effects[0].AfterDigest, bare.Effects[0].AfterPayload = 0, "", nil
	seedAttempt(t, g, "edits-1", "call-bare")
	if _, err := ingestResultV1(g, bare); err != nil {
		t.Fatal(err)
	}
	bodied, _, err := g.ix.AppendResultObservation(store.ResultObservation{ObservationID: "res_" + observation.DigestBytes([]byte("bodied"))[len("sha256-v1:"):][:32],
		SessionID: "edits-1", Runtime: "claude", Tool: "Edit", NativeCallID: "call-bare", NativeCallKind: "tool_use_id",
		SourceKind: "vendor-transcript", SourceRef: "/tmp/t.jsonl", SourceDigest: "sha256-v1:bodied", State: "success",
		CompletedAt: time.Now().Unix() + 5, Completeness: "metadata-only", DeliveryMode: "direct"},
		[]store.ResultEffect{{Ordinal: 0, RawIdentity: "src/b.go", Operation: "update", EvidenceSource: "derived-input",
			Completeness: "partial", ReplacementAfterBytes: 4, ReplacementAfterDigest: observation.DigestBytes([]byte("four")),
			ReplacementAfterPayload: []byte("four"), DiffCompleteness: "unavailable"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.ix.ReconcileResultObservation(bodied, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if err := g.ix.RelateLogicalResults(bodied, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	after := requestEdits(t, "id=edits-1&section=edits")
	paths := map[string]int{}
	for _, file := range after.Files {
		paths[file.Path] += len(file.Edits)
	}
	if paths["src/b.go"] != 1 {
		t.Fatalf("bodied transcript copy hidden by a bodiless live peer: %+v", after.Files)
	}
	for kind, want := range map[string]string{"effect_before": "old", "effect_after": "new"} {
		body, err := g.SessionBody("edits-1", kind, 0, live, 0)
		if err != nil || body.Text != want || body.Completeness != "complete" || body.Bytes != len(want) {
			t.Fatalf("%s body=%+v err=%v", kind, body, err)
		}
	}
	// The transcript copy never retained a before side: that side is unavailable, not empty.
	if _, err := g.SessionBody("edits-1", "effect_before", 0, copyID, 0); err == nil {
		t.Fatal("unretained before side was released")
	}
	// Bodies stay owned by the session that recorded them.
	if _, err := g.SessionBody("other", "effect_after", 0, live, 0); err == nil {
		t.Fatal("foreign session released a replacement body")
	}
	req := httptest.NewRequest("GET", "/api/govern/session?id=edits-1&section=body&body_kind=effect_before&result_id="+strconv.FormatInt(live, 10)+"&ordinal=0", nil)
	w := httptest.NewRecorder()
	handleGovernSession(w, req)
	if w.Code != 200 {
		t.Fatalf("body route status %d: %s", w.Code, w.Body.String())
	}
}

func TestSessionEditsPagesUnderTheConfiguredCeiling(t *testing.T) {
	g := resultTestGovernor(t)
	prior := governor
	governor = g
	t.Cleanup(func() { governor = prior })
	seedLinkedEdit(t, g, "edits-2", "call-a")
	seedLinkedEdit(t, g, "edits-2", "call-b")
	seedLinkedEdit(t, g, "edits-2", "call-c")
	page := requestEdits(t, "id=edits-2&section=edits&limit=2")
	if page.Total != 3 || page.Returned != 2 || page.Limit != 2 || page.Offset != 0 {
		t.Fatalf("page=%+v", page)
	}
	next := requestEdits(t, "id=edits-2&section=edits&limit=2&offset=2")
	if next.Total != 3 || next.Returned != 1 || next.Offset != 2 {
		t.Fatalf("next=%+v", next)
	}
	config, _ := consoleConfig()
	over := requestEdits(t, "id=edits-2&section=edits&limit=999999")
	if over.Limit != config.EditsPageSize {
		t.Fatalf("limit %d exceeded edits_page_size %d", over.Limit, config.EditsPageSize)
	}
	for _, bad := range []string{"limit=0", "limit=x", "offset=-1"} {
		req := httptest.NewRequest("GET", "/api/govern/session?id=edits-2&section=edits&"+bad, nil)
		w := httptest.NewRecorder()
		handleGovernSession(w, req)
		if w.Code != 400 {
			t.Fatalf("%s: status %d", bad, w.Code)
		}
	}
}
