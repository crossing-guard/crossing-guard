package store

import (
	"strings"
	"testing"

	"crossing-guard/engine"
)

func appendChained(t *testing.T, ix *Index, session string, anchor *engine.ChainAnchor, reason string) int64 {
	t.Helper()
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	work := *anchor
	id, err := tx.AppendEvent(EventRecord{TS: 1000 + anchor.Seq, SessionID: session, Runtime: "claude", Verb: "exec",
		Tool: "Bash", Tags: `[{"key":"command","value":"echo ` + reason + `"}]`, Decision: "allow", Reason: reason, Origin: "live"}, &work)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	*anchor = work
	return id
}

func TestV31FreshStoreHasDeviceAndSchemaValidGlobalIDs(t *testing.T) {
	ix := openResultTestIndex(t)
	id, linked, err := ix.Device()
	if err != nil || !strings.HasPrefix(id, "dev_") || !engine.IsTypedID(id) || linked {
		t.Fatalf("device row: id=%q linked=%v err=%v", id, linked, err)
	}
	a := engine.NewGenesisAnchor("claude/s1")
	appendChained(t, ix, "claude/s1", a, "first")
	var gid string
	if err := ix.db.QueryRow(`SELECT global_id FROM event ORDER BY id DESC LIMIT 1`).Scan(&gid); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(gid, "evt_") || !engine.IsTypedID(gid) {
		t.Fatalf("minted global id %q is not schema-valid", gid)
	}
}

func TestV31EventChainVerifiesAndDetectsTamper(t *testing.T) {
	ix := openResultTestIndex(t)
	a := engine.NewGenesisAnchor("claude/s1")
	for _, r := range []string{"one", "two", "three"} {
		appendChained(t, ix, "claude/s1", a, r)
	}
	rep, err := ix.VerifyEventChain("claude/s1", a)
	if err != nil || rep.Status != "verified" || rep.Chained != 3 || rep.HeldSpan != [2]int64{1, 3} {
		t.Fatalf("want verified held 1–3: %+v err=%v", rep, err)
	}
	seq, tail, ok, err := ix.EventChainTail("claude/s1")
	if err != nil || !ok || seq != 3 || tail != a.Tail {
		t.Fatalf("tail: seq=%d ok=%v tail==held:%v err=%v", seq, ok, tail == a.Tail, err)
	}
	// The same-user agent edits a row in place.
	if _, err := ix.db.Exec(`UPDATE event SET reason = 'laundered' WHERE session_id = ? AND chain_seq = 2`, "claude/s1"); err != nil {
		t.Fatal(err)
	}
	rep, _ = ix.VerifyEventChain("claude/s1", a)
	if rep.Status != "fork" || !strings.Contains(rep.Detail, "seq 2") {
		t.Fatalf("edited row must fork at seq 2: %+v", rep)
	}
	// Editing the tags column is caught too: the digest is recomputed from stored bytes.
	if _, err := ix.db.Exec(`UPDATE event SET reason = 'two', tags = '[]' WHERE session_id = ? AND chain_seq = 2`, "claude/s1"); err != nil {
		t.Fatal(err)
	}
	if rep, _ = ix.VerifyEventChain("claude/s1", a); rep.Status != "fork" {
		t.Fatalf("edited tags must fork: %+v", rep)
	}
}

func TestV31RestartSeedsFromDiskAndReportsTwoSpans(t *testing.T) {
	ix := openResultTestIndex(t)
	a := engine.NewGenesisAnchor("s")
	appendChained(t, ix, "s", a, "before-restart")
	appendChained(t, ix, "s", a, "before-restart-2")
	seq, tail, _, _ := ix.EventChainTail("s")
	restarted := engine.NewDiskSeededAnchor(seq, tail) // what the daemon does on first sight after boot
	appendChained(t, ix, "s", restarted, "after-restart")
	rep, _ := ix.VerifyEventChain("s", restarted)
	if rep.Status != "verified" || rep.DiskSpan != [2]int64{1, 2} || rep.HeldSpan != [2]int64{3, 3} {
		t.Fatalf("restart must split disk 1–2 from held 3: %+v", rep)
	}
	// Truncating the pre-restart rows is invisible to a disk-seeded anchor for that span
	// — the declared window — but truncating the held row is caught.
	if _, err := ix.db.Exec(`DELETE FROM event WHERE session_id = 's' AND chain_seq = 3`); err != nil {
		t.Fatal(err)
	}
	if rep, _ = ix.VerifyEventChain("s", restarted); rep.Status != "fork" || !strings.Contains(rep.Detail, "held anchor") {
		t.Fatalf("deleting the held row must fail the tail: %+v", rep)
	}
}

func TestV31OutboxIsBornAtHeadAndAtomicWithTheEvent(t *testing.T) {
	ix := openResultTestIndex(t)
	a := engine.NewGenesisAnchor("s")
	appendChained(t, ix, "s", a, "unlinked-1")
	appendChained(t, ix, "s", a, "unlinked-2")
	if n, _ := ix.OutboxPending(); n != 0 {
		t.Fatalf("unlinked device must enqueue nothing, got %d", n)
	}
	if err := ix.SetLinked(true); err != nil {
		t.Fatal(err)
	}
	appendChained(t, ix, "s", a, "linked-1")
	if n, _ := ix.OutboxPending(); n != 1 {
		t.Fatalf("linked device enqueues one row per event, got %d (history must not be enqueued)", n)
	}
	// A rolled-back event leaves no orphan outbox row.
	tx, _ := ix.BeginGov()
	work := *a
	if _, err := tx.AppendEvent(EventRecord{TS: 9, SessionID: "s", Verb: "exec", Origin: "live"}, &work); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if n, _ := ix.OutboxPending(); n != 1 {
		t.Fatalf("rolled-back event must not leave an outbox row, got %d", n)
	}
	if rep, _ := ix.VerifyEventChain("s", a); rep.Status != "verified" {
		t.Fatalf("the held anchor was not advanced by the rollback, chain must still verify: %+v", rep)
	}
}

func TestV31BackfillIsDeterministicAndLegacyRowsStayUnchained(t *testing.T) {
	ix := openResultTestIndex(t)
	// Rows that predate the column: insert around the writer so global_id keeps its
	// empty default, exactly as a pre-31 store looks after the ALTER.
	for i := 0; i < 3; i++ {
		if _, err := ix.db.Exec(`INSERT INTO event(ts,session_id,runtime,verb,origin) VALUES(?,?,?,?,?)`, 100+i, "legacy/s", "codex", "exec", "imported"); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateSyncV31(ix.db); err != nil {
		t.Fatal(err)
	}
	first := map[int64]string{}
	rows, _ := ix.db.Query(`SELECT id, global_id, hash IS NULL FROM event WHERE session_id = 'legacy/s' ORDER BY id`)
	for rows.Next() {
		var id int64
		var gid string
		var unchained bool
		if err := rows.Scan(&id, &gid, &unchained); err != nil {
			t.Fatal(err)
		}
		if gid == "" || !engine.IsTypedID(gid) || !unchained {
			t.Fatalf("legacy row %d: gid=%q unchained=%v", id, gid, unchained)
		}
		first[id] = gid
	}
	rows.Close()
	if err := migrateSyncV31(ix.db); err != nil { // idempotent: a second run changes nothing
		t.Fatal(err)
	}
	rows, _ = ix.db.Query(`SELECT id, global_id FROM event WHERE session_id = 'legacy/s'`)
	for rows.Next() {
		var id int64
		var gid string
		_ = rows.Scan(&id, &gid)
		if first[id] != gid {
			t.Fatalf("backfill not deterministic for row %d: %s vs %s", id, first[id], gid)
		}
	}
	rows.Close()
	if rep, _ := ix.VerifyEventChain("legacy/s", nil); rep.Status != "none" || rep.Legacy != 3 {
		t.Fatalf("all-legacy session must report none, never verified: %+v", rep)
	}
}
