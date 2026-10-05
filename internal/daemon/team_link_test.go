package daemon

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"crossing-guard/engine"
	"crossing-guard/internal/rulebook"
	"crossing-guard/internal/teamlink"
	"crossing-guard/store"
	"crossing-guard/teamwire"
)

// teamFake is a team server that accepts whatever a signed client sends and decides the
// enrollment when told to; signature verification is the teamlink package's test.
type teamFake struct {
	status atomic.Value // string: the enrollment decision the next poll reports
	pushes atomic.Int64
	orgID  string // the organization this server is; "" = org_1
}

func (f *teamFake) org() string {
	if f.orgID != "" {
		return f.orgID
	}
	return "org_1"
}

func newTeamFake(status string) *teamFake {
	f := &teamFake{}
	f.status.Store(status)
	return f
}

func (f *teamFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	switch r.URL.Path {
	case teamwire.RouteEnrollStart:
		w.WriteHeader(202)
		_ = json.NewEncoder(w).Encode(map[string]any{"user_code": "QQQQ-RRRR", "verification_url": "http://fake/approve?code=QQQQ-RRRR", "poll_interval_seconds": 1, "expires_at": time.Now().Add(time.Minute).UTC().Format(time.RFC3339)})
	case teamwire.RouteEnrollPoll:
		_ = json.NewEncoder(w).Encode(map[string]any{"status": f.status.Load(), "device": map[string]any{"approved_by": "usr_owner"}, "organization": map[string]any{"id": f.org(), "name": "Acme"}})
	case teamwire.RoutePush:
		var in teamwire.PushRequest
		_ = json.Unmarshal(body, &in)
		if len(in.Records) > 0 && in.Records[0].Kind == teamwire.KindDeviceReport {
			f.pushes.Add(1) // reports only: the outbox drain pushes on its own cadence
		}
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

func teamTestLinker(t *testing.T) (*teamLinker, *store.Index, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	ix, err := store.Open(filepath.Join(dir, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	old := governor
	governor = NewGovernor(ix, nil)
	t.Cleanup(func() { governor = old; _ = ix.Close() })
	tl := &teamLinker{storeDir: dir, now: time.Now}
	tl.reload()
	old2 := team
	team = tl
	t.Cleanup(func() { team = old2 })
	// Cleanups run last-registered first: the linker's goroutines read the
	// governor global, so they must be gone before the governor is restored.
	t.Cleanup(tl.stopJobs)
	return tl, ix, dir
}

func TestTeamLinkLifecycleWithChainedEvents(t *testing.T) {
	tl, ix, dir := teamTestLinker(t)
	if tl.state != teamUnlinked {
		t.Fatalf("fresh: %s %s", tl.state, tl.problem)
	}
	if _, err := tl.beginLink("http://team.example.com", "laptop"); err == nil {
		t.Fatal("plaintext beyond loopback must be refused")
	}
	fake := newTeamFake("pending")
	ts := httptest.NewServer(fake)
	defer ts.Close()
	p, err := tl.beginLink(ts.URL, "laptop")
	if err != nil || p.UserCode != "QQQQ-RRRR" || p.Fingerprint == "" {
		t.Fatalf("begin: %+v err=%v", p, err)
	}
	if _, err := tl.beginLink(ts.URL, "laptop"); err == nil {
		t.Fatal("a second link while one is pending must be refused")
	}
	st := tl.status()
	if st.State != teamPending || st.Pending == nil || st.Pending.UserCode != "QQQQ-RRRR" {
		t.Fatalf("status while pending: %+v", st)
	}
	fake.status.Store("approved")
	deadline := time.Now().Add(10 * time.Second)
	for tl.status().State != teamLinked && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	st = tl.status()
	if st.State != teamLinked || st.Organization.Name != "Acme" || st.ApprovedBy != "usr_owner" || st.Device.Fingerprint != p.Fingerprint {
		t.Fatalf("after approval: %+v", st)
	}
	if _, linked, _ := ix.Device(); !linked {
		t.Fatal("the store must be marked linked")
	}
	if _, found, _ := teamlink.LoadKey(dir); !found {
		t.Fatal("the key must be on disk")
	}
	if _, err := tl.beginLink(ts.URL, "other"); err == nil {
		t.Fatal("a link while linked must be refused: a good link is never swapped silently")
	}
	// The first report goes out on its own goroutine right away.
	for fake.pushes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	for tl.status().Report.Outcome == "" && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	st = tl.status()
	if fake.pushes.Load() == 0 || st.Report.Outcome != teamwire.StatusAccepted || len(st.Report.Document) == 0 {
		t.Fatalf("first report: pushes=%d %+v", fake.pushes.Load(), st.Report)
	}
	var doc teamwire.DeviceReport
	if err := json.Unmarshal(st.Report.Document, &doc); err != nil || doc.DeviceID != st.Device.ID || doc.Store.SchemaVersion != store.SchemaVersion {
		t.Fatalf("the exact document is shown: %s err=%v", st.Report.Document, err)
	}
	// Link-state transitions are chained under the daemon's own session and verify.
	rows, err := ix.EventChainRows(teamSessionID)
	if err != nil || len(rows) < 2 {
		t.Fatalf("chained link events: %d err=%v", len(rows), err)
	}
	if rep, _ := ix.VerifyEventChain(teamSessionID, nil); rep.Status != "verified" {
		t.Fatalf("link events must verify: %+v", rep)
	}
	kinds := ""
	for _, r := range rows {
		kinds += r.Body.Tool + " "
	}
	if !strings.Contains(kinds, "team.link.started") || !strings.Contains(kinds, "team.link ") {
		t.Fatalf("kinds: %s", kinds)
	}
	// Drift: a hand edit of team.json stops reporting and says so.
	raw, _ := os.ReadFile(teamlink.PathsIn(dir).Document)
	if err := os.WriteFile(teamlink.PathsIn(dir).Document, []byte(strings.Replace(string(raw), ts.URL, "https://evil.example", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	pushesBefore := fake.pushes.Load()
	tl.reportOnce()
	st = tl.status()
	if st.State != teamDrift || fake.pushes.Load() != pushesBefore || !strings.Contains(st.Problem, "changed outside the daemon") {
		t.Fatalf("drift: %+v pushes=%d", st, fake.pushes.Load())
	}
	// Unlink from a drifted state: files gone, store unlinked, the transition chained.
	acked, err := tl.unlink()
	if err != nil || !acked {
		t.Fatalf("unlink: acked=%v err=%v", acked, err)
	}
	if _, linked, _ := ix.Device(); linked {
		t.Fatal("the store must be unlinked")
	}
	if _, err := os.Stat(teamlink.PathsIn(dir).Key); !os.IsNotExist(err) {
		t.Fatal("the key must be gone")
	}
	if st := tl.status(); st.State != teamUnlinked {
		t.Fatalf("after unlink: %+v", st)
	}
	rows, _ = ix.EventChainRows(teamSessionID)
	last := rows[len(rows)-1].Body
	if last.Tool != "team.unlink" {
		t.Fatalf("last chained event %q", last.Tool)
	}
}

func TestTeamStatusRouteIsTypedAndRefusesBadLinkBodies(t *testing.T) {
	teamTestLinker(t)
	rec := httptest.NewRecorder()
	handleTeamStatus(rec, httptest.NewRequest("GET", "/api/team", nil))
	var st teamStatusResponse
	if rec.Code != 200 || json.NewDecoder(rec.Body).Decode(&st) != nil || st.State != teamUnlinked || st.Device.ID == "" || len(st.Sends) == 0 {
		t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	handleTeamLink(rec, httptest.NewRequest("POST", "/api/team/link", strings.NewReader(`{"server":"","extra":1}`)))
	if rec.Code != 400 {
		t.Fatalf("bad body: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	handleTeamLink(rec, httptest.NewRequest("POST", "/api/team/link", strings.NewReader(`{"server":"http://team.example.com"}`)))
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "loopback") {
		t.Fatalf("plaintext link: %d %s (want 400: the URL is refused client-side, not a link-state conflict)", rec.Code, rec.Body.String())
	}
}

func TestInconsistentLinkFilesSendNothing(t *testing.T) {
	tl, _, dir := teamTestLinker(t)
	key, _ := teamlink.NewKey()
	_ = teamlink.SaveKey(dir, key) // a key with no team.json and an unlinked store
	tl.reload()
	if tl.state != teamInconsistent {
		t.Fatalf("a key alone is inconsistent, got %s", tl.state)
	}
	if _, err := tl.beginLink("http://127.0.0.1:1", "x"); err == nil {
		t.Fatal("linking from an inconsistent state must be refused")
	}
	// The design's invariant is "nothing is sent": measure it, don't infer it from
	// the state name. A counting fake hears zero pushes from an inconsistent link.
	fake := newTeamFake("pending")
	ts := httptest.NewServer(fake)
	defer ts.Close()
	tl.reportOnce()
	if fake.pushes.Load() != 0 {
		t.Fatalf("an inconsistent link must send nothing, sent %d", fake.pushes.Load())
	}
}

func TestUnlinkDuringInFlightReportDiscardsTheOutcome(t *testing.T) {
	tl, ix, dir := teamTestLinker(t)
	// A fake server that blocks the push until the test has unlinked: the push is
	// in flight when the link dies, which is the race the generation guard closes.
	release := make(chan struct{})
	blocked := &teamFake{}
	blocked.status.Store("approved")
	blockedHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case teamwire.RouteEnrollStart:
			w.WriteHeader(202)
			_ = json.NewEncoder(w).Encode(map[string]any{"user_code": "QQQQ-RRRR", "verification_url": "http://fake/approve?code=QQQQ-RRRR", "poll_interval_seconds": 1, "expires_at": time.Now().Add(time.Minute).UTC().Format(time.RFC3339)})
		case teamwire.RouteEnrollPoll:
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "approved", "device": map[string]any{"approved_by": "usr_owner"}, "organization": map[string]any{"id": "org_1", "name": "Acme"}})
		case teamwire.RoutePush:
			<-release // hold the report until the test says
			blocked.pushes.Add(1)
			out := teamwire.PushResponse{ServerTime: time.Now().Unix()}
			_ = json.NewEncoder(w).Encode(out)
		case teamwire.RouteDevicesRevoke:
			w.WriteHeader(204)
		default:
			w.WriteHeader(404)
		}
	})
	ts := httptest.NewServer(blockedHandler)
	defer ts.Close()
	if _, err := tl.beginLink(ts.URL, "laptop"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for tl.status().State != teamLinked && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if tl.status().State != teamLinked {
		t.Fatalf("linked: %s", tl.status().State)
	}
	pushing := make(chan struct{})
	go func() { close(pushing); tl.reportOnce() }()
	<-pushing
	// The push is now held inside the fake. Unlink, then let the push finish.
	time.Sleep(100 * time.Millisecond)
	if _, err := tl.unlink(); err != nil {
		t.Fatal(err)
	}
	close(release)
	time.Sleep(200 * time.Millisecond)
	st := tl.status()
	if st.State != teamUnlinked {
		t.Fatalf("after unlink the state must be unlinked, got %s", st.State)
	}
	if _, err := os.Stat(teamlink.PathsIn(dir).Report); !os.IsNotExist(err) {
		t.Fatal("the report state must be gone with the link")
	}
	if st.Report.Outcome != "" || st.Report.LastAt != "" {
		t.Fatalf("a dead link's outcome must not be recorded: %+v", st.Report)
	}
	// The link event chain ends with team.unlink, not a post-unlink team.revoked.
	rows, _ := ix.EventChainRows(teamSessionID)
	last := rows[len(rows)-1].Body
	if last.Tool != "team.unlink" {
		t.Fatalf("last chained event %q, want team.unlink", last.Tool)
	}
}

func TestDriftBaselineMissingIsInconsistentNotDrift(t *testing.T) {
	tl, ix, dir := teamTestLinker(t)
	fake := newTeamFake("approved")
	ts := httptest.NewServer(fake)
	defer ts.Close()
	if _, err := tl.beginLink(ts.URL, "laptop"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for tl.status().State != teamLinked && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if tl.status().State != teamLinked {
		t.Fatalf("linked: %s", tl.status().State)
	}
	// The review's scenario is a RESTART with the baseline lost: in-memory digests
	// gone, files linked, no report state. A fresh linker over the same store is
	// that restart; a missing baseline must read inconsistent (the daemon cannot
	// tell its writes from a stranger's), never "linked", and never a drift
	// accusation.
	if err := os.Remove(teamlink.PathsIn(dir).Report); err != nil {
		t.Fatal(err)
	}
	restarted := &teamLinker{storeDir: dir, now: time.Now}
	restarted.reload()
	st := restarted.status()
	if st.State != teamInconsistent || !strings.Contains(st.Problem, "record of what it wrote is missing") {
		t.Fatalf("missing baseline after restart: %+v", st)
	}
	// And its report tick sends nothing.
	before := fake.pushes.Load()
	restarted.reportOnce()
	if fake.pushes.Load() != before {
		t.Fatal("a link with no baseline must not report")
	}
	_ = ix
}

// --- the layers pull job and adoption (item 3c part 2, design (b)) ---------------------

// layersFake serves the catalog, the signed bundle, and the document bytes — the
// 3b server's three routes, as a fake.
type layersFake struct {
	teamFake
	mu         sync.Mutex // guards catalog and signedByID once the server is running
	catalog    map[string]any
	signedByID map[string][]byte
}

func (f *layersFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/sync/v1/bundles":
		_ = json.NewEncoder(w).Encode(f.catalog)
	case "/sync/v1/bundles/document/" + strings.TrimPrefix(r.URL.Path, "/sync/v1/bundles/document/"):
		f.teamFake.ServeHTTP(w, r) // falls through to the push default
	default:
		if id := strings.TrimPrefix(r.URL.Path, "/sync/v1/bundles/"); id != r.URL.Path && f.signedByID != nil {
			if raw, ok := f.signedByID[id]; ok {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(raw)
				return
			}
		}
		f.teamFake.ServeHTTP(w, r)
	}
}

// signedOrgBundle builds one schema-valid signed bundle document (the author's
// bytes) with the given rule, signed by the given org key, and its catalog entry.
func signedOrgBundle(t *testing.T, scope, ruleID, matches string, orgKey ed25519.PrivateKey, keyID string, extra ...map[string]any) (teamwire.BundleEntry, []byte, string) {
	t.Helper()
	ruleBody, err := json.Marshal(map[string]any{
		"rules": []map[string]any{{
			"id": ruleID, "action": "deny", "message": "team rule " + ruleID,
			"if": map[string]any{"tag": "command", "matches": matches}}}})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(ruleBody)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	doc := map[string]any{
		"schema_version":  "1.0",
		"id":              "bnd_01M3HZTEST00000000000000X",
		"organization_id": "org_1",
		"scope":           map[string]any{"type": "organization", "id": "org_1"},
		"revision":        1,
		"created_at":      "2026-09-28T12:00:00Z",
		"expires_at":      time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
		"failure_mode":    map[string]any{"stateful_tier": "fail-open"},
		"documents": []map[string]any{{
			"kind": "rulebook", "name": "test rules", "digest": digest, "media_type": "application/json",
			"body": string(ruleBody)}},
		"signature": map[string]any{"algorithm": "ed25519", "key_id": keyID},
	}
	for _, more := range extra { // e.g. a content_policy the bundle's signature covers
		for k, v := range more {
			doc[k] = v
		}
	}
	raw, _ := json.Marshal(doc)
	canonical, err := teamwire.SignedCanonical(raw)
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(orgKey, canonical)
	var parsed map[string]json.RawMessage
	_ = json.Unmarshal(raw, &parsed)
	var sigDoc map[string]json.RawMessage
	_ = json.Unmarshal(parsed["signature"], &sigDoc)
	v, _ := json.Marshal(base64.StdEncoding.EncodeToString(sig))
	sigDoc["value"] = json.RawMessage(v)
	sigBytes, _ := json.Marshal(sigDoc)
	parsed["signature"] = sigBytes
	full, _ := json.Marshal(parsed)

	pubB64 := base64.StdEncoding.EncodeToString(orgKey.Public().(ed25519.PublicKey))
	entry := teamwire.BundleEntry{ID: doc["id"].(string), Scope: "organization", Revision: 1,
		ExpiresAt: doc["expires_at"].(string), FailureMode: "fail-open",
		OrgPublicKey: pubB64,
		Documents:    []teamwire.BundleDoc{{Kind: "rulebook", Name: "test rules", Digest: digest, MediaType: "application/json"}}}
	return entry, full, digest
}

// TestLayersPullVerifiesPinsAndAdopts is item 3c part 2's journey (design (b)):
// a linked device pulls the catalog, fetches the ORIGINAL signed bytes, verifies
// against the org key the server presents, chains the pin event with the key's
// fingerprint, adopts after "the diff" (the entry's rules), and un-adopts.
func TestLayersPullVerifiesPinsAndAdopts(t *testing.T) {
	tl, ix, dir := teamTestLinker(t)
	lf := &layersFake{}
	lf.teamFake.status.Store("approved")
	lf.teamFake.pushes.Add(0)
	orgPub, orgKey, err2 := ed25519.GenerateKey(rand.Reader)
	if err2 != nil {
		t.Fatal(err2)
	}
	_ = orgPub
	entry, signed, digest := signedOrgBundle(t, "organization", "org-test-deny", `org-test-bad`, orgKey, "k_test_2026")
	lf.catalog = map[string]any{"bundles": []any{map[string]any{
		"id": entry.ID, "scope": entry.Scope, "revision": entry.Revision,
		"expires_at": entry.ExpiresAt, "failure_mode": entry.FailureMode,
		"documents":      []any{map[string]any{"kind": "rulebook", "name": "test rules", "digest": digest, "media_type": "application/json"}},
		"org_public_key": entry.OrgPublicKey}}}
	lf.signedByID = map[string][]byte{entry.ID: signed}
	ts := httptest.NewServer(lf)
	defer ts.Close()

	// Link through the real flow.
	if _, err := tl.beginLink(ts.URL, "layers device"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for tl.status().State != teamLinked && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if tl.status().State != teamLinked {
		t.Fatalf("linked: %s", tl.status().State)
	}

	// The first pull runs promptly; wait for the pin event to be chained.
	for i := 0; i < 100; i++ {
		tl.pullLayersOnce()
		pinned := loadPinnedOrgKey(dir)
		if pinned.KeyID == "k_test_2026" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	pinned := loadPinnedOrgKey(dir)
	if pinned.KeyID != "k_test_2026" || pinned.PublicKey == "" || pinned.Fingerprint == "" {
		t.Fatalf("the pin was not recorded from the first verified bundle: %+v", pinned)
	}
	rows, _ := ix.EventChainRows(teamSessionID)
	kinds := ""
	for _, r := range rows {
		kinds += r.Body.Tool + " "
	}
	if !strings.Contains(kinds, "team.org-key.pinned") {
		t.Fatalf("the pin event must be chained under the daemon's session: %s", kinds)
	}

	// The catalog is now offered with the rules in words.
	tl.pullLayersOnce()
	tl.mu.Lock()
	available := tl.available
	tl.mu.Unlock()
	if len(available) != 1 || !strings.Contains(available[0].Rules[0].ID, "org-test-deny") {
		t.Fatalf("the verified bundle must be offered with its rules: %+v", available)
	}

	// Adopt: the route's own gate (verified list, staging, flip) — called directly.
	httpReq := httptest.NewRequest("POST", "/api/team/layers/adopt", strings.NewReader(`{"scope":"organization","digest":"`+available[0].ID+`","state_token":"`+available[0].StateToken+`"}`))
	rec := httptest.NewRecorder()
	handleTeamAdopt(rec, httpReq)
	if rec.Code != 200 {
		t.Fatalf("adopt: %d %s", rec.Code, rec.Body.String())
	}
	doc, err := loadLayersDoc(dir)
	record, found := doc.Bundle("org_1", "organization")
	if err != nil || !found || record.RulebookDigest == "" || record.BundleID != available[0].ID {
		t.Fatalf("the adoption record: %+v err=%v", doc.Adopted, err)
	}
	// The record points at the STAGED rulebook digest (what the loader reads);
	// the digest must be one of the bundle's documents.
	inBundle := false
	for _, d := range available[0].Documents {
		if record.RulebookDigest == d.Digest {
			inBundle = true
		}
	}
	if !inBundle {
		t.Fatalf("the adoption's digest must be a staged document of the bundle: %s", record.RulebookDigest)
	}
	// The staged body parses — the loader can use it, for a checkout the daemon
	// has resolved (the pointer index).
	if err := rulebook.SetRepositoryResolution(dir, checkoutForTest(), "repo_test"); err != nil {
		t.Fatal(err)
	}
	pol, reasons, err := LoadLayeredForTest(checkoutForTest(), dir)
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, r := range pol.Rules {
		if r.ID == "org-test-deny" && r.Layer == "organization" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the adopted layer must decide for this checkout: rules=%+v reasons=%v", pol.Rules, reasons)
	}
	if rep, _ := ix.VerifyEventChain(teamSessionID, nil); rep.Status != "verified" {
		t.Fatalf("the chain must still verify with the layer events: %+v", rep)
	}

	// Un-adopt.
	rec = httptest.NewRecorder()
	handleTeamUnadopt(rec, httptest.NewRequest("POST", "/api/team/layers/unadopt", strings.NewReader(`{"scope":"organization"}`)))
	if rec.Code != 200 {
		t.Fatalf("unadopt: %d %s", rec.Code, rec.Body.String())
	}
	doc, _ = loadLayersDoc(dir)
	if len(doc.Adopted) != 0 {
		t.Fatal("the adoption must be gone")
	}
}

// A tampered signature is not offered — the pull's core honesty.
func TestLayersPullRefusesATamperedBundle(t *testing.T) {
	tl, _, dir := teamTestLinker(t)
	lf := &layersFake{}
	lf.teamFake.status.Store("approved")
	_, orgKey, gerr := ed25519.GenerateKey(rand.Reader)
	if gerr != nil {
		t.Fatal(gerr)
	}
	entry, signed, digest := signedOrgBundle(t, "organization", "evil-rule", `evil-bad`, orgKey, "k_evil")
	// Tamper the SIGNED bytes: bump the revision inside the signed document.
	var parsed map[string]json.RawMessage
	_ = json.Unmarshal(signed, &parsed)
	parsed["revision"] = json.RawMessage(`9`)
	tampered, _ := json.Marshal(parsed)
	lf.catalog = map[string]any{"bundles": []any{map[string]any{
		"id": entry.ID, "scope": entry.Scope, "revision": 9,
		"expires_at": entry.ExpiresAt, "failure_mode": entry.FailureMode,
		"documents":      []any{map[string]any{"kind": "rulebook", "name": "test rules", "digest": digest, "media_type": "application/json"}},
		"org_public_key": entry.OrgPublicKey}}}
	lf.signedByID = map[string][]byte{entry.ID: tampered}
	ts := httptest.NewServer(lf)
	defer ts.Close()
	if _, err := tl.beginLink(ts.URL, "victim"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for tl.status().State != teamLinked && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	tl.pullLayersOnce()
	if pinned := loadPinnedOrgKey(dir); pinned.KeyID != "" {
		t.Fatal("a tampered bundle must not pin anything")
	}
	tl.mu.Lock()
	available := tl.available
	tl.mu.Unlock()
	if len(available) != 0 {
		t.Fatalf("a tampered bundle must not be offered: %+v", available)
	}
	_ = signed
}

// loadLayersDoc reads layers.json in tests (the package's own helper for its file).
func loadLayersDoc(storeDir string) (rulebook.LayersDocument, error) {
	return rulebook.LoadLayers(storeDir)
}

// LoadLayeredForTest and checkoutForTest keep the journey test honest about what
// it exercises: the loader reads the adoption records + the pointer index, with a
// checkout the daemon has resolved.
func checkoutForTest() string {
	return "/tmp/team-layer-journey-checkout"
}

func LoadLayeredForTest(cwd, storeDir string) (*engine.Policy, []string, error) {
	return rulebook.LoadLayered(cwd, storeDir)
}

// TestPinSurvivesARestartWithoutASecondPinEvent pins the postwork M-5 fold: the
// disk is the pin's home; a restart reloads it, and the next pull does not chain
// team.org-key.pinned again.
func TestPinSurvivesARestartWithoutASecondPinEvent(t *testing.T) {
	tl, ix, dir := teamTestLinker(t)
	lf := &layersFake{}
	lf.teamFake.status.Store("approved")
	orgPub, orgKey, gerr := ed25519.GenerateKey(rand.Reader)
	if gerr != nil {
		t.Fatal(gerr)
	}
	_ = orgPub
	entry, signed, _ := signedOrgBundle(t, "organization", "pin-once", `pin-once-bad`, orgKey, "k_once")
	lf.catalog = map[string]any{"bundles": []any{map[string]any{
		"id": entry.ID, "scope": entry.Scope, "revision": entry.Revision,
		"expires_at": entry.ExpiresAt, "failure_mode": entry.FailureMode,
		"documents":      []any{map[string]any{"kind": "rulebook", "name": "r", "digest": entry.Documents[0].Digest, "media_type": "application/json"}},
		"org_public_key": entry.OrgPublicKey}}}
	lf.signedByID = map[string][]byte{entry.ID: signed}
	ts := httptest.NewServer(lf)
	defer ts.Close()
	if _, err := tl.beginLink(ts.URL, "pin-once"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for tl.status().State != teamLinked && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	tl.pullLayersOnce()
	if pinned := loadPinnedOrgKey(dir); pinned.KeyID != "k_once" {
		t.Fatalf("first pull pins: %+v", pinned)
	}
	countEvents := func(kind string) int {
		rows, _ := ix.EventChainRows(teamSessionID)
		n := 0
		for _, r := range rows {
			if r.Body.Tool == kind {
				n++
			}
		}
		return n
	}
	if got := countEvents("team.org-key.pinned"); got != 1 {
		t.Fatalf("exactly one pin event: %d", got)
	}
	// The RESTART: a fresh linker over the same store; reload reads the pin from
	// disk (the M-5 fix), and the next pull sees the same pin — no second event.
	fresh := &teamLinker{storeDir: dir, now: time.Now}
	pinned := loadPinnedOrgKey(dir)
	if pinned.KeyID != "k_once" {
		t.Fatalf("the restart reload finds the pin on disk: %+v", pinned)
	}
	fresh.pinnedKey = pinned
	fresh.pullLayersOnce()
	if got := countEvents("team.org-key.pinned"); got != 1 {
		t.Fatalf("a restart must not chain a second pin event: %d", got)
	}
}

// stopJobs is the test cleanup that keeps a linker's goroutines from outliving
// the governor they read. It must wait for a running report job and refuse
// every later start, or the next test's governor swap races with it.
func TestStopJobsWaitsAndRefusesNewJobs(t *testing.T) {
	tl, _, _ := teamTestLinker(t)
	fake := newTeamFake("approved")
	// The report job's first push is held in the server until the test lets it
	// go, so stopJobs has a job in flight that it must wait for.
	pushing, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == teamwire.RoutePush {
			once.Do(func() { close(pushing) })
			<-release
		}
		fake.ServeHTTP(w, r)
	}))
	defer ts.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	if _, err := tl.beginLink(ts.URL, "laptop"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-pushing:
	case <-time.After(10 * time.Second):
		t.Fatalf("the report job never pushed; state %s", tl.status().State)
	}
	stopped := make(chan struct{})
	go func() { tl.stopJobs(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("stopJobs returned while the report job was still pushing")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("stopJobs never returned after the push finished")
	}
	tl.mu.Lock()
	running := tl.stop != nil || tl.pullStop != nil || tl.pushStop != nil
	tl.startReportingLocked()
	tl.startLayersPullLocked()
	restarted := tl.stop != nil || tl.pullStop != nil || tl.pushStop != nil
	tl.state = teamUnlinked // only the stop may refuse the enrollment below, not the link state
	tl.mu.Unlock()
	if running || restarted {
		t.Fatalf("jobs after stopJobs: before restart %v, after %v", running, restarted)
	}
	if _, err := tl.beginLink(ts.URL, "laptop"); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("a stopped linker must refuse a new enrollment: %v", err)
	}
	tl.stopJobs() // a second stop is a no-op, not a double close
}

// The Settings page's Organization key row reads pinned_org_key.fingerprint,
// .key_id and .pinned_at. Without json names the pin went out as KeyID,
// Fingerprint and PinnedAt, and the row never rendered.
func TestTeamLayersPinnedKeyWireNames(t *testing.T) {
	body, err := json.Marshal(teamLayersState{PinnedKey: orgKeyPin{KeyID: "k", PublicKey: "p", Fingerprint: "f", PinnedAt: "t"}})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Pin map[string]string `json:"pinned_org_key"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"key_id": "k", "public_key": "p", "fingerprint": "f", "pinned_at": "t"}
	if fmt.Sprint(out.Pin) != fmt.Sprint(want) {
		t.Fatalf("pinned_org_key on the wire: %s", body)
	}
	// The page's model takes the object as `pin` and reads the three fields from it.
	settings := readStatic(t, "js/views/settings-team-model.js")
	for _, field := range []string{"layers.pinned_org_key", "pin.fingerprint", "pin.key_id", "pin.pinned_at"} {
		if !strings.Contains(settings, field) {
			t.Fatalf("settings-team-model.js no longer reads %s; update this pin with the wire", field)
		}
	}
}
