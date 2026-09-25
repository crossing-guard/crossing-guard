package changeenv

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// CheckoutFacts is the bounded, read-only identity and dirty summary used by the
// workspace selector. SnapshotDigest covers the exact HEAD and porcelain bytes; it
// is an optimistic-concurrency fact, not a retained evidence record.
type CheckoutFacts struct {
	Repository     Repository `json:"repository"`
	Branch         string     `json:"branch,omitempty"`
	Head           string     `json:"head"`
	Detached       bool       `json:"detached"`
	Dirty          bool       `json:"dirty"`
	Staged         int        `json:"staged"`
	Unstaged       int        `json:"unstaged"`
	Untracked      int        `json:"untracked"`
	Conflicted     int        `json:"conflicted"`
	SnapshotDigest string     `json:"snapshot_digest"`
	ObservedAt     int64      `json:"observed_at"`
}

func gitOutput(ctx context.Context, dir string, args ...string) ([]byte, error) {
	out, err := gitRawContext(ctx, dir, args...)
	return bytes.TrimSpace(out), err
}

// ResolveRepository is the unbounded form; callers with a deadline use
// ResolveRepositoryContext (workspace-panes plan §4.1, R15).
func ResolveRepository(dir string) (Repository, error) {
	return ResolveRepositoryContext(context.Background(), dir)
}

func ResolveRepositoryContext(ctx context.Context, dir string) (Repository, error) {
	rootb, err := gitOutput(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return Repository{}, err
	}
	root, err := filepath.Abs(string(rootb))
	if err != nil {
		return Repository{}, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return Repository{}, err
	}
	commonb, err := gitOutput(ctx, root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return Repository{}, err
	}
	common, err := filepath.EvalSymlinks(string(commonb))
	if err != nil {
		return Repository{}, err
	}
	gitDirBytes, err := gitOutput(ctx, root, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		return Repository{}, err
	}
	gitDir, err := filepath.EvalSymlinks(string(gitDirBytes))
	if err != nil {
		return Repository{}, err
	}
	idKind := "local-sha256"
	basis := root
	if rem, er := gitOutput(ctx, root, "config", "--get-all", "remote.origin.url"); er == nil {
		origins := nonemptyLines(string(rem))
		if len(origins) == 1 {
			idKind = "remote-sha256"
			basis = normalizeRemote(origins[0])
		}
	}
	return Repository{ID: digest(idKind+"-v1:", []byte(basis)), CheckoutID: digest("checkout-sha256-v1:", []byte(root+"\x00"+gitDir)), IdentityKind: idKind, Root: root, CommonDir: common}, nil
}

// InspectCheckout resolves repository identity once and observes only Git's stable,
// machine-readable status. It deliberately does not enumerate paths or retain a diff.
func InspectCheckout(dir string) (CheckoutFacts, error) {
	return InspectCheckoutContext(context.Background(), dir)
}

func InspectCheckoutContext(ctx context.Context, dir string) (CheckoutFacts, error) {
	repo, err := ResolveRepositoryContext(ctx, dir)
	if err != nil {
		return CheckoutFacts{}, err
	}
	headBytes, err := gitOutput(ctx, repo.Root, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return CheckoutFacts{}, err
	}
	status, err := gitRawContext(ctx, repo.Root, "status", "--porcelain=v2", "-z", "--untracked-files=normal")
	if err != nil {
		return CheckoutFacts{}, err
	}
	branchBytes, branchErr := gitOutput(ctx, repo.Root, "symbolic-ref", "--quiet", "--short", "HEAD")
	facts := CheckoutFacts{Repository: repo, Head: string(headBytes), Detached: branchErr != nil,
		SnapshotDigest: digest("workspace-snapshot-sha256-v1:", append(append([]byte(string(headBytes)+"\x00"), status...), 0)),
		ObservedAt:     time.Now().UnixMilli()}
	if branchErr == nil {
		facts.Branch = string(branchBytes)
	}
	for len(status) > 0 {
		end := bytes.IndexByte(status, 0)
		if end < 0 {
			end = len(status)
		}
		record := status[:end]
		status = status[end:]
		if len(status) > 0 {
			status = status[1:]
		}
		if len(record) == 0 {
			continue
		}
		switch record[0] {
		case '?':
			facts.Untracked++
		case 'u':
			facts.Conflicted++
		case '1', '2':
			fields := bytes.Fields(record)
			if len(fields) < 2 || len(fields[1]) != 2 {
				return CheckoutFacts{}, fmt.Errorf("unexpected git status record")
			}
			if fields[1][0] != '.' {
				facts.Staged++
			}
			if fields[1][1] != '.' {
				facts.Unstaged++
			}
			if record[0] == '2' {
				// Porcelain v2 -z emits the rename/copy origin as a second NUL field.
				originEnd := bytes.IndexByte(status, 0)
				if originEnd < 0 {
					return CheckoutFacts{}, fmt.Errorf("incomplete git rename status record")
				}
				status = status[originEnd+1:]
			}
		default:
			return CheckoutFacts{}, fmt.Errorf("unexpected git status record kind %q", record[0])
		}
	}
	facts.Dirty = facts.Staged+facts.Unstaged+facts.Untracked+facts.Conflicted > 0
	return facts, nil
}

func nonemptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// normalizeRemote removes transport credentials and spelling-only variation while
// deliberately retaining scheme, host, port and repository suffix. It does not guess
// that two different transports or paths identify the same repository.
func normalizeRemote(raw string) string {
	raw = strings.TrimSpace(raw)
	if u, err := url.Parse(raw); err == nil && u.Scheme != "" && u.Host != "" {
		u.Scheme = strings.ToLower(u.Scheme)
		u.Host = strings.ToLower(u.Host)
		u.User = nil
		u.RawQuery = ""
		u.Fragment = ""
		u.Path = strings.TrimSuffix(u.Path, "/")
		return u.String()
	}
	// SCP syntax is not a URL. Drop only the optional user and normalize host case;
	// keep the marker distinct from ssh:// so the transformation stays conservative.
	if colon := strings.IndexByte(raw, ':'); colon > 0 && !strings.Contains(raw[:colon], "/") {
		host := raw[:colon]
		if at := strings.LastIndexByte(host, '@'); at >= 0 {
			host = host[at+1:]
		}
		return "scp://" + strings.ToLower(host) + "/" + strings.TrimSuffix(strings.TrimPrefix(raw[colon+1:], "/"), "/")
	}
	return strings.TrimSuffix(raw, "/")
}
