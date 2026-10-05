package daemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crossing-guard/engine"
	"crossing-guard/store"
)

// These tests pin the audit dry-run over agent tags: it used to
// evaluate agent:* rules over a tag set that never held an agent key, so a
// `not: agent:x` rule fired on every session and an `agent:x` rule never did.

const (
	auditReviewed    = "agent:agent-rt7:reviewed"
	auditNeedsReview = "agent:agent-rt7:needs-review"
)

// armAuditAgentRules arms the plan §6 rules. sessionKey is a session: tag the
// fixture transcript really yields, so the whole-session term is live.
func armAuditAgentRules(t *testing.T, sessionKey string) []engine.Rule {
	t.Helper()
	doc := `{"rules":[
	 {"id":"audit-neg","action":"observe","severity":"high","message":"not reviewed",
	  "if":{"all":[{"tag":"` + sessionKey + `"},{"not":{"tag":"` + auditReviewed + `"}}]}},
	 {"id":"audit-pos","action":"observe","severity":"medium","message":"flagged",
	  "if":{"tag":"` + auditNeedsReview + `"}},
	 {"id":"audit-session","action":"observe","severity":"low","message":"any tool",
	  "if":{"tag":"` + sessionKey + `"}}
	]}`
	path := filepath.Join(t.TempDir(), "rules.json")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_RULES", path)
	auditMigrationErr = nil
	t.Cleanup(func() { auditMigrationErr = nil })
	live, _, err := livePolicyRules()
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 3 {
		t.Fatalf("armed %d rules, want 3", len(live))
	}
	return live
}

// auditAgentStore opens a store as the daemon's governor and admits the run the
// tag rows reference (orchestration_tag.run_id is a foreign key).
func auditAgentStore(t *testing.T) (*store.Index, func(tagID, key, session string)) {
	t.Helper()
	g := statefulGovernor(t)
	prior := governor
	governor = g
	t.Cleanup(func() { governor = prior })
	now := time.Now().Unix()
	binding, err := g.ix.PutManagedBinding(store.ManagedBinding{BindingID: "agent-rt7", State: "enabled",
		Role: "follower", ProjectRoot: "/repo", ProfileID: "annotator", ProfileSourceDigest: "sha256-v1:source",
		ProfileBundleDigest: "sha256-v1:bundle", Runtime: "codex", Authority: []string{}, AllowedProfiles: []store.ManagedProfileRef{}},
		store.ManagedBindingAbsentToken("agent-rt7"), now)
	if err != nil {
		t.Fatal(err)
	}
	group := store.ManagedGroup{GroupID: "org_rt7", BindingID: binding.BindingID, State: "active", RootTaskID: "task_root",
		RootRuntime: "codex", ProjectRoot: "/repo", CreatedAt: now, UpdatedAt: now}
	run := store.ManagedRun{RunID: "orun_rt7", IdempotencyKey: "idem_rt7", GroupID: group.GroupID, BindingID: binding.BindingID,
		BindingStateToken: binding.StateToken, Role: "follower", ProfileID: binding.ProfileID,
		ProfileSourceDigest: binding.ProfileSourceDigest, ProfileBundleDigest: binding.ProfileBundleDigest,
		SourceTaskID: "task_root", SourceEventID: 1, AdmittedAt: now, Citations: []string{}, Detail: map[string]any{}}
	if _, _, err := g.ix.AdmitManagedRun(group, run, store.ManagedGroupBudget{MaxTotal: 8, MaxActive: 2}); err != nil {
		t.Fatal(err)
	}
	put := func(tagID, key, session string) {
		t.Helper()
		if err := g.ix.PutOrchestrationTags([]store.OrchestrationTag{{TagID: tagID, RunID: run.RunID,
			BindingID: binding.BindingID, AgentKey: key, Tag: strings.TrimPrefix(key, "agent:agent-rt7:"),
			Runtime: "codex", SessionID: session, AppliedAt: now}}); err != nil {
			t.Fatal(err)
		}
	}
	return g.ix, put
}

// auditFixtureTags is a whole-session tag set from a real detector pass, and a
// session: key it holds.
func auditFixtureTags(t *testing.T) ([]engine.Tag, string) {
	t.Helper()
	detectors, _ := engine.DefaultDetectors()
	if err := initAudit(detectors); err != nil {
		t.Fatal(err)
	}
	detail := &SessionDetail{}
	detail.Events = []harvestEvent{{Kind: "tool_call", Name: "Edit", Text: `{"file_path":"/repo/internal/daemon/thing.go"}`}}
	tags := sessionTags(detail)
	for _, tag := range tags {
		if strings.HasPrefix(tag.Key, sessionPrefix) {
			return tags, tag.Key
		}
	}
	t.Fatal("fixture transcript yields no session: tag")
	return nil, ""
}

func auditRuleIDs(findings []AuditFinding) map[string]AuditFinding {
	out := map[string]AuditFinding{}
	for _, finding := range findings {
		out[strings.TrimSuffix(finding.Rule, " (armed)")] = finding
	}
	return out
}

func TestAuditDryRunReadsTheSessionsAgentKeys(t *testing.T) {
	tags, sessionKey := auditFixtureTags(t)
	live := armAuditAgentRules(t, sessionKey)
	ix, put := auditAgentStore(t)
	// Codex stores keys under the thread uuid; the harvested row's ID is the
	// rollout stem (plan §2 session identity).
	put("otag_reviewed", auditReviewed, "thread-uuid-1")
	put("otag_needs", auditNeedsReview, "thread-uuid-1")
	put("otag_retracted", auditReviewed, "retracted-uuid")
	if err := ix.RetractOrchestrationTag("otag_retracted", time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	evaluate := func(row SessionSummary) (map[string]AuditFinding, string) {
		t.Helper()
		evalTags, state, _ := withAgentTags(live, tags, row, time.Now().Unix())
		return auditRuleIDs(livePolicyFindings(live, evalTags, auditUnknownFor(nil, state), row)), state
	}

	tagged := SessionSummary{Runtime: "codex", ID: "rollout-2026-09-30-thread-uuid-1", ThreadID: "thread-uuid-1"}
	got, state := evaluate(tagged)
	if state != auditAgentRead {
		t.Fatalf("agent_state = %q, want read", state)
	}
	if _, fired := got["audit-neg"]; fired {
		t.Error("`not: agent:x` fired on a session that carries x (the RT-7 false finding)")
	}
	pos, fired := got["audit-pos"]
	if !fired {
		t.Fatal("`agent:x` never fired on a session that carries x (the RT-7 false clean)")
	}
	if len(pos.Fired) != 1 || pos.Fired[0].Key != auditNeedsReview || pos.Fired[0].DetectorKind != provenanceModelClaimed ||
		pos.Fired[0].Value != provenanceModelClaimed {
		t.Errorf("an agent claim must show as model-claimed, never unknown: %+v", pos.Fired)
	}

	// A Codex child rollout: the hook filed its actions under the parent thread,
	// so the gate decided them over the parent's keys (plan RT-3).
	child := SessionSummary{Runtime: "codex", ID: "rollout-child", MetaID: "child-meta", ThreadID: "thread-uuid-1",
		LineageKind: "native-subagent"}
	if got, _ := evaluate(child); got["audit-neg"].Rule != "" {
		t.Error("a child rollout must read the keys the gate saw, its parent thread's")
	}

	// No keys: the negated rule fires truthfully and names what was absent.
	plain := SessionSummary{Runtime: "claude", ID: "plain-session"}
	got, _ = evaluate(plain)
	neg, fired := got["audit-neg"]
	if !fired {
		t.Fatal("a never-reviewed session must fire `not: agent:x`, as the live gate would")
	}
	if len(neg.Absent) != 1 || neg.Absent[0] != auditReviewed {
		t.Errorf("absent = %v, want [%s]", neg.Absent, auditReviewed)
	}
	if _, fired := got["audit-pos"]; fired {
		t.Error("`agent:x` fired on a session without x")
	}

	// A retracted row is not a live key.
	if got, _ := evaluate(SessionSummary{Runtime: "codex", ID: "r", ThreadID: "retracted-uuid"}); got["audit-neg"].Rule == "" {
		t.Error("a retracted key must not satisfy `not: agent:x`")
	}
}

func TestAuditSkipsAgentRulesWhenKeysAreUnreadable(t *testing.T) {
	tags, sessionKey := auditFixtureTags(t)
	live := armAuditAgentRules(t, sessionKey)
	prior := governor
	governor = nil // F-8 degraded start-up: audit serves, the store does not
	t.Cleanup(func() { governor = prior })

	row := SessionSummary{Runtime: "claude", ID: "any"}
	check := func(why string) {
		t.Helper()
		evalTags, state, err := withAgentTags(live, tags, row, time.Now().Unix())
		if state != auditAgentUnavailable || err == nil {
			t.Fatalf("%s: agent_state = %q err = %v, want unavailable with its reason", why, state, err)
		}
		got := auditRuleIDs(livePolicyFindings(live, evalTags, engine.UnknownAgent, row))
		if _, fired := got["audit-neg"]; fired {
			t.Errorf("%s: an unreadable key set must not evaluate `not: agent:x`", why)
		}
		if _, fired := got["audit-session"]; !fired {
			t.Errorf("%s: rules without agent: terms must still be evaluated", why)
		}
	}
	check("no store")

	// A store that answers with an error is the same partial set.
	governor = prior
	priorRead := auditAgentTagsRead
	auditAgentTagsRead = func(SessionSummary, int64) ([]engine.Tag, error) { return nil, errors.New("database is locked") }
	t.Cleanup(func() { auditAgentTagsRead = priorRead })
	check("read error")
}

func TestAuditReadsNoAgentKeysWithoutAnAgentRule(t *testing.T) {
	tags, sessionKey := auditFixtureTags(t)
	path := filepath.Join(t.TempDir(), "rules.json")
	if err := os.WriteFile(path, []byte(`{"rules":[{"id":"audit-session","action":"observe","severity":"low",
	  "if":{"tag":"`+sessionKey+`"}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_RULES", path)
	live, _, err := livePolicyRules()
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	priorRead := auditAgentTagsRead
	auditAgentTagsRead = func(SessionSummary, int64) ([]engine.Tag, error) { reads++; return nil, nil }
	t.Cleanup(func() { auditAgentTagsRead = priorRead })

	evalTags, state, _ := withAgentTags(live, tags, SessionSummary{Runtime: "claude", ID: "any"}, time.Now().Unix())
	if state != auditAgentNotNeeded || reads != 0 || len(evalTags) != len(tags) {
		t.Fatalf("state=%q reads=%d tags %d→%d: nothing may be read or added", state, reads, len(tags), len(evalTags))
	}
}

// TestAuditSessionRouteReadsAgentKeys drives the GET /api/audit/session handler
// over a real Claude transcript under a scratch HOME.
func TestAuditSessionRouteReadsAgentKeys(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	auditFixtureTags(t) // detectors
	path := filepath.Join(t.TempDir(), "rules.json")
	// A not-only rule is exactly the RT-7 shape: before the fix it fired everywhere.
	if err := os.WriteFile(path, []byte(`{"rules":[
	 {"id":"audit-neg","action":"observe","severity":"high","if":{"not":{"tag":"`+auditReviewed+`"}}},
	 {"id":"audit-pos","action":"observe","severity":"medium","if":{"tag":"`+auditNeedsReview+`"}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_RULES", path)
	auditMigrationErr = nil
	t.Cleanup(func() { auditMigrationErr = nil })
	_, put := auditAgentStore(t)
	put("otag_reviewed_route", auditReviewed, "route-reviewed")
	put("otag_needs_route", auditNeedsReview, "route-reviewed")
	for _, id := range []string{"route-reviewed", "route-plain"} {
		transcript := filepath.Join(home, ".claude", "projects", "-repo", id+".jsonl")
		if err := os.MkdirAll(filepath.Dir(transcript), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(transcript, []byte(`{"type":"user","uuid":"u1","sessionId":"`+id+
			`","timestamp":"2026-09-30T10:00:00Z","message":{"role":"user","content":"hello"}}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	get := func(id string) (rules map[string]AuditFinding, state string) {
		t.Helper()
		rec := httptest.NewRecorder()
		handleAuditSession(rec, httptest.NewRequest(http.MethodGet, "/api/audit/session?runtime=claude&id="+id, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", id, rec.Code, rec.Body.String())
		}
		var body struct {
			Matched    []AuditFinding `json:"matched"`
			AgentState string         `json:"agent_state"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return auditRuleIDs(body.Matched), body.AgentState
	}
	has := func(rules map[string]AuditFinding, id string) bool { _, ok := rules[id]; return ok }
	if rules, state := get("route-reviewed"); has(rules, "audit-neg") || !has(rules, "audit-pos") || state != auditAgentRead {
		t.Errorf("reviewed session: rules=%v state=%q, want audit-pos only, read", rules, state)
	}
	rules, state := get("route-plain")
	if !has(rules, "audit-neg") || has(rules, "audit-pos") || state != auditAgentRead {
		t.Errorf("plain session: rules=%v state=%q, want audit-neg only, read", rules, state)
	}
	if absent := rules["audit-neg"].Absent; len(absent) != 1 || absent[0] != auditReviewed {
		t.Errorf("plain session absent = %v, want [%s]", absent, auditReviewed)
	}
}

// TestAbsentTermsNamesOnlyWhatMatchedNothing pins what the `absent` evidence may
// claim: only terms of a satisfied `not:` that match no tag, walked in Match's
// branch order, with a value or pattern term named as such.
func TestAbsentTermsNamesOnlyWhatMatchedNothing(t *testing.T) {
	tags := []engine.Tag{{Key: "x", Value: "c"}, {Key: "a", Value: "1"}}
	term := func(tag string) engine.Predicate { return engine.Predicate{Tag: tag} }
	not := func(p engine.Predicate) engine.Predicate { return engine.Predicate{Not: &p} }
	for _, tc := range []struct {
		name string
		p    engine.Predicate
		want []string
	}{
		{"bare absent", not(term("b")), []string{"b"}},
		{"present is never absent", not(term("x")), nil},
		{"pattern term names its pattern", not(engine.Predicate{Tag: "x", Matches: "^(a|b)$"}), []string{"x~/^(a|b)$/"}},
		{"value term names its value", not(engine.Predicate{Tag: "x", Value: "d"}), []string{"x=d"}},
		{"not over all lists only the missing part", not(engine.Predicate{All: []engine.Predicate{term("a"), term("b")}}), []string{"b"}},
		{"nested in all", engine.Predicate{All: []engine.Predicate{term("a"), not(term("b"))}}, []string{"b"}},
		{"double negation is not absence", not(not(term("b"))), nil},
	} {
		got := absentTerms(tc.p, tags, nil)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s: absent = %v, want %v", tc.name, got, tc.want)
		}
	}
}
