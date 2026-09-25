package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validWorkspaceConfigJSON(allowedRoot, worktreeRoot string) string {
	return `{
  "format_version": 1,
  "allowed_roots": [` + quoteJSON(allowedRoot) + `],
  "worktrees": {
    "root": ` + quoteJSON(worktreeRoot) + `,
    "max_managed": 8,
    "max_aggregate_bytes": 10737418240,
    "max_replay_files": 2000,
    "max_replay_bytes": 268435456,
    "max_replay_file_bytes": 20971520,
    "unused_warning_hours": 168,
    "failed_create_recovery_hours": 24,
    "include_untracked_default": false,
    "copy_ignored_default": false
  },
  "review": {
    "max_files": 500,
    "max_rendered_file_bytes": 2097152,
    "max_rendered_total_bytes": 8388608,
    "max_hunks": 2000,
    "max_rendered_line_bytes": 32768
  },
  "mutation": {
    "max_patch_bytes": 2097152,
    "max_patch_lines": 1000,
    "max_paths": 500,
    "preview_ttl_seconds": 300,
    "approval_ttl_seconds": 600,
    "max_active_per_selection": 1,
    "max_recovery_bytes": 268435456,
    "recovery_retention_hours": 24
  }
}`
}

func quoteJSON(value string) string {
	return `"` + strings.ReplaceAll(value, `\`, `\\`) + `"`
}

func TestLoadConfigRequiresExplicitValidBudgetsAndCanonicalRoots(t *testing.T) {
	base := t.TempDir()
	allowed := filepath.Join(base, "repo")
	if err := os.Mkdir(allowed, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, "workspace.json")
	if err := os.WriteFile(path, []byte(validWorkspaceConfigJSON(filepath.Join(allowed, "."), filepath.Join(base, "managed"))), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	canonicalAllowed, _ := filepath.EvalSymlinks(allowed)
	canonicalBase, _ := filepath.EvalSymlinks(base)
	if config.AllowedRoots[0] != canonicalAllowed || config.Worktrees.Root != filepath.Join(canonicalBase, "managed") || config.Worktrees.MaxManaged != 8 {
		t.Fatalf("config = %+v", config)
	}
}

func TestLoadConfigRejectsUnknownFieldsOverlapAndMissingLimits(t *testing.T) {
	base := t.TempDir()
	allowed := filepath.Join(base, "repo")
	if err := os.Mkdir(allowed, 0o700); err != nil {
		t.Fatal(err)
	}
	worktreeFile := filepath.Join(base, "not-a-directory")
	if err := os.WriteFile(worktreeFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"unknown": strings.Replace(validWorkspaceConfigJSON(allowed, filepath.Join(base, "managed")), `"format_version": 1`, `"format_version": 1, "mystery": true`, 1),
		"overlap": validWorkspaceConfigJSON(allowed, filepath.Join(allowed, "worktrees")),
		"zero":    strings.Replace(validWorkspaceConfigJSON(allowed, filepath.Join(base, "managed")), `"max_managed": 8`, `"max_managed": 0`, 1),
		"file":    validWorkspaceConfigJSON(allowed, worktreeFile),
	} {
		path := filepath.Join(base, name+".json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil {
			t.Fatalf("%s config succeeded", name)
		}
	}
}
