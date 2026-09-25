package transcription

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const sampleWhisperJSON = `{"transcription":[{"text":" Rebase the branch.<|endoftext|>","offsets":{"from":0,"to":1500},
 "tokens":[{"text":"[_BEG_]","p":0.9,"offsets":{"from":0,"to":0}},
  {"text":" Re","p":0.9,"offsets":{"from":0,"to":200}},{"text":"base","p":0.8,"offsets":{"from":200,"to":400}},
  {"text":" the","p":0.95,"offsets":{"from":400,"to":600}},{"text":" branch","p":0.85,"offsets":{"from":600,"to":1000}},
  {"text":".","p":0.99,"offsets":{"from":1000,"to":1100}},{"text":"[_TT_75]","p":0.5,"offsets":{"from":1500,"to":1500}},
  {"text":"<|endoftext|>","p":0.4,"offsets":{"from":1500,"to":1500}}]}]}`

func TestParseWhisperJSONBuildsWordsAndConfidence(t *testing.T) {
	decoded, err := parseWhisperJSON([]byte(sampleWhisperJSON))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Text != "Rebase the branch." || !decoded.HasConfidence {
		t.Fatalf("decoded = %+v", decoded)
	}
	if len(decoded.Words) != 3 || decoded.Words[0].Text != "Rebase" || decoded.Words[0].EndMS != 400 || decoded.Words[2].Text != "branch." {
		t.Fatalf("words = %+v", decoded.Words)
	}
	want := (0.9 + 0.8 + 0.95 + 0.85 + 0.99) / 5
	if decoded.Confidence < want-0.001 || decoded.Confidence > want+0.001 {
		t.Fatalf("confidence = %v want %v", decoded.Confidence, want)
	}
}

// passthroughSandbox runs the executable directly and records the spec, so
// the adapter's argv can be asserted without sandbox-exec.
type passthroughSandbox struct{ specs []SandboxSpec }

func (p *passthroughSandbox) Available() (bool, string) { return true, "" }
func (p *passthroughSandbox) Command(ctx context.Context, spec SandboxSpec) (*exec.Cmd, error) {
	p.specs = append(p.specs, spec)
	cmd := exec.CommandContext(ctx, spec.Executable, spec.Args...)
	cmd.Dir = spec.Dir
	return cmd, nil
}

func TestWhisperCPPDecodeArgvCarriesHintsOnlyAndParsesOutput(t *testing.T) {
	work := t.TempDir()
	script := filepath.Join(work, "fake-whisper.sh")
	// The fake finds the -of prefix and writes the JSON there, like whisper-cli.
	body := "#!/bin/sh\nwhile [ $# -gt 0 ]; do if [ \"$1\" = \"-of\" ]; then prefix=\"$2\"; fi; shift; done\n" +
		"printf '%s' '" + strings.ReplaceAll(sampleWhisperJSON, "\n", "") + "' > \"$prefix.json\"\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	sandbox := &passthroughSandbox{}
	backend := &whisperCPP{settings: whisperCPPSettings{Executable: script, ModelPath: "/tmp/model.bin", Language: "en", CPUOnly: true}, sandbox: sandbox}
	decoded, err := backend.Decode(context.Background(), DecodeRequest{WAVPath: filepath.Join(work, "clip.wav"), WorkDir: work,
		Hints: Hints{ProjectName: "crossing-guard", Keywords: []string{"diff"}}, Window: true, CaptureLimit: 1024}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Text != "Rebase the branch." {
		t.Fatalf("decoded = %+v", decoded)
	}
	argv := strings.Join(sandbox.specs[0].Args, " ")
	if !strings.Contains(argv, "--no-gpu") || !strings.Contains(argv, "--output-json-full") || !strings.Contains(argv, "--prompt Project: crossing-guard. Terms: diff.") {
		t.Fatalf("argv = %q", argv)
	}
	if strings.Contains(argv, "Rebase") {
		t.Fatal("dictated text must never appear in argv")
	}
	if got := sandbox.specs[0].WritePaths; len(got) != 1 || got[0] != work {
		t.Fatalf("write paths = %v", got)
	}
}

func TestWhisperCPPDecodeReportsEngineErrorWithBoundedDetail(t *testing.T) {
	work := t.TempDir()
	script := filepath.Join(work, "fail.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'model load failed' >&2\nexit 3\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	backend := &whisperCPP{settings: whisperCPPSettings{Executable: script, ModelPath: "/tmp/model.bin", Language: "en"}, sandbox: &passthroughSandbox{}}
	_, err := backend.Decode(context.Background(), DecodeRequest{WAVPath: "x.wav", WorkDir: work, CaptureLimit: 8}, nil)
	typed, ok := err.(*Error)
	if !ok || typed.State != EndEngineError || typed.Message != "dictation failed" || !strings.Contains(typed.Detail, "exit status 3") || strings.Contains(typed.Detail, "model load failed") {
		t.Fatalf("err = %#v", err)
	}
}
