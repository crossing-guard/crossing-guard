package guardcli

import (
	"path/filepath"
	"strings"
	"testing"

	"crossing-guard/engine"
	"crossing-guard/store"
)

// `coverage` with no governance store: the daemon-fact catalog is still known, model
// claims are not, and the note names the store it looked for — so an agent: rule is
// unverified rather than a guessed INERT (state-producer-declarations plan D-6).
func TestCoverageStateProducersWithoutStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CG_INDEX", "")
	t.Setenv("CPMEM_INDEX", "")
	sp := coverageStateProducers()
	want := filepath.Join(home, ".crossing-guard", "index.sqlite")
	if !sp.SessionFactsKnown || sp.AgentClaimsKnown || !strings.Contains(sp.AgentClaimsNote, "no governance store at "+want) {
		t.Fatalf("no store: %+v", sp)
	}
	claim := engine.Rule{ID: "c", Action: "ask", If: engine.Predicate{Tag: "agent:b1:reviewed"}}
	if b := engine.Boundary(claim, nil, sp, engine.ReachStop); !b.Unverified || b.CanFire {
		t.Fatalf("claim rule: %+v", b)
	}
	wip := engine.Rule{ID: "w", Action: "ask", If: engine.Predicate{Tag: "session:work", Value: "uncommitted"}}
	if b := engine.Boundary(wip, nil, sp, engine.ReachStop); !b.CanFire {
		t.Fatalf("fact rule: %+v", b)
	}

	// A readable store makes the claim set known, and the note names the path read.
	ix, err := store.Open(want)
	if err != nil {
		t.Fatal(err)
	}
	ix.Close()
	sp = coverageStateProducers()
	if !sp.AgentClaimsKnown || !strings.Contains(sp.AgentClaimsNote, want) {
		t.Fatalf("with store: %+v", sp)
	}
	if b := engine.Boundary(claim, nil, sp, engine.ReachStop); b.Unverified || b.CanFire {
		t.Fatalf("claim rule with no binding must be INERT once the store is read: %+v", b)
	}
}
