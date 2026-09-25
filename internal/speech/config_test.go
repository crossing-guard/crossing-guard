package speech

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const examplePath = "../../docs/public/reference/config/speech.example.json"

func TestExampleFileLoadsAndNamesItsPlaceholderProblem(t *testing.T) {
	dataDir := t.TempDir()
	raw, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(dataDir), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(dataDir)
	if err != nil {
		t.Fatalf("example must validate: %v", err)
	}
	if loaded.Ready {
		t.Fatal("example placeholder model path must not be ready")
	}
	joined := strings.Join(loaded.Problems, "\n")
	if !strings.Contains(joined, "model file") && !strings.Contains(joined, "executable") {
		t.Fatalf("problems = %v", loaded.Problems)
	}
	config := loaded.File.Transcription
	if config.Backend != "whispercpp" || config.HintPolicyFor("openai-batch").ProjectName || config.DisclosureFor("openai-batch") == nil {
		t.Fatalf("example defaults changed: %+v", config)
	}
}

func TestLoadReportsMissingUnknownFieldAndTrailingData(t *testing.T) {
	dataDir := t.TempDir()
	_, err := Load(dataDir)
	if err == nil || !strings.Contains(err.Error(), "disabled until") {
		t.Fatalf("missing file err = %v", err)
	}
	if err := os.WriteFile(Path(dataDir), []byte(`{"format_version":1,"transcription":{},"extra":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dataDir); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field err = %v", err)
	}
	if err := os.WriteFile(Path(dataDir), []byte(`{"format_version":2,"transcription":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dataDir); err == nil || !strings.Contains(err.Error(), "format_version") {
		t.Fatalf("version err = %v", err)
	}
}

func TestLoadIsReadyWithARealExecutableAndMatchingChecksum(t *testing.T) {
	dataDir := t.TempDir()
	model := filepath.Join(dataDir, "model.bin")
	if err := os.WriteFile(model, []byte("not really a model"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("not really a model"))
	raw, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	whisper := document["transcription"].(map[string]any)["backends"].(map[string]any)["whispercpp"].(map[string]any)
	whisper["executable"] = "/bin/sh"
	whisper["model_path"] = model
	whisper["model_sha256"] = hex.EncodeToString(sum[:])
	encoded, _ := json.Marshal(document)
	if err := os.WriteFile(Path(dataDir), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Ready {
		if _, statErr := os.Stat("/usr/bin/sandbox-exec"); errors.Is(statErr, os.ErrNotExist) {
			t.Skipf("no sandbox on this platform: %v", loaded.Problems)
		}
		t.Fatalf("expected ready, problems = %v", loaded.Problems)
	}
	if loaded.Backend == nil || loaded.Backend.Name() != "whispercpp" || len(loaded.Backend.Capabilities().Facts) == 0 {
		t.Fatalf("backend = %v", loaded.Backend)
	}
	root, err := PrepareClipRoot(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(root, "dict_old")
	_ = os.MkdirAll(stale, 0o700)
	if _, err := PrepareClipRoot(dataDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("boot cleanup left a stale clip directory")
	}
}
