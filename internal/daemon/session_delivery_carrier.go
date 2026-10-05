package daemon

// The carrier half of helper delivery (helper-session-attachment plan D5,
// delivery-claim-on-reply plan): when a hook boundary for a session reaches
// the daemon, pending helper messages for that session travel back inside the
// receipt the hook client already reads. The REPLY owns the claim: messages
// are claimed only while the hook can still read the reply (its request is
// alive and its stated deadline leaves the configured margin), the receipt is
// written and flushed, and only then is the handoff recorded on the run. A
// reply that fails to reach the hook puts its messages back to pending for
// the next boundary. Only the direct HTTP path for a non-duplicate row claims
// (red-team H1): spool replay and duplicate receipts have no hook waiting to
// print anything. Which observation kinds may carry is configuration in OUR
// vocabulary, never a vendor event name (red-team A4).

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"crossing-guard/internal/observation"
	"crossing-guard/store"
)

// carrierBoundary is one hook boundary as the ingest handler saw it.
type carrierBoundary struct {
	Runtime, SessionID string
	Kind               string // OUR observation kind
	ObservationID      string
	NativeCallID       string
	DeliveryMode       string
	Duplicate          bool
	// Carrier is the client's own declaration that it can print context for
	// this runtime; without it nothing is claimed (an old plugin or a hook
	// without an encoder would otherwise turn "delivered" into "dropped").
	Carrier bool
}

// hookCanStillRead reports whether a reply written now can reach the hook:
// its request is alive and, when it stated a deadline, at least the claim
// margin remains. A hook that states no deadline (an older client) is judged
// by the context alone.
func hookCanStillRead(r *http.Request, margin time.Duration) bool {
	if r.Context().Err() != nil {
		return false
	}
	if deadline, ok := observation.ParseHookDeadline(r.Header.Get(observation.HookDeadlineHeader)); ok && time.Until(deadline) < margin {
		return false
	}
	return true
}

// claimForReply claims the pending messages this boundary may carry. The
// check before the claim is cheap and only avoids pointless claims; the
// authoritative check is replyWithDeliveries's after the claim returns,
// because the claim waits on the governor's write lock and can outlive the
// hook (review F2).
func claimForReply(g *Governor, r *http.Request, boundary carrierBoundary) []store.SessionDelivery {
	config := orchestrationConfig()
	if g == nil || g.ix == nil || boundary.Duplicate || !boundary.Carrier || boundary.DeliveryMode != "direct" || !config.IsCarrierKind(boundary.Kind) {
		return nil
	}
	if !hookCanStillRead(r, config.ClaimMargin()) {
		return nil
	}
	g.writeMu.Lock()
	// A handoff's brief rides only a kind at which the runtime's hook can tell a
	// nested call from the session's own, so it is never handed to a child (team
	// rest-of-release plan §6.5, K-1); helper rows ride every carrier kind.
	claimed, err := g.ix.ClaimSessionDeliveriesAt(store.SessionDeliveryClaim{Runtime: boundary.Runtime, SessionID: boundary.SessionID,
		Kind: boundary.Kind, ObservationID: boundary.ObservationID, NativeCallID: boundary.NativeCallID, Now: time.Now().Unix(),
		MaxBytes: config.Delivery.ClaimBytes, CarriesHandoff: handoffCarrierKind(boundary.Runtime, boundary.Kind)})
	g.writeMu.Unlock()
	if err != nil {
		// A failed claim leaves the record pending for the next boundary; the
		// hook proceeds without context. Loud, never fatal to the receipt.
		log.Printf("session delivery claim failed for %s/%s at %s: %v", boundary.Runtime, boundary.SessionID, boundary.Kind, err)
		return nil
	}
	return claimed
}

// replyWithDeliveries writes one hook boundary's receipt with the messages
// claimed for it. Settlement rules (review F1): after the claim returns the
// request must still be live and inside its deadline, or the messages go back
// to pending and the receipt carries none; after the write, only a write
// error releases them. A context cancelled after a successful write is the
// hook closing a reply it already read, never a reason to release (that
// would deliver the message twice). Flush is for latency only: the API
// timing wrapper does not surface flush errors. The remaining gap is a hook
// that closes in the microseconds between the check and the write.
func replyWithDeliveries[T any](w http.ResponseWriter, r *http.Request, g *Governor, boundary carrierBoundary,
	receipt T, attach func(*T, []observation.Delivery)) {
	claimed := claimForReply(g, r, boundary)
	if len(claimed) > 0 && !hookCanStillRead(r, orchestrationConfig().ClaimMargin()) {
		releaseClaimedDeliveries(g, boundary, claimed, errors.New("the hook's deadline passed while the claim waited"))
		claimed = nil
	}
	deliveries := make([]observation.Delivery, 0, len(claimed))
	for _, record := range claimed {
		deliveries = append(deliveries, observation.Delivery{DeliveryID: record.DeliveryID, Message: record.Message})
	}
	if len(deliveries) > 0 {
		attach(&receipt, deliveries)
	}
	body, err := json.Marshal(receipt)
	if err != nil {
		releaseClaimedDeliveries(g, boundary, claimed, err)
		http.Error(w, "encode receipt: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, writeErr := w.Write(append(body, '\n'))
	if writeErr == nil {
		_ = http.NewResponseController(w).Flush()
	}
	if len(claimed) == 0 {
		return
	}
	if writeErr != nil {
		releaseClaimedDeliveries(g, boundary, claimed, writeErr)
		return
	}
	for _, record := range claimed {
		recordSessionDeliveryHandoff(g.ix, record)
	}
}

// observeBoundaryCarries reports whether a pre-tool observation's reply can
// carry helper messages: only when the hook will let the tool run. A deny
// boundary's hook prints its denial and drops everything else (review F3).
func observeBoundaryCarries(e observation.Envelope) bool {
	return e.Carrier && e.Decision == "allow"
}

func releaseClaimedDeliveries(g *Governor, boundary carrierBoundary, claimed []store.SessionDelivery, cause error) {
	if len(claimed) == 0 {
		return
	}
	ids := make([]string, 0, len(claimed))
	for _, record := range claimed {
		ids = append(ids, record.DeliveryID)
	}
	g.writeMu.Lock()
	released, err := g.ix.ReleaseSessionDeliveries(ids, boundary.ObservationID)
	g.writeMu.Unlock()
	if err != nil {
		log.Printf("session delivery release failed for %s/%s at %s (%d claimed, cause %v): %v",
			boundary.Runtime, boundary.SessionID, boundary.Kind, len(ids), cause, err)
		return
	}
	log.Printf("session delivery: reply to %s/%s at %s could not carry its messages (%v); %d back to pending",
		boundary.Runtime, boundary.SessionID, boundary.Kind, cause, released)
}
