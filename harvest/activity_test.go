package harvest

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestRepositoryGroupKey(t *testing.T) {
	for name, tc := range map[string]struct {
		session SessionSummary
		want    string
	}{
		"cwd":       {SessionSummary{Cwd: "/Users/me/repo", Project: "fallback"}, "/Users/me/repo"},
		"fallback":  {SessionSummary{Project: "vendor-project"}, "vendor-project"},
		"private":   {SessionSummary{Cwd: "/private/tmp/repo"}, "/tmp/repo"},
		"worktree":  {SessionSummary{Runtime: "claude", Cwd: "/Users/me/repo/.claude/worktrees/task"}, "/Users/me/repo"},
		"alternate": {SessionSummary{Runtime: "claude", Cwd: "/Users/me/repo/.claude-worktrees/task"}, "/Users/me/repo"},
		"root":      {SessionSummary{Cwd: "/"}, "/"},
		"missing":   {SessionSummary{}, NoProjectKey},
	} {
		t.Run(name, func(t *testing.T) {
			if got := RepositoryGroupKey(tc.session); got != tc.want {
				t.Fatalf("RepositoryGroupKey() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseOpenFileRecordsKeepsCommandOwnership(t *testing.T) {
	records := parseOpenFileRecords("p10\ncCodex-helper\nn/a.jsonl\nn/b.jsonl\np11\ncclaude\nn/c.jsonl\n")
	if len(records) != 3 || records[0].Command != "Codex-helper" || records[1].Path != "/b.jsonl" || records[2].Command != "claude" {
		t.Fatalf("unexpected records: %#v", records)
	}
}

func TestObserveOpenSessionsUsesExactPathAndVendorProcess(t *testing.T) {
	original := observeOpenFilesFn
	t.Cleanup(func() { observeOpenFilesFn = original })
	dir := t.TempDir()
	codexPath := filepath.Join(dir, "codex.jsonl")
	claudePath := filepath.Join(dir, "claude.jsonl")
	observeOpenFilesFn = func(context.Context, []string) ([]openFileRecord, error) {
		return []openFileRecord{
			{Command: "codex", Path: codexPath},
			{Command: "codex", Path: claudePath}, // wrong vendor process
			{Command: "claude", Path: filepath.Join(dir, "elsewhere", "claude.jsonl")},
		}, nil
	}
	observation := ObserveOpenSessions(context.Background(), []SessionSummary{
		{Runtime: "codex", Path: codexPath}, {Runtime: "claude", Path: claudePath},
	})
	if observation.Capability.Status != "available" || !observation.OpenPaths[codexPath] || observation.OpenPaths[claudePath] {
		t.Fatalf("unexpected observation: %#v", observation)
	}
}

func TestObserveOpenSessionsFailureIsUnavailable(t *testing.T) {
	original := observeOpenFilesFn
	t.Cleanup(func() { observeOpenFilesFn = original })
	observeOpenFilesFn = func(context.Context, []string) ([]openFileRecord, error) {
		return nil, errors.New("permission denied")
	}
	observation := ObserveOpenSessions(context.Background(), []SessionSummary{{Runtime: "codex", Path: "/tmp/a"}})
	if observation.Capability.Status != "unavailable" || len(observation.OpenPaths) != 0 {
		t.Fatalf("failure became a closed population: %#v", observation)
	}
}
