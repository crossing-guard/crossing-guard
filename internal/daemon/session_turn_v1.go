package daemon

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"crossing-guard/harvest"
	"crossing-guard/internal/observation"
	"crossing-guard/store"
)

// sessionStatusRefold is the one seam between collection and the status
// decider: every receiver that lands a fact about a session calls it. The
// fold owner installs the real function; until then it is a no-op so this
// receiver is testable alone. It is read from task goroutines and swapped by
// tests, so the hook lives behind an atomic rather than a bare variable.
var sessionStatusRefoldHook atomic.Pointer[func(runtime, sessionID string)]

func sessionStatusRefold(runtime, sessionID string) {
	if fn := sessionStatusRefoldHook.Load(); fn != nil {
		(*fn)(runtime, sessionID)
	}
}

// swapSessionStatusRefold installs fn and returns the restore. Production
// installs the fold once at init; tests use the restore.
func swapSessionStatusRefold(fn func(runtime, sessionID string)) func() {
	previous := sessionStatusRefoldHook.Swap(&fn)
	return func() { sessionStatusRefoldHook.Store(previous) }
}

func validateSessionTurnEnvelope(turn observation.SessionTurnEnvelope) error {
	if turn.Schema != observation.SessionTurnSchemaV1 {
		return invalidObservation("session turn schema is invalid")
	}
	if len(turn.ObservationID) != 36 || !strings.HasPrefix(turn.ObservationID, "trn_") {
		return invalidObservation("session_turn_observation_id must be trn_ plus 32 lowercase hex characters")
	}
	for _, character := range turn.ObservationID[4:] {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return invalidObservation("session_turn_observation_id must be trn_ plus 32 lowercase hex characters")
		}
	}
	if turn.CollectorID == "" || len(turn.CollectorID) > 128 || len(turn.CollectorVersion) > 512 {
		return invalidObservation("session turn collector identity is invalid")
	}
	if turn.Runtime == "" || !validRuntimeName(turn.Runtime) || turn.SessionID == "" || len(turn.SessionID) > 512 {
		return invalidObservation("session turn runtime/session identity is invalid")
	}
	if !observation.SessionTurnKinds[turn.Kind] {
		return invalidObservation("session turn kind is not in the framework vocabulary")
	}
	if len(turn.NativeSource) > observation.MaxNativeSourceBytes || len(turn.Cwd) > 16<<10 || len(turn.TranscriptPath) > 16<<10 {
		return invalidObservation("session turn source/cwd metadata is invalid")
	}
	if turn.ObservedAt <= 0 || turn.DeliveryAttempts < 1 ||
		(turn.DeliveryMode != "direct" && turn.DeliveryMode != "replay") {
		return invalidObservation("session turn delivery metadata is invalid")
	}
	return nil
}

// resolveCatalogSessionID maps the hook's native session identity onto the
// catalog row the rail shows, through the ONE existing owner of that mapping
// (harvest.FindAll — exact match or a Codex thread's newest rollout). Empty
// when no catalog row exists yet; the decider then keys by native identity.
func resolveCatalogSessionID(runtime, nativeID string) string {
	matches := harvest.FindAll(runtime, nativeID)
	if len(matches) == 0 {
		return ""
	}
	primary := 0
	for i := range matches {
		if matches[i].Modified.After(matches[primary].Modified) {
			primary = i
		}
	}
	return matches[primary].ID
}

func ingestSessionTurnV1(g *Governor, turn observation.SessionTurnEnvelope) (observation.SessionTurnReceipt, error) {
	if err := validateSessionTurnEnvelope(turn); err != nil {
		return observation.SessionTurnReceipt{}, err
	}
	digest, err := turn.Digest()
	if err != nil {
		return observation.SessionTurnReceipt{}, err
	}
	row := store.SessionTurnObservation{
		ObservationID: turn.ObservationID, Runtime: turn.Runtime, SessionID: turn.SessionID,
		CatalogSessionID: resolveCatalogSessionID(turn.Runtime, turn.SessionID),
		Kind:             turn.Kind, ObservedAt: turn.ObservedAt,
		// The daemon clock, in milliseconds, is the decider's only comparator:
		// Stop and SessionEnd from one process land in the same second.
		ReceivedAtMS: time.Now().UnixMilli(),
		NativeSource: turn.NativeSource, SourceRef: turn.TranscriptPath,
		EvidenceDigest: digest, CollectorID: turn.CollectorID,
		CollectorVersion: turn.CollectorVersion,
		DeliveryAttempts: turn.DeliveryAttempts, DeliveryMode: turn.DeliveryMode,
	}
	g.writeMu.Lock()
	transaction, err := g.ix.BeginGov()
	created := false
	if err == nil {
		err = transaction.EnsureSessionRoot(turn.Runtime, turn.SessionID, turn.TranscriptPath, turn.Cwd)
	}
	if err == nil {
		created, _, err = transaction.AppendSessionTurn(row)
	}
	if err == nil {
		err = transaction.Commit()
	} else if transaction != nil {
		_ = transaction.Rollback()
	}
	g.writeMu.Unlock()
	if err != nil {
		return observation.SessionTurnReceipt{}, err
	}
	if created {
		sessionStatusRefold(turn.Runtime, turn.SessionID)
		requestNaturalSignalEmit()
	}
	// Pending helper messages are claimed by the HTTP reply, not here
	// (delivery-claim-on-reply plan D1).
	return observation.SessionTurnReceipt{
		Schema: observation.SessionTurnSchemaV1, ObservationID: turn.ObservationID, Duplicate: !created,
	}, nil
}

func handleGovernSessionTurnV1(w http.ResponseWriter, r *http.Request) {
	if governor == nil {
		http.Error(w, governorNotConfigured, http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, observation.MaxEnvelopeBytes)
	var turn observation.SessionTurnEnvelope
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&turn); err != nil {
		http.Error(w, "invalid session turn observation: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		http.Error(w, "invalid session turn observation: trailing JSON data", http.StatusBadRequest)
		return
	}
	receipt, err := ingestSessionTurnV1(governor, turn)
	if err != nil {
		var validation *observationValidationError
		switch {
		case errors.As(err, &validation):
			http.Error(w, validation.Error(), http.StatusBadRequest)
		case errors.Is(err, store.ErrSessionTurnCollision):
			http.Error(w, err.Error(), http.StatusConflict)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	replyWithDeliveries(w, r, governor, carrierBoundary{Runtime: turn.Runtime, SessionID: turn.SessionID, Kind: turn.Kind,
		ObservationID: turn.ObservationID, DeliveryMode: turn.DeliveryMode, Duplicate: receipt.Duplicate, Carrier: turn.Carrier},
		receipt, func(receipt *observation.SessionTurnReceipt, deliveries []observation.Delivery) {
			receipt.Deliveries = deliveries
		})
}
