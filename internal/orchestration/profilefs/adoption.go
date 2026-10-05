package profilefs

// Team adoption of shared agents (team rest-of-release plan §4.1 decisions 3–5).
// An adoption record sits beside the selections, one per (organization, scope), and
// says where a profile came from: bytes and digests alone cannot, because one set of
// bytes can be local, in the organization bundle and in a repository bundle at once.
// Expiry and policy are not here; they live in the adopted-bundle record the rulebook
// owner keeps.

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Who wrote a selection revision. A revision records exactly one of these.
const (
	SelectedByLocalClient  = "authenticated-local-client"
	SelectedByTeamAdoption = "team-adoption"
)

// What adopting one team profile document does on this device.
const (
	// TeamDocumentNew: no agent uses the id; adoption writes the selection.
	TeamDocumentNew = "new"
	// TeamDocumentUnchanged: the current selection already has these digests; nothing
	// is written to it (the lead's own device, or an unchanged profile in a new
	// revision).
	TeamDocumentUnchanged = "unchanged"
	// TeamDocumentUpdate: an adoption of the same (organization, scope) wrote the
	// current selection and the digests differ; adoption writes a new current revision.
	TeamDocumentUpdate = "update"
	// TeamDocumentCollision: an agent the adoption did not write uses the id; the
	// document is not adopted and the member's agent is never replaced (OD-6).
	TeamDocumentCollision = "collision"
)

const (
	adoptionFormat       = "crossing-guard-orchestration-profile-adoption-v1"
	adoptionTokenFrame   = "crossing-guard-orchestration-profile-adoption-state-v1\x00"
	adoptionAbsentFrame  = "crossing-guard-orchestration-profile-adoption-absent-v1\x00"
	adoptionKeyFrame     = "crossing-guard-orchestration-profile-adoption-key-v1\x00"
	teamPlanTokenFrame   = "crossing-guard-orchestration-profile-team-plan-v1\x00"
	adoptionsDirectory   = "adoptions"
	scopeOrganization    = "organization"
	scopeRepositoryStart = "repository:"
)

// SelectionRelease marks a selection an adoption wrote and Un-adopt (or a later
// revision that dropped the profile) let go of. The agent stays listed, inert, as no
// longer shared; a later adoption of the same (organization, scope) treats the
// selection as its own.
type SelectionRelease struct {
	OrganizationID string `json:"organization_id"`
	Scope          string `json:"scope"`
	ReleasedAt     string `json:"released_at"`
}

// AdoptionDocument is one profile an adoption lists.
type AdoptionDocument struct {
	ProfileID    string `json:"profile_id"`
	Name         string `json:"name"`
	SourceDigest string `json:"source_digest"`
	BundleDigest string `json:"bundle_digest"`
}

// Adoption is the adoption record of one (organization, scope).
type Adoption struct {
	FormatVersion  string             `json:"format_version"`
	OrganizationID string             `json:"organization_id"`
	Scope          string             `json:"scope"`
	BundleID       string             `json:"bundle_id"`
	AdoptedAt      string             `json:"adopted_at"`
	Documents      []AdoptionDocument `json:"documents"`
	StateToken     string             `json:"state_token"`
}

// Lists reports whether the adoption lists exactly this revision of the profile.
func (adoption Adoption) Lists(profileID, sourceDigest, bundleDigest string) bool {
	for _, document := range adoption.Documents {
		if document.ProfileID == profileID && document.SourceDigest == sourceDigest && document.BundleDigest == bundleDigest {
			return true
		}
	}
	return false
}

func (adoption Adoption) names(profileID string) bool {
	for _, document := range adoption.Documents {
		if document.ProfileID == profileID {
			return true
		}
	}
	return false
}

// TeamDocument is one profile document of a verified bundle: its name in the bundle
// and its exact PROFILE.md bytes.
type TeamDocument struct {
	Name   string
	Source []byte
}

// TeamDocumentPlan is what adopting one document would do, decided against the
// device's current state. PreviousSource is the current text when the outcome is an
// update, so a caller can show the diff before Adopt.
type TeamDocumentPlan struct {
	Name           string `json:"name"`
	ProfileID      string `json:"profile_id"`
	ProfileName    string `json:"profile_name"`
	Version        string `json:"version"`
	SourceDigest   string `json:"source_digest"`
	BundleDigest   string `json:"bundle_digest"`
	Outcome        string `json:"outcome"`
	Source         string `json:"source"`
	PreviousSource string `json:"previous_source,omitempty"`
	// Adopted is true when the adoption record already lists this exact revision.
	Adopted bool `json:"adopted"`
}

// TeamPlan is PlanTeam's answer. StateToken covers the adoption record and every
// selection (and draft) the plan read; AdoptTeam refuses when it no longer matches.
type TeamPlan struct {
	OrganizationID string             `json:"organization_id"`
	Scope          string             `json:"scope"`
	Documents      []TeamDocumentPlan `json:"documents"`
	StateToken     string             `json:"state_token"`
	// Current is the adoption record as it stands, when there is one.
	Current *Adoption `json:"current,omitempty"`
}

// AdoptTeamCommand adopts a verified bundle's profile documents for one
// (organization, scope). Pins returns the pin source for one profile id; it is
// consulted only when an update would trim that agent's bounded history.
type AdoptTeamCommand struct {
	OrganizationID     string
	Scope              string
	BundleID           string
	Documents          []TeamDocument
	ExpectedStateToken string
	Pins               func(profileID string) PinSource
}

// AdoptTeamResult is what an adoption did: the record it wrote and, per document, the
// outcome that was applied.
type AdoptTeamResult struct {
	Adoption  Adoption           `json:"adoption"`
	Documents []TeamDocumentPlan `json:"documents"`
}

// adoptedReadOnly refuses a local edit of an agent whose current selection an
// adoption wrote (OD-21). Duplicate makes the member's own agent under a new id.
func adoptedReadOnly(current *selectionRecord) error {
	if current == nil || current.Current.SelectedBy != SelectedByTeamAdoption {
		return nil
	}
	return problem("adopted_read_only", "id", "This agent is shared by your team and is read-only on this device.",
		"Duplicate it to make your own agent under a new id, then edit the duplicate.")
}

func validAdoptionScope(scope string) bool {
	return scope == scopeOrganization ||
		(strings.HasPrefix(scope, scopeRepositoryStart) && len(scope) > len(scopeRepositoryStart))
}

func validateRelease(record selectionRecord) error {
	if record.Released == nil {
		return nil
	}
	if record.Current.SelectedBy != SelectedByTeamAdoption || record.Released.OrganizationID == "" ||
		!validAdoptionScope(record.Released.Scope) {
		return errors.New("invalid profile selection release")
	}
	if _, err := time.Parse(time.RFC3339Nano, record.Released.ReleasedAt); err != nil {
		return errors.New("invalid profile selection release time")
	}
	return nil
}

func adoptionKey(value string) string {
	return strings.Replace(framedDigest(adoptionKeyFrame, []byte(value)), ":", "-", 1)
}

func (owner *Owner) adoptionPath(organizationID, scope string) string {
	return filepath.Join(owner.root, adoptionsDirectory, adoptionKey(organizationID), adoptionKey(scope)+".json")
}

func adoptionAbsentToken(organizationID, scope string) string {
	return framedDigest(adoptionAbsentFrame, []byte(organizationID+"\x00"+scope))
}

func adoptionStateToken(adoption Adoption) (string, error) {
	unsigned := adoption
	unsigned.StateToken = ""
	body, err := marshalCanonical(unsigned)
	if err != nil {
		return "", err
	}
	return framedDigest(adoptionTokenFrame, body), nil
}

func validateAdoption(adoption Adoption) error {
	if adoption.FormatVersion != adoptionFormat || adoption.OrganizationID == "" || !validAdoptionScope(adoption.Scope) ||
		len(adoption.Documents) > maxAdoptionDocuments {
		return errors.New("invalid profile adoption record")
	}
	if _, err := time.Parse(time.RFC3339Nano, adoption.AdoptedAt); err != nil {
		return errors.New("invalid profile adoption time")
	}
	for _, document := range adoption.Documents {
		if !profileIDPattern.MatchString(document.ProfileID) || !validDigest(document.SourceDigest) || !validDigest(document.BundleDigest) {
			return errors.New("invalid profile adoption document")
		}
	}
	token, err := adoptionStateToken(adoption)
	if err != nil || token != adoption.StateToken {
		return errors.New("profile adoption state token mismatch")
	}
	return nil
}

// maxAdoptionDocuments bounds a stored record; a bundle carries far fewer (the wire
// cap is checked before anything reaches this owner).
const maxAdoptionDocuments = 64

func (owner *Owner) readAdoptionPath(path string) (*Adoption, error) {
	raw, err := readRegularFile(path, maxJSONBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var adoption Adoption
	if err := decodeStrictJSON(raw, &adoption); err != nil {
		return nil, err
	}
	if err := validateAdoption(adoption); err != nil {
		return nil, err
	}
	if adoption.Documents == nil {
		adoption.Documents = []AdoptionDocument{}
	}
	return &adoption, nil
}

func (owner *Owner) readAdoption(organizationID, scope string) (*Adoption, error) {
	if err := owner.checkOwnerRoot(); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	adoption, err := owner.readAdoptionPath(owner.adoptionPath(organizationID, scope))
	if err != nil || adoption == nil {
		return nil, err
	}
	if adoption.OrganizationID != organizationID || adoption.Scope != scope {
		return nil, errors.New("profile adoption identity mismatch")
	}
	return adoption, nil
}

func (owner *Owner) writeAdoption(adoption Adoption) error {
	if err := validateAdoption(adoption); err != nil {
		return err
	}
	path := owner.adoptionPath(adoption.OrganizationID, adoption.Scope)
	dir := filepath.Dir(path)
	if err := owner.ensureOwnedDirectory(dir); err != nil {
		return err
	}
	body, err := marshalIndented(adoption)
	if err != nil {
		return err
	}
	return atomicReplace(dir, path, body, ".adoption-*")
}

// Adoption returns the adoption record of one (organization, scope).
func (owner *Owner) Adoption(organizationID, scope string) (Adoption, bool, error) {
	adoption, err := owner.readAdoption(organizationID, scope)
	if err != nil {
		return Adoption{}, false, integrityConflict(err)
	}
	if adoption == nil {
		return Adoption{}, false, nil
	}
	return *adoption, true, nil
}

// Adoptions lists every adoption record, ordered by organization and scope. Records
// stay after an unlink and across a relink, keyed by organization id.
func (owner *Owner) Adoptions() ([]Adoption, error) {
	out := []Adoption{}
	if err := owner.checkOwnerRoot(); errors.Is(err, fs.ErrNotExist) {
		return out, nil
	} else if err != nil {
		return nil, storageProblem(err)
	}
	root := filepath.Join(owner.root, adoptionsDirectory)
	organizations, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, storageProblem(err)
	}
	for _, organization := range organizations {
		if !organization.IsDir() {
			continue
		}
		scopes, err := os.ReadDir(filepath.Join(root, organization.Name()))
		if err != nil {
			return nil, storageProblem(err)
		}
		for _, scope := range scopes {
			if scope.IsDir() || !strings.HasSuffix(scope.Name(), ".json") {
				continue
			}
			adoption, err := owner.readAdoptionPath(filepath.Join(root, organization.Name(), scope.Name()))
			if err != nil {
				return nil, integrityConflict(err)
			}
			if adoption != nil {
				out = append(out, *adoption)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].OrganizationID != out[j].OrganizationID {
			return out[i].OrganizationID < out[j].OrganizationID
		}
		return out[i].Scope < out[j].Scope
	})
	return out, nil
}

// ownedByAdoption reports whether the adoption of (organization, scope) may write a
// new revision over this selection: an adoption wrote its current revision, and that
// adoption is this one. A released selection belongs to the pair it was released
// from. A selection no living adoption lists is the remainder of an adoption that
// stopped between its selection and its record, and the next adoption completes it.
func ownedByAdoption(selection *selectionRecord, organizationID, scope string, all []Adoption) bool {
	if selection.Current.SelectedBy != SelectedByTeamAdoption {
		return false
	}
	if selection.Released != nil {
		return selection.Released.OrganizationID == organizationID && selection.Released.Scope == scope
	}
	claimed := false
	for _, adoption := range all {
		if !adoption.names(selection.ProfileID) {
			continue
		}
		if adoption.OrganizationID == organizationID && adoption.Scope == scope {
			return true
		}
		claimed = true
	}
	return !claimed
}

// PlanTeam decides, without writing anything, what adopting these documents for one
// (organization, scope) would do. Every document must parse; two documents may not
// carry one profile id.
func (owner *Owner) PlanTeam(organizationID, scope string, documents []TeamDocument) (TeamPlan, error) {
	if organizationID == "" || !validAdoptionScope(scope) {
		return TeamPlan{}, invalid("scope", "Name the organization and a scope of organization or repository:<id>.")
	}
	plan, _, err := owner.planTeam(organizationID, scope, documents)
	return plan, err
}

// teamStep is one planned document with what the adoption would write for it.
type teamStep struct {
	document  Document
	selection *selectionRecord
}

func (owner *Owner) planTeam(organizationID, scope string, documents []TeamDocument) (TeamPlan, []teamStep, error) {
	current, err := owner.readAdoption(organizationID, scope)
	if err != nil {
		return TeamPlan{}, nil, integrityConflict(err)
	}
	all, err := owner.Adoptions()
	if err != nil {
		return TeamPlan{}, nil, err
	}
	plan := TeamPlan{OrganizationID: organizationID, Scope: scope, Documents: []TeamDocumentPlan{}, Current: current}
	token := []string{adoptionAbsentToken(organizationID, scope)}
	if current != nil {
		token[0] = current.StateToken
	}
	steps := make([]teamStep, 0, len(documents))
	seen := map[string]string{}
	for _, input := range documents {
		document, parseErr := Parse("PROFILE.md", input.Source)
		if parseErr != nil {
			return TeamPlan{}, nil, parseErr
		}
		if first, duplicate := seen[document.Profile.ID]; duplicate {
			return TeamPlan{}, nil, problem("invalid_profile", "id",
				"Two documents ("+first+" and "+input.Name+") carry the agent id "+document.Profile.ID+".",
				"A bundle carries each agent once.")
		}
		seen[document.Profile.ID] = input.Name
		selection, readErr := owner.readSelection(document.Profile.ID)
		if readErr != nil {
			return TeamPlan{}, nil, integrityConflict(readErr)
		}
		draft, draftErr := owner.readDraft(document.Profile.ID)
		if draftErr != nil {
			return TeamPlan{}, nil, integrityConflict(draftErr)
		}
		item := TeamDocumentPlan{Name: input.Name, ProfileID: document.Profile.ID, ProfileName: document.Profile.Name,
			Version: document.Profile.Version, SourceDigest: document.SourceDigest, BundleDigest: document.BundleDigest,
			Source: string(document.Source)}
		item.Adopted = current != nil && current.Lists(item.ProfileID, item.SourceDigest, item.BundleDigest)
		switch {
		case selection == nil && draft != nil:
			// An agent the member is still drafting uses the id.
			item.Outcome = TeamDocumentCollision
			token = append(token, document.Profile.ID, "draft", draft.StateToken)
		case selection == nil:
			item.Outcome = TeamDocumentNew
			token = append(token, document.Profile.ID, absentStateToken(document.Profile.ID))
		case selection.Current.SourceDigest == document.SourceDigest && selection.Current.BundleDigest == document.BundleDigest:
			item.Outcome = TeamDocumentUnchanged
			token = append(token, document.Profile.ID, selection.StateToken)
		case ownedByAdoption(selection, organizationID, scope, all):
			item.Outcome = TeamDocumentUpdate
			if previous, inspectErr := owner.inspectDocument(selection.Current); inspectErr == nil {
				item.PreviousSource = string(previous.document.Source)
			}
			token = append(token, document.Profile.ID, selection.StateToken)
		default:
			item.Outcome = TeamDocumentCollision
			token = append(token, document.Profile.ID, selection.StateToken)
		}
		plan.Documents = append(plan.Documents, item)
		steps = append(steps, teamStep{document: document, selection: selection})
	}
	plan.StateToken = framedDigest(teamPlanTokenFrame, []byte(strings.Join(token, "\x00")))
	return plan, steps, nil
}

// AdoptTeam adopts a verified bundle's profile documents: bytes, then selections,
// then the adoption record, each step idempotent by digest, so an adoption that
// stopped part-way is completed by adopting again. A stale state token refuses the
// adoption before anything is written. A colliding document is left out and named in
// the result; the rest adopts.
func (owner *Owner) AdoptTeam(command AdoptTeamCommand) (AdoptTeamResult, error) {
	if command.OrganizationID == "" || !validAdoptionScope(command.Scope) || command.BundleID == "" {
		return AdoptTeamResult{}, invalid("scope", "Name the organization, the scope and the bundle.")
	}
	if command.ExpectedStateToken == "" {
		return AdoptTeamResult{}, problem("state_conflict", "state_token", "The adoption state token is missing.",
			"Read the offer again before adopting.")
	}
	var result AdoptTeamResult
	err := owner.withMutationLock(func() error {
		plan, steps, err := owner.planTeam(command.OrganizationID, command.Scope, command.Documents)
		if err != nil {
			return err
		}
		if plan.StateToken != command.ExpectedStateToken {
			return problem("state_conflict", "state_token", "An agent or this adoption changed after the offer was shown.",
				"Read the offer again before adopting; nothing was changed.")
		}
		writes, err := owner.teamSelections(plan, steps, command.Pins)
		if err != nil {
			return err
		}
		for index, step := range steps {
			if plan.Documents[index].Outcome == TeamDocumentCollision {
				continue
			}
			if err := owner.installDocument(step.document); err != nil {
				return err
			}
		}
		for _, record := range writes {
			if err := owner.writeSelection(record); err != nil {
				return err
			}
		}
		adoption, err := owner.recordAdoption(command, plan)
		if err != nil {
			return err
		}
		result = AdoptTeamResult{Adoption: adoption, Documents: plan.Documents}
		return nil
	})
	return result, err
}

// teamSelections builds every selection record the adoption writes — new agents,
// updated agents, and the release of agents the new revision no longer carries —
// before anything is written, so a refusal changes nothing.
func (owner *Owner) teamSelections(plan TeamPlan, steps []teamStep, pins func(string) PinSource) ([]selectionRecord, error) {
	writes := []selectionRecord{}
	carried := map[string]bool{}
	for index, step := range steps {
		item := plan.Documents[index]
		carried[item.ProfileID] = true
		if item.Outcome == TeamDocumentUnchanged {
			if retaken, changed, err := retakenSelection(step.selection, plan.OrganizationID, plan.Scope); err != nil {
				return nil, err
			} else if changed {
				writes = append(writes, retaken)
			}
			continue
		}
		if item.Outcome != TeamDocumentNew && item.Outcome != TeamDocumentUpdate {
			continue
		}
		var keep PinSource
		if pins != nil {
			keep = pins(item.ProfileID)
		}
		next, err := owner.nextSelection(step.selection, step.document, "PROFILE.md", SelectedByTeamAdoption, keep)
		if err != nil {
			return nil, err
		}
		writes = append(writes, next)
	}
	if plan.Current == nil {
		return writes, nil
	}
	for _, listed := range plan.Current.Documents {
		if carried[listed.ProfileID] {
			continue
		}
		released, changed, err := owner.releasedSelection(listed, plan.OrganizationID, plan.Scope)
		if err != nil {
			return nil, err
		}
		if changed {
			writes = append(writes, released)
		}
	}
	return writes, nil
}

// retakenSelection returns a selection this (organization, scope) released, with the
// mark removed, when the adoption carries the very revision it released: the agent is
// shared again and no new revision is needed. Any other selection is left as it is.
func retakenSelection(selection *selectionRecord, organizationID, scope string) (selectionRecord, bool, error) {
	if selection == nil || selection.Released == nil || selection.Released.OrganizationID != organizationID ||
		selection.Released.Scope != scope {
		return selectionRecord{}, false, nil
	}
	retaken := *selection
	retaken.Released = nil
	token, err := selectionStateToken(retaken)
	if err != nil {
		return selectionRecord{}, false, err
	}
	retaken.StateToken = token
	return retaken, true, nil
}

// releasedSelection returns the selection of one listed profile marked released,
// when an adoption wrote its current revision and it is not released already. A
// selection the adoption did not write (the lead's own agent) is never touched, and
// neither is one another adoption still lists: two scopes can carry the same revision
// of one agent, and it stays shared until the last of them lets it go.
func (owner *Owner) releasedSelection(listed AdoptionDocument, organizationID, scope string) (selectionRecord, bool, error) {
	selection, err := owner.readSelection(listed.ProfileID)
	if err != nil {
		return selectionRecord{}, false, integrityConflict(err)
	}
	if selection == nil || selection.Current.SelectedBy != SelectedByTeamAdoption || selection.Released != nil ||
		selection.Current.SourceDigest != listed.SourceDigest || selection.Current.BundleDigest != listed.BundleDigest {
		return selectionRecord{}, false, nil
	}
	if elsewhere, err := owner.listedByAnotherAdoption(listed, organizationID, scope); err != nil || elsewhere {
		return selectionRecord{}, false, err
	}
	released := *selection
	released.Released = &SelectionRelease{OrganizationID: organizationID, Scope: scope,
		ReleasedAt: owner.now().UTC().Format(time.RFC3339Nano)}
	token, err := selectionStateToken(released)
	if err != nil {
		return selectionRecord{}, false, err
	}
	released.StateToken = token
	return released, true, nil
}

// listedByAnotherAdoption reports whether an adoption other than (organizationID,
// scope) lists this very revision of the profile.
func (owner *Owner) listedByAnotherAdoption(listed AdoptionDocument, organizationID, scope string) (bool, error) {
	adoptions, err := owner.Adoptions()
	if err != nil {
		return false, err
	}
	for _, other := range adoptions {
		if other.OrganizationID == organizationID && other.Scope == scope {
			continue
		}
		for _, document := range other.Documents {
			if document.ProfileID == listed.ProfileID && document.SourceDigest == listed.SourceDigest &&
				document.BundleDigest == listed.BundleDigest {
				return true, nil
			}
		}
	}
	return false, nil
}

func (owner *Owner) recordAdoption(command AdoptTeamCommand, plan TeamPlan) (Adoption, error) {
	adoption := Adoption{FormatVersion: adoptionFormat, OrganizationID: command.OrganizationID, Scope: command.Scope,
		BundleID: command.BundleID, AdoptedAt: owner.now().UTC().Format(time.RFC3339Nano), Documents: []AdoptionDocument{}}
	for _, item := range plan.Documents {
		if item.Outcome == TeamDocumentCollision {
			continue
		}
		adoption.Documents = append(adoption.Documents, AdoptionDocument{ProfileID: item.ProfileID, Name: item.ProfileName,
			SourceDigest: item.SourceDigest, BundleDigest: item.BundleDigest})
	}
	token, err := adoptionStateToken(adoption)
	if err != nil {
		return Adoption{}, err
	}
	adoption.StateToken = token
	if err := owner.writeAdoption(adoption); err != nil {
		return Adoption{}, err
	}
	return adoption, nil
}

// RefreshAdoption records that a newer revision with the same signed content is now
// the adopted bundle: only the bundle id changes. The caller compared the signed
// digests; this owner only writes.
func (owner *Owner) RefreshAdoption(organizationID, scope, bundleID string) (Adoption, error) {
	if bundleID == "" {
		return Adoption{}, invalid("bundle_id", "Name the bundle.")
	}
	var refreshed Adoption
	err := owner.withMutationLock(func() error {
		current, err := owner.readAdoption(organizationID, scope)
		if err != nil {
			return integrityConflict(err)
		}
		if current == nil {
			return problem("not_found", "adoption", "Nothing is adopted for that organization and scope.", "Adopt the bundle first.")
		}
		next := *current
		next.BundleID = bundleID
		token, err := adoptionStateToken(next)
		if err != nil {
			return err
		}
		next.StateToken = token
		if next.StateToken != current.StateToken {
			if err := owner.writeAdoption(next); err != nil {
				return err
			}
		}
		refreshed = next
		return nil
	})
	return refreshed, err
}

// Unadopt removes the adoption record of one (organization, scope). Each selection
// the adoption wrote is first marked released — the owner has no removal — so the
// agent stays listed as no longer shared and a later adoption of the same scope takes
// it up again. A selection the adoption did not write is left byte-for-byte as it
// was, and then the record is the only file removed. found is false when nothing was
// adopted.
func (owner *Owner) Unadopt(organizationID, scope string) (removed Adoption, found bool, err error) {
	if organizationID == "" || !validAdoptionScope(scope) {
		return Adoption{}, false, invalid("scope", "Name the organization and a scope of organization or repository:<id>.")
	}
	if rootErr := owner.checkOwnerRoot(); errors.Is(rootErr, fs.ErrNotExist) {
		return Adoption{}, false, nil
	}
	err = owner.withMutationLock(func() error {
		current, readErr := owner.readAdoption(organizationID, scope)
		if readErr != nil {
			return integrityConflict(readErr)
		}
		if current == nil {
			return nil
		}
		for _, listed := range current.Documents {
			released, changed, releaseErr := owner.releasedSelection(listed, organizationID, scope)
			if releaseErr != nil {
				return releaseErr
			}
			if changed {
				if writeErr := owner.writeSelection(released); writeErr != nil {
					return writeErr
				}
			}
		}
		path := owner.adoptionPath(organizationID, scope)
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
			return removeErr
		}
		if syncErr := syncDirectory(filepath.Dir(path)); syncErr != nil {
			return syncErr
		}
		removed, found = *current, true
		return nil
	})
	return removed, found, err
}

// Provenance says where one agent's current selection came from.
type Provenance struct {
	// SelectedBy is who wrote the current revision.
	SelectedBy string
	// Adoption is the record that lists the agent, when one does.
	Adoption *Adoption
	// Released is set when an adoption wrote the selection and let it go.
	Released *SelectionRelease
}

// Provenance reads where profileID's current selection came from. found is false when
// no agent uses the id.
func (owner *Owner) Provenance(profileID string) (Provenance, bool, error) {
	if !profileIDPattern.MatchString(profileID) {
		return Provenance{}, false, invalid("id", "Use a valid lowercase profile ID.")
	}
	selection, err := owner.readSelection(profileID)
	if err != nil {
		return Provenance{}, false, integrityConflict(err)
	}
	if selection == nil {
		return Provenance{}, false, nil
	}
	out := Provenance{SelectedBy: selection.Current.SelectedBy, Released: selection.Released}
	if out.SelectedBy != SelectedByTeamAdoption || out.Released != nil {
		return out, true, nil
	}
	adoption, err := owner.adoptionNaming(profileID)
	if err != nil {
		return Provenance{}, false, err
	}
	out.Adoption = adoption
	return out, true, nil
}

func (owner *Owner) adoptionNaming(profileID string) (*Adoption, error) {
	all, err := owner.Adoptions()
	if err != nil {
		return nil, err
	}
	for index := range all {
		if all[index].names(profileID) {
			return &all[index], nil
		}
	}
	return nil, nil
}

// AdoptionOfRevision returns the adoption a place on this exact revision belongs to:
// the revision is stored for the agent, an adoption wrote it, the selection is not
// released, and an adoption record names the agent. It answers false for the lead's
// own agent and for a member's imported or duplicated agent.
func (owner *Owner) AdoptionOfRevision(profileID, sourceDigest, bundleDigest string) (Adoption, bool, error) {
	if !profileIDPattern.MatchString(profileID) || !validDigest(sourceDigest) || !validDigest(bundleDigest) {
		return Adoption{}, false, nil
	}
	selection, err := owner.readSelection(profileID)
	if err != nil {
		return Adoption{}, false, integrityConflict(err)
	}
	if selection == nil || selection.Released != nil {
		return Adoption{}, false, nil
	}
	for _, revision := range append([]Revision{selection.Current}, selection.History...) {
		if revision.SourceDigest != sourceDigest || revision.BundleDigest != bundleDigest {
			continue
		}
		if revision.SelectedBy != SelectedByTeamAdoption {
			return Adoption{}, false, nil
		}
		adoption, err := owner.adoptionNaming(profileID)
		if err != nil || adoption == nil {
			return Adoption{}, false, err
		}
		return *adoption, true, nil
	}
	return Adoption{}, false, nil
}

func marshalCanonical(value any) ([]byte, error) { return json.Marshal(value) }

func marshalIndented(value any) ([]byte, error) {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}
