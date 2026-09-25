package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// ErrSessionTurnCollision reports reuse of one turn observation identity for
// a different fact — a retry may repeat a row, never rewrite it.
var ErrSessionTurnCollision = errors.New("session turn observation collision")

// SessionTurnObservation is one turn-boundary fact from an installed hook.
// Kind is the framework's closed vocabulary; ReceivedAtMS is the daemon clock
// and is the only ordering the status decider trusts.
type SessionTurnObservation struct {
	ObservationID    string `json:"observation_id"`
	Runtime          string `json:"runtime"`
	SessionID        string `json:"session_id"`
	CatalogSessionID string `json:"catalog_session_id,omitempty"`
	Kind             string `json:"kind"`
	ObservedAt       int64  `json:"observed_at"`
	ReceivedAtMS     int64  `json:"received_at_ms"`
	NativeSource     string `json:"native_source,omitempty"`
	SourceRef        string `json:"source_ref,omitempty"`
	EvidenceDigest   string `json:"evidence_digest"`
	CollectorID      string `json:"collector_id"`
	CollectorVersion string `json:"collector_version,omitempty"`
	DeliveryAttempts int    `json:"delivery_attempts"`
	DeliveryMode     string `json:"delivery_mode"`
	// RowID is the store-assigned monotonic identity; the browser's attention
	// ledger compares it within the turn cursor space.
	RowID int64 `json:"row_id,omitempty"`
}

const sessionTurnCols = `observation_id,runtime,session_id,catalog_session_id,kind,
	observed_at,received_at_ms,native_source,source_ref,evidence_digest,
	collector_id,collector_version,delivery_attempts,delivery_mode`

func scanSessionTurn(row interface{ Scan(...any) error }) (SessionTurnObservation, error) {
	var turn SessionTurnObservation
	err := row.Scan(&turn.RowID, &turn.ObservationID, &turn.Runtime, &turn.SessionID,
		&turn.CatalogSessionID, &turn.Kind, &turn.ObservedAt, &turn.ReceivedAtMS,
		&turn.NativeSource, &turn.SourceRef, &turn.EvidenceDigest, &turn.CollectorID,
		&turn.CollectorVersion, &turn.DeliveryAttempts, &turn.DeliveryMode)
	return turn, err
}

// AppendSessionTurn inserts one turn row. A repeat of the same fact (retry,
// replay) is a no-op that bumps delivery_attempts; the same identity carrying a
// different fact is a collision.
func (g *GovTx) AppendSessionTurn(turn SessionTurnObservation) (bool, int64, error) {
	result, err := g.tx.Exec(`INSERT INTO session_turn_observation(`+sessionTurnCols+`)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(observation_id) DO NOTHING`,
		turn.ObservationID, turn.Runtime, turn.SessionID, turn.CatalogSessionID, turn.Kind,
		turn.ObservedAt, turn.ReceivedAtMS, turn.NativeSource, turn.SourceRef,
		turn.EvidenceDigest, turn.CollectorID, turn.CollectorVersion,
		turn.DeliveryAttempts, turn.DeliveryMode)
	if err != nil {
		return false, 0, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, 0, err
	}
	existing, err := scanSessionTurn(g.tx.QueryRow(`SELECT rowid,`+sessionTurnCols+`
		FROM session_turn_observation WHERE observation_id=?`, turn.ObservationID))
	if err != nil {
		return false, 0, err
	}
	if rows == 1 {
		return true, existing.RowID, nil
	}
	if existing.Runtime != turn.Runtime || existing.SessionID != turn.SessionID ||
		existing.Kind != turn.Kind || existing.EvidenceDigest != turn.EvidenceDigest {
		return false, 0, ErrSessionTurnCollision
	}
	if _, err := g.tx.Exec(`UPDATE session_turn_observation
		SET delivery_attempts=MAX(delivery_attempts,?) WHERE observation_id=?`,
		turn.DeliveryAttempts, turn.ObservationID); err != nil {
		return false, 0, err
	}
	return false, existing.RowID, nil
}

// SessionTurnsFor returns the newest turn rows for one session (by native id),
// newest first, bounded. The decider needs only the newest few; `limit` is
// its configured lookback.
func (ix *Index) SessionTurnsFor(runtime, sessionID string, limit int) ([]SessionTurnObservation, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("session turns: limit must be positive (the bound is policy, owned by configuration)")
	}
	rows, err := ix.db.Query(`SELECT rowid,`+sessionTurnCols+` FROM session_turn_observation
		WHERE runtime=? AND session_id=? ORDER BY received_at_ms DESC, rowid DESC LIMIT ?`,
		runtime, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionTurnObservation
	for rows.Next() {
		turn, err := scanSessionTurn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, turn)
	}
	return out, rows.Err()
}

// SessionTurnsAfter returns turn rows with rowid strictly greater than after,
// oldest first, bounded — the natural signal emitter's durable cursor read
// (helper-session-attachment plan D2). The returned cursor is the last row's
// rowid, unchanged when nothing is new.
func (ix *Index) SessionTurnsAfter(after int64, limit int) ([]SessionTurnObservation, int64, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := ix.db.Query(`SELECT rowid,`+sessionTurnCols+` FROM session_turn_observation
		WHERE rowid > ? ORDER BY rowid ASC LIMIT ?`, after, limit)
	if err != nil {
		return nil, after, err
	}
	defer rows.Close()
	out := []SessionTurnObservation{}
	last := after
	for rows.Next() {
		turn, err := scanSessionTurn(rows)
		if err != nil {
			return nil, after, err
		}
		out = append(out, turn)
		last = turn.RowID
	}
	return out, last, rows.Err()
}

// SessionTurnHead is the newest turn rowid (0 when the table is empty): where a
// consumer meeting this table for the first time starts.
func (ix *Index) SessionTurnHead() (int64, error) {
	var head int64
	err := ix.db.QueryRow(`SELECT COALESCE(MAX(rowid),0) FROM session_turn_observation`).Scan(&head)
	return head, err
}

// PruneSessionTurns deletes rows older than the retention boundary (daemon
// clock, milliseconds). Retention is policy and arrives from configuration.
func (ix *Index) PruneSessionTurns(beforeMS int64) (int64, error) {
	result, err := ix.db.Exec(`DELETE FROM session_turn_observation WHERE received_at_ms < ?`, beforeMS)
	if err != nil {
		return 0, fmt.Errorf("prune session turns: %w", err)
	}
	return result.RowsAffected()
}

// SessionNewestActionAt returns the newest governed-action instant (event.ts,
// daemon seconds) for a session — the decider's action.observed fact.
func (ix *Index) SessionNewestActionAt(sessionID string) (int64, bool, error) {
	var ts sql.NullInt64
	if err := ix.db.QueryRow(`SELECT MAX(ts) FROM event WHERE session_id=?`, sessionID).Scan(&ts); err != nil {
		return 0, false, err
	}
	return ts.Int64, ts.Valid, nil
}

// SessionsWithRecentTurns lists sessions that produced a turn row after the
// boundary (daemon ms), newest first — so a session that handed back moments
// ago is on the rail even when no presence lane holds it.
func (ix *Index) SessionsWithRecentTurns(sinceMS int64, limit int) ([]SessionTurnObservation, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("recent turn sessions: limit must be positive (the bound is policy, owned by configuration)")
	}
	rows, err := ix.db.Query(`SELECT rowid,`+sessionTurnCols+` FROM session_turn_observation
		WHERE rowid IN (SELECT MAX(rowid) FROM session_turn_observation WHERE received_at_ms >= ?
		GROUP BY runtime, session_id) ORDER BY received_at_ms DESC LIMIT ?`, sinceMS, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionTurnObservation
	for rows.Next() {
		turn, err := scanSessionTurn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, turn)
	}
	return out, rows.Err()
}
