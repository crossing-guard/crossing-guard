package workspace

// ReviewService owns the live Workspace Diff (session-review-diff-plan §3;
// workspace-panes plan §4.1; owner decision O-G, 2026-09-05): it resolves a
// subject to the one folder it recorded through the existing resolver and
// delegates observation to changeenv. Reading git in a folder the session
// already ran in needs no allowlist — the daemon already holds that folder's
// transcripts and edit bodies — so the diff routes work with no workspace file.
// workspace.json remains the gate for mutation and managed worktrees only.
// The service never picks a folder when the subject recorded several, and it
// never reads a path from a client.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"crossing-guard/internal/changeenv"
	"crossing-guard/store"
)

// ReviewRepository is the store surface the review service reads.
type ReviewRepository interface {
	WorkspaceSubjectRootsDetailed(subjectKind, subjectID string) ([]string, bool, error)
	CurrentWorkspaceSelection(subjectKind, subjectID string) (store.WorkspaceSelectionRecord, bool, error)
}

// ReviewBudgets are the observation budgets and policy the console
// configuration publishes (daemon.json `diff.*`); the daemon passes them in.
type ReviewBudgets struct {
	BaseRef           string
	MaxFiles          int
	MaxStatusEntries  int
	MaxFileBytes      int64
	ContextLines      int
	MaxRefs           int
	GitTimeoutSeconds int
	// The Files pane's budgets (files plan §8).
	MaxEntriesPerDir int
	MaxReadBytes     int64
}

// ReviewProblem is a typed reason the pane renders (diff plan §5). Folder
// names the folder the problem is about; Roots lists the folders an ambiguous
// session recorded.
type ReviewProblem struct {
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Folder  string   `json:"folder,omitempty"`
	Roots   []string `json:"roots,omitempty"`
}

// ReviewCheckout names the folder a diff describes; it never claims authorship.
type ReviewCheckout struct {
	Root       string     `json:"root"`
	Branch     string     `json:"branch,omitempty"`
	Head       string     `json:"head"`
	Detached   bool       `json:"detached"`
	ObservedAt int64      `json:"observed_at"`
	Dirty      DirtyFacts `json:"dirty"`
}

// ReviewCheckoutResponse answers the checkout probe: the folder or the reason.
type ReviewCheckoutResponse struct {
	Checkout *ReviewCheckout `json:"checkout,omitempty"`
	Problem  *ReviewProblem  `json:"problem,omitempty"`
}

// ReviewBase names the ref a branch scope compared against, as resolved.
type ReviewBase struct {
	Ref       string `json:"ref"`
	OID       string `json:"oid"`
	MergeBase string `json:"merge_base,omitempty"`
}

// ReviewDiffResponse is one scope's listing; Problem replaces Files when the
// scope could not be observed.
type ReviewDiffResponse struct {
	Scope     string                        `json:"scope"`
	Checkout  *ReviewCheckout               `json:"checkout,omitempty"`
	Base      *ReviewBase                   `json:"base,omitempty"`
	Files     []changeenv.LiveDiffFile      `json:"files"`
	Truncated *changeenv.LiveDiffTruncation `json:"truncated,omitempty"`
	Problem   *ReviewProblem                `json:"problem,omitempty"`
}

// ReviewPatchResponse is one file's patch in a scope.
type ReviewPatchResponse struct {
	Scope   string                   `json:"scope"`
	File    *changeenv.LiveDiffPatch `json:"file,omitempty"`
	Problem *ReviewProblem           `json:"problem,omitempty"`
}

// ReviewRefsResponse is the compare picker's population.
type ReviewRefsResponse struct {
	Branches []changeenv.LiveDiffBranch `json:"branches"`
	Commits  []changeenv.LiveDiffCommit `json:"commits"`
	Problem  *ReviewProblem             `json:"problem,omitempty"`
}

// ReviewTreeResponse is one directory of the session's checkout.
type ReviewTreeResponse struct {
	Checkout *ReviewCheckout        `json:"checkout,omitempty"`
	Listing  *changeenv.TreeListing `json:"listing,omitempty"`
	Problem  *ReviewProblem         `json:"problem,omitempty"`
}

// ReviewFileResponse is one file of the session's checkout, bounded.
type ReviewFileResponse struct {
	File    *changeenv.TreeFile `json:"file,omitempty"`
	Problem *ReviewProblem      `json:"problem,omitempty"`
}

// ReviewService is the one owner of the live Workspace Diff; see the file comment.
type ReviewService struct {
	budgets    ReviewBudgets
	repository ReviewRepository
}

// NewReviewService takes the published budgets and the store the resolver reads.
func NewReviewService(budgets ReviewBudgets, repository ReviewRepository) *ReviewService {
	return &ReviewService{budgets: budgets, repository: repository}
}

func (s *ReviewService) treeConfig() changeenv.TreeConfig {
	return changeenv.TreeConfig{MaxEntriesPerDir: s.budgets.MaxEntriesPerDir, MaxReadBytes: s.budgets.MaxReadBytes}
}

func (s *ReviewService) liveConfig() changeenv.LiveDiffConfig {
	b := s.budgets
	return changeenv.LiveDiffConfig{BaseRef: b.BaseRef, MaxFiles: b.MaxFiles, MaxStatusEntries: b.MaxStatusEntries,
		MaxFileBytes: b.MaxFileBytes, ContextLines: b.ContextLines, MaxRefs: b.MaxRefs}
}

// deadline bounds one observation by the configured seconds, under the
// request's own context so a gone client cancels git.
func (s *ReviewService) deadline(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, time.Duration(s.budgets.GitTimeoutSeconds)*time.Second)
}

func (s *ReviewService) timeoutProblem() *ReviewProblem {
	return &ReviewProblem{Code: "git-timeout", Message: fmt.Sprintf("Reading that folder took longer than %d seconds.", s.budgets.GitTimeoutSeconds)}
}

// resolveRoot yields the one folder a subject recorded, or the problem that
// stops it. A bound workspace selection supersedes the recorded folders.
func (s *ReviewService) resolveRoot(ctx context.Context, subject Subject) (changeenv.Repository, *ReviewProblem, error) {
	if err := validateSubject(subject); err != nil {
		return changeenv.Repository{}, nil, err
	}
	root := ""
	if current, found, err := s.repository.CurrentWorkspaceSelection(subject.Kind, subject.ID); err != nil {
		return changeenv.Repository{}, nil, err
	} else if found && current.Root != "" {
		root = current.Root
	}
	if root == "" {
		roots, ambiguous, err := s.repository.WorkspaceSubjectRootsDetailed(subject.Kind, subject.ID)
		if err != nil {
			return changeenv.Repository{}, nil, err
		}
		if ambiguous {
			return changeenv.Repository{}, &ReviewProblem{Code: "ambiguous-folder", Roots: roots,
				Message: "This session worked in more than one folder, so there is no single folder to compare."}, nil
		}
		if len(roots) == 0 {
			return changeenv.Repository{}, &ReviewProblem{Code: "no-recorded-folder",
				Message: "This session did not record a folder, so there is nothing to compare."}, nil
		}
		root = roots[0]
	}
	isRepo, err := changeenv.IsRepository(ctx, root)
	if err != nil {
		return changeenv.Repository{}, problemFromLive(err, ctx, s), nil
	}
	if !isRepo {
		return changeenv.Repository{}, &ReviewProblem{Code: "not-a-repository", Folder: root,
			Message: fmt.Sprintf("%s is not a Git repository.", root)}, nil
	}
	repo, err := changeenv.ResolveRepositoryContext(ctx, root)
	if err != nil {
		return changeenv.Repository{}, problemFromLive(err, ctx, s), nil
	}
	return repo, nil, nil
}

func (s *ReviewService) checkoutFacts(ctx context.Context, repo changeenv.Repository) (*ReviewCheckout, *ReviewProblem) {
	facts, err := changeenv.InspectCheckoutContext(ctx, repo.Root)
	if err != nil {
		return nil, problemFromLive(err, ctx, s)
	}
	return &ReviewCheckout{Root: facts.Repository.Root, Branch: facts.Branch, Head: facts.Head, Detached: facts.Detached,
		ObservedAt: facts.ObservedAt / 1000, Dirty: DirtyFacts{Dirty: facts.Dirty, Staged: facts.Staged, Unstaged: facts.Unstaged,
			Untracked: facts.Untracked, Conflicted: facts.Conflicted}}, nil
}

// problemFromLive keeps changeenv's typed code; an untyped error under an
// expired deadline is the timeout, and anything else is git's own failure.
func problemFromLive(err error, ctx context.Context, s *ReviewService) *ReviewProblem {
	var typed *changeenv.LiveDiffError
	if errors.As(err, &typed) {
		if typed.Code == "git-timeout" {
			return s.timeoutProblem()
		}
		return &ReviewProblem{Code: typed.Code, Message: typed.Message}
	}
	if ctx.Err() != nil {
		return s.timeoutProblem()
	}
	return &ReviewProblem{Code: "git-failed", Message: err.Error()}
}

// Checkout resolves the folder without observing a diff: enough for the
// breadcrumb and for disabling the git scopes with a reason.
func (s *ReviewService) Checkout(parent context.Context, subject Subject) (ReviewCheckoutResponse, error) {
	ctx, cancel := s.deadline(parent)
	defer cancel()
	repo, problem, err := s.resolveRoot(ctx, subject)
	if err != nil || problem != nil {
		return ReviewCheckoutResponse{Problem: problem}, err
	}
	checkout, problem := s.checkoutFacts(ctx, repo)
	return ReviewCheckoutResponse{Checkout: checkout, Problem: problem}, nil
}

// Diff lists one scope's changed files.
func (s *ReviewService) Diff(parent context.Context, subject Subject, scope, base string) (ReviewDiffResponse, error) {
	ctx, cancel := s.deadline(parent)
	defer cancel()
	out := ReviewDiffResponse{Scope: scope, Files: []changeenv.LiveDiffFile{}}
	repo, problem, err := s.resolveRoot(ctx, subject)
	if err != nil || problem != nil {
		out.Problem = problem
		return out, err
	}
	out.Checkout, out.Problem = s.checkoutFacts(ctx, repo)
	if out.Problem != nil {
		return out, nil
	}
	result, err := changeenv.LiveDiff(ctx, repo.Root, scope, base, s.liveConfig())
	if err != nil {
		out.Problem = problemFromLive(err, ctx, s)
		return out, nil
	}
	out.Files, out.Truncated = result.Files, result.Truncated
	if result.Base != "" {
		out.Base = &ReviewBase{Ref: result.Base, OID: result.BaseOID, MergeBase: result.MergeBase}
	}
	return out, nil
}

// File renders one file's patch in a scope.
func (s *ReviewService) File(parent context.Context, subject Subject, scope, base, path string) (ReviewPatchResponse, error) {
	ctx, cancel := s.deadline(parent)
	defer cancel()
	out := ReviewPatchResponse{Scope: scope}
	repo, problem, err := s.resolveRoot(ctx, subject)
	if err != nil || problem != nil {
		out.Problem = problem
		return out, err
	}
	patch, err := changeenv.LiveDiffPatchFor(ctx, repo.Root, scope, base, path, s.liveConfig())
	if err != nil {
		out.Problem = problemFromLive(err, ctx, s)
		return out, nil
	}
	out.File = &patch
	return out, nil
}

// Refs lists the branches and recent commits a reader may compare against.
func (s *ReviewService) Refs(parent context.Context, subject Subject) (ReviewRefsResponse, error) {
	ctx, cancel := s.deadline(parent)
	defer cancel()
	out := ReviewRefsResponse{Branches: []changeenv.LiveDiffBranch{}, Commits: []changeenv.LiveDiffCommit{}}
	repo, problem, err := s.resolveRoot(ctx, subject)
	if err != nil || problem != nil {
		out.Problem = problem
		return out, err
	}
	refs, err := changeenv.LiveDiffRefsFor(ctx, repo.Root, s.liveConfig())
	if err != nil {
		out.Problem = problemFromLive(err, ctx, s)
		return out, nil
	}
	out.Branches, out.Commits = refs.Branches, refs.Commits
	return out, nil
}

// Tree lists one directory of the session's checkout (files plan §2). The root
// listing also carries the checkout facts: one request paints the crumb and the
// root, and a re-list of the root on the turn boundary refreshes both. A
// checkout that cannot be observed has no root to list, so its problem stands
// alone.
func (s *ReviewService) Tree(parent context.Context, subject Subject, dir string) (ReviewTreeResponse, error) {
	ctx, cancel := s.deadline(parent)
	defer cancel()
	repo, problem, err := s.resolveRoot(ctx, subject)
	if err != nil || problem != nil {
		return ReviewTreeResponse{Problem: problem}, err
	}
	out := ReviewTreeResponse{}
	if dir == "" {
		out.Checkout, out.Problem = s.checkoutFacts(ctx, repo)
		if out.Problem != nil {
			return out, nil
		}
	}
	listing, err := changeenv.ListTree(ctx, repo.Root, dir, s.treeConfig())
	if err != nil {
		out.Problem = problemFromLive(err, ctx, s)
		return out, nil
	}
	out.Listing = &listing
	return out, nil
}

// Read serves one listed file of the session's checkout, bounded (files plan §3).
func (s *ReviewService) Read(parent context.Context, subject Subject, path string) (ReviewFileResponse, error) {
	ctx, cancel := s.deadline(parent)
	defer cancel()
	repo, problem, err := s.resolveRoot(ctx, subject)
	if err != nil || problem != nil {
		return ReviewFileResponse{Problem: problem}, err
	}
	file, err := changeenv.ReadTreeFile(ctx, repo.Root, path, s.treeConfig())
	if err != nil {
		return ReviewFileResponse{Problem: problemFromLive(err, ctx, s)}, nil
	}
	return ReviewFileResponse{File: &file}, nil
}
