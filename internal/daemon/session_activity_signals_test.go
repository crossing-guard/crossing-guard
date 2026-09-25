package daemon

// Focused tests for the natural-session signal emitter (natural-session plan
// Slice B): consent default-off, hook-exact translation, the double-fire
// guard including the console-resume window, the session.active bootstrap,
// per-runtime end-fact honesty, and durable position resumption.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"crossing-guard/internal/sessionactivity"
	"crossing-guard/store"
)

// naturalFixture drives emitNaturalSessionSignals directly against the
// durable activity rows — no scheduler loop needed.
func naturalFixture(t *testing.T, driver ChatDriver) agentHostFixture {
	t.Helper()
	fixture := newAgentHostFixture(t, driver)
	// The sessions row that scope resolution reads cwd from.
	if err := govTx(t, fixture.host.ix, func(tx *store.GovTx) error {
		return tx.EnsureSessionRoot("codex", "ses-natural", "", fixture.root)
	}); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func govTx(t *testing.T, ix *store.Index, fn func(*store.GovTx) error) error {
	t.Helper()
	tx, err := ix.BeginGov()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func appendNaturalActivity(t *testing.T, fixture agentHostFixture, entryKind string, receivedAt int64) {
	t.Helper()
	state, evidenceClass, validUntil := "open", "positive-open", receivedAt+60
	if entryKind == "end" {
		state, evidenceClass, validUntil = "stopped", "explicit-end", 0
	}
	err := govTx(t, fixture.host.ix, func(tx *store.GovTx) error {
		_, err := tx.AppendSessionActivity(store.SessionActivityObservation{
			ObservationID: fmt.Sprintf("obs_nat_%d", receivedAt), Runtime: "codex", SessionID: "ses-natural",
			State: state, ObservedAt: receivedAt, ValidUntil: validUntil, EvidenceClass: evidenceClass,
			EntryKind:      entryKind,
			EvidenceDigest: "sha256-v1:test", CollectorID: "daemon-observation-v1", ReceivedAt: receivedAt,
			DeliveryAttempts: 1, DeliveryMode: "direct"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func bindNaturalFollower(t *testing.T, fixture agentHostFixture, bindingID string, watch bool) store.ManagedBinding {
	t.Helper()
	preview := selectManagedProfile(t, fixture.owner, naturalFollowerProfileSource())
	binding, err := fixture.host.putBinding(managedBindingCommand{BindingID: bindingID,
		ProfileID: preview.ProfileID, ProfileSourceDigest: preview.SourceDigest,
		ProfileBundleDigest: preview.BundleDigest, ProjectRoot: fixture.root,
		Runtime: "managed-fixture", GrantedAuthority: []string{}, WatchNatural: watch,
		ExpectedStateToken: store.ManagedBindingAbsentToken(bindingID)})
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

// naturalFollowerProfileSource selects ALL natural lifecycle signals so
// terminal and non-terminal selection is exercised.
func naturalFollowerProfileSource() []byte {
	return []byte(`---
format-version: 1
kind: crossing-guard-orchestration-profile
id: natural-follower
version: "1.0.0"
name: Natural follower
description: Watches natural sessions.
role: follower
type: follower
may-tag:
  - plan
stages:
  session.started: Record that the session began.
  session.ended: Review the ended session.
  session.active: Record that the session is active.
execution: managed-turn
trigger:
  event: session.started
context:
  - kind: session.tags
output:
  kind: advice
authority-requests: []
requirements:
  capabilities:
    - managed-turn
limits:
  timeout: 2m
  max-hops: 1
  max-depth: 1
failure:
  missing-required-context: record-unavailable
  unavailable-capability: record-unavailable
  timeout: record-unavailable
  malformed-output: record-unavailable
---
Observe the natural session lifecycle and record grounded facts.
`)
}

// TestWatchNaturalOffMeansZeroNaturalFires pins the consent default (plan
// acceptance 2): a binding that never opted in stays silent for every
// natural-session signal, while the same activity row fires an opted-in
// binding exactly once — and the durable position makes re-emission a no-op.
func TestWatchNaturalOffMeansZeroNaturalFires(t *testing.T) {
	driver := managedDynamicFixtureDriver{commandFor: func(ChatRequest) string {
		return jsonTextCommand(`{"action":"no_action","message":"Natural session noted.","citations":[]}`)
	}}
	fixture := naturalFixture(t, driver)
	bindNaturalFollower(t, fixture, "agent-silent", false)
	bindNaturalFollower(t, fixture, "agent-watching", true)
	appendNaturalActivity(t, fixture, "first-action", 100)
	fixture.host.emitNaturalSignalsOnce()
	runs, err := fixture.host.ix.ManagedRuns(50)
	if err != nil {
		t.Fatal(err)
	}
	watching := 0
	for _, run := range runs {
		if run.BindingID == "agent-watching" {
			watching++
		}
		if run.BindingID == "agent-silent" {
			t.Fatalf("consent-off binding fired on natural activity: %+v", run)
		}
	}
	if watching != 1 {
		t.Fatalf("opted-in binding run count=%d, want exactly 1", watching)
	}
	// Durable position: re-running the emitter must not re-fire.
	fixture.host.emitNaturalSignalsOnce()
	runs, _ = fixture.host.ix.ManagedRuns(50)
	watching = 0
	for _, run := range runs {
		if run.BindingID == "agent-watching" {
			watching++
		}
	}
	if watching != 1 {
		t.Fatalf("re-emission re-fired the same lifecycle row: %d runs", watching)
	}
}

// TestNaturalSignalsFlowThroughSelectorMap pins acceptance 3's mechanics: the
// started fact routes by selector map and the run records the catalog kind.
func TestNaturalSignalsFlowThroughSelectorMap(t *testing.T) {
	driver := managedDynamicFixtureDriver{commandFor: func(ChatRequest) string {
		return jsonTextCommand(`{"action":"no_action","message":"Session begun.","citations":[]}`)
	}}
	fixture := naturalFixture(t, driver)
	bindNaturalFollower(t, fixture, "agent-flow", true)
	appendNaturalActivity(t, fixture, "first-action", 110)
	fixture.host.emitNaturalSignalsOnce()
	runs := waitForRuns(t, fixture.host, func(runs []store.ManagedRun) bool {
		for _, run := range runs {
			if run.BindingID == "agent-flow" && run.State == "completed" {
				return true
			}
		}
		return false
	})
	found := false
	for _, run := range runs {
		if run.BindingID == "agent-flow" && run.Detail["signal"] == "session.started" {
			found = true
		}
	}
	if !found {
		t.Fatalf("session.started never routed: %+v", runs)
	}
}

// TestNaturalEndFactIsHookExactOnly pins B3: only "end" entry kinds fire
// session.ended — presence-style rows never do.
func TestNaturalEndFactIsHookExactOnly(t *testing.T) {
	driver := managedDynamicFixtureDriver{commandFor: func(ChatRequest) string {
		return jsonTextCommand(`{"action":"no_action","message":"Ended.","citations":[]}`)
	}}
	fixture := naturalFixture(t, driver)
	bindNaturalFollower(t, fixture, "agent-end", true)
	appendNaturalActivity(t, fixture, "first-action", 200)
	fixture.host.emitNaturalSignalsOnce()
	runs, _ := fixture.host.ix.ManagedRuns(50)
	for _, run := range runs {
		if run.BindingID == "agent-end" && run.Detail["signal"] == "session.ended" {
			t.Fatalf("presence row fired session.ended: %+v", run.Detail)
		}
	}
	// The hook-exact end row does fire.
	appendNaturalActivity(t, fixture, "end", 210)
	fixture.host.emitNaturalSignalsOnce()
	ended := false
	runs, _ = fixture.host.ix.ManagedRuns(50)
	for _, run := range runs {
		if run.BindingID == "agent-end" && run.Detail["signal"] == "session.ended" {
			ended = true
		}
	}
	if !ended {
		t.Fatal("hook-exact end row never fired session.ended")
	}
}

// TestTaskOwnedSessionNeverFiresNaturally pins the double-fire guard
// (acceptance 10): once the console task stream owns the session identity —
// including through a console resume — natural emission is suppressed.
func TestTaskOwnedSessionNeverFiresNaturally(t *testing.T) {
	driver := managedDynamicFixtureDriver{commandFor: func(ChatRequest) string {
		return jsonTextCommand(`{"action":"no_action","message":"Owned.","citations":[]}`)
	}}
	fixture := newAgentHostFixture(t, driver)
	// The console task resumes the SAME vendor session (runtime codex, native
	// id ses-natural) — the M10 resume window. The fixture registry gains a
	// codex driver so task admission accepts the same runtime the natural
	// activity row carries.
	original := chatDrivers
	chatDrivers["codex"] = driver
	t.Cleanup(func() { chatDrivers = original })
	if err := govTx(t, fixture.host.ix, func(tx *store.GovTx) error {
		return tx.EnsureSessionRoot("codex", "ses-natural", "", fixture.root)
	}); err != nil {
		t.Fatal(err)
	}
	bindNaturalFollower(t, fixture, "agent-guarded", true)
	if _, _, err := fixture.tasks.Create(ChatRequest{Runtime: "codex", Prompt: "hello",
		SessionID: "ses-natural", Cwd: fixture.root}, "idem-owned"); err != nil {
		t.Fatal(err)
	}
	appendNaturalActivity(t, fixture, "first-action", 300)
	fixture.host.emitNaturalSignalsOnce()
	runs, _ := fixture.host.ix.ManagedRuns(50)
	for _, run := range runs {
		if run.BindingID == "agent-guarded" {
			t.Fatalf("task-owned session fired a natural run: %+v", run)
		}
	}
}

// TestOpenCodeEmitsNoNaturalEndFact pins B3's honest absence: a runtime with
// no closure lane (opencode) produces no natural session.ended fire even
// though start-class rows flow.
func TestOpenCodeEmitsNoNaturalEndFact(t *testing.T) {
	driver := managedDynamicFixtureDriver{commandFor: func(ChatRequest) string {
		return jsonTextCommand(`{"action":"no_action","message":"Noted.","citations":[]}`)
	}}
	fixture := naturalFixture(t, driver)
	bindNaturalFollower(t, fixture, "agent-oc", true)
	if err := govTx(t, fixture.host.ix, func(tx *store.GovTx) error {
		return tx.EnsureSessionRoot("opencode", "ses_oc", "", fixture.root)
	}); err != nil {
		t.Fatal(err)
	}
	err := govTx(t, fixture.host.ix, func(tx *store.GovTx) error {
		_, err := tx.AppendSessionActivity(store.SessionActivityObservation{
			ObservationID: "obs_oc_start", Runtime: "opencode", SessionID: "ses_oc",
			State: "open", ObservedAt: 10, ValidUntil: 70, EvidenceClass: "positive-open", EntryKind: "first-action",
			EvidenceDigest: "sha256-v1:test", CollectorID: "daemon-observation-v1", ReceivedAt: 10,
			DeliveryAttempts: 1, DeliveryMode: "direct"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.host.emitNaturalSignalsOnce()
	runs, _ := fixture.host.ix.ManagedRuns(50)
	signals := map[string]int{}
	for _, run := range runs {
		if run.BindingID == "agent-oc" {
			if signal, ok := run.Detail["signal"].(string); ok {
				signals[signal]++
			}
		}
	}
	if signals["session.ended"] != 0 {
		t.Fatalf("opencode emitted an end fact: %+v", signals)
	}
	if signals["session.started"] == 0 {
		t.Fatalf("opencode start fact should flow: %+v", signals)
	}
}

// TestServedCatalogPublishesPerRuntimeEndEvidence pins M9: session.ended
// lists claude/codex hook-exact evidence and never opencode.
func TestServedCatalogPublishesPerRuntimeEndEvidence(t *testing.T) {
	served := servedSignalCatalog(true)
	var ended, active *servedSignal
	for i := range served {
		switch served[i].Kind {
		case "session.ended":
			ended = &served[i]
		case "session.active":
			active = &served[i]
		}
	}
	if ended == nil {
		t.Fatal("catalog lost session.ended")
	}
	seen := map[string]string{}
	for _, by := range ended.ServedBy {
		seen[by.Runtime] = by.Evidence
	}
	if seen["claude"] != "hook-exact" || seen["codex"] != "hook-exact" {
		t.Fatalf("end-fact evidence classes wrong: %+v", seen)
	}
	if _, ok := seen["opencode"]; ok {
		t.Fatalf("opencode must not claim end-fact serving: %+v", seen)
	}
	if active == nil {
		t.Fatal("catalog lost session.active")
	}
	if !active.Served {
		t.Fatal("session-activity signals must read served with the emitter live")
	}
}

// TestWatchNaturalPersistsThroughPutBinding pins the field's storage round trip.
func TestWatchNaturalPersistsThroughPutBinding(t *testing.T) {
	driver := managedDynamicFixtureDriver{commandFor: func(ChatRequest) string {
		return jsonTextCommand(`{"action":"no_action","message":"x","citations":[]}`)
	}}
	fixture := naturalFixture(t, driver)
	binding := bindNaturalFollower(t, fixture, "agent-persist", true)
	stored, found, err := fixture.host.ix.ManagedBinding("agent-persist")
	if err != nil || !found {
		t.Fatalf("stored=%v err=%v", found, err)
	}
	if !stored.WatchNatural {
		t.Fatal("watch_natural did not persist")
	}
	if stored.StateToken != binding.StateToken {
		t.Fatal("state token mismatch on read-back")
	}
}

// TestSessionActiveBootstrapFiresOnAttach pins H4: saving a watch_natural
// binding fires session.active for every matching open session exactly once,
// keyed to the binding's state token.
func TestSessionActiveBootstrapFiresOnAttach(t *testing.T) {
	driver := managedDynamicFixtureDriver{commandFor: func(ChatRequest) string {
		return jsonTextCommand(`{"action":"no_action","message":"Active.","citations":[]}`)
	}}
	fixture := naturalFixture(t, driver)
	// The stub's open session needs its sessions row so the exact-root scope
	// check can resolve a cwd (the same lookup the emitter runs in
	// production).
	if err := govTx(t, fixture.host.ix, func(tx *store.GovTx) error {
		return tx.EnsureSessionRoot("codex", "ses-bootstrap", "", fixture.root)
	}); err != nil {
		t.Fatal(err)
	}
	saved := nativeSessionActivity
	stub := newBootstrapActivityStub()
	stub.Start(context.Background())
	t.Cleanup(func() { stub.Close(); setNativeSessionActivity(saved) })
	setNativeSessionActivity(stub)

	binding := bindNaturalFollower(t, fixture, "agent-attach", true)
	runs, _ := fixture.host.ix.ManagedRuns(50)
	fired := 0
	for _, run := range runs {
		if run.BindingID == binding.BindingID && run.Detail["signal"] == "session.active" {
			fired++
		}
	}
	if fired != 1 {
		t.Fatalf("session.active fired %d times, want 1", fired)
	}
}

// newBootstrapActivityStub supplies one open codex session to the presence
// projection for bootstrap tests. Only Snapshot is consumed by
// emitSessionActiveBootstrap.
func newBootstrapActivityStub() *sessionactivity.Service {
	sampler := func(context.Context, time.Time) (sessionactivity.Capability, []sessionactivity.Item) {
		return sessionactivity.Capability{Status: "available"},
			[]sessionactivity.Item{{
				Runtime: "codex", CatalogSessionID: "ses-bootstrap", Presence: "open",
				Evidence: "file_open", Freshness: "live", Authority: "observed",
			}}
	}
	return sessionactivity.NewService(sampler, time.Hour, time.Second)
}
