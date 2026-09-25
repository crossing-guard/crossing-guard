package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// eventStreamTestDeadline bounds every wait so a wedged stream fails the test
// instead of hanging the suite.
const eventStreamTestDeadline = 2 * time.Second

// readEnvelopes parses every SSE frame in a recorded body into its envelope.
func readEnvelopes(t *testing.T, body string) []feedFrame {
	t.Helper()
	var frames []feedFrame
	for _, block := range strings.Split(body, "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var frame feedFrame
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame); err != nil {
				t.Fatalf("envelope %q: %v", line, err)
			}
			frames = append(frames, frame)
		}
	}
	return frames
}

func recordEventStream(t *testing.T, query string, hold time.Duration) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest("GET", "/api/events/stream"+query, nil).WithContext(ctx)
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { handleEventStream(recorder, request); close(done) }()
	time.Sleep(hold)
	cancel()
	select {
	case <-done:
	case <-time.After(eventStreamTestDeadline):
		t.Fatal("event stream did not end after its context was cancelled")
	}
	return recorder.Body.String()
}

func TestEventStreamStartsWithHelloThenEveryFeedSnapshot(t *testing.T) {
	repository := &taskHTTPRepositoryStub{events: []TaskEvent{{SchemaVersion: 1, EventID: 1, TaskID: "task",
		Runtime: "fixture", Sequence: 1, Kind: "task.started", Payload: map[string]any{"type": "spawn"}}},
		minimum: 1, maximum: 1}
	installTaskHTTPStub(t, repository)
	installTestSessionActivity(t)
	body := recordEventStream(t, "?tasks=0&activity=0", 30*time.Millisecond)
	frames := readEnvelopes(t, body)
	if len(frames) < 3 || frames[0].Feed != feedStream || frames[0].Event != "hello" {
		t.Fatalf("first frame must be the pacing hello: %q", body)
	}
	var hello eventStreamHello
	if err := json.Unmarshal(frames[0].Payload, &hello); err != nil || hello.KeepaliveSeconds <= 0 || hello.BackoffCapSeconds <= 0 {
		t.Fatalf("hello must carry configured pacing: %s", frames[0].Payload)
	}
	seen := map[string]string{}
	for _, frame := range frames[1:] {
		if _, ok := seen[frame.Feed]; !ok {
			seen[frame.Feed] = frame.Event
		}
	}
	if seen[feedApprovals] != "snapshot" {
		t.Errorf("approvals feed must open with its snapshot: %v", seen)
	}
	if seen[feedTasks] != "task" {
		t.Errorf("tasks feed must replay from the cursor: %v", seen)
	}
	if seen[feedActivity] != "activity" {
		t.Errorf("activity feed must publish the current generation: %v", seen)
	}
	if _, ok := seen[feedSession]; ok {
		t.Errorf("no subject was given, so no session frames may appear: %v", seen)
	}
	if !strings.Contains(body, "event: "+feedTasks+"\n") {
		t.Errorf("the SSE event name is the feed: %q", body)
	}
}

func TestEventStreamPayloadsAreByteIdenticalToLegacyRoutes(t *testing.T) {
	event := TaskEvent{SchemaVersion: 1, EventID: 3, TaskID: "task", Runtime: "fixture", Sequence: 3,
		Kind: "task.activity", SourceKind: "fixture", EvidenceClass: "observed", Freshness: "live",
		Payload: map[string]any{"type": "delta", "text": "héllo"}}
	repository := &taskHTTPRepositoryStub{events: []TaskEvent{event}, minimum: 3, maximum: 3}
	installTaskHTTPStub(t, repository)
	installTestSessionActivity(t)

	legacy := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		handleRuntimeTaskStream(legacy, httptest.NewRequest("GET", "/api/runtime-tasks/stream?after=0", nil).WithContext(ctx))
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done
	var legacyTask string
	for _, line := range strings.Split(legacy.Body.String(), "\n") {
		if strings.HasPrefix(line, "data: ") {
			legacyTask = strings.TrimPrefix(line, "data: ")
		}
	}
	if legacyTask == "" {
		t.Fatalf("legacy route produced no task frame: %q", legacy.Body.String())
	}
	muxed := ""
	for _, frame := range readEnvelopes(t, recordEventStream(t, "?tasks=0&activity=0", 30*time.Millisecond)) {
		if frame.Feed == feedTasks && frame.Event == "task" {
			muxed = string(frame.Payload)
		}
	}
	if muxed != legacyTask {
		t.Fatalf("task payload differs between routes:\nlegacy %s\nmuxed  %s", legacyTask, muxed)
	}
	legacyActivity := httptest.NewRecorder()
	handleSessionActivityList(legacyActivity, httptest.NewRequest("GET", "/api/session-activity", nil))
	var muxedActivity string
	for _, frame := range readEnvelopes(t, recordEventStream(t, "?tasks=0&activity=0", 30*time.Millisecond)) {
		if frame.Feed == feedActivity {
			muxedActivity = string(frame.Payload)
		}
	}
	if strings.TrimSpace(legacyActivity.Body.String()) != muxedActivity {
		t.Fatalf("activity payload differs from the snapshot route:\nlegacy %s\nmuxed  %s", legacyActivity.Body.String(), muxedActivity)
	}
}

func TestEventStreamResetForOneFeedEndsTheConnectionAfterFlushingIt(t *testing.T) {
	repository := &taskHTTPRepositoryStub{minimum: 1, maximum: 2}
	installTaskHTTPStub(t, repository)
	installTestSessionActivity(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest("GET", "/api/events/stream?tasks=99&activity=0", nil).WithContext(ctx)
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { handleEventStream(recorder, request); close(done) }()
	select {
	case <-done:
	case <-time.After(eventStreamTestDeadline):
		t.Fatal("a task reset must end the connection so the client reconnects with its cursor")
	}
	frames := readEnvelopes(t, recorder.Body.String())
	var reset *feedFrame
	for i := range frames {
		if frames[i].Feed == feedTasks && frames[i].Event == "reset" {
			reset = &frames[i]
		}
	}
	if reset == nil || reset.Cursor != 2 || !strings.Contains(string(reset.Payload), `"through_event_id":2`) {
		t.Fatalf("reset frame missing or wrong: %q", recorder.Body.String())
	}
	for _, frame := range frames {
		if frame.Feed == feedApprovals && frame.Event == "snapshot" {
			return // the other feeds still delivered before the close
		}
	}
	t.Fatalf("approvals snapshot was not flushed before the reset close: %q", recorder.Body.String())
}

func TestEventStreamRejectsMalformedRequests(t *testing.T) {
	for _, query := range []string{"?tasks=-1", "?activity=x", "?runtime=claude", "?client_id=short"} {
		recorder := httptest.NewRecorder()
		handleEventStream(recorder, httptest.NewRequest("GET", "/api/events/stream"+query, nil))
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s: status=%d, want 400", query, recorder.Code)
		}
	}
}

func TestEventStreamHoldsThePresenceLeaseForTheConnection(t *testing.T) {
	installTaskHTTPStub(t, &taskHTTPRepositoryStub{})
	installTestSessionActivity(t)
	base := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	clock := base
	router := newApprovalAttentionRouter(func(string) {})
	router.now = func() time.Time { return clock }
	previous := approvalAttention
	approvalAttention = router
	t.Cleanup(func() { approvalAttention = previous })

	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest("GET", "/api/events/stream?client_id=tab_lease_1", nil).WithContext(ctx)
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { handleEventStream(recorder, request); close(done) }()
	time.Sleep(20 * time.Millisecond)
	router.Update("tab_lease_1", true, true)
	clock = base.Add(10 * approvalPresenceLease)
	if !router.hasActiveViewer() {
		t.Fatal("an attached client must not expire by time while its stream is open")
	}
	cancel()
	<-done
	clock = clock.Add(approvalPresenceLease + time.Second)
	if router.hasActiveViewer() {
		t.Fatal("after the stream closes the last reported state must age out on the fallback lease")
	}
}

func TestLegacyTaskStreamStillResetsAndForwards(t *testing.T) {
	repository := &taskHTTPRepositoryStub{minimum: 1, maximum: 2}
	installTaskHTTPStub(t, repository)
	recorder := httptest.NewRecorder()
	handleRuntimeTaskStream(recorder, httptest.NewRequest("GET", "/api/runtime-tasks/stream?after=99", nil))
	if body := recorder.Body.String(); !strings.Contains(body, "event: reset") {
		t.Fatalf("legacy task route lost its reset: %q", body)
	}
}

func TestLegacyApprovalStreamStillOpensWithSnapshot(t *testing.T) {
	srv := approvalsServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/approvals/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "event: snapshot") {
		t.Fatalf("legacy approvals route must open with its snapshot: %q %v", line, err)
	}
}

// ---- the opening barrier (event-stream-reset-opening-barrier-plan §4) -------
//
// Pins: the per-producer order tests cannot compile on 5d63b93 (no opened
// signal), and TestEventStreamSlowOpenerIsFlushedBeforeAFastReset fails against
// the old loop logic (the loop returned on the first restart and the drain found
// nothing; shown with a go test -overlay mutant in the verification record).
// Guards: the three tests after it pass on the old logic too and exist so the
// barrier can never over-wait.

// feedRecorder records the order in which one producer emits and opens. Like
// the multiplexer, it counts only the first opened call.
type feedRecorder struct {
	log    []string
	opened chan struct{}
	once   sync.Once
}

func newFeedRecorder() *feedRecorder { return &feedRecorder{opened: make(chan struct{})} }

func (r *feedRecorder) emit(frame feedFrame) error {
	r.log = append(r.log, frame.Feed+"/"+frame.Event)
	return nil
}

func (r *feedRecorder) open() {
	r.once.Do(func() { r.log = append(r.log, "opened"); close(r.opened) })
}

// runProducerToOpening runs one producer until it opens or returns, cancels
// it, and returns what it recorded. The log is read only after the producer
// returned, so the race detector sees the ordering it needs.
func runProducerToOpening(t *testing.T, run feedProducer) (string, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recorder := newFeedRecorder()
	result := make(chan error, 1)
	go func() { result <- run(ctx, recorder.emit, recorder.open) }()
	select {
	case <-recorder.opened:
		cancel()
	case err := <-result:
		return strings.Join(recorder.log, " "), err
	case <-time.After(eventStreamTestDeadline):
		t.Fatal("producer neither opened nor returned")
	}
	select {
	case err := <-result:
		return strings.Join(recorder.log, " "), err
	case <-time.After(eventStreamTestDeadline):
		t.Fatal("producer did not end after its context was cancelled")
	}
	return "", nil
}

func TestApprovalFeedOpensAfterItsSnapshot(t *testing.T) {
	previous := approvals
	approvals = newApprovalsHub()
	t.Cleanup(func() { approvals = previous })
	log, err := runProducerToOpening(t, approvalFeed)
	if log != "approvals/snapshot opened" || !errors.Is(err, context.Canceled) {
		t.Fatalf("approvals must open right after its snapshot: %q %v", log, err)
	}
}

func TestTaskFeedOpensOnItsFirstFrameOrAnEmptyReplay(t *testing.T) {
	events := []TaskEvent{
		{SchemaVersion: 1, EventID: 1, TaskID: "task", Runtime: "fixture", Sequence: 1, Kind: "task.started"},
		{SchemaVersion: 1, EventID: 2, TaskID: "task", Runtime: "fixture", Sequence: 2, Kind: "task.activity"},
	}
	cases := []struct {
		name             string
		events           []TaskEvent
		minimum, maximum int64
		after            int64
		want             string
		err              error
	}{
		{"a stale cursor resets and returns without opening", nil, 1, 2, 99, "tasks/reset", errFeedRestart},
		{"the first replay frame opens, the rest follow", events, 1, 2, 0, "tasks/task opened tasks/task", context.Canceled},
		{"an empty replay opens with no frame", nil, 0, 0, 0, "opened", context.Canceled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			installTaskHTTPStub(t, &taskHTTPRepositoryStub{events: tc.events, minimum: tc.minimum, maximum: tc.maximum})
			log, err := runProducerToOpening(t, func(ctx context.Context, emit feedEmitter, opened func()) error {
				return taskFeed(ctx, tc.after, emit, opened)
			})
			if log != tc.want || !errors.Is(err, tc.err) {
				t.Fatalf("got %q %v, want %q %v", log, err, tc.want, tc.err)
			}
		})
	}
}

// waitForActivityGeneration returns the service's generation once its first
// refresh has landed; the fixture refreshes hourly after that, so the value
// is stable for the test.
func waitForActivityGeneration(t *testing.T) int64 {
	t.Helper()
	deadline := time.Now().Add(eventStreamTestDeadline)
	for time.Now().Before(deadline) {
		if generation := sessionActivityService().Snapshot().Generation; generation >= 1 {
			return generation
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("session activity never refreshed")
	return 0
}

func TestActivityFeedOpensAfterTheQueuedFrameOrAtOnceWhenCaughtUp(t *testing.T) {
	installTestSessionActivity(t)
	generation := waitForActivityGeneration(t)
	feed := func(after int64) feedProducer {
		return func(ctx context.Context, emit feedEmitter, opened func()) error {
			return activityFeed(ctx, after, emit, opened)
		}
	}
	if log, err := runProducerToOpening(t, feed(generation-1)); log != "activity/activity opened" || !errors.Is(err, context.Canceled) {
		t.Fatalf("a cursor behind the service must open after the queued generation frame: %q %v", log, err)
	}
	if log, err := runProducerToOpening(t, feed(generation)); log != "opened" || !errors.Is(err, context.Canceled) {
		t.Fatalf("a caught-up cursor has no opening frame and must open at once: %q %v", log, err)
	}
	if log, err := runProducerToOpening(t, feed(generation+1)); log != "activity/reset" || !errors.Is(err, errFeedRestart) {
		t.Fatalf("a cursor ahead of the service resets and returns: %q %v", log, err)
	}
}

func TestSessionFeedOpensOnTheLiveProducersFirstFrame(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	feed := func(runtime, id string) feedProducer {
		return func(ctx context.Context, emit feedEmitter, opened func()) error {
			return sessionFeed(ctx, runtime, id, emit, opened)
		}
	}
	if log, err := runProducerToOpening(t, feed("", "")); log != "" || err != nil {
		t.Fatalf("no subject: the feed ends quietly with no frame: %q %v", log, err)
	}
	if log, err := runProducerToOpening(t, feed("claude", "no-such-session")); log != "session/unavailable opened" || err != nil {
		t.Fatalf("an unresolvable subject opens on its terminal unavailable frame: %q %v", log, err)
	}
}

// recordMultiplexedStream runs the multiplexer over fake producers and returns
// the body once the connection ended. It reports false, with no body, when the
// connection is still open at the deadline (reading the recorder then would race).
func recordMultiplexedStream(t *testing.T, ctx context.Context, producers []feedProducer) (string, bool) {
	t.Helper()
	recorder := httptest.NewRecorder()
	writer := &feedFrameWriter{w: recorder, flusher: recorder}
	done := make(chan struct{})
	go func() { runEventStream(ctx, producers, writer, sessionStreamConfig()); close(done) }()
	select {
	case <-done:
		return recorder.Body.String(), true
	case <-time.After(eventStreamTestDeadline):
		return "", false
	}
}

// openThenBlock is a fake feed: run its opening, then wait for the connection.
func openThenBlock(open func(emit feedEmitter, opened func()) error) feedProducer {
	return func(ctx context.Context, emit feedEmitter, opened func()) error {
		if err := open(emit, opened); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}
}

func resetAtOnce(ctx context.Context, emit feedEmitter, opened func()) error {
	if err := emitJSON(emit, "b", "reset", 0, nil); err != nil {
		return err
	}
	return errFeedRestart
}

func TestEventStreamSlowOpenerIsFlushedBeforeAFastReset(t *testing.T) {
	slow := openThenBlock(func(emit feedEmitter, opened func()) error {
		time.Sleep(30 * time.Millisecond) // the approvals hub lock and marshal, exaggerated
		if err := emitJSON(emit, "a", "snapshot", 0, nil); err != nil {
			return err
		}
		opened()
		return nil
	})
	body, ended := recordMultiplexedStream(t, context.Background(), []feedProducer{slow, resetAtOnce})
	if !ended {
		t.Fatal("a reset must still end the connection once every feed has opened")
	}
	if !strings.Contains(body, `"feed":"a","event":"snapshot"`) || !strings.Contains(body, `"feed":"b","event":"reset"`) {
		t.Fatalf("the slow feed's opening frame must be on the wire before the reset close: %q", body)
	}
}

func TestEventStreamOpenedWithoutAFrameDoesNotHoldTheClose(t *testing.T) {
	silent := openThenBlock(func(_ feedEmitter, opened func()) error { opened(); return nil })
	body, ended := recordMultiplexedStream(t, context.Background(), []feedProducer{silent, resetAtOnce})
	if !ended || !strings.Contains(body, `"feed":"b","event":"reset"`) {
		t.Fatalf("a feed that opened with nothing to say must not delay the reset close: ended=%v %q", ended, body)
	}
}

func TestEventStreamClientDisconnectEndsTheWaitForOpenings(t *testing.T) {
	never := func(ctx context.Context, _ feedEmitter, _ func()) error { <-ctx.Done(); return ctx.Err() }
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	if _, ended := recordMultiplexedStream(t, ctx, []feedProducer{never, resetAtOnce}); !ended {
		t.Fatal("a client that leaves while a reset waits on another feed's opening must end the handler")
	}
}

func TestEventStreamRestartAfterEveryFeedOpenedStillCloses(t *testing.T) {
	aOpened := make(chan struct{})
	open := func(feed string, after func()) func(emit feedEmitter, opened func()) error {
		return func(emit feedEmitter, opened func()) error {
			if err := emitJSON(emit, feed, "snapshot", 0, nil); err != nil {
				return err
			}
			opened()
			after()
			return nil
		}
	}
	// b restarts only once a has opened, so this is the live phase by construction.
	restartAfterA := func(ctx context.Context, emit feedEmitter, opened func()) error {
		if err := open("b", func() {})(emit, opened); err != nil {
			return err
		}
		select {
		case <-aOpened:
		case <-ctx.Done():
			return ctx.Err()
		}
		return errFeedRestart
	}
	a := openThenBlock(open("a", func() { close(aOpened) }))
	body, ended := recordMultiplexedStream(t, context.Background(), []feedProducer{a, restartAfterA})
	if !ended || !strings.Contains(body, `"feed":"a","event":"snapshot"`) || !strings.Contains(body, `"feed":"b","event":"snapshot"`) {
		t.Fatalf("a restart after every feed opened must still close with both openings on the wire: ended=%v %q", ended, body)
	}
}
