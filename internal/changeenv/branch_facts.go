package changeenv

// Branch facts for another agent's checkout: what a merge of that branch would
// bring (recall-mcp-v1-plan §3.3). It is the Diff pane's since-base scope with
// the file reads left out: path names only.

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
)

// Fact states: read, not readable (with a reason), or read and absent.
const (
	FactMeasured    = "measured"
	FactUnavailable = "unavailable"
	FactNone        = "none"
)

// FactState says whether a group of facts was read or why it was not.
type FactState struct {
	State  string `json:"state"` // FactMeasured | FactUnavailable | FactNone
	Reason string `json:"reason,omitempty"`
}

// BranchBase is the ref a branch is compared against and where it forked.
type BranchBase struct {
	FactState
	Name      string `json:"name,omitempty"`
	OID       string `json:"oid,omitempty"`
	MergeBase string `json:"merge_base,omitempty"`
	// Ahead and Behind count commits on HEAD and on the base since the merge base.
	Ahead  int `json:"ahead,omitempty"`
	Behind int `json:"behind,omitempty"`
}

// BranchUpstream is the tracking branch, or state "none" when there is none.
type BranchUpstream struct {
	FactState
	Name   string `json:"name,omitempty"`
	Ahead  int    `json:"ahead,omitempty"`
	Behind int    `json:"behind,omitempty"`
}

// BranchChanges lists every path that differs between the compared commit and
// the working tree (staged, unstaged; both sides of a rename) plus untracked
// files. Scope "since_base" compares from the merge base, so committed branch
// work is included; "uncommitted" compares from HEAD because no base was found.
type BranchChanges struct {
	FactState
	Scope string   `json:"scope,omitempty"`
	Files []string `json:"files"`
}

// BranchFacts is one checkout's branch as a merge would see it.
type BranchFacts struct {
	Branch   string         `json:"branch,omitempty"`
	Head     string         `json:"head,omitempty"`
	Base     BranchBase     `json:"base"`
	Upstream BranchUpstream `json:"upstream"`
	Changes  BranchChanges  `json:"changed_files"`
}

// ReadBranchFacts reads root's branch facts. The base is resolved exactly as
// the Diff pane resolves it (cfg.BaseRef, else origin/HEAD, else the default
// candidates). The changed-path list is complete; callers cap what they emit.
// A group that cannot be read is marked unavailable with the reason; the error
// is only for a checkout whose HEAD cannot be read at all.
func ReadBranchFacts(ctx context.Context, root string, cfg LiveDiffConfig) (BranchFacts, error) {
	facts := BranchFacts{Changes: BranchChanges{Files: []string{}}}
	head := LiveDiffResult{}
	if err := observeHead(ctx, root, &head); err != nil {
		return facts, err
	}
	facts.Branch, facts.Head = head.Branch, head.Head
	facts.Upstream = readUpstream(ctx, root)
	facts.Changes.Scope = "since_base"
	scope, err := resolveLiveScope(ctx, root, "since-base", "", cfg, &head)
	if err != nil {
		if ctx.Err() != nil {
			facts.Base.FactState, facts.Changes.FactState = Unavailable(err), Unavailable(err)
			return facts, nil
		}
		// No base: the uncommitted work is still a collision risk worth naming.
		facts.Base.FactState = Unavailable(err)
		facts.Changes.Scope = "uncommitted"
		if scope, err = resolveLiveScope(ctx, root, "working", "", cfg, &head); err != nil {
			facts.Changes.FactState = Unavailable(err)
			return facts, nil
		}
	} else {
		facts.Base = BranchBase{FactState: FactState{State: FactMeasured}, Name: head.Base, OID: head.BaseOID, MergeBase: head.MergeBase}
		if behind, ahead, err := leftRightCount(ctx, root, head.BaseOID+"...HEAD"); err == nil {
			facts.Base.Behind, facts.Base.Ahead = behind, ahead
		} else {
			facts.Base.FactState = Unavailable(err)
		}
	}
	records, err := listTrackedRecords(ctx, root, scope, "")
	if err != nil {
		facts.Changes.FactState = Unavailable(err)
		return facts, nil
	}
	untracked, err := listUntracked(ctx, root, "")
	if err != nil {
		facts.Changes.FactState = Unavailable(err)
		return facts, nil
	}
	seen := map[string]bool{}
	for _, record := range records {
		for _, path := range []string{record.oldPath, record.path} {
			if path != "" && !seen[path] {
				seen[path] = true
				facts.Changes.Files = append(facts.Changes.Files, path)
			}
		}
	}
	for _, path := range untracked {
		if !seen[path] {
			seen[path] = true
			facts.Changes.Files = append(facts.Changes.Files, path)
		}
	}
	sort.Strings(facts.Changes.Files)
	facts.Changes.State = FactMeasured
	return facts, nil
}

func readUpstream(ctx context.Context, root string) BranchUpstream {
	out, _, err := runLiveGit(ctx, root, liveListingBytes, false, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
	if err != nil {
		// Only git's own "no upstream" answer means not pushed. A configured
		// upstream whose remote branch is gone, or any other failure, is not
		// a fact about pushing: it is unavailable with git's words.
		var typed *LiveDiffError
		if errors.As(err, &typed) && typed.Code == "git-failed" && noUpstreamMessage(typed.Message) {
			return BranchUpstream{FactState: FactState{State: FactNone, Reason: "no upstream branch"}}
		}
		return BranchUpstream{FactState: Unavailable(err)}
	}
	upstream := BranchUpstream{FactState: FactState{State: FactMeasured}, Name: strings.TrimSpace(string(out))}
	behind, ahead, err := leftRightCount(ctx, root, "@{u}...HEAD")
	if err != nil {
		upstream.FactState = Unavailable(err)
		return upstream
	}
	upstream.Behind, upstream.Ahead = behind, ahead
	return upstream
}

// leftRightCount answers `rev-list --left-right --count A...B` as (left, right).
func leftRightCount(ctx context.Context, root, rangeSpec string) (int, int, error) {
	out, _, err := runLiveGit(ctx, root, liveListingBytes, false, "rev-list", "--left-right", "--count", rangeSpec, "--")
	if err != nil {
		return 0, 0, err
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		return 0, 0, &LiveDiffError{Code: "git-failed", Message: "unexpected rev-list count output"}
	}
	left, errLeft := strconv.Atoi(fields[0])
	right, errRight := strconv.Atoi(fields[1])
	if errLeft != nil || errRight != nil {
		return 0, 0, &LiveDiffError{Code: "git-failed", Message: "unexpected rev-list count output"}
	}
	return left, right, nil
}

// noUpstreamMessage recognizes git's answers for a branch with no upstream
// configured, and for a detached HEAD, which has none either.
func noUpstreamMessage(message string) bool {
	message = strings.ToLower(message)
	return strings.Contains(message, "no upstream configured") || strings.Contains(message, "head does not point to a branch")
}

// Unavailable is a fact group that could not be read, with the reason. A
// timeout reads "deadline", the one spelling the recall routes promise.
func Unavailable(err error) FactState {
	var typed *LiveDiffError
	if errors.As(err, &typed) {
		if typed.Code == "git-timeout" {
			return FactState{State: FactUnavailable, Reason: "deadline"}
		}
		return FactState{State: FactUnavailable, Reason: typed.Message}
	}
	return FactState{State: FactUnavailable, Reason: err.Error()}
}
