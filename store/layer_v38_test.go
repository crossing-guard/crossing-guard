package store

import (
	"strings"
	"testing"

	"crossing-guard/engine"
	"crossing-guard/schemas"
)

// Schema 38 (team plan §5.16, item 3a): event.layer — the distribution tier whose
// rule decided. Every test here mirrors its rule_id predecessor, because the field is
// the rule field's twin: staged beside it, chained after it, shipped with it.

func TestV38MigrationIsIdempotentAndBackfillsNothing(t *testing.T) {
	ix := openResultTestIndex(t)
	// A row from before the field: it must stay empty — a layer guessed from the
	// reason's prose would be inference stored as fact, exactly like rule_id.
	if _, err := ix.db.Exec(`INSERT INTO event(ts,session_id,runtime,verb,tool,tags,decision,reason,origin,global_id,rule_id)
		VALUES(1,'s','claude','exec','Bash','[]','deny','Enforced by rule canary-deny: guarded operation — denied.','live','evt_01M2N4T1GQXPDHEZANWXNG3N10','canary-deny')`); err != nil {
		t.Fatal(err)
	}
	for run := 0; run < 2; run++ {
		if err := migrateLayerV38(ix.db); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
	}
	var layer string
	if err := ix.db.QueryRow(`SELECT layer FROM event WHERE session_id='s'`).Scan(&layer); err != nil || layer != "" {
		t.Fatalf("no layer may be inferred: layer=%q err=%v", layer, err)
	}
}

func TestV38ChainCommitsToTheLayerAndOldRowsStillVerify(t *testing.T) {
	// The canonical forms of the past, computed independently of the marshaler today:
	// pre-33 (no rule, no layer) is the rule test's own constant; pre-38 (rule, no
	// layer) is every row chained between schemas 33 and 37. If a future field is added
	// without omitempty — or inserted between Rule and Layer — every chained row on
	// every install stops verifying, and these two lines say so first.
	old := engine.EventChainBody{GlobalID: "evt_x", Seq: 1, TS: 5, Session: "s", Runtime: "claude", Verb: "exec",
		Tool: "Bash", TagsDigest: engine.TagsDigest(`[]`), Decision: "allow", Reason: "r", Origin: "live"}
	const pre33 = "9fbbc637fb8f6a3a8e25ef6ca6d98f89e0da401e7abf23eea37ea68b071ff829"
	if got := engine.HashChainEntry("p", old); got != pre33 {
		t.Fatalf("the canonical form of a pre-33 row changed: %s", got)
	}
	pre38 := old
	pre38.Rule = "deny-alpha"
	const pre38Hash = "14cfbffca7b6458a27fe4fda692dba921601b045a71f24d372e0860818d5894a"
	if got := engine.HashChainEntry("p", pre38); got != pre38Hash {
		t.Fatalf("the canonical form of a pre-38 (ruled, unlayered) row changed: %s", got)
	}
	withLayer := pre38
	withLayer.Layer = engine.LayerOrganization
	if engine.HashChainEntry("p", pre38) == engine.HashChainEntry("p", withLayer) {
		t.Fatal("the layer must be inside the hash")
	}

	// Layered and unlayered rows verify together in one session's chain.
	ix := openResultTestIndex(t)
	a := engine.NewGenesisAnchor("claude/s1")
	appendRuled(t, ix, EventRecord{TS: 1, SessionID: "claude/s1", Runtime: "claude", Verb: "exec", Tool: "Bash",
		Decision: "allow", Reason: "no rule matched", Origin: "live"}, a)
	appendRuled(t, ix, EventRecord{TS: 2, SessionID: "claude/s1", Runtime: "claude", Verb: "exec", Tool: "Bash",
		Decision: "deny", Reason: "blocked", Origin: "live", RuleID: "deny-org", Layer: "organization"}, a)
	if rep, err := ix.VerifyEventChain("claude/s1", a); err != nil || rep.Status != "verified" || rep.Chained != 2 {
		t.Fatalf("layered and unlayered rows verify together: %+v err=%v", rep, err)
	}
	// Re-attributing a block to a different tier is detectable, like re-attributing
	// its rule.
	if _, err := ix.db.Exec(`UPDATE event SET layer='user' WHERE chain_seq=2`); err != nil {
		t.Fatal(err)
	}
	if rep, _ := ix.VerifyEventChain("claude/s1", a); rep.Status != "fork" || !strings.Contains(rep.Detail, "seq 2") {
		t.Fatalf("an edited layer must fork at seq 2: %+v", rep)
	}
}

func TestV38WireEventCarriesTheLayer(t *testing.T) {
	ix := openResultTestIndex(t)
	a := engine.NewGenesisAnchor("claude/s1")
	appendRuled(t, ix, EventRecord{TS: 2, SessionID: "claude/s1", Runtime: "claude", Verb: "exec", Tool: "Bash",
		Decision: "deny", Reason: "blocked", Origin: "live", RuleID: "deny-org", Layer: "organization"}, a)
	inputs, err := ix.EventWireInputs("claude/s1")
	if err != nil || len(inputs) != 1 || inputs[0].Layer != engine.LayerOrganization {
		t.Fatalf("wire input must carry the layer: %+v err=%v", inputs, err)
	}
	raw, err := engine.EncodeWireEvent(inputs[0], nil)
	if err != nil || !strings.Contains(string(raw), `"layer":"organization"`) {
		t.Fatalf("payload.layer stops being always-null: %s err=%v", raw, err)
	}
	if err := schemas.Validate("event.schema.json", raw); err != nil {
		t.Fatalf("a layered event must still be schema-valid: %v", err)
	}
	// An unlayered row encodes with layer null, never a guessed tier.
	appendRuled(t, ix, EventRecord{TS: 3, SessionID: "claude/s1", Runtime: "claude", Verb: "exec", Tool: "Bash",
		Decision: "allow", Reason: "no rule matched", Origin: "live"}, a)
	inputs, err = ix.EventWireInputs("claude/s1")
	if err != nil || len(inputs) != 2 {
		t.Fatalf("re-read: %+v err=%v", inputs, err)
	}
	raw2, err := engine.EncodeWireEvent(inputs[1], nil)
	if err != nil || strings.Contains(string(raw2), `"layer":"`) {
		t.Fatalf("an unlayered row must not assert a tier: %s err=%v", raw2, err)
	}
}

// The partial index exists for per-layer filtering; most rows carry no layer, exactly
// as most carry no rule. This reads the plan so the index cannot become decoration.
func TestV38PartialIndexExists(t *testing.T) {
	ix := openResultTestIndex(t)
	rows, err := ix.db.Query(`EXPLAIN QUERY PLAN SELECT id FROM event WHERE layer != '' AND layer=?`, "organization")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	plan := ""
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail + "\n"
	}
	if !strings.Contains(plan, "event_layer") {
		t.Fatalf("a per-layer query does not use the partial index:\n%s", plan)
	}
}
