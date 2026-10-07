package daemon

import (
	"context"
	"errors"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"crossing-guard/internal/changeenv"
	"crossing-guard/internal/guardcli"
	"crossing-guard/internal/teamlink"
	"crossing-guard/memory"
	"crossing-guard/store"
	"crossing-guard/teamwire"
)

// The memory pull job (team item 5, plan decisions 4, 5, 15, 16, 17, 18). Its own
// goroutine on the link's lifecycle, at pull_interval, never on the hook path
// (invariant 5): each tick pulls the organization's memory rows after this device's
// cursor, lands them page by page (pull_apply_batch rows per local transaction), writes
// the mirror for what landed (never a git commit), advances the cursor, and acknowledges
// it to the server — the one owner of deletion progress (decision 16). A crash between
// landing and the cursor re-pulls and no-ops (decision 17). The same tick runs the
// guarded identity upgrade for weak repository records (decision 18), which never
// enqueues.

// memoryPullCursorPrefix is the sync_cursor scope the memory pull keeps, completed by the
// organization's id: a cursor is a position in ONE organization's sequence, so a later
// link to another organization can never start from this one's (PW-M4).
const memoryPullCursorPrefix = "memory:"

func memoryPullCursor(orgID string) string { return memoryPullCursorPrefix + orgID }

func (t *teamLinker) startMemoryPullLocked() {
	if t.memPullStop != nil || t.halted {
		return
	}
	stop := make(chan struct{})
	t.memPullStop = stop
	t.jobs.Go(func() { t.runMemoryPull(stop) })
}

func (t *teamLinker) runMemoryPull(stop chan struct{}) {
	t.mu.Lock()
	interval, first := t.doc.PullInterval.Duration, t.doc.MemoryPullStartDelay.Duration
	t.mu.Unlock()
	timer := time.NewTimer(first)
	defer timer.Stop()
	for {
		select {
		case <-stop:
			return
		case <-timer.C:
		}
		t.pullMemoryOnce()
		t.upgradeMemoryIdentities()
		timer.Reset(interval)
	}
}

// pullMemorySoon runs one pull now, as one of the linker's jobs (stopped and waited for
// with the others), instead of waiting for the cadence. It does nothing when the device
// is not linked or the linker is stopping.
func (t *teamLinker) pullMemorySoon() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.halted || t.state != teamLinked {
		return
	}
	t.jobs.Go(t.pullMemoryOnce)
}

// pullMemoryOnce pulls and lands up to memory_pull_pages pages. Before every store write
// it re-checks the link generation, so a tick that was in flight when the device
// unlinked lands nothing and records no cursor after the unlink's reset (PW-M4).
func (t *teamLinker) pullMemoryOnce() {
	t.memPullMu.Lock()
	defer t.memPullMu.Unlock()
	t.mu.Lock()
	if t.state != teamLinked || t.key == nil {
		t.mu.Unlock()
		return
	}
	client, err := teamlink.NewClient(t.doc.Server, t.deviceID, t.key, t.doc.RequestTimeout.Duration, t.now)
	gen, batch, pages, orgID := t.gen, t.doc.PullApplyBatch, t.doc.MemoryPullPages, t.doc.Organization.ID
	scope := memoryPullCursor(orgID)
	retry := t.report.Pull.Unlandable > 0 && t.report.Pull.UnlandableBuild != guardcli.BuildVersion()
	if retry {
		t.report.Pull.Unlandable, t.report.Pull.UnlandableBuild = 0, ""
	}
	t.mu.Unlock()
	if retry {
		// A different build skipped rows it could not land; this one starts the pull over
		// (landing is idempotent), so a record a newer build can read is not lost (CR-4).
		if err := governor.ix.ClearSyncCursor(scope); err != nil {
			log.Printf("team memory: the pull could not be restarted for skipped rows: %v", err)
		}
	}
	if err != nil {
		t.recordPull(gen, "error", err.Error(), store.MemorySyncEffects{}, 0)
		return
	}
	// The reset epoch is read before the cursor: a restart between the two reads then
	// fails the compare-and-set instead of slipping past it (FR-3).
	epoch, err := governor.ix.MemoryPullEpoch()
	if err != nil {
		t.recordPull(gen, "error", "reading the pull epoch: "+err.Error(), store.MemorySyncEffects{}, 0)
		return
	}
	cursor, err := governor.ix.SyncCursor(scope)
	if err != nil {
		t.recordPull(gen, "error", "reading the cursor: "+err.Error(), store.MemorySyncEffects{}, 0)
		return
	}
	var total store.MemorySyncEffects
	for page := 0; page < pages; page++ {
		resp, err := client.Pull(cursor, []string{teamwire.KindMemory, teamwire.KindTombstone})
		if err != nil {
			if teamlink.Code(err) == teamwire.CodeDeviceRevoked {
				t.mu.Lock()
				if t.gen == gen && t.state == teamLinked {
					t.state, t.problem = teamRevoked, "the server revoked this device's key; sync is paused and nothing local is deleted — unlink and link again to re-enroll"
				}
				t.mu.Unlock()
			}
			t.recordPull(gen, "error", err.Error(), total, cursor)
			return
		}
		if !t.sameLink(gen) {
			return
		}
		eff, err := governor.ix.ApplyPulledRows(resp.Rows, batch, orgID)
		mirrorMemoryEffects(eff)
		total.Landed = append(total.Landed, eff.Landed...)
		total.Removed = append(total.Removed, eff.Removed...)
		total.Held += eff.Held
		total.Ignored += eff.Ignored
		total.Conflicts += eff.Conflicts
		total.Discarded += eff.Discarded
		total.Shadowed += eff.Shadowed
		total.Aliased += eff.Aliased
		total.Unlandable += eff.Unlandable
		if errors.Is(err, store.ErrLinkChanged) {
			return // the device unlinked under this tick: nothing landed, nothing recorded
		}
		if err != nil {
			// Nothing past the failing row advanced the cursor: it re-pulls and the rows
			// that landed no-op (decision 17).
			t.recordPull(gen, "error", "landing: "+err.Error(), total, cursor)
			return
		}
		if resp.Cursor > cursor {
			if !t.sameLink(gen) {
				return
			}
			// Compare-and-set from the cursor this tick read: a reset made meanwhile (a
			// deletion the team did not take restarts the pull) is never overwritten (CR-2).
			moved, err := governor.ix.AdvanceSyncCursor(scope, cursor, resp.Cursor, epoch, t.now().Unix())
			if err != nil {
				t.recordPull(gen, "error", "recording the cursor: "+err.Error(), total, cursor)
				return
			}
			if !moved {
				t.recordPull(gen, "pulled", "", total, 0)
				return
			}
			cursor = resp.Cursor
			if _, err := client.Ack(cursor); err != nil {
				log.Printf("team memory: cursor %d not acknowledged (the next tick re-acknowledges): %v", cursor, err)
			}
		}
		if !resp.More {
			break
		}
	}
	t.recordPull(gen, "pulled", "", total, cursor)
}

// sameLink reports whether the link the caller started under is still the current one.
func (t *teamLinker) sameLink(gen uint64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.gen == gen && t.state == teamLinked
}

// recordPull persists one tick's outcome and counts.
func (t *teamLinker) recordPull(gen uint64, outcome, detail string, eff store.MemorySyncEffects, cursor int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.gen != gen {
		return
	}
	p := &t.report.Pull
	p.LastAt, p.Outcome, p.Error = t.now(), outcome, detail
	if cursor > 0 {
		p.Cursor = cursor // the stored position, which a restart of the pull moves back
	}
	p.Landed += int64(len(eff.Landed))
	p.Deleted += int64(len(eff.Removed))
	p.Held += int64(eff.Held)
	p.Ignored += int64(eff.Ignored)
	p.Conflicts += int64(eff.Conflicts)
	p.Discarded += int64(eff.Discarded)
	p.Shadowed += int64(eff.Shadowed)
	p.Aliased += int64(eff.Aliased)
	if eff.Unlandable > 0 {
		p.Unlandable += int64(eff.Unlandable)
		p.UnlandableBuild = guardcli.BuildVersion()
	}
	if err := teamlink.SaveReportState(t.storeDir, t.report); err != nil {
		log.Printf("team memory: state not written: %v", err)
	}
	if outcome == "error" {
		log.Printf("team memory: pull: %s", detail)
	}
}

// mirrorMemoryEffects keeps the write-through mirror in step with a sync transition: a
// landed record's file is written, a deleted one removed. Never a git commit (pulled
// bodies must not enter the mirror's history, decision 5 / criterion 7).
func mirrorMemoryEffects(eff store.MemorySyncEffects) {
	for _, r := range eff.Landed {
		mirrorStoreRecord(r)
	}
	for _, id := range eff.Removed {
		_ = memory.MirrorDelete(memory.DefaultDir(), id)
	}
}

// resolveCheckout is the repository resolution the identity upgrade uses (a var so a test
// can count the calls and make one time out without a slow git).
var resolveCheckout = changeenv.ResolveRepositoryContext

// upgradeMemoryIdentities is decision 18's guarded upgrade: a weak repository record
// whose label exactly one resolved checkout carries — and that checkout resolves to one
// remote — becomes remote-sha256 under that repository's id. An ambiguous label stays
// weak, with the reason recorded for the console. The upgrade is a local revision that
// never enqueues: sharing upgraded records is the explicit share action (decision 14).
//
// It forks git, so it runs only when its inputs changed since the last run — the set of
// weak labels and the checkout roots this device has seen — never on every tick (PW-M6).
// A root that could not be resolved in time makes the whole pass inconclusive: a label
// is called unique only when every seen root answered. The write names the revision that
// was read, so an edit made meanwhile is never overwritten; the record is retried when
// the inputs next change.
func (t *teamLinker) upgradeMemoryIdentities() {
	t.mu.Lock()
	linked, maxRoots, timeout, last := t.state == teamLinked, t.doc.IdentityUpgradeRoots, t.doc.IdentityResolveTimeout.Duration, t.upgradeInputs
	t.mu.Unlock()
	if !linked {
		return
	}
	recs, err := governor.ix.ListMemory("")
	if err != nil {
		return
	}
	weak := map[string][]store.MemoryRecord{} // folded label → records
	var labels []string
	for _, r := range recs {
		if r.ScopeType == store.MemoryScopeRepository && r.RepositoryIdentity == "weak" && r.ScopeID != "" {
			label := strings.ToLower(r.ScopeID)
			if _, seen := weak[label]; !seen {
				labels = append(labels, label)
			}
			weak[label] = append(weak[label], r)
		}
	}
	sort.Strings(labels)
	if len(weak) == 0 {
		return
	}
	roots, err := governor.ix.CheckoutRoots(maxRoots)
	if err != nil {
		return
	}
	// The pass's inputs: every weak record (by label and id — a new record under a known
	// label is new input) and every checkout root seen.
	var keys []string
	for _, label := range labels {
		for _, r := range weak[label] {
			keys = append(keys, label+"\x02"+r.ID)
		}
	}
	sort.Strings(keys)
	inputs := strings.Join(keys, "\x00") + "\x01" + strings.Join(roots, "\x00")
	if inputs == last {
		return
	}
	resolved := map[string]map[string]changeenv.Repository{} // folded label → repository id → repository
	for _, root := range roots {
		if info, err := os.Stat(root); err != nil || !info.IsDir() {
			continue // the checkout is gone: it cannot make a label ambiguous
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		repo, err := resolveCheckout(ctx, root)
		timedOut := ctx.Err() != nil
		cancel()
		if timedOut {
			return // inconclusive: retried on the next tick, nothing is upgraded on a guess
		}
		if err != nil {
			continue
		}
		label := strings.ToLower(memory.ProjectFromCommonDir(repo.CommonDir, repo.Root))
		if _, ok := weak[label]; !ok {
			continue
		}
		if resolved[label] == nil {
			resolved[label] = map[string]changeenv.Repository{}
		}
		resolved[label][repo.ID] = repo
	}
	actor := store.MemoryActor{AuthorType: "daemon", AuthorID: "identity-upgrade", ActorSource: "daemon"}
	for label, records := range weak {
		repos := resolved[label]
		note := ""
		var target changeenv.Repository
		switch {
		case len(repos) == 0:
			note = "no checkout of a repository named " + label + " has been seen on this device"
		case len(repos) > 1:
			note = "more than one repository on this device is named " + label + "; the folder name cannot say which"
		default:
			for _, r := range repos {
				target = r
			}
			if target.IdentityKind != "remote-sha256" {
				note = "the repository named " + label + " has no single origin remote"
			}
		}
		for _, r := range records {
			if note != "" {
				if r.IdentityNote != note {
					_ = governor.ix.SetMemoryIdentityNote(r.ID, note)
				}
				continue
			}
			read := r.Revision
			r.ScopeID, r.RepositoryIdentity, r.IdentityNote = target.ID, "remote-sha256", ""
			saved, err := governor.ix.ReviseMemoryAt(r, read, nil, nil, actor)
			if err != nil {
				if !errors.Is(err, store.ErrMemoryStale) && !errors.Is(err, store.ErrMemoryNotFound) {
					log.Printf("team memory: identity upgrade of %s not written: %v", r.ID, err)
				}
				inputs = "" // edited or deleted meanwhile: run again
				continue
			}
			mirrorStoreRecord(saved)
		}
	}
	t.mu.Lock()
	t.upgradeInputs = inputs
	t.mu.Unlock()
}
