package engine

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPortableTargetNeverReturnsAnAbsolutePath(t *testing.T) {
	root, repo := "/Users/dev/work/api", "remote-sha256-v1:abc"
	in := PortableTarget("file:/Users/dev/work/api/internal/x.go", root, repo)
	if in == nil || in.Path != "internal/x.go" || in.RepositoryID == nil || *in.RepositoryID != repo || in.OutsideRepository {
		t.Fatalf("inside the checkout → repo-relative: %+v", in)
	}
	for _, outside := range []string{"file:/etc/passwd", "file:/Users/dev/.ssh/id_rsa", "file:/Users/dev/work/api-other/x.go", "file:/Users/dev/work/api"} {
		got := PortableTarget(outside, root, repo)
		if got == nil || !got.OutsideRepository || got.Path != "" || got.RepositoryID != nil || got.Name != "" {
			t.Fatalf("%s must ship as outside_repository with no path, digest, or name: %+v", outside, got)
		}
	}
	// No repository known → nothing about the path ships.
	if got := PortableTarget("file:/Users/dev/work/api/x.go", "", ""); got == nil || !got.OutsideRepository || got.Path != "" {
		t.Fatalf("unknown repository → outside_repository: %+v", got)
	}
	if got := PortableTarget("url:api.example.com", root, repo); got == nil || got.Kind != "url" || got.Name != "api.example.com" {
		t.Fatalf("url targets ship their host: %+v", got)
	}
	if got := PortableTarget("mcp:shop__getOrder", root, repo); got == nil || got.Kind != "mcp" || got.Name != "shop__getOrder" {
		t.Fatalf("mcp targets ship their tool: %+v", got)
	}
	if PortableTarget("", root, repo) != nil {
		t.Fatal("no target → null")
	}
}

func TestWireSessionIDIsStablePerDeviceAndSession(t *testing.T) {
	a := WireSessionID("dev_A", "claude/3f6c")
	if a != WireSessionID("dev_A", "claude/3f6c") || !IsTypedID(a) || !strings.HasPrefix(a, "ses_") {
		t.Fatalf("stable typed id: %q", a)
	}
	if a == WireSessionID("dev_B", "claude/3f6c") || a == WireSessionID("dev_A", "codex/3f6c") {
		t.Fatal("different device or session must differ")
	}
}

func TestRedactTextUsesOnlyTheRoleAgnosticSecretPatterns(t *testing.T) {
	dets, err := DefaultDetectors()
	if err != nil {
		t.Fatal(err)
	}
	in := "curl -H 'Authorization: Bearer abcdefghijklmnop' and key AKIAIOSFODNN7EXAMPLE"
	out, fired := RedactText(in, dets)
	if strings.Contains(out, "abcdefghijklmnop") || strings.Contains(out, "AKIAIOSFODNN7EXAMPLE") {
		t.Fatalf("secrets survived: %q", out)
	}
	if !strings.Contains(out, "[redacted:secret.bearer]") || !strings.Contains(out, "[redacted:secret.aws-key]") || len(fired) != 2 {
		t.Fatalf("markers name the detector: %q fired=%v", out, fired)
	}
	if clean, fired := RedactText("no rule matched", dets); clean != "no rule matched" || fired != nil {
		t.Fatalf("clean text is untouched: %q %v", clean, fired)
	}
}

func TestEncodeWireEventShapesALiveChainedRow(t *testing.T) {
	dets, _ := DefaultDetectors()
	frozen := `[{"key":"data-class","value":"internal","detector":"area.internal","provenance":"observed","evidence":"path-prefix"},{"key":"exec","value":"run","detector":"exec.run","provenance":"observed","evidence":"tool=Bash"}]`
	body := EventChainBody{GlobalID: NewTypedID("evt"), TS: 1789563504, Session: "claude/3f6c", Runtime: "claude", Verb: "write", Tool: "Edit",
		Target: "file:/Users/dev/work/api/internal/x.go", TagsDigest: TagsDigest(frozen), Decision: "deny",
		Reason: "Blocked: Authorization: Bearer abcdefghijklmnop leaked", Origin: "live"}
	anchor := NewGenesisAnchor("claude/3f6c")
	prev, hash := anchor.Advance(&body)
	raw, err := EncodeWireEvent(WireEventInput{DeviceID: "dev_A", GlobalID: body.GlobalID, TS: body.TS, ReceivedAt: body.TS + 1,
		Session: "claude/3f6c", Runtime: "claude", CatalogID: "cat-1", Verb: "write", Tool: "Edit",
		TargetEntityID: body.Target, FrozenTags: frozen, Decision: "deny", Reason: body.Reason, Origin: "live",
		ChainSeq: body.Seq, PrevHash: prev, Hash: hash, RepositoryID: "remote-sha256-v1:abc", CheckoutRoot: "/Users/dev/work/api"}, dets)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(raw); strings.Contains(s, "/Users/") || strings.Contains(s, "abcdefghijklmnop") {
		t.Fatalf("an absolute path or a secret shipped: %s", s)
	}
	var ev WireEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Type != "action.write" || ev.Sequence != 1 || ev.Chain == nil || ev.Chain.Hash != hash || ev.Provenance.ContentHash != "sha256:"+hash {
		t.Fatalf("chain/type: %+v", ev)
	}
	if len(ev.Sensitivity) != 1 || ev.Sensitivity[0] != "internal" || ev.Visibility != "organization" {
		t.Fatalf("sensitivity comes from data-class tags: %+v", ev.Sensitivity)
	}
	if ev.Payload.Target == nil || ev.Payload.Target.Path != "internal/x.go" || ev.Session.NativeID != "3f6c" || ev.Session.CatalogID != "cat-1" {
		t.Fatalf("target/session: %+v %+v", ev.Payload.Target, ev.Session)
	}
	// The redacted reason ships, and the chain — computed over the ORIGINAL reason and the
	// tags digest — still verifies: verification never needs what was redacted.
	if !strings.Contains(ev.Payload.Reason, "[redacted:secret.bearer]") || ev.Payload.TagsDigest != TagsDigest(frozen) {
		t.Fatalf("reason/digest: %q %q", ev.Payload.Reason, ev.Payload.TagsDigest)
	}
	if r := VerifyEventChain("claude/3f6c", []ChainRow{{Body: body, Prev: prev, Hash: hash}}, anchor); r.Status != "verified" {
		t.Fatalf("chain must verify: %+v", r)
	}
}

func TestEncodeWireEventLegacyRowHasNoChainAndUnknownSensitivity(t *testing.T) {
	raw, err := EncodeWireEvent(WireEventInput{DeviceID: "dev_A", GlobalID: DeterministicTypedID("evt", "dev_A:7"), TS: 100,
		Session: "codex/legacy", Verb: "exec", Tool: "Bash", FrozenTags: "", Origin: "imported"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var ev WireEvent
	_ = json.Unmarshal(raw, &ev)
	if ev.Chain != nil || ev.Sequence != 0 || ev.Provenance.Kind != "imported" || ev.Sensitivity[0] != "unknown" || ev.Payload.Target != nil {
		t.Fatalf("legacy row: %s", raw)
	}
	if _, err := EncodeWireEvent(WireEventInput{GlobalID: "obs_24d10dff5a6cc781", Session: "x/y"}, nil); err == nil {
		t.Fatal("a non-typed id must be refused, never shipped")
	}
}

func TestWireSessionIsOneIDWhateverLocalStringNamedIt(t *testing.T) {
	encode := func(session, runtime string) WireEvent {
		raw, err := EncodeWireEvent(WireEventInput{DeviceID: "dev_A", GlobalID: NewTypedID("evt"), TS: 1,
			Session: session, Runtime: runtime, Verb: "exec", Origin: "live"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		var ev WireEvent
		_ = json.Unmarshal(raw, &ev)
		return ev
	}
	bare := encode("3f6c1a2e", "claude")       // a live hook: bare native id, runtime in its own column
	composite := encode("claude/3f6c1a2e", "") // an import: "<vendor>/<id>"
	both := encode("claude/3f6c1a2e", "claude")
	if bare.Session.ID != composite.Session.ID || composite.Session.ID != both.Session.ID {
		t.Fatalf("one session, one wire id: %s %s %s", bare.Session.ID, composite.Session.ID, both.Session.ID)
	}
	if bare.Session.NativeID != "3f6c1a2e" || bare.Session.Runtime != "claude" || composite.Session.NativeID != "3f6c1a2e" {
		t.Fatalf("normalized identities: %+v %+v", bare.Session, composite.Session)
	}
}

// The §7.1 reservation: a sandbox-spooled observation encodes under its own provenance
// kind and the result is schema-valid wire JSON (the enum carries the value).
func TestEncodeWireEventCarriesTheSandboxHookProvenance(t *testing.T) {
	raw, err := EncodeWireEvent(WireEventInput{DeviceID: "dev_A", GlobalID: DeterministicTypedID("evt", "dev_A:sandbox"), TS: 100,
		Session: "claude/s1", Runtime: "claude", Verb: "exec", Tool: "Bash", Decision: "allow", Origin: "sandbox-hook"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"kind":"sandbox-hook"`) {
		t.Fatalf("provenance kind not carried: %s", raw)
	}
}

// Postwork C1: frozen evidence from the real classifier carries the absolute path the
// hook saw ("path=/Users/…"); on the wire it is repository-relative inside the checkout
// and "[absolute path]" outside it. Built from Classify's own output, not a literal.
func TestEncodeWireEventNeverShipsAnAbsolutePathInTags(t *testing.T) {
	dets, err := DefaultDetectors()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ path, want string }{
		{"/Users/dev/work/api/docs/guide.md", "path=docs/guide.md"},
		{"/Users/dev/notes/private.md", "path=[absolute path]"},
	} {
		tags := Classify(Event{Tool: "Read", Path: c.path, Role: "tool_call"}, dets)
		found := false
		for _, tg := range tags {
			found = found || strings.HasPrefix(tg.Evidence, "path=")
		}
		if !found {
			t.Fatalf("fixture premise: the classifier must freeze path evidence for %s: %+v", c.path, tags)
		}
		frozen, _ := json.Marshal(tags)
		raw, err := EncodeWireEvent(WireEventInput{DeviceID: "dev_A", GlobalID: DeterministicTypedID("evt", c.path), TS: 1, Session: "claude/s",
			Runtime: "claude", Verb: "read", Tool: "Read", TargetEntityID: "file:" + c.path, FrozenTags: string(frozen), Decision: "allow",
			Reason: "read " + c.path, Origin: "live", RepositoryID: "remote-sha256-v1:r", CheckoutRoot: "/Users/dev/work/api"}, dets)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "/Users/") || !strings.Contains(string(raw), c.want) {
			t.Fatalf("%s: %s", c.path, raw)
		}
	}
}
