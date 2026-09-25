package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"crossing-guard/internal/speech"
	"crossing-guard/internal/transcription"
)

// scriptedBackend answers the final pass with fixed text and no partials.
type scriptedBackend struct{ text string }

func (scriptedBackend) Name() string { return speechTestBackend }
func (scriptedBackend) Capabilities() transcription.Capabilities {
	return transcription.Capabilities{Backend: speechTestBackend, DisplayName: "Scripted",
		ReportsConfidence: true, PreferredSampleRate: 16000}
}
func (b scriptedBackend) Decode(context.Context, transcription.DecodeRequest, transcription.PartialFunc) (transcription.Decoded, error) {
	return transcription.Decoded{Text: b.text, Confidence: 0.9, HasConfidence: true}, nil
}

const speechTestBackend = "whispercpp"

func speechTestConfig() transcription.Config {
	settings, _ := json.Marshal(map[string]any{"executable": "/bin/sh", "model_path": "/tmp/model.bin",
		"model_sha256": strings.Repeat("a", 64), "language": "en", "cpu_only": true, "sandbox": "strict"})
	return transcription.Config{
		Backend: speechTestBackend,
		Backends: map[string]transcription.BackendSection{speechTestBackend: {
			Hints: transcription.HintPolicy{ProjectName: true}, Settings: settings}},
		Disclosure: transcription.DisclosureLimits{TokenSeconds: 120},
		Audio:      transcription.Audio{Channels: 1, Bits: 16},
		Limits: transcription.Limits{MinMS: 250, MaxSeconds: 120, SilenceStopSeconds: 15, MinPeak: 0.01, MinRMS: 0.003,
			MinConfidence: 0.2, FinalDeadlineSeconds: 30, MaxConcurrent: 1, AbandonSeconds: 20, SweepSeconds: 5,
			FailurePauseCount: 3, FailurePauseSeconds: 10, MaxCaptureBytes: 1 << 20},
		Streaming: transcription.Streaming{FrameIntervalMS: 250, MaxFrameBytes: 65536, PartialMinIntervalMS: 700,
			WindowSeconds: 8, OverlapSeconds: 2, KeepaliveSeconds: 15, MaxStreams: 2},
		UI: transcription.UI{PushToTalk: "Alt+Space", HoldThresholdMS: 300, AutoSendMinWords: 3, DimTailWords: 2, TintSeconds: 4},
	}
}

func installSpeechForHTTP(t *testing.T, backend transcription.Backend) *transcription.Service {
	t.Helper()
	config := speechTestConfig()
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "clips")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	service, err := transcription.NewService(transcription.ServiceOptions{Config: config, Backend: backend,
		ClipRoot: root, Hints: speechHintSource{}})
	if err != nil {
		t.Fatal(err)
	}
	speechMu.Lock()
	previousLoaded, previousProblem, previousService, previousStreams := speechLoaded, speechProblem, dictations, speechStreams
	speechLoaded = &speech.Loaded{Path: "/tmp/speech.json", File: speech.File{FormatVersion: 1, Transcription: config}, Backend: backend, Ready: true}
	speechProblem, dictations, speechStreams = "", service, make(chan struct{}, config.Streaming.MaxStreams)
	speechMu.Unlock()
	t.Cleanup(func() {
		speechMu.Lock()
		speechLoaded, speechProblem, dictations, speechStreams = previousLoaded, previousProblem, previousService, previousStreams
		speechMu.Unlock()
	})
	return service
}

func speechMux() *http.ServeMux {
	mux := http.NewServeMux()
	registerSpeechRoutes(mux)
	return mux
}

func tone16k(ms int) []byte {
	samples := 16000 * ms / 1000
	out := make([]byte, samples*2)
	for i := 0; i < samples; i++ {
		value := int16(12000 * ((i/18)%2*2 - 1)) // square wave, plenty of energy
		out[2*i], out[2*i+1] = byte(value), byte(value>>8)
	}
	return out
}

func TestSpeechCapabilitiesReportEachState(t *testing.T) {
	speechMu.Lock()
	previousLoaded, previousProblem, previousService := speechLoaded, speechProblem, dictations
	speechLoaded, speechProblem, dictations = nil, "dictation is disabled until /tmp/speech.json is configured", nil
	speechMu.Unlock()
	t.Cleanup(func() {
		speechMu.Lock()
		speechLoaded, speechProblem, dictations = previousLoaded, previousProblem, previousService
		speechMu.Unlock()
	})
	recorder := httptest.NewRecorder()
	speechMux().ServeHTTP(recorder, httptest.NewRequest("GET", "/api/speech/capabilities", nil))
	var unconfigured speechCapabilities
	_ = json.Unmarshal(recorder.Body.Bytes(), &unconfigured)
	if unconfigured.State != "unconfigured" || !strings.Contains(unconfigured.Reason, "disabled until") {
		t.Fatalf("unconfigured = %+v", unconfigured)
	}
	create := httptest.NewRecorder()
	speechMux().ServeHTTP(create, httptest.NewRequest("POST", "/api/speech/dictations", strings.NewReader("{}")))
	if create.Code != http.StatusServiceUnavailable || !bytes.Contains(create.Body.Bytes(), []byte(`"code":"unavailable"`)) {
		t.Fatalf("unconfigured create status=%d body=%s", create.Code, create.Body.String())
	}

	installSpeechForHTTP(t, scriptedBackend{text: "ready text"})
	recorder = httptest.NewRecorder()
	speechMux().ServeHTTP(recorder, httptest.NewRequest("GET", "/api/speech/capabilities", nil))
	var ready speechCapabilities
	_ = json.Unmarshal(recorder.Body.Bytes(), &ready)
	if ready.State != "ready" || ready.Backend == nil || ready.Backend.PreferredSampleRate != 16000 || ready.UI == nil || ready.UI.PushToTalk != "Alt+Space" || ready.Limits.MinPeak != 0.01 {
		t.Fatalf("ready = %+v", ready)
	}
}

func TestSpeechDictationJourneyOverHTTP(t *testing.T) {
	installSpeechForHTTP(t, scriptedBackend{text: "then run the full gate"})
	mux := speechMux()

	create := httptest.NewRecorder()
	mux.ServeHTTP(create, httptest.NewRequest("POST", "/api/speech/dictations", strings.NewReader(`{"cwd":"`+t.TempDir()+`"}`)))
	if create.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	var created transcription.Created
	_ = json.Unmarshal(create.Body.Bytes(), &created)

	// Stream first, so the terminal event is observed end to end.
	streamCtx, cancelStream := context.WithCancel(context.Background())
	defer cancelStream()
	streamRecorder := httptest.NewRecorder()
	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		mux.ServeHTTP(streamRecorder, httptest.NewRequest("GET", "/api/speech/dictations/"+created.ID+"/stream", nil).WithContext(streamCtx))
	}()

	frame := tone16k(500)
	for seq, offset := int64(0), int64(0); seq < 2; seq++ {
		request := httptest.NewRequest("POST", "/api/speech/dictations/"+created.ID+"/frames?seq="+itoa(seq)+"&offset="+itoa(offset), bytes.NewReader(frame))
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNoContent {
			t.Fatalf("frame %d status=%d body=%s", seq, recorder.Code, recorder.Body.String())
		}
		offset += int64(len(frame))
	}
	tooLarge := httptest.NewRecorder()
	mux.ServeHTTP(tooLarge, httptest.NewRequest("POST", "/api/speech/dictations/"+created.ID+"/frames?seq=2&offset="+itoa(int64(2*len(frame))), bytes.NewReader(make([]byte, 70000))))
	if tooLarge.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize frame status=%d", tooLarge.Code)
	}

	finish := httptest.NewRecorder()
	mux.ServeHTTP(finish, httptest.NewRequest("POST", "/api/speech/dictations/"+created.ID+"/finish", strings.NewReader(`{"reason":"release"}`)))
	var ended transcription.Event
	_ = json.Unmarshal(finish.Body.Bytes(), &ended)
	if finish.Code != http.StatusOK || ended.State != transcription.EndFinal || ended.Text != "then run the full gate" || ended.DurationMS != 1000 {
		t.Fatalf("finish status=%d body=%s", finish.Code, finish.Body.String())
	}
	select {
	case <-streamDone:
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not close after the terminal event")
	}
	body := streamRecorder.Body.String()
	if !strings.Contains(body, "event: ended") || !strings.Contains(body, `"text":"then run the full gate"`) || streamRecorder.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream body = %q", body)
	}

	again := httptest.NewRecorder()
	mux.ServeHTTP(again, httptest.NewRequest("POST", "/api/speech/dictations/"+created.ID+"/finish", strings.NewReader(`{}`)))
	if again.Code != http.StatusConflict {
		t.Fatalf("second finish status=%d", again.Code)
	}
	missing := httptest.NewRecorder()
	mux.ServeHTTP(missing, httptest.NewRequest("DELETE", "/api/speech/dictations/dict_missing", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("cancel missing status=%d", missing.Code)
	}
}

func TestSpeechDisclosureRouteRejectsBackendsWithoutOne(t *testing.T) {
	installSpeechForHTTP(t, scriptedBackend{text: "x"})
	recorder := httptest.NewRecorder()
	speechMux().ServeHTTP(recorder, httptest.NewRequest("POST", "/api/speech-disclosures",
		strings.NewReader(`{"backend_id":"whispercpp","operation":"transcription","disclosure_version":1}`)))
	if recorder.Code != http.StatusConflict || !bytes.Contains(recorder.Body.Bytes(), []byte(`"code":"no_disclosure"`)) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	bad := httptest.NewRecorder()
	speechMux().ServeHTTP(bad, httptest.NewRequest("POST", "/api/speech-disclosures", strings.NewReader(`{"operation":"synthesis"}`)))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("bad operation status=%d", bad.Code)
	}
}

func TestSpeechHintSourceUsesValidatedCwdOnly(t *testing.T) {
	dir := t.TempDir()
	hints, err := speechHintSource{}.Hints(context.Background(), transcription.HintRequest{Cwd: dir})
	if err != nil || hints.ProjectName != filepath.Base(dir) || hints.BranchName != "" {
		t.Fatalf("hints = %+v err=%v", hints, err)
	}
	hints, _ = speechHintSource{}.Hints(context.Background(), transcription.HintRequest{Cwd: "/definitely/not/a/dir"})
	if hints.ProjectName != "" {
		t.Fatalf("invalid cwd must yield no project hint: %+v", hints)
	}
}

func itoa(value int64) string { return strconv.FormatInt(value, 10) }
