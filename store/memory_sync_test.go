package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"crossing-guard/engine"
	"crossing-guard/teamwire"
)

// The store half of team item 5 against an in-memory team that answers exactly as the
// plan's decision 13a orders its checks (duplicate → same-device conflict → stale_base →
// accept). The server's real-Postgres simulations drive the same store code against the
// real ingest; these pin the device's transitions one by one.

type fakeTeam struct {
	t       *testing.T
	seq     int64
	current map[string]teamwire.PulledMemory // global id → the server's current revision
	stored  map[string]int64                 // global id + wire hash → server revision (13a check 1)
	pushIDs map[string]string                // device + push id → wire hash (check 2)
	rows    []teamwire.PullRow
	tombs   map[string]bool
	// refuseDeletes answers every tombstone delete_not_allowed (a member deleting a
	// record that is not theirs); refuseMemory answers a device's memory pushes by code.
	refuseDeletes bool
	refuseMemory  map[string]string
}

func newFakeTeam(t *testing.T) *fakeTeam {
	return &fakeTeam{t: t, current: map[string]teamwire.PulledMemory{}, stored: map[string]int64{}, pushIDs: map[string]string{}, tombs: map[string]bool{}}
}

func (f *fakeTeam) push(device, author string, rec teamwire.PushRecord) teamwire.PushResult {
	if rec.Kind == teamwire.KindTombstone {
		var tomb teamwire.Tombstone
		if err := json.Unmarshal(rec.Body, &tomb); err != nil {
			f.t.Fatal(err)
		}
		if f.refuseDeletes {
			return teamwire.PushResult{ID: rec.ID, Status: teamwire.StatusRejected, Error: &teamwire.Error{Code: teamwire.CodeDeleteNotAllowed}}
		}
		if f.tombs[tomb.RecordID] {
			return teamwire.PushResult{ID: rec.ID, Status: teamwire.StatusDuplicate}
		}
		f.tombs[tomb.RecordID] = true
		delete(f.current, tomb.RecordID)
		f.seq++
		f.rows = append(f.rows, teamwire.PullRow{Seq: f.seq, Kind: teamwire.KindTombstone, Tombstone: &teamwire.PulledTombstone{Tombstone: tomb, DeletedByUserID: author}})
		return teamwire.PushResult{ID: rec.ID, Status: teamwire.StatusAccepted}
	}
	var w teamwire.MemoryRecord
	if err := json.Unmarshal(rec.Body, &w); err != nil {
		f.t.Fatal(err)
	}
	if teamwire.MemoryWireHash(w) != w.ContentHash {
		f.t.Fatalf("the device's wire hash does not recompute: %s", rec.ID)
	}
	if f.tombs[w.ID] {
		return teamwire.PushResult{ID: rec.ID, Status: teamwire.StatusRejected, Error: &teamwire.Error{Code: teamwire.CodeTombstoned}}
	}
	if n, ok := f.stored[w.ID+w.ContentHash]; ok {
		return teamwire.PushResult{ID: rec.ID, Status: teamwire.StatusDuplicate, Memory: &teamwire.MemoryAnswer{ServerRevision: n}}
	}
	if h, ok := f.pushIDs[device+rec.ID]; ok && h != w.ContentHash {
		return teamwire.PushResult{ID: rec.ID, Status: teamwire.StatusConflict}
	}
	if code := f.refuseMemory[device]; code != "" {
		return teamwire.PushResult{ID: rec.ID, Status: teamwire.StatusRejected, Error: &teamwire.Error{Code: code}}
	}
	cur, has := f.current[w.ID]
	if (has && w.BaseContentHash != cur.WireHash) || (!has && w.BaseContentHash != "") {
		var c *teamwire.PulledMemory
		if has {
			cc := cur
			c = &cc
		}
		return teamwire.PushResult{ID: rec.ID, Status: teamwire.StatusRejected, Error: &teamwire.Error{Code: teamwire.CodeStaleBase}, Memory: &teamwire.MemoryAnswer{Current: c}}
	}
	next := teamwire.PulledMemory{Record: w, WireHash: w.ContentHash, ServerRevision: cur.ServerRevision + 1, AuthorUserID: author}
	f.current[w.ID] = next
	f.stored[w.ID+w.ContentHash] = next.ServerRevision
	f.pushIDs[device+rec.ID] = w.ContentHash
	f.seq++
	n := next
	f.rows = append(f.rows, teamwire.PullRow{Seq: f.seq, Kind: teamwire.KindMemory, Memory: &n})
	return teamwire.PushResult{ID: rec.ID, Status: teamwire.StatusAccepted, Memory: &teamwire.MemoryAnswer{ServerRevision: next.ServerRevision}}
}

// pullAfter serves, like the server, each record's CURRENT revision only: an earlier
// revision of a record revised again later is not in the page.
func (f *fakeTeam) pullAfter(cursor int64) []teamwire.PullRow {
	latest := map[string]int64{}
	for _, r := range f.rows {
		if r.Kind == teamwire.KindMemory {
			latest[r.Memory.Record.ID] = r.Seq
		}
	}
	var out []teamwire.PullRow
	for _, r := range f.rows {
		if r.Seq <= cursor || (r.Kind == teamwire.KindMemory && latest[r.Memory.Record.ID] != r.Seq) {
			continue
		}
		out = append(out, r)
	}
	return out
}

type device struct {
	t      *testing.T
	ix     *Index
	name   string
	author string
	dets   []engine.Detector
}

func newDevice(t *testing.T, name string) *device {
	ix := openTestMemoryStore(t)
	if err := ix.SetLinked(true); err != nil {
		t.Fatal(err)
	}
	dets, err := engine.DefaultDetectors()
	if err != nil {
		t.Fatal(err)
	}
	return &device{t: t, ix: ix, name: name, author: "usr_" + name, dets: dets}
}

// drainWith pushes until the outbox is empty, answering each record with answer (nil =
// the fake team). Returns the codes settled, in order.
func (d *device) drain(f *fakeTeam) []string {
	d.t.Helper()
	var codes []string
	for range 20 {
		rows, err := d.ix.OutboxBatch(50, nil)
		if err != nil {
			d.t.Fatal(err)
		}
		if len(rows) == 0 {
			return codes
		}
		for _, row := range rows {
			if !memorySyncKind(row.Kind) {
				continue
			}
			rec, code, err := d.ix.MemoryPushRecord(row, d.dets, "org_test")
			if err != nil {
				d.t.Fatal(err)
			}
			s := MemorySettle{Seq: row.Seq, Code: code}
			if code == "" {
				res := f.push(d.name, d.author, rec)
				s.Code = res.Status
				if res.Error != nil {
					s.Code = res.Error.Code
				}
				if res.Memory != nil {
					s.ServerRevision, s.Current = res.Memory.ServerRevision, res.Memory.Current
				}
			}
			codes = append(codes, s.Code)
			if _, err := d.ix.SettleMemoryPush(s, 1); err != nil {
				d.t.Fatal(err)
			}
		}
	}
	d.t.Fatal("outbox did not drain")
	return nil
}

func memorySyncKind(kind string) bool { return kind == OutboxMemory || kind == OutboxTombstone }

func (d *device) pull(f *fakeTeam) MemorySyncEffects {
	d.t.Helper()
	cursor, err := d.ix.SyncCursor("memory")
	if err != nil {
		d.t.Fatal(err)
	}
	rows := f.pullAfter(cursor)
	eff, err := d.ix.ApplyPulledRows(rows, 1, "")
	if err != nil {
		d.t.Fatal(err)
	}
	if len(rows) > 0 {
		if err := d.ix.SetSyncCursor("memory", rows[len(rows)-1].Seq, 1); err != nil {
			d.t.Fatal(err)
		}
	}
	return eff
}

func (d *device) rec(id string) MemoryRecord {
	d.t.Helper()
	r, err := d.ix.MemoryByID(id)
	if err != nil {
		d.t.Fatalf("%s: %s: %v", d.name, id, err)
	}
	return r
}

func (d *device) byGID(gid string) (MemoryRecord, bool) {
	d.t.Helper()
	r, ok, err := d.ix.MemoryByGlobalID(gid)
	if err != nil {
		d.t.Fatal(err)
	}
	return r, ok
}

func (d *device) edit(id, body string) MemoryRecord {
	d.t.Helper()
	r := d.rec(id)
	r.Body = body
	saved, err := d.ix.UpsertMemory(r, nil, nil, MemoryActor{AuthorType: "user", AuthorID: d.name, ActorSource: "cli"})
	if err != nil {
		d.t.Fatal(err)
	}
	return saved
}

func (d *device) pendingRows(gid string) int {
	d.t.Helper()
	n, err := unackedMemoryRows(d.ix.db, gid)
	if err != nil {
		d.t.Fatal(err)
	}
	return n
}

func (d *device) conflicts(gid string) []MemoryConflict {
	d.t.Helper()
	c, err := d.ix.MemoryConflicts(gid, 50)
	if err != nil {
		d.t.Fatal(err)
	}
	return c
}

func inSync(r MemoryRecord) bool { return MemoryProjectionHash(r) == r.SyncedProjectionHash }

func orgRecord(id, body string) MemoryRecord {
	r := memRecord(id)
	r.Body, r.ScopeType, r.ScopeID, r.Status = body, MemoryScopeOrganization, "org_test", "active"
	return r
}

// shareFromA creates an organization record on A, pushes it, and lands it on B.
func shareFromA(t *testing.T, f *fakeTeam, a, b *device, id, body string) string {
	t.Helper()
	r, err := a.ix.CreateMemory(orgRecord(id, body), nil, nil, memHuman())
	if err != nil {
		t.Fatal(err)
	}
	if r.ShareState != "shared" {
		t.Fatalf("a record created at organization scope on a linked device is shared by default, got %q", r.ShareState)
	}
	if got := a.drain(f); len(got) != 1 || got[0] != teamwire.StatusAccepted {
		t.Fatalf("first push: %v", got)
	}
	b.pull(f)
	if _, ok := b.byGID(r.GlobalID); !ok {
		t.Fatal("B did not land the shared record")
	}
	return r.GlobalID
}

// Criterion 41 (decision 14): only active, shared records at a travelling scope leave;
// the pre-44 backlog is acked not_shareable on its first tick, never encoded from current
// state; an identity upgrade enqueues nothing until the share action.
func TestMemoryGateRefusesTheBacklogAndUnreviewedRecords(t *testing.T) {
	d := newDevice(t, "a")
	for _, r := range []MemoryRecord{
		func() MemoryRecord {
			r := memRecord("weak")
			r.ScopeType, r.ScopeID, r.RepositoryIdentity = MemoryScopeRepository, "proj", "weak"
			return r
		}(),
		func() MemoryRecord { r := orgRecord("draft", "x"); r.Status = "pending"; return r }(),
		func() MemoryRecord { r := orgRecord("rejected", "x"); r.Status = "rejected"; return r }(),
	} {
		if _, err := d.ix.CreateMemory(r, nil, nil, memHuman()); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := d.ix.OutboxPending(); n != 0 {
		t.Fatalf("weak, pending and rejected records must enqueue nothing, got %d rows", n)
	}
	// The 291-row shape: pending memory rows written before schema 44 name no revision.
	weak := d.rec("weak")
	for range 3 {
		if _, err := d.ix.db.Exec(`INSERT INTO sync_outbox(record_kind,global_id,content_hash,scope,enqueued_at) VALUES('memory',?,?,'repository:proj',1)`, weak.GlobalID, weak.ContentHash); err != nil {
			t.Fatal(err)
		}
	}
	f := newFakeTeam(t)
	codes := d.drain(f)
	if len(codes) != 3 || len(f.rows) != 0 {
		t.Fatalf("the backlog must be refused locally, nothing sent: codes %v, sent %d", codes, len(f.rows))
	}
	for _, c := range codes {
		if c != teamwire.CodeNotShareable {
			t.Fatalf("backlog row settled %q, want not_shareable", c)
		}
	}
	sum, err := d.ix.MemorySyncSummaryCounts("org_test", 0)
	if err != nil || sum.NotShareable != 3 {
		t.Fatalf("not_shareable must be counted: %+v %v", sum, err)
	}
	// An identity upgrade is a local revision that never enqueues (decision 18).
	weak.ScopeID, weak.RepositoryIdentity = "rep_remote_id", "remote-sha256"
	if _, err := d.ix.UpsertMemory(weak, nil, nil, memHuman()); err != nil {
		t.Fatal(err)
	}
	if n, _ := d.ix.OutboxPending(); n != 0 {
		t.Fatalf("an identity upgrade must not enqueue, got %d", n)
	}
	bulk, imported, err := d.ix.MemoryShareCandidates("org_test")
	if err != nil || len(bulk) != 1 || len(imported) != 0 {
		t.Fatalf("the upgraded record is a share candidate: %d %d %v", len(bulk), len(imported), err)
	}
	if n, err := d.ix.ShareMemory(nil, "org_test"); err != nil || n != 1 {
		t.Fatalf("share action: %d %v", n, err)
	}
	if codes := d.drain(f); len(codes) != 1 || codes[0] != teamwire.StatusAccepted {
		t.Fatalf("the shared record pushes once: %v", codes)
	}
}

// Criterion 40, push-then-pull: B edits offline after A's edit landed on the server; B's
// push answers stale_base (no security event), B's body survives as a conflict copy, the
// carried server revision lands, and B's next edit is accepted.
func TestTwoDevicesOfflineEditPushThenPull(t *testing.T) {
	f := newFakeTeam(t)
	a, b := newDevice(t, "a"), newDevice(t, "b")
	gid := shareFromA(t, f, a, b, "vpn", "v1")
	a.edit("vpn", "A's edit")
	a.drain(f)
	b.edit("vpn", "B's edit")
	codes := b.drain(f)
	if len(codes) != 1 || codes[0] != teamwire.CodeStaleBase {
		t.Fatalf("B's stale push: %v", codes)
	}
	r := b.rec("vpn")
	if r.Body != "A's edit" || !inSync(r) {
		t.Fatalf("the carried revision must land and leave B in sync: body %q insync %v", r.Body, inSync(r))
	}
	if c := b.conflicts(gid); len(c) != 1 || c[0].Body != "B's edit" || c[0].Reason != "stale_base" {
		t.Fatalf("B's body must survive as a conflict copy: %+v", c)
	}
	b.edit("vpn", "B again")
	if codes := b.drain(f); len(codes) != 1 || codes[0] != teamwire.StatusAccepted {
		t.Fatalf("B's next edit must be accepted: %v", codes)
	}
	if f.current[gid].Record.Body != "B again" {
		t.Fatalf("server holds %q", f.current[gid].Record.Body)
	}
}

// Criterion 40, pull-then-push: B pulls A's revision while its own edit is unacked — the
// pull is held — then B's push answers stale_base, the carried revision lands, the held
// one (no newer) is discarded by rule 1, and the edit survives as a conflict copy.
func TestTwoDevicesOfflineEditPullThenPush(t *testing.T) {
	f := newFakeTeam(t)
	a, b := newDevice(t, "a"), newDevice(t, "b")
	gid := shareFromA(t, f, a, b, "vpn", "v1")
	a.edit("vpn", "A's edit")
	a.drain(f)
	b.edit("vpn", "B's edit")
	if eff := b.pull(f); eff.Held != 1 {
		t.Fatalf("a pull over an unacked push must be held: %+v", eff)
	}
	if r := b.rec("vpn"); r.Body != "B's edit" || r.HeldServerRevision != 2 {
		t.Fatalf("held, not landed: body %q held %d", r.Body, r.HeldServerRevision)
	}
	if codes := b.drain(f); len(codes) != 1 || codes[0] != teamwire.CodeStaleBase {
		t.Fatalf("B's push: %v", codes)
	}
	r := b.rec("vpn")
	if r.Body != "A's edit" || r.HeldServerRevision != 0 || !inSync(r) || r.ServerRevision != 2 {
		t.Fatalf("after settle: body %q held %d insync %v server %d", r.Body, r.HeldServerRevision, inSync(r), r.ServerRevision)
	}
	if c := b.conflicts(gid); len(c) != 1 || c[0].Body != "B's edit" {
		t.Fatalf("conflict copy: %+v", c)
	}
	b.edit("vpn", "B next")
	if codes := b.drain(f); codes[0] != teamwire.StatusAccepted {
		t.Fatalf("next edit: %v", codes)
	}
}

// Criterion 40 (Q-1): with two queued offline edits on B and A's concurrent write, B's
// second edit is acked superseded and A's edit survives on the server.
func TestStaleBaseSupersedesLaterQueuedEdits(t *testing.T) {
	f := newFakeTeam(t)
	a, b := newDevice(t, "a"), newDevice(t, "b")
	gid := shareFromA(t, f, a, b, "vpn", "v1")
	b.edit("vpn", "B one")
	b.edit("vpn", "B two")
	if rows, _ := b.ix.OutboxBatch(50, nil); len(rows) != 1 {
		t.Fatalf("one revision per record in flight: %d rows in the batch", len(rows))
	}
	a.edit("vpn", "A wins")
	a.drain(f)
	codes := b.drain(f)
	if len(codes) != 1 || codes[0] != teamwire.CodeStaleBase {
		t.Fatalf("only the head is pushed and answered stale: %v", codes)
	}
	var superseded int
	if err := b.ix.db.QueryRow(`SELECT count(*) FROM sync_outbox WHERE global_id=? AND ack_code='superseded'`, gid).Scan(&superseded); err != nil || superseded != 1 {
		t.Fatalf("the later queued edit must be acked superseded: %d %v", superseded, err)
	}
	if f.current[gid].Record.Body != "A wins" {
		t.Fatalf("A's edit must survive on the server, got %q", f.current[gid].Record.Body)
	}
	if c := b.conflicts(gid); len(c) != 1 || c[0].Body != "B two" {
		t.Fatalf("the conflict copy holds B's latest body: %+v", c)
	}
}

// Criterion 40 (Q-2): a pull that arrives while B's push is unacked is held and lands
// after EACH answer type.
func TestHeldPullLandsAfterEveryAnswer(t *testing.T) {
	for _, answer := range []string{teamwire.StatusAccepted, teamwire.StatusDuplicate, teamwire.CodeStaleBase, teamwire.CodeNotShareable, "internal_exhausted"} {
		t.Run(answer, func(t *testing.T) {
			f := newFakeTeam(t)
			a, b := newDevice(t, "a"), newDevice(t, "b")
			gid := shareFromA(t, f, a, b, "vpn", "v1")
			b.edit("vpn", "B edit")
			a.edit("vpn", "A edit")
			a.drain(f)
			if eff := b.pull(f); eff.Held != 1 {
				t.Fatalf("held: %+v", eff)
			}
			rows, _ := b.ix.OutboxBatch(50, nil)
			rec, _, err := b.ix.MemoryPushRecord(rows[0], b.dets, "org_test")
			if err != nil {
				t.Fatal(err)
			}
			s := MemorySettle{Seq: rows[0].Seq, Code: answer}
			switch answer {
			case teamwire.StatusAccepted, teamwire.StatusDuplicate:
				// The server stored B's revision after A's (as though B's base were current).
				s.ServerRevision = 3
			case teamwire.CodeStaleBase:
				cur := f.current[gid]
				s.Current = &cur
			}
			_ = rec
			if _, err := b.ix.SettleMemoryPush(s, 1); err != nil {
				t.Fatal(err)
			}
			r := b.rec("vpn")
			if r.HeldServerRevision != 0 {
				t.Fatalf("the held slot must be cleared after %s", answer)
			}
			switch answer {
			case teamwire.StatusAccepted, teamwire.StatusDuplicate:
				// B's own revision (server 3) is newer than the held one (2): rule 1 discards it.
				if r.Body != "B edit" || r.ServerRevision != 3 || r.PushedHash == "" {
					t.Fatalf("%s: body %q server %d pushed %q", answer, r.Body, r.ServerRevision, r.PushedHash)
				}
			default:
				if r.Body != "A edit" || r.ServerRevision != 2 {
					t.Fatalf("%s: the held revision must land: body %q server %d", answer, r.Body, r.ServerRevision)
				}
				if c := b.conflicts(gid); len(c) != 1 || c[0].Body != "B edit" {
					t.Fatalf("%s: B's unsent edit must survive as a conflict copy: %+v", answer, c)
				}
			}
		})
	}
}

// Criterion 40: a pending local edit overwritten by a pull becomes a conflict copy; a
// console edit (pending) followed by promote reaches the team with the edit's body.
func TestPendingEditIsKeptAndPromoteCarriesIt(t *testing.T) {
	f := newFakeTeam(t)
	a, b := newDevice(t, "a"), newDevice(t, "b")
	gid := shareFromA(t, f, a, b, "vpn", "v1")
	r := b.rec("vpn")
	r.Body, r.Status = "B draft", "pending" // the console re-drafts an edit as pending
	if _, err := b.ix.UpsertMemory(r, nil, nil, memHuman()); err != nil {
		t.Fatal(err)
	}
	if n := b.pendingRows(gid); n != 0 {
		t.Fatalf("a pending edit enqueues nothing, got %d", n)
	}
	if _, err := b.ix.PromoteMemory("vpn", memHuman()); err != nil {
		t.Fatal(err)
	}
	b.drain(f)
	if f.current[gid].Record.Body != "B draft" {
		t.Fatalf("promote must carry the edit's body to the team, server has %q", f.current[gid].Record.Body)
	}
	// Now a pending edit that a teammate's revision overwrites.
	a.pull(f)
	r = a.rec("vpn")
	r.Body, r.Status = "A draft", "pending"
	if _, err := a.ix.UpsertMemory(r, nil, nil, memHuman()); err != nil {
		t.Fatal(err)
	}
	b.edit("vpn", "B again")
	b.drain(f)
	a.pull(f)
	if got := a.rec("vpn"); got.Body != "B again" || got.Status != "active" {
		t.Fatalf("the team revision lands active: %q %s", got.Body, got.Status)
	}
	if c := a.conflicts(gid); len(c) != 1 || c[0].Body != "A draft" || c[0].Reason != "pulled_over_edit" {
		t.Fatalf("A's pending draft must be a conflict copy: %+v", c)
	}
}

// Criterion 40 (O-9, T-3, U-1, U-2, U-4): rejecting a shared record enqueues nothing and
// leaves the server unchanged; the next landed revision restores it without a stale
// reason; a diverged rejected draft is kept as a conflict copy; re-promoting a
// non-diverged rejected record enqueues nothing, a diverged one sends the draft.
func TestRejectIsALocalFlip(t *testing.T) {
	f := newFakeTeam(t)
	a, b := newDevice(t, "a"), newDevice(t, "b")
	gid := shareFromA(t, f, a, b, "vpn", "v1")
	if _, err := b.ix.RejectMemory("vpn", "not true here", memHuman()); err != nil {
		t.Fatal(err)
	}
	if r := b.rec("vpn"); r.Status != "rejected" || r.RejectReason != "not true here" {
		t.Fatalf("reject: %s %q", r.Status, r.RejectReason)
	}
	if n := b.pendingRows(gid); n != 0 || f.current[gid].ServerRevision != 1 {
		t.Fatalf("reject enqueues nothing and leaves the server unchanged: %d rows, server rev %d", n, f.current[gid].ServerRevision)
	}
	// Re-promoting a non-diverged rejected record has nothing to send (U-2).
	if _, err := b.ix.PromoteMemory("vpn", memHuman()); err != nil {
		t.Fatal(err)
	}
	if n := b.pendingRows(gid); n != 0 {
		t.Fatalf("re-promoting an unchanged record must enqueue nothing, got %d", n)
	}
	if _, err := b.ix.RejectMemory("vpn", "still not", memHuman()); err != nil {
		t.Fatal(err)
	}
	a.edit("vpn", "A v2")
	a.drain(f)
	b.pull(f)
	r := b.rec("vpn")
	if r.Status != "active" || r.Body != "A v2" || r.RejectReason != "" {
		t.Fatalf("the landed revision restores it, no stale reason: %s %q %q", r.Status, r.Body, r.RejectReason)
	}
	if c := b.conflicts(gid); len(c) != 0 {
		t.Fatalf("a non-diverged rejected record needs no conflict copy: %+v", c)
	}
	// A diverged rejected draft: kept as a conflict copy when the next revision lands.
	r.Body, r.Status = "B draft", "pending"
	if _, err := b.ix.UpsertMemory(r, nil, nil, memHuman()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ix.RejectMemory("vpn", "drop it", memHuman()); err != nil {
		t.Fatal(err)
	}
	// Re-promoting the diverged rejected draft sends the draft (U-1).
	if _, err := b.ix.PromoteMemory("vpn", memHuman()); err != nil {
		t.Fatal(err)
	}
	if n := b.pendingRows(gid); n != 1 {
		t.Fatalf("re-promoting a diverged draft must enqueue it, got %d", n)
	}
	b.drain(f)
	if f.current[gid].Record.Body != "B draft" {
		t.Fatalf("server: %q", f.current[gid].Record.Body)
	}
	// Reject again after a local draft, then a teammate's revision lands: conflict copy.
	a.pull(f)
	r = b.rec("vpn")
	r.Body, r.Status = "B second draft", "pending"
	if _, err := b.ix.UpsertMemory(r, nil, nil, memHuman()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ix.RejectMemory("vpn", "no", memHuman()); err != nil {
		t.Fatal(err)
	}
	a.edit("vpn", "A v3")
	a.drain(f)
	b.pull(f)
	if c := b.conflicts(gid); len(c) != 1 || c[0].Body != "B second draft" {
		t.Fatalf("the diverged rejected draft is a conflict copy: %+v", c)
	}
}

// Criterion 40 (S-1 under O-9): an unsent earlier active edit followed by a draft and a
// reject is not lost — it stays in revision history, and the draft is the conflict copy
// made when the next team revision lands.
func TestUnsentActiveEditSurvivesDraftAndReject(t *testing.T) {
	f := newFakeTeam(t)
	a, b := newDevice(t, "a"), newDevice(t, "b")
	gid := shareFromA(t, f, a, b, "vpn", "v1")
	if _, err := b.ix.db.Exec(`UPDATE memory_record SET share_state='unshared' WHERE global_id=?`, gid); err != nil {
		t.Fatal(err)
	}
	b.edit("vpn", "B active edit, never sent")
	r := b.rec("vpn")
	r.Body, r.Status = "B draft", "pending"
	if _, err := b.ix.UpsertMemory(r, nil, nil, memHuman()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ix.RejectMemory("vpn", "no", memHuman()); err != nil {
		t.Fatal(err)
	}
	a.edit("vpn", "A v2")
	a.drain(f)
	b.pull(f)
	var n int
	if err := b.ix.db.QueryRow(`SELECT count(*) FROM memory_revision WHERE global_id=? AND body LIKE '%B active edit, never sent%'`, gid).Scan(&n); err != nil || n == 0 {
		t.Fatalf("the unsent active edit must remain in revision history: %d %v", n, err)
	}
	if c := b.conflicts(gid); len(c) != 1 || c[0].Body != "B draft" {
		t.Fatalf("conflict copy: %+v", c)
	}
	if got := b.rec("vpn"); got.ShareState != "unshared" {
		t.Fatalf("landing keeps an existing unshared record unshared (S-2), got %s", got.ShareState)
	}
}

// Criterion 40 (K-3, S-3): a duplicate answer after a crash sets pushed_hash; a redacted
// accept leaves the record in sync though the server holds a different body.
func TestDuplicateAfterCrashAndRedactedAcceptStayInSync(t *testing.T) {
	f := newFakeTeam(t)
	d := newDevice(t, "a")
	secret := "deploy key ghp_" + strings.Repeat("a1B2c3", 6) + " lives in the vault"
	r, err := d.ix.CreateMemory(orgRecord("keys", secret), nil, nil, memHuman())
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := d.ix.OutboxBatch(50, nil)
	rec, code, err := d.ix.MemoryPushRecord(rows[0], d.dets, "org_test")
	if err != nil || code != "" {
		t.Fatalf("encode: %q %v", code, err)
	}
	if strings.Contains(string(rec.Body), "ghp_") {
		t.Fatalf("the wire body must be redacted: %s", rec.Body)
	}
	// CR-14: no local account name (the test's author is "tester") or session id rides
	// the wire; the author of record is the one the server authenticates.
	if strings.Contains(string(rec.Body), "tester") || !strings.Contains(string(rec.Body), `"author":{"type":"user","id":"`+teamwire.WireAuthorID+`"}`) {
		t.Fatalf("the wire author must be the fixed value: %s", rec.Body)
	}
	f.push(d.name, d.author, rec) // the server commits; the answer is lost (crash)
	if codes := d.drain(f); len(codes) != 1 || codes[0] != teamwire.StatusDuplicate {
		t.Fatalf("the retry resends the frozen body and is a duplicate: %v", codes)
	}
	got := d.rec("keys")
	if got.PushedHash == "" || got.ServerRevision != 1 || !inSync(got) || got.Body != secret {
		t.Fatalf("duplicate must set the sync state, local body unredacted and in sync: pushed %q rev %d insync %v", got.PushedHash, got.ServerRevision, inSync(got))
	}
	// Pulling back its own redacted record is a no-op (criterion 45).
	if eff := d.pull(f); len(eff.Landed) != 0 || eff.Ignored != 1 {
		t.Fatalf("own record pulled back must no-op: %+v", eff)
	}
	if d.rec("keys").Body != secret {
		t.Fatal("the redacted copy must not overwrite the local body")
	}
	_ = r
}

// Criterion 45: a crash between landing and the cursor advance re-pulls and no-ops; a
// lagging page carrying an older server revision is ignored.
func TestRePullAndLaggingPageNoOp(t *testing.T) {
	f := newFakeTeam(t)
	a, b := newDevice(t, "a"), newDevice(t, "b")
	gid := shareFromA(t, f, a, b, "vpn", "v1")
	a.edit("vpn", "v2")
	a.drain(f)
	rows := f.pullAfter(1)
	if _, err := b.ix.ApplyPulledRows(rows, 1, ""); err != nil {
		t.Fatal(err)
	}
	// crash: cursor not advanced; the re-pull applies the same rows again
	eff, err := b.ix.ApplyPulledRows(f.pullAfter(0), 1, "")
	if err != nil || len(eff.Landed) != 0 || len(b.conflicts(gid)) != 0 {
		t.Fatalf("re-pull must no-op: %+v %v", eff, err)
	}
	if r := b.rec("vpn"); r.Body != "v2" || r.ServerRevision != 2 {
		t.Fatalf("lagging page rolled back: %q %d", r.Body, r.ServerRevision)
	}
}

// Criterion 40 (S-5): a late settle for a deleted record does not re-create it; a pulled
// tombstone over an unacked edit keeps the edit as a conflict copy and erases history.
func TestLateSettleAndPulledTombstone(t *testing.T) {
	f := newFakeTeam(t)
	a, b := newDevice(t, "a"), newDevice(t, "b")
	gid := shareFromA(t, f, a, b, "vpn", "v1")
	b.edit("vpn", "B edit")
	rows, _ := b.ix.OutboxBatch(50, nil)
	if _, _, err := b.ix.MemoryPushRecord(rows[0], b.dets, "org_test"); err != nil {
		t.Fatal(err)
	}
	if err := b.ix.DeleteMemory("vpn", memHuman()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ix.SettleMemoryPush(MemorySettle{Seq: rows[0].Seq, Code: teamwire.StatusAccepted, ServerRevision: 2}, 1); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.byGID(gid); ok {
		t.Fatal("a late settle re-created a deleted record")
	}
	// A's unacked edit meets B's deletion.
	a.edit("vpn", "A unsent edit")
	b.drain(f) // pushes B's tombstone
	eff := a.pull(f)
	if _, ok := a.byGID(gid); ok || len(eff.Removed) != 1 || eff.Discarded != 1 {
		t.Fatalf("the tombstone applies at once: %+v", eff)
	}
	if c := a.conflicts(gid); len(c) != 1 || c[0].Reason != "deleted" || c[0].Body != "A unsent edit" {
		t.Fatalf("the discarded edit is a conflict copy: %+v", c)
	}
	var revs int
	if err := a.ix.db.QueryRow(`SELECT count(*) FROM memory_revision WHERE global_id=?`, gid).Scan(&revs); err != nil || revs != 0 {
		t.Fatalf("O-7: a deleted team record's revisions are erased, %d left %v", revs, err)
	}
	if n := a.pendingRows(gid); n != 0 {
		t.Fatalf("A's queued edit must be superseded, %d pending", n)
	}
	// No later pull re-creates it.
	if eff := a.pull(f); len(eff.Landed) != 0 {
		t.Fatalf("re-created: %+v", eff)
	}
}

// Criterion 40: two queued offline edits push as two revisions, one in flight at a time;
// an internal retry of revision N never makes N+1 a conflict copy; a stale save is refused.
func TestQueuedRevisionsInOrderAndInternalRetry(t *testing.T) {
	f := newFakeTeam(t)
	d := newDevice(t, "a")
	r, err := d.ix.CreateMemory(orgRecord("vpn", "v1"), nil, nil, memHuman())
	if err != nil {
		t.Fatal(err)
	}
	d.edit("vpn", "v2")
	rows, _ := d.ix.OutboxBatch(50, nil)
	if len(rows) != 1 || rows[0].Revision != 1 {
		t.Fatalf("only the head revision is in flight: %+v", rows)
	}
	// revision 1 answered internal (not acknowledged), then retried and accepted
	rec, _, err := d.ix.MemoryPushRecord(rows[0], d.dets, "org_test")
	if err != nil {
		t.Fatal(err)
	}
	f.push(d.name, d.author, rec)
	codes := d.drain(f)
	if len(codes) != 2 || codes[0] != teamwire.StatusDuplicate || codes[1] != teamwire.StatusAccepted {
		t.Fatalf("the retry dedupes and revision 2 follows on the right base: %v", codes)
	}
	if c := d.conflicts(r.GlobalID); len(c) != 0 {
		t.Fatalf("no conflict copy may result: %+v", c)
	}
	if f.current[r.GlobalID].ServerRevision != 2 {
		t.Fatalf("two revisions on the server, got %d", f.current[r.GlobalID].ServerRevision)
	}
	cur := d.rec("vpn")
	cur.Body = "stale save"
	if _, err := d.ix.ReviseMemoryAt(cur, cur.Revision-1, nil, nil, memHuman()); !errors.Is(err, ErrMemoryStale) {
		t.Fatalf("a save made against an old revision must be refused: %v", err)
	}
}

// Criterion 42 (decision 15): two team records sharing a slug both land, the second under
// an alias, and a revision of the aliased record pushes the original wire slug; a pulled
// record colliding with a local user record is not recalled (shadowed). A slug deleted,
// re-created and deleted again yields two tombstones and leaves the first record's
// history intact until its own erasure.
func TestSlugCollisionsAndRepeatedDeletion(t *testing.T) {
	f := newFakeTeam(t)
	a, b, c := newDevice(t, "a"), newDevice(t, "b"), newDevice(t, "c")
	shareFromA(t, f, a, c, "vpn", "from A")
	if _, err := b.ix.CreateMemory(orgRecord("vpn", "from B"), nil, nil, memHuman()); err != nil {
		t.Fatal(err)
	}
	b.drain(f)
	eff := c.pull(f)
	if eff.Aliased != 1 {
		t.Fatalf("the second record lands under an alias: %+v", eff)
	}
	bGID := b.rec("vpn").GlobalID
	aliased, ok := c.byGID(bGID)
	if !ok || aliased.ID == "vpn" || aliased.WireSlug != "vpn" || aliased.Collision != "alias" || !inSync(aliased) {
		t.Fatalf("alias landing: %+v", aliased)
	}
	c.edit(aliased.ID, "C edits B's record")
	c.drain(f)
	if got := f.current[bGID].Record.Slug; got != "vpn" {
		t.Fatalf("the aliased record must push the wire slug, got %q", got)
	}
	// A pulled record colliding with a local user record is shadowed.
	d := newDevice(t, "d")
	if _, err := d.ix.CreateMemory(memRecord("vpn"), nil, nil, memHuman()); err != nil {
		t.Fatal(err)
	}
	if eff := d.pull(f); eff.Shadowed < 1 {
		t.Fatalf("a collision with a user record shadows: %+v", eff)
	}
	// Repeated deletion of one slug: two tombstones, the first history intact.
	e := newDevice(t, "e")
	if err := e.ix.SetLinked(false); err != nil {
		t.Fatal(err)
	}
	first, err := e.ix.CreateMemory(memRecord("once"), nil, nil, memHuman())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ix.DeleteMemory("once", memHuman()); err != nil {
		t.Fatal(err)
	}
	second, err := e.ix.CreateMemory(memRecord("once"), nil, nil, memHuman())
	if err != nil || second.GlobalID == first.GlobalID {
		t.Fatalf("re-create: %v", err)
	}
	if err := e.ix.DeleteMemory("once", memHuman()); err != nil {
		t.Fatal(err)
	}
	var tombs, firstRevs int
	if err := e.ix.db.QueryRow(`SELECT count(*) FROM memory_tombstone WHERE id='once'`).Scan(&tombs); err != nil || tombs != 2 {
		t.Fatalf("two tombstones for one slug, got %d %v", tombs, err)
	}
	if err := e.ix.db.QueryRow(`SELECT count(*) FROM memory_revision WHERE global_id=?`, first.GlobalID).Scan(&firstRevs); err != nil || firstRevs != 1 {
		t.Fatalf("the first record's history (a user record, D7) must be intact: %d %v", firstRevs, err)
	}
}

// The schema-44 migration rebuilds a v42 memory_revision and memory_tombstone in place.
func TestTeamMemoryV44RebuildsOlderTables(t *testing.T) {
	ix := openTestMemoryStore(t)
	for _, q := range []string{
		`DROP TABLE memory_revision`, `DROP TABLE memory_tombstone`,
		`CREATE TABLE memory_revision(revision_id INTEGER PRIMARY KEY, record_id TEXT NOT NULL, revision INTEGER NOT NULL, author TEXT NOT NULL,
			actor_source TEXT NOT NULL, content_hash TEXT NOT NULL, prev_hash TEXT NOT NULL DEFAULT '', changed TEXT NOT NULL DEFAULT '',
			body TEXT NOT NULL, created_at INTEGER NOT NULL, UNIQUE(record_id,revision))`,
		`CREATE TABLE memory_tombstone(id TEXT PRIMARY KEY, deleted_at INTEGER NOT NULL, deleted_by TEXT NOT NULL DEFAULT '')`,
		`INSERT INTO memory_revision(record_id,revision,author,actor_source,content_hash,body,created_at) VALUES('old',1,'u','cli','sha256:x','{"id":"mem_OLD"}',1)`,
		`INSERT INTO memory_tombstone(id,deleted_at) VALUES('gone',1)`,
	} {
		if _, err := ix.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateTeamMemoryV44(ix.db); err != nil {
		t.Fatal(err)
	}
	var gid, tomb string
	if err := ix.db.QueryRow(`SELECT global_id FROM memory_revision WHERE record_id='old'`).Scan(&gid); err != nil || gid != "mem_OLD" {
		t.Fatalf("global id backfilled from the body: %q %v", gid, err)
	}
	if err := ix.db.QueryRow(`SELECT global_id FROM memory_tombstone WHERE id='gone'`).Scan(&tomb); err != nil || tomb != "slug:gone" {
		t.Fatalf("legacy tombstone rekeyed: %q %v", tomb, err)
	}
	if !ix.MemoryTombstoned("gone") {
		t.Fatal("the legacy slug must still read as tombstoned")
	}
	if err := migrateTeamMemoryV44(ix.db); err != nil {
		t.Fatalf("the migration must be idempotent: %v", err)
	}
}

// The content deletion (decision 9): prior hash over the sorted sent body hashes of the
// session's accepted rows; consent withdrawn in the same transaction; nothing sent → refused.
func TestContentTombstoneCommitsToWhatWasSent(t *testing.T) {
	d := newDevice(t, "a")
	if _, err := d.ix.EnqueueContentTombstone("claude/s1", "ses_X", 10); !errors.Is(err, ErrNothingSent) {
		t.Fatalf("nothing sent: %v", err)
	}
	for i, h := range []string{"sha256:bb", "sha256:aa"} {
		if _, err := d.ix.db.Exec(`INSERT INTO sync_outbox(record_kind,global_id,content_hash,scope,enqueued_at,acked_at,ack_code,sent_body_hash)
			VALUES('session_content',?,'d','claude/s1',1,2,'accepted',?)`, "cnt_"+string(rune('A'+i)), h); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.ix.SetContentOptIn("claude/s1", true, 1); err != nil {
		t.Fatal(err)
	}
	if n, err := d.ix.ContentSent("claude/s1"); err != nil || n != 2 {
		t.Fatalf("sent: %d %v", n, err)
	}
	n, err := d.ix.EnqueueContentTombstone("claude/s1", "ses_X", 10)
	if err != nil || n != 2 {
		t.Fatalf("enqueue: %d %v", n, err)
	}
	if opt, _ := d.ix.ContentOptIns(); len(opt) != 0 {
		t.Fatalf("the deletion withdraws consent: %v", opt)
	}
	if left, _ := d.ix.ContentSent("claude/s1"); left != 0 {
		t.Fatalf("covered content no longer offers a deletion, %d left", left)
	}
	rows, _ := d.ix.OutboxBatch(10, nil)
	if len(rows) != 1 {
		t.Fatalf("rows %+v", rows)
	}
	rec, code, err := d.ix.MemoryPushRecord(rows[0], d.dets, "org_test")
	if err != nil || code != "" {
		t.Fatalf("%q %v", code, err)
	}
	var tomb teamwire.Tombstone
	if err := json.Unmarshal(rec.Body, &tomb); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("sha256:aa\nsha256:bb"))
	if tomb.RecordType != teamwire.TombstoneSessionContent || tomb.RecordID != "ses_X" || len(tomb.RequiredProjectionCleanup) != 1 ||
		tomb.RequiredProjectionCleanup[0] != "cache" || tomb.PriorContentHash != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("tombstone: %+v", tomb)
	}
}

// PW-H1 (decision 14): the import door runs unattended, so an imported record is never
// shared by being written — even on a linked device at a scope that can travel — and an
// import never becomes a revision of a team record. It is shared one record at a time.
func TestImportedRecordsAreNeverSharedByBeingWritten(t *testing.T) {
	f := newFakeTeam(t)
	d := newDevice(t, "a")
	imp := memRecord("imported")
	imp.Source, imp.Status = "import", "active"
	imp.ScopeType, imp.ScopeID, imp.RepositoryIdentity = MemoryScopeRepository, "rep_remote", "remote-sha256"
	saved, err := d.ix.UpsertMemory(imp, nil, nil, MemoryActor{AuthorType: "user", AuthorID: "daemon-import", ActorSource: "daemon"})
	if err != nil {
		t.Fatal(err)
	}
	if saved.ShareState != "unshared" || d.pendingRows(saved.GlobalID) != 0 {
		t.Fatalf("an imported record must not be shared or queued: %s, %d rows", saved.ShareState, d.pendingRows(saved.GlobalID))
	}
	saved.Body = "the vendor file changed"
	if _, err := d.ix.UpsertMemory(saved, nil, nil, memHuman()); err != nil {
		t.Fatal(err)
	}
	if n := d.pendingRows(saved.GlobalID); n != 0 || len(d.drain(f)) != 0 {
		t.Fatalf("a re-import must not queue it either: %d", n)
	}
	bulk, individual, err := d.ix.MemoryShareCandidates("org_test")
	if err != nil || len(bulk) != 0 || len(individual) != 1 {
		t.Fatalf("an imported record is an individual candidate only: %d %d %v", len(bulk), len(individual), err)
	}
	if n, err := d.ix.ShareMemory(nil, "org_test"); err != nil || n != 0 {
		t.Fatalf("the bulk action never shares an import: %d %v", n, err)
	}
	if n, err := d.ix.ShareMemory([]string{"imported"}, "org_test"); err != nil || n != 1 {
		t.Fatalf("shared one by one: %d %v", n, err)
	}
	if codes := d.drain(f); len(codes) != 1 || codes[0] != teamwire.StatusAccepted {
		t.Fatalf("then it is sent: %v", codes)
	}
}

// PW-M2 (O-7): no copy of a body outlives its record on a device — the wire body frozen
// on the outbox row is blanked when the row is settled, and a deletion (local or pulled)
// leaves the body in no table.
func TestNoBodySurvivesSettleOrDeletion(t *testing.T) {
	f := newFakeTeam(t)
	a, b := newDevice(t, "a"), newDevice(t, "b")
	gid := shareFromA(t, f, a, b, "vpn", "the-unique-body-marker")
	bodies := func(d *device) int {
		var n int
		for _, q := range []string{
			`SELECT count(*) FROM sync_outbox WHERE sent_wire_body LIKE '%the-unique-body-marker%'`,
			`SELECT count(*) FROM memory_revision WHERE body LIKE '%the-unique-body-marker%'`,
			`SELECT count(*) FROM memory_record WHERE body LIKE '%the-unique-body-marker%' OR held_wire_record LIKE '%the-unique-body-marker%'`,
			`SELECT count(*) FROM search_document WHERE text LIKE '%the-unique-body-marker%'`,
		} {
			var c int
			if err := d.ix.db.QueryRow(q).Scan(&c); err != nil {
				t.Fatal(err)
			}
			n += c
		}
		return n
	}
	var frozen int
	if err := a.ix.db.QueryRow(`SELECT count(*) FROM sync_outbox WHERE global_id=? AND sent_wire_body != ''`, gid).Scan(&frozen); err != nil || frozen != 0 {
		t.Fatalf("a settled row keeps no wire body: %d %v", frozen, err)
	}
	if err := a.ix.DeleteMemory("vpn", memHuman()); err != nil {
		t.Fatal(err)
	}
	a.drain(f)
	b.pull(f)
	if n := bodies(a); n != 0 {
		t.Fatalf("the deleting device still holds the body in %d places", n)
	}
	if n := bodies(b); n != 0 {
		t.Fatalf("the device the deletion reached still holds the body in %d places", n)
	}
}

// PW-M5 (criterion 47, the device half): a deletion the team refuses does not leave this
// device without the record — the local tombstone goes, the pull starts over, and the
// record returns as the team holds it.
func TestARefusedDeletionBringsTheRecordBack(t *testing.T) {
	f := newFakeTeam(t)
	a, b := newDevice(t, "a"), newDevice(t, "b")
	gid := shareFromA(t, f, a, b, "vpn", "v1")
	if err := b.ix.DeleteMemory("vpn", memHuman()); err != nil {
		t.Fatal(err)
	}
	rows, _ := b.ix.OutboxBatch(10, nil)
	eff, err := b.ix.SettleMemoryPush(MemorySettle{Seq: rows[0].Seq, Code: teamwire.CodeDeleteNotAllowed}, 1)
	if err != nil || eff.DeletionsRefused != 1 {
		t.Fatalf("settle: %+v %v", eff, err)
	}
	if cursor, _ := b.ix.SyncCursor("memory"); cursor != 0 {
		t.Fatalf("the pull must start over, cursor %d", cursor)
	}
	b.pull(f)
	r, ok := b.byGID(gid)
	if !ok || r.Body != "v1" || r.Status != "active" {
		t.Fatalf("the record must return: %+v %v", r, ok)
	}
	if sum, _ := b.ix.MemorySyncSummaryCounts("org_test", 0); sum.DeletionsRefused != 1 {
		t.Fatalf("the refusal is counted: %+v", sum)
	}
}

// PW-M7: a pulled row this device's rules refuse is skipped and counted; the rows behind
// it — a deletion included — still land. PW-M3: a record received from a team is never a
// bulk share candidate, and an organization record of another organization is refused
// before any body is built. PW-L6: narrowing to user scope takes a record out of sharing.
func TestUnlandableRowsForeignOrganizationsAndNarrowing(t *testing.T) {
	f := newFakeTeam(t)
	a, b := newDevice(t, "a"), newDevice(t, "b")
	gid := shareFromA(t, f, a, b, "vpn", "v1")
	bad := f.rows[0]
	m := *bad.Memory
	m.Record.ID, m.Record.Title = "mem_01BADBADBADBADBADBADBADBAD", " "
	good := *bad.Memory
	good.Record.ID, good.Record.Slug = "mem_01G00DG00DG00DG00DG00DG00D", "after-the-bad-row"
	eff, err := b.ix.ApplyPulledRows([]teamwire.PullRow{{Seq: 9, Kind: teamwire.KindMemory, Memory: &m}, {Seq: 10, Kind: teamwire.KindMemory, Memory: &good}}, 1, "")
	if err != nil || eff.Unlandable != 1 || len(eff.Landed) != 1 {
		t.Fatalf("the bad row is skipped, the next lands: %+v %v", eff, err)
	}
	// After an unlink the pulled records are unshared; the bulk action must not offer them.
	if err := b.ix.ResetSyncQueue(); err != nil {
		t.Fatal(err)
	}
	bulk, individual, err := b.ix.MemoryShareCandidates("org_test")
	if err != nil || len(bulk) != 0 || len(individual) != 2 {
		t.Fatalf("pulled records are individual candidates only: %d %d %v", len(bulk), len(individual), err)
	}
	if _, err := b.ix.ShareMemory([]string{"vpn"}, "org_test"); err != nil {
		t.Fatal(err)
	}
	rows, _ := b.ix.OutboxBatch(10, nil)
	if _, code, err := b.ix.MemoryPushRecord(rows[0], b.dets, "org_another"); err != nil || code != teamwire.CodeNotShareable {
		t.Fatalf("another organization's record must be refused on the device: %q %v", code, err)
	}
	// Narrowing a team record to user scope detaches a copy (O-11); deleting the copy is
	// local and the team's record stays.
	r := a.rec("vpn")
	r.ScopeType, r.ScopeID = MemoryScopeUser, ""
	narrowed, err := a.ix.NarrowMemoryToUser(r, nil, nil, memHuman())
	if err != nil || narrowed.ID == "vpn" || narrowed.GlobalID == gid || narrowed.ShareState != "unshared" || MemoryIsTeamRecord(narrowed) {
		t.Fatalf("narrowed: %+v %v", narrowed, err)
	}
	if err := a.ix.DeleteMemory(narrowed.ID, memHuman()); err != nil {
		t.Fatal(err)
	}
	if n := a.pendingRows(gid); n != 0 {
		t.Fatalf("deleting the private copy must not send a tombstone, %d rows", n)
	}
	if team := a.rec("vpn"); !MemoryIsTeamRecord(team) || team.Body != "v1" {
		t.Fatalf("the team's record stays: %+v", team)
	}
}

// O-11 (owner ruling 2026-10-03): narrowing a team record to user scope on a device
// DETACHES it. The copy is a new record of the user's own — new global id, new local id,
// revision 1, no sync state, nothing queued; the team's record keeps its id, body, sync
// state and scope on this device and still recalls as the team's. A teammate's later
// revision lands on the team's record and leaves the copy alone; so does a later deletion.
func TestNarrowingATeamRecordDetachesACopy(t *testing.T) {
	f := newFakeTeam(t)
	a, b := newDevice(t, "a"), newDevice(t, "b")
	gid := shareFromA(t, f, a, b, "runbook", "team v1")
	before := b.rec("runbook")

	mine := b.rec("runbook")
	mine.ScopeType, mine.ScopeID, mine.Body = MemoryScopeUser, "", "my own notes on top"
	copyRec, err := b.ix.NarrowMemoryToUser(mine, nil, nil, memHuman())
	if err != nil {
		t.Fatal(err)
	}
	if copyRec.ID == "runbook" || !strings.HasPrefix(copyRec.ID, "runbook-") || copyRec.GlobalID == gid || copyRec.GlobalID == "" {
		t.Fatalf("the copy has its own ids: %+v", copyRec)
	}
	if copyRec.ScopeType != MemoryScopeUser || copyRec.Revision != 1 || copyRec.ShareState != "unshared" || copyRec.SyncOrigin != "local" ||
		copyRec.PushedHash != "" || copyRec.ServerRevision != 0 || copyRec.SyncedProjectionHash != "" || copyRec.TeamAuthor != "" || copyRec.Body != "my own notes on top" {
		t.Fatalf("the copy is the user's own, with no sync state: %+v", copyRec)
	}
	team := b.rec("runbook")
	if team.GlobalID != gid || team.ScopeType != MemoryScopeOrganization || team.Body != "team v1" || team.Revision != before.Revision ||
		team.PushedHash != before.PushedHash || team.ServerRevision != before.ServerRevision || !inSync(team) || !MemoryIsTeamRecord(team) {
		t.Fatalf("the team's record is untouched: %+v", team)
	}
	if n := b.pendingRows(gid) + b.pendingRows(copyRec.GlobalID); n != 0 {
		t.Fatalf("a detach queues nothing for the team, %d rows", n)
	}
	if codes := b.drain(f); len(codes) != 0 {
		t.Fatalf("nothing is sent: %v", codes)
	}
	if f.current[gid].Record.Body != "team v1" {
		t.Fatalf("the team's record is untouched for everyone else: %+v", f.current[gid].Record)
	}

	// A later team revision lands on the team's record; the copy is not overwritten.
	a.edit("runbook", "team v2")
	if codes := a.drain(f); len(codes) != 1 || codes[0] != teamwire.StatusAccepted {
		t.Fatalf("a's revision: %v", codes)
	}
	if eff := b.pull(f); len(eff.Landed) != 1 || eff.Conflicts != 0 {
		t.Fatalf("the revision lands with no conflict: %+v", eff)
	}
	if team := b.rec("runbook"); team.Body != "team v2" || team.Collision != "" {
		t.Fatalf("the team's record took the revision: %+v", team)
	}
	if c := b.rec(copyRec.ID); c.Body != "my own notes on top" || c.Revision != 1 {
		t.Fatalf("the copy is untouched by the team's revision: %+v", c)
	}

	// A later team deletion removes the team's record; the copy stays, with its history.
	if err := a.ix.DeleteMemory("runbook", memHuman()); err != nil {
		t.Fatal(err)
	}
	a.drain(f)
	if eff := b.pull(f); len(eff.Removed) != 1 || eff.Removed[0] != "runbook" || eff.Discarded != 0 {
		t.Fatalf("the deletion removes the team's record only: %+v", eff)
	}
	if _, ok := b.byGID(gid); ok {
		t.Fatal("the team's record is gone")
	}
	if c := b.rec(copyRec.ID); c.Body != "my own notes on top" {
		t.Fatalf("the copy survives the team's deletion: %+v", c)
	}
	var revs int
	if err := b.ix.db.QueryRow(`SELECT count(*) FROM memory_revision WHERE global_id=?`, copyRec.GlobalID).Scan(&revs); err != nil || revs != 1 {
		t.Fatalf("the copy keeps its own history: %d %v", revs, err)
	}

	// A record the team never received is narrowed in place (no detach).
	if err := a.ix.SetLinked(false); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ix.UpsertMemory(orgRecord("local-only", "x"), nil, nil, memHuman()); err != nil {
		t.Fatal(err)
	}
	lo := a.rec("local-only")
	lo.ScopeType, lo.ScopeID = MemoryScopeUser, ""
	if got, err := a.ix.UpsertMemory(lo, nil, nil, memHuman()); err != nil || got.ID != "local-only" || got.Revision != 2 {
		t.Fatalf("a record the team never held narrows in place: %+v %v", got, err)
	}
}

// The schema-44 migration through Open, on a store file as an older binary left it: the
// memory and outbox tables in their old shapes with rows in them. It runs from a v42
// store and from a store ALREADY at main's v43 (understanding retention took 43 while
// item 5 was in review): the step probes the layout and never assumes where it starts.
func TestOpenMigratesAnOlderStoreFile(t *testing.T) {
	for _, from := range []int{42, 43} {
		t.Run(fmt.Sprintf("from v%d", from), func(t *testing.T) { openMigratesOlderStoreFile(t, from) })
	}
}

func openMigratesOlderStoreFile(t *testing.T, from int) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ix.SetLinked(true); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"keep", "gone"} {
		r := memRecord(id)
		r.ScopeType, r.ScopeID, r.RepositoryIdentity = MemoryScopeRepository, "proj", "weak"
		if _, err := ix.CreateMemory(r, nil, nil, memHuman()); err != nil {
			t.Fatal(err)
		}
	}
	keep, _ := ix.MemoryByID("keep")
	// Rewind the memory and outbox tables to the layout before item 5.
	var drops []string
	for _, c := range []string{"share_state", "sync_origin", "pushed_hash", "server_revision", "synced_projection_hash", "held_wire_record", "held_server_revision", "wire_slug", "collision", "team_author", "identity_note", "detached_from"} {
		drops = append(drops, `ALTER TABLE memory_record DROP COLUMN `+c)
	}
	for _, c := range []string{"revision", "ack_code", "sent_body_hash", "sent_wire_body", "sent_wire_hash"} {
		drops = append(drops, `ALTER TABLE sync_outbox DROP COLUMN `+c)
	}
	for _, q := range append([]string{
		`DROP INDEX IF EXISTS sync_outbox_record`, `DROP INDEX IF EXISTS sync_outbox_kind_scope`, `DROP INDEX IF EXISTS memory_tombstone_slug`, `DROP TABLE memory_conflict`,
		`CREATE TABLE memory_revision_old(revision_id INTEGER PRIMARY KEY, record_id TEXT NOT NULL, revision INTEGER NOT NULL, author TEXT NOT NULL,
			actor_source TEXT NOT NULL, content_hash TEXT NOT NULL, prev_hash TEXT NOT NULL DEFAULT '', changed TEXT NOT NULL DEFAULT '',
			body TEXT NOT NULL, created_at INTEGER NOT NULL, UNIQUE(record_id,revision))`,
		`INSERT INTO memory_revision_old SELECT revision_id, record_id, revision, author, actor_source, content_hash, prev_hash, changed, body, created_at FROM memory_revision`,
		`DROP TABLE memory_revision`, `ALTER TABLE memory_revision_old RENAME TO memory_revision`,
		`DROP TABLE memory_tombstone`,
		`CREATE TABLE memory_tombstone(id TEXT PRIMARY KEY, deleted_at INTEGER NOT NULL, deleted_by TEXT NOT NULL DEFAULT '')`,
		`INSERT INTO memory_tombstone(id, deleted_at) VALUES('deleted-before', 1)`,
		`INSERT INTO sync_outbox(record_kind,global_id,content_hash,scope,enqueued_at) VALUES('memory','` + keep.GlobalID + `','h','repository:proj',1)`,
	}, drops...) {
		if _, err := ix.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := ix.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, from)); err != nil {
		t.Fatal(err)
	}
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}
	ix, err = Open(path)
	if err != nil {
		t.Fatalf("opening a v%d store: %v", from, err)
	}
	defer ix.Close()
	var version, revs int
	_ = ix.db.QueryRow(`PRAGMA user_version`).Scan(&version)
	if err := ix.db.QueryRow(`SELECT count(*) FROM memory_revision WHERE global_id=? AND revision=1`, keep.GlobalID).Scan(&revs); err != nil || revs != 1 || version != SchemaVersion {
		t.Fatalf("revisions re-keyed by global id at v%d: %d %v", version, revs, err)
	}
	got, err := ix.MemoryByID("keep")
	if err != nil || got.ShareState != "unshared" || got.SyncOrigin != "local" || !ix.MemoryTombstoned("deleted-before") {
		t.Fatalf("existing records are unshared and legacy tombstones kept: %+v %v", got, err)
	}
	rows, err := ix.OutboxBatch(10, nil)
	if err != nil || len(rows) != 1 || rows[0].Revision != 0 {
		t.Fatalf("the backlog row names no revision: %+v %v", rows, err)
	}
	if _, code, _ := ix.MemoryPushRecord(rows[0], nil, "org"); code != teamwire.CodeNotShareable {
		t.Fatalf("and is refused not_shareable: %q", code)
	}
	if _, err := ix.UpsertMemory(got, nil, nil, memHuman()); err != nil {
		t.Fatalf("a write after the migration: %v", err)
	}
}

// CR-1: an acknowledgement belongs to the link it was made under. After an unlink, what
// an earlier team server accepted no longer counts — "delete what was sent" offers
// nothing and queues no deletion for it, a deletion made under that link is not asked
// about — and a relink to the SAME organization (the server keeps the device's row and
// its content) takes its own acknowledgements back.
func TestAcknowledgementsBelongToTheirLink(t *testing.T) {
	d := newDevice(t, "a")
	if err := d.ix.SetLinkedOrganization("org_A", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ix.db.Exec(`INSERT INTO sync_outbox(record_kind,global_id,content_hash,scope,enqueued_at,acked_at,ack_code,sent_body_hash)
		VALUES('session_content','cnt_A','d','claude/s1',1,2,'accepted','sha256:aa')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ix.db.Exec(`INSERT INTO sync_outbox(record_kind,global_id,content_hash,scope,enqueued_at,acked_at,ack_code)
		VALUES('memory','mem_X','d','organization:org_A',1,2,'not_shareable'), ('tombstone','mem_Y','d','memory',1,2,'accepted')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ix.db.Exec(`INSERT INTO memory_tombstone(global_id,id,deleted_at,origin) VALUES('mem_Y','gone',1,'local')`); err != nil {
		t.Fatal(err)
	}
	if n, _ := d.ix.ContentSent("claude/s1"); n != 1 {
		t.Fatalf("while linked the accepted chunk counts: %d", n)
	}
	// unlink
	if err := d.ix.SetLinked(false); err != nil {
		t.Fatal(err)
	}
	if err := d.ix.ResetSyncQueue(); err != nil {
		t.Fatal(err)
	}
	// link to ANOTHER organization
	if err := d.ix.SetLinked(true); err != nil {
		t.Fatal(err)
	}
	if err := d.ix.SetLinkedOrganization("org_B", 3); err != nil {
		t.Fatal(err)
	}
	if n, _ := d.ix.ContentSent("claude/s1"); n != 0 {
		t.Fatalf("another server holds none of it: the footer must offer nothing, got %d", n)
	}
	if _, err := d.ix.EnqueueContentTombstone("claude/s1", "ses_X", 10); !errors.Is(err, ErrNothingSent) {
		t.Fatalf("no deletion is queued for the old link's content: %v", err)
	}
	if n, _ := d.ix.OutboxPending(); n != 0 {
		t.Fatalf("no tombstone may be queued, %d pending", n)
	}
	sent, err := d.ix.MemoryDeletionsSent(10)
	sum, serr := d.ix.MemorySyncSummaryCounts("org_B", 0)
	if err != nil || serr != nil || len(sent) != 0 || sum.NotShareable != 0 {
		t.Fatalf("the old link's deletions and refusals are not this link's: %+v %+v %v %v", sent, sum, err, serr)
	}
	// unlink again and relink to the FIRST organization: its acknowledgements are its own.
	_ = d.ix.SetLinked(false)
	if err := d.ix.ResetSyncQueue(); err != nil {
		t.Fatal(err)
	}
	_ = d.ix.SetLinked(true)
	if err := d.ix.SetLinkedOrganization("org_A", 5); err != nil {
		t.Fatal(err)
	}
	if n, _ := d.ix.ContentSent("claude/s1"); n != 1 {
		t.Fatalf("the same server still holds it: %d", n)
	}
	if sent, _ := d.ix.MemoryDeletionsSent(10); len(sent) != 1 {
		t.Fatalf("and the deletion made under it: %+v", sent)
	}
}

// CR-2: the pull cursor advances by compare-and-set from the value the tick read, so a
// reset made meanwhile is never overwritten. CR-10: a page lands only while the device is
// linked to the organization it was pulled from, checked inside the landing transaction.
func TestCursorCompareAndSetAndLinkGuard(t *testing.T) {
	f := newFakeTeam(t)
	a, b := newDevice(t, "a"), newDevice(t, "b")
	shareFromA(t, f, a, b, "vpn", "v1")
	if moved, err := b.ix.AdvanceSyncCursor("memory:org", 0, 7, 0, 1); err != nil || !moved {
		t.Fatalf("first advance: %v %v", moved, err)
	}
	// a tick read 7; meanwhile the cursor is reset (a deletion the team did not take)
	if err := b.ix.ClearSyncCursor("memory:org"); err != nil {
		t.Fatal(err)
	}
	if moved, err := b.ix.AdvanceSyncCursor("memory:org", 7, 9, 0, 2); err != nil || moved {
		t.Fatalf("a tick in flight must not put the old position back: %v %v", moved, err)
	}
	if c, _ := b.ix.SyncCursor("memory:org"); c != 0 {
		t.Fatalf("the reset stands: %d", c)
	}
	// FR-3: a tick that began at the BEGINNING (cursor 0, before the reset) reads the same
	// cursor value after it; the reset epoch tells them apart.
	epoch, err := b.ix.MemoryPullEpoch()
	if err != nil || epoch == 0 {
		t.Fatalf("a reset moves the epoch: %d %v", epoch, err)
	}
	if moved, err := b.ix.AdvanceSyncCursor("memory:org", 0, 5, 0, 3); err != nil || moved {
		t.Fatalf("a tick that started from 0 before the reset must not record its position: %v %v", moved, err)
	}
	if moved, err := b.ix.AdvanceSyncCursor("memory:org", 0, 5, epoch, 3); err != nil || !moved {
		t.Fatalf("a tick that started after the reset advances: %v %v", moved, err)
	}
	if err := b.ix.ClearSyncCursor("memory:org"); err != nil {
		t.Fatal(err)
	}
	if err := b.ix.SetLinkedOrganization("org_test", 1); err != nil {
		t.Fatal(err)
	}
	a.edit("vpn", "v2")
	a.drain(f)
	rows := f.pullAfter(1)
	if _, err := b.ix.ApplyPulledRows(rows, 1, "org_other"); !errors.Is(err, ErrLinkChanged) {
		t.Fatalf("a page from another organization's link must not land: %v", err)
	}
	if b.rec("vpn").Body != "v1" {
		t.Fatal("nothing may have landed")
	}
	if eff, err := b.ix.ApplyPulledRows(rows, 1, "org_test"); err != nil || len(eff.Landed) != 1 {
		t.Fatalf("under its own link the page lands: %+v %v", eff, err)
	}
}

// CR-3: one predicate. A deletion the team never received — dead-lettered after the
// server kept failing to store it — is recovered exactly as a refused one is, and the
// count is the same predicate. CR-5: a record the team never held (a shared draft whose
// first push was never frozen) is deleted locally: history kept, no tombstone.
func TestUndeliveredDeletionsRecoverAndDraftsStayLocal(t *testing.T) {
	f := newFakeTeam(t)
	a, b := newDevice(t, "a"), newDevice(t, "b")
	gid := shareFromA(t, f, a, b, "vpn", "v1")
	if err := b.ix.DeleteMemory("vpn", memHuman()); err != nil {
		t.Fatal(err)
	}
	rows, _ := b.ix.OutboxBatch(10, nil)
	eff, err := b.ix.SettleMemoryPush(MemorySettle{Seq: rows[0].Seq, Code: "internal_exhausted"}, 1)
	if err != nil || eff.DeletionsRefused != 1 {
		t.Fatalf("an undelivered deletion is recovered: %+v %v", eff, err)
	}
	b.pull(f)
	if _, ok := b.byGID(gid); !ok {
		t.Fatal("the record the team still holds must return")
	}
	if sum, _ := b.ix.MemorySyncSummaryCounts("org_test", 0); sum.DeletionsRefused != 1 {
		t.Fatalf("counted by the same predicate: %+v", sum)
	}
	for _, c := range []string{teamwire.StatusAccepted, teamwire.StatusDuplicate, teamwire.CodeSuperseded} {
		if !MemoryDeletionAccepted(c) {
			t.Fatalf("%s is an accepted deletion", c)
		}
	}
	for _, c := range []string{teamwire.CodeDeleteNotAllowed, "internal_exhausted", "over_push_limit", "encode_failed", teamwire.CodeSourceMissing} {
		if MemoryDeletionAccepted(c) {
			t.Fatalf("%s is not", c)
		}
	}
	// A draft created shared on a linked device, never sent: deleting it tells no one.
	draft := orgRecord("draft", "never sent")
	draft.Status = "pending"
	saved, err := a.ix.CreateMemory(draft, nil, nil, memHuman())
	if err != nil || saved.ShareState != "shared" || MemoryIsTeamRecord(saved) {
		t.Fatalf("a shared draft is not the team's until a revision is accepted: %+v %v", saved, err)
	}
	done, err := a.ix.DeleteMemoryReport("draft", memHuman())
	if err != nil || done.Team || done.Queued {
		t.Fatalf("deleting it is local: %+v %v", done, err)
	}
	var revs, tombs int
	_ = a.ix.db.QueryRow(`SELECT count(*) FROM memory_revision WHERE global_id=?`, saved.GlobalID).Scan(&revs)
	_ = a.ix.db.QueryRow(`SELECT count(*) FROM sync_outbox WHERE global_id=? AND record_kind='tombstone'`, saved.GlobalID).Scan(&tombs)
	if revs != 1 || tombs != 0 {
		t.Fatalf("history kept (D7), no tombstone: %d revisions, %d tombstones", revs, tombs)
	}
	// An active record whose first revision was frozen for sending may be on the server:
	// that deletion does propagate.
	if _, err := a.ix.CreateMemory(orgRecord("sent-unanswered", "frozen"), nil, nil, memHuman()); err != nil {
		t.Fatal(err)
	}
	rows, _ = a.ix.OutboxBatch(10, nil)
	if _, _, err := a.ix.MemoryPushRecord(rows[0], a.dets, "org_test"); err != nil {
		t.Fatal(err)
	}
	done, err = a.ix.DeleteMemoryReport("sent-unanswered", memHuman())
	if err != nil || !done.Team || !done.Queued {
		t.Fatalf("a revision that may have left makes it the team's to delete: %+v %v", done, err)
	}
}

// PW-M2, reaching the erasure with a body still frozen: a row frozen for sending and not
// yet answered is blanked by a local deletion, by a pulled deletion, and by unlink — the
// three call sites the settle path never reaches.
func TestFrozenBodiesAreErasedBeforeTheyAreSettled(t *testing.T) {
	frozen := func(d *device, gid string) int {
		var n int
		if err := d.ix.db.QueryRow(`SELECT count(*) FROM sync_outbox WHERE global_id=? AND record_kind='memory' AND sent_wire_body != ''`, gid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	freeze := func(d *device) {
		rows, _ := d.ix.OutboxBatch(10, nil)
		if _, code, err := d.ix.MemoryPushRecord(rows[0], d.dets, "org_test"); err != nil || code != "" {
			t.Fatalf("freeze: %q %v", code, err)
		}
	}
	for _, how := range []string{"local delete", "pulled deletion", "unlink"} {
		t.Run(how, func(t *testing.T) {
			f := newFakeTeam(t)
			a, b := newDevice(t, "a"), newDevice(t, "b")
			gid := shareFromA(t, f, a, b, "vpn", "v1")
			b.edit("vpn", "frozen-and-unanswered")
			freeze(b)
			if frozen(b, gid) != 1 {
				t.Fatal("the precondition: a non-blank frozen body")
			}
			switch how {
			case "local delete":
				if err := b.ix.DeleteMemory("vpn", memHuman()); err != nil {
					t.Fatal(err)
				}
			case "pulled deletion":
				if err := a.ix.DeleteMemory("vpn", memHuman()); err != nil {
					t.Fatal(err)
				}
				a.drain(f)
				b.pull(f)
			case "unlink":
				if err := b.ix.ResetSyncQueue(); err != nil {
					t.Fatal(err)
				}
			}
			if n := frozen(b, gid); n != 0 {
				t.Fatalf("%s left %d frozen bodies", how, n)
			}
		})
	}
}

// PW-L4: a stale_base answer's carried revision lands even when its number is no higher
// than the device's (the device was accepted at a number the server later reused for
// another revision is impossible, but a device restored from a backup can be ahead): the
// test is the hash. CR-4: a carried revision this build cannot land still moves the base.
func TestCarriedRevisionLandsByHashAndAnUnlandableOneMovesTheBase(t *testing.T) {
	f := newFakeTeam(t)
	a, b := newDevice(t, "a"), newDevice(t, "b")
	gid := shareFromA(t, f, a, b, "vpn", "v1")
	if _, err := b.ix.db.Exec(`UPDATE memory_record SET server_revision=9 WHERE global_id=?`, gid); err != nil {
		t.Fatal(err)
	}
	a.edit("vpn", "the team's current")
	a.drain(f)
	b.edit("vpn", "B's edit")
	if codes := b.drain(f); len(codes) != 1 || codes[0] != teamwire.CodeStaleBase {
		t.Fatalf("%v", codes)
	}
	if r := b.rec("vpn"); r.Body != "the team's current" || !inSync(r) {
		t.Fatalf("the carried revision lands by hash though its number (2) is below the device's (9): %q", r.Body)
	}
	// An unlandable carried revision: the base moves to it; the local content stays.
	b.edit("vpn", "B again")
	rows, _ := b.ix.OutboxBatch(10, nil)
	if _, _, err := b.ix.MemoryPushRecord(rows[0], b.dets, "org_test"); err != nil {
		t.Fatal(err)
	}
	cur := f.current[gid]
	cur.Record.Category, cur.WireHash, cur.ServerRevision = "a-category-a-newer-build-knows", "sha256:newer", 12
	eff, err := b.ix.SettleMemoryPush(MemorySettle{Seq: rows[0].Seq, Code: teamwire.CodeStaleBase, Current: &cur}, 1)
	if err != nil || eff.Unlandable != 1 {
		t.Fatalf("%+v %v", eff, err)
	}
	if r := b.rec("vpn"); r.PushedHash != "sha256:newer" || r.ServerRevision != 12 || r.Body != "B again" || inSync(r) {
		t.Fatalf("the base moves, the content stays diverged: %+v", r)
	}
}

// PW-L10: one runtime's bare native id is never read as another runtime's session.
func TestContentOfOneRuntimeIsNotAnothers(t *testing.T) {
	d := newDevice(t, "a")
	tx, err := d.ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.AppendEvent(EventRecord{TS: 1, SessionID: "same-id", Runtime: "codex", Verb: "exec", Tool: "Bash", Decision: "allow", Origin: "live", Tags: "[]"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ix.db.Exec(`INSERT INTO sync_outbox(record_kind,global_id,content_hash,scope,enqueued_at,acked_at,ack_code,sent_body_hash)
		VALUES('session_content','cnt_1','d','same-id',1,2,'accepted','sha256:aa')`); err != nil {
		t.Fatal(err)
	}
	if n, _ := d.ix.ContentSent("codex/same-id"); n != 1 {
		t.Fatalf("the runtime whose events carry the bare id owns the content: %d", n)
	}
	if n, _ := d.ix.ContentSent("claude/same-id"); n != 0 {
		t.Fatalf("another runtime with the same native id must not: %d", n)
	}
	if _, err := d.ix.EnqueueContentTombstone("claude/same-id", "ses_X", 5); !errors.Is(err, ErrNothingSent) {
		t.Fatalf("and cannot delete it: %v", err)
	}
}

// CR-8: an organization record written under another organization's link is no share
// candidate, bulk or individual. CR-7: the not_shareable count is bounded by a window.
func TestShareCandidatesKnowTheOrganizationAndRefusalsAgeOut(t *testing.T) {
	d := newDevice(t, "a")
	if err := d.ix.SetLinked(false); err != nil {
		t.Fatal(err)
	}
	for id, org := range map[string]string{"ours": "org_test", "theirs": "org_earlier"} {
		r := orgRecord(id, "x")
		r.ScopeID = org
		if _, err := d.ix.CreateMemory(r, nil, nil, memHuman()); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.ix.SetLinked(true); err != nil {
		t.Fatal(err)
	}
	bulk, individual, err := d.ix.MemoryShareCandidates("org_test")
	if err != nil || len(bulk) != 1 || bulk[0].ID != "ours" || len(individual) != 0 {
		t.Fatalf("only this organization's record is a candidate: %+v %+v %v", bulk, individual, err)
	}
	if _, err := d.ix.ShareMemory([]string{"theirs"}, "org_test"); !errors.Is(err, ErrMemoryNotShareable) {
		t.Fatalf("another organization's record cannot be shared here: %v", err)
	}
	if n, err := d.ix.ShareMemory(nil, "org_test"); err != nil || n != 1 {
		t.Fatalf("bulk shares the one: %d %v", n, err)
	}
	if sum, _ := d.ix.MemorySyncSummaryCounts("org_test", 0); sum.ShareCandidates != 0 {
		t.Fatalf("and the count agrees: %+v", sum)
	}
	if _, err := d.ix.db.Exec(`INSERT INTO sync_outbox(record_kind,global_id,content_hash,scope,enqueued_at,acked_at,ack_code)
		VALUES('memory','m1','d','s',1,100,'not_shareable'), ('memory','m2','d','s',1,900,'not_shareable')`); err != nil {
		t.Fatal(err)
	}
	if sum, _ := d.ix.MemorySyncSummaryCounts("org_test", 500); sum.NotShareable != 1 {
		t.Fatalf("a refusal older than the window leaves the count: %+v", sum)
	}
}

// FR-1: only an EXPLICIT narrowing detaches. A write at user scope that merely omitted
// its scope — `memory upsert --id x` with no --scope, an import of a mirror file — updates
// the team's record where it is, at its stored scope, and is sent as its next revision.
// A repeat narrowing revises the copy already detached; it never mints another.
func TestOnlyAnExplicitNarrowingDetachesAndItIsIdempotent(t *testing.T) {
	f := newFakeTeam(t)
	a, b := newDevice(t, "a"), newDevice(t, "b")
	gid := shareFromA(t, f, a, b, "runbook", "team v1")

	// The documented "reuse = update in place": user scope by default, nothing explicit.
	r := memRecord("runbook")
	r.Body, r.Status = "updated in place", "active"
	if r.ScopeType != MemoryScopeUser {
		t.Fatalf("the fixture must be scope-less: %q", r.ScopeType)
	}
	saved, err := b.ix.UpsertMemory(r, nil, nil, memHuman())
	if err != nil || saved.ID != "runbook" || saved.GlobalID != gid || saved.ScopeType != MemoryScopeOrganization || saved.ScopeID != "org_test" || saved.Body != "updated in place" {
		t.Fatalf("a scope-less write updates the team record at its stored scope: %+v %v", saved, err)
	}
	if codes := b.drain(f); len(codes) != 1 || codes[0] != teamwire.StatusAccepted || f.current[gid].Record.Body != "updated in place" {
		t.Fatalf("and reaches the team as its next revision: %v %q", codes, f.current[gid].Record.Body)
	}
	var copies int
	_ = b.ix.db.QueryRow(`SELECT count(*) FROM memory_record WHERE id != 'runbook'`).Scan(&copies)
	if copies != 0 {
		t.Fatalf("no copy is made by a scope-less write: %d", copies)
	}

	// Narrowing in so many words detaches; a second and third narrowing revise that copy.
	var ids []string
	for i, body := range []string{"mine 1", "mine 2", "mine 3"} {
		n := b.rec("runbook")
		n.Body = body
		c, err := b.ix.NarrowMemoryToUser(n, nil, nil, memHuman())
		if err != nil || c.ScopeType != MemoryScopeUser || c.Body != body || c.Revision != int64(i+1) || c.GlobalID == gid {
			t.Fatalf("narrowing %d: %+v %v", i+1, c, err)
		}
		ids = append(ids, c.ID)
	}
	if ids[0] == "runbook" || ids[1] != ids[0] || ids[2] != ids[0] {
		t.Fatalf("every narrowing lands on the one detached copy: %v", ids)
	}
	_ = b.ix.db.QueryRow(`SELECT count(*) FROM memory_record WHERE id != 'runbook'`).Scan(&copies)
	if team := b.rec("runbook"); copies != 1 || team.Body != "updated in place" || team.ScopeType != MemoryScopeOrganization || b.pendingRows(gid) != 0 {
		t.Fatalf("one copy, the team's record untouched and nothing queued: %d %+v", copies, team)
	}
	// Narrowing the COPY (already the user's own) is an ordinary write of it.
	own := b.rec(ids[0])
	own.Body = "mine 4"
	if c, err := b.ix.NarrowMemoryToUser(own, nil, nil, memHuman()); err != nil || c.ID != ids[0] || c.Revision != 4 {
		t.Fatalf("writing the copy itself: %+v %v", c, err)
	}
}

// FR-2: a record that returns after a deletion the team did not take never re-uses a
// push id this device already had accepted. Its local revision starts above every
// revision this device ever queued for that global id, so the next edit is a new push id
// and the team answers accepted — never conflict (which the server records as tampering).
func TestAReturnedRecordNeverReusesAPushID(t *testing.T) {
	f := newFakeTeam(t)
	a, b := newDevice(t, "a"), newDevice(t, "b")
	gid := shareFromA(t, f, a, b, "runbook", "v1")
	b.edit("runbook", "b's edit") // local revision 2: push id gid@2, accepted
	if codes := b.drain(f); len(codes) != 1 || codes[0] != teamwire.StatusAccepted {
		t.Fatalf("b's edit: %v", codes)
	}
	f.refuseDeletes = true
	if err := b.ix.DeleteMemory("runbook", memHuman()); err != nil {
		t.Fatal(err)
	}
	if codes := b.drain(f); len(codes) != 1 || codes[0] != teamwire.CodeDeleteNotAllowed {
		t.Fatalf("the deletion is refused: %v", codes)
	}
	if eff := b.pull(f); len(eff.Landed) != 1 {
		t.Fatalf("the record returns: %+v", eff)
	}
	back := b.rec("runbook")
	if back.GlobalID != gid || back.Revision <= 2 || back.Body != "b's edit" {
		t.Fatalf("it returns above every revision this device queued: %+v", back)
	}
	edited := b.edit("runbook", "edited after it came back")
	codes := b.drain(f)
	if len(codes) != 1 || codes[0] != teamwire.StatusAccepted {
		t.Fatalf("the edit after the return must be accepted, never a conflict: %v (local revision %d)", codes, edited.Revision)
	}
	if f.current[gid].Record.Body != "edited after it came back" {
		t.Fatalf("the team has the edit: %q", f.current[gid].Record.Body)
	}
}

// FR-4: "the team holds it or may" counts only what the CURRENT link took or has not yet
// answered. A revision the server refused was never the team's: the record narrows in
// place and its deletion sends nothing. After an unlink a pulled record is local.
func TestRefusedAndEndedLinkRecordsAreNotTheTeams(t *testing.T) {
	f := newFakeTeam(t)
	a, b := newDevice(t, "a"), newDevice(t, "b")
	gid := shareFromA(t, f, a, b, "vpn", "v1")

	f.refuseMemory = map[string]string{"b": teamwire.CodeOrganizationScopeAdmin}
	mine, err := b.ix.CreateMemory(orgRecord("member-org", "a member wrote this at organization scope"), nil, nil, memHuman())
	if err != nil {
		t.Fatal(err)
	}
	if codes := b.drain(f); len(codes) != 1 || codes[0] != teamwire.CodeOrganizationScopeAdmin {
		t.Fatalf("refused: %v", codes)
	}
	if code, _ := b.ix.MemoryRefusal(mine.GlobalID); code != teamwire.CodeOrganizationScopeAdmin {
		t.Fatalf("the record knows its last answer: %q", code)
	}
	n := b.rec("member-org")
	n.Body = "now private"
	got, err := b.ix.NarrowMemoryToUser(n, nil, nil, memHuman())
	if err != nil || got.ID != "member-org" || got.GlobalID != mine.GlobalID || got.ScopeType != MemoryScopeUser {
		t.Fatalf("a record the team refused narrows in place: %+v %v", got, err)
	}
	second, err := b.ix.CreateMemory(orgRecord("member-org-2", "x"), nil, nil, memHuman())
	if err != nil {
		t.Fatal(err)
	}
	b.drain(f)
	done, err := b.ix.DeleteMemoryReport("member-org-2", memHuman())
	if err != nil || done.Team || done.Queued || b.pendingRows(second.GlobalID) != 0 {
		t.Fatalf("deleting a record the team refused tells the team nothing: %+v %v", done, err)
	}

	// After an unlink, a record pulled from the old team is a local record.
	if err := b.ix.ResetSyncQueue(); err != nil {
		t.Fatal(err)
	}
	if err := b.ix.SetLinked(false); err != nil {
		t.Fatal(err)
	}
	p := b.rec("vpn")
	p.Body = "kept for myself"
	kept, err := b.ix.NarrowMemoryToUser(p, nil, nil, memHuman())
	if err != nil || kept.ID != "vpn" || kept.GlobalID != gid || kept.ScopeType != MemoryScopeUser {
		t.Fatalf("after an unlink a pulled record narrows in place: %+v %v", kept, err)
	}
}

// The count behind a handoff brief's closing line is of the TEAM's records, and a device
// with no link has no team: while linked, the sender's accepted record and the
// receiver's pulled copy each count once; after an unlink neither does — not the pulled
// copy alone, which is what stays marked as received.
func TestTeamRepositoryMemoryCountNeedsALink(t *testing.T) {
	const repository = "repo_0123456789abcdef"
	f := newFakeTeam(t)
	a, b := newDevice(t, "a"), newDevice(t, "b")
	shared := memRecord("runbook")
	shared.Body, shared.Status = "how the drain is run", "active"
	shared.ScopeType, shared.ScopeID, shared.RepositoryIdentity = MemoryScopeRepository, repository, "remote-sha256"
	created, err := a.ix.CreateMemory(shared, nil, nil, memHuman())
	if err != nil {
		t.Fatal(err)
	}
	if created.ShareState != "shared" {
		t.Fatalf("a record created at a repository's team scope on a linked device is shared, got %q", created.ShareState)
	}
	if codes := a.drain(f); len(codes) != 1 || codes[0] != teamwire.StatusAccepted {
		t.Fatalf("the record is accepted: %v", codes)
	}
	b.pull(f)
	count := func(d *device) int {
		t.Helper()
		n, err := d.ix.CountTeamRepositoryMemory(repository)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count(a) != 1 || count(b) != 1 {
		t.Fatalf("linked: the sender counts %d and the receiver %d, want 1 and 1", count(a), count(b))
	}
	for _, d := range []*device{a, b} {
		if err := d.ix.ResetSyncQueue(); err != nil {
			t.Fatal(err)
		}
		if err := d.ix.SetLinked(false); err != nil {
			t.Fatal(err)
		}
	}
	if got, ok := b.byGID(created.GlobalID); !ok || got.SyncOrigin != "pulled" || got.Status != "active" {
		t.Fatalf("an unlink leaves the received record on the device: %+v %v", got, ok)
	}
	if count(a) != 0 || count(b) != 0 {
		t.Fatalf("unlinked: the sender counts %d and the receiver %d, want 0 and 0", count(a), count(b))
	}
}

// FR-5: the freeze takes only a row still waiting. A deletion that acknowledged the row
// between the drain's read and the freeze leaves nothing frozen and nothing to send.
func TestTheFreezeNeverTakesAnAcknowledgedRow(t *testing.T) {
	a := newDevice(t, "a")
	r, err := a.ix.CreateMemory(orgRecord("draft", "never sent"), nil, nil, memHuman())
	if err != nil {
		t.Fatal(err)
	}
	rows, err := a.ix.OutboxBatch(10, nil)
	if err != nil || len(rows) != 1 {
		t.Fatalf("one queued row: %d %v", len(rows), err)
	}
	// Another process acknowledges the row (as a deletion of the never-sent draft does)
	// after the drain read the record as shareable and before it froze the body.
	if _, err := a.ix.db.Exec(`UPDATE sync_outbox SET acked_at=1, ack_code=? WHERE seq=?`, teamwire.CodeSuperseded, rows[0].Seq); err != nil {
		t.Fatal(err)
	}
	rec, code, err := a.ix.MemoryPushRecord(rows[0], a.dets, "org_test")
	if err != nil || code != teamwire.CodeSuperseded || len(rec.Body) != 0 {
		t.Fatalf("an acknowledged row is not encoded for sending: %q %v body=%d", code, err, len(rec.Body))
	}
	var body, hash string
	_ = a.ix.db.QueryRow(`SELECT sent_wire_body, sent_wire_hash FROM sync_outbox WHERE seq=?`, rows[0].Seq).Scan(&body, &hash)
	if body != "" || hash != "" {
		t.Fatalf("nothing was frozen on it: %q %q (record %s)", body, hash, r.GlobalID)
	}
}

// FR-6: a record whose edit the team refused says so, and "take the team's version" gives
// the edit up: the team's current revision lands with the next pull and the local body is
// kept as a conflict copy. Nothing is sent.
func TestARefusedEditIsSaidOnTheRecordAndTheTeamsVersionCanBeTaken(t *testing.T) {
	f := newFakeTeam(t)
	a, b := newDevice(t, "a"), newDevice(t, "b")
	gid := shareFromA(t, f, a, b, "policy", "the team's text")
	if code, _ := b.ix.MemoryRefusal(gid); code != "" {
		t.Fatalf("no refusal yet: %q", code)
	}
	f.refuseMemory = map[string]string{"b": teamwire.CodeOrganizationScopeAdmin}
	b.edit("policy", "a member's edit")
	if err := b.ix.TakeTeamVersion("policy"); !errors.Is(err, ErrMemoryInFlight) {
		t.Fatalf("not while the edit is still being sent: %v", err)
	}
	if codes := b.drain(f); len(codes) != 1 || codes[0] != teamwire.CodeOrganizationScopeAdmin {
		t.Fatalf("refused: %v", codes)
	}
	refusals, err := b.ix.MemoryRefusals()
	if err != nil || refusals[gid] != teamwire.CodeOrganizationScopeAdmin || len(refusals) != 1 {
		t.Fatalf("the record carries the refusal: %v %v", refusals, err)
	}
	if r := b.rec("policy"); r.Body != "a member's edit" || inSync(r) {
		t.Fatalf("the edit stays local and diverged: %+v", r)
	}
	if err := b.ix.TakeTeamVersion("policy"); err != nil {
		t.Fatal(err)
	}
	if n := b.pendingRows(gid); n != 0 {
		t.Fatalf("taking the team's version sends nothing: %d", n)
	}
	if eff := b.pull(f); len(eff.Landed) != 1 || eff.Conflicts != 1 {
		t.Fatalf("the team's revision lands over the edit, which is kept: %+v", eff)
	}
	r := b.rec("policy")
	c := b.conflicts(gid)
	if r.Body != "the team's text" || !inSync(r) || len(c) != 1 || c[0].Reason != "pulled_over_edit" {
		t.Fatalf("the team's version is back and the edit is a conflict copy: %+v %+v", r, c)
	}
	if err := b.ix.TakeTeamVersion("no-such-record"); !errors.Is(err, ErrMemoryNotFound) {
		t.Fatalf("an unknown id: %v", err)
	}
	own, _ := b.ix.CreateMemory(memRecord("own-note"), nil, nil, memHuman())
	if err := b.ix.TakeTeamVersion(own.ID); !errors.Is(err, ErrMemoryNotTeam) {
		t.Fatalf("a record the team does not hold: %v", err)
	}
}

// FR-1's column on a store the MERGED build already brought to schema 44: detached_from
// was added to the v44 step after it merged, so a store at 44 without the column gains it
// on its next open, and a narrowing then works.
func TestAStoreAlreadyAt44GainsTheDetachedFromColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ix.db.Exec(`ALTER TABLE memory_record DROP COLUMN detached_from`); err != nil {
		t.Fatal(err)
	}
	var version int
	_ = ix.db.QueryRow(`PRAGMA user_version`).Scan(&version)
	if version != SchemaVersion {
		t.Fatalf("the store is at %d", version)
	}
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}
	if ix, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	cols, err := columnSet(ix.db, "memory_record")
	if err != nil || !cols["detached_from"] {
		t.Fatalf("the column is added on open: %v %v", cols["detached_from"], err)
	}
}

// Journey C-6: the device that only PULLED a teammate's records said "Memory shared
// from here — 2 records". A pulled record is held as shared, so the count of what this
// device shared must leave pulled records out; they are counted as pulled.
func TestSharedFromHereDoesNotCountPulledRecords(t *testing.T) {
	f := newFakeTeam(t)
	a, b := newDevice(t, "a"), newDevice(t, "b")
	shareFromA(t, f, a, b, "vpn", "v1")
	shareFromA(t, f, a, b, "wifi", "v1")
	here, err := a.ix.MemorySyncSummaryCounts("org_test", 0)
	if err != nil || here.Shared != 2 || here.Pulled != 0 {
		t.Fatalf("the device that shared them: %+v %v", here, err)
	}
	there, err := b.ix.MemorySyncSummaryCounts("org_test", 0)
	if err != nil || there.Shared != 0 || there.Pulled != 2 {
		t.Fatalf("the device that only pulled them shared none: shared=%d pulled=%d %v", there.Shared, there.Pulled, err)
	}
}
