package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// The event chain: tamper evidence for the append-only event log, per session.
//
// This is the per-tag ledger's mechanism (sha256 over prev ‖ canonical body, a tail the
// daemon holds in RAM) applied to event rows, with its own anchor — the two chains do
// not share a tail, because Ledger.Verify walks ledger.jsonl and would read every event
// advance as a forged tail (team plan §5.12, recorded deviation from R10).
//
// The chain commits to a DIGEST of the frozen tags, never the tags. Push-time
// redaction of the command tag or evidence fragments can change what is sent without
// touching what was chained, and a server verifies over digests it can never open
// (invariant 13). The walker lives here, not in the store, so the server verifies
// pushed rows with the same code.

// EventChainGenesis is the chain-root sentinel for a session's event chain. Advance and
// VerifyEventChain must agree on it exactly, so it lives in one place both call.
func EventChainGenesis(session string) string { return "genesis-event:" + session }

// EventChainBody is the canonical, order-fixed body an entry's hash covers. Field order
// is the wire order; Go's encoding/json marshals struct fields in declaration order.
type EventChainBody struct {
	GlobalID   string `json:"global_id"`
	Seq        int64  `json:"seq"`
	TS         int64  `json:"ts"`
	Session    string `json:"session"`
	Runtime    string `json:"runtime"`
	Verb       string `json:"verb"`
	Tool       string `json:"tool"`
	Target     string `json:"target"`
	TagsDigest string `json:"tags_digest"`
	Decision   string `json:"decision"`
	Reason     string `json:"reason"`
	Origin     string `json:"origin"`
}

// TagsDigest is the commitment to a frozen tags document: the exact bytes as stored.
func TagsDigest(frozenTagsJSON string) string {
	sum := sha256.Sum256([]byte(frozenTagsJSON))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// HashChainEntry is sha256(prev ‖ canonical body), hex — the ledger's shape.
func HashChainEntry(prev string, body EventChainBody) string {
	canon, _ := json.Marshal(body)
	h := sha256.Sum256(append([]byte(prev), canon...))
	return hex.EncodeToString(h[:])
}

// ChainAnchor is a session's held tail. Origin says what the holder can vouch for:
//   - "genesis"      the daemon watched this session from its first chained event;
//   - "disk-seeded"  the daemon restarted and continued from the tail it found in the
//     store — the forgeable case ADR 0016 measured, so rows at or below
//     SeededSeq are reported as a separate, weaker span.
type ChainAnchor struct {
	Seq       int64
	Tail      string
	Origin    string
	SeededSeq int64
}

// NewGenesisAnchor starts a session's chain from nothing.
func NewGenesisAnchor(session string) *ChainAnchor {
	return &ChainAnchor{Tail: EventChainGenesis(session), Origin: "genesis"}
}

// NewDiskSeededAnchor continues a chain from a tail read back from the store after a
// restart. The seam is remembered so verification can say which rows were held.
func NewDiskSeededAnchor(seq int64, tail string) *ChainAnchor {
	return &ChainAnchor{Seq: seq, Tail: tail, Origin: "disk-seeded", SeededSeq: seq}
}

// Advance appends one entry: assigns the next seq, hashes the body against the held
// tail, and moves the tail. Returns the entry's prev and hash for persistence.
func (a *ChainAnchor) Advance(body *EventChainBody) (prev, hash string) {
	prev = a.Tail
	body.Seq = a.Seq + 1
	hash = HashChainEntry(prev, *body)
	a.Seq, a.Tail = body.Seq, hash
	return prev, hash
}

// ChainRow is one persisted entry as read back: the body plus what was stored beside it.
// A legacy row (pre-chain) has an empty Hash and is counted, never verified.
type ChainRow struct {
	Body EventChainBody
	Prev string
	Hash string
}

// ChainReport is what a verification says, with the spans kept separate on purpose.
type ChainReport struct {
	Session string `json:"session"`
	Rows    int    `json:"rows"`
	Chained int    `json:"chained"`
	Legacy  int    `json:"legacy"`
	// Status: "verified" | "fork" | "gap" | "none" | "empty".
	Status string `json:"status"`
	Detail string `json:"detail"`
	// DiskSpan covers seqs the holder did not watch (disk-seeded); HeldSpan those it did.
	// Either is [0,0] when empty. TailMatchesHeld is nil when no anchor was offered.
	DiskSpan        [2]int64 `json:"disk_span"`
	HeldSpan        [2]int64 `json:"held_span"`
	TailMatchesHeld *bool    `json:"tail_matches_held,omitempty"`
}

// VerifyEventChain walks a session's rows in append order and recomputes every hash.
// With a held anchor it also checks the final tail against it — the check that catches a
// perfect local recompute, because the same-user agent cannot reach the held tail.
func VerifyEventChain(session string, rows []ChainRow, held *ChainAnchor) ChainReport {
	r := ChainReport{Session: session, Rows: len(rows)}
	if len(rows) == 0 {
		r.Status, r.Detail = "empty", "no events for this session"
		return r
	}
	prev := EventChainGenesis(session)
	var lastSeq int64
	var lastHash string
	for _, row := range rows {
		if row.Hash == "" {
			r.Legacy++
			continue
		}
		r.Chained++
		if row.Body.Seq != lastSeq+1 {
			r.Status = "gap"
			r.Detail = fmt.Sprintf("seq %d follows %d", row.Body.Seq, lastSeq)
			return r
		}
		if row.Prev != prev {
			r.Status = "fork"
			r.Detail = fmt.Sprintf("seq %d: prev does not match the preceding tail", row.Body.Seq)
			return r
		}
		if HashChainEntry(row.Prev, row.Body) != row.Hash {
			r.Status = "fork"
			r.Detail = fmt.Sprintf("seq %d: stored hash does not match its body (body edited)", row.Body.Seq)
			return r
		}
		prev, lastSeq, lastHash = row.Hash, row.Body.Seq, row.Hash
	}
	if r.Chained == 0 {
		r.Status, r.Detail = "none", "every row predates the chain"
		return r
	}
	r.Status = "verified"
	r.Detail = fmt.Sprintf("%d chained rows recompute; %d legacy rows unchained", r.Chained, r.Legacy)
	if held != nil {
		match := held.Seq == lastSeq && held.Tail == lastHash
		r.TailMatchesHeld = &match
		if !match {
			r.Status = "fork"
			r.Detail = fmt.Sprintf("tail ≠ held anchor (store seq %d %.8s…, held seq %d %.8s…) → rewrite/truncate/forge", lastSeq, lastHash, held.Seq, held.Tail)
			return r
		}
		if held.Origin == "disk-seeded" && held.SeededSeq > 0 {
			r.DiskSpan = [2]int64{1, held.SeededSeq}
			if lastSeq > held.SeededSeq {
				r.HeldSpan = [2]int64{held.SeededSeq + 1, lastSeq}
			}
			r.Detail += fmt.Sprintf("; seqs 1–%d verified from disk only (daemon restarted), %d–%d held", held.SeededSeq, held.SeededSeq+1, lastSeq)
		} else {
			r.HeldSpan = [2]int64{1, lastSeq}
		}
	} else {
		r.DiskSpan = [2]int64{1, lastSeq}
		r.Detail += "; no held anchor offered — internal consistency only"
	}
	return r
}
