package teamwire

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crossing-guard/schemas"
)

// The shared vector. The key is the Ed25519 key whose seed is bytes 1..32; Ed25519 is
// deterministic, so the signature is a constant any implementation must reproduce. The
// same values are printed in docs/specifications/synchronization-protocol.md.
var vectorRequest = SignedRequest{
	Origin: "https://team.example.com", DeviceID: "dev_01M2N4T1GQXPDHEZANWXNG3N10", Method: "POST",
	Route: RoutePush, Timestamp: 1789660800, Nonce: "AAECAwQFBgcICQoLDA0ODw",
	BodySHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
}

const vectorCanonical = "crossing-guard-device-v1\n" +
	"https://team.example.com\n" +
	"dev_01M2N4T1GQXPDHEZANWXNG3N10\n" +
	"POST\n" +
	"/sync/v1/push\n" +
	"1789660800\n" +
	"AAECAwQFBgcICQoLDA0ODw\n" +
	"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// Both constants were produced by the RFC 8032 reference implementation in Python — not by
// this package — so the test checks Go against an independent implementation, not against
// itself.
const (
	vectorSignature   = "5nmEpKF49rCnBo-lZ0cEdhIoM4jaHKZ7ZlHAMGzBzKTxdfhlgKbsY-FB_IHDCuXsQ4e2dL9EDH4rHjzcyYqcBQ"
	vectorFingerprint = "CPV0-CWYP-XP44-QW0W-5GH2"
)

func vectorKey() ed25519.PrivateKey {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	return ed25519.NewKeyFromSeed(seed)
}

func TestSigningVector(t *testing.T) {
	canonical, err := vectorRequest.Canonical()
	if err != nil || string(canonical) != vectorCanonical {
		t.Fatalf("canonical string drifted:\n%s\nerr=%v", canonical, err)
	}
	if BodyDigest(nil) != vectorRequest.BodySHA256 || BodyDigest([]byte{}) != vectorRequest.BodySHA256 {
		t.Fatal("an empty body hashes to the sha256 of the empty string")
	}
	key := vectorKey()
	sig, err := Sign(key, vectorRequest)
	if err != nil {
		t.Fatal(err)
	}
	if sig != vectorSignature {
		t.Fatalf("signature vector drifted: %s", sig)
	}
	pub := key.Public().(ed25519.PublicKey)
	if err := Verify(pub, vectorRequest, sig); err != nil {
		t.Fatalf("vector must verify: %v", err)
	}
	if got := KeyFingerprint(pub); got != vectorFingerprint || len(strings.ReplaceAll(got, "-", "")) != 20 {
		t.Fatalf("fingerprint drifted: %s", got)
	}
}

// Every signed field is load-bearing: changing any one of them must break the signature.
func TestASignatureIsBoundToEveryField(t *testing.T) {
	key := vectorKey()
	pub := key.Public().(ed25519.PublicKey)
	sig, _ := Sign(key, vectorRequest)
	for name, mutate := range map[string]func(*SignedRequest){
		"another server":  func(r *SignedRequest) { r.Origin = "https://staging.example.com" },
		"another device":  func(r *SignedRequest) { r.DeviceID = "dev_01M2N4T1GQXPDHEZANWXNG3N11" },
		"another method":  func(r *SignedRequest) { r.Method = "PUT" },
		"another route":   func(r *SignedRequest) { r.Route = RouteDevicesRevoke },
		"another second":  func(r *SignedRequest) { r.Timestamp++ },
		"another nonce":   func(r *SignedRequest) { r.Nonce = "AAECAwQFBgcICQoLDA0ODx" },
		"another payload": func(r *SignedRequest) { r.BodySHA256 = BodyDigest([]byte("x")) },
	} {
		r := vectorRequest
		mutate(&r)
		if err := Verify(pub, r, sig); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: want ErrBadSignature, got %v", name, err)
		}
	}
	other := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	if err := Verify(other, vectorRequest, sig); !errors.Is(err, ErrBadSignature) {
		t.Errorf("another key: %v", err)
	}
}

// A field that could smuggle a newline would let one request's fields be read as another's.
func TestCanonicalRefusesAnythingOutsideTheGrammar(t *testing.T) {
	for name, mutate := range map[string]func(*SignedRequest){
		"newline in route":           func(r *SignedRequest) { r.Route = "/sync/v1/push\nPOST" },
		"query string":               func(r *SignedRequest) { r.Route = "/sync/v1/push?x=1" },
		"received path, not pattern": func(r *SignedRequest) { r.Route = "/guard/sync/v1/../v1/push%2f" },
		"non-canonical origin":       func(r *SignedRequest) { r.Origin = "https://Team.Example.com:443/" },
		"not a device id":            func(r *SignedRequest) { r.DeviceID = "evt_01M2N4T1GQXPDHEZANWXNG3N10" },
		"lowercase method":           func(r *SignedRequest) { r.Method = "post" },
		"zero timestamp":             func(r *SignedRequest) { r.Timestamp = 0 },
		"thirteen-digit time":        func(r *SignedRequest) { r.Timestamp = 1_000_000_000_000 },
		"short nonce":                func(r *SignedRequest) { r.Nonce = "short" },
		"uppercase body digest":      func(r *SignedRequest) { r.BodySHA256 = strings.ToUpper(r.BodySHA256) },
	} {
		r := vectorRequest
		mutate(&r)
		if _, err := r.Canonical(); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: want ErrMalformed, got %v", name, err)
		}
	}
	if err := Verify(vectorKey().Public().(ed25519.PublicKey), vectorRequest, "not base64url!"); !errors.Is(err, ErrMalformed) {
		t.Errorf("signature encoding: %v", err)
	}
}

// The replay rule. A request stamped at the FAR FUTURE edge of the window stays acceptable
// for Window + ForwardSkew after it is first seen. Forgetting its nonce "Window after
// receipt" — the obvious implementation — lets it be replayed in the gap.
func TestNonceMustOutliveTheRequestNotTheReceipt(t *testing.T) {
	f := Freshness{Window: 300 * time.Second, ForwardSkew: 30 * time.Second}
	now := int64(1_789_660_800)
	stamped := now + 30 // the far edge: a clock running fast, or an attacker choosing it
	if err := f.Check(stamped, now); err != nil {
		t.Fatalf("far-edge request is acceptable at first sight: %v", err)
	}
	naivePrune := now + 300 // "window after receipt"
	if err := f.Check(stamped, naivePrune+1); err != nil {
		t.Fatalf("test premise: the request is still acceptable after a receipt-based prune: %v", err)
	}
	if expiry := f.NonceExpiry(stamped); expiry <= naivePrune+1 {
		t.Fatalf("nonce expiry %d must outlive the last acceptable second", expiry)
	}
	last := stamped + 300
	if f.Check(stamped, last) != nil || !errors.Is(f.Check(stamped, last+1), ErrTimestampOutOfWindow) {
		t.Fatal("acceptable through ts+Window, refused after")
	}
	if f.NonceExpiry(stamped) != last+1 {
		t.Fatalf("the nonce may be forgotten exactly when the request can no longer pass: %d vs %d", f.NonceExpiry(stamped), last+1)
	}
	if !errors.Is(f.Check(now+31, now), ErrTimestampOutOfWindow) || !errors.Is(f.Check(now-301, now), ErrTimestampOutOfWindow) {
		t.Fatal("both edges are closed one second out")
	}
}

func TestCanonicalOrigin(t *testing.T) {
	for in, want := range map[string]string{
		"https://Team.Example.com":        "https://team.example.com",
		"https://team.example.com:443/":   "https://team.example.com",
		"https://team.example.com:8443":   "https://team.example.com:8443",
		"https://corp.example.com/guard/": "https://corp.example.com", // a path prefix is a deployment detail
		"http://127.0.0.1:8787":           "http://127.0.0.1:8787",
		"http://localhost:80":             "http://localhost",
		"http://[::1]:8787":               "http://[::1]:8787",
		" https://team.example.com ":      "https://team.example.com",
	} {
		if got, err := CanonicalOrigin(in); err != nil || got != want {
			t.Errorf("%q → %q err=%v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "team.example.com", "ftp://team.example.com", "https://user:pw@team.example.com",
		"https://team.example.com?x=1", "https://team.example.com#f", "https://", "https://tëam.example.com"} {
		if got, err := CanonicalOrigin(bad); err == nil {
			t.Errorf("%q must be refused, got %q", bad, got)
		}
	}
}

func TestNonceShape(t *testing.T) {
	a, err := NewNonce()
	b, _ := NewNonce()
	if err != nil || a == b || !nonceShape.MatchString(a) || len(a) != 22 {
		t.Fatalf("nonce %q %q err=%v", a, b, err)
	}
}

// The Go type and the JSON Schema are two descriptions of one document; this is what keeps
// them one. The fixture decodes strictly, re-encodes, and must still be schema-valid.
func TestDeviceReportTypeMatchesTheSchema(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "schemas", "fixtures", "valid", "device-report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report DeviceReport
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&report); err != nil {
		t.Fatalf("the fixture has a field the type lacks: %v", err)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := schemas.Validate("device-report.schema.json", encoded); err != nil {
		t.Fatalf("the type emits something the schema rejects: %v", err)
	}
	var a, b any
	_ = json.Unmarshal(raw, &a)
	_ = json.Unmarshal(encoded, &b)
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatalf("round trip lost or invented data:\n%s\n%s", ja, jb)
	}
}

func TestRuntimeStandingLadder(t *testing.T) {
	now := time.Date(2026, 9, 17, 16, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *string { s := now.Add(-d).Format(time.RFC3339); return &s }
	garbage := "yesterday-ish"
	fresh := 30 * 24 * time.Hour
	for _, tc := range []struct {
		name string
		r    RuntimeReport
		want Standing
	}{
		{"hook gone, however recently it fired", RuntimeReport{Attached: false, Firing: Observed{at(time.Minute)},
			Canary: CanaryReport{at(time.Minute), true}}, StandingNotAttached},
		{"registration is not coverage", RuntimeReport{Attached: true, Canary: CanaryReport{nil, true}}, StandingNeverFired},
		{"firing, but the rulebook has no proof rule", RuntimeReport{Attached: true, Firing: Observed{at(time.Hour)},
			Canary: CanaryReport{at(time.Hour), false}}, StandingNoCanaryRule},
		{"firing, never proven", RuntimeReport{Attached: true, Firing: Observed{at(time.Hour)},
			Canary: CanaryReport{nil, true}}, StandingCanaryNever},
		{"proven long ago", RuntimeReport{Attached: true, Firing: Observed{at(time.Hour)},
			Canary: CanaryReport{at(31 * 24 * time.Hour), true}}, StandingCanaryStale},
		{"proven", RuntimeReport{Attached: true, Firing: Observed{at(time.Hour)},
			Canary: CanaryReport{at(29 * 24 * time.Hour), true}}, StandingEnforced},
		{"an unparseable time is never trusted", RuntimeReport{Attached: true, Firing: Observed{at(time.Hour)},
			Canary: CanaryReport{&garbage, true}}, StandingCanaryNever},
	} {
		if got := RuntimeStanding(tc.r, now, fresh); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
	proven := RuntimeReport{Attached: true, Firing: Observed{at(time.Hour)}, Canary: CanaryReport{at(time.Minute), true}}
	if got := RuntimeStanding(proven, now, 0); got != StandingCanaryStale {
		t.Errorf("a reader with no window must fail closed to stale, got %s", got)
	}
}

// The portability fence. teamwire is imported by a server in another module; the moment it
// pulls in anything local-install-shaped, that server inherits it. The only non-standard
// imports allowed are this module's own portable packages.
func TestTeamwireStaysPortable(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	allowed := map[string]bool{"crossing-guard/teamwire": true, "crossing-guard/engine": true}
	for _, dep := range strings.Fields(string(out)) {
		if !allowed[dep] {
			t.Errorf("teamwire must stay portable, but it depends on %s", dep)
		}
	}
}

// Every item-4 body type marshals to its schema: the valid fixture decodes into the Go
// type and re-encodes schema-valid — the type and the schema cannot drift apart silently.
func TestSessionReviewBodiesRoundTripTheirSchemas(t *testing.T) {
	for file, into := range map[string]any{"session.json": &SessionRecord{}, "session-checkpoint-fact.json": &CheckpointFact{},
		"session-content.json": &ContentChunk{}} {
		raw, err := os.ReadFile(filepath.Join("..", "schemas", "fixtures", "valid", file))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, into); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		out, err := json.Marshal(into)
		if err != nil {
			t.Fatal(err)
		}
		schema := strings.TrimSuffix(file, ".json") + ".schema.json"
		if v, err := schemas.Violations(schema, out); err != nil || len(v) > 0 {
			t.Fatalf("%s re-encoded is not schema-valid: %v %v\n%s", file, err, v, out)
		}
	}
	for _, k := range []string{KindDeviceReport, KindEvent, KindSession, KindCheckpointFact, KindSessionContent, KindMemory, KindTombstone} {
		if SchemaForKind(k) == "" {
			t.Errorf("kind %s names no schema", k)
		}
	}
}

// The encoder's chunk cap restates the schema's; the two cannot drift apart silently.
func TestContentChunkCapMatchesTheSchema(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "schemas", "session-content.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Properties struct {
			Body struct {
				MaxLength int `json:"maxLength"`
			} `json:"body"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || doc.Properties.Body.MaxLength != ContentChunkMaxBytes {
		t.Fatalf("schema body maxLength %d, ContentChunkMaxBytes %d (%v)", doc.Properties.Body.MaxLength, ContentChunkMaxBytes, err)
	}
}

// Item 5: the memory record (1.1) and tombstone bodies round-trip their schemas, and the
// wire hash ignores exactly content_hash and base_content_hash — the one hash both sides
// compute with this function (decision 13c).
func TestMemoryBodiesRoundTripAndHashAgreement(t *testing.T) {
	for file, into := range map[string]any{"memory.json": &MemoryRecord{}, "tombstone.json": &Tombstone{}, "tombstone-session-content.json": &Tombstone{}} {
		raw, err := os.ReadFile(filepath.Join("..", "schemas", "fixtures", "valid", file))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, into); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		out, err := json.Marshal(into)
		if err != nil {
			t.Fatal(err)
		}
		schema := "tombstone.schema.json"
		if file == "memory.json" {
			schema = "memory.schema.json"
		}
		if v, err := schemas.Violations(schema, out); err != nil || len(v) > 0 {
			t.Fatalf("%s re-encoded is not schema-valid: %v %v\n%s", file, err, v, out)
		}
	}
	var r MemoryRecord
	raw, _ := os.ReadFile(filepath.Join("..", "schemas", "fixtures", "valid", "memory.json"))
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	h := MemoryWireHash(r)
	r.ContentHash, r.BaseContentHash = "sha256:"+strings.Repeat("0", 64), ""
	if MemoryWireHash(r) != h {
		t.Fatal("content_hash and base_content_hash must not enter the wire hash")
	}
	r.Body += "."
	if MemoryWireHash(r) == h {
		t.Fatal("the body must enter the wire hash")
	}
	if MemoryPushID("mem_X", 7) != "mem_X@7" {
		t.Fatal("push id shape")
	}
}
