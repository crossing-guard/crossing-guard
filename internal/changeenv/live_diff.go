package changeenv

// Live workspace diff (session-review-diff-plan §2–§5; workspace-panes plan
// §4.1). A checkout is observed on demand under a deadline and answers one
// question: what is different in this folder now, compared with a named base.
// File identity comes from Git's -z protocols, never from a patch header; a
// change that cannot be shown as text is a typed row with no hunks; every cap
// that truncates is stated. Nothing here writes to a repository.
//
// Layout: this file holds the contract types and the three entry points;
// live_diff_observe.go runs git and reads the worktree; live_diff_parse.go reads
// the -z protocols and classifies rows.

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// LiveDiffConfig carries the operator's review budgets; there are no defaults here.
type LiveDiffConfig struct {
	BaseRef          string
	MaxFiles         int
	MaxStatusEntries int
	MaxFileBytes     int64
	ContextLines     int
	MaxRefs          int
}

// LiveScopes is the one scope vocabulary; the HTTP adapter validates against it.
var LiveScopes = []string{"working", "staged", "unstaged", "branch", "since-base"}

// ValidBaseRef refuses a base that git could read as an option or that no ref
// name can carry, before git sees it.
func ValidBaseRef(name string) error {
	if strings.HasPrefix(name, "-") || strings.ContainsAny(name, "\x00\n") || len(name) > 512 {
		return &LiveDiffError{Code: "base-ref-missing", Message: fmt.Sprintf("%q is not a ref name", name)}
	}
	return nil
}

// ValidRepositoryPath is the one rule for a client-supplied repository path:
// relative, clean, and never escaping the root.
func ValidRepositoryPath(path string) bool { return validManifestPath(path) }

// LiveDiffFile is one changed path. Kind says whether hunks can exist for it:
// text, binary, rename (no text change), mode (mode-only), typechange (file ↔
// symlink), conflict, submodule, or special (not a regular file).
type LiveDiffFile struct {
	Path      string `json:"path"`
	OldPath   string `json:"old_path,omitempty"`
	Status    string `json:"status"` // A M D R C T U, or ? for untracked
	Kind      string `json:"kind"`
	Added     int    `json:"added"`
	Removed   int    `json:"removed"`
	Freshness string `json:"freshness"`
	Bytes     int64  `json:"bytes,omitempty"`
	Untracked bool   `json:"untracked,omitempty"`
	// Truncated marks a row whose counts and freshness cover only the first
	// MaxFileBytes of a larger worktree file.
	Truncated bool `json:"truncated,omitempty"`
}

// LiveDiffTruncation states which cap cut the list and how many rows it dropped.
type LiveDiffTruncation struct {
	Files int    `json:"files"`
	Cap   string `json:"cap"`
}

// LiveDiffResult is one scope's listing with the checkout facts it was read from.
type LiveDiffResult struct {
	Scope      string              `json:"scope"`
	Base       string              `json:"base,omitempty"`
	BaseOID    string              `json:"base_oid,omitempty"`
	MergeBase  string              `json:"merge_base,omitempty"`
	Head       string              `json:"head"`
	Branch     string              `json:"branch,omitempty"`
	ObservedAt int64               `json:"observed_at"`
	Files      []LiveDiffFile      `json:"files"`
	Truncated  *LiveDiffTruncation `json:"truncated,omitempty"`
}

// LiveDiffPatch is one file's rendered patch and the freshness it was read at.
type LiveDiffPatch struct {
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	Freshness string `json:"freshness"`
	Patch     string `json:"patch"`
	Bytes     int    `json:"bytes"`
	Truncated bool   `json:"truncated"`
}

// LiveDiffBranch is one local or remote branch offered by the compare picker.
type LiveDiffBranch struct {
	Name    string `json:"name"`
	Head    string `json:"head"`
	Current bool   `json:"current"`
}

// LiveDiffCommit is one recent commit offered by the compare picker.
type LiveDiffCommit struct {
	SHA     string `json:"sha"`
	Short   string `json:"short"`
	Subject string `json:"subject"`
	Author  string `json:"author"`
	When    int64  `json:"when"`
}

// LiveDiffRefs is the compare picker's population.
type LiveDiffRefs struct {
	Branches []LiveDiffBranch `json:"branches"`
	Commits  []LiveDiffCommit `json:"commits"`
}

// LiveDiffError is a typed failure the panes render by code (diff plan §5;
// files plan §7): git-failed, git-timeout, base-ref-missing, unborn-head,
// not-a-repository, invalid-path, file-not-in-scope, and from the Files
// reader file-not-in-tree, not-a-file, not-a-directory, unreadable,
// unsupported-platform.
type LiveDiffError struct {
	Code    string
	Message string
	// ExitCode is git's exit status when the code is git-failed, else 0.
	ExitCode int
}

func (e *LiveDiffError) Error() string { return e.Code + ": " + e.Message }

// LiveDiff lists the changed files of one scope with per-file freshness.
func LiveDiff(ctx context.Context, root, scope, base string, cfg LiveDiffConfig) (LiveDiffResult, error) {
	result := LiveDiffResult{Scope: scope, ObservedAt: time.Now().Unix(), Files: []LiveDiffFile{}}
	if err := observeHead(ctx, root, &result); err != nil {
		return result, err
	}
	resolved, err := resolveLiveScope(ctx, root, scope, base, cfg, &result)
	if err != nil {
		return result, err
	}
	records, err := listTrackedRecords(ctx, root, resolved, "")
	if err != nil {
		return result, err
	}
	var untracked []string
	if resolved.untracked {
		if untracked, err = listUntracked(ctx, root, ""); err != nil {
			return result, err
		}
	}
	// The status cap bounds the per-file reads below, which is where the cost is.
	if cfg.MaxStatusEntries > 0 && len(records)+len(untracked) > cfg.MaxStatusEntries {
		dropped := len(records) + len(untracked) - cfg.MaxStatusEntries
		if len(records) > cfg.MaxStatusEntries {
			records = records[:cfg.MaxStatusEntries]
			untracked = nil
		} else {
			untracked = untracked[:cfg.MaxStatusEntries-len(records)]
		}
		result.Truncated = &LiveDiffTruncation{Files: dropped, Cap: "max_status_entries"}
	}
	files := make([]LiveDiffFile, 0, len(records)+len(untracked))
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return result, deadlineError()
		}
		files = append(files, trackedFile(root, resolved, record, cfg))
	}
	for _, path := range untracked {
		if err := ctx.Err(); err != nil {
			return result, deadlineError()
		}
		files = append(files, untrackedFile(root, path, cfg))
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	if cfg.MaxFiles > 0 && len(files) > cfg.MaxFiles {
		dropped := len(files) - cfg.MaxFiles
		if result.Truncated != nil {
			dropped += result.Truncated.Files
		}
		result.Truncated = &LiveDiffTruncation{Files: dropped, Cap: "max_files"}
		files = files[:cfg.MaxFiles]
	}
	result.Files = files
	return result, nil
}

// LiveDiffPatchFor renders one file's patch, bounded by the file byte budget,
// with the file's freshness as observed now so the pane can see it move.
func LiveDiffPatchFor(ctx context.Context, root, scope, base, path string, cfg LiveDiffConfig) (LiveDiffPatch, error) {
	if !validManifestPath(path) {
		return LiveDiffPatch{}, &LiveDiffError{Code: "invalid-path", Message: fmt.Sprintf("%q is not a repository path", path)}
	}
	result := LiveDiffResult{}
	if err := observeHead(ctx, root, &result); err != nil {
		return LiveDiffPatch{}, err
	}
	resolved, err := resolveLiveScope(ctx, root, scope, base, cfg, &result)
	if err != nil {
		return LiveDiffPatch{}, err
	}
	file, found, err := locateFile(ctx, root, resolved, cfg, path)
	if err != nil {
		return LiveDiffPatch{}, err
	}
	out := LiveDiffPatch{Path: path, Kind: file.Kind, Freshness: file.Freshness}
	if !found {
		return out, &LiveDiffError{Code: "file-not-in-scope", Message: fmt.Sprintf("%s is not changed in this scope", path)}
	}
	if file.Kind != "text" {
		return out, nil
	}
	contextFlag := "-U" + strconv.Itoa(cfg.ContextLines)
	var body []byte
	var exceeded bool
	if file.Untracked {
		body, exceeded, err = runLiveGit(ctx, root, cfg.MaxFileBytes, true, "diff", "--no-ext-diff", "--no-color", "--no-index", contextFlag, "--", os.DevNull, path)
	} else {
		paths := []string{path}
		if file.OldPath != "" {
			paths = []string{file.OldPath, path}
		}
		body, exceeded, err = runLiveGit(ctx, root, cfg.MaxFileBytes, true, resolved.diffArgs([]string{contextFlag}, paths)...)
	}
	if err != nil {
		return out, err
	}
	if exceeded {
		body = trimToLine(body)
	}
	if !utf8.Valid(body) {
		out.Kind = "binary"
		return out, nil
	}
	out.Patch, out.Bytes, out.Truncated = string(body), len(body), exceeded
	return out, nil
}

// LiveDiffRefsFor lists branches and recent commits for the compare picker,
// each bounded by MaxRefs.
func LiveDiffRefsFor(ctx context.Context, root string, cfg LiveDiffConfig) (LiveDiffRefs, error) {
	out := LiveDiffRefs{Branches: []LiveDiffBranch{}, Commits: []LiveDiffCommit{}}
	count := strconv.Itoa(cfg.MaxRefs)
	refs, _, err := runLiveGit(ctx, root, liveListingBytes, false, "for-each-ref", "--sort=-committerdate", "--count="+count,
		"--format=%(refname:short)%00%(objectname)%00%(HEAD)%00%(symref)", "refs/heads", "refs/remotes")
	if err != nil {
		return out, err
	}
	out.Branches = parseBranches(refs)
	log, _, err := runLiveGit(ctx, root, liveListingBytes, false, "log", "-n", count, "--format=%H%x00%h%x00%s%x00%an%x00%ct")
	if err != nil {
		return out, err
	}
	out.Commits = parseCommits(log)
	return out, nil
}
