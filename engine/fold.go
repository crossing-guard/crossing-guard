package engine

// The fold: how frozen tags become durable session and entity state.
//
// Three halves live here so that every consumer — the daemon's live path, the store's
// persistence, and a server replaying pushed events — accumulates state by one rule:
//
//   - FactFromTag       the mapping    (a tag becomes one fact)
//   - ResourceDetectors the routing    (which facts also fold onto the target entity)
//   - FoldFact          the merge      (identity, window, first-writer provenance)
//
// The store's SQL upserts implement FoldFact's contract; a test pins them equal. The
// governance model's requirement is that live accumulation equals a ts-ordered replay,
// which is why the merge is order-independent.

// StateFact is one materialized fact in the fold — the row shape of session_state and
// entity_state. Provenance here is the engine TRUST enum (observed | identity-derived |
// user-asserted), not lineage.
type StateFact struct {
	Key        string `json:"key"`
	Value      string `json:"value"`
	Detector   string `json:"detector"`
	Provenance string `json:"provenance"`
	Evidence   string `json:"evidence"`
	FirstSeen  int64  `json:"first_seen"`
	LastSeen   int64  `json:"last_seen"`
}

// FactFromTag is the mapping half: a frozen tag becomes one state fact. The observation
// window is left zero; FoldFact sets it from the event timestamp.
func FactFromTag(t Tag) StateFact {
	return StateFact{Key: t.Key, Value: t.Value, Detector: t.Detector,
		Provenance: string(t.Provenance), Evidence: t.Evidence}
}

// ResourceDetectors is the routing half: the ids of detectors whose scope is
// "resource", whose facts fold onto each target entity as well as onto the session.
func ResourceDetectors(dets []Detector) map[string]bool {
	res := make(map[string]bool)
	for _, d := range dets {
		if d.Scope == "resource" {
			res[d.ID] = true
		}
	}
	return res
}

// FoldFact is the merge half, and the contract the store's SQL upserts implement.
// Identity is (key, value, detector). The observation window folds MIN(first_seen) /
// MAX(last_seen) so live accumulation equals a ts-ordered replay. Provenance and
// evidence are first-writer-wins: provenance is fixed per detector, and evidence is a
// non-load-bearing sample — updating either on conflict would make the fold
// order-sensitive again for no governance benefit. The incoming fact's own window is
// ignored; ts is the observation. Returns the (possibly appended) slice.
func FoldFact(facts []StateFact, in StateFact, ts int64) []StateFact {
	for i := range facts {
		f := &facts[i]
		if f.Key == in.Key && f.Value == in.Value && f.Detector == in.Detector {
			if ts < f.FirstSeen {
				f.FirstSeen = ts
			}
			if ts > f.LastSeen {
				f.LastSeen = ts
			}
			return facts
		}
	}
	in.FirstSeen, in.LastSeen = ts, ts
	return append(facts, in)
}
