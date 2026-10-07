package daemon

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crossing-guard/internal/orchestration/profilefs"
	"crossing-guard/teamwire"
)

// Building a bundle to publish (team rest-of-release plan §4.3; criteria 60 and 93).

func (rig *adoptionRig) build(body string) (teamBundleBuildResponse, int, string) {
	rig.t.Helper()
	rec := rig.post(handleTeamBundleBuild, body)
	var out teamBundleBuildResponse
	if rec.Code == 200 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			rig.t.Fatal(err)
		}
	}
	return out, rec.Code, rec.Body.String()
}

// signBuilt signs a built file the way `bundle sign` does and returns the signed
// document.
func signBuilt(t *testing.T, path string, key ed25519.PrivateKey) []byte {
	t.Helper()
	unsigned, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := teamwire.SignBundle(unsigned, key, teamwire.OrgKeyID(key.Public().(ed25519.PublicKey)))
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func TestBundleBuildWritesAnUnsignedFileInTheDataDirectoryAndADeviceAcceptsItSigned(t *testing.T) {
	rig := newAdoptionRig(t)
	key := testOrgKey(t)
	source := testAgentSource(t, "team-reviewer", "Built by the lead.")
	selectOwnAgent(t, rig.owner(), source)

	built, code, body := rig.build(`{"scope":"organization","agents":["team-reviewer"]}`)
	if code != 200 {
		t.Fatalf("build: %d %s", code, body)
	}
	// Criterion 93: a fixed, product-owned folder; the sign command contains the path.
	wantPath := filepath.Join(rig.dir, "bundles", "organization-r1.json")
	if built.Path != wantPath || built.SignedPath != filepath.Join(rig.dir, "bundles", "organization-r1.signed.json") ||
		!strings.Contains(built.SignCommand, built.Path) || !strings.HasPrefix(built.SignCommand, "crossing-guard bundle sign --key ") {
		t.Fatalf("built = %+v", built)
	}
	info, err := os.Stat(built.Path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the built file must exist, private to the user: %v %v", info, err)
	}
	raw, _ := os.ReadFile(built.Path)
	if strings.Contains(string(raw), `"signature"`) {
		t.Fatal("the built file must be unsigned")
	}
	// A scope's first bundle: revision 1, a fresh bnd_ id, schema 1.1, and the
	// defaults of team.json (fail-open, 720 h), with no content policy.
	expires, _ := time.Parse(time.RFC3339, built.ExpiresAt)
	if built.Revision != 1 || built.PublishedRevision != 0 || !strings.HasPrefix(built.BundleID, "bnd_") ||
		built.SchemaVersion != "1.1" || built.FailureMode != "fail-open" || len(built.ContentPolicy) != 0 ||
		time.Until(expires) < 719*time.Hour || time.Until(expires) > 721*time.Hour {
		t.Fatalf("first-bundle defaults: %+v", built)
	}
	if len(built.Documents) != 1 || built.Documents[0].Change != builtDocumentAdded || built.Documents[0].ProfileID != "team-reviewer" ||
		!textChanged(built.Documents[0].TextDiff) {
		t.Fatalf("documents = %+v", built.Documents)
	}

	// Signed as `bundle sign` signs, the device's own pull verifies and offers it.
	signed := signBuilt(t, built.Path, key)
	rig.serve(served{signed, key})
	offer := offerOf(t, rig.pull(), built.BundleID)
	if offer.Revision != 1 || offer.Documents[0].Change != profilefs.TeamDocumentUnchanged {
		t.Fatalf("the lead's device must be offered its own bundle: %+v", offer)
	}
	// One changed byte in the signed document is refused.
	tampered := []byte(strings.Replace(string(signed), `"revision": 1`, `"revision": 2`, 1))
	if string(tampered) == string(signed) {
		t.Fatal("the tamper did not change the document")
	}
	entry := catalogEntryOf(t, served{tampered, key})
	rig.linker.mu.Lock()
	rig.linker.available = nil
	rig.linker.mu.Unlock()
	if err := clearPinnedOrgKey(rig.dir); err != nil {
		t.Fatal(err)
	}
	rig.serveCatalog([]any{entry}, map[string][]byte{built.BundleID: tampered})
	if state := rig.pull(); len(state.Available) != 0 || !reasonsMention(state, built.BundleID, "signature") {
		t.Fatalf("a tampered bundle must be refused: %+v", state)
	}
}

func TestBundleBuildCarriesForwardAndReportsTheDiff(t *testing.T) {
	rig := newAdoptionRig(t)
	key := testOrgKey(t)
	selectOwnAgent(t, rig.owner(), testAgentSource(t, "team-reviewer", "Version one."))
	selectOwnAgent(t, rig.owner(), testAgentSource(t, "team-helper", "A helper."))
	rules := filepath.Join(t.TempDir(), "rules.json")
	if err := os.WriteFile(rules, []byte(`{"rules":[{"id":"lead-deny","action":"deny","message":"no","if":{"tag":"command","matches":"lead-bad"}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_RULES", rules)

	first, code, body := rig.build(`{"scope":"organization","agents":["team-reviewer","team-helper"],"failure_mode":"fail-closed","content_policy":"consent","expires_in":"48h"}`)
	if code != 200 {
		t.Fatalf("build: %d %s", code, body)
	}
	expires, _ := time.Parse(time.RFC3339, first.ExpiresAt)
	if first.FailureMode != "fail-closed" || compactSignedJSON(first.ContentPolicy) != `{"sync_content":"consent"}` ||
		time.Until(expires) > 49*time.Hour || time.Until(expires) < 47*time.Hour {
		t.Fatalf("flags must set the first bundle's fields: %+v", first)
	}
	rig.serve(served{signBuilt(t, first.Path, key), key})

	// The next revision: one agent removed, one changed, the rules added; failure mode
	// and content policy carry forward because no flag changes them.
	preview, _ := rig.owner().Preview("PROFILE.md", testAgentSource(t, "team-reviewer", "Version two."))
	if _, err := rig.owner().Select(profilefs.SelectCommand{SourceName: "PROFILE.md", Source: testAgentSource(t, "team-reviewer", "Version two."),
		ExpectedSourceDigest: preview.SourceDigest, ExpectedBundleDigest: preview.BundleDigest, ExpectedStateToken: preview.StateToken}); err != nil {
		t.Fatal(err)
	}
	second, code, body := rig.build(`{"scope":"organization","agents":["team-reviewer"],"remove_agents":["team-helper"],"rules":true}`)
	if code != 200 {
		t.Fatalf("build: %d %s", code, body)
	}
	if second.Revision != 2 || second.PublishedRevision != 1 || second.BundleID == first.BundleID ||
		second.Path != filepath.Join(rig.dir, "bundles", "organization-r2.json") ||
		second.FailureMode != "fail-closed" || compactSignedJSON(second.ContentPolicy) != `{"sync_content":"consent"}` || len(second.Changes) != 0 {
		t.Fatalf("the next revision carries failure mode and content policy forward: %+v", second)
	}
	changes := map[string]string{}
	for _, document := range second.Documents {
		label := document.ProfileID
		if label == "" {
			label = document.Kind
		}
		changes[label] = document.Change
		if document.ProfileID == "team-reviewer" && !textChanged(document.TextDiff) {
			t.Fatalf("a changed profile carries its text diff: %+v", document)
		}
	}
	if changes["team-reviewer"] != builtDocumentChanged || changes["team-helper"] != builtDocumentRemoved || changes["rulebook"] != builtDocumentAdded {
		t.Fatalf("document changes = %v", changes)
	}
	if second.RuleChanges == nil || len(second.RuleChanges.Added) != 1 || second.RuleChanges.Added[0] != "lead-deny" {
		t.Fatalf("rule changes come from the rule diff: %+v", second.RuleChanges)
	}
	// A flag changes a carried field, and the change is reported as its own line.
	third, code, body := rig.build(`{"scope":"organization","content_policy":"mandated"}`)
	if code != 200 || len(third.Changes) != 1 || third.Changes[0].Field != "content_policy" {
		t.Fatalf("build: %d %s %+v", code, body, third.Changes)
	}
}

func TestBundleBuildRefusals(t *testing.T) {
	rig := newAdoptionRig(t)
	selectOwnAgent(t, rig.owner(), testAgentSource(t, "team-reviewer", "x"))
	for body, want := range map[string]int{
		`{}`:                       http.StatusBadRequest,
		`{"scope":"team"}`:         http.StatusBadRequest,
		`{"scope":"organization"}`: http.StatusConflict, // no document
		`{"scope":"organization","agents":["no-such-agent"]}`:                            http.StatusConflict,
		`{"scope":"organization","remove_agents":["team-reviewer"]}`:                     http.StatusConflict, // nothing published to remove from
		`{"scope":"organization","agents":["team-reviewer"],"failure_mode":"fail-soft"}`: http.StatusBadRequest,
		`{"scope":"organization","agents":["team-reviewer"],"content_policy":"always"}`:  http.StatusBadRequest,
		`{"scope":"organization","agents":["team-reviewer"],"expires_in":"-1h"}`:         http.StatusBadRequest,
		`{"scope":"repository","cwd":"/nonexistent/place","agents":["team-reviewer"]}`:   http.StatusBadRequest,
	} {
		if _, code, text := rig.build(body); code != want {
			t.Fatalf("%s: %d, want %d (%s)", body, code, want, text)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(rig.dir, "bundles")); len(entries) != 0 {
		t.Fatalf("a refused build must write no file: %v", entries)
	}
	// An unlinked device builds nothing.
	if _, err := rig.linker.unlink(); err != nil {
		t.Fatal(err)
	}
	if _, code, _ := rig.build(`{"scope":"organization","agents":["team-reviewer"]}`); code != http.StatusConflict {
		t.Fatalf("an unlinked device must refuse: %d", code)
	}
}

func TestTextDiffKeepsCommonLinesAndMarksTheRest(t *testing.T) {
	before := "alpha\nbeta\ngamma\ndelta\n"
	after := "alpha\nBETA\ngamma\ndelta\nepsilon\n"
	got := []string{}
	for _, line := range textDiff(before, after) {
		got = append(got, line.Op+":"+line.Text)
	}
	want := "keep:alpha,remove:beta,add:BETA,keep:gamma,keep:delta,add:epsilon"
	if strings.Join(got, ",") != want {
		t.Fatalf("diff = %v, want %s", got, want)
	}
	if textChanged(textDiff(before, before)) {
		t.Fatal("equal texts have no change")
	}
	moved := textDiff("one\ntwo\nthree\n", "three\none\ntwo\n")
	keeps := 0
	for _, line := range moved {
		if line.Op == textDiffKeep {
			keeps++
		}
	}
	if keeps != 2 || len(moved) != 4 {
		t.Fatalf("a moved line is one removal and one addition around the kept run: %+v", moved)
	}
	if lines := textDiff("", "new\n"); len(lines) != 1 || lines[0].Op != textDiffAdd {
		t.Fatalf("a new text is all additions: %+v", lines)
	}
}
