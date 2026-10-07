package changeenv

import (
	"context"
	"path/filepath"

	"crossing-guard/memory"
)

// MemoryRepositoryScope is where repository identity for a memory record is MINTED (team
// item 5 decision 18): a repository-scoped memory created where dir resolves to a checkout
// with exactly one origin remote is keyed by that repository's remote-derived id
// (remote-sha256) at create time, on every door. Anything else stays weak — keyed by the
// folder-name label, which never travels (O-4) — and note says why, for the console:
//   - dir is not a git checkout;
//   - label names a repository other than the one dir is in;
//   - the checkout has no single origin remote.
//
// label is the caller's folder-name label ("" = the folder's own). It forks git, so it
// belongs off the hook path; callers bound it with ctx.
func MemoryRepositoryScope(ctx context.Context, dir, label string) (scopeID, identity, note string) {
	fallback := label
	if fallback == "" && dir != "" {
		fallback = filepath.Base(filepath.Clean(dir))
	}
	if dir == "" {
		return fallback, "weak", "no folder was given, so the repository is known only by name"
	}
	repo, err := ResolveRepositoryContext(ctx, dir)
	if err != nil {
		return fallback, "weak", "the folder is not a git checkout, so the repository is known only by its name"
	}
	here := memory.ProjectFromCommonDir(repo.CommonDir, repo.Root)
	if label != "" && !memory.SameRepositoryScope(label, here) {
		return label, "weak", "the label names a repository other than the folder the record was written in"
	}
	if repo.IdentityKind != "remote-sha256" {
		return here, "weak", "the repository has no single origin remote, so it is known only by its folder name"
	}
	return repo.ID, "remote-sha256", ""
}
