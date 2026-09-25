package transcription

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWhisperCPPRealSandboxedDecode runs the installed whisper-cli inside the
// strict sandbox against a real model. It skips unless CG_WHISPER_MODEL names
// a readable model file and the Homebrew executable exists, so the gate never
// depends on a model download.
func TestWhisperCPPRealSandboxedDecode(t *testing.T) {
	model := os.Getenv("CG_WHISPER_MODEL")
	executable := "/opt/homebrew/bin/whisper-cli"
	if model == "" {
		t.Skip("CG_WHISPER_MODEL not set")
	}
	if _, err := os.Stat(model); err != nil {
		t.Skipf("model not readable: %v", err)
	}
	if _, err := os.Stat(executable); err != nil {
		t.Skipf("whisper-cli not installed: %v", err)
	}
	if ok, reason := NewStrictSandbox().Available(); !ok {
		t.Skip(reason)
	}
	fixture := "/private/tmp/cg-speech-proof/long-direct.wav" // name-lint: historical proof path
	if _, err := os.Stat(fixture); err != nil {
		t.Skipf("fixture missing: %v", err)
	}
	work := t.TempDir()
	wav := filepath.Join(work, "clip.wav")
	if err := os.WriteFile(wav, resampleFixture(t, fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	backend := &whisperCPP{settings: whisperCPPSettings{Executable: executable, ModelPath: model, Language: "en", CPUOnly: true}, sandbox: NewStrictSandbox()}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	started := time.Now()
	decoded, err := backend.Decode(ctx, DecodeRequest{WAVPath: wav, WorkDir: work, Hints: Hints{ProjectName: "crossing-guard", Keywords: []string{"diff"}}, CaptureLimit: 1 << 20}, nil)
	if err != nil {
		t.Fatalf("sandboxed decode failed after %s: %v", time.Since(started), detailOf(err))
	}
	t.Logf("sandboxed decode in %s: %q (confidence %.3f, %d words)", time.Since(started), decoded.Text, decoded.Confidence, len(decoded.Words))
	if !strings.Contains(strings.ToLower(decoded.Text), "crossing") || !decoded.HasConfidence || decoded.Confidence < 0.5 || len(decoded.Words) < 20 {
		t.Fatalf("unexpected decode: %+v", decoded)
	}
}

// resampleFixture converts the 22.05 kHz prototype fixture to 16 kHz mono PCM
// WAV bytes with nearest-sample decimation, enough for a recognition check.
func resampleFixture(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sourceRate := int(binary.LittleEndian.Uint32(raw[24:]))
	data := raw[44:]
	samples := len(data) / 2
	targetRate := 16000
	outSamples := samples * targetRate / sourceRate
	pcm := make([]byte, outSamples*2)
	for i := 0; i < outSamples; i++ {
		source := i * sourceRate / targetRate
		copy(pcm[2*i:], data[2*source:2*source+2])
	}
	tmp := filepath.Join(t.TempDir(), "resampled.wav")
	if err := WriteWAV(tmp, targetRate, pcm); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
