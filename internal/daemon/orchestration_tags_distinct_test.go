package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crossing-guard/internal/orchestration"
	"crossing-guard/store"
)

// These pin the distinct read behind the tag display. Tag rows are
// append-only and an annotator re-claims its tags every turn, so a 200-row read
// filled with repeats of one key and hid the older keys from the helper's
// session.tags context, the session agents panel and (on this base) the
// stateful decision.

const (
	distinctReviewed = "agent:agent-pw4:reviewed"
	distinctOther    = "agent:agent-pw4:other"
	distinctLate     = "agent:agent-pw4:late"
)

// longTagSession admits the FK prerequisites and returns a row builder whose
// tag ids have writeClaimTags' shape: one id per run × tag × session identity.
func longTagSession(t *testing.T, ix *store.Index, now int64) func(key, session string, applied int64, n int) store.OrchestrationTag {
	t.Helper()
	binding, err := ix.PutManagedBinding(store.ManagedBinding{BindingID: "agent-pw4", State: "enabled",
		Role: "follower", ProjectRoot: "/repo", ProfileID: "annotator", ProfileSourceDigest: "sha256-v1:source",
		ProfileBundleDigest: "sha256-v1:bundle", Runtime: "codex", Authority: []string{}, AllowedProfiles: []store.ManagedProfileRef{}},
		store.ManagedBindingAbsentToken("agent-pw4"), now)
	if err != nil {
		t.Fatal(err)
	}
	group := store.ManagedGroup{GroupID: "org_pw4", BindingID: binding.BindingID, State: "active", RootTaskID: "task_root",
		RootRuntime: "codex", ProjectRoot: "/repo", CreatedAt: now, UpdatedAt: now}
	run := store.ManagedRun{RunID: "orun_pw4", IdempotencyKey: "idem_pw4", GroupID: group.GroupID, BindingID: binding.BindingID,
		BindingStateToken: binding.StateToken, Role: "follower", ProfileID: binding.ProfileID,
		ProfileSourceDigest: binding.ProfileSourceDigest, ProfileBundleDigest: binding.ProfileBundleDigest,
		SourceTaskID: "task_root", SourceEventID: 1, AdmittedAt: now, Citations: []string{}, Detail: map[string]any{}}
	if _, _, err := ix.AdmitManagedRun(group, run, store.ManagedGroupBudget{MaxTotal: 8, MaxActive: 2}); err != nil {
		t.Fatal(err)
	}
	return func(key, session string, applied int64, n int) store.OrchestrationTag {
		tag := key[len("agent:agent-pw4:"):]
		return store.OrchestrationTag{TagID: managedID("otag_", run.RunID, fmt.Sprint(n), tag, session), RunID: run.RunID,
			BindingID: binding.BindingID, AgentKey: key, Tag: tag, Runtime: "codex", SessionID: session,
			Anchor: fmt.Sprint(n), AppliedAt: applied}
	}
}

// seedLongTagSession writes one old `reviewed` row and 250 strictly newer
// `other` repeats under each named identity.
func seedLongTagSession(t *testing.T, ix *store.Index, now int64, sessions ...string) func(key, session string, applied int64, n int) store.OrchestrationTag {
	t.Helper()
	row := longTagSession(t, ix, now)
	rows := []store.OrchestrationTag{}
	for _, session := range sessions {
		rows = append(rows, row(distinctReviewed, session, now-1000, 0))
		for i := 1; i <= 250; i++ {
			rows = append(rows, row(distinctOther, session, now-500+int64(i), i))
		}
	}
	if err := ix.PutOrchestrationTags(rows); err != nil {
		t.Fatal(err)
	}
	return row
}

func TestSessionTagsContextNamesEveryKeyInALongSession(t *testing.T) {
	g := statefulGovernor(t)
	seedLongTagSession(t, g.ix, time.Now().Unix(), "long")
	reader := &managedContextReader{ix: g.ix}
	read := func(maxBytes int) (string, orchestration.ContextCoverage) {
		t.Helper()
		envelope, err := reader.ReadContext(context.Background(), orchestration.ContextRequest{SessionID: "long",
			Selections: []orchestration.ContextSelection{{Kind: "session.tags", Required: true, MaxBytes: maxBytes}}})
		if err != nil {
			t.Fatal(err)
		}
		body := ""
		for _, item := range envelope.Items {
			if item.Label == "session.tags" {
				body = item.Body
			}
		}
		for _, coverage := range envelope.Coverage {
			if coverage.Kind == "session.tags" {
				return body, coverage
			}
		}
		t.Fatalf("no session.tags coverage: %+v", envelope.Coverage)
		return "", orchestration.ContextCoverage{}
	}

	body, coverage := read(8192)
	want := "- other (" + distinctOther + ")\n- reviewed (" + distinctReviewed + ")\n"
	if body != want || coverage.State != "supplied" {
		t.Fatalf("session.tags body=%q coverage=%+v; want every key once, newest first", body, coverage)
	}
	if !strings.Contains(coverage.Detail, "2 distinct tags") {
		t.Fatalf("detail=%q", coverage.Detail)
	}
	// A limit that falls inside the second line keeps only whole lines: a cut
	// line would read as a real, shorter tag.
	first := "- other (" + distinctOther + ")\n"
	encodedFirst, _ := json.Marshal(first)
	body, coverage = read(len(encodedFirst) + 8)
	if body != first || coverage.State != "truncated" {
		t.Fatalf("mid-line limit: body=%q coverage=%+v", body, coverage)
	}
	// A limit below the first line supplies no item, and says so.
	body, coverage = read(len(encodedFirst) - 8)
	if body != "" || coverage.State != "truncated" {
		t.Fatalf("sub-line limit: body=%q coverage=%+v", body, coverage)
	}
}

func TestSessionTagsRouteOneRowPerKeyAcrossIdentities(t *testing.T) {
	g := statefulGovernor(t)
	now := time.Now().Unix()
	row := seedLongTagSession(t, g.ix, now, "cat-pw4", "native-pw4")
	// A key only the native identity carries, newer than everything: the merge
	// must still put it first. The native identity's newest `other` is one
	// second newer than the catalog's: the merge must keep it, not the first
	// row seen.
	newestOther := row(distinctOther, "native-pw4", now-500+251, 251)
	if err := g.ix.PutOrchestrationTags([]store.OrchestrationTag{row(distinctLate, "native-pw4", now, 999), newestOther}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerOrchestrationManagedRoutes(mux, &orchestrationManagedHost{ix: g.ix}, nil)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet,
		"/api/orchestration/tags?session_id=cat-pw4&session_id=native-pw4", nil))
	var got struct {
		Tags []store.OrchestrationTag `json:"tags"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatalf("status=%d body=%s err=%v", response.Code, response.Body.String(), err)
	}
	keys := []string{}
	for _, tag := range got.Tags {
		keys = append(keys, tag.AgentKey)
	}
	if strings.Join(keys, ",") != strings.Join([]string{distinctLate, distinctOther, distinctReviewed}, ",") {
		t.Fatalf("keys=%v; want each key once across identities, newest first", keys)
	}
	if got.Tags[1].TagID != newestOther.TagID || got.Tags[1].SessionID != "native-pw4" {
		t.Fatalf("other's row is not its newest claim across identities: %+v", got.Tags[1])
	}

	// Failure path: an identity with no rows is an empty list, not an error.
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/orchestration/tags?session_id=nobody", nil))
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"tags":[]}` {
		t.Fatalf("empty identity: status=%d body=%s", response.Code, response.Body.String())
	}
}

// On this base DecideStateful reads the same function, so the distinct read
// also ends the decision's false block (red-team RT-2).
func TestStatefulNegatedAgentTermSurvivesALongSession(t *testing.T) {
	rules := filepath.Join(t.TempDir(), "rules.json")
	doc := `{"rules":[{"id":"deny-unless-reviewed","action":"deny","message":"not reviewed",
	   "if":{"all":[{"tag":"command","matches":"NEG_CANARY"},{"not":{"tag":"` + distinctReviewed + `"}}]}}]}`
	if err := os.WriteFile(rules, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_RULES", rules)
	g := statefulGovernor(t)
	now := time.Now().Unix()
	row := seedLongTagSession(t, g.ix, now, "long")
	control := []store.OrchestrationTag{}
	for i := 1; i <= 250; i++ {
		control = append(control, row(distinctOther, "control", now-500+int64(i), i))
	}
	if err := g.ix.PutOrchestrationTags(control); err != nil {
		t.Fatal(err)
	}
	decide := func(session string) string {
		t.Helper()
		d, _, err := g.DecideStateful(Observation{SessionID: session, Tool: "Bash", Command: "run NEG_CANARY", TS: now})
		if err != nil {
			t.Fatal(err)
		}
		if d == nil {
			return ""
		}
		return d.Rule
	}
	if rule := decide("long"); rule != "" {
		t.Fatalf("`not: %s` fired although the session carries it (false block): %s", distinctReviewed, rule)
	}
	if rule := decide("control"); rule != "deny-unless-reviewed" {
		t.Fatalf("the negated rule is not live: control session got %q", rule)
	}
}
