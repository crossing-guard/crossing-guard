package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// ErrSessionActivityCollision reports reuse of one observation identity for
// different immutable lifecycle facts.
var ErrSessionActivityCollision = errors.New("session activity observation collision")

// SessionActivityObservation is append-only evidence that a runtime session was open
// at one boundary or explicitly ended. It is not a durable running lease.
type SessionActivityObservation struct {
	ObservationID    string `json:"observation_id"`
	Runtime          string `json:"runtime"`
	SessionID        string `json:"session_id"`
	State            string `json:"state"`
	ObservedAt       int64  `json:"observed_at"`
	ValidUntil       int64  `json:"valid_until"`
	EvidenceClass    string `json:"evidence_class"`
	EntryKind        string `json:"entry_kind"`
	NativeSource     string `json:"native_source,omitempty"`
	SourceRef        string `json:"source_ref,omitempty"`
	EvidenceDigest   string `json:"evidence_digest"`
	CollectorID      string `json:"collector_id"`
	CollectorVersion string `json:"collector_version,omitempty"`
	ReceivedAt       int64  `json:"received_at"`
	DeliveryAttempts int    `json:"delivery_attempts"`
	DeliveryMode     string `json:"delivery_mode"`
	// RowID is the store-assigned monotonic identity of this row; the natural
	// signal emitter anchors run idempotency on it (never on a timestamp).
	RowID int64 `json:"row_id,omitempty"`
}

const sessionActivityCols = `observation_id,runtime,session_id,state,observed_at,
	valid_until,evidence_class,entry_kind,native_source,source_ref,evidence_digest,
	collector_id,collector_version,received_at,delivery_attempts,delivery_mode`

func scanSessionActivity(row interface{ Scan(...any) error }) (SessionActivityObservation, error) {
	var activity SessionActivityObservation
	err := row.Scan(&activity.ObservationID, &activity.Runtime, &activity.SessionID,
		&activity.State, &activity.ObservedAt, &activity.ValidUntil, &activity.EvidenceClass,
		&activity.EntryKind, &activity.NativeSource, &activity.SourceRef,
		&activity.EvidenceDigest, &activity.CollectorID, &activity.CollectorVersion,
		&activity.ReceivedAt, &activity.DeliveryAttempts, &activity.DeliveryMode)
	return activity, err
}

// EnsureSessionRoot creates the canonical composite session parent without
// overwriting transcript-owned presentation fields.
func (g *GovTx) EnsureSessionRoot(runtime, sessionID, sourceRef, cwd string) error {
	if runtime == "" || sessionID == "" {
		return fmt.Errorf("session root requires runtime and native session identity")
	}
	_, err := g.tx.Exec(`INSERT INTO sessions(vendor,id,path,cwd,project,title,modified,turns)
		VALUES(?,?,?,?,?,?,0,0) ON CONFLICT(vendor,id) DO UPDATE SET
		path=CASE WHEN sessions.path='' THEN excluded.path ELSE sessions.path END,
		cwd=CASE WHEN sessions.cwd='' THEN excluded.cwd ELSE sessions.cwd END`,
		runtime, sessionID, sourceRef, Sanitize(cwd), "", "")
	return err
}

// AppendSessionActivity inserts one immutable fact and rejects identity collisions.
func (g *GovTx) AppendSessionActivity(activity SessionActivityObservation) (bool, error) {
	result, err := g.tx.Exec(`INSERT INTO session_activity_observation(`+sessionActivityCols+`)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(observation_id) DO NOTHING`,
		activity.ObservationID, activity.Runtime, activity.SessionID, activity.State,
		activity.ObservedAt, activity.ValidUntil, activity.EvidenceClass, activity.EntryKind,
		activity.NativeSource, activity.SourceRef, activity.EvidenceDigest, activity.CollectorID,
		activity.CollectorVersion, activity.ReceivedAt, activity.DeliveryAttempts,
		activity.DeliveryMode)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if rows == 1 {
		return true, nil
	}
	existing, err := scanSessionActivity(g.tx.QueryRow(`SELECT `+sessionActivityCols+`
		FROM session_activity_observation WHERE observation_id=?`, activity.ObservationID))
	if err != nil {
		return false, err
	}
	if !sameSessionActivityFact(existing, activity) {
		return false, ErrSessionActivityCollision
	}
	_, err = g.tx.Exec(`UPDATE session_activity_observation
		SET delivery_attempts=MAX(delivery_attempts,?) WHERE observation_id=?`,
		activity.DeliveryAttempts, activity.ObservationID)
	if err != nil {
		return false, err
	}
	return false, nil
}

func sameSessionActivityFact(a, b SessionActivityObservation) bool {
	// EvidenceClass is receipt-time provenance (direct vs late replay), not part of
	// the retry-stable native event identity. The first durable receipt wins.
	a.EvidenceClass, b.EvidenceClass = "", ""
	a.ReceivedAt, b.ReceivedAt = 0, 0
	a.DeliveryAttempts, b.DeliveryAttempts = 0, 0
	a.DeliveryMode, b.DeliveryMode = "", ""
	return a == b
}

// SessionHasOpenActivity reports whether the latest lifecycle fact begins an open
// interval. ValidUntil still bounds what that fact directly proves; this query only
// prevents every later action from manufacturing another "first action" marker.
func (g *GovTx) SessionHasOpenActivity(runtime, sessionID string) (bool, error) {
	var state string
	err := g.tx.QueryRow(`SELECT state FROM session_activity_observation
		WHERE runtime=? AND session_id=? ORDER BY observed_at DESC,rowid DESC LIMIT 1`,
		runtime, sessionID).Scan(&state)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return state == "open", err
}

// SessionActivityObservations returns bounded lifecycle evidence newest first.
func (ix *Index) SessionActivityObservations(runtime, sessionID string, limit int) ([]SessionActivityObservation, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := ix.db.Query(`SELECT `+sessionActivityCols+` FROM session_activity_observation
		WHERE runtime=? AND session_id=? ORDER BY observed_at DESC,rowid DESC LIMIT ?`,
		runtime, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	activities := []SessionActivityObservation{}
	for rows.Next() {
		activity, err := scanSessionActivity(rows)
		if err != nil {
			return nil, err
		}
		activities = append(activities, activity)
	}
	return activities, rows.Err()
}

// SessionActivityByID loads one exact activity observation.
func (ix *Index) SessionActivityByID(observationID string) (SessionActivityObservation, bool, error) {
	activity, err := scanSessionActivity(ix.db.QueryRow(`SELECT `+sessionActivityCols+`
		FROM session_activity_observation WHERE observation_id=?`, observationID))
	if err == sql.ErrNoRows {
		return SessionActivityObservation{}, false, nil
	}
	return activity, err == nil, err
}

// SessionActivityRowID returns the highest durable activity rowid — the
// cursor for cross-session bounded incremental reads (the natural-session
// signal emitter consumes exactly this shape; rowid is stable and append-only
// for this table, matching the durable position pattern).
func (ix *Index) SessionActivityRowID() (int64, error) {
	var rowid sql.NullInt64
	err := ix.db.QueryRow(`SELECT max(rowid) FROM session_activity_observation`).Scan(&rowid)
	if err != nil {
		return 0, err
	}
	return rowid.Int64, nil
}

// SessionActivityAfter returns activity rows with rowid strictly greater than
// after, oldest first, bounded. The natural-session emitter translates exactly
// these rows onto catalog signals; nothing else reads this shape.
// SessionActivityHead is the newest activity rowid (0 when empty).
func (ix *Index) SessionActivityHead() (int64, error) {
	var head int64
	err := ix.db.QueryRow(`SELECT COALESCE(MAX(rowid),0) FROM session_activity_observation`).Scan(&head)
	return head, err
}

func (ix *Index) SessionActivityAfter(after int64, limit int) ([]SessionActivityObservation, int64, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := ix.db.Query(`SELECT rowid,`+sessionActivityCols+` FROM session_activity_observation
		WHERE rowid > ? ORDER BY rowid ASC LIMIT ?`, after, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	activities := []SessionActivityObservation{}
	last := after
	for rows.Next() {
		var rowid int64
		var activity SessionActivityObservation
		if err := rows.Scan(&rowid, &activity.ObservationID, &activity.Runtime, &activity.SessionID,
			&activity.State, &activity.ObservedAt, &activity.ValidUntil, &activity.EvidenceClass,
			&activity.EntryKind, &activity.NativeSource, &activity.SourceRef,
			&activity.EvidenceDigest, &activity.CollectorID, &activity.CollectorVersion,
			&activity.ReceivedAt, &activity.DeliveryAttempts, &activity.DeliveryMode); err != nil {
			return nil, 0, err
		}
		activity.RowID = rowid
		activities = append(activities, activity)
		last = rowid
	}
	return activities, last, rows.Err()
}

// HookLiveSession is one hook-exact liveness candidate for the presence
// sampler (session-presence-honesty plan, Slice B): a session whose newest
// lifecycle row says it is open, with its newest governance-event time for
// freshness.
type HookLiveSession struct {
	Runtime     string `json:"runtime"`
	SessionID   string `json:"session_id"`
	LastEventAt int64  `json:"last_event_at"`
}

// HookLiveSessions returns sessions whose NEWEST lifecycle row — by append
// order (rowid), never observed_at, because hook clocks skew (red-team P1) —
// is not an end, restricted to a lifecycle horizon, joined with the newest
// governance event at or after eventCutoff. A session without a recent event
// does not appear: hook liveness is evidence of recent action, and absence
// of evidence is reported as absence. Bounded and read-only; replay
// artifacts stay inert via the exact entry-kind list (red-team P5).
func (ix *Index) HookLiveSessions(lifecycleHorizon, eventCutoff int64, limit int) ([]HookLiveSession, error) {
	if limit < 1 || limit > 500 {
		limit = 200
	}
	rows, err := ix.db.Query(`
WITH ranked AS (
  SELECT runtime, session_id, entry_kind,
         ROW_NUMBER() OVER (PARTITION BY runtime, session_id ORDER BY rowid DESC) AS recency
  FROM session_activity_observation
  WHERE observed_at >= ?
    AND entry_kind IN ('start','resume','context-reset','context-compact','first-action','end')
), live AS (
  SELECT runtime, session_id FROM ranked WHERE recency = 1 AND entry_kind != 'end'
)
SELECT live.runtime, live.session_id, MAX(event.ts)
FROM live JOIN event ON event.session_id = live.session_id AND event.ts >= ?
GROUP BY live.runtime, live.session_id
ORDER BY 3 DESC, live.runtime ASC, live.session_id ASC
LIMIT ?`, lifecycleHorizon, eventCutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HookLiveSession{}
	for rows.Next() {
		var item HookLiveSession
		if err := rows.Scan(&item.Runtime, &item.SessionID, &item.LastEventAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
