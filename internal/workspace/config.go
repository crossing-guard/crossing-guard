package workspace

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	FormatVersion int            `json:"format_version"`
	AllowedRoots  []string       `json:"allowed_roots"`
	Worktrees     WorktreeConfig `json:"worktrees"`
	Review        ReviewConfig   `json:"review"`
	Mutation      MutationConfig `json:"mutation"`
}

type WorktreeConfig struct {
	Root                      string `json:"root"`
	MaxManaged                int    `json:"max_managed"`
	MaxAggregateBytes         int64  `json:"max_aggregate_bytes"`
	MaxReplayFiles            int    `json:"max_replay_files"`
	MaxReplayBytes            int64  `json:"max_replay_bytes"`
	MaxReplayFileBytes        int64  `json:"max_replay_file_bytes"`
	UnusedWarningHours        int    `json:"unused_warning_hours"`
	FailedCreateRecoveryHours int    `json:"failed_create_recovery_hours"`
	IncludeUntrackedDefault   bool   `json:"include_untracked_default"`
	CopyIgnoredDefault        bool   `json:"copy_ignored_default"`
}

type ReviewConfig struct {
	MaxFiles              int   `json:"max_files"`
	MaxRenderedFileBytes  int64 `json:"max_rendered_file_bytes"`
	MaxRenderedTotalBytes int64 `json:"max_rendered_total_bytes"`
	MaxHunks              int   `json:"max_hunks"`
	MaxRenderedLineBytes  int64 `json:"max_rendered_line_bytes"`
}

type MutationConfig struct {
	MaxPatchBytes          int64 `json:"max_patch_bytes"`
	MaxPatchLines          int   `json:"max_patch_lines"`
	MaxPaths               int   `json:"max_paths"`
	PreviewTTLSeconds      int   `json:"preview_ttl_seconds"`
	ApprovalTTLSeconds     int   `json:"approval_ttl_seconds"`
	MaxActivePerSelection  int   `json:"max_active_per_selection"`
	MaxRecoveryBytes       int64 `json:"max_recovery_bytes"`
	RecoveryRetentionHours int   `json:"recovery_retention_hours"`
}

// LoadConfig strictly reads one operator-owned workspace.json. Missing or invalid
// configuration is returned as an error so the composition root can disable only
// this capability with the exact reason; there are no fallback budgets.
func LoadConfig(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()
	decoder := json.NewDecoder(io.LimitReader(f, 1<<20))
	decoder.DisallowUnknownFields()
	var config Config
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("decode workspace config: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return Config{}, fmt.Errorf("decode workspace config: multiple JSON values")
		}
		return Config{}, fmt.Errorf("decode workspace config: %w", err)
	}
	if err := config.validateAndCanonicalize(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func (c *Config) validateAndCanonicalize() error {
	if c.FormatVersion != 1 {
		return fmt.Errorf("workspace config format_version must be 1")
	}
	if len(c.AllowedRoots) == 0 {
		return fmt.Errorf("workspace config requires at least one allowed root")
	}
	seen := map[string]bool{}
	for i, root := range c.AllowedRoots {
		canonical, err := canonicalExistingDirectory(root)
		if err != nil {
			return fmt.Errorf("allowed_roots[%d]: %w", i, err)
		}
		if seen[canonical] {
			return fmt.Errorf("allowed_roots[%d] duplicates %q", i, canonical)
		}
		seen[canonical] = true
		c.AllowedRoots[i] = canonical
	}
	worktreeRoot, err := canonicalFutureDirectory(c.Worktrees.Root)
	if err != nil {
		return fmt.Errorf("worktrees.root: %w", err)
	}
	c.Worktrees.Root = worktreeRoot
	for _, root := range c.AllowedRoots {
		if pathWithin(worktreeRoot, root) || pathWithin(root, worktreeRoot) {
			return fmt.Errorf("worktrees.root must be outside every allowed source root")
		}
	}
	if c.Worktrees.MaxManaged <= 0 || c.Worktrees.MaxAggregateBytes <= 0 ||
		c.Worktrees.MaxReplayFiles <= 0 || c.Worktrees.MaxReplayBytes <= 0 ||
		c.Worktrees.MaxReplayFileBytes <= 0 || c.Worktrees.UnusedWarningHours <= 0 ||
		c.Worktrees.FailedCreateRecoveryHours <= 0 {
		return fmt.Errorf("workspace worktree limits must all be positive")
	}
	for _, limit := range []struct {
		name  string
		value int64
	}{{"max_files", int64(c.Review.MaxFiles)}, {"max_rendered_file_bytes", c.Review.MaxRenderedFileBytes},
		{"max_rendered_total_bytes", c.Review.MaxRenderedTotalBytes}, {"max_hunks", int64(c.Review.MaxHunks)},
		{"max_rendered_line_bytes", c.Review.MaxRenderedLineBytes}} {
		if limit.value <= 0 {
			return fmt.Errorf("workspace review.%s must be positive", limit.name)
		}
	}
	if c.Mutation.MaxPatchBytes <= 0 || c.Mutation.MaxPatchLines <= 0 ||
		c.Mutation.MaxPaths <= 0 || c.Mutation.PreviewTTLSeconds <= 0 ||
		c.Mutation.ApprovalTTLSeconds <= 0 || c.Mutation.MaxActivePerSelection != 1 ||
		c.Mutation.MaxRecoveryBytes <= 0 || c.Mutation.RecoveryRetentionHours <= 0 {
		return fmt.Errorf("workspace mutation limits must be positive and max_active_per_selection must be 1")
	}
	return nil
}

func canonicalExistingDirectory(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" || !filepath.IsAbs(raw) {
		return "", fmt.Errorf("path must be absolute")
	}
	clean := filepath.Clean(raw)
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("path is not a directory")
	}
	if resolved == string(filepath.Separator) {
		return "", fmt.Errorf("filesystem root is not an allowed workspace root")
	}
	return resolved, nil
}

func canonicalFutureDirectory(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" || !filepath.IsAbs(raw) {
		return "", fmt.Errorf("path must be absolute")
	}
	clean := filepath.Clean(raw)
	if clean == string(filepath.Separator) {
		return "", fmt.Errorf("filesystem root is not a valid managed-worktree root")
	}
	probe := clean
	remaining := []string{}
	for {
		info, err := os.Lstat(probe)
		if err == nil {
			if len(remaining) == 0 && !info.IsDir() {
				return "", fmt.Errorf("path is not a directory")
			}
			break
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", err
		}
		remaining = append(remaining, filepath.Base(probe))
		probe = parent
	}
	resolved, err := filepath.EvalSymlinks(probe)
	if err != nil {
		return "", err
	}
	for i := len(remaining) - 1; i >= 0; i-- {
		resolved = filepath.Join(resolved, remaining[i])
	}
	return resolved, nil
}

func pathWithin(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
