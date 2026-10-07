package daemon

// The device's link to the team server (team plan §5.15, build order 2c). The daemon is
// the ONE owner: it generates the key, runs the device-authorization enrollment, writes
// team.json and the key beside the store, pushes the device report on its own cadence,
// and notices when the files change behind its back. The CLI and the console only ask it
// over the loopback routes in team_http.go.
//
// Hostile-server linking is DETECTED, not pretended away (D-16): a link cannot be swapped
// while one exists, every link-state transition is a chained local event under the
// daemon's own session, a drift in the files raises a persistent flag and stops
// reporting, and the server the device sends to is always visible.

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"crossing-guard/engine"
	"crossing-guard/internal/guardcli"
	"crossing-guard/internal/orchestration/profilefs"
	"crossing-guard/internal/rulebook"
	"crossing-guard/internal/teamlink"
	"crossing-guard/store"
	"crossing-guard/teamwire"
)

// teamSessionID is the session the daemon's own link-state transitions are chained under.
// It is a session like any other in the store: visible, chain-verifiable, never hidden.
const teamSessionID = "daemon/team-link"

// Link states as the routes report them.
const (
	teamUnlinked     = "unlinked"
	teamPending      = "pending"
	teamLinked       = "linked"
	teamInconsistent = "inconsistent" // team.json, the key, and the store's linked flag disagree
	teamDrift        = "drift"        // a link file changed behind the daemon's back
	teamRevoked      = "revoked"      // the server refused the key; sync paused, nothing deleted
)

type pendingEnrollment struct {
	UserCode        string    `json:"user_code"`
	VerificationURL string    `json:"verification_url"`
	Fingerprint     string    `json:"fingerprint"`
	ExpiresAt       time.Time `json:"expires_at"`
	Server          string    `json:"server"`
	Name            string    `json:"name"`
	cancel          chan struct{}
}

// teamReportState is the persisted report outcome beside the store: when the last
// report went and how, the exact document it sent, and the link files' digests as the
// daemon wrote them — the baseline every drift check compares against. The type, its
// file, and its lock live in teamlink (one contract, one owner); Remove deletes it
// with the link.
type teamReportState = teamlink.ReportState

type teamLinker struct {
	mu       sync.Mutex
	storeDir string
	doc      teamlink.Document
	found    bool
	key      ed25519.PrivateKey
	deviceID string
	pending  *pendingEnrollment
	report   teamReportState
	state    string
	problem  string // why the state is inconsistent/drift/revoked, for people
	stop     chan struct{}
	pullStop chan struct{} // the layers pull job's stop (3c: keyed on the same lifecycle)
	pushStop chan struct{} // the outbox drain's stop (item 4: the same lifecycle)
	// memPullStop is the memory pull job's stop (item 5: the same lifecycle); memPullMu
	// keeps one pull tick at a time.
	memPullStop chan struct{}
	memPullMu   sync.Mutex
	// upgradeInputs is what the last identity-upgrade pass ran over (weak labels and
	// checkout roots); the pass runs again only when it changes.
	upgradeInputs string
	pushMu        sync.Mutex // serializes drain ticks (never held with mu across a push)
	// parked is the drain's parked kinds → when each is next probed (item 4 decision
	// 1, N2). In memory: a restart re-probes each once, cheaply.
	parked map[string]time.Time
	// fitBytes is the batch size the server last accepted after a 413 (0 = the
	// configured push_max_bytes); in memory, re-learned after a restart.
	fitBytes int64
	gen      uint64         // the link generation: bumped by completeLink and unlink
	jobs     sync.WaitGroup // the enrollment poll, report, layers pull and outbox drain goroutines
	halted   bool           // stopJobs ran: no job starts again
	now      func() time.Time

	// available is the last verified catalog (the console's offer list); pinnedKey
	// is the org key this device pinned (3c part 2). layerReasons is the loader's
	// latest notes. All owned by the pull job.
	available    []teamLayerEntry
	pinnedKey    orgKeyPin
	layerReasons []string
	// unusable lists verified bundles this device cannot use; keyMismatches is the
	// `org key mismatch` state, one entry per presented key. Both are the last pull's.
	unusable      []teamUnusableBundle
	keyMismatches []teamKeyMismatch
	// adoptMu serializes adoption acts — a pull that refreshes, Adopt, Un-adopt and
	// re-pin — so no two interleave. It is taken before mu, never while holding it.
	adoptMu sync.Mutex
	// profiles is the profile owner for this data directory, made on first use.
	profiles *profilefs.Owner
	// places turns an adoption's places off; nil means the store's own writers.
	places adoptionPlaces
}

var team *teamLinker

// startTeamLink loads the link beside the store and, when linked and consistent, starts
// the report job. Called from initGovernor: no governor, no link.
func startTeamLink(storeDir string) {
	t := &teamLinker{storeDir: storeDir, now: time.Now}
	t.reload()
	team = t
	if t.state == teamLinked {
		t.startReporting()
	}
	log.Printf("team link: %s%s", t.state, t.describe())
}

func (t *teamLinker) describe() string {
	switch t.state {
	case teamLinked:
		return " to " + t.doc.Server + " as " + t.doc.Device.Name
	case teamInconsistent, teamDrift, teamRevoked:
		return ": " + t.problem
	}
	return ""
}

// reload reads the files and the store and derives the state. Three facts must agree —
// team.json, the key, and sync_device.linked — or nothing is sent.
func (t *teamLinker) reload() {
	t.mu.Lock()
	defer t.mu.Unlock()
	doc, found, err := teamlink.Load(t.storeDir)
	if err != nil {
		t.state, t.problem = teamInconsistent, err.Error()
		return
	}
	key, keyFound, err := teamlink.LoadKey(t.storeDir)
	if err != nil {
		t.state, t.problem = teamInconsistent, err.Error()
		return
	}
	t.doc, t.found, t.key = doc, found, key
	id, linked, err := governor.ix.Device()
	if err != nil {
		t.state, t.problem = teamInconsistent, "store: "+err.Error()
		return
	}
	t.deviceID = id
	if rs, err := teamlink.LoadReportState(t.storeDir); err == nil {
		t.report = rs
	}
	// The pinned org key reloads from disk (postwork M-5): a restart must not
	// re-pin and chain the first-contact event again — the disk record is the
	// pin's home, memory is its cache.
	t.pinnedKey = clearPinWithoutLink(t.storeDir, doc.Linked(), loadPinnedOrgKey(t.storeDir))
	switch {
	case !doc.Linked() && !keyFound && !linked:
		t.state, t.problem = teamUnlinked, ""
	case doc.Linked() && keyFound && linked:
		switch t.driftCheck() {
		case driftChanged:
			t.state, t.problem = teamDrift, "team.json or the device key changed outside the daemon; reporting stopped — unlink and link again"
			return
		case driftNoBaseline:
			// A missing baseline is a broken record of the daemon's own writes,
			// not an accusation against anyone: inconsistent, with the real cause.
			t.state, t.problem = teamInconsistent, "the daemon's record of what it wrote is missing — unlink and link again"
			return
		}
		if teamwire.KeyFingerprint(key.Public().(ed25519.PublicKey)) != doc.Device.Fingerprint {
			t.state, t.problem = teamInconsistent, "the device key does not match the fingerprint team.json records"
			return
		}
		// A device linked before the store recorded its organization gets the record now.
		if org, err := governor.ix.LinkedOrganization(); err == nil && org != doc.Organization.ID {
			if err := governor.ix.SetLinkedOrganization(doc.Organization.ID, t.now().Unix()); err != nil {
				t.state, t.problem = teamInconsistent, "store: "+err.Error()
				return
			}
		}
		t.state, t.problem = teamLinked, ""
	default:
		t.state, t.problem = teamInconsistent, fmt.Sprintf("team.json linked=%v, key present=%v, store linked=%v — unlink to reset", doc.Linked(), keyFound, linked)
	}
}

// The drift fact has three outcomes, not two.
const (
	driftOK         = iota // the link files match the recorded baseline
	driftChanged           // they changed outside the daemon — drift
	driftNoBaseline        // the baseline itself is missing — inconsistent, not drift
)

// driftCheck is the ONE drift predicate, shared by reload and the report tick, so the
// two can never disagree about one fact. It compares the link files on disk against the
// digests the daemon recorded when it last wrote them. A missing baseline is NOT drift
// ("changed outside the daemon" would be a false accusation): the daemon cannot tell
// its writes from a stranger's, and that is a broken link. The caller holds t.mu.
func (t *teamLinker) driftCheck() int {
	if t.report.Digests.Document == "" {
		return driftNoBaseline
	}
	if now := teamlink.DigestsIn(t.storeDir); now != t.report.Digests {
		return driftChanged
	}
	return driftOK
}

// --- link -----------------------------------------------------------------------------

// platformString is the one wire fact for the device's platform, sent identically by
// the enrollment and recorded in team.json.
func platformString() string { return runtime.GOOS + "/" + runtime.GOARCH }

// beginLink starts an enrollment. It refuses while linked or pending: a good link cannot
// be silently swapped for another (D-16).
func (t *teamLinker) beginLink(server, name string) (*pendingEnrollment, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.halted {
		return nil, errors.New("the team link is stopped")
	}
	switch t.state {
	case teamLinked, teamDrift, teamRevoked:
		return nil, fmt.Errorf("already linked to %s; unlink first", t.doc.Server)
	case teamInconsistent:
		return nil, errors.New("link state is inconsistent (" + t.problem + "); unlink to reset")
	}
	if t.pending != nil {
		return nil, fmt.Errorf("an enrollment is already pending (code %s); wait for it or unlink", t.pending.UserCode)
	}
	key, err := teamlink.NewKey()
	if err != nil {
		return nil, err
	}
	client, err := teamlink.NewClient(server, t.deviceID, key, t.doc.RequestTimeout.Duration, t.now)
	if err != nil {
		return nil, &teamlink.ErrServerURL{Err: err}
	}
	name = strings.TrimSpace(name)
	if name == "" {
		// An empty hostname is a fact the server's own validation will name; the
		// device's name is not something to invent here.
		name, _ = os.Hostname()
	}
	start, err := client.StartEnrollment(name, platformString(), guardcli.BuildVersion())
	if err != nil {
		return nil, &teamlink.ErrEnrollmentStart{Err: err}
	}
	// An unparseable expires_at falls back to the local enrollment timeout: the
	// server's clock is authoritative when it speaks RFC 3339, and this is the
	// bounded wait when it does not.
	expires, _ := time.Parse(time.RFC3339, start.ExpiresAt)
	if expires.IsZero() {
		expires = t.now().Add(t.doc.EnrollmentTimeout.Duration)
	}
	p := &pendingEnrollment{UserCode: start.UserCode, VerificationURL: start.VerificationURL, Fingerprint: client.Fingerprint(),
		ExpiresAt: expires, Server: client.Origin(), Name: name, cancel: make(chan struct{})}
	t.pending, t.key, t.state = p, key, teamPending
	interval := t.doc.PollInterval.Duration
	if start.PollIntervalSeconds > 0 {
		interval = time.Duration(start.PollIntervalSeconds) * time.Second
	}
	t.linkEvent("team.link.started", "enrollment started with "+client.Origin()+" as "+name+", key "+client.Fingerprint())
	t.jobs.Go(func() { t.pollEnrollment(client, p, interval) })
	return p, nil
}

func (t *teamLinker) pollEnrollment(client *teamlink.Client, p *pendingEnrollment, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-p.cancel:
			return
		case <-ticker.C:
		}
		if t.now().After(p.ExpiresAt) {
			t.endPending(p, "the enrollment code expired before it was approved")
			return
		}
		res, err := client.PollEnrollment()
		if err != nil {
			if teamlink.Code(err) == teamwire.CodeNoPendingEnrollment {
				t.endPending(p, "the server no longer has this enrollment")
				return
			}
			continue // a transient failure: keep polling until the code expires
		}
		switch res.Status {
		case teamwire.EnrollPending:
			continue
		case teamwire.EnrollApproved:
			org := teamlink.Organization{}
			if res.Organization != nil {
				org = *res.Organization
			}
			approvedBy := ""
			if res.Device != nil {
				approvedBy = res.Device.ApprovedBy
			}
			t.completeLink(client, p, org, approvedBy)
			return
		default:
			t.endPending(p, "the enrollment was "+res.Status)
			return
		}
	}
}

func (t *teamLinker) endPending(p *pendingEnrollment, why string) {
	t.mu.Lock()
	if t.pending == p {
		t.pending, t.key, t.state, t.problem = nil, nil, teamUnlinked, why
		t.mu.Unlock()
		// Only a pending enrollment that still exists gets a refused event; an
		// unlinked one is gone, and an event after team.unlink would chain a
		// transition for something that no longer exists.
		t.linkEvent("team.link.refused", why)
		return
	}
	t.mu.Unlock()
}

// completeLink writes the key, then team.json, then flips the store — in that order, so a
// crash between steps leaves a state reload calls inconsistent rather than half-linked.
func (t *teamLinker) completeLink(client *teamlink.Client, p *pendingEnrollment, org teamlink.Organization, approvedBy string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pending != p {
		return
	}
	doc := t.doc
	doc.Server, doc.Organization, doc.ApprovedBy = client.Origin(), org, approvedBy
	doc.Device = teamlink.DeviceFacts{Name: p.Name, Platform: platformString(), Fingerprint: p.Fingerprint}
	doc.LinkedAt = t.now().UTC().Format(time.RFC3339)
	if err := teamlink.SaveKey(t.storeDir, t.key); err != nil {
		t.state, t.problem = teamInconsistent, "could not write the device key: "+err.Error()
		return
	}
	if err := teamlink.Save(t.storeDir, doc); err != nil {
		t.state, t.problem = teamInconsistent, "could not write team.json: "+err.Error()
		return
	}
	if err := governor.ix.SetLinked(true); err != nil {
		t.state, t.problem = teamInconsistent, "could not mark the store linked: "+err.Error()
		return
	}
	// The store knows which organization its sync state belongs to (item 5): a pulled
	// page is landed, and an acknowledgement counted, only for this link.
	if err := governor.ix.SetLinkedOrganization(org.ID, t.now().Unix()); err != nil {
		t.state, t.problem = teamInconsistent, "could not record the linked organization: "+err.Error()
		return
	}
	t.doc, t.found, t.pending, t.state, t.problem = doc, true, nil, teamLinked, ""
	t.gen++
	t.report = teamReportState{Digests: teamlink.DigestsIn(t.storeDir)}
	if err := teamlink.SaveReportState(t.storeDir, t.report); err != nil {
		// The drift baseline IS the link's integrity record: without it the next
		// tick cannot tell the daemon's writes from a stranger's. A failed write is
		// a broken link, not a deferred one.
		t.state, t.problem = teamInconsistent, "could not record the drift baseline: "+err.Error()
	}
	t.linkEvent("team.link", "linked to "+doc.Server+" ("+org.Name+") as "+p.Name+", key "+p.Fingerprint+", approved by "+approvedBy)
	t.startReportingLocked()
}

// unlink revokes the key on the server (best effort), then removes the files and flips
// the store. The server deletes nothing; a device that cannot reach it unlinks anyway and
// says the server still lists it.
func (t *teamLinker) unlink() (serverAcknowledged bool, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pending != nil {
		close(t.pending.cancel)
		t.pending, t.key = nil, nil
	}
	if t.stop != nil {
		close(t.stop)
		t.stop = nil
	}
	if t.pullStop != nil {
		close(t.pullStop)
		t.pullStop = nil
	}
	if t.pushStop != nil {
		close(t.pushStop)
		t.pushStop = nil
	}
	if t.memPullStop != nil {
		close(t.memPullStop)
		t.memPullStop = nil
	}
	t.parked, t.fitBytes, t.available = nil, 0, nil
	t.unusable, t.keyMismatches, t.layerReasons = nil, nil, nil
	if t.doc.Linked() && t.key != nil {
		if client, cerr := teamlink.NewClient(t.doc.Server, t.deviceID, t.key, t.doc.RequestTimeout.Duration, t.now); cerr == nil {
			serverAcknowledged = client.RevokeSelf() == nil
		}
	}
	server := t.doc.Server
	if err := teamlink.Remove(t.storeDir); err != nil {
		return serverAcknowledged, err
	}
	if err := governor.ix.SetLinked(false); err != nil {
		return serverAcknowledged, err
	}
	// What was queued for THIS link, and every consent given under it, ends with it:
	// a later link — perhaps to another organization — must not receive this one's
	// backlog or inherit its opt-ins (postwork C2).
	if err := governor.ix.ResetSyncQueue(); err != nil {
		return serverAcknowledged, err
	}
	// The organization-key pin belongs to the link and goes with it; a later link pins
	// anew. What this device adopted stays, keeps the key id it was verified under,
	// and runs to its expiry: with no pull there is no refresh (plan §4.1 decision 5,
	// §4.3).
	if err := clearPinnedOrgKey(t.storeDir); err != nil {
		return serverAcknowledged, err
	}
	t.pinnedKey = orgKeyPin{}
	def, _ := teamlink.Default()
	t.gen++
	t.doc, t.found, t.key, t.report, t.state, t.problem = def, false, nil, teamReportState{}, teamUnlinked, ""
	detail := "unlinked from " + server
	if !serverAcknowledged && server != "" {
		detail += " (the server could not be told; it still lists this device until an admin revokes it)"
	}
	t.linkEvent("team.unlink", detail)
	return serverAcknowledged, nil
}

// stopJobs ends the link's background goroutines and waits for them to return,
// leaving the files and the store as they are. No job starts afterwards. The
// caller must not hold t.mu.
func (t *teamLinker) stopJobs() {
	t.mu.Lock()
	t.halted = true
	if t.pending != nil {
		close(t.pending.cancel)
		t.pending = nil
	}
	if t.stop != nil {
		close(t.stop)
		t.stop = nil
	}
	if t.pullStop != nil {
		close(t.pullStop)
		t.pullStop = nil
	}
	if t.memPullStop != nil {
		close(t.memPullStop)
		t.memPullStop = nil
	}
	if t.pushStop != nil {
		close(t.pushStop)
		t.pushStop = nil
	}
	t.mu.Unlock()
	t.jobs.Wait()
}

// linkEvent chains a link-state transition as a live event under the daemon's own
// session, so it verifies like any other and reaches the server with everything else.
func (t *teamLinker) linkEvent(kind, detail string) {
	if governor == nil {
		return
	}
	// Observe takes the governor's write lock itself.
	if err := governor.Observe(Observation{SessionID: teamSessionID, Tool: kind, Command: detail, TS: t.now().Unix(),
		Decision: "allow", Reason: detail, Origin: "live"}); err != nil {
		log.Printf("team link: %s not recorded: %v", kind, err)
	}
}

// --- report -----------------------------------------------------------------------------

func (t *teamLinker) startReporting() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.startReportingLocked()
}

func (t *teamLinker) startReportingLocked() {
	if t.stop != nil || t.halted {
		return
	}
	stop := make(chan struct{})
	t.stop = stop
	t.jobs.Go(func() { t.runReporting(stop) })
	t.startLayersPullLocked() // the catalog pull rides the same lifecycle (3c)
	t.startPushLocked()       // and so does the outbox drain (item 4)
	t.startMemoryPullLocked() // and the memory pull (item 5)
}

// runReporting is the report job: its own goroutine and ticker, the cadence from
// team.json, the due time seeded from the persisted last report. It never runs on the
// lifecycle scheduler's goroutine and never inside a tool call.
func (t *teamLinker) runReporting(stop chan struct{}) {
	t.mu.Lock()
	interval := t.doc.ReportInterval.Duration
	due := t.report.LastAt.Add(interval)
	t.mu.Unlock()
	first := time.Until(due)
	if first < 0 {
		first = 0
	}
	timer := time.NewTimer(first)
	defer timer.Stop()
	for {
		select {
		case <-stop:
			return
		case <-timer.C:
		}
		t.reportOnce()
		timer.Reset(interval)
	}
}

// reportOnce checks the files for drift, builds the report from the store and the
// runtimes' configurations, pushes it, and persists the outcome with the exact document.
// The push runs without t.mu; the link generation captured under the lock discards any
// result that lands after an unlink or a newer link — a dead link's outcome must not
// become a live one's report state.
func (t *teamLinker) reportOnce() {
	t.mu.Lock()
	if t.state != teamLinked {
		t.mu.Unlock()
		return
	}
	if t.driftCheck() == driftChanged {
		t.state, t.problem = teamDrift, "team.json or the device key changed outside the daemon; reporting stopped — unlink and link again"
		t.mu.Unlock()
		t.linkEvent("team.drift", t.problem)
		return
	}
	gen := t.gen
	client, err := teamlink.NewClient(t.doc.Server, t.deviceID, t.key, t.doc.RequestTimeout.Duration, t.now)
	t.mu.Unlock()
	if err != nil {
		t.recordOutcome(gen, "error", err.Error(), "", nil)
		return
	}
	facts, err := t.gatherFacts()
	if err != nil {
		t.recordOutcome(gen, "error", "gathering facts: "+err.Error(), "", nil)
		return
	}
	report := teamlink.BuildReport(facts)
	rec, err := teamlink.Record(engine.NewTypedID("rep"), report)
	if err != nil {
		t.recordOutcome(gen, "error", err.Error(), "", nil)
		return
	}
	res, err := client.Push([]teamwire.PushRecord{rec})
	if err != nil {
		if teamlink.Code(err) == teamwire.CodeDeviceRevoked {
			t.mu.Lock()
			if t.gen == gen && t.state == teamLinked {
				t.state, t.problem = teamRevoked, "the server revoked this device's key; sync is paused and nothing local is deleted — unlink and link again to re-enroll"
			}
			t.mu.Unlock()
			t.recordOutcome(gen, "revoked", err.Error(), rec.ID, rec.Body)
			t.linkEvent("team.revoked", "the server refused a report with device_revoked")
			return
		}
		t.recordOutcome(gen, "error", err.Error(), rec.ID, rec.Body)
		return
	}
	outcome, detail := "error", "no result for the record"
	for _, r := range res.Results {
		if r.ID == rec.ID {
			outcome, detail = r.Status, ""
			if r.Error != nil {
				detail = r.Error.Code + ": " + r.Error.Message
			}
		}
	}
	t.recordOutcome(gen, outcome, detail, rec.ID, rec.Body)
}

// recordOutcome persists the report outcome for the link generation gen. An outcome from
// a superseded generation is dropped: its files are gone (unlink) or belong to a newer
// link, and writing it would overwrite the live record with a dead link's state.
func (t *teamLinker) recordOutcome(gen uint64, outcome, detail, recordID string, document []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.gen != gen {
		return
	}
	t.report.LastAt, t.report.Outcome, t.report.Error, t.report.RecordID = t.now(), outcome, detail, recordID
	if document != nil {
		t.report.Document = json.RawMessage(document)
	}
	if err := teamlink.SaveReportState(t.storeDir, t.report); err != nil {
		log.Printf("team link: report state not written: %v", err)
	}
	if outcome == "error" || outcome == "rejected" || outcome == "conflict" {
		log.Printf("team link: report %s: %s", outcome, detail)
	}
}

// gatherFacts reads the three rungs and the rulebook. Attach comes from the runtimes'
// configuration files; firing and the canary from stored live events, never from memory.
func (t *teamLinker) gatherFacts() (teamlink.ReportFacts, error) {
	observed, err := runtimeObservations()
	if err != nil {
		return teamlink.ReportFacts{}, err
	}
	// The device id is the linker's state and is read under its lock like the rest:
	// a reload writes it, and this runs on the reporting job with the lock released.
	t.mu.Lock()
	deviceID := t.deviceID
	t.mu.Unlock()
	facts := teamlink.ReportFacts{DeviceID: deviceID, ReportedAt: t.now(), DaemonVersion: guardcli.BuildVersion(), StoreSchema: store.SchemaVersion}
	for _, a := range guardcli.RuntimeAttachStates() {
		// attached = the configuration names our hook AND that binary exists: a
		// configured hook whose binary is gone cannot fire, so "attached" says the
		// defensible rung (both facts), not the configuration fact alone.
		rf := teamlink.RuntimeFacts{Name: a.Name, Attached: a.Attached && a.BinaryPresent}
		if o, ok := observed.live[a.Name]; ok {
			at := time.Unix(o.TS, 0)
			rf.LastLive = &at
		}
		if o, ok := observed.canaries[a.Name]; ok {
			at := time.Unix(o.TS, 0)
			rf.Canary = &at
		}
		facts.Runtimes = append(facts.Runtimes, rf)
	}
	// A rulebook that fails to load reports as no layer and a canary that says
	// "not provable": a weak negative, named as such rather than read as fact. Log
	// once per gather; the report's consumer sees the absence, not a fabricated layer.
	if doc, err := rulebook.LoadDocumentPreview(); err == nil && doc.Active {
		facts.RulebookDigest, facts.RulebookLayer = doc.Digest, "user"
	} else if err != nil {
		log.Printf("team link: rulebook preview failed (reported as no layer): %v", err)
	}
	facts.CanaryRuleActive = observed.ruleActive
	return facts, nil
}
