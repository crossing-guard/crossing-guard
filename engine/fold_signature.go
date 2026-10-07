package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// The fold's repin tripwire (team item 4 decision 8). The team server refolds pushed
// events with the fold of the client commit it PINS; a client built past a changed fold
// would fold differently from the server that reviews its sessions, silently. Both
// repositories fold FoldSignatureFixture and pin the resulting FoldSignature to the same
// constant, so a behavioral change to FoldFact or FactFromTag fails the client's test
// here — and is, by that failure, a named repin event for the server.

// FoldFixtureEvent is one event of the tripwire's input: its time and frozen tags.
type FoldFixtureEvent struct {
	TS   int64
	Tags []Tag
}

// FoldSignatureFixture exercises every rule of the fold: identity (key, value, detector),
// an out-of-order window, first-writer provenance and evidence, and a same-key fact from
// a second detector.
var FoldSignatureFixture = []FoldFixtureEvent{
	{TS: 200, Tags: []Tag{{Key: "data-class", Value: "internal", Detector: "area.internal", Provenance: Observed, Evidence: "path-prefix"}}},
	{TS: 100, Tags: []Tag{{Key: "data-class", Value: "internal", Detector: "area.internal", Provenance: Observed, Evidence: "later-sample"},
		{Key: "exec", Value: "run", Detector: "exec.run", Provenance: Observed, Evidence: "tool=Bash"}}},
	{TS: 300, Tags: []Tag{{Key: "data-class", Value: "internal", Detector: "area.other", Provenance: IdentityDerived},
		{Key: "exec", Value: "run", Detector: "exec.run", Provenance: UserAsserted, Evidence: "ignored"}}},
}

// FoldSignature is the canonical digest of a fold's facts: sorted by (key, value,
// detector), JSON-encoded, sha256.
func FoldSignature(facts []StateFact) string {
	sorted := append([]StateFact(nil), facts...)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.Key != b.Key {
			return a.Key < b.Key
		}
		if a.Value != b.Value {
			return a.Value < b.Value
		}
		return a.Detector < b.Detector
	})
	raw, err := json.Marshal(sorted)
	if err != nil { // StateFact is strings and ints; unreachable, but never a silent digest
		return "unencodable: " + err.Error()
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
