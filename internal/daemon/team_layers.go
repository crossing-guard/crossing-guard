package daemon

// The layers pull job (team plan §5.16.3, item 3c part 2, design (b)): the device's
// half of "deploy governance policy". The pull runs on its own cadence from
// team.json (never inside a tool call — invariant 5), fetches each bundle's ORIGINAL
// signed bytes, verifies the Ed25519 signature against the org key this device
// PINNED, and hands every verified bundle to the adoption owner
// (team_adoption_offer.go), which decides whether it is offered, refreshed, or named
// as unusable. Adoption is an explicit local action after a shown diff —
// availability is not activation.

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"crossing-guard/internal/rulebook"
	"crossing-guard/internal/teamlink"
	"crossing-guard/teamwire"
)

// orgKeyPin is the pinned organization key's facts — layers.json holds it and the
// pin event chains it. The json names are GET /api/team/layers' pinned_org_key
// fields, which the console's Organization key row reads.
type orgKeyPin struct {
	KeyID       string `json:"key_id"`
	PublicKey   string `json:"public_key"` // base64
	Fingerprint string `json:"fingerprint"`
	PinnedAt    string `json:"pinned_at"`
}

// loadPinnedOrgKey reads the pin from layers.json (empty when unpinned).
func loadPinnedOrgKey(storeDir string) orgKeyPin {
	doc, err := rulebook.LoadLayers(storeDir)
	if err != nil {
		return orgKeyPin{}
	}
	return orgKeyPin{KeyID: doc.PinnedOrgKeyID, PublicKey: doc.PinnedOrgKeyPublicKey,
		Fingerprint: doc.PinnedOrgKeyFP, PinnedAt: doc.PinnedAt}
}

// savePinnedOrgKey records the pin under layers.json's lock, in the one
// read-change-write an adoption shares, so neither can lose the other's update.
// replaced is the key id a re-pin replaced; a first pin passes "".
func savePinnedOrgKey(storeDir string, pin orgKeyPin, replaced string) error {
	return rulebook.UpdateLayers(storeDir, func(doc *rulebook.LayersDocument) error {
		doc.PinnedOrgKeyID = pin.KeyID
		doc.PinnedOrgKeyPublicKey = pin.PublicKey
		doc.PinnedOrgKeyFP = pin.Fingerprint
		doc.PinnedAt = pin.PinnedAt
		if replaced != "" {
			doc.ReplacedOrgKeyID = replaced
		}
		return nil
	})
}

// clearPinnedOrgKey removes the pin: it belongs to the link and goes with it (plan
// §4.3, §17.2). Adoption records keep the key id they were verified under.
func clearPinnedOrgKey(storeDir string) error {
	return rulebook.UpdateLayers(storeDir, func(doc *rulebook.LayersDocument) error {
		doc.PinnedOrgKeyID, doc.PinnedOrgKeyPublicKey, doc.PinnedOrgKeyFP, doc.PinnedAt = "", "", "", ""
		doc.ReplacedOrgKeyID = ""
		return nil
	})
}

// verifiedBundle is one pulled bundle whose signature verified. Every value the
// device records, shows or compares about a bundle is read from here — decoded from
// the bytes the signature covered — and never from the unsigned catalog entry (plan
// §4.1 decision 5).
type verifiedBundle struct {
	raw    []byte
	signed teamwire.SignedBundle
	// scope is the signed scope in the device's spelling: "organization" or
	// "repository:<id>".
	scope   string
	expires time.Time
	// signedDigest and digestWithoutKey are the two refresh comparisons.
	signedDigest     string
	digestWithoutKey string
}

func (v verifiedBundle) keyID() string { return v.signed.Signature.KeyID }

// teamKeyMismatch is the `org key mismatch` state: this device is pinned to one key
// and the server presents a bundle signed by another. Both fingerprints are shown so
// a person can compare the presented one with the team console's Policy page before
// trusting it; nothing signed by the presented key is offered or adopted until then.
type teamKeyMismatch struct {
	PinnedKeyID          string   `json:"pinned_key_id"`
	PinnedFingerprint    string   `json:"pinned_fingerprint"`
	PresentedKeyID       string   `json:"presented_key_id"`
	PresentedFingerprint string   `json:"presented_fingerprint"`
	Bundles              []string `json:"bundles"`
	presentedPublicKey   string
}

// startLayersPullLocked launches the pull job beside the report job.
func (t *teamLinker) startLayersPullLocked() {
	if t.pullStop != nil || t.halted {
		return
	}
	stop := make(chan struct{})
	t.pullStop = stop
	t.jobs.Go(func() { t.runLayersPull(stop) })
}

// runLayersPull ticks the pull cadence (§5.4: pull 60 s) and stops with the link.
func (t *teamLinker) runLayersPull(stop chan struct{}) {
	t.mu.Lock()
	interval := t.doc.PullInterval.Duration
	t.mu.Unlock()
	if interval <= 0 {
		interval = time.Minute
	}
	timer := time.NewTimer(time.Second) // the first pull is prompt: the pin rides it
	defer timer.Stop()
	for {
		select {
		case <-stop:
			return
		case <-timer.C:
		}
		t.pullLayersOnce()
		timer.Reset(interval)
	}
}

// errNotLinked reports an act that needs a link on a device that has none.
var errNotLinked = errors.New("the device is not linked")

// pullLayersOnce is one tick of the pull job; a failure is logged and is the next
// tick's business.
func (t *teamLinker) pullLayersOnce() {
	if err := t.pullLayers(); err != nil && !errors.Is(err, errNotLinked) {
		log.Printf("team layers: %v", err)
	}
}

// pullOutcome is what one pull found, built up bundle by bundle and then published
// as the in-memory offer in one step.
type pullOutcome struct {
	available  []teamLayerEntry
	unusable   []teamUnusableBundle
	mismatches []teamKeyMismatch
	notes      []string
	firstPin   *orgKeyPin
}

func (o *pullOutcome) refuse(bundleID, why string) {
	o.notes = append(o.notes, "bundle "+bundleID+" not offered: "+why)
}

func (o *pullOutcome) mismatch(found teamKeyMismatch, bundleID string) {
	for index := range o.mismatches {
		if o.mismatches[index].PresentedKeyID == found.PresentedKeyID && o.mismatches[index].PresentedFingerprint == found.PresentedFingerprint {
			o.mismatches[index].Bundles = append(o.mismatches[index].Bundles, bundleID)
			return
		}
	}
	found.Bundles = []string{bundleID}
	o.mismatches = append(o.mismatches, found)
}

// pullLayers pulls the catalog, then fetches and verifies each bundle's ORIGINAL
// signed bytes: every bundle whose signature verifies against the PINNED org key (or
// pins on first contact) goes to the adoption owner. The pin rides the first verified
// bundle (§5.16.2 d2), chained as team.org-key.pinned under the daemon's session; a
// bundle signed by any other key in the same pull is a mismatch, not a second pin.
func (t *teamLinker) pullLayers() error {
	t.mu.Lock()
	if t.state != teamLinked || t.key == nil {
		t.mu.Unlock()
		return errNotLinked
	}
	client, err := teamlink.NewClient(t.doc.Server, t.deviceID, t.key, t.doc.RequestTimeout.Duration, t.now)
	gen := t.gen
	organization := t.doc.Organization
	t.mu.Unlock()
	if err != nil {
		return fmt.Errorf("the pull could not sign: %w", err)
	}
	cat, err := client.Catalog()
	if err != nil {
		if teamlink.Code(err) == teamwire.CodeDeviceRevoked {
			t.mu.Lock()
			if t.gen == gen && t.state == teamLinked {
				t.state, t.problem = teamRevoked, "the server revoked this device's key; sync is paused and nothing local is deleted — unlink and link again to re-enroll"
			}
			t.mu.Unlock()
			t.linkEvent("team.revoked", "the server refused a catalog pull with device_revoked")
		}
		return fmt.Errorf("catalog pull failed: %w", err)
	}
	// One adoption act at a time: a pull that refreshes a record and an Adopt for the
	// same scope must not interleave.
	t.adoptMu.Lock()
	defer t.adoptMu.Unlock()
	outcome := pullOutcome{available: make([]teamLayerEntry, 0, len(cat.Bundles))}
	pin := loadPinnedOrgKey(t.storeDir)
	for i := range cat.Bundles {
		entry := cat.Bundles[i]
		raw, err := client.SignedBundle(entry.ID)
		if err != nil {
			outcome.refuse(entry.ID, err.Error())
			continue // an unverifiable bundle is simply not offered — named, not silent
		}
		verified, firstPin, mismatch, err := verifySignedBundle(entry, raw, pin, t.now())
		if mismatch != nil {
			outcome.mismatch(*mismatch, entry.ID)
		}
		if err != nil {
			outcome.refuse(entry.ID, err.Error())
			continue
		}
		if firstPin != nil {
			pin, outcome.firstPin = *firstPin, firstPin
		}
		t.assessBundle(entry, verified, organization, &outcome)
	}
	// The loader's own notes for the CURRENT adoptions (expired/unloadable/not-
	// yet-staged) ride the same state.
	if _, reasons, rerr := rulebook.LoadLayered("", t.storeDir); rerr == nil {
		outcome.notes = append(outcome.notes, reasons...)
	}
	t.mu.Lock()
	if t.gen != gen {
		t.mu.Unlock()
		return nil // the link changed while this pull ran; its findings belong to no link
	}
	t.available = outcome.available
	t.unusable = outcome.unusable
	t.keyMismatches = outcome.mismatches
	t.layerReasons = outcome.notes
	t.mu.Unlock()

	// The pointer index's PRODUCTION writer (postwork H-2): for every adopted or
	// offered repository-scoped bundle, resolve each governed session's
	// (root → repository_id) from the store — the daemon may spawn git, the hook
	// may not — and record it so the hook's stat-only lookup finds the layer.
	t.recordRepositoryResolutions(outcome.available)
	t.recordFirstPin(outcome.firstPin, gen)
	return nil
}

// recordFirstPin records the organization key the first verified bundle presented,
// chained once. The DISK is authoritative: the save happens before the memory update,
// and a failed save leaves the device unpinned — verification goes through the disk
// record, so a half-pin can never be trusted (postwork M-5). A pinned device never
// reaches here with another key: that is the mismatch state, and only a person's
// re-pin replaces a pin (plan §4.3).
func (t *teamLinker) recordFirstPin(firstPin *orgKeyPin, gen uint64) {
	if firstPin == nil {
		return
	}
	if err := savePinnedOrgKey(t.storeDir, *firstPin, ""); err != nil {
		log.Printf("team layers: the pin could not be recorded: %v", err)
		return
	}
	if !t.keepRecordedPin(*firstPin, gen) {
		return
	}
	t.linkEvent("team.org-key.pinned", "organization key "+firstPin.KeyID+" ("+firstPin.Fingerprint+") pinned from the first verified bundle; compare with the admin console's Policy page")
}

// keepRecordedPin is the second half of recording a pin, first or re-pinned: the pin
// is already on disk, and it becomes the linker's pin only if the link it was recorded
// for is still the link (gen). If the link ended meanwhile, the pin goes with it — it
// is cleared from disk and false is returned — so no pin outlives its link.
func (t *teamLinker) keepRecordedPin(pin orgKeyPin, gen uint64) bool {
	t.mu.Lock()
	if t.gen == gen {
		t.pinnedKey = pin
		t.mu.Unlock()
		return true
	}
	t.mu.Unlock()
	if err := clearPinnedOrgKey(t.storeDir); err != nil {
		log.Printf("team layers: a pin recorded for an ended link could not be cleared: %v", err)
	}
	return false
}

// clearPinWithoutLink is the start-up repair for an unlink that stopped part-way: the
// link's files are gone and the pin, which is cleared last, is still recorded. A pin
// belongs to a link; with none, it is cleared, so a later link pins anew instead of
// meeting a key it never verified under that link.
func clearPinWithoutLink(storeDir string, linked bool, pin orgKeyPin) orgKeyPin {
	if linked || pin.KeyID == "" {
		return pin
	}
	if err := clearPinnedOrgKey(storeDir); err != nil {
		log.Printf("team layers: organization key %s is pinned with no link and could not be cleared: %v", pin.KeyID, err)
		return pin
	}
	log.Printf("team layers: organization key %s was pinned with no link (an unlink that did not finish); the pin was cleared", pin.KeyID)
	return orgKeyPin{}
}

// verifySignedBundle checks one bundle's signature against the pinned organization
// key, or against the key the catalog presents when nothing is pinned yet (first
// contact, returned as the pin to record). A pinned device that meets another key id
// gets the mismatch state back with the refusal, carrying the presented key's
// fingerprint only when the bundle really verifies under that key. It reads no file
// and makes no request.
func verifySignedBundle(entry teamwire.BundleEntry, raw []byte, pin orgKeyPin, now time.Time) (verifiedBundle, *orgKeyPin, *teamKeyMismatch, error) {
	var signed teamwire.SignedBundle
	if err := json.Unmarshal(raw, &signed); err != nil {
		return verifiedBundle{}, nil, nil, fmt.Errorf("not the signed document the server stored: %w", err)
	}
	if signed.Signature == nil || signed.Signature.Algorithm != "ed25519" || signed.Signature.KeyID == "" || signed.Signature.Value == "" {
		return verifiedBundle{}, nil, nil, errors.New("missing signature facts")
	}
	sig, err := base64.StdEncoding.DecodeString(signed.Signature.Value)
	if err != nil {
		return verifiedBundle{}, nil, nil, errors.New("signature is not base64")
	}
	canonical, err := teamwire.SignedCanonical(raw)
	if err != nil {
		return verifiedBundle{}, nil, nil, err
	}
	presented, presentedErr := base64.StdEncoding.DecodeString(entry.OrgPublicKey)
	presentedOK := presentedErr == nil && len(presented) == ed25519.PublicKeySize
	var firstPin *orgKeyPin
	switch {
	case pin.KeyID != "" && signed.Signature.KeyID != pin.KeyID:
		// Already pinned: only the pinned key verifies. A different key_id is the
		// mismatch state, not a new trust decision (§5.16.2 d6).
		refusal := fmt.Errorf("it is signed by organization key %s while this device is pinned to %s; compare the new key's fingerprint with the team console's Policy page and trust it, or wait for the bundle to be signed again",
			signed.Signature.KeyID, pin.KeyID)
		if !presentedOK || !ed25519.Verify(presented, canonical, sig) {
			return verifiedBundle{}, nil, nil, refusal
		}
		return verifiedBundle{}, nil, &teamKeyMismatch{PinnedKeyID: pin.KeyID, PinnedFingerprint: pin.Fingerprint,
			PresentedKeyID: signed.Signature.KeyID, PresentedFingerprint: teamwire.KeyFingerprint(presented),
			presentedPublicKey: entry.OrgPublicKey}, refusal
	case pin.KeyID != "":
		pinned, err := base64.StdEncoding.DecodeString(pin.PublicKey)
		if err != nil || len(pinned) != ed25519.PublicKeySize {
			return verifiedBundle{}, nil, nil, errors.New("the pinned key is not a verification key")
		}
		if !ed25519.Verify(pinned, canonical, sig) {
			return verifiedBundle{}, nil, nil, errors.New("the signature does not verify against the pinned organization key")
		}
	default:
		// First contact: the pin is the key the server PRESENTS with the catalog
		// (the item-3c seam — org_public_key). The signature must verify against
		// it here, and the pin event's fingerprint is what the admin compares
		// with the console's Policy page.
		if !presentedOK {
			return verifiedBundle{}, nil, nil, errors.New("the catalog did not present a verification key to pin")
		}
		if !ed25519.Verify(presented, canonical, sig) {
			return verifiedBundle{}, nil, nil, errors.New("the signature does not verify against the key the server presented")
		}
		firstPin = &orgKeyPin{KeyID: signed.Signature.KeyID, PublicKey: entry.OrgPublicKey,
			Fingerprint: teamwire.KeyFingerprint(presented), PinnedAt: now.UTC().Format(time.RFC3339)}
	}
	verified, err := decodeVerified(raw, signed)
	if err != nil {
		return verifiedBundle{}, nil, nil, err
	}
	return verified, firstPin, nil, nil
}

// decodeVerified derives what the device compares from bytes whose signature has
// already verified.
func decodeVerified(raw []byte, signed teamwire.SignedBundle) (verifiedBundle, error) {
	expires, err := time.Parse(time.RFC3339, signed.ExpiresAt)
	if err != nil {
		return verifiedBundle{}, errors.New("the signed expiry is not a time")
	}
	scope, err := signedScope(signed.Scope)
	if err != nil {
		return verifiedBundle{}, err
	}
	signedDigest, err := teamwire.BundleSignedDigest(raw)
	if err != nil {
		return verifiedBundle{}, fmt.Errorf("the signed digest could not be computed: %w", err)
	}
	withoutKey, err := teamwire.BundleSignedDigestIgnoringKey(raw)
	if err != nil {
		return verifiedBundle{}, fmt.Errorf("the signed digest could not be computed: %w", err)
	}
	return verifiedBundle{raw: raw, signed: signed, scope: scope, expires: expires,
		signedDigest: signedDigest, digestWithoutKey: withoutKey}, nil
}

// Signed scope types (bundle.schema.json).
const (
	bundleScopeOrganization = "organization"
	bundleScopeRepository   = "repository"
)

// signedScope spells a signed scope the way the device keys its records.
func signedScope(scope teamwire.BundleScope) (string, error) {
	switch {
	case scope.Type == bundleScopeOrganization && scope.ID != "":
		return rulebook.ScopeOrganization, nil
	case scope.Type == bundleScopeRepository && scope.ID != "":
		return rulebook.ScopeRepositoryPrefix + scope.ID, nil
	}
	return "", fmt.Errorf("the signed scope %q is not one this device can resolve", scope.Type+":"+scope.ID)
}

// repositoryResolveLimit is how many governed sessions one pass reads to resolve
// folders to repositories: team.json's identity_upgrade_roots, the link's one bound on
// such a pass (its default loads unlinked too).
func (t *teamLinker) repositoryResolveLimit() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.doc.IdentityUpgradeRoots
}

// recordRepositoryResolutions writes the root→id pointer index for one tick (the
// index's PRODUCTION writer — postwork H-2): for every adopted or offered
// repository-scoped bundle, resolve each governed session's (root → repository_id)
// through the store — the daemon may read git config, the hook may not — and
// record it so the hook's stat-only lookup finds the layer. Sessions without a
// resolvable repository stay "not yet staged" in the hook's reason (§5.6.18's
// honest first rung). A failure logs once per tick and leaves the prior index.
func (t *teamLinker) recordRepositoryResolutions(available []teamLayerEntry) {
	repoScopes := map[string]bool{}
	for _, b := range available {
		if strings.HasPrefix(b.Scope, rulebook.ScopeRepositoryPrefix) {
			repoScopes[strings.TrimPrefix(b.Scope, rulebook.ScopeRepositoryPrefix)] = true
		}
	}
	if len(repoScopes) == 0 {
		return
	}
	sessions, err := governor.ix.EnvelopeSessionIDs(t.repositoryResolveLimit())
	if err != nil {
		log.Printf("team layers: repository resolutions could not be listed: %v", err)
		return
	}
	for _, sessionID := range sessions {
		id, root, ok, err := governor.ix.SessionRepository(sessionID)
		if err != nil || !ok || root == "" {
			continue
		}
		if !repoScopes[id] {
			continue // that repository has no bundle scope
		}
		if err := rulebook.SetRepositoryResolution(t.storeDir, root, id); err != nil {
			log.Printf("team layers: the repository pointer for %s could not be recorded: %v", root, err)
		}
	}
}
