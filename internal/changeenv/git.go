package changeenv

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"crossing-guard/store"
)

type SnapshotInput struct {
	SessionID, Runtime, Title, RepoDir, Base string
	RetainBodies                             bool
}

const (
	GitTreeProtocol          = "git-tree-v2"
	maxGitManifestEntries    = 20000
	maxGitManifestFileBytes  = 16 << 20
	maxGitUntrackedAggregate = 256 << 20
)

// GitManifestEntry is one bounded, non-ignored path in the captured checkout.
// Kind is structural filesystem evidence, not a language or project convention.
type GitManifestEntry struct {
	Path    string `json:"path"`
	Mode    string `json:"mode"`
	Kind    string `json:"kind"` // regular, symlink, gitlink, or missing
	Tracked bool   `json:"tracked"`
	Present bool   `json:"present"`
	Size    int64  `json:"size,omitempty"`
}

// GitCapture is the immutable source observation shared by revision evidence and
// durable understanding. Manifest is sorted and safe to pass to analyzers: they
// still must reject every Kind other than regular.
type GitCapture struct {
	Repository          Repository `json:"repository"`
	Base, Head, Version string
	Items               []store.ChangeItem `json:"items"`
	Manifest            []GitManifestEntry `json:"manifest"`
	SnapshotDigest      string             `json:"snapshot_digest"`
	Sparse              bool               `json:"sparse_checkout"`
	Attempts            int                `json:"attempts"`
}

type cappedOutput struct {
	buf      bytes.Buffer
	max      int
	exceeded bool
	total    int
	hasher   hash.Hash
}

func (w *cappedOutput) Write(p []byte) (int, error) {
	n := len(p)
	w.total += n
	if w.hasher != nil {
		_, _ = w.hasher.Write(p)
	}
	remaining := w.max + 1 - w.buf.Len()
	if remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		_, _ = w.buf.Write(p[:remaining])
	}
	if w.buf.Len() > w.max || remaining < len(p) {
		w.exceeded = true
	}
	return n, nil
}

func runBoundedGitBody(ctx context.Context, root string, args ...string) (body []byte, total int, bodyDigest string, bounded bool, err error) {
	hasher := sha256.New()
	out := &cappedOutput{max: maxCheckpointPathBytes, hasher: hasher}
	argv := append([]string{"-C", root, "--no-optional-locks", "-c", "core.quotepath=false"}, args...)
	cmd := exec.CommandContext(ctx, "git", argv...)
	cmd.Env = append(cmd.Environ(), "LC_ALL=C")
	cmd.Stdout = out
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, out.total, "", out.exceeded, ctx.Err()
		}
		return nil, out.total, "", out.exceeded, err
	}
	bodyDigest = "sha256-v1:" + fmt.Sprintf("%x", hasher.Sum(nil))
	if out.exceeded {
		return nil, out.total, bodyDigest, true, nil
	}
	body = make([]byte, out.buf.Len())
	copy(body, out.buf.Bytes())
	return body, out.total, bodyDigest, false, nil
}

func gitLayerPatch(ctx context.Context, root, base, head, layer, path string) (body []byte,
	total int, bodyDigest string, bounded bool, err error) {
	args := []string{"diff", "--binary", "--full-index", "--no-ext-diff"}
	switch layer {
	case "committed":
		args = append(args, base, head)
	case "index":
		args = append(args, "--cached", head)
	case "worktree":
		// With no revision argument Git compares the index to the filesystem.
	default:
		return nil, 0, "", false, fmt.Errorf("unsupported checkpoint layer %q", layer)
	}
	args = append(args, "--", path)
	body, total, bodyDigest, bounded, err = runBoundedGitBody(ctx, root, args...)
	if err == nil && len(body) > 0 && !utf8.Valid(body) {
		return nil, total, "", bounded, fmt.Errorf("git diff returned non-UTF-8 data")
	}
	return body, total, bodyDigest, bounded, err
}

func gitLayerContent(ctx context.Context, repo Repository, capture GitCapture,
	item store.ChangeItem) (body []byte, total int, bodyDigest string, bounded bool,
	sourceIdentity string, err error) {
	switch item.Layer {
	case "committed":
		sourceIdentity = "git-blob:" + capture.Head + ":" + item.Path
		body, total, bodyDigest, bounded, err = runBoundedGitBody(ctx, repo.Root,
			"show", capture.Head+":"+item.Path)
	case "index":
		sourceIdentity = "git-index:" + capture.SnapshotDigest + ":" + item.Path
		body, total, bodyDigest, bounded, err = runBoundedGitBody(ctx, repo.Root,
			"show", ":"+item.Path)
	case "worktree":
		sourceIdentity = "filesystem:" + capture.SnapshotDigest + ":" + item.Path
		body, total, bodyDigest, bounded, err = readCheckpointRegular(repo.Root, item.Path)
	case "untracked":
		sourceIdentity = "absent-index->filesystem:" + capture.SnapshotDigest + ":" + item.Path
		body, total, bodyDigest, bounded, err = readCheckpointRegular(repo.Root, item.Path)
	default:
		err = fmt.Errorf("unsupported checkpoint layer %q", item.Layer)
	}
	return
}

// snapshotTestHook is nil in production. Tests use it to mutate a repository
// between the two consistency observations without adding another collector path.
var snapshotTestHook func(attempt int)

// checkpointReadTestHook is nil in production. Tests use it after the safe file
// descriptor is open to prove a pathname replacement cannot redirect retained bytes.
var checkpointReadTestHook func()

// checkpointPathOpenTestHook is nil in production. Tests use it immediately before
// component-wise opening to replace a parent path and verify the outer capture fails.
var checkpointPathOpenTestHook func(relative string)

// manifestPathOpenTestHook is nil in production. Tests use it immediately before
// component-wise manifest inspection to exercise parent replacement races.
var manifestPathOpenTestHook func(relative string)

func expectedMissingCheckpointContent(item store.ChangeItem, err error) bool {
	if item.Status != "D" {
		return false
	}
	switch item.Layer {
	case "committed", "index":
		return true
	case "worktree":
		return errors.Is(err, os.ErrNotExist)
	default:
		return false
	}
}

func gitRawContext(ctx context.Context, root string, args ...string) ([]byte, error) {
	argv := append(append([]string{"-C", root, "--no-optional-locks"}, gitReadGuard(ctx)...), args...)
	cmd := exec.CommandContext(ctx, "git", argv...)
	cmd.Env = append(cmd.Environ(), "LC_ALL=C")
	out := &cappedOutput{max: 16 << 20}
	cmd.Stdout = out
	err := cmd.Run()
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), ctx.Err())
		}
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	if out.exceeded {
		return nil, fmt.Errorf("git output exceeds 16 MiB")
	}
	b := out.buf.Bytes()
	if !utf8.Valid(b) {
		return nil, fmt.Errorf("git returned a non-UTF-8 path")
	}
	return b, nil
}
func resolveOID(ctx context.Context, root, ref string) (string, error) {
	b, e := gitRawContext(ctx, root, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	return strings.TrimSpace(string(b)), e
}

func observeGit(ctx context.Context, repo Repository, baseRef string) (GitCapture, error) {
	base, err := resolveOID(ctx, repo.Root, baseRef)
	if err != nil {
		return GitCapture{}, err
	}
	head, err := resolveOID(ctx, repo.Root, "HEAD")
	if err != nil {
		return GitCapture{}, err
	}
	ver, _ := gitRawContext(ctx, repo.Root, "--version")
	committed, err := gitRawContext(ctx, repo.Root, "-c", "core.quotepath=false", "-c", "diff.renames=true", "diff", "--raw", "-z", "--no-abbrev", "--find-renames=50%", "--find-copies=50%", base, head)
	if err != nil {
		return GitCapture{}, err
	}
	status, err := gitRawContext(ctx, repo.Root, "-c", "core.quotepath=false", "status", "--porcelain=v2", "-z", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return GitCapture{}, err
	}
	// The raw/status protocols describe membership and status but tracked worktree
	// object IDs can be all-zero. Hash a bounded binary diff into the consistency
	// witness so same-status content mutation cannot pass the double observation.
	worktreeContent, err := gitRawContext(ctx, repo.Root, "-c", "core.quotepath=false", "diff", "--binary", "--full-index", "--no-ext-diff", "HEAD")
	if err != nil {
		return GitCapture{}, err
	}
	items, err := parseRawDiff(committed, "committed")
	if err != nil {
		return GitCapture{}, err
	}
	more, err := parseStatus(status)
	if err != nil {
		return GitCapture{}, err
	}
	items = append(items, more...)
	sort.Slice(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Layer != b.Layer {
			return a.Layer < b.Layer
		}
		return a.Status < b.Status
	})
	manifest, untrackedContentDigest, err := buildGitManifest(ctx, repo.Root)
	if err != nil {
		return GitCapture{}, err
	}
	sparse := false
	if b, e := gitRawContext(ctx, repo.Root, "config", "--bool", "core.sparseCheckout"); e == nil {
		sparse = strings.TrimSpace(string(b)) == "true"
	}
	canon, _ := json.Marshal(struct {
		Protocol, Base, Head, WorktreeContentDigest, UntrackedContentDigest string
		Items                                                               []store.ChangeItem
		Manifest                                                            []GitManifestEntry
		Sparse                                                              bool
	}{GitTreeProtocol, base, head, digest("sha256-v1:", worktreeContent), untrackedContentDigest, items, manifest, sparse})
	return GitCapture{Repository: repo, Base: base, Head: head, Version: strings.TrimSpace(string(ver)), Items: items, Manifest: manifest, SnapshotDigest: digest("git-tree-v2-sha256:", canon), Sparse: sparse}, nil
}

// ObserveGit performs one bounded source observation. Durable analysis uses it
// on both sides of analyzer work; ordinary revision capture uses CaptureGit's
// adjacent double observation.
func ObserveGit(ctx context.Context, repo Repository, baseRef string) (GitCapture, error) {
	return observeGit(ctx, repo, baseRef)
}

func SameGitSource(a, b GitCapture) bool {
	return a.Repository.ID == b.Repository.ID && a.Repository.CheckoutID == b.Repository.CheckoutID &&
		a.Base == b.Base && a.Head == b.Head && a.SnapshotDigest == b.SnapshotDigest
}

type gitStageEntry struct {
	mode  string
	stage string
}

type manifestPathEvidence struct {
	Mode       os.FileMode
	Size       int64
	Body       []byte
	LinkTarget string
}

func parseGitStage(raw []byte) (map[string]gitStageEntry, error) {
	out := map[string]gitStageEntry{}
	for _, record := range bytes.Split(raw, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		meta, pathBytes, ok := bytes.Cut(record, []byte{'\t'})
		if !ok || len(pathBytes) == 0 {
			return nil, fmt.Errorf("malformed git index manifest record")
		}
		fields := strings.Fields(string(meta))
		if len(fields) != 3 {
			return nil, fmt.Errorf("malformed git index manifest metadata")
		}
		path := string(pathBytes)
		if prior, exists := out[path]; exists && (prior.stage != fields[2] || prior.mode != fields[0]) {
			return nil, fmt.Errorf("unmerged index path %q has multiple stages", path)
		}
		if fields[2] != "0" {
			return nil, fmt.Errorf("unmerged index path %q cannot be analyzed", path)
		}
		out[path] = gitStageEntry{mode: fields[0], stage: fields[2]}
	}
	return out, nil
}

func validManifestPath(path string) bool {
	if path == "" || filepath.IsAbs(path) || strings.ContainsRune(path, '\x00') {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	return clean == path && clean != ".." && !strings.HasPrefix(clean, "../")
}

// buildGitManifest enumerates exactly Git's tracked and non-ignored untracked
// population. It never follows a symlink and hashes untracked content so a
// same-status mutation changes the snapshot identity.
func buildGitManifest(ctx context.Context, root string) ([]GitManifestEntry, string, error) {
	pathsRaw, err := gitRawContext(ctx, root, "-c", "core.quotepath=false", "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, "", err
	}
	stageRaw, err := gitRawContext(ctx, root, "-c", "core.quotepath=false", "ls-files", "--stage", "-z")
	if err != nil {
		return nil, "", err
	}
	staged, err := parseGitStage(stageRaw)
	if err != nil {
		return nil, "", err
	}
	pathRecords := bytes.Split(pathsRaw, []byte{0})
	manifest := make([]GitManifestEntry, 0, len(pathRecords))
	untrackedHash := sha256.New()
	untrackedBytes := int64(0)
	seen := map[string]bool{}
	for _, pathBytes := range pathRecords {
		if len(pathBytes) == 0 {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		path := string(pathBytes)
		if !validManifestPath(path) {
			return nil, "", fmt.Errorf("git returned unsafe manifest path %q", path)
		}
		if seen[path] {
			continue
		}
		seen[path] = true
		if len(manifest) >= maxGitManifestEntries {
			return nil, "", fmt.Errorf("git manifest exceeds %d paths", maxGitManifestEntries)
		}
		stage, tracked := staged[path]
		entry := GitManifestEntry{Path: path, Mode: stage.mode, Tracked: tracked}
		if manifestPathOpenTestHook != nil {
			manifestPathOpenTestHook(path)
		}
		evidence, pathErr := readManifestPath(root, path, !tracked)
		if stage.mode == "160000" {
			entry.Kind = "gitlink"
			if pathErr == nil {
				entry.Present = true
			} else if !os.IsNotExist(pathErr) {
				return nil, "", fmt.Errorf("inspect gitlink %q: %w", path, pathErr)
			}
			manifest = append(manifest, entry)
			continue
		}
		if pathErr != nil {
			if tracked && os.IsNotExist(pathErr) {
				entry.Kind = "missing"
				manifest = append(manifest, entry)
				continue
			}
			return nil, "", fmt.Errorf("inspect manifest path %q: %w", path, pathErr)
		}
		entry.Present = true
		entry.Size = evidence.Size
		switch {
		case evidence.Mode&os.ModeSymlink != 0:
			entry.Kind = "symlink"
			if !tracked {
				entry.Mode = "untracked:symlink"
				untrackedBytes += int64(len(evidence.LinkTarget))
				_, _ = fmt.Fprintf(untrackedHash, "%s\x00%s\x00%s\x00", path, entry.Mode,
					evidence.LinkTarget)
			}
		case evidence.Mode.IsRegular():
			entry.Kind = "regular"
			if !tracked {
				entry.Mode = fmt.Sprintf("untracked:%04o", evidence.Mode.Perm())
				untrackedBytes += int64(len(evidence.Body))
				_, _ = fmt.Fprintf(untrackedHash, "%s\x00%s\x00%d\x00", path, entry.Mode,
					len(evidence.Body))
				_, _ = untrackedHash.Write(evidence.Body)
				_, _ = untrackedHash.Write([]byte{0})
			}
		default:
			return nil, "", fmt.Errorf("manifest path %q is a special file (%s)", path,
				evidence.Mode.String())
		}
		if untrackedBytes > maxGitUntrackedAggregate {
			return nil, "", fmt.Errorf("untracked manifest content exceeds 256 MiB")
		}
		manifest = append(manifest, entry)
	}
	sort.Slice(manifest, func(i, j int) bool { return manifest[i].Path < manifest[j].Path })
	return manifest, fmt.Sprintf("sha256-v1:%x", untrackedHash.Sum(nil)), nil
}

// CaptureGit performs the bounded double observation used by both C6 revision
// evidence and C7 durable analysis. A caller receives no capture unless two
// consecutive observations agree.
func CaptureGit(ctx context.Context, repo Repository, baseRef string) (GitCapture, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	for attempt := 1; attempt <= 3; attempt++ {
		a, err := observeGit(ctx, repo, baseRef)
		if err != nil {
			return GitCapture{}, err
		}
		if snapshotTestHook != nil {
			snapshotTestHook(attempt)
		}
		b, err := observeGit(ctx, repo, baseRef)
		if err != nil {
			return GitCapture{}, err
		}
		if a.SnapshotDigest == b.SnapshotDigest && a.Base == b.Base && a.Head == b.Head {
			b.Attempts = attempt
			return b, nil
		}
	}
	return GitCapture{}, fmt.Errorf("repository changed during capture")
}

func parseRawDiff(raw []byte, layer string) ([]store.ChangeItem, error) {
	parts := bytes.Split(raw, []byte{0})
	out := []store.ChangeItem{}
	for i := 0; i < len(parts) && len(parts[i]) > 0; i++ {
		meta := string(parts[i])
		if !strings.HasPrefix(meta, ":") {
			return nil, fmt.Errorf("malformed raw diff record")
		}
		fields := strings.Fields(meta)
		if len(fields) != 5 {
			return nil, fmt.Errorf("malformed raw diff metadata")
		}
		status := fields[4]
		i++
		if i >= len(parts) {
			return nil, fmt.Errorf("raw diff missing path")
		}
		p := string(parts[i])
		item := store.ChangeItem{Path: p, Layer: layer, Status: status}
		if strings.HasPrefix(status, "R") || strings.HasPrefix(status, "C") {
			item.OldPath = p
			i++
			if i >= len(parts) {
				return nil, fmt.Errorf("raw rename missing destination")
			}
			item.Path = string(parts[i])
		}
		out = append(out, item)
	}
	return out, nil
}

func parseStatus(raw []byte) ([]store.ChangeItem, error) {
	parts := bytes.Split(raw, []byte{0})
	out := []store.ChangeItem{}
	for i := 0; i < len(parts) && len(parts[i]) > 0; i++ {
		s := string(parts[i])
		switch s[0] {
		case '?':
			out = append(out, store.ChangeItem{Path: strings.TrimPrefix(s, "? "), Layer: "untracked", Status: "?"})
		case '1':
			f := strings.SplitN(s, " ", 9)
			if len(f) != 9 {
				return nil, fmt.Errorf("malformed porcelain v2 record")
			}
			out = appendXY(out, f[8], "", f[1])
		case '2':
			f := strings.SplitN(s, " ", 10)
			if len(f) != 10 {
				return nil, fmt.Errorf("malformed porcelain v2 rename")
			}
			i++
			if i >= len(parts) {
				return nil, fmt.Errorf("rename missing old path")
			}
			out = appendXY(out, f[9], string(parts[i]), f[1])
		case 'u':
			f := strings.SplitN(s, " ", 11)
			if len(f) != 11 {
				return nil, fmt.Errorf("malformed unmerged record")
			}
			out = append(out, store.ChangeItem{Path: f[10], Layer: "worktree", Status: "U"})
		default:
			return nil, fmt.Errorf("unsupported porcelain record %q", s[0])
		}
	}
	return out, nil
}
func appendXY(out []store.ChangeItem, path, old, xy string) []store.ChangeItem {
	if len(xy) > 0 && xy[0] != '.' {
		out = append(out, store.ChangeItem{Path: path, OldPath: old, Layer: "index", Status: string(xy[0])})
	}
	if len(xy) > 1 && xy[1] != '.' {
		out = append(out, store.ChangeItem{Path: path, OldPath: old, Layer: "worktree", Status: string(xy[1])})
	}
	return out
}

func RecordSnapshot(ix *store.Index, in SnapshotInput) (*store.ChangeRecord, error) {
	r, err := CaptureSnapshotRecord(context.Background(), in)
	if err != nil {
		return nil, err
	}
	if err := ix.AppendChange(r); err != nil {
		return nil, err
	}
	return r, nil
}

// CaptureSnapshotRecord runs the existing bounded Git observation and constructs the
// existing change-record evidence without persisting it. Automatic attachment capture
// uses this seam so the store can atomically append the revision and checkpoint link.
func CaptureSnapshotRecord(ctx context.Context, in SnapshotInput) (*store.ChangeRecord, error) {
	record, _, err := CaptureCheckpointEvidence(ctx, in)
	return record, err
}

const (
	maxCheckpointPathBytes  = 1 << 20
	maxCheckpointTotalBytes = 8 << 20
	maxCheckpointPatchPaths = 128
)

func appendCheckpointLimitation(payload *store.CheckpointPayload, limitation string) {
	if payload.Limitation == "" {
		payload.Limitation = limitation
		return
	}
	payload.Limitation += "," + limitation
}

func captureCheckpointPayload(ctx context.Context, in SnapshotInput, repo Repository,
	capture GitCapture, item store.ChangeItem, retained, patchPaths *int) (store.CheckpointPayload, error) {
	payload := store.CheckpointPayload{Path: item.Path, Layer: item.Layer, Status: item.Status,
		ContentCompleteness: "unavailable", PatchCompleteness: "unavailable"}
	if !validManifestPath(item.Path) {
		payload.Limitation = "unsafe-path"
		return payload, nil
	}
	contentBody, contentBytes, contentDigest, contentBounded, sourceIdentity, contentErr :=
		gitLayerContent(ctx, repo, capture, item)
	payload.SourceIdentity, payload.ContentBytes, payload.ContentDigest = sourceIdentity,
		contentBytes, contentDigest
	switch {
	case contentErr != nil:
		if !expectedMissingCheckpointContent(item, contentErr) {
			return store.CheckpointPayload{}, fmt.Errorf("secure checkpoint content %s (%s): %w",
				item.Path, item.Layer, contentErr)
		}
		payload.Limitation = "content-source-unavailable"
	case contentBounded || *retained+len(contentBody) > maxCheckpointTotalBytes || !in.RetainBodies:
		payload.ContentCompleteness = "metadata-only"
		switch {
		case contentBounded:
			payload.Limitation = "content-bounded"
		case *retained+len(contentBody) > maxCheckpointTotalBytes:
			payload.Limitation = "content-total-bounded"
		default:
			payload.Limitation = "body-retention-disabled"
		}
	default:
		payload.ContentCompleteness = "complete"
		payload.ContentPayload = contentBody
		*retained += len(contentBody)
	}
	if item.Layer == "untracked" {
		return payload, nil
	}
	if *patchPaths >= maxCheckpointPatchPaths || *retained >= maxCheckpointTotalBytes {
		payload.PatchCompleteness = "metadata-only"
		if *patchPaths >= maxCheckpointPatchPaths {
			appendCheckpointLimitation(&payload, "patch-count-bounded")
		} else {
			appendCheckpointLimitation(&payload, "patch-total-bounded")
		}
		return payload, nil
	}
	*patchPaths++
	patchBody, patchBytes, patchDigest, patchBounded, patchErr := gitLayerPatch(ctx,
		repo.Root, capture.Base, capture.Head, item.Layer, item.Path)
	payload.PatchBytes, payload.PatchDigest = patchBytes, patchDigest
	switch {
	case patchErr != nil:
		appendCheckpointLimitation(&payload, "patch-capture-failed")
	case patchBytes == 0:
		payload.PatchCompleteness = "unavailable"
	case patchBounded || *retained+len(patchBody) > maxCheckpointTotalBytes || !in.RetainBodies:
		payload.PatchCompleteness = "metadata-only"
		if !in.RetainBodies && !patchBounded {
			appendCheckpointLimitation(&payload, "body-retention-disabled")
		} else {
			appendCheckpointLimitation(&payload, "patch-bounded")
		}
	default:
		payload.PatchCompleteness = "complete"
		payload.PatchPayload = patchBody
		*retained += len(patchBody)
	}
	return payload, nil
}

// CaptureCheckpointEvidence extends the existing bounded Git observer with bounded
// per-changed-path bodies. It uses the same repository resolver and Git capture, then
// re-observes the source after reading bodies so a concurrent mutation cannot be stored
// as if it belonged to the stable checkpoint.
func CaptureCheckpointEvidence(ctx context.Context, in SnapshotInput) (*store.ChangeRecord, []store.CheckpointPayload, error) {
	if strings.TrimSpace(in.SessionID) == "" || strings.TrimSpace(in.RepoDir) == "" || strings.TrimSpace(in.Base) == "" {
		return nil, nil, fmt.Errorf("snapshot requires session, repo, and base")
	}
	repo, err := ResolveRepository(in.RepoDir)
	if err != nil {
		return nil, nil, err
	}
	start := time.Now().UnixNano()
	got, err := CaptureGit(ctx, repo, in.Base)
	if err != nil {
		return nil, nil, err
	}
	payloads := make([]store.CheckpointPayload, 0, len(got.Items))
	retained := 0
	patchPaths := 0
	seen := map[string]bool{}
	for _, item := range got.Items {
		key := item.Path + "\x00" + item.Layer
		if seen[key] {
			continue
		}
		seen[key] = true
		payload, err := captureCheckpointPayload(ctx, in, repo, got, item, &retained, &patchPaths)
		if err != nil {
			return nil, nil, err
		}
		payloads = append(payloads, payload)
	}
	after, err := ObserveGit(ctx, repo, in.Base)
	if err != nil {
		return nil, nil, err
	}
	if !SameGitSource(got, after) {
		return nil, nil, fmt.Errorf("repository changed while retaining checkpoint payload")
	}
	end := time.Now().UnixNano()
	layers, _ := json.Marshal([]string{"committed", "index", "worktree", "untracked"})
	r := &store.ChangeRecord{SessionID: in.SessionID, RepositoryID: repo.ID, CheckoutID: repo.CheckoutID, RepositoryIdentityKind: repo.IdentityKind, CheckoutRoot: repo.Root, SessionRuntimeClaim: in.Runtime, SessionTitleClaim: in.Title, Kind: "revision", EvidenceClass: "observed", SourceKind: "git", SourceRef: "git:" + got.Head, SourceDisplay: "Git snapshot", SourceDigest: got.SnapshotDigest, RecordedAt: end, CaptureStartedAt: start, CaptureEndedAt: end, BaseRevision: got.Base, HeadRevision: got.Head, GitVersion: got.Version, SnapshotDigest: got.SnapshotDigest, CaptureAttempts: got.Attempts, IncludedLayers: string(layers), SparseCheckout: got.Sparse, CommonDirDigest: digest("sha256-v1:", []byte(repo.CommonDir)), NonAtomic: true, Items: got.Items}
	return r, payloads, nil
}
