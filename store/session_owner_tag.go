package store

// session_owner_tag / session_owner_note: text the owner attaches to a session
// to organize his own work (session-organization plan §3.1, implementation plan
// §4.1). This is user content, not evidence: nothing here is a detector hit, a
// governance input, or something an agent is shown. It deliberately lives
// apart from session_state and orchestration_tag, which the rule evaluator
// reads. No tag name, key or note text is ever supplied by the framework.
//
// Rows are keyed by (runtime, the runtime adapter's canonical session id); the
// caller resolves that identity, the store never infers it. A tag row also
// snapshots the session's title, repository and last activity at apply time so
// a tagged session can still be listed after its transcript file is gone.

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

const sessionOwnerTagSchema = `
CREATE TABLE IF NOT EXISTS session_owner_tag(
  tag_id TEXT PRIMARY KEY,
  runtime TEXT NOT NULL CHECK(length(runtime) > 0),
  session_id TEXT NOT NULL CHECK(length(session_id) > 0),
  key TEXT NOT NULL DEFAULT '' CHECK(length(key) <= 64),
  value TEXT NOT NULL CHECK(length(value) BETWEEN 1 AND 64),
  key_fold TEXT NOT NULL DEFAULT '',
  value_fold TEXT NOT NULL,
  applied_at INTEGER NOT NULL,
  retracted_at INTEGER NOT NULL DEFAULT 0,
  session_title TEXT NOT NULL DEFAULT '',
  session_repository TEXT NOT NULL DEFAULT '',
  session_cwd TEXT NOT NULL DEFAULT '',
  session_touched_at INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS session_owner_tag_active
  ON session_owner_tag(runtime,session_id,key_fold,value_fold) WHERE retracted_at=0;
CREATE INDEX IF NOT EXISTS session_owner_tag_fold ON session_owner_tag(key_fold,value_fold);
CREATE TABLE IF NOT EXISTS session_owner_note(
  runtime TEXT NOT NULL CHECK(length(runtime) > 0),
  session_id TEXT NOT NULL CHECK(length(session_id) > 0),
  text TEXT NOT NULL CHECK(length(text) BETWEEN 1 AND 500),
  updated_at INTEGER NOT NULL,
  PRIMARY KEY(runtime,session_id)
);
`

// The storage bounds the CHECKs above enforce. They are the only compiled
// numbers in this feature: a column bound is a storage fact, not a tunable.
const (
	SessionOwnerTagPartMax = 64
	SessionOwnerNoteMax    = 500
	// SessionOwnerTagKeylessKey is the key a keyless tag is matched under when
	// tags are projected for a query. An owner key spelled this way is refused
	// so the two can never be confused.
	SessionOwnerTagKeylessKey = "tag"
)

var ErrSessionOwnerTagInvalid = errors.New("session owner tag is invalid")

// SessionOwnerTagValue is one tag as the owner wrote it. Key is optional.
type SessionOwnerTagValue struct {
	Key   string `json:"key,omitempty"`
	Value string `json:"value"`
}

// SessionOwnerTarget names one session and carries what is remembered about it.
type SessionOwnerTarget struct {
	Runtime    string
	SessionID  string
	Title      string
	Repository string
	Cwd        string
	TouchedAt  int64
}

// SessionOwnerTag is one active tag row.
type SessionOwnerTag struct {
	Runtime    string
	SessionID  string
	Key        string
	Value      string
	AppliedAt  int64
	Title      string
	Repository string
	Cwd        string
	TouchedAt  int64
}

// SessionOwnerNote is the one replaceable line the owner keeps on a session.
type SessionOwnerNote struct {
	Runtime   string
	SessionID string
	Text      string
	UpdatedAt int64
}

// SessionOwnerTagUse is one distinct tag with how much it is used.
type SessionOwnerTagUse struct {
	Key           string `json:"key,omitempty"`
	Value         string `json:"value"`
	Sessions      int    `json:"sessions"`
	LastAppliedAt int64  `json:"last_applied_at"`
}

// FoldSessionOwnerTagPart is the one place tag text is case-folded. SQLite's
// lower() is ASCII-only, so folding happens here and is stored.
func FoldSessionOwnerTagPart(part string) string { return strings.ToLower(part) }

// ValidateSessionOwnerTag reports why a tag cannot be stored, in words fit to
// show the owner.
func ValidateSessionOwnerTag(tag SessionOwnerTagValue) error {
	if tag.Value == "" {
		return fmt.Errorf("%w: a tag needs some text", ErrSessionOwnerTagInvalid)
	}
	if FoldSessionOwnerTagPart(tag.Key) == SessionOwnerTagKeylessKey {
		return fmt.Errorf("%w: %q cannot be used as a key", ErrSessionOwnerTagInvalid, tag.Key)
	}
	// A tag is written and queried as key:value, split at the first colon, so a
	// colon inside either part could never be typed back.
	if strings.Contains(tag.Key, ":") || strings.Contains(tag.Value, ":") {
		return fmt.Errorf("%w: only one colon, between key and value", ErrSessionOwnerTagInvalid)
	}
	for _, part := range []string{tag.Key, tag.Value} {
		if len(part) > SessionOwnerTagPartMax {
			return fmt.Errorf("%w: %q is longer than %d bytes", ErrSessionOwnerTagInvalid, part, SessionOwnerTagPartMax)
		}
		if part != strings.TrimSpace(part) {
			return fmt.Errorf("%w: %q starts or ends with a space", ErrSessionOwnerTagInvalid, part)
		}
		if index := strings.IndexFunc(part, forbiddenSessionOwnerTagRune); index >= 0 {
			return fmt.Errorf("%w: %q contains a character tags cannot use (= * \" or a control character)", ErrSessionOwnerTagInvalid, part)
		}
	}
	return nil
}

func forbiddenSessionOwnerTagRune(r rune) bool {
	return r == '=' || r == '*' || r == '"' || unicode.IsControl(r)
}

func validateSessionOwnerTarget(target SessionOwnerTarget) error {
	if target.Runtime == "" || target.SessionID == "" {
		return fmt.Errorf("%w: session identity is incomplete", ErrSessionOwnerTagInvalid)
	}
	return nil
}

// newSessionOwnerTagID is random rather than derived: the same tag can be put
// on, taken off and put back on one session within a second, and each of those
// is its own row.
func newSessionOwnerTagID() (string, error) {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	// "ownt_", not the "otag_" of agent-claimed tags: the two are different facts
	// from different authors and must be tellable apart in a log or an export.
	return "ownt_" + hex.EncodeToString(raw[:]), nil
}

// ChangeSessionOwnerTags takes tags off and puts tags on, for every target, in
// one transaction: every tag is validated before anything is written, and all
// of it happens or none of it does. Either list may be empty, not both.
func (ix *Index) ChangeSessionOwnerTags(targets []SessionOwnerTarget, apply, retract []SessionOwnerTagValue, now int64) error {
	if err := validateSessionOwnerWrite(targets, append(append([]SessionOwnerTagValue(nil), apply...), retract...)); err != nil {
		return err
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, target := range targets {
		for _, tag := range retract {
			if err := retractSessionOwnerTag(tx, target, tag, now); err != nil {
				return err
			}
		}
	}
	for _, tag := range apply {
		spelled, err := firstSpellingOfSessionOwnerTag(tx, tag)
		if err != nil {
			return err
		}
		for _, target := range targets {
			if err := applySessionOwnerTag(tx, target, spelled, now); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// ApplySessionOwnerTags puts every tag on every target, in one transaction:
// all of it happens or none of it does. Applying a tag a session already has
// refreshes what is remembered about the session and nothing else. A tag that
// already exists anywhere in another letter case is stored as first written.
func (ix *Index) ApplySessionOwnerTags(targets []SessionOwnerTarget, tags []SessionOwnerTagValue, now int64) error {
	return ix.ChangeSessionOwnerTags(targets, tags, nil, now)
}

func validateSessionOwnerWrite(targets []SessionOwnerTarget, tags []SessionOwnerTagValue) error {
	if len(targets) == 0 || len(tags) == 0 {
		return fmt.Errorf("%w: nothing to write", ErrSessionOwnerTagInvalid)
	}
	for _, target := range targets {
		if err := validateSessionOwnerTarget(target); err != nil {
			return err
		}
	}
	for _, tag := range tags {
		if err := ValidateSessionOwnerTag(tag); err != nil {
			return err
		}
	}
	return nil
}

func firstSpellingOfSessionOwnerTag(tx *sql.Tx, tag SessionOwnerTagValue) (SessionOwnerTagValue, error) {
	spelled := tag
	err := tx.QueryRow(`SELECT key,value FROM session_owner_tag
		WHERE key_fold=? AND value_fold=? ORDER BY applied_at,tag_id LIMIT 1`,
		FoldSessionOwnerTagPart(tag.Key), FoldSessionOwnerTagPart(tag.Value)).Scan(&spelled.Key, &spelled.Value)
	if errors.Is(err, sql.ErrNoRows) {
		return tag, nil
	}
	return spelled, err
}

func applySessionOwnerTag(tx *sql.Tx, target SessionOwnerTarget, tag SessionOwnerTagValue, now int64) error {
	keyFold, valueFold := FoldSessionOwnerTagPart(tag.Key), FoldSessionOwnerTagPart(tag.Value)
	tagID, err := newSessionOwnerTagID()
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO session_owner_tag(tag_id,runtime,session_id,key,value,key_fold,value_fold,
			applied_at,session_title,session_repository,session_cwd,session_touched_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(runtime,session_id,key_fold,value_fold) WHERE retracted_at=0 DO UPDATE SET
			session_title=excluded.session_title, session_repository=excluded.session_repository,
			session_cwd=excluded.session_cwd, session_touched_at=excluded.session_touched_at`,
		tagID, target.Runtime, target.SessionID,
		tag.Key, tag.Value, keyFold, valueFold, now,
		target.Title, target.Repository, target.Cwd, target.TouchedAt)
	if err != nil {
		return err
	}
	// Schema 39: every apply appends one journal row in the same transaction.
	// A re-apply of a tag the session already carries is still an owner act
	// worth one fact — it refreshes the session snapshot, and the flow
	// translator deduplicates membership by member row, not by change row.
	return appendOwnerTagChange(tx, target, tag, "applied", now)
}

// RetractSessionOwnerTags takes every listed tag off every target. Removing a
// tag a session does not carry is not an error.
func (ix *Index) RetractSessionOwnerTags(targets []SessionOwnerTarget, tags []SessionOwnerTagValue, now int64) error {
	return ix.ChangeSessionOwnerTags(targets, nil, tags, now)
}

func retractSessionOwnerTag(tx *sql.Tx, target SessionOwnerTarget, tag SessionOwnerTagValue, now int64) error {
	result, err := tx.Exec(`UPDATE session_owner_tag SET retracted_at=?
		WHERE runtime=? AND session_id=? AND key_fold=? AND value_fold=? AND retracted_at=0`,
		now, target.Runtime, target.SessionID,
		FoldSessionOwnerTagPart(tag.Key), FoldSessionOwnerTagPart(tag.Value))
	if err != nil {
		return err
	}
	// Schema 39: a retract that found no live row removed nothing, so it
	// journals no change fact — the journal records the owner's tag state
	// transitions, not every no-op request.
	removed, err := result.RowsAffected()
	if err != nil || removed == 0 {
		return err
	}
	return appendOwnerTagChange(tx, target, tag, "removed", now)
}

// RenameSessionOwnerTag changes one tag into another on every session that
// carries it, keeping when it was applied. A session that already carries the
// new tag simply loses the old one. It returns how many sessions changed.
func (ix *Index) RenameSessionOwnerTag(from, to SessionOwnerTagValue, now int64) (int, error) {
	if err := ValidateSessionOwnerTag(from); err != nil {
		return 0, err
	}
	if err := ValidateSessionOwnerTag(to); err != nil {
		return 0, err
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	fromKey, fromValue := FoldSessionOwnerTagPart(from.Key), FoldSessionOwnerTagPart(from.Value)
	if fromKey == FoldSessionOwnerTagPart(to.Key) && fromValue == FoldSessionOwnerTagPart(to.Value) {
		// Only the capitals change: it is the same tag, so every row of it —
		// history included — takes the new spelling, and nothing is re-applied.
		result, err := tx.Exec(`UPDATE session_owner_tag SET key=?,value=? WHERE key_fold=? AND value_fold=?`,
			to.Key, to.Value, fromKey, fromValue)
		if err != nil {
			return 0, err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		return int(changed), tx.Commit()
	}
	// A tag that already exists keeps the spelling it was first given.
	to, err = firstSpellingOfSessionOwnerTag(tx, to)
	if err != nil {
		return 0, err
	}
	carriers, err := activeSessionOwnerTagRows(tx, `WHERE retracted_at=0 AND key_fold=? AND value_fold=?`, fromKey, fromValue)
	if err != nil {
		return 0, err
	}
	for _, row := range carriers {
		target := SessionOwnerTarget{Runtime: row.Runtime, SessionID: row.SessionID, Title: row.Title,
			Repository: row.Repository, Cwd: row.Cwd, TouchedAt: row.TouchedAt}
		if err := retractSessionOwnerTag(tx, target, from, now); err != nil {
			return 0, err
		}
		if err := applySessionOwnerTag(tx, target, to, row.AppliedAt); err != nil {
			return 0, err
		}
	}
	return len(carriers), tx.Commit()
}

// PurgeSessionOwnerTag deletes a tag everywhere, history included. This is the
// owner's delete for his own content; it is not a retract. Schema 39: every
// live carrier still gets one removal journal row first, in the same
// transaction, so a purge is observable as a membership exit (pass-2 C3).
func (ix *Index) PurgeSessionOwnerTag(tag SessionOwnerTagValue) (int, error) {
	if err := ValidateSessionOwnerTag(tag); err != nil {
		return 0, err
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	carriers, err := activeSessionOwnerTagRows(tx, `WHERE key_fold=? AND value_fold=?`,
		FoldSessionOwnerTagPart(tag.Key), FoldSessionOwnerTagPart(tag.Value))
	if err != nil {
		return 0, err
	}
	now := time.Now().Unix()
	for _, row := range carriers {
		target := SessionOwnerTarget{Runtime: row.Runtime, SessionID: row.SessionID, Title: row.Title,
			Repository: row.Repository, Cwd: row.Cwd, TouchedAt: row.TouchedAt}
		if err := appendOwnerTagChange(tx, target, tag, "removed", now); err != nil {
			return 0, err
		}
	}
	if _, err := tx.Exec(`DELETE FROM session_owner_tag WHERE key_fold=? AND value_fold=?`,
		FoldSessionOwnerTagPart(tag.Key), FoldSessionOwnerTagPart(tag.Value)); err != nil {
		return 0, err
	}
	return len(carriers), tx.Commit()
}

type sessionOwnerQuerier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

func activeSessionOwnerTagRows(db sessionOwnerQuerier, where string, args ...any) ([]SessionOwnerTag, error) {
	rows, err := db.Query(`SELECT runtime,session_id,key,value,applied_at,session_title,
		session_repository,session_cwd,session_touched_at FROM session_owner_tag `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionOwnerTag
	for rows.Next() {
		var tag SessionOwnerTag
		if err := rows.Scan(&tag.Runtime, &tag.SessionID, &tag.Key, &tag.Value, &tag.AppliedAt,
			&tag.Title, &tag.Repository, &tag.Cwd, &tag.TouchedAt); err != nil {
			return nil, err
		}
		out = append(out, tag)
	}
	return out, rows.Err()
}

// AllActiveSessionOwnerTags reads every active tag, newest first, up to limit.
// The second result reports that the limit cut the read short, so a caller
// never presents a partial read as the whole truth. It takes no transaction:
// the store opens write-locked transactions, and this is a read.
func (ix *Index) AllActiveSessionOwnerTags(limit int) ([]SessionOwnerTag, bool, error) {
	tags, err := activeSessionOwnerTagRows(ix.db,
		`WHERE retracted_at=0 ORDER BY applied_at DESC,tag_id LIMIT ?`, limit+1)
	if err != nil {
		return nil, false, err
	}
	if len(tags) > limit {
		return tags[:limit], true, nil
	}
	return tags, false, nil
}

// SessionOwnerTagVocabulary lists the owner's distinct tags, most recently
// used first. It is the only source of tag suggestions: what he already wrote.
func (ix *Index) SessionOwnerTagVocabulary(limit int) ([]SessionOwnerTagUse, error) {
	// The spelling is that of the tag's earliest row — MIN() would pick the
	// binary-smallest spelling, and could take key and value from different rows.
	rows, err := ix.db.Query(`SELECT key,value,sessions,last_applied FROM (
		SELECT key,value,value_fold,
			COUNT(*) OVER (PARTITION BY key_fold,value_fold) AS sessions,
			MAX(applied_at) OVER (PARTITION BY key_fold,value_fold) AS last_applied,
			ROW_NUMBER() OVER (PARTITION BY key_fold,value_fold ORDER BY applied_at,tag_id) AS first_row
		FROM session_owner_tag WHERE retracted_at=0)
		WHERE first_row=1 ORDER BY last_applied DESC,value_fold LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SessionOwnerTagUse{}
	for rows.Next() {
		var use SessionOwnerTagUse
		if err := rows.Scan(&use.Key, &use.Value, &use.Sessions, &use.LastAppliedAt); err != nil {
			return nil, err
		}
		out = append(out, use)
	}
	return out, rows.Err()
}

// PutSessionOwnerNote replaces the session's note; empty text clears it.
func (ix *Index) PutSessionOwnerNote(target SessionOwnerTarget, text string, now int64) error {
	if err := validateSessionOwnerTarget(target); err != nil {
		return err
	}
	text = strings.TrimSpace(text)
	if len(text) > SessionOwnerNoteMax {
		return fmt.Errorf("%w: a note is at most %d bytes", ErrSessionOwnerTagInvalid, SessionOwnerNoteMax)
	}
	if text == "" {
		_, err := ix.db.Exec(`DELETE FROM session_owner_note WHERE runtime=? AND session_id=?`,
			target.Runtime, target.SessionID)
		return err
	}
	_, err := ix.db.Exec(`INSERT INTO session_owner_note(runtime,session_id,text,updated_at) VALUES(?,?,?,?)
		ON CONFLICT(runtime,session_id) DO UPDATE SET text=excluded.text, updated_at=excluded.updated_at`,
		target.Runtime, target.SessionID, text, now)
	return err
}

// AllSessionOwnerNotes reads every note up to limit; the bool reports truncation.
func (ix *Index) AllSessionOwnerNotes(limit int) ([]SessionOwnerNote, bool, error) {
	rows, err := ix.db.Query(`SELECT runtime,session_id,text,updated_at FROM session_owner_note
		ORDER BY updated_at DESC LIMIT ?`, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []SessionOwnerNote
	for rows.Next() {
		var note SessionOwnerNote
		if err := rows.Scan(&note.Runtime, &note.SessionID, &note.Text, &note.UpdatedAt); err != nil {
			return nil, false, err
		}
		out = append(out, note)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

// RememberedSessionOwnerTag returns what the owner's tags remember of one
// session — the most recently refreshed snapshot among its active tags. It is
// a keyed read, so it does not depend on any whole-table bound.
func (ix *Index) RememberedSessionOwnerTag(runtime, sessionID string) (SessionOwnerTag, bool, error) {
	tags, err := activeSessionOwnerTagRows(ix.db, `WHERE runtime=? AND session_id=? AND retracted_at=0
		ORDER BY session_touched_at DESC,applied_at DESC LIMIT 1`, runtime, sessionID)
	if err != nil || len(tags) == 0 {
		return SessionOwnerTag{}, false, err
	}
	return tags[0], true, nil
}

// SessionOwnerTagsFor lists one session's active owner tags, newest first.
// A daemon-side read for the flow machinery's query rows (tag VALUES stay
// daemon-side — invariant 3).
func (ix *Index) SessionOwnerTagsFor(runtime, sessionID string, limit int) ([]SessionOwnerTag, error) {
	return activeSessionOwnerTagRows(ix.db, `WHERE runtime=? AND session_id=? AND retracted_at=0
		ORDER BY applied_at DESC LIMIT ?`, runtime, sessionID, limit)
}

// SetOwnerKeptSessionsLimit bounds how many owner-tagged sessions the
// transcript orphan sweep spares, most recently tagged first. The daemon sets
// it from configuration; zero (the state of a store nobody configured, such as
// the CLI's) spares every tagged session.
func (ix *Index) SetOwnerKeptSessionsLimit(limit int) {
	if limit < 0 {
		limit = 0
	}
	ix.ownerKeptSessionsLimit.Store(int64(limit))
}

// ownerKeptTranscriptKeys lists the sessions an active owner tag keeps from
// the transcript orphan sweep, most recently tagged first. It reads inside the
// sweep's own transaction so a tag applied while the sweep runs is not missed.
// limit <= 0 keeps every tagged session: when nothing has set a bound, the
// safe direction is never to discard what the owner marked.
func ownerKeptTranscriptKeys(tx *sql.Tx, limit int) ([]TranscriptProjectionKey, error) {
	query := `SELECT runtime,session_id FROM session_owner_tag WHERE retracted_at=0
		AND runtime<>'' AND session_id<>'' GROUP BY runtime,session_id ORDER BY MAX(applied_at) DESC`
	args := []any{}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []TranscriptProjectionKey
	for rows.Next() {
		var key TranscriptProjectionKey
		if err := rows.Scan(&key.Runtime, &key.SessionID); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}
