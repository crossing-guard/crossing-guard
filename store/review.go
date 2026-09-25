package store

import (
	"database/sql"
	"strings"
	"unicode/utf8"
)

const (
	defaultReviewPageLimit = 25
	maxReviewPageLimit     = 100
	maxStatementSnippet    = 320
	maxReviewResultLinks   = 200
)

// SessionStatement is one storage-sanitized row from the rebuildable transcript index.
// Storage sanitization removes invalid/control bytes and bounds text; it is not a claim
// of secret redaction. The row is display evidence only: search_document lineage does
// not turn rebuildable conversation text into a governance fact.
type SessionStatement struct {
	Runtime          string `json:"runtime"`
	SessionID        string `json:"session_id"`
	ObservedAt       string `json:"observed_at,omitempty"`
	Role             string `json:"role"`
	Snippet          string `json:"snippet"`
	SnippetTruncated bool   `json:"snippet_truncated"`
	SourceState      string `json:"source_state"`
}

// SessionStatementPage is a bounded projection with an exact index-row total.
type SessionStatementPage struct {
	Statements []SessionStatement `json:"statements"`
	Total      int                `json:"total"`
	Offset     int                `json:"offset"`
	Limit      int                `json:"limit"`
}

func reviewPage(offset, limit int) (int, int) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 || limit > maxReviewPageLimit {
		limit = defaultReviewPageLimit
	}
	return offset, limit
}

func statementSnippet(text string) (string, bool) {
	if len(text) <= maxStatementSnippet {
		return text, false
	}
	cut := maxStatementSnippet
	for cut > 0 && !utf8.ValidString(text[:cut]) {
		cut--
	}
	return strings.TrimSpace(text[:cut]) + "…", true
}

// SessionStatements returns only user/assistant rows for one composite runtime/native
// session identity. Total is exact for the rebuildable storage-sanitized projection, not a claim
// that the native transcript itself was complete.
func (ix *Index) SessionStatements(runtime, sessionID string, offset, limit int) (SessionStatementPage, error) {
	offset, limit = reviewPage(offset, limit)
	out := SessionStatementPage{Statements: []SessionStatement{}, Offset: offset, Limit: limit}
	if runtime == "" || sessionID == "" {
		return out, nil
	}
	where := `owner_kind='transcript' AND vendor=? AND session_id=? AND kind IN ('user','assistant')`
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM search_document WHERE `+where, runtime, sessionID).
		Scan(&out.Total); err != nil {
		return out, err
	}
	rows, err := ix.db.Query(`SELECT vendor,session_id,ts,kind,text FROM search_document
		WHERE `+where+` ORDER BY document_order,id LIMIT ? OFFSET ?`,
		runtime, sessionID, limit, offset)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var statement SessionStatement
		var text string
		if err := rows.Scan(&statement.Runtime, &statement.SessionID, &statement.ObservedAt,
			&statement.Role, &text); err != nil {
			return out, err
		}
		statement.Snippet, statement.SnippetTruncated = statementSnippet(text)
		statement.SourceState = "rebuildable-storage-sanitized-index"
		out.Statements = append(out.Statements, statement)
	}
	return out, rows.Err()
}

// EventTagValue identifies an exact frozen detector tag value. Queries never rerun a
// detector over historical input.
type EventTagValue struct {
	Key   string
	Value string
}

// TaggedEventPage is a bounded event page with an exact matching-event total.
type TaggedEventPage struct {
	Events []EventRecord
	Total  int
	Offset int
	Limit  int
}

// TaggedEventsForSession returns a bounded event page matching any supplied exact
// frozen tag. Malformed/legacy tag JSON is retained by the event log but does not match.
func (ix *Index) TaggedEventsForSession(sessionID string, wanted []EventTagValue, offset, limit int) (TaggedEventPage, error) {
	offset, limit = reviewPage(offset, limit)
	out := TaggedEventPage{Events: []EventRecord{}, Offset: offset, Limit: limit}
	if sessionID == "" || len(wanted) == 0 {
		return out, nil
	}
	predicates := make([]string, 0, len(wanted))
	args := []any{sessionID}
	for _, tag := range wanted {
		predicates = append(predicates, `(json_extract(j.value,'$.key')=? AND json_extract(j.value,'$.value')=?)`)
		args = append(args, tag.Key, tag.Value)
	}
	where := `e.session_id=? AND EXISTS (SELECT 1 FROM json_each(
		CASE WHEN json_valid(e.tags) THEN e.tags ELSE '[]' END) j WHERE ` +
		strings.Join(predicates, " OR ") + `)`
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM event e WHERE `+where, args...).Scan(&out.Total); err != nil {
		return out, err
	}
	queryArgs := append(append([]any{}, args...), limit, offset)
	events, err := scanEvents(ix.db.Query(`SELECT `+eventCols+` FROM event e WHERE `+where+
		` ORDER BY e.ts,e.id LIMIT ? OFFSET ?`, queryArgs...))
	if err != nil {
		return out, err
	}
	out.Events = events
	return out, nil
}

// EventResultLink is bounded result metadata related to one action by the current
// reconciliation record. Selected distinguishes exact/indirect joins from ambiguous
// candidates; no timestamp-nearest inference is performed.
type EventResultLink struct {
	EventID       int64  `json:"event_id"`
	ResultID      int64  `json:"result_id"`
	JoinClass     string `json:"join_class"`
	Selected      bool   `json:"selected"`
	State         string `json:"state"`
	DurationMS    int64  `json:"duration_ms,omitempty"`
	RawBytes      int    `json:"raw_bytes"`
	RetainedBytes int    `json:"retained_bytes"`
	StdoutBytes   int    `json:"stdout_bytes"`
	StderrBytes   int    `json:"stderr_bytes"`
	Completeness  string `json:"completeness"`
}

// ResultLinksForEvents returns current reconciliation facts for a bounded event set.
func (ix *Index) ResultLinksForEvents(eventIDs []int64) ([]EventResultLink, int, error) {
	if len(eventIDs) == 0 {
		return []EventResultLink{}, 0, nil
	}
	if len(eventIDs) > maxReviewPageLimit {
		eventIDs = eventIDs[:maxReviewPageLimit]
	}
	current, args := resultLinkQuery(eventIDs)
	var total int
	if err := ix.db.QueryRow(current+` SELECT COUNT(*) FROM matched`, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := ix.db.Query(current+` SELECT event_id,result_id,join_class,selected,state,duration_ms,
		raw_bytes,retained_bytes,stdout_bytes,stderr_bytes,completeness FROM matched
		ORDER BY event_id,result_id LIMIT ?`, append(args, maxReviewResultLinks)...)
	if err != nil {
		return nil, 0, err
	}
	out, err := scanEventResultLinks(rows)
	return out, total, err
}

func resultLinkQuery(eventIDs []int64) (string, []any) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(eventIDs)), ",")
	args := make([]any, 0, len(eventIDs))
	for _, eventID := range eventIDs {
		args = append(args, eventID)
	}
	return `WITH current AS (
		SELECT r.id,r.result_id,r.join_class FROM result_reconciliation r
		WHERE r.id=(SELECT MAX(r2.id) FROM result_reconciliation r2 WHERE r2.result_id=r.result_id)
	), matched AS (
		SELECT c.event_id,o.id result_id,current.join_class,c.selected,o.state,o.duration_ms,
			o.raw_bytes,o.retained_bytes,o.stdout_bytes,o.stderr_bytes,o.completeness
		FROM current JOIN result_observation o ON o.id=current.result_id
		JOIN result_reconciliation_candidate c ON c.reconciliation_id=current.id
		WHERE c.event_id IN (` + placeholders + `)
	)`, args
}

func scanEventResultLinks(rows *sql.Rows) ([]EventResultLink, error) {
	defer rows.Close()
	out := []EventResultLink{}
	for rows.Next() {
		var link EventResultLink
		var selected int
		if err := rows.Scan(&link.EventID, &link.ResultID, &link.JoinClass, &selected,
			&link.State, &link.DurationMS, &link.RawBytes, &link.RetainedBytes,
			&link.StdoutBytes, &link.StderrBytes, &link.Completeness); err != nil {
			return nil, err
		}
		link.Selected = selected == 1
		out = append(out, link)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
