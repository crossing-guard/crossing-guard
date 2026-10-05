package store

// C6 typed change-envelope persistence. This file owns SQL only: producers,
// repository normalization, comparisons and presentation live in internal/changeenv.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	defaultFileEvidenceLimit = 100
	maxFileEvidenceLimit     = 200
)

type ChangeRecord struct {
	ID                     int64        `json:"id"`
	SessionID              string       `json:"session_id"`
	RepositoryID           string       `json:"repository_id"`
	CheckoutID             string       `json:"checkout_id"`
	RepositoryIdentityKind string       `json:"repository_identity_kind"`
	CheckoutRoot           string       `json:"checkout_root"`
	SessionRuntimeClaim    string       `json:"session_runtime_claim,omitempty"`
	SessionTitleClaim      string       `json:"session_title_claim,omitempty"`
	Kind                   string       `json:"kind"`
	EvidenceClass          string       `json:"evidence_class"`
	SourceKind             string       `json:"source_kind"`
	SourceRef              string       `json:"source_ref"`
	SourceDisplay          string       `json:"source_display"`
	SourceDigest           string       `json:"source_digest"`
	RecordedAt             int64        `json:"recorded_at"`
	CaptureStartedAt       int64        `json:"capture_started_at,omitempty"`
	CaptureEndedAt         int64        `json:"capture_ended_at,omitempty"`
	BaseRevision           string       `json:"base_revision,omitempty"`
	HeadRevision           string       `json:"head_revision,omitempty"`
	GitVersion             string       `json:"git_version,omitempty"`
	SnapshotDigest         string       `json:"snapshot_digest,omitempty"`
	CaptureAttempts        int          `json:"capture_attempts,omitempty"`
	IncludedLayers         string       `json:"included_layers,omitempty"`
	IgnoredIncluded        bool         `json:"ignored_included"`
	SparseCheckout         bool         `json:"sparse_checkout"`
	CommonDirDigest        string       `json:"common_dir_digest,omitempty"`
	NonAtomic              bool         `json:"non_atomic"`
	IntentLabel            string       `json:"intent_label,omitempty"`
	VerificationName       string       `json:"verification_name,omitempty"`
	VerificationBoundary   string       `json:"verification_boundary,omitempty"`
	VerificationNameClass  string       `json:"verification_name_class,omitempty"`
	VerificationResult     string       `json:"verification_result,omitempty"`
	ExecutablePath         string       `json:"executable_path,omitempty"`
	ExecutableDigest       string       `json:"executable_digest,omitempty"`
	ArgvDigest             string       `json:"argv_digest,omitempty"`
	ExitCode               *int         `json:"exit_code,omitempty"`
	Signal                 string       `json:"signal,omitempty"`
	Termination            string       `json:"termination,omitempty"`
	EnvironmentState       string       `json:"environment_state,omitempty"`
	ChildCleanup           string       `json:"child_cleanup,omitempty"`
	Limitation             string       `json:"limitation,omitempty"`
	Items                  []ChangeItem `json:"items,omitempty"`
}

type ChangeItem struct {
	Ordinal int    `json:"ordinal"`
	Path    string `json:"path"`
	OldPath string `json:"old_path,omitempty"`
	Symbol  string `json:"symbol,omitempty"`
	Layer   string `json:"layer,omitempty"`
	Status  string `json:"status,omitempty"`
}

type CheckpointPayload struct {
	CheckpointID        int64  `json:"checkpoint_id"`
	Ordinal             int    `json:"ordinal"`
	Path                string `json:"path"`
	Layer               string `json:"layer"`
	Status              string `json:"status"`
	ContentBytes        int    `json:"content_bytes"`
	ContentDigest       string `json:"content_digest,omitempty"`
	ContentCompleteness string `json:"content_completeness"`
	ContentPayload      []byte `json:"-"`
	PatchBytes          int    `json:"patch_bytes"`
	PatchDigest         string `json:"patch_digest,omitempty"`
	PatchCompleteness   string `json:"patch_completeness"`
	PatchPayload        []byte `json:"-"`
	SourceIdentity      string `json:"source_identity,omitempty"`
	Limitation          string `json:"limitation,omitempty"`
}

// ErrCheckpointCompletionCollision means a completed checkpoint was replayed with
// different change or payload facts. The existing completion remains unchanged.
var ErrCheckpointCompletionCollision = errors.New("checkpoint completion identity collision")

// LinkedResultEffect is an exact result effect joined to its selected action.
type LinkedResultEffect struct {
	ResultID        int64  `json:"result_id"`
	EventID         int64  `json:"event_id"`
	Ordinal         int    `json:"ordinal"`
	CompletedAt     int64  `json:"completed_at"`
	RawIdentity     string `json:"raw_identity"`
	Operation       string `json:"operation"`
	EvidenceSource  string `json:"evidence_source"`
	ContentDigest   string `json:"content_digest,omitempty"`
	DiffDigest      string `json:"diff_digest,omitempty"`
	Completeness    string `json:"completeness"`
	ContentMeasured bool   `json:"content_measured"`
	DiffMeasured    bool   `json:"diff_measured"`
}

type PathReconciliation struct {
	ID                      int64  `json:"id"`
	SessionID               string `json:"session_id"`
	CheckoutID              string `json:"checkout_id"`
	PredecessorCheckpointID int64  `json:"predecessor_checkpoint_id,omitempty"`
	CurrentCheckpointID     int64  `json:"current_checkpoint_id"`
	EventID                 int64  `json:"event_id,omitempty"`
	ResultID                int64  `json:"result_id,omitempty"`
	Path                    string `json:"path"`
	OldPath                 string `json:"old_path,omitempty"`
	Classification          string `json:"classification"`
	CompetingActor          string `json:"competing_actor,omitempty"`
	RuntimeEffectDigest     string `json:"runtime_effect_digest,omitempty"`
	GitEffectDigest         string `json:"git_effect_digest,omitempty"`
	Algorithm               string `json:"algorithm"`
	ReconciledAt            int64  `json:"reconciled_at"`
	SupersedesID            int64  `json:"supersedes_id,omitempty"`
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func nullableInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// AppendChange appends one typed record and its items atomically.
func (ix *Index) AppendChange(r *ChangeRecord) error {
	if r == nil || r.SessionID == "" || r.RepositoryID == "" || r.CheckoutID == "" {
		return fmt.Errorf("change record requires session, repository, and checkout identity")
	}
	if len(r.Items) > 10000 {
		return fmt.Errorf("change record has %d items; maximum is 10000", len(r.Items))
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := appendChangeTx(tx, r); err != nil {
		return err
	}
	return tx.Commit()
}

func appendChangeTx(tx *sql.Tx, r *ChangeRecord) error {
	res, err := tx.Exec(`INSERT INTO change_record(
		session_id,repository_id,checkout_id,repository_identity_kind,checkout_root,
		session_runtime_claim,session_title_claim,kind,evidence_class,source_kind,
		source_ref,source_display,source_digest,recorded_at,capture_started_at,capture_ended_at,
		base_revision,head_revision,git_version,snapshot_digest,capture_attempts,included_layers,
		ignored_included,sparse_checkout,common_dir_digest,non_atomic,intent_label,
		verification_name,verification_boundary,verification_name_class,verification_result,
		executable_path,executable_digest,argv_digest,exit_code,signal,termination,
		environment_state,child_cleanup,limitation)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.SessionID, r.RepositoryID, r.CheckoutID, r.RepositoryIdentityKind, r.CheckoutRoot,
		r.SessionRuntimeClaim, r.SessionTitleClaim, r.Kind, r.EvidenceClass, r.SourceKind,
		r.SourceRef, r.SourceDisplay, r.SourceDigest, r.RecordedAt, nullableInt(r.CaptureStartedAt), nullableInt(r.CaptureEndedAt),
		nullable(r.BaseRevision), nullable(r.HeadRevision), nullable(r.GitVersion), nullable(r.SnapshotDigest),
		nullableInt(int64(r.CaptureAttempts)), nullable(r.IncludedLayers), boolInt(r.IgnoredIncluded),
		boolInt(r.SparseCheckout), nullable(r.CommonDirDigest), boolInt(r.NonAtomic), nullable(r.IntentLabel),
		nullable(r.VerificationName), nullable(r.VerificationBoundary), nullable(r.VerificationNameClass),
		nullable(r.VerificationResult), nullable(r.ExecutablePath), nullable(r.ExecutableDigest), nullable(r.ArgvDigest),
		r.ExitCode, nullable(r.Signal), nullable(r.Termination), nullable(r.EnvironmentState), nullable(r.ChildCleanup), nullable(r.Limitation))
	if err != nil {
		return err
	}
	recordID, err := res.LastInsertId()
	if err != nil {
		return err
	}
	for i, item := range r.Items {
		_, err = tx.Exec(`INSERT INTO change_item(record_id,ordinal,path,old_path,symbol,layer,status) VALUES(?,?,?,?,?,?,?)`,
			recordID, i, item.Path, item.OldPath, item.Symbol, item.Layer, item.Status)
		if err != nil {
			return err
		}
	}
	r.ID = recordID
	return nil
}

// CompleteSessionCheckpoint atomically appends the existing-format Git revision and
// links it as the one completed attachment baseline. A failed link leaves no orphan
// revision that a reader could mistake for the baseline.
func (ix *Index) CompleteSessionCheckpoint(checkpointID int64, r *ChangeRecord, endedAt int64) error {
	return ix.CompleteSessionCheckpointWithPayload(checkpointID, r, nil, endedAt)
}

func validateCheckpointBody(kind string, body []byte, bodyBytes int, digest, completeness string) error {
	if bodyBytes < 0 {
		return fmt.Errorf("checkpoint %s byte count is negative", kind)
	}
	switch completeness {
	case "complete":
		if body == nil {
			return fmt.Errorf("checkpoint %s is complete but its body is not retained", kind)
		}
	case "metadata-only", "unavailable":
		if body != nil {
			return fmt.Errorf("checkpoint %s %s evidence must omit its body", kind, completeness)
		}
		if completeness == "metadata-only" && bodyBytes > 0 && digest == "" {
			return fmt.Errorf("checkpoint %s metadata-only evidence requires a digest", kind)
		}
	default:
		return fmt.Errorf("checkpoint %s completeness %q is invalid", kind, completeness)
	}
	if body == nil {
		return nil
	}
	if len(body) != bodyBytes {
		return fmt.Errorf("checkpoint %s byte count does not match retained body", kind)
	}
	if digest != retainedBodyDigest(body) {
		return fmt.Errorf("checkpoint %s digest does not match retained body", kind)
	}
	return nil
}

func checkpointCompletionDigest(r *ChangeRecord, payloads []CheckpointPayload) (string, error) {
	record := *r
	record.ID = 0
	identityPayloads := append([]CheckpointPayload(nil), payloads...)
	for index := range identityPayloads {
		identityPayloads[index].CheckpointID = 0
		identityPayloads[index].Ordinal = index
		identityPayloads[index].ContentPayload = nil
		identityPayloads[index].PatchPayload = nil
	}
	body, err := json.Marshal(struct {
		Record   ChangeRecord        `json:"record"`
		Payloads []CheckpointPayload `json:"payloads"`
	}{Record: record, Payloads: identityPayloads})
	if err != nil {
		return "", fmt.Errorf("encode checkpoint completion identity: %w", err)
	}
	return retainedBodyDigest(body), nil
}

func normalizeAndValidateCheckpointPayloads(payloads []CheckpointPayload) error {
	for index := range payloads {
		payload := &payloads[index]
		if payload.ContentCompleteness == "" {
			payload.ContentCompleteness = "unavailable"
		}
		if payload.PatchCompleteness == "" {
			payload.PatchCompleteness = "unavailable"
		}
		if err := validateCheckpointBody("content", payload.ContentPayload, payload.ContentBytes,
			payload.ContentDigest, payload.ContentCompleteness); err != nil {
			return fmt.Errorf("checkpoint payload %d: %w", index, err)
		}
		if err := validateCheckpointBody("patch", payload.PatchPayload, payload.PatchBytes,
			payload.PatchDigest, payload.PatchCompleteness); err != nil {
			return fmt.Errorf("checkpoint payload %d: %w", index, err)
		}
	}
	return nil
}

// CompleteSessionCheckpointWithPayload atomically stores one revision and its verified,
// content-addressed payload bodies. Same-fact replay is a no-op; different replay fails.
func (ix *Index) CompleteSessionCheckpointWithPayload(checkpointID int64, r *ChangeRecord, payloads []CheckpointPayload, endedAt int64) error {
	if r == nil {
		return fmt.Errorf("checkpoint completion requires a change record")
	}
	if err := normalizeAndValidateCheckpointPayloads(payloads); err != nil {
		return err
	}
	completionDigest, err := checkpointCompletionDigest(r, payloads)
	if err != nil {
		return err
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var sessionID, repositoryID, checkoutID, status, existingCompletionDigest string
	if err := tx.QueryRow(`SELECT session_id,repository_id,checkout_id,status,detail_digest FROM session_checkpoint WHERE id=?`, checkpointID).
		Scan(&sessionID, &repositoryID, &checkoutID, &status, &existingCompletionDigest); err != nil {
		return err
	}
	if status == "complete" {
		if existingCompletionDigest != completionDigest {
			return ErrCheckpointCompletionCollision
		}
		return tx.Commit()
	}
	if status != "capturing" || sessionID != r.SessionID || repositoryID != r.RepositoryID || checkoutID != r.CheckoutID {
		return fmt.Errorf("checkpoint completion identity/state mismatch")
	}
	if err := appendChangeTx(tx, r); err != nil {
		return err
	}
	for ordinal, payload := range payloads {
		_, err := tx.Exec(`INSERT INTO checkpoint_payload(checkpoint_id,ordinal,path,layer,status,
			content_bytes,content_digest,content_completeness,content_payload,patch_bytes,
			patch_digest,patch_completeness,patch_payload,source_identity,limitation)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, checkpointID, ordinal, payload.Path, payload.Layer,
			payload.Status, payload.ContentBytes, payload.ContentDigest, payload.ContentCompleteness,
			nil, payload.PatchBytes, payload.PatchDigest,
			payload.PatchCompleteness, nil, payload.SourceIdentity,
			payload.Limitation)
		if err != nil {
			return err
		}
		for _, body := range []struct {
			kind, digest string
			payload      []byte
		}{
			{kind: "content", digest: payload.ContentDigest, payload: payload.ContentPayload},
			{kind: "patch", digest: payload.PatchDigest, payload: payload.PatchPayload},
		} {
			if body.payload == nil {
				continue
			}
			if body.digest == "" {
				return fmt.Errorf("checkpoint %s body requires digest", body.kind)
			}
			if _, err := tx.Exec(`INSERT INTO evidence_body(digest,media_type,body_bytes,payload)
				VALUES(?,'application/octet-stream',?,?) ON CONFLICT(digest) DO NOTHING`,
				body.digest, len(body.payload), body.payload); err != nil {
				return err
			}
			var existing []byte
			if err := tx.QueryRow(`SELECT payload FROM evidence_body WHERE digest=?`, body.digest).Scan(&existing); err != nil {
				return err
			}
			if !bytes.Equal(existing, body.payload) {
				return fmt.Errorf("checkpoint evidence digest collision")
			}
			if _, err := tx.Exec(`INSERT INTO checkpoint_payload_body(
				checkpoint_id,ordinal,kind,body_digest) VALUES(?,?,?,?)`, checkpointID,
				ordinal, body.kind, body.digest); err != nil {
				return err
			}
		}
	}
	res, err := tx.Exec(`UPDATE session_checkpoint SET status='complete',capture_ended_at=?,
		change_record_id=?,failure_kind='',detail_digest=? WHERE id=? AND status='capturing'`,
		endedAt, r.ID, completionDigest, checkpointID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("checkpoint completion update affected %d rows: %v", n, err)
	}
	if err := enqueueCheckpointFact(tx, checkpointID, sessionID, "complete:"+completionDigest, endedAt); err != nil {
		return err
	}
	return tx.Commit()
}

func (ix *Index) ChangeRecordByID(id int64) (ChangeRecord, error) {
	rows, err := ix.db.Query(`SELECT `+changeCols+` FROM change_record WHERE id=?`, id)
	if err != nil {
		return ChangeRecord{}, err
	}
	records, err := scanChange(rows)
	if err != nil {
		return ChangeRecord{}, err
	}
	if len(records) == 0 {
		return ChangeRecord{}, sql.ErrNoRows
	}
	records[0].Items, err = ix.ChangeItems(id)
	if err != nil {
		return ChangeRecord{}, err
	}
	return records[0], nil
}

func (ix *Index) CompletedCheckpointsForSessionCheckout(sessionID, checkoutID string) ([]SessionCheckpoint, error) {
	rows, err := ix.db.Query(`SELECT `+checkpointCols+` FROM session_checkpoint
		WHERE session_id=? AND checkout_id=? AND status='complete' ORDER BY capture_ended_at,id`, sessionID, checkoutID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SessionCheckpoint{}
	for rows.Next() {
		var c SessionCheckpoint
		if err := rows.Scan(&c.ID, &c.Runtime, &c.SessionID, &c.ScopeKey, &c.Kind, &c.RequestID,
			&c.TriggerEventID, &c.TriggerResultID, &c.TriggerObservationID, &c.PredecessorID,
			&c.WorkingDirectory, &c.RepositoryID, &c.CheckoutID, &c.CheckoutRoot, &c.Status,
			&c.BoundaryClass, &c.RequestedAt, &c.CaptureStartedAt, &c.CaptureEndedAt,
			&c.CaptureAttempts, &c.ChangeRecordID, &c.FailureKind, &c.DetailDigest); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (ix *Index) CheckpointPayloads(checkpointID int64) ([]CheckpointPayload, error) {
	rows, err := ix.db.Query(`SELECT checkpoint_id,ordinal,path,layer,status,content_bytes,
		content_digest,content_completeness,patch_bytes,patch_digest,patch_completeness,
		source_identity,limitation FROM checkpoint_payload WHERE checkpoint_id=? ORDER BY ordinal`, checkpointID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CheckpointPayload{}
	for rows.Next() {
		var p CheckpointPayload
		if err := rows.Scan(&p.CheckpointID, &p.Ordinal, &p.Path, &p.Layer, &p.Status,
			&p.ContentBytes, &p.ContentDigest, &p.ContentCompleteness, &p.PatchBytes,
			&p.PatchDigest, &p.PatchCompleteness, &p.SourceIdentity, &p.Limitation); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// linkedResultEffectsForSessionCTE selects the effects of one session whose
// latest reconciliation is exact; "latest" is the highest id, as it was before
// this shape. The session's results are selected first and each one's latest
// reconciliation is read by a correlated lookup that
// result_reconciliation_current serves: resolving the latest reconciliation of
// every result in the store and filtering afterwards ran once per checkpoint
// under the daemon's write lock (observe-hook-latency plan §15).
// The caller appends further `AND` terms on o and e, then closes the CTE.
const linkedResultEffectsForSessionCTE = `WITH matched AS (
	SELECT o.id result_id,cand.event_id,e.ordinal,o.completed_at,e.raw_identity,e.operation,e.evidence_source,
		e.content_digest,e.diff_digest,e.completeness,
		CASE WHEN e.content_payload IS NULL THEN 0 ELSE 1 END content_measured,
		CASE WHEN e.diff_payload IS NULL THEN 0 ELSE 1 END diff_measured
	FROM result_observation o
	JOIN result_reconciliation r ON r.result_id=o.id
		AND r.id=(SELECT MAX(r2.id) FROM result_reconciliation r2 WHERE r2.result_id=o.id)
		AND r.join_class='exact'
	JOIN result_reconciliation_candidate cand ON cand.reconciliation_id=r.id AND cand.selected=1
	JOIN result_effect e ON e.result_id=o.id WHERE o.session_id=?`

// linkedResultEffectsOrder is total, so a LIMIT keeps the same rows whatever
// plan the query runs under.
const linkedResultEffectsOrder = ` ORDER BY completed_at,result_id,ordinal,event_id`

const linkedResultEffectColumns = ` SELECT result_id,event_id,ordinal,completed_at,raw_identity,operation,evidence_source,
	content_digest,diff_digest,completeness,content_measured,diff_measured FROM matched`

func (ix *Index) LinkedResultEffectsForSession(sessionID string) ([]LinkedResultEffect, error) {
	rows, err := ix.db.Query(linkedResultEffectsForSessionCTE+`)`+linkedResultEffectColumns+
		linkedResultEffectsOrder, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LinkedResultEffect{}
	for rows.Next() {
		var effect LinkedResultEffect
		if err := rows.Scan(&effect.ResultID, &effect.EventID, &effect.Ordinal, &effect.CompletedAt,
			&effect.RawIdentity, &effect.Operation, &effect.EvidenceSource,
			&effect.ContentDigest, &effect.DiffDigest, &effect.Completeness,
			&effect.ContentMeasured, &effect.DiffMeasured); err != nil {
			return nil, err
		}
		out = append(out, effect)
	}
	return out, rows.Err()
}

// LinkedResultEffectsForFile returns bounded exact effects and their full matching total.
func (ix *Index) LinkedResultEffectsForFile(sessionID string, identities []string, limit int) ([]LinkedResultEffect, int, error) {
	if limit <= 0 || limit > maxFileEvidenceLimit {
		limit = defaultFileEvidenceLimit
	}
	if len(identities) == 0 {
		return []LinkedResultEffect{}, 0, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(identities)), ",")
	current := linkedResultEffectsForSessionCTE + ` AND e.raw_identity IN (` + placeholders + `))`
	args := []any{sessionID}
	for _, identity := range identities {
		args = append(args, identity)
	}
	var total int
	if err := ix.db.QueryRow(current+` SELECT COUNT(*) FROM matched`, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := ix.db.Query(current+linkedResultEffectColumns+linkedResultEffectsOrder+` LIMIT ?`,
		append(args, limit)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []LinkedResultEffect{}
	for rows.Next() {
		var effect LinkedResultEffect
		if err := rows.Scan(&effect.ResultID, &effect.EventID, &effect.Ordinal, &effect.CompletedAt,
			&effect.RawIdentity, &effect.Operation, &effect.EvidenceSource,
			&effect.ContentDigest, &effect.DiffDigest, &effect.Completeness,
			&effect.ContentMeasured, &effect.DiffMeasured); err != nil {
			return nil, 0, err
		}
		out = append(out, effect)
	}
	return out, total, rows.Err()
}

// FileEventsForSession returns bounded actions declaring any supplied file identity.
func (ix *Index) FileEventsForSession(sessionID string, identities []string, limit int) ([]EventRecord, int, error) {
	if limit <= 0 || limit > maxFileEvidenceLimit {
		limit = defaultFileEvidenceLimit
	}
	if len(identities) == 0 {
		return []EventRecord{}, 0, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(identities)), ",")
	matched := `WITH matched AS (SELECT DISTINCT e.id FROM event e
		LEFT JOIN event_resource er ON er.event_id=e.id LEFT JOIN entity n ON n.id=er.entity_id
		LEFT JOIN entity legacy ON legacy.id=e.target_entity_id
		WHERE e.session_id=? AND ((n.kind='file' AND n.identity IN (` + placeholders + `))
			OR (legacy.kind='file' AND legacy.identity IN (` + placeholders + `))))`
	args := []any{sessionID}
	for twice := 0; twice < 2; twice++ {
		for _, identity := range identities {
			args = append(args, identity)
		}
	}
	var total int
	if err := ix.db.QueryRow(matched+` SELECT COUNT(*) FROM matched`, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	events, err := scanEvents(ix.db.Query(matched+` SELECT event.id,event.ts,event.session_id,event.runtime,event.verb,event.tool,
		event.target_entity_id,event.tags,event.decision,event.reason,event.origin,COALESCE(event.rule_id,''),COALESCE(event.layer,'')
		FROM event JOIN matched ON matched.id=event.id ORDER BY event.ts,event.id LIMIT ?`, append(args, limit)...))
	return events, total, err
}

// PathReconciliationsForFile returns bounded path joins and their full matching total.
func (ix *Index) PathReconciliationsForFile(sessionID string, paths []string, limit int) ([]PathReconciliation, int, error) {
	if limit <= 0 || limit > maxFileEvidenceLimit {
		limit = defaultFileEvidenceLimit
	}
	if len(paths) == 0 {
		return []PathReconciliation{}, 0, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(paths)), ",")
	args := []any{sessionID}
	for _, path := range paths {
		args = append(args, path)
	}
	where := `session_id=? AND (path IN (` + placeholders + `) OR old_path IN (` + placeholders + `))`
	for _, path := range paths {
		args = append(args, path)
	}
	var total int
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM path_reconciliation WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := ix.db.Query(`SELECT id,session_id,checkout_id,COALESCE(predecessor_checkpoint_id,0),
		current_checkpoint_id,COALESCE(event_id,0),COALESCE(result_id,0),path,old_path,
		classification,competing_actor,runtime_effect_digest,git_effect_digest,algorithm,
		reconciled_at,COALESCE(supersedes_id,0) FROM path_reconciliation WHERE `+where+
		` ORDER BY reconciled_at,id LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []PathReconciliation{}
	for rows.Next() {
		var fact PathReconciliation
		if err := rows.Scan(&fact.ID, &fact.SessionID, &fact.CheckoutID,
			&fact.PredecessorCheckpointID, &fact.CurrentCheckpointID, &fact.EventID,
			&fact.ResultID, &fact.Path, &fact.OldPath, &fact.Classification,
			&fact.CompetingActor, &fact.RuntimeEffectDigest, &fact.GitEffectDigest,
			&fact.Algorithm, &fact.ReconciledAt, &fact.SupersedesID); err != nil {
			return nil, 0, err
		}
		out = append(out, fact)
	}
	return out, total, rows.Err()
}

// pathReconciliationSupersedeLookup finds the fact a new one would supersede. It
// runs once per path under the daemon's write lock, so it must stay a search of
// path_reconciliation_current: a new checkpoint never matches, and without the
// index every call read the whole table.
const pathReconciliationSupersedeLookup = `SELECT id,classification,runtime_effect_digest,git_effect_digest
	FROM path_reconciliation WHERE current_checkpoint_id=? AND path=? AND algorithm=?
	ORDER BY id DESC LIMIT 1`

func (ix *Index) AppendPathReconciliation(f PathReconciliation) error {
	var existingID int64
	var class, runtimeDigest, gitDigest string
	err := ix.db.QueryRow(pathReconciliationSupersedeLookup, f.CurrentCheckpointID, f.Path, f.Algorithm).
		Scan(&existingID, &class, &runtimeDigest, &gitDigest)
	if err == nil && class == f.Classification && runtimeDigest == f.RuntimeEffectDigest && gitDigest == f.GitEffectDigest {
		return nil
	}
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if existingID != 0 {
		f.SupersedesID = existingID
	}
	_, err = ix.db.Exec(`INSERT INTO path_reconciliation(session_id,checkout_id,
		predecessor_checkpoint_id,current_checkpoint_id,event_id,result_id,path,old_path,
		classification,competing_actor,runtime_effect_digest,git_effect_digest,algorithm,
		reconciled_at,supersedes_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, f.SessionID,
		f.CheckoutID, nullableInt(f.PredecessorCheckpointID), f.CurrentCheckpointID,
		nullableInt(f.EventID), nullableInt(f.ResultID), f.Path, f.OldPath, f.Classification,
		f.CompetingActor, f.RuntimeEffectDigest, f.GitEffectDigest, f.Algorithm,
		f.ReconciledAt, nullableInt(f.SupersedesID))
	return err
}

func (ix *Index) PathReconciliationsForSession(sessionID string, limit int) ([]PathReconciliation, error) {
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	rows, err := ix.db.Query(`SELECT id,session_id,checkout_id,COALESCE(predecessor_checkpoint_id,0),
		current_checkpoint_id,COALESCE(event_id,0),COALESCE(result_id,0),path,old_path,
		classification,competing_actor,runtime_effect_digest,git_effect_digest,algorithm,
		reconciled_at,COALESCE(supersedes_id,0) FROM path_reconciliation
		WHERE session_id=? ORDER BY reconciled_at,id LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PathReconciliation{}
	for rows.Next() {
		var fact PathReconciliation
		if err := rows.Scan(&fact.ID, &fact.SessionID, &fact.CheckoutID,
			&fact.PredecessorCheckpointID, &fact.CurrentCheckpointID, &fact.EventID,
			&fact.ResultID, &fact.Path, &fact.OldPath, &fact.Classification,
			&fact.CompetingActor, &fact.RuntimeEffectDigest, &fact.GitEffectDigest,
			&fact.Algorithm, &fact.ReconciledAt, &fact.SupersedesID); err != nil {
			return nil, err
		}
		out = append(out, fact)
	}
	return out, rows.Err()
}

const changeCols = `id,session_id,repository_id,checkout_id,repository_identity_kind,checkout_root,
 session_runtime_claim,session_title_claim,kind,evidence_class,source_kind,source_ref,source_display,
 source_digest,recorded_at,COALESCE(capture_started_at,0),COALESCE(capture_ended_at,0),
 COALESCE(base_revision,''),COALESCE(head_revision,''),COALESCE(git_version,''),COALESCE(snapshot_digest,''),
 COALESCE(capture_attempts,0),COALESCE(included_layers,''),COALESCE(ignored_included,0),
 COALESCE(sparse_checkout,0),COALESCE(common_dir_digest,''),COALESCE(non_atomic,0),COALESCE(intent_label,''),
 COALESCE(verification_name,''),COALESCE(verification_boundary,''),COALESCE(verification_name_class,''),
 COALESCE(verification_result,''),COALESCE(executable_path,''),COALESCE(executable_digest,''),
 COALESCE(argv_digest,''),exit_code,COALESCE(signal,''),COALESCE(termination,''),
 COALESCE(environment_state,''),COALESCE(child_cleanup,''),COALESCE(limitation,'')`

func scanChange(rows *sql.Rows) ([]ChangeRecord, error) {
	defer rows.Close()
	out := []ChangeRecord{}
	for rows.Next() {
		var r ChangeRecord
		var ignored, sparse, nonAtomic int
		var exit sql.NullInt64
		err := rows.Scan(&r.ID, &r.SessionID, &r.RepositoryID, &r.CheckoutID, &r.RepositoryIdentityKind, &r.CheckoutRoot,
			&r.SessionRuntimeClaim, &r.SessionTitleClaim, &r.Kind, &r.EvidenceClass, &r.SourceKind, &r.SourceRef, &r.SourceDisplay,
			&r.SourceDigest, &r.RecordedAt, &r.CaptureStartedAt, &r.CaptureEndedAt, &r.BaseRevision, &r.HeadRevision,
			&r.GitVersion, &r.SnapshotDigest, &r.CaptureAttempts, &r.IncludedLayers, &ignored, &sparse, &r.CommonDirDigest,
			&nonAtomic, &r.IntentLabel, &r.VerificationName, &r.VerificationBoundary, &r.VerificationNameClass,
			&r.VerificationResult, &r.ExecutablePath, &r.ExecutableDigest, &r.ArgvDigest, &exit, &r.Signal, &r.Termination,
			&r.EnvironmentState, &r.ChildCleanup, &r.Limitation)
		if err != nil {
			return nil, err
		}
		r.IgnoredIncluded = ignored == 1
		r.SparseCheckout = sparse == 1
		r.NonAtomic = nonAtomic == 1
		if exit.Valid {
			v := int(exit.Int64)
			r.ExitCode = &v
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (ix *Index) ChangeRecordsForSession(sessionID string, limit int) ([]ChangeRecord, int, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	var total int
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM change_record WHERE session_id=?`, sessionID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := ix.db.Query(`SELECT `+changeCols+` FROM change_record WHERE session_id=? ORDER BY recorded_at DESC,id DESC LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, 0, err
	}
	recs, err := scanChange(rows)
	if err != nil {
		return nil, 0, err
	}
	for i := range recs {
		recs[i].Items, err = ix.ChangeItems(recs[i].ID)
		if err != nil {
			return nil, 0, err
		}
	}
	return recs, total, nil
}

func (ix *Index) ChangeItems(recordID int64) ([]ChangeItem, error) {
	rows, err := ix.db.Query(`SELECT ordinal,path,old_path,symbol,layer,status FROM change_item WHERE record_id=? ORDER BY ordinal`, recordID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChangeItem{}
	for rows.Next() {
		var i ChangeItem
		if err := rows.Scan(&i.Ordinal, &i.Path, &i.OldPath, &i.Symbol, &i.Layer, &i.Status); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

type SessionFileTouch struct {
	Identity     string `json:"identity"`
	Lineage      string `json:"lineage"`
	TargetSource string `json:"target_source"`
	Events       int    `json:"events"`
}

type SessionTouchPopulation struct {
	Touches       []SessionFileTouch
	DistinctFiles int
	EventTotal    int
	NonFileEvents int
	Partial       bool
	LastEventTS   int64
}

// SessionFileTouches reads the full event population, independent of presentation caps.
func (ix *Index) SessionFileTouches(sessionID string, limit int) (SessionTouchPopulation, error) {
	if limit <= 0 || limit > 10000 {
		limit = 10000
	}
	var out SessionTouchPopulation
	stat, err := ix.EventStatForSession(sessionID)
	if err != nil {
		return out, err
	}
	out.EventTotal, out.LastEventTS = stat.Total, stat.LastEventTS
	const resources = `WITH event_resources AS (
		SELECT e.id event_id,e.origin,n.identity,n.kind,er.source
		FROM event e JOIN event_resource er ON er.event_id=e.id JOIN entity n ON n.id=er.entity_id
		WHERE e.session_id=?
		UNION ALL
		SELECT e.id,e.origin,n.identity,n.kind,'legacy event.target_entity_id'
		FROM event e JOIN entity n ON n.id=e.target_entity_id
		WHERE e.session_id=? AND NOT EXISTS (SELECT 1 FROM event_resource er WHERE er.event_id=e.id)
	)`
	if err := ix.db.QueryRow(resources+` SELECT COUNT(*) FROM event e WHERE e.session_id=? AND NOT EXISTS
		(SELECT 1 FROM event_resources r WHERE r.event_id=e.id AND r.kind='file')`, sessionID, sessionID, sessionID).Scan(&out.NonFileEvents); err != nil {
		return out, err
	}
	if err := ix.db.QueryRow(resources+` SELECT COUNT(DISTINCT identity) FROM event_resources WHERE kind='file'`, sessionID, sessionID).Scan(&out.DistinctFiles); err != nil {
		return out, err
	}
	rows, err := ix.db.Query(resources+` SELECT identity,CASE WHEN MIN(origin)=MAX(origin) THEN MIN(origin) ELSE 'mixed' END,
		CASE WHEN MIN(source)=MAX(source) THEN MIN(source) ELSE 'mixed' END,COUNT(DISTINCT event_id)
		FROM event_resources WHERE kind='file' GROUP BY identity ORDER BY identity LIMIT ?`, sessionID, sessionID, limit)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var t SessionFileTouch
		if err := rows.Scan(&t.Identity, &t.Lineage, &t.TargetSource, &t.Events); err != nil {
			return out, err
		}
		out.Touches = append(out.Touches, t)
	}
	out.Partial = out.DistinctFiles > len(out.Touches)
	return out, rows.Err()
}

func (ix *Index) EnvelopeSessionIDs(limit int) ([]string, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := ix.db.Query(`SELECT session_id FROM change_record GROUP BY session_id ORDER BY MAX(recorded_at) DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SessionEditRow is one recorded edit of a session: a successful effect the
// runtime reported with its retained bodies described, never carried. The kind
// names which body pair exists — a replacement (before/after), a written
// content, or a runtime-supplied diff — because that is all the store knows
// about the edit's shape; there are no line offsets.
type SessionEditRow struct {
	ResultID     int64  `json:"result_id"`
	Ordinal      int    `json:"ordinal"`
	EventID      int64  `json:"event_id,omitempty"`
	CompletedAt  int64  `json:"completed_at"`
	Path         string `json:"path"`
	Operation    string `json:"operation"`
	Kind         string `json:"kind"` // replacement | content | diff
	ReplaceAll   bool   `json:"replace_all"`
	BeforeBytes  int    `json:"before_bytes"`
	AfterBytes   int    `json:"after_bytes"`
	ContentBytes int    `json:"content_bytes"`
	DiffBytes    int    `json:"diff_bytes"`
	SourceKind   string `json:"source_kind"`
	Tool         string `json:"tool,omitempty"`
}

// sessionEditPopulation is the bodied, successful, exactly reconciled effects of
// one session, with a rank per source kind: the live hook copy first, then a
// vendor patch result, then the vendor transcript. One logical result recorded
// from several sources keeps the best-ranked copy (ties: the earliest id), and
// only copies inside this population can displace one another, so a live copy
// that retained no body never hides the transcript copy that did.
//
// The session's results are selected first and each one's latest reconciliation
// is read by a correlated lookup: resolving the latest reconciliation for every
// result in the store and filtering afterwards took 18–26 seconds against the
// installed store (100k results); this shape takes milliseconds.
const sessionEditPopulation = `WITH mine AS (
	SELECT o.id result_id,o.completed_at,o.source_kind,o.tool,
		(SELECT r.id FROM result_reconciliation r WHERE r.result_id=o.id ORDER BY r.id DESC LIMIT 1) rec_id
	FROM result_observation o WHERE o.session_id=? AND o.state='success'
), current AS (
	SELECT m.* FROM mine m JOIN result_reconciliation r ON r.id=m.rec_id WHERE r.join_class='exact'
), bodied AS (
	SELECT c.result_id,cand.event_id,e.ordinal,c.completed_at,e.raw_identity,e.operation,e.replace_all,
		e.replacement_before_bytes,e.replacement_after_bytes,e.content_bytes,e.diff_bytes,c.source_kind,c.tool,
		CASE c.source_kind WHEN 'live-post-tool' THEN 0 WHEN 'vendor-patch-result' THEN 1 ELSE 2 END rank,
		CASE WHEN e.replacement_after_payload IS NOT NULL OR e.replacement_before_payload IS NOT NULL THEN 'replacement'
		     WHEN e.content_payload IS NOT NULL THEN 'content' ELSE 'diff' END kind
	FROM current c
	JOIN result_reconciliation_candidate cand ON cand.reconciliation_id=c.rec_id AND cand.selected=1
	JOIN result_effect e ON e.result_id=c.result_id
	WHERE e.replacement_after_payload IS NOT NULL OR e.replacement_before_payload IS NOT NULL
		OR e.content_payload IS NOT NULL OR e.diff_payload IS NOT NULL
), edits AS (
	SELECT b.* FROM bodied b WHERE NOT EXISTS (
		SELECT 1 FROM logical_result_relation l JOIN bodied p
			ON p.result_id = CASE WHEN l.result_id=b.result_id THEN l.peer_result_id ELSE l.result_id END
		WHERE (l.result_id=b.result_id OR l.peer_result_id=b.result_id)
		AND (p.rank<b.rank OR (p.rank=b.rank AND p.result_id<b.result_id))
	)
)`

// SessionEditRows pages a session's recorded edits in completion order. The
// caller owns the page ceiling (the console configuration's edits_page_size);
// this only refuses a non-positive limit. The count and the page are two reads,
// so Total can trail a page that raced a fresh ingest.
func (ix *Index) SessionEditRows(sessionID string, limit, offset int) ([]SessionEditRow, int, error) {
	if limit <= 0 {
		return nil, 0, fmt.Errorf("session edits: limit must be positive")
	}
	if offset < 0 {
		offset = 0
	}
	var total int
	if err := ix.db.QueryRow(sessionEditPopulation+` SELECT COUNT(*) FROM edits`, sessionID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := ix.db.Query(sessionEditPopulation+` SELECT result_id,event_id,ordinal,completed_at,raw_identity,operation,replace_all,
		replacement_before_bytes,replacement_after_bytes,content_bytes,diff_bytes,source_kind,tool,kind
		FROM edits ORDER BY completed_at,result_id,ordinal LIMIT ? OFFSET ?`, sessionID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []SessionEditRow{}
	for rows.Next() {
		var row SessionEditRow
		var replaceAll int
		if err := rows.Scan(&row.ResultID, &row.EventID, &row.Ordinal, &row.CompletedAt, &row.Path, &row.Operation, &replaceAll,
			&row.BeforeBytes, &row.AfterBytes, &row.ContentBytes, &row.DiffBytes, &row.SourceKind, &row.Tool, &row.Kind); err != nil {
			return nil, 0, err
		}
		row.ReplaceAll = replaceAll == 1
		out = append(out, row)
	}
	return out, total, rows.Err()
}
