package guardcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func isolatedRuntimeSearch(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", filepath.Join(home, "bin"))
	oldDirs, oldGlobs := extraBinDirs, nvmGlobs
	extraBinDirs, nvmGlobs = []string{"~/.local/bin"}, nil
	t.Cleanup(func() { extraBinDirs, nvmGlobs = oldDirs, oldGlobs })
	return home
}

func markerExecutable(t *testing.T, path, marker string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf invoked > '"+marker+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
}

func TestClaudePresenceRequiresClientAndNeverExecutesIt(t *testing.T) {
	home := isolatedRuntimeSearch(t)
	installer := claudeInstaller{}
	settings := installer.ResolveConfig()
	if settings == "" || installer.RuntimePresent() {
		t.Fatal("potential settings path fabricated a client")
	}
	if err := os.MkdirAll(filepath.Dir(settings), 0700); err != nil {
		t.Fatal(err)
	}
	const contents = "{\"foreign\":true}\n"
	if err := os.WriteFile(settings, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	if installer.RuntimePresent() {
		t.Fatal("config-only state fabricated a client")
	}
	binary, marker := filepath.Join(home, ".local/bin/claude"), filepath.Join(home, "invoked")
	markerExecutable(t, binary, marker)
	if !installer.RuntimePresent() {
		t.Fatal("client outside minimal PATH was missed")
	}
	if err := os.Remove(settings); err != nil {
		t.Fatal(err)
	}
	if !installer.RuntimePresent() {
		t.Fatal("fresh client requires settings")
	}
	if err := os.Chmod(binary, 0600); err != nil {
		t.Fatal(err)
	}
	if installer.RuntimePresent() {
		t.Fatal("nonexecutable reported as client")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("provider was executed: %v", err)
	}
}

func TestSharedRuntimeResolutionPreservesPrecedence(t *testing.T) {
	home := isolatedRuntimeSearch(t)
	marker := filepath.Join(home, "invoked")
	paths := []string{filepath.Join(home, "configured/tool"), filepath.Join(home, "bin/tool"), filepath.Join(home, ".local/bin/tool"), filepath.Join(home, "fallback/tool")}
	for _, path := range paths {
		markerExecutable(t, path, marker)
	}
	got, err := ResolveRuntimeBinary("tool", paths[0], paths[3])
	if err != nil || got != paths[0] {
		t.Fatalf("configured=%q err=%v", got, err)
	}
	if _, err := ResolveRuntimeBinary("tool", filepath.Join(home, "missing"), paths[3]); err == nil {
		t.Fatal("invalid override silently fell back")
	}
	for _, want := range paths[1:] {
		got, err = ResolveRuntimeBinary("tool", "", paths[3])
		if err != nil || got != want {
			t.Fatalf("got %q want %q err=%v", got, want, err)
		}
		if err := os.Remove(want); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ResolveRuntimeBinary("tool", "", paths[3]); err == nil || !strings.Contains(err.Error(), "searched:") {
		t.Fatalf("missing error=%v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("resolver executed provider: %v", err)
	}
	if !strings.Contains(RuntimeAgentPATH(), filepath.Join(home, ".local/bin")) {
		t.Fatal("service PATH lost shared prefix")
	}
}
