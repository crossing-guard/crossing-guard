package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
)

var ErrResultObservationCollision = errors.New("result observation identity collision")
var ErrResultAliasCollision = errors.New("result native-call alias collision")

const (
	defaultExactResultLimit = 25
	maxExactResultLimit     = 200
)

type ResultObservation struct {
	ID                   int64          `json:"id"`
	ObservationID        string         `json:"observation_id"`
	SessionID            string         `json:"session_id"`
	SuppliedSessionID    string         `json:"supplied_session_id,omitempty"`
	Runtime              string         `json:"runtime"`
	Tool                 string         `json:"tool,omitempty"`
	NativeCallID         string         `json:"native_call_id,omitempty"`
	NativeCallKind       string         `json:"native_call_kind,omitempty"`
	SourceKind           string         `json:"source_kind"`
	SourceRef            string         `json:"source_ref,omitempty"`
	SourceSequence       string         `json:"source_sequence,omitempty"`
	SourceDigest         string         `json:"source_digest"`
	SourceSegmentID      string         `json:"source_segment_id,omitempty"`
	CollectorID          string         `json:"collector_id,omitempty"`
	CollectorVersion     string         `json:"collector_version,omitempty"`
	State                string         `json:"state"`
	ErrorClass           string         `json:"error_class,omitempty"`
	StartedAt            int64          `json:"started_at,omitempty"`
	ReturnedAt           int64          `json:"returned_at,omitempty"`
	CompletedAt          int64          `json:"completed_at,omitempty"`
	DurationMS           int64          `json:"duration_ms,omitempty"`
	MediaType            string         `json:"media_type,omitempty"`
	RawBytes             int            `json:"raw_bytes"`
	RawFieldBytes        int            `json:"raw_field_bytes"`
	DecodedBytes         int            `json:"decoded_bytes"`
	RetainedBytes        int            `json:"retained_bytes"`
	ExternalLocatorBytes int            `json:"external_locator_bytes"`
	PayloadDigest        string         `json:"payload_digest,omitempty"`
	Completeness         string         `json:"completeness"`
	Payload              []byte         `json:"-"`
	StdoutBytes          int            `json:"stdout_bytes"`
	StdoutDigest         string         `json:"stdout_digest,omitempty"`
	StderrBytes          int            `json:"stderr_bytes"`
	StderrDigest         string         `json:"stderr_digest,omitempty"`
	QueuedAt             int64          `json:"queued_at,omitempty"`
	ReceivedAt           int64          `json:"received_at,omitempty"`
	DeliveryAttempts     int            `json:"delivery_attempts"`
	DeliveryMode         string         `json:"delivery_mode"`
	JoinClass            string         `json:"join_class,omitempty"`
	ActionCoverageClass  string         `json:"action_coverage_class"`
	ActionBoundaryAt     int64          `json:"action_observation_boundary_at,omitempty"`
	Effects              []ResultEffect `json:"effects,omitempty"`
}

type ResultEffect struct {
	ResultID                 int64  `json:"result_id"`
	Ordinal                  int    `json:"ordinal"`
	EntityID                 string `json:"entity_id,omitempty"`
	RawIdentity              string `json:"raw_identity"`
	Operation                string `json:"operation"`
	MoveTarget               string `json:"move_target,omitempty"`
	EvidenceSource           string `json:"evidence_source"`
	SourceField              string `json:"source_field,omitempty"`
	Completeness             string `json:"completeness"`
	ReplaceAll               bool   `json:"replace_all,omitempty"`
	ReplacementBeforeBytes   int    `json:"replacement_before_bytes"`
	ReplacementBeforeDigest  string `json:"replacement_before_digest,omitempty"`
	ReplacementBeforePayload []byte `json:"-"`
	ReplacementAfterBytes    int    `json:"replacement_after_bytes"`
	ReplacementAfterDigest   string `json:"replacement_after_digest,omitempty"`
	ReplacementAfterPayload  []byte `json:"-"`
	ContentBytes             int    `json:"content_bytes"`
	ContentDigest            string `json:"content_digest,omitempty"`
	ContentPayload           []byte `json:"-"`
	DiffBytes                int    `json:"diff_bytes"`
	DiffDigest               string `json:"diff_digest,omitempty"`
	DiffCompleteness         string `json:"diff_completeness"`
	DiffPayload              []byte `json:"-"`
}

// RetainedTextBody carries body bytes and the metadata needed for release validation.
type RetainedTextBody struct {
	MediaType    string
	Digest       string
	Completeness string
	Bytes        []byte
}

// EventInputBodyForSession returns retained input only for a session-owned event.
func (ix *Index) EventInputBodyForSession(sessionID string, eventID int64) (RetainedTextBody, error) {
	var out RetainedTextBody
	err := ix.db.QueryRow(`SELECT i.media_type,i.digest,i.completeness,i.payload
		FROM event_input i JOIN event e ON e.id=i.event_id WHERE e.session_id=? AND e.id=?`,
		sessionID, eventID).Scan(&out.MediaType, &out.Digest, &out.Completeness, &out.Bytes)
	return out, err
}

// ResultBodyForSession returns a retained result only when its session matches.
func (ix *Index) ResultBodyForSession(sessionID string, resultID int64) (RetainedTextBody, error) {
	var out RetainedTextBody
	err := ix.db.QueryRow(`SELECT media_type,payload_digest,completeness,payload
		FROM result_observation WHERE session_id=? AND id=?`, sessionID, resultID).
		Scan(&out.MediaType, &out.Digest, &out.Completeness, &out.Bytes)
	return out, err
}

// ResultEffectBodyForSession returns one retained content/diff body after ownership checks.
func (ix *Index) ResultEffectBodyForSession(sessionID string, resultID int64, ordinal int, kind string) (RetainedTextBody, error) {
	var out RetainedTextBody
	out.MediaType = "text/plain; charset=utf-8"
	var query string
	switch kind {
	case "effect_content":
		query = `SELECT e.content_digest,e.completeness,e.content_payload`
	case "effect_diff":
		query = `SELECT e.diff_digest,e.diff_completeness,e.diff_payload`
	case "effect_before", "effect_after":
		// A replacement side is complete exactly when its bytes were retained; the
		// effect's own completeness describes the whole effect, not this side. The
		// column name is assembled from this switch's two literals, never from input.
		side := "before"
		if kind == "effect_after" {
			side = "after"
		}
		query = `SELECT e.replacement_` + side + `_digest,
			CASE WHEN e.replacement_` + side + `_payload IS NULL THEN 'unavailable' ELSE 'complete' END,
			e.replacement_` + side + `_payload`
	default:
		return out, sql.ErrNoRows
	}
	err := ix.db.QueryRow(query+` FROM result_effect e JOIN result_observation o ON o.id=e.result_id
		WHERE o.session_id=? AND e.result_id=? AND e.ordinal=?`, sessionID, resultID, ordinal).
		Scan(&out.Digest, &out.Completeness, &out.Bytes)
	return out, err
}

type ResultCandidate struct {
	EventID         int64  `json:"event_id"`
	CandidateReason string `json:"candidate_reason"`
	Selected        bool   `json:"selected"`
}

type ResultNativeCallAlias struct {
	ResultID       int64  `json:"result_id"`
	NativeCallKind string `json:"native_call_kind"`
	NativeCallID   string `json:"native_call_id"`
	Algorithm      string `json:"algorithm"`
}

type ResultAliasRepairCandidate struct {
	ResultID      int64
	ObservationID string
	NativeCallID  string
}

type AmbiguousResultRepairCandidate struct {
	ResultID      int64
	ObservationID string
}

type ResultAliasRepairContract struct {
	Runtime              string
	ResultSourceKind     string
	ResultNativeCallKind string
	AliasNativeCallKind  string
	Tool                 string
}

type ResultReconciliation struct {
	ID           int64             `json:"id"`
	ResultID     int64             `json:"result_id"`
	JoinClass    string            `json:"join_class"`
	Algorithm    string            `json:"algorithm"`
	ReconciledAt int64             `json:"reconciled_at"`
	SupersedesID int64             `json:"supersedes_id,omitempty"`
	Candidates   []ResultCandidate `json:"candidates,omitempty"`
}

type ResultStats struct {
	SourceObserved     int `json:"source_observed"`
	LogicalCompletions int `json:"logical_completions"`
	Exact              int `json:"exact"`
	Indirect           int `json:"indirect"`
	Ambiguous          int `json:"ambiguous"`
	Unjoined           int `json:"unjoined"`
	ActionObserved     int `json:"action_observed"`
	ActionAmbiguous    int `json:"action_ambiguous"`
	ExpectedMissing    int `json:"expected_action_missing"`
	BeforeBoundary     int `json:"before_action_boundary"`
	NoBoundary         int `json:"no_action_boundary"`
	ActionTimeUnknown  int `json:"action_time_unknown"`
}

type ResultActionCoverage struct {
	ResultID   int64  `json:"result_id"`
	JoinClass  string `json:"join_class,omitempty"`
	Class      string `json:"action_coverage_class"`
	BoundaryAt int64  `json:"action_observation_boundary_at,omitempty"`
}

type LiveSessionRuntime struct {
	SessionID        string `json:"session_id"`
	Runtime          string `json:"runtime"`
	TranscriptPath   string `json:"transcript_path,omitempty"`
	WorkingDirectory string `json:"working_directory,omitempty"`
}

type IdleSession struct {
	SessionID        string
	Runtime          string
	LastEventAt      int64
	WorkingDirectory string
}

type MissingResultCandidate struct {
	EventID        int64
	ObservationID  string
	Runtime        string
	NativeCallID   string
	NativeCallKind string
}

type TranscriptCursor struct {
	Runtime             string `json:"runtime"`
	SourceRef           string `json:"source_ref"`
	SourceSegmentID     string `json:"source_segment_id"`
	SessionID           string `json:"session_id,omitempty"`
	WorkingDirectory    string `json:"working_directory,omitempty"`
	FileSize            int64  `json:"file_size"`
	FileMTime           int64  `json:"file_mtime"`
	GenerationDigest    string `json:"generation_digest"`
	SourceSequence      string `json:"source_sequence"`
	CommittedOffset     int64  `json:"committed_offset"`
	SourceLine          int64  `json:"source_line"`
	ParserState         []byte `json:"-"`
	ParserStateDigest   string `json:"parser_state_digest,omitempty"`
	RescanNeeded        bool   `json:"rescan_needed"`
	ContinuationNeeded  bool   `json:"continuation_needed"`
	ActionParserVersion int    `json:"action_parser_version"`
	UpdatedAt           int64  `json:"updated_at"`
}

func normalizeResultDefaults(r *ResultObservation) {
	if r.Completeness == "" {
		r.Completeness = "unavailable"
	}
	if r.DeliveryAttempts < 1 {
		r.DeliveryAttempts = 1
	}
	if r.DeliveryMode == "" {
		r.DeliveryMode = "transcript"
	}
	if r.PayloadDigest == "" && (r.Completeness == "metadata-only" || r.Completeness == "redacted") {
		r.PayloadDigest = r.SourceDigest
	}
	if r.RetainedBytes == 0 && len(r.Payload) > 0 {
		r.RetainedBytes = len(r.Payload)
	}
}

func retainedBodyDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256-v1:" + hex.EncodeToString(sum[:])
}

func validateRetainedBody(label string, body []byte, bodyBytes int, digest string,
	required bool) error {
	if bodyBytes < 0 {
		return fmt.Errorf("%s byte count is negative", label)
	}
	if body == nil {
		if required {
			return fmt.Errorf("%s is marked complete but its body is not retained", label)
		}
		return nil
	}
	if len(body) != bodyBytes {
		return fmt.Errorf("%s byte count does not match retained body", label)
	}
	if digest != retainedBodyDigest(body) {
		return fmt.Errorf("%s digest does not match retained body", label)
	}
	return nil
}

func validateResultBodies(r ResultObservation, effects []ResultEffect) error {
	if r.RawBytes < 0 || r.RawFieldBytes < 0 || r.DecodedBytes < 0 || r.RetainedBytes < 0 ||
		r.ExternalLocatorBytes < 0 || r.StdoutBytes < 0 || r.StderrBytes < 0 {
		return errors.New("result byte metadata cannot be negative")
	}
	switch r.Completeness {
	case "complete":
		if r.Payload == nil {
			return errors.New("complete result must retain its payload")
		}
		if err := validateRetainedBody("result payload", r.Payload, r.RetainedBytes,
			r.PayloadDigest, true); err != nil {
			return err
		}
	case "metadata-only", "redacted":
		if r.Payload != nil || r.RetainedBytes != 0 || r.PayloadDigest == "" {
			return errors.New("metadata-only result must omit payload and retain a digest")
		}
	case "unavailable":
		if r.Payload != nil || r.RetainedBytes != 0 {
			return errors.New("unavailable result must omit payload")
		}
	default:
		return fmt.Errorf("unsupported result completeness %q", r.Completeness)
	}
	for index := range effects {
		e := &effects[index]
		if e.Completeness == "" {
			e.Completeness = "unknown"
		}
		if e.DiffCompleteness == "" {
			e.DiffCompleteness = "unavailable"
		}
		if e.ContentBytes == 0 && len(e.ContentPayload) > 0 {
			e.ContentBytes = len(e.ContentPayload)
		}
		if e.DiffBytes == 0 && len(e.DiffPayload) > 0 {
			e.DiffBytes = len(e.DiffPayload)
		}
		for _, body := range []struct {
			label   string
			payload []byte
			bytes   int
			digest  string
		}{
			{fmt.Sprintf("result effect %d before", index), e.ReplacementBeforePayload, e.ReplacementBeforeBytes, e.ReplacementBeforeDigest},
			{fmt.Sprintf("result effect %d after", index), e.ReplacementAfterPayload, e.ReplacementAfterBytes, e.ReplacementAfterDigest},
			{fmt.Sprintf("result effect %d content", index), e.ContentPayload, e.ContentBytes, e.ContentDigest},
		} {
			// Replacement/content metadata may be retained without its body. A non-nil
			// payload is the exact producer claim that bytes are present, including an
			// intentionally empty body; only that claim requires body verification.
			requireEffectBody := body.payload != nil
			if err := validateRetainedBody(body.label, body.payload, body.bytes, body.digest,
				requireEffectBody); err != nil {
				return err
			}
		}
		if err := validateRetainedBody(fmt.Sprintf("result effect %d diff", index),
			e.DiffPayload, e.DiffBytes, e.DiffDigest, e.DiffCompleteness == "complete"); err != nil {
			return err
		}
	}
	return nil
}

func (ix *Index) AppendResultObservation(r ResultObservation, effects []ResultEffect) (int64, bool, error) {
	normalizeResultDefaults(&r)
	if err := r.Validate(); err != nil {
		return 0, false, err
	}
	if err := validateResultBodies(r, effects); err != nil {
		return 0, false, err
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var existingID int64
	var existingDigest string
	err = tx.QueryRow(`SELECT id,source_digest FROM result_observation WHERE observation_id=?`, r.ObservationID).Scan(&existingID, &existingDigest)
	if err == nil {
		if existingDigest != r.SourceDigest {
			return 0, false, ErrResultObservationCollision
		}
		if err := tx.Commit(); err != nil {
			return 0, false, err
		}
		return existingID, true, nil
	}
	if err != sql.ErrNoRows {
		return 0, false, err
	}
	res, err := tx.Exec(`INSERT INTO result_observation(observation_id,session_id,supplied_session_id,
		runtime,tool,native_call_id,native_call_kind,source_kind,source_ref,source_sequence,
		source_digest,source_segment_id,collector_id,collector_version,state,error_class,
		started_at,returned_at,completed_at,duration_ms,media_type,raw_bytes,raw_field_bytes,
		decoded_bytes,retained_bytes,external_locator_bytes,payload_digest,completeness,payload,
		stdout_bytes,stdout_digest,stderr_bytes,stderr_digest,queued_at,received_at,
		delivery_attempts,delivery_mode)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ObservationID, r.SessionID, r.SuppliedSessionID, r.Runtime, r.Tool, r.NativeCallID,
		r.NativeCallKind, r.SourceKind, r.SourceRef, r.SourceSequence, r.SourceDigest,
		r.SourceSegmentID, r.CollectorID, r.CollectorVersion, r.State, r.ErrorClass,
		r.StartedAt, r.ReturnedAt, r.CompletedAt, r.DurationMS, r.MediaType, r.RawBytes,
		r.RawFieldBytes, r.DecodedBytes, r.RetainedBytes, r.ExternalLocatorBytes,
		r.PayloadDigest, r.Completeness, retainedBytes(r.Payload), r.StdoutBytes,
		r.StdoutDigest, r.StderrBytes, r.StderrDigest, r.QueuedAt, r.ReceivedAt,
		r.DeliveryAttempts, r.DeliveryMode)
	if err != nil {
		return 0, false, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, false, err
	}
	for ordinal := range effects {
		e := effects[ordinal]
		e.Ordinal = ordinal
		_, err = tx.Exec(`INSERT INTO result_effect(result_id,ordinal,entity_id,raw_identity,
			operation,move_target,evidence_source,source_field,completeness,replace_all,
			replacement_before_bytes,replacement_before_digest,replacement_before_payload,replacement_after_bytes,
			replacement_after_digest,replacement_after_payload,content_bytes,content_digest,content_payload,diff_bytes,
			diff_digest,diff_completeness,diff_payload) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, e.Ordinal, e.EntityID, e.RawIdentity, e.Operation, e.MoveTarget, e.EvidenceSource,
			e.SourceField, e.Completeness, boolInt(e.ReplaceAll), e.ReplacementBeforeBytes,
			e.ReplacementBeforeDigest, retainedBytes(e.ReplacementBeforePayload), e.ReplacementAfterBytes, e.ReplacementAfterDigest, retainedBytes(e.ReplacementAfterPayload),
			e.ContentBytes, e.ContentDigest, retainedBytes(e.ContentPayload), e.DiffBytes,
			e.DiffDigest, e.DiffCompleteness, retainedBytes(e.DiffPayload))
		if err != nil {
			return 0, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return id, false, nil
}

func nullableBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

func retainedBytes(value []byte) any {
	if value == nil {
		return nil
	}
	return value
}

func (ix *Index) AppendResultNativeCallAlias(alias ResultNativeCallAlias) (bool, error) {
	if alias.ResultID <= 0 || alias.NativeCallKind == "" || alias.NativeCallID == "" || alias.Algorithm == "" {
		return false, errors.New("result native-call alias requires result, kind, id, and algorithm")
	}
	if len(alias.NativeCallKind) > 128 || len(alias.NativeCallID) > 16<<10 || len(alias.Algorithm) > 128 {
		return false, errors.New("result native-call alias exceeds retention limit")
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`INSERT INTO result_native_call_alias(result_id,native_call_kind,native_call_id,algorithm)
		VALUES(?,?,?,?) ON CONFLICT(result_id,native_call_kind,native_call_id) DO NOTHING`,
		alias.ResultID, alias.NativeCallKind, alias.NativeCallID, alias.Algorithm)
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if rows == 0 {
		var algorithm string
		if err := tx.QueryRow(`SELECT algorithm FROM result_native_call_alias
			WHERE result_id=? AND native_call_kind=? AND native_call_id=?`, alias.ResultID,
			alias.NativeCallKind, alias.NativeCallID).Scan(&algorithm); err != nil {
			return false, err
		}
		if algorithm != alias.Algorithm {
			return false, ErrResultAliasCollision
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return rows == 0, nil
}

func (ix *Index) ResultNativeCallAliases(resultID int64) ([]ResultNativeCallAlias, error) {
	rows, err := ix.db.Query(`SELECT result_id,native_call_kind,native_call_id,algorithm
		FROM result_native_call_alias WHERE result_id=? ORDER BY native_call_kind,native_call_id`, resultID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ResultNativeCallAlias{}
	for rows.Next() {
		var alias ResultNativeCallAlias
		if err := rows.Scan(&alias.ResultID, &alias.NativeCallKind, &alias.NativeCallID, &alias.Algorithm); err != nil {
			return nil, err
		}
		out = append(out, alias)
	}
	return out, rows.Err()
}

func (ix *Index) ReconcileResultObservation(resultID, reconciledAt int64) (ResultReconciliation, error) {
	var runtime, session, nativeID, nativeKind, tool string
	if err := ix.db.QueryRow(`SELECT runtime,session_id,native_call_id,native_call_kind,tool
		FROM result_observation WHERE id=?`, resultID).Scan(&runtime, &session, &nativeID, &nativeKind, &tool); err != nil {
		return ResultReconciliation{}, err
	}
	type identityRef struct {
		kind, id, reason string
	}
	identities := []identityRef{}
	if nativeID != "" {
		identities = append(identities, identityRef{kind: nativeKind, id: nativeID,
			reason: "runtime+session+native-id-kind+tool"})
	}
	aliases, err := ix.ResultNativeCallAliases(resultID)
	if err != nil {
		return ResultReconciliation{}, err
	}
	for _, alias := range aliases {
		identities = append(identities, identityRef{kind: alias.NativeCallKind, id: alias.NativeCallID,
			reason: "runtime+session+native-id-alias:" + alias.Algorithm + "+tool"})
	}
	candidateByEvent := map[int64]ResultCandidate{}
	for _, identity := range identities {
		rows, err := ix.db.Query(`SELECT e.id,e.tool,e.decision FROM event e JOIN event_delivery d ON d.event_id=e.id
			WHERE e.runtime=? AND e.session_id=? AND d.native_call_id=? AND d.native_call_kind=?
			ORDER BY e.id`, runtime, session, identity.id, identity.kind)
		if err != nil {
			return ResultReconciliation{}, err
		}
		for rows.Next() {
			var eventID int64
			var eventTool, decision string
			if err := rows.Scan(&eventID, &eventTool, &decision); err != nil {
				_ = rows.Close()
				return ResultReconciliation{}, err
			}
			if tool != "" && eventTool != "" && tool != eventTool {
				continue
			}
			reason := identity.reason
			if decision != "" {
				reason += "+decision:" + decision
			}
			if _, exists := candidateByEvent[eventID]; !exists {
				candidateByEvent[eventID] = ResultCandidate{EventID: eventID, CandidateReason: reason}
			}
		}
		if err := rows.Close(); err != nil {
			return ResultReconciliation{}, err
		}
	}
	candidateIDs := make([]int64, 0, len(candidateByEvent))
	for eventID := range candidateByEvent {
		candidateIDs = append(candidateIDs, eventID)
	}
	sort.Slice(candidateIDs, func(i, j int) bool { return candidateIDs[i] < candidateIDs[j] })
	candidates := make([]ResultCandidate, 0, len(candidateIDs))
	for _, eventID := range candidateIDs {
		candidates = append(candidates, candidateByEvent[eventID])
	}
	joinClass := "unjoined"
	duplicateDelivery := false
	if len(candidates) == 1 {
		joinClass, candidates[0].Selected = "exact", true
	} else if len(candidates) > 1 {
		allowed := -1
		for index, candidate := range candidates {
			if strings.HasSuffix(candidate.CandidateReason, "+decision:allow") {
				if allowed != -1 {
					allowed = -2
					break
				}
				allowed = index
			}
		}
		if allowed >= 0 {
			joinClass, candidates[allowed].Selected = "exact", true
		} else if equivalent, equivalentErr := ix.equivalentActionCandidates(candidates); equivalentErr != nil {
			return ResultReconciliation{}, equivalentErr
		} else if equivalent {
			joinClass, candidates[0].Selected, duplicateDelivery = "exact", true, true
			for index := range candidates {
				if index == 0 {
					candidates[index].CandidateReason += "+duplicate-delivery-representative"
				} else {
					candidates[index].CandidateReason += "+duplicate-delivery-peer"
				}
			}
		} else {
			joinClass = "ambiguous"
		}
	}
	previous, found, err := ix.currentResultReconciliation(resultID)
	if err != nil {
		return ResultReconciliation{}, err
	}
	if found && previous.JoinClass == joinClass && sameCandidateSet(previous.Candidates, candidates) {
		return previous, nil
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return ResultReconciliation{}, err
	}
	defer func() { _ = tx.Rollback() }()
	supersedes := any(nil)
	var supersedesID int64
	if found {
		supersedes, supersedesID = previous.ID, previous.ID
	}
	algorithm := "native-id-v1"
	if len(aliases) > 0 {
		algorithm = "native-id-alias-v1"
	}
	if duplicateDelivery {
		algorithm = "native-id-duplicate-delivery-v1"
	}
	res, err := tx.Exec(`INSERT INTO result_reconciliation(result_id,join_class,algorithm,reconciled_at,supersedes_id)
		VALUES(?,?,?,?,?)`, resultID, joinClass, algorithm, reconciledAt, supersedes)
	if err != nil {
		return ResultReconciliation{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return ResultReconciliation{}, err
	}
	for _, candidate := range candidates {
		if _, err := tx.Exec(`INSERT INTO result_reconciliation_candidate(reconciliation_id,event_id,candidate_reason,selected)
			VALUES(?,?,?,?)`, id, candidate.EventID, candidate.CandidateReason, boolInt(candidate.Selected)); err != nil {
			return ResultReconciliation{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return ResultReconciliation{}, err
	}
	return ResultReconciliation{ID: id, ResultID: resultID, JoinClass: joinClass,
		Algorithm: algorithm, ReconciledAt: reconciledAt, SupersedesID: supersedesID,
		Candidates: candidates}, nil
}

type actionCandidateSignature struct {
	TS, RawBytes, CapturedBytes                                     int64
	Verb, Tool, Target, Tags, Decision, Reason, Origin              string
	CollectorID, CollectorVersion, NativeCallID, NativeCallKind     string
	MediaType, InputDigest, InputCompleteness, InputSourceReference string
}

type actionResourceSignature struct {
	Ordinal                                                 int
	EntityID, Source, RawIdentity, Operation, EvidenceClass string
	SourceField, Completeness                               string
}

func (ix *Index) actionCandidateSignature(eventID int64) (actionCandidateSignature, []actionResourceSignature, error) {
	var signature actionCandidateSignature
	err := ix.db.QueryRow(`SELECT e.ts,COALESCE(e.verb,''),COALESCE(e.tool,''),
		COALESCE(e.target_entity_id,''),COALESCE(e.tags,''),COALESCE(e.decision,''),
		COALESCE(e.reason,''),COALESCE(e.origin,''),d.collector_id,d.collector_version,
		d.native_call_id,d.native_call_kind,i.media_type,i.raw_bytes,i.captured_bytes,
		i.digest,i.completeness,i.source_ref FROM event e
		JOIN event_delivery d ON d.event_id=e.id JOIN event_input i ON i.event_id=e.id
		WHERE e.id=?`, eventID).Scan(&signature.TS, &signature.Verb, &signature.Tool,
		&signature.Target, &signature.Tags, &signature.Decision, &signature.Reason,
		&signature.Origin, &signature.CollectorID, &signature.CollectorVersion,
		&signature.NativeCallID, &signature.NativeCallKind, &signature.MediaType,
		&signature.RawBytes, &signature.CapturedBytes, &signature.InputDigest,
		&signature.InputCompleteness, &signature.InputSourceReference)
	if err != nil {
		return signature, nil, err
	}
	if signature.InputDigest == "" {
		return signature, nil, nil
	}
	rows, err := ix.db.Query(`SELECT ordinal,entity_id,source,raw_identity,operation,
		evidence_class,source_field,completeness FROM event_resource WHERE event_id=? ORDER BY ordinal`, eventID)
	if err != nil {
		return signature, nil, err
	}
	defer rows.Close()
	resources := []actionResourceSignature{}
	for rows.Next() {
		var resource actionResourceSignature
		if err := rows.Scan(&resource.Ordinal, &resource.EntityID, &resource.Source,
			&resource.RawIdentity, &resource.Operation, &resource.EvidenceClass,
			&resource.SourceField, &resource.Completeness); err != nil {
			return signature, nil, err
		}
		resources = append(resources, resource)
	}
	return signature, resources, rows.Err()
}

func (ix *Index) equivalentActionCandidates(candidates []ResultCandidate) (bool, error) {
	if len(candidates) < 2 {
		return false, nil
	}
	want, wantResources, err := ix.actionCandidateSignature(candidates[0].EventID)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil || want.InputDigest == "" {
		return false, err
	}
	for _, candidate := range candidates[1:] {
		got, gotResources, err := ix.actionCandidateSignature(candidate.EventID)
		if err == sql.ErrNoRows {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if got != want || len(gotResources) != len(wantResources) {
			return false, nil
		}
		for index := range wantResources {
			if gotResources[index] != wantResources[index] {
				return false, nil
			}
		}
	}
	return true, nil
}

func (ix *Index) AmbiguousResultRepairCandidates(limit int) ([]AmbiguousResultRepairCandidate, error) {
	if limit <= 0 || limit > 256 {
		limit = 256
	}
	rows, err := ix.db.Query(`SELECT reconciliation.result_id,observation.observation_id
		FROM result_reconciliation reconciliation
		JOIN result_observation observation ON observation.id=reconciliation.result_id
		WHERE reconciliation.join_class='ambiguous'
		AND reconciliation.id=(SELECT MAX(current.id) FROM result_reconciliation current
			WHERE current.result_id=reconciliation.result_id)
		ORDER BY reconciliation.result_id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AmbiguousResultRepairCandidate{}
	for rows.Next() {
		var candidate AmbiguousResultRepairCandidate
		if err := rows.Scan(&candidate.ResultID, &candidate.ObservationID); err != nil {
			return nil, err
		}
		out = append(out, candidate)
	}
	return out, rows.Err()
}

func sameCandidateSet(a, b []ResultCandidate) bool {
	if len(a) != len(b) {
		return false
	}
	aIDs, bIDs := make([]int64, len(a)), make([]int64, len(b))
	for i := range a {
		aIDs[i] = a[i].EventID
	}
	for i := range b {
		bIDs[i] = b[i].EventID
	}
	sort.Slice(aIDs, func(i, j int) bool { return aIDs[i] < aIDs[j] })
	sort.Slice(bIDs, func(i, j int) bool { return bIDs[i] < bIDs[j] })
	for i := range aIDs {
		if aIDs[i] != bIDs[i] {
			return false
		}
	}
	return true
}

func (ix *Index) currentResultReconciliation(resultID int64) (ResultReconciliation, bool, error) {
	var out ResultReconciliation
	var supersedes sql.NullInt64
	err := ix.db.QueryRow(`SELECT id,result_id,join_class,algorithm,reconciled_at,supersedes_id
		FROM result_reconciliation WHERE result_id=? ORDER BY reconciled_at DESC,id DESC LIMIT 1`, resultID).
		Scan(&out.ID, &out.ResultID, &out.JoinClass, &out.Algorithm, &out.ReconciledAt, &supersedes)
	if err == sql.ErrNoRows {
		return out, false, nil
	}
	if err != nil {
		return out, false, err
	}
	if supersedes.Valid {
		out.SupersedesID = supersedes.Int64
	}
	rows, err := ix.db.Query(`SELECT event_id,candidate_reason,selected FROM result_reconciliation_candidate
		WHERE reconciliation_id=? ORDER BY event_id`, out.ID)
	if err != nil {
		return out, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var candidate ResultCandidate
		var selected int
		if err := rows.Scan(&candidate.EventID, &candidate.CandidateReason, &selected); err != nil {
			return out, false, err
		}
		candidate.Selected = selected == 1
		out.Candidates = append(out.Candidates, candidate)
	}
	return out, true, rows.Err()
}

// CurrentResultReconciliation exposes the latest append-only reconciliation fact for
// a result detail view. It does not recalculate or guess a join.
func (ix *Index) CurrentResultReconciliation(resultID int64) (ResultReconciliation, bool, error) {
	return ix.currentResultReconciliation(resultID)
}

func (ix *Index) ResultStatsForSession(sessionID string) (ResultStats, error) {
	var stats ResultStats
	err := ix.db.QueryRow(`SELECT COUNT(*),COUNT(CASE WHEN NOT EXISTS (
		SELECT 1 FROM logical_result_relation l WHERE l.peer_result_id=o.id
	) THEN 1 END) FROM result_observation o WHERE session_id=?`, sessionID).
		Scan(&stats.SourceObserved, &stats.LogicalCompletions)
	if err != nil {
		return stats, err
	}
	rows, err := ix.db.Query(`WITH boundaries AS (
		SELECT e.runtime,MIN(e.ts) boundary_at FROM event e
		JOIN event_delivery d ON d.event_id=e.id
		WHERE e.origin='live' AND e.session_id=? GROUP BY e.runtime
	), classified AS (
		SELECT COALESCE(r.join_class,'') join_class,
		CASE
			WHEN r.join_class IN ('exact','indirect','same-logical-result') THEN 'action-observed'
			WHEN r.join_class='ambiguous' THEN 'action-ambiguous'
			WHEN o.source_kind='live-post-tool' THEN 'expected-action-missing'
			WHEN COALESCE(b.boundary_at,0)=0 THEN 'no-action-boundary'
			WHEN o.completed_at=0 THEN 'action-time-unknown'
			WHEN o.completed_at<b.boundary_at THEN 'before-action-boundary'
			ELSE 'expected-action-missing'
		END action_class
		FROM result_observation o
		LEFT JOIN result_reconciliation r ON r.result_id=o.id
			AND r.id=(SELECT MAX(current.id) FROM result_reconciliation current WHERE current.result_id=o.id)
		LEFT JOIN boundaries b ON b.runtime=o.runtime
		WHERE o.session_id=?
	) SELECT join_class,action_class,COUNT(*) FROM classified GROUP BY join_class,action_class`, sessionID, sessionID)
	if err != nil {
		return stats, err
	}
	defer rows.Close()
	for rows.Next() {
		var class string
		var actionClass string
		var count int
		if err := rows.Scan(&class, &actionClass, &count); err != nil {
			return stats, err
		}
		switch class {
		case "exact":
			stats.Exact += count
		case "indirect":
			stats.Indirect += count
		case "ambiguous":
			stats.Ambiguous += count
		case "unjoined":
			stats.Unjoined += count
		}
		switch actionClass {
		case "action-observed":
			stats.ActionObserved += count
		case "action-ambiguous":
			stats.ActionAmbiguous += count
		case "expected-action-missing":
			stats.ExpectedMissing += count
		case "before-action-boundary":
			stats.BeforeBoundary += count
		case "no-action-boundary":
			stats.NoBoundary += count
		case "action-time-unknown":
			stats.ActionTimeUnknown += count
		}
	}
	return stats, rows.Err()
}

func classifyResultActionCoverage(sourceKind, joinClass string, completedAt, boundaryAt int64) string {
	switch joinClass {
	case "exact", "indirect", "same-logical-result":
		return "action-observed"
	case "ambiguous":
		return "action-ambiguous"
	}
	if sourceKind == "live-post-tool" {
		return "expected-action-missing"
	}
	if boundaryAt == 0 {
		return "no-action-boundary"
	}
	if completedAt == 0 {
		return "action-time-unknown"
	}
	if completedAt < boundaryAt {
		return "before-action-boundary"
	}
	return "expected-action-missing"
}

func (ix *Index) ResultActionCoverage(resultID int64) (ResultActionCoverage, error) {
	var out ResultActionCoverage
	var sourceKind string
	var completedAt int64
	err := ix.db.QueryRow(`SELECT o.id,o.source_kind,o.completed_at,COALESCE((
		SELECT r.join_class FROM result_reconciliation r WHERE r.result_id=o.id
		ORDER BY r.reconciled_at DESC,r.id DESC LIMIT 1),''),COALESCE((
		SELECT MIN(e.ts) FROM event e JOIN event_delivery d ON d.event_id=e.id
		WHERE e.origin='live' AND e.session_id=o.session_id AND e.runtime=o.runtime),0)
		FROM result_observation o WHERE o.id=?`, resultID).
		Scan(&out.ResultID, &sourceKind, &completedAt, &out.JoinClass, &out.BoundaryAt)
	if err != nil {
		return out, err
	}
	out.Class = classifyResultActionCoverage(sourceKind, out.JoinClass, completedAt, out.BoundaryAt)
	return out, nil
}

func (ix *Index) ResultsForSession(sessionID string, limit int) ([]ResultObservation, error) {
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	rows, err := ix.db.Query(`SELECT id,observation_id,session_id,supplied_session_id,runtime,tool,
		native_call_id,native_call_kind,source_kind,source_ref,source_sequence,source_digest,
		source_segment_id,collector_id,collector_version,state,error_class,started_at,returned_at,
		completed_at,duration_ms,media_type,raw_bytes,raw_field_bytes,decoded_bytes,retained_bytes,
		external_locator_bytes,payload_digest,completeness,stdout_bytes,stdout_digest,stderr_bytes,
		stderr_digest,queued_at,received_at,delivery_attempts,delivery_mode,COALESCE((
			SELECT current.join_class FROM result_reconciliation current WHERE current.result_id=result_observation.id
			ORDER BY current.reconciled_at DESC,current.id DESC LIMIT 1),''),COALESCE((
			SELECT MIN(action.ts) FROM event action JOIN event_delivery delivery ON delivery.event_id=action.id
			WHERE action.origin='live' AND action.session_id=result_observation.session_id
			AND action.runtime=result_observation.runtime),0)
		FROM result_observation WHERE session_id=? ORDER BY completed_at,id LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ResultObservation{}
	for rows.Next() {
		var r ResultObservation
		if err := rows.Scan(&r.ID, &r.ObservationID, &r.SessionID, &r.SuppliedSessionID,
			&r.Runtime, &r.Tool, &r.NativeCallID, &r.NativeCallKind, &r.SourceKind, &r.SourceRef,
			&r.SourceSequence, &r.SourceDigest, &r.SourceSegmentID, &r.CollectorID,
			&r.CollectorVersion, &r.State, &r.ErrorClass, &r.StartedAt, &r.ReturnedAt,
			&r.CompletedAt, &r.DurationMS, &r.MediaType, &r.RawBytes, &r.RawFieldBytes,
			&r.DecodedBytes, &r.RetainedBytes, &r.ExternalLocatorBytes, &r.PayloadDigest,
			&r.Completeness, &r.StdoutBytes, &r.StdoutDigest, &r.StderrBytes, &r.StderrDigest,
			&r.QueuedAt, &r.ReceivedAt, &r.DeliveryAttempts, &r.DeliveryMode,
			&r.JoinClass, &r.ActionBoundaryAt); err != nil {
			return nil, err
		}
		r.ActionCoverageClass = classifyResultActionCoverage(r.SourceKind, r.JoinClass,
			r.CompletedAt, r.ActionBoundaryAt)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for index := range out {
		out[index].Effects, err = ix.ResultEffects(out[index].ID)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// LiveResultObservation is the bounded projection of one live post-tool
// completion the natural signal emitter reads: identity and tool only, no
// payloads (helper-session-attachment plan D2).
type LiveResultObservation struct {
	ID          int64
	Runtime     string
	SessionID   string
	Tool        string
	CompletedAt int64
	ReceivedAt  int64
}

// ResultObservationHead is the newest result id (0 when empty): where a
// consumer meeting this table for the first time starts.
func (ix *Index) ResultObservationHead() (int64, error) {
	var head int64
	err := ix.db.QueryRow(`SELECT COALESCE(MAX(id),0) FROM result_observation`).Scan(&head)
	return head, err
}

// ResultObservationsAfter returns live post-tool completions with id strictly
// greater than after, oldest first, bounded. Only source_kind='live-post-tool'
// rows qualify: transcript-derived rows describe calls whose boundary has
// already passed and would double every hook-observed call (red-team H6). The
// returned cursor is the last examined id in the scanned range so the emitter
// advances past filtered-out rows too.
func (ix *Index) ResultObservationsAfter(after int64, limit int) ([]LiveResultObservation, int64, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := ix.db.Query(`SELECT id,runtime,session_id,tool,completed_at,received_at,source_kind
		FROM result_observation WHERE id > ? ORDER BY id ASC LIMIT ?`, after, limit)
	if err != nil {
		return nil, after, err
	}
	defer rows.Close()
	out := []LiveResultObservation{}
	last := after
	for rows.Next() {
		var item LiveResultObservation
		var sourceKind string
		if err := rows.Scan(&item.ID, &item.Runtime, &item.SessionID, &item.Tool, &item.CompletedAt, &item.ReceivedAt, &sourceKind); err != nil {
			return nil, after, err
		}
		last = item.ID
		if sourceKind != "live-post-tool" {
			continue
		}
		out = append(out, item)
	}
	return out, last, rows.Err()
}

// ResultObservationByObservationID returns one exact stored completion with effect
// metadata. Retained bodies remain JSON-hidden and require an explicit body reader.
func (ix *Index) ResultObservationByObservationID(observationID string) (ResultObservation, error) {
	var r ResultObservation
	err := ix.db.QueryRow(`SELECT id,observation_id,session_id,supplied_session_id,runtime,tool,
		native_call_id,native_call_kind,source_kind,source_ref,source_sequence,source_digest,
		source_segment_id,collector_id,collector_version,state,error_class,started_at,returned_at,
		completed_at,duration_ms,media_type,raw_bytes,raw_field_bytes,decoded_bytes,retained_bytes,
		external_locator_bytes,payload_digest,completeness,stdout_bytes,stdout_digest,stderr_bytes,
		stderr_digest,queued_at,received_at,delivery_attempts,delivery_mode,COALESCE((
			SELECT current.join_class FROM result_reconciliation current WHERE current.result_id=result_observation.id
			ORDER BY current.reconciled_at DESC,current.id DESC LIMIT 1),''),COALESCE((
			SELECT MIN(action.ts) FROM event action JOIN event_delivery delivery ON delivery.event_id=action.id
			WHERE action.origin='live' AND action.session_id=result_observation.session_id
			AND action.runtime=result_observation.runtime),0)
		FROM result_observation WHERE observation_id=?`, observationID).
		Scan(&r.ID, &r.ObservationID, &r.SessionID, &r.SuppliedSessionID,
			&r.Runtime, &r.Tool, &r.NativeCallID, &r.NativeCallKind, &r.SourceKind, &r.SourceRef,
			&r.SourceSequence, &r.SourceDigest, &r.SourceSegmentID, &r.CollectorID,
			&r.CollectorVersion, &r.State, &r.ErrorClass, &r.StartedAt, &r.ReturnedAt,
			&r.CompletedAt, &r.DurationMS, &r.MediaType, &r.RawBytes, &r.RawFieldBytes,
			&r.DecodedBytes, &r.RetainedBytes, &r.ExternalLocatorBytes, &r.PayloadDigest,
			&r.Completeness, &r.StdoutBytes, &r.StdoutDigest, &r.StderrBytes, &r.StderrDigest,
			&r.QueuedAt, &r.ReceivedAt, &r.DeliveryAttempts, &r.DeliveryMode,
			&r.JoinClass, &r.ActionBoundaryAt)
	if err != nil {
		return r, err
	}
	r.ActionCoverageClass = classifyResultActionCoverage(r.SourceKind, r.JoinClass,
		r.CompletedAt, r.ActionBoundaryAt)
	r.Effects, err = ix.ResultEffects(r.ID)
	return r, err
}

// ResultEffects returns typed effect metadata and locally retained bodies. Payload
// fields are deliberately excluded from JSON, so ordinary APIs expose sizes/digests
// and operations without returning code content.
func (ix *Index) ResultEffects(resultID int64) ([]ResultEffect, error) {
	rows, err := ix.db.Query(`SELECT result_id,ordinal,entity_id,raw_identity,operation,move_target,
		evidence_source,source_field,completeness,replace_all,replacement_before_bytes,
		replacement_before_digest,replacement_before_payload,replacement_after_bytes,
		replacement_after_digest,replacement_after_payload,content_bytes,content_digest,
		content_payload,diff_bytes,diff_digest,diff_completeness,diff_payload
		FROM result_effect WHERE result_id=? ORDER BY ordinal`, resultID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ResultEffect{}
	for rows.Next() {
		var effect ResultEffect
		if err := rows.Scan(&effect.ResultID, &effect.Ordinal, &effect.EntityID, &effect.RawIdentity,
			&effect.Operation, &effect.MoveTarget, &effect.EvidenceSource, &effect.SourceField,
			&effect.Completeness, &effect.ReplaceAll, &effect.ReplacementBeforeBytes,
			&effect.ReplacementBeforeDigest, &effect.ReplacementBeforePayload,
			&effect.ReplacementAfterBytes, &effect.ReplacementAfterDigest,
			&effect.ReplacementAfterPayload, &effect.ContentBytes, &effect.ContentDigest,
			&effect.ContentPayload, &effect.DiffBytes, &effect.DiffDigest,
			&effect.DiffCompleteness, &effect.DiffPayload); err != nil {
			return nil, err
		}
		out = append(out, effect)
	}
	return out, rows.Err()
}

func (ix *Index) LiveSessionRuntimes(limit int) ([]LiveSessionRuntime, error) {
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	rows, err := ix.db.Query(`WITH recent AS (
		SELECT session_id,runtime,MAX(ts) max_ts FROM event
		WHERE origin='live' AND session_id!='' AND runtime!=''
		GROUP BY session_id,runtime ORDER BY max_ts DESC LIMIT ?
	) SELECT recent.session_id,recent.runtime,COALESCE((
		SELECT i.source_ref FROM event e JOIN event_input i ON i.event_id=e.id
		WHERE e.session_id=recent.session_id AND e.runtime=recent.runtime AND i.source_ref!=''
		ORDER BY e.ts DESC,e.id DESC LIMIT 1),''),COALESCE((
		SELECT working_directory FROM session_checkpoint c WHERE c.session_id=recent.session_id
		AND c.working_directory!='' ORDER BY c.requested_at DESC,c.id DESC LIMIT 1),'')
		FROM recent ORDER BY recent.max_ts DESC`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LiveSessionRuntime{}
	for rows.Next() {
		var item LiveSessionRuntime
		if err := rows.Scan(&item.SessionID, &item.Runtime, &item.TranscriptPath, &item.WorkingDirectory); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (ix *Index) LiveSessionRuntimesForSession(sessionID string, limit int) ([]LiveSessionRuntime, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := ix.db.Query(`WITH runtimes AS (
		SELECT session_id,runtime,MAX(ts) max_ts FROM event
		WHERE origin='live' AND session_id=? AND runtime!=''
		GROUP BY session_id,runtime ORDER BY max_ts DESC LIMIT ?
	) SELECT runtimes.session_id,runtimes.runtime,COALESCE((
		SELECT i.source_ref FROM event e JOIN event_input i ON i.event_id=e.id
		WHERE e.session_id=runtimes.session_id AND e.runtime=runtimes.runtime AND i.source_ref!=''
		ORDER BY e.ts DESC,e.id DESC LIMIT 1),''),COALESCE((
		SELECT working_directory FROM session_checkpoint c WHERE c.session_id=runtimes.session_id
		AND c.working_directory!='' ORDER BY c.requested_at DESC,c.id DESC LIMIT 1),'')
		FROM runtimes ORDER BY runtimes.max_ts DESC`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LiveSessionRuntime{}
	for rows.Next() {
		var item LiveSessionRuntime
		if err := rows.Scan(&item.SessionID, &item.Runtime, &item.TranscriptPath, &item.WorkingDirectory); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (ix *Index) IdleSessions(cutoff, eligibleAfter int64, limit int) ([]IdleSession, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := ix.db.Query(`WITH idle AS (
		SELECT session_id,runtime,MAX(ts) last_event_at FROM event
		WHERE origin='live' AND session_id!='' AND ts>=? GROUP BY session_id,runtime
		HAVING MAX(ts)<=?
	) SELECT idle.session_id,idle.runtime,idle.last_event_at,COALESCE((
		SELECT working_directory FROM session_checkpoint c WHERE c.session_id=idle.session_id
		AND c.working_directory!='' ORDER BY c.requested_at DESC,c.id DESC LIMIT 1),'')
		FROM idle ORDER BY idle.last_event_at LIMIT ?`, eligibleAfter, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []IdleSession{}
	for rows.Next() {
		var item IdleSession
		if err := rows.Scan(&item.SessionID, &item.Runtime, &item.LastEventAt, &item.WorkingDirectory); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (ix *Index) TranscriptCursor(runtime, sourceRef, segment string) (TranscriptCursor, bool, error) {
	var c TranscriptCursor
	var rescan, continuation int
	err := ix.db.QueryRow(`SELECT runtime,source_ref,source_segment_id,session_id,working_directory,
		file_size,file_mtime,generation_digest,source_sequence,committed_offset,source_line,
		parser_state,parser_state_digest,rescan_needed,continuation_needed,action_parser_version,updated_at FROM transcript_cursor
		WHERE runtime=? AND source_ref=? AND source_segment_id=?`, runtime, sourceRef, segment).
		Scan(&c.Runtime, &c.SourceRef, &c.SourceSegmentID, &c.SessionID, &c.WorkingDirectory,
			&c.FileSize, &c.FileMTime, &c.GenerationDigest, &c.SourceSequence,
			&c.CommittedOffset, &c.SourceLine, &c.ParserState, &c.ParserStateDigest,
			&rescan, &continuation, &c.ActionParserVersion, &c.UpdatedAt)
	if err == sql.ErrNoRows {
		return c, false, nil
	}
	c.RescanNeeded = rescan == 1
	c.ContinuationNeeded = continuation == 1
	return c, err == nil, err
}

type transcriptCursorExecer interface {
	Exec(string, ...any) (sql.Result, error)
}

func upsertTranscriptCursor(exec transcriptCursorExecer, c TranscriptCursor) error {
	_, err := exec.Exec(`INSERT INTO transcript_cursor(runtime,source_ref,source_segment_id,
		session_id,working_directory,file_size,file_mtime,generation_digest,source_sequence,
		committed_offset,source_line,parser_state,parser_state_digest,rescan_needed,continuation_needed,
		action_parser_version,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(runtime,source_ref,source_segment_id) DO UPDATE SET
		session_id=excluded.session_id,working_directory=excluded.working_directory,
		file_size=excluded.file_size,file_mtime=excluded.file_mtime,
		generation_digest=excluded.generation_digest,source_sequence=excluded.source_sequence,
		committed_offset=excluded.committed_offset,source_line=excluded.source_line,
		parser_state=excluded.parser_state,parser_state_digest=excluded.parser_state_digest,
		rescan_needed=excluded.rescan_needed,continuation_needed=excluded.continuation_needed,
		action_parser_version=excluded.action_parser_version,
		updated_at=excluded.updated_at`, c.Runtime,
		c.SourceRef, c.SourceSegmentID, c.SessionID, c.WorkingDirectory, c.FileSize,
		c.FileMTime, c.GenerationDigest, c.SourceSequence, c.CommittedOffset, c.SourceLine,
		nullableBytes(c.ParserState), c.ParserStateDigest, boolInt(c.RescanNeeded),
		boolInt(c.ContinuationNeeded), c.ActionParserVersion, c.UpdatedAt)
	return err
}

func (ix *Index) UpsertTranscriptCursor(c TranscriptCursor) error {
	return upsertTranscriptCursor(ix.db, c)
}

// CommitTranscriptCursor advances transcript progress only in the same transaction
// that durably records parser-loss occurrences discovered while producing it. A crash
// can therefore replay both, but can never hide a loss behind an advanced cursor.
func (ix *Index) CommitTranscriptCursor(c TranscriptCursor, issues []CollectionIssue) error {
	tx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, issue := range issues {
		if err := ensureCollectionIssueTx(tx, issue); err != nil {
			return err
		}
	}
	if err := upsertTranscriptCursor(tx, c); err != nil {
		return err
	}
	return tx.Commit()
}

// TranscriptActionBackfillSources returns a small, indexed candidate window. The
// coordinator performs filesystem checks and admits at most one; this selector never
// resets a cursor or starts work by itself.
func (ix *Index) TranscriptActionBackfillSources(runtime, resultSourceKind, resultNativeCallKind string, targetVersion, limit int) ([]LiveSessionRuntime, error) {
	if limit <= 0 || limit > 32 {
		limit = 8
	}
	if runtime == "" || resultSourceKind == "" || resultNativeCallKind == "" || targetVersion < 1 {
		return nil, errors.New("transcript action backfill contract is incomplete")
	}
	rows, err := ix.db.Query(`SELECT c.session_id,c.runtime,c.source_ref,c.working_directory
		FROM transcript_cursor c
		WHERE c.action_parser_version>=0 AND c.action_parser_version<?
		AND c.runtime=? AND c.session_id!='' AND c.source_ref!=''
		AND EXISTS (SELECT 1 FROM result_observation r
			WHERE r.session_id=c.session_id AND r.runtime=c.runtime AND r.source_ref=c.source_ref
			AND r.source_kind=? AND r.native_call_kind=?)
		ORDER BY c.updated_at DESC,c.source_ref LIMIT ?`, targetVersion, runtime, resultSourceKind,
		resultNativeCallKind, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LiveSessionRuntime{}
	for rows.Next() {
		var item LiveSessionRuntime
		if err := rows.Scan(&item.SessionID, &item.Runtime, &item.TranscriptPath,
			&item.WorkingDirectory); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (ix *Index) TranscriptActionBackfillActiveSource() (LiveSessionRuntime, bool, error) {
	var active LiveSessionRuntime
	err := ix.db.QueryRow(`SELECT session_id,runtime,source_ref,working_directory
		FROM transcript_cursor WHERE action_parser_version=-1 ORDER BY updated_at LIMIT 1`).
		Scan(&active.SessionID, &active.Runtime, &active.TranscriptPath, &active.WorkingDirectory)
	if err == sql.ErrNoRows {
		return active, false, nil
	}
	return active, err == nil, err
}

// BeginTranscriptActionBackfill atomically claims one eligible source and resets only
// its existing lifecycle cursor. -1 distinguishes committed repair progress from an
// untouched v12 cursor so later chunks never reset to byte zero again.
func (ix *Index) BeginTranscriptActionBackfill(runtime, sourceRef, segment string, targetVersion int, at int64) (bool, error) {
	if targetVersion < 1 {
		return false, errors.New("transcript action backfill target version is invalid")
	}
	res, err := ix.db.Exec(`UPDATE transcript_cursor SET committed_offset=0,source_line=0,
		source_sequence='',parser_state=NULL,parser_state_digest='',rescan_needed=1,
		continuation_needed=1,action_parser_version=-1,updated_at=?
		WHERE runtime=? AND source_ref=? AND source_segment_id=?
		AND action_parser_version>=0 AND action_parser_version<?
		AND NOT EXISTS (SELECT 1 FROM transcript_cursor active WHERE active.action_parser_version=-1)`,
		at, runtime, sourceRef, segment, targetVersion)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// BlockTranscriptActionBackfill parks a collision or a source that disappeared during
// repair. It deliberately does not mark the parser complete; explicit recovery is
// required, periodic sweeps cannot spin it, and later sources cannot be starved.
func (ix *Index) BlockTranscriptActionBackfill(runtime, sourceRef, segment string, at int64) error {
	_, err := ix.db.Exec(`UPDATE transcript_cursor SET action_parser_version=-2,
		rescan_needed=0,continuation_needed=0,updated_at=?
		WHERE runtime=? AND source_ref=? AND source_segment_id=? AND action_parser_version=-1`,
		at, runtime, sourceRef, segment)
	return err
}

func (ix *Index) MarkTranscriptRescan(sessionID, runtime, sourceRef, segment, cwd string, at int64) error {
	_, err := ix.db.Exec(`INSERT INTO transcript_cursor(runtime,source_ref,source_segment_id,
		session_id,working_directory,file_size,file_mtime,generation_digest,source_sequence,
		committed_offset,source_line,parser_state,parser_state_digest,rescan_needed,continuation_needed,updated_at)
		VALUES(?,?,?,?,?,0,0,'','',0,0,NULL,'',1,0,?)
		ON CONFLICT(runtime,source_ref,source_segment_id) DO UPDATE SET
		session_id=excluded.session_id,working_directory=excluded.working_directory,
		rescan_needed=1,updated_at=excluded.updated_at`, runtime, sourceRef, segment,
		sessionID, cwd, at)
	return err
}

func (ix *Index) TranscriptRescanSources(limit int) ([]LiveSessionRuntime, error) {
	if limit <= 0 || limit > 256 {
		limit = 64
	}
	rows, err := ix.db.Query(`SELECT session_id,runtime,source_ref,working_directory
		FROM transcript_cursor WHERE rescan_needed=1 AND session_id!='' AND source_ref!=''
		ORDER BY updated_at,runtime,source_ref LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LiveSessionRuntime{}
	for rows.Next() {
		var item LiveSessionRuntime
		if err := rows.Scan(&item.SessionID, &item.Runtime, &item.TranscriptPath,
			&item.WorkingDirectory); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// IsFirstLogicalCompletion separates a newly retained primary source fact from the
// first durable observation of the logical completion. Expensive follow-on work uses
// this boundary; source provenance remains append-only even when false.
func (ix *Index) IsFirstLogicalCompletion(resultID int64) (bool, error) {
	var earlier int
	err := ix.db.QueryRow(`SELECT EXISTS(
		SELECT 1 FROM logical_result_relation
		WHERE (result_id=? AND peer_result_id<?) OR (peer_result_id=? AND result_id<?)
	)`, resultID, resultID, resultID, resultID).Scan(&earlier)
	return earlier == 0, err
}

func (ix *Index) ReconcileResultsForEvent(eventID, at int64) error {
	rows, err := ix.db.Query(`SELECT DISTINCT r.id FROM result_observation r JOIN event e ON e.id=?
		JOIN event_delivery d ON d.event_id=e.id
		LEFT JOIN result_native_call_alias a ON a.result_id=r.id
		WHERE r.runtime=e.runtime AND r.session_id=e.session_id AND (
			(r.native_call_id=d.native_call_id AND r.native_call_kind=d.native_call_kind) OR
			(a.native_call_id=d.native_call_id AND a.native_call_kind=d.native_call_kind)
		)`, eventID)
	if err != nil {
		return err
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := ix.ReconcileResultObservation(id, at); err != nil {
			return err
		}
	}
	return nil
}

// ResultAliasRepairCandidates returns only mechanically provable historical pairs for
// an adapter-supplied contract. The bounded selector lets an upgraded daemon append the
// same fact to old results without rewriting them.
func (ix *Index) ResultAliasRepairCandidates(contract ResultAliasRepairContract, limit int) ([]ResultAliasRepairCandidate, error) {
	if limit <= 0 || limit > 256 {
		limit = 256
	}
	if contract.Runtime == "" || contract.ResultSourceKind == "" || contract.ResultNativeCallKind == "" ||
		contract.AliasNativeCallKind == "" || contract.Tool == "" {
		return nil, errors.New("result alias repair contract is incomplete")
	}
	rows, err := ix.db.Query(`SELECT r.id,r.observation_id,r.native_call_id
		FROM result_observation r
		LEFT JOIN result_native_call_alias a ON a.result_id=r.id
			AND a.native_call_kind=? AND a.native_call_id=r.native_call_id
		WHERE r.runtime=? AND r.source_kind=?
			AND r.native_call_kind=? AND r.native_call_id!='' AND a.result_id IS NULL
			AND (SELECT COUNT(*) FROM event_delivery d JOIN event e ON e.id=d.event_id
				WHERE d.native_call_kind=? AND d.native_call_id=r.native_call_id
				AND e.runtime=r.runtime AND e.session_id=r.session_id AND e.tool=?)=1
		ORDER BY r.id LIMIT ?`, contract.AliasNativeCallKind, contract.Runtime,
		contract.ResultSourceKind, contract.ResultNativeCallKind, contract.AliasNativeCallKind,
		contract.Tool, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ResultAliasRepairCandidate{}
	for rows.Next() {
		var candidate ResultAliasRepairCandidate
		if err := rows.Scan(&candidate.ResultID, &candidate.ObservationID, &candidate.NativeCallID); err != nil {
			return nil, err
		}
		out = append(out, candidate)
	}
	return out, rows.Err()
}

func (ix *Index) ObservationIDForEvent(eventID int64) (string, error) {
	var id string
	err := ix.db.QueryRow(`SELECT observation_id FROM event_delivery WHERE event_id=?`, eventID).Scan(&id)
	return id, err
}

// ExactResultObservationIDsForEvent returns every exact result identity for collection repair.
func (ix *Index) ExactResultObservationIDsForEvent(eventID int64) ([]string, error) {
	rows, _, err := ix.exactResultObservationIDsForEvent(eventID, 0)
	return rows, err
}

// ExactResultObservationIDsForEventPage returns a bounded prefix and exact matching total.
func (ix *Index) ExactResultObservationIDsForEventPage(eventID int64, limit int) ([]string, int, error) {
	if limit <= 0 || limit > maxExactResultLimit {
		limit = defaultExactResultLimit
	}
	return ix.exactResultObservationIDsForEvent(eventID, limit)
}

func (ix *Index) exactResultObservationIDsForEvent(eventID int64, limit int) ([]string, int, error) {
	base := ` FROM result_observation o
		JOIN result_reconciliation r ON r.result_id=o.id
		JOIN result_reconciliation_candidate c ON c.reconciliation_id=r.id AND c.selected=1
		WHERE c.event_id=? AND r.id=(SELECT MAX(r2.id) FROM result_reconciliation r2 WHERE r2.result_id=o.id)
		`
	var total int
	if err := ix.db.QueryRow(`SELECT COUNT(*)`+base, eventID).Scan(&total); err != nil {
		return nil, 0, err
	}
	query := `SELECT o.observation_id` + base + ` ORDER BY o.id`
	args := []any{eventID}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := ix.db.Query(query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, 0, err
		}
		out = append(out, id)
	}
	return out, total, rows.Err()
}

func (ix *Index) MissingResultsForSession(sessionID string) ([]MissingResultCandidate, error) {
	rows, err := ix.db.Query(`SELECT e.id,d.observation_id,e.runtime,d.native_call_id,d.native_call_kind
		FROM event e JOIN event_delivery d ON d.event_id=e.id
		WHERE e.session_id=? AND (e.decision='allow' OR e.origin='transcript') AND NOT EXISTS (
			SELECT 1 FROM result_observation r JOIN result_reconciliation rr ON rr.result_id=r.id
			JOIN result_reconciliation_candidate c ON c.reconciliation_id=rr.id AND c.selected=1
			WHERE c.event_id=e.id AND rr.id=(SELECT MAX(rr2.id) FROM result_reconciliation rr2 WHERE rr2.result_id=r.id)
		) ORDER BY e.id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MissingResultCandidate{}
	for rows.Next() {
		var item MissingResultCandidate
		if err := rows.Scan(&item.EventID, &item.ObservationID, &item.Runtime,
			&item.NativeCallID, &item.NativeCallKind); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (ix *Index) RelateLogicalResults(resultID, at int64) error {
	_, err := ix.db.Exec(`INSERT INTO logical_result_relation(result_id,peer_result_id,relation,algorithm,related_at)
		SELECT MIN(a.id,b.id),MAX(a.id,b.id),'same-logical-result','native-id-alias-v1',?
		FROM result_observation a JOIN result_observation b
		ON a.runtime=b.runtime AND a.session_id=b.session_id
		AND (
			(a.native_call_kind=b.native_call_kind AND a.native_call_id=b.native_call_id) OR
			EXISTS (SELECT 1 FROM result_native_call_alias x WHERE x.result_id=a.id
				AND x.native_call_kind=b.native_call_kind AND x.native_call_id=b.native_call_id) OR
			EXISTS (SELECT 1 FROM result_native_call_alias y WHERE y.result_id=b.id
				AND y.native_call_kind=a.native_call_kind AND y.native_call_id=a.native_call_id) OR
			EXISTS (SELECT 1 FROM result_native_call_alias x JOIN result_native_call_alias y
				ON x.native_call_kind=y.native_call_kind AND x.native_call_id=y.native_call_id
				WHERE x.result_id=a.id AND y.result_id=b.id)
		)
		WHERE a.id=? AND b.id!=a.id AND a.native_call_id!=''
		ON CONFLICT(result_id,peer_result_id) DO NOTHING`, at, resultID)
	return err
}

func (r ResultObservation) Validate() error {
	if r.ObservationID == "" || r.SessionID == "" || r.Runtime == "" || r.SourceKind == "" || r.SourceDigest == "" {
		return fmt.Errorf("result observation requires identity, session, runtime, source kind, and digest")
	}
	return nil
}
