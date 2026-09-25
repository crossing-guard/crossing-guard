package transcription

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func dirGone(t *testing.T, root, id string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(root, id))
	return errors.Is(err, os.ErrNotExist)
}

// An abandon while a window decode is in flight must wait for the decode and
// then delete the clip; nothing may outlive the dictation.
func TestAbandonWaitsForInFlightDecodeBeforeDeletingClip(t *testing.T) {
	backend := newFakeBackend(testLocalBackend, true, false)
	backend.block = make(chan struct{})
	backend.ignoreCancel = true
	service, clock, root := newTestService(t, backend, func(c *Config) { c.Streaming.PartialMinIntervalMS = 1 })
	created, _ := service.Create(context.Background(), CreateRequest{})
	sendFrames(t, service, created.ID, tonePCM(16000, 3000, 0.4), 16000*2*3)
	waitDecode(t, backend, 1)
	clock.Advance(21 * time.Second)
	swept := make(chan struct{})
	go func() { service.Sweep(); close(swept) }()
	select {
	case <-swept:
		t.Fatal("sweep ended the dictation without waiting for the in-flight decode")
	case <-time.After(150 * time.Millisecond):
	}
	if dirGone(t, root, created.ID) {
		t.Fatal("clip directory deleted while a decode was still running")
	}
	close(backend.block)
	select {
	case <-swept:
	case <-time.After(2 * time.Second):
		t.Fatal("sweep did not complete after the decode released")
	}
	if !dirGone(t, root, created.ID) {
		t.Fatal("clip directory survived the abandon")
	}
	events, _ := service.Events(created.ID)
	if ended := waitEvent(t, events, EventEnded); ended.State != EndAbandoned {
		t.Fatalf("ended = %+v", ended)
	}
}

// A cancel during the final pass ends the dictation as cancelled, waits for
// the final decode, and leaves no clip behind; Finish then reports cancelled,
// never an empty state.
func TestCancelDuringFinalPassWaitsAndReportsCancelled(t *testing.T) {
	backend := newFakeBackend(testLocalBackend, false, false)
	backend.block = make(chan struct{})
	backend.ignoreCancel = true
	service, _, root := newTestService(t, backend, nil)
	created, _ := service.Create(context.Background(), CreateRequest{})
	sendFrames(t, service, created.ID, tonePCM(16000, 800, 0.4), 4000)
	finished := make(chan Event, 1)
	go func() { ended, _ := service.Finish(created.ID, "release"); finished <- ended }()
	waitDecode(t, backend, 1)
	cancelled := make(chan struct{})
	go func() { _ = service.Cancel(created.ID); close(cancelled) }()
	select {
	case <-cancelled:
		t.Fatal("cancel returned before the final decode released")
	case <-time.After(150 * time.Millisecond):
	}
	close(backend.block)
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not complete")
	}
	ended := <-finished
	if ended.State != EndCancelled || ended.Kind != EventEnded {
		t.Fatalf("finish after cancel = %+v", ended)
	}
	if !dirGone(t, root, created.ID) {
		t.Fatal("clip directory survived the cancel")
	}
}

// The final-pass deadline is the fail-closed budget: a backend that never
// answers ends the dictation as a timeout with the clip deleted.
func TestFinalDeadlineEndsAsTimeout(t *testing.T) {
	backend := newFakeBackend(testLocalBackend, false, false)
	backend.block = make(chan struct{})
	defer close(backend.block)
	service, _, root := newTestService(t, backend, func(c *Config) { c.Limits.FinalDeadlineSeconds = 1 })
	created, _ := service.Create(context.Background(), CreateRequest{})
	sendFrames(t, service, created.ID, tonePCM(16000, 800, 0.4), 4000)
	ended, err := service.Finish(created.ID, "release")
	if err != nil || ended.State != EndTimeout || ended.Message != "took too long" {
		t.Fatalf("ended = %+v err=%v", ended, err)
	}
	if !dirGone(t, root, created.ID) {
		t.Fatal("clip directory survived the timeout")
	}
}

// A frame that arrives after Finish has started is refused, never silently
// accepted and dropped.
func TestFrameAfterFinishIsRefused(t *testing.T) {
	backend := newFakeBackend(testLocalBackend, false, false)
	backend.block = make(chan struct{})
	defer close(backend.block)
	service, _, _ := newTestService(t, backend, nil)
	created, _ := service.Create(context.Background(), CreateRequest{})
	frame := tonePCM(16000, 500, 0.4)
	sendFrames(t, service, created.ID, frame, len(frame))
	go func() { _, _ = service.Finish(created.ID, "release") }()
	waitDecode(t, backend, 1)
	if err := service.Frame(created.ID, 1, int64(len(frame)), frame); !errors.Is(err, ErrNotListening) {
		t.Fatalf("late frame = %v", err)
	}
}

// Once the last committed word has scrolled out of the window, the stray
// check has nothing to overlap with and must not stall partials forever.
func TestCommitWindowDoesNotStallAfterCommittedWordScrollsOut(t *testing.T) {
	words := []Word{{Text: "later", StartMS: 100, EndMS: 400}, {Text: "words", StartMS: 400, EndMS: 2900}}
	committed, tail, ok := commitWindow(words, 10000, 12000, "later words", 5000, "branch")
	if !ok || len(committed) != 1 || committed[0].Text != "later" || tail != "words" {
		t.Fatalf("commit = %+v tail=%q ok=%v", committed, tail, ok)
	}
	_, _, ok = commitWindow(words, 4000, 6000, "later words", 5000, "branch")
	if ok {
		t.Fatal("a decode inside the committed window that lacks the last committed word must be discarded")
	}
}
