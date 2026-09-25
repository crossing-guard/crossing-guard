package daemon

import (
	"errors"
	"log"
	"strings"
	"time"

	"crossing-guard/internal/taskinput"
	"crossing-guard/internal/workspace"
)

const (
	taskConsoleScope      = "local-console"
	taskTerminalRetention = 7 * 24 * time.Hour
)

type TaskApplicationService struct {
	repository    taskRepository
	executions    *TaskExecutionRegistry
	subscribers   *TaskSubscriberHub
	deltas        *TaskDeltaBuffer
	runtime       func(string) (ChatDriver, bool)
	catalog       func(string, string) (bool, error)
	launchDataDir string
	inputs        taskInputClaimer
	states        TaskStateMachine
	workspace     taskWorkspace
}

type taskWorkspace interface {
	AcquireForConsumer(string, string, string) (workspace.Candidate, int64, error)
	ReleaseConsumer(string, string, string) error
}

type taskInputClaimer interface {
	Preflight(string, []string) ([]taskinput.ResolvedInput, error)
	Claim(string, []string, string) (taskinput.Claim, error)
	ConsumeTask(string) error
}

type taskRuntimeLookup func(string) (ChatDriver, bool)
type taskCatalogLookup func(string, string) (bool, error)

func NewTaskApplicationService(repository taskRepository, executions *TaskExecutionRegistry, subscribers *TaskSubscriberHub, runtime taskRuntimeLookup, catalog taskCatalogLookup, launchDataDir string) *TaskApplicationService {
	return NewTaskApplicationServiceWithInputs(repository, executions, subscribers, runtime, catalog, nil, launchDataDir)
}

func NewTaskApplicationServiceWithInputs(repository taskRepository, executions *TaskExecutionRegistry, subscribers *TaskSubscriberHub, runtime taskRuntimeLookup, catalog taskCatalogLookup, inputs taskInputClaimer, launchDataDir string) *TaskApplicationService {
	service := &TaskApplicationService{repository: repository, executions: executions,
		subscribers: subscribers, states: TaskStateMachine{}, runtime: runtime, catalog: catalog,
		inputs: inputs, launchDataDir: launchDataDir}
	service.deltas = NewTaskDeltaBuffer(service.appendDelta)
	return service
}

func (s *TaskApplicationService) SetWorkspaceSelectionService(service taskWorkspace) {
	s.workspace = service
}

func registeredTaskRuntime(name string) (ChatDriver, bool) {
	driver, ok := chatDrivers[name]
	return driver, ok
}

func (s *TaskApplicationService) Create(req ChatRequest, idempotencyKey string) (RuntimeTask, bool, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return RuntimeTask{}, false, errors.New("prompt required")
	}
	req.CatalogSessionID = strings.TrimSpace(req.CatalogSessionID)
	if len(req.CatalogSessionID) > 1000 {
		return RuntimeTask{}, false, errors.New("catalog_session_id is too long")
	}
	if len(idempotencyKey) < 8 || len(idempotencyKey) > 200 {
		return RuntimeTask{}, false, errors.New("idempotency_key must be 8-200 characters")
	}
	hasInputScope := strings.TrimSpace(req.InputScopeID) != ""
	hasInputIDs := len(req.InputIDs) > 0
	if hasInputScope != hasInputIDs {
		return RuntimeTask{}, false, errors.New("input_scope_id and input_ids must be provided together")
	}
	if len(req.InputIDs) > 100 {
		return RuntimeTask{}, false, errors.New("too many task input references")
	}
	driver, ok := s.runtime(req.Runtime)
	if !ok {
		return RuntimeTask{}, false, errors.New("unknown runtime")
	}
	if canonicalizer, ok := driver.(chatRequestCanonicalizer); ok {
		var err error
		req, err = canonicalizer.CanonicalizeChatRequest(req)
		if err != nil {
			return RuntimeTask{}, false, err
		}
	}
	if err := validateChatMode(driver, req.Mode); err != nil {
		return RuntimeTask{}, false, err
	}
	if req.WorkspaceSelectionID != "" {
		if strings.TrimSpace(req.Cwd) != "" {
			return RuntimeTask{}, false, errors.New("cwd must be omitted when workspace_selection_id is supplied")
		}
		if s.workspace == nil {
			return RuntimeTask{}, false, errors.New("workspace selection is unavailable")
		}
	} else {
		cwd, err := validateChatCwd(req.Cwd)
		if err != nil {
			return RuntimeTask{}, false, err
		}
		req.Cwd = cwd
	}
	digest := taskRequestDigest(req)
	if existing, found, err := s.repository.ByIdempotency(taskConsoleScope, idempotencyKey); err != nil {
		return RuntimeTask{}, false, err
	} else if found {
		// The store performs the authoritative digest collision check below. This fast
		// path is only for completed retries and therefore verifies the persisted key by
		// attempting the idempotent create with the same digest.
		checked, _, _, err := s.repository.Create(taskCreateRecord{ID: existing.ID,
			ConsoleScope: taskConsoleScope, IdempotencyKey: idempotencyKey,
			RequestDigest: digest, Runtime: req.Runtime, CatalogSessionID: req.CatalogSessionID, NativeSessionID: req.SessionID,
			WorkingDirectory: req.Cwd, CreatedAt: existing.CreatedAt,
			RetentionDeadline: existing.RetentionDeadline})
		return checked, false, err
	}
	var resolvedInputs []taskinput.ResolvedInput
	if hasInputScope {
		if s.inputs == nil {
			return RuntimeTask{}, false, errors.New("task input admission is unavailable")
		}
		preflighted, err := s.inputs.Preflight(req.InputScopeID, req.InputIDs)
		if err != nil {
			return RuntimeTask{}, false, err
		}
		resolvedInputs = preflighted
		validator, ok := driver.(chatInputValidator)
		if !ok {
			return RuntimeTask{}, false, errors.New("selected runtime does not accept task inputs")
		}
		if err := validator.ValidateChatInputs(req, resolvedInputs); err != nil {
			return RuntimeTask{}, false, err
		}
	}
	if req.CatalogSessionID != "" {
		exists, err := s.catalog(req.Runtime, req.CatalogSessionID)
		if err != nil {
			return RuntimeTask{}, false, err
		}
		if !exists {
			return RuntimeTask{}, false, errors.New("catalog_session_id does not identify an existing session for this runtime")
		}
	}

	// Ownership (Part A): a session another process is using right now is
	// refused unless the person said "send anyway". Judged here, where the
	// session identity lives, and again at launch for a queued reservation.
	if !req.AllowSharedSession {
		inUse, err := taskSessionInUse(time.Now().UTC(), req.Runtime, req.CatalogSessionID, req.SessionID)
		if err != nil {
			logSessionStatusFailure(req.Runtime, firstNonEmpty(req.SessionID, req.CatalogSessionID), err)
		} else if inUse {
			return RuntimeTask{}, false, ErrSessionInUse
		}
	}
	reservation, err := s.executions.Reserve(req.Runtime)
	if err != nil {
		return RuntimeTask{}, false, err
	}
	createdAt := time.Now().UnixMilli()
	taskID := newTaskID()
	workspaceVersion := int64(0)
	if req.WorkspaceSelectionID != "" {
		candidate, version, workspaceErr := s.workspace.AcquireForConsumer(req.WorkspaceSelectionID, "task", taskID)
		if workspaceErr != nil {
			s.executions.Release(reservation)
			return RuntimeTask{}, false, workspaceErr
		}
		req.Cwd, workspaceVersion = candidate.Root, version
	}
	record := taskCreateRecord{ID: taskID, ConsoleScope: taskConsoleScope,
		IdempotencyKey: idempotencyKey, RequestDigest: digest, Runtime: req.Runtime,
		CatalogSessionID: req.CatalogSessionID, NativeSessionID: req.SessionID, WorkingDirectory: req.Cwd, CreatedAt: createdAt,
		WorkspaceSelectionID: req.WorkspaceSelectionID, WorkspaceSelectionVersion: workspaceVersion,
		RetentionDeadline: time.UnixMilli(createdAt).Add(taskTerminalRetention).UnixMilli()}
	task, queuedEvent, created, err := s.repository.Create(record)
	if err != nil {
		s.executions.Release(reservation)
		if req.WorkspaceSelectionID != "" {
			_ = s.workspace.ReleaseConsumer(req.WorkspaceSelectionID, "task", taskID)
		}
		return RuntimeTask{}, false, err
	}
	if !created {
		s.executions.Release(reservation)
		if req.WorkspaceSelectionID != "" {
			_ = s.workspace.ReleaseConsumer(req.WorkspaceSelectionID, "task", taskID)
		}
		return task, false, nil
	}
	if hasInputScope {
		claim, claimErr := s.inputs.Claim(req.InputScopeID, req.InputIDs, task.ID)
		if claimErr != nil {
			s.executions.Release(reservation)
			s.finish(task.ID, executionOutcome{Err: claimErr})
			failed, _, _ := s.repository.ByID(task.ID)
			return failed, true, nil
		}
		resolvedInputs = claim.Inputs
	}
	s.subscribers.Publish(queuedEvent)
	if len(resolvedInputs) > 0 {
		items := make([]map[string]any, 0, len(resolvedInputs))
		for _, input := range resolvedInputs {
			items = append(items, map[string]any{"id": input.ID, "name": input.Name,
				"kind": input.Kind, "media_type": input.MediaType, "bytes": input.PreparedBytes})
		}
		_ = s.appendAndPublish(task.ID, "task.inputs", "task-input-owner",
			ChatEvent{"type": "inputs", "items": items}, time.Now().UnixMilli())
	}
	cmd, err := driver.BuildCmd(req, ChatLaunchContext{TaskID: task.ID, DataDir: s.launchDataDir, Inputs: resolvedInputs})
	if err != nil {
		s.executions.Release(reservation)
		s.finish(task.ID, executionOutcome{Err: err})
		failed, _, _ := s.repository.ByID(task.ID)
		return failed, true, nil
	}
	launch := taskExecutionLaunch{taskID: task.ID, runtime: task.Runtime, cmd: cmd, driver: driver}
	launch.started = func() { s.started(task.ID, task.Runtime) }
	launch.event = func(event ChatEvent) { s.ingest(task.ID, task.Runtime, event) }
	launch.done = func(outcome executionOutcome) { s.finish(task.ID, outcome) }
	if !req.AllowSharedSession && (req.SessionID != "" || req.CatalogSessionID != "") {
		// The same ownership rule, asked again when the launch actually
		// happens; a fold failure admits, as at request time. The rule is
		// captured now: the launch goroutine must not read package state later.
		rule := taskSessionInUse
		launch.admit = func() error {
			inUse, err := rule(time.Now().UTC(), req.Runtime, req.CatalogSessionID, req.SessionID)
			if err != nil {
				// Fail open at launch too, and say so: a store outage must not
				// silently switch off half the gate.
				logSessionStatusFailure(req.Runtime, firstNonEmpty(req.SessionID, req.CatalogSessionID), err)
				return nil
			}
			if inUse {
				return ErrSessionInUse
			}
			return nil
		}
	}
	s.executions.Schedule(reservation, launch)
	return task, true, nil
}

func (s *TaskApplicationService) started(taskID, runtime string) {
	now := time.Now().UnixMilli()
	updated, event, changed, err := s.transitionWithEvent(taskID, TaskQueued, TaskStarting,
		now, "", "task.started", runtime+"-owned-stream",
		ChatEvent{"type": "spawn", "text": runtime + " task", "task_id": taskID})
	if err != nil || !changed {
		if err != nil {
			log.Printf("runtime task %s could not persist start: %v", taskID, err)
			_, _ = s.executions.Interrupt(taskID)
		}
		return
	}
	s.subscribers.Publish(event)
	_, _, _ = s.transition(taskID, TaskStarting, TaskRunning, time.Now().UnixMilli(), "")
	// An owned turn starting is the one fully observed "working"; tell the
	// status decider now rather than on the sampler's next pass.
	sessionStatusRefold(updated.Runtime, firstNonEmpty(updated.NativeSessionID, updated.CatalogSessionID))
}

func (s *TaskApplicationService) ingest(taskID, runtime string, event ChatEvent) {
	if anyString(event["type"]) == "session" {
		if nativeID := anyString(event["id"]); nativeID != "" {
			updated, err := s.repository.SetNativeSession(taskID, nativeID, time.Now().UnixMilli())
			if err == nil && updated {
				sessionStatusRefold(runtime, nativeID) // the session this task belongs to is now known
			}
			if err == nil && !updated {
				// The runtime reported a DIFFERENT native session than the one this
				// task was created with (fork after resume). Record the fact durably
				// instead of dropping it; turn anchoring depends on this honesty.
				_ = s.appendAndPublish(taskID, "session.forked", "task-owner",
					map[string]any{"type": "session.forked", "reported_native_session_id": nativeID},
					time.Now().UnixMilli())
			}
		}
	}
	kind := taskEventKind(event)
	if kind == "message.delta" || kind == "reasoning.delta" {
		s.deltas.Add(taskID, runtime, kind, event)
		return
	}
	if err := s.appendAndPublish(taskID, kind, runtime+"-owned-stream", event, time.Now().UnixMilli()); err != nil {
		s.failPersistence(taskID, runtime, err)
	}
}

func (s *TaskApplicationService) finish(taskID string, outcome executionOutcome) {
	if s.inputs != nil {
		defer func() {
			if err := s.inputs.ConsumeTask(taskID); err != nil {
				log.Printf("runtime task %s input cleanup requires recovery: %v", taskID, err)
			}
		}()
	}
	s.deltas.FlushTask(taskID)
	task, found, err := s.repository.ByID(taskID)
	if err != nil || !found || terminalTaskLifecycle(task.Lifecycle) {
		return
	}
	target := TaskCompleted
	errorText := ""
	payload := ChatEvent{"type": "done"}
	if outcome.Interrupted {
		target = TaskInterrupted
		payload["interrupted"] = true
	} else if outcome.Err != nil {
		target = TaskFailed
		errorText = outcome.Err.Error()
		prefix := "process: "
		if errors.Is(outcome.Err, ErrSessionInUse) {
			prefix = "" // the daemon's own refusal, not the vendor process's failure
		}
		payload = ChatEvent{"type": "error", "text": prefix + truncate(errorText, 500)}
	}
	updated, event, changed, err := s.transitionWithEvent(taskID, task.Lifecycle, target,
		time.Now().UnixMilli(), errorText, "task."+string(target),
		task.Runtime+"-owned-stream", payload)
	if err != nil || !changed {
		if err != nil {
			log.Printf("runtime task %s could not persist terminal state: %v", taskID, err)
		}
		return
	}
	s.subscribers.Publish(event)
	// Terminal state is an attention-raising fact; refold the session now.
	sessionStatusRefold(updated.Runtime, firstNonEmpty(updated.NativeSessionID, updated.CatalogSessionID))
	if task.WorkspaceSelectionID != "" && s.workspace != nil {
		if err := s.workspace.ReleaseConsumer(task.WorkspaceSelectionID, "task", task.ID); err != nil {
			log.Printf("runtime task %s could not release workspace lease: %v", task.ID, err)
		}
	}
}

func (s *TaskApplicationService) transition(taskID string, from, to TaskLifecycle, at int64, errorText string) (RuntimeTask, bool, error) {
	if err := s.states.Validate(from, to); err != nil {
		return RuntimeTask{}, false, err
	}
	return s.repository.Transition(taskID, from, to, at, errorText)
}

func (s *TaskApplicationService) transitionWithEvent(taskID string, from, to TaskLifecycle, at int64, errorText, kind, sourceKind string, payload ChatEvent) (RuntimeTask, TaskEvent, bool, error) {
	if err := s.states.Validate(from, to); err != nil {
		return RuntimeTask{}, TaskEvent{}, false, err
	}
	return s.repository.TransitionWithEvent(taskID, from, to, at, errorText,
		kind, sourceKind, map[string]any(payload))
}

func (s *TaskApplicationService) appendAndPublish(taskID, kind, sourceKind string, payload ChatEvent, occurredAt int64) error {
	event, err := s.repository.Append(taskID, kind, sourceKind, map[string]any(payload), occurredAt)
	if err == nil {
		s.subscribers.Publish(event)
	}
	return err
}

func (s *TaskApplicationService) appendDelta(taskID, runtime, kind string, payload ChatEvent) {
	if err := s.appendAndPublish(taskID, kind, runtime+"-owned-stream", payload, time.Now().UnixMilli()); err != nil {
		s.failPersistence(taskID, runtime, err)
	}
}

func (s *TaskApplicationService) failPersistence(taskID, runtime string, cause error) {
	task, found, err := s.repository.ByID(taskID)
	if err != nil || !found || terminalTaskLifecycle(task.Lifecycle) {
		return
	}
	payload := ChatEvent{"type": "error", "text": "Task stopped because live state could not be saved."}
	_, event, changed, transitionErr := s.transitionWithEvent(taskID, task.Lifecycle,
		TaskFailed, time.Now().UnixMilli(), cause.Error(), "task.failed",
		runtime+"-owned-stream", payload)
	if transitionErr != nil {
		log.Printf("runtime task %s persistence failure could not be recorded: event=%v terminal=%v", taskID, cause, transitionErr)
	} else if changed {
		s.subscribers.Publish(event)
	}
	_, _ = s.executions.Interrupt(taskID)
}

func (s *TaskApplicationService) Interrupt(taskID string) (RuntimeTask, error) {
	task, found, err := s.repository.ByID(taskID)
	if err != nil {
		return RuntimeTask{}, err
	}
	if !found {
		return RuntimeTask{}, errors.New("runtime task not found")
	}
	if terminalTaskLifecycle(task.Lifecycle) {
		return task, nil
	}
	if !task.Controllable {
		return RuntimeTask{}, errors.New("runtime task is not controllable")
	}
	interrupted, err := s.executions.Interrupt(taskID)
	if err != nil {
		return RuntimeTask{}, err
	}
	if !interrupted {
		return RuntimeTask{}, errors.New("runtime task control authority is unavailable")
	}
	// Active processes complete the transition from their wait callback. A queued
	// launch invokes that same callback synchronously.
	for range 20 {
		task, _, err = s.repository.ByID(taskID)
		if err != nil || terminalTaskLifecycle(task.Lifecycle) {
			return task, err
		}
		time.Sleep(5 * time.Millisecond)
	}
	return task, nil
}

func (s *TaskApplicationService) Task(id string) (RuntimeTask, bool, error) {
	return s.repository.ByID(id)
}

func (s *TaskApplicationService) Events(taskID string, afterSequence int64, limit int) ([]TaskEvent, error) {
	return s.repository.Events(taskID, afterSequence, limit)
}

// EventsAfter exposes the existing durable global event order to in-process
// consumers. Live subscriber delivery remains a wake-up hint, never cursor truth.
func (s *TaskApplicationService) EventsAfter(afterEventID int64, limit int) ([]TaskEvent, error) {
	return s.repository.EventsAfter(afterEventID, limit)
}

func (s *TaskApplicationService) List(runtime, nativeSessionID string, limit int) ([]RuntimeTask, error) {
	return s.repository.List(runtime, nativeSessionID, limit)
}

func (s *TaskApplicationService) Watermark() (int64, error) { return s.repository.Watermark() }

func (s *TaskApplicationService) PruneExpired(now time.Time) (int64, error) {
	return s.repository.DeleteExpired(now.UnixMilli())
}

// RecoverLostTasks runs before routes are served. A persisted PID is never used to
// reclaim authority; non-terminal work from an earlier daemon is marked unknown.
func (s *TaskApplicationService) RecoverLostTasks() error {
	tasks, err := s.repository.ListActive()
	if err != nil {
		return err
	}
	for _, task := range tasks {
		if terminalTaskLifecycle(task.Lifecycle) {
			continue
		}
		updated, event, changed, err := s.transitionWithEvent(task.ID, task.Lifecycle,
			TaskUnknown, time.Now().UnixMilli(),
			"daemon restarted without runtime-native reattachment proof",
			"task.unknown", task.Runtime+"-owned-stream", ChatEvent{"type": "error",
				"text": "Task control was lost when the daemon restarted."})
		if err != nil {
			return err
		}
		if changed {
			_ = updated
			s.subscribers.Publish(event)
			if task.WorkspaceSelectionID != "" && s.workspace != nil {
				if err := s.workspace.ReleaseConsumer(task.WorkspaceSelectionID, "task", task.ID); err != nil {
					return err
				}
			}
		}
		if s.inputs != nil {
			if err := s.inputs.ConsumeTask(task.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *TaskApplicationService) Subscribe(taskID string, afterSequence int64) (taskSubscription, []TaskEvent, error) {
	subscription := s.subscribers.Subscribe(taskID)
	replay, err := s.repository.Events(taskID, afterSequence, 2000)
	if err != nil {
		s.subscribers.Unsubscribe(subscription.ID)
		return taskSubscription{}, nil, err
	}
	return subscription, replay, nil
}

func (s *TaskApplicationService) SubscribeAll(afterEventID int64) (taskSubscription, []TaskEvent, bool, error) {
	subscription := s.subscribers.Subscribe("")
	replay, err := s.repository.EventsAfter(afterEventID, 2000)
	if err != nil {
		s.subscribers.Unsubscribe(subscription.ID)
		return taskSubscription{}, nil, false, err
	}
	truncated := false
	if len(replay) == 2000 {
		more, moreErr := s.repository.EventsAfter(replay[len(replay)-1].EventID, 1)
		if moreErr != nil {
			s.subscribers.Unsubscribe(subscription.ID)
			return taskSubscription{}, nil, false, moreErr
		}
		truncated = len(more) > 0
	}
	return subscription, replay, truncated, nil
}

func (s *TaskApplicationService) EventRange() (int64, int64, error) {
	return s.repository.EventRange()
}

func (s *TaskApplicationService) Unsubscribe(id uint64) { s.subscribers.Unsubscribe(id) }
