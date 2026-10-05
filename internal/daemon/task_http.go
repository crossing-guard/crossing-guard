package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"crossing-guard/internal/taskinput"
	"crossing-guard/store"
)

var runtimeTasks *TaskApplicationService
var runtimeTaskIndex *store.Index

// runtimeTasksProblem says why the task service never opened (the
// taskInputsProblem shape). Written once by Main before the listener exists and
// only read afterwards; closing the service does not clear it.
var runtimeTasksProblem string

// runtimeTasksUnavailable is the 503 text for a route that needs the task
// service: its subject plus the start-up reason, so the console pill and the
// composer say why (degraded-surfaces-state-the-reason plan §2.1).
func runtimeTasksUnavailable(subject string) string {
	if runtimeTasksProblem == "" {
		return subject
	}
	return subject + ": " + runtimeTasksProblem
}

func initRuntimeTaskService(path string, workspace taskWorkspace) error {
	index, err := store.Open(path)
	if err != nil {
		return err
	}
	service := NewTaskApplicationServiceWithInputs(taskStoreRepository{index: index},
		NewTaskExecutionRegistry(), NewTaskSubscriberHub(), registeredTaskRuntime, catalogSessionExists,
		configuredTaskInputs(), filepath.Dir(path))
	service.SetWorkspaceSelectionService(workspace)
	if _, err := service.PruneExpired(time.Now()); err != nil {
		index.Close()
		return err
	}
	if err := service.RecoverLostTasks(); err != nil {
		index.Close()
		return err
	}
	runtimeTaskIndex = index
	runtimeTasks = service
	return nil
}

func closeRuntimeTaskService() {
	if runtimeTaskIndex != nil {
		_ = runtimeTaskIndex.Close()
	}
	runtimeTaskIndex = nil
	runtimeTasks = nil
}

// runtimeTaskRefusal is a typed refusal of a task create: a data code the browser
// branches on, and a sentence.
type runtimeTaskRefusal struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func handleRuntimeTaskCreate(w http.ResponseWriter, r *http.Request) {
	if runtimeTasks == nil {
		http.Error(w, runtimeTasksUnavailable("runtime task service unavailable"), http.StatusServiceUnavailable)
		return
	}
	var req ChatRequest
	if err := decodeManagedJSON(w, r, &req, 0); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	key := req.IdempotencyKey
	if key == "" {
		key = r.Header.Get("Idempotency-Key")
	}
	task, created, err := runtimeTasks.Create(req, key)
	if err != nil {
		if writeEffortError(w, err) {
			return
		}
		var inputErr *taskinput.Error
		if errors.As(err, &inputErr) {
			writeTaskInputServiceError(w, err)
			return
		}
		var handoffErr *handoffLaunchError
		if errors.As(err, &handoffErr) {
			// The same typed shape as session_in_use: the composer branches on the
			// code ("the sender withdrew this handoff" is handoff_withdrawn).
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(runtimeTaskRefusal{Code: handoffErr.Code, Message: handoffErr.Message})
			return
		}
		if errors.Is(err, ErrSessionInUse) {
			// A typed refusal: the browser branches on the code and shows the
			// message; the sentence is never the discriminator.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"code": "session_in_use", "message": err.Error()})
			return
		}
		status := http.StatusBadRequest
		if errors.Is(err, ErrTaskAdmissionBusy) {
			status = http.StatusTooManyRequests
		} else if runtimeTaskIdempotencyConflict(err) {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	if created {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
	}
	writeJSON(w, task)
}

// handleRuntimeTaskList is the console's opening task snapshot. Its only 503 is
// the nil-service branch, which lasts for the life of the process: the event
// stream client relies on that to open the stream without the snapshot instead
// of retrying it (degraded-surfaces-state-the-reason plan §2.2). Any transient
// failure added here must not be a 503.
func handleRuntimeTaskList(w http.ResponseWriter, r *http.Request) {
	if runtimeTasks == nil {
		http.Error(w, runtimeTasksUnavailable("runtime task service unavailable"), http.StatusServiceUnavailable)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	// Capture the replay boundary BEFORE reading projections. Events committed after
	// this point may already appear in the snapshot, but they are replayed after the
	// earlier watermark too; the browser's task sequence reducer removes that duplicate.
	// Capturing the watermark last would create an unobservable interval.
	watermark, err := runtimeTasks.Watermark()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tasks, err := runtimeTasks.List(r.URL.Query().Get("session_runtime"),
		r.URL.Query().Get("native_session_id"), limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for index := range tasks {
		after := tasks[index].LastSequence - 512
		if after < 0 {
			after = 0
		}
		tasks[index].Events, err = runtimeTasks.Events(tasks[index].ID, after, 512)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, map[string]any{"tasks": tasks, "through_event_id": watermark})
}

func handleRuntimeTaskInterrupt(w http.ResponseWriter, r *http.Request) {
	if runtimeTasks == nil {
		http.Error(w, runtimeTasksUnavailable("runtime task service unavailable"), http.StatusServiceUnavailable)
		return
	}
	id := strings.TrimSpace(r.PathValue("task"))
	if id == "" {
		http.Error(w, "task required", http.StatusBadRequest)
		return
	}
	task, err := runtimeTasks.Interrupt(id)
	if err != nil {
		status := http.StatusConflict
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, task)
}

func handleRuntimeTaskStream(w http.ResponseWriter, r *http.Request) {
	if runtimeTasks == nil {
		http.Error(w, runtimeTasksUnavailable("runtime task service unavailable"), http.StatusServiceUnavailable)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	after, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if err != nil || after < 0 {
		http.Error(w, "after must be a non-negative event id", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	start, err := taskFeedOpen(after)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if start.reset != nil {
		writeJSONSSE(w, flusher, "reset", start.resetCursor, start.reset)
		return
	}
	defer runtimeTasks.Unsubscribe(start.subscription.ID)
	forward := taskForwarder(after)
	for _, event := range start.replay {
		if forward(event) {
			writeJSONSSE(w, flusher, "task", event.EventID, event)
		}
	}
	keepalive := time.NewTicker(taskLegacyKeepalive)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case event, ok := <-start.subscription.Events:
			if !ok {
				return
			}
			if forward(event) {
				writeJSONSSE(w, flusher, "task", event.EventID, event)
			}
		}
	}
}

// taskLegacyKeepalive is the single-feed route's comment cadence; the
// multiplexed route reads its cadence from the session-stream configuration.
const taskLegacyKeepalive = 15 * time.Second

func writeJSONSSE(w http.ResponseWriter, flusher http.Flusher, eventName string, eventID int64, value any) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return
	}
	if eventID > 0 {
		fmt.Fprintf(w, "id: %d\n", eventID)
	}
	if eventName != "" {
		fmt.Fprintf(w, "event: %s\n", eventName)
	}
	fmt.Fprintf(w, "data: %s\n\n", encoded)
	flusher.Flush()
}
