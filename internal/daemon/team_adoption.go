package daemon

// The team adoption owner (team rest-of-release plan §4.1): adopting a verified
// bundle hands each document to its owner — bytes, selection, profile adoption record,
// then the adopted-bundle record — and Un-adopt takes it back and turns its places
// off. It also answers the orchestration hosts' questions about adopted agents
// (placeAdoptions) and says which adopted bundle requires session content.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"crossing-guard/internal/orchestration/profilefs"
	"crossing-guard/internal/rulebook"
	"crossing-guard/store"
	"crossing-guard/teamwire"
)

// Surfaces that can ask for an adoption act; the chained event records which did.
const (
	teamSurfaceConsole = "console"
	teamSurfaceCommand = "command"
)

// adoptionPlaces turns off the places an adoption governs. The store's own
// switch-off is the default; a host that caches a binding wires its own.
type adoptionPlaces interface {
	TurnOffAdoption(adoptionKey string, now time.Time) (store.AdoptionPlacesOff, error)
}

// storeAdoptionPlaces turns places off through the store's binding writers.
type storeAdoptionPlaces struct{ ix *store.Index }

func (places storeAdoptionPlaces) TurnOffAdoption(adoptionKey string, now time.Time) (store.AdoptionPlacesOff, error) {
	return places.ix.DisableAdoptionPlaces(adoptionKey, now.Unix())
}

// adoptionPlaceOwner returns who turns places off: the wired owner, or the store.
func (t *teamLinker) adoptionPlaceOwner() adoptionPlaces {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.places != nil {
		return t.places
	}
	if governor == nil {
		return nil
	}
	return storeAdoptionPlaces{ix: governor.ix}
}

// teamAdoptionProblem is an adoption refusal with the status the route answers.
type teamAdoptionProblem struct {
	status  int
	message string
}

func (p *teamAdoptionProblem) Error() string { return p.message }

func adoptionRefusal(status int, format string, args ...any) error {
	return &teamAdoptionProblem{status: status, message: fmt.Sprintf(format, args...)}
}

// adoptBundle adopts one verified, offered bundle. clientToken, when the caller sent
// one, must be the token of the offer it was shown; either way the device state the
// offer was built against must not have moved, or nothing changes. The offer is
// rebuilt before the call returns, so it reads adopted with the adopted bundle's own
// id and revision in the same response.
func (t *teamLinker) adoptBundle(scope, bundleID, clientToken, surface string) (teamLayerEntry, rulebook.AdoptedBundle, error) {
	t.adoptMu.Lock()
	defer t.adoptMu.Unlock()
	t.mu.Lock()
	linked := t.state == teamLinked
	organization := t.doc.Organization
	t.mu.Unlock()
	if !linked {
		return teamLayerEntry{}, rulebook.AdoptedBundle{}, adoptionRefusal(409, "the device is not linked; nothing to adopt")
	}
	// Only a VERIFIED, currently-offered bundle can be adopted, from the bytes this
	// device verified — never a re-fetch (postwork H-1).
	offer, err := t.offeredEntry(scope, bundleID)
	if err != nil {
		return teamLayerEntry{}, rulebook.AdoptedBundle{}, adoptionRefusal(404, "%s", err.Error())
	}
	if clientToken != offer.StateToken {
		t.replaceOffer(scope)
		return teamLayerEntry{}, rulebook.AdoptedBundle{}, adoptionRefusal(409, "the offer changed after it was shown; read it again before adopting — nothing was changed")
	}
	var signed teamwire.SignedBundle
	if err := json.Unmarshal(offer.RawSigned, &signed); err != nil || signed.ID != offer.ID {
		return teamLayerEntry{}, rulebook.AdoptedBundle{}, adoptionRefusal(500, "the verified bundle's bytes are no longer decodable; pull again")
	}
	verified, err := decodeVerified(offer.RawSigned, signed)
	if err != nil {
		return teamLayerEntry{}, rulebook.AdoptedBundle{}, adoptionRefusal(500, "the verified bundle's bytes are no longer decodable; pull again")
	}
	layers, err := rulebook.LoadLayers(t.storeDir)
	if err != nil {
		return teamLayerEntry{}, rulebook.AdoptedBundle{}, adoptionRefusal(500, "the adoption records could not be read")
	}
	previous, adopted := adoptedRecord(layers, signed.OrganizationID, scope)
	if why := revisionNotNewer(previous, adopted, verified); why != "" {
		return teamLayerEntry{}, rulebook.AdoptedBundle{}, adoptionRefusal(409, "%s", why)
	}
	if offer.StateToken != offerStateToken(offer, previous, adopted) {
		t.replaceOffer(scope)
		return teamLayerEntry{}, rulebook.AdoptedBundle{}, adoptionRefusal(409, "what this device adopted changed after the offer was shown; read it again before adopting — nothing was changed")
	}
	if err := t.offerStillCurrent(verified, offer); err != nil {
		t.replaceOffer(scope)
		return teamLayerEntry{}, rulebook.AdoptedBundle{}, err
	}
	record, result, err := t.applyAdoption(verified, offer, organization.Name)
	if err != nil {
		t.replaceOffer(scope)
		return teamLayerEntry{}, rulebook.AdoptedBundle{}, err
	}
	t.linkEvent("team.layer.adopted", adoptionEventDetail(record, result, surface))
	t.replaceOffer(scope)
	rebuilt, err := t.offeredEntry(scope, bundleID)
	if err != nil {
		return teamLayerEntry{}, rulebook.AdoptedBundle{}, adoptionRefusal(500, "the adoption was recorded but the offer could not be rebuilt")
	}
	return rebuilt, record, nil
}

// offerStillCurrent refuses an adoption whose agents moved on this device after the
// offer was built, before anything is staged, so a refused adoption changes nothing.
func (t *teamLinker) offerStillCurrent(v verifiedBundle, offer teamLayerEntry) error {
	owner, err := t.profileOwner()
	if err != nil {
		return adoptionRefusal(500, "the agent store is unavailable: %v", err)
	}
	plan, err := owner.PlanTeam(v.signed.OrganizationID, v.scope, teamProfileDocuments(v))
	if err != nil {
		return adoptionRefusal(409, "the bundle's agents could not be adopted: %v", err)
	}
	if plan.StateToken != offer.planToken {
		return adoptionRefusal(409, "an agent changed on this device after the offer was shown; read it again before adopting — nothing was changed")
	}
	return nil
}

// applyAdoption writes one adoption in the crash order of plan §4.1 decision 5 —
// bytes, selection, profile adoption record, adopted-bundle record — each step
// idempotent by digest, so adopting again completes an adoption that stopped
// part-way. A detectors document is never staged.
func (t *teamLinker) applyAdoption(v verifiedBundle, offer teamLayerEntry, organizationName string) (rulebook.AdoptedBundle, profilefs.AdoptTeamResult, error) {
	rulebookDigest := ""
	for _, document := range v.signed.Documents {
		if document.Kind != teamwire.BundleKindRulebook {
			continue
		}
		if _, err := rulebook.StageLayer(t.storeDir, document.Digest, []byte(document.Body)); err != nil {
			return rulebook.AdoptedBundle{}, profilefs.AdoptTeamResult{}, adoptionRefusal(500, "rule document %s could not be staged: %v", document.Name, err)
		}
		rulebookDigest = document.Digest
	}
	owner, err := t.profileOwner()
	if err != nil {
		return rulebook.AdoptedBundle{}, profilefs.AdoptTeamResult{}, adoptionRefusal(500, "the agent store is unavailable: %v", err)
	}
	result, err := owner.AdoptTeam(profilefs.AdoptTeamCommand{OrganizationID: v.signed.OrganizationID, Scope: v.scope,
		BundleID: v.signed.ID, Documents: teamProfileDocuments(v), ExpectedStateToken: offer.planToken, Pins: governorPinSource})
	if err != nil {
		if profilefs.ProblemCode(err) == "state_conflict" {
			return rulebook.AdoptedBundle{}, profilefs.AdoptTeamResult{}, adoptionRefusal(409, "an agent changed on this device after the offer was shown; read it again before adopting — nothing was changed")
		}
		return rulebook.AdoptedBundle{}, profilefs.AdoptTeamResult{}, adoptionRefusal(409, "the bundle's agents could not be adopted: %v", err)
	}
	record := rulebook.AdoptedBundle{OrganizationID: v.signed.OrganizationID, OrganizationName: organizationName,
		Scope: v.scope, BundleID: v.signed.ID, Revision: v.signed.Revision, KeyID: v.keyID(),
		FailureMode: v.signed.FailureMode.StatefulTier, ContentPolicy: v.signed.ContentPolicy,
		ExpiresAt: v.expires, AdoptedAt: t.now(), SignedDigest: v.signedDigest, RulebookDigest: rulebookDigest,
		SchemaVersion: v.signed.SchemaVersion, SignedDigestWithoutKey: v.digestWithoutKey}
	if err := rulebook.AdoptBundle(t.storeDir, record); err != nil {
		return rulebook.AdoptedBundle{}, profilefs.AdoptTeamResult{}, adoptionRefusal(500, "the adoption could not be recorded: %v", err)
	}
	return record, result, nil
}

func adoptionEventDetail(record rulebook.AdoptedBundle, result profilefs.AdoptTeamResult, surface string) string {
	agents, inUse := []string{}, []string{}
	for _, document := range result.Documents {
		if document.Outcome == profilefs.TeamDocumentCollision {
			inUse = append(inUse, document.ProfileID)
		} else {
			agents = append(agents, document.ProfileID)
		}
	}
	detail := fmt.Sprintf("adopted %s revision %d (bundle %s, organization %s, key %s, expires %s, failure mode %s) from the %s",
		record.Scope, record.Revision, record.BundleID, record.OrganizationID, record.KeyID,
		record.ExpiresAt.UTC().Format(time.RFC3339), record.FailureMode, surface)
	if record.RulebookDigest != "" {
		detail += "; rulebook " + record.RulebookDigest
	}
	if len(agents) > 0 {
		detail += "; agents " + strings.Join(agents, ", ")
	}
	if len(inUse) > 0 {
		detail += "; not adopted because the id is in use: " + strings.Join(inUse, ", ")
	}
	return detail
}

// teamUnadoptResult is what Un-adopt did.
type teamUnadoptResult struct {
	OrganizationID string
	Scope          string
	BundleID       string
	Revision       int64
	Found          bool
	PlacesOff      store.AdoptionPlacesOff
}

// unadoptBundle is the member's Un-adopt: the adoption's places are turned off, the
// profile adoption record is removed (its selections marked released), and the
// adopted-bundle record is removed, in that order, so a stop part-way leaves places
// off rather than running an agent nothing lists. It works unlinked: adoption records
// outlive the link. organizationID may be empty when one record holds the scope.
func (t *teamLinker) unadoptBundle(organizationID, scope, surface string) (teamUnadoptResult, error) {
	t.adoptMu.Lock()
	defer t.adoptMu.Unlock()
	layers, err := rulebook.LoadLayers(t.storeDir)
	if err != nil {
		return teamUnadoptResult{}, adoptionRefusal(500, "the adoption records could not be read")
	}
	record, found, err := t.recordToUnadopt(layers, organizationID, scope)
	if err != nil {
		return teamUnadoptResult{}, err
	}
	out := teamUnadoptResult{OrganizationID: record.OrganizationID, Scope: scope, BundleID: record.BundleID,
		Revision: record.Revision, Found: found, PlacesOff: store.AdoptionPlacesOff{Managed: []string{}}}
	if !found && organizationID != "" {
		out.OrganizationID = organizationID
	}
	owner, err := t.profileOwner()
	if err != nil {
		return teamUnadoptResult{}, adoptionRefusal(500, "the agent store is unavailable: %v", err)
	}
	target := out.OrganizationID
	if target != "" {
		if places := t.adoptionPlaceOwner(); places != nil {
			off, err := places.TurnOffAdoption(store.AdoptionKey(target, scope), t.now())
			if err != nil {
				return teamUnadoptResult{}, adoptionRefusal(500, "the adoption's places could not be turned off, so nothing was un-adopted: %v", err)
			}
			out.PlacesOff = off
		}
		if _, removed, err := owner.Unadopt(target, scope); err != nil {
			return teamUnadoptResult{}, adoptionRefusal(500, "the agents' adoption could not be removed: %v", err)
		} else if removed {
			out.Found = true
		}
	}
	if found {
		if _, err := rulebook.UnadoptBundle(t.storeDir, record.OrganizationID, scope); err != nil {
			return teamUnadoptResult{}, adoptionRefusal(500, "the un-adoption could not be recorded: %v", err)
		}
	}
	if out.Found {
		t.linkEvent("team.layer.unadopted", fmt.Sprintf("un-adopted %s (organization %s, bundle %s) from the %s; its rules no longer apply and %d of its places were turned off",
			scope, out.OrganizationID, out.BundleID, surface, out.PlacesOff.Count()))
	}
	t.replaceOffer(scope)
	return out, nil
}

// recordToUnadopt picks the adopted-bundle record an Un-adopt of scope means: the
// named organization's, else the linked organization's, else the only one for that
// scope. Two organizations' records for one scope need the organization named.
func (t *teamLinker) recordToUnadopt(layers rulebook.LayersDocument, organizationID, scope string) (rulebook.AdoptedBundle, bool, error) {
	if scope != rulebook.ScopeOrganization && !strings.HasPrefix(scope, rulebook.ScopeRepositoryPrefix) {
		return rulebook.AdoptedBundle{}, false, adoptionRefusal(400, "scope must be organization or repository:<id>")
	}
	if organizationID != "" {
		record, found := layers.Bundle(organizationID, scope)
		return record, found, nil
	}
	t.mu.Lock()
	linked := t.doc.Organization.ID
	t.mu.Unlock()
	if record, found := layers.Bundle(linked, scope); found && linked != "" {
		return record, true, nil
	}
	var only rulebook.AdoptedBundle
	count := 0
	for _, record := range layers.Adopted {
		if record.Scope == scope {
			only, count = record, count+1
		}
	}
	switch count {
	case 0:
		return rulebook.AdoptedBundle{OrganizationID: linked}, false, nil
	case 1:
		return only, true, nil
	}
	return rulebook.AdoptedBundle{}, false, adoptionRefusal(409, "more than one organization's bundle is adopted for %s; name the organization_id to un-adopt", scope)
}

// --- the content mandate ------------------------------------------------------------

// contentPolicyDocument is the signed content_policy object.
type contentPolicyDocument struct {
	SyncContent  string   `json:"sync_content"`
	Repositories []string `json:"repositories"`
}

// contentPolicyMandated is the sync_content value that requires session content.
const contentPolicyMandated = "mandated"

// adoptedContentMandate reads the content mandate from the adopted-bundle records of
// the linked organization — organization or repository scope, the first that requires
// content — and from nothing in an offer: a newer revision that is offered and not
// adopted cannot switch upload on (plan §4.1 decision 5, OD-27). An adopted bundle
// past its expiry requires nothing.
//
// It reads layers.json, which takes that file's own lock and the disk, so it is called
// WITHOUT t.mu held: the callers copy the organization id out under the lock and read
// the records after it (or before taking it). contentMandateIn is the decision alone.
func adoptedContentMandate(storeDir, organization string, now time.Time) *contentMandate {
	return contentMandateIn(readAdoptionRecords(storeDir), organization, now)
}

// readAdoptionRecords reads layers.json for the mandate. Records that cannot be read
// require nothing, and the reason is logged.
func readAdoptionRecords(storeDir string) rulebook.LayersDocument {
	layers, err := rulebook.LoadLayers(storeDir)
	if err != nil {
		log.Printf("team link: the adopted bundles could not be read, so no content is required: %v", err)
		return rulebook.LayersDocument{}
	}
	return layers
}

// contentMandateIn decides the mandate from adoption records already read. It reads
// nothing, so it may run under any lock.
func contentMandateIn(layers rulebook.LayersDocument, organization string, now time.Time) *contentMandate {
	if organization == "" {
		return nil
	}
	for _, record := range layers.Adopted {
		if record.OrganizationID != organization || len(record.ContentPolicy) == 0 || now.After(record.ExpiresAt) {
			continue
		}
		var policy contentPolicyDocument
		if json.Unmarshal(record.ContentPolicy, &policy) == nil && policy.SyncContent == contentPolicyMandated {
			return &contentMandate{BundleID: record.BundleID, Repositories: policy.Repositories}
		}
	}
	return nil
}

// --- placeAdoptions -----------------------------------------------------------------

// teamPlaceAdoptions answers the orchestration hosts and the roster from the two
// adoption records. It holds no state of its own.
type teamPlaceAdoptions struct{ linker *teamLinker }

// currentPlaceAdoptions is the adoption seam the hosts ask through: the team linker's
// when there is one, and "nothing is adopted" otherwise.
func currentPlaceAdoptions() placeAdoptions {
	if team == nil {
		return noPlaceAdoptions{}
	}
	return teamPlaceAdoptions{linker: team}
}

func splitAdoptionKey(adoptionKey string) (organizationID, scope string, ok bool) {
	organizationID, scope, ok = strings.Cut(adoptionKey, store.AdoptionKeySeparator)
	return organizationID, scope, ok && organizationID != "" && scope != ""
}

func (a teamPlaceAdoptions) KeyFor(profileID, sourceDigest, bundleDigest string) string {
	owner, err := a.linker.profileOwner()
	if err != nil {
		return ""
	}
	adoption, found, err := owner.AdoptionOfRevision(profileID, sourceDigest, bundleDigest)
	if err != nil || !found {
		return ""
	}
	return store.AdoptionKey(adoption.OrganizationID, adoption.Scope)
}

func (a teamPlaceAdoptions) HoldReason(adoptionKey, profileID, sourceDigest, bundleDigest string) string {
	if adoptionKey == "" {
		return ""
	}
	organizationID, scope, ok := splitAdoptionKey(adoptionKey)
	if !ok {
		return placeHoldAdoptionEnded
	}
	owner, err := a.linker.profileOwner()
	if err != nil {
		return placeHoldAdoptionEnded
	}
	adoption, found, err := owner.Adoption(organizationID, scope)
	if err != nil {
		// A record that cannot be read cannot vouch for the place; it starts no run.
		log.Printf("team adoption: the adoption record for %s could not be read: %v", scope, err)
		return placeHoldAdoptionEnded
	}
	if !found {
		return placeHoldAdoptionEnded
	}
	if !adoption.Lists(profileID, sourceDigest, bundleDigest) {
		return placeHoldVersionGone
	}
	layers, err := rulebook.LoadLayers(a.linker.storeDir)
	if err != nil {
		return placeHoldAdoptionEnded
	}
	record, found := layers.Bundle(organizationID, scope)
	if !found {
		return placeHoldAdoptionEnded // an adoption that stopped before its last step
	}
	if a.linker.now().After(record.ExpiresAt) {
		return placeHoldExpired
	}
	return ""
}

func (a teamPlaceAdoptions) MoveAllowed(adoptionKey, profileID, sourceDigest, bundleDigest string) bool {
	if adoptionKey == "" {
		return true
	}
	organizationID, scope, ok := splitAdoptionKey(adoptionKey)
	if !ok {
		return false
	}
	owner, err := a.linker.profileOwner()
	if err != nil {
		return false
	}
	adoption, found, err := owner.Adoption(organizationID, scope)
	return err == nil && found && adoption.Lists(profileID, sourceDigest, bundleDigest)
}

func (a teamPlaceAdoptions) Origin(profileID string) (placeAdoptionOrigin, bool) {
	owner, err := a.linker.profileOwner()
	if err != nil {
		return placeAdoptionOrigin{}, false
	}
	origin, found, err := owner.Provenance(profileID)
	if err != nil || !found || origin.SelectedBy != profilefs.SelectedByTeamAdoption {
		return placeAdoptionOrigin{}, false
	}
	out := placeAdoptionOrigin{ReadOnly: true}
	switch {
	case origin.Released != nil:
		out.OrganizationID, out.Scope, out.Released = origin.Released.OrganizationID, origin.Released.Scope, true
	case origin.Adoption != nil:
		out.OrganizationID, out.Scope, out.BundleID = origin.Adoption.OrganizationID, origin.Adoption.Scope, origin.Adoption.BundleID
	default:
		return out, true // an adoption wrote it and stopped before its record
	}
	out.OrganizationName = a.linker.organizationName(out.OrganizationID)
	if layers, err := rulebook.LoadLayers(a.linker.storeDir); err == nil {
		if record, found := layers.Bundle(out.OrganizationID, out.Scope); found {
			if !out.Released {
				out.Revision, out.ExpiresAt = record.Revision, record.ExpiresAt.UTC().Format(time.RFC3339)
			}
			if out.OrganizationName == "" {
				out.OrganizationName = record.OrganizationName
			}
		}
	}
	return out, true
}

// organizationName names an organization when this device is linked to it; the
// adopted-bundle record carries the name for every other case.
func (t *teamLinker) organizationName(organizationID string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.doc.Organization.ID == organizationID {
		return t.doc.Organization.Name
	}
	return ""
}

// asAdoptionProblem unwraps an adoption refusal for a route.
func asAdoptionProblem(err error) (*teamAdoptionProblem, bool) {
	var refusal *teamAdoptionProblem
	ok := errors.As(err, &refusal)
	return refusal, ok
}
