package store

// usage_call, usage_call_copy, usage_source, usage_prune: the model calls the
// usage recorder reads from vendor sources (token-usage-analytics plan §3.4-
// §3.6). A row records an OBSERVED call; it outlives the vendor file it came
// from, because vendors prune their own history (Claude after 30 days). Rows
// hold counts and opaque ids only, never prompt, output, title or path text,
// and nothing here feeds sync_outbox.
//
// One call is one row, keyed by (runtime, call_id). When several sources hold
// the same call (a resumed or forked transcript copies history), one owns the
// row and the others are remembered in usage_call_copy, so a copy can take
// over if the owner's source is rewritten without it (plan §3.5, red-team
// S3-1). Only a completed re-read of a live source and an explicit prune
// delete rows.

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
)

const usageCallSchema = `
CREATE TABLE IF NOT EXISTS usage_call(
  runtime TEXT NOT NULL CHECK(length(runtime) > 0),
  call_id TEXT NOT NULL CHECK(length(call_id) > 0),
  session_id TEXT NOT NULL CHECK(length(session_id) > 0),
  parent_session_id TEXT NOT NULL DEFAULT '',
  agent TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL CHECK(length(source) > 0),
  source_born_ms INTEGER,
  epoch INTEGER NOT NULL,
  first_at_ms INTEGER NOT NULL,
  at_ms INTEGER NOT NULL,
  model TEXT NOT NULL DEFAULT '',
  effort TEXT NOT NULL DEFAULT '',
  client TEXT NOT NULL DEFAULT '',
  input INTEGER, cache_read INTEGER, cache_write INTEGER, output INTEGER, reasoning INTEGER,
  context_window INTEGER,
  cost_amount REAL, cost_unit TEXT, cost_basis TEXT,
  parts TEXT NOT NULL DEFAULT '',
  other TEXT NOT NULL DEFAULT '',
  reader TEXT NOT NULL,
  observed_at_ms INTEGER NOT NULL,
  PRIMARY KEY(runtime, call_id)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS usage_call_at ON usage_call(at_ms);
CREATE INDEX IF NOT EXISTS usage_call_session ON usage_call(runtime, session_id);
CREATE INDEX IF NOT EXISTS usage_call_source ON usage_call(runtime, source, epoch);
CREATE TABLE IF NOT EXISTS usage_call_copy(
  runtime TEXT NOT NULL,
  call_id TEXT NOT NULL,
  source TEXT NOT NULL,
  epoch INTEGER NOT NULL,
  session_id TEXT NOT NULL,
  parent_session_id TEXT NOT NULL DEFAULT '',
  agent TEXT NOT NULL DEFAULT '',
  source_born_ms INTEGER,
  first_at_ms INTEGER NOT NULL,
  at_ms INTEGER NOT NULL,
  PRIMARY KEY(runtime, call_id, source)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS usage_call_copy_source ON usage_call_copy(runtime, source, epoch);
CREATE INDEX IF NOT EXISTS usage_call_copy_at ON usage_call_copy(at_ms);
CREATE TABLE IF NOT EXISTS usage_source(
  runtime TEXT NOT NULL CHECK(length(runtime) > 0),
  source TEXT NOT NULL CHECK(length(source) > 0),
  session_id TEXT NOT NULL,
  session_alias TEXT NOT NULL DEFAULT '',
  parent_session_id TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL DEFAULT '',
  marker TEXT NOT NULL,
  cursor BLOB,
  reader TEXT NOT NULL,
  epoch INTEGER NOT NULL,
  complete INTEGER NOT NULL CHECK(complete IN (0,1)),
  updated_at_ms INTEGER NOT NULL,
  PRIMARY KEY(runtime, source)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS usage_source_parent ON usage_source(runtime, parent_session_id) WHERE parent_session_id <> '';
CREATE INDEX IF NOT EXISTS usage_source_session ON usage_source(runtime, session_id);
CREATE TABLE IF NOT EXISTS usage_prune(id INTEGER PRIMARY KEY CHECK(id = 1), horizon_ms INTEGER NOT NULL);
`

// UsageCallRecord is one call as the store keeps it. Counts are nil when the
// runtime did not state them; Parts and Other are canonical JSON or "".
type UsageCallRecord struct {
	Runtime, CallID                   string
	SessionID, ParentSessionID, Agent string
	Source                            string
	SourceBornMS                      *int64 // nil when the platform reports no birth time
	FirstAtMS, AtMS                   int64
	Model, Effort, Client             string
	Input, CacheRead, CacheWrite      *int64
	Output, Reasoning, ContextWindow  *int64
	Cost                              *UsageCost
	Parts, Other                      string
	Reader                            string
}

// UsageCost is one call's runtime-stated cost in one unit and basis.
type UsageCost struct {
	Amount      float64
	Unit, Basis string
}

// UsageSourceState is what the recorder keeps per native source.
type UsageSourceState struct {
	Runtime, Source                        string
	SessionID, SessionAlias, ParentSession string
	// Role is the vendor-published agent type of a delegated source, an opaque
	// type name ("general-purpose", "guardian"); never prompt or title text.
	Role           string
	Marker, Reader string
	Cursor         []byte
	Epoch          int64
	Complete       bool
	UpdatedAtMS    int64
}

// UsageSourceWrite is one read of one source, written in one transaction.
// Restart means the read began the source again (it was rewritten, or its
// reader changed), so this write opens a new epoch.
type UsageSourceWrite struct {
	State   UsageSourceState
	Calls   []UsageCallRecord
	Restart bool
}

// UsageWriteResult counts what one source write did.
type UsageWriteResult struct {
	Written, Copies, BelowHorizon, Removed, Promoted int
	Epoch                                            int64
}

// ErrUsageWriteInvalid rejects a write whose state or calls do not name the
// same runtime and source.
var ErrUsageWriteInvalid = errors.New("usage source write is invalid")

// UsageSourceStates returns the recorder's state for every source of one
// runtime, by source key.
func (ix *Index) UsageSourceStates(runtime string) (map[string]UsageSourceState, error) {
	rows, err := ix.db.Query(`SELECT runtime,source,session_id,session_alias,parent_session_id,role,marker,
		cursor,reader,epoch,complete,updated_at_ms FROM usage_source WHERE runtime=?`, runtime)
	if err != nil {
		return nil, fmt.Errorf("read usage sources: %w", err)
	}
	defer rows.Close()
	out := map[string]UsageSourceState{}
	for rows.Next() {
		var state UsageSourceState
		if err := rows.Scan(&state.Runtime, &state.Source, &state.SessionID, &state.SessionAlias,
			&state.ParentSession, &state.Role, &state.Marker, &state.Cursor, &state.Reader, &state.Epoch,
			&state.Complete, &state.UpdatedAtMS); err != nil {
			return nil, err
		}
		out[state.Source] = state
	}
	return out, rows.Err()
}

// WriteUsageSource writes one source's calls and its new state in one
// transaction: calls older than the prune horizon are dropped, a restart opens
// a new epoch, and a write that completes the source removes that source's
// rows from older epochs after promoting any copy another source holds.
func (ix *Index) WriteUsageSource(write UsageSourceWrite) (UsageWriteResult, error) {
	state := write.State
	if state.Runtime == "" || state.Source == "" || state.SessionID == "" {
		return UsageWriteResult{}, ErrUsageWriteInvalid
	}
	for _, call := range write.Calls {
		if call.Runtime != state.Runtime || call.Source != state.Source || call.CallID == "" || call.SessionID == "" {
			return UsageWriteResult{}, ErrUsageWriteInvalid
		}
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return UsageWriteResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := writeUsageSourceTx(tx, write)
	if err != nil {
		return UsageWriteResult{}, err
	}
	return result, tx.Commit()
}

func writeUsageSourceTx(tx *sql.Tx, write UsageSourceWrite) (UsageWriteResult, error) {
	state := write.State
	result := UsageWriteResult{Epoch: 1}
	var previousEpoch int64
	err := tx.QueryRow(`SELECT epoch FROM usage_source WHERE runtime=? AND source=?`,
		state.Runtime, state.Source).Scan(&previousEpoch)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return result, fmt.Errorf("read usage source: %w", err)
	case write.Restart:
		result.Epoch = previousEpoch + 1
	default:
		result.Epoch = previousEpoch
	}
	horizon, err := usagePruneHorizon(tx)
	if err != nil {
		return result, err
	}
	for _, call := range write.Calls {
		if call.AtMS < horizon {
			result.BelowHorizon++
			continue
		}
		copied, err := writeUsageCall(tx, call, result.Epoch, state.UpdatedAtMS)
		if err != nil {
			return result, err
		}
		if copied {
			result.Copies++
		} else {
			result.Written++
		}
	}
	if state.Complete {
		removed, promoted, err := removeStaleUsageRows(tx, state.Runtime, state.Source, result.Epoch)
		if err != nil {
			return result, err
		}
		result.Removed, result.Promoted = removed, promoted
	}
	// A role is a fixed vendor fact of its source: an empty one (a listing that
	// cannot state it, a malformed cursor state) never overwrites a stored one
	// (session usage breakdown plan S-1).
	_, err = tx.Exec(`INSERT INTO usage_source(runtime,source,session_id,session_alias,parent_session_id,
		role,marker,cursor,reader,epoch,complete,updated_at_ms) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(runtime,source) DO UPDATE SET session_id=excluded.session_id,
		session_alias=excluded.session_alias, parent_session_id=excluded.parent_session_id,
		role=CASE WHEN excluded.role<>'' THEN excluded.role ELSE usage_source.role END,
		marker=excluded.marker, cursor=excluded.cursor, reader=excluded.reader, epoch=excluded.epoch,
		complete=excluded.complete, updated_at_ms=excluded.updated_at_ms`,
		state.Runtime, state.Source, state.SessionID, state.SessionAlias, state.ParentSession, state.Role,
		state.Marker, state.Cursor, state.Reader, result.Epoch, state.Complete, state.UpdatedAtMS)
	if err != nil {
		return result, fmt.Errorf("write usage source: %w", err)
	}
	return result, nil
}

// UpdateUsageSourceRoles stores roles, by source key, on one runtime's known
// sources in one transaction. It touches no cursor, marker, epoch, completion
// or updated_at_ms, so no epoch or copy logic runs and no as-of moves; an
// empty role is skipped (S-1). It returns how many rows changed.
func (ix *Index) UpdateUsageSourceRoles(runtime string, roles map[string]string) (int, error) {
	tx, err := ix.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	changed := 0
	for source, role := range roles {
		if role == "" {
			continue
		}
		result, err := tx.Exec(`UPDATE usage_source SET role=? WHERE runtime=? AND source=? AND role<>?`,
			role, runtime, source, role)
		if err != nil {
			return 0, fmt.Errorf("write usage source role: %w", err)
		}
		if n, err := result.RowsAffected(); err == nil {
			changed += int(n)
		}
	}
	return changed, tx.Commit()
}

// usageOwner is who holds a call's row, and the facts that order holders.
type usageOwner struct {
	source, sessionID, parentSessionID, agent string
	bornMS                                    *int64
	firstAtMS, atMS, epoch                    int64
}

// before reports whether a precedes b under the attribution rule (plan §3.5,
// D-4): earlier first line, then earlier-born source (absent last), then the
// smaller session id, then the smaller source key.
func (a usageOwner) before(b usageOwner) bool {
	if a.firstAtMS != b.firstAtMS {
		return a.firstAtMS < b.firstAtMS
	}
	if bornA, bornB := bornOrLast(a.bornMS), bornOrLast(b.bornMS); bornA != bornB {
		return bornA < bornB
	}
	if a.sessionID != b.sessionID {
		return a.sessionID < b.sessionID
	}
	return a.source < b.source
}

func bornOrLast(born *int64) int64 {
	if born == nil {
		return math.MaxInt64
	}
	return *born
}

func ownerOf(call UsageCallRecord, epoch int64) usageOwner {
	return usageOwner{source: call.Source, sessionID: call.SessionID, parentSessionID: call.ParentSessionID,
		agent: call.Agent, bornMS: call.SourceBornMS, firstAtMS: call.FirstAtMS, atMS: call.AtMS, epoch: epoch}
}

// writeUsageCall applies the attribution rule to one call and reports whether
// it was kept only as a copy.
func writeUsageCall(tx *sql.Tx, call UsageCallRecord, epoch, nowMS int64) (bool, error) {
	var current usageOwner
	err := tx.QueryRow(`SELECT source,session_id,parent_session_id,agent,source_born_ms,first_at_ms,at_ms,epoch
		FROM usage_call WHERE runtime=? AND call_id=?`, call.Runtime, call.CallID).Scan(&current.source,
		&current.sessionID, &current.parentSessionID, &current.agent, &current.bornMS, &current.firstAtMS,
		&current.atMS, &current.epoch)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, insertUsageCall(tx, call, epoch, nowMS)
	case err != nil:
		return false, fmt.Errorf("read usage call: %w", err)
	}
	candidate := ownerOf(call, epoch)
	// A holder whose read began after this call's first line (the line fell in
	// an earlier read) compares with the earliest first line it ever stated
	// (code red-team C-9).
	var earlier sql.NullInt64
	if err := tx.QueryRow(`SELECT first_at_ms FROM usage_call_copy WHERE runtime=? AND call_id=? AND source=?`,
		call.Runtime, call.CallID, call.Source).Scan(&earlier); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("read usage copy: %w", err)
	}
	if earlier.Valid && earlier.Int64 < candidate.firstAtMS {
		candidate.firstAtMS = earlier.Int64
		call.FirstAtMS = earlier.Int64
	}
	if current.source == call.Source {
		// The owner re-read: a later line of the same call (a streaming
		// snapshot) updates the figures; the first line's time is kept.
		call.FirstAtMS = min(call.FirstAtMS, current.firstAtMS)
		return false, updateUsageCall(tx, call, epoch)
	}
	if !candidate.before(current) {
		return true, rememberUsageCopy(tx, call.Runtime, call.CallID, candidate)
	}
	if err := rememberUsageCopy(tx, call.Runtime, call.CallID, current); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`DELETE FROM usage_call_copy WHERE runtime=? AND call_id=? AND source=?`,
		call.Runtime, call.CallID, call.Source); err != nil {
		return false, err
	}
	return false, updateUsageCall(tx, call, epoch)
}

func insertUsageCall(tx *sql.Tx, call UsageCallRecord, epoch, nowMS int64) error {
	amount, unit, basis := usageCostColumns(call.Cost)
	_, err := tx.Exec(`INSERT INTO usage_call(runtime,call_id,session_id,parent_session_id,agent,source,
		source_born_ms,epoch,first_at_ms,at_ms,model,effort,client,input,cache_read,cache_write,output,
		reasoning,context_window,cost_amount,cost_unit,cost_basis,parts,other,reader,observed_at_ms)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		call.Runtime, call.CallID, call.SessionID, call.ParentSessionID, call.Agent, call.Source,
		call.SourceBornMS, epoch, call.FirstAtMS, call.AtMS, call.Model, call.Effort, call.Client,
		call.Input, call.CacheRead, call.CacheWrite, call.Output, call.Reasoning, call.ContextWindow,
		amount, unit, basis, call.Parts, call.Other, call.Reader, nowMS)
	if err != nil {
		return fmt.Errorf("insert usage call: %w", err)
	}
	return nil
}

func updateUsageCall(tx *sql.Tx, call UsageCallRecord, epoch int64) error {
	amount, unit, basis := usageCostColumns(call.Cost)
	_, err := tx.Exec(`UPDATE usage_call SET session_id=?,parent_session_id=?,agent=?,source=?,
		source_born_ms=?,epoch=?,first_at_ms=?,at_ms=?,model=?,effort=?,client=?,input=?,cache_read=?,
		cache_write=?,output=?,reasoning=?,context_window=?,cost_amount=?,cost_unit=?,cost_basis=?,
		parts=?,other=?,reader=? WHERE runtime=? AND call_id=?`,
		call.SessionID, call.ParentSessionID, call.Agent, call.Source, call.SourceBornMS, epoch,
		call.FirstAtMS, call.AtMS, call.Model, call.Effort, call.Client, call.Input, call.CacheRead,
		call.CacheWrite, call.Output, call.Reasoning, call.ContextWindow, amount, unit, basis,
		call.Parts, call.Other, call.Reader, call.Runtime, call.CallID)
	if err != nil {
		return fmt.Errorf("update usage call: %w", err)
	}
	return nil
}

func rememberUsageCopy(tx *sql.Tx, runtime, callID string, holder usageOwner) error {
	_, err := tx.Exec(`INSERT INTO usage_call_copy(runtime,call_id,source,epoch,session_id,parent_session_id,
		agent,source_born_ms,first_at_ms,at_ms) VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(runtime,call_id,source) DO UPDATE SET epoch=excluded.epoch,session_id=excluded.session_id,
		parent_session_id=excluded.parent_session_id,agent=excluded.agent,source_born_ms=excluded.source_born_ms,
		first_at_ms=min(usage_call_copy.first_at_ms,excluded.first_at_ms),at_ms=excluded.at_ms`,
		runtime, callID, holder.source, holder.epoch, holder.sessionID, holder.parentSessionID, holder.agent,
		holder.bornMS, holder.firstAtMS, holder.atMS)
	if err != nil {
		return fmt.Errorf("remember usage copy: %w", err)
	}
	return nil
}

// removeStaleUsageRows runs when a source's read completes. Rows that source
// owns from an older epoch are calls it no longer holds: each passes to the
// best holder among OTHER sources' copies, or is removed. Its own older copy
// rows go too, so a stale copy can never be promoted later (S3-1).
func removeStaleUsageRows(tx *sql.Tx, runtime, source string, epoch int64) (int, int, error) {
	rows, err := tx.Query(`SELECT call_id FROM usage_call WHERE runtime=? AND source=? AND epoch<?`,
		runtime, source, epoch)
	if err != nil {
		return 0, 0, fmt.Errorf("find stale usage calls: %w", err)
	}
	var stale []string
	for rows.Next() {
		var callID string
		if err := rows.Scan(&callID); err != nil {
			rows.Close()
			return 0, 0, err
		}
		stale = append(stale, callID)
	}
	if err := rows.Close(); err != nil {
		return 0, 0, err
	}
	removed, promoted := 0, 0
	for _, callID := range stale {
		took, err := promoteUsageCopy(tx, runtime, callID, source)
		if err != nil {
			return removed, promoted, err
		}
		if took {
			promoted++
			continue
		}
		if _, err := tx.Exec(`DELETE FROM usage_call WHERE runtime=? AND call_id=?`, runtime, callID); err != nil {
			return removed, promoted, fmt.Errorf("remove stale usage call: %w", err)
		}
		removed++
	}
	if _, err := tx.Exec(`DELETE FROM usage_call_copy WHERE runtime=? AND source=? AND epoch<?`,
		runtime, source, epoch); err != nil {
		return removed, promoted, fmt.Errorf("remove stale usage copies: %w", err)
	}
	return removed, promoted, nil
}

// promoteUsageCopy hands a call's row to the best holder among other sources.
// Copy rows keep identity only: copies state identical figures, so the row's
// figures and reader stay as they are.
func promoteUsageCopy(tx *sql.Tx, runtime, callID, excluded string) (bool, error) {
	var holder usageOwner
	err := tx.QueryRow(`SELECT source,epoch,session_id,parent_session_id,agent,source_born_ms,first_at_ms,at_ms
		FROM usage_call_copy WHERE runtime=? AND call_id=? AND source<>?
		ORDER BY first_at_ms, COALESCE(source_born_ms, 9223372036854775807), session_id, source LIMIT 1`,
		runtime, callID, excluded).Scan(&holder.source, &holder.epoch, &holder.sessionID,
		&holder.parentSessionID, &holder.agent, &holder.bornMS, &holder.firstAtMS, &holder.atMS)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("find usage copy: %w", err)
	}
	if _, err := tx.Exec(`UPDATE usage_call SET source=?,epoch=?,session_id=?,parent_session_id=?,agent=?,
		source_born_ms=?,first_at_ms=? WHERE runtime=? AND call_id=?`, holder.source, holder.epoch,
		holder.sessionID, holder.parentSessionID, holder.agent, holder.bornMS, holder.firstAtMS,
		runtime, callID); err != nil {
		return false, fmt.Errorf("promote usage copy: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM usage_call_copy WHERE runtime=? AND call_id=? AND source=?`,
		runtime, callID, holder.source); err != nil {
		return false, err
	}
	return true, nil
}

func usageCostColumns(cost *UsageCost) (any, any, any) {
	if cost == nil {
		return nil, nil, nil
	}
	return cost.Amount, cost.Unit, cost.Basis
}

// usagePruneHorizon is the largest horizon ever pruned, in milliseconds; 0
// when nothing was pruned. It is read inside the recorder's write
// transaction because prune runs in another process (S3-5).
func usagePruneHorizon(q interface {
	QueryRow(string, ...any) *sql.Row
}) (int64, error) {
	var horizon int64
	err := q.QueryRow(`SELECT horizon_ms FROM usage_prune WHERE id=1`).Scan(&horizon)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read usage prune horizon: %w", err)
	}
	return horizon, nil
}

// CountUsageCallsBefore reports how many recorded calls completed before
// cutoffMS: the dry-run half of `crossing-guard prune --usage` (plan D-12).
func (ix *Index) CountUsageCallsBefore(cutoffMS int64) (int64, error) {
	var count int64
	err := ix.db.QueryRow(`SELECT COUNT(*) FROM usage_call WHERE at_ms < ?`, cutoffMS).Scan(&count)
	return count, err
}

// PruneUsageCallsBefore deletes recorded calls and remembered copies that
// completed before cutoffMS and raises the prune horizon to it, so no later
// re-read inserts them again. It is an operator act, never automatic (D-7).
func (ix *Index) PruneUsageCallsBefore(cutoffMS int64) (int64, error) {
	tx, err := ix.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.Exec(`DELETE FROM usage_call WHERE at_ms < ?`, cutoffMS)
	if err != nil {
		return 0, fmt.Errorf("prune usage calls: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM usage_call_copy WHERE at_ms < ?`, cutoffMS); err != nil {
		return 0, fmt.Errorf("prune usage copies: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO usage_prune(id,horizon_ms) VALUES(1,?)
		ON CONFLICT(id) DO UPDATE SET horizon_ms=max(usage_prune.horizon_ms,excluded.horizon_ms)`,
		cutoffMS); err != nil {
		return 0, fmt.Errorf("record usage prune horizon: %w", err)
	}
	return deleted, tx.Commit()
}

// UsagePruneHorizon is the largest horizon ever pruned, in milliseconds.
func (ix *Index) UsagePruneHorizon() (int64, error) { return usagePruneHorizon(ix.db) }
