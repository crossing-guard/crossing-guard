package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ncruces/go-sqlite3"
)

const searchIndexTriggersV22 = `
CREATE TRIGGER IF NOT EXISTS search_document_ai AFTER INSERT ON search_document BEGIN
  INSERT INTO events_fts(rowid,vendor,session_id,ts,kind,text)
    VALUES(new.id,new.vendor,new.session_id,new.ts,new.kind,new.text);
END;
CREATE TRIGGER IF NOT EXISTS search_document_ad AFTER DELETE ON search_document BEGIN
  INSERT INTO events_fts(events_fts,rowid,vendor,session_id,ts,kind,text)
    VALUES('delete',old.id,old.vendor,old.session_id,old.ts,old.kind,old.text);
END;
CREATE TRIGGER IF NOT EXISTS search_document_au AFTER UPDATE ON search_document BEGIN
  INSERT INTO events_fts(events_fts,rowid,vendor,session_id,ts,kind,text)
    VALUES('delete',old.id,old.vendor,old.session_id,old.ts,old.kind,old.text);
  INSERT INTO events_fts(rowid,vendor,session_id,ts,kind,text)
    VALUES(new.id,new.vendor,new.session_id,new.ts,new.kind,new.text);
END;`

const externalEventsFTSBodyV22 = `(
  vendor,session_id,ts,kind,text,
  content='search_document',content_rowid='id'
)`

const externalEventsFTSV22 = `CREATE VIRTUAL TABLE events_fts USING fts5` + externalEventsFTSBodyV22

const searchIndexSchemaV22 = `
CREATE TABLE IF NOT EXISTS search_document(
  id INTEGER PRIMARY KEY,
  owner_kind TEXT NOT NULL CHECK(owner_kind IN ('transcript','memory')),
  vendor TEXT NOT NULL,
  session_id TEXT NOT NULL,
  document_order INTEGER NOT NULL CHECK(document_order >= 0),
  ts TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL,
  text TEXT NOT NULL,
  lineage TEXT NOT NULL DEFAULT '',
  UNIQUE(owner_kind,vendor,session_id,document_order)
);
CREATE INDEX IF NOT EXISTS search_document_owner
  ON search_document(owner_kind,vendor,session_id,id);
CREATE VIRTUAL TABLE IF NOT EXISTS events_fts USING fts5` + externalEventsFTSBodyV22 + `;
` + searchIndexTriggersV22 + `
CREATE TABLE IF NOT EXISTS transcript_index_projection(
  runtime TEXT NOT NULL,
  session_id TEXT NOT NULL,
  generation_marker TEXT NOT NULL DEFAULT '',
  indexed_at INTEGER NOT NULL DEFAULT 0,
  source_count INTEGER NOT NULL DEFAULT 0 CHECK(source_count >= 0),
  document_count INTEGER NOT NULL DEFAULT 0 CHECK(document_count >= 0),
  last_attempted_at INTEGER NOT NULL DEFAULT 0,
  limitation TEXT NOT NULL DEFAULT '' CHECK(limitation='' OR json_valid(limitation)),
  PRIMARY KEY(runtime,session_id)
);
`

var ErrTranscriptProjectionChanged = errors.New("transcript projection changed after planning")

// IsBusyError keeps the selected SQLite driver's contention vocabulary inside the
// persistence package while allowing concrete application adapters to classify retryable
// repository failures.
func IsBusyError(err error) bool { return errors.Is(err, sqlite3.BUSY) }

// searchIndexReplacementTestHook is nil in production. Focused store tests use it to
// prove a fault after content/session writes but before the projection marker rolls the
// complete canonical-session transaction back.
var searchIndexReplacementTestHook func(searchDocumentExecutor) error

type SessionRow struct {
	Vendor    string `json:"vendor"`
	ID        string `json:"id"`
	CatalogID string `json:"catalog_id,omitempty"`
	ResumeID  string `json:"resume_id,omitempty"`
	Path      string `json:"path"`
	CWD       string `json:"cwd"`
	Project   string `json:"project"`
	Title     string `json:"title"`
	Modified  int64  `json:"modified"`
	Turns     int    `json:"turns"`
}

type SearchDocument struct {
	Order     int
	Timestamp string
	Kind      string
	Text      string
	Lineage   string
}

type EventHit struct {
	Vendor    string `json:"vendor"`
	SessionID string `json:"session_id"`
	CatalogID string `json:"catalog_id,omitempty"`
	ResumeID  string `json:"resume_id,omitempty"`
	TS        string `json:"ts"`
	Kind      string `json:"kind"`
	Text      string `json:"text"`
	Path      string `json:"path,omitempty"`
	CWD       string `json:"cwd,omitempty"`
	Project   string `json:"project,omitempty"`
	Title     string `json:"title,omitempty"`
	Modified  int64  `json:"modified,omitempty"`
	Turns     int    `json:"turns,omitempty"`
}

type TranscriptProjectionKey struct {
	Runtime   string
	SessionID string
}

type TranscriptProjectionLimitation struct {
	Kind          string `json:"kind"`
	Runtime       string `json:"runtime,omitempty"`
	SessionID     string `json:"session_id,omitempty"`
	ObservedBytes int64  `json:"observed_bytes,omitempty"`
	LimitBytes    int64  `json:"limit_bytes,omitempty"`
	ObservedCount int    `json:"observed_count,omitempty"`
	LimitCount    int    `json:"limit_count,omitempty"`
}

var transcriptProjectionLimitationKinds = map[string]struct{}{
	"unsupported-adapter":    {},
	"discovery-incomplete":   {},
	"source-too-large":       {},
	"source-unreadable":      {},
	"source-mutated":         {},
	"read-cancelled":         {},
	"document-limit":         {},
	"text-limit":             {},
	"repository-busy":        {},
	"repository-unavailable": {},
	"invalid-projection":     {},
}

type TranscriptProjectionState struct {
	Key             TranscriptProjectionKey
	Generation      string
	IndexedAt       time.Time
	SourceCount     int
	DocumentCount   int
	LastAttemptedAt time.Time
	Limitation      *TranscriptProjectionLimitation
}

type TranscriptProjection struct {
	Session            SessionRow
	ExpectedGeneration string
	ExpectedIndexedAt  time.Time
	Generation         string
	IndexedAt          time.Time
	SourceCount        int
	Documents          []SearchDocument
}

type TranscriptProjectionReplaceResult struct {
	State    TranscriptProjectionState
	Replaced bool
}

func migrateSearchIndexV22(db schemaDB, version int) error {
	sessionColumns, err := columnSet(db, "sessions")
	if err != nil {
		return fmt.Errorf("inspect session identity v22: %w", err)
	}
	if !sessionColumns["catalog_id"] {
		if _, err := db.Exec(`ALTER TABLE sessions ADD COLUMN catalog_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add catalog session identity v22: %w", err)
		}
	}
	if !sessionColumns["resume_id"] {
		if _, err := db.Exec(`ALTER TABLE sessions ADD COLUMN resume_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add resume session identity v22: %w", err)
		}
	}
	var definition string
	if err := db.QueryRow(`SELECT COALESCE(sql,'') FROM sqlite_schema
		WHERE type='table' AND name='events_fts'`).Scan(&definition); err != nil {
		return fmt.Errorf("inspect events FTS v22: %w", err)
	}
	if strings.Contains(strings.ToLower(definition), "content='search_document'") ||
		strings.Contains(strings.ToLower(definition), `content="search_document"`) {
		if version >= 22 {
			return nil
		}
		return validateSearchIndexV22(db)
	}

	if _, err := db.Exec(`DROP TRIGGER IF EXISTS search_document_ai;
		DROP TRIGGER IF EXISTS search_document_ad;
		DROP TRIGGER IF EXISTS search_document_au;
		DELETE FROM search_document;
		INSERT INTO search_document(id,owner_kind,vendor,session_id,document_order,ts,kind,text,lineage)
		SELECT rowid,CASE WHEN vendor='memory' THEN 'memory' ELSE 'transcript' END,
			vendor,session_id,rowid,ts,kind,text,'' FROM events_fts ORDER BY rowid;
		DROP TABLE events_fts;`); err != nil {
		return fmt.Errorf("copy legacy search content v22: %w", err)
	}
	if _, err := db.Exec(externalEventsFTSV22); err != nil {
		return fmt.Errorf("create external events FTS v22: %w", err)
	}
	if _, err := db.Exec(searchIndexTriggersV22); err != nil {
		return fmt.Errorf("create search document triggers v22: %w", err)
	}
	if _, err := db.Exec(`INSERT INTO events_fts(events_fts) VALUES('rebuild')`); err != nil {
		return fmt.Errorf("rebuild external events FTS v22: %w", err)
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO transcript_index_projection(
		runtime,session_id,generation_marker,indexed_at,source_count,document_count,last_attempted_at,limitation)
		SELECT vendor,session_id,'',0,0,COUNT(*),0,'' FROM search_document
		WHERE owner_kind='transcript' GROUP BY vendor,session_id`); err != nil {
		return fmt.Errorf("initialize transcript projection state v22: %w", err)
	}
	return validateSearchIndexV22(db)
}

func validateSearchIndexV22(db schemaDB) error {
	if _, err := db.Exec(searchIndexTriggersV22); err != nil {
		return fmt.Errorf("ensure search document triggers v22: %w", err)
	}
	if _, err := db.Exec(`INSERT INTO events_fts(events_fts,rank) VALUES('integrity-check',1)`); err != nil {
		return fmt.Errorf("validate external events FTS v22: %w", err)
	}
	var documents, indexed int
	if err := db.QueryRow(`SELECT COUNT(*) FROM search_document`).Scan(&documents); err != nil {
		return err
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM events_fts`).Scan(&indexed); err != nil {
		return err
	}
	if documents != indexed {
		return fmt.Errorf("search index v22 content mismatch: documents=%d indexed=%d", documents, indexed)
	}
	return nil
}

func (ix *Index) TranscriptProjectionStates() ([]TranscriptProjectionState, error) {
	rows, err := ix.db.Query(`SELECT runtime,session_id,generation_marker,indexed_at,
		source_count,document_count,last_attempted_at,limitation
		FROM transcript_index_projection ORDER BY runtime,session_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TranscriptProjectionState{}
	for rows.Next() {
		state, scanErr := scanTranscriptProjectionState(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, state)
	}
	return out, rows.Err()
}

type projectionStateScanner interface {
	Scan(...any) error
}

func scanTranscriptProjectionState(row projectionStateScanner) (TranscriptProjectionState, error) {
	var state TranscriptProjectionState
	var indexedAt, lastAttemptedAt int64
	var limitationJSON string
	if err := row.Scan(&state.Key.Runtime, &state.Key.SessionID, &state.Generation, &indexedAt,
		&state.SourceCount, &state.DocumentCount, &lastAttemptedAt, &limitationJSON); err != nil {
		return TranscriptProjectionState{}, err
	}
	state.IndexedAt = unixMilliTime(indexedAt)
	state.LastAttemptedAt = unixMilliTime(lastAttemptedAt)
	if limitationJSON != "" {
		var limitation TranscriptProjectionLimitation
		if err := json.Unmarshal([]byte(limitationJSON), &limitation); err != nil || limitation.Kind == "" {
			return TranscriptProjectionState{}, fmt.Errorf("invalid stored transcript projection limitation")
		}
		if _, ok := transcriptProjectionLimitationKinds[limitation.Kind]; !ok {
			return TranscriptProjectionState{}, fmt.Errorf("unknown stored transcript projection limitation")
		}
		state.Limitation = &limitation
	}
	return state, nil
}

func unixMilliTime(value int64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	return time.UnixMilli(value).UTC()
}

func timeUnixMilli(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UTC().UnixMilli()
}

func (ix *Index) RecordTranscriptProjectionLimitation(key TranscriptProjectionKey,
	expectedGeneration string, expectedIndexedAt, attemptedAt time.Time,
	limitation TranscriptProjectionLimitation) (TranscriptProjectionState, error) {
	if key.Runtime == "" || key.SessionID == "" || limitation.Kind == "" {
		return TranscriptProjectionState{}, fmt.Errorf("transcript projection limitation identity is incomplete")
	}
	if _, ok := transcriptProjectionLimitationKinds[limitation.Kind]; !ok ||
		limitation.ObservedBytes < 0 || limitation.LimitBytes < 0 ||
		limitation.ObservedCount < 0 || limitation.LimitCount < 0 {
		return TranscriptProjectionState{}, fmt.Errorf("transcript projection limitation is invalid")
	}
	limitation.Runtime = key.Runtime
	limitation.SessionID = key.SessionID
	body, err := json.Marshal(limitation)
	if err != nil {
		return TranscriptProjectionState{}, err
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return TranscriptProjectionState{}, err
	}
	defer func() { _ = tx.Rollback() }()
	current, _, err := transcriptProjectionStateTx(tx, key)
	if err != nil {
		return TranscriptProjectionState{}, err
	}
	if current.Generation != expectedGeneration ||
		timeUnixMilli(current.IndexedAt) != timeUnixMilli(expectedIndexedAt) {
		return current, nil
	}
	_, err = tx.Exec(`INSERT INTO transcript_index_projection(
		runtime,session_id,generation_marker,indexed_at,source_count,document_count,last_attempted_at,limitation)
		VALUES(?,?,'',0,0,0,?,?) ON CONFLICT(runtime,session_id) DO UPDATE SET
		last_attempted_at=excluded.last_attempted_at,limitation=excluded.limitation`,
		key.Runtime, key.SessionID, timeUnixMilli(attemptedAt), string(body))
	if err != nil {
		return TranscriptProjectionState{}, err
	}
	state, err := scanTranscriptProjectionState(tx.QueryRow(`SELECT runtime,session_id,
		generation_marker,indexed_at,source_count,document_count,last_attempted_at,limitation
		FROM transcript_index_projection WHERE runtime=? AND session_id=?`, key.Runtime, key.SessionID))
	if err != nil {
		return TranscriptProjectionState{}, err
	}
	if err := tx.Commit(); err != nil {
		return TranscriptProjectionState{}, err
	}
	return state, nil
}

func (ix *Index) transcriptProjectionState(key TranscriptProjectionKey) (TranscriptProjectionState, error) {
	return scanTranscriptProjectionState(ix.db.QueryRow(`SELECT runtime,session_id,generation_marker,indexed_at,
		source_count,document_count,last_attempted_at,limitation FROM transcript_index_projection
		WHERE runtime=? AND session_id=?`, key.Runtime, key.SessionID))
}

func validateTranscriptProjection(projection TranscriptProjection) error {
	if projection.Session.Vendor == "" || projection.Session.ID == "" || projection.Generation == "" ||
		projection.IndexedAt.IsZero() || projection.SourceCount < 1 || len(projection.Documents) == 0 {
		return fmt.Errorf("transcript projection is incomplete")
	}
	titles := 0
	for index, document := range projection.Documents {
		if document.Order != index || document.Kind == "" {
			return fmt.Errorf("transcript projection document order is invalid")
		}
		if document.Kind == "title" {
			titles++
		}
	}
	if titles != 1 || projection.Documents[0].Kind != "title" {
		return fmt.Errorf("transcript projection title document is invalid")
	}
	return nil
}

func (ix *Index) ReplaceTranscriptProjection(projection TranscriptProjection) (
	TranscriptProjectionReplaceResult, error) {
	if err := validateTranscriptProjection(projection); err != nil {
		return TranscriptProjectionReplaceResult{}, err
	}
	key := TranscriptProjectionKey{Runtime: projection.Session.Vendor, SessionID: projection.Session.ID}
	tx, err := ix.db.Begin()
	if err != nil {
		return TranscriptProjectionReplaceResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	current, exists, err := transcriptProjectionStateTx(tx, key)
	if err != nil {
		return TranscriptProjectionReplaceResult{}, err
	}
	if current.Generation != projection.ExpectedGeneration ||
		timeUnixMilli(current.IndexedAt) != timeUnixMilli(projection.ExpectedIndexedAt) {
		if exists && current.Generation == projection.Generation {
			return TranscriptProjectionReplaceResult{State: current, Replaced: false}, nil
		}
		return TranscriptProjectionReplaceResult{}, ErrTranscriptProjectionChanged
	}

	if err := replaceOwnedSearchDocuments(tx, "transcript", key.Runtime, key.SessionID,
		projection.Documents); err != nil {
		return TranscriptProjectionReplaceResult{}, err
	}
	if _, err := tx.Exec(`INSERT INTO sessions(vendor,id,path,cwd,project,title,modified,turns,catalog_id,resume_id)
		VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(vendor,id) DO UPDATE SET
		path=excluded.path,cwd=excluded.cwd,project=excluded.project,title=excluded.title,
		modified=excluded.modified,turns=excluded.turns,catalog_id=excluded.catalog_id,
		resume_id=excluded.resume_id`, projection.Session.Vendor,
		projection.Session.ID, projection.Session.Path, Sanitize(projection.Session.CWD),
		Sanitize(projection.Session.Project), Sanitize(projection.Session.Title),
		projection.Session.Modified, projection.Session.Turns,
		Sanitize(projection.Session.CatalogID), Sanitize(projection.Session.ResumeID)); err != nil {
		return TranscriptProjectionReplaceResult{}, err
	}
	if searchIndexReplacementTestHook != nil {
		if err := searchIndexReplacementTestHook(tx); err != nil {
			return TranscriptProjectionReplaceResult{}, err
		}
	}
	indexedAt := timeUnixMilli(projection.IndexedAt)
	if _, err := tx.Exec(`INSERT INTO transcript_index_projection(
		runtime,session_id,generation_marker,indexed_at,source_count,document_count,last_attempted_at,limitation)
		VALUES(?,?,?,?,?,?,?,'') ON CONFLICT(runtime,session_id) DO UPDATE SET
		generation_marker=excluded.generation_marker,indexed_at=excluded.indexed_at,
		source_count=excluded.source_count,document_count=excluded.document_count,
		last_attempted_at=excluded.last_attempted_at,limitation=''`, key.Runtime, key.SessionID,
		projection.Generation, indexedAt, projection.SourceCount, len(projection.Documents), indexedAt); err != nil {
		return TranscriptProjectionReplaceResult{}, err
	}
	state, err := scanTranscriptProjectionState(tx.QueryRow(`SELECT runtime,session_id,
		generation_marker,indexed_at,source_count,document_count,last_attempted_at,limitation
		FROM transcript_index_projection WHERE runtime=? AND session_id=?`, key.Runtime, key.SessionID))
	if err != nil {
		return TranscriptProjectionReplaceResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return TranscriptProjectionReplaceResult{}, err
	}
	return TranscriptProjectionReplaceResult{State: state, Replaced: true}, nil
}

func transcriptProjectionStateTx(tx *sql.Tx, key TranscriptProjectionKey) (
	TranscriptProjectionState, bool, error) {
	state, err := scanTranscriptProjectionState(tx.QueryRow(`SELECT runtime,session_id,
		generation_marker,indexed_at,source_count,document_count,last_attempted_at,limitation
		FROM transcript_index_projection WHERE runtime=? AND session_id=?`, key.Runtime, key.SessionID))
	if errors.Is(err, sql.ErrNoRows) {
		return TranscriptProjectionState{Key: key}, false, nil
	}
	return state, err == nil, err
}

type searchDocumentExecutor interface {
	Exec(string, ...any) (sql.Result, error)
}

func replaceOwnedSearchDocuments(db searchDocumentExecutor, ownerKind, vendor, sessionID string,
	documents []SearchDocument) error {
	if ownerKind == "" || vendor == "" || sessionID == "" {
		return fmt.Errorf("search document owner identity is incomplete")
	}
	if _, err := db.Exec(`DELETE FROM search_document
		WHERE owner_kind=? AND vendor=? AND session_id=?`, ownerKind, vendor, sessionID); err != nil {
		return err
	}
	for index, document := range documents {
		if document.Order != index || document.Kind == "" {
			return fmt.Errorf("search document order is invalid")
		}
		if _, err := db.Exec(`INSERT INTO search_document(
			owner_kind,vendor,session_id,document_order,ts,kind,text,lineage)
			VALUES(?,?,?,?,?,?,?,?)`, ownerKind, vendor, sessionID, document.Order,
			document.Timestamp, document.Kind, Sanitize(document.Text), document.Lineage); err != nil {
			return err
		}
	}
	return nil
}

func (ix *Index) replaceMemorySearchDocument(id string, ts int64, text string) error {
	if id == "" {
		return fmt.Errorf("memory search identity is incomplete")
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	documents := []SearchDocument{{Order: 0, Timestamp: fmt.Sprintf("%d", ts),
		Kind: "memory", Text: text, Lineage: "memory:" + id}}
	if err := replaceOwnedSearchDocuments(tx, "memory", "memory", id, documents); err != nil {
		return err
	}
	return tx.Commit()
}

func (ix *Index) RemoveTranscriptProjectionOrphans(keep []TranscriptProjectionKey) (int, error) {
	allowed := make(map[TranscriptProjectionKey]struct{}, len(keep))
	for _, key := range keep {
		if key.Runtime == "" || key.SessionID == "" {
			return 0, fmt.Errorf("transcript projection keep identity is incomplete")
		}
		allowed[key] = struct{}{}
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	// A session the owner tagged is kept even after its transcript file is gone:
	// its catalogue row and its indexed text are what he will be shown. Joining
	// the keep set (rather than guarding each statement below) means such a
	// session is never in the removal set, so the reported count stays true.
	kept, err := ownerKeptTranscriptKeys(tx, int(ix.ownerKeptSessionsLimit.Load()))
	if err != nil {
		return 0, err
	}
	for _, key := range kept {
		allowed[key] = struct{}{}
	}
	rows, err := tx.Query(`SELECT runtime,session_id FROM transcript_index_projection`)
	if err != nil {
		return 0, err
	}
	var remove []TranscriptProjectionKey
	for rows.Next() {
		var key TranscriptProjectionKey
		if err := rows.Scan(&key.Runtime, &key.SessionID); err != nil {
			_ = rows.Close()
			return 0, err
		}
		if _, ok := allowed[key]; !ok {
			remove = append(remove, key)
		}
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	sort.Slice(remove, func(i, j int) bool {
		if remove[i].Runtime == remove[j].Runtime {
			return remove[i].SessionID < remove[j].SessionID
		}
		return remove[i].Runtime < remove[j].Runtime
	})
	for _, key := range remove {
		if _, err := tx.Exec(`DELETE FROM search_document
			WHERE owner_kind='transcript' AND vendor=? AND session_id=?`, key.Runtime, key.SessionID); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`DELETE FROM transcript_index_projection
			WHERE runtime=? AND session_id=?`, key.Runtime, key.SessionID); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`DELETE FROM sessions WHERE vendor=? AND id=?
			AND NOT EXISTS(SELECT 1 FROM session_activity_observation
				WHERE runtime=? AND session_id=?)
			AND NOT EXISTS(SELECT 1 FROM event WHERE runtime=? AND session_id=?)
			AND NOT EXISTS(SELECT 1 FROM result_observation WHERE runtime=? AND session_id=?)
			AND NOT EXISTS(SELECT 1 FROM session_checkpoint WHERE runtime=? AND session_id=?)
			AND NOT EXISTS(SELECT 1 FROM collection_issue WHERE runtime=? AND session_id=?)
			AND NOT EXISTS(SELECT 1 FROM orchestration_review_invocation WHERE runtime=? AND session_id=?)
			AND NOT EXISTS(SELECT 1 FROM runtime_task WHERE runtime=?
				AND (native_session_id=? OR catalog_session_id=?))
			AND NOT EXISTS(SELECT 1 FROM change_record WHERE session_id=?)
			AND NOT EXISTS(SELECT 1 FROM session_state WHERE session_id=?)`,
			key.Runtime, key.SessionID,
			key.Runtime, key.SessionID,
			key.Runtime, key.SessionID,
			key.Runtime, key.SessionID,
			key.Runtime, key.SessionID,
			key.Runtime, key.SessionID,
			key.Runtime, key.SessionID,
			key.Runtime, key.SessionID, key.SessionID,
			key.SessionID, key.SessionID); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`UPDATE sessions SET path='',cwd='',project='',title='',modified=0,turns=0,
			catalog_id='',resume_id=''
			WHERE vendor=? AND id=?`, key.Runtime, key.SessionID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(remove), nil
}

// ListSessions lists indexed sessions newest-first, optionally filtered by vendor and
// a project substring.
func (ix *Index) ListSessions(vendor, project string, limit int) ([]SessionRow, error) {
	where, args := "1=1", []any{}
	if vendor != "" {
		where += " AND vendor=?"
		args = append(args, vendor)
	}
	if project != "" {
		where += " AND project LIKE ?"
		args = append(args, "%"+project+"%")
	}
	args = append(args, limit)
	rows, err := ix.db.Query(
		"SELECT vendor,id,path,cwd,project,title,modified,turns,catalog_id,resume_id FROM sessions WHERE "+where+" ORDER BY modified DESC LIMIT ?", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SessionRow{}
	for rows.Next() {
		var row SessionRow
		if err := rows.Scan(&row.Vendor, &row.ID, &row.Path, &row.CWD, &row.Project,
			&row.Title, &row.Modified, &row.Turns, &row.CatalogID, &row.ResumeID); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (ix *Index) SessionByID(vendor, id string) (SessionRow, bool, error) {
	var row SessionRow
	query := "SELECT vendor,id,path,cwd,project,title,modified,turns,catalog_id,resume_id FROM sessions WHERE id=?"
	args := []any{id}
	if vendor != "" {
		query += " AND vendor=?"
		args = append(args, vendor)
	}
	query += " ORDER BY modified DESC LIMIT 1"
	err := ix.db.QueryRow(query, args...).Scan(&row.Vendor, &row.ID, &row.Path, &row.CWD,
		&row.Project, &row.Title, &row.Modified, &row.Turns, &row.CatalogID, &row.ResumeID)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionRow{}, false, nil
	}
	return row, err == nil, err
}

func (ix *Index) SearchEvents(query string, limit int) ([]EventHit, error) {
	match := ftsQuery(query)
	if match == "" {
		return nil, fmt.Errorf("empty query")
	}
	rows, err := ix.db.Query(`SELECT events_fts.vendor,events_fts.session_id,
		events_fts.ts,events_fts.kind,snippet(events_fts,4,'','','…',24),
		COALESCE(s.path,''),COALESCE(s.cwd,''),COALESCE(s.project,''),COALESCE(s.title,''),
		COALESCE(s.modified,0),COALESCE(s.turns,0),COALESCE(s.catalog_id,''),COALESCE(s.resume_id,'')
		FROM events_fts LEFT JOIN sessions s
		ON s.vendor=events_fts.vendor AND s.id=events_fts.session_id
		WHERE events_fts MATCH ? ORDER BY rank LIMIT ?`, match, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EventHit{}
	for rows.Next() {
		var hit EventHit
		if err := rows.Scan(&hit.Vendor, &hit.SessionID, &hit.TS, &hit.Kind, &hit.Text,
			&hit.Path, &hit.CWD, &hit.Project, &hit.Title, &hit.Modified, &hit.Turns,
			&hit.CatalogID, &hit.ResumeID); err != nil {
			return nil, err
		}
		out = append(out, hit)
	}
	return out, rows.Err()
}

// SessionsMatchingText reports which of the given sessions mention the words,
// best match first, in one match pass: the id set travels as a single JSON
// parameter, so there is no chunking and the full-text match runs once however
// large the set is. It answers in sessions, not events — a limit on events lets
// a few long sessions crowd out every other match — and the bool reports that
// more sessions matched than limit. Filtering a global top-N afterwards would
// lose a session that ranks below N globally but first within the set. Callers
// check the runtime: the key carries the vendor the text was indexed under.
func (ix *Index) SessionsMatchingText(query string, sessionIDs []string, limit int) ([]TranscriptProjectionKey, bool, error) {
	match := ftsQuery(query)
	if match == "" {
		return nil, false, fmt.Errorf("empty query")
	}
	if len(sessionIDs) == 0 {
		return nil, false, nil
	}
	ids, err := json.Marshal(sessionIDs)
	if err != nil {
		return nil, false, err
	}
	rows, err := ix.db.Query(`SELECT vendor,session_id FROM (
			SELECT events_fts.vendor AS vendor, events_fts.session_id AS session_id, MIN(rank) AS best
			FROM events_fts
			WHERE events_fts MATCH ? AND events_fts.session_id IN (SELECT value FROM json_each(?))
			GROUP BY events_fts.vendor, events_fts.session_id)
		ORDER BY best LIMIT ?`, match, string(ids), limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []TranscriptProjectionKey
	for rows.Next() {
		var key TranscriptProjectionKey
		if err := rows.Scan(&key.Runtime, &key.SessionID); err != nil {
			return nil, false, err
		}
		out = append(out, key)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

// TranscriptDocument is one piece of a session's text as it was kept for search.
type TranscriptDocument struct {
	Order int    `json:"order"`
	TS    string `json:"ts"`
	Kind  string `json:"kind"`
	Text  string `json:"text"`
}

// TranscriptDocuments returns the text kept for one session, in conversation
// order. It is what remains readable of a session whose transcript file is
// gone: prompts and replies clipped to the search bound, tool rows as one line.
func (ix *Index) TranscriptDocuments(runtime, sessionID string, limit int) ([]TranscriptDocument, error) {
	rows, err := ix.db.Query(`SELECT document_order,ts,kind,text FROM search_document
		WHERE owner_kind='transcript' AND vendor=? AND session_id=?
		ORDER BY document_order LIMIT ?`, runtime, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TranscriptDocument{}
	for rows.Next() {
		var doc TranscriptDocument
		if err := rows.Scan(&doc.Order, &doc.TS, &doc.Kind, &doc.Text); err != nil {
			return nil, err
		}
		out = append(out, doc)
	}
	return out, rows.Err()
}

const maxBlobBytes = 4000

func Sanitize(value string) string {
	value = strings.ToValidUTF8(value, "")
	value = strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, value)
	if len(value) > maxBlobBytes {
		value = strings.ToValidUTF8(value[:maxBlobBytes], "")
	}
	return value
}

func ftsQuery(text string) string {
	var parts []string
	for _, token := range strings.Fields(text) {
		token = strings.ReplaceAll(token, `"`, "")
		if token != "" {
			parts = append(parts, `"`+token+`"`)
		}
	}
	return strings.Join(parts, " ")
}
