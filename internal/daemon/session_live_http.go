package daemon

// Natural-session live view (natural-session plan, Slice B-GUI). ONE
// authenticated SSE endpoint carries everything the open session's pane needs:
// transcript deltas (new canonical events, by seq), the turn state, the
// governed-action deltas (ingested observation cursor), and an identity frame
// when the session stops being resolvable. No second SSE framework and no
// WebSocket.
//
// Cadence, honestly stated (this comment previously claimed the opposite):
// each open stream POLLS its own session on the configured cadence. LoadSession
// does not ride the scan burst coalescer — that coalescer wraps ScanSessions,
// and a coalescer deduplicates concurrent calls anyway; it cannot notice that a
// file grew. The poll is therefore the update mechanism, its cadence is
// configuration rather than a compiled constant, and its floor is the coalescer
// window because reading faster than that cannot surface anything newer.
//
// One re-read per tick feeds both the delta frame and the state frame, so the
// header can never describe a different moment than the transcript beneath it.
//
// Stream budget (red-team B1): at most one live-session stream per tab — the
// client subscribes only for the selected session and unsubscribes before
// switching. The server cannot enforce per-tab identity; it enforces the
// honest contract: one stream per connection, bounded windows, and a
// daemon-wide cap.

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"crossing-guard/internal/sessionactivity"
)

// Windows, cadences and caps are policy and live in the session-stream
// configuration owner, not here (see session_stream_config.go).

// liveSessionStreams bounds concurrent live-session streams daemon-wide (B1's
// budget made server-enforceable). Registered at subscribe, released on close.
// Sized once from configuration, because a channel's capacity is fixed at
// creation and the cap is a daemon-lifetime property.
var (
	liveSessionStreams     chan struct{}
	liveSessionStreamsOnce sync.Once
)

// liveSessionStreamBudget sizes the cap on first use, after the data
// directory is known — a package-level initializer would resolve the
// configuration before main set the path and read the wrong file.
func liveSessionStreamBudget() chan struct{} {
	liveSessionStreamsOnce.Do(func() {
		liveSessionStreams = make(chan struct{}, sessionStreamConfig().MaxStreams)
	})
	return liveSessionStreams
}

func registerSessionLiveRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/session/live", handleSessionLive)
}

// acquireLiveSessionSlot takes one unit of the daemon-wide live-stream budget.
func acquireLiveSessionSlot() (release func(), ok bool) {
	budget := liveSessionStreamBudget()
	select {
	case budget <- struct{}{}:
		return func() { <-budget }, true
	default:
		return nil, false
	}
}

func handleSessionLive(w http.ResponseWriter, r *http.Request) {
	runtime, id := r.URL.Query().Get("runtime"), r.URL.Query().Get("id")
	if runtime == "" || id == "" {
		http.Error(w, "runtime and id are required", http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	release, ok := acquireLiveSessionSlot()
	if !ok {
		http.Error(w, "live session stream budget exhausted", http.StatusServiceUnavailable)
		return
	}
	defer release()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	emit := func(event string, payload any) error {
		return writeLiveSSE(w, flusher, event, time.Now().Unix(), payload)
	}
	keepalive := time.NewTicker(sessionStreamConfig().Keepalive())
	defer keepalive.Stop()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-keepalive.C:
				if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
					cancel()
					return
				}
				flusher.Flush()
			}
		}
	}()
	_ = sessionLiveProducer(ctx, runtime, id, emit)
	cancel()
	<-done
}

// sessionLiveProducer is the ONE owner of the live view's frames: the bounded
// snapshot, the state frame, then transcript/state/governance deltas on the
// configured poll until the context ends or the session stops resolving. It
// returns nil when the feed is over; the caller owns the transport.
func sessionLiveProducer(ctx context.Context, runtime, id string, emit func(event string, payload any) error) error {
	// Snapshot: the bounded newest window, exactly like the open view's
	// initial render. A missing session is an honest terminal event, never a
	// silent empty stream pretending to be live.
	detail, err := LoadSession(runtime, id)
	if err != nil || detail == nil {
		// The terminal unavailable event is best-effort: the client is the
		// party that may already be gone, and there is nothing after it.
		_ = emit("unavailable", map[string]string{"error": "session unavailable"})
		return nil
	}
	config := sessionStreamConfig()
	lastSeq := 0
	snapshot := map[string]any{
		"runtime": detail.Runtime, "id": detail.ID,
		"events": []liveEvent{}, "total_events": 0, "hidden": 0,
		// Client pacing is policy too; the browser must not compile it in.
		"backoff_cap_seconds": config.ClientBackoffCapSeconds,
		"age_tick_seconds":    config.ClientAgeTickSeconds,
	}
	if n := len(detail.Events); n > 0 {
		first := 0
		if n > config.SnapshotEvents {
			first = n - config.SnapshotEvents
		}
		window := annotateThoughts(detail.Events, config.MinThought().Milliseconds())[first:]
		snapshot["events"], snapshot["total_events"], snapshot["hidden"] = window, n, first
		lastSeq = window[len(window)-1].Seq
	}
	if err := emit("snapshot", snapshot); err != nil {
		return err
	}
	// The status frame rides the same stream so the header can never disagree
	// with the rail: both render the ONE decider's item for this session.
	state := liveSessionStatusItem(runtime, id, detail)
	if err := emit("state", state); err != nil {
		return err
	}
	governanceCursor := initialGovernanceCursor(runtime, id)
	poll := time.NewTicker(config.Poll())
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-poll.C:
			// One re-read per tick serves the transcript deltas AND the state
			// frame, so the two can never describe different moments.
			current, currentErr := LoadSession(runtime, id)
			if currentErr != nil || current == nil {
				// The session we were asked for is no longer resolvable. Say
				// that and stop; never report it as "ended", which would claim
				// a completion nobody observed.
				_ = emit("identity", map[string]any{"following": false})
				return nil
			}
			deltas, newLast := deltasAfter(annotateThoughts(current.Events, config.MinThought().Milliseconds()), lastSeq)
			if len(deltas) > 0 {
				// New rows only, so each row is classified once, when first sent.
				addTranscriptFacts(deltas)
				if err := emit("events", map[string]any{"events": deltas}); err != nil {
					return err
				}
				lastSeq = newLast
			}
			next := liveSessionStatusItem(runtime, id, current)
			if !sameSessionStatus(next, state) {
				if err := emit("state", next); err != nil {
					return err
				}
				state = next
			}
			actions, newCursor := governanceDeltas(runtime, id, governanceCursor)
			if len(actions) > 0 {
				if err := emit("governance", map[string]any{"actions": actions}); err != nil {
					return err
				}
				governanceCursor = newCursor
			}
		}
	}
}

// deltasAfter returns the events beyond lastSeq from an already-loaded
// transcript. It takes the events rather than re-reading so one poll tick
// produces one read, shared by the delta and state frames.
func deltasAfter(events []liveEvent, lastSeq int) ([]liveEvent, int) {
	var out []liveEvent
	for _, event := range events {
		if event.Seq > lastSeq {
			out = append(out, event)
		}
	}
	window := sessionStreamConfig().SnapshotEvents
	if n := len(out); n > window {
		out = out[n-window:]
	}
	if len(out) > 0 {
		return out, out[len(out)-1].Seq
	}
	return nil, lastSeq
}

// liveSessionStatusItem folds the selected session through the one decider.
// The pane's header and the rail's dot therefore read the same frame.
func liveSessionStatusItem(runtime, id string, detail *SessionDetail) sessionactivity.Item {
	now := time.Now().UTC()
	nativeID := detail.ResumeID
	if nativeID == "" {
		nativeID = id
	}
	item := sessionactivity.Item{Runtime: runtime, CatalogSessionID: id, NativeSessionID: nativeID,
		Presence: "unknown", Execution: "unknown", Evidence: "none", Freshness: "unknown",
		Authority: "none", ObservedAt: now}
	if sessionActivityService() != nil {
		for _, candidate := range sessionActivityService().Snapshot().Items {
			if candidate.Runtime == runtime && candidate.CatalogSessionID == id {
				item = candidate
				break
			}
		}
	}
	frame, err := foldSessionStatus(now, runtime, id, nativeID)
	if err != nil {
		logSessionStatusFailure(runtime, nativeID, err)
	}
	// Turn progress rides the same frame, for this open session only: the
	// transcript was read once for this tick and the boundaries come from it.
	config := sessionStreamConfig()
	progress := decideTurnProgress(frame.Execution, frame.Attention,
		turnBoundaries(detail.Events, config.ProgressWindowRecords), now, config.Quiet())
	frame.Progress, frame.ProgressTool = progress.Progress, progress.Tool
	return applySessionStatus(item, frame)
}

func sameSessionStatus(a, b sessionactivity.Item) bool {
	return a.Execution == b.Execution && a.Attention == b.Attention &&
		a.AttentionID == b.AttentionID && a.AttentionSource == b.AttentionSource && a.SinceMS == b.SinceMS &&
		a.Progress == b.Progress && a.ProgressTool == b.ProgressTool
}

// liveGovernedAction is the bounded projection of one ingested observation
// for the open session — the governor-firing visibility the owner asked for.
type liveGovernedAction struct {
	EventID  int64  `json:"event_id"`
	Tool     string `json:"tool,omitempty"`
	Decision string `json:"decision,omitempty"`
	Reason   string `json:"reason,omitempty"`
	TS       int64  `json:"ts"`
}

// initialGovernanceCursor seeds the observation cursor: everything already
// recorded is history (the panels render it at open); only new rows stream.
func initialGovernanceCursor(runtime, id string) int64 {
	if governor == nil || governor.ix == nil {
		return 0
	}
	cursor, err := governor.ix.SessionGovernanceCursor(runtime, id)
	if err != nil {
		return 0
	}
	return cursor
}

// governanceDeltas reads ingested observation rows for the session after the
// cursor, bounded; the cursor is the row's event id (durable, monotonic).
func governanceDeltas(runtime, id string, cursor int64) ([]liveGovernedAction, int64) {
	if governor == nil || governor.ix == nil {
		return nil, cursor
	}
	rows, newCursor, err := governor.ix.SessionGovernanceAfter(runtime, id, cursor, sessionStreamConfig().GovernanceWindow)
	if err != nil || len(rows) == 0 {
		return nil, cursor
	}
	out := make([]liveGovernedAction, 0, len(rows))
	for _, row := range rows {
		out = append(out, liveGovernedAction{EventID: row.ID, Tool: row.Tool,
			Decision: row.Decision, Reason: row.Reason, TS: row.TS})
	}
	return out, newCursor
}

// writeLiveSSE writes one named SSE event with a JSON payload. The shared
// writeJSONSSE (task_http.go) fits task events; this local writer keeps the
// live view's payload shapes independent.
func writeLiveSSE(w http.ResponseWriter, flusher http.Flusher, event string, generation int64, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte("event: " + event + "\ndata: " + string(body) + "\n\n")); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}
