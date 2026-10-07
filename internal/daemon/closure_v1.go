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

func validateClosureEnvelope(e observation.ClosureEnvelope) error {
	if e.Schema != observation.ClosureSchemaV1 || e.HookEventName != "SessionEnd" {
		return invalidObservation("closure schema/hook event is invalid")
	}
	if len(e.ObservationID) != 36 || !strings.HasPrefix(e.ObservationID, "cls_") {
		return invalidObservation("closure_observation_id must be cls_ plus 32 lowercase hex characters")
	}
	if e.SessionID == "" || e.Runtime == "" || !validRuntimeName(e.Runtime) || len(e.Cwd) > 16<<10 {
		return invalidObservation("closure session/runtime/cwd is invalid")
	}
	if e.DeliveryAttempts < 1 || (e.DeliveryMode != "direct" && e.DeliveryMode != "replay") {
		return invalidObservation("closure delivery metadata is invalid")
	}
	return nil
}

func ingestClosureV1(g *Governor, e observation.ClosureEnvelope) (observation.ClosureReceipt, error) {
	if err := validateClosureEnvelope(e); err != nil {
		return observation.ClosureReceipt{}, err
	}
	digest, err := e.Digest()
	if err != nil {
		return observation.ClosureReceipt{}, err
	}
	cwd := filepath.Clean(e.Cwd)
	scope := "cwd-unavailable:" + e.ObservationID
	checkpoint := store.SessionCheckpoint{Runtime: e.Runtime, SessionID: e.SessionID, ScopeKey: scope, Kind: "closing",
		RequestID: e.ObservationID, TriggerObservationID: e.ObservationID, WorkingDirectory: e.Cwd,
		Status: "pending", BoundaryClass: "exact-close", RequestedAt: e.ObservedAt}
	if e.Cwd != "" && filepath.IsAbs(cwd) {
		checkpoint.ScopeKey = "cwd:" + cwd
		if repo, err := changeenv.ResolveRepository(cwd); err == nil {
			checkpoint.ScopeKey = "checkout:" + repo.CheckoutID
			checkpoint.RepositoryID, checkpoint.CheckoutID, checkpoint.CheckoutRoot = repo.ID, repo.CheckoutID, repo.Root
		}
	}
	g.writeMu.Lock()
	tx, err := g.ix.BeginGov()
	if err == nil {
		err = tx.EnsureSessionRoot(e.Runtime, e.SessionID, e.TranscriptPath, e.Cwd)
	}
	if err == nil {
		_, err = tx.AppendSessionActivity(store.SessionActivityObservation{
			ObservationID: e.ObservationID, Runtime: e.Runtime, SessionID: e.SessionID,
			State: "stopped", ObservedAt: e.ObservedAt, EvidenceClass: "explicit-end",
			EntryKind: "end", SourceRef: e.TranscriptPath, EvidenceDigest: digest,
			CollectorID: e.CollectorID, CollectorVersion: e.CollectorVersion,
			ReceivedAt: time.Now().Unix(), DeliveryAttempts: e.DeliveryAttempts,
			DeliveryMode: e.DeliveryMode,
		})
	}
	if err == nil {
		checkpoint, _, err = tx.EnsureSessionCheckpoint(checkpoint)
	}
	if err == nil {
		err = tx.Commit()
	} else if tx != nil {
		_ = tx.Rollback()
	}
	g.writeMu.Unlock()
	if err != nil {
		return observation.ClosureReceipt{}, err
	}
	sessionStatusRefold(e.Runtime, e.SessionID)
	// A session's end is the honest moment its vendor's auto-memory is
	// settled: the daemon imports it (daemon-memory-import plan D1b), off
	// this path, throttled by memory.json.
	requestMemoryImport("session-end")
	// The same lifecycle moment, an independent step with no data dependency
	// on the import (synthesis v1 plan §2 / DS-RT2): if the owner enabled it,
	// draft at most one pending lesson candidate from this session's events.
	requestMemorySynthesis(e.Runtime, e.SessionID)
	if checkpoint.Status != "complete" {
		go func(c store.SessionCheckpoint) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = captureSessionCheckpoint(ctx, g, e.SessionID, e.Runtime, e.ObservationID, c, "exact-close")
		}(checkpoint)
	}
	go func() {
		if err := finalizeMissingResults(g, e.SessionID); err != nil &&
			g.lifecycle != nil {
			g.lifecycle.stats.failed.Add(1)
		}
	}()
	if err := requestLifecycleHint(g, store.LiveSessionRuntime{SessionID: e.SessionID, Runtime: e.Runtime,
		TranscriptPath: e.TranscriptPath, WorkingDirectory: e.Cwd}); err != nil {
		return observation.ClosureReceipt{}, fmt.Errorf("queue closure lifecycle reconciliation: %w", err)
	}
	return observation.ClosureReceipt{Schema: observation.ClosureSchemaV1, ObservationID: e.ObservationID,
		Checkpoint: observation.CheckpointReceipt{ID: checkpoint.ID, Status: checkpoint.Status,
			BoundaryClass: checkpoint.BoundaryClass, ChangeRecordID: checkpoint.ChangeRecordID,
			FailureKind: checkpoint.FailureKind}}, nil
}

func handleGovernClosureV1(w http.ResponseWriter, r *http.Request) {
	if governor == nil {
		http.Error(w, governorNotConfigured, http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, observation.MaxEnvelopeBytes)
	var envelope observation.ClosureEnvelope
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&envelope); err != nil {
		http.Error(w, "invalid closure observation: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		http.Error(w, "invalid closure observation: trailing JSON data", http.StatusBadRequest)
		return
	}
	receipt, err := ingestClosureV1(governor, envelope)
	if err != nil {
		var validation *observationValidationError
		if errors.As(err, &validation) {
			http.Error(w, validation.Error(), http.StatusBadRequest)
			return
		}
		if errors.Is(err, store.ErrSessionActivityCollision) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, receipt)
}
