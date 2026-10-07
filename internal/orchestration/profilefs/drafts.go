package profilefs

// Inert drafts (agents-settings-redesign plan §5; lifecycle design §8.1).
// One draft per profile id holds work in progress: exact bytes that may not
// validate yet. A draft never runs and never changes a binding; publishing it
// selects its exact bytes through the same install + selection path an import
// uses, and removes the draft, under the one mutation lock.

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

const (
	draftFormat         = "crossing-guard-orchestration-profile-draft-v1"
	draftTokenFrame     = "crossing-guard-orchestration-profile-draft-state-v1\x00"
	draftAbsentFrame    = "crossing-guard-orchestration-profile-draft-absent-v1\x00"
	maxDraftRecordBytes = MaxSourceBytes*2 + 64*1024
)

// Draft is one profile's work in progress as the API returns it. Digests and
// Normalized are present only when the bytes validate; Problem says why not.
type Draft struct {
	ProfileID        string           `json:"profile_id"`
	StateToken       string           `json:"state_token"`
	UpdatedAt        string           `json:"updated_at"`
	BaseSourceDigest string           `json:"base_source_digest,omitempty"`
	BaseBundleDigest string           `json:"base_bundle_digest,omitempty"`
	Source           string           `json:"source"`
	SourceDigest     string           `json:"source_digest,omitempty"`
	BundleDigest     string           `json:"bundle_digest,omitempty"`
	Normalized       *CompiledProfile `json:"normalized,omitempty"`
	Problem          *Problem         `json:"problem,omitempty"`
}

type draftRecord struct {
	FormatVersion    string `json:"format_version"`
	ProfileID        string `json:"profile_id"`
	BaseSourceDigest string `json:"base_source_digest"`
	BaseBundleDigest string `json:"base_bundle_digest"`
	UpdatedAt        string `json:"updated_at"`
	Source           string `json:"source"`
	StateToken       string `json:"state_token"`
}

// DraftCommand saves exact bytes as profileID's draft under CAS.
type DraftCommand struct {
	ProfileID          string
	Source             []byte
	BaseSourceDigest   string
	BaseBundleDigest   string
	ExpectedStateToken string
}

// SelectionAbsentToken is the selection state of an agent never published:
// the token a first publish expects.
func SelectionAbsentToken(profileID string) string {
	return absentStateToken(profileID)
}

// DraftAbsentToken is the CAS token for creating a draft that does not exist.
func DraftAbsentToken(profileID string) string {
	return framedDigest(draftAbsentFrame, []byte(profileID))
}

// Draft returns profileID's draft, or found=false.
func (owner *Owner) Draft(profileID string) (Draft, bool, error) {
	if !profileIDPattern.MatchString(profileID) {
		return Draft{}, false, invalid("id", "Use a valid lowercase profile ID.")
	}
	record, err := owner.readDraft(profileID)
	if err != nil {
		return Draft{}, false, integrityConflict(err)
	}
	if record == nil {
		return Draft{}, false, nil
	}
	return draftFromRecord(*record), true, nil
}

// Drafts lists every stored draft, ordered by profile id. A draft file that
// fails to read is skipped here and surfaced by Draft for its own id.
func (owner *Owner) Drafts() ([]Draft, error) {
	out := []Draft{}
	if err := owner.checkOwnerRoot(); errors.Is(err, fs.ErrNotExist) {
		return out, nil
	} else if err != nil {
		return out, storageProblem(err)
	}
	dir := filepath.Join(owner.root, "drafts")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, storageProblem(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		record, readErr := owner.readDraftPath(filepath.Join(dir, entry.Name()), "")
		if readErr != nil || record == nil {
			continue
		}
		out = append(out, draftFromRecord(*record))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProfileID < out[j].ProfileID })
	return out, nil
}

// PutDraft stores exact bytes as the draft. The bytes need not validate; they
// must be bounded UTF-8 with no NUL, and a valid document must carry the
// draft's own id.
func (owner *Owner) PutDraft(command DraftCommand) (Draft, error) {
	if !profileIDPattern.MatchString(command.ProfileID) {
		return Draft{}, invalid("id", "Use a valid lowercase profile ID.")
	}
	if len(command.Source) == 0 || len(command.Source) > MaxSourceBytes || !validDraftText(command.Source) {
		return Draft{}, invalid("source", "Use nonempty UTF-8 up to 262144 bytes without NUL bytes.")
	}
	if document, err := Parse("PROFILE.md", command.Source); err == nil && document.Profile.ID != command.ProfileID {
		return Draft{}, invalid("id", "A draft keeps its agent's ID.")
	}
	var saved Draft
	err := owner.withMutationLock(func() error {
		current, readErr := owner.readDraft(command.ProfileID)
		if readErr != nil {
			return integrityConflict(readErr)
		}
		selection, selectionErr := owner.readSelection(command.ProfileID)
		if selectionErr != nil {
			return integrityConflict(selectionErr)
		}
		if err := adoptedReadOnly(selection); err != nil {
			return err
		}
		expected := DraftAbsentToken(command.ProfileID)
		base := [2]string{command.BaseSourceDigest, command.BaseBundleDigest}
		if current != nil {
			expected = current.StateToken
			base = [2]string{current.BaseSourceDigest, current.BaseBundleDigest}
		}
		if command.ExpectedStateToken != expected {
			return problem("state_conflict", "state_token", "The draft changed in another window.",
				"Reload the agent to see the newest draft.")
		}
		record := draftRecord{FormatVersion: draftFormat, ProfileID: command.ProfileID,
			BaseSourceDigest: base[0], BaseBundleDigest: base[1],
			UpdatedAt: owner.now().UTC().Format(time.RFC3339Nano), Source: string(command.Source)}
		token, tokenErr := draftStateToken(record)
		if tokenErr != nil {
			return tokenErr
		}
		record.StateToken = token
		if writeErr := owner.writeDraft(record); writeErr != nil {
			return writeErr
		}
		saved = draftFromRecord(record)
		return nil
	})
	return saved, err
}

// DiscardDraft deletes the draft when expectedToken still matches it.
func (owner *Owner) DiscardDraft(profileID, expectedToken string) error {
	if !profileIDPattern.MatchString(profileID) {
		return invalid("id", "Use a valid lowercase profile ID.")
	}
	return owner.withMutationLock(func() error {
		current, err := owner.readDraft(profileID)
		if err != nil {
			return integrityConflict(err)
		}
		if current == nil {
			return problem("not_found", "draft", "There is no draft to discard.", "Reload the agent.")
		}
		if current.StateToken != expectedToken {
			return problem("state_conflict", "state_token", "The draft changed in another window.",
				"Reload the agent to see the newest draft.")
		}
		return owner.removeDraft(profileID)
	})
}

// PublishDraft selects the draft's exact bytes and removes the draft, both
// under the one mutation lock. expectedSelectionToken is the selection state
// the author saw (the absent token for a never-published agent), so a version
// selected elsewhere since then conflicts instead of being silently replaced.
func (owner *Owner) PublishDraft(profileID, expectedDraftToken, expectedSelectionToken string, pins PinSource) (SelectResult, error) {
	if !profileIDPattern.MatchString(profileID) {
		return SelectResult{}, invalid("id", "Use a valid lowercase profile ID.")
	}
	if err := owner.ensureOwnerRoot(); err != nil {
		return SelectResult{}, storageProblem(err)
	}
	var result SelectResult
	err := owner.withMutationLock(func() error {
		current, err := owner.readDraft(profileID)
		if err != nil {
			return integrityConflict(err)
		}
		if current == nil {
			return problem("not_found", "draft", "There is no draft to publish.", "Reload the agent.")
		}
		if current.StateToken != expectedDraftToken {
			return problem("state_conflict", "state_token", "The draft changed in another window.",
				"Reload the agent to see the newest draft.")
		}
		document, parseErr := Parse("PROFILE.md", []byte(current.Source))
		if parseErr != nil {
			return parseErr
		}
		if document.Profile.ID != profileID {
			return invalid("id", "A draft keeps its agent's ID.")
		}
		selection, readErr := owner.readSelection(profileID)
		if readErr != nil {
			return integrityConflict(readErr)
		}
		if err := draftBaseCurrent(*current, selection); err != nil {
			return err
		}
		if err := versionFree(document, selection); err != nil {
			return err
		}
		selected, selectErr := owner.selectLocked(document, "PROFILE.md", expectedSelectionToken, pins)
		if selectErr != nil {
			return selectErr
		}
		// The version is selected at this point; a draft file that will not go
		// away is reported beside the success, never as a failed publish.
		if removeErr := owner.removeDraft(profileID); removeErr != nil {
			selected.Note = "Published; the draft could not be removed (" + removeErr.Error() + "). Discard it."
		}
		result = selected
		return nil
	})
	return result, err
}

// draftBaseCurrent refuses a publish when the agent's selected version moved
// since the draft started (an import or another publish landed meanwhile).
func draftBaseCurrent(draft draftRecord, selection *selectionRecord) error {
	if selection == nil {
		if draft.BaseSourceDigest == "" && draft.BaseBundleDigest == "" {
			return nil
		}
	} else if selection.Current.SourceDigest == draft.BaseSourceDigest && selection.Current.BundleDigest == draft.BaseBundleDigest {
		return nil
	}
	return problem("state_conflict", "base", "A different version was published since this draft started.",
		"Review the current version, then discard this draft or edit it again.")
}

// versionFree refuses a version label that already names a different stored
// revision of the agent, so versions stay unambiguous in Versions and moves.
func versionFree(document Document, selection *selectionRecord) error {
	if selection == nil {
		return nil
	}
	for _, revision := range append([]Revision{selection.Current}, selection.History...) {
		if revision.Version == document.Profile.Version &&
			(revision.SourceDigest != document.SourceDigest || revision.BundleDigest != document.BundleDigest) {
			return problem("version_taken", "version", "Another stored version already uses this version number.",
				"Choose a new version number for this draft.")
		}
	}
	return nil
}

func draftFromRecord(record draftRecord) Draft {
	out := Draft{ProfileID: record.ProfileID, StateToken: record.StateToken, UpdatedAt: record.UpdatedAt,
		BaseSourceDigest: record.BaseSourceDigest, BaseBundleDigest: record.BaseBundleDigest, Source: record.Source}
	document, err := Parse("PROFILE.md", []byte(record.Source))
	if err != nil {
		p := publicProblem(err)
		out.Problem = &p
		return out
	}
	out.SourceDigest, out.BundleDigest = document.SourceDigest, document.BundleDigest
	profile := document.Profile
	out.Normalized = &profile
	return out
}

func draftStateToken(record draftRecord) (string, error) {
	record.StateToken = ""
	body, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	return framedDigest(draftTokenFrame, body), nil
}

func validDraftText(source []byte) bool {
	for _, b := range source {
		if b == 0 {
			return false
		}
	}
	return strings.ToValidUTF8(string(source), "�") == string(source)
}

func (owner *Owner) draftPath(profileID string) string {
	return filepath.Join(owner.root, "drafts", profileKey(profileID)+".json")
}

func (owner *Owner) readDraft(profileID string) (*draftRecord, error) {
	if err := owner.checkOwnerRoot(); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return owner.readDraftPath(owner.draftPath(profileID), profileID)
}

func (owner *Owner) readDraftPath(path, expectedProfileID string) (*draftRecord, error) {
	body, err := readRegularFile(path, maxDraftRecordBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var record draftRecord
	if err := decodeStrictJSON(body, &record); err != nil {
		return nil, err
	}
	if record.FormatVersion != draftFormat || !profileIDPattern.MatchString(record.ProfileID) ||
		(expectedProfileID != "" && record.ProfileID != expectedProfileID) ||
		filepath.Base(path) != profileKey(record.ProfileID)+".json" {
		return nil, errors.New("profile draft record is invalid")
	}
	token, err := draftStateToken(record)
	if err != nil || token != record.StateToken {
		return nil, errors.New("profile draft integrity mismatch")
	}
	return &record, nil
}

func (owner *Owner) writeDraft(record draftRecord) error {
	dir := filepath.Join(owner.root, "drafts")
	if err := owner.ensureOwnedDirectory(dir); err != nil {
		return err
	}
	body, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return atomicReplace(dir, owner.draftPath(record.ProfileID), append(body, '\n'), ".draft-*")
}

func (owner *Owner) removeDraft(profileID string) error {
	path := owner.draftPath(profileID)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}
