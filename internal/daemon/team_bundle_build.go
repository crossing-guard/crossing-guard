package daemon

// Building a bundle to publish (team rest-of-release plan §4.3). `bundle build` and
// the Agents page's Share with team… both ask this route. It runs on a LINKED device:
// it reads the scope's currently published bundle (pulled and verified), the named
// agents' current PROFILE.md sources and, with rules, the device's selected rulebook,
// and writes an UNSIGNED bundle file with the next revision under the data
// directory's bundles/ folder — never a checkout, never a Downloads folder. Signing
// is a separate plain command with a key no daemon holds.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"crossing-guard/engine"
	"crossing-guard/internal/repofile"
	"crossing-guard/internal/rulebook"
	"crossing-guard/internal/teamlink"
	"crossing-guard/profiledoc"
	"crossing-guard/ruledoc"
	"crossing-guard/teamwire"
)

// bundlesDirectory is the product-owned folder a built bundle is written to.
const bundlesDirectory = "bundles"

// The build's own document name for the device's rulebook.
const builtRulebookName = "rulebook.json"

// Document changes a build reports against the published revision.
const (
	builtDocumentAdded     = "added"
	builtDocumentRemoved   = "removed"
	builtDocumentChanged   = "changed"
	builtDocumentUnchanged = "unchanged"
)

// Content policy values `bundle build` takes.
var contentPolicyValues = []string{"off", "consent", contentPolicyMandated}

// teamBundleBuildRequest is POST /api/team/bundles/build's body.
type teamBundleBuildRequest struct {
	// Scope is "organization", "repository:<id>", or "repository" with Cwd inside a
	// checkout of that repository.
	Scope string `json:"scope"`
	Cwd   string `json:"cwd,omitempty"`
	// Agents are profile ids to add, or to replace with the device's current
	// version; RemoveAgents are profile ids to leave out.
	Agents       []string `json:"agents,omitempty"`
	RemoveAgents []string `json:"remove_agents,omitempty"`
	// Rules replaces the bundle's rulebook with the device's selected rulebook.
	Rules         bool   `json:"rules,omitempty"`
	FailureMode   string `json:"failure_mode,omitempty"`
	ContentPolicy string `json:"content_policy,omitempty"`
	ExpiresIn     string `json:"expires_in,omitempty"`
}

// teamBuiltDocument is one document of the built bundle, or one the build left out,
// with what changed against the published revision.
type teamBuiltDocument struct {
	Kind      string         `json:"kind"`
	Name      string         `json:"name"`
	Digest    string         `json:"digest"`
	Change    string         `json:"change"`
	ProfileID string         `json:"profile_id,omitempty"`
	TextDiff  []textDiffLine `json:"text_diff,omitempty"`
}

// teamBundleBuildResponse is the route's answer and everything the Share sheet shows:
// where the unsigned file is, the sign command that already contains that path, where
// the signed file will be, and the diff against the published revision.
type teamBundleBuildResponse struct {
	Path              string              `json:"path"`
	SignedPath        string              `json:"signed_path"`
	SignCommand       string              `json:"sign_command"`
	BundleID          string              `json:"bundle_id"`
	OrganizationID    string              `json:"organization_id"`
	Scope             string              `json:"scope"`
	Revision          int64               `json:"revision"`
	PublishedRevision int64               `json:"published_revision"`
	SchemaVersion     string              `json:"schema_version"`
	FailureMode       string              `json:"failure_mode"`
	ContentPolicy     json.RawMessage     `json:"content_policy,omitempty"`
	ExpiresAt         string              `json:"expires_at"`
	Documents         []teamBuiltDocument `json:"documents"`
	RuleChanges       *teamRuleChanges    `json:"rule_changes,omitempty"`
	Changes           []teamOfferChange   `json:"changes"`
}

// keyFilePlaceholder stands in the sign command for the one path the product never
// knows: where the lead keeps the organization key.
const keyFilePlaceholder = "<your organization key file>"

func handleTeamBundleBuild(w http.ResponseWriter, r *http.Request) {
	if team == nil {
		writeGovernorUnavailable(w)
		return
	}
	var req teamBundleBuildRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.Scope == "" {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, teamErrorResponse{Error: "body must be {\"scope\": \"organization|repository|repository:<id>\", \"cwd\", \"agents\", \"remove_agents\", \"rules\", \"failure_mode\", \"content_policy\", \"expires_in\"}"})
		return
	}
	out, err := team.buildBundle(req)
	if err != nil {
		writeAdoptionRefusal(w, err)
		return
	}
	writeJSON(w, out)
}

// buildBundle builds the next revision of one scope's bundle and writes it unsigned.
func (t *teamLinker) buildBundle(req teamBundleBuildRequest) (teamBundleBuildResponse, error) {
	t.mu.Lock()
	linked := t.state == teamLinked
	organization := t.doc.Organization
	defaults := t.doc.Bundle
	t.mu.Unlock()
	if !linked || organization.ID == "" {
		return teamBundleBuildResponse{}, adoptionRefusal(http.StatusConflict, "the device is not linked; a bundle is built on a linked device")
	}
	scope, err := t.buildScope(req)
	if err != nil {
		return teamBundleBuildResponse{}, err
	}
	// The published revision is read fresh: a stale view would build a revision the
	// server refuses as not newer.
	if err := t.pullLayers(); err != nil {
		return teamBundleBuildResponse{}, adoptionRefusal(http.StatusBadGateway, "the published bundle could not be read from the team server: %v", err)
	}
	published, err := t.publishedBundle(organization.ID, scope)
	if err != nil {
		return teamBundleBuildResponse{}, err
	}
	bundle, err := t.nextBundle(req, organization, scope, published, defaults)
	if err != nil {
		return teamBundleBuildResponse{}, err
	}
	if err := checkBuiltBundle(bundle); err != nil {
		return teamBundleBuildResponse{}, err
	}
	path, err := writeUnsignedBundle(t.storeDir, scope, bundle)
	if err != nil {
		return teamBundleBuildResponse{}, adoptionRefusal(http.StatusInternalServerError, "the bundle file could not be written: %v", err)
	}
	out := teamBundleBuildResponse{Path: path, SignedPath: teamwire.SignedBundlePath(path),
		SignCommand: "crossing-guard bundle sign --key " + shellQuote(keyFilePlaceholder) + " " + shellQuote(path),
		BundleID:    bundle.ID, OrganizationID: bundle.OrganizationID, Scope: scope, Revision: bundle.Revision,
		SchemaVersion: bundle.SchemaVersion, FailureMode: bundle.FailureMode.StatefulTier,
		ContentPolicy: bundle.ContentPolicy, ExpiresAt: bundle.ExpiresAt, Changes: []teamOfferChange{}}
	if published != nil {
		out.PublishedRevision = published.signed.Revision
	}
	fillBuildDiff(&out, published, bundle)
	return out, nil
}

// buildScope resolves the scope a build is for. A bare "repository" is resolved from
// the checkout the caller stands in, through the repository identity the store
// already holds for a governed session there.
func (t *teamLinker) buildScope(req teamBundleBuildRequest) (string, error) {
	switch {
	case req.Scope == rulebook.ScopeOrganization:
		return req.Scope, nil
	case strings.HasPrefix(req.Scope, rulebook.ScopeRepositoryPrefix) && len(req.Scope) > len(rulebook.ScopeRepositoryPrefix):
		return req.Scope, nil
	case req.Scope != bundleScopeRepository:
		return "", adoptionRefusal(http.StatusBadRequest, "scope must be organization, repository, or repository:<id>")
	}
	root, ok := repofile.Locate(req.Cwd)
	if req.Cwd == "" || !ok {
		return "", adoptionRefusal(http.StatusBadRequest, "a repository bundle is built inside a checkout of that repository; %q is not inside one", req.Cwd)
	}
	sessions, err := governor.ix.EnvelopeSessionIDs(t.repositoryResolveLimit())
	if err != nil {
		return "", adoptionRefusal(http.StatusInternalServerError, "the repository could not be identified: %v", err)
	}
	for _, sessionID := range sessions {
		id, sessionRoot, found, err := governor.ix.SessionRepository(sessionID)
		if err == nil && found && id != "" && sessionRoot == root {
			return rulebook.ScopeRepositoryPrefix + id, nil
		}
	}
	return "", adoptionRefusal(http.StatusConflict, "this device has not identified the repository at %s yet; run a governed session in it, or name the scope as repository:<id>", root)
}

// publishedBundle returns the scope's currently published bundle as the pull verified
// it, or nil when the scope has none yet. A published revision this device cannot
// use, or one hidden behind a key this device has not trusted, stops the build: the
// next revision would be built on something the device did not read.
func (t *teamLinker) publishedBundle(organizationID, scope string) (*verifiedBundle, error) {
	t.mu.Lock()
	entries := append([]teamLayerEntry(nil), t.available...)
	unusable := append([]teamUnusableBundle(nil), t.unusable...)
	mismatches := len(t.keyMismatches)
	t.mu.Unlock()
	if mismatches > 0 {
		return nil, adoptionRefusal(http.StatusConflict, "the server presents a bundle signed by a key this device has not trusted; trust the new key first, so the published revision can be read")
	}
	for _, bundle := range unusable {
		if bundle.OrganizationID == organizationID && bundle.Scope == scope {
			return nil, adoptionRefusal(http.StatusConflict, "the published revision %d of %s can't be used on this device (%s); the next revision cannot be built on it here", bundle.Revision, scope, bundle.Reason)
		}
	}
	var latest *verifiedBundle
	for _, entry := range entries {
		if entry.OrganizationID != organizationID || entry.Scope != scope {
			continue
		}
		var signed teamwire.SignedBundle
		if err := json.Unmarshal(entry.RawSigned, &signed); err != nil {
			return nil, adoptionRefusal(http.StatusInternalServerError, "the published bundle's bytes are no longer decodable; pull again")
		}
		verified, err := decodeVerified(entry.RawSigned, signed)
		if err != nil {
			return nil, adoptionRefusal(http.StatusInternalServerError, "the published bundle's bytes are no longer decodable; pull again")
		}
		if latest == nil || verified.signed.Revision > latest.signed.Revision {
			latest = &verified
		}
	}
	return latest, nil
}

// nextBundle assembles the next revision: the published documents carried forward,
// agents removed and added, the rulebook replaced when asked, and failure mode and
// content policy carried forward unless the request changes them. A scope's first
// bundle takes its failure mode and expiry from team.json's bundle defaults.
func (t *teamLinker) nextBundle(req teamBundleBuildRequest, organization teamlink.Organization, scope string, published *verifiedBundle, defaults teamlink.BundleDefaults) (teamwire.SignedBundle, error) {
	now := t.now().UTC()
	bundle := teamwire.SignedBundle{SchemaVersion: teamwire.BundleSchemaVersion, ID: engine.NewTypedID(teamwire.BundleIDPrefix),
		OrganizationID: organization.ID, Revision: 1, CreatedAt: now.Format(time.RFC3339),
		FailureMode: teamwire.BundleFailureMode{StatefulTier: defaults.DefaultFailureMode},
		Scope:       teamwire.BundleScope{Type: bundleScopeOrganization, ID: organization.ID}}
	if id, isRepository := strings.CutPrefix(scope, rulebook.ScopeRepositoryPrefix); isRepository {
		bundle.Scope = teamwire.BundleScope{Type: bundleScopeRepository, ID: id}
	}
	if published != nil {
		bundle.Revision = published.signed.Revision + 1
		bundle.FailureMode = published.signed.FailureMode
		bundle.ContentPolicy = published.signed.ContentPolicy
		bundle.Documents = append(bundle.Documents, published.signed.Documents...)
	}
	if req.FailureMode != "" {
		if req.FailureMode != teamlink.FailOpen && req.FailureMode != teamlink.FailClosed {
			return teamwire.SignedBundle{}, adoptionRefusal(http.StatusBadRequest, "failure_mode must be %s or %s", teamlink.FailOpen, teamlink.FailClosed)
		}
		bundle.FailureMode.StatefulTier = req.FailureMode
	}
	policy, err := nextContentPolicy(req.ContentPolicy, bundle.ContentPolicy)
	if err != nil {
		return teamwire.SignedBundle{}, err
	}
	bundle.ContentPolicy = policy
	expiresIn := defaults.DefaultExpiry.Duration
	if req.ExpiresIn != "" {
		if expiresIn, err = time.ParseDuration(req.ExpiresIn); err != nil || expiresIn <= 0 {
			return teamwire.SignedBundle{}, adoptionRefusal(http.StatusBadRequest, "expires_in must be a positive duration such as 720h")
		}
	}
	bundle.ExpiresAt = now.Add(expiresIn).Format(time.RFC3339)
	if bundle.Documents, err = t.nextDocuments(req, bundle.Documents); err != nil {
		return teamwire.SignedBundle{}, err
	}
	return bundle, nil
}

// nextContentPolicy returns the content policy the next revision carries: the
// published one, with sync_content replaced when the request names a value. The
// repository list, when the published policy has one, is carried forward.
func nextContentPolicy(requested string, published json.RawMessage) (json.RawMessage, error) {
	if requested == "" {
		return published, nil
	}
	known := false
	for _, value := range contentPolicyValues {
		known = known || value == requested
	}
	if !known {
		return nil, adoptionRefusal(http.StatusBadRequest, "content_policy must be one of %s", strings.Join(contentPolicyValues, ", "))
	}
	policy := map[string]json.RawMessage{}
	if len(published) > 0 {
		if err := json.Unmarshal(published, &policy); err != nil {
			return nil, adoptionRefusal(http.StatusConflict, "the published content policy could not be read: %v", err)
		}
	}
	value, err := json.Marshal(requested)
	if err != nil {
		return nil, err
	}
	policy["sync_content"] = value
	return json.Marshal(policy)
}

// nextDocuments applies the request to the carried-forward documents.
func (t *teamLinker) nextDocuments(req teamBundleBuildRequest, documents []teamwire.SignedBundleDoc) ([]teamwire.SignedBundleDoc, error) {
	adding := map[string]bool{}
	for _, id := range req.Agents {
		adding[id] = true
	}
	for _, id := range req.RemoveAgents {
		if adding[id] {
			return nil, adoptionRefusal(http.StatusBadRequest, "agent %s is both added and removed", id)
		}
		index := profileDocumentIndex(documents, id)
		if index < 0 {
			return nil, adoptionRefusal(http.StatusConflict, "the published bundle carries no agent with the id %s to remove", id)
		}
		documents = append(documents[:index:index], documents[index+1:]...)
	}
	if len(req.Agents) > 0 {
		owner, err := t.profileOwner()
		if err != nil {
			return nil, adoptionRefusal(http.StatusInternalServerError, "the agent store is unavailable: %v", err)
		}
		for _, id := range req.Agents {
			detail, err := owner.Get(id)
			if err != nil || detail.Source == "" {
				return nil, adoptionRefusal(http.StatusConflict, "agent %s has no published version on this device to share", id)
			}
			document := teamwire.SignedBundleDoc{Kind: teamwire.BundleKindProfile, Name: id + "/" + profileSourceName,
				Digest: teamwire.BundleDocumentDigest([]byte(detail.Source)), MediaType: teamwire.BundleMediaTypeProfile, Body: detail.Source}
			if index := profileDocumentIndex(documents, id); index >= 0 {
				document.Name = documents[index].Name
				documents[index] = document
			} else {
				documents = append(documents, document)
			}
		}
	}
	if !req.Rules {
		return documents, nil
	}
	selected, err := rulebook.LoadDocument()
	if err != nil || len(selected.Raw) == 0 {
		return nil, adoptionRefusal(http.StatusConflict, "this device has no rulebook to share")
	}
	document := teamwire.SignedBundleDoc{Kind: teamwire.BundleKindRulebook, Name: builtRulebookName,
		Digest: teamwire.BundleDocumentDigest(selected.Raw), MediaType: teamwire.BundleMediaTypeRulebook, Body: string(selected.Raw)}
	for index := range documents {
		if documents[index].Kind == teamwire.BundleKindRulebook {
			document.Name = documents[index].Name
			documents[index] = document
			return documents, nil
		}
	}
	return append(documents, document), nil
}

// profileDocumentIndex finds the profile document that carries one agent id.
func profileDocumentIndex(documents []teamwire.SignedBundleDoc, profileID string) int {
	for index, document := range documents {
		if document.Kind != teamwire.BundleKindProfile {
			continue
		}
		if parsed, err := profiledoc.Parse(profileSourceName, []byte(document.Body)); err == nil && parsed.Profile.ID == profileID {
			return index
		}
	}
	return -1
}

// checkBuiltBundle validates a built bundle with the caps and the shared parsers — the
// checks the server runs at upload and a device runs before an offer — so an
// authoring error stops here.
func checkBuiltBundle(bundle teamwire.SignedBundle) error {
	if len(bundle.Documents) == 0 {
		return adoptionRefusal(http.StatusConflict, "the bundle would carry no document; add an agent or the rules")
	}
	built := verifiedBundle{signed: bundle}
	if code, index := teamwire.CheckBundleCaps(bundle); code != "" {
		return adoptionRefusal(http.StatusConflict, "%s: %s", code, capReason(code, index, built))
	}
	if index, why := firstUnusableDocument(built); index >= 0 {
		return adoptionRefusal(http.StatusConflict, "%s: %s", teamwire.CodeBundleDocumentInvalid, why)
	}
	seen := map[string]string{}
	for _, document := range bundle.Documents {
		if document.Kind != teamwire.BundleKindProfile {
			continue
		}
		parsed, _ := profiledoc.Parse(profileSourceName, []byte(document.Body))
		if first, duplicate := seen[parsed.Profile.ID]; duplicate {
			return adoptionRefusal(http.StatusConflict, "%s: documents %s and %s carry the agent id %s", teamwire.CodeBundleDocumentInvalid, first, document.Name, parsed.Profile.ID)
		}
		seen[parsed.Profile.ID] = document.Name
	}
	return nil
}

// bundleFileSlug spells a scope as a file name part.
func bundleFileSlug(scope string) string {
	var slug strings.Builder
	for _, char := range scope {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9', char == '.', char == '_', char == '-':
			slug.WriteRune(char)
		default:
			slug.WriteRune('-')
		}
	}
	return slug.String()
}

// writeUnsignedBundle writes the built bundle to <data directory>/bundles/
// <scope>-r<revision>.json, private to the user, replacing an earlier build of the
// same revision. The file carries no signature.
func writeUnsignedBundle(storeDir, scope string, bundle teamwire.SignedBundle) (string, error) {
	dir := filepath.Join(storeDir, bundlesDirectory)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	bundle.Signature = nil
	raw, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-r%d.json", bundleFileSlug(scope), bundle.Revision))
	temporary, err := os.CreateTemp(dir, ".bundle-*")
	if err != nil {
		return "", err
	}
	name := temporary.Name()
	defer func() { _ = os.Remove(name) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return "", err
	}
	if _, err := temporary.Write(append(raw, '\n')); err != nil {
		_ = temporary.Close()
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	return path, os.Rename(name, path)
}

// shellQuote quotes one argument for a POSIX shell.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// fillBuildDiff writes the diff of the built bundle against the published revision:
// each document's change, each changed profile's text diff, the rule changes, and
// the changed signed fields.
func fillBuildDiff(out *teamBundleBuildResponse, published *verifiedBundle, bundle teamwire.SignedBundle) {
	before := []teamwire.SignedBundleDoc{}
	if published != nil {
		before = published.signed.Documents
		record := rulebook.AdoptedBundle{FailureMode: published.signed.FailureMode.StatefulTier,
			ContentPolicy: published.signed.ContentPolicy, SchemaVersion: published.signed.SchemaVersion}
		out.Changes = signedFieldChanges(record, verifiedBundle{signed: bundle})
	}
	matched := map[int]bool{}
	for _, document := range bundle.Documents {
		item := teamBuiltDocument{Kind: document.Kind, Name: document.Name, Digest: document.Digest, Change: builtDocumentAdded}
		item.ProfileID = builtProfileID(document)
		if index := matchingDocument(before, document, item.ProfileID); index >= 0 {
			matched[index] = true
			item.Change = builtDocumentUnchanged
			if before[index].Digest != document.Digest {
				item.Change = builtDocumentChanged
				if document.Kind == teamwire.BundleKindProfile {
					item.TextDiff = textDiff(before[index].Body, document.Body)
				}
			}
		} else if document.Kind == teamwire.BundleKindProfile {
			item.TextDiff = textDiff("", document.Body)
		}
		out.Documents = append(out.Documents, item)
	}
	for index, document := range before {
		if !matched[index] {
			out.Documents = append(out.Documents, teamBuiltDocument{Kind: document.Kind, Name: document.Name,
				Digest: document.Digest, Change: builtDocumentRemoved, ProfileID: builtProfileID(document)})
		}
	}
	out.RuleChanges = builtRuleChanges(before, bundle.Documents)
}

func builtProfileID(document teamwire.SignedBundleDoc) string {
	if document.Kind != teamwire.BundleKindProfile {
		return ""
	}
	parsed, err := profiledoc.Parse(profileSourceName, []byte(document.Body))
	if err != nil {
		return ""
	}
	return parsed.Profile.ID
}

// matchingDocument finds the published document a built one continues: the same
// agent for a profile, the rulebook for a rulebook, the same name otherwise.
func matchingDocument(published []teamwire.SignedBundleDoc, document teamwire.SignedBundleDoc, profileID string) int {
	for index, candidate := range published {
		if candidate.Kind != document.Kind {
			continue
		}
		switch document.Kind {
		case teamwire.BundleKindProfile:
			if builtProfileID(candidate) == profileID {
				return index
			}
		case teamwire.BundleKindRulebook:
			return index
		default:
			if candidate.Name == document.Name {
				return index
			}
		}
	}
	return -1
}

// builtRuleChanges is ruledoc's mechanical diff between the published rulebook and
// the built one; nil when nothing changed.
func builtRuleChanges(published, built []teamwire.SignedBundleDoc) *teamRuleChanges {
	policy := func(documents []teamwire.SignedBundleDoc) *engine.Policy {
		for _, document := range documents {
			if document.Kind == teamwire.BundleKindRulebook {
				if parsed, err := ruledoc.Parse([]byte(document.Body)); err == nil {
					return parsed
				}
			}
		}
		return &engine.Policy{}
	}
	added, removed, changed := ruledoc.MechanicalDiff(policy(published), policy(built))
	if len(added)+len(removed)+len(changed) == 0 {
		return nil
	}
	if changed == nil {
		changed = []ruledoc.RuleChange{}
	}
	return &teamRuleChanges{Added: nonNilStrings(added), Removed: nonNilStrings(removed), Changed: changed}
}
