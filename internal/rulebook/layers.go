// Package rulebook: the layered loader (team plan §5.16.2 d5, item 3c). The user
// layer's ONE owner stays selection_store.go; this file adds the ADOPTED team
// layers and the read-time concatenation they feed.
//
// The tie-break is the concatenation order itself (postwork F4's pin): user, then
// the checkout's repository layer, then the organization layer. Same-rank rules
// resolve to the earlier tier — the user's own document always speaks first, and
// tighten-only holds regardless (the property test in engine).

package rulebook

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"crossing-guard/engine"
	"crossing-guard/internal/filelock"
	"crossing-guard/internal/repofile"
	"crossing-guard/ruledoc"
	"crossing-guard/teamwire"
)

// Layer state files, beside the store's policy root (the link files' home is the
// daemon's store dir; the rulebook's is its own root — both daemon-owned, one
// directory tree).
const (
	layersFile   = "layers.json"
	layersLock   = "layers.lock"
	layersDir    = "layers"
	repoIndexSub = "repository-index.json"
	docDir       = "documents"
)

// LayersFormatVersion is layers.json's current format: version 2 holds one
// adopted-bundle record per (organization, scope). Version 1 held one rule layer per
// scope and is migrated on read (team rest-of-release plan §4.1 decision 5, K-2).
const LayersFormatVersion = 2

// Scope spellings an adopted-bundle record carries.
const (
	ScopeOrganization     = "organization"
	ScopeRepositoryPrefix = "repository:"
)

// Failure modes a bundle carries for its expiry.
const (
	FailOpen   = "fail-open"
	FailClosed = "fail-closed"
)

// AdoptedBundle is the adopted-bundle record: what this device adopted for one
// (organization, scope), written at every adoption whether or not the bundle carries
// a rulebook. Every field was decoded from the bytes the signature verified. Expiry,
// failure mode and content policy are read from this record and nowhere else.
type AdoptedBundle struct {
	OrganizationID string `json:"organization_id"`
	// OrganizationName is the organization's display name at adoption, kept so a
	// reason shown offline or after an unlink can name it without the link document.
	OrganizationName string `json:"organization_name,omitempty"`
	Scope            string `json:"scope"` // "organization" | "repository:<id>"
	BundleID         string `json:"bundle_id"`
	Revision         int64  `json:"revision"`
	KeyID            string `json:"key_id"`
	FailureMode      string `json:"failure_mode"` // fail-open | fail-closed
	// ContentPolicy is the signed content_policy object (re-indented with the file),
	// or empty.
	ContentPolicy json.RawMessage `json:"content_policy,omitempty"`
	ExpiresAt     time.Time       `json:"expires_at"`
	AdoptedAt     time.Time       `json:"adopted_at"`
	// SignedDigest is teamwire.BundleSignedDigest of the adopted document: a pulled
	// bundle refreshes without a prompt only when its digest equals this one.
	SignedDigest string `json:"signed_digest"`
	// RulebookDigest names the staged rule document, or is empty for a bundle with
	// no rulebook.
	RulebookDigest string `json:"rulebook_digest"`
	// SchemaVersion is the adopted document's bundle format, kept so a changed
	// format can be shown as its own labelled field ("bundle format 1.0 → 1.1").
	SchemaVersion string `json:"schema_version,omitempty"`
	// SignedDigestWithoutKey is teamwire.BundleSignedDigestIgnoringKey of the adopted
	// document. It is compared only after a re-pin, to tell a bundle whose one
	// difference is the key id the person already confirmed.
	SignedDigestWithoutKey string `json:"signed_digest_without_key,omitempty"`
}

// LayersDocument is layers.json: the versioned document of what this device adopted
// (§5.16.2 d5) and the organization key it pinned. It imports NOTHING from the
// selection record — the user layer's selection is a different concept that stays
// where it is (RT3-15).
type LayersDocument struct {
	FormatVersion int             `json:"format_version"`
	Adopted       []AdoptedBundle `json:"adopted"`
	// PinnedOrgKey records the organization key this device pinned (§5.16.2 d2):
	// the fingerprint the pin event chained, so the Team console can show it and a
	// changed key raises the mismatch state.
	PinnedOrgKeyID        string `json:"pinned_org_key_id,omitempty"`
	PinnedOrgKeyPublicKey string `json:"pinned_org_key_public_key,omitempty"`
	PinnedOrgKeyFP        string `json:"pinned_org_key_fingerprint,omitempty"`
	PinnedAt              string `json:"pinned_at,omitempty"`
	// ReplacedOrgKeyID is the key id the last re-pin replaced. A bundle that differs
	// from an adopted record only in its key id refreshes without a prompt when the
	// record's key id is this one and the bundle's is the current pin.
	ReplacedOrgKeyID string `json:"replaced_org_key_id,omitempty"`
}

// layerAdoptionV1 and layersDocumentV1 are format version 1, read only to migrate.
type layerAdoptionV1 struct {
	Scope       string    `json:"scope"`
	Digest      string    `json:"digest"`
	AdoptedAt   time.Time `json:"adopted_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	FailureMode string    `json:"failure_mode"`
}

type layersDocumentV1 struct {
	Organization          *layerAdoptionV1            `json:"organization,omitempty"`
	Repositories          map[string]*layerAdoptionV1 `json:"repositories,omitempty"`
	PinnedOrgKeyID        string                      `json:"pinned_org_key_id,omitempty"`
	PinnedOrgKeyPublicKey string                      `json:"pinned_org_key_public_key,omitempty"`
	PinnedOrgKeyFP        string                      `json:"pinned_org_key_fingerprint,omitempty"`
	PinnedAt              string                      `json:"pinned_at,omitempty"`
}

// migrateLayersV1 turns a version 1 document into version 2. A version 1 adoption
// recorded no organization, bundle id, revision, key id or signed digest, so those
// stay empty: its rules keep applying to their expiry exactly as before, and the next
// pulled bundle for that scope asks, because an empty signed digest equals nothing.
func migrateLayersV1(old layersDocumentV1) LayersDocument {
	doc := LayersDocument{FormatVersion: LayersFormatVersion, Adopted: []AdoptedBundle{},
		PinnedOrgKeyID: old.PinnedOrgKeyID, PinnedOrgKeyPublicKey: old.PinnedOrgKeyPublicKey,
		PinnedOrgKeyFP: old.PinnedOrgKeyFP, PinnedAt: old.PinnedAt}
	carry := func(a *layerAdoptionV1) {
		if a == nil {
			return
		}
		doc.Adopted = append(doc.Adopted, AdoptedBundle{Scope: a.Scope, FailureMode: a.FailureMode,
			ExpiresAt: a.ExpiresAt, AdoptedAt: a.AdoptedAt, RulebookDigest: a.Digest})
	}
	ids := make([]string, 0, len(old.Repositories))
	for id := range old.Repositories {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		carry(old.Repositories[id])
	}
	carry(old.Organization)
	return doc
}

// LayersPathsIn returns the layer files for a store directory (the daemon's data
// dir, where team.json already lives — one home for the link's state).
func LayersPathsIn(storeDir string) (doc, lock, dir string) {
	return filepath.Join(storeDir, layersFile), filepath.Join(storeDir, layersLock), filepath.Join(storeDir, layersDir)
}

// LoadLayers reads layers.json; a missing file is the unadopted zero document. A
// version 1 file is migrated in memory; the file itself moves to version 2 at the
// next write. A file from a newer format is refused rather than guessed at.
func LoadLayers(storeDir string) (LayersDocument, error) {
	docPath, _, _ := LayersPathsIn(storeDir)
	raw, err := os.ReadFile(docPath)
	if err != nil {
		if os.IsNotExist(err) {
			return LayersDocument{FormatVersion: LayersFormatVersion, Adopted: []AdoptedBundle{}}, nil
		}
		return LayersDocument{}, err
	}
	return decodeLayers(raw)
}

func decodeLayers(raw []byte) (LayersDocument, error) {
	var version struct {
		FormatVersion int `json:"format_version"`
	}
	if err := json.Unmarshal(raw, &version); err != nil {
		return LayersDocument{}, fmt.Errorf("%s: %w", layersFile, err)
	}
	switch {
	case version.FormatVersion <= 1:
		var old layersDocumentV1
		if err := json.Unmarshal(raw, &old); err != nil {
			return LayersDocument{}, fmt.Errorf("%s: %w", layersFile, err)
		}
		return migrateLayersV1(old), nil
	case version.FormatVersion == LayersFormatVersion:
		var doc LayersDocument
		if err := json.Unmarshal(raw, &doc); err != nil {
			return LayersDocument{}, fmt.Errorf("%s: %w", layersFile, err)
		}
		if doc.Adopted == nil {
			doc.Adopted = []AdoptedBundle{}
		}
		return doc, nil
	}
	return LayersDocument{}, fmt.Errorf("%s: format version %d is newer than this build reads (%d)", layersFile, version.FormatVersion, LayersFormatVersion)
}

// UpdateLayers is the one writer of layers.json: it reads the document, applies
// change, and writes the result, all under the mutation lock, so a pin save and an
// adoption can never lose each other's update. The file is written 0600 — adoption
// records are link state, not published content. A change that returns an error
// writes nothing.
func UpdateLayers(storeDir string, change func(*LayersDocument) error) error {
	docPath, lockPath, _ := LayersPathsIn(storeDir)
	return filelock.With(lockPath, 0o600, func() error {
		doc, err := LoadLayers(storeDir)
		if err != nil {
			return err
		}
		if err := change(&doc); err != nil {
			return err
		}
		doc.FormatVersion = LayersFormatVersion
		if doc.Adopted == nil {
			doc.Adopted = []AdoptedBundle{}
		}
		raw, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return err
		}
		return writeLayersFile(docPath, raw)
	})
}

// Bundle returns the adopted-bundle record for one (organization, scope).
func (doc LayersDocument) Bundle(organizationID, scope string) (AdoptedBundle, bool) {
	for _, record := range doc.Adopted {
		if record.OrganizationID == organizationID && record.Scope == scope {
			return record, true
		}
	}
	return AdoptedBundle{}, false
}

func validLayerScope(scope string) bool {
	return scope == ScopeOrganization ||
		(strings.HasPrefix(scope, ScopeRepositoryPrefix) && len(scope) > len(ScopeRepositoryPrefix))
}

// StageLayer writes one document body into the store dir's digest-addressed
// document area — CAS by the digest itself, so re-staging is idempotent and the
// file's bytes are provably the digest (RT3-11's one-digest-store principle,
// scoped to THIS tree: the selection store's documents/ holds the USER document;
// a team layer's bytes live beside the link's state, under storeDir). The write
// order (§5.16.2 d5): body first, then the layers.json flip — a crash between
// leaves an unreferenced body, never a record pointing at nothing.
func StageLayer(storeDir, digest string, body []byte) (string, error) {
	// The bundle body cap is the wire contract's (OD-7): the hot-path cost of a team
	// layer must never be set by whatever bytes arrive.
	if len(body) > teamwire.BundleMaxBodyBytes {
		return "", fmt.Errorf("document body is %d bytes; the cap is %d", len(body), teamwire.BundleMaxBodyBytes)
	}
	sum := sha256.Sum256(body)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != digest {
		return "", fmt.Errorf("document body does not match its digest %s", digest)
	}
	dir := filepath.Join(storeDir, layersDir, docDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, strings.Replace(digest, ":", "-", 1)+".json")
	if existing, err := os.ReadFile(path); err == nil {
		if string(existing) != string(body) {
			return "", fmt.Errorf("digest collision at %s", path)
		}
		return path, nil
	}
	return path, writeLayersFile(path, body)
}

// AdoptBundle writes the adopted-bundle record for its (organization, scope),
// replacing an earlier one. A rulebook it names must already be staged (StageLayer
// first). A version 1 record for the same scope, which names no organization, is
// replaced too: it is the same adoption, recorded before organizations were.
func AdoptBundle(storeDir string, record AdoptedBundle) error {
	if record.OrganizationID == "" {
		return fmt.Errorf("an adopted bundle names its organization")
	}
	if !validLayerScope(record.Scope) {
		return fmt.Errorf("unknown layer scope %q", record.Scope)
	}
	if record.AdoptedAt.IsZero() {
		record.AdoptedAt = time.Now()
	}
	return UpdateLayers(storeDir, func(doc *LayersDocument) error {
		kept := doc.Adopted[:0:0]
		for _, existing := range doc.Adopted {
			sameScope := existing.Scope == record.Scope
			if sameScope && (existing.OrganizationID == record.OrganizationID || existing.OrganizationID == "") {
				continue
			}
			kept = append(kept, existing)
		}
		doc.Adopted = append(kept, record)
		return nil
	})
}

// UnadoptBundle removes the record for one (organization, scope) and reports
// whether there was one; a missing record is not an error.
func UnadoptBundle(storeDir, organizationID, scope string) (bool, error) {
	if !validLayerScope(scope) {
		return false, fmt.Errorf("unknown layer scope %q", scope)
	}
	removed := false
	err := UpdateLayers(storeDir, func(doc *LayersDocument) error {
		kept := doc.Adopted[:0:0]
		for _, existing := range doc.Adopted {
			if existing.OrganizationID == organizationID && existing.Scope == scope {
				removed = true
				continue
			}
			kept = append(kept, existing)
		}
		doc.Adopted = kept
		return nil
	})
	return removed, err
}

// LayerRefresh is what a refresh without a prompt may change in an adopted-bundle
// record: the bundle id, the revision and the expiry of a new revision whose signed
// digest equals the record's. KeyID and SignedDigest are set only by the one
// exception — a bundle that differs in nothing but the key id a re-pin confirmed —
// and are left alone when empty.
type LayerRefresh struct {
	BundleID     string
	Revision     int64
	ExpiresAt    time.Time
	KeyID        string
	SignedDigest string
	// OrganizationName refreshes the display name when the caller knows it.
	OrganizationName string
}

// ErrNoAdoptedBundle reports a refresh of a (organization, scope) nothing adopted.
var ErrNoAdoptedBundle = errors.New("no adopted bundle for that organization and scope")

// RefreshTeamLayer applies a refresh to the adopted-bundle record of one
// (organization, scope). The caller has verified the bundle and compared digests;
// this only writes. Failure mode, content policy and the rulebook digest never
// change here.
func RefreshTeamLayer(storeDir, organizationID, scope string, refresh LayerRefresh) error {
	return UpdateLayers(storeDir, func(doc *LayersDocument) error {
		for index := range doc.Adopted {
			record := &doc.Adopted[index]
			if record.OrganizationID != organizationID || record.Scope != scope {
				continue
			}
			record.BundleID, record.Revision, record.ExpiresAt = refresh.BundleID, refresh.Revision, refresh.ExpiresAt
			if refresh.KeyID != "" && refresh.SignedDigest != "" {
				record.KeyID, record.SignedDigest = refresh.KeyID, refresh.SignedDigest
			}
			if refresh.OrganizationName != "" {
				record.OrganizationName = refresh.OrganizationName
			}
			return nil
		}
		return ErrNoAdoptedBundle
	})
}

// repoRootHash is the staging key for one checkout root (§5.16.2 d5): the sha256 of
// the root path, so a root with odd characters cannot become a filesystem problem.
func repoRootHash(root string) string {
	sum := sha256.Sum256([]byte(root))
	return hex.EncodeToString(sum[:])[:16]
}

// SetRepositoryResolution records the daemon's root→id resolution (the pointer
// index the hook reads stat-only — R14's bridge). The daemon resolves repository_id
// (it may spawn git); the hook discovers the root with repofile and looks the id up.
func SetRepositoryResolution(storeDir, checkoutRoot, repositoryID string) error {
	idx, err := loadRepositoryIndex(storeDir)
	if err != nil {
		return err
	}
	if idx.Roots == nil {
		idx.Roots = map[string]string{}
	}
	idx.Roots[repoRootHash(checkoutRoot)] = repositoryID
	return saveRepositoryIndex(storeDir, idx)
}

type repositoryIndex struct {
	Roots map[string]string `json:"roots"` // root-hash -> repository_id
}

func repositoryIndexPath(storeDir string) string {
	return filepath.Join(storeDir, layersDir, repoIndexSub)
}

func loadRepositoryIndex(storeDir string) (repositoryIndex, error) {
	var idx repositoryIndex
	raw, err := os.ReadFile(repositoryIndexPath(storeDir))
	if err != nil {
		if os.IsNotExist(err) {
			return repositoryIndex{}, nil
		}
		return idx, err
	}
	err = json.Unmarshal(raw, &idx)
	return idx, err
}

func saveRepositoryIndex(storeDir string, idx repositoryIndex) error {
	if err := os.MkdirAll(filepath.Join(storeDir, layersDir), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	return writeLayersFile(repositoryIndexPath(storeDir), raw)
}

// LoadLayered concatenates the device's effective policy at decide time
// (§5.16.3): the user layer ++ the checkout's repository layer ++ the organization
// layer, each parsed by ruledoc — the ONE evaluator — and each rule stamped with
// its tier. The concatenation order is the tie-break (postwork F4): same-rank
// rules resolve to the earlier tier, the user's own first.
//
// Failure modes, stated (§5.16.2 d6/d9):
//   - the USER layer never depends on team parsing; its load errors are loud, as
//     today;
//   - an adopted layer past expires_at applies its failure_mode: fail-open drops
//     its rules for new decisions, fail-closed denies with a reason naming the
//     expired layer (a deny rule over the command tag, appended LAST so tighten-
//     only holds);
//   - a layer that fails to parse is SKIPPED — the decision's reason names it
//     (adopted-but-unloadable) via the returned notes, never a silent absence.
//
// LayeredPolicy is the loader's full result for a caller that must slice the
// layers apart: the USER prefix's count rides with the policy (postwork M-1 —
// counting via a second Load raced the file between loads and mis-windowed).
type LayeredPolicy struct {
	Policy        *engine.Policy
	Reasons       []string
	UserRuleCount int    // the user layer occupies Rules[0:UserRuleCount]; repository + organization follow
	UserPath      string // the user document's path (LoadedDocument.Path); it may not exist
	UserDigest    string // the user document's content digest (LoadedDocument.Digest)
	// LayersErr is why the adoption records could not be read, when they could not:
	// the policy then holds the user layer only. A hook decides on that (Reasons says
	// why); a caller that must not act on a partial rulebook reads this.
	LayersErr error
}

// unloadableLayerMark is the one phrase a reason carries when an adopted layer's staged
// rule document does not parse. UnloadableLayer reads it back, so the phrase is built
// and recognised in one place.
const unloadableLayerMark = " adopted but unloadable: "

// UnloadableLayer returns the reason for the first adopted layer whose staged rule
// document could not be loaded, or "". The policy then lacks that layer's rules: a hook
// decides on what did load (the reason rides its decision); a caller that must not act
// on a partial rulebook — route admission at bind — reads this, as it reads LayersErr.
func (p LayeredPolicy) UnloadableLayer() string {
	for _, reason := range p.Reasons {
		if strings.Contains(reason, unloadableLayerMark) {
			return reason
		}
	}
	return ""
}

// LoadLayeredFull is the loader; LoadLayered keeps the three-value signature for
// its decide-only callers. An empty storeDir means the caller knows no store: the
// user layer alone, with no reasons — never layers.json relative to the process's
// working directory (stagedPath refuses the same case).
func LoadLayeredFull(cwd, storeDir string) (LayeredPolicy, error) {
	userDoc, err := LoadDocument()
	if err != nil {
		return LayeredPolicy{}, err
	}
	user := userDoc.Policy
	out := LayeredPolicy{Policy: &engine.Policy{Rules: make([]engine.Rule, 0, len(user.Rules))}, UserPath: userDoc.Path, UserDigest: userDoc.Digest}
	for _, r := range user.Rules {
		r.Layer = engine.LayerUser
		out.Policy.Rules = append(out.Policy.Rules, r)
	}
	out.UserRuleCount = len(out.Policy.Rules)
	if storeDir == "" {
		return out, nil
	}
	doc, err := LoadLayers(storeDir)
	if err != nil {
		// The user layer decides; the reasons name why the team layers did not.
		out.Reasons = []string{"the adoption records could not be read: " + err.Error()}
		out.LayersErr = err
		return out, nil
	}
	// The repository layer BEFORE the organization layer (§5.16.2 d5's order).
	repoLayer, repoReasons := loadRepositoryLayer(cwd, storeDir, doc)
	for _, r := range repoLayer {
		r.Layer = engine.LayerRepository
		out.Policy.Rules = append(out.Policy.Rules, r)
	}
	out.Reasons = append(out.Reasons, repoReasons...)
	for _, record := range doc.Adopted {
		if record.Scope != ScopeOrganization {
			continue
		}
		rules, orgReasons := loadAdopted(storeDir, record)
		for _, r := range rules {
			r.Layer = engine.LayerOrganization
			out.Policy.Rules = append(out.Policy.Rules, r)
		}
		out.Reasons = append(out.Reasons, orgReasons...)
	}
	return out, nil
}

// LoadLayered keeps the decide-only callers' shape.
func LoadLayered(cwd, storeDir string) (*engine.Policy, []string, error) {
	lp, err := LoadLayeredFull(cwd, storeDir)
	return lp.Policy, lp.Reasons, err
}

// loadRepositoryLayer reads THIS checkout's adopted repository layers, keyed by
// checkout root via the pointer index (R14); a shared builder so both hook lanes
// decide through the same layers in the same order. More than one organization may
// have a record for one repository (a device that relinked elsewhere keeps the
// earlier organization's adoption to its expiry); each applies, in adoption order.
func loadRepositoryLayer(cwd, storeDir string, doc LayersDocument) ([]engine.Rule, []string) {
	root, ok := repofile.Locate(cwd)
	if !ok {
		return nil, nil
	}
	idx, err := loadRepositoryIndex(storeDir)
	if err != nil {
		return nil, nil
	}
	id := idx.Roots[repoRootHash(root)]
	var rules []engine.Rule
	var reasons []string
	anyRepository := false
	for _, record := range doc.Adopted {
		if !strings.HasPrefix(record.Scope, ScopeRepositoryPrefix) {
			continue
		}
		anyRepository = true
		if id == "" || record.Scope != ScopeRepositoryPrefix+id {
			continue
		}
		layerRules, layerReasons := loadAdopted(storeDir, record)
		rules = append(rules, layerRules...)
		reasons = append(reasons, layerReasons...)
	}
	if id == "" && anyRepository {
		return nil, []string{"repository layer: not yet staged"}
	}
	return rules, reasons
}

// loadAdopted parses one adopted bundle's staged rule document, applying expiry. A
// bundle with no rulebook contributes no rules while it is in force; past its expiry
// its failure mode applies like any other bundle's.
func loadAdopted(storeDir string, adopt AdoptedBundle) ([]engine.Rule, []string) {
	var raw []byte
	if adopt.RulebookDigest != "" {
		path, err := stagedPath(storeDir, adopt.RulebookDigest)
		if err != nil {
			return nil, []string{"layer " + adopt.Scope + " adopted but unreadable"}
		}
		if raw, err = os.ReadFile(path); err != nil {
			return nil, []string{"layer " + adopt.Scope + " adopted but unreadable"}
		}
	}
	// Expiry first (§5.16.2 d6): the local clock is what a device has offline;
	// doctor and the Team console name the skew.
	if time.Now().After(adopt.ExpiresAt) {
		if adopt.FailureMode == FailClosed {
			return []engine.Rule{expiredDenyRule(adopt)}, []string{"layer " + adopt.Scope + " expired (fail-closed): rules deny until re-adopted"}
		}
		return nil, []string{"layer " + adopt.Scope + " expired (fail-open): its rules are absent for new decisions"}
	}
	if adopt.RulebookDigest == "" {
		return nil, nil
	}
	parsed, err := ruledoc.Parse(raw)
	if err != nil {
		// A corrupted staged layer fails loudly, not silently (§5.16.2 d9): the
		// decision carries the reason, and the report marks the layer unloadable.
		return nil, []string{"layer " + adopt.Scope + unloadableLayerMark + err.Error()}
	}
	return parsed.Rules, nil
}

// expiredLayerDateLayout is how an expiry date reads in a block reason.
const expiredLayerDateLayout = "2 January 2006"

// expiredOwner names whose bundle expired, in the words a person reads: the
// organization's name, its id when no name was recorded, or a plain phrase for a
// version 1 record that named neither.
func expiredOwner(adopt AdoptedBundle) string {
	switch {
	case adopt.OrganizationName != "":
		return adopt.OrganizationName
	case adopt.OrganizationID != "":
		return adopt.OrganizationID
	}
	return "your organization"
}

// expiredDenyRule is fail-closed's rule: deny everything, loudly, with a reason
// naming the organization, the date, and that the rules are set to block until
// renewed (plan §4.2). Appended last so tighten-only holds by construction.
func expiredDenyRule(adopt AdoptedBundle) engine.Rule {
	return engine.Rule{
		ID:     "layer-expired-" + adopt.Scope,
		Action: "deny",
		Message: expiredOwner(adopt) + "'s shared rules for " + adopt.Scope + " expired on " +
			adopt.ExpiresAt.UTC().Format(expiredLayerDateLayout) +
			" and are set to block until renewed (failure mode fail-closed); the next refresh lifts this, and Un-adopt in Settings → Team lifts it at once",
		Intent: "an adopted team bundle expired offline; fail-closed denies new actions until the device refreshes or un-adopts",
		// No value and no pattern: every action carries a command fact, empty for a
		// call that is not a shell command, and a value-less term matches any value.
		// A pattern such as "." would match shell commands only.
		If: engine.Predicate{Tag: engine.CommandTagKey},
	}
}

// writeLayersFile writes one state file 0600 atomically (temp, chmod, rename) —
// the link files' sequence, not a second writer beside it.
func writeLayersFile(path string, raw []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	ok = true
	return nil
}

func stagedPath(storeDir, digest string) (string, error) {
	if storeDir == "" {
		return "", fmt.Errorf("no store directory")
	}
	return filepath.Join(storeDir, layersDir, docDir, strings.Replace(digest, ":", "-", 1)+".json"), nil
}
