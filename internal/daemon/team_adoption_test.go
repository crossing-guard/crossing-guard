package daemon

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"crossing-guard/internal/orchestration/profilefs"
	"crossing-guard/internal/rulebook"
	"crossing-guard/store"
	"crossing-guard/teamwire"
)

// Shared agents on a device (team rest-of-release plan §4; criteria 52–55, 60, 61,
// 73–76, 80, 85, 90). Every test links a real linker to a fake team server that
// serves signed bundles, and drives the pull and the adoption routes.

// --- fixtures -------------------------------------------------------------------------

// testBundle is one bundle document before signing.
type testBundle struct {
	ID            string
	Organization  string
	ScopeType     string
	ScopeID       string
	Revision      int64
	Schema        string
	FailureMode   string
	ContentPolicy map[string]any
	Expires       time.Time
	Created       string
	KeyID         string
	Documents     []teamwire.SignedBundleDoc
}

func newTestBundle(id string, revision int64, documents ...teamwire.SignedBundleDoc) testBundle {
	return testBundle{ID: id, Organization: "org_1", ScopeType: "organization", ScopeID: "org_1", Revision: revision,
		Schema: teamwire.BundleSchemaVersion, FailureMode: "fail-open", Expires: time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second),
		Created: "2026-10-04T12:00:00Z", KeyID: "k_one", Documents: documents}
}

// sign returns the signed document, signed exactly as `bundle sign` signs.
func (b testBundle) sign(t *testing.T, key ed25519.PrivateKey) []byte {
	t.Helper()
	doc := map[string]any{"schema_version": b.Schema, "id": b.ID, "organization_id": b.Organization,
		"scope": map[string]any{"type": b.ScopeType, "id": b.ScopeID}, "revision": b.Revision,
		"created_at": b.Created, "expires_at": b.Expires.Format(time.RFC3339),
		"failure_mode": map[string]any{"stateful_tier": b.FailureMode}, "documents": b.Documents}
	if b.ContentPolicy != nil {
		doc["content_policy"] = b.ContentPolicy
	}
	unsigned, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := teamwire.SignBundle(unsigned, key, b.KeyID)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func testRuleDocument(name, ruleID, matches string) teamwire.SignedBundleDoc {
	body, _ := json.Marshal(map[string]any{"rules": []map[string]any{{"id": ruleID, "action": "deny",
		"message": "team rule " + ruleID, "if": map[string]any{"tag": "command", "matches": matches}}}})
	return teamwire.SignedBundleDoc{Kind: teamwire.BundleKindRulebook, Name: name,
		Digest: teamwire.BundleDocumentDigest(body), MediaType: teamwire.BundleMediaTypeRulebook, Body: string(body)}
}

func testProfileDocument(name string, source []byte) teamwire.SignedBundleDoc {
	return teamwire.SignedBundleDoc{Kind: teamwire.BundleKindProfile, Name: name,
		Digest: teamwire.BundleDocumentDigest(source), MediaType: teamwire.BundleMediaTypeProfile, Body: string(source)}
}

// testAgentSource is a valid PROFILE.md for agent id whose text differs by note.
func testAgentSource(t *testing.T, id, note string) []byte {
	t.Helper()
	source, err := profilefs.Duplicate(compatibleReviewProfileSource(), id, "Agent "+id, "Shared agent. "+note)
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func testOrgKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// served is one signed bundle with the key the catalog presents for it.
type served struct {
	raw []byte
	key ed25519.PrivateKey
}

// catalogEntryOf builds the catalog entry a truthful server serves for a signed
// bundle: every field copied from the signed document.
func catalogEntryOf(t *testing.T, item served) map[string]any {
	t.Helper()
	var signed teamwire.SignedBundle
	if err := json.Unmarshal(item.raw, &signed); err != nil {
		t.Fatal(err)
	}
	scope := "organization"
	if signed.Scope.Type == "repository" {
		scope = "repository:" + signed.Scope.ID
	}
	documents := []any{}
	for _, document := range signed.Documents {
		documents = append(documents, map[string]any{"kind": document.Kind, "name": document.Name,
			"digest": document.Digest, "media_type": document.MediaType})
	}
	return map[string]any{"id": signed.ID, "scope": scope, "revision": signed.Revision, "expires_at": signed.ExpiresAt,
		"failure_mode": signed.FailureMode.StatefulTier, "documents": documents,
		"org_public_key": base64.StdEncoding.EncodeToString(item.key.Public().(ed25519.PublicKey))}
}

// adoptionRig is a linked device and the fake server it pulls from.
type adoptionRig struct {
	t      *testing.T
	linker *teamLinker
	ix     *store.Index
	dir    string
	fake   *layersFake
	// clockAhead is how far the linker's clock runs ahead of the wall clock. The
	// linker's jobs read its clock while a test moves it, so the clock is one function,
	// installed before the link starts those jobs, over a value behind a lock — never a
	// reassignment of the linker's field under running jobs.
	clockMu    sync.Mutex
	clockAhead time.Duration
}

func (rig *adoptionRig) now() time.Time {
	rig.clockMu.Lock()
	defer rig.clockMu.Unlock()
	return time.Now().Add(rig.clockAhead)
}

// setClockAhead moves the linker's clock ahead of the wall clock; zero puts it back.
func (rig *adoptionRig) setClockAhead(ahead time.Duration) {
	rig.clockMu.Lock()
	defer rig.clockMu.Unlock()
	rig.clockAhead = ahead
}

func newAdoptionRig(t *testing.T) *adoptionRig {
	t.Helper()
	tl, ix, dir := teamTestLinker(t)
	fake := &layersFake{}
	fake.teamFake.status.Store("approved")
	rig := &adoptionRig{t: t, linker: tl, ix: ix, dir: dir, fake: fake}
	tl.now = rig.now
	rig.serve()
	linkTo(t, tl, fake)
	return rig
}

// serve replaces what the fake server offers, with truthful catalog entries.
func (rig *adoptionRig) serve(items ...served) {
	rig.t.Helper()
	entries := []any{}
	byID := map[string][]byte{}
	for _, item := range items {
		entry := catalogEntryOf(rig.t, item)
		entries = append(entries, entry)
		byID[entry["id"].(string)] = item.raw
	}
	rig.serveCatalog(entries, byID)
}

func (rig *adoptionRig) serveCatalog(entries []any, byID map[string][]byte) {
	rig.fake.mu.Lock()
	defer rig.fake.mu.Unlock()
	rig.fake.catalog = map[string]any{"bundles": entries}
	rig.fake.signedByID = byID
}

func (rig *adoptionRig) pull() teamLayersState {
	rig.t.Helper()
	if err := rig.linker.pullLayers(); err != nil {
		rig.t.Fatalf("pull: %v", err)
	}
	return rig.state()
}

func (rig *adoptionRig) state() teamLayersState {
	rig.t.Helper()
	state, err := rig.linker.layersState()
	if err != nil {
		rig.t.Fatal(err)
	}
	return state
}

func (rig *adoptionRig) post(handler http.HandlerFunc, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest("POST", "/", strings.NewReader(body)))
	return rec
}

func (rig *adoptionRig) adopt(scope, bundleID string) teamAdoptResponse {
	rig.t.Helper()
	// As every caller does: the offer is read first, and its state token is sent back.
	shown := offerOf(rig.t, rig.state(), bundleID)
	rec := rig.post(handleTeamAdopt, `{"scope":"`+scope+`","digest":"`+bundleID+`","state_token":"`+shown.StateToken+`"}`)
	if rec.Code != 200 {
		rig.t.Fatalf("adopt %s: %d %s", bundleID, rec.Code, rec.Body)
	}
	var out teamAdoptResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		rig.t.Fatal(err)
	}
	return out
}

func (rig *adoptionRig) record(organization, scope string) (rulebook.AdoptedBundle, bool) {
	rig.t.Helper()
	layers, err := rulebook.LoadLayers(rig.dir)
	if err != nil {
		rig.t.Fatal(err)
	}
	return layers.Bundle(organization, scope)
}

func (rig *adoptionRig) events(kind string) []string {
	rows, _ := rig.ix.EventChainRows(teamSessionID)
	out := []string{}
	for _, row := range rows {
		if row.Body.Tool == kind {
			out = append(out, row.Body.Reason)
		}
	}
	return out
}

func (rig *adoptionRig) owner() *profilefs.Owner {
	rig.t.Helper()
	owner, err := rig.linker.profileOwner()
	if err != nil {
		rig.t.Fatal(err)
	}
	return owner
}

func selectOwnAgent(t *testing.T, owner *profilefs.Owner, source []byte) profilefs.SelectResult {
	t.Helper()
	preview, err := owner.Preview("PROFILE.md", source)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.Select(profilefs.SelectCommand{SourceName: "PROFILE.md", Source: source,
		ExpectedSourceDigest: preview.SourceDigest, ExpectedBundleDigest: preview.BundleDigest, ExpectedStateToken: preview.StateToken})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func offerOf(t *testing.T, state teamLayersState, bundleID string) teamLayerEntry {
	t.Helper()
	for _, entry := range state.Available {
		if entry.ID == bundleID {
			return entry
		}
	}
	t.Fatalf("bundle %s is not offered: available=%d unusable=%+v reasons=%v", bundleID, len(state.Available), state.Unusable, state.Reasons)
	return teamLayerEntry{}
}

func reasonsMention(state teamLayersState, words ...string) bool {
	for _, reason := range state.Reasons {
		all := true
		for _, word := range words {
			all = all && strings.Contains(reason, word)
		}
		if all {
			return true
		}
	}
	return false
}

// --- criterion 52: profile adoption -----------------------------------------------------

func TestProfileOnlyBundleIsOfferedAdoptedAndTheOfferIsTruthfulAtOnce(t *testing.T) {
	rig := newAdoptionRig(t)
	key := testOrgKey(t)
	source := testAgentSource(t, "team-reviewer", "First text.")
	bundle := newTestBundle("bnd_profile_only_r1", 1, testProfileDocument("team-reviewer/PROFILE.md", source))
	rig.serve(served{bundle.sign(t, key), key})

	offer := offerOf(t, rig.pull(), bundle.ID)
	if offer.Status != offerStatusOffered || offer.Adopted || len(offer.Documents) != 1 {
		t.Fatalf("offer = %+v", offer)
	}
	document := offer.Documents[0]
	if document.Kind != teamwire.BundleKindProfile || document.State != offerDocumentOffered ||
		document.ProfileID != "team-reviewer" || document.Text != string(source) || document.Change != profilefs.TeamDocumentNew {
		t.Fatalf("the offer must show the profile and its text: %+v", document)
	}

	adopted := rig.adopt("organization", bundle.ID)
	if adopted.BundleID != bundle.ID || adopted.Revision != 1 || !adopted.Offer.Adopted ||
		adopted.Offer.Status != offerStatusAdopted || adopted.Offer.Documents[0].State != offerDocumentAdopted {
		t.Fatalf("the adopt response must read adopted with the adopted bundle's own id: %+v", adopted)
	}
	// The stored offer says the same without another pull.
	if again := offerOf(t, rig.state(), bundle.ID); !again.Adopted || again.Documents[0].State != offerDocumentAdopted {
		t.Fatalf("the in-memory offer was not updated in the same call: %+v", again)
	}
	// The agent is listed, inert: no place exists for it.
	detail, err := rig.owner().Get("team-reviewer")
	if err != nil || detail.RuntimeEffects || detail.Current.SelectedBy != profilefs.SelectedByTeamAdoption {
		t.Fatalf("detail = %+v %v", detail, err)
	}
	if bindings, _ := rig.ix.ManagedBindings(false); len(bindings) != 0 {
		t.Fatalf("adoption must turn nothing on: %+v", bindings)
	}
	if _, found, _ := rig.ix.ReviewBinding(); found {
		t.Fatal("adoption must turn nothing on")
	}
	// A profile-only bundle has an adopted-bundle record too, with no rulebook.
	record, found := rig.record("org_1", "organization")
	if !found || record.BundleID != bundle.ID || record.RulebookDigest != "" || record.OrganizationName != "Acme" ||
		record.SignedDigest == "" || record.KeyID != "k_one" || record.SchemaVersion != "1.1" {
		t.Fatalf("record = %+v found=%v", record, found)
	}
	if len(rig.events("team.layer.adopted")) != 1 {
		t.Fatalf("the adoption must be chained once: %v", rig.events("team.layer.adopted"))
	}
}

// Criterion 52's failure path, and criterion 90's device half: a profile the parser
// refuses keeps the bundle from being offered, naming the document; the adopted
// revision stays in force and nothing of the new one can be adopted.
func TestUnparseableProfileIsNotOfferedAndTheAdoptedRevisionStays(t *testing.T) {
	rig := newAdoptionRig(t)
	key := testOrgKey(t)
	good := newTestBundle("bnd_good_r4", 4, testProfileDocument("a/PROFILE.md", testAgentSource(t, "team-reviewer", "Good.")))
	rig.serve(served{good.sign(t, key), key})
	rig.pull()
	rig.adopt("organization", good.ID)

	bad := newTestBundle("bnd_bad_r6", 6, testProfileDocument("a/PROFILE.md", []byte("this is not a profile")))
	rig.serve(served{bad.sign(t, key), key})
	state := rig.pull()
	if len(state.Available) != 0 || len(state.Unusable) != 1 {
		t.Fatalf("a bundle with an unparseable profile must not be offered: %+v", state)
	}
	unusable := state.Unusable[0]
	if unusable.Revision != 6 || unusable.StillOnRevision != 4 || unusable.Document != "a/PROFILE.md" ||
		unusable.Code != teamwire.CodeBundleDocumentInvalid || unusable.Reason == "" ||
		unusable.Documents[0].State != offerDocumentCantBeUsed {
		t.Fatalf("revision 6 can't be used · still on revision 4, as data: %+v", unusable)
	}
	if rec := rig.post(handleTeamAdopt, `{"scope":"organization","digest":"`+bad.ID+`","state_token":"sha256:shown"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("an unusable bundle cannot be adopted: %d %s", rec.Code, rec.Body)
	}
	if record, _ := rig.record("org_1", "organization"); record.BundleID != good.ID || record.Revision != 4 {
		t.Fatalf("the adopted revision must stay in force: %+v", record)
	}
}

// §17.2: a bundle with no usable document is refused at offer, never a 500 at adopt.
func TestBundleWithNoUsableDocumentIsRefusedAtOffer(t *testing.T) {
	rig := newAdoptionRig(t)
	key := testOrgKey(t)
	body := []byte(`{"detectors":[]}`)
	bundle := newTestBundle("bnd_detectors_only", 1, teamwire.SignedBundleDoc{Kind: teamwire.BundleKindDetectors, Name: "detectors.json",
		Digest: teamwire.BundleDocumentDigest(body), MediaType: "application/json", Body: string(body)})
	rig.serve(served{bundle.sign(t, key), key})
	state := rig.pull()
	if len(state.Available) != 0 || len(state.Unusable) != 1 || state.Unusable[0].Code != codeBundleNothingUsable ||
		state.Unusable[0].Documents[0].State != offerDocumentNotApplied {
		t.Fatalf("state = %+v", state)
	}
	if rec := rig.post(handleTeamAdopt, `{"scope":"organization","digest":"`+bundle.ID+`","state_token":"sha256:shown"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("adopt must answer 404, never 500: %d %s", rec.Code, rec.Body)
	}
}

// --- criterion 53: the lead's own device --------------------------------------------------

func TestLeadAdoptingTheirOwnBundleKeepsTheirSelectionAndPlaces(t *testing.T) {
	rig := newAdoptionRig(t)
	key := testOrgKey(t)
	source := testAgentSource(t, "team-reviewer", "The lead wrote this.")
	own := selectOwnAgent(t, rig.owner(), source)
	selectionPath := filepath.Join(rig.dir, "orchestration", "profiles", "selections")
	before := readTree(t, selectionPath)

	bundle := newTestBundle("bnd_lead_r1", 1, testProfileDocument("team-reviewer/PROFILE.md", source))
	bundle.Expires = time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	rig.serve(served{bundle.sign(t, key), key})
	if offer := offerOf(t, rig.pull(), bundle.ID); offer.Documents[0].Change != profilefs.TeamDocumentUnchanged {
		t.Fatalf("the lead's own agent has the bundle's digests: %+v", offer.Documents[0])
	}
	rig.adopt("organization", bundle.ID)
	if after := readTree(t, selectionPath); !sameTree(before, after) {
		t.Fatal("adopting the bundle the lead published changed the lead's selection bytes")
	}
	// The lead's places record no adoption key, so nothing holds them at expiry.
	adoptions := currentPlaceAdoptions()
	current := own.Detail.Current
	if adoptionKey := adoptions.KeyFor("team-reviewer", current.SourceDigest, current.BundleDigest); adoptionKey != "" {
		t.Fatalf("the lead's own place must record no adoption key, got %q", adoptionKey)
	}
	rig.setClockAhead(48 * time.Hour)
	if reason := adoptions.HoldReason("", "team-reviewer", current.SourceDigest, current.BundleDigest); reason != "" {
		t.Fatalf("a place with no adoption key is never held, got %q", reason)
	}
	if _, isAdopted := adoptions.Origin("team-reviewer"); isAdopted {
		t.Fatal("the lead's own agent is not an adopted agent")
	}
	// Un-adopt: the selection is byte-identical and the adoption record is the only
	// file removed under the profile root.
	places := &recordingPlaces{}
	rig.linker.places = places
	profileRoot := filepath.Join(rig.dir, "orchestration", "profiles")
	withRecord := readTree(t, profileRoot)
	if rec := rig.post(handleTeamUnadopt, `{"scope":"organization"}`); rec.Code != 200 {
		t.Fatalf("unadopt: %d %s", rec.Code, rec.Body)
	}
	withoutRecord := readTree(t, profileRoot)
	removed := []string{}
	for path, body := range withRecord {
		after, kept := withoutRecord[path]
		if !kept {
			removed = append(removed, path)
		} else if after != body {
			t.Fatalf("%s changed at un-adopt", path)
		}
	}
	if len(removed) != 1 || !strings.Contains(removed[0], "adoptions") || len(withoutRecord) != len(withRecord)-1 {
		t.Fatalf("the adoption record must be the only file removed: %v", removed)
	}
	if after := readTree(t, selectionPath); !sameTree(before, after) {
		t.Fatal("un-adopt changed the lead's selection bytes")
	}
	if len(places.keys) != 1 || places.keys[0] != store.AdoptionKey("org_1", "organization") {
		t.Fatalf("un-adopt turns off only places recording this adoption's key: %v", places.keys)
	}
}

// recordingPlaces records which adoption keys were asked to be turned off.
type recordingPlaces struct{ keys []string }

func (p *recordingPlaces) TurnOffAdoption(adoptionKey string, _ time.Time) (store.AdoptionPlacesOff, error) {
	p.keys = append(p.keys, adoptionKey)
	return store.AdoptionPlacesOff{Managed: []string{"place-a"}}, nil
}

func readTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || info.Name() == "mutation.lock" {
			return err
		}
		raw, readErr := os.ReadFile(path)
		out[path] = string(raw)
		return readErr
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sameTree(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for path, body := range a {
		if b[path] != body {
			return false
		}
	}
	return true
}

// --- criterion 54: collision, and the stale state token -----------------------------------

func TestCollisionIsNamedTheRestAdoptsAndAStaleTokenChangesNothing(t *testing.T) {
	rig := newAdoptionRig(t)
	key := testOrgKey(t)
	mine := testAgentSource(t, "team-reviewer", "My own agent with that id.")
	selectOwnAgent(t, rig.owner(), mine)
	bundle := newTestBundle("bnd_collision_r1", 1,
		testProfileDocument("team-reviewer/PROFILE.md", testAgentSource(t, "team-reviewer", "The team's.")),
		testProfileDocument("team-helper/PROFILE.md", testAgentSource(t, "team-helper", "A helper.")))
	rig.serve(served{bundle.sign(t, key), key})
	offer := offerOf(t, rig.pull(), bundle.ID)
	if offer.Documents[0].State != offerDocumentIDInUse || !strings.Contains(offer.Documents[0].Reason, "team-reviewer") ||
		offer.Documents[1].State != offerDocumentOffered {
		t.Fatalf("the collision must be named before Adopt: %+v", offer.Documents)
	}

	// Stale: the member imports their own team-helper after the offer was shown.
	profileRoot := filepath.Join(rig.dir, "orchestration", "profiles")
	selectOwnAgent(t, rig.owner(), testAgentSource(t, "team-helper", "Mine, imported meanwhile."))
	before := readTree(t, profileRoot)
	rec := rig.post(handleTeamAdopt, `{"scope":"organization","digest":"`+bundle.ID+`","state_token":"`+offer.StateToken+`"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("a stale offer must refuse the adoption: %d %s", rec.Code, rec.Body)
	}
	if after := readTree(t, profileRoot); !sameTree(before, after) {
		t.Fatal("a refused adoption changed profile state")
	}
	if _, found := rig.record("org_1", "organization"); found {
		t.Fatal("a refused adoption wrote an adopted-bundle record")
	}
	if rec := rig.post(handleTeamAdopt, `{"scope":"organization","digest":"`+bundle.ID+`","state_token":"sha256:not-the-offer"}`); rec.Code != http.StatusConflict {
		t.Fatalf("a token that is not the offer's must refuse: %d %s", rec.Code, rec.Body)
	}
	// The refusal rebuilt the offer: both ids are now in use, and adopting is allowed
	// again — the bundle adopts with both documents skipped and named.
	refreshed := offerOf(t, rig.state(), bundle.ID)
	if refreshed.Documents[1].State != offerDocumentIDInUse || refreshed.StateToken == offer.StateToken {
		t.Fatalf("the offer must be rebuilt after a refusal: %+v", refreshed.Documents)
	}
	adopted := rig.adopt("organization", bundle.ID)
	if adopted.Offer.Documents[0].State != offerDocumentIDInUse || adopted.Offer.Documents[1].State != offerDocumentIDInUse {
		t.Fatalf("documents = %+v", adopted.Offer.Documents)
	}
	if detail, _ := rig.owner().Get("team-reviewer"); detail.Source != string(mine) || detail.Current.SelectedBy != profilefs.SelectedByLocalClient {
		t.Fatal("the member's agent was replaced")
	}
}

func TestCollisionSkipsOneDocumentAndAdoptsTheRest(t *testing.T) {
	rig := newAdoptionRig(t)
	key := testOrgKey(t)
	mine := testAgentSource(t, "team-reviewer", "My own agent with that id.")
	selectOwnAgent(t, rig.owner(), mine)
	bundle := newTestBundle("bnd_collision_rest", 1,
		testProfileDocument("team-reviewer/PROFILE.md", testAgentSource(t, "team-reviewer", "The team's.")),
		testProfileDocument("team-helper/PROFILE.md", testAgentSource(t, "team-helper", "A helper.")),
		testRuleDocument("rules", "team-deny", "team-bad"))
	rig.serve(served{bundle.sign(t, key), key})
	rig.pull()
	adopted := rig.adopt("organization", bundle.ID)
	states := []string{}
	for _, document := range adopted.Offer.Documents {
		states = append(states, document.State)
	}
	if strings.Join(states, ",") != "id_in_use,adopted,adopted" {
		t.Fatalf("states = %v", states)
	}
	if helper, err := rig.owner().Get("team-helper"); err != nil || helper.Current.SelectedBy != profilefs.SelectedByTeamAdoption {
		t.Fatalf("the rest of the bundle must adopt: %+v %v", helper, err)
	}
	if detail, _ := rig.owner().Get("team-reviewer"); detail.Source != string(mine) {
		t.Fatal("the member's agent was replaced")
	}
	if record, _ := rig.record("org_1", "organization"); record.RulebookDigest == "" {
		t.Fatal("the rulebook must adopt with the rest")
	}
	if detail := rig.events("team.layer.adopted"); len(detail) != 1 || !strings.Contains(detail[0], "id is in use: team-reviewer") {
		t.Fatalf("the chained event names the skipped agent: %v", detail)
	}
}

// --- criteria 55 and 85: refresh is exact --------------------------------------------------

// refreshFixture is one adopted revision 1 and the bundle every variant starts from.
func refreshFixture(t *testing.T) (*adoptionRig, ed25519.PrivateKey, testBundle) {
	t.Helper()
	rig := newAdoptionRig(t)
	key := testOrgKey(t)
	base := newTestBundle("bnd_refresh_r1", 1,
		testRuleDocument("rules", "team-deny", "team-bad"),
		testProfileDocument("team-reviewer/PROFILE.md", testAgentSource(t, "team-reviewer", "Adopted text.")))
	base.ContentPolicy = map[string]any{"sync_content": "consent"}
	base.Schema = teamwire.BundleSchemaVersionLegacy
	rig.serve(served{base.sign(t, key), key})
	rig.pull()
	rig.adopt("organization", base.ID)
	return rig, key, base
}

// THE refresh rule (plan §4.1 decision 5, "exact"). A revision that changes any
// signed field other than id, revision, expires_at and created_at must ASK: the
// record does not move, the bundle is offered as changed with the field named, and
// the device keeps the failure behaviour and content policy it adopted. This test
// fails if the signed-digest equality in refreshVerdict is removed or weakened.
func TestRefreshAsksWhenAnySignedFieldBeyondTheFiveChanges(t *testing.T) {
	rig, key, base := refreshFixture(t)
	adoptedRecord, _ := rig.record("org_1", "organization")
	variants := []struct {
		name    string
		change  func(*testBundle)
		field   string // the labelled field the offer must name; "" for a document change
		checkOn func(*testing.T, teamLayerEntry)
	}{
		{name: "failure_mode only", change: func(b *testBundle) { b.FailureMode = "fail-closed" }, field: "failure_mode"},
		{name: "content_policy only", change: func(b *testBundle) { b.ContentPolicy = map[string]any{"sync_content": "mandated"} }, field: "content_policy"},
		{name: "schema_version only", change: func(b *testBundle) { b.Schema = teamwire.BundleSchemaVersion }, field: "schema_version"},
		{name: "a profile body", change: func(b *testBundle) {
			b.Documents = append([]teamwire.SignedBundleDoc{}, b.Documents...)
			b.Documents[1] = testProfileDocument("team-reviewer/PROFILE.md", testAgentSource(t, "team-reviewer", "Changed text."))
		}, checkOn: func(t *testing.T, offer teamLayerEntry) {
			if offer.Documents[1].Change != profilefs.TeamDocumentUpdate || !textChanged(offer.Documents[1].TextDiff) {
				t.Fatalf("a changed profile must be shown with its text diff: %+v", offer.Documents[1])
			}
		}},
		{name: "a rule body", change: func(b *testBundle) {
			b.Documents = append([]teamwire.SignedBundleDoc{}, b.Documents...)
			b.Documents[0] = testRuleDocument("rules", "team-deny", "something-else")
		}, checkOn: func(t *testing.T, offer teamLayerEntry) {
			if offer.RuleChanges == nil || len(offer.RuleChanges.Changed) != 1 {
				t.Fatalf("a changed rule must be shown: %+v", offer.RuleChanges)
			}
		}},
		{name: "a dropped document", change: func(b *testBundle) { b.Documents = b.Documents[:1] }},
	}
	for index, variant := range variants {
		next := base
		next.ID, next.Revision = "bnd_refresh_variant_"+string(rune('a'+index)), 2
		next.Expires = base.Expires.Add(240 * time.Hour)
		variant.change(&next)
		rig.serve(served{next.sign(t, key), key})
		offer := offerOf(t, rig.pull(), next.ID)
		after, _ := rig.record("org_1", "organization")
		if after.BundleID != adoptedRecord.BundleID || after.Revision != 1 || !after.ExpiresAt.Equal(adoptedRecord.ExpiresAt) ||
			after.FailureMode != "fail-open" || after.SignedDigest != adoptedRecord.SignedDigest ||
			compactSignedJSON(after.ContentPolicy) != `{"sync_content":"consent"}` {
			t.Fatalf("%s: a changed revision must not refresh the record: %+v", variant.name, after)
		}
		if offer.Status != offerStatusChanged || offer.Adopted || offer.AdoptedRevision != 1 {
			t.Fatalf("%s: the changed revision must be offered and ask: %+v", variant.name, offer)
		}
		if variant.field != "" {
			if len(offer.Changes) != 1 || offer.Changes[0].Field != variant.field || offer.Changes[0].From == offer.Changes[0].To {
				t.Fatalf("%s: the changed field must be its own labelled line: %+v", variant.name, offer.Changes)
			}
		} else if len(offer.Changes) != 0 {
			t.Fatalf("%s: no signed field changed, only documents: %+v", variant.name, offer.Changes)
		}
		if variant.checkOn != nil {
			variant.checkOn(t, offer)
		}
		// Until adopted, content upload is that of the adopted bundle: consent, not a mandate.
		rig.linker.mu.Lock()
		mandate := adoptedContentMandate(rig.dir, rig.linker.doc.Organization.ID, rig.linker.now())
		rig.linker.mu.Unlock()
		if mandate != nil {
			t.Fatalf("%s: an offered revision must not require content: %+v", variant.name, mandate)
		}
	}
	if refreshed := rig.events("team.bundle.refreshed"); len(refreshed) != 0 {
		t.Fatalf("nothing may have refreshed: %v", refreshed)
	}
	// "Bundle format 1.0 → 1.1" reads as its own line.
	formatOnly := base
	formatOnly.ID, formatOnly.Revision, formatOnly.Schema = "bnd_refresh_format", 2, teamwire.BundleSchemaVersion
	rig.serve(served{formatOnly.sign(t, key), key})
	if change := offerOf(t, rig.pull(), formatOnly.ID).Changes[0]; change.Label != "Bundle format" || change.From != "1.0" || change.To != "1.1" {
		t.Fatalf("change = %+v", change)
	}
}

// The other half: a revision equal in every signed field except id, revision, expiry
// and creation time refreshes with no prompt, moves expiry, and chains one event.
func TestRefreshIsSilentWhenOnlyTheFiveFieldsChange(t *testing.T) {
	rig, key, base := refreshFixture(t)
	next := base
	next.ID, next.Revision, next.Created = "bnd_refresh_r2", 2, "2026-10-20T08:00:00Z"
	next.Expires = base.Expires.Add(240 * time.Hour)
	rig.serve(served{next.sign(t, key), key})
	offer := offerOf(t, rig.pull(), next.ID)
	record, _ := rig.record("org_1", "organization")
	if record.BundleID != next.ID || record.Revision != 2 || !record.ExpiresAt.Equal(next.Expires) {
		t.Fatalf("the record must take the new id, revision and expiry: %+v", record)
	}
	if !offer.Adopted || offer.Status != offerStatusAdopted {
		t.Fatalf("a refreshed bundle reads adopted with no prompt: %+v", offer)
	}
	adoption, found, _ := rig.owner().Adoption("org_1", "organization")
	if !found || adoption.BundleID != next.ID {
		t.Fatalf("the profile adoption record must take the new bundle id: %+v", adoption)
	}
	events := rig.events("team.bundle.refreshed")
	if len(events) != 1 || !strings.Contains(events[0], "revision 2") {
		t.Fatalf("one chained refresh event: %v", events)
	}
	if rep, _ := rig.ix.VerifyEventChain(teamSessionID, nil); rep.Status != "verified" {
		t.Fatalf("the chain must verify with the refresh event: %+v", rep)
	}
	// A second pull of the same revision changes nothing and chains nothing.
	rig.pull()
	if len(rig.events("team.bundle.refreshed")) != 1 {
		t.Fatal("the same revision must not refresh twice")
	}
}

// The verdict itself, on real signed bundles: equal digests refresh; a key-id
// difference refreshes only after a re-pin that replaced exactly the record's key,
// and only when nothing else differs.
func TestRefreshVerdictOnSignedBundles(t *testing.T) {
	key := testOrgKey(t)
	verified := func(b testBundle) verifiedBundle {
		t.Helper()
		raw := b.sign(t, key)
		var signed teamwire.SignedBundle
		if err := json.Unmarshal(raw, &signed); err != nil {
			t.Fatal(err)
		}
		v, err := decodeVerified(raw, signed)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	base := newTestBundle("bnd_v_r1", 1, testRuleDocument("rules", "deny", "bad"))
	adopted := verified(base)
	record := rulebook.AdoptedBundle{BundleID: base.ID, Revision: 1, KeyID: adopted.keyID(),
		SignedDigest: adopted.signedDigest, SignedDigestWithoutKey: adopted.digestWithoutKey}

	same := base
	same.ID, same.Revision, same.Created, same.Expires = "bnd_v_r2", 2, "2026-11-01T00:00:00Z", base.Expires.Add(time.Hour)
	if got := refreshVerdict(record, verified(same), "k_one", ""); got != refreshSilent {
		t.Fatalf("only the five fields changed: %s", got)
	}
	rekeyed := same
	rekeyed.KeyID = "k_two"
	if got := refreshVerdict(record, verified(rekeyed), "k_two", ""); got != refreshAsks {
		t.Fatalf("a key-id difference with no re-pin must ask: %s", got)
	}
	if got := refreshVerdict(record, verified(rekeyed), "k_two", "k_other"); got != refreshAsks {
		t.Fatalf("a re-pin that replaced another key does not cover this record: %s", got)
	}
	if got := refreshVerdict(record, verified(rekeyed), "k_three", "k_one"); got != refreshAsks {
		t.Fatalf("the bundle's key must be the current pin: %s", got)
	}
	if got := refreshVerdict(record, verified(rekeyed), "k_two", "k_one"); got != refreshRepinned {
		t.Fatalf("after the re-pin, a key-id-only difference refreshes: %s", got)
	}
	rekeyedAndChanged := rekeyed
	rekeyedAndChanged.FailureMode = "fail-closed"
	if got := refreshVerdict(record, verified(rekeyedAndChanged), "k_two", "k_one"); got != refreshAsks {
		t.Fatalf("a re-pin confirms the key and nothing else: %s", got)
	}
	// A record migrated from format version 1 has no digest: everything asks.
	if got := refreshVerdict(rulebook.AdoptedBundle{}, verified(same), "k_one", ""); got != refreshAsks {
		t.Fatalf("a record with no signed digest equals nothing: %s", got)
	}
}

// --- criteria 61 and 80: rotation, re-pin, and a device that meets two keys -----------------

func TestRepinShowsBothFingerprintsRefusesAWrongOneAndKeepsAdoptedAgents(t *testing.T) {
	rig, oldKey, base := refreshFixture(t)
	oldPin := loadPinnedOrgKey(rig.dir)
	newKey := testOrgKey(t)
	resigned := base
	resigned.ID, resigned.Revision, resigned.KeyID = "bnd_refresh_resigned", 2, "k_two"
	resigned.Expires = base.Expires.Add(240 * time.Hour)
	rig.serve(served{resigned.sign(t, newKey), newKey})

	state := rig.pull()
	newFingerprint := teamwire.KeyFingerprint(newKey.Public().(ed25519.PublicKey))
	if len(state.Available) != 0 || len(state.KeyMismatches) != 1 {
		t.Fatalf("a bundle under another key is a mismatch, not an offer: %+v", state)
	}
	mismatch := state.KeyMismatches[0]
	if mismatch.PinnedFingerprint != oldPin.Fingerprint || mismatch.PresentedFingerprint != newFingerprint ||
		mismatch.PinnedKeyID != "k_one" || mismatch.PresentedKeyID != "k_two" || mismatch.Bundles[0] != resigned.ID {
		t.Fatalf("the mismatch must show both fingerprints: %+v", mismatch)
	}
	// Until the device re-pins, what it adopted keeps working.
	if record, _ := rig.record("org_1", "organization"); record.BundleID != base.ID || record.KeyID != "k_one" {
		t.Fatalf("the adopted bundle must stay in force: %+v", record)
	}
	if rec := rig.post(handleTeamAdopt, `{"scope":"organization","digest":"`+resigned.ID+`","state_token":"sha256:shown"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("nothing under the other key can be adopted: %d", rec.Code)
	}

	// A wrong fingerprint is refused and the pin is unchanged.
	wrong := teamwire.KeyFingerprint(testOrgKey(t).Public().(ed25519.PublicKey))
	if rec := rig.post(handleTeamRepin, `{"fingerprint":"`+wrong+`","surface":"console"}`); rec.Code != http.StatusConflict {
		t.Fatalf("a wrong fingerprint must be refused: %d %s", rec.Code, rec.Body)
	}
	if rec := rig.post(handleTeamRepin, `{"surface":"console"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("the request must carry the presented fingerprint: %d", rec.Code)
	}
	if rec := rig.post(handleTeamRepin, `{"fingerprint":"`+newFingerprint+`"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("the request must say which surface asked: %d", rec.Code)
	}
	if pin := loadPinnedOrgKey(rig.dir); pin != oldPin {
		t.Fatalf("a refused re-pin changed the pin: %+v", pin)
	}
	if len(rig.events("team.org-key.repinned")) != 0 {
		t.Fatal("a refused re-pin chained an event")
	}

	// The right fingerprint replaces the pin, chains the event with the surface, and
	// the re-signed bundle refreshes with no re-adoption.
	rec := rig.post(handleTeamRepin, `{"fingerprint":"`+newFingerprint+`","surface":"command"}`)
	if rec.Code != 200 {
		t.Fatalf("repin: %d %s", rec.Code, rec.Body)
	}
	pin := loadPinnedOrgKey(rig.dir)
	if pin.KeyID != "k_two" || pin.Fingerprint != newFingerprint {
		t.Fatalf("pin = %+v", pin)
	}
	events := rig.events("team.org-key.repinned")
	if len(events) != 1 || !strings.Contains(events[0], "command") || !strings.Contains(events[0], oldPin.Fingerprint) ||
		!strings.Contains(events[0], newFingerprint) {
		t.Fatalf("the chained event records both keys and the surface: %v", events)
	}
	record, _ := rig.record("org_1", "organization")
	if record.BundleID != resigned.ID || record.Revision != 2 || record.KeyID != "k_two" || !record.ExpiresAt.Equal(resigned.Expires) {
		t.Fatalf("the re-signed bundle must refresh with no prompt after the re-pin: %+v", record)
	}
	state = rig.state()
	if len(state.KeyMismatches) != 0 || !offerOf(t, state, resigned.ID).Adopted {
		t.Fatalf("after the re-pin: %+v", state)
	}
	if detail, err := rig.owner().Get("team-reviewer"); err != nil || detail.Current.SelectedBy != profilefs.SelectedByTeamAdoption {
		t.Fatalf("the adopted agent must be kept with no re-adoption: %v", err)
	}
	if refreshed := rig.events("team.bundle.refreshed"); len(refreshed) != 1 || !strings.Contains(refreshed[0], "re-pin") {
		t.Fatalf("refreshed = %v", refreshed)
	}
	_ = oldKey
}

// Criterion 61's failure path: after the re-pin, a bundle that differs in more than
// its key id still asks.
func TestRepinDoesNotRefreshABundleThatChangedSomethingElse(t *testing.T) {
	rig, _, base := refreshFixture(t)
	newKey := testOrgKey(t)
	changed := base
	changed.ID, changed.Revision, changed.KeyID, changed.FailureMode = "bnd_refresh_resigned_changed", 2, "k_two", "fail-closed"
	rig.serve(served{changed.sign(t, newKey), newKey})
	rig.pull()
	fingerprint := teamwire.KeyFingerprint(newKey.Public().(ed25519.PublicKey))
	if rec := rig.post(handleTeamRepin, `{"fingerprint":"`+fingerprint+`","surface":"console"}`); rec.Code != 200 {
		t.Fatalf("repin: %d %s", rec.Code, rec.Body)
	}
	record, _ := rig.record("org_1", "organization")
	if record.BundleID != base.ID || record.KeyID != "k_one" || record.FailureMode != "fail-open" {
		t.Fatalf("a re-pin confirms a key, not a changed failure mode: %+v", record)
	}
	offer := offerOf(t, rig.state(), changed.ID)
	if offer.Status != offerStatusChanged || len(offer.Changes) != 2 {
		t.Fatalf("the bundle must ask, naming failure mode and signing key: %+v", offer.Changes)
	}
}

// Criterion 80's failure path: a device that links during a rotation meets both keys
// in one pull. It pins the first, shows the mismatch with both fingerprints, and
// adopts nothing under the other key.
func TestDeviceMeetingTwoKeysPinsOneAndOffersNothingUnderTheOther(t *testing.T) {
	rig := newAdoptionRig(t)
	keyA, keyB := testOrgKey(t), testOrgKey(t)
	organization := newTestBundle("bnd_two_keys_org", 3, testRuleDocument("rules", "org-deny", "org-bad"))
	repository := newTestBundle("bnd_two_keys_repo", 1, testRuleDocument("rules", "repo-deny", "repo-bad"))
	repository.ScopeType, repository.ScopeID, repository.KeyID = "repository", "repo_1", "k_two"
	rig.serve(served{organization.sign(t, keyA), keyA}, served{repository.sign(t, keyB), keyB})
	state := rig.pull()
	if len(state.Available) != 1 || state.Available[0].ID != organization.ID {
		t.Fatalf("only the first key's bundle is offered: %+v", state.Available)
	}
	if pin := loadPinnedOrgKey(rig.dir); pin.KeyID != "k_one" {
		t.Fatalf("pin = %+v", pin)
	}
	if len(state.KeyMismatches) != 1 || state.KeyMismatches[0].PinnedFingerprint == "" ||
		state.KeyMismatches[0].PresentedFingerprint != teamwire.KeyFingerprint(keyB.Public().(ed25519.PublicKey)) {
		t.Fatalf("the mismatch must show both fingerprints: %+v", state.KeyMismatches)
	}
	if rec := rig.post(handleTeamAdopt, `{"scope":"repository:repo_1","digest":"`+repository.ID+`","state_token":"sha256:shown"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("nothing under the other key is adopted until re-pin: %d", rec.Code)
	}
	if len(rig.events("team.org-key.pinned")) != 1 {
		t.Fatalf("one pin event: %v", rig.events("team.org-key.pinned"))
	}
}

// --- criteria 60 and 76: caps, monotonic revision, 1.0 bundles ------------------------------

func TestOverCapAndNonMonotonicBundlesAreRefusedByName(t *testing.T) {
	rig := newAdoptionRig(t)
	key := testOrgKey(t)
	nine := []teamwire.SignedBundleDoc{}
	for index := 0; index < teamwire.BundleMaxDocuments+1; index++ {
		nine = append(nine, testProfileDocument("p", testAgentSource(t, "agent-"+string(rune('a'+index)), "x")))
	}
	big := strings.Repeat("x", teamwire.BundleMaxBodyBytes+1)
	cases := []struct {
		name   string
		bundle testBundle
		code   string
	}{
		{"a ninth document", newTestBundle("bnd_cap_nine", 1, nine...), teamwire.CodeBundleTooManyDocuments},
		{"a second rulebook", newTestBundle("bnd_cap_rulebooks", 1, testRuleDocument("a", "one", "x"), testRuleDocument("b", "two", "y")), teamwire.CodeBundleTooManyRulebooks},
		{"an over-size body", newTestBundle("bnd_cap_body", 1, teamwire.SignedBundleDoc{Kind: teamwire.BundleKindProfile, Name: "big",
			Digest: teamwire.BundleDocumentDigest([]byte(big)), MediaType: teamwire.BundleMediaTypeProfile, Body: big}), teamwire.CodeBundleBodyTooLarge},
	}
	for _, item := range cases {
		// Criterion 76's failure path: the caps apply to a 1.0 bundle too.
		item.bundle.Schema = teamwire.BundleSchemaVersionLegacy
		rig.serve(served{item.bundle.sign(t, key), key})
		state := rig.pull()
		if len(state.Available) != 0 || len(state.Unusable) != 1 || state.Unusable[0].Code != item.code {
			t.Fatalf("%s must be refused by name: %+v", item.name, state)
		}
		if rec := rig.post(handleTeamAdopt, `{"scope":"organization","digest":"`+item.bundle.ID+`","state_token":"sha256:shown"}`); rec.Code != http.StatusNotFound {
			t.Fatalf("%s must not be adoptable: %d", item.name, rec.Code)
		}
	}
	if _, found := rig.record("org_1", "organization"); found {
		t.Fatal("an over-cap bundle recorded something")
	}

	// A 1.0 bundle inside the caps is offered and adoptable (criterion 76).
	legacy := newTestBundle("bnd_legacy_r5", 5, testRuleDocument("rules", "legacy-deny", "legacy-bad"))
	legacy.Schema = teamwire.BundleSchemaVersionLegacy
	rig.serve(served{legacy.sign(t, key), key})
	if offer := offerOf(t, rig.pull(), legacy.ID); offer.SchemaVersion != "1.0" {
		t.Fatalf("offer = %+v", offer)
	}
	rig.adopt("organization", legacy.ID)

	// A revision that is not greater than the adopted one is refused by name.
	for _, revision := range []int64{5, 4} {
		older := newTestBundle("bnd_not_newer", revision, testRuleDocument("rules", "older-deny", "older-bad"))
		rig.serve(served{older.sign(t, key), key})
		state := rig.pull()
		if len(state.Available) != 0 || !reasonsMention(state, older.ID, "not newer than the adopted revision 5") {
			t.Fatalf("revision %d must be refused by name: available=%d reasons=%v", revision, len(state.Available), state.Reasons)
		}
	}
	if record, _ := rig.record("org_1", "organization"); record.BundleID != legacy.ID {
		t.Fatalf("the adopted bundle must stay: %+v", record)
	}
	// The adopted bundle itself, served again, is simply the adopted bundle.
	rig.serve(served{legacy.sign(t, key), key})
	if offer := offerOf(t, rig.pull(), legacy.ID); !offer.Adopted {
		t.Fatalf("the identical adopted bundle must read adopted: %+v", offer)
	}
}

// --- criterion 85's failure path: the catalog entry is not believed -------------------------

func TestCatalogEntryThatDiffersFromItsSignedDocumentIsRefusedAndNamed(t *testing.T) {
	rig := newAdoptionRig(t)
	key := testOrgKey(t)
	bundle := newTestBundle("bnd_catalog_lies", 1, testRuleDocument("rules", "deny", "bad"))
	raw := bundle.sign(t, key)
	lies := map[string]func(entry map[string]any){
		"scope": func(entry map[string]any) { entry["scope"] = "repository:repo_other" },
		"expiry": func(entry map[string]any) {
			entry["expires_at"] = time.Now().Add(9000 * time.Hour).UTC().Format(time.RFC3339)
		},
		"failure mode":  func(entry map[string]any) { entry["failure_mode"] = "fail-closed" },
		"document list": func(entry map[string]any) { entry["documents"] = []any{} },
		"revision":      func(entry map[string]any) { entry["revision"] = 7 },
	}
	for field, lie := range lies {
		entry := catalogEntryOf(t, served{raw, key})
		lie(entry)
		rig.serveCatalog([]any{entry}, map[string][]byte{bundle.ID: raw})
		state := rig.pull()
		if len(state.Available) != 0 || len(state.Unusable) != 0 || !reasonsMention(state, bundle.ID, "catalog entry's "+field) {
			t.Fatalf("a catalog entry whose %s differs must be refused and named: %+v", field, state)
		}
	}
	if _, found := rig.record("org_1", "organization"); found {
		t.Fatal("nothing from a refused catalog entry may be recorded")
	}
	// A bundle signed for another organization is refused too.
	foreign := newTestBundle("bnd_other_org", 1, testRuleDocument("rules", "deny", "bad"))
	foreign.Organization, foreign.ScopeID = "org_2", "org_2"
	rig.serve(served{foreign.sign(t, key), key})
	if state := rig.pull(); len(state.Available) != 0 || !reasonsMention(state, foreign.ID, "org_2") {
		t.Fatalf("a bundle signed for another organization must be refused: %+v", state)
	}
}

// --- criterion 75: a detectors document ---------------------------------------------------

func TestDetectorsDocumentIsListedNotAppliedAndNeverStaged(t *testing.T) {
	rig := newAdoptionRig(t)
	key := testOrgKey(t)
	const marker = "detector-body-marker-7f3a"
	body := []byte(`{"detectors":[{"id":"` + marker + `"}]}`)
	bundle := newTestBundle("bnd_with_detectors", 1, testRuleDocument("rules", "deny", "bad"),
		teamwire.SignedBundleDoc{Kind: teamwire.BundleKindDetectors, Name: "detectors.json",
			Digest: teamwire.BundleDocumentDigest(body), MediaType: "application/json", Body: string(body)})
	rig.serve(served{bundle.sign(t, key), key})
	if offer := offerOf(t, rig.pull(), bundle.ID); offer.Documents[1].State != offerDocumentNotApplied {
		t.Fatalf("offer = %+v", offer.Documents)
	}
	adopted := rig.adopt("organization", bundle.ID)
	if adopted.Offer.Documents[0].State != offerDocumentAdopted || adopted.Offer.Documents[1].State != offerDocumentNotApplied {
		t.Fatalf("the rest of the bundle adopts and the detectors stay not applied: %+v", adopted.Offer.Documents)
	}
	err := filepath.Walk(rig.dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || strings.HasPrefix(filepath.Base(path), "index.sqlite") {
			return err
		}
		raw, readErr := os.ReadFile(path)
		if readErr == nil && strings.Contains(string(raw), marker) {
			t.Fatalf("a detector document was written under the data directory: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// --- OD-27 and criterion 85: the content mandate follows the adopted record -----------------

func TestContentMandateIsReadFromTheAdoptedRecordOfAnyScope(t *testing.T) {
	rig := newAdoptionRig(t)
	key := testOrgKey(t)
	mandate := func() *contentMandate {
		rig.linker.mu.Lock()
		defer rig.linker.mu.Unlock()
		return adoptedContentMandate(rig.dir, rig.linker.doc.Organization.ID, rig.linker.now())
	}
	repository := newTestBundle("bnd_repo_mandate", 1, testRuleDocument("rules", "deny", "bad"))
	repository.ScopeType, repository.ScopeID = "repository", "repo_1"
	repository.ContentPolicy = map[string]any{"sync_content": "mandated", "repositories": []string{"repo_1"}}
	rig.serve(served{repository.sign(t, key), key})
	rig.pull()
	if mandate() != nil {
		t.Fatal("an offered bundle requires nothing: availability is not activation")
	}
	rig.adopt("repository:repo_1", repository.ID)
	got := mandate()
	if got == nil || got.BundleID != repository.ID || len(got.Repositories) != 1 {
		t.Fatalf("an adopted repository-scope bundle may require content: %+v", got)
	}
	if st := rig.linker.status(); st.Content.Mandate == nil || st.Content.Mandate.BundleID != repository.ID {
		t.Fatalf("the status route reads the same mandate: %+v", st.Content)
	}
	// Past its expiry an adopted bundle requires nothing.
	rig.setClockAhead(48 * time.Hour)
	if mandate() != nil {
		t.Fatal("an expired bundle must not require content")
	}
	rig.setClockAhead(0)
	if rec := rig.post(handleTeamUnadopt, `{"scope":"repository:repo_1"}`); rec.Code != 200 {
		t.Fatalf("unadopt: %d %s", rec.Code, rec.Body)
	}
	if mandate() != nil {
		t.Fatal("un-adopt ends the mandate")
	}
}

// --- criteria 55, 73, 74: places, expiry, restore, un-adopt, unlink, relink -----------------

func TestAdoptedPlaceIsHeldAtExpiryReleasedByRefreshAndHeldWhenItsVersionIsDropped(t *testing.T) {
	rig := newAdoptionRig(t)
	key := testOrgKey(t)
	first := newTestBundle("bnd_places_r1", 1, testProfileDocument("team-reviewer/PROFILE.md", testAgentSource(t, "team-reviewer", "Version one.")))
	first.Expires = time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	rig.serve(served{first.sign(t, key), key})
	rig.pull()
	rig.adopt("organization", first.ID)
	adoptions := currentPlaceAdoptions()
	one, _ := rig.owner().Get("team-reviewer")
	source, compiled := one.Current.SourceDigest, one.Current.BundleDigest
	adoptionKey := adoptions.KeyFor("team-reviewer", source, compiled)
	if adoptionKey != store.AdoptionKey("org_1", "organization") {
		t.Fatalf("a place on an adopted revision records the adoption key, got %q", adoptionKey)
	}
	if reason := adoptions.HoldReason(adoptionKey, "team-reviewer", source, compiled); reason != "" {
		t.Fatalf("an adopted, unexpired place runs: %q", reason)
	}
	origin, isAdopted := adoptions.Origin("team-reviewer")
	if !isAdopted || !origin.ReadOnly || origin.Released || origin.OrganizationName != "Acme" || origin.BundleID != first.ID || origin.Revision != 1 {
		t.Fatalf("origin = %+v", origin)
	}

	// Past expiry the place is held and says why.
	rig.setClockAhead(2 * time.Hour)
	if reason := adoptions.HoldReason(adoptionKey, "team-reviewer", source, compiled); reason != placeHoldExpired {
		t.Fatalf("past expiry an adopted place starts no run: %q", reason)
	}
	// The next refresh releases it with no action.
	second := first
	second.ID, second.Revision, second.Expires = "bnd_places_r2", 2, time.Now().Add(240*time.Hour).UTC().Truncate(time.Second)
	rig.serve(served{second.sign(t, key), key})
	rig.pull()
	if reason := adoptions.HoldReason(adoptionKey, "team-reviewer", source, compiled); reason != "" {
		t.Fatalf("a refresh must release the place with no action: %q", reason)
	}
	rig.setClockAhead(0)

	// A newer revision replaces the profile: the place pinned to the old digests is
	// held with that reason, and may move only to a revision the adoption lists.
	third := second
	third.ID, third.Revision = "bnd_places_r3", 3
	third.Documents = []teamwire.SignedBundleDoc{testProfileDocument("team-reviewer/PROFILE.md", testAgentSource(t, "team-reviewer", "Version two."))}
	rig.serve(served{third.sign(t, key), key})
	rig.pull()
	rig.adopt("organization", third.ID)
	two, _ := rig.owner().Get("team-reviewer")
	if reason := adoptions.HoldReason(adoptionKey, "team-reviewer", source, compiled); reason != placeHoldVersionGone {
		t.Fatalf("a place on digests the adoption dropped is held: %q", reason)
	}
	if adoptions.MoveAllowed(adoptionKey, "team-reviewer", source, compiled) {
		t.Fatal("a move to a revision the adoption does not list must be refused")
	}
	if !adoptions.MoveAllowed(adoptionKey, "team-reviewer", two.Current.SourceDigest, two.Current.BundleDigest) {
		t.Fatal("a move to the revision the adoption lists must be allowed")
	}
	if !adoptions.MoveAllowed("", "team-reviewer", source, compiled) {
		t.Fatal("a place with no adoption key moves freely")
	}

	// Un-adopt turns the places off, and a later Adopt leaves them off: adoption
	// never calls the place owner again.
	places := &recordingPlaces{}
	rig.linker.places = places
	rec := rig.post(handleTeamUnadopt, `{"scope":"organization","surface":"command"}`)
	var unadopted teamUnadoptResponse
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &unadopted) != nil {
		t.Fatalf("unadopt: %d %s", rec.Code, rec.Body)
	}
	if len(places.keys) != 1 || places.keys[0] != adoptionKey || unadopted.PlacesOff.Count() != 1 ||
		unadopted.BundleID != third.ID || unadopted.Revision != 3 || !unadopted.WasAdopted {
		t.Fatalf("un-adopt must turn the adoption's places off and answer with the bundle: %+v keys=%v", unadopted, places.keys)
	}
	if len(unadopted.Offers) != 1 || unadopted.Offers[0].Adopted || unadopted.Offers[0].Status != offerStatusOffered {
		t.Fatalf("the offer must read not adopted in the same response: %+v", unadopted.Offers)
	}
	if reason := adoptions.HoldReason(adoptionKey, "team-reviewer", two.Current.SourceDigest, two.Current.BundleDigest); reason != placeHoldAdoptionEnded {
		t.Fatalf("after un-adopt nothing vouches for the place: %q", reason)
	}
	if origin, _ := adoptions.Origin("team-reviewer"); !origin.Released || origin.OrganizationID != "org_1" {
		t.Fatalf("the agent reads as no longer shared: %+v", origin)
	}
	rig.adopt("organization", third.ID)
	if len(places.keys) != 1 {
		t.Fatalf("adopting again must not touch places: %v", places.keys)
	}
	if origin, _ := adoptions.Origin("team-reviewer"); origin.Released {
		t.Fatalf("the same scope takes the released agent up again: %+v", origin)
	}
}

func TestUnlinkClearsThePinKeepsAdoptionsAndARelinkElsewhereCollides(t *testing.T) {
	rig := newAdoptionRig(t)
	key := testOrgKey(t)
	bundle := newTestBundle("bnd_unlink_r1", 1, testRuleDocument("rules", "deny", "bad"),
		testProfileDocument("team-reviewer/PROFILE.md", testAgentSource(t, "team-reviewer", "From Acme.")))
	bundle.Expires = time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	rig.serve(served{bundle.sign(t, key), key})
	rig.pull()
	rig.adopt("organization", bundle.ID)
	if loadPinnedOrgKey(rig.dir).KeyID == "" {
		t.Fatal("the device must be pinned before the unlink")
	}
	if _, err := rig.linker.unlink(); err != nil {
		t.Fatal(err)
	}
	// The pin goes with the link: the four pin fields and the in-memory pin.
	layers, _ := rulebook.LoadLayers(rig.dir)
	if layers.PinnedOrgKeyID != "" || layers.PinnedOrgKeyPublicKey != "" || layers.PinnedOrgKeyFP != "" || layers.PinnedAt != "" {
		t.Fatalf("unlink must clear the pin: %+v", layers)
	}
	if rig.linker.pinnedKey != (orgKeyPin{}) {
		t.Fatalf("the in-memory pin must be cleared: %+v", rig.linker.pinnedKey)
	}
	// The adoption stays, keeps its key id, and is listed under its organization.
	record, found := layers.Bundle("org_1", "organization")
	if !found || record.KeyID != "k_one" || record.BundleID != bundle.ID {
		t.Fatalf("the adoption must outlive the link: %+v", record)
	}
	state := rig.state()
	if len(state.AdoptedBundles) != 1 || state.AdoptedBundles[0].Linked || state.AdoptedBundles[0].OrganizationName != "Acme" ||
		len(state.AdoptedBundles[0].Agents) != 1 || state.AdoptedBundles[0].RuleCount != 1 {
		t.Fatalf("an unlinked device lists what it adopted under its organization, with its rule count: %+v", state.AdoptedBundles)
	}
	adoptions := currentPlaceAdoptions()
	detail, _ := rig.owner().Get("team-reviewer")
	adoptionKey := adoptions.KeyFor("team-reviewer", detail.Current.SourceDigest, detail.Current.BundleDigest)
	if reason := adoptions.HoldReason(adoptionKey, "team-reviewer", detail.Current.SourceDigest, detail.Current.BundleDigest); reason != "" {
		t.Fatalf("after unlink an adopted agent keeps running to its expiry: %q", reason)
	}
	rig.setClockAhead(2 * time.Hour)
	if reason := adoptions.HoldReason(adoptionKey, "team-reviewer", detail.Current.SourceDigest, detail.Current.BundleDigest); reason != placeHoldExpired {
		t.Fatalf("and is then held: %q", reason)
	}
	rig.setClockAhead(0)

	// Relink to another organization: it pins anew, the earlier adoption is neither
	// refreshed nor replaced, and a same-id profile is a collision.
	otherKey := testOrgKey(t)
	rig.fake.orgID = "org_2"
	other := newTestBundle("bnd_other_org_r9", 9, testProfileDocument("team-reviewer/PROFILE.md", testAgentSource(t, "team-reviewer", "From the other organization.")))
	other.Organization, other.ScopeID, other.KeyID = "org_2", "org_2", "k_other"
	rig.serve(served{other.sign(t, otherKey), otherKey})
	linkTo(t, rig.linker, rig.fake)
	offer := offerOf(t, rig.pull(), other.ID)
	if offer.Documents[0].State != offerDocumentIDInUse || offer.Status != offerStatusOffered {
		t.Fatalf("a same-id profile from the new organization is a collision: %+v", offer)
	}
	if pin := loadPinnedOrgKey(rig.dir); pin.KeyID != "k_other" {
		t.Fatalf("a later link pins anew: %+v", pin)
	}
	rig.adopt("organization", other.ID)
	kept, found := rig.record("org_1", "organization")
	if !found || kept.BundleID != bundle.ID || kept.Revision != 1 || !kept.ExpiresAt.Equal(bundle.Expires) {
		t.Fatalf("the earlier organization's adoption must be untouched and never refreshed: %+v", kept)
	}
	if after, _ := rig.owner().Get("team-reviewer"); after.Source != detail.Source {
		t.Fatal("the earlier organization's agent was replaced")
	}
	if len(rig.events("team.bundle.refreshed")) != 0 {
		t.Fatal("nothing may have refreshed")
	}
	// Two organizations now hold the organization scope; the earlier one is named.
	if rec := rig.post(handleTeamUnadopt, `{"scope":"organization","organization_id":"org_1"}`); rec.Code != 200 {
		t.Fatalf("un-adopt of the earlier organization: %d %s", rec.Code, rec.Body)
	}
	if _, found := rig.record("org_1", "organization"); found {
		t.Fatal("the earlier organization's adoption must be gone")
	}
	if _, found := rig.record("org_2", "organization"); !found {
		t.Fatal("the linked organization's adoption must stay")
	}
}

// A partial adoption — the profile adoption record written, the adopted-bundle record
// not — shows as partial, and Adopt completes it.
func TestPartialAdoptionShowsAsPartialAndAdoptCompletesIt(t *testing.T) {
	rig := newAdoptionRig(t)
	key := testOrgKey(t)
	source := testAgentSource(t, "team-reviewer", "Partial.")
	bundle := newTestBundle("bnd_partial_r1", 1, testRuleDocument("rules", "deny", "bad"), testProfileDocument("team-reviewer/PROFILE.md", source))
	rig.serve(served{bundle.sign(t, key), key})
	offer := offerOf(t, rig.pull(), bundle.ID)
	// The first three steps, as a daemon that stopped before the last one left them.
	if _, err := rig.owner().AdoptTeam(profilefs.AdoptTeamCommand{OrganizationID: "org_1", Scope: "organization", BundleID: bundle.ID,
		Documents: []profilefs.TeamDocument{{Name: "team-reviewer/PROFILE.md", Source: source}}, ExpectedStateToken: offer.planToken}); err != nil {
		t.Fatal(err)
	}
	partial := offerOf(t, rig.pull(), bundle.ID)
	if partial.Status != offerStatusPartial || partial.Adopted {
		t.Fatalf("offer = %+v", partial)
	}
	adopted := rig.adopt("organization", bundle.ID)
	if !adopted.Offer.Adopted || adopted.Adopted.RulebookDigest == "" {
		t.Fatalf("Adopt must complete a partial adoption: %+v", adopted)
	}
}

// The un-adopt and adopt routes refuse what they cannot mean.
func TestAdoptionRoutesRefuseMalformedRequests(t *testing.T) {
	rig := newAdoptionRig(t)
	for body, handler := range map[string]http.HandlerFunc{
		`{"scope":"organization"}`: handleTeamAdopt,
		// Red-team Low 5: the offer's state token is required, not optional.
		`{"scope":"organization","digest":"bnd_x"}`:             handleTeamAdopt,
		`{"scope":"organization","digest":"x","surface":"fax"}`: handleTeamAdopt,
		`{}`:                  handleTeamUnadopt,
		`{"scope":"nowhere"}`: handleTeamUnadopt,
	} {
		if rec := rig.post(handler, body); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", body, rec.Code, rec.Body)
		}
	}
	// Un-adopting a scope nothing adopted is not an error.
	rec := rig.post(handleTeamUnadopt, `{"scope":"organization"}`)
	var out teamUnadoptResponse
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.WasAdopted {
		t.Fatalf("unadopt of nothing: %d %s", rec.Code, rec.Body)
	}
}

// Red-team Low 2: Unlink clears the pin last, so an error part-way leaves a pin with
// no link. At the next start the pin is cleared: a pin belongs to a link.
func TestAPinWithNoLinkIsClearedAtStart(t *testing.T) {
	tl, _, dir := teamTestLinker(t)
	orphan := orgKeyPin{KeyID: "k_old", PublicKey: "cHVibGlj", Fingerprint: "AAAA-BBBB", PinnedAt: "2026-10-01T00:00:00Z"}
	if err := savePinnedOrgKey(dir, orphan, ""); err != nil {
		t.Fatal(err)
	}
	tl.reload()
	if pin := loadPinnedOrgKey(dir); pin.KeyID != "" {
		t.Fatalf("a pin with no link survived the start: %+v", pin)
	}
	tl.mu.Lock()
	inMemory := tl.pinnedKey
	tl.mu.Unlock()
	if inMemory.KeyID != "" {
		t.Fatalf("the linker still holds the orphaned pin: %+v", inMemory)
	}

	// A linked device's pin is its own and stays.
	rig := newAdoptionRig(t)
	if err := savePinnedOrgKey(rig.dir, orphan, ""); err != nil {
		t.Fatal(err)
	}
	rig.linker.reload()
	if pin := loadPinnedOrgKey(rig.dir); pin.KeyID != "k_old" {
		t.Fatalf("a linked device's pin was cleared at reload: %+v", pin)
	}
}

// Red-team Low 3: a re-pin saved the new pin before checking that the link it was
// asked under is still the link. A pin recorded for a link that ended meanwhile is
// cleared, exactly as a first pin is.
func TestAPinRecordedForAnEndedLinkIsCleared(t *testing.T) {
	rig := newAdoptionRig(t)
	rig.linker.mu.Lock()
	gen := rig.linker.gen
	rig.linker.mu.Unlock()
	pin := orgKeyPin{KeyID: "k_new", PublicKey: "cHVibGlj", Fingerprint: "CCCC-DDDD", PinnedAt: "2026-10-04T00:00:00Z"}
	if err := savePinnedOrgKey(rig.dir, pin, "k_old"); err != nil {
		t.Fatal(err)
	}
	if !rig.linker.keepRecordedPin(pin, gen) {
		t.Fatal("the link stands: the pin is kept")
	}
	if kept := loadPinnedOrgKey(rig.dir); kept.KeyID != "k_new" {
		t.Fatalf("a pin for the standing link: %+v", kept)
	}
	// The link ended (its generation moved) between the save and the check.
	if rig.linker.keepRecordedPin(pin, gen+1) {
		t.Fatal("a pin recorded for another link generation must not be kept")
	}
	if left := loadPinnedOrgKey(rig.dir); left.KeyID != "" {
		t.Fatalf("the pin outlived its link: %+v", left)
	}
}

// Red-team Low 22: the mandate's read of layers.json ran while the linker's lock was
// held (the push drain and the status route). The read is now a separate step taken
// outside the lock, and the decision takes records already read: with the linker's
// lock held, the status route's own decision and a drain still answer, and the
// decision reads no file (its store directory does not exist).
func TestContentMandateIsDecidedFromRecordsReadOutsideTheLinkerLock(t *testing.T) {
	expires := time.Now().Add(time.Hour)
	layers := rulebook.LayersDocument{Adopted: []rulebook.AdoptedBundle{{OrganizationID: "org_1", Scope: "organization", BundleID: "bnd_m",
		ExpiresAt: expires, ContentPolicy: json.RawMessage(`{"sync_content":"mandated","repositories":["a/b"]}`)}}}
	if mandate := contentMandateIn(layers, "org_1", time.Now()); mandate == nil || mandate.BundleID != "bnd_m" {
		t.Fatalf("the mandate of an adopted, unexpired record: %+v", mandate)
	}
	if contentMandateIn(layers, "org_other", time.Now()) != nil || contentMandateIn(layers, "", time.Now()) != nil ||
		contentMandateIn(layers, "org_1", expires.Add(time.Minute)) != nil {
		t.Fatal("another organization's record, no organization, or an expired record requires nothing")
	}
	if adoptedContentMandate(filepath.Join(t.TempDir(), "no-such-store"), "org_1", time.Now()) != nil {
		t.Fatal("records that cannot be read require nothing")
	}
	// The status route answers while layers.json's own lock is held by another writer
	// only if its read is not nested inside the linker's lock; here the weaker, exact
	// property: status() completes and reports the mandate with the records on disk.
	rig := newAdoptionRig(t)
	if got := rig.linker.status(); got.Content.Mandate != nil {
		t.Fatalf("nothing adopted requires nothing: %+v", got.Content.Mandate)
	}
}
