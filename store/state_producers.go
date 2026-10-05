package store

// The coverage compiler's non-detector producers (state-producer-declarations plan
// D-2, D-3, D-6). engine.Boundary labels a session:/agent: term from detectors AND
// from these declarations; without them a rule the stateful tier fires on reads INERT.
// Every production coverage surface builds its declarations here, so the catalog and
// the claim derivation have one owner.

import (
	"fmt"

	"crossing-guard/engine"
)

// Model-claim producer gaps: why a claim-backed rule can miss a session even though a
// binding may write the tag.
var agentClaimGaps = []string{
	"model judgment — the claim is the agent's, not a detection",
	"fires only in sessions the binding watches",
	"fires only where the group recorded the session's live (native) id",
}

// StateProducersFor returns every declared non-detector producer this caller can see.
// The direct-fact catalog is compiled in and always present. Model-claim producers
// are read from ix, all or nothing: a nil index or any read error leaves
// AgentClaimsKnown false with the reason, so an agent: term reads UNVERIFIED rather
// than a guessed INERT.
func StateProducersFor(ix *Index, now int64) engine.StateProducers {
	sp := engine.StateProducers{SessionFactsKnown: true}
	for _, fact := range DirectSessionFacts() {
		sp.Producers = append(sp.Producers, fact.StateProducer())
	}
	if ix == nil {
		sp.AgentClaimsNote = "this surface does not read the binding set"
		return sp
	}
	claims, bindings, keys, err := ix.agentClaimProducers(now)
	if err != nil {
		sp.AgentClaimsNote = "the binding set could not be read: " + err.Error()
		return sp
	}
	sp.Producers = append(sp.Producers, claims...)
	sp.AgentClaimsKnown = true
	sp.AgentClaimsNote = fmt.Sprintf("read %d binding(s) and %d active claim key(s)", bindings, keys)
	return sp
}

// agentClaimProducers derives agent:<binding>:<tag> producers from saved bindings and
// live claim rows:
//   - a binding that can still complete a claim run declares its DeclaredTags. That is
//     an enabled binding, or a disabled one with a run still admitted, running or
//     parked: claim completion does not check the binding's state.
//   - an active claim row is read by the stateful tier whatever its binding's state,
//     so its key is a producer too.
func (ix *Index) agentClaimProducers(now int64) ([]engine.StateProducer, int, int, error) {
	bindings, err := ix.ManagedBindings(false)
	if err != nil {
		return nil, 0, 0, err
	}
	inFlight, err := ix.distinctStrings(`SELECT DISTINCT binding_id FROM orchestration_managed_run WHERE state IN ('admitted','running','parked')`)
	if err != nil {
		return nil, 0, 0, err
	}
	active, err := ix.distinctPairs(`SELECT DISTINCT agent_key,binding_id FROM orchestration_tag
		WHERE retracted_at=0 AND (expires_at=0 OR expires_at>?) ORDER BY agent_key`, now)
	if err != nil {
		return nil, 0, 0, err
	}
	var out []engine.StateProducer
	seen := map[string]bool{}
	add := func(key, source string) {
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, engine.StateProducer{Tag: key, Values: []string{OrchestrationTagProvenance},
			// Harvest too: the audit reads a session's agent: keys into its dry run.
			Source: source, Gaps: agentClaimGaps, Live: true, Harvest: true})
	}
	for _, b := range bindings {
		if b.State != "enabled" && !inFlight[b.BindingID] {
			continue
		}
		for _, tag := range b.DeclaredTags {
			add(engine.AgentStatePrefix+b.BindingID+":"+tag, "binding:"+b.BindingID)
		}
	}
	for _, pair := range active {
		add(pair[0], "claims:"+pair[1])
	}
	return out, len(bindings), len(active), nil
}

// distinctStrings reads one string column into a set. A scan or iteration error fails
// the whole read, so a caller never mistakes a cut-short set for a complete one.
func (ix *Index) distinctStrings(query string, args ...any) (map[string]bool, error) {
	rows, err := ix.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}

// distinctPairs reads two string columns, all or nothing, in query order.
func (ix *Index) distinctPairs(query string, args ...any) ([][2]string, error) {
	rows, err := ix.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var pair [2]string
		if err := rows.Scan(&pair[0], &pair[1]); err != nil {
			return nil, err
		}
		out = append(out, pair)
	}
	return out, rows.Err()
}
