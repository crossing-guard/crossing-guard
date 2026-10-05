package store

// session_message_invocation: one agent-initiated cross-vendor send
// (session-message-cross-vendor-plan §4). The MCP caller has no managed run, so
// the daemon mints this record instead — the ledger home for admission facts
// (digest of caller+target+wrapper bytes), the resolved tier, and the terminal
// outcome. It is NOT the boundary carrier's pending store: the carrier claims
// from session_delivery; this table's by-target index serves the per-target
// budget and the console display only.

import (
	"database/sql"
	"errors"
)

// SessionMessageInvocation is one send request the daemon admitted or refused.
// State vocabulary (plan §4, confirming pass C-1): pending = minted, admitted,
// awaiting the delivery call's receipt; refused = a pre-delivery check failed
// (the record is the admission fact either way); accepted = the transport took
// the message (reachable terminal success for socket targets — transport
// success is never consumption); delivered = a hook boundary's reply carried
// it (hook targets only); expired | unavailable | unknown as the receipt says.
const (
	SessionMessageInvocationPending     = "pending"
	SessionMessageInvocationRefused     = "refused"
	SessionMessageInvocationAccepted    = "accepted"
	SessionMessageInvocationDelivered   = "delivered"
	SessionMessageInvocationExpired     = "expired"
	SessionMessageInvocationUnavailable = "unavailable"
	SessionMessageInvocationUnknown     = "unknown"
)

// SessionMessageInvocation is one row of session_message_invocation.
type SessionMessageInvocation struct {
	InvocationID      string `json:"invocation_id"`
	CallerRuntime     string `json:"caller_runtime"`
	CallerNativeID    string `json:"caller_native_id"`
	TargetRuntime     string `json:"target_runtime"`
	TargetCatalogID   string `json:"target_catalog_id,omitempty"`
	TargetNativeID    string `json:"target_native_id,omitempty"`
	Message           string `json:"message"`
	State             string `json:"state"`
	CallerCanonicalID string `json:"caller_canonical_id,omitempty"`
	TargetCanonicalID string `json:"target_canonical_id,omitempty"`
	Receipt           string `json:"receipt_json,omitempty"`
	Digest            string `json:"digest"`
	CreatedAt         int64  `json:"created_at"`
	SettledAt         int64  `json:"settled_at,omitempty"`
	Detail            string `json:"detail,omitempty"`
	// DeliveryRunID links the pending row the boundary carrier drains for hook
	// targets: the synthetic orun_mcp_<invocation_id> run id (plan RT-10).
	DeliveryRunID string `json:"delivery_run_id,omitempty"`
}

const sessionMessageInvocationCols = `invocation_id,caller_runtime,caller_native_id,target_runtime,
	target_catalog_id,target_native_id,message,state,caller_canonical_id,target_canonical_id,
	receipt,digest,created_at,settled_at,detail,delivery_run_id`

func scanSessionMessageInvocation(row interface{ Scan(...any) error }) (SessionMessageInvocation, error) {
	var r SessionMessageInvocation
	err := row.Scan(&r.InvocationID, &r.CallerRuntime, &r.CallerNativeID, &r.TargetRuntime,
		&r.TargetCatalogID, &r.TargetNativeID, &r.Message, &r.State, &r.CallerCanonicalID,
		&r.TargetCanonicalID, &r.Receipt, &r.Digest, &r.CreatedAt, &r.SettledAt, &r.Detail, &r.DeliveryRunID)
	return r, err
}

// ErrInvocationDuplicateDigest marks D11's refusal: an identical
// caller+target+message digest in a non-refused state is already inside the
// TTL window. Its text carries the first invocation id (postwork PW-6).
type errInvocationDuplicate struct{ firstID string }

func (e errInvocationDuplicate) Error() string {
	return "an identical message is already pending or delivered for this target"
}

// Is makes every errInvocationDuplicate match the sentinel regardless of the
// first id it carries (postwork PW-6: the route's errors.Is must fire).
func (e errInvocationDuplicate) Is(target error) bool {
	_, ok := target.(errInvocationDuplicate)
	return ok
}

// ErrInvocationDuplicateDigest is the sentinel errors.Is matches; the typed
// form carries the first invocation's id.
var ErrInvocationDuplicateDigest = errInvocationDuplicate{}

// InvocationIDFromDuplicateError returns the first invocation id a duplicate
// refusal carries, "" for any other error.
func InvocationIDFromDuplicateError(err error) string {
	var typed errInvocationDuplicate
	if errors.As(err, &typed) {
		return typed.firstID
	}
	return ""
}

// MintSessionMessageInvocation inserts one record in a single transaction and,
// when the send is admitted for a hook target, enqueues the carrier's pending
// row in the SAME transaction (plan RT-8) so two callers racing one target
// cannot pass the cap between the writes. pendingRow may be zero; its
// DeliveryID must be unique. The digest check (D11) counts non-refused rows
// within the window and refuses the duplicate with the first invocation's id
// in the error text.
func (ix *Index) MintSessionMessageInvocation(record SessionMessageInvocation, pendingRow *SessionDelivery, maxPending int) (SessionMessageInvocation, error) {
	if record.InvocationID == "" || record.CallerRuntime == "" || record.TargetRuntime == "" || record.Message == "" {
		return SessionMessageInvocation{}, errors.New("session message invocation requires id, caller runtime, target runtime, and message")
	}
	if record.Digest == "" {
		return SessionMessageInvocation{}, errors.New("session message invocation requires a digest")
	}
	if record.State != SessionMessageInvocationPending && record.State != SessionMessageInvocationRefused {
		return SessionMessageInvocation{}, errors.New("a minted invocation starts pending or refused")
	}
	if pendingRow != nil && (pendingRow.RunID == "" || pendingRow.ExpiresAt <= record.CreatedAt) {
		return SessionMessageInvocation{}, errors.New("the carrier pending row needs a run id and an expiry after the mint")
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return SessionMessageInvocation{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if record.State == SessionMessageInvocationPending {
		if err := ix.mintDigestCheck(tx, record); err != nil {
			return SessionMessageInvocation{}, err
		}
	}
	if _, err := tx.Exec(`INSERT INTO session_message_invocation(`+sessionMessageInvocationCols+`)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(invocation_id) DO NOTHING`,
		record.InvocationID, record.CallerRuntime, record.CallerNativeID, record.TargetRuntime,
		record.TargetCatalogID, record.TargetNativeID, record.Message, record.State,
		record.CallerCanonicalID, record.TargetCanonicalID, record.Receipt,
		record.Digest, record.CreatedAt, record.SettledAt, record.Detail, record.DeliveryRunID); err != nil {
		return SessionMessageInvocation{}, err
	}
	if pendingRow != nil {
		var pending int
		if err := tx.QueryRow(`SELECT count(*) FROM session_delivery WHERE state='pending' AND runtime=? AND native_session_id=? AND delivery_id<>?`,
			pendingRow.Runtime, pendingRow.NativeSessionID, pendingRow.DeliveryID).Scan(&pending); err != nil {
			return SessionMessageInvocation{}, err
		}
		if maxPending > 0 && pending >= maxPending {
			return SessionMessageInvocation{}, ErrSessionDeliveryCap
		}
		if _, err := tx.Exec(`INSERT INTO session_delivery(`+sessionDeliveryCols+`)
			VALUES(?,?,?,?,?,?,?,'pending',?,?,0,'','','',?) ON CONFLICT(delivery_id) DO NOTHING`,
			pendingRow.DeliveryID, pendingRow.RunID, pendingRow.Runtime, pendingRow.NativeSessionID,
			pendingRow.CatalogSessionID, pendingRow.Message, pendingRow.Boundary,
			pendingRow.CreatedAt, pendingRow.ExpiresAt, pendingRow.Detail); err != nil {
			return SessionMessageInvocation{}, err
		}
	}
	return record, tx.Commit()
}

// mintDigestCheck is D11 inside the mint transaction: one bounded look-up of
// the newest non-refused row carrying this digest inside the window.
func (ix *Index) mintDigestCheck(tx *sql.Tx, record SessionMessageInvocation) error {
	var firstID string
	var created int64
	err := tx.QueryRow(`SELECT invocation_id,created_at FROM session_message_invocation
		WHERE digest=? AND state<>'refused' ORDER BY created_at DESC, invocation_id DESC LIMIT 1`,
		record.Digest).Scan(&firstID, &created)
	switch {
	case err == sql.ErrNoRows:
		return nil
	case err != nil:
		return err
	}
	return errInvocationDuplicate{firstID: firstID}
}

// SettleSessionMessageInvocation records one terminal outcome on the record.
// Only a pending (or, for the carrier handoff, delivered-bound) row settles;
// a repeat of the same state is a no-op so the boundary and the sweeper can
// race without double-writing. An unsettled record past its sweeper budget is
// the caller's problem — see StuckSessionMessageInvocations.
func (ix *Index) SettleSessionMessageInvocation(invocationID, state, receipt string, settledAt int64, detail string) error {
	if invocationID == "" || state == "" {
		return errors.New("settling requires an invocation id and a state")
	}
	switch state {
	case SessionMessageInvocationAccepted, SessionMessageInvocationDelivered,
		SessionMessageInvocationExpired, SessionMessageInvocationUnavailable, SessionMessageInvocationUnknown, SessionMessageInvocationRefused:
	default:
		return errors.New("settled state must be terminal")
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.Exec(`UPDATE session_message_invocation SET state=?,receipt=?,settled_at=?,detail=?
		WHERE invocation_id=? AND state IN ('pending','accepted')`,
		state, receipt, settledAt, detail, invocationID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		// Already settled (or the id is unknown). Terminal means terminal.
		return nil
	}
	return tx.Commit()
}

// DeliveryRunPrefix marks synthetic delivery runs minted by the send route.
// Real run ids come from managedID("orun_", …) whose remainder is hash hex —
// never "mcp_" (plan RT-10 + confirming pass) — so a prefix match is exact.
const DeliveryRunPrefix = "orun_mcp_"

// InvocationIDForDeliveryRun returns the invocation id a synthetic delivery
// run id embeds, and whether the run id is one of ours at all.
func InvocationIDForDeliveryRun(runID string) (string, bool) {
	if len(runID) <= len(DeliveryRunPrefix) {
		return "", false
	}
	if runID[:len(DeliveryRunPrefix)] != DeliveryRunPrefix {
		return "", false
	}
	return runID[len(DeliveryRunPrefix):], true
}

// EnqueueCarrierRowForInvocation inserts the boundary carrier's pending row
// for an already-minted invocation (postwork PW-1: the receipt-driven
// dispatch). The per-target cap check and the insert are one transaction; a
// row over the cap leaves no row and the caller settles the record
// unavailable.
func (ix *Index) EnqueueCarrierRowForInvocation(row SessionDelivery, maxPending int) error {
	if row.DeliveryID == "" || row.RunID == "" || row.Runtime == "" || row.NativeSessionID == "" || row.Message == "" {
		return errors.New("the carrier pending row requires id, run, runtime, native session, and message")
	}
	if row.ExpiresAt <= row.CreatedAt {
		return errors.New("the carrier pending row must expire after it is created")
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var pending int
	if err := tx.QueryRow(`SELECT count(*) FROM session_delivery WHERE state='pending' AND runtime=? AND native_session_id=? AND delivery_id<>?`,
		row.Runtime, row.NativeSessionID, row.DeliveryID).Scan(&pending); err != nil {
		return err
	}
	if maxPending > 0 && pending >= maxPending {
		return ErrSessionDeliveryCap
	}
	if _, err := tx.Exec(`INSERT INTO session_delivery(`+sessionDeliveryCols+`)
		VALUES(?,?,?,?,?,?,?,'pending',?,?,0,'','','',?) ON CONFLICT(delivery_id) DO NOTHING`,
		row.DeliveryID, row.RunID, row.Runtime, row.NativeSessionID, row.CatalogSessionID,
		row.Message, row.Boundary, row.CreatedAt, row.ExpiresAt, row.Detail); err != nil {
		return err
	}
	return tx.Commit()
}

// StuckSessionMessageInvocations returns minted records that never settled:
// pending past the TTL (the delivery call died before answering). The
// sweeper settles them unknown (plan RT-4/C-1; postwork PW-2: `accepted` is
// terminal and is never swept — a record whose receipt was recorded keeps its
// true outcome).
func (ix *Index) StuckSessionMessageInvocations(now, ttlSeconds int64, limit int) ([]SessionMessageInvocation, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := ix.db.Query(`SELECT `+sessionMessageInvocationCols+` FROM session_message_invocation
		WHERE state='pending' AND created_at<=?
		ORDER BY created_at ASC LIMIT ?`, now-ttlSeconds, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SessionMessageInvocation{}
	for rows.Next() {
		r, err := scanSessionMessageInvocation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SessionMessageInvocationByID returns one record.
func (ix *Index) SessionMessageInvocationByID(invocationID string) (SessionMessageInvocation, bool, error) {
	r, err := scanSessionMessageInvocation(ix.db.QueryRow(`SELECT `+sessionMessageInvocationCols+`
		FROM session_message_invocation WHERE invocation_id=?`, invocationID))
	if err == sql.ErrNoRows {
		return SessionMessageInvocation{}, false, nil
	}
	if err != nil {
		return SessionMessageInvocation{}, false, err
	}
	return r, true, nil
}

// CountRecentInvocationsByCaller is the per-caller window's substrate (plan
// RT-12): non-refused sends by one caller identity inside the window.
func (ix *Index) CountRecentInvocationsByCaller(callerRuntime, callerNativeID string, sinceUnix int64) (int, error) {
	var n int
	err := ix.db.QueryRow(`SELECT count(*) FROM session_message_invocation
		WHERE state<>'refused' AND caller_runtime=? AND caller_native_id=? AND created_at>?`,
		callerRuntime, callerNativeID, sinceUnix).Scan(&n)
	return n, err
}

// CountRecentInvocationsForTarget is the per-target budget's invocation half
// (plan D10): non-refused sends targeted at one session inside the window.
// The target is named by native id or, when the record holds only the catalog
// half, by that.
func (ix *Index) CountRecentInvocationsForTarget(targetRuntime, nativeID, catalogID string, sinceUnix int64) (int, error) {
	var n int
	err := ix.db.QueryRow(`SELECT count(*) FROM session_message_invocation
		WHERE state IN ('pending','accepted','delivered') AND target_runtime=?
		AND (target_native_id=? OR (target_native_id='' AND target_catalog_id<>'' AND target_catalog_id=?))
		AND created_at>?`, targetRuntime, nativeID, catalogID, sinceUnix).Scan(&n)
	return n, err
}

// RecentAcceptedEdges returns the caller→target pairs of accepted and delivered
// records inside the window — the cycle detector's substrate (plan D9/RT-5).
// Refused, expired, and unknown records form no edge.
func (ix *Index) RecentAcceptedEdges(sinceUnix int64, limit int) ([]SessionMessageInvocation, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := ix.db.Query(`SELECT `+sessionMessageInvocationCols+` FROM session_message_invocation
		WHERE state IN ('accepted','delivered') AND created_at>? ORDER BY created_at DESC LIMIT ?`,
		sinceUnix, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SessionMessageInvocation{}
	for rows.Next() {
		r, err := scanSessionMessageInvocation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecentSessionMessageInvocations returns the newest records first, capped —
// the Agents pages' by-caller/by-target read.
func (ix *Index) RecentSessionMessageInvocations(limit int) ([]SessionMessageInvocation, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := ix.db.Query(`SELECT `+sessionMessageInvocationCols+` FROM session_message_invocation
		ORDER BY created_at DESC, invocation_id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SessionMessageInvocation{}
	for rows.Next() {
		r, err := scanSessionMessageInvocation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountSessionMessageInvocations returns the table's row count — the doctor's
// report-only probe uses it; it writes nothing.
func (ix *Index) CountSessionMessageInvocations() (int, error) {
	var n int
	err := ix.db.QueryRow(`SELECT count(*) FROM session_message_invocation`).Scan(&n)
	return n, err
}
