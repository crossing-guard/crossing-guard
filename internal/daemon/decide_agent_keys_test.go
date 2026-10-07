package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"crossing-guard/store"
)

// TestStatefulAgentTermsSeeEveryKeyInALongSession pins the complete key read
// for a stateful decision. Tag rows are append-only, so a long
// session's newer repeats of one key used to push older keys past the
// 200-row display read the decision shared. A `not: agent:x` term then turned
// true on a missing key (false block) and an `agent:x` term turned false
// (false allow). The decision now reads every live key.
func TestStatefulAgentTermsSeeEveryKeyInALongSession(t *testing.T) {
	const (
		reviewed    = "agent:agent-rt5:reviewed"
		needsReview = "agent:agent-rt5:needs-review"
		other       = "agent:agent-rt5:other"
	)
	rules := filepath.Join(t.TempDir(), "rules.json")
	doc := `{"rules":[
	  {"id":"deny-unless-reviewed","action":"deny","message":"not reviewed",
	   "if":{"all":[{"tag":"command","matches":"NEG_CANARY"},{"not":{"tag":"` + reviewed + `"}}]}},
	  {"id":"deny-when-flagged","action":"deny","message":"flagged",
	   "if":{"all":[{"tag":"command","matches":"POS_CANARY"},{"tag":"` + needsReview + `"}]}}
	]}`
	if err := os.WriteFile(rules, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_RULES", rules)
	g := statefulGovernor(t)

	now := time.Now().Unix()
	binding, err := g.ix.PutManagedBinding(store.ManagedBinding{BindingID: "agent-rt5", State: "enabled",
		Role: "follower", ProjectRoot: "/repo", ProfileID: "annotator", ProfileSourceDigest: "sha256-v1:source",
		ProfileBundleDigest: "sha256-v1:bundle", Runtime: "codex", Authority: []string{}, AllowedProfiles: []store.ManagedProfileRef{}},
		store.ManagedBindingAbsentToken("agent-rt5"), now)
	if err != nil {
		t.Fatal(err)
	}
	group := store.ManagedGroup{GroupID: "org_rt5", BindingID: binding.BindingID, State: "active", RootTaskID: "task_root",
		RootRuntime: "codex", ProjectRoot: "/repo", CreatedAt: now, UpdatedAt: now}
	run := store.ManagedRun{RunID: "orun_rt5", IdempotencyKey: "idem_rt5", GroupID: group.GroupID, BindingID: binding.BindingID,
		BindingStateToken: binding.StateToken, Role: "follower", ProfileID: binding.ProfileID,
		ProfileSourceDigest: binding.ProfileSourceDigest, ProfileBundleDigest: binding.ProfileBundleDigest,
		SourceTaskID: "task_root", SourceEventID: 1, AdmittedAt: now, Citations: []string{}, Detail: map[string]any{}}
	if _, _, err := g.ix.AdmitManagedRun(group, run, store.ManagedGroupBudget{MaxTotal: 8, MaxActive: 2}); err != nil {
		t.Fatal(err)
	}
	row := func(id, key, session string, applied int64) store.OrchestrationTag {
		return store.OrchestrationTag{TagID: id, RunID: run.RunID, BindingID: binding.BindingID, AgentKey: key,
			Tag: key[len("agent:agent-rt5:"):], SessionID: session, AppliedAt: applied}
	}
	// Session "long": the two old keys, then 250 strictly newer repeats.
	// Session "control": only the repeats, so the negated rule must fire there.
	rows := []store.OrchestrationTag{
		row("tag_reviewed", reviewed, "long", now-1000),
		row("tag_needs_review", needsReview, "long", now-1000),
	}
	for i := 0; i < 250; i++ {
		rows = append(rows, row(fmt.Sprintf("tag_long_%03d", i), other, "long", now-500+int64(i)))
		rows = append(rows, row(fmt.Sprintf("tag_control_%03d", i), other, "control", now-500+int64(i)))
	}
	if err := g.ix.PutOrchestrationTags(rows); err != nil {
		t.Fatal(err)
	}

	decide := func(session, command string) string {
		t.Helper()
		d, _, err := g.DecideStateful(Observation{SessionID: session, Tool: "Bash", Command: command, TS: now})
		if err != nil {
			t.Fatal(err)
		}
		if d == nil {
			return ""
		}
		return d.Rule
	}
	if rule := decide("long", "run NEG_CANARY"); rule != "" {
		t.Fatalf("`not: %s` fired although the session carries it (false block): %s", reviewed, rule)
	}
	if rule := decide("long", "run POS_CANARY"); rule != "deny-when-flagged" {
		t.Fatalf("`%s` did not fire although the session carries it (false allow): %q", needsReview, rule)
	}
	if rule := decide("control", "run NEG_CANARY"); rule != "deny-unless-reviewed" {
		t.Fatalf("the negated rule is not live: control session got %q", rule)
	}
}
