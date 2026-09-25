package daemon

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"crossing-guard/internal/transcriptindex"
	"crossing-guard/store"
)

func TestSessionSectionsAreBoundedAndDrillIntoOwnedFacts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	if _, err := ix.ReplaceTranscriptProjection(store.TranscriptProjection{
		Session:    store.SessionRow{Vendor: "opaque-a", ID: "sectioned", Title: "Indexed title"},
		Generation: "g1", IndexedAt: time.Unix(1, 0).UTC(), SourceCount: 1,
		Documents: []store.SearchDocument{
			{Order: 0, Kind: "title", Text: "Indexed title"},
			{Order: 1, Timestamp: "1", Kind: "user", Text: "continue"},
			{Order: 2, Timestamp: "2", Kind: "assistant", Text: "done"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	filePath := filepath.Join(root, "foo.go")
	for index, tool := range []string{"Read", "Edit", "Bash"} {
		tx, beginErr := ix.BeginGov()
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		runtime := []string{"opaque-a", "opaque-b"}[index%2]
		tags := ""
		if tool == "Bash" {
			tags = `[{"key":"test","value":"run","detector":"test.run","provenance":"observed","scope":"session"}]`
		}
		eventID, appendErr := tx.AppendEvent(store.EventRecord{TS: int64(100 + index),
			SessionID: "sectioned", Runtime: runtime, Verb: "use", Tool: tool,
			Tags: tags, Decision: "allow", Origin: "live"}, nil)
		if appendErr != nil {
			t.Fatal(appendErr)
		}
		if index == 1 {
			if err := tx.UpsertEntity("file:"+filePath, "file", filePath, 101); err != nil {
				t.Fatal(err)
			}
			if err := tx.AppendEventResourceEvidence(store.EventResourceEvidence{EventID: eventID,
				EntityID: "file:" + filePath, Source: "tool_input.file_path", RawIdentity: filePath,
				Operation: "write", EvidenceClass: "declared", SourceField: "file_path",
				Completeness: "complete"}); err != nil {
				t.Fatal(err)
			}
			if err := tx.AppendEventDelivery(store.EventDelivery{EventID: eventID,
				ObservationID: "obs-edit", ObservationSchema: "test", EnvelopeDigest: "sha256-v1:env",
				CollectorID: "test", ReceivedAt: 101, DeliveryAttempts: 1, DeliveryMode: "direct"}); err != nil {
				t.Fatal(err)
			}
			if err := tx.AppendEventInput(store.EventInput{EventID: eventID,
				MediaType: "application/json", RawBytes: 2, CapturedBytes: 2,
				Digest: "sha256-v1:input", Completeness: "complete", Payload: []byte(`{}`)}); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	if err := ix.AppendChange(&store.ChangeRecord{SessionID: "sectioned", RepositoryID: "repo",
		CheckoutID: "checkout", RepositoryIdentityKind: "local-sha256", CheckoutRoot: root,
		Kind: "revision", EvidenceClass: "observed", SourceKind: "git", SourceRef: "HEAD",
		SourceDisplay: "HEAD", SourceDigest: "sha256-v1:revision", RecordedAt: 200,
		CaptureStartedAt: 190, CaptureEndedAt: 200, BaseRevision: "base", HeadRevision: "head",
		GitVersion: "test", SnapshotDigest: "sha256-v1:snapshot", CaptureAttempts: 1,
		IncludedLayers: "worktree",
		Items:          []store.ChangeItem{{Path: "foo.go", Layer: "worktree", Status: "M"}}}); err != nil {
		t.Fatal(err)
	}
	prior := governor
	governor = NewGovernor(ix, nil)
	t.Cleanup(func() { governor = prior })
	priorCoverage := daemonTranscriptIndexCoverage.Load()
	daemonTranscriptIndexCoverage.Store(transcriptindex.Coverage{
		State: transcriptindex.CoverageCurrent, DiscoveredSessions: 1, IndexedSessions: 1,
		CoverageAsOf: time.Unix(10, 0).UTC(), Sessions: []transcriptindex.SessionCoverage{{
			Key:   transcriptindex.SessionKey{Runtime: "opaque-a", SessionID: "sectioned"},
			State: transcriptindex.CoverageCurrent,
		}},
	})
	t.Cleanup(func() { daemonTranscriptIndexCoverage.Store(priorCoverage) })

	summary := httptest.NewRecorder()
	handleGovernSession(summary, httptest.NewRequest("GET", "/api/govern/session?id=sectioned&section=summary", nil))
	if summary.Code != 200 {
		t.Fatalf("summary status %d: %s", summary.Code, summary.Body.String())
	}
	if summary.Body.Len() > 64<<10 {
		t.Fatalf("summary exceeded defensive bound: %d", summary.Body.Len())
	}
	var raw map[string]any
	if err := json.Unmarshal(summary.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if raw["section"] != "summary" || raw["events"] != nil || raw["change"] != nil {
		t.Fatalf("summary leaked unrelated rows: %s", summary.Body.String())
	}
	if !strings.Contains(summary.Body.String(), `"title":"Indexed title"`) || !strings.Contains(summary.Body.String(), `"title_source":"index"`) {
		t.Fatalf("summary did not use indexed session metadata: %s", summary.Body.String())
	}
	statements := httptest.NewRecorder()
	handleGovernSession(statements, httptest.NewRequest("GET", "/api/govern/session?id=sectioned&runtime=opaque-a&section=statements&statement_limit=1", nil))
	if statements.Code != 200 || statements.Body.Len() > 64<<10 ||
		!strings.Contains(statements.Body.String(), `"state":"rebuildable"`) ||
		!strings.Contains(statements.Body.String(), `"coverage":{"state":"current"`) ||
		!strings.Contains(statements.Body.String(), `"total":2`) ||
		strings.Contains(statements.Body.String(), `"tool_call"`) {
		t.Fatalf("bounded statements status %d: %s", statements.Code, statements.Body.String())
	}
	checks := httptest.NewRecorder()
	handleGovernSession(checks, httptest.NewRequest("GET", "/api/govern/session?id=sectioned&section=checks&check_limit=25", nil))
	if checks.Code != 200 || checks.Body.Len() > 64<<10 ||
		!strings.Contains(checks.Body.String(), `"total":1`) ||
		!strings.Contains(checks.Body.String(), `"result_state":"missing"`) {
		t.Fatalf("bounded checks status %d: %s", checks.Code, checks.Body.String())
	}
	codeChanges := httptest.NewRecorder()
	handleGovernSession(codeChanges, httptest.NewRequest("GET", "/api/govern/session?id=sectioned&section=code-changes&code_limit=1", nil))
	if codeChanges.Code != 200 || codeChanges.Body.Len() > 512<<10 ||
		!strings.Contains(codeChanges.Body.String(), `"function_call_coverage":"unavailable"`) {
		t.Fatalf("bounded code-change status %d: %s", codeChanges.Code, codeChanges.Body.String())
	}
	for _, forbidden := range []string{`"duplicate"`, `"huge_method"`, `"controller_violation"`, `"quality_score"`} {
		if strings.Contains(codeChanges.Body.String(), forbidden) {
			t.Fatalf("code-change response leaked judgment %s: %s", forbidden, codeChanges.Body.String())
		}
	}
	badCodeLimit := httptest.NewRecorder()
	handleGovernSession(badCodeLimit, httptest.NewRequest("GET", "/api/govern/session?id=sectioned&section=code-changes&code_limit=26", nil))
	if badCodeLimit.Code != 400 {
		t.Fatalf("invalid code-change limit status=%d body=%s", badCodeLimit.Code, badCodeLimit.Body.String())
	}
	unsafeCodeFile := httptest.NewRecorder()
	handleGovernSession(unsafeCodeFile, httptest.NewRequest("GET", "/api/govern/session?id=sectioned&section=code-change-file&path=../escape.go", nil))
	if unsafeCodeFile.Code != 400 {
		t.Fatalf("unsafe code-change path status=%d body=%s", unsafeCodeFile.Code, unsafeCodeFile.Body.String())
	}

	first := httptest.NewRecorder()
	handleGovernSession(first, httptest.NewRequest("GET", "/api/govern/session?id=sectioned&section=trace&limit=1", nil))
	if first.Code != 200 {
		t.Fatalf("trace status %d: %s", first.Code, first.Body.String())
	}
	if first.Body.Len() > 512<<10 {
		t.Fatalf("trace page exceeded defensive bound: %d", first.Body.Len())
	}
	var trace sessionTraceResponse
	if err := json.Unmarshal(first.Body.Bytes(), &trace); err != nil {
		t.Fatal(err)
	}
	if len(trace.Events) != 1 || trace.EventCount != 3 || trace.NextCursor == "" {
		t.Fatalf("bad first trace page: %+v", trace)
	}
	second := httptest.NewRecorder()
	handleGovernSession(second, httptest.NewRequest("GET", "/api/govern/session?id=sectioned&section=trace&limit=1&cursor="+url.QueryEscape(trace.NextCursor), nil))
	if second.Code != 200 {
		t.Fatalf("second trace status %d: %s", second.Code, second.Body.String())
	}
	var next sessionTraceResponse
	if err := json.Unmarshal(second.Body.Bytes(), &next); err != nil {
		t.Fatal(err)
	}
	if len(next.Events) != 1 || next.Events[0].ID == trace.Events[0].ID || next.SnapshotID != trace.SnapshotID {
		t.Fatalf("cursor did not preserve a stable walk: first=%+v next=%+v", trace, next)
	}

	action := httptest.NewRecorder()
	handleGovernSession(action, httptest.NewRequest("GET", "/api/govern/session?id=sectioned&section=action&event_id="+strconv.FormatInt(next.Events[0].ID, 10), nil))
	if action.Code != 200 {
		t.Fatalf("action status %d: %s", action.Code, action.Body.String())
	}
	if !strings.Contains(action.Body.String(), `"result_count":0`) || !strings.Contains(action.Body.String(), `"results_truncated":false`) {
		t.Fatalf("action response omitted explicit result bounds: %s", action.Body.String())
	}
	body := httptest.NewRecorder()
	handleGovernSession(body, httptest.NewRequest("GET", "/api/govern/session?id=sectioned&section=body&body_kind=event_input&event_id="+strconv.FormatInt(next.Events[0].ID, 10), nil))
	if body.Code != 200 || !strings.Contains(body.Body.String(), `"text":"{}"`) {
		t.Fatalf("retained body status %d: %s", body.Code, body.Body.String())
	}
	wrongBody := httptest.NewRecorder()
	handleGovernSession(wrongBody, httptest.NewRequest("GET", "/api/govern/session?id=other&section=body&body_kind=event_input&event_id="+strconv.FormatInt(next.Events[0].ID, 10), nil))
	if wrongBody.Code != 404 {
		t.Fatalf("cross-session body status=%d body=%s", wrongBody.Code, wrongBody.Body.String())
	}
	badBody := httptest.NewRecorder()
	handleGovernSession(badBody, httptest.NewRequest("GET", "/api/govern/session?id=sectioned&section=body&body_kind=effect_diff&result_id=bad&ordinal=-1", nil))
	if badBody.Code != 400 {
		t.Fatalf("invalid body selector status=%d body=%s", badBody.Code, badBody.Body.String())
	}
	wrong := httptest.NewRecorder()
	handleGovernSession(wrong, httptest.NewRequest("GET", "/api/govern/session?id=other&section=action&event_id="+strconv.FormatInt(next.Events[0].ID, 10), nil))
	if wrong.Code != 404 {
		t.Fatalf("cross-session action status=%d body=%s", wrong.Code, wrong.Body.String())
	}

	change := httptest.NewRecorder()
	handleGovernSession(change, httptest.NewRequest("GET", "/api/govern/session?id=sectioned&section=change&change_limit=25", nil))
	var changed sessionChangeResponse
	if change.Code != 200 || json.Unmarshal(change.Body.Bytes(), &changed) != nil {
		t.Fatalf("change response %d: %s", change.Code, change.Body.String())
	}
	key := changed.Change.Repositories[0].Files[0].EvidenceKey
	if key == "" {
		t.Fatal("change row has no server-owned file evidence key")
	}
	impact := httptest.NewRecorder()
	handleGovernSession(impact, httptest.NewRequest("GET", "/api/govern/session?id=sectioned&section=impact&impact_limit=25", nil))
	if impact.Code != 200 || strings.Contains(impact.Body.String(), `"files"`) || strings.Contains(impact.Body.String(), `"verification"`) || strings.Contains(impact.Body.String(), `"observed_touches"`) {
		t.Fatalf("impact leaked unrelated Change populations: %d %s", impact.Code, impact.Body.String())
	}
	verify := httptest.NewRecorder()
	handleGovernSession(verify, httptest.NewRequest("GET", "/api/govern/session?id=sectioned&section=verify&repository_limit=25", nil))
	if verify.Code != 200 || strings.Contains(verify.Body.String(), `"files"`) || strings.Contains(verify.Body.String(), `"downstream"`) || strings.Contains(verify.Body.String(), `"observed_touches"`) {
		t.Fatalf("verify leaked unrelated Change populations: %d %s", verify.Code, verify.Body.String())
	}
	file := httptest.NewRecorder()
	handleGovernSession(file, httptest.NewRequest("GET", "/api/govern/session?id=sectioned&section=file&file_key="+url.QueryEscape(key), nil))
	if file.Code != 200 {
		t.Fatalf("file detail status %d: %s", file.Code, file.Body.String())
	}
}
