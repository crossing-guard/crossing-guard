package engine

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// Ledger is the daemon-owned, append-only, hash-chained session ledger — the Go port
// of ledgerd.py. Each session's tail ANCHOR is held in process memory (never restored
// from disk: that one out-of-reach secret is what makes it tamper-EVIDENT against a
// same-user in-session agent — proven by the tamper spike). The daemon also holds the
// accumulated per-session tag set, so a decision spans the whole session (a compound
// predicate can fire on a tag observed several events earlier).
type Ledger struct {
	path   string
	mu     sync.Mutex
	anchor map[string]anchorRec // session -> held tail        (RAM only)
	seq    map[string]int       // session -> last seq
	tags   map[string][]Tag     // session -> accumulated tags (deduped)
	dets   []Detector
	pol    *Policy
}

type anchorRec struct {
	Seq  int
	Tail string
}

type entryBody struct {
	Session string `json:"session"`
	Seq     int    `json:"seq"`
	Tag     Tag    `json:"tag"`
}

// ChainEntry is one persisted line: the body + the chain links.
type ChainEntry struct {
	Body entryBody `json:"body"`
	Prev string    `json:"prev"`
	Hash string    `json:"hash"`
}

// Observation is what the daemon returns for one event: the event's own tags, the
// session-accumulated tags, the water mark, and the decision over the SESSION tags.
type Observation struct {
	Session     string   `json:"session"`
	Tags        []Tag    `json:"tags"`
	SessionTags []Tag    `json:"session_tags"`
	Watermark   string   `json:"watermark"`
	Decision    Decision `json:"decision"`
	WeakNeg     bool     `json:"weak_negative"`
}

func NewLedger(path string, dets []Detector, pol *Policy) (*Ledger, error) {
	l := &Ledger{path: path, anchor: map[string]anchorRec{}, seq: map[string]int{},
		tags: map[string][]Tag{}, dets: dets, pol: pol}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if f, err := os.Create(path); err == nil {
			f.Close()
		}
	} else {
		l.reindex() // rebuild seq counters only — NEVER the anchor (would defeat it)
	}
	return l, nil
}

func (l *Ledger) reindex() {
	for _, e := range l.readAll() {
		if e.Body.Seq > l.seq[e.Body.Session] {
			l.seq[e.Body.Session] = e.Body.Seq
		}
	}
}

func (l *Ledger) readAll() []ChainEntry {
	var out []ChainEntry
	f, err := os.Open(l.path)
	if err != nil {
		return out // deleted ledger → no entries; the held anchor catches it
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e ChainEntry
		if json.Unmarshal(line, &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

func (l *Ledger) canon(b entryBody) []byte {
	x, _ := json.Marshal(b) // struct field order is deterministic in Go
	return x
}

func (l *Ledger) hashOf(prev string, b entryBody) string {
	h := sha256.Sum256(append([]byte(prev), l.canon(b)...))
	return hex.EncodeToString(h[:])
}

// genesisAnchor is the chain-root sentinel for a session's ledger. appendTag
// and Verify must agree on it exactly, or every verification of that session
// fails — so it lives in one place both call.
func genesisAnchor(session string) string { return "genesis:" + session }

func (l *Ledger) appendTag(session string, tag Tag) {
	prev := genesisAnchor(session)
	if a, ok := l.anchor[session]; ok {
		prev = a.Tail
	}
	seq := l.seq[session] + 1
	body := entryBody{Session: session, Seq: seq, Tag: tag}
	h := l.hashOf(prev, body)
	entry := ChainEntry{Body: body, Prev: prev, Hash: h}
	if f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		b, _ := json.Marshal(entry)
		_, _ = f.Write(append(b, '\n'))
		f.Close()
	}
	l.anchor[session] = anchorRec{Seq: seq, Tail: h} // advance the HELD anchor
	l.seq[session] = seq
}

func (l *Ledger) addSessionTag(session string, t Tag) {
	key := t.identityKey()
	for _, e := range l.tags[session] {
		if e.identityKey() == key {
			return
		}
	}
	l.tags[session] = append(l.tags[session], t)
}

// Observe classifies an event, appends a chain entry per tag, accumulates the session
// tag set, and decides over the WHOLE session — the session-scoped decision.
func (l *Ledger) Observe(session string, ev Event) Observation {
	l.mu.Lock()
	defer l.mu.Unlock()
	// The ledger observes tool actions; a caller that leaves Role empty would
	// otherwise be silently skipped by every role-scoped detector in the default
	// library (fail-OPEN — the whole session watermark stuck at zero). Default the
	// role here so no /observe caller can disarm session governance by omission.
	if ev.Role == "" {
		ev.Role = "tool_call"
	}
	tags := Classify(ev, l.dets)
	for _, t := range tags {
		l.appendTag(session, t)
		l.addSessionTag(session, t)
	}
	sess := l.tags[session]
	return Observation{Session: session, Tags: tags, SessionTags: sess,
		Watermark: WaterMark(sess), Decision: Decide(sess, l.pol),
		WeakNeg: len(sess) == 0}
}

// Verify walks the on-disk chain for a session and checks the tail against the HELD
// anchor — catches rewrite/truncate/delete even when the on-disk chain is internally
// perfect (the same-user agent can recompute hashes but cannot reach the held tail).
func (l *Ledger) Verify(session string) (bool, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	held, hasHeld := l.anchor[session]
	var es []ChainEntry
	for _, e := range l.readAll() {
		if e.Body.Session == session {
			es = append(es, e)
		}
	}
	if len(es) == 0 {
		if hasHeld {
			return false, fmt.Sprintf("no entries on disk but daemon holds anchor seq=%d → deletion/truncation detected", held.Seq)
		}
		return true, "no entries, nothing held (session never observed)"
	}
	prev := genesisAnchor(session)
	for _, e := range es {
		if e.Prev != prev {
			return false, fmt.Sprintf("chain BREAK at seq=%d", e.Body.Seq)
		}
		if l.hashOf(e.Prev, e.Body) != e.Hash {
			return false, fmt.Sprintf("hash MISMATCH at seq=%d (body edited)", e.Body.Seq)
		}
		prev = e.Hash
	}
	if hasHeld && (es[len(es)-1].Hash != held.Tail || es[len(es)-1].Body.Seq != held.Seq) {
		return false, fmt.Sprintf("tail ≠ HELD anchor (disk %.8s…, daemon holds %.8s…) → rewrite/truncate/forge detected",
			es[len(es)-1].Hash, held.Tail)
	}
	return true, fmt.Sprintf("chain valid + tail matches held anchor (seq=%d)", held.Seq)
}

// SessionsHeld reports how many sessions the daemon holds an anchor for (for /status).
func (l *Ledger) SessionsHeld() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.anchor)
}
