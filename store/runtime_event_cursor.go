package store

import "crossing-guard/ruledoc"

// RuntimeEventEvidence is the bounded live-event projection used by a verification
// watcher. Tags stay server-side so a browser never receives captured command content.
type RuntimeEventEvidence struct {
	ID        int64
	SessionID string
	Verb      string
	Tool      string
	Decision  string
	Reason    string
	Tags      string
	RuleID    string
}

// RuntimeEventCursor returns the append-only event boundary for one runtime. A later
// watcher compares IDs, never wall clocks or aggregate counts, so old events cannot
// certify a repaired attachment.
func (ix *Index) RuntimeEventCursor(runtimeName string) (int64, error) {
	var cursor int64
	err := ix.db.QueryRow(`SELECT COALESCE(MAX(id),0) FROM event WHERE runtime=? AND origin='live'`, runtimeName).Scan(&cursor)
	return cursor, err
}

// RuntimeEventsAfter returns only live events strictly beyond a captured cursor.
func (ix *Index) RuntimeEventsAfter(runtimeName string, afterID int64, limit int) ([]RuntimeEventEvidence, error) {
	if limit <= 0 {
		limit = 25
	}
	if limit > 100 {
		limit = 100
	}
	rows, err := ix.db.Query(`SELECT id,session_id,COALESCE(verb,''),COALESCE(tool,''),
		COALESCE(decision,''),COALESCE(reason,''),COALESCE(tags,''),rule_id
		FROM event WHERE runtime=? AND origin='live' AND id>? ORDER BY id LIMIT ?`, runtimeName, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []RuntimeEventEvidence{}
	for rows.Next() {
		var event RuntimeEventEvidence
		if err := rows.Scan(&event.ID, &event.SessionID, &event.Verb, &event.Tool,
			&event.Decision, &event.Reason, &event.Tags, &event.RuleID); err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// RuntimeObservation is one runtime's latest moment of a kind, with the event that proves it.
type RuntimeObservation struct {
	TS            int64
	EventGlobalID string
}

// nonRuntimeFilter excludes runtime values that never credit a real runtime: the empty string
// is a hook installed before the field existed, and the demo runtime is `crossing-guard
// demo` driving the hook itself rather than an agent doing so.
const nonRuntimeFilter = `runtime NOT IN ('','` + ruledoc.DemoRuntime + `')`

// RuntimeCanaries returns, per runtime, the latest LIVE event in which ruleID denied an
// action — the durable form of "the canary was blocked here". It is the only source the
// fleet's third rung may use: configuration presence and ordinary traffic prove the hook
// is attached and firing, never that a deny reaches the agent.
func (ix *Index) RuntimeCanaries(ruleID string) (map[string]RuntimeObservation, error) {
	// `rule_id != ''` is redundant with `rule_id = ?` to a reader and load-bearing to the
	// planner: it is the partial index's own term, and without it the index is not used.
	return ix.runtimeLatest(`rule_id != '' AND rule_id=? AND origin='live' AND decision='deny' AND `+nonRuntimeFilter, ruleID)
}

// RuntimeLastLive returns, per runtime, the latest live event of any kind. RuntimeStats is
// not reused: it counts every origin, and an imported transcript proves nothing about a hook.
func (ix *Index) RuntimeLastLive() (map[string]RuntimeObservation, error) {
	return ix.runtimeLatest(`origin='live' AND ` + nonRuntimeFilter)
}

// runtimeLatest groups the newest matching event per runtime. where is always a
// compile-time literal from this file, never caller input.
func (ix *Index) runtimeLatest(where string, args ...any) (map[string]RuntimeObservation, error) {
	rows, err := ix.db.Query(`SELECT e.runtime, e.ts, e.global_id FROM event e
		JOIN (SELECT runtime r, MAX(id) m FROM event WHERE `+where+` GROUP BY runtime) latest ON e.id = latest.m`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]RuntimeObservation{}
	for rows.Next() {
		var name string
		var o RuntimeObservation
		if err := rows.Scan(&name, &o.TS, &o.EventGlobalID); err != nil {
			return nil, err
		}
		out[name] = o
	}
	return out, rows.Err()
}
