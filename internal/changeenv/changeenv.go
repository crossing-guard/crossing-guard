package changeenv

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"crossing-guard/store"
)

const maxSourceBytes = 16 << 20

// sourceTestHook is nil in production. It lets tests prove a source mutation is
// refused at the exact pre/post identity boundary.
var sourceTestHook func()

type Repository struct {
	ID           string `json:"id"`
	CheckoutID   string `json:"checkout_id"`
	IdentityKind string `json:"identity_kind"`
	Root         string `json:"root"`
	CommonDir    string `json:"-"`
}

type Source struct {
	Ref     string
	Display string
	Digest  string
}

func digest(prefix string, b []byte) string {
	h := sha256.Sum256(b)
	return prefix + hex.EncodeToString(h[:])
}

func sourceFile(repo Repository, path string) (Source, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return Source{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return Source{}, fmt.Errorf("source symlinks are refused: %s", path)
	}
	if info.Size() > maxSourceBytes {
		return Source{}, fmt.Errorf("source is %d bytes; maximum is %d", info.Size(), maxSourceBytes)
	}
	f, err := os.Open(path)
	if err != nil {
		return Source{}, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return Source{}, err
	}
	if !os.SameFile(info, opened) {
		return Source{}, fmt.Errorf("source changed before hashing")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, maxSourceBytes+1))
	if err != nil {
		return Source{}, err
	}
	if n > maxSourceBytes {
		return Source{}, fmt.Errorf("source exceeds %d bytes", maxSourceBytes)
	}
	if sourceTestHook != nil {
		sourceTestHook()
	}
	openedAfter, err := f.Stat()
	if err != nil {
		return Source{}, err
	}
	after, err := os.Lstat(path)
	if err != nil {
		return Source{}, err
	}
	if !os.SameFile(info, after) || !os.SameFile(opened, openedAfter) || opened.Size() != openedAfter.Size() || !opened.ModTime().Equal(openedAfter.ModTime()) || info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) {
		return Source{}, fmt.Errorf("source changed while hashing")
	}
	abs, _ := filepath.Abs(path)
	ref := ""
	display := filepath.Base(path)
	if rel, e := filepath.Rel(repo.Root, abs); e == nil && !outside(rel) {
		ref = filepath.ToSlash(rel)
	} else {
		ref = digest("external-sha256-v1:", []byte(abs))
	}
	return Source{Ref: ref, Display: display, Digest: "sha256-v1:" + hex.EncodeToString(h.Sum(nil))}, nil
}

// SourceFile returns the bounded, mutation-checked identity used for an
// explicitly selected source document. It stores no source bytes.
func SourceFile(repo Repository, path string) (Source, error) { return sourceFile(repo, path) }

func outside(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel)
}

func normalizePaths(repo Repository, paths, symbols []string) ([]store.ChangeItem, error) {
	seen := map[string]bool{}
	out := []store.ChangeItem{}
	add := func(p, s string) error {
		if p == "" {
			return fmt.Errorf("empty path")
		}
		if !filepath.IsAbs(p) {
			slashPath := filepath.ToSlash(p)
			for _, component := range strings.FieldsFunc(slashPath, func(r rune) bool { return r == '/' }) {
				if component == "." || component == ".." {
					return fmt.Errorf("path contains forbidden component: %s", p)
				}
			}
			if strings.Contains(slashPath, "//") || strings.HasSuffix(slashPath, "/") {
				return fmt.Errorf("path contains an empty component: %s", p)
			}
		}
		abs := p
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(repo.Root, p)
		}
		real, err := resolveAllowMissing(abs)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(repo.Root, real)
		if err != nil || outside(rel) {
			return fmt.Errorf("path outside repository: %s", p)
		}
		rel = filepath.ToSlash(filepath.Clean(rel))
		if rel == "." || strings.Contains(rel, "/../") {
			return fmt.Errorf("invalid repository path: %s", p)
		}
		key := rel + "\x00" + s
		if !seen[key] {
			seen[key] = true
			out = append(out, store.ChangeItem{Path: rel, Symbol: s})
		}
		return nil
	}
	for _, p := range paths {
		if err := add(p, ""); err != nil {
			return nil, err
		}
	}
	for _, v := range symbols {
		p, s, ok := strings.Cut(v, "::")
		if !ok || s == "" {
			return nil, fmt.Errorf("symbol must be PATH::SYMBOL: %s", v)
		}
		if err := add(p, s); err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path == out[j].Path {
			return out[i].Symbol < out[j].Symbol
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

func resolveAllowMissing(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	probe := abs
	var missing []string
	for {
		real, err := filepath.EvalSymlinks(probe)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				real = filepath.Join(real, missing[i])
			}
			return filepath.Clean(real), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", err
		}
		missing = append(missing, filepath.Base(probe))
		probe = parent
	}
}

type ClaimInput struct {
	SessionID, Runtime, Title, RepoDir, SourcePath, Intent string
	Paths, Symbols                                         []string
}

func RecordClaim(ix *store.Index, kind string, in ClaimInput) (*store.ChangeRecord, error) {
	if kind != "declaration" && kind != "implementation" {
		return nil, fmt.Errorf("unsupported claim kind %s", kind)
	}
	if strings.TrimSpace(in.SessionID) == "" || strings.TrimSpace(in.RepoDir) == "" {
		return nil, fmt.Errorf("%s requires session and repo", kind)
	}
	if kind == "declaration" && strings.TrimSpace(in.Intent) == "" {
		return nil, fmt.Errorf("declaration requires intent")
	}
	if strings.TrimSpace(in.SourcePath) == "" {
		return nil, fmt.Errorf("%s requires source-file", kind)
	}
	repo, err := ResolveRepository(in.RepoDir)
	if err != nil {
		return nil, err
	}
	src, err := sourceFile(repo, in.SourcePath)
	if err != nil {
		return nil, err
	}
	items, err := normalizePaths(repo, in.Paths, in.Symbols)
	if err != nil {
		return nil, err
	}
	r := &store.ChangeRecord{SessionID: in.SessionID, RepositoryID: repo.ID, CheckoutID: repo.CheckoutID, RepositoryIdentityKind: repo.IdentityKind, CheckoutRoot: repo.Root, SessionRuntimeClaim: in.Runtime, SessionTitleClaim: in.Title, Kind: kind, EvidenceClass: "claimed", SourceKind: "file", SourceRef: src.Ref, SourceDisplay: src.Display, SourceDigest: src.Digest, RecordedAt: time.Now().UnixNano(), Items: items}
	if kind == "declaration" {
		r.IntentLabel = in.Intent
	}
	if err := ix.AppendChange(r); err != nil {
		return nil, err
	}
	return r, nil
}

type VerificationClaimInput struct{ SessionID, Runtime, Title, RepoDir, SourcePath, Name, Boundary, Result string }

func RecordVerificationClaim(ix *store.Index, in VerificationClaimInput) (*store.ChangeRecord, error) {
	if strings.TrimSpace(in.SessionID) == "" || strings.TrimSpace(in.RepoDir) == "" {
		return nil, fmt.Errorf("verification claim requires session and repo")
	}
	if strings.TrimSpace(in.SourcePath) == "" {
		return nil, fmt.Errorf("verification claim requires source-file")
	}
	if in.Name == "" || in.Boundary == "" {
		return nil, fmt.Errorf("verification claim requires name and boundary")
	}
	switch in.Result {
	case "pass", "fail", "unavailable":
	default:
		return nil, fmt.Errorf("result must be pass, fail, or unavailable")
	}
	repo, err := ResolveRepository(in.RepoDir)
	if err != nil {
		return nil, err
	}
	src, err := sourceFile(repo, in.SourcePath)
	if err != nil {
		return nil, err
	}
	r := &store.ChangeRecord{SessionID: in.SessionID, RepositoryID: repo.ID, CheckoutID: repo.CheckoutID, RepositoryIdentityKind: repo.IdentityKind, CheckoutRoot: repo.Root, SessionRuntimeClaim: in.Runtime, SessionTitleClaim: in.Title, Kind: "verification", EvidenceClass: "claimed", SourceKind: "file", SourceRef: src.Ref, SourceDisplay: src.Display, SourceDigest: src.Digest, RecordedAt: time.Now().UnixNano(), VerificationName: in.Name, VerificationBoundary: in.Boundary, VerificationNameClass: "claimed", VerificationResult: in.Result}
	if err := ix.AppendChange(r); err != nil {
		return nil, err
	}
	return r, nil
}
