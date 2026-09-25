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
		view, err := decodeSessionView(entry, sessionQueryLimits(limits))
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
func decodeSessionView(entry json.RawMessage, limits sessionquery.Limits) (SavedSessionView, error) {
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
	return view, validateSessionView(view, limits)
}

func validateSessionView(view SavedSessionView, limits sessionquery.Limits) error {
	if view.ID == "" || strings.ContainsAny(view.ID, "/\\ \t\n") {
		return fmt.Errorf("%w: it needs an id without spaces or slashes", errSessionViewInvalid)
	}
	name := strings.TrimSpace(view.Name)
	if name == "" || len(name) > sessionViewNameMax {
		return fmt.Errorf("%w: it needs a name of at most %d bytes", errSessionViewInvalid, sessionViewNameMax)
	}
	if _, err := sessionquery.Parse(view.Query, limits); err != nil {
		return err
	}
	if _, err := sessionquery.ParseGroupBy(view.GroupBy); err != nil {
		return err
	}
	_, err := sessionquery.ParseSort(view.Sort)
	return err
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
		}
		for _, view := range next {
			if err := validateSessionView(view, sessionQueryLimits(limits)); err != nil {
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
