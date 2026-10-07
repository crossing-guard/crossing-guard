package changeenv

// The live diff's git runner, base and scope resolution, and worktree reads.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"unicode/utf8"
)

// liveListingBytes bounds one listing command's output; a patch is bounded by
// the configured file budget instead.
const liveListingBytes = 16 << 20

// defaultBaseCandidates are tried, in order, when neither the request nor the
// configuration names a base and the remote has no default branch.
var defaultBaseCandidates = []string{"main", "master"}

func deadlineError() *LiveDiffError {
	return &LiveDiffError{Code: "git-timeout", Message: "reading the folder took too long"}
}

// runLiveGit runs one bounded git command and captures stderr's first line as
// the typed message. Exit status 1 is a result for diff commands (differences
// found), so a caller may allow it. It is the third runner in this package
// beside gitRawContext (listings, no stderr) and runBoundedGitBody (checkpoint
// bodies, fixed cap) because the pane needs git's own words and a configured
// cap; the plan's §13 records the choice.
func runLiveGit(ctx context.Context, root string, limit int64, allowExit1 bool, args ...string) ([]byte, bool, error) {
	argv := append(append([]string{"-C", root, "--no-optional-locks", "-c", "core.quotepath=false"}, gitReadGuard(ctx)...), args...)
	cmd := exec.CommandContext(ctx, "git", argv...)
	cmd.Env = append(cmd.Environ(), "LC_ALL=C")
	out := &cappedOutput{max: int(limit)}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = out, &stderr
	err := cmd.Run()
	if err != nil {
		if ctx.Err() != nil {
			return nil, out.exceeded, deadlineError()
		}
		var exit *exec.ExitError
		code := -1
		if errors.As(err, &exit) {
			code = exit.ExitCode()
		}
		if !(allowExit1 && code == 1) {
			return nil, out.exceeded, &LiveDiffError{Code: "git-failed", Message: firstLine(stderr.String(), err.Error()), ExitCode: code}
		}
	}
	body := out.buf.Bytes()
	if out.exceeded && int64(len(body)) > limit {
		body = body[:limit]
	}
	return body, out.exceeded, nil
}

func firstLine(text, fallback string) string {
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return fallback
}

// IsRepository says whether dir is inside a git work tree, distinguishing "not
// a repository" (git's own verdict) from git failing for another reason.
func IsRepository(ctx context.Context, dir string) (bool, error) {
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return false, nil
	}
	_, _, err := runLiveGit(ctx, dir, liveListingBytes, false, "rev-parse", "--is-inside-work-tree")
	var typed *LiveDiffError
	if errors.As(err, &typed) && typed.Code == "git-failed" && typed.ExitCode == 128 && strings.Contains(strings.ToLower(typed.Message), "not a git repository") {
		return false, nil
	}
	return err == nil, err
}

// liveOID resolves a ref to a commit id. With --quiet, a missing ref exits 1
// and says nothing; any other failure keeps git's message.
func liveOID(ctx context.Context, root, ref string) (string, error) {
	out, _, err := runLiveGit(ctx, root, liveListingBytes, false, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func isMissingRef(err error) bool {
	var typed *LiveDiffError
	return errors.As(err, &typed) && typed.Code == "git-failed" && typed.ExitCode == 1
}

// resolveBase names the base the branch scopes compare against: the request,
// else the configured ref, else the remote's default branch, else the named
// candidates. An option-shaped value is refused before git sees it.
func resolveBase(ctx context.Context, root, requested, configured string) (name, oid string, err error) {
	name = strings.TrimSpace(requested)
	if name == "" {
		name = strings.TrimSpace(configured)
	}
	if err := ValidBaseRef(name); err != nil {
		return "", "", err
	}
	candidates := []string{name}
	if name == "" {
		candidates = nil
		if out, _, e := runLiveGit(ctx, root, liveListingBytes, false, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); e == nil && len(bytes.TrimSpace(out)) > 0 {
			candidates = append(candidates, strings.TrimSpace(string(out)))
		}
		candidates = append(candidates, defaultBaseCandidates...)
	}
	for _, candidate := range candidates {
		oid, e := liveOID(ctx, root, candidate)
		if e == nil {
			return candidate, oid, nil
		}
		if !isMissingRef(e) {
			return "", "", e
		}
	}
	if name == "" {
		name = "the default branch"
	}
	return "", "", &LiveDiffError{Code: "base-ref-missing", Message: fmt.Sprintf("base ref %s was not found", name)}
}

type liveScope struct {
	revs      []string
	cached    bool
	untracked bool // untracked files count as additions
	worktree  bool // the target side is the filesystem
}

func resolveLiveScope(ctx context.Context, root, scope, base string, cfg LiveDiffConfig, result *LiveDiffResult) (liveScope, error) {
	out := liveScope{}
	switch scope {
	case "working":
		out.revs, out.untracked, out.worktree = []string{"HEAD"}, true, true
	case "staged":
		out.revs, out.cached = []string{"HEAD"}, true
	case "unstaged":
		out.worktree = true
	case "branch", "since-base":
		name, oid, err := resolveBase(ctx, root, base, cfg.BaseRef)
		if err != nil {
			return liveScope{}, err
		}
		result.Base, result.BaseOID = name, oid
		mergeBase := oid
		if mb, _, err := runLiveGit(ctx, root, liveListingBytes, true, "merge-base", oid, "HEAD"); err == nil && len(bytes.TrimSpace(mb)) > 0 {
			mergeBase = strings.TrimSpace(string(mb))
		}
		result.MergeBase = mergeBase
		if scope == "branch" {
			out.revs = []string{mergeBase, "HEAD"}
		} else {
			out.revs, out.untracked, out.worktree = []string{mergeBase}, true, true
		}
	default:
		return liveScope{}, &LiveDiffError{Code: "git-failed", Message: fmt.Sprintf("unknown scope %q", scope)}
	}
	return out, nil
}

// diffArgs orders one diff invocation as git reads it: flags, then revisions,
// then `--` and the pathspecs. Anything after `--` is a path to git, so the
// revisions must come first.
func (s liveScope) diffArgs(flags []string, paths []string) []string {
	args := []string{"diff", "--no-ext-diff", "--no-color", "-M"}
	if s.cached {
		args = append(args, "--cached")
	}
	args = append(args, flags...)
	args = append(args, s.revs...)
	if len(paths) > 0 {
		args = append(args, "--")
		args = append(args, paths...)
	}
	return args
}

func observeHead(ctx context.Context, root string, result *LiveDiffResult) error {
	head, err := liveOID(ctx, root, "HEAD")
	if err != nil {
		if isMissingRef(err) {
			return &LiveDiffError{Code: "unborn-head", Message: "this repository has no commits yet"}
		}
		return err
	}
	result.Head = head
	if out, _, err := runLiveGit(ctx, root, liveListingBytes, false, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
		result.Branch = strings.TrimSpace(string(out))
	}
	return nil
}

func listTrackedRecords(ctx context.Context, root string, scope liveScope, only string) ([]rawRecord, error) {
	var paths []string
	if only != "" {
		paths = []string{only}
	}
	raw, _, err := runLiveGit(ctx, root, liveListingBytes, true, scope.diffArgs([]string{"--raw", "-z", "--no-abbrev"}, paths)...)
	if err != nil {
		return nil, err
	}
	stats, _, err := runLiveGit(ctx, root, liveListingBytes, true, scope.diffArgs([]string{"--numstat", "-z"}, paths)...)
	if err != nil {
		return nil, err
	}
	records, err := parseLiveRaw(raw)
	if err != nil {
		return nil, err
	}
	unmerged, err := listUnmerged(ctx, root, scope, only)
	if err != nil {
		return nil, err
	}
	byPath := parseLiveNumstat(stats)
	for index := range records {
		if unmerged[records[index].path] {
			records[index].status = "U"
		}
		records[index].stat = byPath[records[index].path]
	}
	return records, nil
}

// listUnmerged names the paths with unresolved conflicts. A revision-to-worktree
// diff reports them as plain modifications, so the index is asked directly; a
// commit-only scope has no index side and gets none.
func listUnmerged(ctx context.Context, root string, scope liveScope, only string) (map[string]bool, error) {
	out := map[string]bool{}
	if !scope.worktree && !scope.cached {
		return out, nil
	}
	args := []string{"ls-files", "-u", "-z"}
	if only != "" {
		args = append(args, "--", only)
	}
	raw, _, err := runLiveGit(ctx, root, liveListingBytes, false, args...)
	if err != nil {
		return nil, err
	}
	for _, part := range bytes.Split(raw, []byte{0}) {
		if _, path, found := bytes.Cut(part, []byte{'\t'}); found {
			out[string(path)] = true
		}
	}
	return out, nil
}

func listUntracked(ctx context.Context, root string, only string) ([]string, error) {
	args := []string{"ls-files", "--others", "--exclude-standard", "-z"}
	if only != "" {
		args = append(args, "--", only)
	}
	out, _, err := runLiveGit(ctx, root, liveListingBytes, false, args...)
	if err != nil {
		return nil, err
	}
	paths := []string{}
	for _, part := range bytes.Split(out, []byte{0}) {
		if len(part) > 0 {
			paths = append(paths, string(part))
		}
	}
	return paths, nil
}

func locateFile(ctx context.Context, root string, scope liveScope, cfg LiveDiffConfig, path string) (LiveDiffFile, bool, error) {
	records, err := listTrackedRecords(ctx, root, scope, path)
	if err != nil {
		return LiveDiffFile{}, false, err
	}
	for _, record := range records {
		if record.path == path {
			return trackedFile(root, scope, record, cfg), true, nil
		}
	}
	if scope.untracked {
		paths, err := listUntracked(ctx, root, path)
		if err != nil {
			return LiveDiffFile{}, false, err
		}
		for _, candidate := range paths {
			if candidate == path {
				return untrackedFile(root, path, cfg), true, nil
			}
		}
	}
	return LiveDiffFile{}, false, nil
}

// worktreeContent is what the filesystem says about one path: a content hash
// over at most maxBytes, its size, whether it reads as binary, its line count,
// and whether the hash covers only a prefix. Only regular files and symlinks are
// read; anything else is reported as special without being opened, so a FIFO
// cannot hold the observation open. The prefix (at most the configured file
// budget) is materialised in memory for the hash and the line count.
type worktreeContent struct {
	hash      string
	size      int64
	binary    bool
	lines     int
	truncated bool
	special   bool
}

func worktreeFacts(root, path string, maxBytes int64) (worktreeContent, error) {
	// Every component is opened without following symlinks (the Files reader's
	// seam), so a symlinked directory cannot redirect an untracked read.
	read, err := statAndReadBounded(root, path, maxBytes)
	if err != nil {
		return worktreeContent{}, err
	}
	hasher := sha256.New()
	if read.Mode&os.ModeSymlink != 0 {
		hasher.Write([]byte("symlink:" + read.LinkTarget))
		return worktreeContent{hash: hex.EncodeToString(hasher.Sum(nil)), size: int64(len(read.LinkTarget)), lines: 1}, nil
	}
	if !read.Mode.IsRegular() {
		return worktreeContent{special: true, size: read.Size}, nil
	}
	hasher.Write(read.Body)
	counter := &lineCounter{}
	_, _ = counter.Write(read.Body) // lineCounter cannot fail
	out := worktreeContent{size: read.Size, lines: counter.lines, truncated: read.Truncated, binary: looksBinary(read.Body)}
	if out.truncated {
		// The hash covers a prefix, so the size and mtime keep it moving.
		fmt.Fprintf(hasher, "\x00%d\x00%d", read.Size, read.ModTime.UnixNano())
	}
	out.hash = hex.EncodeToString(hasher.Sum(nil))
	return out, nil
}

type lineCounter struct{ lines int }

func (c *lineCounter) Write(p []byte) (int, error) {
	c.lines += bytes.Count(p, []byte{'\n'})
	if len(p) > 0 && p[len(p)-1] != '\n' {
		c.lines++
	}
	return len(p), nil
}

// binarySniffBytes is the prefix examined for a NUL or invalid UTF-8; one
// window for the Diff pane's rows and the Files pane's reads.
const binarySniffBytes = 8000

func freshnessOf(parts ...string) string {
	hasher := sha256.New()
	for _, part := range parts {
		hasher.Write([]byte(part))
		hasher.Write([]byte{0})
	}
	return "sha256-v1:" + hex.EncodeToString(hasher.Sum(nil))
}

func trackedFile(root string, scope liveScope, record rawRecord, cfg LiveDiffConfig) LiveDiffFile {
	file := LiveDiffFile{Path: record.path, OldPath: record.oldPath, Status: record.status, Added: record.stat.added, Removed: record.stat.removed}
	target := record.dstOID
	if scope.worktree && record.status != "D" {
		content, err := worktreeFacts(root, record.path, cfg.MaxFileBytes)
		switch {
		case err != nil:
			// A worktree side that cannot be read has no content identity; say so
			// rather than reuse the all-zero object id git reports for the worktree.
			target = "unreadable:" + err.Error()
		case content.special:
			file.Kind = "special"
			target = "special"
		default:
			target, file.Bytes, file.Truncated = content.hash, content.size, content.truncated
		}
	}
	if file.Kind == "" {
		file.Kind = classify(record)
	}
	file.Freshness = freshnessOf(record.srcOID, target, record.srcMode, record.dstMode)
	return file
}

func untrackedFile(root, path string, cfg LiveDiffConfig) LiveDiffFile {
	file := LiveDiffFile{Path: path, Status: "?", Kind: "text", Untracked: true}
	content, err := worktreeFacts(root, path, cfg.MaxFileBytes)
	switch {
	case err != nil:
		file.Kind = "special"
		file.Freshness = freshnessOf("untracked", path, "unreadable:"+err.Error())
	case content.special:
		file.Kind = "special"
		file.Bytes = content.size
		file.Freshness = freshnessOf("untracked", path, "special")
	default:
		file.Bytes, file.Added, file.Truncated = content.size, content.lines, content.truncated
		if content.binary {
			file.Kind, file.Added = "binary", 0
		}
		file.Freshness = freshnessOf("untracked", content.hash)
	}
	return file
}

// trimToLine cuts a bounded patch back to its last complete line, then to its
// last complete rune, so a prefix never reads as binary.
func trimToLine(body []byte) []byte {
	if cut := bytes.LastIndexByte(body, '\n'); cut >= 0 {
		body = body[:cut+1]
	}
	for len(body) > 0 && !utf8.Valid(body) {
		body = body[:len(body)-1]
	}
	return body
}
