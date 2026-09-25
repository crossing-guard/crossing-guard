package daemon

import (
	"testing"
	"time"

	"crossing-guard/internal/sessionactivity"
)

// The rail's open set is keyed by runtime + CATALOG id, the same key the row dot
// uses. Codex rows share a ResumeID (a subagent rollout carries its parent's
// thread), so keying by the presence COLLAPSE key would mark sibling rows open
// that the dot leaves dark. One observed session, one open row.
func TestRailOpennessDoesNotFanOutAcrossRowsSharingAResumeID(t *testing.T) {
	base := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	// Measured on the installed store 2026-09-04: 17 codex resume ids are shared
	// by more than one rail row, one of them by four — including the very session
	// the file-open lane observes.
	observed := sessionFixture("/repo", "codex", "rollout-observed", base)
	observed.ResumeID = "thread-shared"
	sibling := sessionFixture("/repo", "codex", "rollout-sibling", base.Add(-time.Minute))
	sibling.ResumeID = "thread-shared"

	rows := []SessionSummary{observed, sibling}
	presence := openFixture(observed)

	groups := buildSessionRepositories(rows, presence)
	if len(groups) != 1 || groups[0].OpenTotal != 1 {
		t.Fatalf("one observed session must produce exactly one open row: %#v", groups)
	}
	page, found := buildSessionPage(rows, presence, "/repo", "open", 0, 15, "", "")
	if !found || page.Total != 1 || len(page.Sessions) != 1 || page.Sessions[0].ID != "rollout-observed" {
		t.Fatalf("open page listed a row the presence lane never observed: %#v", page)
	}
}

// The builders decide openness ONLY from the set handed to them. Nothing about a
// session's own path, runtime, or modification time may reintroduce a second
// decider, however a future edit spells it.
func TestRailOpennessComesOnlyFromThePresenceParameter(t *testing.T) {
	base := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	held := sessionFixture("/repo", "codex", "held", base)
	notHeld := sessionFixture("/repo", "claude", "not-held", base.Add(-time.Minute))
	rows := []SessionSummary{held, notHeld}

	// The presence lane says the row nothing holds open IS open (hook evidence),
	// and says nothing about the one a descriptor would have proved.
	presence := openFixture(notHeld)
	page, found := buildSessionPage(rows, presence, "/repo", "open", 0, 15, "", "")
	if !found || page.Total != 1 || page.Sessions[0].ID != "not-held" {
		t.Fatalf("the presence set must decide, not the session's path: %#v", page)
	}

	// And the inverse: an empty set closes everything, whatever the rows look like.
	empty, found := buildSessionPage(rows, openFixture(), "/repo", "open", 0, 15, "", "")
	if !found || empty.Total != 0 {
		t.Fatalf("an empty presence set must close every row: %#v", empty)
	}
}

// Unknown is rendered as unknown. A presence set whose capability is not
// available never yields "zero open" under an available header.
func TestRailOpennessTreatsUnavailableAsUnknownNotClosed(t *testing.T) {
	row := sessionFixture("/repo", "claude", "a", time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC))
	unavailable := presenceOpenSet{
		Capability: sessionactivity.Capability{Status: "unavailable", Detail: "not reported yet"},
		Open:       map[string]bool{presenceCatalogKey("claude", "a"): true},
	}
	groups := buildSessionRepositories([]SessionSummary{row}, unavailable)
	if len(groups) != 1 || groups[0].OpenTotal != 0 {
		t.Fatalf("an unavailable reading must not claim open sessions: %#v", groups)
	}
	// The reading's own detail must reach the wire; it is what the header renders.
	page, found := buildSessionPage([]SessionSummary{row}, unavailable, "/repo", "open", 0, 15, "", "")
	if !found || page.Activity == nil || page.Activity.Status != "unavailable" ||
		page.Activity.Detail != "not reported yet" {
		t.Fatalf("the response capability lost the reading's own detail: %#v", page.Activity)
	}
}

// Before the sampler has started — the window after a daemon restart — the rail
// must say it does not know, not that nothing is open.
func TestCurrentPresenceOpenSetIsUnavailableBeforeTheSamplerStarts(t *testing.T) {
	previous := sessionActivityService()
	t.Cleanup(func() { setNativeSessionActivity(previous) })
	setNativeSessionActivity(nil)

	set := currentPresenceOpenSet(time.Now())
	if set.Capability.Status != "unavailable" || len(set.Open) != 0 {
		t.Fatalf("a missing sampler must read unavailable: %#v", set)
	}
	if set.Capability.Detail == "" {
		t.Fatal("an unavailable reading must say why")
	}
}

// The expiry gates, exercised directly. The earlier version of this test drove a
// Service that was never Started, so its snapshot capability stayed the zero
// value and the staleness branch was never reached — it passed with the branch
// deleted. Testing the pure decision removes that whole class of vacuum.
func TestPresenceOpenSetFromEnforcesBothExpiryGates(t *testing.T) {
	now := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	ttl := sessionActivityConfig().SamplerTTL()
	item := func(id string, expires time.Time) sessionactivity.Item {
		return sessionactivity.Item{Runtime: "claude", CatalogSessionID: id, Presence: "open", ExpiresAt: expires}
	}
	snapshot := func(observed time.Time, items ...sessionactivity.Item) sessionactivity.Snapshot {
		return sessionactivity.Snapshot{
			Capability: sessionactivity.Capability{Status: "available", Detail: "sampled"},
			ObservedAt: observed, Items: items,
		}
	}

	fresh := presenceOpenSetFrom(snapshot(now.Add(-ttl/2), item("a", now.Add(time.Minute))), now)
	if fresh.Capability.Status != "available" || !fresh.Open[presenceCatalogKey("claude", "a")] {
		t.Fatalf("a reading inside its window must be usable: %#v", fresh)
	}

	// Gate 1: the snapshot itself is older than the sampler's validity window.
	stale := presenceOpenSetFrom(snapshot(now.Add(-ttl-time.Second), item("a", now.Add(time.Minute))), now)
	if stale.Capability.Status != "unavailable" || len(stale.Open) != 0 {
		t.Fatalf("a snapshot past the sampler TTL must read unavailable: %#v", stale)
	}

	// Gate 2 — the load-bearing one, because Service.Replace resets the snapshot
	// clock on every event refold without re-sampling: a fresh snapshot carrying
	// an item whose own ExpiresAt has passed.
	expired := presenceOpenSetFrom(snapshot(now, item("a", now.Add(-time.Second))), now)
	if expired.Capability.Status != "available" {
		t.Fatalf("a fresh snapshot stays usable: %#v", expired)
	}
	if len(expired.Open) != 0 {
		t.Fatalf("an item past its own ExpiresAt must not count as open: %#v", expired)
	}

	// A reading that cannot say when it was taken fails CLOSED.
	undated := presenceOpenSetFrom(snapshot(time.Time{}, item("a", now.Add(time.Minute))), now)
	if undated.Capability.Status != "unavailable" || len(undated.Open) != 0 {
		t.Fatalf("a reading with no timestamp proves nothing: %#v", undated)
	}
}

// The hook lane grades an aging session "stale" while still publishing it open —
// that grade is its fade, not a different state. A session doing one long tool
// call emits no hook event, so dropping the fading tail would reintroduce the
// false negative this whole change exists to remove. Open therefore means "an
// unexpired open item", and the fade is the owner's liveness_window_seconds.
func TestRailOpennessCountsTheFadingTail(t *testing.T) {
	now := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	set := presenceOpenSetFrom(sessionactivity.Snapshot{
		Capability: sessionactivity.Capability{Status: "available"},
		ObservedAt: now,
		Items: []sessionactivity.Item{{
			Runtime: "claude", CatalogSessionID: "quiet", Presence: "open",
			Evidence: "hook_liveness", Freshness: "stale", ExpiresAt: now.Add(time.Minute),
		}},
	}, now)
	row := sessionFixture("/repo", "claude", "quiet", now)
	if !set.isOpen(row) {
		t.Fatal("a stale-graded open item must still count as open; the fade is the liveness window")
	}
}

// The rail's key and the browser store's exactKey are one contract written in two
// languages. Pin the bytes on this side so a rename here cannot silently split it
// from static/js/session/session-activity-store.js.
func TestPresenceCatalogKeyMatchesTheBrowsersExactKey(t *testing.T) {
	if got := presenceCatalogKey("claude", "abc"); got != "claude\x00abc" {
		t.Fatalf("key shape drifted from the browser's exactKey: %q", got)
	}
	// The collapse key and the catalog key coincide when there is no native id.
	// They are different contracts; only their inputs may overlap.
	if presenceIdentity("claude", "abc", "") != presenceCatalogKey("claude", "abc") {
		t.Fatal("the two key spaces stopped agreeing on their one shared input")
	}
}

// The probe deadline must fail inside the sampler pass, not consume it.
func TestActivityProbeTimeoutStaysInsideTheSamplerBudget(t *testing.T) {
	if budget := sessionActivityConfig().SamplerTimeout(); activityProbeTimeout >= budget {
		t.Fatalf("probe deadline %s must stay under the sampler budget %s", activityProbeTimeout, budget)
	}
}
