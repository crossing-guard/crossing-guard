package changeenv

// The session edit set (workspace-panes implementation plan §4.2, owner decision
// O-D): what this session's runtime reported changing, as recorded before/after
// pairs, written contents, or runtime-supplied diffs — grouped by file, in the
// order they completed. No line positions exist for these bodies, so an edit is
// shown as an unanchored pair, never as a hunk with numbers.

import (
	"strings"

	"crossing-guard/store"
)

// SessionEdit is one recorded edit, bodies described by kind and size.
type SessionEdit struct {
	ResultID     int64  `json:"result_id"`
	Ordinal      int    `json:"ordinal"`
	EventID      int64  `json:"event_id,omitempty"`
	CompletedAt  int64  `json:"completed_at"`
	Operation    string `json:"operation"`
	Kind         string `json:"kind"`
	ReplaceAll   bool   `json:"replace_all"`
	BeforeBytes  int    `json:"before_bytes"`
	AfterBytes   int    `json:"after_bytes"`
	ContentBytes int    `json:"content_bytes"`
	DiffBytes    int    `json:"diff_bytes"`
	Tool         string `json:"tool,omitempty"`
}

// SessionEditFile groups a path's edits. DisplayPath is the path relative to
// the session's recorded working directory when it lies under it, otherwise the
// path as recorded; Path is always the recorded identity.
type SessionEditFile struct {
	Path        string        `json:"path"`
	DisplayPath string        `json:"display_path"`
	Edits       []SessionEdit `json:"edits"`
}

// SessionEdits is one page of the session's edits, grouped by file.
type SessionEdits struct {
	SessionID string            `json:"session_id"`
	Total     int               `json:"total"`
	Offset    int               `json:"offset"`
	Limit     int               `json:"limit"`
	Returned  int               `json:"returned"`
	Files     []SessionEditFile `json:"files"`
}

// BuildSessionEdits projects the store's edit rows into files. It orders inputs
// and delegates; bytes and SQL stay with their owners.
func BuildSessionEdits(ix *store.Index, sessionID string, limit, offset int) (SessionEdits, error) {
	rows, total, err := ix.SessionEditRows(sessionID, limit, offset)
	if err != nil {
		return SessionEdits{}, err
	}
	out := SessionEdits{SessionID: sessionID, Total: total, Offset: offset, Limit: limit, Returned: len(rows), Files: []SessionEditFile{}}
	cwd := sessionWorkingDirectory(ix, sessionID)
	index := map[string]int{}
	for _, row := range rows {
		position, seen := index[row.Path]
		if !seen {
			position = len(out.Files)
			index[row.Path] = position
			out.Files = append(out.Files, SessionEditFile{Path: row.Path, DisplayPath: displayPath(row.Path, cwd), Edits: []SessionEdit{}})
		}
		out.Files[position].Edits = append(out.Files[position].Edits, SessionEdit{
			ResultID: row.ResultID, Ordinal: row.Ordinal, EventID: row.EventID, CompletedAt: row.CompletedAt,
			Operation: row.Operation, Kind: row.Kind, ReplaceAll: row.ReplaceAll,
			BeforeBytes: row.BeforeBytes, AfterBytes: row.AfterBytes, ContentBytes: row.ContentBytes, DiffBytes: row.DiffBytes,
			Tool: row.Tool,
		})
	}
	return out, nil
}

// sessionWorkingDirectory reads the recorded cwd. An unknown session and a
// store error both yield "", which leaves every path as recorded; when two
// runtimes recorded the same id, SessionByID's own choice (the most recently
// modified row) decides which cwd is stripped.
func sessionWorkingDirectory(ix *store.Index, sessionID string) string {
	row, found, err := ix.SessionByID("", sessionID)
	if err != nil || !found {
		return ""
	}
	return row.CWD
}

// displayPath strips the working directory prefix for reading; it never
// resolves, joins, or invents a path.
func displayPath(path, cwd string) string {
	if cwd == "" {
		return path
	}
	prefix := strings.TrimSuffix(cwd, "/") + "/"
	if strings.HasPrefix(path, prefix) {
		return strings.TrimPrefix(path, prefix)
	}
	return path
}
