package changeenv

// The Files pane's inventory and reader (console-files-pane-plan §2–§3). Git is
// the inventory: one bounded `ls-files` per directory, folded into that
// directory's immediate children; the read route serves only a path the same
// listing returns, through the secure bounded reader. Nothing here writes.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// ErrUnsupportedPlatform is the fail-closed answer of a build without the
// component-wise no-follow opener.
var ErrUnsupportedPlatform = errors.New("secure file reading is unavailable on this platform")

// secureStat is what the no-follow stat reports about one name.
type secureStat struct {
	Mode    os.FileMode
	Size    int64
	ModTime time.Time
}

// boundedRead is the secure reader's answer: the stat, a link target for a
// symlink, and for a regular file the first maxBytes bytes.
type boundedRead struct {
	secureStat
	LinkTarget string
	Body       []byte
	Truncated  bool
}

// TreeConfig carries the operator's Files budgets; there are no defaults here.
type TreeConfig struct {
	MaxEntriesPerDir int
	MaxReadBytes     int64
}

// TreeEntry is one immediate child of a listed directory.
type TreeEntry struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Kind      string `json:"kind"` // dir file symlink submodule missing nested-repository special
	Untracked bool   `json:"untracked,omitempty"`
	Size      int64  `json:"size,omitempty"`
	ModTime   int64  `json:"mtime,omitempty"`
}

// TreeListing is one directory's children. State names why a directory has no
// entries: not-in-checkout, ignored, empty, or nested-repository. Dropped counts
// entries the cap or the listing cut; UnreadableNames counts names that are not
// valid UTF-8 and cannot cross JSON.
type TreeListing struct {
	Dir             string      `json:"dir"`
	Entries         []TreeEntry `json:"entries"`
	State           string      `json:"state,omitempty"`
	Truncated       bool        `json:"truncated,omitempty"`
	Dropped         int         `json:"dropped,omitempty"`
	UnreadableNames int         `json:"unreadable_names,omitempty"`
}

// TreeFile is one file's bounded read.
type TreeFile struct {
	Path      string `json:"path"`
	Kind      string `json:"kind"` // text binary symlink special
	Size      int64  `json:"size"`
	ModTime   int64  `json:"mtime"`
	Text      string `json:"text,omitempty"`
	Bytes     int    `json:"bytes"`
	Truncated bool   `json:"truncated"`
	Target    string `json:"target,omitempty"`
}

// ValidTreeDir accepts "" or "." as the root and otherwise a repository path
// with no .git component.
func ValidTreeDir(dir string) (string, bool) {
	dir = strings.TrimSuffix(dir, "/")
	if dir == "" || dir == "." {
		return "", true
	}
	if !ValidTreePath(dir) {
		return "", false
	}
	return dir, true
}

// ValidTreePath is the read route's rule: a repository path with no .git
// component. The compare is case-insensitive because the common filesystems
// are, and git itself refuses any .git component in the index.
func ValidTreePath(path string) bool {
	if !ValidRepositoryPath(path) {
		return false
	}
	for _, component := range strings.Split(path, "/") {
		if strings.EqualFold(component, ".git") {
			return false
		}
	}
	return true
}

// ListTree lists one directory's immediate children from git's population.
func ListTree(ctx context.Context, root, dir string, cfg TreeConfig) (TreeListing, error) {
	dir, ok := ValidTreeDir(dir)
	if !ok {
		return TreeListing{}, treeErr("invalid-path", "dir must be a relative repository path outside .git")
	}
	out := TreeListing{Dir: dir, Entries: []TreeEntry{}}
	raw, exceeded, err := runLiveGit(ctx, root, liveListingBytes, false, lsFilesArgs(dir, true)...)
	if err != nil {
		return out, err
	}
	paths, badNames := parseLsFiles(raw, exceeded)
	if dir != "" && isSelfOnly(paths, dir) {
		// `--directory` collapses a wholly untracked directory to itself, which is
		// the answer wanted for a child but not for the directory being listed:
		// list it once more without the collapse. That second walk is bounded only
		// by the listing cap, which the plan's §13 states. Git never enters a
		// nested repository, so an empty second listing is either that or an empty
		// directory.
		if raw, exceeded, err = runLiveGit(ctx, root, liveListingBytes, false, lsFilesArgs(dir, false)...); err != nil {
			return out, err
		}
		paths, badNames = parseLsFiles(raw, exceeded)
		if len(paths) == 0 || isSelfOnly(paths, dir) {
			out.State = "empty"
			if hasGitDir(root, dir) {
				out.State = "nested-repository"
			}
			return out, nil
		}
	}
	children := foldChildren(dir, paths)
	out.UnreadableNames = badNames
	out.Truncated = exceeded
	if len(children) == 0 {
		state, err := emptyState(ctx, root, dir)
		if err != nil {
			return out, err
		}
		out.State = state
		return out, nil
	}
	entries := sortedEntries(children)
	if cfg.MaxEntriesPerDir > 0 && len(entries) > cfg.MaxEntriesPerDir {
		out.Dropped = len(entries) - cfg.MaxEntriesPerDir
		out.Truncated = true
		entries = entries[:cfg.MaxEntriesPerDir]
	}
	out.Entries = statEntries(root, dir, entries)
	return out, nil
}

// ReadTreeFile reads one file git lists, bounded by the configured bytes.
func ReadTreeFile(ctx context.Context, root, path string, cfg TreeConfig) (TreeFile, error) {
	if !ValidTreePath(path) {
		return TreeFile{}, treeErr("invalid-path", fmt.Sprintf("%q is not a repository path outside .git", path))
	}
	if found, err := inTree(ctx, root, path); err != nil {
		return TreeFile{}, err
	} else if !found {
		return TreeFile{}, treeErr("file-not-in-tree", fmt.Sprintf("%s is not a file git lists in this checkout", path))
	}
	read, err := statAndReadBounded(root, path, cfg.MaxReadBytes)
	if err != nil {
		return TreeFile{}, readProblem(err, path)
	}
	out := TreeFile{Path: path, Size: read.Size, ModTime: read.ModTime.Unix()}
	switch {
	case read.Mode&os.ModeSymlink != 0:
		out.Kind, out.Target = "symlink", read.LinkTarget
		return out, nil
	case !read.Mode.IsRegular():
		out.Kind = "special"
		return out, nil
	case looksBinary(read.Body):
		out.Kind = "binary"
		return out, nil
	}
	body := read.Body
	if read.Truncated {
		body = trimToLine(body)
	}
	out.Kind, out.Text, out.Bytes, out.Truncated = "text", string(body), len(body), read.Truncated
	return out, nil
}

func treeErr(code, message string) error { return &LiveDiffError{Code: code, Message: message} }

// readProblem names why the secure reader refused a listed path.
func readProblem(err error, path string) error {
	switch {
	case errors.Is(err, ErrUnsupportedPlatform):
		return treeErr("unsupported-platform", err.Error())
	case errors.Is(err, os.ErrNotExist):
		return treeErr("not-a-file", fmt.Sprintf("%s is listed but has no file", path))
	case errors.Is(err, syscall.ENOTDIR), errors.Is(err, syscall.ELOOP):
		return treeErr("not-a-directory", fmt.Sprintf("%s is not reachable through directories", path))
	}
	return treeErr("unreadable", err.Error())
}

func looksBinary(body []byte) bool {
	sniff := body
	if len(sniff) > binarySniffBytes {
		sniff = sniff[:binarySniffBytes]
	}
	return bytes.IndexByte(sniff, 0) >= 0 || !utf8.Valid(sniff)
}

type listedPath struct {
	path      string
	mode      string
	untracked bool
	directory bool
}

// parseLsFiles reads `ls-files --stage --others -z` records: stage lines carry
// `<mode> <oid> <stage>\t<path>`; bare paths are untracked; a trailing slash is
// an untracked directory (`--directory`). A listing the runner cut mid-record
// loses its last record rather than inventing a name from the fragment.
func parseLsFiles(raw []byte, exceeded bool) ([]listedPath, int) {
	parts := bytes.Split(raw, []byte{0})
	if exceeded && len(parts) > 0 {
		parts = parts[:len(parts)-1]
	}
	out := []listedPath{}
	badNames := 0
	for _, part := range parts {
		if len(part) == 0 {
			continue
		}
		if !utf8.Valid(part) {
			badNames++
			continue
		}
		record := string(part)
		if meta, path, found := strings.Cut(record, "\t"); found && strings.Count(meta, " ") == 2 {
			out = append(out, listedPath{path: path, mode: strings.Fields(meta)[0]})
			continue
		}
		if strings.HasSuffix(record, "/") {
			out = append(out, listedPath{path: strings.TrimSuffix(record, "/"), untracked: true, directory: true})
			continue
		}
		out = append(out, listedPath{path: record, untracked: true})
	}
	return out, badNames
}

func isSelfOnly(paths []listedPath, dir string) bool {
	return len(paths) == 1 && paths[0].directory && paths[0].path == dir
}

func lsFilesArgs(dir string, directory bool) []string {
	args := []string{"--literal-pathspecs", "ls-files", "--stage", "--others", "--exclude-standard", "-z"}
	if directory {
		args = append(args, "--directory")
	}
	if dir != "" {
		args = append(args, "--", dir+"/")
	}
	return args
}

func kindOfMode(mode string) string {
	switch mode {
	case "120000":
		return "symlink"
	case "160000":
		return "submodule"
	}
	return "file"
}

// foldChildren turns the recursive listing under dir into immediate children.
// A directory is untracked only while every path git listed beneath it is; a
// path outside the requested prefix is a git invariant violation and is never
// shown.
func foldChildren(dir string, paths []listedPath) map[string]*TreeEntry {
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	children := map[string]*TreeEntry{}
	for _, item := range paths {
		if !strings.HasPrefix(item.path, prefix) {
			continue
		}
		name, _, nested := strings.Cut(item.path[len(prefix):], "/")
		if name == "" {
			continue
		}
		existing := children[name]
		if nested || item.directory {
			if existing == nil {
				children[name] = &TreeEntry{Name: name, Path: prefix + name, Kind: "dir", Untracked: item.untracked}
				continue
			}
			existing.Kind = "dir"
			existing.Untracked = existing.Untracked && item.untracked
			continue
		}
		if existing != nil {
			continue
		}
		children[name] = &TreeEntry{Name: name, Path: prefix + name, Kind: kindOfMode(item.mode), Untracked: item.untracked}
	}
	return children
}

func sortedEntries(children map[string]*TreeEntry) []TreeEntry {
	out := make([]TreeEntry, 0, len(children))
	for _, entry := range children {
		out = append(out, *entry)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Kind == "dir") != (out[j].Kind == "dir") {
			return out[i].Kind == "dir"
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// statEntries fills size and mtime from one open directory fd; a tracked path
// with no inode becomes `missing`. If the directory itself cannot be opened
// the entries keep git's word alone (no sizes, no missing marks); the listing
// already proved git could read it, so this is a race with a deletion, and
// the next listing says so.
func statEntries(root, dir string, entries []TreeEntry) []TreeEntry {
	fd, err := openSecureDir(root, dir)
	if err != nil {
		return entries
	}
	defer closeFD(fd)
	for index := range entries {
		entry := &entries[index]
		if entry.Kind == "dir" || entry.Kind == "submodule" {
			continue
		}
		stat, err := statInDir(fd, entry.Name)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				entry.Kind = "missing"
			}
			continue
		}
		entry.Size, entry.ModTime = stat.Size, stat.ModTime.Unix()
		switch {
		case stat.Mode&os.ModeSymlink != 0:
			entry.Kind = "symlink"
		case stat.Mode.IsDir():
			entry.Kind = "dir"
		case !stat.Mode.IsRegular():
			entry.Kind = "special"
		}
	}
	return entries
}

// hasGitDir says whether dir holds any `.git` entry (a directory or gitfile,
// even a dangling link): stricter than git's own test, never looser.
func hasGitDir(root, dir string) bool {
	fd, err := openSecureDir(root, dir)
	if err != nil {
		return false
	}
	defer closeFD(fd)
	_, err = statInDir(fd, ".git")
	return err == nil
}

// emptyState says why a directory listed nothing: absent, ignored, or empty.
func emptyState(ctx context.Context, root, dir string) (string, error) {
	if dir == "" {
		return "empty", nil
	}
	fd, err := openSecureDir(root, dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "not-in-checkout", nil
		}
		return "", readProblem(err, dir)
	}
	closeFD(fd)
	// check-ignore takes pathnames, not pathspecs, so the literal flag is not
	// accepted; the dir has already passed the path rule.
	_, _, err = runLiveGit(ctx, root, liveListingBytes, false, "check-ignore", "-q", "--", dir+"/")
	if err == nil {
		return "ignored", nil
	}
	var typed *LiveDiffError
	if errors.As(err, &typed) && typed.ExitCode == 1 {
		return "empty", nil
	}
	return "", err
}

// inTree proves a path belongs to git's population with an exact pathspec.
func inTree(ctx context.Context, root, path string) (bool, error) {
	raw, _, err := runLiveGit(ctx, root, liveListingBytes, false, "--literal-pathspecs", "ls-files", "--stage", "--others", "--exclude-standard", "-z", "--", path)
	if err != nil {
		return false, err
	}
	paths, _ := parseLsFiles(raw, false)
	for _, item := range paths {
		if item.path == path && !item.directory {
			return true, nil
		}
	}
	return false, nil
}
