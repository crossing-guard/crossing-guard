package store

import (
	"strings"
	"testing"

	"crossing-guard/engine"
	"crossing-guard/schemas"
)

// The contract test of team plan §5.13: a REAL event, written by the one writer and read
// back by the store, encodes to wire JSON that the embedded event schema accepts — and
// carries no absolute path and no secret. If the encoder and the schema ever drift, this
// is where it shows.
func TestStoredEventsEncodeToSchemaValidWireJSON(t *testing.T) {
	ix := openResultTestIndex(t)
	const session = "claude/3f6c1a2e"
	const root = "/Users/dev/work/api"
	repo := "remote-sha256-v1:" + strings.Repeat("c", 64)

	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := tx.EnsureSessionCheckpoint(SessionCheckpoint{SessionID: session, ScopeKey: "checkout:wire",
		Kind: "settled", RequestID: "wire-checkpoint", WorkingDirectory: root, RepositoryID: repo,
		CheckoutID: "checkout", CheckoutRoot: root, Status: "pending", BoundaryClass: "settled", RequestedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	anchor := engine.NewGenesisAnchor(session)
	appendOne := func(e EventRecord, a *engine.ChainAnchor) {
		t.Helper()
		tx, err := ix.BeginGov()
		if err != nil {
			t.Fatal(err)
		}
		var work *engine.ChainAnchor
		if a != nil {
			c := *a
			work = &c
		}
		if _, err := tx.AppendEvent(e, work); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if a != nil {
			*a = *work
		}
	}
	inside := `[{"key":"data-class","value":"internal","detector":"area.internal","provenance":"observed","evidence":"path-prefix"}]`
	appendOne(EventRecord{TS: 1789563504, SessionID: session, Runtime: "claude", Verb: "write", Tool: "Edit",
		TargetEntityID: "file:" + root + "/internal/x.go", Tags: inside, Decision: "deny",
		Reason: "Blocked: Authorization: Bearer abcdefghijklmnop", Origin: "live"}, anchor)
	appendOne(EventRecord{TS: 1789563505, SessionID: session, Runtime: "claude", Verb: "read", Tool: "Read",
		TargetEntityID: "file:/etc/hosts", Tags: "[]", Decision: "allow", Reason: "no rule matched", Origin: "live"}, anchor)
	appendOne(EventRecord{TS: 1789563400, SessionID: session, Runtime: "claude", Verb: "exec", Tool: "Bash",
		Tags: "", Origin: "imported"}, nil) // unchained, as a legacy/imported row is

	dets, err := engine.DefaultDetectors()
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := ix.EventWireInputs(session)
	if err != nil || len(inputs) != 3 {
		t.Fatalf("want 3 inputs, got %d err=%v", len(inputs), err)
	}
	var sessionIDs []string
	for i, in := range inputs {
		if in.RepositoryID != repo || in.CheckoutRoot != root || !strings.HasPrefix(in.DeviceID, "dev_") {
			t.Fatalf("input %d lacks repository/device context: %+v", i, in)
		}
		raw, err := engine.EncodeWireEvent(in, dets)
		if err != nil {
			t.Fatalf("encode %d: %v", i, err)
		}
		if err := schemas.Validate("event.schema.json", raw); err != nil {
			t.Fatalf("event %d is not schema-valid: %v\n%s", i, err, raw)
		}
		body := string(raw)
		for _, leak := range []string{"/Users/", "/etc/", root, "abcdefghijklmnop"} {
			if strings.Contains(body, leak) {
				t.Fatalf("event %d shipped %q: %s", i, leak, body)
			}
		}
		sessionIDs = append(sessionIDs, engine.WireSessionID(in.DeviceID, in.Session))
	}
	if sessionIDs[0] != sessionIDs[1] || sessionIDs[1] != sessionIDs[2] {
		t.Fatalf("one session must have one wire id: %v", sessionIDs)
	}
	first, _ := engine.EncodeWireEvent(inputs[0], dets)
	for _, want := range []string{`"path":"internal/x.go"`, `[redacted:secret.bearer]`, `"sensitivity":["internal"]`, `"seq":1`} {
		if !strings.Contains(string(first), want) {
			t.Fatalf("first event lacks %s: %s", want, first)
		}
	}
	second, _ := engine.EncodeWireEvent(inputs[1], dets)
	if !strings.Contains(string(second), `"outside_repository":true`) || strings.Contains(string(second), `"path"`) {
		t.Fatalf("out-of-repo target must ship with no path: %s", second)
	}
	third, _ := engine.EncodeWireEvent(inputs[2], dets)
	if !strings.Contains(string(third), `"chain":null`) || !strings.Contains(string(third), `"kind":"imported"`) {
		t.Fatalf("unchained row: %s", third)
	}
	// Redaction changed what shipped, not what was chained.
	if rep, _ := ix.VerifyEventChain(session, anchor); rep.Status != "verified" || rep.Chained != 2 || rep.Legacy != 1 {
		t.Fatalf("chain must still verify after encoding with redaction: %+v", rep)
	}
}
