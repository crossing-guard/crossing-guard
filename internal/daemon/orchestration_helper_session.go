package daemon

// Helper session persistence (helper-persistent-session plan, schema 30).
// A follower/helper binding owns ONE vendor session per source session; every
// turn after the first resumes it through the same ChatRequest.SessionID
// mechanism the reply path uses. Signals that arrive while a turn occupies
// the session coalesce into the group's single pending slot and launch when
// the session frees. None of this is visible to the helper: its prompt,
// claim vocabulary, and signal catalog are unchanged.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"crossing-guard/internal/orchestration/profilefs"
	"crossing-guard/store"
)

// persistentAgent reports whether a matched profile forms a helper session:
// the design table's follower/coordinator/monitor row (managed-turn
// followers and helpers). Reviewers never do.
func persistentAgent(compiled profilefs.CompiledProfile) bool {
	agentType := compiled.AgentType()
	return compiled.Execution == "managed-turn" && (agentType == "follower" || agentType == "helper")
}

// helperSessionBudget resolves the admission budget for a persistent group.
// MaxTotal keeps the binding's owner budget (now turns per source session);
// concurrency is the profile's declared max-concurrency bounded by the
// adapter's ConcurrentTurns capability — no compiled constant; the store
// coalesces beyond it.
func helperSessionBudget(binding store.ManagedBinding, compiled profilefs.CompiledProfile) store.ManagedGroupBudget {
	limits := resolveAgentLimits(binding.Limits)
	active := int64(compiled.Limits.MaxConcurrency)
	if active < 1 || !runtimeConcurrentTurns(binding.Runtime) {
		active = 1
	}
	return store.ManagedGroupBudget{MaxTotal: limits.MaxTotal, MaxActive: active, CoalesceWhenOccupied: true}
}

// runtimeConcurrentTurns is the adapter's answer to "may two turns run on one
// of your sessions at once" — false on every shipped adapter (2026-09-12:
// two writers on one vendor file is the hazard the ownership gate exists for).
func runtimeConcurrentTurns(runtime string) bool {
	provider, ok := chatDrivers[runtime].(chatCapabilityProvider)
	if !ok {
		return false
	}
	return provider.ChatCapability().ConcurrentTurns
}

// groupForSource finds or shapes the pairing group for one binding and source
// session (plan D1). Lookup by root task id first, then by session identity;
// a group found by task id that lacks an identity the task now carries learns
// it once. A miss shapes a new group keyed on the session identity (task id
// only when the task has none yet); AdmitManagedRun inserts it.
func (host *orchestrationManagedHost) groupForSource(binding store.ManagedBinding, task RuntimeTask) (store.ManagedGroup, error) {
	now := time.Now().Unix()
	existing, found, err := host.ix.ManagedGroupForSource(binding.BindingID, task.ID, task.Runtime, task.CatalogSessionID, task.NativeSessionID)
	if err != nil {
		return store.ManagedGroup{}, err
	}
	if found {
		if (existing.RootCatalogSessionID == "" && task.CatalogSessionID != "") ||
			(existing.RootNativeSessionID == "" && task.NativeSessionID != "") {
			if err := host.ix.SetGroupRootIdentity(existing.GroupID, task.CatalogSessionID, task.NativeSessionID, now); err != nil {
				return store.ManagedGroup{}, err
			}
			if existing.RootCatalogSessionID == "" {
				existing.RootCatalogSessionID = task.CatalogSessionID
			}
			if existing.RootNativeSessionID == "" {
				existing.RootNativeSessionID = task.NativeSessionID
			}
		}
		return existing, nil
	}
	key := task.CatalogSessionID
	if key == "" {
		key = task.NativeSessionID
	}
	if key == "" {
		key = task.ID
	}
	return store.ManagedGroup{GroupID: managedID("org_", binding.BindingID, task.Runtime, key), BindingID: binding.BindingID,
		State: "active", RootTaskID: task.ID, RootRuntime: task.Runtime, RootCatalogSessionID: task.CatalogSessionID,
		RootNativeSessionID: task.NativeSessionID, ProjectRoot: task.WorkingDirectory, CreatedAt: now, UpdatedAt: now}, nil
}

// helperTurnResumes is the ONE predicate for "this launch resumes the
// group's helper session": a recorded session on the binding's PRIMARY
// runtime, launched on that runtime. A fallback-route relaunch and an edited
// primary runtime both run fresh (plan D3, no flip-flop). The turn request
// and the continuation-context lower bound both consult it, so a fresh
// session never receives only the transcript delta.
func helperTurnResumes(group store.ManagedGroup, primaryRuntime, runtime string) bool {
	return group.HelperNativeSessionID != "" && group.HelperRuntime == runtime && runtime == primaryRuntime
}

// helperTurnRequest is the one owner of the child ChatRequest for every turn
// launch (first launch, parked relaunch): it re-reads the group and resumes
// its helper session exactly when helperTurnResumes says so (the mechanism
// resumeParent already uses).
func (host *orchestrationManagedHost) helperTurnRequest(groupID, primaryRuntime, runtime, model, mode, cwd, prompt string, effort *store.ThinkingEffort) ChatRequest {
	request := ChatRequest{Runtime: runtime, Prompt: prompt, Model: model, Mode: mode, Cwd: cwd, ThinkingEffort: effort, effortSource: "binding"}
	group, found, err := host.ix.ManagedGroup(groupID)
	if err != nil || !found || !helperTurnResumes(group, primaryRuntime, runtime) {
		return request
	}
	request.SessionID = group.HelperNativeSessionID
	return request
}

// adoptHelperSession records the helper session a settled turn ran in (plan
// D2/D5), exactly once per run (the store marks the run). Identity is only
// ever REPORTED: the vendor's session event fills the child task row on a
// fresh turn; on a resumed turn the row is pre-filled at Create, so the
// vendor's answer is read from the task's own events — a `session` activity
// (resume honoured) or a `session.forked` (resume answered with another
// session, adopted). A failed turn clears the session only when the vendor
// never answered at all; a parked or still-active run never adopts or
// clears (it still owns a relaunch). Fallback-route attempts never adopt.
func (host *orchestrationManagedHost) adoptHelperSession(run store.ManagedRun, binding store.ManagedBinding) {
	if run.Kind != "" || run.ChildTaskID == "" || run.Detail["rerouted_from"] != nil {
		return
	}
	settled, found, err := host.ix.ManagedRun(run.RunID)
	if err != nil || !found {
		return
	}
	// A turn the deadline watcher stopped neither adopts nor clears (plan
	// §4.2, RT-5): a vendor slow to answer — 11 min 40 s in the 2026-09-24
	// incident — is not evidence the helper session is bad.
	if settled.State == "failed" && settled.ErrorClass == "timeout" {
		return
	}
	task, found, err := host.tasks.Task(run.ChildTaskID)
	if err != nil || !found || task.Runtime != binding.Runtime {
		return
	}
	now := time.Now().Unix()
	reported, vendorAnswered := task.NativeSessionID, false
	// The session answer arrives within the first events of a turn (spawn,
	// then the vendor's session frame); a fork is recorded in the same
	// position. Reading the head is enough and bounded.
	if events, err := host.tasks.Events(task.ID, 0, 200); err == nil {
		for _, item := range events {
			switch item.Kind {
			case "session.forked":
				if id := anyString(item.Payload["reported_native_session_id"]); id != "" {
					reported, vendorAnswered = id, true
				}
			case "task.activity":
				if sessionFrameID(item) != "" {
					vendorAnswered = true
				}
			}
		}
	}
	switch settled.State {
	case "completed":
		if reported == "" {
			return
		}
		if _, err := host.ix.AdoptHelperSessionForRun(run.RunID, run.GroupID, task.Runtime, reported, sourceTranscriptSeq(run.Detail), now); err != nil {
			log.Printf("helper session adopt failed for group %s: %v", run.GroupID, err)
		}
	case "failed", "interrupted", "unknown":
		if vendorAnswered {
			return
		}
		if _, err := host.ix.ClearHelperSessionForRun(run.RunID, run.GroupID, now); err != nil {
			log.Printf("helper session clear failed for group %s: %v", run.GroupID, err)
		}
	}
}

// drainPendingSignal launches the group's coalesced signal once no run
// occupies the helper session (plan D4). Outcomes are recorded, never
// returned: the caller is a terminal path or the sweep, and a retry loop
// must never re-drive a drained launch. The slot is cleared only after the
// admission landed (created, idempotently found, or coalesced again — the
// last replaces the slot, so the token no longer matches and the clear is a
// no-op); a crash before that leaves it for the next drain.
func (host *orchestrationManagedHost) drainPendingSignal(groupID string) {
	take, found, err := host.ix.TakeGroupPendingSignal(groupID)
	if err != nil {
		log.Printf("pending signal read failed for group %s: %v", groupID, err)
		return
	}
	if !found {
		return
	}
	now := time.Now()
	drop := func(reason string) {
		// Recorded on the group (pending_dropped + reason), never a silent loss.
		if _, err := host.ix.DropGroupPendingSignal(groupID, take.Token, reason, now.Unix()); err != nil {
			log.Printf("pending signal drop failed for group %s: %v", groupID, err)
		}
		log.Printf("pending signal for group %s dropped: %s", groupID, reason)
	}
	var task RuntimeTask
	if err := json.Unmarshal(take.Signal.Task, &task); err != nil {
		drop("unreadable pending task: " + err.Error())
		return
	}
	match, ok := host.matchBinding(take.Signal.BindingID, take.Signal.Producer, task, take.Signal.Signal)
	if !ok {
		drop("binding is gone, disabled, or no longer selects " + take.Signal.Signal)
		return
	}
	if take.Signal.Producer != managedTaskStreamKind && host.sessionIsTaskOwned(naturalSessionSignal{Runtime: task.Runtime,
		CatalogSessionID: task.CatalogSessionID, NativeSessionID: task.NativeSessionID}) {
		// The natural router's ownership gate, re-applied at launch time: a
		// session the daemon started owning while the signal waited is fed by
		// the task stream now.
		drop("session became task-owned while the signal waited")
		return
	}
	event := TaskEvent{Producer: take.Signal.Producer, EventID: take.Signal.EventID, TaskID: task.ID,
		Sequence: take.Signal.Sequence, Kind: take.Signal.Kind, OccurredAt: take.Signal.OccurredAt}
	extra := map[string]any{"coalesced_signals": take.Coalesced, "waited_ms": now.UnixMilli() - take.Signal.At}
	if err := host.launchAgentRunWithDetail(match, task, event, take.Signal.Signal, nil, extra); err != nil {
		var suppressed errManagedSuppressed
		if errors.As(err, &suppressed) {
			drop("admission suppressed: " + suppressed.class)
			return
		}
		log.Printf("pending signal launch failed for group %s (kept for the next drain): %v", groupID, err)
		return
	}
	if _, err := host.ix.ClearGroupPendingSignal(groupID, take.Token, now.Unix()); err != nil {
		log.Printf("pending signal clear failed for group %s: %v", groupID, err)
	}
}

// matchBinding re-selects one binding for a drained signal exactly as the
// live routers do (scope, consent, pinned revision, selector), so a binding
// edited while the signal waited is judged by its current state.
func (host *orchestrationManagedHost) matchBinding(bindingID, producer string, task RuntimeTask, signal string) (matchedAgent, bool) {
	binding, found, err := host.ix.ManagedBinding(bindingID)
	if err != nil || !found || binding.State != "enabled" {
		return matchedAgent{}, false
	}
	if producer != managedTaskStreamKind && !binding.WatchNatural {
		return matchedAgent{}, false
	}
	if !bindingScopeMatches(binding, task, newFolderScope(task.WorkingDirectory)) {
		return matchedAgent{}, false
	}
	return host.selectBinding(binding, signal)
}

// selectBinding is the selection half shared by the task router, the natural
// router and the drain: pinned revision, managed-shape validation, selector.
func (host *orchestrationManagedHost) selectBinding(binding store.ManagedBinding, signal string) (matchedAgent, bool) {
	detail, err := host.profiles.GetRevision(binding.ProfileID, binding.ProfileSourceDigest, binding.ProfileBundleDigest)
	if err != nil || detail.Normalized == nil {
		return matchedAgent{}, false
	}
	compiled := *detail.Normalized
	if validateManagedProfile(compiled) != nil {
		return matchedAgent{}, false
	}
	stagePrompt, selected := profileSignalSelectors(compiled)[signal]
	if !selected {
		return matchedAgent{}, false
	}
	return matchedAgent{binding: binding, compiled: compiled, stagePrompt: stagePrompt}, true
}

// reconcileHelperSessionsOnce rides the lifecycle sweep (plan D4): (1)
// liveness — a run still `running` whose child task is absent, or terminal
// with its terminal event already behind the pump's durable position (the
// pump passed it unlinked), is finished through the ordinary terminal path;
// a run `admitted` past the settle window with no child ever linked is
// settled as a lost launch. The position gate keeps the sweep from racing
// the pump over a terminal event it is about to handle. (2) Every group
// holding a pending signal is drained — the restart case included, since
// RecoverManagedRunsUnknown flips runs by SQL and no terminal path runs.
func (host *orchestrationManagedHost) reconcileHelperSessionsOnce() {
	if host.tasks == nil {
		return
	}
	batch := orchestrationConfig().HelperSession.SweepBatch
	position, err := host.ix.OrchestrationStreamPosition(managedTaskStreamKind)
	if err != nil {
		log.Printf("helper session reconcile: pump position unavailable: %v", err)
		return
	}
	running, err := host.ix.RunningManagedRuns(batch)
	if err != nil {
		log.Printf("helper session reconcile: running runs unavailable: %v", err)
	}
	for _, run := range running {
		task, found, err := host.tasks.Task(run.ChildTaskID)
		if err != nil {
			continue
		}
		kind := "task.unknown"
		if found {
			kind = terminalKindForLifecycle(task.Lifecycle)
			if kind == "" || task.LastEventID > position {
				continue
			}
		}
		if err := host.finishManagedChild(run, TaskEvent{TaskID: run.ChildTaskID, Kind: kind}); err != nil {
			log.Printf("helper session reconcile: run %s could not finish: %v", run.RunID, err)
		}
	}
	stale, err := host.ix.StaleAdmittedManagedRuns(time.Now().Add(-orchestrationConfig().TaskSettle()).Unix(), batch)
	if err != nil {
		log.Printf("helper session reconcile: stale admissions unavailable: %v", err)
	}
	for _, run := range stale {
		if err := host.ix.CompleteManagedRun(run.RunID, "failed", "", "", nil, run.Detail, "launch_lost",
			"The helper turn was admitted but its child task was never linked; the session is free again.", time.Now().Unix()); err != nil {
			log.Printf("helper session reconcile: stale admission %s: %v", run.RunID, err)
			continue
		}
		host.drainPendingSignal(run.GroupID)
	}
	groups, err := host.ix.PendingManagedGroups(batch)
	if err != nil {
		log.Printf("helper session reconcile: pending groups unavailable: %v", err)
		return
	}
	for _, group := range groups {
		host.drainPendingSignal(group.GroupID)
	}
}

func terminalKindForLifecycle(lifecycle TaskLifecycle) string {
	switch lifecycle {
	case TaskCompleted:
		return "task.completed"
	case TaskFailed:
		return "task.failed"
	case TaskInterrupted:
		return "task.interrupted"
	case TaskUnknown:
		return "task.unknown"
	}
	return ""
}

func pendingSignalFor(binding store.ManagedBinding, task RuntimeTask, event TaskEvent, signal string) store.ManagedPendingSignal {
	task.Events = nil
	body, _ := json.Marshal(task)
	return store.ManagedPendingSignal{BindingID: binding.BindingID, Producer: event.Producer, EventID: event.EventID,
		Sequence: event.Sequence, Signal: signal, Kind: event.Kind, OccurredAt: event.OccurredAt, Task: body, At: time.Now().UnixMilli()}
}

// ── Turn deadlines (managed-turn-profile-limits plan §4.2) ─────────────────

// turnTimeout is the marker the deadline watcher writes on a run before it
// stops that run's child. It names the child, so it can never apply to a
// later attempt of the same run.
type turnTimeout struct {
	ChildTaskID string `json:"child_task_id"`
	Limit       string `json:"limit"`
	LimitMS     int64  `json:"limit_ms"`
	StartedAtMS int64  `json:"started_at_ms"`
	StoppedAtMS int64  `json:"stopped_at_ms"`
}

func turnTimeoutFrom(detail map[string]any) turnTimeout {
	marker := turnTimeout{}
	raw, ok := detail["timeout"]
	if !ok {
		return marker
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return marker
	}
	_ = json.Unmarshal(encoded, &marker)
	return marker
}

// turnTimeoutMarker returns the run's timeout marker when it names the run's
// CURRENT child — the only child it may settle.
func turnTimeoutMarker(run store.ManagedRun) (turnTimeout, bool) {
	marker := turnTimeoutFrom(run.Detail)
	return marker, marker.ChildTaskID != "" && marker.ChildTaskID == run.ChildTaskID
}

// pinnedTurnLimit is one pinned revision's timeout, cached by digest pair:
// revisions are immutable, so the cache never goes stale and a pass does not
// re-parse PROFILE.md per running run.
type pinnedTurnLimit struct {
	limit time.Duration
	text  string
}

// runTurnDeadlineWatcher enforces helper-turn timeouts on their own configured
// cadence (helper_session.turn_deadline_check_ms). The 30 s lifecycle sweep
// shares its goroutine with other work, so riding it would silently make that
// lateness the precision of a user-facing limit.
func (host *orchestrationManagedHost) runTurnDeadlineWatcher() {
	defer host.wg.Done()
	every := orchestrationConfig().TurnDeadlineCheck()
	if every <= 0 {
		// Never trust a swapped or half-loaded value with a ticker.
		every = defaultOrchestrationConfig().TurnDeadlineCheck()
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	limits := map[string]pinnedTurnLimit{}
	for {
		select {
		case <-host.ctx.Done():
			return
		case <-ticker.C:
			host.enforceTurnDeadlinesOnce(time.Now(), limits)
		}
	}
}

// enforceTurnDeadlinesOnce is one watcher pass over running helper turns and
// delegate children — never reply or correction runs, which are the source
// session's own work. For a child past its pinned profile timeout it marks the
// run for exactly that child and interrupts the child only once the mark is
// durable (plan §4.2, R2-3): an unmarked kill would settle as an ordinary
// failure, which can park and relaunch the turn or clear a healthy helper
// session. A child that already ended is left to the pump, however late the
// pump is, so a provider death before the deadline still reaches the provider
// lane.
func (host *orchestrationManagedHost) enforceTurnDeadlinesOnce(now time.Time, limits map[string]pinnedTurnLimit) {
	if host.tasks == nil {
		return
	}
	running, err := host.ix.RunningManagedRuns(orchestrationConfig().HelperSession.SweepBatch)
	if err != nil {
		log.Printf("turn deadlines: running runs unavailable: %v", err)
		return
	}
	for _, run := range running {
		if (run.Kind != "" && run.Kind != "delegate") || run.ChildTaskID == "" {
			continue
		}
		limit, ok := host.pinnedTurnLimit(run, limits)
		if !ok {
			continue
		}
		started, live := host.childTurnStart(run.ChildTaskID)
		if !live || now.Sub(started) < limit.limit {
			continue
		}
		if _, marked := turnTimeoutMarker(run); !marked {
			wrote, err := host.ix.MarkManagedRunTimedOut(run.RunID, run.ChildTaskID, map[string]any{
				"limit": limit.text, "limit_ms": limit.limit.Milliseconds(),
				"started_at_ms": started.UnixMilli(), "stopped_at_ms": now.UnixMilli()})
			if err != nil {
				log.Printf("turn deadlines: run %s could not be marked (not stopped this pass): %v", run.RunID, err)
				continue
			}
			if !wrote {
				continue // the run settled, parked, or moved to another child since the read
			}
		}
		if _, err := host.tasks.Interrupt(run.ChildTaskID); err != nil {
			log.Printf("turn deadlines: run %s child %s interrupt failed (retried next pass): %v", run.RunID, run.ChildTaskID, err)
		}
	}
}

// pinnedTurnLimit reads the run's pinned revision's timeout (the child's own
// revision for a delegate run).
func (host *orchestrationManagedHost) pinnedTurnLimit(run store.ManagedRun, limits map[string]pinnedTurnLimit) (pinnedTurnLimit, bool) {
	key := run.ProfileID + "\x00" + run.ProfileSourceDigest + "\x00" + run.ProfileBundleDigest
	if cached, ok := limits[key]; ok {
		return cached, true
	}
	detail, err := host.profiles.GetRevision(run.ProfileID, run.ProfileSourceDigest, run.ProfileBundleDigest)
	if err != nil || detail.Normalized == nil {
		return pinnedTurnLimit{}, false
	}
	limit, err := profilefs.Duration(detail.Normalized.Limits.Timeout)
	if err != nil || limit <= 0 {
		return pinnedTurnLimit{}, false
	}
	limits[key] = pinnedTurnLimit{limit: limit, text: detail.Normalized.Limits.Timeout}
	return limits[key], true
}

// childTurnStart is when the child left the execution queue — its
// `task.started` event (Unix ms) — and whether it is still starting or
// running. A queued child has not started, so it is never overdue: the timeout
// bounds run time, not time spent waiting for a runtime slot.
func (host *orchestrationManagedHost) childTurnStart(childTaskID string) (time.Time, bool) {
	task, found, err := host.tasks.Task(childTaskID)
	if err != nil || !found || (task.Lifecycle != TaskStarting && task.Lifecycle != TaskRunning) {
		return time.Time{}, false
	}
	events, err := host.tasks.Events(childTaskID, 0, 16)
	if err != nil {
		return time.Time{}, false
	}
	for _, event := range events {
		if event.Kind == "task.started" {
			return time.UnixMilli(event.OccurredAt), true
		}
	}
	return time.Time{}, false
}

// settleTimedOutRun ends a run whose child the watcher stopped (failure:
// record-unavailable). A classified provider failure on that child still
// counts toward its route's breaker (plan §4.2, RT-11), so an outage trips
// the circuit instead of launching doomed turns — but the run itself never
// parks: a relaunch would repeat the turn its wall-time bound just ended.
func (host *orchestrationManagedHost) settleTimedOutRun(run store.ManagedRun, event TaskEvent, marker turnTimeout) error {
	if class, providerDetail := host.providerFailureForTask(event.TaskID); class != "" {
		runtime, model := host.attemptRoute(run)
		host.recordProviderFailure(runtime, model, class, providerDetail)
	}
	ran := (time.Duration(marker.StoppedAtMS-marker.StartedAtMS) * time.Millisecond).Round(time.Second)
	recovery := fmt.Sprintf("The helper turn was stopped after %s; its profile timeout is %s. The next signal runs in the same helper session.", ran, marker.Limit)
	return host.ix.CompleteManagedRun(run.RunID, "failed", "", "", nil, run.Detail, "timeout", boundedRecovery(recovery), time.Now().Unix())
}

// attemptRoute is the route the run's current attempt used: the provider
// ledger's newest attempt when it moved routes, else the binding's primary.
func (host *orchestrationManagedHost) attemptRoute(run store.ManagedRun) (string, string) {
	if ledger := providerOutageLedgerFrom(run.Detail); len(ledger.Attempts) > 0 {
		last := ledger.Attempts[len(ledger.Attempts)-1]
		return last.Runtime, last.Model
	}
	binding, found, err := host.ix.ManagedBinding(run.BindingID)
	if err != nil || !found {
		return "", ""
	}
	return binding.Runtime, binding.Model
}
