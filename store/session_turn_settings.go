package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ThinkingEffort holds a native model-scoped value, never a global effort enum.
type ThinkingEffort struct {
	Kind  string `json:"kind"`
	Value string `json:"value,omitempty"`
}

func (e ThinkingEffort) Validate() error {
	if e.Kind == "inherit" && e.Value == "" {
		return nil
	}
	if e.Kind == "level" && e.Value != "" && len(e.Value) <= 256 && utf8.ValidString(e.Value) && !strings.ContainsFunc(e.Value, unicode.IsControl) {
		return nil
	}
	return errors.New("thinking_effort requires inherit or a bounded level value")
}

type TaskRequestedSettings struct {
	Model         string         `json:"model,omitempty"`
	EffortLabel   string         `json:"thinking_effort_label,omitempty"`
	Effort        ThinkingEffort `json:"thinking_effort"`
	Source        string         `json:"source"`
	CatalogDigest string         `json:"catalog_digest,omitempty"`
}

type SessionTurnSettings struct {
	Runtime   string         `json:"runtime"`
	SessionID string         `json:"session_id"`
	Model     string         `json:"model"`
	Effort    ThinkingEffort `json:"thinking_effort"`
	Token     string         `json:"token"`
	UpdatedAt int64          `json:"updated_at"`
}

var ErrSessionEffortConflict = errors.New("session effort changed; reload the saved setting")

// Follow the existing column-probe migration discipline: partial legacy fixtures
// and reopened stores may already have these additive columns.
func migrateThinkingEffortV37(db schemaDB) error {
	for _, addition := range []struct{ table, column, ddl string }{
		{"runtime_task", "requested_settings", `ALTER TABLE runtime_task ADD COLUMN requested_settings TEXT NOT NULL DEFAULT ''`},
		{"orchestration_managed_binding", "thinking_effort", `ALTER TABLE orchestration_managed_binding ADD COLUMN thinking_effort TEXT NOT NULL DEFAULT 'null'`},
	} {
		columns, err := columnSet(db, addition.table)
		if err != nil {
			return err
		}
		if len(columns) > 0 && !columns[addition.column] {
			if _, err := db.Exec(addition.ddl); err != nil {
				return err
			}
		}
	}
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS session_turn_settings(runtime TEXT NOT NULL, session_id TEXT NOT NULL, model TEXT NOT NULL,
 effort TEXT NOT NULL, revision INTEGER NOT NULL CHECK(revision>0), updated_at INTEGER NOT NULL,
 PRIMARY KEY(runtime,session_id,model));`)
	return err
}

type settingsQuerier interface{ QueryRow(string, ...any) *sql.Row }

func readSessionTurnSettings(db settingsQuerier, runtime, session, model string) (SessionTurnSettings, error) {
	out := SessionTurnSettings{Runtime: runtime, SessionID: session, Model: model, Effort: ThinkingEffort{Kind: "inherit"}, Token: "0"}
	var raw string
	var revision int64
	err := db.QueryRow(`SELECT effort,revision,updated_at FROM session_turn_settings WHERE runtime=? AND session_id=? AND model=?`, runtime, session, model).Scan(&raw, &revision, &out.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if err = json.Unmarshal([]byte(raw), &out.Effort); err != nil {
		return out, err
	}
	out.Token = strconv.FormatInt(revision, 10)
	return out, nil
}
func (ix *Index) SessionTurnSettings(runtime, session, model string) (SessionTurnSettings, error) {
	return readSessionTurnSettings(ix.db, runtime, session, model)
}
func (ix *Index) SaveSessionTurnSettings(runtime, session, model, expected string, effort ThinkingEffort) (SessionTurnSettings, error) {
	if runtime == "" || session == "" || model == "" || len(runtime) > 100 || len(session) > 1000 || len(model) > 256 {
		return SessionTurnSettings{}, errors.New("runtime, session and concrete model required")
	}
	if err := effort.Validate(); err != nil {
		return SessionTurnSettings{}, err
	}
	revision, err := strconv.ParseInt(expected, 10, 64)
	if err != nil || revision < 0 || revision > 1<<60 {
		return SessionTurnSettings{}, ErrSessionEffortConflict
	}
	raw, err := json.Marshal(effort)
	if err != nil {
		return SessionTurnSettings{}, err
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return SessionTurnSettings{}, err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UnixMilli()
	result, err := tx.Exec(`INSERT INTO session_turn_settings(runtime,session_id,model,effort,revision,updated_at)
 SELECT ?,?,?,?,1,? WHERE ?=0
 ON CONFLICT(runtime,session_id,model) DO NOTHING`, runtime, session, model, string(raw), now, revision)
	if err != nil {
		return SessionTurnSettings{}, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return SessionTurnSettings{}, err
	}
	if n == 0 {
		result, err = tx.Exec(`UPDATE session_turn_settings SET effort=?,revision=revision+1,updated_at=? WHERE runtime=? AND session_id=? AND model=? AND revision=?`, string(raw), now, runtime, session, model, revision)
		if err != nil {
			return SessionTurnSettings{}, err
		}
		n, err = result.RowsAffected()
		if err != nil {
			return SessionTurnSettings{}, err
		}
	}
	if n != 1 {
		return SessionTurnSettings{}, ErrSessionEffortConflict
	}
	out, err := readSessionTurnSettings(tx, runtime, session, model)
	if err != nil {
		return out, err
	}
	return out, tx.Commit()
}

// Called inside task creation after detecting a new idempotency key. It closes
// the gap between resolving a saved selection and admitting the queued turn.
func checkTaskSessionEffort(tx *sql.Tx, in RuntimeTaskRecord) error {
	if in.SessionEffortToken == "" {
		return nil
	}
	if in.RequestedSettings == nil || in.SessionEffortID == "" {
		return ErrSessionEffortConflict
	}
	saved, err := readSessionTurnSettings(tx, in.Runtime, in.SessionEffortID, in.RequestedSettings.Model)
	if err != nil {
		return err
	}
	if saved.Token != in.SessionEffortToken || saved.Effort != in.RequestedSettings.Effort {
		return ErrSessionEffortConflict
	}
	return nil
}

func encodeRequestedSettings(settings *TaskRequestedSettings) (string, error) {
	if settings == nil {
		return "", nil
	}
	if err := settings.Effort.Validate(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		return "", err
	}
	if len(raw) > 4096 {
		return "", fmt.Errorf("requested settings exceed bound")
	}
	return string(raw), nil
}
