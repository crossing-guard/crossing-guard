package daemon

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"crossing-guard/internal/teamlink"
	"crossing-guard/store"
	"crossing-guard/teamwire"
)

// Handoff transport, the device's side (team rest-of-release plan §6.6). One job on the
// link's lifecycle — started and stopped with the outbox drain, never on the hook path
// (invariant 5) — does two things on the link's pull cadence:
//
//   - the handoff pull: its OWN request (kinds ["handoff"]) and its OWN cursor
//     (sync_cursor scope "handoff:<organization id>"), at most handoff.pull_pages pages
//     a tick, each page landed in one store transaction. An older server answers that
//     request 400; the pull is parked and retried every parked_kind_retry, and the
//     memory pull — a different request — is untouched;
//   - the member directory refresh, every members_refresh: user id and display name
//     only (OD-10), replaced whole so a removed member is absent at the next refresh.
//
// Everything this file logs or chains about a handoff is ids, codes and counts —
// never a title or text (criterion 65).

// handoffSyncState is the job's in-memory state for one linker: when a parked pull is
// next tried, when the directory was last refreshed, and the last tick's outcome for
// the console. In memory on purpose: a restart re-probes once, cheaply.
type handoffSyncState struct {
	mu sync.Mutex
	// tick serializes ticks: a page is landed before the next is asked for.
	tick sync.Mutex
	// parkedUntil is set while the server does not carry the handoff pull.
	parkedUntil    time.Time
	parked         bool
	membersAt      time.Time
	membersGen     uint64
	lastAt         time.Time
	outcome        string
	problem        string
	membersProblem string
	unlandable     int
}

// handoffSyncStates holds each linker's state. The linker's own struct belongs to the
// link owner; this job's state is kept beside it, keyed by the linker.
var handoffSyncStates sync.Map

func (t *teamLinker) handoffSync() *handoffSyncState {
	state, _ := handoffSyncStates.LoadOrStore(t, &handoffSyncState{})
	return state.(*handoffSyncState)
}

// Handoff sync outcomes, as the console reads them.
const (
	handoffSyncPulled = "pulled"
	handoffSyncParked = "parked"
	handoffSyncError  = "error"
)

// runHandoffSync is the job. Its first tick waits the link's start delay (the same one
// the memory pull waits), then it runs at pull_interval.
func (t *teamLinker) runHandoffSync(stop chan struct{}) {
	interval, first, running := t.reclaimHandoffsAtStart(stop)
	if !running {
		return
	}
	timer := time.NewTimer(first)
	defer timer.Stop()
	for {
		select {
		case <-stop:
			return
		case <-timer.C:
		}
		t.refreshMembersIfDue()
		t.pullHandoffsOnce()
		timer.Reset(interval)
	}
}

// reclaimHandoffsAtStart is the job's first act: a relink to the same organization takes
// its handoff rows back (§6.7). It reads the link and reclaims under the linker's lock,
// and only while the job's stop is still open. unlink closes that stop and marks the
// rows as an ended link's under the same lock, so the two cannot interleave: a job
// that is scheduled only after its link was ended finds stop closed and reclaims
// nothing. Without that, the job could read the organization, lose the processor to an
// unlink, and then take back the rows the unlink had just ended — the device would
// offer Decline and Close on handoffs of a link it no longer has.
func (t *teamLinker) reclaimHandoffsAtStart(stop chan struct{}) (interval, first time.Duration, running bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	select {
	case <-stop:
		return 0, 0, false
	default:
	}
	if governor != nil {
		if err := governor.ix.ReclaimHandoffs(t.doc.Organization.ID); err != nil {
			log.Printf("team handoff: rows of an earlier link to this organization not reclaimed: %v", err)
		}
	}
	return t.doc.PullInterval.Duration, t.doc.MemoryPullStartDelay.Duration, true
}

// handoffTick is what one tick needs from the linker, read under its lock.
type handoffTick struct {
	client  *teamlink.Client
	gen     uint64
	orgID   string
	pages   int
	retry   time.Duration
	refresh time.Duration
	now     time.Time
}

// handoffTickFacts reads the tick's facts; ok is false when the device is not linked.
func (t *teamLinker) handoffTickFacts() (handoffTick, bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state != teamLinked || t.key == nil {
		return handoffTick{}, false, nil
	}
	client, err := teamlink.NewClient(t.doc.Server, t.deviceID, t.key, t.doc.RequestTimeout.Duration, t.now)
	return handoffTick{client: client, gen: t.gen, orgID: t.doc.Organization.ID, pages: t.doc.Handoff.PullPages,
		retry: t.doc.ParkedKindRetry.Duration, refresh: t.doc.MembersRefresh.Duration, now: t.now()}, true, err
}

// markRevoked pauses sync when the server says this device's key was revoked — the
// same transition the drain and the memory pull make.
func (t *teamLinker) markRevoked(gen uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.gen == gen && t.state == teamLinked {
		t.state, t.problem = teamRevoked, "the server revoked this device's key; sync is paused and nothing local is deleted — unlink and link again to re-enroll"
	}
}

// pullHandoffsOnce pulls and lands up to handoff.pull_pages pages. The link generation
// is re-checked before every store write, and the store checks the linked organization
// inside its transaction, so a tick in flight across an unlink lands nothing.
func (t *teamLinker) pullHandoffsOnce() {
	state := t.handoffSync()
	state.tick.Lock()
	defer state.tick.Unlock()
	tick, linked, err := t.handoffTickFacts()
	if !linked {
		return
	}
	if err != nil {
		state.record(tick.now, handoffSyncError, err.Error())
		return
	}
	state.mu.Lock()
	waiting := state.parked && tick.now.Before(state.parkedUntil)
	state.mu.Unlock()
	if waiting {
		return
	}
	scope := store.HandoffCursorScope(tick.orgID)
	cursor, err := governor.ix.SyncCursor(scope)
	if err != nil {
		state.record(tick.now, handoffSyncError, "reading the cursor: "+err.Error())
		return
	}
	bootstrapped, err := governor.ix.HandoffBootstrapped(tick.orgID)
	if err != nil {
		state.record(tick.now, handoffSyncError, "reading the cursor: "+err.Error())
		return
	}
	for page := 0; page < tick.pages; page++ {
		// Every page of the link's first handoff pull says so, until one page reports
		// no more: the server then knows this device has caught up.
		resp, err := tick.client.PullHandoffs(cursor, !bootstrapped)
		if err != nil {
			t.handoffPullFailed(state, tick, err)
			return
		}
		if !t.sameLink(tick.gen) {
			return
		}
		eff, err := governor.ix.LandHandoffPage(store.HandoffPage{OrganizationID: tick.orgID, Rows: resp.Rows, From: cursor, To: resp.Cursor, Drained: !resp.More, At: tick.now.Unix()})
		if errors.Is(err, store.ErrLinkChanged) {
			return
		}
		if err != nil {
			state.record(tick.now, handoffSyncError, "landing: "+err.Error())
			log.Printf("team handoff: pull: a page was not landed (it is pulled again): %v", err)
			return
		}
		t.handoffLanded(state, eff)
		if !eff.Moved || resp.Cursor <= cursor || !resp.More {
			break
		}
		cursor = resp.Cursor
	}
	state.mu.Lock()
	state.parked = false
	state.mu.Unlock()
	state.record(tick.now, handoffSyncPulled, "")
}

// handoffPullFailed sorts a failed pull: revoked, an older server (parked), or an error
// retried on the next tick.
func (t *teamLinker) handoffPullFailed(state *handoffSyncState, tick handoffTick, err error) {
	if teamlink.Code(err) == teamwire.CodeDeviceRevoked {
		t.markRevoked(tick.gen)
		state.record(tick.now, handoffSyncError, err.Error())
		return
	}
	var refused *teamlink.ErrServer
	if errors.As(err, &refused) && refused.Status == http.StatusBadRequest {
		state.mu.Lock()
		first := !state.parked
		state.parked, state.parkedUntil = true, tick.now.Add(tick.retry)
		state.mu.Unlock()
		if first {
			log.Printf("team handoff: the server does not carry handoffs yet (the handoff pull was answered 400); parked, retried every %s", tick.retry)
		}
		state.record(tick.now, handoffSyncParked, "this team server does not carry handoffs")
		return
	}
	state.record(tick.now, handoffSyncError, err.Error())
	log.Printf("team handoff: pull: %v", err)
}

// handoffLanded counts a page's effects and logs the ones a person should hear about.
func (t *teamLinker) handoffLanded(state *handoffSyncState, eff store.HandoffLandEffects) {
	state.mu.Lock()
	state.unlandable += eff.Unlandable
	state.mu.Unlock()
	if eff.HashConflicts > 0 {
		log.Printf("team handoff: %d pulled row(s) carried a wire hash that differs from the one held for the same id; refused, the held rows are unchanged", eff.HashConflicts)
		t.linkEvent("team.handoff.hash_conflict", fmt.Sprintf("%d pulled handoff row(s) refused: wire hash differs from the held one", eff.HashConflicts))
	}
	if eff.Unlandable > 0 {
		log.Printf("team handoff: %d pulled row(s) could not be read and were skipped", eff.Unlandable)
	}
	if eff.Receipts > 0 {
		t.pushSoon()
	}
}

func (s *handoffSyncState) record(at time.Time, outcome, problem string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastAt, s.outcome, s.problem = at, outcome, problem
}

// refreshMembersIfDue re-reads the member directory when members_refresh has passed, or
// when the link changed since the last refresh.
func (t *teamLinker) refreshMembersIfDue() {
	state := t.handoffSync()
	tick, linked, err := t.handoffTickFacts()
	if !linked || err != nil {
		return
	}
	state.mu.Lock()
	due := state.membersAt.IsZero() || state.membersGen != tick.gen || !tick.now.Before(state.membersAt.Add(tick.refresh))
	state.mu.Unlock()
	if due {
		t.refreshMembers(state, tick)
	}
}

// refreshMembers replaces the directory with the server's list. A failure keeps the
// cache as it is and is retried on the next tick.
func (t *teamLinker) refreshMembers(state *handoffSyncState, tick handoffTick) {
	resp, err := tick.client.Members()
	if err != nil {
		if teamlink.Code(err) == teamwire.CodeDeviceRevoked {
			t.markRevoked(tick.gen)
		}
		state.mu.Lock()
		state.membersProblem = err.Error()
		state.mu.Unlock()
		return
	}
	if !t.sameLink(tick.gen) {
		return
	}
	if err := governor.ix.ReplaceTeamMembers(tick.orgID, resp.Members, resp.Self, tick.now.Unix()); err != nil {
		if !errors.Is(err, store.ErrLinkChanged) {
			log.Printf("team handoff: the member directory was not written: %v", err)
		}
		return
	}
	state.mu.Lock()
	state.membersAt, state.membersGen, state.membersProblem = tick.now, tick.gen, ""
	state.mu.Unlock()
}

// refreshMembersNow refreshes the directory outside the cadence — a person opened the
// send sheet. It reports whether the device is linked.
func (t *teamLinker) refreshMembersNow() bool {
	tick, linked, err := t.handoffTickFacts()
	if !linked || err != nil {
		return linked
	}
	t.refreshMembers(t.handoffSync(), tick)
	return true
}

// settleHandoffs applies each handoff or receipt answer in its own store transaction —
// the acknowledgement and the state together — and chains what changed, by id and code.
func (t *teamLinker) settleHandoffs(settles []store.HandoffSettle, now time.Time) {
	for _, s := range settles {
		out, err := governor.ix.SettleHandoffPush(s, now.Unix())
		if err != nil {
			log.Printf("team push: handoff answer for row %d not settled (it re-pushes; the server dedupes): %v", s.Seq, err)
			continue
		}
		if out.HandoffID == "" {
			continue // already acknowledged: nothing changed
		}
		accepted := out.Code == teamwire.StatusAccepted || out.Code == teamwire.StatusDuplicate
		switch {
		case out.Kind == store.OutboxHandoff && !accepted:
			log.Printf("team push: handoff %s refused: %s", out.HandoffID, out.Code)
			t.linkEvent("team.handoff.refused", "handoff "+out.HandoffID+" refused: "+out.Code)
		case out.Kind == store.OutboxHandoffReceipt && !accepted:
			log.Printf("team push: %s receipt for handoff %s rejected: %s", out.Transition, out.HandoffID, out.Code)
			t.linkEvent("team.handoff.receipt_rejected", out.Transition+" receipt for handoff "+out.HandoffID+" rejected: "+out.Code)
		}
	}
}

// handoffTransport is the transport's state as GET /api/team/handoffs reports it.
// Facts only; the console words them.
type handoffTransport struct {
	// ServerCarriesHandoffs is false while the server answered a handoff kind
	// unsupported_kind or the handoff pull 400: "this team server does not carry
	// handoffs". Queued items stay queued and leave when it does.
	ServerCarriesHandoffs bool   `json:"server_carries_handoffs"`
	PullOutcome           string `json:"pull_outcome,omitempty"`
	PullProblem           string `json:"pull_problem,omitempty"`
	PullAt                string `json:"pull_at,omitempty"`
	MembersRefreshedAt    string `json:"members_refreshed_at,omitempty"`
	MembersProblem        string `json:"members_problem,omitempty"`
	// HashConflicts counts pulled rows refused because their wire hash differed from
	// the one held for the same id.
	HashConflicts int `json:"hash_conflicts"`
	// Unlandable counts pulled rows this build could not read, since the daemon started.
	Unlandable int `json:"unlandable"`
}

// handoffTransportStatus reads the transport's state for the console.
func (t *teamLinker) handoffTransportStatus(orgID string) handoffTransport {
	t.mu.Lock()
	_, documentParked := t.parked[teamwire.KindHandoff]
	_, receiptParked := t.parked[teamwire.KindHandoffReceipt]
	t.mu.Unlock()
	state := t.handoffSync()
	state.mu.Lock()
	out := handoffTransport{ServerCarriesHandoffs: !documentParked && !receiptParked && !state.parked,
		PullOutcome: state.outcome, PullProblem: state.problem, MembersProblem: state.membersProblem, Unlandable: state.unlandable}
	if !state.lastAt.IsZero() {
		out.PullAt = state.lastAt.UTC().Format(time.RFC3339)
	}
	if !state.membersAt.IsZero() {
		out.MembersRefreshedAt = state.membersAt.UTC().Format(time.RFC3339)
	}
	state.mu.Unlock()
	if conflicts, err := governor.ix.HandoffHashConflicts(orgID); err == nil {
		out.HashConflicts = conflicts
	}
	return out
}
