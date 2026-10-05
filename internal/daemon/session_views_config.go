package daemon

// Saved session views are the owner's configuration (ADR 0026: "authored
// meaning, selection, threshold, workflow, view, or consequence"), held in one
// file in the data directory:
//
//	<dataDir>/session-views.json
//	{"format_version":1,"views":[{"id","name","query","group_by","sort"}]}
//
// The console writes this file when the owner saves, renames, reorders or
// deletes a view, so nothing has to be written by hand; and because it is a
// plain file it can be read, edited, copied to another machine or versioned.
//
// The framework contributes the mechanism and no opinion: there is no built-in
// view, no embedded view file and no example view. An absent file is an empty
// list. session_organization_boundary_test.go fails the build if that ever changes.
//
// One ordered file, not one file per view: order is the owner's, and a reorder
// must be atomic.

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"crossing-guard/internal/filelock"
	"crossing-guard/internal/sessionquery"
	"crossing-guard/store"
)

const (
	sessionViewsFormatVersion = 1
	sessionViewNameMax        = 120
	sessionViewsOriginNone    = "none"
)

// SavedSessionView is one saved view as written in the file.
type SavedSessionView struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Query   string `json:"query"`
	GroupBy string `json:"group_by,omitempty"`
	Sort    string `json:"sort,omitempty"`
	// RecordKind selects what the view lists: "sessions" (the default and the
	// only kind before memory views existed) or "memory" (memory-first-class-
	// records plan §6: the same file, the same grammar, one more record kind).
	// The empty string means sessions — additive, so an existing file is valid
	// untouched.
	RecordKind string `json:"record_kind,omitempty"`
	// Board turns the view into the kanban rendering (sessions-board plan
	// rev 3): cards grouped by the view's own `group_by` — which MUST be
	// `tag-key:<key>` for a board view (one grouping truth, RT-B2/N1) — with
	// the owner's declared column order and empty-column visibility. The
	// grouping derives from GroupBy; the board config never names a second
	// group field.
	Board *SavedViewBoard `json:"board,omitempty"`
}

// SavedViewBoard is one view's board configuration. Column names are the
// owner's vocabulary (no compiled columns — the flows precedent); the
// framework renders whatever tag values exist, in this order first.
type SavedViewBoard struct {
	// Columns is the declared column order. A value observed in the data but
	// not declared renders after the declared ones; declared columns render
	// even when empty when EmptyColumns is set.
	Columns []string `json:"columns,omitempty"`
	// EmptyColumns renders declared columns that currently hold no sessions.
	EmptyColumns bool `json:"empty_columns,omitempty"`
	// Placement are the owner's rules for sessions he has not placed himself
	// (board-observed-columns plan §2): a session with no owner tag under the
	// board's key is in the column of the first rule, in this order, whose
	// query it matches. A rule writes nothing.
	Placement []SavedViewPlacement `json:"placement,omitempty"`
}

// SavedViewPlacement is one placement rule: a declared column and a query in
// the session grammar.
type SavedViewPlacement struct {
	Column string `json:"column"`
	Query  string `json:"query"`
}

// sessionViewBoardLimits are the owner's budgets for one board view.
type sessionViewBoardLimits struct{ columns, rules int }

func sessionViewBoardBudgets(o ConsoleSessionOrganization) sessionViewBoardLimits {
	return sessionViewBoardLimits{columns: o.BoardColumnsMax, rules: o.BoardPlacementRulesMax}
}

// SavedSessionViewRejection names an entry that could not be used, in words fit to
// show the owner next to the view's name.
type SavedSessionViewRejection struct {
	Index   int    `json:"index"`
	Name    string `json:"name,omitempty"`
	Problem string `json:"problem"`
}

// sessionViewsDocument is what one read of the file yields.
type sessionViewsDocument struct {
	Views    []SavedSessionView          `json:"views"`
	Rejected []SavedSessionViewRejection `json:"rejected"`
	// Origin is the file's path, or "none" when the owner has saved no view.
	Origin string `json:"origin"`
	// StateToken names the exact bytes read. A write must present it, so a
	// second browser or an editor cannot have its change silently overwritten.
	StateToken string `json:"state_token"`
}

var (
	errSessionViewsStale    = errors.New("the saved views changed since they were loaded")
	errSessionViewsRejected = errors.New("session-views.json has an entry that cannot be read; fix or remove it before saving from the console")
	errSessionViewInvalid   = errors.New("view is invalid")
	errSessionViewNotFound  = errors.New("no such view")
)

func sessionViewsPath(dataDir string) string { return filepath.Join(dataDir, "session-views.json") }

// loadSessionViews reads the file on every call. It is small, and caching it
// would either miss a hand edit or latch onto one caught half-written.
func loadSessionViews(dataDir string, limits ConsoleSessionOrganization) sessionViewsDocument {
	document := sessionViewsDocument{Views: []SavedSessionView{}, Rejected: []SavedSessionViewRejection{}, Origin: sessionViewsOriginNone}
	path := sessionViewsPath(dataDir)
	raw, err := readBoundedFile(path, limits.ViewsFileBytesMax)
	if errors.Is(err, os.ErrNotExist) {
		document.StateToken = sessionViewsToken(nil)
		return document
	}
	document.Origin = path
	if err != nil {
		document.Rejected = append(document.Rejected, SavedSessionViewRejection{Index: -1, Problem: err.Error()})
		document.StateToken = sessionViewsToken(nil)
		return document
	}
	document.StateToken = sessionViewsToken(raw)
	var file struct {
		FormatVersion int               `json:"format_version"`
		Views         []json.RawMessage `json:"views"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		document.Rejected = append(document.Rejected, SavedSessionViewRejection{Index: -1, Problem: "the file cannot be read: " + err.Error()})
		return document
	}
	// Anything after the document would be silently erased by the next save.
	if decoder.More() {
		document.Rejected = append(document.Rejected, SavedSessionViewRejection{Index: -1, Problem: "the file has text after its closing brace"})
		return document
	}
	if file.FormatVersion != sessionViewsFormatVersion {
		document.Rejected = append(document.Rejected, SavedSessionViewRejection{Index: -1,
			Problem: fmt.Sprintf("format_version %d is not supported", file.FormatVersion)})
		return document
	}
	seen := map[string]bool{}
	for index, entry := range file.Views {
		view, err := decodeSessionView(entry, sessionQueryLimits(limits), sessionViewBoardBudgets(limits))
		if err == nil && seen[view.ID] {
			err = fmt.Errorf("the id %q is used twice", view.ID)
		}
		if err == nil && len(document.Views) >= limits.ViewsMax {
			err = fmt.Errorf("more than %d views", limits.ViewsMax)
		}
		if err != nil {
			document.Rejected = append(document.Rejected, SavedSessionViewRejection{Index: index, Name: view.Name, Problem: err.Error()})
			continue
		}
		seen[view.ID] = true
		document.Views = append(document.Views, view)
	}
	return document
}

func readBoundedFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("the file is larger than %d bytes", limit)
	}
	return raw, nil
}

func sessionViewsToken(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

// decodeSessionView reads one entry strictly. The partly decoded view comes
// back even on failure so a rejection can carry the name the owner gave it.
func decodeSessionView(entry json.RawMessage, limits sessionquery.Limits, board sessionViewBoardLimits) (SavedSessionView, error) {
	var view SavedSessionView
	decoder := json.NewDecoder(bytes.NewReader(entry))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&view); err != nil {
		var loose struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(entry, &loose)
		return SavedSessionView{Name: loose.Name}, errors.New("the entry has a field this version does not know, or the wrong type")
	}
	return view, validateSessionView(view, limits, board)
}

func validateSessionView(view SavedSessionView, limits sessionquery.Limits, board sessionViewBoardLimits) error {
	if view.ID == "" || strings.ContainsAny(view.ID, "/\\ \t\n") {
		return fmt.Errorf("%w: it needs an id without spaces or slashes", errSessionViewInvalid)
	}
	switch view.RecordKind {
	case "", sessionViewRecordKindSessions:
		// the default kind; nothing more to check
	case sessionViewRecordKindMemory:
		// a memory view: valid by shape; the query grammar is the same one
	default:
		return fmt.Errorf("%w: record_kind %q is not one of sessions|memory", errSessionViewInvalid, view.RecordKind)
	}
	name := strings.TrimSpace(view.Name)
	if name == "" || len(name) > sessionViewNameMax {
		return fmt.Errorf("%w: it needs a name of at most %d bytes", errSessionViewInvalid, sessionViewNameMax)
	}
	if _, err := sessionquery.Parse(view.Query, limits); err != nil {
		return err
	}
	groupBy, err := sessionquery.ParseGroupBy(view.GroupBy)
	if err != nil {
		return err
	}
	if _, err := sessionquery.ParseSort(view.Sort); err != nil {
		return err
	}
	if view.Board != nil {
		return validateSessionViewBoard(*view.Board, groupBy, limits, board)
	}
	return nil
}

// A view's record kind: sessions (also the empty default) or memory records.
const (
	sessionViewRecordKindSessions = "sessions"
	sessionViewRecordKindMemory   = "memory"
)

// validateSessionViewBoard holds a board to its rules (sessions-board plan
// §3 and board-observed-columns plan §2.1): one grouping truth, a tag key the
// owner's moves can be written under; column names that are the owner's
// vocabulary, storable as that key's values, each naming one group; placement
// rules that name a declared column and can be decided the same way by every
// read of the board.
func validateSessionViewBoard(config SavedViewBoard, groupBy sessionquery.GroupBy, limits sessionquery.Limits, budgets sessionViewBoardLimits) error {
	if groupBy.Kind != sessionquery.GroupTagKey || groupBy.Key == "" {
		return fmt.Errorf("%w: a board view needs group_by=tag-key:<key>", errSessionViewInvalid)
	}
	if len(config.Columns) > budgets.columns {
		return fmt.Errorf("%w: a board declares at most %d columns", errSessionViewInvalid, budgets.columns)
	}
	// A move is written as an owner tag under the board's key, so the key
	// must be one a tag can have, whether or not a column is declared yet.
	if err := store.ValidateSessionOwnerTag(store.SessionOwnerTagValue{Key: groupBy.Key, Value: groupBy.Key}); err != nil {
		return fmt.Errorf("%w: the board's key %q cannot be a tag key: %v", errSessionViewInvalid, groupBy.Key, err)
	}
	// Columns that read the same group are one column: a tag group's key
	// ignores letter case (board-column-letter-case plan §2).
	columnByGroup := map[string]string{}
	for _, column := range config.Columns {
		column = strings.TrimSpace(column)
		if column == "" {
			return fmt.Errorf("%w: a board column needs a name", errSessionViewInvalid)
		}
		// A move writes the column as an owner tag under the board's key, so
		// both must be storable as one.
		if err := store.ValidateSessionOwnerTag(store.SessionOwnerTagValue{Key: groupBy.Key, Value: column}); err != nil {
			return fmt.Errorf("%w: board column %q cannot be a tag: %v", errSessionViewInvalid, column, err)
		}
		group := sessionGroupKey(groupBy, column)
		if twin, found := columnByGroup[group]; found {
			if twin == column {
				return fmt.Errorf("%w: board column %q is declared twice", errSessionViewInvalid, column)
			}
			return fmt.Errorf("%w: board columns %q and %q differ only by letter case, so they are one column; keep one", errSessionViewInvalid, twin, column)
		}
		columnByGroup[group] = column
	}
	if len(config.Placement) > budgets.rules {
		return fmt.Errorf("%w: a board has at most %d placement rules", errSessionViewInvalid, budgets.rules)
	}
	for index, rule := range config.Placement {
		if err := validateBoardPlacementRule(rule, groupBy, columnByGroup, limits); err != nil {
			return fmt.Errorf("%w: placement rule %d (%s): %v", errSessionViewInvalid, index+1, strings.TrimSpace(rule.Column), err)
		}
	}
	return nil
}

// validateBoardPlacementRule refuses a rule the board's reads could not all
// decide alike. Search words are ranked and capped; status: and open: depend
// on the open-session observation, which a column read of a durable view
// does not take. A rule that names the board's own key could only match a
// session the owner placed, and those never reach the rules.
func validateBoardPlacementRule(rule SavedViewPlacement, groupBy sessionquery.GroupBy, columnByGroup map[string]string, limits sessionquery.Limits) error {
	if _, declared := columnByGroup[sessionGroupKey(groupBy, strings.TrimSpace(rule.Column))]; !declared {
		return errors.New("it names a column the board does not declare")
	}
	query, err := sessionquery.Parse(rule.Query, limits)
	if err != nil {
		return err
	}
	if query.Empty() {
		return errors.New("it needs a query")
	}
	if !query.Durable() {
		return errors.New("a rule cannot use status:, open: or search words")
	}
	for _, key := range query.TagKeys() {
		if key == groupBy.Key {
			return fmt.Errorf("it names %q, the key this board writes moves under", groupBy.Key)
		}
	}
	return nil
}

// mutateSessionViews is every write. Under the lock it re-reads the file from
// disk — never a list some request loaded earlier — refuses if the caller's
// token is stale or if the file holds an entry it could not read (saving would
// erase the owner's half-made edit), applies the change, validates the whole
// list and replaces the file atomically.
func mutateSessionViews(dataDir string, limits ConsoleSessionOrganization, expectedToken string, change func([]SavedSessionView) ([]SavedSessionView, error)) (sessionViewsDocument, error) {
	path := sessionViewsPath(dataDir)
	var result sessionViewsDocument
	err := filelock.With(path+".lock", 0o600, func() error {
		current := loadSessionViews(dataDir, limits)
		if len(current.Rejected) > 0 {
			return errSessionViewsRejected
		}
		if expectedToken != current.StateToken {
			return errSessionViewsStale
		}
		next, err := change(append([]SavedSessionView(nil), current.Views...))
		if err != nil {
			return err
		}
		if len(next) > limits.ViewsMax {
			return fmt.Errorf("%w: at most %d views", errSessionViewInvalid, limits.ViewsMax)
		}
		seen := map[string]bool{}
		for index := range next {
			next[index].Name = strings.TrimSpace(next[index].Name)
			if board := next[index].Board; board != nil {
				// A column is stored as it is validated and rendered: trimmed.
				trimmed := *board
				trimmed.Columns = make([]string, len(board.Columns))
				for at, column := range board.Columns {
					trimmed.Columns[at] = strings.TrimSpace(column)
				}
				trimmed.Placement = make([]SavedViewPlacement, len(board.Placement))
				for at, rule := range board.Placement {
					trimmed.Placement[at] = SavedViewPlacement{Column: strings.TrimSpace(rule.Column), Query: strings.TrimSpace(rule.Query)}
				}
				if len(trimmed.Placement) == 0 {
					trimmed.Placement = nil
				}
				next[index].Board = &trimmed
			}
		}
		for _, view := range next {
			if err := validateSessionView(view, sessionQueryLimits(limits), sessionViewBoardBudgets(limits)); err != nil {
				return err
			}
			if seen[view.ID] {
				return fmt.Errorf("%w: the id %q is used twice", errSessionViewInvalid, view.ID)
			}
			seen[view.ID] = true
		}
		if err := writeSessionViews(path, next, limits.ViewsFileBytesMax); err != nil {
			return err
		}
		result = loadSessionViews(dataDir, limits)
		return nil
	})
	return result, err
}

// writeSessionViews refuses to write a file the next read would refuse: a save
// that succeeded and then showed no views would lock the owner out.
func writeSessionViews(path string, views []SavedSessionView, maxBytes int64) error {
	if views == nil {
		views = []SavedSessionView{}
	}
	body, err := json.MarshalIndent(struct {
		FormatVersion int                `json:"format_version"`
		Views         []SavedSessionView `json:"views"`
	}{sessionViewsFormatVersion, views}, "", "  ")
	if err != nil {
		return err
	}
	if int64(len(body))+1 > maxBytes {
		return fmt.Errorf("%w: the views file would be larger than %d bytes", errSessionViewInvalid, maxBytes)
	}
	temp := path + ".tmp"
	if err := os.WriteFile(temp, append(body, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(temp, path)
}

func newSessionViewID() (string, error) {
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "view_" + hex.EncodeToString(raw[:]), nil
}
