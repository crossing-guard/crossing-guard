package store

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"crossing-guard/engine"
)

func canonicalFacts(in []engine.StateFact) []engine.StateFact {
	out := append([]engine.StateFact(nil), in...)
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

// The fold's merge contract is engine.FoldFact; the SQL upserts here persist it. This
// pins the two halves equal — a randomized observation sequence folded through the
// engine and through a real store yields identical session_state rows, every field —
// so a server replaying pushed events with the same engine function reproduces the
// client's state (team plan §5.6 criterion 16, client half).
func TestSessionStateUpsertEqualsEngineFold(t *testing.T) {
	ix := openResultTestIndex(t)
	rng := rand.New(rand.NewSource(11))
	const session = "fold/equivalence"
	var want []engine.StateFact
	for i := 0; i < 300; i++ {
		fact := engine.StateFact{
			Key:        []string{"k1", "k2", "k3"}[rng.Intn(3)],
			Value:      []string{"a", "b"}[rng.Intn(2)],
			Detector:   []string{"d1", "d2"}[rng.Intn(2)],
			Provenance: []string{"observed", "user-asserted"}[rng.Intn(2)],
			Evidence:   fmt.Sprintf("ev%d", i),
		}
		ts := int64(rng.Intn(1000))
		want = engine.FoldFact(want, fact, ts)
		tx, err := ix.BeginGov()
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.UpsertSessionState(session, fact, ts); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ix.SessionState(session)
	if err != nil {
		t.Fatal(err)
	}
	want, got = canonicalFacts(want), canonicalFacts(got)
	if len(got) != len(want) {
		t.Fatalf("store folded %d facts, engine folded %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("fact %d diverged\n store:  %+v\n engine: %+v", i, got[i], want[i])
		}
	}
}
