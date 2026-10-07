package store

import (
	"path/filepath"
	"strings"
	"testing"

	"crossing-guard/engine"
)

func producerTags(sp engine.StateProducers) map[string]string {
	out := map[string]string{}
	for _, p := range sp.Producers {
		out[p.Tag] = p.Source
	}
	return out
}

// Without a store only the compiled catalog is known; model claims stay unknown so an
// agent: term reads unverified, never a guessed INERT.
func TestStateProducersWithoutStoreKnowOnlyTheCatalog(t *testing.T) {
	sp := StateProducersFor(nil, 0)
	if !sp.SessionFactsKnown || sp.AgentClaimsKnown || sp.AgentClaimsNote == "" {
		t.Fatalf("knowledge flags: %+v", sp)
	}
	if len(sp.Producers) != 1 {
		t.Fatalf("producers: %+v", sp.Producers)
	}
	p := sp.Producers[0]
	if p.Tag != "session:work" || strings.Join(p.Values, ",") != "uncommitted" || p.Source != "daemon:checkpoint-settle" ||
		!p.Live || p.Harvest || !p.Enumerable || len(p.Gaps) != 3 {
		t.Fatalf("catalog producer: %+v", p)
	}
}

// Model-claim producers are every binding that can still complete a claim run, plus
// every active claim row: an enabled binding, a disabled one with a run in flight, and
// the rows a disabled binding left behind. A disabled binding with nothing in flight
// and no active rows produces nothing.
func TestStateProducersDeriveModelClaimProducers(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	put := func(id string, tags []string) ManagedBinding {
		b := testManagedBinding()
		b.BindingID, b.DeclaredTags = id, tags
		saved, err := ix.PutManagedBinding(b, ManagedBindingAbsentToken(id), 1)
		if err != nil {
			t.Fatal(err)
		}
		return saved
	}
	admit := func(b ManagedBinding, runID string) ManagedRun {
		group := ManagedGroup{GroupID: "org_" + runID, BindingID: b.BindingID, State: "active", RootTaskID: "task_root",
			RootRuntime: "codex", ProjectRoot: "/repo", CreatedAt: 2, UpdatedAt: 2}
		run := ManagedRun{RunID: runID, IdempotencyKey: "idem_" + runID, GroupID: group.GroupID, BindingID: b.BindingID,
			BindingStateToken: b.StateToken, Role: "follower", ProfileID: b.ProfileID, ProfileSourceDigest: b.ProfileSourceDigest,
			ProfileBundleDigest: b.ProfileBundleDigest, SourceTaskID: "task_root", SourceEventID: 1, AdmittedAt: 2,
			Citations: []string{}, Detail: map[string]any{}}
		stored, _, err := ix.AdmitManagedRun(group, run, ManagedGroupBudget{MaxTotal: 8, MaxActive: 2})
		if err != nil {
			t.Fatal(err)
		}
		return stored
	}
	disable := func(b ManagedBinding) {
		current, _, err := ix.ManagedBinding(b.BindingID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ix.DisableManagedBinding(b.BindingID, current.StateToken, 3); err != nil {
			t.Fatal(err)
		}
	}

	put("enabled", []string{"reviewed"})
	idle := put("idle", []string{"plan"})
	disable(idle)
	inFlight := put("in-flight", []string{"red-teamed"})
	admit(inFlight, "run_flight")
	disable(inFlight)
	left := put("left-behind", []string{"undeclared-now"})
	run := admit(left, "run_left")
	if err := ix.PutOrchestrationTags([]OrchestrationTag{{TagID: "t1", RunID: run.RunID, BindingID: left.BindingID,
		AgentKey: "agent:left-behind:legacy", Tag: "legacy", SessionID: "s1", AppliedAt: 2}}); err != nil {
		t.Fatal(err)
	}
	if err := ix.CompleteManagedRun(run.RunID, "completed", "", "", nil, map[string]any{}, "", "", 3); err != nil {
		t.Fatal(err)
	}
	disable(left)
	// A disabled binding whose only run is parked still completes claims.
	parked := put("parked", []string{"parked-tag"})
	parkedRun := admit(parked, "run_parked")
	if _, err := ix.db.Exec(`UPDATE orchestration_managed_run SET state='parked' WHERE run_id=?`, parkedRun.RunID); err != nil {
		t.Fatal(err)
	}
	disable(parked)
	// Retracted and expired rows are not read by the stateful tier, so they produce nothing.
	gone := put("gone", []string{"x"})
	goneRun := admit(gone, "run_gone")
	if err := ix.PutOrchestrationTags([]OrchestrationTag{
		{TagID: "t_retracted", RunID: goneRun.RunID, BindingID: gone.BindingID, AgentKey: "agent:gone:retracted", Tag: "retracted", SessionID: "s1", AppliedAt: 2},
		{TagID: "t_expired", RunID: goneRun.RunID, BindingID: gone.BindingID, AgentKey: "agent:gone:expired", Tag: "expired", SessionID: "s1", AppliedAt: 2, ExpiresAt: 9},
	}); err != nil {
		t.Fatal(err)
	}
	if err := ix.RetractOrchestrationTag("t_retracted", 3); err != nil {
		t.Fatal(err)
	}
	if err := ix.CompleteManagedRun(goneRun.RunID, "completed", "", "", nil, map[string]any{}, "", "", 3); err != nil {
		t.Fatal(err)
	}
	disable(gone)

	sp := StateProducersFor(ix, 10)
	if !sp.AgentClaimsKnown || !sp.SessionFactsKnown || !strings.Contains(sp.AgentClaimsNote, "6 binding(s)") {
		t.Fatalf("knowledge flags: %+v", sp)
	}
	got := producerTags(sp)
	want := map[string]string{
		"session:work":               "daemon:checkpoint-settle",
		"agent:enabled:reviewed":     "binding:enabled",
		"agent:in-flight:red-teamed": "binding:in-flight",
		"agent:left-behind:legacy":   "claims:left-behind",
		"agent:parked:parked-tag":    "binding:parked",
	}
	if len(got) != len(want) {
		t.Fatalf("producers %v, want %v", got, want)
	}
	for tag, source := range want {
		if got[tag] != source {
			t.Errorf("%s: source %q, want %q (all: %v)", tag, got[tag], source, got)
		}
	}
	for _, p := range sp.Producers {
		if strings.HasPrefix(p.Tag, engine.AgentStatePrefix) &&
			// Harvest too: the audit reads a session's agent: keys into its dry run.
			(p.Enumerable || !p.Live || !p.Harvest || strings.Join(p.Values, ",") != OrchestrationTagProvenance) {
			t.Errorf("claim producer shape: %+v", p)
		}
	}

	// The label a user sees agrees: a declared claim can fire, an undeclared one is INERT.
	for tag, canFire := range map[string]bool{"agent:enabled:reviewed": true, "agent:idle:plan": false} {
		b := engine.Boundary(engine.Rule{ID: "r", Action: "ask", If: engine.Predicate{Tag: tag}}, nil, sp, engine.ReachStop)
		if b.CanFire != canFire || b.Unverified {
			t.Errorf("%s: %+v", tag, b)
		}
	}

	// All or nothing: a read failure leaves claims unknown, never a partial set.
	ix.Close()
	failed := StateProducersFor(ix, 10)
	if failed.AgentClaimsKnown || !strings.Contains(failed.AgentClaimsNote, "could not be read") || len(failed.Producers) != 1 {
		t.Fatalf("failed read: %+v", failed)
	}
}

// Only a declared fact reaches session_state without a detector: the zero value is
// refused, and a declared one lands with its detector as the evidence source.
func TestUpsertSessionStateDirectTakesOnlyDeclaredFacts(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	if err := ix.UpsertSessionStateDirect("s1", DirectSessionFact{}, "x", 1); err == nil {
		t.Fatal("an undeclared fact must be refused")
	}
	forged := FactUncommittedWork
	forged.value = "forged" // only this package can do this; the catalog check still refuses it
	if err := ix.UpsertSessionStateDirect("s1", forged, "x", 1); err == nil {
		t.Fatal("a fact outside the catalog must be refused")
	}
	if err := ix.UpsertSessionStateDirect("s1", FactUncommittedWork, "change_record=7", 5); err != nil {
		t.Fatal(err)
	}
	rows, err := ix.SessionState("s1")
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	r := rows[0]
	if r.Key != "work" || r.Value != "uncommitted" || r.Detector != "checkpoint-settle" || r.Evidence != "change_record=7" {
		t.Fatalf("row: %+v", r)
	}
	if head, err := ix.UncommittedWorkFacetHead(); err != nil || head != 5 {
		t.Fatalf("facet head=%d err=%v", head, err)
	}
}
