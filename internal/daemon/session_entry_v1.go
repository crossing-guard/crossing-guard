package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"crossing-guard/internal/changeenv"
	"crossing-guard/internal/observation"
	"crossing-guard/store"
)

const sessionEntryObservationValidity = int64(1)

var errSessionEntryCheckpointPending = errors.New("session entry checkpoint is not terminal")

func validSessionEntryKind(kind string) bool {
	switch kind {
	case "start", "resume", "context-reset", "context-compact", "unknown":
		return true
	default:
		return false
	}
}

func validateSessionEntryEnvelope(entry observation.SessionEntryEnvelope) error {
	if entry.Schema != observation.SessionEntrySchemaV1 || entry.HookEventName != "SessionStart" {
		return invalidObservation("session entry schema/hook event is invalid")
	}
	if len(entry.ObservationID) != 36 || !strings.HasPrefix(entry.ObservationID, "ent_") {
		return invalidObservation("session_entry_observation_id must be ent_ plus 32 lowercase hex characters")
	}
	for _, character := range entry.ObservationID[4:] {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return invalidObservation("session_entry_observation_id must be ent_ plus 32 lowercase hex characters")
		}
	}
	if entry.CollectorID == "" || len(entry.CollectorID) > 128 || len(entry.CollectorVersion) > 512 {
		return invalidObservation("session entry collector identity is invalid")
	}
	if entry.Runtime == "" || !validRuntimeName(entry.Runtime) || entry.SessionID == "" || len(entry.SessionID) > 512 {
		return invalidObservation("session entry runtime/session identity is invalid")
	}
	if !validSessionEntryKind(entry.EntryKind) || len(entry.NativeSource) > 128 ||
		len(entry.Cwd) > 16<<10 || len(entry.TranscriptPath) > 16<<10 {
		return invalidObservation("session entry source/cwd metadata is invalid")
	}
	if entry.ObservedAt <= 0 || entry.DeliveryAttempts < 1 ||
		(entry.DeliveryMode != "direct" && entry.DeliveryMode != "replay") {
		return invalidObservation("session entry delivery metadata is invalid")
	}
	return nil
}

func sessionEntryCheckpoint(entry observation.SessionEntryEnvelope) store.SessionCheckpoint {
	cwd := filepath.Clean(entry.Cwd)
	scope := "cwd-unavailable:" + entry.ObservationID
	checkpoint := store.SessionCheckpoint{
		Runtime: entry.Runtime, SessionID: entry.SessionID, ScopeKey: scope, Kind: "attachment",
		RequestID: entry.ObservationID, TriggerObservationID: entry.ObservationID,
		WorkingDirectory: entry.Cwd, Status: "pending", BoundaryClass: "unconfirmed",
		RequestedAt: entry.ObservedAt,
	}
	if entry.Cwd != "" && filepath.IsAbs(cwd) {
		checkpoint.ScopeKey = "cwd:" + cwd
		if repository, err := changeenv.ResolveRepository(cwd); err == nil {
			checkpoint.ScopeKey = "checkout:" + repository.CheckoutID
			checkpoint.RepositoryID = repository.ID
			checkpoint.CheckoutID = repository.CheckoutID
			checkpoint.CheckoutRoot = repository.Root
		}
	}
	return checkpoint
}

func ingestSessionEntryV1(ctx context.Context, g *Governor, entry observation.SessionEntryEnvelope) (observation.SessionEntryReceipt, error) {
	if err := validateSessionEntryEnvelope(entry); err != nil {
		return observation.SessionEntryReceipt{}, err
	}
	digest, err := entry.Digest()
	if err != nil {
		return observation.SessionEntryReceipt{}, err
	}
	receivedAt := time.Now().Unix()
	evidenceClass := "positive-open"
	if entry.DeliveryMode == "direct" && entry.EntryKind == "start" {
		evidenceClass = "explicit-start"
	}
	activity := store.SessionActivityObservation{
		ObservationID: entry.ObservationID, Runtime: entry.Runtime, SessionID: entry.SessionID,
		State: "open", ObservedAt: entry.ObservedAt,
		ValidUntil:    entry.ObservedAt + sessionEntryObservationValidity,
		EvidenceClass: evidenceClass, EntryKind: entry.EntryKind,
		NativeSource: entry.NativeSource, SourceRef: entry.TranscriptPath,
		EvidenceDigest: digest, CollectorID: entry.CollectorID,
		CollectorVersion: entry.CollectorVersion, ReceivedAt: receivedAt,
		DeliveryAttempts: entry.DeliveryAttempts, DeliveryMode: entry.DeliveryMode,
	}
	checkpointRequest := sessionEntryCheckpoint(entry)
	g.writeMu.Lock()
	transaction, err := g.ix.BeginGov()
	created := false
	checkpoint := store.SessionCheckpoint{}
	if err == nil {
		err = transaction.EnsureSessionRoot(entry.Runtime, entry.SessionID, entry.TranscriptPath, entry.Cwd)
	}
	if err == nil {
		created, err = transaction.AppendSessionActivity(activity)
	}
	if err == nil {
		checkpoint, _, err = transaction.EnsureSessionCheckpoint(checkpointRequest)
	}
	if err == nil {
		err = transaction.Commit()
	} else if transaction != nil {
		_ = transaction.Rollback()
	}
	g.writeMu.Unlock()
	if err != nil {
		return observation.SessionEntryReceipt{}, err
	}
	if created {
		sessionStatusRefold(entry.Runtime, entry.SessionID)
	}
	if !created {
		if stored, found, loadErr := g.ix.SessionActivityByID(entry.ObservationID); loadErr != nil {
			return observation.SessionEntryReceipt{}, loadErr
		} else if found {
			evidenceClass = stored.EvidenceClass
		}
	}
	if checkpoint.Status != "complete" {
		boundary := "unconfirmed"
		if entry.DeliveryMode == "replay" {
			boundary = "late-replay"
		} else if created && entry.EntryKind == "start" && checkpoint.CaptureAttempts == 0 {
			boundary = "direct-pre-release"
		}
		checkpoint = captureSessionCheckpoint(ctx, g, entry.SessionID, entry.Runtime,
			entry.ObservationID, checkpoint, boundary)
	}
	if checkpoint.Status == "pending" || checkpoint.Status == "capturing" {
		return observation.SessionEntryReceipt{}, errSessionEntryCheckpointPending
	}
	if err := requestLifecycleHint(g, store.LiveSessionRuntime{SessionID: entry.SessionID,
		Runtime: entry.Runtime, TranscriptPath: entry.TranscriptPath,
		WorkingDirectory: entry.Cwd}); err != nil {
		return observation.SessionEntryReceipt{}, fmt.Errorf("queue session entry lifecycle reconciliation: %w", err)
	}
	requestNaturalSignalEmit()
	return observation.SessionEntryReceipt{
		Schema: observation.SessionEntrySchemaV1, ObservationID: entry.ObservationID,
		Duplicate: !created, EvidenceClass: evidenceClass,
		Checkpoint: observation.CheckpointReceipt{
			ID: checkpoint.ID, Status: checkpoint.Status, BoundaryClass: checkpoint.BoundaryClass,
			ChangeRecordID: checkpoint.ChangeRecordID, FailureKind: checkpoint.FailureKind,
		},
	}, nil
}

func handleGovernSessionEntryV1(w http.ResponseWriter, r *http.Request) {
	if governor == nil {
		http.Error(w, "governor not configured", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, observation.MaxEnvelopeBytes)
	var entry observation.SessionEntryEnvelope
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&entry); err != nil {
		http.Error(w, "invalid session entry observation: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		http.Error(w, "invalid session entry observation: trailing JSON data", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), observation.DirectCaptureBudget)
	defer cancel()
	receipt, err := ingestSessionEntryV1(ctx, governor, entry)
	if err != nil {
		var validation *observationValidationError
		switch {
		case errors.As(err, &validation):
			http.Error(w, validation.Error(), http.StatusBadRequest)
		case errors.Is(err, store.ErrSessionActivityCollision):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, errSessionEntryCheckpointPending):
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	writeJSON(w, receipt)
}
