package engine

import (
	"math/rand"
	"sort"
	"testing"
)

func sortedFacts(in []StateFact) []StateFact {
	out := append([]StateFact(nil), in...)
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Key != b.Key {
			return a.Key < b.Key
		}
		if a.Value != b.Value {
			return a.Value < b.Value
		}
		return a.Detector < b.Detector
	})
	return out
}

func TestFoldFactIdentityAndWindow(t *testing.T) {
	var facts []StateFact
	facts = FoldFact(facts, StateFact{Key: "data-class", Value: "pii", Detector: "d1", Provenance: "observed", Evidence: "first"}, 20)
	facts = FoldFact(facts, StateFact{Key: "data-class", Value: "pii", Detector: "d1", Provenance: "changed", Evidence: "second"}, 10)
	facts = FoldFact(facts, StateFact{Key: "data-class", Value: "pii", Detector: "d1"}, 30)
	facts = FoldFact(facts, StateFact{Key: "data-class", Value: "pii", Detector: "d2"}, 25)
	if len(facts) != 2 {
		t.Fatalf("identity is (key,value,detector): want 2 facts, got %d", len(facts))
	}
	f := facts[0]
	if f.FirstSeen != 10 || f.LastSeen != 30 {
		t.Fatalf("window must fold MIN/MAX: got [%d,%d]", f.FirstSeen, f.LastSeen)
	}
	if f.Provenance != "observed" || f.Evidence != "first" {
		t.Fatalf("provenance/evidence are first-writer-wins: got %q/%q", f.Provenance, f.Evidence)
	}
	if facts[1].FirstSeen != 25 || facts[1].LastSeen != 25 {
		t.Fatalf("a new fact's window is the observation ts: got [%d,%d]", facts[1].FirstSeen, facts[1].LastSeen)
	}
}

// The governance model's requirement: live accumulation equals a ts-ordered replay.
// Any permutation of the same observations must fold to the same facts.
func TestFoldFactIsOrderIndependent(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	type obs struct {
		f  StateFact
		ts int64
	}
	var seq []obs
	for i := 0; i < 200; i++ {
		seq = append(seq, obs{StateFact{
			Key: []string{"k1", "k2", "k3"}[rng.Intn(3)], Value: []string{"a", "b"}[rng.Intn(2)],
			Detector: []string{"d1", "d2"}[rng.Intn(2)], Provenance: "p", Evidence: "e",
		}, int64(rng.Intn(1000))})
	}
	fold := func(order []int) []StateFact {
		var facts []StateFact
		for _, i := range order {
			facts = FoldFact(facts, seq[i].f, seq[i].ts)
		}
		return sortedFacts(facts)
	}
	base := make([]int, len(seq))
	for i := range base {
		base[i] = i
	}
	want := fold(base)
	for trial := 0; trial < 20; trial++ {
		perm := append([]int(nil), base...)
		rng.Shuffle(len(perm), func(i, j int) { perm[i], perm[j] = perm[j], perm[i] })
		got := fold(perm)
		if len(got) != len(want) {
			t.Fatalf("trial %d: %d facts vs %d", trial, len(got), len(want))
		}
		for i := range want {
			// Evidence/provenance are first-writer-wins and therefore legitimately order-
			// dependent; identity and window must not be.
			if got[i].Key != want[i].Key || got[i].Value != want[i].Value || got[i].Detector != want[i].Detector ||
				got[i].FirstSeen != want[i].FirstSeen || got[i].LastSeen != want[i].LastSeen {
				t.Fatalf("trial %d fact %d: %+v vs %+v", trial, i, got[i], want[i])
			}
		}
	}
}

func TestFactFromTagAndResourceDetectors(t *testing.T) {
	f := FactFromTag(Tag{Key: "k", Value: "v", Detector: "d", Provenance: "observed", Evidence: "ev"})
	if f.Key != "k" || f.Value != "v" || f.Detector != "d" || f.Provenance != "observed" || f.Evidence != "ev" || f.FirstSeen != 0 || f.LastSeen != 0 {
		t.Fatalf("mapping drifted: %+v", f)
	}
	res := ResourceDetectors([]Detector{{ID: "r", Scope: "resource"}, {ID: "s", Scope: "session"}, {ID: "n"}})
	if !res["r"] || res["s"] || res["n"] || len(res) != 1 {
		t.Fatalf("only resource-scoped detectors route onto entities: %v", res)
	}
}
