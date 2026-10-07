package daemon

import (
	"context"
	"log"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	"crossing-guard/harvest"
	"crossing-guard/internal/sessionactivity"
)

// Every duration here is policy and comes from the session-activity
// configuration owner (session_activity_config.go), with one declared exception.

// activityProbeTimeout bounds the one bounded platform probe. It is the sampler's
// inner deadline and must stay under the sampler's own configurable timeout, or a
// probe that hangs consumes the whole pass instead of failing inside it. Pinned by
// TestActivityProbeTimeoutStaysInsideTheSamplerBudget.
const activityProbeTimeout = 1500 * time.Millisecond

var nativeSessionActivity *sessionactivity.Service

// nativeSessionActivityRef mirrors nativeSessionActivity for readers on task
// and timer goroutines (the status publish, the ownership rule), which must
// not read a bare package variable that boot and shutdown write.
var nativeSessionActivityRef atomic.Pointer[sessionactivity.Service]

func setNativeSessionActivity(service *sessionactivity.Service) {
	nativeSessionActivity = service
	nativeSessionActivityRef.Store(service)
}

func sessionActivityService() *sessionactivity.Service { return nativeSessionActivityRef.Load() }

func initSessionActivityService() {
	config := sessionActivityConfig()
	setNativeSessionActivity(sessionactivity.NewService(sampleNativeSessionActivity,
		config.SamplerInterval(), config.SamplerTimeout()))
	nativeSessionActivity.Start(context.Background())
}

func closeSessionActivityService() {
	cancelPendingSessionStatus()
	if nativeSessionActivity != nil {
		nativeSessionActivity.Close()
	}
	setNativeSessionActivity(nil)
}

// presenceIdentity is the collapse key (session-presence-honesty plan, Slice
// A): one presence item per LOGICAL session — the canonical native identity
// from the runtime's identity owner, else the catalog identity. A resume handle
// is not an identity: a child may carry its parent's resume handle.
func presenceIdentity(runtime, catalogID, nativeID string) string {
	identity := nativeID
	if identity == "" {
		identity = catalogID
	}
	return runtime + "\x00" + identity
}

// presenceOpenSet is the daemon's ONE answer to "which sessions are open",
// handed to the rail so /api/sessions stops deciding it a second time from a raw
// platform probe. Open is keyed by runtime + CATALOG id — byte-identical to
// exactKey() in static/js/session/session-activity-store.js, so the rail and the
// row dot are always talking about the same session.
//
// They can still reach different VERDICTS about it, and the two places they do
// are named here rather than papered over: the rail refuses an unavailable
// capability and drops items past ExpiresAt; the browser store reads neither,
// downgrading an expired item to freshness "stale" and still drawing a dot.
//
// The key is deliberately NOT the presence collapse key presenceIdentity():
// resumed multi-rollout threads share a canonical identity, so keying by it
// would mark sibling rows Open that the dot leaves dark.
type presenceOpenSet struct {
	Capability sessionactivity.Capability
	Open       map[string]bool
}

// presenceCatalogKey matches the browser's exactKey exactly. Pinned by
// TestPresenceCatalogKeyMatchesTheBrowsersExactKey.
func presenceCatalogKey(runtime, catalogID string) string {
	return runtime + "\x00" + catalogID
}

// unknownPresence is the named closed state: no reading, so nothing is claimed
// open and the capability says why. A zero presenceOpenSet behaves the same way,
// but only by accident of its zero value; callers should say what they mean.
func unknownPresence(detail string) presenceOpenSet {
	return presenceOpenSet{
		Capability: sessionactivity.Capability{Status: "unavailable", Detail: detail},
		Open:       map[string]bool{},
	}
}

// currentPresenceOpenSet reads the presence service's published snapshot.
func currentPresenceOpenSet(now time.Time) presenceOpenSet {
	service := sessionActivityService()
	if service == nil {
		return unknownPresence("Session-activity observation has not started yet.")
	}
	return presenceOpenSetFrom(service.Snapshot(), now)
}

// presenceOpenSetFrom is the whole decision, pure so its gates are testable
// without a running sampler.
//
// An absent, unavailable, or expired reading yields an UNAVAILABLE capability,
// never an empty open set presented as "nothing is open" — absence of evidence
// is reported as absence. The window after a daemon restart, and a sampler pass
// whose probe failed, both land there.
//
// Two expiry gates, and the load-bearing one is the second. The snapshot-level
// gate only catches a service that has gone completely silent, because
// Service.Replace stamps a fresh snapshot ObservedAt on every event refold
// without re-sampling. What actually keeps a dead session out is the per-item
// ExpiresAt below, which the sampler sets from its own TTL and which
// applySessionStatus preserves across refolds.
//
// Both comparisons are WALL clock: Service stores ObservedAt through .UTC(),
// which strips the monotonic reading, so Sub falls back to wall time. A clock
// step backwards can therefore make a stale reading look fresh. That is
// acceptable for a fade and unacceptable to leave undocumented.
//
// Freshness grade is deliberately NOT consulted. The hook lane publishes
// presence "open" with grades live / recent / stale, and stale is its fade, not
// a different state — a session doing one long tool call emits no hook event and
// would otherwise vanish from the rail while still working. So Open means "the
// presence owner has an unexpired open item", and the fade is the owner's
// liveness_window_seconds. Pinned by TestRailOpennessCountsTheFadingTail.
func presenceOpenSetFrom(snapshot sessionactivity.Snapshot, now time.Time) presenceOpenSet {
	if snapshot.Capability.Status != "available" {
		detail := snapshot.Capability.Detail
		if detail == "" {
			detail = "Session-activity observation has not reported yet."
		}
		return unknownPresence(detail)
	}
	// Fail CLOSED on a missing timestamp: a reading that cannot say when it was
	// taken proves nothing about now.
	ttl := sessionActivityConfig().SamplerTTL()
	if snapshot.ObservedAt.IsZero() || (ttl > 0 && now.Sub(snapshot.ObservedAt) > ttl) {
		return unknownPresence("The last session-activity reading is older than its validity window.")
	}
	open := map[string]bool{}
	for _, item := range snapshot.Items {
		if item.Presence != "open" || item.CatalogSessionID == "" {
			continue
		}
		if !item.ExpiresAt.IsZero() && now.After(item.ExpiresAt) {
			continue
		}
		open[presenceCatalogKey(item.Runtime, item.CatalogSessionID)] = true
	}
	return presenceOpenSet{Capability: snapshot.Capability, Open: open}
}

// observeSessionActivity runs the one bounded platform probe. It bounds only the
// SAMPLER now: since the rail reads the published snapshot, no HTTP request
// spawns an lsof subprocess.
func observeSessionActivity(parent context.Context, sessions []SessionSummary) harvest.ActivityObservation {
	ctx, cancel := context.WithTimeout(parent, activityProbeTimeout)
	defer cancel()
	return harvest.ObserveOpenSessions(ctx, sessions)
}

func sampleNativeSessionActivity(ctx context.Context, now time.Time) (sessionactivity.Capability, []sessionactivity.Item) {
	sessions := ScanSessions()
	pruneSessionTurns(now)
	observation := observeSessionActivity(ctx, sessions)
	capability := sessionactivity.Capability{
		Status: observation.Capability.Status,
		Detail: observation.Capability.Detail,
	}
	var items []sessionactivity.Item
	if capability.Status == "available" {
		// One published sentence for what "open" means, so the browser renders the
		// daemon's answer instead of keeping its own copy that a fourth lane would
		// silently falsify.
		capability.Detail = "Open means a live vendor process holds the exact session file, " +
			"or the runtime's own lifecycle hooks saw the session start and act inside the " +
			"liveness window; it does not mean a model turn is generating."
		items = assemblePresenceItems(now, sessions, observation.OpenPaths, hookLiveSessionItems(now, sessions))
	}
	// Status candidate folding continues even when exact presence probing is
	// unavailable (plan invariant 18): an unavailable probe suppresses Open
	// membership but not durable status candidates from turn/action rows.
	// One owner-attention read per pass serves both the candidate source
	// (an unresolved ask keeps a quiet session on the rail) and the fold.
	attention := readOwnerAttentionBatch(now)
	items = mergeOwnerAttentionCandidates(now, sessions, items, attention)
	items = mergeStatusCandidates(now, sessions, items)
	// Every item carries the ONE decider's frame, so a sampler pass and an
	// event-driven replace can never disagree about a session.
	return capability, pruneResolvedAttentionCandidates(decorateSessionStatusWith(now, items, attention), attention)
}

// mergeStatusCandidates adds sessions that produced a status-deciding fact
// inside the quiet window but which no presence lane holds — a session that
// handed back a moment ago, or one with a recent governed action, belongs on
// the rail even if nothing holds its file. Candidates carry presence=unknown;
// they never enter presenceOpenSetFrom, whose presence == "open" gate is
// unchanged (plan D2).
//
// Turn candidates: active turn.started/input.requested inside quiet and the
// latest turn.ended across the turn-retention horizon. Action candidates:
// latest action facts inside quiet_seconds. Both are bounded, deduplicated by
// (runtime, native session id), ordered by newest deciding fact, and one
// final max_rail_sessions cap applies to status-only items.
func mergeStatusCandidates(now time.Time, sessions []SessionSummary, items []sessionactivity.Item) []sessionactivity.Item {
	if governor == nil || governor.ix == nil {
		return items
	}
	config := sessionActivityConfig()
	quiet := sessionStreamConfig().Quiet()
	seen := map[string]bool{}
	for _, item := range items {
		seen[presenceIdentity(item.Runtime, item.CatalogSessionID, item.NativeSessionID)] = true
	}

	type candidate struct {
		item     sessionactivity.Item
		deciding int64 // newest deciding fact timestamp (ms)
	}
	var candidates []candidate

	// Turn candidates: recent turn-boundary rows inside the quiet window.
	recentTurns, err := governor.ix.SessionsWithRecentTurns(now.Add(-quiet).UnixMilli(), config.MaxRailSessions)
	if err == nil {
		for _, turn := range recentTurns {
			resolved, found := identifySession(sessions, turn.Runtime, turn.SessionID)
			if !found {
				continue
			}
			nativeID := harvest.CanonicalID(resolved)
			key := presenceIdentity(resolved.Runtime, resolved.ID, nativeID)
			if seen[key] {
				continue
			}
			seen[key] = true
			candidates = append(candidates, candidate{
				item: sessionactivity.Item{
					Runtime: resolved.Runtime, CatalogSessionID: resolved.ID, NativeSessionID: nativeID,
					Presence: "unknown", Execution: "unknown", Evidence: "native_protocol", Freshness: "live",
					Authority: "observed", Controllable: false, ObservedAt: now, ExpiresAt: now.Add(quiet),
					Detail: "Recently active; whether it is still open is unknown.",
				},
				deciding: turn.ReceivedAtMS,
			})
		}
	}

	// Action candidates: sessions with a recent governed action inside quiet.
	actionCandidates, err := governor.ix.SessionActionCandidates(now.Add(-quiet).UnixMilli(), config.MaxRailSessions)
	if err == nil {
		for _, ac := range actionCandidates {
			resolved, found := identifySession(sessions, ac.Runtime, ac.SessionID)
			if !found {
				continue
			}
			nativeID := harvest.CanonicalID(resolved)
			key := presenceIdentity(resolved.Runtime, resolved.ID, nativeID)
			if seen[key] {
				continue
			}
			seen[key] = true
			candidates = append(candidates, candidate{
				item: sessionactivity.Item{
					Runtime: resolved.Runtime, CatalogSessionID: resolved.ID, NativeSessionID: nativeID,
					Presence: "unknown", Execution: "unknown", Evidence: "native_protocol", Freshness: "live",
					Authority: "observed", Controllable: false, ObservedAt: now, ExpiresAt: now.Add(quiet),
					Detail: "Recently active; whether it is still open is unknown.",
				},
				deciding: ac.NewestActionAt * 1000,
			})
		}
	}

	// Order by newest deciding fact and apply one final status-only cap.
	for i := 1; i < len(candidates); i++ {
		for j := i; j > 0 && candidates[j].deciding > candidates[j-1].deciding; j-- {
			candidates[j], candidates[j-1] = candidates[j-1], candidates[j]
		}
	}
	maxRail := config.MaxRailSessions
	if len(items)+len(candidates) > maxRail && maxRail > len(items) {
		candidates = candidates[:maxRail-len(items)]
	}
	for _, c := range candidates {
		items = append(items, c.item)
	}
	return items
}

// assemblePresenceItems is the pure presence assembly (separated for tests):
// Slice A identity collapse over the file_open lane, then the hook-liveness
// merge where file_open outranks hook_liveness per identity.
func assemblePresenceItems(now time.Time, sessions []SessionSummary, openPaths map[string]bool, hookItems []sessionactivity.Item) []sessionactivity.Item {
	type candidate struct {
		item     sessionactivity.Item
		modified time.Time
		held     int
	}
	best := map[string]*candidate{}
	order := []string{}
	for _, session := range sessions {
		if session.Path == "" || !openPaths[filepath.Clean(session.Path)] {
			continue
		}
		nativeID := harvest.CanonicalID(session)
		key := presenceIdentity(session.Runtime, session.ID, nativeID)
		item := sessionactivity.Item{
			Runtime:          session.Runtime,
			CatalogSessionID: session.ID,
			NativeSessionID:  nativeID,
			Presence:         "open",
			Execution:        "unknown",
			Evidence:         "file_open",
			Freshness:        "live",
			Authority:        "observed",
			Controllable:     false,
			ObservedAt:       now,
			ExpiresAt:        now.Add(sessionActivityConfig().SamplerTTL()),
			Detail:           "Open in a running process; whether a reply is being written is unknown.",
		}
		existing, ok := best[key]
		if !ok {
			best[key] = &candidate{item: item, modified: session.Modified, held: 1}
			order = append(order, key)
			continue
		}
		existing.held++
		if session.Modified.After(existing.modified) {
			existing.item, existing.modified = item, session.Modified
		}
	}
	// Hook-liveness lane (Slice B): sessions whose runtime publishes
	// hook-exact start evidence, whose newest lifecycle row is open, and
	// which acted inside the window. file_open outranks hook_liveness for
	// the same identity (a held file is the stronger fact), so a session
	// already collapsed above is never double-listed (red-team P4).
	for _, live := range hookItems {
		key := presenceIdentity(live.Runtime, live.CatalogSessionID, live.NativeSessionID)
		if _, ok := best[key]; ok {
			continue
		}
		item := live
		best[key] = &candidate{item: item, held: 1}
		order = append(order, key)
	}
	items := make([]sessionactivity.Item, 0, len(order))
	for _, key := range order {
		entry := best[key]
		if entry.held > 1 {
			entry.item.Detail = entry.item.Detail +
				" " + strconv.Itoa(entry.held) + " session files for this session are held open; showing the newest."
		}
		items = append(items, entry.item)
	}
	return items
}

// hookLiveSessionItems builds the hook-liveness candidates. Identity resolves
// through the harvested session records — a session the harvester cannot
// resolve is omitted, never emitted with guessed identity (red-team P2).
// Eligibility rides the vendor adapters' published activity evidence
// (harvest.ActivityEvidenceReporter, ADR 0020): only a runtime whose
// session.started evidence is hook-exact participates.
func hookLiveSessionItems(now time.Time, sessions []SessionSummary) []sessionactivity.Item {
	if governor == nil || governor.ix == nil {
		return nil
	}
	config := sessionActivityConfig()
	rows, err := governor.ix.HookLiveSessions(now.Add(-config.LivenessHorizon()).Unix(),
		now.Add(-config.LivenessWindow()).Unix(), config.MaxRailSessions)
	if err != nil {
		return nil // the file_open lane still serves; a store hiccup must not wedge the sampler
	}
	items := []sessionactivity.Item{}
	for _, row := range rows {
		if harvest.ActivityEvidence(row.Runtime)["session.started"] != "hook-exact" {
			continue
		}
		resolved, found := identifySession(sessions, row.Runtime, row.SessionID)
		if !found {
			continue
		}
		age := now.Sub(time.Unix(row.LastEventAt, 0))
		freshness := "recent"
		if age <= config.LivenessLive() {
			freshness = "live"
		} else if age > config.LivenessRecent() {
			freshness = "stale"
		}
		items = append(items, sessionactivity.Item{
			Runtime:          row.Runtime,
			CatalogSessionID: resolved.ID,
			NativeSessionID:  harvest.CanonicalID(resolved),
			Presence:         "open",
			Execution:        "unknown",
			Evidence:         "hook_liveness",
			Freshness:        freshness,
			Authority:        "observed",
			Controllable:     false,
			ObservedAt:       now,
			ExpiresAt:        now.Add(config.SamplerTTL()),
			Detail: "Open; last acted " + age.Truncate(time.Second).String() +
				" ago. Whether a reply is being written is unknown; a session that stops without saying so fades out rather than reading closed.",
		})
	}
	return items
}

// pruneSessionTurns bounds the turn table on the sampler's cadence; retention
// is policy from the configuration owner.
func pruneSessionTurns(now time.Time) {
	if governor == nil || governor.ix == nil {
		return
	}
	if _, err := governor.ix.PruneSessionTurns(now.Add(-sessionActivityConfig().TurnRetention()).UnixMilli()); err != nil {
		log.Printf("session turn retention prune failed: %v", err)
	}
}
