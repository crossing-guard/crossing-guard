package daemon

// GET /api/sessions/peers — which other open sessions work in the caller's
// repository, and what a merge of their branch would bring
// (recall-mcp-v1-plan §3.3). Worktrees count as the same repository (owner,
// 2026-09-26): a session in the same checkout shares the caller's working
// tree; one in a sibling worktree is a merge risk. The route states facts and
// their gaps; deciding whether two sessions do the same work is the agent's.

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"crossing-guard/internal/changeenv"
	"crossing-guard/memory"
)

const (
	peerSameCheckout    = "same_checkout"
	peerSiblingWorktree = "sibling_worktree"
	peerOtherRepository = "other_repository"
	peerUnresolved      = "unresolved"
)

// peerRelationships is the output order; each relationship is capped alone so
// sessions in other repositories never crowd out same-repository peers.
var peerRelationships = []string{peerSameCheckout, peerSiblingWorktree, peerOtherRepository, peerUnresolved}

type peerChangedFiles struct {
	changeenv.FactState
	Scope     string   `json:"scope,omitempty"`
	Files     []string `json:"files"`
	Total     int      `json:"total"`
	Truncated bool     `json:"truncated,omitempty"`
}

type peerGitFacts struct {
	Branch       string                   `json:"branch,omitempty"`
	Head         string                   `json:"head,omitempty"`
	Base         changeenv.BranchBase     `json:"base"`
	Upstream     changeenv.BranchUpstream `json:"upstream"`
	ChangedFiles peerChangedFiles         `json:"changed_files"`
	PullRequest  changeenv.FactState      `json:"pull_request"`
}

type peerCaller struct {
	Runtime     string `json:"runtime,omitempty"`
	ID          string `json:"id,omitempty"`
	Identified  bool   `json:"identified"`
	FromSession string `json:"from_session,omitempty"`
	// FromSessionState is "found", or "not_found" when a watched session was
	// named but is not in the catalog (the answer is then from the caller's place).
	FromSessionState string        `json:"from_session_state,omitempty"`
	Place            string        `json:"place"`
	PlaceSource      string        `json:"place_source"`
	RepositoryRoot   string        `json:"repository_root,omitempty"`
	CheckoutRoot     string        `json:"checkout_root,omitempty"`
	MemoryRepository string        `json:"memory_repository,omitempty"`
	Resolution       string        `json:"resolution"` // git | folder | unresolved
	Reason           string        `json:"reason,omitempty"`
	Git              *peerGitFacts `json:"git,omitempty"`
}

type sessionPeer struct {
	Runtime           string        `json:"runtime"`
	ID                string        `json:"id"`
	Title             string        `json:"title,omitempty"`
	Cwd               string        `json:"cwd,omitempty"`
	Modified          time.Time     `json:"modified"`
	Model             string        `json:"model,omitempty"`
	Turns             int           `json:"turns,omitempty"`
	HasChangeEvidence bool          `json:"has_change_evidence"`
	Relationship      string        `json:"relationship"`
	Reason            string        `json:"reason,omitempty"`
	CheckoutRoot      string        `json:"checkout_root,omitempty"`
	Git               *peerGitFacts `json:"git,omitempty"`
	OverlapFiles      []string      `json:"overlap_files,omitempty"`
	OverlapNote       string        `json:"overlap_note,omitempty"`
	// AgentOf is set when this session is a Crossing Guard agent's own (a
	// managed run's child or a helper session): the session it serves.
	AgentOf *peerAgentOf `json:"agent_of,omitempty"`
}

type peerAgentOf struct {
	Role    string `json:"role,omitempty"`
	Runtime string `json:"runtime,omitempty"`
	ID      string `json:"id,omitempty"`
}

type sessionPeersResponse struct {
	Caller          peerCaller    `json:"caller"`
	State           string        `json:"state"` // available | unavailable
	Detail          string        `json:"detail,omitempty"`
	Scope           string        `json:"scope"`
	Peers           []sessionPeer `json:"peers"`
	CandidatesTotal int           `json:"candidates_total"`
	// AgentSessions says how Crossing Guard's own agent sessions were treated:
	// "excluded" (the default; AgentSessionsExcluded counts them), "included"
	// (labeled agent_of), or "unknown" when the orchestration record could
	// not be read, in which case they may appear unlabeled.
	AgentSessions         string              `json:"agent_sessions"`
	AgentSessionsExcluded int                 `json:"agent_sessions_excluded"`
	Truncated             bool                `json:"truncated"`
	Counts                map[string]int      `json:"counts"`
	Activity              presenceCapabilityJ `json:"activity"`
	// ChangeEvidenceProblem says why sessions known only from recorded change
	// evidence are missing from the candidates (the store could not be read);
	// absent when the store was read or does not exist yet.
	ChangeEvidenceProblem string `json:"change_evidence_problem,omitempty"`
}

// presenceCapabilityJ is the presence capability as the route reports it.
type presenceCapabilityJ struct {
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Place kinds: a git checkout, a plain folder, or not resolved (with a reason).
const (
	placeGit        = "git"
	placeFolder     = "folder"
	placeUnresolved = "unresolved"
	// reasonDeadline is the one spelling of "the route ran out of time".
	reasonDeadline = "deadline"
	// noPullRequestSource is why pull-request state is always unavailable.
	noPullRequestSource = "Crossing Guard has no pull-request source"
)

// placeResolution is one folder's repository identity, or why there is none.
type placeResolution struct {
	kind   string // placeGit | placeFolder | placeUnresolved
	repo   changeenv.Repository
	folder string
	reason string
}

func handleSessionPeers(w http.ResponseWriter, r *http.Request) {
	handleSessionPeersWith(w, r, ScanSessions, currentPresenceOpenSet)
}

// maxPeerPlaceBytes bounds a client-supplied folder, as the repository view does.
const maxPeerPlaceBytes = 4096

// peerRequest is the validated query.
type peerRequest struct {
	scope, runtime, id, fromRuntime, fromID, cwd, cwdSource string
	placeOnly, includeAgents                                bool
}

func parsePeerRequest(r *http.Request) (peerRequest, error) {
	query := r.URL.Query()
	request := peerRequest{scope: query.Get("scope"), runtime: query.Get("runtime"), id: query.Get("id"),
		fromRuntime: query.Get("from_runtime"), fromID: query.Get("from_id"), cwd: query.Get("cwd"),
		cwdSource: query.Get("cwd_source"), placeOnly: query.Get("place_only") == "1",
		includeAgents: query.Get("include_agents") == "1"}
	if request.scope == "" {
		request.scope = "repository"
	}
	if request.scope != "repository" && request.scope != "all" {
		return request, errors.New("scope must be repository or all")
	}
	for name, value := range map[string]string{"runtime": request.runtime, "id": request.id,
		"from_runtime": request.fromRuntime, "from_id": request.fromID, "cwd_source": request.cwdSource} {
		if len(value) > maxSessionIdentityBytes {
			return request, errors.New(name + " is too long")
		}
	}
	if len(request.cwd) > maxPeerPlaceBytes {
		return request, errors.New("cwd is too long")
	}
	return request, nil
}

func handleSessionPeersWith(w http.ResponseWriter, r *http.Request, scan func() []SessionSummary, openSet func(time.Time) presenceOpenSet) {
	request, err := parsePeerRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	config, _ := consoleConfig()
	limits := config.Recall
	// The deadline covers the whole route, catalog scan and presence included.
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(limits.PeerRouteDeadlineMS)*time.Millisecond)
	defer cancel()
	// Every open session's folder is read here, on any authenticated request:
	// no repository-configured command runs for these reads.
	ctx = changeenv.WithUntrustedFolders(ctx)

	sessions := scan()
	out := sessionPeersResponse{Scope: request.scope, Peers: []sessionPeer{}, Counts: map[string]int{},
		ChangeEvidenceProblem: sessionScanCoalescer.changeEvidenceProblem()}
	placeCaller(&out.Caller, sessions, request)
	if out.Caller.Place == "" {
		http.Error(w, "the caller's place is unknown: pass cwd or an identified session", http.StatusBadRequest)
		return
	}
	if request.placeOnly {
		// The caller's place and labels alone: no candidates, no git facts.
		describeCallerPlace(&out.Caller, resolvePlaces(ctx, []string{out.Caller.Place}, limits)[out.Caller.Place])
		out.State = "place_only"
		writeJSON(w, out)
		return
	}
	presence := openSet(time.Now())
	out.Activity = presenceCapabilityJ{Status: presence.Capability.Status, Detail: presence.Capability.Detail}
	if presence.Capability.Status != "available" {
		// isOpen is false for every row now; "nothing is open" would be a lie.
		out.State, out.Detail = "unavailable", presence.Capability.Detail
		out.Caller.Resolution = placeUnresolved
		writeJSON(w, out)
		return
	}
	out.State = "available"

	candidates, agentOf := collectPeerCandidates(&out, sessions, presence, request)
	folders := []string{out.Caller.Place}
	for _, candidate := range candidates {
		folders = append(folders, candidate.Cwd)
	}
	places := resolvePlaces(ctx, folders, limits)
	callerPlace := places[out.Caller.Place]
	describeCallerPlace(&out.Caller, callerPlace)
	byRelationship := classifyPeers(&out, candidates, agentOf, places, callerPlace, request.scope)
	attachGitFacts(ctx, &out, byRelationship, callerPlace, config)
	for _, relationship := range peerRelationships {
		peers := byRelationship[relationship]
		out.Counts[relationship] += len(peers)
		if len(peers) > limits.PeersMax {
			peers, out.Truncated = peers[:limits.PeersMax], true
		}
		out.Peers = append(out.Peers, peers...)
	}
	writeJSON(w, out)
}

// placeCaller names who is asking and from where: a watched session's
// catalog folder when a helper names one, else the caller's own catalog
// folder, else the folder the request carries.
func placeCaller(caller *peerCaller, sessions []SessionSummary, request peerRequest) {
	caller.Runtime, caller.ID = request.runtime, request.id
	own, callerFound := identifySession(sessions, request.runtime, request.id)
	caller.Identified = callerFound
	if request.fromRuntime != "" || request.fromID != "" {
		from, fromFound := identifySession(sessions, request.fromRuntime, request.fromID)
		if fromFound && from.Cwd != "" {
			caller.FromSession = from.Runtime + "/" + from.ID
			caller.FromSessionState = "found"
			caller.Place, caller.PlaceSource = from.Cwd, "catalog:from_session"
			return
		}
		// Said, never silently replaced: the answer below is from the caller's own place.
		caller.FromSessionState = "not_found"
	}
	if callerFound && own.Cwd != "" {
		caller.Place, caller.PlaceSource = own.Cwd, "catalog"
		return
	}
	caller.Place, caller.PlaceSource = request.cwd, request.cwdSource
	if caller.PlaceSource == "" {
		caller.PlaceSource = "request"
	}
}

// collectPeerCandidates keeps every open session that is not the caller or
// the watched session (every row of either: resumed segments are separate
// rows). Crossing Guard's own agent sessions (helpers, followers) run in the
// binding's root, so each would read as "someone shares your checkout": they
// are excluded unless asked for, found by the run and helper-session linkage
// for exactly these sessions — never by who launched a session, so an
// owner's console task stays a peer.
func collectPeerCandidates(out *sessionPeersResponse, sessions []SessionSummary, presence presenceOpenSet, request peerRequest) ([]SessionSummary, map[string]*peerAgentOf) {
	open := []SessionSummary{}
	ids := []string{}
	for _, session := range sessions {
		if !presence.isOpen(session) || isNamedSession(session, request.runtime, request.id) ||
			isNamedSession(session, request.fromRuntime, request.fromID) {
			continue
		}
		open = append(open, session)
		ids = append(ids, sessionIdentityAlternates(session)...)
	}
	parents := agentSessionParentsRead(ids)
	switch {
	case parents == nil && len(ids) > 0:
		out.AgentSessions = "unknown" // the linkage could not be read; agents may appear unlabeled
	case request.includeAgents:
		out.AgentSessions = "included"
	default:
		out.AgentSessions = "excluded"
	}
	agentOf := map[string]*peerAgentOf{}
	candidates := []SessionSummary{}
	for _, session := range open {
		if parent, isAgent := agentParentOf(session, parents); isAgent {
			if !request.includeAgents {
				out.AgentSessionsExcluded++
				continue
			}
			parentID := parent.RootCatalogSessionID
			if parentID == "" {
				parentID = parent.RootNativeSessionID
			}
			agentOf[session.Runtime+"\x00"+session.ID] = &peerAgentOf{Role: parent.Role, Runtime: parent.RootRuntime, ID: parentID}
		}
		candidates = append(candidates, session)
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Modified.After(candidates[j].Modified) })
	out.CandidatesTotal = len(candidates)
	return candidates, agentOf
}

// classifyPeers labels every candidate against the caller's place; sessions
// in other repositories are counted, and listed only for scope=all.
func classifyPeers(out *sessionPeersResponse, candidates []SessionSummary, agentOf map[string]*peerAgentOf,
	places map[string]placeResolution, callerPlace placeResolution, scope string) map[string][]sessionPeer {
	byRelationship := map[string][]sessionPeer{}
	for _, candidate := range candidates {
		peer := sessionPeer{Runtime: candidate.Runtime, ID: candidate.ID, Title: candidate.Title, Cwd: candidate.Cwd,
			Modified: candidate.Modified, Model: candidate.Model, Turns: candidate.Turns, HasChangeEvidence: candidate.HasChangeEvidence,
			AgentOf: agentOf[candidate.Runtime+"\x00"+candidate.ID]}
		place := places[candidate.Cwd]
		peer.Relationship, peer.Reason = classifyPeer(callerPlace, place)
		if place.kind == placeGit {
			peer.CheckoutRoot = place.repo.Root
		}
		if peer.Relationship == peerOtherRepository && scope != "all" {
			out.Counts[peerOtherRepository]++ // counted, not listed
			continue
		}
		byRelationship[peer.Relationship] = append(byRelationship[peer.Relationship], peer)
	}
	return byRelationship
}

// attachGitFacts reads the caller's checkout and every sibling worktree once
// each, and the overlap between them.
func attachGitFacts(ctx context.Context, out *sessionPeersResponse, byRelationship map[string][]sessionPeer,
	callerPlace placeResolution, config ConsoleConfig) {
	limits := config.Recall
	roots := []string{}
	seen := map[string]bool{}
	if callerPlace.kind == placeGit {
		roots, seen[callerPlace.repo.Root] = append(roots, callerPlace.repo.Root), true
	}
	for _, peer := range byRelationship[peerSiblingWorktree] {
		if !seen[peer.CheckoutRoot] {
			roots, seen[peer.CheckoutRoot] = append(roots, peer.CheckoutRoot), true
		}
	}
	facts := readPeerFacts(ctx, roots, config, limits)
	if callerPlace.kind == placeGit {
		out.Caller.Git = emitPeerFacts(facts[callerPlace.repo.Root], limits.PeerChangedFilesMax)
	}
	for index := range byRelationship[peerSiblingWorktree] {
		peer := &byRelationship[peerSiblingWorktree][index]
		peer.Git = emitPeerFacts(facts[peer.CheckoutRoot], limits.PeerChangedFilesMax)
		if callerPlace.kind == placeGit {
			peer.OverlapFiles, peer.OverlapNote = overlap(facts[callerPlace.repo.Root], facts[peer.CheckoutRoot])
		}
	}
}

// describeCallerPlace fills the caller block from its resolved place,
// including the memory label the memory package derives from it. The
// repository root is named only for the standard layout (<root>/.git); a
// bare repository or a submodule has no such folder.
func describeCallerPlace(caller *peerCaller, place placeResolution) {
	caller.Resolution, caller.Reason = place.kind, place.reason
	switch place.kind {
	case placeGit:
		caller.CheckoutRoot = place.repo.Root
		if strings.HasSuffix(place.repo.CommonDir, string(filepath.Separator)+".git") {
			caller.RepositoryRoot = filepath.Dir(place.repo.CommonDir)
		}
		caller.MemoryRepository = memory.ProjectFromCommonDir(place.repo.CommonDir, place.repo.Root)
	case placeFolder:
		caller.CheckoutRoot, caller.RepositoryRoot = place.folder, place.folder
		caller.MemoryRepository = memory.ProjectFromCommonDir("", place.folder)
	}
}

// identifySession finds the session one runtime/id names under the one
// identity rule (belongsTo → the runtime's MatchID): a child rollout that
// carries its parent's thread id is not the parent.
func identifySession(sessions []SessionSummary, runtime, id string) (SessionSummary, bool) {
	for _, session := range sessions {
		if isNamedSession(session, runtime, id) {
			return session, true
		}
	}
	return SessionSummary{}, false
}

// isNamedSession is belongsTo for a caller-supplied name: both halves are
// required, because an empty id must name no session.
func isNamedSession(session SessionSummary, runtime, id string) bool {
	return runtime != "" && id != "" && belongsTo(session, runtime, id)
}

// resolvePlaces resolves each distinct folder's repository identity with at
// most PeerGitConcurrency resolutions in flight. A folder not reached before
// ctx ends is unresolved with reason "deadline".
func resolvePlaces(ctx context.Context, folders []string, limits ConsoleRecallDefaults) map[string]placeResolution {
	distinct := []string{}
	out := map[string]placeResolution{}
	for _, folder := range folders {
		if _, seen := out[folder]; seen {
			continue
		}
		out[folder] = placeResolution{kind: placeUnresolved, reason: reasonDeadline}
		distinct = append(distinct, folder)
	}
	var mu sync.Mutex
	runBounded(ctx, len(distinct), limits.PeerGitConcurrency, func(i int) {
		folder := distinct[i]
		one, cancel := context.WithTimeout(ctx, time.Duration(limits.PeerGitTimeoutMS)*time.Millisecond)
		defer cancel()
		resolved := resolvePlace(one, folder)
		mu.Lock()
		out[folder] = resolved
		mu.Unlock()
	})
	return out
}

func resolvePlace(ctx context.Context, folder string) placeResolution {
	if folder == "" {
		return placeResolution{kind: placeUnresolved, reason: "the session reports no folder"}
	}
	info, err := os.Stat(folder)
	if err != nil || !info.IsDir() {
		return placeResolution{kind: placeUnresolved, reason: "the folder is gone"}
	}
	repo, err := changeenv.ResolveRepositoryContext(ctx, folder)
	if err == nil {
		return placeResolution{kind: placeGit, repo: repo}
	}
	if ctx.Err() != nil {
		return placeResolution{kind: placeUnresolved, reason: reasonDeadline}
	}
	isRepo, repoErr := changeenv.IsRepository(ctx, folder)
	if repoErr == nil && !isRepo {
		canonical, err := filepath.EvalSymlinks(folder)
		if err != nil {
			canonical = filepath.Clean(folder)
		}
		return placeResolution{kind: placeFolder, folder: canonical}
	}
	return placeResolution{kind: placeUnresolved, reason: "git could not read the folder: " + changeenv.Unavailable(err).Reason}
}

// classifyPeer names a peer's relationship to the caller's place.
func classifyPeer(caller, peer placeResolution) (string, string) {
	if peer.kind == placeUnresolved {
		return peerUnresolved, peer.reason
	}
	if caller.kind == placeUnresolved {
		return peerUnresolved, "the caller's own folder could not be resolved: " + caller.reason
	}
	if caller.kind == placeGit && peer.kind == placeGit {
		switch {
		case caller.repo.Root == peer.repo.Root:
			return peerSameCheckout, ""
		case caller.repo.CommonDir == peer.repo.CommonDir:
			return peerSiblingWorktree, ""
		}
		return peerOtherRepository, ""
	}
	if caller.kind == placeFolder && peer.kind == placeFolder && caller.folder == peer.folder {
		return peerSameCheckout, ""
	}
	return peerOtherRepository, ""
}

// readPeerFacts reads each checkout's branch facts once, bounded like
// resolvePlaces. A checkout not reached before ctx ends reads as deadline.
func readPeerFacts(ctx context.Context, roots []string, config ConsoleConfig, limits ConsoleRecallDefaults) map[string]*changeenv.BranchFacts {
	out := map[string]*changeenv.BranchFacts{}
	var mu sync.Mutex
	cfg := changeenv.LiveDiffConfig{BaseRef: config.Diff.BaseRef}
	runBounded(ctx, len(roots), limits.PeerGitConcurrency, func(i int) {
		one, cancel := context.WithTimeout(ctx, time.Duration(limits.PeerGitTimeoutMS)*time.Millisecond)
		defer cancel()
		facts, err := changeenv.ReadBranchFacts(one, roots[i], cfg)
		if err != nil {
			facts = changeenv.BranchFacts{Base: changeenv.BranchBase{FactState: changeenv.Unavailable(err)},
				Upstream: changeenv.BranchUpstream{FactState: changeenv.Unavailable(err)},
				Changes:  changeenv.BranchChanges{FactState: changeenv.Unavailable(err), Files: []string{}}}
		}
		mu.Lock()
		out[roots[i]] = &facts
		mu.Unlock()
	})
	return out
}

// emitPeerFacts is the reported form: the changed-file list capped, the
// missing PR source stated.
func emitPeerFacts(facts *changeenv.BranchFacts, maxFiles int) *peerGitFacts {
	if facts == nil {
		deadline := changeenv.FactState{State: changeenv.FactUnavailable, Reason: reasonDeadline}
		return &peerGitFacts{Base: changeenv.BranchBase{FactState: deadline}, Upstream: changeenv.BranchUpstream{FactState: deadline},
			ChangedFiles: peerChangedFiles{FactState: deadline, Files: []string{}},
			PullRequest:  changeenv.FactState{State: changeenv.FactUnavailable, Reason: noPullRequestSource}}
	}
	files := facts.Changes.Files
	changed := peerChangedFiles{FactState: facts.Changes.FactState, Scope: facts.Changes.Scope, Total: len(files)}
	if len(files) > maxFiles {
		files, changed.Truncated = files[:maxFiles], true
	}
	changed.Files = append([]string{}, files...)
	return &peerGitFacts{Branch: facts.Branch, Head: facts.Head, Base: facts.Base, Upstream: facts.Upstream,
		ChangedFiles: changed,
		PullRequest:  changeenv.FactState{State: changeenv.FactUnavailable, Reason: noPullRequestSource}}
}

// overlap is the paths both checkouts change, from the complete lists.
func overlap(caller, peer *changeenv.BranchFacts) ([]string, string) {
	if caller == nil || peer == nil || caller.Changes.State != changeenv.FactMeasured || peer.Changes.State != changeenv.FactMeasured {
		return nil, "not computed: a changed-file list is unavailable"
	}
	mine := map[string]bool{}
	for _, path := range caller.Changes.Files {
		mine[path] = true
	}
	both := []string{}
	for _, path := range peer.Changes.Files {
		if mine[path] {
			both = append(both, path)
		}
	}
	note := ""
	if caller.Changes.Scope == "uncommitted" || peer.Changes.Scope == "uncommitted" {
		note = "no base branch was found for one side, so only its uncommitted changes were compared"
	}
	return both, note
}

// runBounded calls fn(0..n-1) with at most limit calls in flight, starting no
// new call once ctx is done.
func runBounded(ctx context.Context, n, limit int, fn func(int)) {
	if limit <= 0 {
		limit = 1
	}
	slots := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := 0; i < n && ctx.Err() == nil; i++ {
		acquired := false
		select {
		case <-ctx.Done():
		case slots <- struct{}{}:
			acquired = true
		}
		if ctx.Err() != nil {
			// A slot and the deadline can be ready together; never start late.
			if acquired {
				<-slots
			}
			break
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-slots }()
			fn(i)
		}(i)
	}
	wg.Wait()
}
