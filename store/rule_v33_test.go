package store

import (
	"strings"
	"testing"

	"crossing-guard/engine"
	"crossing-guard/ruledoc"
	"crossing-guard/schemas"
)

func appendRuled(t *testing.T, ix *Index, e EventRecord, anchor *engine.ChainAnchor) {
	t.Helper()
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	var work *engine.ChainAnchor
	if anchor != nil {
		copied := *anchor
		work = &copied
	}
	if e.Tags == "" {
		e.Tags = `[]`
	}
	if _, err := tx.AppendEvent(e, work); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if anchor != nil {
		*anchor = *work
	}
}

func TestV33MigrationIsIdempotentAndBackfillsNothing(t *testing.T) {
	ix := openResultTestIndex(t)
	// A row from before the field: the prose names a rule, the column must stay empty.
	if _, err := ix.db.Exec(`INSERT INTO event(ts,session_id,runtime,verb,tool,tags,decision,reason,origin,global_id)
		VALUES(1,'s','claude','exec','Bash','[]','deny','Enforced by rule canary-deny: guarded operation — denied.','live','evt_01M2N4T1GQXPDHEZANWXNG3N10')`); err != nil {
		t.Fatal(err)
	}
	for run := 0; run < 2; run++ {
		if err := migrateRuleV33(ix.db); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
	}
	var rule string
	if err := ix.db.QueryRow(`SELECT rule_id FROM event WHERE session_id='s'`).Scan(&rule); err != nil || rule != "" {
		t.Fatalf("a rule id parsed out of prose would be inference stored as fact: rule_id=%q err=%v", rule, err)
	}
	canaries, err := ix.RuntimeCanaries(ruledoc.CanaryRuleID)
	if err != nil || len(canaries) != 0 {
		t.Fatalf("a legacy prose-only row must not credit a canary: %v err=%v", canaries, err)
	}
}

func TestRuntimeCanariesCountOnlyALiveDenyByTheProofRuleFromARealRuntime(t *testing.T) {
	ix := openResultTestIndex(t)
	canary := ruledoc.CanaryRuleID
	for _, e := range []EventRecord{
		{TS: 10, SessionID: "a", Runtime: "claude", Decision: "deny", RuleID: canary, Origin: "live"},
		{TS: 20, SessionID: "a", Runtime: "claude", Decision: "deny", RuleID: canary, Origin: "live"}, // latest wins
		{TS: 30, SessionID: "a", Runtime: "claude", Decision: "deny", RuleID: "destructive-rm", Origin: "live"},
		{TS: 40, SessionID: "b", Runtime: "codex", Decision: "allow", RuleID: canary, Origin: "live"},       // would-block under enforcement off
		{TS: 50, SessionID: "c", Runtime: "demo", Decision: "deny", RuleID: canary, Origin: "live"},         // `demo` drives the hook itself
		{TS: 60, SessionID: "d", Runtime: "", Decision: "deny", RuleID: canary, Origin: "live"},             // pre-attribution hook
		{TS: 70, SessionID: "e", Runtime: "opencode", Decision: "deny", RuleID: canary, Origin: "imported"}, // lineage is not live
	} {
		e.Verb, e.Tool = "exec", "Bash"
		appendRuled(t, ix, e, nil)
	}
	canaries, err := ix.RuntimeCanaries(canary)
	if err != nil {
		t.Fatal(err)
	}
	if len(canaries) != 1 || canaries["claude"].TS != 20 || !engine.IsTypedID(canaries["claude"].EventGlobalID) {
		t.Fatalf("only claude's latest live proof-rule deny counts: %+v", canaries)
	}
	live, err := ix.RuntimeLastLive()
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 2 || live["claude"].TS != 30 || live["codex"].TS != 40 {
		t.Fatalf("firing = latest live event per real runtime, imports and demo excluded: %+v", live)
	}
}

func TestV33ChainCommitsToTheRuleAndOldRowsStillVerify(t *testing.T) {
	// A body chained before the field existed marshals without "rule", so its hash is
	// what it always was. This literal is the pre-33 canonical form.
	old := engine.EventChainBody{GlobalID: "evt_x", Seq: 1, TS: 5, Session: "s", Runtime: "claude", Verb: "exec",
		Tool: "Bash", TagsDigest: engine.TagsDigest(`[]`), Decision: "allow", Reason: "r", Origin: "live"}
	// Computed independently of this code (sha256 over "p" + the pre-33 JSON, by hand):
	// if a future field is added without omitempty, every chained row on every install
	// stops verifying, and this is the line that says so first.
	const pre33 = "9fbbc637fb8f6a3a8e25ef6ca6d98f89e0da401e7abf23eea37ea68b071ff829"
	if got := engine.HashChainEntry("p", old); got != pre33 {
		t.Fatalf("the canonical form of a pre-33 row changed: %s", got)
	}
	withRule := old
	withRule.Rule = "deny-alpha"
	if engine.HashChainEntry("p", old) == engine.HashChainEntry("p", withRule) {
		t.Fatal("the rule must be inside the hash")
	}

	ix := openResultTestIndex(t)
	a := engine.NewGenesisAnchor("claude/s1")
	appendRuled(t, ix, EventRecord{TS: 1, SessionID: "claude/s1", Runtime: "claude", Verb: "exec", Tool: "Bash",
		Decision: "allow", Reason: "no rule matched", Origin: "live"}, a)
	appendRuled(t, ix, EventRecord{TS: 2, SessionID: "claude/s1", Runtime: "claude", Verb: "exec", Tool: "Bash",
		Decision: "deny", Reason: "blocked", Origin: "live", RuleID: "deny-alpha"}, a)
	if rep, err := ix.VerifyEventChain("claude/s1", a); err != nil || rep.Status != "verified" || rep.Chained != 2 {
		t.Fatalf("ruled and unruled rows verify together: %+v err=%v", rep, err)
	}
	// Re-attributing a block to a different rule is now detectable.
	if _, err := ix.db.Exec(`UPDATE event SET rule_id='something-harmless' WHERE chain_seq=2`); err != nil {
		t.Fatal(err)
	}
	if rep, _ := ix.VerifyEventChain("claude/s1", a); rep.Status != "fork" || !strings.Contains(rep.Detail, "seq 2") {
		t.Fatalf("an edited rule_id must fork at seq 2: %+v", rep)
	}
}

func TestV33WireEventCarriesTheRule(t *testing.T) {
	ix := openResultTestIndex(t)
	a := engine.NewGenesisAnchor("claude/s1")
	appendRuled(t, ix, EventRecord{TS: 2, SessionID: "claude/s1", Runtime: "claude", Verb: "exec", Tool: "Bash",
		Decision: "deny", Reason: "blocked", Origin: "live", RuleID: "deny-alpha"}, a)
	inputs, err := ix.EventWireInputs("claude/s1")
	if err != nil || len(inputs) != 1 || inputs[0].Rule != "deny-alpha" {
		t.Fatalf("wire input must carry the rule: %+v err=%v", inputs, err)
	}
	raw, err := engine.EncodeWireEvent(inputs[0], nil)
	if err != nil || !strings.Contains(string(raw), `"rule":"deny-alpha"`) {
		t.Fatalf("payload.rule stops being always-null: %s err=%v", raw, err)
	}
	if err := schemas.Validate("event.schema.json", raw); err != nil {
		t.Fatalf("a ruled event must still be schema-valid: %v", err)
	}
}

// SQLite qualifies a partial index only when the query repeats its WHERE term literally;
// `rule_id = ?` alone does not. This reads the plan so the index cannot silently become
// decoration.
func TestRuntimeCanariesUseThePartialIndex(t *testing.T) {
	ix := openResultTestIndex(t)
	rows, err := ix.db.Query(`EXPLAIN QUERY PLAN SELECT runtime r, MAX(id) m FROM event WHERE rule_id != '' AND rule_id=? AND origin='live' AND decision='deny' AND `+nonRuntimeFilter+` GROUP BY runtime`, ruledoc.CanaryRuleID)
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
	if !strings.Contains(plan, "event_rule") {
		t.Fatalf("the canary query does not use the partial index:\n%s", plan)
	}
}
