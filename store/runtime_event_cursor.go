package store

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
		COALESCE(decision,''),COALESCE(reason,''),COALESCE(tags,'')
		FROM event WHERE runtime=? AND origin='live' AND id>? ORDER BY id LIMIT ?`, runtimeName, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []RuntimeEventEvidence{}
	for rows.Next() {
		var event RuntimeEventEvidence
		if err := rows.Scan(&event.ID, &event.SessionID, &event.Verb, &event.Tool,
			&event.Decision, &event.Reason, &event.Tags); err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
