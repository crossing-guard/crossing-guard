package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"crossing-guard/internal/orchestration/profilefs"
	"crossing-guard/store"
)

// repeatedTagFixture writes one old `reviewed` claim in each of two quiet
// sessions and, newer than both, more repeats of `reviewed` in one busy session
// than the default recall.tag_lookup_limit (200) — the shape an annotator that
// re-claims every turn leaves behind.
func repeatedTagFixture(t *testing.T) (*http.ServeMux, *store.Index) {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // ScanSessions reads no real transcripts
	root := withConsoleDataDir(t) // consoleConfig reads this test's daemon.json, not a leaked one
	owner, err := profilefs.New(root)
	if err != nil {
		t.Fatal(err)
	}
	ix, err := store.Open(filepath.Join(root, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	host, err := newOrchestrationManagedHost(ix, owner, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { host.close(); _ = ix.Close() })
	mux := http.NewServeMux()
	registerOrchestrationManagedRoutes(mux, host, owner)

	binding, err := ix.PutManagedBinding(store.ManagedBinding{BindingID: "agent-xs", State: "enabled",
		Role: "follower", ProjectRoot: root, ProfileID: "follower", ProfileSourceDigest: "sha256-v1:s",
		ProfileBundleDigest: "sha256-v1:b", Runtime: "managed-fixture", Mode: "", Authority: []string{},
		AllowedProfiles: []store.ManagedProfileRef{}}, store.ManagedBindingAbsentToken("agent-xs"), 1)
	if err != nil {
		t.Fatal(err)
	}
	group := store.ManagedGroup{GroupID: "org_xs", BindingID: binding.BindingID, State: "active",
		RootTaskID: "task_root", RootRuntime: "managed-fixture", ProjectRoot: root, CreatedAt: 1, UpdatedAt: 1}
	run := store.ManagedRun{RunID: "orun_xs", IdempotencyKey: "idem_xs", GroupID: group.GroupID,
		BindingID: binding.BindingID, BindingStateToken: binding.StateToken, Role: "follower",
		ProfileID: "follower", ProfileSourceDigest: "sha256-v1:s", ProfileBundleDigest: "sha256-v1:b",
		SourceTaskID: "task_root", SourceEventID: 1, AdmittedAt: 1, Citations: []string{}, Detail: map[string]any{}}
	if _, _, err := ix.AdmitManagedRun(group, run, store.ManagedGroupBudget{MaxTotal: 8, MaxActive: 2}); err != nil {
		t.Fatal(err)
	}
	row := func(id, session string, applied int64) store.OrchestrationTag {
		return store.OrchestrationTag{TagID: id, RunID: run.RunID, BindingID: binding.BindingID,
			AgentKey: "agent:agent-xs:reviewed", Tag: "reviewed", Runtime: "managed-fixture",
			SessionID: session, Anchor: "1", AppliedAt: applied}
	}
	tags := []store.OrchestrationTag{row("otag_q1", "quiet-1", 10), row("otag_q2", "quiet-2", 11)}
	for i := 0; i < 201; i++ {
		tags = append(tags, row(fmt.Sprintf("otag_busy_%03d", i), "busy", int64(100+i)))
	}
	if err := ix.PutOrchestrationTags(tags); err != nil {
		t.Fatal(err)
	}
	return mux, ix
}

func getTagJSON(t *testing.T, mux *http.ServeMux, path string, into any) {
	t.Helper()
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s status=%d body=%s", path, response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), into); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
}

func TestTagLookupListsEverySessionPastOneSessionsRepeats(t *testing.T) {
	mux, _ := repeatedTagFixture(t)
	for _, path := range []string{"/api/orchestration/tags?tag=reviewed", "/api/orchestration/tags?agent_key=agent:agent-xs:reviewed"} {
		var got tagLookupResponse
		getTagJSON(t, mux, path, &got)
		ids := []string{}
		for _, session := range got.Sessions {
			ids = append(ids, session.ID)
		}
		if fmt.Sprint(ids) != "[busy quiet-2 quiet-1]" || got.Truncated || len(got.Tags) != 3 {
			t.Fatalf("GET %s sessions=%v truncated=%v tags=%d; want [busy quiet-2 quiet-1], uncut, one row each",
				path, ids, got.Truncated, len(got.Tags))
		}
		if got.Tags[0].TagID != "otag_busy_200" {
			t.Fatalf("GET %s kept %s for busy; want its newest claim otag_busy_200", path, got.Tags[0].TagID)
		}
	}
}

// The limit still cuts distinct sessions, and says so: recall.tag_lookup_limit
// 2 lists the two newest sessions and reports truncated.
func TestTagLookupStillReportsACutOverDistinctSessions(t *testing.T) {
	mux, _ := repeatedTagFixture(t)
	root := consoleDataDir()
	if err := os.WriteFile(filepath.Join(root, "daemon.json"), []byte(`{"recall":{"tag_lookup_limit":2}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	invalidateConsoleConfig()
	if limits, _ := consoleConfig(); limits.Recall.TagLookupLimit != 2 {
		t.Fatalf("tag_lookup_limit=%d; the test's daemon.json was not read", limits.Recall.TagLookupLimit)
	}
	var got tagLookupResponse
	getTagJSON(t, mux, "/api/orchestration/tags?tag=reviewed", &got)
	ids := []string{}
	for _, session := range got.Sessions {
		ids = append(ids, session.ID)
	}
	if fmt.Sprint(ids) != "[busy quiet-2]" || !got.Truncated {
		t.Fatalf("sessions=%v truncated=%v; want [busy quiet-2] truncated", ids, got.Truncated)
	}
	var summary tagSummaryResponse
	getTagJSON(t, mux, "/api/orchestration/tags/summary", &summary)
	if !summary.Truncated || len(summary.Tags) != 1 || summary.Tags[0].Sessions != 2 {
		t.Fatalf("summary=%+v; want 2 sessions counted, truncated", summary)
	}
}

func TestTagSummaryCountsEverySessionPastOneSessionsRepeats(t *testing.T) {
	mux, _ := repeatedTagFixture(t)
	var got tagSummaryResponse
	getTagJSON(t, mux, "/api/orchestration/tags/summary", &got)
	if got.Truncated || len(got.Tags) != 1 || got.Tags[0].Tag != "reviewed" || got.Tags[0].Sessions != 3 {
		t.Fatalf("summary=%+v; want reviewed in 3 sessions, uncut", got)
	}
}

func TestRailTagSnapshotHoldsEverySessionPastOneSessionsRepeats(t *testing.T) {
	_, ix := repeatedTagFixture(t)
	snapshot := readSessionTagSnapshot(ix, 3, time.Unix(1_700_000_000, 0))
	if !snapshot.available || snapshot.truncated {
		t.Fatalf("snapshot available=%v truncated=%v; want a whole read", snapshot.available, snapshot.truncated)
	}
	for _, session := range []string{"busy", "quiet-1", "quiet-2"} {
		if len(snapshot.agent[session]) != 1 {
			t.Fatalf("snapshot.agent[%s]=%d rows; want 1 (all: %d sessions)", session, len(snapshot.agent[session]), len(snapshot.agent))
		}
	}
}
