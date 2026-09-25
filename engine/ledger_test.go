package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func newTestLedger(t *testing.T) *Ledger {
	d, err := LoadDetectors("testdata/detectors.json")
	if err != nil {
		t.Fatal(err)
	}
	pol, err := LoadPolicy("testdata/policy.json")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, err := NewLedger(path, d, pol)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// The tamper property, in Go: a same-user agent rewrites the on-disk chain PERFECTLY
// (recomputing every hash), and Verify still catches it via the held anchor.
func TestLedgerTamperEvident(t *testing.T) {
	l := newTestLedger(t)
	l.Observe("s1", Event{Tool: "getOrderAddress"}) // personal-data
	l.Observe("s1", Event{Tool: "getVendorProduct"})
	if ok, why := l.Verify("s1"); !ok {
		t.Fatalf("honest chain should verify, got %q", why)
	}
	// the in-session agent rewrites the ledger file with a fresh, internally-perfect
	// chain (this is the A5 "full rewrite" — the strongest attack).
	forged := []ChainEntry{}
	prev := "genesis:s1"
	for i, val := range []string{"public", "public"} {
		body := entryBody{Session: "s1", Seq: i + 1,
			Tag: Tag{Key: "data-class", Value: val, Detector: "forged"}}
		h := l.hashOf(prev, body)
		forged = append(forged, ChainEntry{Body: body, Prev: prev, Hash: h})
		prev = h
	}
	f, _ := os.Create(l.path)
	for _, e := range forged {
		b, _ := json.Marshal(e)
		f.Write(append(b, '\n'))
	}
	f.Close()
	if ok, why := l.Verify("s1"); ok {
		t.Fatal("forged-but-internally-perfect chain must FAIL against the held anchor")
	} else {
		t.Logf("caught: %s", why)
	}
	// deleting the ledger is also caught
	os.Remove(l.path)
	if ok, _ := l.Verify("s1"); ok {
		t.Fatal("deleted ledger must fail (held anchor attests it existed)")
	}
}

// The point of wiring the hook to the daemon: a compound predicate fires on tags
// observed across DIFFERENT events in the session — impossible with event-only tags.
func TestSessionScopedCompoundFires(t *testing.T) {
	l := newTestLedger(t)
	// event 1: internal business data (vendor cost) — earlier in the session
	o1 := l.Observe("s2", Event{Tool: "getVendorProduct"})
	if o1.Decision.Decision != "allow" {
		t.Fatalf("internal data alone should not block, got %s", o1.Decision.Decision)
	}
	// event 2: an external destination — NOW. Event-only tags would see only
	// destination-class=external and NOT fire. Session-scoped, {internal, external}
	// trips external-egress-of-internal.
	o2 := l.Observe("s2", Event{Destination: "https://random-saas.com/upload"})
	if o2.Decision.Decision != "block" {
		t.Fatalf("session-scoped compound (internal seen earlier AND external now) "+
			"should BLOCK, got %s (session tags: %v)", o2.Decision.Decision, o2.SessionTags)
	}
	if o2.Decision.Rule != "external-egress-of-internal" {
		t.Fatalf("wrong rule fired: %s", o2.Decision.Rule)
	}
}
