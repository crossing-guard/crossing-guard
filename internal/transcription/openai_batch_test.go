package transcription

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func openAIFixture(t *testing.T, handler http.HandlerFunc) (*openAIBatch, string) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	t.Setenv("CG_TEST_OPENAI_KEY", "sk-test-secret-value")
	backend := &openAIBatch{settings: openAIBatchSettings{APIKeyEnv: "CG_TEST_OPENAI_KEY", Model: "gpt-transcribe", Origin: server.URL}, client: server.Client()}
	wav := filepath.Join(t.TempDir(), "clip.wav")
	if err := WriteWAV(wav, 16000, tonePCM(16000, 300, 0.3)); err != nil {
		t.Fatal(err)
	}
	return backend, wav
}

func TestOpenAIBatchStreamsDeltasThenFinalText(t *testing.T) {
	var seenModel, seenStream, seenPrompt, seenAuth string
	backend, wav := openAIFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(4 << 20); err != nil {
			t.Fatal(err)
		}
		seenModel, seenStream, seenPrompt = r.FormValue("model"), r.FormValue("stream"), r.FormValue("prompt")
		seenAuth = r.Header.Get("Authorization")
		if _, header, err := r.FormFile("file"); err != nil || header.Filename != "dictation.wav" {
			t.Fatalf("file part: %v %v", header, err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"transcript.text.delta\",\"delta\":\"then run \"}\n\n" +
			"data: {\"type\":\"transcript.text.delta\",\"delta\":\"the gate\"}\n\n" +
			"data: {\"type\":\"transcript.text.done\",\"text\":\"then run the gate\"}\n\n"))
	})
	var partials []string
	decoded, err := backend.Decode(context.Background(), DecodeRequest{WAVPath: wav, Hints: Hints{ProjectName: "crossing-guard"}},
		func(text string) { partials = append(partials, text) })
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Text != "then run the gate" || decoded.HasConfidence {
		t.Fatalf("decoded = %+v", decoded)
	}
	if len(partials) != 2 || partials[1] != "then run the gate" {
		t.Fatalf("partials = %v", partials)
	}
	if seenModel != "gpt-transcribe" || seenStream != "true" || seenPrompt != "Project: crossing-guard." || seenAuth != "Bearer sk-test-secret-value" {
		t.Fatalf("request fields model=%q stream=%q prompt=%q auth=%q", seenModel, seenStream, seenPrompt, seenAuth)
	}
}

func TestOpenAIBatchRejectionsAreProviderErrorsWithoutTheKey(t *testing.T) {
	backend, wav := openAIFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad key sk-test-secret-value"}`))
	})
	_, err := backend.Decode(context.Background(), DecodeRequest{WAVPath: wav}, nil)
	typed, ok := err.(*Error)
	if !ok || typed.State != EndProviderError || !strings.Contains(typed.Message, "rejected") || strings.Contains(typed.Detail, "sk-test-secret-value") {
		t.Fatalf("err = %#v", err)
	}
	if _, err := backend.Decode(context.Background(), DecodeRequest{WAVPath: wav, Window: true}, nil); err == nil {
		t.Fatal("window decode must be refused")
	}
	t.Setenv("CG_TEST_OPENAI_KEY", "")
	_, err = backend.Decode(context.Background(), DecodeRequest{WAVPath: wav}, nil)
	if typed, ok := err.(*Error); !ok || typed.State != EndProviderError {
		t.Fatalf("missing key err = %#v", err)
	}
}

func TestOpenAIBatchTimeoutIsATimeout(t *testing.T) {
	release := make(chan struct{})
	backend, wav := openAIFixture(t, func(w http.ResponseWriter, r *http.Request) { <-release })
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := backend.Decode(ctx, DecodeRequest{WAVPath: wav}, nil)
	if StateOf(err) != EndCancelled {
		t.Fatalf("cancelled decode state = %v (%v)", StateOf(err), err)
	}
}
