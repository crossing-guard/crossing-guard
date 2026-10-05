package teamlink

import (
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"crossing-guard/engine"
	"crossing-guard/schemas"
	"crossing-guard/teamwire"
)

func TestDocumentDefaultsStrictLoadAndRoundTrip(t *testing.T) {
	dir := t.TempDir()
	d, found, err := Load(dir)
	if err != nil || found || d.Linked() || d.ReportInterval.Duration != 300*time.Second || d.Sync.Content != "off" {
		t.Fatalf("defaults: %+v found=%v err=%v", d, found, err)
	}
	d.Server, d.LinkedAt, d.Device.Name = "https://Team.Example.com:443/", "2026-09-25T12:00:00Z", "laptop"
	if err := Save(dir, d); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(PathsIn(dir).Document)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("team.json mode %v, want 0600", info.Mode().Perm())
	}
	back, found, err := Load(dir)
	if err != nil || !found || !back.Linked() || back.Device.Name != "laptop" {
		t.Fatalf("round trip: %+v found=%v err=%v", back, found, err)
	}
	if err := os.WriteFile(PathsIn(dir).Document, []byte(`{"format_version":1,"surprise":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "surprise") {
		t.Fatalf("unknown keys are errors: %v", err)
	}
	bad := d
	bad.Sync.Content = "always"
	if err := Save(dir, bad); err == nil {
		t.Fatal("an unknown content policy must not be saved")
	}
}

func TestKeyAndDigests(t *testing.T) {
	dir := t.TempDir()
	if _, found, err := LoadKey(dir); err != nil || found {
		t.Fatalf("no key yet: found=%v err=%v", found, err)
	}
	key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveKey(dir, key); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(PathsIn(dir).Key)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", info.Mode().Perm())
	}
	back, found, err := LoadKey(dir)
	if err != nil || !found || !back.Equal(key) {
		t.Fatalf("key round trip: found=%v err=%v", found, err)
	}
	before := DigestsIn(dir)
	if before.Key == "" || before.Document != "" {
		t.Fatalf("digests: %+v", before)
	}
	if err := os.WriteFile(PathsIn(dir).Key, []byte("tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if after := DigestsIn(dir); after.Key == before.Key {
		t.Fatal("a changed key file must change its digest")
	}
	if _, _, err := LoadKey(dir); err == nil {
		t.Fatal("a tampered key file is an error, not a key")
	}
	if err := Remove(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(PathsIn(dir).Key); !os.IsNotExist(err) {
		t.Fatal("Remove deletes the key")
	}
}

// fakeServer verifies every request exactly as the real server does — signature over the
// route pattern, declared body digest, window — and answers per route.
type fakeServer struct {
	t      *testing.T
	origin string
	pub    ed25519.PublicKey
	device string
	polls  int
	status string
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	ts, _ := strconv.ParseInt(r.Header.Get(teamwire.HeaderTimestamp), 10, 64)
	sr := teamwire.SignedRequest{Origin: f.origin, DeviceID: r.Header.Get(teamwire.HeaderDevice), Method: r.Method, Route: r.URL.Path,
		Timestamp: ts, Nonce: r.Header.Get(teamwire.HeaderNonce), BodySHA256: r.Header.Get(teamwire.HeaderBodyDigest)}
	if sr.DeviceID != f.device || teamwire.BodyDigest(body) != sr.BodySHA256 {
		f.t.Errorf("device or digest mismatch on %s", r.URL.Path)
	}
	if err := teamwire.Verify(f.pub, sr, r.Header.Get(teamwire.HeaderSignature)); err != nil {
		w.WriteHeader(401)
		_ = json.NewEncoder(w).Encode(teamwire.ErrorBody{Error: teamwire.Error{Code: teamwire.CodeUnauthenticated, Message: "bad signature"}})
		return
	}
	switch r.URL.Path {
	case teamwire.RouteEnrollStart:
		var in map[string]any
		if err := json.Unmarshal(body, &in); err != nil || in["public_key"] == "" || in["name"] == "" {
			w.WriteHeader(422)
			return
		}
		w.WriteHeader(202)
		_ = json.NewEncoder(w).Encode(map[string]any{"user_code": "ABCD-EFGH", "verification_url": f.origin + "/approve?code=ABCD-EFGH", "poll_interval_seconds": 1, "expires_at": time.Now().Add(time.Minute).UTC().Format(time.RFC3339)})
	case teamwire.RouteEnrollPoll:
		f.polls++
		_ = json.NewEncoder(w).Encode(map[string]any{"status": f.status, "device": map[string]any{"id": f.device, "approved_by": "usr_x"}, "organization": map[string]any{"id": "org_1", "name": "Acme"}})
	case teamwire.RoutePush:
		var in teamwire.PushRequest
		_ = json.Unmarshal(body, &in)
		out := teamwire.PushResponse{ServerTime: time.Now().Unix()}
		for _, rec := range in.Records {
			out.Results = append(out.Results, teamwire.PushResult{ID: rec.ID, Status: teamwire.StatusAccepted})
		}
		_ = json.NewEncoder(w).Encode(out)
	case teamwire.RouteDevicesRevoke:
		w.WriteHeader(204)
	default:
		w.WriteHeader(404)
	}
}

func TestClientSignsAsTheServerVerifies(t *testing.T) {
	key, _ := NewKey()
	f := &fakeServer{t: t, pub: key.Public().(ed25519.PublicKey), device: engine.NewTypedID(engine.DeviceIDPrefix), status: "pending"}
	ts := httptest.NewServer(f)
	defer ts.Close()
	f.origin, _ = teamwire.CanonicalOrigin(ts.URL)
	c, err := NewClient(ts.URL+"/", f.device, key, 5*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	start, err := c.StartEnrollment("laptop", "darwin/arm64", "test")
	if err != nil || start.UserCode != "ABCD-EFGH" || start.PollIntervalSeconds != 1 {
		t.Fatalf("start: %+v err=%v", start, err)
	}
	if poll, err := c.PollEnrollment(); err != nil || poll.Status != "pending" {
		t.Fatalf("poll: %+v err=%v", poll, err)
	}
	f.status = "approved"
	poll, err := c.PollEnrollment()
	if err != nil || poll.Status != "approved" || poll.Organization == nil || poll.Organization.Name != "Acme" || poll.Device.ApprovedBy != "usr_x" {
		t.Fatalf("approved: %+v err=%v", poll, err)
	}
	rec, _ := Record("rep_1", BuildReport(ReportFacts{DeviceID: f.device, ReportedAt: time.Now(), DaemonVersion: "t", StoreSchema: 33}))
	res, err := c.Push([]teamwire.PushRecord{rec})
	if err != nil || len(res.Results) != 1 || res.Results[0].Status != teamwire.StatusAccepted {
		t.Fatalf("push: %+v err=%v", res, err)
	}
	if err := c.RevokeSelf(); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// Another key is refused by the fake exactly as by the real server, and the client
	// surfaces the envelope's code.
	other, _ := NewKey()
	wrong, _ := NewClient(ts.URL, f.device, other, 5*time.Second, nil)
	if _, err := wrong.PollEnrollment(); Code(err) != teamwire.CodeUnauthenticated {
		t.Fatalf("wrong key: %v", err)
	}
}

func TestClientRefusesPlaintextBeyondLoopback(t *testing.T) {
	key, _ := NewKey()
	if _, err := NewClient("http://team.example.com", "dev_01M2N4T1GQXPDHEZANWXNG3N10", key, time.Second, nil); err == nil {
		t.Fatal("http beyond loopback must be refused")
	}
	// A DNS name that merely starts like a loopback address is not one: the check
	// is netip.Addr.IsLoopback() or exactly "localhost", never a string prefix.
	if _, err := NewClient("http://127.evil.example", "dev_01M2N4T1GQXPDHEZANWXNG3N10", key, time.Second, nil); err == nil {
		t.Fatal("http to a 127.-prefixed DNS name must be refused")
	}
	if _, err := NewClient("http://127.0.0.1.nip.io", "dev_01M2N4T1GQXPDHEZANWXNG3N10", key, time.Second, nil); err == nil {
		t.Fatal("http to a wildcard DNS name that resolves 127.0.0.1 must be refused: the check is the literal address, not what it resolves to")
	}
	if _, err := NewClient("http://127.0.0.1:8787", "dev_01M2N4T1GQXPDHEZANWXNG3N10", key, time.Second, nil); err != nil {
		t.Fatalf("http on loopback is the development case: %v", err)
	}
	if _, err := NewClient("http://[::1]:8787", "dev_01M2N4T1GQXPDHEZANWXNG3N10", key, time.Second, nil); err != nil {
		t.Fatalf("http on IPv6 loopback is the development case: %v", err)
	}
	if _, err := NewClient("https://team.example.com", "dev_01M2N4T1GQXPDHEZANWXNG3N10", key, time.Second, nil); err != nil {
		t.Fatalf("https anywhere: %v", err)
	}
}

func TestBuildReportIsSchemaValidAndCarriesNoFreeText(t *testing.T) {
	now := time.Now()
	at := now.Add(-time.Minute)
	facts := ReportFacts{DeviceID: engine.NewTypedID(engine.DeviceIDPrefix), ReportedAt: now, DaemonVersion: "0.0.0+abc", StoreSchema: 33,
		Runtimes:       []RuntimeFacts{{Name: "claude", Attached: true, LastLive: &at, Canary: &at}, {Name: "codex", Attached: false}},
		RulebookDigest: "sha256:" + strings.Repeat("a", 64), CanaryRuleActive: true}
	rec, err := Record("rep_x", BuildReport(facts))
	if err != nil {
		t.Fatal(err)
	}
	if err := schemas.Validate("device-report.schema.json", rec.Body); err != nil {
		t.Fatalf("the built report must be schema-valid: %v", err)
	}
	var back teamwire.DeviceReport
	_ = json.Unmarshal(rec.Body, &back)
	if len(back.Runtimes) != 2 || back.Runtimes[0].Canary.ObservedAt == nil || back.Runtimes[1].Firing.ObservedAt != nil || !back.Runtimes[0].Canary.RuleActive {
		t.Fatalf("rungs: %+v", back.Runtimes)
	}
	if len(back.Rulebook.Layers) != 1 || back.Rulebook.Layers[0].Origin != "user" {
		t.Fatalf("layers: %+v", back.Rulebook.Layers)
	}
	if strings.Contains(string(rec.Body), filepath.Join("Users")) || strings.Contains(string(rec.Body), "evidence") {
		t.Fatalf("no path, no free text: %s", rec.Body)
	}
}
