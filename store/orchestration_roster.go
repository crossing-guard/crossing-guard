package store

// Agent-page reads over the orchestration history (agents-settings-redesign
// plan §4, §6): runs and review invocations grouped into outcome classes,
// keyset pages, and windowed counts. Reads only; the outcome class is a
// projection of stored state, never written back.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Outcome classes shared by managed runs and review invocations.
const (
	OutcomeRunning  = "running"
	OutcomeWaiting  = "waiting"
	OutcomeActed    = "acted"
	OutcomeQuiet    = "quiet"
	OutcomeFailed   = "failed"
	OutcomeSkipped  = "skipped"
	OutcomeDeferred = "deferred"
)

// managedOutcomeSQL classifies one orchestration_managed_run row. A follower
// that applied tags acted even when its action is no_action.
const managedOutcomeSQL = `CASE
  WHEN state IN ('admitted','running') THEN 'running'
  WHEN state='parked' THEN 'waiting'
  WHEN state='completed' AND ((action<>'' AND action<>'no_action')
    OR COALESCE(json_array_length(json_extract(detail_json,'$.tags')),0)>0) THEN 'acted'
  WHEN state='completed' THEN 'quiet'
  WHEN state IN ('failed','unknown') THEN 'failed'
  WHEN state='suppressed' THEN 'skipped'
  WHEN state='deferred' THEN 'deferred'
  ELSE 'failed' END`

// reviewOutcomeSQL classifies one review invocation; an abstaining reviewer
// stayed quiet.
const reviewOutcomeSQL = `CASE
  WHEN state IN ('admitted','running') THEN 'running'
  WHEN state='completed' AND decision='abstain' THEN 'quiet'
  WHEN state='completed' THEN 'acted'
  WHEN state='suppressed' THEN 'skipped'
  ELSE 'failed' END`

// OutcomeCounts is one window's run counts per outcome class.
type OutcomeCounts struct {
	Runs     int64 `json:"runs"`
	Acted    int64 `json:"acted"`
	Quiet    int64 `json:"quiet"`
	Failed   int64 `json:"failed"`
	Skipped  int64 `json:"skipped"`
	Deferred int64 `json:"deferred"`
	Running  int64 `json:"running"`
	Waiting  int64 `json:"waiting"`
}

func (c *OutcomeCounts) add(outcome string, n int64) {
	c.Runs += n
	switch outcome {
	case OutcomeActed:
		c.Acted += n
	case OutcomeQuiet:
		c.Quiet += n
	case OutcomeFailed:
		c.Failed += n
	case OutcomeSkipped:
		c.Skipped += n
	case OutcomeDeferred:
		c.Deferred += n
	case OutcomeRunning:
		c.Running += n
	case OutcomeWaiting:
		c.Waiting += n
	}
}

// DayCounts is one local calendar day of a window.
type DayCounts struct {
	Day int64 `json:"day"` // days since the epoch in the caller's offset
	OutcomeCounts
}

// OutcomeWindow is a window's totals, its days oldest first, and the newest
// run inside or before it.
type OutcomeWindow struct {
	Totals      OutcomeCounts `json:"totals"`
	Days        []DayCounts   `json:"days"`
	LastAt      int64         `json:"last_at,omitempty"`
	LastOutcome string        `json:"last_outcome,omitempty"`
	LastAction  string        `json:"last_action,omitempty"`
}

// HistoryScope selects one agent's history: its profile id, optionally one
// binding (place). The review lane is selected by profile id alone.
type HistoryScope struct {
	ProfileID string
	BindingID string
}

func (scope HistoryScope) where() (string, []any) {
	clauses := []string{"profile_id=?"}
	args := []any{scope.ProfileID}
	if scope.BindingID != "" {
		clauses = append(clauses, "binding_id=?")
		args = append(args, scope.BindingID)
	}
	return strings.Join(clauses, " AND "), args
}

// ManagedOutcomeWindow counts an agent's managed decisions (kind ” runs)
// over `days` local days ending today, in the caller's UTC offset.
func (ix *Index) ManagedOutcomeWindow(scope HistoryScope, now, offsetSeconds int64, days int) (OutcomeWindow, error) {
	return ix.outcomeWindow("orchestration_managed_run", managedOutcomeSQL, "action", "kind='' AND ", scope, now, offsetSeconds, days)
}

// ReviewOutcomeWindow is ManagedOutcomeWindow for review invocations.
func (ix *Index) ReviewOutcomeWindow(scope HistoryScope, now, offsetSeconds int64, days int) (OutcomeWindow, error) {
	return ix.outcomeWindow("orchestration_review_invocation", reviewOutcomeSQL, "''", "", scope, now, offsetSeconds, days)
}

func (ix *Index) outcomeWindow(table, outcomeSQL, actionColumn, extra string, scope HistoryScope, now, offsetSeconds int64, days int) (OutcomeWindow, error) {
	if days < 1 {
		days = 1
	}
	today := floorDiv(now+offsetSeconds, 86400)
	firstDay := today - int64(days) + 1
	since := firstDay*86400 - offsetSeconds
	out := OutcomeWindow{Days: make([]DayCounts, days)}
	for index := range out.Days {
		out.Days[index].Day = firstDay + int64(index)
	}
	where, args := scope.where()
	query := fmt.Sprintf(`SELECT (admitted_at+?)/86400 AS day, %s AS outcome, COUNT(*) FROM %s
		WHERE %s%s AND admitted_at>=? GROUP BY day, outcome`, outcomeSQL, table, extra, where)
	rows, err := ix.db.Query(query, append(append([]any{offsetSeconds}, args...), since)...)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var day, count int64
		var outcome string
		if err := rows.Scan(&day, &outcome, &count); err != nil {
			rows.Close()
			return out, err
		}
		out.Totals.add(outcome, count)
		if index := day - firstDay; index >= 0 && index < int64(days) {
			out.Days[index].add(outcome, count)
		}
	}
	if err := rows.Close(); err != nil {
		return out, err
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	last := fmt.Sprintf(`SELECT admitted_at, %s, %s FROM %s WHERE %s%s ORDER BY admitted_at DESC LIMIT 1`,
		outcomeSQL, actionColumn, table, extra, where)
	err = ix.db.QueryRow(last, args...).Scan(&out.LastAt, &out.LastOutcome, &out.LastAction)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return out, err
}

// ManagedOutcomeTotals counts every managed decision of an agent per class.
func (ix *Index) ManagedOutcomeTotals(scope HistoryScope) (OutcomeCounts, error) {
	return ix.outcomeTotals("orchestration_managed_run", managedOutcomeSQL, "kind='' AND ", scope)
}

// ReviewOutcomeTotals counts every review invocation of an agent per class.
func (ix *Index) ReviewOutcomeTotals(scope HistoryScope) (OutcomeCounts, error) {
	return ix.outcomeTotals("orchestration_review_invocation", reviewOutcomeSQL, "", scope)
}

func (ix *Index) outcomeTotals(table, outcomeSQL, extra string, scope HistoryScope) (OutcomeCounts, error) {
	var out OutcomeCounts
	where, args := scope.where()
	rows, err := ix.db.Query(fmt.Sprintf(`SELECT %s AS outcome, COUNT(*) FROM %s WHERE %s%s GROUP BY outcome`,
		outcomeSQL, table, extra, where), args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var outcome string
		var count int64
		if err := rows.Scan(&outcome, &count); err != nil {
			return out, err
		}
		out.add(outcome, count)
	}
	return out, rows.Err()
}

// FlowCeilingBreachActive reports whether one of this agent's runs recorded
// a flow ceiling breach on a member whose ceiling is still breached — the
// first-class failure the roster attention lane counts directly (flows pilot
// pass-2 C1). The owner's re-tag re-arms the ceiling (breached back to 0),
// which clears the lane; a breach is not a life sentence.
func (ix *Index) FlowCeilingBreachActive(profileID string) (bool, error) {
	var present int
	err := ix.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM orchestration_managed_run r
		JOIN orchestration_group g ON g.group_id=r.group_id
		JOIN orchestration_flow_ceiling c ON c.member_runtime=g.root_runtime
		  AND c.member_session_id=g.root_catalog_session_id AND c.breached=1
		WHERE r.profile_id=? AND r.error_class='flow_ceiling_breach')`, profileID).Scan(&present)
	return present == 1, err
}

// RecentSettledOutcomes returns the classes of an agent's newest settled
// runs across the given bindings (newest first), for the attention rule.
func (ix *Index) RecentSettledOutcomes(profileID string, bindingIDs []string, limit int) ([]string, error) {
	if len(bindingIDs) == 0 || limit < 1 {
		return []string{}, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(bindingIDs)), ",")
	args := []any{profileID}
	for _, id := range bindingIDs {
		args = append(args, id)
	}
	args = append(args, limit)
	rows, err := ix.db.Query(fmt.Sprintf(`SELECT %s FROM orchestration_managed_run
		WHERE kind='' AND profile_id=? AND binding_id IN (%s)
		AND state IN ('completed','failed','unknown')
		ORDER BY admitted_at DESC, run_id DESC LIMIT ?`, managedOutcomeSQL, placeholders), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var outcome string
		if err := rows.Scan(&outcome); err != nil {
			return nil, err
		}
		out = append(out, outcome)
	}
	return out, rows.Err()
}

// ClassifiedManagedRun is a stored run with its outcome class.
type ClassifiedManagedRun struct {
	ManagedRun
	Outcome string `json:"outcome"`
}

// PageCursor is the keyset position after the last row of a page.
type PageCursor struct {
	AdmittedAt int64
	ID         string
}

// ManagedRunsPage returns up to limit decisions newest first, strictly
// before cursor when it is set, optionally one outcome class.
func (ix *Index) ManagedRunsPage(scope HistoryScope, outcome string, cursor PageCursor, limit int) ([]ClassifiedManagedRun, bool, error) {
	where, args := scope.where()
	query := `SELECT ` + managedRunColumns + `, ` + managedOutcomeSQL + ` AS outcome FROM orchestration_managed_run WHERE kind='' AND ` + where
	if cursor.AdmittedAt > 0 {
		query += ` AND (admitted_at<? OR (admitted_at=? AND run_id<?))`
		args = append(args, cursor.AdmittedAt, cursor.AdmittedAt, cursor.ID)
	}
	if outcome != "" {
		query = `SELECT * FROM (` + query + `) WHERE outcome=?`
		args = append(args, outcome)
	}
	query += ` ORDER BY admitted_at DESC, run_id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := ix.db.Query(query, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []ClassifiedManagedRun{}
	for rows.Next() {
		var item ClassifiedManagedRun
		var citations, detail string
		err := rows.Scan(&item.RunID, &item.IdempotencyKey, &item.GroupID, &item.BindingID, &item.BindingStateToken, &item.Role, &item.Kind, &item.ProfileID, &item.ProfileSourceDigest, &item.ProfileBundleDigest, &item.SourceTaskID, &item.SourceEventID, &item.ChildTaskID, &item.State, &item.Action, &item.Message, &citations, &detail, &item.ErrorClass, &item.Recovery, &item.AdmittedAt, &item.StartedAt, &item.CompletedAt, &item.Outcome)
		if err != nil {
			return nil, false, err
		}
		if err := json.Unmarshal([]byte(citations), &item.Citations); err != nil {
			return nil, false, err
		}
		if err := json.Unmarshal([]byte(detail), &item.Detail); err != nil {
			return nil, false, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, nil
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// ClassifiedReviewInvocation is a stored review invocation with its class.
type ClassifiedReviewInvocation struct {
	ReviewInvocation
	Outcome string `json:"outcome"`
}

// ReviewInvocationsPage is ManagedRunsPage for one reviewer agent's
// invocations (selected by profile id; the review binding is a singleton).
func (ix *Index) ReviewInvocationsPage(profileID, outcome string, cursor PageCursor, limit int) ([]ClassifiedReviewInvocation, bool, error) {
	query := `SELECT ` + reviewInvocationColumns + `, ` + reviewOutcomeSQL + ` AS outcome FROM orchestration_review_invocation WHERE profile_id=?`
	args := []any{profileID}
	if cursor.AdmittedAt > 0 {
		query += ` AND (admitted_at<? OR (admitted_at=? AND invocation_id<?))`
		args = append(args, cursor.AdmittedAt, cursor.AdmittedAt, cursor.ID)
	}
	if outcome != "" {
		query = `SELECT * FROM (` + query + `) WHERE outcome=?`
		args = append(args, outcome)
	}
	query += ` ORDER BY admitted_at DESC, invocation_id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := ix.db.Query(query, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []ClassifiedReviewInvocation{}
	for rows.Next() {
		var outcomeClass string
		invocation, err := scanReviewInvocation(outcomeScanner{row: rows, extra: &outcomeClass})
		if err != nil {
			return nil, false, err
		}
		out = append(out, ClassifiedReviewInvocation{ReviewInvocation: invocation, Outcome: outcomeClass})
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, nil
}

// outcomeScanner lets an existing row scanner read a row that carries one
// extra trailing column (the outcome class).
type outcomeScanner struct {
	row   interface{ Scan(...any) error }
	extra *string
}

func (scanner outcomeScanner) Scan(dest ...any) error {
	return scanner.row.Scan(append(dest, scanner.extra)...)
}

// LiveHelperSessions counts, per binding, the groups holding a helper
// session a future turn would resume.
func (ix *Index) LiveHelperSessions(bindingIDs []string) (map[string]int64, error) {
	out := map[string]int64{}
	if len(bindingIDs) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(bindingIDs)), ",")
	args := make([]any, 0, len(bindingIDs))
	for _, id := range bindingIDs {
		args = append(args, id)
	}
	rows, err := ix.db.Query(`SELECT binding_id, COUNT(*) FROM orchestration_group
		WHERE helper_native_session_id<>'' AND state='active' AND binding_id IN (`+placeholders+`)
		GROUP BY binding_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var count int64
		if err := rows.Scan(&id, &count); err != nil {
			return nil, err
		}
		out[id] = count
	}
	return out, rows.Err()
}

// RecentSettledReviewOutcomes is RecentSettledOutcomes for a reviewer agent.
func (ix *Index) RecentSettledReviewOutcomes(profileID string, limit int) ([]string, error) {
	if limit < 1 {
		return []string{}, nil
	}
	rows, err := ix.db.Query(`SELECT `+reviewOutcomeSQL+` FROM orchestration_review_invocation
		WHERE profile_id=? AND state NOT IN ('admitted','running','suppressed')
		ORDER BY admitted_at DESC, invocation_id DESC LIMIT ?`, profileID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var outcome string
		if err := rows.Scan(&outcome); err != nil {
			return nil, err
		}
		out = append(out, outcome)
	}
	return out, rows.Err()
}
