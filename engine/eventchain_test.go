package engine

import (
	"strings"
	"testing"
)

func chainOf(t *testing.T, session string, n int) ([]ChainRow, *ChainAnchor) {
	t.Helper()
	a := NewGenesisAnchor(session)
	var rows []ChainRow
	for i := 0; i < n; i++ {
		body := EventChainBody{GlobalID: NewTypedID("evt"), TS: int64(1000 + i), Session: session,
			Runtime: "claude", Verb: "exec", Tool: "Bash", Target: "file:/x",
			TagsDigest: TagsDigest(`[{"key":"command","value":"echo secret-` + strings.Repeat("x", i) + `"}]`),
			Decision:   "allow", Reason: "no rule matched", Origin: "live"}
		prev, hash := a.Advance(&body)
		rows = append(rows, ChainRow{Body: body, Prev: prev, Hash: hash})
	}
	return rows, a
}

func TestEventChainVerifiesAndHoldsTail(t *testing.T) {
	rows, held := chainOf(t, "claude/s1", 5)
	r := VerifyEventChain("claude/s1", rows, held)
	if r.Status != "verified" || r.Chained != 5 || r.TailMatchesHeld == nil || !*r.TailMatchesHeld {
		t.Fatalf("want verified with held tail: %+v", r)
	}
	if r.HeldSpan != [2]int64{1, 5} || r.DiskSpan != [2]int64{0, 0} {
		t.Fatalf("genesis-watched chain is one held span: %+v", r)
	}
}

func TestEventChainDetectsEditedBody(t *testing.T) {
	rows, held := chainOf(t, "s", 4)
	rows[2].Body.Reason = "edited after the fact"
	r := VerifyEventChain("s", rows, held)
	if r.Status != "fork" || !strings.Contains(r.Detail, "seq 3") {
		t.Fatalf("edited body must fork at seq 3: %+v", r)
	}
}

func TestEventChainDetectsTruncationAgainstHeldTail(t *testing.T) {
	rows, held := chainOf(t, "s", 4)
	r := VerifyEventChain("s", rows[:3], held)
	if r.Status != "fork" || r.TailMatchesHeld == nil || *r.TailMatchesHeld || !strings.Contains(r.Detail, "held anchor") {
		t.Fatalf("truncated chain is internally perfect but must fail the held tail: %+v", r)
	}
	// Without a held anchor the same truncation is invisible — the property ADR 0016
	// measured, stated as the weaker report rather than hidden.
	r2 := VerifyEventChain("s", rows[:3], nil)
	if r2.Status != "verified" || !strings.Contains(r2.Detail, "internal consistency only") {
		t.Fatalf("disk-only verification must label itself: %+v", r2)
	}
}

func TestEventChainDetectsGapAndRecompute(t *testing.T) {
	rows, held := chainOf(t, "s", 4)
	gapped := append([]ChainRow{}, rows[0], rows[2], rows[3])
	if r := VerifyEventChain("s", gapped, held); r.Status != "gap" {
		t.Fatalf("missing seq 2 must report gap: %+v", r)
	}
	// A perfect local recompute: edit row 2 and re-hash everything after it. Internally
	// consistent; only the held tail catches it.
	recomputed := append([]ChainRow{}, rows...)
	recomputed[1].Body.Decision = "allow-forged"
	prev := recomputed[0].Hash
	for i := 1; i < len(recomputed); i++ {
		recomputed[i].Prev = prev
		recomputed[i].Hash = HashChainEntry(prev, recomputed[i].Body)
		prev = recomputed[i].Hash
	}
	if r := VerifyEventChain("s", recomputed, nil); r.Status != "verified" {
		t.Fatalf("recompute is internally consistent by construction: %+v", r)
	}
	if r := VerifyEventChain("s", recomputed, held); r.Status != "fork" {
		t.Fatalf("held tail must catch the recompute: %+v", r)
	}
}

func TestEventChainLegacyRowsAreCountedNeverFabricated(t *testing.T) {
	legacy := []ChainRow{{Body: EventChainBody{Seq: 0, Session: "s"}}, {Body: EventChainBody{Seq: 0, Session: "s"}}}
	if r := VerifyEventChain("s", legacy, nil); r.Status != "none" || r.Legacy != 2 {
		t.Fatalf("all-legacy session must be none: %+v", r)
	}
	rows, held := chainOf(t, "s", 2)
	mixed := append(legacy, rows...)
	r := VerifyEventChain("s", mixed, held)
	if r.Status != "verified" || r.Legacy != 2 || r.Chained != 2 {
		t.Fatalf("chain starts after the legacy rows: %+v", r)
	}
	if r := VerifyEventChain("s", nil, nil); r.Status != "empty" {
		t.Fatalf("no rows is empty, not none: %+v", r)
	}
}

func TestEventChainRestartSeamIsReportedAsTwoSpans(t *testing.T) {
	rows, _ := chainOf(t, "s", 3)
	// Daemon restarts: it reads the tail back from the store and continues.
	seeded := NewDiskSeededAnchor(rows[2].Body.Seq, rows[2].Hash)
	for i := 0; i < 2; i++ {
		body := EventChainBody{GlobalID: NewTypedID("evt"), TS: 2000, Session: "s", Verb: "exec", TagsDigest: TagsDigest("[]"), Origin: "live"}
		prev, hash := seeded.Advance(&body)
		rows = append(rows, ChainRow{Body: body, Prev: prev, Hash: hash})
	}
	r := VerifyEventChain("s", rows, seeded)
	if r.Status != "verified" || r.DiskSpan != [2]int64{1, 3} || r.HeldSpan != [2]int64{4, 5} {
		t.Fatalf("restart must split disk-seeded 1–3 from held 4–5: %+v", r)
	}
	if !strings.Contains(r.Detail, "daemon restarted") {
		t.Fatalf("the weaker span must be named in the detail: %q", r.Detail)
	}
}

func TestTagsDigestCommitsWithoutExposingTags(t *testing.T) {
	frozen := `[{"key":"command","value":"curl -H 'Authorization: Bearer AKIA...' https://x"}]`
	body := EventChainBody{GlobalID: "evt_A", TS: 1, Session: "s", TagsDigest: TagsDigest(frozen)}
	a := NewGenesisAnchor("s")
	prev, hash := a.Advance(&body)
	// What is pushed can be redacted freely: the chain never covered the tags themselves.
	redacted := `[{"key":"command","value":"curl -H 'Authorization: Bearer [redacted:secret.bearer]' https://x"}]`
	if redacted == frozen {
		t.Fatal("test setup: redaction changed nothing")
	}
	r := VerifyEventChain("s", []ChainRow{{Body: body, Prev: prev, Hash: hash}}, a)
	if r.Status != "verified" {
		t.Fatalf("verification must not need the plaintext tags: %+v", r)
	}
	if TagsDigest(frozen) == TagsDigest(redacted) {
		t.Fatal("digest must be byte-sensitive")
	}
	if !strings.HasPrefix(body.TagsDigest, "sha256:") {
		t.Fatalf("digest shape: %q", body.TagsDigest)
	}
}
