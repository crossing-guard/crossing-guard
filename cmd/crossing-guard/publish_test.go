package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"crossing-guard/teamwire"
)

// Publishing with an offline key (team rest-of-release plan §4.3; criterion 60's
// command half).

// printedFact reads one "label: value" line of org-key's output.
func printedFact(t *testing.T, output, label string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, label+":") {
			return strings.TrimSpace(strings.TrimPrefix(line, label+":"))
		}
	}
	t.Fatalf("no %q line in:\n%s", label, output)
	return ""
}

func TestOrgKeyInitWritesOnePrivateFileAndPrintsItsPublicFacts(t *testing.T) {
	keys, data := t.TempDir(), t.TempDir()
	path := filepath.Join(keys, "acme.key")
	var out, errs bytes.Buffer
	if code := runOrgKey([]string{"init", "--out", path}, &out, &errs, []string{data}); code != 0 {
		t.Fatalf("init: %d %s", code, errs.String())
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the key file must be readable by its owner only: %v %v", info, err)
	}
	private, err := readOrgKey(path)
	if err != nil {
		t.Fatal(err)
	}
	public := private.Public().(ed25519.PublicKey)
	// The printed values are what the server computes at registration and a device
	// shows at its pin: the shared fingerprint, and the raw public key in base64.
	if got := printedFact(t, out.String(), "fingerprint"); got != teamwire.KeyFingerprint(public) {
		t.Fatalf("fingerprint = %s", got)
	}
	if got := printedFact(t, out.String(), "public key"); got != base64.StdEncoding.EncodeToString(public) {
		t.Fatalf("public key = %s", got)
	}
	if got := printedFact(t, out.String(), "key id"); got != teamwire.OrgKeyID(public) {
		t.Fatalf("key id = %s", got)
	}
	// show prints the public half again, and nothing private.
	var shown bytes.Buffer
	if code := runOrgKey([]string{"show", "--key", path}, &shown, &errs, nil); code != 0 {
		t.Fatalf("show: %d %s", code, errs.String())
	}
	for _, label := range []string{"key id", "fingerprint", "public key"} {
		if printedFact(t, shown.String(), label) != printedFact(t, out.String(), label) {
			t.Fatalf("show must print the same %s", label)
		}
	}
	keyFile, _ := os.ReadFile(path)
	body := strings.Join(strings.Split(string(keyFile), "\n")[1:3], "")
	if strings.Contains(out.String()+shown.String(), body[:40]) {
		t.Fatal("the private key must never be printed")
	}
	// The data directory holds no private key.
	if entries, _ := os.ReadDir(data); len(entries) != 0 {
		t.Fatalf("init wrote into the data directory: %v", entries)
	}
}

func TestOrgKeyInitRefusesToOverwriteAndRefusesTheDataDirectoryAndACheckout(t *testing.T) {
	keys, data, checkout := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(checkout, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(data, "bundles"), 0o700); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(keys, "existing.key")
	if err := os.WriteFile(existing, []byte("already here"), 0o600); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		existing:                       "already exists",
		filepath.Join(data, "org.key"): "data directory",
		filepath.Join(data, "bundles", "org.key"): "data directory",
		filepath.Join(checkout, "org.key"):        "git checkout",
		filepath.Join(checkout, "nested", "k"):    "git checkout",
		filepath.Join(keys, "missing", "org.key"): "does not exist",
	} {
		var out, errs bytes.Buffer
		if code := runOrgKey([]string{"init", "--out", path}, &out, &errs, []string{data}); code != 1 || !strings.Contains(errs.String(), want) {
			t.Fatalf("%s: code %d, stderr %q, want %q", path, code, errs.String(), want)
		}
		if out.Len() != 0 {
			t.Fatalf("%s: a refused init printed key facts", path)
		}
	}
	if raw, _ := os.ReadFile(existing); string(raw) != "already here" {
		t.Fatal("an existing file was overwritten")
	}
	for _, dir := range []string{data, filepath.Join(data, "bundles"), checkout, filepath.Join(checkout, "nested")} {
		entries, _ := os.ReadDir(dir)
		for _, entry := range entries {
			if !entry.IsDir() {
				t.Fatalf("a refused init wrote %s in %s", entry.Name(), dir)
			}
		}
	}
	var errs bytes.Buffer
	if code := runOrgKey([]string{"init"}, &bytes.Buffer{}, &errs, nil); code != 2 {
		t.Fatalf("a malformed command is a usage error: %d", code)
	}
}

// The round trip: init → build → sign → the signed file verifies under the key init
// printed, with the key id init printed; a tampered byte is refused. The build here is
// a stand-in for the daemon that writes what the daemon writes: an unsigned bundle
// file. The daemon's own build, pull and verification are tested in internal/daemon.
func TestOrgKeyInitBundleBuildBundleSignRoundTrip(t *testing.T) {
	keys, data := t.TempDir(), t.TempDir()
	keyPath := filepath.Join(keys, "acme.key")
	var initOut, errs bytes.Buffer
	if code := runOrgKey([]string{"init", "--out", keyPath}, &initOut, &errs, []string{data}); code != 0 {
		t.Fatalf("init: %d %s", code, errs.String())
	}
	body := `{"rules":[]}`
	unsignedPath := filepath.Join(data, "bundles", "organization-r3.json")
	var sent bundleBuildRequest
	build := func(req bundleBuildRequest) (bundleBuildResult, error) {
		sent = req
		raw, err := json.MarshalIndent(teamwire.SignedBundle{SchemaVersion: teamwire.BundleSchemaVersion, ID: "bnd_01ROUNDTRIP000000000000000",
			OrganizationID: "org_1", Scope: teamwire.BundleScope{Type: "organization", ID: "org_1"}, Revision: 3,
			CreatedAt: "2026-10-04T12:00:00Z", ExpiresAt: "2026-11-03T12:00:00Z",
			FailureMode: teamwire.BundleFailureMode{StatefulTier: req.FailureMode},
			Documents: []teamwire.SignedBundleDoc{{Kind: teamwire.BundleKindRulebook, Name: "rulebook.json",
				Digest: teamwire.BundleDocumentDigest([]byte(body)), MediaType: teamwire.BundleMediaTypeRulebook, Body: body}}}, "", "  ")
		if err != nil {
			return bundleBuildResult{}, err
		}
		if err := os.MkdirAll(filepath.Dir(unsignedPath), 0o700); err != nil {
			return bundleBuildResult{}, err
		}
		if err := os.WriteFile(unsignedPath, raw, 0o600); err != nil {
			return bundleBuildResult{}, err
		}
		return bundleBuildResult{Path: unsignedPath, SignedPath: teamwire.SignedBundlePath(unsignedPath),
			SignCommand: "crossing-guard bundle sign --key '<your organization key file>' '" + unsignedPath + "'",
			BundleID:    "bnd_01ROUNDTRIP000000000000000", Scope: "organization", Revision: 3, PublishedRevision: 2,
			FailureMode: req.FailureMode, ExpiresAt: "2026-11-03T12:00:00Z"}, nil
	}
	var buildOut bytes.Buffer
	args := []string{"build", "--scope", "organization", "--agent", "team-reviewer", "--agent", "team-helper", "--remove-agent", "old-agent",
		"--rules", "--failure-mode", "fail-closed", "--content-policy", "consent", "--expires-in", "48h"}
	if code := runBundle(args, &buildOut, &errs, build); code != 0 {
		t.Fatalf("build: %d %s", code, errs.String())
	}
	if sent.Scope != "organization" || strings.Join(sent.Agents, ",") != "team-reviewer,team-helper" || strings.Join(sent.RemoveAgents, ",") != "old-agent" ||
		!sent.Rules || sent.FailureMode != "fail-closed" || sent.ContentPolicy != "consent" || sent.ExpiresIn != "48h" || sent.Cwd == "" {
		t.Fatalf("the verb must send every flag: %+v", sent)
	}
	if !strings.Contains(buildOut.String(), unsignedPath) || !strings.Contains(buildOut.String(), "bundle sign --key") {
		t.Fatalf("build must print the file and the sign command:\n%s", buildOut.String())
	}

	var signOut bytes.Buffer
	if code := runBundle([]string{"sign", "--key", keyPath, unsignedPath}, &signOut, &errs, nil); code != 0 {
		t.Fatalf("sign: %d %s", code, errs.String())
	}
	signedPath := filepath.Join(data, "bundles", "organization-r3.signed.json")
	signed, err := os.ReadFile(signedPath)
	if err != nil {
		t.Fatalf("the signed file must sit beside the input as …-r3.signed.json: %v", err)
	}
	public, err := base64.StdEncoding.DecodeString(printedFact(t, initOut.String(), "public key"))
	if err != nil {
		t.Fatal(err)
	}
	if err := teamwire.VerifyBundleSignature(signed, ed25519.PublicKey(public)); err != nil {
		t.Fatalf("the signed bundle must verify under the key init printed: %v", err)
	}
	var doc teamwire.SignedBundle
	if err := json.Unmarshal(signed, &doc); err != nil || doc.Signature.KeyID != printedFact(t, initOut.String(), "key id") {
		t.Fatalf("the signature must name the key id init printed: %+v %v", doc.Signature, err)
	}
	tampered := []byte(strings.Replace(string(signed), "fail-closed", "fail-open", 1))
	if err := teamwire.VerifyBundleSignature(tampered, ed25519.PublicKey(public)); err == nil {
		t.Fatal("a tampered byte must be refused")
	}
	// The unsigned input is left as it was, and the data directory holds no key.
	if raw, _ := os.ReadFile(unsignedPath); strings.Contains(string(raw), `"signature"`) {
		t.Fatal("sign must not change its input")
	}
	_ = filepath.Walk(data, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			if raw, _ := os.ReadFile(path); strings.Contains(string(raw), "PRIVATE KEY") {
				t.Fatalf("the data directory holds a private key: %s", path)
			}
		}
		return nil
	})
}

func TestBundleVerbRefusals(t *testing.T) {
	never := func(bundleBuildRequest) (bundleBuildResult, error) {
		t.Fatal("a malformed command must not reach the daemon")
		return bundleBuildResult{}, nil
	}
	for _, args := range [][]string{{}, {"publish"}, {"build"}, {"build", "--agent", "a"}, {"build", "--scope"}, {"build", "--scope", "organization", "--bogus", "x"}} {
		var errs bytes.Buffer
		if code := runBundle(args, &bytes.Buffer{}, &errs, never); code != 2 {
			t.Fatalf("%v: code %d, want a usage error", args, code)
		}
	}
	dir := t.TempDir()
	notAKey := filepath.Join(dir, "not-a-key")
	if err := os.WriteFile(notAKey, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	var errs bytes.Buffer
	if code := runBundle([]string{"sign", "--key", notAKey, filepath.Join(dir, "missing.json")}, &bytes.Buffer{}, &errs, nil); code != 1 ||
		!strings.Contains(errs.String(), "not an organization key file") {
		t.Fatalf("sign with a file that is no key: %d %s", code, errs.String())
	}
}

// Red-team Low 21: `bundle build --out` copied the unsigned bundle to any path, a git
// checkout included. A path inside a checkout is refused before anything is built; a
// path outside one is written.
func TestBundleBuildRefusesAnOutPathInsideACheckout(t *testing.T) {
	checkout := t.TempDir()
	if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(checkout, "sub")
	if err := os.MkdirAll(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	never := func(bundleBuildRequest) (bundleBuildResult, error) {
		t.Fatal("a refused --out must build nothing")
		return bundleBuildResult{}, nil
	}
	var errs bytes.Buffer
	target := filepath.Join(inside, "bundle.json")
	if code := runBundle([]string{"build", "--scope", "organization", "--out", target}, &bytes.Buffer{}, &errs, never); code != 1 ||
		!strings.Contains(errs.String(), "inside the git checkout") {
		t.Fatalf("an --out inside a checkout: code %d %s", code, errs.String())
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("the bundle was written into the checkout")
	}

	data, outside := t.TempDir(), t.TempDir()
	built := filepath.Join(data, "organization-r1.json")
	if err := os.WriteFile(built, []byte(`{"id":"bnd_x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	build := func(bundleBuildRequest) (bundleBuildResult, error) {
		return bundleBuildResult{Path: built, SignCommand: "crossing-guard bundle sign --key '<key>' '" + built + "'", Scope: "organization", Revision: 1}, nil
	}
	copied := filepath.Join(outside, "bundle.json")
	errs.Reset()
	if code := runBundle([]string{"build", "--scope", "organization", "--out", copied}, &bytes.Buffer{}, &errs, build); code != 0 {
		t.Fatalf("an --out outside any checkout: code %d %s", code, errs.String())
	}
	if raw, err := os.ReadFile(copied); err != nil || string(raw) != `{"id":"bnd_x"}` {
		t.Fatalf("the copy: %s %v", raw, err)
	}
}
