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
	"sync"
	"time"

	"crossing-guard/internal/changeenv"
	"crossing-guard/internal/collectionconfig"
	"crossing-guard/internal/observation"
	"crossing-guard/store"
)

var checkpointCaptureLocks sync.Map

var captureCheckpointEvidence = changeenv.CaptureCheckpointEvidence

func checkpointCaptureLock(checkpoint store.SessionCheckpoint) *sync.Mutex {
	key := checkpoint.CheckoutID
	if key == "" {
		key = checkpoint.ScopeKey
	}
	value, _ := checkpointCaptureLocks.LoadOrStore(key, &sync.Mutex{})
	return value.(*sync.Mutex)
}

func handleGovernObserveV1(w http.ResponseWriter, r *http.Request) {
	if governor == nil {
		http.Error(w, "governor not configured", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, observation.MaxEnvelopeBytes)
	var envelope observation.Envelope
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&envelope); err != nil {
		http.Error(w, "invalid v1 observation: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		http.Error(w, "invalid v1 observation: trailing JSON data", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), observation.DirectCaptureBudget)
	defer cancel()
	receipt, err := ingestObservationV1(ctx, governor, envelope)
	if err != nil {
		if errors.Is(err, ErrObservationCollision) {
			recordObservationIssue(governor, envelope, "collision", err)
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		var validation *observationValidationError
		if errors.As(err, &validation) {
			http.Error(w, validation.Error(), http.StatusBadRequest)
			return
		}
		governor.observeFailures.Add(1)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// The optional orchestration layer sees only the already committed receipt and
	// bounded envelope. It has no response channel into this lower observation path.
	if reviewHost != nil {
		reviewHost.offer(envelope, receipt)
	}
	replyWithDeliveries(w, r, governor, carrierBoundary{Runtime: envelope.Runtime, SessionID: envelope.SessionID, Kind: "tool.started",
		ObservationID: envelope.ObservationID, NativeCallID: envelope.NativeCallID, DeliveryMode: envelope.DeliveryMode,
		Duplicate: receipt.Duplicate, Carrier: observeBoundaryCarries(envelope)},
		receipt, func(receipt *observation.Receipt, deliveries []observation.Delivery) { receipt.Deliveries = deliveries })
}

type observationValidationError struct{ reason string }

func (e *observationValidationError) Error() string { return e.reason }
func invalidObservation(format string, args ...any) error {
	return &observationValidationError{reason: fmt.Sprintf(format, args...)}
}

func validateObservationV1(e observation.Envelope) error {
	if e.Schema != observation.SchemaV1 {
		return invalidObservation("observation_schema must be %s", observation.SchemaV1)
	}
	if len(e.ObservationID) != 36 || !strings.HasPrefix(e.ObservationID, "obs_") {
		return invalidObservation("observation_id must be obs_ plus 32 lowercase hex characters")
	}
	for _, r := range e.ObservationID[4:] {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return invalidObservation("observation_id must be obs_ plus 32 lowercase hex characters")
		}
	}
	if e.ActionID != "" {
		if len(e.ActionID) != 36 || !strings.HasPrefix(e.ActionID, "act_") {
			return invalidObservation("action_id must be act_ plus 32 lowercase hex characters")
		}
		for _, r := range e.ActionID[4:] {
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
				return invalidObservation("action_id must be act_ plus 32 lowercase hex characters")
			}
		}
	}
	if e.CollectorID == "" || len(e.CollectorID) > 128 {
		return invalidObservation("collector_id is required and bounded")
	}
	if len(e.CollectorVersion) > 512 || len(e.NativeCallID) > 16<<10 || len(e.NativeCallKind) > 128 || len(e.TranscriptPath) > 16<<10 {
		return invalidObservation("collector/native source metadata exceeds its bound")
	}
	if e.SessionID == "" || len(e.SessionID) > 512 {
		return invalidObservation("session is required and bounded")
	}
	if e.Tool == "" || len(e.Tool) > 512 {
		return invalidObservation("tool is required and bounded")
	}
	if len(e.Cwd) > 16<<10 || len(e.FilePath) > 16<<10 || len(e.URL) > 64<<10 ||
		len(e.Command) > 256<<10 || len(e.Content) > 256<<10 || len(e.Reason) > 64<<10 {
		return invalidObservation("legacy projection field exceeds its bound")
	}
	if len(e.FilePaths) > 512 {
		return invalidObservation("too many compatibility file paths")
	}
	if len(e.Runtime) > 64 || !validRuntimeName(e.Runtime) {
		return invalidObservation("runtime must be a short identifier")
	}
	switch e.Decision {
	case "", "allow", "deny", "ask":
	default:
		return invalidObservation("decision must be allow, deny, or ask")
	}
	if e.DeliveryAttempts < 1 || (e.DeliveryMode != "direct" && e.DeliveryMode != "replay") {
		return invalidObservation("delivery metadata is invalid")
	}
	if e.ToolInputBytes < 0 || e.ToolInputBytes > 1<<30 {
		return invalidObservation("tool_input_bytes is invalid")
	}
	switch e.ToolInputCompleteness {
	case "complete":
		if e.ToolInput == nil || len(e.ToolInput) > observation.MaxRetainedInput || len(e.ToolInput) != e.ToolInputBytes || !json.Valid(e.ToolInput) ||
			observation.DigestBytes(e.ToolInput) != e.ToolInputDigest {
			return invalidObservation("complete tool_input body/size/digest is inconsistent")
		}
	case "metadata-only":
		if e.ToolInput != nil || e.ToolInputDigest == "" {
			return invalidObservation("metadata-only tool_input must omit payload and retain digest")
		}
	case "unavailable":
		if e.ToolInput != nil {
			return invalidObservation("unavailable tool_input must omit payload")
		}
	default:
		return invalidObservation("tool_input_completeness is invalid")
	}
	if len(e.ResourceClaims) > 512 {
		return invalidObservation("too many resource claims")
	}
	for i, claim := range e.ResourceClaims {
		if claim.Ordinal != i || claim.Kind == "" || len(claim.Kind) > 64 || !validRuntimeName(claim.Kind) || claim.RawIdentity == "" || claim.SourceField == "" ||
			claim.EvidenceClass != "declared" || len(claim.RawIdentity) > 16<<10 || len(claim.Identity) > 16<<10 || len(claim.SourceField) > 256 {
			return invalidObservation("resource claim %d is malformed", i)
		}
		switch claim.Operation {
		case "read", "search", "write", "patch", "execute", "connect", "use", "unknown":
		default:
			return invalidObservation("resource claim %d operation is invalid", i)
		}
		switch claim.Completeness {
		case "complete", "partial", "unresolved":
		default:
			return invalidObservation("resource claim %d completeness is invalid", i)
		}
		if claim.Kind == "file" && claim.Identity != "" && !filepath.IsAbs(filepath.Clean(claim.Identity)) {
			return invalidObservation("resource claim %d resolved file is not absolute", i)
		}
	}
	return nil
}

func ingestObservationV1(ctx context.Context, g *Governor, e observation.Envelope) (observation.Receipt, error) {
	if err := validateObservationV1(e); err != nil {
		return observation.Receipt{}, err
	}
	if e.TS == 0 {
		e.TS = time.Now().Unix()
	}
	digest, err := e.Digest()
	if err != nil {
		return observation.Receipt{}, err
	}
	attachment := prepareAttachment(g.ix, e)
	input := store.EventInput{MediaType: observation.InputMediaTypeJSON, RawBytes: e.ToolInputBytes,
		CapturedBytes: len(e.ToolInput), Digest: e.ToolInputDigest, Completeness: e.ToolInputCompleteness,
		Payload: e.ToolInput, SourceRef: e.TranscriptPath}
	receivedAt := time.Now().Unix()
	// A session-activity fact asserts a canonical (runtime, native session) root.
	// An envelope without that identity is still valid governance evidence; it just
	// cannot claim a session root. Attaching activity anyway made the store reject
	// the whole append, and spooled replays of such envelopes retried forever.
	var activity *store.SessionActivityObservation
	if e.Runtime != "" && e.SessionID != "" {
		activity = &store.SessionActivityObservation{ObservationID: "open:" + e.ObservationID,
			Runtime: e.Runtime, SessionID: e.SessionID, State: "open", ObservedAt: e.TS,
			ValidUntil: e.TS + sessionEntryObservationValidity, EvidenceClass: "positive-open",
			EntryKind: "first-action", SourceRef: e.TranscriptPath, EvidenceDigest: digest,
			CollectorID: e.CollectorID, CollectorVersion: e.CollectorVersion, ReceivedAt: receivedAt,
			DeliveryAttempts: e.DeliveryAttempts, DeliveryMode: e.DeliveryMode}
	}
	result, err := g.ObserveV1(Observation{SessionID: e.SessionID, Runtime: e.Runtime, Tool: e.Tool,
		Command: e.Command, Content: e.Content, FilePath: e.FilePath, FilePaths: e.FilePaths,
		Cwd: e.Cwd, URL: e.URL, Skill: e.Skill, TS: e.TS, Decision: e.Decision, Reason: e.Reason,
		Origin: "live", ResourceClaims: e.ResourceClaims}, ObservationEvidence{
		Delivery: store.EventDelivery{ObservationID: e.ObservationID, ObservationSchema: e.Schema,
			ActionID: e.ActionID, EnvelopeDigest: digest, CollectorID: e.CollectorID, CollectorVersion: e.CollectorVersion,
			NativeCallID: e.NativeCallID, NativeCallKind: e.NativeCallKind, QueuedAt: e.QueuedAt,
			ReceivedAt: receivedAt, DeliveryAttempts: e.DeliveryAttempts, DeliveryMode: e.DeliveryMode},
		Input: input, Attachment: attachment, Activity: activity})
	if err != nil {
		return observation.Receipt{}, fmt.Errorf("append v1 observation: %w", err)
	}
	g.writeMu.Lock()
	reconcileErr := g.ix.ReconcileResultsForEvent(result.EventID, time.Now().Unix())
	if reconcileErr == nil {
		var exactResultIDs []string
		exactResultIDs, reconcileErr = g.ix.ExactResultObservationIDsForEvent(result.EventID)
		if reconcileErr == nil {
			for _, resultObservationID := range exactResultIDs {
				if reconcileErr = g.ix.ResolveCollectionIssue(observationIssueID("result-unjoined",
					resultObservationID), time.Now().Unix()); reconcileErr != nil {
					break
				}
			}
		}
	}
	g.writeMu.Unlock()
	if reconcileErr != nil {
		return observation.Receipt{}, fmt.Errorf("reconcile pending results: %w", reconcileErr)
	}
	checkpoint := result.Checkpoint
	if checkpoint.ID != 0 && checkpoint.Status != "complete" {
		checkpoint = captureAttachmentCheckpoint(ctx, g, e, checkpoint)
	}
	attachmentIsCurrentBoundary := checkpoint.Status == "complete" && checkpoint.BoundaryClass == "direct-pre-release" && checkpoint.TriggerObservationID == e.ObservationID
	if mutatingObservation(e) && !attachmentIsCurrentBoundary {
		_ = capturePreMutation(ctx, g, e)
	}
	if kind := preBoundaryKind(e); kind != "" {
		_ = capturePreBoundary(ctx, g, e, kind)
	}
	if err := requestLifecycleHint(g, store.LiveSessionRuntime{SessionID: e.SessionID, Runtime: e.Runtime,
		TranscriptPath: e.TranscriptPath, WorkingDirectory: e.Cwd}); err != nil {
		return observation.Receipt{}, fmt.Errorf("queue action lifecycle reconciliation: %w", err)
	}
	requestNaturalSignalEmit()
	// Pending helper messages are claimed by the HTTP reply, not here
	// (delivery-claim-on-reply plan D1): only the reply knows whether the hook
	// can still read them.
	return observation.Receipt{Schema: observation.SchemaV1, ObservationID: e.ObservationID, ActionID: e.ActionID,
		EventID: result.EventID, Duplicate: result.Duplicate, Checkpoint: observation.CheckpointReceipt{
			ID: checkpoint.ID, Status: checkpoint.Status, BoundaryClass: checkpoint.BoundaryClass,
			ChangeRecordID: checkpoint.ChangeRecordID, FailureKind: checkpoint.FailureKind}}, nil
}

func prepareAttachment(ix *store.Index, e observation.Envelope) *store.SessionCheckpoint {
	if existing, found, err := ix.SessionCheckpointForWorkingDirectory(e.Runtime, e.SessionID, e.Cwd); err == nil && found {
		return &existing
	}
	cwd := filepath.Clean(e.Cwd)
	scope := "cwd-unavailable:" + e.ObservationID
	if e.Cwd != "" && filepath.IsAbs(cwd) {
		scope = "cwd:" + cwd
	}
	c := &store.SessionCheckpoint{Runtime: e.Runtime, SessionID: e.SessionID, ScopeKey: scope, Kind: "attachment",
		TriggerObservationID: e.ObservationID, WorkingDirectory: e.Cwd, Status: "pending",
		BoundaryClass: "unconfirmed", RequestedAt: time.Now().Unix()}
	if e.Cwd == "" || !filepath.IsAbs(cwd) {
		return c
	}
	if repo, err := changeenv.ResolveRepository(cwd); err == nil {
		c.ScopeKey = "checkout:" + repo.CheckoutID
		c.RepositoryID, c.CheckoutID, c.CheckoutRoot = repo.ID, repo.CheckoutID, repo.Root
	}
	return c
}

func captureAttachmentCheckpoint(ctx context.Context, g *Governor, e observation.Envelope, checkpoint store.SessionCheckpoint) store.SessionCheckpoint {
	boundary := "direct-pre-release"
	if e.DeliveryMode == "replay" {
		boundary = "late-replay"
	} else if checkpoint.CaptureAttempts > 0 || checkpoint.Status == "failed" || checkpoint.Status == "unavailable" {
		boundary = "unconfirmed"
	}
	return captureSessionCheckpoint(ctx, g, e.SessionID, e.Runtime, e.ObservationID, checkpoint, boundary)
}

func captureSessionCheckpoint(ctx context.Context, g *Governor, sessionID, runtime, observationID string, checkpoint store.SessionCheckpoint, boundary string) store.SessionCheckpoint {
	current := captureSessionCheckpointSource(ctx, g, sessionID, runtime, observationID, checkpoint, boundary)
	requestUnderstandingCheckpoint(g, current)
	return current
}

func captureSessionCheckpointSource(ctx context.Context, g *Governor, sessionID, runtime, observationID string, checkpoint store.SessionCheckpoint, boundary string) store.SessionCheckpoint {
	select {
	case g.checkpointSlot <- struct{}{}:
		defer func() { <-g.checkpointSlot }()
	default:
		select {
		case g.checkpointSlot <- struct{}{}:
			defer func() { <-g.checkpointSlot }()
		case <-ctx.Done():
			return checkpoint
		}
	}
	lock := checkpointCaptureLock(checkpoint)
	lock.Lock()
	defer lock.Unlock()
	ix := g.ix
	now := time.Now().Unix()
	g.writeMu.Lock()
	claimed, won, err := ix.ClaimSessionCheckpoint(checkpoint.ID, now, now-30, boundary)
	g.writeMu.Unlock()
	if !won && err == nil {
		return claimed
	}
	if err != nil {
		g.writeMu.Lock()
		current, currentErr := ix.SessionCheckpointByID(checkpoint.ID)
		g.writeMu.Unlock()
		if currentErr == nil {
			return current
		}
		return checkpoint
	}
	cwd := filepath.Clean(claimed.WorkingDirectory)
	failureBoundary := boundary
	if failureBoundary == "direct-pre-release" {
		failureBoundary = "unconfirmed"
	}
	// Failure issues key by the checkpoint's OWNING observation, exactly as
	// success resolution and both async recovery lanes already do. A checkpoint
	// minted by observation A and recaptured during observation B must not mint
	// a B-keyed issue that no later success can ever resolve (the 2026-08-31
	// deadline-cancelled recapture left exactly that permanent ledger garbage).
	issueObservationID := claimed.TriggerObservationID
	if issueObservationID == "" {
		issueObservationID = observationID
	}
	if claimed.WorkingDirectory == "" || !filepath.IsAbs(cwd) {
		g.writeMu.Lock()
		_ = ix.FailSessionCheckpoint(claimed.ID, time.Now().Unix(), "unavailable", failureBoundary, "working-directory-unavailable", "")
		recordObservationIssueLocked(ix, observation.Envelope{ObservationID: issueObservationID, SessionID: sessionID, Runtime: runtime, CollectorID: "checkpoint-scheduler"}, "checkpoint-failed", errors.New("working directory unavailable"))
		current, _ := ix.SessionCheckpointByID(claimed.ID)
		g.writeMu.Unlock()
		return current
	}
	record, payloads, captureErr := captureCheckpointEvidence(ctx, changeenv.SnapshotInput{
		SessionID: sessionID, Runtime: runtime, RepoDir: cwd, Base: "HEAD",
		RetainBodies: g.resultPayloadMode == collectionconfig.CompleteBounded})
	if captureErr != nil {
		status, kind := "failed", "git-capture-failed"
		if strings.Contains(captureErr.Error(), "not a git repository") || strings.Contains(captureErr.Error(), "rev-parse") {
			status, kind = "unavailable", "repository-unavailable"
		}
		if errors.Is(captureErr, context.Canceled) || errors.Is(captureErr, context.DeadlineExceeded) {
			status, kind = "failed", "capture-cancelled"
		}
		g.writeMu.Lock()
		_ = ix.FailSessionCheckpoint(claimed.ID, time.Now().Unix(), status, failureBoundary, kind, observation.DigestBytes([]byte(captureErr.Error())))
		recordObservationIssueLocked(ix, observation.Envelope{ObservationID: issueObservationID, SessionID: sessionID, Runtime: runtime, CollectorID: "checkpoint-scheduler"}, "checkpoint-failed", captureErr)
		current, _ := ix.SessionCheckpointByID(claimed.ID)
		g.writeMu.Unlock()
		return current
	}
	current := func() store.SessionCheckpoint {
		g.writeMu.Lock()
		defer g.writeMu.Unlock()
		if err := ix.SetSessionCheckpointRepository(claimed.ID, record.RepositoryID, record.CheckoutID, record.CheckoutRoot); err != nil {
			_ = ix.FailSessionCheckpoint(claimed.ID, time.Now().Unix(), "failed", failureBoundary, "checkpoint-link-failed", observation.DigestBytes([]byte(err.Error())))
			recordObservationIssueLocked(ix, observation.Envelope{ObservationID: issueObservationID, SessionID: sessionID, Runtime: runtime, CollectorID: "checkpoint-scheduler"}, "checkpoint-failed", err)
			current, _ := ix.SessionCheckpointByID(claimed.ID)
			return current
		}
		completedAt := time.Now().Unix()
		if err := ix.CompleteSessionCheckpointWithPayload(claimed.ID, record, payloads, completedAt); err != nil {
			_ = ix.FailSessionCheckpoint(claimed.ID, time.Now().Unix(), "failed", failureBoundary, "checkpoint-store-failed", observation.DigestBytes([]byte(err.Error())))
			recordObservationIssueLocked(ix, observation.Envelope{ObservationID: issueObservationID, SessionID: sessionID, Runtime: runtime, CollectorID: "checkpoint-scheduler"}, "checkpoint-failed", err)
		} else {
			_ = ix.ResolveCollectionIssue(observationIssueID("checkpoint-failed", claimed.TriggerObservationID), completedAt)
			boundedPaths := 0
			for _, payload := range payloads {
				if strings.Contains(payload.Limitation, "bounded") {
					boundedPaths++
				}
			}
			if boundedPaths > 0 {
				_ = ix.RecordCollectionIssue(store.CollectionIssue{
					IssueID:   observationIssueID("checkpoint-payload-bounded", claimed.RequestID),
					SessionID: sessionID, Runtime: runtime, ObservationID: observationID,
					CollectorID: "checkpoint-scheduler", Kind: "checkpoint-payload-bounded",
					AffectedCount: boundedPaths, FirstSeen: completedAt, LastSeen: completedAt,
					DetailDigest: observation.DigestBytes([]byte(fmt.Sprintf("checkpoint=%d paths=%d", claimed.ID, boundedPaths))),
				})
			}
		}
		current, _ := ix.SessionCheckpointByID(claimed.ID)
		if current.Status == "complete" {
			if err := reconcileCheckpointPaths(ix, current); err != nil {
				recordObservationIssueLocked(ix, observation.Envelope{ObservationID: observationID, SessionID: sessionID, Runtime: runtime, CollectorID: "checkpoint-scheduler"}, "reconciliation-failed", err)
			}
		}
		return current
	}()
	return current
}

func recordObservationIssue(g *Governor, e observation.Envelope, kind string, cause error) {
	g.writeMu.Lock()
	defer g.writeMu.Unlock()
	recordObservationIssueLocked(g.ix, e, kind, cause)
}

func recordObservationIssueLocked(ix *store.Index, e observation.Envelope, kind string, cause error) {
	now := time.Now().Unix()
	detail := ""
	if cause != nil {
		detail = observation.DigestBytes([]byte(cause.Error()))
	}
	issueID := observationIssueID(kind, e.ObservationID)
	_ = ix.RecordCollectionIssue(store.CollectionIssue{IssueID: issueID, SessionID: e.SessionID,
		Runtime: e.Runtime, ObservationID: e.ObservationID, CollectorID: "daemon-observation-v1",
		Kind: kind, AffectedCount: 1, FirstSeen: now, LastSeen: now, DetailDigest: detail})
}

func observationIssueID(kind, observationID string) string {
	return observation.DigestBytes([]byte(kind + "\x00" + observationID))
}
