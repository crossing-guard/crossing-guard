package store

// session_delivery: one helper message waiting for the target session's own
// next boundary (helper-session-attachment plan D5). It is the delivery
// attempt in its queued state, not a broker: one row names one session, is
// claimed by exactly one boundary (claim-then-respond, inside the ingest
// critical section), never fans out, never retries, and expires visibly.

import (
	"database/sql"
	"errors"
	"fmt"
)

const sessionDeliverySchemaV29 = `
CREATE TABLE IF NOT EXISTS session_delivery(
  delivery_id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL,
  runtime TEXT NOT NULL,
  native_session_id TEXT NOT NULL,
  catalog_session_id TEXT NOT NULL DEFAULT '',
  message TEXT NOT NULL,
  boundary TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL CHECK(state IN ('pending','delivered','expired')),
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  delivered_at INTEGER NOT NULL DEFAULT 0,
  delivered_kind TEXT NOT NULL DEFAULT '',
  delivered_native_call_id TEXT NOT NULL DEFAULT '',
  delivered_observation_id TEXT NOT NULL DEFAULT '',
  detail TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS session_delivery_pending
  ON session_delivery(state,runtime,native_session_id,created_at);
CREATE INDEX IF NOT EXISTS session_delivery_run ON session_delivery(run_id);
`

// SessionDelivery is one pending or settled helper message for one session.
type SessionDelivery struct {
	DeliveryID             string `json:"delivery_id"`
	RunID                  string `json:"run_id"`
	Runtime                string `json:"runtime"`
	NativeSessionID        string `json:"native_session_id"`
	CatalogSessionID       string `json:"catalog_session_id,omitempty"`
	Message                string `json:"message"`
	Boundary               string `json:"boundary,omitempty"`
	State                  string `json:"state"` // pending | delivered | expired
	CreatedAt              int64  `json:"created_at"`
	ExpiresAt              int64  `json:"expires_at"`
	DeliveredAt            int64  `json:"delivered_at,omitempty"`
	DeliveredKind          string `json:"delivered_kind,omitempty"`
	DeliveredNativeCallID  string `json:"delivered_native_call_id,omitempty"`
	DeliveredObservationID string `json:"delivered_observation_id,omitempty"`
	Detail                 string `json:"detail,omitempty"`
}

// ErrSessionDeliveryCap is returned when the target session already holds the
// configured number of pending records; the caller records `pending_cap`.
var ErrSessionDeliveryCap = errors.New("session delivery pending cap reached")

const sessionDeliveryCols = `delivery_id,run_id,runtime,native_session_id,catalog_session_id,message,
	boundary,state,created_at,expires_at,delivered_at,delivered_kind,delivered_native_call_id,
	delivered_observation_id,detail`

func scanSessionDelivery(row interface{ Scan(...any) error }) (SessionDelivery, error) {
	var d SessionDelivery
	err := row.Scan(&d.DeliveryID, &d.RunID, &d.Runtime, &d.NativeSessionID, &d.CatalogSessionID,
		&d.Message, &d.Boundary, &d.State, &d.CreatedAt, &d.ExpiresAt, &d.DeliveredAt,
		&d.DeliveredKind, &d.DeliveredNativeCallID, &d.DeliveredObservationID, &d.Detail)
	return d, err
}

// EnqueueSessionDelivery records one pending message for the exact session,
// refusing a session that already holds maxPending pending records. A repeat
// of the same delivery id is a no-op (the run owns the id).
func (ix *Index) EnqueueSessionDelivery(d SessionDelivery, maxPending int) error {
	if d.DeliveryID == "" || d.RunID == "" || d.Runtime == "" || d.NativeSessionID == "" || d.Message == "" {
		return errors.New("session delivery requires id, run, runtime, native session, and message")
	}
	if d.ExpiresAt <= d.CreatedAt {
		return errors.New("session delivery must expire after it is created")
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var pending int
	if err := tx.QueryRow(`SELECT count(*) FROM session_delivery WHERE state='pending' AND runtime=? AND native_session_id=? AND delivery_id<>?`,
		d.Runtime, d.NativeSessionID, d.DeliveryID).Scan(&pending); err != nil {
		return err
	}
	if maxPending > 0 && pending >= maxPending {
		return ErrSessionDeliveryCap
	}
	if _, err := tx.Exec(`INSERT INTO session_delivery(`+sessionDeliveryCols+`)
		VALUES(?,?,?,?,?,?,?,'pending',?,?,0,'','','',?) ON CONFLICT(delivery_id) DO NOTHING`,
		d.DeliveryID, d.RunID, d.Runtime, d.NativeSessionID, d.CatalogSessionID, d.Message,
		d.Boundary, d.CreatedAt, d.ExpiresAt, d.Detail); err != nil {
		return err
	}
	return tx.Commit()
}

// Bounds on one boundary's claim: how many records the reader examines, and
// the default byte budget when the caller passes none. The byte budget is the
// caller's policy (configuration); these are mechanical ceilings.
const (
	sessionDeliveryClaimScan    = 50
	sessionDeliveryClaimDefault = 7000
	sessionDeliveryExpireScan   = 200
)

// SessionDeliveryClaim is one boundary's claim: the session, the observation that
// carries it, and what the boundary may carry.
type SessionDeliveryClaim struct {
	Runtime, SessionID string
	Kind               string // OUR observation kind
	ObservationID      string
	NativeCallID       string
	Now                int64
	MaxBytes           int
	// CarriesHandoff says this boundary may carry a handoff's brief (a row whose
	// run_id has HandoffDeliveryRunPrefix): true only at a kind where the runtime's
	// hook can tell a nested call from the session's own, so a brief is never handed
	// to a child (team rest-of-release plan §6.5, K-1). Helper rows are unaffected.
	CarriesHandoff bool
}

// ClaimSessionDeliveries is ClaimSessionDeliveriesAt for a boundary that carries no
// handoff brief — the default, so a caller that does not say so never hands one over.
func (ix *Index) ClaimSessionDeliveries(runtime, sessionID, kind, observationID, nativeCallID string, now int64, maxBytes int) ([]SessionDelivery, error) {
	return ix.ClaimSessionDeliveriesAt(SessionDeliveryClaim{Runtime: runtime, SessionID: sessionID, Kind: kind,
		ObservationID: observationID, NativeCallID: nativeCallID, Now: now, MaxBytes: maxBytes})
}

// ClaimSessionDeliveriesAt marks pending records for the session delivered at
// this boundary until the next record would push the drained bytes past
// MaxBytes (the first record always drains), and returns them; records past the
// budget stay pending for the next boundary. A handoff's brief sorts first,
// ahead of older helper rows, so the encoder's own cap can only ever cut what
// follows it; the rest drain in creation order. The
// session may be named by either identity. The claim commits before the
// caller responds, so a boundary that dies after reading loses the message
// rather than seeing it again (invariant 6). Records past their expiry are
// left for expiry. An empty session takes no write transaction.
func (ix *Index) ClaimSessionDeliveriesAt(claim SessionDeliveryClaim) ([]SessionDelivery, error) {
	runtime, sessionID, kind, now, maxBytes := claim.Runtime, claim.SessionID, claim.Kind, claim.Now, claim.MaxBytes
	if runtime == "" || sessionID == "" || kind == "" {
		return nil, nil
	}
	if maxBytes <= 0 {
		maxBytes = sessionDeliveryClaimDefault
	}
	// Rows this boundary may not carry are invisible to it and stay pending.
	carried := ` AND run_id NOT LIKE '` + HandoffDeliveryRunPrefix + `%'`
	if claim.CarriesHandoff {
		carried = ""
	}
	var pending int
	if err := ix.db.QueryRow(`SELECT count(*) FROM session_delivery WHERE state='pending' AND runtime=? AND (native_session_id=? OR catalog_session_id=?) AND expires_at>?`+carried,
		runtime, sessionID, sessionID, now).Scan(&pending); err != nil {
		return nil, err
	}
	if pending == 0 {
		return nil, nil
	}
	limit := sessionDeliveryClaimScan
	tx, err := ix.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.Query(`SELECT `+sessionDeliveryCols+` FROM session_delivery
		WHERE state='pending' AND runtime=? AND (native_session_id=? OR catalog_session_id=?) AND expires_at>?`+carried+`
		ORDER BY (run_id LIKE '`+HandoffDeliveryRunPrefix+`%') DESC, created_at ASC, delivery_id ASC LIMIT ?`, runtime, sessionID, sessionID, now, limit)
	if err != nil {
		return nil, err
	}
	claimed := []SessionDelivery{}
	drained := 0
	for rows.Next() {
		d, err := scanSessionDelivery(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if len(claimed) > 0 && drained+len(d.Message) > maxBytes {
			break
		}
		drained += len(d.Message)
		claimed = append(claimed, d)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for index := range claimed {
		if _, err := tx.Exec(`UPDATE session_delivery SET state='delivered',delivered_at=?,delivered_kind=?,
			delivered_native_call_id=?,delivered_observation_id=? WHERE delivery_id=? AND state='pending'`,
			now, kind, claim.NativeCallID, claim.ObservationID, claimed[index].DeliveryID); err != nil {
			return nil, err
		}
		claimed[index].State, claimed[index].DeliveredAt = "delivered", now
		claimed[index].DeliveredKind, claimed[index].DeliveredNativeCallID = kind, claim.NativeCallID
		claimed[index].DeliveredObservationID = claim.ObservationID
	}
	if len(claimed) == 0 {
		return claimed, nil
	}
	return claimed, tx.Commit()
}

// ReleaseSessionDeliveries returns records one boundary claimed to pending
// when the reply carrying them never reached the hook (delivery-claim-on-reply
// plan D4). Only rows still marked delivered by that same observation are
// touched, so a record a later boundary re-claimed is never disturbed. A
// released record is indistinguishable from one never claimed. A handoff's brief
// is the exception: while it was handed over, its handoff may have been
// withdrawn, its claim moved, or the device given up on it, and none of those
// could cancel a row that was not pending. Such a row ends here, expired with
// the named detail, instead of going back to pending (handoffBriefReleaseEndTx).
func (ix *Index) ReleaseSessionDeliveries(deliveryIDs []string, observationID string) (int64, error) {
	if len(deliveryIDs) == 0 || observationID == "" {
		return 0, nil
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var released int64
	for _, id := range deliveryIDs {
		ended, err := handoffBriefReleaseEndTx(tx, id)
		if err != nil {
			return 0, err
		}
		if ended != "" {
			if _, err := tx.Exec(`UPDATE session_delivery SET state='expired',detail=?,delivered_at=0,delivered_kind='',
				delivered_native_call_id='',delivered_observation_id='' WHERE delivery_id=? AND state='delivered' AND delivered_observation_id=?`,
				ended, id, observationID); err != nil {
				return 0, err
			}
			continue
		}
		result, err := tx.Exec(`UPDATE session_delivery SET state='pending',delivered_at=0,delivered_kind='',
			delivered_native_call_id='',delivered_observation_id='' WHERE delivery_id=? AND state='delivered' AND delivered_observation_id=?`,
			id, observationID)
		if err != nil {
			return 0, err
		}
		n, _ := result.RowsAffected()
		released += n
	}
	return released, tx.Commit()
}

// ExpireSessionDeliveries marks pending records past their expiry as expired
// and returns them so the owning runs can record the outcome.
func (ix *Index) ExpireSessionDeliveries(now int64) ([]SessionDelivery, error) {
	tx, err := ix.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.Query(`SELECT `+sessionDeliveryCols+` FROM session_delivery WHERE state='pending' AND expires_at<=? ORDER BY created_at ASC LIMIT ?`, now, sessionDeliveryExpireScan)
	if err != nil {
		return nil, err
	}
	expired := []SessionDelivery{}
	for rows.Next() {
		d, err := scanSessionDelivery(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		expired = append(expired, d)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for index := range expired {
		if _, err := tx.Exec(`UPDATE session_delivery SET state='expired',detail=? WHERE delivery_id=? AND state='pending'`,
			fmt.Sprintf("no carrier boundary before expiry at %d", expired[index].ExpiresAt), expired[index].DeliveryID); err != nil {
			return nil, err
		}
		expired[index].State = "expired"
	}
	if len(expired) == 0 {
		return expired, nil
	}
	return expired, tx.Commit()
}

// SessionDeliveryForRun returns the delivery record a run enqueued, if any.
func (ix *Index) SessionDeliveryForRun(runID string) (SessionDelivery, bool, error) {
	d, err := scanSessionDelivery(ix.db.QueryRow(`SELECT `+sessionDeliveryCols+` FROM session_delivery WHERE run_id=? ORDER BY created_at DESC LIMIT 1`, runID))
	if errors.Is(err, sql.ErrNoRows) {
		return SessionDelivery{}, false, nil
	}
	if err != nil {
		return SessionDelivery{}, false, err
	}
	return d, true, nil
}
