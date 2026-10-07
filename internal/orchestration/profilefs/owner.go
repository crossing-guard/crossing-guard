package profilefs

import (
	"bytes"
	"crossing-guard/profiledoc"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"crossing-guard/internal/filelock"
)

const (
	manifestFormat  = "crossing-guard-orchestration-profile-manifest-v1"
	selectionFormat = "crossing-guard-orchestration-profile-selection-v1"
	maxHistory      = 50
	maxJSONBytes    = 512 * 1024

	selectionTokenFrame = "crossing-guard-orchestration-profile-selection-state-v1\x00"
	absentTokenFrame    = "crossing-guard-orchestration-profile-selection-absent-v1\x00"
	profileKeyFrame     = "crossing-guard-orchestration-profile-key-v1\x00"
)

type Owner struct {
	dataDir string
	root    string
	now     func() time.Time
}

type Manifest struct {
	FormatVersion string `json:"format_version"`
	ProfileID     string `json:"profile_id"`
	SourceName    string `json:"source_name"`
	SourceDigest  string `json:"source_digest"`
	BundleDigest  string `json:"bundle_digest"`
	SourceBytes   int    `json:"source_bytes"`
	CompiledBytes int    `json:"compiled_bytes"`
}

type Revision struct {
	ProfileID    string `json:"profile_id"`
	Version      string `json:"version"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	Role         string `json:"role"`
	SourceName   string `json:"source_name"`
	SourceDigest string `json:"source_digest"`
	BundleDigest string `json:"bundle_digest"`
	SelectedAt   string `json:"selected_at"`
	SelectedBy   string `json:"selected_by"`
}

// selectionRecord is one agent's selection file. It is decoded strictly and its
// state token hashes the whole record, so Released is a pointer that is omitted when
// nil: a record that was never released marshals to the bytes it always had, and its
// token is unchanged. A build older than this field refuses a released record.
type selectionRecord struct {
	FormatVersion string            `json:"format_version"`
	ProfileID     string            `json:"profile_id"`
	StateToken    string            `json:"state_token"`
	Current       Revision          `json:"current"`
	History       []Revision        `json:"history"`
	Released      *SelectionRelease `json:"released,omitempty"`
}

type ProfileSummary struct {
	ProfileID      string   `json:"profile_id"`
	Name           string   `json:"name"`
	Description    string   `json:"description"`
	Role           string   `json:"role"`
	Version        string   `json:"version"`
	SourceDigest   string   `json:"source_digest"`
	BundleDigest   string   `json:"bundle_digest"`
	SelectedAt     string   `json:"selected_at"`
	SelectionState string   `json:"selection_state"`
	RuntimeEffects bool     `json:"runtime_effects"`
	Integrity      string   `json:"integrity"`
	Problem        *Problem `json:"problem,omitempty"`
}

type ListProblem struct {
	SelectionKey string  `json:"selection_key"`
	Problem      Problem `json:"problem"`
}

type ProfileList struct {
	Profiles       []ProfileSummary `json:"profiles"`
	Problems       []ListProblem    `json:"problems"`
	StorageRoot    string           `json:"storage_root"`
	SelectionState string           `json:"selection_state"`
	RuntimeEffects bool             `json:"runtime_effects"`
	Note           string           `json:"note"`
}

type Detail struct {
	ProfileID      string           `json:"profile_id"`
	StateToken     string           `json:"state_token,omitempty"`
	SelectionState string           `json:"selection_state"`
	RuntimeEffects bool             `json:"runtime_effects"`
	Integrity      string           `json:"integrity"`
	Current        Revision         `json:"current"`
	History        []Revision       `json:"history"`
	Manifest       *Manifest        `json:"manifest,omitempty"`
	Source         string           `json:"source,omitempty"`
	Normalized     *CompiledProfile `json:"normalized,omitempty"`
	Problem        *Problem         `json:"problem,omitempty"`
}

type SelectCommand struct {
	SourceName           string
	Source               []byte
	ExpectedSourceDigest string
	ExpectedBundleDigest string
	ExpectedStateToken   string
	// Pins lists the revisions bindings still run. It is called under the
	// mutation lock, and only when this selection would trim the bounded
	// history: a trim that would drop a pinned revision is refused, and so is
	// one whose pins cannot be read, so a place never loses its version.
	Pins PinSource
}

// PinSource lists the revisions that must stay stored for one agent.
type PinSource func() ([]RevisionRef, error)

// RevisionRef is one exact revision identity and, for a pin, what holds it.
type RevisionRef struct {
	SourceDigest string `json:"source_digest"`
	BundleDigest string `json:"bundle_digest"`
	Holder       string `json:"holder,omitempty"`
}

type SelectResult struct {
	Changed bool   `json:"changed"`
	Detail  Detail `json:"profile"`
	Note    string `json:"note"`
}

func New(dataDir string) (*Owner, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("orchestration profile data root is required")
	}
	clean := filepath.Clean(dataDir)
	return &Owner{dataDir: clean, root: filepath.Join(clean, "orchestration", "profiles"), now: time.Now}, nil
}

// Preview parses exact source bytes and reads only the matching selection state. It
// creates no directory, lock, temporary file, document, or selection.
func (owner *Owner) Preview(sourceName string, source []byte) (Preview, error) {
	document, err := Parse(sourceName, source)
	if err != nil {
		return Preview{}, err
	}
	record, err := owner.readSelection(document.Profile.ID)
	if err != nil {
		return Preview{}, integrityConflict(err)
	}
	token := absentStateToken(document.Profile.ID)
	state := "not_selected"
	if record != nil {
		token = record.StateToken
		state = "selected_inert"
	}
	return previewDocument(document, token, state), nil
}

// Select reparses re-sent exact bytes under the mutation lock, compares both content
// identities and the persistent per-profile state token, then writes immutable bytes
// before atomically publishing selection.
func (owner *Owner) Select(command SelectCommand) (SelectResult, error) {
	initial, err := Parse(command.SourceName, command.Source)
	if err != nil {
		return SelectResult{}, err
	}
	if initial.SourceDigest != command.ExpectedSourceDigest || initial.BundleDigest != command.ExpectedBundleDigest {
		return SelectResult{}, problem("state_conflict", "digests", "The profile bytes no longer match the preview.",
			"Preview the current PROFILE.md bytes again before selecting.")
	}
	if command.ExpectedStateToken == "" {
		return SelectResult{}, problem("state_conflict", "state_token", "The selection state token is missing.",
			"Preview the current PROFILE.md bytes again before selecting.")
	}
	var result SelectResult
	err = owner.withMutationLock(func() error {
		document, parseErr := Parse(command.SourceName, command.Source)
		if parseErr != nil {
			return parseErr
		}
		if document.SourceDigest != command.ExpectedSourceDigest || document.BundleDigest != command.ExpectedBundleDigest {
			return problem("state_conflict", "digests", "The profile bytes no longer match the preview.",
				"Preview the current PROFILE.md bytes again before selecting.")
		}
		selected, selectErr := owner.selectLocked(document, command.SourceName, command.ExpectedStateToken, command.Pins)
		result = selected
		return selectErr
	})
	return result, err
}

// selectLocked is Select's body; the caller holds the mutation lock. It is
// shared with PublishDraft so both select through one path under one lock.
func (owner *Owner) selectLocked(document Document, sourceName, expectedToken string, pins PinSource) (SelectResult, error) {
	current, readErr := owner.readSelection(document.Profile.ID)
	if readErr != nil {
		return SelectResult{}, integrityConflict(readErr)
	}
	actualToken := absentStateToken(document.Profile.ID)
	if current != nil {
		actualToken = current.StateToken
		if _, inspectErr := owner.inspectDocument(current.Current); inspectErr != nil {
			return SelectResult{}, integrityConflict(inspectErr)
		}
	}
	if actualToken != expectedToken {
		return SelectResult{}, problem("state_conflict", "state_token", "The selected profile changed after this preview.",
			"Preview the current PROFILE.md bytes again before selecting.")
	}
	if err := adoptedReadOnly(current); err != nil {
		return SelectResult{}, err
	}
	if current != nil && current.Current.SourceDigest == document.SourceDigest &&
		current.Current.BundleDigest == document.BundleDigest {
		detail, detailErr := owner.detailFromRecord(*current)
		if detailErr != nil {
			return SelectResult{}, detailErr
		}
		return SelectResult{Changed: false, Detail: detail,
			Note: "This exact inert profile revision was already selected; nothing was changed."}, nil
	}
	next, nextErr := owner.nextSelection(current, document, sourceName, SelectedByLocalClient, pins)
	if nextErr != nil {
		return SelectResult{}, nextErr
	}
	if installErr := owner.installDocument(document); installErr != nil {
		return SelectResult{}, installErr
	}
	if writeErr := owner.writeSelection(next); writeErr != nil {
		return SelectResult{}, writeErr
	}
	detail, detailErr := owner.detailFromRecord(next)
	if detailErr != nil {
		return SelectResult{}, detailErr
	}
	return SelectResult{Changed: true, Detail: detail,
		Note: "Exact profile source was selected as inert reusable configuration. It cannot run."}, nil
}

// nextSelection builds the record that makes document the current revision, written
// by selectedBy, with the previous current pushed onto the bounded history. It writes
// nothing. A trim that would drop a revision a place still runs is refused.
func (owner *Owner) nextSelection(current *selectionRecord, document Document, sourceName, selectedBy string, pins PinSource) (selectionRecord, error) {
	history := []Revision{}
	if current != nil {
		history = append(history, current.Current)
		history = append(history, current.History...)
		if len(history) > maxHistory {
			if err := keepsPinned(history[maxHistory:], pins); err != nil {
				return selectionRecord{}, err
			}
			history = history[:maxHistory]
		}
	}
	revision := Revision{ProfileID: document.Profile.ID, Version: document.Profile.Version,
		Name: document.Profile.Name, Description: document.Profile.Description, Role: document.Profile.Role,
		SourceName: sourceName, SourceDigest: document.SourceDigest, BundleDigest: document.BundleDigest,
		SelectedAt: owner.now().UTC().Format(time.RFC3339Nano), SelectedBy: selectedBy}
	next := selectionRecord{FormatVersion: selectionFormat, ProfileID: document.Profile.ID,
		Current: revision, History: history}
	token, tokenErr := selectionStateToken(next)
	if tokenErr != nil {
		return selectionRecord{}, tokenErr
	}
	next.StateToken = token
	return next, nil
}

// keepsPinned refuses a history trim that would drop a revision a binding
// still runs; pins that cannot be read refuse it too rather than guess.
func keepsPinned(evicted []Revision, pins PinSource) error {
	if pins == nil {
		return nil
	}
	pinned, err := pins()
	if err != nil {
		return problem("pins_unavailable", "history", "Which places run this agent's versions could not be read.",
			"Nothing was published. Try again.")
	}
	for _, revision := range evicted {
		for _, ref := range pinned {
			if ref.SourceDigest == revision.SourceDigest && ref.BundleDigest == revision.BundleDigest {
				return problem("pinned_revision", "history",
					"A place still runs the oldest stored version of this agent: "+ref.Holder+".",
					"Move that place to a newer version or turn it off, then publish again.")
			}
		}
	}
	return nil
}

// withMutationLock runs apply under the one profile mutation lock, keeping
// domain problems and wrapping everything else as storage problems.
func (owner *Owner) withMutationLock(apply func() error) error {
	if err := owner.ensureOwnerRoot(); err != nil {
		return storageProblem(err)
	}
	if err := owner.validateLockPath(); err != nil {
		return storageProblem(err)
	}
	err := filelock.With(filepath.Join(owner.root, "mutation.lock"), 0o600, apply)
	if err != nil {
		var domain *Problem
		if errors.As(err, &domain) {
			return err
		}
		return storageProblem(err)
	}
	return nil
}

func (owner *Owner) List() (ProfileList, error) {
	result := ProfileList{Profiles: []ProfileSummary{}, Problems: []ListProblem{}, StorageRoot: owner.root,
		SelectionState: "selected_inert", RuntimeEffects: false,
		Note: "Profiles are reusable instructions only. Selecting one does not run it."}
	if err := owner.checkOwnerRoot(); errors.Is(err, fs.ErrNotExist) {
		return result, nil
	} else if err != nil {
		return result, storageProblem(err)
	}
	selectionDir := filepath.Join(owner.root, "selections")
	info, err := os.Lstat(selectionDir)
	if errors.Is(err, fs.ErrNotExist) {
		return result, nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return result, storageProblem(errors.New("profile selections path is not a directory"))
	}
	entries, err := os.ReadDir(selectionDir)
	if err != nil {
		return result, storageProblem(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(selectionDir, entry.Name())
		record, readErr := owner.readSelectionPath(path, "")
		if readErr != nil {
			result.Problems = append(result.Problems, ListProblem{SelectionKey: entry.Name(),
				Problem: publicProblem(integrityConflict(readErr))})
			continue
		}
		detail, detailErr := owner.detailFromRecord(*record)
		summary := ProfileSummary{ProfileID: record.ProfileID, Name: record.Current.Name,
			Description: record.Current.Description, Role: record.Current.Role, Version: record.Current.Version,
			SourceDigest: record.Current.SourceDigest, BundleDigest: record.Current.BundleDigest,
			SelectedAt: record.Current.SelectedAt, SelectionState: "selected_inert", RuntimeEffects: false,
			Integrity: "verified"}
		if detailErr != nil {
			summary.Integrity = "needs_attention"
			p := publicProblem(integrityConflict(detailErr))
			summary.Problem = &p
		} else {
			summary.Integrity = detail.Integrity
		}
		result.Profiles = append(result.Profiles, summary)
	}
	sort.Slice(result.Profiles, func(i, j int) bool { return result.Profiles[i].ProfileID < result.Profiles[j].ProfileID })
	sort.Slice(result.Problems, func(i, j int) bool { return result.Problems[i].SelectionKey < result.Problems[j].SelectionKey })
	return result, nil
}

func (owner *Owner) Get(profileID string) (Detail, error) {
	if !profileIDPattern.MatchString(profileID) {
		return Detail{}, invalid("id", "Use a valid lowercase profile ID.")
	}
	record, err := owner.readSelection(profileID)
	if err != nil {
		return Detail{}, integrityConflict(err)
	}
	if record == nil {
		return Detail{}, problem("not_found", "id", "The selected profile was not found.",
			"Return to the profile list or import PROFILE.md.")
	}
	detail, inspectErr := owner.detailFromRecord(*record)
	if inspectErr == nil {
		return detail, nil
	}
	p := publicProblem(integrityConflict(inspectErr))
	return Detail{ProfileID: record.ProfileID, SelectionState: "selected_inert", RuntimeEffects: false,
		Integrity: "needs_attention", Current: record.Current, History: append([]Revision(nil), record.History...),
		Problem: &p}, nil
}

// GetRevision resolves one exact immutable revision that is still attributable to a
// selected profile's current or bounded history. It does not select, activate, bind,
// or otherwise grant runtime effect to that revision.
func (owner *Owner) GetRevision(profileID, sourceDigest, bundleDigest string) (Detail, error) {
	if !profileIDPattern.MatchString(profileID) || !validDigest(sourceDigest) || !validDigest(bundleDigest) {
		return Detail{}, invalid("revision", "Use an exact profile ID, source digest, and compiled digest.")
	}
	record, err := owner.readSelection(profileID)
	if err != nil {
		return Detail{}, integrityConflict(err)
	}
	if record == nil {
		return Detail{}, problem("not_found", "revision", "The selected profile revision was not found.",
			"Review the profile selection and binding identities.")
	}
	revisions := append([]Revision{record.Current}, record.History...)
	for _, revision := range revisions {
		if revision.SourceDigest != sourceDigest || revision.BundleDigest != bundleDigest {
			continue
		}
		inspected, inspectErr := owner.inspectDocument(revision)
		if inspectErr != nil {
			return Detail{}, integrityConflict(inspectErr)
		}
		return Detail{ProfileID: profileID, SelectionState: "selected_inert", RuntimeEffects: false,
			Integrity: "verified", Current: revision, Manifest: &inspected.manifest,
			Source: string(inspected.document.Source), Normalized: &inspected.document.Profile}, nil
	}
	return Detail{}, problem("not_found", "revision", "The selected profile revision was not found.",
		"Review the profile selection and binding identities.")
}

func previewDocument(document Document, stateToken, selectionState string) Preview {
	sees := make([]string, 0, len(document.Profile.Context))
	for _, context := range document.Profile.Context {
		item := context.Kind
		if context.Selector != "" {
			item += " (" + context.Selector + ")"
		}
		if context.Required {
			item += " · required"
		}
		sees = append(sees, item)
	}
	when := document.Profile.Trigger.Event
	if len(document.Profile.Trigger.States) > 0 {
		when += " in states " + strings.Join(document.Profile.Trigger.States, ", ")
	}
	does := "Produces " + document.Profile.Output.Kind + ". Authority is granted at deploy time, per binding; the profile only requests it."
	appears := map[string]string{
		"review-recommendation": "beside the reviewed action", "approval-response": "beside the held approval",
		"advice": "as attributed agent advice", "draft-reply": "in the transcript and the session's agent panel",
		"stage-classification": "in agent activity", "intervention": "beside the affected task",
	}[document.Profile.Output.Kind]
	limits := []string{document.Profile.Limits.Timeout + " timeout",
		fmt.Sprintf("%d hop(s), %d level(s) deep", document.Profile.Limits.MaxHops, document.Profile.Limits.MaxDepth),
		fmt.Sprintf("%d input bytes, %d output bytes, %d tokens", document.Profile.Limits.MaxInputBytes,
			document.Profile.Limits.MaxOutputBytes, document.Profile.Limits.MaxTokens)}
	return Preview{ProfileID: document.Profile.ID, SourceDigest: document.SourceDigest,
		BundleDigest: document.BundleDigest, StateToken: stateToken, SelectionState: selectionState,
		RuntimeEffects: false, When: when, Sees: sees, Does: does, Appears: appears,
		Destination: document.Profile.Requirements.Destination.Locality, Limits: limits,
		RequestedAuthority: append([]string(nil), document.Profile.Authority...), AuthorityGranted: false,
		Normalized: document.Profile}
}

func (owner *Owner) detailFromRecord(record selectionRecord) (Detail, error) {
	inspected, err := owner.inspectDocument(record.Current)
	if err != nil {
		return Detail{}, err
	}
	return Detail{ProfileID: record.ProfileID, StateToken: record.StateToken, SelectionState: "selected_inert", RuntimeEffects: false,
		Integrity: "verified", Current: record.Current, History: append([]Revision(nil), record.History...),
		Manifest: &inspected.manifest, Source: string(inspected.document.Source),
		Normalized: &inspected.document.Profile}, nil
}

type inspectedDocument struct {
	manifest Manifest
	document Document
}

func (owner *Owner) inspectDocument(revision Revision) (inspectedDocument, error) {
	dir, err := owner.documentDirectory(revision.BundleDigest, revision.SourceDigest)
	if err != nil {
		return inspectedDocument{}, err
	}
	if err := owner.checkDocumentDirectory(dir); err != nil {
		return inspectedDocument{}, err
	}
	source, err := readRegularFile(filepath.Join(dir, "PROFILE.md"), MaxSourceBytes)
	if err != nil {
		return inspectedDocument{}, err
	}
	canonical, err := readRegularFile(filepath.Join(dir, "compiled-profile.json"), maxJSONBytes)
	if err != nil {
		return inspectedDocument{}, err
	}
	manifestRaw, err := readRegularFile(filepath.Join(dir, "manifest.json"), maxJSONBytes)
	if err != nil {
		return inspectedDocument{}, err
	}
	var manifest Manifest
	if err := decodeStrictJSON(manifestRaw, &manifest); err != nil {
		return inspectedDocument{}, err
	}
	document, err := Parse(revision.SourceName, source)
	if err != nil {
		return inspectedDocument{}, err
	}
	if !bytes.Equal(canonical, document.Canonical) || manifest.FormatVersion != manifestFormat ||
		manifest.ProfileID != revision.ProfileID || manifest.SourceName != revision.SourceName ||
		manifest.SourceDigest != revision.SourceDigest || manifest.BundleDigest != revision.BundleDigest ||
		manifest.SourceBytes != len(source) || manifest.CompiledBytes != len(canonical) ||
		document.Profile.ID != revision.ProfileID || document.Profile.Version != revision.Version ||
		document.SourceDigest != revision.SourceDigest || document.BundleDigest != revision.BundleDigest {
		return inspectedDocument{}, errors.New("profile immutable document integrity mismatch")
	}
	return inspectedDocument{manifest: manifest, document: document}, nil
}

func (owner *Owner) installDocument(document Document) error {
	if err := owner.ensureOwnedDirectory(filepath.Join(owner.root, "documents")); err != nil {
		return err
	}
	bundleDir := filepath.Join(owner.root, "documents", digestKey(document.BundleDigest))
	if err := owner.ensureOwnedDirectory(bundleDir); err != nil {
		return err
	}
	final := filepath.Join(bundleDir, digestKey(document.SourceDigest))
	if info, err := os.Lstat(final); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return integrityConflict(errors.New("profile immutable path is not a directory"))
		}
		revision := Revision{ProfileID: document.Profile.ID, Version: document.Profile.Version,
			SourceName: "PROFILE.md", SourceDigest: document.SourceDigest, BundleDigest: document.BundleDigest}
		_, inspectErr := owner.inspectDocument(revision)
		return inspectErr
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	staging, err := os.MkdirTemp(bundleDir, ".source-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	if err := os.Chmod(staging, 0o700); err != nil {
		return err
	}
	manifest := Manifest{FormatVersion: manifestFormat, ProfileID: document.Profile.ID, SourceName: "PROFILE.md",
		SourceDigest: document.SourceDigest, BundleDigest: document.BundleDigest,
		SourceBytes: len(document.Source), CompiledBytes: len(document.Canonical)}
	manifestRaw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	manifestRaw = append(manifestRaw, '\n')
	for name, body := range map[string][]byte{"PROFILE.md": document.Source,
		"compiled-profile.json": document.Canonical, "manifest.json": manifestRaw} {
		if err := writeNewFile(filepath.Join(staging, name), body); err != nil {
			return err
		}
	}
	if err := syncDirectory(staging); err != nil {
		return err
	}
	if err := os.Rename(staging, final); err != nil {
		if info, statErr := os.Lstat(final); statErr == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			revision := Revision{ProfileID: document.Profile.ID, Version: document.Profile.Version,
				SourceName: "PROFILE.md", SourceDigest: document.SourceDigest, BundleDigest: document.BundleDigest}
			if _, inspectErr := owner.inspectDocument(revision); inspectErr == nil {
				return nil
			}
		}
		return err
	}
	return syncDirectory(bundleDir)
}

func (owner *Owner) writeSelection(record selectionRecord) error {
	if err := validateSelection(record); err != nil {
		return err
	}
	dir := filepath.Join(owner.root, "selections")
	if err := owner.ensureOwnedDirectory(dir); err != nil {
		return err
	}
	path := filepath.Join(dir, profileKey(record.ProfileID)+".json")
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("profile selection path is not a regular file")
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	body, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return atomicReplace(dir, path, append(body, '\n'), ".selection-*")
}

// atomicReplace writes body to path through a synced temporary file in dir
// and a rename, so a reader sees the old bytes or the new ones, never a mix.
func atomicReplace(dir, path string, body []byte, pattern string) error {
	temporary, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(body); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return syncDirectory(dir)
}

func (owner *Owner) readSelection(profileID string) (*selectionRecord, error) {
	if err := owner.checkOwnerRoot(); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	selectionDir := filepath.Join(owner.root, "selections")
	info, err := os.Lstat(selectionDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, errors.New("profile selections path is not a direct directory")
	}
	return owner.readSelectionPath(filepath.Join(selectionDir, profileKey(profileID)+".json"), profileID)
}

func (owner *Owner) readSelectionPath(path, expectedProfileID string) (*selectionRecord, error) {
	raw, err := readRegularFile(path, maxJSONBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var record selectionRecord
	if err := decodeStrictJSON(raw, &record); err != nil {
		return nil, err
	}
	if err := validateSelection(record); err != nil {
		return nil, err
	}
	if expectedProfileID != "" && record.ProfileID != expectedProfileID {
		return nil, errors.New("profile selection identity mismatch")
	}
	if filepath.Base(path) != profileKey(record.ProfileID)+".json" {
		return nil, errors.New("profile selection key mismatch")
	}
	return &record, nil
}

func validateSelection(record selectionRecord) error {
	if record.FormatVersion != selectionFormat || !profileIDPattern.MatchString(record.ProfileID) ||
		record.Current.ProfileID != record.ProfileID || len(record.History) > maxHistory {
		return errors.New("invalid profile selection record")
	}
	if err := validateRevision(record.Current); err != nil {
		return err
	}
	for _, revision := range record.History {
		if revision.ProfileID != record.ProfileID {
			return errors.New("profile selection history identity mismatch")
		}
		if err := validateRevision(revision); err != nil {
			return err
		}
	}
	if err := validateRelease(record); err != nil {
		return err
	}
	token, err := selectionStateToken(record)
	if err != nil || token != record.StateToken {
		return errors.New("profile selection state token mismatch")
	}
	return nil
}

func validateRevision(revision Revision) error {
	if !profileIDPattern.MatchString(revision.ProfileID) || revision.SourceName != "PROFILE.md" ||
		!validDigest(revision.SourceDigest) || !validDigest(revision.BundleDigest) ||
		!oneOf(revision.SelectedBy, SelectedByLocalClient, SelectedByTeamAdoption) || strings.TrimSpace(revision.Name) == "" ||
		!validSemver(revision.Version) || utf8.RuneCountInString(revision.Name) > 100 ||
		utf8.RuneCountInString(revision.Description) > 500 ||
		!oneOf(revision.Role, "reviewer", "follower", "coordinator", "course-corrector", "delegate") {
		return errors.New("invalid profile selection revision")
	}
	if _, err := time.Parse(time.RFC3339Nano, revision.SelectedAt); err != nil {
		return errors.New("invalid profile selection time")
	}
	return nil
}

func selectionStateToken(record selectionRecord) (string, error) {
	copyRecord := record
	copyRecord.StateToken = ""
	body, err := json.Marshal(copyRecord)
	if err != nil {
		return "", err
	}
	return framedDigest(selectionTokenFrame, body), nil
}

func absentStateToken(profileID string) string {
	return framedDigest(absentTokenFrame, []byte(profileID))
}

func profileKey(profileID string) string {
	digest := sha256.Sum256(append([]byte(profileKeyFrame), []byte(profileID)...))
	return "sha256-v1-" + hex.EncodeToString(digest[:])
}

func digestKey(digest string) string { return strings.Replace(digest, ":", "-", 1) }

func validDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256-v1:") || len(value) != len("sha256-v1:")+64 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256-v1:"))
	return err == nil
}

func (owner *Owner) documentDirectory(bundleDigest, sourceDigest string) (string, error) {
	if !validDigest(bundleDigest) || !validDigest(sourceDigest) {
		return "", errors.New("invalid profile document digest")
	}
	return filepath.Join(owner.root, "documents", digestKey(bundleDigest), digestKey(sourceDigest)), nil
}

func (owner *Owner) ensureOwnerRoot() error {
	info, err := os.Lstat(owner.dataDir)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("daemon data root is not a direct directory")
	}
	if err := ensureDirectoryComponent(filepath.Join(owner.dataDir, "orchestration")); err != nil {
		return err
	}
	return ensureDirectoryComponent(owner.root)
}

func (owner *Owner) ensureOwnedDirectory(path string) error {
	if !withinPath(owner.root, path) {
		return errors.New("profile owner path escaped its root")
	}
	relative, err := filepath.Rel(owner.root, path)
	if err != nil {
		return err
	}
	current := owner.root
	if relative == "." {
		return ensureDirectoryComponent(current)
	}
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		if err := ensureDirectoryComponent(current); err != nil {
			return err
		}
	}
	return nil
}

func ensureDirectoryComponent(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.Mkdir(path, 0o700); err != nil {
			return err
		}
		return os.Chmod(path, 0o700)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("profile owner component is not a direct directory")
	}
	return os.Chmod(path, 0o700)
}

func (owner *Owner) checkOwnerRoot() error {
	current := owner.dataDir
	for _, part := range []string{"", "orchestration", "profiles"} {
		if part != "" {
			current = filepath.Join(current, part)
		}
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("profile owner component is not a direct directory")
		}
	}
	return nil
}

func (owner *Owner) validateLockPath() error {
	path := filepath.Join(owner.root, "mutation.lock")
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("profile mutation lock is not a regular file")
	}
	return nil
}

func (owner *Owner) checkDocumentDirectory(path string) error {
	if !withinPath(owner.root, path) {
		return errors.New("profile document path escaped its root")
	}
	relative, err := filepath.Rel(owner.root, path)
	if err != nil {
		return err
	}
	current := owner.root
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("profile document component is not a direct directory")
		}
	}
	return nil
}

func readRegularFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("profile path is not a direct regular file")
	}
	if info.Size() > limit {
		return nil, errors.New("profile file exceeds its storage bound")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(io.LimitReader(file, limit+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if int64(len(body)) > limit {
		return nil, errors.New("profile file exceeds its storage bound")
	}
	return body, nil
}

func writeNewFile(path string, body []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return err
	}
	return directory.Close()
}

func decodeStrictJSON(body []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}

func integrityConflict(cause error) error {
	var domain *Problem
	if errors.As(cause, &domain) && domain.Code == "integrity_conflict" {
		return cause
	}
	return profiledoc.WrapProblem("integrity_conflict", "Stored profile state failed integrity checks.",
		"Preserve the local profile data for review; do not overwrite it as a normal update.", cause)
}

func publicProblem(err error) Problem {
	var source *Problem
	if errors.As(err, &source) {
		return Problem{Code: source.Code, Field: source.Field, Message: source.Message, Recovery: source.Recovery}
	}
	return Problem{Code: "storage_error", Message: "Profile storage is unavailable.",
		Recovery: "Review the local profile storage diagnostics and retry."}
}

func withinPath(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) &&
		!filepath.IsAbs(relative)
}
