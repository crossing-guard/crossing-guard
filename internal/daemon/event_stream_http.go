package daemon

// GET /api/events/stream — the ONE long-lived connection a console tab holds.
//
// Why: a browser allows six connections per origin, shared across every tab of
// the profile. A tab that held four streams (approvals, runtime tasks, session
// activity, live session) plus a presence heartbeat starved its own reads the
// moment a second tab existed — measured 2026-09-04 as "Opening claude
// session…" for 20 s while curl answered in under a second. One socket per tab
// leaves the pool for reads.
//
// Contract (implementation plan §2.1): every frame is an envelope whose
// payload is the feed's legacy body verbatim; per-feed cursors travel in the
// request; a reset for one feed keeps its meaning and ends the connection —
// but only after every feed has opened, so no connection ever closes with
// another feed's opening picture unsent (event-stream-reset-opening-barrier-
// plan §2). The legacy routes remain for clients that want one feed.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

func registerEventStreamRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/events/stream", handleEventStream)
}

// eventStreamRequest is the parsed subscribe request.
type eventStreamRequest struct {
	tasksAfter    int64
	activityAfter int64
	runtime, id   string
	clientID      string
}

func parseEventStreamRequest(r *http.Request) (eventStreamRequest, error) {
	q := r.URL.Query()
	var req eventStreamRequest
	var err error
	if req.tasksAfter, err = parseFeedCursor(q.Get("tasks")); err != nil {
		return req, fmt.Errorf("tasks %w", err)
	}
	if req.activityAfter, err = parseFeedCursor(q.Get("activity")); err != nil {
		return req, fmt.Errorf("activity %w", err)
	}
	req.runtime, req.id = q.Get("runtime"), q.Get("id")
	if (req.runtime == "") != (req.id == "") {
		return req, errors.New("runtime and id must be given together")
	}
	req.clientID = strings.TrimSpace(q.Get("client_id"))
	if req.clientID != "" && !validPresenceClientID(req.clientID) {
		return req, errors.New("invalid client_id")
	}
	return req, nil
}

// parseFeedCursor accepts an absent cursor as zero, matching each legacy
// route's meaning of "from the start".
func parseFeedCursor(raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	after, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || after < 0 {
		return 0, errors.New("cursor must be a non-negative integer")
	}
	return after, nil
}

// eventStreamHello is the first frame: the pacing the client must not compile in.
type eventStreamHello struct {
	KeepaliveSeconds  int `json:"keepalive_seconds"`
	BackoffCapSeconds int `json:"backoff_cap_seconds"`
	AgeTickSeconds    int `json:"age_tick_seconds"`
}

func handleEventStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	req, err := parseEventStreamRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	if req.clientID != "" {
		approvalAttention.Attach(req.clientID)
		defer approvalAttention.Detach(req.clientID)
	}
	config := sessionStreamConfig()
	writer := &feedFrameWriter{w: w, flusher: flusher}
	if err := writer.writeJSON(feedStream, "hello", eventStreamHello{
		KeepaliveSeconds: config.KeepaliveSeconds, BackoffCapSeconds: config.ClientBackoffCapSeconds,
		AgeTickSeconds: config.ClientAgeTickSeconds}); err != nil {
		return
	}
	runEventStream(r.Context(), eventStreamProducers(req), writer, config)
}

// eventStreamProducers is the ONE list of feeds a connection carries.
func eventStreamProducers(req eventStreamRequest) []feedProducer {
	return []feedProducer{
		approvalFeed,
		func(ctx context.Context, emit feedEmitter, opened func()) error {
			return taskFeed(ctx, req.tasksAfter, emit, opened)
		},
		func(ctx context.Context, emit feedEmitter, opened func()) error {
			return activityFeed(ctx, req.activityAfter, emit, opened)
		},
		func(ctx context.Context, emit feedEmitter, opened func()) error {
			return sessionFeed(ctx, req.runtime, req.id, emit, opened)
		},
	}
}

// eventStreamFrameBuffer bounds how far producers may run ahead of the socket.
const eventStreamFrameBuffer = 64

// runEventStream fans the producers into one response. A restart-class end
// (errFeedRestart) closes the connection only once every producer has opened:
// each producer sends its opening frames before its opened signal, so by the
// time the last signal is received those frames are written or buffered, and
// the drain writes the rest. Pinned by
// TestEventStreamSlowOpenerIsFlushedBeforeAFastReset and the per-producer order
// tests in event_stream_http_test.go. Frames emitted after a feed opened stay
// best-effort on a close, as before.
func runEventStream(parent context.Context, producers []feedProducer, writer *feedFrameWriter, config SessionStreamConfig) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	frames := make(chan feedFrame, eventStreamFrameBuffer)
	emit := func(frame feedFrame) error {
		select {
		case frames <- frame:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	var group sync.WaitGroup
	ended := make(chan error, len(producers))
	openings := make(chan struct{}, len(producers))
	for _, run := range producers {
		startFeedProducer(ctx, &group, run, emit, ended, openings)
	}
	keepalive := time.NewTicker(config.Keepalive())
	defer keepalive.Stop()
	defer func() { cancel(); group.Wait() }()
	unopened, closing := len(producers), false
	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			if err := writer.keepalive(); err != nil {
				return
			}
		case frame := <-frames:
			if err := writer.write(frame); err != nil {
				return
			}
		case <-openings:
			unopened--
			if closing && unopened == 0 {
				writer.drain(frames)
				return
			}
		case err := <-ended:
			if err == nil || errors.Is(err, context.Canceled) {
				continue // a quiet feed end (live session finished) keeps the connection
			}
			// A feed that needs a reconnect: keep serving until every feed has
			// opened, then flush and close so the client reconnects with its cursors.
			closing = true
			if unopened == 0 {
				writer.drain(frames)
				return
			}
		}
	}
}

// startFeedProducer runs one producer and reports its opening and its end. It
// sends once on each channel, so the caller must size both to len(producers)
// (runEventStream does) or group.Wait in the caller's defer can leak. The
// producer's return counts as opened: a quiet end, an unavailable frame, a
// failed emit, or a restart holds nothing back from a close.
func startFeedProducer(ctx context.Context, group *sync.WaitGroup, run feedProducer, emit feedEmitter,
	ended chan<- error, openings chan<- struct{}) {
	group.Add(1)
	var once sync.Once
	opened := func() { once.Do(func() { openings <- struct{}{} }) }
	go func() {
		defer group.Done()
		err := run(ctx, emit, opened)
		opened()
		ended <- err
	}()
}

// feedFrameWriter serializes every write to one response.
type feedFrameWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	seq     int64
}

func (f *feedFrameWriter) write(frame feedFrame) error {
	f.seq++
	if _, err := fmt.Fprintf(f.w, "id: %d\nevent: %s\ndata: ", f.seq, frame.Feed); err != nil {
		return err
	}
	if err := writeFeedFrameBody(f.w, frame); err != nil {
		return err
	}
	f.flusher.Flush()
	return nil
}

// writeFeedFrameBody writes the envelope and the frame terminator.
func writeFeedFrameBody(w http.ResponseWriter, frame feedFrame) error {
	body, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	_, err = w.Write(append(body, '\n', '\n'))
	return err
}

func (f *feedFrameWriter) writeJSON(feed, event string, payload any) error {
	return emitJSON(f.write, feed, event, 0, payload)
}

func (f *feedFrameWriter) keepalive() error {
	if _, err := fmt.Fprint(f.w, ": keepalive\n\n"); err != nil {
		return err
	}
	f.flusher.Flush()
	return nil
}

func (f *feedFrameWriter) drain(frames <-chan feedFrame) {
	for {
		select {
		case frame := <-frames:
			if err := f.write(frame); err != nil {
				return
			}
		default:
			return
		}
	}
}
