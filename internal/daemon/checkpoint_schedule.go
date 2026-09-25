package daemon

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"crossing-guard/engine"
	"crossing-guard/internal/changeenv"
	"crossing-guard/internal/observation"
	"crossing-guard/store"
)

const (
	settledQuietWindow = 2 * time.Second
	settledHardWindow  = 30 * time.Second
	idleTimeoutBatch   = 2
	recoveryBatch      = 2
)

type settledCheckpointState struct {
	checkpoint                        store.SessionCheckpoint
	sessionID, runtime, observationID string
	first                             time.Time
	timer                             *time.Timer
}

type settledCheckpointCoordinator struct {
	sync.Mutex
	items map[string]*settledCheckpointState
}

func mutatingObservation(e observation.Envelope) bool {
	if e.Decision != "allow" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(engine.BareTool(e.Tool))) {
	case "edit", "write", "notebookedit", "apply_patch":
		return true
	case "read", "glob", "grep", "webfetch", "websearch", "task", "todowrite", "update_plan":
		return false
	}
	for _, claim := range e.ResourceClaims {
		if claim.Kind == "file" && (claim.Operation == "write" || claim.Operation == "patch") {
			return true
		}
	}
	return false
}

func preBoundaryKind(e observation.Envelope) string {
	if e.Decision != "allow" {
		return ""
	}
	command := strings.TrimSpace(e.Command)
	fields := strings.Fields(command)
	if len(fields) >= 2 && fields[0] == "git" && fields[1] == "commit" {
		return "pre-commit"
	}
	for _, prefix := range []string{"go test", "go vet", "npm test", "npm run test", "pnpm test",
		"yarn test", "pytest", "python -m pytest", "cargo test", "make test", "scripts/check.sh"} {
		if command == prefix || strings.HasPrefix(command, prefix+" ") {
			return "pre-verification"
		}
	}
	return ""
}

func checkpointRequest(e observation.Envelope, kind, requestID, boundary string) store.SessionCheckpoint {
	cwd := filepath.Clean(e.Cwd)
	scope := "cwd-unavailable:" + requestID
	c := store.SessionCheckpoint{Runtime: e.Runtime, SessionID: e.SessionID, ScopeKey: scope, Kind: kind,
		RequestID: requestID, TriggerObservationID: e.ObservationID, WorkingDirectory: e.Cwd,
		Status: "pending", BoundaryClass: boundary, RequestedAt: time.Now().Unix()}
	if e.Cwd != "" && filepath.IsAbs(cwd) {
		c.ScopeKey = "cwd:" + cwd
		if repo, err := changeenv.ResolveRepository(cwd); err == nil {
			c.ScopeKey = "checkout:" + repo.CheckoutID
			c.RepositoryID, c.CheckoutID, c.CheckoutRoot = repo.ID, repo.CheckoutID, repo.Root
		}
	}
	return c
}

func ensureCheckpoint(g *Governor, checkpoint store.SessionCheckpoint) (store.SessionCheckpoint, error) {
	g.writeMu.Lock()
	defer g.writeMu.Unlock()
	tx, err := g.ix.BeginGov()
	if err != nil {
		return store.SessionCheckpoint{}, err
	}
	defer func() { _ = tx.Rollback() }()
	got, _, err := tx.EnsureSessionCheckpoint(checkpoint)
	if err != nil {
		return store.SessionCheckpoint{}, err
	}
	if err := tx.Commit(); err != nil {
		return store.SessionCheckpoint{}, err
	}
	return got, nil
}

func capturePreMutation(ctx context.Context, g *Governor, e observation.Envelope) store.SessionCheckpoint {
	requestID := observation.DigestBytes([]byte("pre-mutation\x00" + e.SessionID + "\x00" + e.Cwd))
	checkpoint, err := ensureCheckpoint(g, checkpointRequest(e, "pre-mutation", requestID, "direct-pre-release"))
	if err != nil {
		return store.SessionCheckpoint{Status: "failed", BoundaryClass: "unconfirmed", FailureKind: "checkpoint-schedule-failed"}
	}
	if checkpoint.Status == "complete" {
		return checkpoint
	}
	return captureSessionCheckpoint(ctx, g, e.SessionID, e.Runtime, e.ObservationID, checkpoint, "direct-pre-release")
}

func capturePreBoundary(ctx context.Context, g *Governor, e observation.Envelope, kind string) store.SessionCheckpoint {
	requestID := observation.DigestBytes([]byte(kind + "\x00" + e.SessionID + "\x00" + e.ObservationID))
	checkpoint, err := ensureCheckpoint(g, checkpointRequest(e, kind, requestID, "direct-pre-release"))
	if err != nil {
		return store.SessionCheckpoint{Status: "failed", BoundaryClass: "unconfirmed", FailureKind: "checkpoint-schedule-failed"}
	}
	if checkpoint.Status == "complete" {
		return checkpoint
	}
	return captureSessionCheckpoint(ctx, g, e.SessionID, e.Runtime, e.ObservationID, checkpoint, "direct-pre-release")
}

func scheduleSettledCheckpoint(g *Governor, e observation.ResultEnvelope, resultID int64) error {
	if e.State != "success" || len(e.Effects) == 0 {
		return nil
	}
	cwd := filepath.Clean(e.Cwd)
	key := e.SessionID + "\x00" + cwd
	g.settled.Lock()
	if existing := g.settled.items[key]; existing != nil {
		g.writeMu.Lock()
		err := g.ix.AddSessionCheckpointTrigger(existing.checkpoint.ID, resultID,
			e.ObservationID, time.Now().Unix())
		g.writeMu.Unlock()
		if err != nil {
			g.settled.Unlock()
			return err
		}
		remaining := settledHardWindow - time.Since(existing.first)
		delay := settledQuietWindow
		if remaining < delay {
			delay = remaining
		}
		if delay < 0 {
			delay = 0
		}
		if existing.timer != nil {
			existing.timer.Stop()
		}
		existing.timer = time.AfterFunc(delay, func() { runSettledCheckpoint(g, key) })
		g.settled.Unlock()
		return nil
	}
	requestID := observation.DigestBytes([]byte("settled\x00" + e.SessionID + "\x00" + cwd + "\x00" + e.ObservationID))
	pre := observation.Envelope{ObservationID: e.ObservationID, SessionID: e.SessionID,
		Runtime: e.Runtime, Cwd: e.Cwd}
	checkpoint := checkpointRequest(pre, "settled", requestID, "settled")
	checkpoint.TriggerResultID = resultID
	checkpoint, err := ensureCheckpoint(g, checkpoint)
	if err != nil {
		g.settled.Unlock()
		return err
	}
	g.writeMu.Lock()
	err = g.ix.AddSessionCheckpointTrigger(checkpoint.ID, resultID, e.ObservationID, time.Now().Unix())
	g.writeMu.Unlock()
	if err != nil {
		g.settled.Unlock()
		return err
	}
	state := &settledCheckpointState{checkpoint: checkpoint, sessionID: e.SessionID,
		runtime: e.Runtime, observationID: e.ObservationID, first: time.Now()}
	state.timer = time.AfterFunc(settledQuietWindow, func() { runSettledCheckpoint(g, key) })
	g.settled.items[key] = state
	g.settled.Unlock()
	return nil
}

func runSettledCheckpoint(g *Governor, key string) {
	g.settled.Lock()
	state := g.settled.items[key]
	delete(g.settled.items, key)
	g.settled.Unlock()
	if state == nil || state.checkpoint.Status == "complete" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	_ = captureSessionCheckpoint(ctx, g, state.sessionID, state.runtime, state.observationID,
		state.checkpoint, "settled")
}

func stopSettledCheckpoints(g *Governor) {
	g.settled.Lock()
	defer g.settled.Unlock()
	for key, state := range g.settled.items {
		if state.timer != nil {
			state.timer.Stop()
		}
		delete(g.settled.items, key)
	}
}

func recoverPendingCheckpointsOnce(g *Governor) {
	pending, err := g.ix.PendingSessionCheckpoints(recoveryBatch)
	if err != nil {
		return
	}
	for _, checkpoint := range pending {
		if checkpoint.Kind == "attachment" || checkpoint.Kind == "pre-mutation" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		boundary := checkpoint.BoundaryClass
		if checkpoint.Kind == "timeout" {
			boundary = "point-in-time"
		}
		_ = captureSessionCheckpoint(ctx, g, checkpoint.SessionID, checkpoint.Runtime, checkpoint.TriggerObservationID,
			checkpoint, boundary)
		cancel()
	}
}

func recoverPendingCheckpoints(g *Governor) {
	go func() {
		recoverPendingCheckpointsOnce(g)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			recoverPendingCheckpointsOnce(g)
		}
	}()
}

func scheduleIdleTimeoutsOnce(g *Governor, now time.Time) error {
	// Timeout is a live collection boundary, not an automatic historical backfill.
	// Only sessions observed since this daemon started are eligible, and a tick does
	// at most two serial captures so Git/store work cannot fan out across a corpus.
	idle, err := g.ix.IdleSessions(now.Add(-5*time.Minute).Unix(), g.startedAt, idleTimeoutBatch)
	if err != nil {
		return err
	}
	for _, session := range idle {
		// Finalization is independent of checkpoint capture. Run it before terminal
		// checkpoint skipping so a transient issue-write failure is retried on the
		// next bounded idle tick even when capture already completed.
		if err := finalizeMissingResults(g, session.SessionID); err != nil {
			return err
		}
		requestID := observation.DigestBytes([]byte("timeout\x00" + session.SessionID + "\x00" + time.Unix(session.LastEventAt, 0).String()))
		e := observation.Envelope{ObservationID: requestID, SessionID: session.SessionID,
			Runtime: session.Runtime, Cwd: session.WorkingDirectory}
		checkpoint := checkpointRequest(e, "timeout", requestID, "point-in-time")
		checkpoint.RequestedAt = now.Unix()
		got, err := ensureCheckpoint(g, checkpoint)
		if err != nil {
			return err
		}
		if got.Status == "complete" || got.Status == "capturing" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		_ = captureSessionCheckpoint(ctx, g, got.SessionID, session.Runtime, got.TriggerObservationID, got, "point-in-time")
		cancel()
	}
	return nil
}

func runIdleTimeoutTick(g *Governor, now time.Time) error {
	err := scheduleIdleTimeoutsOnce(g, now)
	if err != nil && g.lifecycle != nil {
		g.lifecycle.stats.failed.Add(1)
	}
	return err
}

func startIdleTimeoutScheduler(g *Governor) {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for now := range ticker.C {
			if err := runIdleTimeoutTick(g, now); err != nil {
				continue
			}
		}
	}()
}
