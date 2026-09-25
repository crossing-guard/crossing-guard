package collectionconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadUsesCodeEffectsDefaultOnlyWhenAbsent(t *testing.T) {
	loaded, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Document.ResultPayloadMode != CodeEffects || loaded.Origin != "builtin-default" {
		t.Fatalf("loaded = %+v", loaded)
	}
}

func TestLoadRejectsUnknownModeWithoutFallback(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "collection.json"), []byte(`{"format_version":1,"result_payload_mode":"everything"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("expected an unknown-mode error")
	}
}

func TestLoadReadsExplicitMode(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "collection.json"), []byte(`{"format_version":1,"result_payload_mode":"metadata-only"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Document.ResultPayloadMode != MetadataOnly || loaded.Origin != "local-file" {
		t.Fatalf("loaded = %+v", loaded)
	}
}
