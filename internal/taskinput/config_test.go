package taskinput

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testConfig() Config {
	return Config{FormatVersion: 1,
		TextExtensions:  []string{".txt", ".go", ".md", ".json"},
		ImageMediaTypes: []string{"image/png", "image/jpeg", "image/gif", "image/webp"},
		Limits: Limits{MaxItemsPerScope: 8, MaxSourceBytesPerItem: 1 << 20,
			MaxSourceBytesPerScope: 4 << 20, MaxGlobalSourceBytes: 8 << 20,
			MaxTextBytesPerItem: 256 << 10, MaxTextBytesPerScope: 1 << 20,
			MaxImageDimension: 1024, MaxImagePixels: 1 << 20,
			MaxNormalizedBytesPerItem: 2 << 20, MaxNormalizedBytesPerScope: 4 << 20,
			MaxBasenameBytes: 255, MaxDisplayCodePoints: 128,
			UnclaimedExpirySeconds: 3600, MaxConcurrentAdmissions: 2}}
}

func TestConfigValidationNormalizesAllowlists(t *testing.T) {
	config := testConfig()
	config.TextExtensions = []string{".GO", " .md "}
	config.ImageMediaTypes = []string{"IMAGE/PNG"}
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(config.TextExtensions, ",") != ".go,.md" || config.ImageMediaTypes[0] != "image/png" {
		t.Fatalf("config was not normalized: %#v", config)
	}
}

func TestLoadConfigFailsClosedForMissingMalformedAndUnknownFields(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadConfig(dir); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("missing config: %v", err)
	}
	path := ConfigPath(dir)
	if err := os.WriteFile(path, []byte(`{"format_version":1,"surprise":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(dir); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"format_version":1} trailing`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(dir); err == nil {
		t.Fatal("malformed config passed")
	}
	if filepath.Base(path) != "task-inputs.json" {
		t.Fatalf("unexpected config path %s", path)
	}
}
