package daemon

// What a verified bundle becomes on this device (team rest-of-release plan §4.1
// decisions 1, 5, 6, 7): an offer that tells the truth about every document, a
// refresh with no prompt when — and only when — the signed content is the content
// already adopted, or a named reason it cannot be used. Nothing here trusts the
// catalog entry: it is compared with the signed document and refused when it differs.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"crossing-guard/engine"
	"crossing-guard/internal/orchestration/profilefs"
	"crossing-guard/internal/rulebook"
	"crossing-guard/internal/teamlink"
	"crossing-guard/profiledoc"
	"crossing-guard/ruledoc"
	"crossing-guard/teamwire"
)

// Document states an offer shows (plan §4.2, "the words in an offer"). They are data
// the surfaces draw; no code branches on what a surface calls them.
const (
	offerDocumentOffered    = "offered"      // usable, and not adopted yet
	offerDocumentAdopted    = "adopted"      // in force on this device
	offerDocumentCantBeUsed = "cant_be_used" // it does not parse; the whole bundle is not offered
	offerDocumentNotApplied = "not_applied"  // a kind this version does not apply (detectors)
	offerDocumentIDInUse    = "id_in_use"    // the member's own agent uses the id (OD-6)
)

// Offer statuses: where a verified bundle stands against what this device adopted.
const (
	offerStatusOffered = "offered" // nothing is adopted for its scope
	offerStatusAdopted = "adopted" // it is the adopted bundle
	offerStatusChanged = "changed" // another revision is adopted and this one differs; Adopt asks
	offerStatusPartial = "partial" // an adoption of it stopped part-way; Adopt completes it
)

// profileSourceName is the one source name the profile parser accepts.
const profileSourceName = "PROFILE.md"

// teamOfferDocument is one document of a bundle as the offer lists it: its kind, its
// state, and — for a profile — the agent it carries, its text, and the text diff when
// adopting would change an agent already adopted.
type teamOfferDocument struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Digest    string `json:"digest"`
	MediaType string `json:"media_type"`
	State     string `json:"state"`
	// Reason is the technical reason behind a cant_be_used or not_applied state.
	Reason      string `json:"reason,omitempty"`
	ProfileID   string `json:"profile_id,omitempty"`
	ProfileName string `json:"profile_name,omitempty"`
	// Change is what adopting does to the agent on this device (a profilefs team
	// document outcome: new, unchanged, update, collision).
	Change   string         `json:"change,omitempty"`
	Text     string         `json:"text,omitempty"`
	TextDiff []textDiffLine `json:"text_diff,omitempty"`
}

// teamOfferChange is one signed field that differs from the adopted bundle, each on
// its own labelled line: failure mode, content policy, bundle format, signing key.
type teamOfferChange struct {
	Field string `json:"field"`
	Label string `json:"label"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// teamRuleChanges is the rule diff against the adopted rulebook.
type teamRuleChanges struct {
	Added   []string             `json:"added"`
	Removed []string             `json:"removed"`
	Changed []ruledoc.RuleChange `json:"changed"`
}

// teamLayerEntry is one verified bundle as the offer shows it. Every field was
// decoded from the signed bytes.
type teamLayerEntry struct {
	ID             string              `json:"id"`
	OrganizationID string              `json:"organization_id"`
	Scope          string              `json:"scope"`
	Revision       int64               `json:"revision"`
	SchemaVersion  string              `json:"schema_version"`
	KeyID          string              `json:"key_id"`
	ExpiresAt      string              `json:"expires_at"`
	FailureMode    string              `json:"failure_mode"`
	Documents      []teamOfferDocument `json:"documents"`
	ContentPolicy  json.RawMessage     `json:"content_policy,omitempty"`
	Adopted        bool                `json:"adopted"`
	Status         string              `json:"status"`
	// AdoptedRevision is the revision in force for this scope when it is another one.
	AdoptedRevision int64             `json:"adopted_revision,omitempty"`
	Changes         []teamOfferChange `json:"changes,omitempty"`
	Rules           []rulePreview     `json:"rules,omitempty"`
	RuleChanges     *teamRuleChanges  `json:"rule_changes,omitempty"`
	// StateToken names the device state this offer was built against; Adopt refuses
	// when that state has moved.
	StateToken string `json:"state_token"`
	// RawSigned is the author's own signed bundle document, as verified by the
	// pull. It never crosses the loopback API (json "-"): the adopt path stages
	// from THESE bytes — the ones the device verified — never a re-fetch the
	// server could swap (the postwork H-1 TOCTOU fix).
	RawSigned []byte `json:"-"`
	// planToken is the profile owner's state token for this offer's plan.
	planToken string
}

type rulePreview struct {
	ID     string `json:"id"`
	Action string `json:"action"`
	Intent string `json:"intent,omitempty"`
}

// teamUnusableBundle is a verified bundle this device cannot use: "Revision N can't
// be used · still on revision M" as data, with the technical reason. Nothing from it
// is adopted and the adopted revision stays in force.
type teamUnusableBundle struct {
	ID             string `json:"id"`
	OrganizationID string `json:"organization_id"`
	Scope          string `json:"scope"`
	Revision       int64  `json:"revision"`
	// StillOnRevision is the adopted revision that stays in force; 0 when none is.
	StillOnRevision int64               `json:"still_on_revision,omitempty"`
	Code            string              `json:"code"`
	Document        string              `json:"document,omitempty"`
	Reason          string              `json:"reason"`
	Documents       []teamOfferDocument `json:"documents"`
}

// Codes for a bundle that verified and still cannot be used, beside the wire's cap
// codes.
const (
	codeBundleNothingUsable = "bundle_nothing_usable"
)

// profileOwner returns the profile owner for this device's data directory.
func (t *teamLinker) profileOwner() (*profilefs.Owner, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.profiles == nil {
		owner, err := profilefs.New(t.storeDir)
		if err != nil {
			return nil, err
		}
		t.profiles = owner
	}
	return t.profiles, nil
}

// assessBundle takes one bundle whose signature verified through every check that
// stands between verification and an offer, in this order: the catalog entry agrees
// with the signed document; the signed organization is the linked one; the caps (before
// any parser runs); each body matches its digest; the revision is newer than the
// adopted one; every document parses; something in it is usable. The caller holds
// adoptMu.
func (t *teamLinker) assessBundle(entry teamwire.BundleEntry, v verifiedBundle, organization teamlink.Organization, out *pullOutcome) {
	if field := catalogDisagreement(entry, v); field != "" {
		out.refuse(entry.ID, "the catalog entry's "+field+" differs from the signed document; nothing from it is recorded")
		return
	}
	if v.signed.OrganizationID != organization.ID {
		out.refuse(v.signed.ID, "it is signed for organization "+v.signed.OrganizationID+", and this device is linked to "+organization.ID)
		return
	}
	layers, err := rulebook.LoadLayers(t.storeDir)
	if err != nil {
		out.refuse(v.signed.ID, "the adoption records could not be read: "+err.Error())
		return
	}
	record, adopted := adoptedRecord(layers, v.signed.OrganizationID, v.scope)
	unusable := teamUnusableBundle{ID: v.signed.ID, OrganizationID: v.signed.OrganizationID, Scope: v.scope,
		Revision: v.signed.Revision, Documents: offerDocuments(v)}
	if adopted {
		unusable.StillOnRevision = record.Revision
	}
	if code, index := teamwire.CheckBundleCaps(v.signed); code != "" {
		unusable.Code, unusable.Reason = code, capReason(code, index, v)
		if code == teamwire.CodeBundleBodyTooLarge || code == teamwire.CodeBundleTooManyRulebooks {
			unusable.Document = v.signed.Documents[index].Name
		}
		out.unusable = append(out.unusable, unusable)
		return
	}
	if why := revisionNotNewer(record, adopted, v); why != "" {
		out.refuse(v.signed.ID, why)
		return
	}
	if index, why := firstUnusableDocument(v); index >= 0 {
		unusable.Code, unusable.Document, unusable.Reason = teamwire.CodeBundleDocumentInvalid, v.signed.Documents[index].Name, why
		unusable.Documents[index].State, unusable.Documents[index].Reason = offerDocumentCantBeUsed, why
		out.unusable = append(out.unusable, unusable)
		return
	}
	if !hasAppliedDocument(v) {
		unusable.Code, unusable.Reason = codeBundleNothingUsable, "the bundle carries no rulebook and no agent; this version applies nothing else"
		out.unusable = append(out.unusable, unusable)
		return
	}
	if adopted && record.BundleID != v.signed.ID {
		t.refreshIfUnchanged(record, v, layers, organization.Name)
	}
	offer, err := t.buildOffer(v)
	if err != nil {
		unusable.Code, unusable.Reason = teamwire.CodeBundleDocumentInvalid, err.Error()
		out.unusable = append(out.unusable, unusable)
		return
	}
	out.available = append(out.available, offer)
}

// adoptedRecord finds the adopted-bundle record a bundle of (organization, scope)
// replaces: the pair's own record, or a format version 1 record for the scope, which
// named no organization.
func adoptedRecord(layers rulebook.LayersDocument, organizationID, scope string) (rulebook.AdoptedBundle, bool) {
	if record, found := layers.Bundle(organizationID, scope); found {
		return record, true
	}
	return layers.Bundle("", scope)
}

// catalogDisagreement names the first field of the unsigned catalog entry that
// differs from the signed document, or "" when they agree. The catalog entry is an
// index: a difference means the server describes one bundle and serves another.
func catalogDisagreement(entry teamwire.BundleEntry, v verifiedBundle) string {
	if entry.ID != v.signed.ID {
		return "id"
	}
	if entry.Revision != v.signed.Revision {
		return "revision"
	}
	if entry.Scope != v.scope {
		return "scope"
	}
	if expires, err := time.Parse(time.RFC3339, entry.ExpiresAt); err != nil || !expires.Equal(v.expires) {
		return "expiry"
	}
	if entry.FailureMode != v.signed.FailureMode.StatefulTier {
		return "failure mode"
	}
	if len(entry.Documents) != len(v.signed.Documents) {
		return "document list"
	}
	for index, listed := range entry.Documents {
		signed := v.signed.Documents[index]
		if listed.Kind != signed.Kind || listed.Name != signed.Name || listed.Digest != signed.Digest || listed.MediaType != signed.MediaType {
			return "document list"
		}
	}
	return ""
}

func capReason(code string, index int, v verifiedBundle) string {
	switch code {
	case teamwire.CodeBundleTooManyDocuments:
		return fmt.Sprintf("the bundle carries %d documents; a bundle carries at most %d", len(v.signed.Documents), teamwire.BundleMaxDocuments)
	case teamwire.CodeBundleTooManyRulebooks:
		return fmt.Sprintf("the bundle carries more than %d rulebook; %s is one too many", teamwire.BundleMaxRulebooks, v.signed.Documents[index].Name)
	case teamwire.CodeBundleBodyTooLarge:
		return fmt.Sprintf("document %s is %d bytes; a document is at most %d", v.signed.Documents[index].Name,
			len(v.signed.Documents[index].Body), teamwire.BundleMaxBodyBytes)
	}
	return code
}

// revisionNotNewer refuses a bundle whose revision is not greater than the adopted
// record's for its (organization, scope) — a replayed or rolled-back catalog — except
// the adopted bundle itself, which is the same id with the same signed content.
func revisionNotNewer(record rulebook.AdoptedBundle, adopted bool, v verifiedBundle) string {
	if !adopted {
		return ""
	}
	if record.BundleID == v.signed.ID {
		if record.SignedDigest != v.signedDigest {
			return fmt.Sprintf("it reuses the adopted bundle's id %s with different signed content", v.signed.ID)
		}
		return ""
	}
	if v.signed.Revision <= record.Revision {
		return fmt.Sprintf("revision %d is not newer than the adopted revision %d for %s", v.signed.Revision, record.Revision, v.scope)
	}
	return ""
}

func bodyDigest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// firstUnusableDocument parses every document with the parser its owner uses and
// returns the first that fails, with the reason. A detectors document is not parsed:
// this version does not apply it.
func firstUnusableDocument(v verifiedBundle) (int, string) {
	for index, document := range v.signed.Documents {
		if bodyDigest(document.Body) != document.Digest {
			return index, "document " + document.Name + " does not match its digest"
		}
		switch document.Kind {
		case teamwire.BundleKindRulebook:
			if _, err := ruledoc.Parse([]byte(document.Body)); err != nil {
				return index, "rule document " + document.Name + " does not parse: " + err.Error()
			}
		case teamwire.BundleKindProfile:
			if _, err := profiledoc.Parse(profileSourceName, []byte(document.Body)); err != nil {
				return index, "agent document " + document.Name + " does not parse: " + err.Error()
			}
		case teamwire.BundleKindDetectors:
		default:
			return index, "document " + document.Name + " is of kind " + document.Kind + ", which this version does not know"
		}
	}
	return -1, ""
}

func hasAppliedDocument(v verifiedBundle) bool {
	for _, document := range v.signed.Documents {
		if document.Kind == teamwire.BundleKindRulebook || document.Kind == teamwire.BundleKindProfile {
			return true
		}
	}
	return false
}

// offerDocuments lists a bundle's documents with their starting states: a detectors
// document is not applied, everything else is offered until a check says otherwise.
func offerDocuments(v verifiedBundle) []teamOfferDocument {
	out := make([]teamOfferDocument, 0, len(v.signed.Documents))
	for _, document := range v.signed.Documents {
		item := teamOfferDocument{Kind: document.Kind, Name: document.Name, Digest: document.Digest,
			MediaType: document.MediaType, State: offerDocumentOffered}
		if document.Kind == teamwire.BundleKindDetectors {
			item.State, item.Reason = offerDocumentNotApplied, "shared detectors are not applied by this version"
		}
		out = append(out, item)
	}
	return out
}

// Refresh verdicts.
const (
	refreshAsks     = "asks"     // a signed field differs: the person decides
	refreshSilent   = "silent"   // the signed digests are equal
	refreshRepinned = "repinned" // the one difference is the key id a re-pin confirmed
)

// refreshVerdict is the whole rule for refreshing without a prompt (plan §4.1
// decision 5, "exact"). A pulled, verified bundle refreshes silently only when its
// signed digest — every signed field but id, revision, expires_at, created_at and the
// signature value — equals the adopted record's. The one exception: after a chained
// re-pin, a bundle whose digest without the key id equals the record's, signed by the
// current pin, where the record's key id is the pin that re-pin replaced. Everything
// else asks.
func refreshVerdict(record rulebook.AdoptedBundle, v verifiedBundle, pinnedKeyID, replacedKeyID string) string {
	if record.SignedDigest != "" && record.SignedDigest == v.signedDigest {
		return refreshSilent
	}
	repinned := replacedKeyID != "" && record.KeyID == replacedKeyID && v.keyID() == pinnedKeyID && v.keyID() != record.KeyID
	if repinned && record.SignedDigestWithoutKey != "" && record.SignedDigestWithoutKey == v.digestWithoutKey {
		return refreshRepinned
	}
	return refreshAsks
}

// refreshIfUnchanged applies the refresh rule to a newer revision of an adopted
// (organization, scope). On a refresh it moves the bundle id in the profile adoption
// record, then the bundle id, revision and expiry in the adopted-bundle record, and
// chains team.bundle.refreshed. A failure leaves the adopted bundle in force and the
// bundle offered as changed.
func (t *teamLinker) refreshIfUnchanged(record rulebook.AdoptedBundle, v verifiedBundle, layers rulebook.LayersDocument, organizationName string) {
	verdict := refreshVerdict(record, v, layers.PinnedOrgKeyID, layers.ReplacedOrgKeyID)
	if verdict == refreshAsks {
		return
	}
	owner, err := t.profileOwner()
	if err != nil {
		log.Printf("team layers: refresh of %s not applied: %v", v.scope, err)
		return
	}
	if _, found, err := owner.Adoption(record.OrganizationID, v.scope); err != nil {
		log.Printf("team layers: refresh of %s not applied: %v", v.scope, err)
		return
	} else if found {
		if _, err := owner.RefreshAdoption(record.OrganizationID, v.scope, v.signed.ID); err != nil {
			log.Printf("team layers: refresh of %s not applied: %v", v.scope, err)
			return
		}
	}
	refresh := rulebook.LayerRefresh{BundleID: v.signed.ID, Revision: v.signed.Revision, ExpiresAt: v.expires, OrganizationName: organizationName}
	detail := fmt.Sprintf("refreshed %s to revision %d (bundle %s, expires %s) with no prompt: its signed content is the content adopted at revision %d",
		v.scope, v.signed.Revision, v.signed.ID, v.signed.ExpiresAt, record.Revision)
	if verdict == refreshRepinned {
		refresh.KeyID, refresh.SignedDigest = v.keyID(), v.signedDigest
		detail += "; its only difference is the signing key " + v.keyID() + ", which a re-pin on this device confirmed in place of " + record.KeyID
	}
	if err := rulebook.RefreshTeamLayer(t.storeDir, record.OrganizationID, v.scope, refresh); err != nil {
		log.Printf("team layers: refresh of %s not recorded: %v", v.scope, err)
		return
	}
	t.linkEvent("team.bundle.refreshed", detail)
}

// buildOffer builds the truthful offer for one usable verified bundle against the
// device's state right now: the adopted-bundle record, the profile adoption record,
// and every agent the bundle's profiles would touch.
func (t *teamLinker) buildOffer(v verifiedBundle) (teamLayerEntry, error) {
	layers, err := rulebook.LoadLayers(t.storeDir)
	if err != nil {
		return teamLayerEntry{}, err
	}
	owner, err := t.profileOwner()
	if err != nil {
		return teamLayerEntry{}, err
	}
	plan, err := owner.PlanTeam(v.signed.OrganizationID, v.scope, teamProfileDocuments(v))
	if err != nil {
		return teamLayerEntry{}, err
	}
	record, adopted := adoptedRecord(layers, v.signed.OrganizationID, v.scope)
	offer := teamLayerEntry{ID: v.signed.ID, OrganizationID: v.signed.OrganizationID, Scope: v.scope,
		Revision: v.signed.Revision, SchemaVersion: v.signed.SchemaVersion, KeyID: v.keyID(),
		ExpiresAt: v.signed.ExpiresAt, FailureMode: v.signed.FailureMode.StatefulTier,
		ContentPolicy: v.signed.ContentPolicy, RawSigned: v.raw, planToken: plan.StateToken,
		Documents: offerDocuments(v), Status: offerStatusOffered}
	switch {
	case adopted && record.BundleID == v.signed.ID:
		offer.Status, offer.Adopted = offerStatusAdopted, true
	case plan.Current != nil && plan.Current.BundleID == v.signed.ID:
		offer.Status = offerStatusPartial
	case adopted:
		offer.Status, offer.AdoptedRevision = offerStatusChanged, record.Revision
		offer.Changes = signedFieldChanges(record, v)
	}
	if adopted && !offer.Adopted {
		offer.AdoptedRevision = record.Revision
	}
	fillProfileDocuments(&offer, plan)
	t.fillRules(&offer, v, record, adopted)
	offer.StateToken = offerStateToken(offer, record, adopted)
	return offer, nil
}

// teamProfileDocuments are a bundle's profile documents as the profile owner takes
// them.
func teamProfileDocuments(v verifiedBundle) []profilefs.TeamDocument {
	out := []profilefs.TeamDocument{}
	for _, document := range v.signed.Documents {
		if document.Kind == teamwire.BundleKindProfile {
			out = append(out, profilefs.TeamDocument{Name: document.Name, Source: []byte(document.Body)})
		}
	}
	return out
}

// fillProfileDocuments writes each profile document's agent, text and state from the
// profile owner's plan, and marks the other applied documents adopted when the bundle
// is.
func fillProfileDocuments(offer *teamLayerEntry, plan profilefs.TeamPlan) {
	next := 0
	for index := range offer.Documents {
		document := &offer.Documents[index]
		if document.Kind != teamwire.BundleKindProfile {
			if offer.Adopted && document.State == offerDocumentOffered {
				document.State = offerDocumentAdopted
			}
			continue
		}
		planned := plan.Documents[next]
		next++
		document.ProfileID, document.ProfileName = planned.ProfileID, planned.ProfileName
		document.Change, document.Text = planned.Outcome, planned.Source
		switch {
		case planned.Outcome == profilefs.TeamDocumentCollision:
			document.State = offerDocumentIDInUse
			document.Reason = "your own agent already uses the id " + planned.ProfileID
		case offer.Adopted:
			document.State = offerDocumentAdopted
		case planned.Outcome == profilefs.TeamDocumentUpdate:
			document.TextDiff = textDiff(planned.PreviousSource, planned.Source)
		}
	}
}

// fillRules writes the rule preview and, when another revision is adopted, the rule
// diff against its staged rulebook.
func (t *teamLinker) fillRules(offer *teamLayerEntry, v verifiedBundle, record rulebook.AdoptedBundle, adopted bool) {
	var after *engine.Policy
	for _, document := range v.signed.Documents {
		if document.Kind != teamwire.BundleKindRulebook {
			continue
		}
		parsed, err := ruledoc.Parse([]byte(document.Body))
		if err != nil {
			return // assessBundle parsed it already; nothing to preview
		}
		after = parsed
		for _, rule := range parsed.Rules {
			offer.Rules = append(offer.Rules, rulePreview{ID: rule.ID, Action: rule.Action, Intent: rule.Intent})
		}
	}
	if !adopted || offer.Adopted {
		return
	}
	before := &engine.Policy{}
	if record.RulebookDigest != "" {
		if staged, err := readStagedRulebook(t.storeDir, record.RulebookDigest); err == nil {
			before = staged
		}
	}
	if after == nil {
		after = &engine.Policy{}
	}
	added, removed, changed := ruledoc.MechanicalDiff(before, after)
	if len(added)+len(removed)+len(changed) > 0 {
		offer.RuleChanges = &teamRuleChanges{Added: nonNilStrings(added), Removed: nonNilStrings(removed), Changed: changed}
		if offer.RuleChanges.Changed == nil {
			offer.RuleChanges.Changed = []ruledoc.RuleChange{}
		}
	}
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// readStagedRulebook parses an adopted rulebook from the layer store.
func readStagedRulebook(storeDir, digest string) (*engine.Policy, error) {
	_, _, dir := rulebook.LayersPathsIn(storeDir)
	raw, err := os.ReadFile(filepath.Join(dir, "documents", strings.Replace(digest, ":", "-", 1)+".json"))
	if err != nil {
		return nil, err
	}
	return ruledoc.Parse(raw)
}

// signedFieldChanges lists the signed fields that differ from the adopted record,
// each as its own labelled line.
func signedFieldChanges(record rulebook.AdoptedBundle, v verifiedBundle) []teamOfferChange {
	changes := []teamOfferChange{}
	add := func(field, label, from, to string) {
		if from != to {
			changes = append(changes, teamOfferChange{Field: field, Label: label, From: from, To: to})
		}
	}
	add("failure_mode", "Failure mode", record.FailureMode, v.signed.FailureMode.StatefulTier)
	add("content_policy", "Content policy", compactSignedJSON(record.ContentPolicy), compactSignedJSON(v.signed.ContentPolicy))
	if record.SchemaVersion != "" {
		add("schema_version", "Bundle format", record.SchemaVersion, v.signed.SchemaVersion)
	}
	if record.KeyID != "" {
		add("key_id", "Signing key", record.KeyID, v.keyID())
	}
	return changes
}

// compactSignedJSON spells a signed object on one line for comparison and display; an
// absent object is the empty string.
func compactSignedJSON(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	var buffer bytes.Buffer
	if err := json.Compact(&buffer, raw); err != nil {
		return string(raw)
	}
	return buffer.String()
}

// offerStateToken names the device state an offer was built against: the profile
// owner's plan token and the adopted-bundle record for the scope.
func offerStateToken(offer teamLayerEntry, record rulebook.AdoptedBundle, adopted bool) string {
	parts := []string{offer.ID, offer.planToken, "none"}
	if adopted {
		parts[2] = record.BundleID + "\x00" + record.SignedDigest + "\x00" + fmt.Sprint(record.Revision)
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// errOfferGone reports an adopt of a bundle the last pull did not verify.
var errOfferGone = errors.New("no verified bundle offers that scope and id; pull again and check the Team console")

// offeredEntry finds a verified offer by scope and bundle id.
func (t *teamLinker) offeredEntry(scope, bundleID string) (teamLayerEntry, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, entry := range t.available {
		if entry.ID == bundleID && entry.Scope == scope {
			return entry, nil
		}
	}
	return teamLayerEntry{}, errOfferGone
}

// replaceOffer rebuilds the offers of one scope against the device's state now and
// stores them, so the offer is true in the same call that changed that state.
func (t *teamLinker) replaceOffer(scope string) {
	t.mu.Lock()
	entries := append([]teamLayerEntry(nil), t.available...)
	gen := t.gen
	t.mu.Unlock()
	for index := range entries {
		if entries[index].Scope != scope {
			continue
		}
		var signed teamwire.SignedBundle
		if err := json.Unmarshal(entries[index].RawSigned, &signed); err != nil {
			continue
		}
		verified, err := decodeVerified(entries[index].RawSigned, signed)
		if err != nil {
			continue
		}
		if rebuilt, err := t.buildOffer(verified); err == nil {
			entries[index] = rebuilt
		}
	}
	t.mu.Lock()
	if t.gen == gen {
		t.available = entries
	}
	t.mu.Unlock()
}
