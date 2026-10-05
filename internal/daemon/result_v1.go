package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"crossing-guard/internal/collectionconfig"
	"crossing-guard/internal/observation"
	"crossing-guard/store"
)

func validateResultEnvelope(e observation.ResultEnvelope) error {
	if e.Schema != observation.ResultSchemaV1 {
		return invalidObservation("result_schema must be %s", observation.ResultSchemaV1)
	}
	if len(e.ObservationID) != 36 || !strings.HasPrefix(e.ObservationID, "res_") {
		return invalidObservation("result_observation_id must be res_ plus 32 lowercase hex characters")
	}
	for _, r := range e.ObservationID[4:] {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return invalidObservation("result_observation_id must be res_ plus 32 lowercase hex characters")
		}
	}
	if e.SessionID == "" || len(e.SessionID) > 512 || e.Runtime == "" || len(e.Runtime) > 64 || !validRuntimeName(e.Runtime) {
		return invalidObservation("result session/runtime identity is invalid")
	}
	if (e.HookEventName != "PostToolUse" && e.HookEventName != "PostToolUseFailure") || e.CollectorID == "" || len(e.CollectorID) > 128 {
		return invalidObservation("result hook/collector identity is invalid")
	}
	switch e.State {
	case "success", "failure", "cancelled", "timeout", "unknown":
	default:
		return invalidObservation("result state is invalid")
	}
	if e.DeliveryAttempts < 1 || (e.DeliveryMode != "direct" && e.DeliveryMode != "replay") {
		return invalidObservation("result delivery metadata is invalid")
	}
	if e.DurationMS < 0 || e.RawEnvelopeBytes < 0 || e.RawFieldBytes < 0 || e.DecodedBytes < 0 || e.RetainedBytes < 0 || e.ExternalLocatorBytes < 0 || e.StdoutBytes < 0 || e.StderrBytes < 0 {
		return invalidObservation("result byte metadata is invalid")
	}
	if (e.StdoutBytes > 0 && e.StdoutDigest == "") || (e.StderrBytes > 0 && e.StderrDigest == "") {
		return invalidObservation("result stream metadata is inconsistent")
	}
	switch e.Completeness {
	case "complete":
		if e.Payload == nil || len(e.Payload) != e.RetainedBytes || e.PayloadDigest == "" {
			return invalidObservation("complete result payload is inconsistent")
		}
		if e.PayloadDigest != observation.DigestBytes(e.Payload) {
			return invalidObservation("complete result payload digest is inconsistent")
		}
	case "metadata-only", "redacted":
		if e.Payload != nil || e.RetainedBytes != 0 || e.PayloadDigest == "" {
			return invalidObservation("metadata-only result must omit payload and retain digest")
		}
	case "unavailable":
		if e.Payload != nil || e.RetainedBytes != 0 {
			return invalidObservation("unavailable result must omit payload")
		}
	default:
		return invalidObservation("result completeness is invalid")
	}
	retained := 0
	for ordinal, effect := range e.Effects {
		if effect.Ordinal != ordinal || len(effect.RawIdentity) > 16<<10 || effect.RawIdentity == "" {
			return invalidObservation("result effect %d identity/order is invalid", ordinal)
		}
		switch effect.Operation {
		case "create", "update", "delete", "move", "write", "unknown":
		default:
			return invalidObservation("result effect %d operation is invalid", ordinal)
		}
		if (len(effect.BeforePayload) > 0 && len(effect.BeforePayload) != effect.BeforeBytes) ||
			(len(effect.AfterPayload) > 0 && len(effect.AfterPayload) != effect.AfterBytes) ||
			(len(effect.ContentPayload) > 0 && len(effect.ContentPayload) != effect.ContentBytes) ||
			(len(effect.DiffPayload) > 0 && len(effect.DiffPayload) != effect.DiffBytes) {
			return invalidObservation("result effect %d payload/byte metadata is inconsistent", ordinal)
		}
		for _, body := range []struct {
			payload []byte
			digest  string
		}{
			{effect.BeforePayload, effect.BeforeDigest},
			{effect.AfterPayload, effect.AfterDigest},
			{effect.ContentPayload, effect.ContentDigest},
			{effect.DiffPayload, effect.DiffDigest},
		} {
			if len(body.payload) > 0 && body.digest != observation.DigestBytes(body.payload) {
				return invalidObservation("result effect %d retained payload digest is inconsistent", ordinal)
			}
		}
		retained += len(effect.BeforePayload) + len(effect.AfterPayload) + len(effect.ContentPayload) + len(effect.DiffPayload)
	}
	if retained+len(e.Payload) > observation.MaxRetainedInput {
		return invalidObservation("result payload exceeds retained-result bound")
	}
	return nil
}

func enforceResultRetention(e *observation.ResultEnvelope, mode collectionconfig.Mode) {
	for i := range e.Effects {
		effect := &e.Effects[i]
		if effect.Completeness == "complete" || effect.Completeness == "partial" {
			if (effect.BeforeBytes > 0 && len(effect.BeforePayload) == 0) ||
				(effect.AfterBytes > 0 && len(effect.AfterPayload) == 0) ||
				(effect.ContentBytes > 0 && len(effect.ContentPayload) == 0) {
				effect.Completeness = "metadata-only"
			}
		}
		if effect.DiffCompleteness == "complete" && effect.DiffBytes > 0 && len(effect.DiffPayload) == 0 {
			effect.DiffCompleteness = "metadata-only"
		}
	}
	if mode != collectionconfig.CompleteBounded {
		e.Payload = nil
		e.RetainedBytes = 0
		if e.Completeness == "complete" {
			e.Completeness = "metadata-only"
		}
	}
	if mode != collectionconfig.MetadataOnly {
		return
	}
	for i := range e.Effects {
		effect := &e.Effects[i]
		effect.BeforePayload = nil
		effect.AfterPayload = nil
		effect.ContentPayload = nil
		effect.DiffPayload = nil
		if effect.Completeness == "complete" || effect.Completeness == "partial" {
			effect.Completeness = "metadata-only"
		}
		if effect.DiffCompleteness == "complete" || effect.DiffCompleteness == "partial" {
			effect.DiffCompleteness = "metadata-only"
		}
	}
}

func ingestResultV1(g *Governor, e observation.ResultEnvelope) (observation.ResultReceipt, error) {
	if err := validateResultEnvelope(e); err != nil {
		return observation.ResultReceipt{}, err
	}
	digest, err := e.Digest()
	if err != nil {
		return observation.ResultReceipt{}, err
	}
	bounded := false
	if g.resultPayloadMode == collectionconfig.CompleteBounded && e.DecodedBytes > 0 && e.Completeness != "complete" {
		bounded = true
	}
	if g.resultPayloadMode != collectionconfig.MetadataOnly {
		for _, effect := range e.Effects {
			if (effect.BeforeBytes > 0 && len(effect.BeforePayload) == 0) ||
				(effect.AfterBytes > 0 && len(effect.AfterPayload) == 0) ||
				(effect.ContentBytes > 0 && len(effect.ContentPayload) == 0) ||
				(effect.DiffBytes > 0 && len(effect.DiffPayload) == 0 && effect.DiffCompleteness == "metadata-only") {
				bounded = true
			}
		}
	}
	enforceResultRetention(&e, g.resultPayloadMode)
	effects := make([]store.ResultEffect, 0, len(e.Effects))
	for _, effect := range e.Effects {
		effects = append(effects, store.ResultEffect{Ordinal: effect.Ordinal, RawIdentity: effect.RawIdentity,
			Operation: effect.Operation, MoveTarget: effect.MoveTarget, EvidenceSource: effect.EvidenceSource,
			SourceField: effect.SourceField, Completeness: effect.Completeness, ReplaceAll: effect.ReplaceAll,
			ReplacementBeforeBytes: effect.BeforeBytes, ReplacementBeforeDigest: effect.BeforeDigest,
			ReplacementBeforePayload: effect.BeforePayload, ReplacementAfterBytes: effect.AfterBytes,
			ReplacementAfterDigest: effect.AfterDigest, ReplacementAfterPayload: effect.AfterPayload,
			ContentBytes: effect.ContentBytes, ContentDigest: effect.ContentDigest, ContentPayload: effect.ContentPayload,
			DiffBytes: effect.DiffBytes, DiffDigest: effect.DiffDigest, DiffCompleteness: effect.DiffCompleteness,
			DiffPayload: effect.DiffPayload})
	}
	g.writeMu.Lock()
	id, duplicate, err := g.ix.AppendResultObservation(store.ResultObservation{
		ObservationID: e.ObservationID, SessionID: e.SessionID, SuppliedSessionID: e.SuppliedSessionID,
		Runtime: e.Runtime, Tool: e.Tool, NativeCallID: e.NativeCallID, NativeCallKind: e.NativeCallKind,
		SourceKind: "live-post-tool", SourceRef: e.TranscriptPath, SourceSequence: e.ObservationID,
		SourceDigest: digest, SourceSegmentID: e.SourceSegmentID, CollectorID: e.CollectorID,
		CollectorVersion: e.CollectorVersion, State: e.State, ErrorClass: e.ErrorClass,
		CompletedAt: e.CompletedAt, DurationMS: e.DurationMS, MediaType: e.MediaType,
		RawBytes: e.RawEnvelopeBytes, RawFieldBytes: e.RawFieldBytes, DecodedBytes: e.DecodedBytes,
		RetainedBytes: e.RetainedBytes, ExternalLocatorBytes: e.ExternalLocatorBytes,
		PayloadDigest: e.PayloadDigest, Completeness: e.Completeness, Payload: e.Payload,
		StdoutBytes: e.StdoutBytes, StdoutDigest: e.StdoutDigest, StderrBytes: e.StderrBytes,
		StderrDigest: e.StderrDigest, QueuedAt: e.QueuedAt, ReceivedAt: time.Now().Unix(),
		DeliveryAttempts: e.DeliveryAttempts, DeliveryMode: e.DeliveryMode}, effects)
	if err == nil && duplicate {
		g.writeMu.Unlock()
		return resultReceiptWithLifecycleHint(g, e, observation.ResultReceipt{
			Schema: observation.ResultSchemaV1, ObservationID: e.ObservationID,
			ResultID: id, Duplicate: true})
	}
	if err == nil {
		err = g.ix.RelateLogicalResults(id, time.Now().Unix())
	}
	firstLogical := false
	if err == nil {
		firstLogical, err = g.ix.IsFirstLogicalCompletion(id)
	}
	if err == nil {
		var reconciliation store.ResultReconciliation
		reconciliation, err = g.ix.ReconcileResultObservation(id, time.Now().Unix())
		if err == nil {
			now := time.Now().Unix()
			err = syncResultActionIssue(g, id, e.ObservationID, e.SessionID, e.Runtime,
				"daemon-result-v1", reconciliation, now)
			if err == nil && bounded {
				err = g.ix.RecordCollectionIssue(store.CollectionIssue{IssueID: observationIssueID("result-payload-bounded", e.ObservationID),
					SessionID: e.SessionID, Runtime: e.Runtime, ObservationID: e.ObservationID,
					CollectorID: "daemon-result-v1", Kind: "result-payload-bounded", AffectedCount: 1,
					FirstSeen: now, LastSeen: now})
			}
		}
		if err == nil {
			g.writeMu.Unlock()
			if reconciliation.JoinClass == "exact" {
				for _, candidate := range reconciliation.Candidates {
					if !candidate.Selected {
						continue
					}
					eventObservationID, lookupErr := g.ix.ObservationIDForEvent(candidate.EventID)
					if lookupErr != nil {
						return observation.ResultReceipt{}, fmt.Errorf("find reconciled action observation: %w", lookupErr)
					}
					if resolveErr := g.ix.ResolveCollectionIssue(observationIssueID("result-missing",
						eventObservationID), time.Now().Unix()); resolveErr != nil {
						return observation.ResultReceipt{}, fmt.Errorf("resolve recovered result gap: %w", resolveErr)
					}
				}
			}
			if firstLogical {
				if scheduleErr := scheduleSettledCheckpoint(g, e, id); scheduleErr != nil {
					return observation.ResultReceipt{}, scheduleErr
				}
			}
			return resultReceiptWithLifecycleHint(g, e, observation.ResultReceipt{
				Schema: observation.ResultSchemaV1, ObservationID: e.ObservationID,
				ResultID: id, JoinClass: reconciliation.JoinClass})
		}
	}
	g.writeMu.Unlock()
	return observation.ResultReceipt{}, err
}

func resultReceiptWithLifecycleHint(g *Governor, e observation.ResultEnvelope,
	receipt observation.ResultReceipt) (observation.ResultReceipt, error) {
	if err := requestLifecycleHint(g, store.LiveSessionRuntime{SessionID: e.SessionID,
		Runtime: e.Runtime, TranscriptPath: e.TranscriptPath,
		WorkingDirectory: e.Cwd}); err != nil {
		return observation.ResultReceipt{}, fmt.Errorf("queue result lifecycle reconciliation: %w", err)
	}
	if !receipt.Duplicate {
		requestNaturalSignalEmit()
	}
	// Pending helper messages are claimed by the HTTP reply, not here
	// (delivery-claim-on-reply plan D1).
	return receipt, nil
}

func syncResultActionIssue(g *Governor, resultID int64, observationID, sessionID, runtime,
	collectorID string, reconciliation store.ResultReconciliation, at int64) error {
	issueID := observationIssueID("result-unjoined", observationID)
	if reconciliation.JoinClass != "unjoined" && reconciliation.JoinClass != "ambiguous" {
		return g.ix.ResolveCollectionIssue(issueID, at)
	}
	coverage, err := g.ix.ResultActionCoverage(resultID)
	if err != nil {
		return err
	}
	if coverage.Class != "expected-action-missing" && coverage.Class != "action-ambiguous" {
		return nil
	}
	return g.ix.RecordCollectionIssue(store.CollectionIssue{IssueID: issueID,
		SessionID: sessionID, Runtime: runtime, ObservationID: observationID,
		CollectorID: collectorID, Kind: "result-unjoined", AffectedCount: 1,
		FirstSeen: at, LastSeen: at})
}

func recordResultIssue(g *Governor, e observation.ResultEnvelope, kind string, cause error) {
	now := time.Now().Unix()
	detail := ""
	if cause != nil {
		detail = observation.DigestBytes([]byte(cause.Error()))
	}
	g.writeMu.Lock()
	defer g.writeMu.Unlock()
	_ = g.ix.RecordCollectionIssue(store.CollectionIssue{IssueID: observationIssueID(kind, e.ObservationID),
		SessionID: e.SessionID, Runtime: e.Runtime, ObservationID: e.ObservationID,
		CollectorID: "daemon-result-v1", Kind: kind, AffectedCount: 1,
		FirstSeen: now, LastSeen: now, DetailDigest: detail})
}

func handleGovernResultV1(w http.ResponseWriter, r *http.Request) {
	if governor == nil {
		http.Error(w, governorNotConfigured, http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, observation.MaxEnvelopeBytes)
	var envelope observation.ResultEnvelope
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&envelope); err != nil {
		http.Error(w, "invalid result observation: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		http.Error(w, "invalid result observation: trailing JSON data", http.StatusBadRequest)
		return
	}
	receipt, err := ingestResultV1(governor, envelope)
	if err != nil {
		if errors.Is(err, store.ErrResultObservationCollision) {
			recordResultIssue(governor, envelope, "result-collision", err)
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		var validation *observationValidationError
		if errors.As(err, &validation) {
			recordResultIssue(governor, envelope, "malformed", err)
			http.Error(w, validation.Error(), http.StatusBadRequest)
			return
		}
		governor.observeFailures.Add(1)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	replyWithDeliveries(w, r, governor, carrierBoundary{Runtime: envelope.Runtime, SessionID: envelope.SessionID, Kind: "tool.completed",
		ObservationID: envelope.ObservationID, NativeCallID: envelope.NativeCallID, DeliveryMode: envelope.DeliveryMode,
		Duplicate: receipt.Duplicate, Carrier: envelope.Carrier},
		receipt, func(receipt *observation.ResultReceipt, deliveries []observation.Delivery) {
			receipt.Deliveries = deliveries
		})
}

func handleGovernResults(w http.ResponseWriter, r *http.Request) {
	if governor == nil {
		writeGovernorUnavailable(w)
		return
	}
	session := r.URL.Query().Get("session")
	if session == "" {
		http.Error(w, "session is required", http.StatusBadRequest)
		return
	}
	results, err := governor.ix.ResultsForSession(session, 1000)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	stats, err := governor.ix.ResultStatsForSession(session)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"session": session, "results": results, "stats": stats})
}
