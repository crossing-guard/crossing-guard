package transcription

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestService(t *testing.T, backend *fakeBackend, mutate func(*Config)) (*Service, *fakeClock, string) {
	t.Helper()
	config := testConfig()
	if mutate != nil {
		mutate(&config)
	}
	clock := newFakeClock()
	root := filepath.Join(t.TempDir(), "clips")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(ServiceOptions{Config: config, Backend: backend, ClipRoot: root, Clock: clock.Now,
		Hints: fakeHints{hints: Hints{ProjectName: "crossing-guard", BranchName: "feat/speech-to-text"}}})
	if err != nil {
		t.Fatal(err)
	}
	return service, clock, root
}

// frameSender keeps the sequence and offset across calls, as the browser does.
type frameSender struct{ seq, offset int64 }

var senders = map[string]*frameSender{}

func sendFrames(t *testing.T, service *Service, id string, pcm []byte, frameBytes int) {
	t.Helper()
	sender := senders[id]
	if sender == nil {
		sender = &frameSender{}
		senders[id] = sender
	}
	for start := 0; start < len(pcm); start += frameBytes {
		end := start + frameBytes
		if end > len(pcm) {
			end = len(pcm)
		}
		if err := service.Frame(id, sender.seq, sender.offset, pcm[start:end]); err != nil {
			t.Fatalf("frame %d: %v", sender.seq, err)
		}
		sender.seq++
		sender.offset += int64(end - start)
	}
}

func waitEvent(t *testing.T, events <-chan Event, kind string) Event {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case event, ok := <-events:
			if !ok {
				t.Fatalf("stream closed before %s", kind)
			}
			if event.Kind == kind {
				return event
			}
		case <-deadline:
			t.Fatalf("no %s event", kind)
		}
	}
}

func TestDictationLifecycleFinalTextAndCleanup(t *testing.T) {
	backend := newFakeBackend(testLocalBackend, false, false)
	service, _, root := newTestService(t, backend, nil)
	created, err := service.Create(context.Background(), CreateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if created.SampleRate != 16000 || !strings.HasPrefix(created.ID, "dict_") {
		t.Fatalf("created = %+v", created)
	}
	events, ok := service.Events(created.ID)
	if !ok {
		t.Fatal("no stream")
	}
	sendFrames(t, service, created.ID, tonePCM(16000, 1000, 0.4), 4000)
	ended, err := service.Finish(created.ID, "release")
	if err != nil {
		t.Fatal(err)
	}
	if ended.State != EndFinal || ended.Text != "final text" || ended.DurationMS != 1000 || !ended.HasConfidence {
		t.Fatalf("ended = %+v", ended)
	}
	if streamed := waitEvent(t, events, EventEnded); streamed.Text != ended.Text {
		t.Fatalf("stream ended = %+v", streamed)
	}
	if _, err := os.Stat(filepath.Join(root, created.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clip directory not deleted: %v", err)
	}
	requests := backend.recorded()
	if len(requests) != 1 || requests[0].Window || requests[0].Hints.ProjectName != "crossing-guard" || requests[0].Hints.BranchName == "" || requests[0].Hints.Keywords[0] != "diff" {
		t.Fatalf("backend requests = %+v", requests)
	}
	if _, err := service.Create(context.Background(), CreateRequest{}); err != nil {
		t.Fatalf("slot not released after end: %v", err)
	}
}

func TestDictationSilenceIsNothingHeardNotText(t *testing.T) {
	backend := newFakeBackend(testLocalBackend, false, false)
	service, _, _ := newTestService(t, backend, nil)
	created, _ := service.Create(context.Background(), CreateRequest{})
	sendFrames(t, service, created.ID, silencePCM(16000, 1000), 4000)
	ended, _ := service.Finish(created.ID, "release")
	if ended.State != EndNoSpeech || ended.Text != "" || len(backend.recorded()) != 0 {
		t.Fatalf("silence ended = %+v requests=%d", ended, len(backend.recorded()))
	}
}

func TestDictationLowConfidenceIsReviewedNotInserted(t *testing.T) {
	backend := newFakeBackend(testLocalBackend, false, false)
	backend.final = func(PartialFunc) (Decoded, error) {
		return Decoded{Text: "then run the full gate before you open the pole request", Confidence: 0.1, HasConfidence: true}, nil
	}
	service, _, _ := newTestService(t, backend, nil)
	created, _ := service.Create(context.Background(), CreateRequest{})
	sendFrames(t, service, created.ID, tonePCM(16000, 800, 0.4), 4000)
	ended, _ := service.Finish(created.ID, "release")
	if ended.State != EndLowConfidence || !strings.Contains(ended.Text, "pole") {
		t.Fatalf("ended = %+v", ended)
	}
}

func TestDictationFrameGapEndsAndDeletesClip(t *testing.T) {
	backend := newFakeBackend(testLocalBackend, false, false)
	service, _, root := newTestService(t, backend, nil)
	created, _ := service.Create(context.Background(), CreateRequest{})
	frame := tonePCM(16000, 250, 0.4)
	if err := service.Frame(created.ID, 0, 0, frame); err != nil {
		t.Fatal(err)
	}
	err := service.Frame(created.ID, 5, int64(len(frame)), frame)
	var typed *Error
	if !errors.As(err, &typed) || typed.State != EndFrameError {
		t.Fatalf("gap error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, created.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("clip not deleted after frame error")
	}
	if _, err := service.Finish(created.ID, "release"); !errors.Is(err, ErrNotListening) {
		t.Fatalf("finish after end = %v", err)
	}
}

func TestDictationDisclosureGatesCloudBackendAndStripsHints(t *testing.T) {
	backend := newFakeBackend(testCloudBackend, false, true)
	service, clock, _ := newTestService(t, backend, func(c *Config) { c.Backend = testCloudBackend })
	if _, err := service.Create(context.Background(), CreateRequest{}); !errors.Is(err, ErrDisclosureRequired) {
		t.Fatalf("create without token = %v", err)
	}
	if _, _, err := service.MintDisclosure(testCloudBackend, 2); !errors.Is(err, ErrDisclosureInvalid) {
		t.Fatalf("wrong version mint = %v", err)
	}
	token, ttl, err := service.MintDisclosure(testCloudBackend, 1)
	if err != nil || ttl != 120*time.Second {
		t.Fatalf("mint = %v %v", token, err)
	}
	created, err := service.Create(context.Background(), CreateRequest{DisclosureToken: token})
	if err != nil {
		t.Fatal(err)
	}
	sendFrames(t, service, created.ID, tonePCM(16000, 500, 0.4), 4000)
	if _, err := service.Finish(created.ID, "release"); err != nil {
		t.Fatal(err)
	}
	if requests := backend.recorded(); len(requests) != 1 || !requests[0].Hints.Empty() {
		t.Fatalf("cloud backend must receive no hints by default: %+v", requests)
	}
	if _, err := service.Create(context.Background(), CreateRequest{DisclosureToken: token}); !errors.Is(err, ErrDisclosureInvalid) {
		t.Fatalf("token reuse = %v", err)
	}
	token, _, _ = service.MintDisclosure(testCloudBackend, 1)
	clock.Advance(121 * time.Second)
	if _, err := service.Create(context.Background(), CreateRequest{DisclosureToken: token}); !errors.Is(err, ErrDisclosureInvalid) {
		t.Fatalf("expired token = %v", err)
	}
}

func TestDictationPartialLoopCommitsOverlapAndDiscardsStrays(t *testing.T) {
	backend := newFakeBackend(testLocalBackend, true, false)
	backend.window = func(n int) (Decoded, error) {
		switch n {
		case 1:
			return Decoded{Text: "rebase the branch onto main", Words: []Word{
				{Text: "rebase", StartMS: 0, EndMS: 400}, {Text: "the", StartMS: 400, EndMS: 600},
				{Text: "branch", StartMS: 600, EndMS: 1000}, {Text: "onto", StartMS: 1000, EndMS: 2600},
				{Text: "main", StartMS: 2600, EndMS: 2900}}}, nil
		case 2:
			return Decoded{Text: "for", Words: []Word{{Text: "for", StartMS: 0, EndMS: 200}}}, nil
		default:
			return Decoded{Text: "branch onto main and run the gate", Words: []Word{
				{Text: "branch", StartMS: 0, EndMS: 200}, {Text: "onto", StartMS: 200, EndMS: 400},
				{Text: "main", StartMS: 400, EndMS: 700}, {Text: "and", StartMS: 700, EndMS: 900},
				{Text: "run", StartMS: 900, EndMS: 1100}, {Text: "the", StartMS: 1100, EndMS: 1300},
				{Text: "gate", StartMS: 1300, EndMS: 3300}}}, nil
		}
	}
	service, clock, _ := newTestService(t, backend, func(c *Config) {
		c.Streaming.WindowSeconds = 3
		c.Streaming.OverlapSeconds = 1
		c.Streaming.PartialMinIntervalMS = 100
	})
	created, _ := service.Create(context.Background(), CreateRequest{})
	events, _ := service.Events(created.ID)

	// First window: 3 s of audio, decode 1. Cutoff = 3000 - 1000; words ending
	// by 2000 ms commit, "onto" and "main" stay in the tail.
	sendFrames(t, service, created.ID, tonePCM(16000, 3000, 0.4), 16000*2*3)
	first := waitEvent(t, events, EventPartial)
	if first.Committed != "rebase the branch" || first.Tail != "onto main" {
		t.Fatalf("first partial = %+v", first)
	}
	// Second window returns a stray "for": discarded, nothing emitted.
	clock.Advance(time.Second)
	sendFrames(t, service, created.ID, tonePCM(16000, 500, 0.4), 16000)
	waitDecode(t, backend, 2)
	select {
	case event := <-events:
		t.Fatalf("stray decode should emit nothing, got %+v", event)
	case <-time.After(150 * time.Millisecond):
	}
	// Third window: audio now 6 s; window starts at 3000. Words are absolute
	// 3000..6300; cutoff = 6000-1000 = 5000. "branch onto main and run the"
	// end by 4300 and commit; "gate" (ends 6300) is the tail. Words before
	// committedUntilMS (2000) are skipped, so "branch" (3000..3200) is kept
	// because the clip advanced; the overlap rule accepted the decode because
	// it contains the last committed word "branch".
	clock.Advance(time.Second)
	sendFrames(t, service, created.ID, tonePCM(16000, 2500, 0.4), 16000*5)
	third := waitEvent(t, events, EventPartial)
	if !strings.HasPrefix(third.Committed, "rebase the branch") || !strings.HasSuffix(third.Committed, "run the") || third.Tail != "gate" {
		t.Fatalf("third partial = %+v", third)
	}
	ended, err := service.Finish(created.ID, "release")
	if err != nil || ended.State != EndFinal || ended.Text != "final text" {
		t.Fatalf("finish = %+v %v", ended, err)
	}
	if requests := backend.recorded(); requests[len(requests)-1].Window {
		t.Fatal("final decode must not be a window decode")
	}
}

func waitDecode(t *testing.T, backend *fakeBackend, count int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(backend.recorded()) >= count {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("backend saw %d decodes, want %d", len(backend.recorded()), count)
}

func TestDictationFinalFailureKeepsLastSeenWords(t *testing.T) {
	backend := newFakeBackend(testLocalBackend, true, false)
	backend.window = func(int) (Decoded, error) {
		return Decoded{Text: "keep these words", Words: []Word{{Text: "keep", EndMS: 100}, {Text: "these", StartMS: 100, EndMS: 200}, {Text: "words", StartMS: 200, EndMS: 2900}}}, nil
	}
	backend.final = func(PartialFunc) (Decoded, error) { return Decoded{}, Fail(EndEngineError, "dictation failed") }
	service, _, _ := newTestService(t, backend, func(c *Config) { c.Streaming.PartialMinIntervalMS = 1 })
	created, _ := service.Create(context.Background(), CreateRequest{})
	events, _ := service.Events(created.ID)
	sendFrames(t, service, created.ID, tonePCM(16000, 3000, 0.4), 16000*2*3)
	waitEvent(t, events, EventPartial)
	ended, _ := service.Finish(created.ID, "release")
	if ended.State != EndEngineError || ended.Text != "keep these words" || ended.Message != "dictation failed" {
		t.Fatalf("ended = %+v", ended)
	}
}

func TestDictationCancelBusyAndAbandon(t *testing.T) {
	backend := newFakeBackend(testLocalBackend, false, false)
	service, clock, root := newTestService(t, backend, nil)
	created, _ := service.Create(context.Background(), CreateRequest{})
	if _, err := service.Create(context.Background(), CreateRequest{}); !errors.Is(err, ErrBusy) {
		t.Fatalf("second create = %v", err)
	}
	if err := service.Cancel(created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, created.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("clip not deleted on cancel")
	}
	second, _ := service.Create(context.Background(), CreateRequest{})
	events, _ := service.Events(second.ID)
	clock.Advance(21 * time.Second)
	service.Sweep()
	if ended := waitEvent(t, events, EventEnded); ended.State != EndAbandoned {
		t.Fatalf("abandoned = %+v", ended)
	}
	if err := service.Cancel("dict_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id = %v", err)
	}
}

func TestDictationCapFinishesAutomatically(t *testing.T) {
	backend := newFakeBackend(testLocalBackend, false, false)
	service, _, _ := newTestService(t, backend, func(c *Config) { c.Limits.MaxSeconds = 1 })
	created, _ := service.Create(context.Background(), CreateRequest{})
	events, _ := service.Events(created.ID)
	sendFrames(t, service, created.ID, tonePCM(16000, 1000, 0.4), 8000)
	if ended := waitEvent(t, events, EventEnded); ended.State != EndFinal {
		t.Fatalf("cap ended = %+v", ended)
	}
}
