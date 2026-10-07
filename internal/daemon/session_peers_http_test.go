package daemon

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"crossing-guard/internal/sessionactivity"
	"crossing-guard/store"
)

func peerGit(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.name=crossing-guard", "-c", "user.email=crossing-guard@example.invalid"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

func peerWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

type peersFixture struct {
	main, claudeWorktree, elsewhere, otherRepo, plainFolder string
}

// One repository with a worktree under .claude/worktrees and one made with
// `git worktree add ../elsewhere`, another repository, and a non-git folder.
func newPeersFixture(t *testing.T) peersFixture {
	t.Helper()
	base := t.TempDir()
	f := peersFixture{main: filepath.Join(base, "repo"), elsewhere: filepath.Join(base, "elsewhere"),
		otherRepo: filepath.Join(base, "other"), plainFolder: filepath.Join(base, "plain")}
	f.claudeWorktree = filepath.Join(f.main, ".claude", "worktrees", "w1")
	for _, dir := range []string{f.main, f.otherRepo, f.plainFolder} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	peerGit(t, f.main, "init", "-q", "-b", "main")
	peerWrite(t, filepath.Join(f.main, "shared.go"), "package x\n")
	peerWrite(t, filepath.Join(f.main, "only-main.go"), "package x\n")
	peerGit(t, f.main, "add", ".")
	peerGit(t, f.main, "commit", "-q", "-m", "base")
	peerGit(t, f.main, "worktree", "add", "-q", "-b", "claude-branch", f.claudeWorktree)
	peerGit(t, f.main, "worktree", "add", "-q", "-b", "elsewhere-branch", f.elsewhere)
	peerWrite(t, filepath.Join(f.elsewhere, "shared.go"), "package x // peer edit\n")
	peerGit(t, f.elsewhere, "commit", "-q", "-am", "peer edit")
	peerWrite(t, filepath.Join(f.elsewhere, "peer-only.go"), "package x\n")
	peerWrite(t, filepath.Join(f.claudeWorktree, "shared.go"), "package x // caller edit\n")
	peerGit(t, f.otherRepo, "init", "-q", "-b", "main")
	return f
}

func callPeers(t *testing.T, query string, sessions []SessionSummary, presence presenceOpenSet) sessionPeersResponse {
	t.Helper()
	setIndexPath(t.TempDir()) // daemon.json defaults, never the real data dir
	recorder := httptest.NewRecorder()
	handleSessionPeersWith(recorder, httptest.NewRequest("GET", "/api/sessions/peers?"+query, nil),
		func() []SessionSummary { return sessions }, func(time.Time) presenceOpenSet { return presence })
	if recorder.Code != 200 {
		t.Fatalf("status %d: %s", recorder.Code, recorder.Body.String())
	}
	var out sessionPeersResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func peerByID(peers []sessionPeer, id string) (sessionPeer, bool) {
	for _, peer := range peers {
		if peer.ID == id {
			return peer, true
		}
	}
	return sessionPeer{}, false
}

func TestSessionPeersFoldsWorktreesAndStatesMergeFacts(t *testing.T) {
	f := newPeersFixture(t)
	now := time.Now()
	caller := SessionSummary{Runtime: "claude", ID: "me", Cwd: f.claudeWorktree, Modified: now}
	sameCheckout := SessionSummary{Runtime: "opencode", ID: "same", Cwd: f.claudeWorktree, Modified: now.Add(-time.Minute)}
	sibling := SessionSummary{Runtime: "codex", ID: "rollout-x", ThreadID: "thread-x", Cwd: f.elsewhere, Modified: now.Add(-2 * time.Minute)}
	mainCheckout := SessionSummary{Runtime: "codex", ID: "main-one", Cwd: f.main, Modified: now.Add(-3 * time.Minute)}
	other := SessionSummary{Runtime: "claude", ID: "other", Cwd: f.otherRepo, Modified: now.Add(-4 * time.Minute)}
	closed := SessionSummary{Runtime: "claude", ID: "closed", Cwd: f.elsewhere, Modified: now}
	all := []SessionSummary{caller, sameCheckout, sibling, mainCheckout, other, closed}
	presence := openFixture(caller, sameCheckout, sibling, mainCheckout, other)

	out := callPeers(t, "runtime=claude&id=me&cwd=/ignored", all, presence)
	if !out.Caller.Identified || out.Caller.PlaceSource != "catalog" || out.State != "available" {
		t.Fatalf("caller = %+v state %s", out.Caller, out.State)
	}
	if out.Caller.MemoryRepository != "repo" {
		t.Fatalf("memory label %q, want the repository's", out.Caller.MemoryRepository)
	}
	if _, found := peerByID(out.Peers, "me"); found {
		t.Fatal("the identified caller listed itself")
	}
	if _, found := peerByID(out.Peers, "closed"); found {
		t.Fatal("a session that is not open was listed")
	}
	if _, found := peerByID(out.Peers, "other"); found || out.Counts[peerOtherRepository] != 1 {
		t.Fatalf("scope=repository must count, not list, another repository: %v", out.Counts)
	}
	if peer, _ := peerByID(out.Peers, "same"); peer.Relationship != peerSameCheckout || peer.Git != nil {
		t.Fatalf("same checkout = %+v", peer)
	}
	// Both other checkouts of the repository are siblings: the ../elsewhere one
	// the rail's Claude-only fold would miss, and the main checkout itself.
	for _, id := range []string{"rollout-x", "main-one"} {
		if peer, _ := peerByID(out.Peers, id); peer.Relationship != peerSiblingWorktree {
			t.Fatalf("%s = %+v, want sibling_worktree", id, peer)
		}
	}
	peer, _ := peerByID(out.Peers, "rollout-x")
	if peer.Git == nil || peer.Git.Branch != "elsewhere-branch" || peer.Git.Base.Ahead != 1 || peer.Git.Upstream.State != "none" {
		t.Fatalf("sibling git facts = %+v", peer.Git)
	}
	if !reflect.DeepEqual(peer.Git.ChangedFiles.Files, []string{"peer-only.go", "shared.go"}) {
		t.Fatalf("sibling changed files = %v", peer.Git.ChangedFiles.Files)
	}
	if !reflect.DeepEqual(peer.OverlapFiles, []string{"shared.go"}) {
		t.Fatalf("overlap = %v", peer.OverlapFiles)
	}
	if peer.Git.PullRequest.State != "unavailable" {
		t.Fatal("the missing pull-request source must be stated")
	}
	if out.Caller.Git == nil || !reflect.DeepEqual(out.Caller.Git.ChangedFiles.Files, []string{"shared.go"}) {
		t.Fatalf("caller git = %+v", out.Caller.Git)
	}

	wide := callPeers(t, "runtime=claude&id=me&scope=all", all, presence)
	if peer, _ := peerByID(wide.Peers, "other"); peer.Relationship != peerOtherRepository || peer.Git != nil {
		t.Fatalf("scope=all other repository = %+v", peer)
	}
}

func TestSessionPeersFromSessionKeepsTheCallerExcluded(t *testing.T) {
	f := newPeersFixture(t)
	now := time.Now()
	helper := SessionSummary{Runtime: "claude", ID: "helper", Cwd: f.main, Modified: now}
	watched := SessionSummary{Runtime: "codex", ID: "rollout-w", ThreadID: "thread-w", Cwd: f.claudeWorktree, Modified: now}
	sibling := SessionSummary{Runtime: "opencode", ID: "sib", Cwd: f.elsewhere, Modified: now}
	presence := openFixture(helper, watched, sibling)
	out := callPeers(t, "runtime=claude&id=helper&from_runtime=codex&from_id=thread-w", []SessionSummary{helper, watched, sibling}, presence)
	if out.Caller.PlaceSource != "catalog:from_session" || out.Caller.CheckoutRoot == "" ||
		!strings.HasSuffix(out.Caller.CheckoutRoot, filepath.Join(".claude", "worktrees", "w1")) {
		t.Fatalf("place = %+v", out.Caller)
	}
	for _, id := range []string{"helper", "rollout-w"} {
		if _, found := peerByID(out.Peers, id); found {
			t.Fatalf("%s must be excluded", id)
		}
	}
	if peer, _ := peerByID(out.Peers, "sib"); peer.Relationship != peerSiblingWorktree {
		t.Fatalf("sibling from the watched session's place = %+v", peer)
	}
}

func TestSessionPeersUnavailablePresenceIsNotAnEmptyAnswer(t *testing.T) {
	f := newPeersFixture(t)
	out := callPeers(t, "cwd="+f.main+"&cwd_source=process_cwd", []SessionSummary{{Runtime: "codex", ID: "x", Cwd: f.main}},
		presenceOpenSet{Capability: sessionactivity.Capability{Status: "unavailable", Detail: "sampler down"}})
	if out.State != "unavailable" || out.Detail != "sampler down" || len(out.Peers) != 0 || out.Caller.Identified {
		t.Fatalf("unavailable presence = %+v", out)
	}
}

func TestSessionPeersNonGitAndMissingFolders(t *testing.T) {
	f := newPeersFixture(t)
	now := time.Now()
	same := SessionSummary{Runtime: "claude", ID: "same-folder", Cwd: f.plainFolder, Modified: now}
	gone := SessionSummary{Runtime: "claude", ID: "gone", Cwd: filepath.Join(f.plainFolder, "deleted"), Modified: now}
	out := callPeers(t, "cwd="+f.plainFolder, []SessionSummary{same, gone}, openFixture(same, gone))
	if out.Caller.Resolution != "folder" || out.Caller.Identified {
		t.Fatalf("caller = %+v", out.Caller)
	}
	if peer, _ := peerByID(out.Peers, "same-folder"); peer.Relationship != peerSameCheckout {
		t.Fatalf("same non-git folder = %+v", peer)
	}
	if peer, _ := peerByID(out.Peers, "gone"); peer.Relationship != peerUnresolved || peer.Reason == "" {
		t.Fatalf("missing folder = %+v", peer)
	}
}

func TestSessionPeersCapsEachRelationshipAlone(t *testing.T) {
	f := newPeersFixture(t)
	now := time.Now()
	sessions := []SessionSummary{{Runtime: "claude", ID: "sib", Cwd: f.elsewhere, Modified: now.Add(-time.Hour)}}
	for i := 0; i < 25; i++ {
		sessions = append(sessions, SessionSummary{Runtime: "claude", ID: "other-" + string(rune('a'+i)), Cwd: f.otherRepo, Modified: now})
	}
	out := callPeers(t, "cwd="+f.main+"&scope=all", sessions, openFixture(sessions...))
	if _, found := peerByID(out.Peers, "sib"); !found {
		t.Fatal("25 newer sessions elsewhere crowded out the same-repository peer")
	}
	if out.Counts[peerOtherRepository] != 25 || !out.Truncated || out.CandidatesTotal != 26 {
		t.Fatalf("counts %+v truncated %v total %d", out.Counts, out.Truncated, out.CandidatesTotal)
	}
}

func TestSessionPeersPlaceOnlyReadsNoCandidates(t *testing.T) {
	f := newPeersFixture(t)
	peer := SessionSummary{Runtime: "codex", ID: "p", Cwd: f.elsewhere, Modified: time.Now()}
	out := callPeers(t, "cwd="+f.claudeWorktree+"&place_only=1", []SessionSummary{peer}, openFixture(peer))
	if out.State != "place_only" || len(out.Peers) != 0 || out.Caller.MemoryRepository != "repo" ||
		out.Caller.Git != nil || !strings.HasSuffix(out.Caller.RepositoryRoot, "repo") {
		t.Fatalf("place only = %+v", out)
	}
}

// Crossing Guard's own agent sessions run in the binding's root, so each would
// read as "someone shares your checkout". They are excluded by the run and
// helper-session linkage unless asked for; an owner's session in the same
// folder stays a peer.
func TestSessionPeersExcludesAgentSessionsByLinkage(t *testing.T) {
	f := newPeersFixture(t)
	now := time.Now()
	owner := SessionSummary{Runtime: "claude", ID: "owner-task", Cwd: f.main, Modified: now}
	helper := SessionSummary{Runtime: "claude", ID: "helper-session", Cwd: f.main, Modified: now}
	sessions := []SessionSummary{owner, helper}
	restore := agentSessionParentsRead
	t.Cleanup(func() { agentSessionParentsRead = restore })
	agentSessionParentsRead = func([]string) map[string]store.AgentSessionParent {
		return map[string]store.AgentSessionParent{"helper-session": {Role: "helper", RootRuntime: "codex", RootCatalogSessionID: "root-1"}}
	}

	out := callPeers(t, "cwd="+f.main, sessions, openFixture(sessions...))
	if _, found := peerByID(out.Peers, "helper-session"); found || out.AgentSessionsExcluded != 1 || out.AgentSessions != "excluded" {
		t.Fatalf("default = %+v", out)
	}
	if peer, _ := peerByID(out.Peers, "owner-task"); peer.Relationship != peerSameCheckout || peer.AgentOf != nil {
		t.Fatalf("the owner's session must stay a peer: %+v", peer)
	}
	wide := callPeers(t, "cwd="+f.main+"&include_agents=1", sessions, openFixture(sessions...))
	peer, found := peerByID(wide.Peers, "helper-session")
	if !found || peer.AgentOf == nil || peer.AgentOf.ID != "root-1" || peer.AgentOf.Role != "helper" {
		t.Fatalf("include_agents = %+v", peer)
	}
	// An unreadable linkage is said, never read as "no agents".
	agentSessionParentsRead = func([]string) map[string]store.AgentSessionParent { return nil }
	if unknown := callPeers(t, "cwd="+f.main, sessions, openFixture(sessions...)); unknown.AgentSessions != "unknown" {
		t.Fatalf("unreadable linkage = %q", unknown.AgentSessions)
	}
}

// A codex subagent rollout carries its parent's thread id and is often the
// newest row with it. Identified by thread, the caller is the primary rollout
// (and every resumed segment of it), never the child.
func TestSessionPeersIdentifiesACodexCallerByThreadNotItsChild(t *testing.T) {
	f := newPeersFixture(t)
	now := time.Now()
	primary := SessionSummary{Runtime: "codex", ID: "rollout-primary", ThreadID: "thread-a", Cwd: f.main, Modified: now.Add(-time.Minute)}
	resumed := SessionSummary{Runtime: "codex", ID: "rollout-resumed", ThreadID: "thread-a", Cwd: f.main, Modified: now.Add(-30 * time.Second)}
	child := SessionSummary{Runtime: "codex", ID: "rollout-child", ThreadID: "thread-a", LineageKind: "native-thread-spawn",
		Cwd: f.claudeWorktree, Modified: now}
	sessions := []SessionSummary{child, resumed, primary}
	out := callPeers(t, "runtime=codex&id=thread-a", sessions, openFixture(sessions...))
	if !out.Caller.Identified || !strings.HasSuffix(out.Caller.CheckoutRoot, "repo") {
		t.Fatalf("caller placed at the child's folder: %+v", out.Caller)
	}
	for _, id := range []string{"rollout-primary", "rollout-resumed"} {
		if _, found := peerByID(out.Peers, id); found {
			t.Fatalf("the caller's own segment %s was listed as a peer", id)
		}
	}
	if peer, found := peerByID(out.Peers, "rollout-child"); !found || peer.Relationship != peerSiblingWorktree {
		t.Fatalf("the child is another session: %+v", peer)
	}
}

// A watched session that is not in the catalog is said, not silently replaced.
func TestSessionPeersNamesAMissingWatchedSession(t *testing.T) {
	f := newPeersFixture(t)
	out := callPeers(t, "cwd="+f.main+"&from_runtime=codex&from_id=nope", nil, openFixture())
	if out.Caller.FromSessionState != "not_found" || out.Caller.PlaceSource != "request" {
		t.Fatalf("missing watched session = %+v", out.Caller)
	}
}

// The route's deadline turns unreached work into stated gaps, never dropped rows.
func TestPeerWorkPastTheDeadlineIsUnresolvedNotDropped(t *testing.T) {
	f := newPeersFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	limits := defaultConsoleConfig().Recall
	places := resolvePlaces(ctx, []string{f.main, f.elsewhere}, limits)
	for _, folder := range []string{f.main, f.elsewhere} {
		if places[folder].kind != placeUnresolved || places[folder].reason != reasonDeadline {
			t.Fatalf("%s = %+v, want unresolved: deadline", folder, places[folder])
		}
	}
	facts := readPeerFacts(ctx, []string{f.main}, defaultConsoleConfig(), limits)
	emitted := emitPeerFacts(facts[f.main], limits.PeerChangedFilesMax)
	if emitted.ChangedFiles.State != "unavailable" || emitted.ChangedFiles.Reason != reasonDeadline {
		t.Fatalf("facts past the deadline = %+v", emitted.ChangedFiles)
	}
	started := 0
	runBounded(ctx, 5, 2, func(int) { started++ })
	if started != 0 {
		t.Fatalf("runBounded started %d calls after the deadline", started)
	}
}

func TestMemoryExcerptStaysWithinItsBudget(t *testing.T) {
	for _, text := range []string{strings.Repeat("a", 1000), strings.Repeat("é", 500)} {
		if got := memoryExcerpt(text, 400); len(got) > 400 || !strings.HasSuffix(got, "…") {
			t.Fatalf("excerpt of %d bytes is %d bytes: %q", len(text), len(got), got[len(got)-8:])
		}
	}
}
