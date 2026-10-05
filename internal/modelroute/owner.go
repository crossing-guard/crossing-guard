package modelroute

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"crossing-guard/engine"
	"crossing-guard/internal/filelock"
)

// Owner is the one writer and reader of <dataDir>/models/routes/. It holds no state
// beyond its paths, so several owners over one directory agree: every mutation takes
// the directory's mutation lock and every read verifies what it reads.
type Owner struct {
	dataDir string
	root    string
	now     func() time.Time
	mint    func() string
}

// revisionRef names one revision in a selection record.
type revisionRef struct {
	RevisionDigest string `json:"revision_digest"`
	Name           string `json:"name"`
	CreatedAt      string `json:"created_at"`
}

type selectionRecord struct {
	FormatVersion string        `json:"format_version"`
	RouteID       string        `json:"route_id"`
	StateToken    string        `json:"state_token"`
	Current       revisionRef   `json:"current"`
	History       []revisionRef `json:"history"`
	// MigratedAt is set once, on a route the migration pass created from a binding's
	// typed settings (plan §5.6); a route a person created has none.
	MigratedAt string `json:"migrated_at,omitempty"`
}

// Route is a route's current revision with the selection facts beside it.
type Route struct {
	RouteID        string   `json:"route_id"`
	Name           string   `json:"name"`
	Family         string   `json:"family"`
	Kind           string   `json:"kind"`
	Fields         Fields   `json:"fields"`
	RevisionDigest string   `json:"revision_digest"`
	StateToken     string   `json:"state_token"`
	CreatedAt      string   `json:"created_at"`
	MigratedAt     string   `json:"migrated_at,omitempty"`
	History        []string `json:"history"`
}

// ListProblem is one selection file that could not be read as a route.
type ListProblem struct {
	SelectionKey string  `json:"selection_key"`
	Problem      Problem `json:"problem"`
}

// RouteList is every readable route, by name, and every selection that is not.
type RouteList struct {
	Routes   []Route       `json:"routes"`
	Problems []ListProblem `json:"problems"`
}

// Preview is what Select would do with a draft: the normalised draft, the digest and
// state token Select must be given back, and whether anything would change.
type Preview struct {
	Draft         Draft  `json:"draft"`
	Kind          string `json:"kind"`
	PreviewDigest string `json:"preview_digest"`
	StateToken    string `json:"state_token"`
	Exists        bool   `json:"exists"`
	Changed       bool   `json:"changed"`
}

// SelectCommand creates a route (Draft.RouteID empty) or writes a new revision of one.
type SelectCommand struct {
	Draft                 Draft
	ExpectedPreviewDigest string
	ExpectedStateToken    string
	Confirmed             bool
	// Migrated marks a route the migration pass creates; it is ignored on an edit.
	Migrated bool
}

// SelectResult is the route after a Select and whether a revision was written.
type SelectResult struct {
	Changed bool  `json:"changed"`
	Created bool  `json:"created"`
	Route   Route `json:"route"`
}

// New returns the owner of dataDir's routes. It creates nothing.
func New(dataDir string) (*Owner, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("model route data root is required")
	}
	clean := filepath.Clean(dataDir)
	return &Owner{dataDir: clean, root: filepath.Join(clean, "models", "routes"), now: time.Now,
		mint: func() string { return engine.NewTypedID(IDPrefix) }}, nil
}

// Root is the directory the owner stores routes under.
func (owner *Owner) Root() string { return owner.root }

// DataDir is the data directory the owner was opened on.
func (owner *Owner) DataDir() string { return owner.dataDir }

// AbsentStateToken is the state token a create must present: there is no selection yet.
func AbsentStateToken() string { return framedDigest(absentTokenFrame, nil) }

// List reads every route. A missing directory is an empty list, not an error.
func (owner *Owner) List() (RouteList, error) {
	result := RouteList{Routes: []Route{}, Problems: []ListProblem{}}
	entries, err := owner.selectionEntries()
	if err != nil {
		return result, storageProblem(err)
	}
	config, err := loadConfig(owner.dataDir)
	if err != nil {
		return result, storageProblem(err)
	}
	for _, entry := range entries {
		route, readErr := owner.routeAt(entry, "", config)
		if readErr != nil {
			result.Problems = append(result.Problems, ListProblem{SelectionKey: filepath.Base(entry),
				Problem: publicProblem(integrityConflict(readErr))})
			continue
		}
		result.Routes = append(result.Routes, route)
	}
	sort.Slice(result.Routes, func(i, j int) bool {
		left, right := NameKey(result.Routes[i].Name), NameKey(result.Routes[j].Name)
		if left != right {
			return left < right
		}
		return result.Routes[i].RouteID < result.Routes[j].RouteID
	})
	sort.Slice(result.Problems, func(i, j int) bool { return result.Problems[i].SelectionKey < result.Problems[j].SelectionKey })
	return result, nil
}

// Get reads one route's current revision, verifying it against its digest.
func (owner *Owner) Get(routeID string) (Route, error) {
	if !ValidID(routeID) {
		return Route{}, invalid("route_id", "Use a valid route id.")
	}
	config, err := loadConfig(owner.dataDir)
	if err != nil {
		return Route{}, storageProblem(err)
	}
	route, err := owner.routeAt(owner.selectionPath(routeID), routeID, config)
	if errors.Is(err, fs.ErrNotExist) {
		return Route{}, problem(CodeNotFound, "route_id", "This model route was not found.",
			"Choose another model route in Settings → Models.")
	}
	if err != nil {
		return Route{}, integrityConflict(err)
	}
	return route, nil
}

// FindByFields returns the route of a family whose fields equal these, the one with
// the smallest id when several do. The migration pass uses it to stay idempotent.
func (owner *Owner) FindByFields(family string, fields Fields) (Route, bool, error) {
	listed, err := owner.List()
	if err != nil {
		return Route{}, false, err
	}
	found, ok := Route{}, false
	for _, route := range listed.Routes {
		if route.Family != family || !SameFields(route.Fields, fields) {
			continue
		}
		if !ok || route.RouteID < found.RouteID {
			found, ok = route, true
		}
	}
	return found, ok, nil
}

// Preview normalises a draft and reads only the matching selection. It writes nothing:
// no directory, lock, document or selection.
func (owner *Owner) Preview(draft Draft) (Preview, error) {
	config, err := loadConfig(owner.dataDir)
	if err != nil {
		return Preview{}, storageProblem(err)
	}
	normalized, err := normalizeDraft(draft, config)
	if err != nil {
		return Preview{}, err
	}
	digest, err := PreviewDigest(normalized)
	if err != nil {
		return Preview{}, storageProblem(err)
	}
	preview := Preview{Draft: normalized, Kind: KindFor(normalized.Family), PreviewDigest: digest,
		StateToken: AbsentStateToken(), Changed: true}
	if normalized.RouteID != "" {
		current, getErr := owner.Get(normalized.RouteID)
		if getErr != nil {
			return Preview{}, getErr
		}
		if current.Family != normalized.Family {
			return Preview{}, invalid("family", "A route keeps its family; create another route instead.")
		}
		preview.Exists, preview.StateToken = true, current.StateToken
		preview.Changed = current.Name != normalized.Name || !SameFields(current.Fields, normalized.Fields)
	}
	if err := owner.nameFree(normalized, config); err != nil {
		return Preview{}, err
	}
	return preview, nil
}

// Select writes a new immutable revision, then atomically publishes it as the route's
// current one. It requires the previewed digest, the state token and confirmation, and
// re-checks all three under the mutation lock.
func (owner *Owner) Select(command SelectCommand) (SelectResult, error) {
	if !command.Confirmed {
		return SelectResult{}, problem(CodeUnconfirmed, "confirmed", "Confirm the model route before saving it.", "")
	}
	if command.ExpectedStateToken == "" || command.ExpectedPreviewDigest == "" {
		return SelectResult{}, problem(CodeStateConflict, "state_token", "The preview digest and state token are required.",
			"Preview the model route again before saving it.")
	}
	var result SelectResult
	err := owner.withMutationLock(func() error {
		selected, selectErr := owner.selectLocked(command)
		result = selected
		return selectErr
	})
	return result, err
}

func (owner *Owner) selectLocked(command SelectCommand) (SelectResult, error) {
	config, err := loadConfig(owner.dataDir)
	if err != nil {
		return SelectResult{}, storageProblem(err)
	}
	draft, err := normalizeDraft(command.Draft, config)
	if err != nil {
		return SelectResult{}, err
	}
	digest, err := PreviewDigest(draft)
	if err != nil {
		return SelectResult{}, storageProblem(err)
	}
	if digest != command.ExpectedPreviewDigest {
		return SelectResult{}, problem(CodeStateConflict, "preview_digest", "The model route no longer matches the preview.",
			"Preview the model route again before saving it.")
	}
	if err := owner.nameFree(draft, config); err != nil {
		return SelectResult{}, err
	}
	record := selectionRecord{FormatVersion: selectionFormat, History: []revisionRef{}}
	created := draft.RouteID == ""
	if created {
		if command.ExpectedStateToken != AbsentStateToken() {
			return SelectResult{}, problem(CodeStateConflict, "state_token", "The state token is not the one a new route presents.",
				"Preview the model route again before saving it.")
		}
		draft.RouteID = owner.mint()
		record.RouteID = draft.RouteID
		if command.Migrated {
			record.MigratedAt = owner.now().UTC().Format(time.RFC3339Nano)
		}
	} else {
		current, readErr := owner.readSelection(owner.selectionPath(draft.RouteID), draft.RouteID, config)
		if errors.Is(readErr, fs.ErrNotExist) {
			return SelectResult{}, problem(CodeNotFound, "route_id", "This model route was not found.",
				"Reload Settings → Models.")
		}
		if readErr != nil {
			return SelectResult{}, integrityConflict(readErr)
		}
		existing, readErr := owner.routeFromRecord(*current, config)
		if readErr != nil {
			return SelectResult{}, integrityConflict(readErr)
		}
		if current.StateToken != command.ExpectedStateToken {
			return SelectResult{}, problem(CodeStateConflict, "state_token", "The model route changed after this preview.",
				"Preview the model route again before saving it.")
		}
		if existing.Family != draft.Family {
			return SelectResult{}, invalid("family", "A route keeps its family; create another route instead.")
		}
		if existing.Name == draft.Name && SameFields(existing.Fields, draft.Fields) {
			return SelectResult{Changed: false, Route: existing}, nil
		}
		record = *current
		history := append([]revisionRef{current.Current}, current.History...)
		if len(history) > config.HistoryMax {
			history = history[:config.HistoryMax]
		}
		record.History = history
	}
	revision := Revision{FormatVersion: revisionFormat, RouteID: draft.RouteID, Name: draft.Name, Family: draft.Family,
		Kind: KindFor(draft.Family), Fields: draft.Fields, CreatedAt: owner.now().UTC().Format(time.RFC3339Nano)}
	revisionDigest, err := owner.installRevision(revision)
	if err != nil {
		return SelectResult{}, err
	}
	record.Current = revisionRef{RevisionDigest: revisionDigest, Name: revision.Name, CreatedAt: revision.CreatedAt}
	token, err := selectionStateToken(record)
	if err != nil {
		return SelectResult{}, storageProblem(err)
	}
	record.StateToken = token
	if err := owner.writeSelection(record, config); err != nil {
		return SelectResult{}, err
	}
	route, err := owner.routeFromRecord(record, config)
	if err != nil {
		return SelectResult{}, integrityConflict(err)
	}
	return SelectResult{Changed: true, Created: created, Route: route}, nil
}

// Delete removes a route. inUse is asked under the mutation lock; a route a place or a
// chain entry still references is refused, naming them. A nil inUse means nothing can
// reference the route (a test, or a store that does not exist yet).
func (owner *Owner) Delete(routeID, expectedStateToken string, inUse func() ([]string, error)) error {
	if !ValidID(routeID) {
		return invalid("route_id", "Use a valid route id.")
	}
	return owner.withMutationLock(func() error {
		config, err := loadConfig(owner.dataDir)
		if err != nil {
			return storageProblem(err)
		}
		path := owner.selectionPath(routeID)
		record, readErr := owner.readSelection(path, routeID, config)
		if errors.Is(readErr, fs.ErrNotExist) {
			return problem(CodeNotFound, "route_id", "This model route was not found.", "Reload Settings → Models.")
		}
		if readErr != nil {
			return integrityConflict(readErr)
		}
		if record.StateToken != expectedStateToken {
			return problem(CodeStateConflict, "state_token", "The model route changed after it was read.",
				"Reload Settings → Models and try again.")
		}
		if inUse != nil {
			holders, useErr := inUse()
			if useErr != nil {
				return problem(CodeInUse, "route_id", "Which places use this model route could not be read.",
					"Nothing was deleted. Try again.")
			}
			if len(holders) > 0 {
				return problem(CodeInUse, "route_id", "This model route is still used by: "+strings.Join(holders, ", ")+".",
					"Choose another model route for each of those places, then delete this one.")
			}
		}
		if err := os.Remove(path); err != nil {
			return storageProblem(err)
		}
		if err := syncDirectory(filepath.Dir(path)); err != nil {
			return storageProblem(err)
		}
		// The immutable documents are no longer reachable; removing them is tidying,
		// and a failure leaves only unreferenced files.
		for _, ref := range append([]revisionRef{record.Current}, record.History...) {
			_ = os.RemoveAll(filepath.Join(owner.root, "documents", digestKey(ref.RevisionDigest)))
		}
		return nil
	})
}

// nameFree refuses a draft whose name another route already has, after trimming and
// case folding.
func (owner *Owner) nameFree(draft Draft, config Config) error {
	entries, err := owner.selectionEntries()
	if err != nil {
		return storageProblem(err)
	}
	key := NameKey(draft.Name)
	for _, entry := range entries {
		record, readErr := owner.readSelection(entry, "", config)
		if readErr != nil || record.RouteID == draft.RouteID {
			continue
		}
		if NameKey(record.Current.Name) == key {
			return problem(CodeNameTaken, "name", "Another model route is already named "+record.Current.Name+".",
				"Choose a different name.")
		}
	}
	return nil
}

func (owner *Owner) routeAt(path, expectedID string, config Config) (Route, error) {
	record, err := owner.readSelection(path, expectedID, config)
	if err != nil {
		return Route{}, err
	}
	return owner.routeFromRecord(*record, config)
}

func (owner *Owner) routeFromRecord(record selectionRecord, config Config) (Route, error) {
	revision, err := owner.readRevision(record.Current.RevisionDigest, config)
	if err != nil {
		return Route{}, err
	}
	if revision.RouteID != record.RouteID || revision.Name != record.Current.Name || revision.CreatedAt != record.Current.CreatedAt {
		return Route{}, errors.New("model route selection does not match its revision")
	}
	history := make([]string, 0, len(record.History))
	for _, ref := range record.History {
		history = append(history, ref.RevisionDigest)
	}
	return Route{RouteID: record.RouteID, Name: revision.Name, Family: revision.Family, Kind: revision.Kind,
		Fields: revision.Fields, RevisionDigest: record.Current.RevisionDigest, StateToken: record.StateToken,
		CreatedAt: revision.CreatedAt, MigratedAt: record.MigratedAt, History: history}, nil
}

func (owner *Owner) readRevision(digest string, config Config) (Revision, error) {
	if !validDigest(digest) {
		return Revision{}, errors.New("invalid model route revision digest")
	}
	dir := filepath.Join(owner.root, "documents", digestKey(digest))
	if err := owner.checkDirectoryChain(dir); err != nil {
		return Revision{}, err
	}
	raw, err := readRegularFile(filepath.Join(dir, "route.json"), maxJSONBytes)
	if err != nil {
		return Revision{}, err
	}
	var revision Revision
	if err := decodeStrictJSON(raw, &revision); err != nil {
		return Revision{}, err
	}
	if err := validateRevision(revision, config); err != nil {
		return Revision{}, err
	}
	actual, err := RevisionDigest(revision)
	if err != nil || actual != digest {
		return Revision{}, errors.New("model route immutable document integrity mismatch")
	}
	return revision, nil
}

// installRevision writes one immutable document and returns its digest. A document
// that already exists is verified, never rewritten.
func (owner *Owner) installRevision(revision Revision) (string, error) {
	digest, err := RevisionDigest(revision)
	if err != nil {
		return "", storageProblem(err)
	}
	documents := filepath.Join(owner.root, "documents")
	if err := ensureDirectoryComponent(documents); err != nil {
		return "", storageProblem(err)
	}
	final := filepath.Join(documents, digestKey(digest))
	if info, statErr := os.Lstat(final); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", integrityConflict(errors.New("model route immutable path is not a directory"))
		}
		if _, readErr := owner.readRevision(digest, Config{NameMaxRunes: nameMaxRunesCeiling}); readErr != nil {
			return "", integrityConflict(readErr)
		}
		return digest, nil
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return "", storageProblem(statErr)
	}
	staging, err := os.MkdirTemp(documents, ".route-*")
	if err != nil {
		return "", storageProblem(err)
	}
	defer func() { _ = os.RemoveAll(staging) }()
	if err := os.Chmod(staging, 0o700); err != nil {
		return "", storageProblem(err)
	}
	body, err := json.MarshalIndent(revision, "", "  ")
	if err != nil {
		return "", storageProblem(err)
	}
	if err := writeNewFile(filepath.Join(staging, "route.json"), append(body, '\n')); err != nil {
		return "", storageProblem(err)
	}
	if err := syncDirectory(staging); err != nil {
		return "", storageProblem(err)
	}
	if err := os.Rename(staging, final); err != nil {
		return "", storageProblem(err)
	}
	if err := syncDirectory(documents); err != nil {
		return "", storageProblem(err)
	}
	return digest, nil
}

func (owner *Owner) writeSelection(record selectionRecord, config Config) error {
	if err := validateSelection(record, config); err != nil {
		return storageProblem(err)
	}
	dir := filepath.Join(owner.root, "selections")
	if err := ensureDirectoryComponent(dir); err != nil {
		return storageProblem(err)
	}
	path := owner.selectionPath(record.RouteID)
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return integrityConflict(errors.New("model route selection path is not a regular file"))
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return storageProblem(err)
	}
	body, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return storageProblem(err)
	}
	if err := atomicReplace(dir, path, append(body, '\n'), ".selection-*"); err != nil {
		return storageProblem(err)
	}
	return nil
}

func (owner *Owner) selectionPath(routeID string) string {
	return filepath.Join(owner.root, "selections", routeID+".json")
}

// selectionEntries lists the selection files. A directory that does not exist yet has
// none.
func (owner *Owner) selectionEntries() ([]string, error) {
	dir := filepath.Join(owner.root, "selections")
	if err := owner.checkDirectoryChain(dir); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		out = append(out, filepath.Join(dir, entry.Name()))
	}
	sort.Strings(out)
	return out, nil
}

func (owner *Owner) readSelection(path, expectedID string, config Config) (*selectionRecord, error) {
	if err := owner.checkDirectoryChain(filepath.Dir(path)); err != nil {
		return nil, err
	}
	raw, err := readRegularFile(path, maxJSONBytes)
	if err != nil {
		return nil, err
	}
	var record selectionRecord
	if err := decodeStrictJSON(raw, &record); err != nil {
		return nil, err
	}
	if err := validateSelection(record, config); err != nil {
		return nil, err
	}
	if expectedID != "" && record.RouteID != expectedID {
		return nil, errors.New("model route selection identity mismatch")
	}
	if filepath.Base(path) != record.RouteID+".json" {
		return nil, errors.New("model route selection key mismatch")
	}
	return &record, nil
}

func validateSelection(record selectionRecord, config Config) error {
	// A history longer than today's bound is still readable: the bound may have been
	// lowered since. Only the hard ceiling is an integrity failure.
	if record.FormatVersion != selectionFormat || !ValidID(record.RouteID) || len(record.History) > historyMaxCeiling {
		return errors.New("invalid model route selection record")
	}
	for _, ref := range append([]revisionRef{record.Current}, record.History...) {
		if !validDigest(ref.RevisionDigest) || strings.TrimSpace(ref.Name) == "" {
			return errors.New("invalid model route selection revision")
		}
		if _, err := time.Parse(time.RFC3339Nano, ref.CreatedAt); err != nil {
			return errors.New("invalid model route selection time")
		}
	}
	if record.MigratedAt != "" {
		if _, err := time.Parse(time.RFC3339Nano, record.MigratedAt); err != nil {
			return errors.New("invalid model route migration time")
		}
	}
	token, err := selectionStateToken(record)
	if err != nil || token != record.StateToken {
		return errors.New("model route selection state token mismatch")
	}
	return nil
}

func selectionStateToken(record selectionRecord) (string, error) {
	record.StateToken = ""
	body, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	return framedDigest(selectionTokenFrame, body), nil
}

// withMutationLock runs apply under the one route mutation lock, keeping route
// problems and wrapping everything else as a storage problem.
func (owner *Owner) withMutationLock(apply func() error) error {
	if err := owner.ensureRoot(); err != nil {
		return storageProblem(err)
	}
	lock := filepath.Join(owner.root, "mutation.lock")
	if info, err := os.Lstat(lock); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return storageProblem(errors.New("model route mutation lock is not a regular file"))
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return storageProblem(err)
	}
	err := filelock.With(lock, 0o600, apply)
	if err != nil {
		var domain *Problem
		if errors.As(err, &domain) {
			return err
		}
		return storageProblem(err)
	}
	return nil
}

func publicProblem(err error) Problem {
	var source *Problem
	if errors.As(err, &source) {
		return Problem{Code: source.Code, Field: source.Field, Message: source.Message, Recovery: source.Recovery}
	}
	return Problem{Code: CodeStorage, Message: "Model route storage is unavailable.",
		Recovery: "Review the local model route storage and retry."}
}

func digestKey(digest string) string { return strings.Replace(digest, ":", "-", 1) }
