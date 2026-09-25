package changeenv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"crossing-guard/codemap"
	"crossing-guard/store"
)

func testRepo(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", d}, args...)...)
		if b, e := c.CombinedOutput(); e != nil {
			t.Fatalf("git %v: %v %s", args, e, b)
		}
	}
	run("init", "-q")
	if e := os.WriteFile(filepath.Join(d, "base.txt"), []byte("base\n"), 0600); e != nil {
		t.Fatal(e)
	}
	run("add", "base.txt")
	run("-c", "user.name=CrossingGuard", "-c", "user.email=crossing-guard@example.invalid", "commit", "-q", "-m", "base")
	return d
}
func openIndex(t *testing.T) *store.Index {
	t.Helper()
	ix, e := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { ix.Close() })
	return ix
}

func TestCaptureSnapshotRecordSeparatesGitCaptureFromPersistence(t *testing.T) {
	repo := testRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	record, err := CaptureSnapshotRecord(context.Background(), SnapshotInput{SessionID: "attachment", RepoDir: repo, Base: "HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	if record.ID != 0 || record.SessionID != "attachment" || record.Kind != "revision" || record.SnapshotDigest == "" {
		t.Fatalf("captured record = %+v", record)
	}
	ix := openIndex(t)
	recs, total, err := ix.ChangeRecordsForSession("attachment", 10)
	if err != nil || total != 0 || len(recs) != 0 {
		t.Fatalf("capture persisted unexpectedly: total=%d rows=%d err=%v", total, len(recs), err)
	}
}

func TestCaptureCheckpointEvidenceRetainsBoundedChangedPathBodies(t *testing.T) {
	repo := testRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("new body\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	record, payloads, err := CaptureCheckpointEvidence(context.Background(), SnapshotInput{
		SessionID: "checkpoint", RepoDir: repo, Base: "HEAD", RetainBodies: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.ID != 0 || record.SnapshotDigest == "" {
		t.Fatalf("record = %+v", record)
	}
	bodies := map[string]string{}
	patches := map[string]string{}
	for _, payload := range payloads {
		if payload.ContentCompleteness == "complete" {
			bodies[payload.Path] = string(payload.ContentPayload)
			if payload.ContentBytes != len(payload.ContentPayload) || payload.ContentDigest == "" {
				t.Fatalf("payload metadata = %+v", payload)
			}
		}
		if payload.PatchCompleteness == "complete" {
			patches[payload.Path] = string(payload.PatchPayload)
			if payload.PatchBytes != len(payload.PatchPayload) || payload.PatchDigest == "" {
				t.Fatalf("patch metadata = %+v", payload)
			}
		}
	}
	if bodies["base.txt"] != "changed\n" || bodies["new.txt"] != "new body\n" {
		t.Fatalf("retained bodies = %#v payloads=%+v", bodies, payloads)
	}
	if !strings.Contains(patches["base.txt"], "-base") || !strings.Contains(patches["base.txt"], "+changed") {
		t.Fatalf("retained patches = %#v payloads=%+v", patches, payloads)
	}
}

func TestCheckpointEvidenceUsesExactCommittedIndexAndWorktreeFrames(t *testing.T) {
	repo := testRepo(t)
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		body, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, body)
		}
		return strings.TrimSpace(string(body))
	}
	base := run("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("head\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "base.txt")
	run("-c", "user.name=CrossingGuard", "-c", "user.email=crossing-guard@example.invalid",
		"commit", "-q", "-m", "head")
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("index\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "base.txt")
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("worktree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("untracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, payloads, err := CaptureCheckpointEvidence(context.Background(), SnapshotInput{
		SessionID: "layer-frames", RepoDir: repo, Base: base, RetainBodies: true})
	if err != nil {
		t.Fatal(err)
	}
	byLayer := map[string]store.CheckpointPayload{}
	for _, payload := range payloads {
		if payload.Path == "base.txt" || payload.Path == "new.txt" {
			byLayer[payload.Layer+":"+payload.Path] = payload
		}
	}
	for key, want := range map[string]string{
		"committed:base.txt": "head\n",
		"index:base.txt":     "index\n",
		"worktree:base.txt":  "worktree\n",
		"untracked:new.txt":  "untracked\n",
	} {
		got, ok := byLayer[key]
		if !ok || string(got.ContentPayload) != want || got.ContentCompleteness != "complete" {
			t.Fatalf("%s payload=%+v want body %q", key, got, want)
		}
	}
	for key, change := range map[string][2]string{
		"committed:base.txt": {"-base", "+head"},
		"index:base.txt":     {"-head", "+index"},
		"worktree:base.txt":  {"-index", "+worktree"},
	} {
		patch := string(byLayer[key].PatchPayload)
		if !strings.Contains(patch, change[0]) || !strings.Contains(patch, change[1]) {
			t.Fatalf("%s patch=%q want %q and %q", key, patch, change[0], change[1])
		}
	}
	if got := byLayer["untracked:new.txt"]; got.PatchPayload != nil ||
		!strings.HasPrefix(got.SourceIdentity, "absent-index->filesystem:") {
		t.Fatalf("untracked predecessor evidence=%+v", got)
	}
}

func TestCheckpointMetadataOnlyComputesFactsWithoutRetainingBodies(t *testing.T) {
	repo := testRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, payloads, err := CaptureCheckpointEvidence(context.Background(), SnapshotInput{
		SessionID: "metadata-only", RepoDir: repo, Base: "HEAD", RetainBodies: false})
	if err != nil {
		t.Fatal(err)
	}
	if len(payloads) == 0 {
		t.Fatal("no checkpoint facts returned")
	}
	for _, payload := range payloads {
		if payload.ContentPayload != nil || payload.PatchPayload != nil {
			t.Fatalf("metadata-only capture retained a body: %+v", payload)
		}
		if payload.ContentBytes > 0 && (payload.ContentDigest == "" || payload.ContentCompleteness != "metadata-only") {
			t.Fatalf("metadata-only content facts incomplete: %+v", payload)
		}
	}
}

func TestCheckpointFileOpenRejectsSymlinkedParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink privileges vary on Windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "redirect")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := readCheckpointRegular(root, "redirect/secret.txt"); err == nil {
		t.Fatal("checkpoint capture followed a symlinked parent")
	}
}

func TestCaptureCheckpointEvidenceRejectsParentReplacedBySymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("component-wise no-follow capture is unavailable on Windows")
	}
	repo := testRepo(t)
	nested := filepath.Join(repo, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(nested, "tracked.txt")
	if err := os.WriteFile(target, []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "nested/tracked.txt"},
		{"-c", "user.name=CrossingGuard", "-c", "user.email=crossing-guard@example.invalid",
			"commit", "-q", "-m", "nested"}} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if body, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, body)
		}
	}
	if err := os.WriteFile(target, []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "tracked.txt"), []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prior := checkpointPathOpenTestHook
	var hookErr error
	checkpointPathOpenTestHook = func(relative string) {
		if relative != "nested/tracked.txt" || checkpointPathOpenTestHook == nil {
			return
		}
		checkpointPathOpenTestHook = nil
		if err := os.Rename(nested, nested+".original"); err != nil {
			hookErr = err
			return
		}
		hookErr = os.Symlink(outside, nested)
	}
	t.Cleanup(func() { checkpointPathOpenTestHook = prior })
	_, payloads, err := CaptureCheckpointEvidence(context.Background(), SnapshotInput{
		SessionID: "parent-replacement", RepoDir: repo, Base: "HEAD", RetainBodies: true})
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if err == nil || payloads != nil || !strings.Contains(err.Error(), "secure checkpoint content") {
		t.Fatalf("parent replacement produced evidence: payloads=%+v err=%v", payloads, err)
	}
}

func TestCaptureCheckpointEvidenceRejectsManifestParentReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("component-wise no-follow capture is unavailable on Windows")
	}
	repo := testRepo(t)
	nested := filepath.Join(repo, "untracked")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "outside.txt"), []byte("local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "outside.txt"), []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prior := manifestPathOpenTestHook
	var hookErr error
	manifestPathOpenTestHook = func(relative string) {
		if relative != "untracked/outside.txt" || manifestPathOpenTestHook == nil {
			return
		}
		manifestPathOpenTestHook = nil
		if err := os.Rename(nested, nested+".original"); err != nil {
			hookErr = err
			return
		}
		hookErr = os.Symlink(outside, nested)
	}
	t.Cleanup(func() { manifestPathOpenTestHook = prior })
	_, payloads, err := CaptureCheckpointEvidence(context.Background(), SnapshotInput{
		SessionID: "manifest-parent-replacement", RepoDir: repo, Base: "HEAD", RetainBodies: true})
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if err == nil || payloads != nil {
		t.Fatalf("manifest parent replacement produced evidence: payloads=%+v err=%v", payloads, err)
	}
}

func TestCheckpointReplacementRaceCannotCaptureReplacementTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("component-wise no-follow capture is unavailable on Windows")
	}
	repo := testRepo(t)
	target := filepath.Join(repo, "base.txt")
	if err := os.WriteFile(target, []byte("trusted-change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("outside-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prior := checkpointReadTestHook
	var hookErr error
	checkpointReadTestHook = func() {
		checkpointReadTestHook = nil
		if err := os.Rename(target, target+".replaced"); err != nil {
			hookErr = err
			return
		}
		hookErr = os.Symlink(outside, target)
	}
	t.Cleanup(func() { checkpointReadTestHook = prior })
	_, payloads, err := CaptureCheckpointEvidence(context.Background(), SnapshotInput{
		SessionID: "replacement-race", RepoDir: repo, Base: "HEAD", RetainBodies: true})
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if err == nil || payloads != nil {
		t.Fatalf("replacement race produced evidence: payloads=%+v err=%v", payloads, err)
	}
}

func TestClaimSnapshotAndViewKeepEvidenceClassesSeparate(t *testing.T) {
	repo := testRepo(t)
	plan := filepath.Join(repo, "plan.md")
	if e := os.WriteFile(plan, []byte("claim bytes"), 0600); e != nil {
		t.Fatal(e)
	}
	ix := openIndex(t)
	decl, e := RecordClaim(ix, "declaration", ClaimInput{SessionID: "s", RepoDir: repo, SourcePath: plan, Intent: "intent", Paths: []string{"base.txt", "planned.txt"}})
	if e != nil {
		t.Fatal(e)
	}
	if decl.EvidenceClass != "claimed" {
		t.Fatal("declaration was not claimed")
	}
	if e := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("changed\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(repo, "extra.txt"), []byte("extra\n"), 0600); e != nil {
		t.Fatal(e)
	}
	snap, e := RecordSnapshot(ix, SnapshotInput{SessionID: "s", RepoDir: repo, Base: "HEAD"})
	if e != nil {
		t.Fatal(e)
	}
	if snap.EvidenceClass != "observed" {
		t.Fatal("snapshot was not observed")
	}
	v, e := Build(ix, "s", ViewOptions{})
	if e != nil {
		t.Fatal(e)
	}
	r := v.Repositories[0]
	if !r.Divergence.Available || r.Counts["omitted_scope"] != 1 || r.Counts["added_scope"] < 1 {
		t.Fatalf("bad divergence: %+v", r)
	}
	for _, f := range r.Files {
		if f.Path == "planned.txt" && f.Changed != nil {
			t.Fatal("planned-only path promoted to changed")
		}
	}
}

func TestTouchOnlyViewShowsFactsWithoutManufacturingEnvelope(t *testing.T) {
	ix := openIndex(t)
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	id := "file:/repo/touched.go"
	if err := tx.UpsertEntity(id, "file", "/repo/touched.go", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.AppendEvent(store.EventRecord{TS: 1, SessionID: "touch-only", Verb: "write", Tool: "Write", TargetEntityID: id, Origin: "live"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	v, err := Build(ix, "touch-only", ViewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if v.Available || v.GovernedEvents != 1 || v.TouchCount != 1 || len(v.ObservedTouches) != 1 {
		t.Fatalf("touch-only view = %+v", v)
	}
	if strings.Contains(v.Reason, "repository") || len(v.Repositories) != 0 {
		t.Fatalf("touch-only view manufactured repository identity: %+v", v)
	}
}

func TestSourceSymlinkAndOutsidePathAreRefused(t *testing.T) {
	repo := testRepo(t)
	outside := filepath.Join(t.TempDir(), "outside.md")
	os.WriteFile(outside, []byte("x"), 0600)
	link := filepath.Join(repo, "plan.md")
	if e := os.Symlink(outside, link); e != nil {
		t.Fatal(e)
	}
	ix := openIndex(t)
	if _, e := RecordClaim(ix, "declaration", ClaimInput{SessionID: "s", RepoDir: repo, SourcePath: link, Intent: "x", Paths: []string{"base.txt"}}); e == nil {
		t.Fatal("source symlink accepted")
	}
	os.Remove(link)
	os.WriteFile(link, []byte("x"), 0600)
	if _, e := RecordClaim(ix, "declaration", ClaimInput{SessionID: "s", RepoDir: repo, SourcePath: link, Intent: "x", Paths: []string{outside}}); e == nil {
		t.Fatal("outside path accepted")
	}
}

func TestVerifyRunSeparatesClaimedNameFromObservedResult(t *testing.T) {
	repo := testRepo(t)
	ix := openIndex(t)
	r, code, e := VerifyRun(ix, VerifyInput{SessionID: "s", RepoDir: repo, CWD: repo, Name: "claimed meaning", Boundary: "focused", Timeout: 5 * time.Second, Command: []string{"/usr/bin/false"}})
	if e != nil {
		t.Fatal(e)
	}
	if code == 0 || r.EvidenceClass != "observed" || r.VerificationNameClass != "claimed" || r.VerificationResult != "fail" {
		t.Fatalf("bad witness: code=%d %+v", code, r)
	}
}

func TestTwoCheckoutsDoNotSupersede(t *testing.T) {
	a := testRepo(t)
	b := testRepo(t)
	ix := openIndex(t)
	for _, repo := range []string{a, b} {
		p := filepath.Join(repo, "plan.md")
		os.WriteFile(p, []byte(repo), 0600)
		if _, e := RecordClaim(ix, "declaration", ClaimInput{SessionID: "s", RepoDir: repo, SourcePath: p, Intent: "x", Paths: []string{"base.txt"}}); e != nil {
			t.Fatal(e)
		}
	}
	v, e := Build(ix, "s", ViewOptions{RepositoryLimit: 1})
	if e != nil {
		t.Fatal(e)
	}
	if v.RepositoryCount != 2 || v.RepositoryPage.Returned != 1 || v.RepositoryPage.NextOffset == nil {
		t.Fatalf("repository paging lost a checkout: %+v", v.RepositoryPage)
	}
}

func appendFileEvent(t *testing.T, ix *store.Index, session, path, origin string, ts int64) {
	t.Helper()
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	id := "file:" + path
	if err := tx.UpsertEntity(id, "file", path, ts); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if _, err := tx.AppendEvent(store.EventRecord{TS: ts, SessionID: session, Verb: "write", Tool: "fixture", TargetEntityID: id, Origin: origin}, nil); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestTouchedEvidenceIsIndependentAndUsesFullEventPopulation(t *testing.T) {
	repo := testRepo(t)
	plan := filepath.Join(repo, "plan.md")
	if err := os.WriteFile(plan, []byte("plan"), 0600); err != nil {
		t.Fatal(err)
	}
	ix := openIndex(t)
	if _, err := RecordClaim(ix, "declaration", ClaimInput{SessionID: "touches", RepoDir: repo, SourcePath: plan, Intent: "touch test", Paths: []string{"planned-only.txt"}}); err != nil {
		t.Fatal(err)
	}
	common := filepath.Join(repo, "common.txt")
	late := filepath.Join(repo, "late-only.txt")
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{common, late} {
		if err := tx.UpsertEntity("file:"+path, "file", path, 1); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	for i := 0; i < 2001; i++ {
		if _, err := tx.AppendEvent(store.EventRecord{TS: int64(i + 1), SessionID: "touches", Verb: "write", Tool: "fixture", TargetEntityID: "file:" + common, Origin: "imported"}, nil); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if _, err := tx.AppendEvent(store.EventRecord{TS: 3000, SessionID: "touches", Verb: "write", Tool: "fixture", TargetEntityID: "file:" + late, Origin: "live"}, nil); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	v, err := Build(ix, "touches", ViewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]FileRow{}
	for _, row := range v.Repositories[0].Files {
		rows[row.Path] = row
	}
	if rows["planned-only.txt"].Touched != nil {
		t.Fatal("planned evidence was promoted to a touch")
	}
	if rows["late-only.txt"].Touched == nil || rows["late-only.txt"].Changed != nil {
		t.Fatalf("late T-only evidence lost or promoted: row=%+v all=%+v gaps=%+v", rows["late-only.txt"], v.Repositories[0].Files, v.Repositories[0].Gaps)
	}
	if rows["common.txt"].Touched == nil || rows["common.txt"].Touched.Events != 2001 || rows["common.txt"].Touched.Lineage != "imported" {
		t.Fatalf("full touch population not counted: %+v", rows["common.txt"])
	}
}

func TestObservedTargetPlacementKeepsIndependentRootFacts(t *testing.T) {
	selected := t.TempDir()
	nested := filepath.Join(selected, "nested")
	other := t.TempDir()
	if err := os.MkdirAll(nested, 0700); err != nil {
		t.Fatal(err)
	}
	selected, _ = filepath.EvalSymlinks(selected)
	nested, _ = filepath.EvalSymlinks(nested)
	other, _ = filepath.EvalSymlinks(other)
	touches := []store.SessionFileTouch{
		{Identity: filepath.Join(selected, "one.go"), Lineage: "live", Events: 1},
		{Identity: filepath.Join(other, "two.go"), Lineage: "live", Events: 2},
		{Identity: filepath.Join(nested, "three.go"), Lineage: "live", Events: 3},
		{Identity: string([]byte{0}), Lineage: "live", Events: 4},
	}
	if filepath.Separator == '/' {
		touches = append(touches, store.SessionFileTouch{Identity: "/tmp/crossing-guard-c9i-scratch/five.php", Lineage: "live", Events: 5})
	}
	checkouts := []checkoutRoot{{repositoryID: "r1", checkoutID: "c1", display: "selected", root: selected}, {repositoryID: "r2", checkoutID: "c2", display: "nested", root: nested}, {repositoryID: "r3", checkoutID: "c3", display: "other", root: other}}
	placements := placeTouches(touches, []string{selected}, checkouts, "r1\x00c1")
	if got := placements[0].view; got.Scope != "selected-checkout" || got.WorkingDirectory.State != "inside" || got.TemporaryRoot.State != "inside" || len(got.CheckoutMatches) != 1 || !got.CheckoutMatches[0].Selected {
		t.Fatalf("selected placement lost independent facts: %+v", got)
	}
	if got := placements[1].view; got.Scope != "other-checkout" || len(got.CheckoutMatches) != 1 || got.CheckoutMatches[0].Selected {
		t.Fatalf("other checkout placement wrong: %+v", got)
	}
	if got := placements[2].view; got.Scope != "ambiguous-checkout" || len(got.CheckoutMatches) != 2 {
		t.Fatalf("nested roots were not retained as ambiguous: %+v", got)
	}
	if got := placements[3].view; got.Scope != "unresolved" || got.Resolution != "unresolved" || got.ResolutionReason == "" {
		t.Fatalf("resolution failure was hidden: %+v", got)
	}
	if filepath.Separator == '/' {
		if got := placements[4].view; got.Scope != "temporary" || got.TemporaryRoot.State != "inside" || got.TemporaryRoot.Root == "" {
			t.Fatalf("canonical Unix temporary root was not retained: %+v", got)
		}
	}
}

func TestObservedTargetProjectionPagesFiltersAndQualifiesCounts(t *testing.T) {
	root := t.TempDir()
	ix := openIndex(t)
	for i := 0; i < 31; i++ {
		appendFileEvent(t, ix, "observed-page", filepath.Join(root, fmt.Sprintf("file-%02d.go", i)), "live", int64(i+1))
	}
	v, err := Build(ix, "observed-page", ViewOptions{SessionRoots: []string{root}, ObservedScope: "working-directory", ObservedOffset: 5, ObservedLimit: 7})
	if err != nil {
		t.Fatal(err)
	}
	if v.Available || v.ObservedPage.Total != 31 || v.ObservedPage.Offset != 5 || v.ObservedPage.Returned != 7 || v.ObservedPage.NextOffset == nil || !v.ObservedExact || v.ObservedCounts["working-directory"] != 31 {
		t.Fatalf("observed paging/count contract wrong: %+v", v)
	}
	for _, target := range v.ObservedTouches {
		if target.Scope != "working-directory" || target.WorkingDirectory.State != "inside" {
			t.Fatalf("scope filter returned wrong target: %+v", target)
		}
	}
	partialPlacements := placeTouches([]store.SessionFileTouch{{Identity: filepath.Join(root, "known.go")}}, []string{root}, nil, "")
	_, _, counts, exact := observedProjection(partialPlacements, store.SessionTouchPopulation{Partial: true}, ViewOptions{ObservedScope: "all"})
	if exact || counts["working-directory"] != 1 {
		t.Fatalf("bounded scope counts were presented as exact: exact=%v counts=%v", exact, counts)
	}
}

func TestObservedWorkingDirectoryConflictAndNoRepositoryResponseFitting(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	placements := placeTouches([]store.SessionFileTouch{{Identity: filepath.Join(a, "file.go")}}, []string{a, b}, nil, "")
	if got := placements[0].view.WorkingDirectory; got.State != "unavailable" || !strings.Contains(got.Reason, "different working directories") {
		t.Fatalf("conflicting resumed roots chose a winner: %+v", got)
	}
	view := SessionView{ObservedPage: page(80, 0, 80)}
	for i := 0; i < 80; i++ {
		view.ObservedTouches = append(view.ObservedTouches, ObservedTarget{Identity: strings.Repeat("x", 500), DisplayPath: strconv.Itoa(i), Scope: "outside-identified-roots"})
	}
	if err := FitResponse(&view, 5000); err != nil {
		t.Fatal(err)
	}
	if len(view.ObservedTouches) >= 80 || view.ObservedPage.NextOffset == nil || view.ObservedPage.Total != 80 {
		t.Fatalf("no-repository response was not reduced with paging intact: rows=%d page=%+v", len(view.ObservedTouches), view.ObservedPage)
	}
}

func TestMissingAndNonFileCaptureNeverRenderAsZeroTouches(t *testing.T) {
	repo := testRepo(t)
	source := filepath.Join(repo, "plan.md")
	if err := os.WriteFile(source, []byte("plan"), 0600); err != nil {
		t.Fatal(err)
	}
	ix := openIndex(t)
	for _, session := range []string{"no-events", "shell-only"} {
		if _, err := RecordClaim(ix, "declaration", ClaimInput{SessionID: session, RepoDir: repo, SourcePath: source, Intent: "x", Paths: []string{"base.txt"}}); err != nil {
			t.Fatal(err)
		}
	}
	v, err := Build(ix, "no-events", ViewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if v.Repositories[0].Completeness["touched"] || !hasGap(v.Repositories[0].Gaps, "touch_capture_missing") {
		t.Fatalf("missing capture rendered complete: %+v", v.Repositories[0])
	}
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.AppendEvent(store.EventRecord{TS: 1, SessionID: "shell-only", Verb: "exec", Tool: "shell", Origin: "live"}, nil); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	v, err = Build(ix, "shell-only", ViewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !v.Repositories[0].Completeness["touched"] || hasGap(v.Repositories[0].Gaps, "touch_non_file_actions") || v.Repositories[0].Counts["non_file_events"] != 1 {
		t.Fatalf("fileless action was misreported as failed file attribution: %+v", v.Repositories[0])
	}
}

func TestAbsentExplicitAnnotationsAreNotNaturalCollectionFailures(t *testing.T) {
	repo := testRepo(t)
	ix := openIndex(t)
	if _, err := RecordSnapshot(ix, SnapshotInput{SessionID: "automatic", RepoDir: repo, Base: "HEAD"}); err != nil {
		t.Fatal(err)
	}
	v, err := Build(ix, "automatic", ViewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Repositories) != 1 {
		t.Fatalf("snapshot repository missing: %+v", v)
	}
	repoView := v.Repositories[0]
	for _, code := range []string{"declaration_missing", "implementation_missing", "verification_missing", "declaration_annotation_absent", "implementation_annotation_absent", "process_witness_absent"} {
		if hasGap(repoView.Gaps, code) {
			t.Fatalf("optional annotation rendered as natural collection failure %q: %+v", code, repoView.Gaps)
		}
	}
	if repoView.Counts["explicit_declarations"] != 0 || repoView.Counts["implementation_claims"] != 0 || repoView.Counts["process_witnesses"] != 0 {
		t.Fatalf("absent optional evidence counts are not zero: %+v", repoView.Counts)
	}
}

func TestNestedCheckoutTouchIsReportedAmbiguous(t *testing.T) {
	repo := testRepo(t)
	nested := filepath.Join(repo, "nested")
	if err := os.MkdirAll(nested, 0700); err != nil {
		t.Fatal(err)
	}
	repo, _ = filepath.EvalSymlinks(repo)
	nested, _ = filepath.EvalSymlinks(nested)
	ix := openIndex(t)
	for i, root := range []string{repo, nested} {
		r := &store.ChangeRecord{SessionID: "ambiguous", RepositoryID: fmt.Sprintf("r%d", i), CheckoutID: fmt.Sprintf("c%d", i), RepositoryIdentityKind: "local-sha256", CheckoutRoot: root, Kind: "declaration", EvidenceClass: "claimed", SourceKind: "file", SourceRef: "plan", SourceDisplay: "plan", SourceDigest: "sha256-v1:x", RecordedAt: int64(i + 1), IntentLabel: "x"}
		if err := ix.AppendChange(r); err != nil {
			t.Fatal(err)
		}
	}
	appendFileEvent(t, ix, "ambiguous", filepath.Join(nested, "x.go"), "live", 3)
	for offset := 0; offset < 2; offset++ {
		v, err := Build(ix, "ambiguous", ViewOptions{RepositoryOffset: offset, RepositoryLimit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if v.Repositories[0].Counts["touched_total"] != 0 || !hasGap(v.Repositories[0].Gaps, "ambiguous_touches") {
			t.Fatalf("ambiguous touch joined to checkout: %+v", v.Repositories[0])
		}
	}
}

func hasGap(gaps []Gap, code string) bool {
	for _, gap := range gaps {
		if gap.Code == code {
			return true
		}
	}
	return false
}

func TestPathSymlinkParentAndDotComponentsAreRefused(t *testing.T) {
	repo := testRepo(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(repo, "escape")); err != nil {
		t.Fatal(err)
	}
	ix := openIndex(t)
	source := filepath.Join(repo, "source.md")
	if err := os.WriteFile(source, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"escape/new.go", "a/../base.txt", "a//b.go"} {
		if _, err := RecordClaim(ix, "declaration", ClaimInput{SessionID: "paths", RepoDir: repo, SourcePath: source, Intent: "x", Paths: []string{path}}); err == nil {
			t.Fatalf("unsafe path accepted: %s", path)
		}
	}
}

func TestRemoteNormalizationAndMultipleOrigins(t *testing.T) {
	if got, want := normalizeRemote("SSH://User@Example.COM/team/repo.git/"), "ssh://example.com/team/repo.git"; got != want {
		t.Fatalf("remote normalization=%q want %q", got, want)
	}
	if got, want := normalizeRemote("git@Example.COM:team/repo.git"), "scp://example.com/team/repo.git"; got != want {
		t.Fatalf("scp normalization=%q want %q", got, want)
	}
	repo := testRepo(t)
	run := func(args ...string) {
		c := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if b, e := c.CombinedOutput(); e != nil {
			t.Fatalf("git: %v %s", e, b)
		}
	}
	run("config", "--add", "remote.origin.url", "https://example.com/one")
	run("config", "--add", "remote.origin.url", "https://example.com/two")
	r, err := ResolveRepository(repo)
	if err != nil {
		t.Fatal(err)
	}
	if r.IdentityKind != "local-sha256" {
		t.Fatalf("multiple origins produced portable identity: %+v", r)
	}
}

func TestWorktreesShareRemoteRepositoryButNotCheckoutIdentity(t *testing.T) {
	repo := testRepo(t)
	run := func(args ...string) {
		c := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if b, e := c.CombinedOutput(); e != nil {
			t.Fatalf("git: %v %s", e, b)
		}
	}
	run("config", "remote.origin.url", "git@Example.COM:team/repo.git")
	worktree := filepath.Join(t.TempDir(), "worktree")
	run("worktree", "add", "-q", "-b", "fixture-worktree", worktree)
	a, err := ResolveRepository(repo)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ResolveRepository(worktree)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID || a.CheckoutID == b.CheckoutID || a.IdentityKind != "remote-sha256" {
		t.Fatalf("worktree identities wrong: primary=%+v secondary=%+v", a, b)
	}
}

func TestVerifyRunTimeoutKillsProcessGroupAndRecordsResult(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process-group contract")
	}
	repo := testRepo(t)
	ix := openIndex(t)
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	command := []string{"/bin/sh", "-c", `sleep 30 & child=$!; printf '%s' "$child" > "$1"; wait`, "fixture", pidFile}
	r, code, err := VerifyRun(ix, VerifyInput{SessionID: "timeout", RepoDir: repo, CWD: repo, Name: "claimed timeout", Boundary: "focused", Timeout: 150 * time.Millisecond, Command: command})
	if err != nil {
		t.Fatal(err)
	}
	if code != 124 || r.Termination != "timeout" || r.VerificationResult != "fail" || r.ChildCleanup != "process-group" {
		t.Fatalf("bad timeout record: code=%d %+v", code, r)
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		err = processAlive(pid)
		if processGone(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child process %d survived process-group timeout: %v", pid, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestFitResponseMakesPagingExplicit(t *testing.T) {
	rows := make([]FileRow, 200)
	for i := range rows {
		rows[i] = FileRow{Path: strings.Repeat("x", 2000) + strconv.Itoa(i)}
	}
	v := SessionView{Available: true, RepositoryCount: 1, Repositories: []RepositoryView{{Files: rows, FilePage: page(len(rows), 0, len(rows))}}}
	if err := FitResponse(&v, 32<<10); err != nil {
		t.Fatal(err)
	}
	p := v.Repositories[0].FilePage
	if p.Returned >= p.Total || p.NextOffset == nil || p.Exact {
		t.Fatalf("reduction was silent: %+v", p)
	}
}

func TestFitResponseCandidatePageAlwaysMakesForwardProgress(t *testing.T) {
	row := codemap.ResponsibilityCandidate{RightPath: strings.Repeat("candidate", 1000)}
	one := SessionView{Available: true, RepositoryCount: 1, Repositories: []RepositoryView{{Downstream: ImpactView{Candidates: []codemap.ResponsibilityCandidate{row}, CandidatePage: page(2, 0, 2)}}}}
	adjustPage(&one.Repositories[0].Downstream.CandidatePage, 1)
	oneBody, err := json.Marshal(one)
	if err != nil {
		t.Fatal(err)
	}
	v := SessionView{Available: true, RepositoryCount: 1, Repositories: []RepositoryView{{Downstream: ImpactView{Candidates: []codemap.ResponsibilityCandidate{row, row}, CandidatePage: page(2, 0, 2)}}}}
	if err := FitResponse(&v, len(oneBody)); err != nil {
		t.Fatal(err)
	}
	p := v.Repositories[0].Downstream.CandidatePage
	if p.Returned != 1 || p.NextOffset == nil || *p.NextOffset != 1 || p.Exact {
		t.Fatalf("candidate reduction did not progress: %+v", p)
	}
	if err := FitResponse(&one, 1); !errors.Is(err, ErrResponseMetadataOverBudget) {
		t.Fatalf("one-row over-budget response error=%v", err)
	}
}

func TestAdjustPageDoesNotCallTailSliceExact(t *testing.T) {
	p := page(3, 1, 2)
	adjustPage(&p, 2)
	if p.Exact || p.NextOffset != nil {
		t.Fatalf("nonzero-offset tail must remain an inexact population slice: %+v", p)
	}
}

func TestPagingCanonicalizesOutOfRangeOffsets(t *testing.T) {
	values := []string{"a", "b", "c"}
	got, negative := slice(values, -4, 2)
	if !reflect.DeepEqual(got, []string{"a", "b"}) || negative.Offset != 0 || negative.Returned != 2 || negative.Exact || negative.NextOffset == nil || *negative.NextOffset != 2 {
		t.Fatalf("negative offset was not canonicalized: got=%v page=%+v", got, negative)
	}
	got, beyond := slice(values, 99, 2)
	if len(got) != 0 || beyond.Offset != len(values) || beyond.Returned != 0 || beyond.NextOffset != nil {
		t.Fatalf("past-end offset was not canonicalized: got=%v page=%+v", got, beyond)
	}
}

func TestHistoryCapCarriesAnExplicitPartialReason(t *testing.T) {
	ix := openIndex(t)
	root := t.TempDir()
	for i := 0; i < 1001; i++ {
		r := &store.ChangeRecord{SessionID: "bounded-history", RepositoryID: "r", CheckoutID: "c", RepositoryIdentityKind: "local-sha256", CheckoutRoot: root, Kind: "implementation", EvidenceClass: "claimed", SourceKind: "file", SourceRef: "claim.md", SourceDisplay: "claim.md", SourceDigest: "sha256-v1:x", RecordedAt: int64(i + 1)}
		if err := ix.AppendChange(r); err != nil {
			t.Fatal(err)
		}
	}
	v, err := Build(ix, "bounded-history", ViewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !v.Partial || !strings.Contains(v.Reason, "newest 1000 of 1001") || v.History.Total != 1001 || v.History.Returned != 1000 {
		t.Fatalf("history cap was not explicit: partial=%v reason=%q history=%+v", v.Partial, v.Reason, v.History)
	}
}

func TestGitSnapshotCoversLayersAndExcludesIgnoredFiles(t *testing.T) {
	repo := testRepo(t)
	run := func(args ...string) string {
		c := exec.Command("git", append([]string{"-C", repo}, args...)...)
		b, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, b)
		}
		return strings.TrimSpace(string(b))
	}
	for name, body := range map[string]string{"committed.txt": "one\n", "delete.txt": "delete\n", "rename-old.txt": "rename\n", "worktree.txt": "one\n"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	run("add", ".")
	run("-c", "user.name=CrossingGuard", "-c", "user.email=crossing-guard@example.invalid", "commit", "-q", "-m", "fixture base")
	base := run("rev-parse", "HEAD")
	run("sparse-checkout", "init", "--cone")
	if err := os.WriteFile(filepath.Join(repo, "committed.txt"), []byte("two\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run("add", "committed.txt")
	run("-c", "user.name=CrossingGuard", "-c", "user.email=crossing-guard@example.invalid", "commit", "-q", "-m", "committed delta")
	if err := os.WriteFile(filepath.Join(repo, "worktree.txt"), []byte("changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repo, "delete.txt")); err != nil {
		t.Fatal(err)
	}
	run("mv", "rename-old.txt", "rename-new.txt")
	if err := os.WriteFile(filepath.Join(repo, "staged.txt"), []byte("staged\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run("add", "staged.txt")
	if err := os.WriteFile(filepath.Join(repo, "intent.txt"), []byte("intent\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run("add", "-N", "intent.txt")
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("u\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("ignored.txt\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "ignored.txt"), []byte("secret fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ix := openIndex(t)
	r, err := RecordSnapshot(ix, SnapshotInput{SessionID: "layers", RepoDir: repo, Base: base})
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]map[string]bool{}
	for _, item := range r.Items {
		if found[item.Path] == nil {
			found[item.Path] = map[string]bool{}
		}
		found[item.Path][item.Layer] = true
	}
	for path, layer := range map[string]string{"committed.txt": "committed", "worktree.txt": "worktree", "delete.txt": "worktree", "rename-new.txt": "index", "staged.txt": "index", "intent.txt": "worktree", "untracked.txt": "untracked"} {
		if !found[path][layer] {
			t.Errorf("missing %s layer for %s: %+v", layer, path, found[path])
		}
	}
	if _, ok := found["ignored.txt"]; ok || r.IgnoredIncluded {
		t.Fatal("ignored file was represented as observed change")
	}
	if !r.SparseCheckout {
		t.Fatal("sparse checkout state was not recorded")
	}
	if !strings.HasPrefix(r.SnapshotDigest, "git-tree-v2-sha256:") || r.SourceDigest != r.SnapshotDigest {
		t.Fatalf("revision does not carry self-describing v2 source identity: %+v", r)
	}
	repository, err := ResolveRepository(repo)
	if err != nil {
		t.Fatal(err)
	}
	capture, err := CaptureGit(context.Background(), repository, base)
	if err != nil {
		t.Fatal(err)
	}
	manifest := map[string]GitManifestEntry{}
	for _, entry := range capture.Manifest {
		manifest[entry.Path] = entry
	}
	if _, ok := manifest["ignored.txt"]; ok {
		t.Fatal("ignored path leaked into analysis manifest")
	}
	if entry := manifest["untracked.txt"]; entry.Kind != "regular" || entry.Tracked || !entry.Present {
		t.Fatalf("untracked manifest identity missing: %+v", entry)
	}
}

func TestGitSnapshotRetriesThenRefusesConcurrentMutation(t *testing.T) {
	repo := testRepo(t)
	file := filepath.Join(repo, "base.txt")
	ix := openIndex(t)
	mutations := 0
	snapshotTestHook = func(attempt int) {
		if attempt == 1 {
			mutations++
			_ = os.WriteFile(file, []byte(fmt.Sprintf("mutation %d\n", mutations)), 0600)
		}
	}
	t.Cleanup(func() { snapshotTestHook = nil })
	r, err := RecordSnapshot(ix, SnapshotInput{SessionID: "retry", RepoDir: repo, Base: "HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	if r.CaptureAttempts != 2 {
		t.Fatalf("capture did not report retry: %+v", r)
	}
	snapshotTestHook = func(attempt int) {
		mutations++
		_ = os.WriteFile(file, []byte(fmt.Sprintf("always %d\n", mutations)), 0600)
	}
	if _, err := RecordSnapshot(ix, SnapshotInput{SessionID: "refuse", RepoDir: repo, Base: "HEAD"}); err == nil || !strings.Contains(err.Error(), "changed during capture") {
		t.Fatalf("concurrent mutation accepted: %v", err)
	}
	_, total, err := ix.ChangeRecordsForSession("refuse", 10)
	if err != nil || total != 0 {
		t.Fatalf("refused snapshot wrote evidence: total=%d err=%v", total, err)
	}
}

func TestGitTreeV2DetectsSameStatusUntrackedContentMutation(t *testing.T) {
	repo := testRepo(t)
	untracked := filepath.Join(repo, "untracked.txt")
	if err := os.WriteFile(untracked, []byte("one\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ix := openIndex(t)
	snapshotTestHook = func(attempt int) {
		if attempt == 1 {
			_ = os.WriteFile(untracked, []byte("two\n"), 0600)
		}
	}
	t.Cleanup(func() { snapshotTestHook = nil })
	r, err := RecordSnapshot(ix, SnapshotInput{SessionID: "untracked-mutation", RepoDir: repo, Base: "HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	if r.CaptureAttempts != 2 {
		t.Fatalf("same-status untracked mutation did not invalidate first observation: %+v", r)
	}
}

func TestGitTreeV2DoesNotFollowSymlinkAndRefusesOversizedUntrackedFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink fixture")
	}
	repo := testRepo(t)
	outside := filepath.Join(t.TempDir(), "outside-secret")
	if err := os.WriteFile(outside, []byte("outside content must not be read"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(repo, "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	repository, err := ResolveRepository(repo)
	if err != nil {
		t.Fatal(err)
	}
	first, err := CaptureGit(context.Background(), repository, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	var found GitManifestEntry
	for _, entry := range first.Manifest {
		if entry.Path == "link.txt" {
			found = entry
		}
	}
	if found.Kind != "symlink" || found.Tracked || !found.Present {
		t.Fatalf("symlink was not represented without traversal: %+v", found)
	}
	if err := os.WriteFile(outside, []byte("changed outside bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := CaptureGit(context.Background(), repository, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if first.SnapshotDigest != second.SnapshotDigest {
		t.Fatal("snapshot followed symlink target content")
	}
	large := filepath.Join(repo, "too-large-untracked.bin")
	f, err := os.Create(large)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxGitManifestFileBytes + 1); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := CaptureGit(context.Background(), repository, "HEAD"); err == nil || !strings.Contains(err.Error(), "exceeds 16 MiB") {
		t.Fatalf("oversized untracked file accepted: %v", err)
	}
}

func TestGitTreeV2HonorsCancellation(t *testing.T) {
	repo := testRepo(t)
	repository, err := ResolveRepository(repo)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CaptureGit(ctx, repository, "HEAD"); err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("canceled capture continued: %v", err)
	}
}

func TestSourceMutationAndMissingSourceAreRefused(t *testing.T) {
	repo := testRepo(t)
	source := filepath.Join(repo, "plan.md")
	if err := os.WriteFile(source, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	ix := openIndex(t)
	sourceTestHook = func() { _ = os.WriteFile(source, []byte("after mutation"), 0600) }
	t.Cleanup(func() { sourceTestHook = nil })
	if _, err := RecordClaim(ix, "declaration", ClaimInput{SessionID: "mutated", RepoDir: repo, SourcePath: source, Intent: "x", Paths: []string{"base.txt"}}); err == nil || !strings.Contains(err.Error(), "changed while hashing") {
		t.Fatalf("mutated source accepted: %v", err)
	}
	sourceTestHook = nil
	if _, err := RecordClaim(ix, "declaration", ClaimInput{SessionID: "missing", RepoDir: repo, Intent: "x", Paths: []string{"base.txt"}}); err == nil || !strings.Contains(err.Error(), "requires source-file") {
		t.Fatalf("missing source accepted: %v", err)
	}
	tooLarge := filepath.Join(repo, "too-large.md")
	if err := os.WriteFile(tooLarge, make([]byte, maxSourceBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := RecordClaim(ix, "declaration", ClaimInput{SessionID: "large", RepoDir: repo, SourcePath: tooLarge, Intent: "x", Paths: []string{"base.txt"}}); err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("over-limit source accepted: %v", err)
	}
}

func TestGitInvalidBaseAndNonRepositoryAreRefused(t *testing.T) {
	ix := openIndex(t)
	repo := testRepo(t)
	if _, err := RecordSnapshot(ix, SnapshotInput{SessionID: "bad-base", RepoDir: repo, Base: "definitely-not-a-ref"}); err == nil {
		t.Fatal("invalid base accepted")
	}
	if _, err := RecordSnapshot(ix, SnapshotInput{SessionID: "not-repo", RepoDir: t.TempDir(), Base: "HEAD"}); err == nil {
		t.Fatal("non-repository accepted")
	}
}

func TestGitNonUTFPathAndUnmergedProtocolAreExplicit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("raw non-UTF filename fixture")
	}
	repo := testRepo(t)
	badName := string([]byte{'b', 'a', 'd', '-', 0xff})
	if err := os.WriteFile(filepath.Join(repo, badName), []byte("x"), 0600); err != nil {
		t.Skipf("filesystem rejected non-UTF fixture: %v", err)
	}
	ix := openIndex(t)
	if _, err := RecordSnapshot(ix, SnapshotInput{SessionID: "non-utf", RepoDir: repo, Base: "HEAD"}); err == nil || !strings.Contains(err.Error(), "non-UTF-8") {
		t.Fatalf("non-UTF path was not explicitly refused: %v", err)
	}
	unmerged := []byte("u UU N... 100644 100644 100644 100644 a b c conflict.txt\x00")
	items, err := parseStatus(unmerged)
	if err != nil || len(items) != 1 || items[0].Path != "conflict.txt" || items[0].Status != "U" {
		t.Fatalf("unmerged protocol: %+v err=%v", items, err)
	}
	raw := []byte(":100644 100644 a b R100\x00old.go\x00new.go\x00")
	items, err = parseRawDiff(raw, "committed")
	if err != nil || len(items) != 1 || items[0].OldPath != "old.go" || items[0].Path != "new.go" {
		t.Fatalf("rename protocol: %+v err=%v", items, err)
	}
}

func TestCappedGitOutputNeverBuffersPastCeiling(t *testing.T) {
	w := &cappedOutput{max: 32}
	p := []byte(strings.Repeat("x", 1000))
	if n, err := w.Write(p); err != nil || n != len(p) {
		t.Fatalf("writer contract: n=%d err=%v", n, err)
	}
	if !w.exceeded || w.buf.Len() != 33 {
		t.Fatalf("cap not enforced: len=%d exceeded=%v", w.buf.Len(), w.exceeded)
	}
}

func TestGitSnapshotRefusesOverBudgetDiff(t *testing.T) {
	repo := testRepo(t)
	large := filepath.Join(repo, "large.txt")
	if err := os.WriteFile(large, []byte("small\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c := exec.Command("git", "-C", repo, "add", "large.txt")
	if b, e := c.CombinedOutput(); e != nil {
		t.Fatalf("git add: %v %s", e, b)
	}
	c = exec.Command("git", "-C", repo, "-c", "user.name=CrossingGuard", "-c", "user.email=crossing-guard@example.invalid", "commit", "-q", "-m", "large base")
	if b, e := c.CombinedOutput(); e != nil {
		t.Fatalf("git commit: %v %s", e, b)
	}
	if err := os.WriteFile(large, []byte(strings.Repeat("changed line\n", 1500000)), 0600); err != nil {
		t.Fatal(err)
	}
	ix := openIndex(t)
	if _, err := RecordSnapshot(ix, SnapshotInput{SessionID: "large-diff", RepoDir: repo, Base: "HEAD"}); err == nil || !strings.Contains(err.Error(), "exceeds 16 MiB") {
		t.Fatalf("over-budget Git output accepted: %v", err)
	}
	_, total, err := ix.ChangeRecordsForSession("large-diff", 10)
	if err != nil || total != 0 {
		t.Fatalf("over-budget snapshot wrote a record: total=%d err=%v", total, err)
	}
}

func TestVerifyRunFailureModesAndPrivacy(t *testing.T) {
	repo := testRepo(t)
	ix := openIndex(t)
	marker := filepath.Join(t.TempDir(), "must-not-run")
	if _, code, err := VerifyRun(ix, VerifyInput{RepoDir: repo, CWD: repo, Name: "invalid", Boundary: "focused", Timeout: time.Second, Command: []string{"/usr/bin/touch", marker}}); err == nil || code != 2 {
		t.Fatalf("missing session accepted: code=%d err=%v", code, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("invalid witness executed command before validation")
	}
	if _, code, err := VerifyRun(ix, VerifyInput{SessionID: "outside", RepoDir: repo, CWD: t.TempDir(), Name: "outside", Boundary: "focused", Timeout: time.Second, Command: []string{"/usr/bin/true"}}); err == nil || code != 2 {
		t.Fatalf("outside cwd accepted: code=%d err=%v", code, err)
	}
	r, code, err := VerifyRun(ix, VerifyInput{SessionID: "missing-exe", RepoDir: repo, CWD: repo, Name: "missing", Boundary: "focused", Timeout: time.Second, Command: []string{"crossing-guard-command-that-does-not-exist"}})
	if err != nil || code != 127 || r.VerificationResult != "unavailable" || r.Termination != "start-failed" {
		t.Fatalf("start failure not witnessed: code=%d record=%+v err=%v", code, r, err)
	}
	secretArg := "sensitive-argv-fixture"
	r, code, err = VerifyRun(ix, VerifyInput{SessionID: "private-argv", RepoDir: repo, CWD: repo, Name: "argument privacy", Boundary: "focused", Timeout: time.Second, Command: []string{"/usr/bin/true", secretArg}})
	if err != nil || code != 0 || r.VerificationResult != "pass" {
		t.Fatalf("pass witness: code=%d record=%+v err=%v", code, r, err)
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), secretArg) {
		t.Fatal("raw argv was persisted")
	}
	if r.ExecutablePath == "" || !strings.HasPrefix(r.ExecutableDigest, "sha256-v1:") || r.ArgvDigest == "" {
		t.Fatalf("process identity incomplete: %+v", r)
	}
	ix.Close()
	if _, code, err := VerifyRun(ix, VerifyInput{SessionID: "write-fail", RepoDir: repo, CWD: repo, Name: "write failure", Boundary: "focused", Timeout: time.Second, Command: []string{"/usr/bin/true"}}); err == nil || code != 2 || !strings.Contains(err.Error(), "NOT RECORDED") {
		t.Fatalf("store failure hid witnessed-but-unrecorded state: code=%d err=%v", code, err)
	}
}

func TestVerifyRunInterruptCleansProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX interrupt contract")
	}
	repo := testRepo(t)
	ix := openIndex(t)
	go func() {
		time.Sleep(200 * time.Millisecond)
		p, _ := os.FindProcess(os.Getpid())
		_ = p.Signal(os.Interrupt)
	}()
	r, code, err := VerifyRun(ix, VerifyInput{SessionID: "interrupt", RepoDir: repo, CWD: repo, Name: "interrupt", Boundary: "focused", Timeout: 5 * time.Second, Command: []string{"/bin/sh", "-c", "sleep 30"}})
	if err != nil || code != 130 || r.Termination != "signal" || r.Signal != "interrupt" {
		t.Fatalf("interrupt result: code=%d record=%+v err=%v", code, r, err)
	}
}

func TestEveryPTDeltaCCombinationRemainsIndependent(t *testing.T) {
	repo := testRepo(t)
	canonical, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	ix := openIndex(t)
	itemsFor := func(bit int, revision bool) []store.ChangeItem {
		var out []store.ChangeItem
		for mask := 0; mask < 16; mask++ {
			if mask&bit == 0 {
				continue
			}
			item := store.ChangeItem{Path: fmt.Sprintf("combo-%02d.go", mask)}
			if revision {
				item.Layer = "worktree"
				item.Status = "M"
			}
			out = append(out, item)
		}
		return out
	}
	decl := &store.ChangeRecord{SessionID: "combos", RepositoryID: "r", CheckoutID: "c", RepositoryIdentityKind: "local-sha256", CheckoutRoot: canonical, Kind: "declaration", EvidenceClass: "claimed", SourceKind: "file", SourceRef: "plan", SourceDisplay: "plan", SourceDigest: "sha256-v1:p", RecordedAt: 1, IntentLabel: "all combinations", Items: itemsFor(1, false)}
	rev := &store.ChangeRecord{SessionID: "combos", RepositoryID: "r", CheckoutID: "c", RepositoryIdentityKind: "local-sha256", CheckoutRoot: canonical, Kind: "revision", EvidenceClass: "observed", SourceKind: "git", SourceRef: "git:head", SourceDisplay: "Git snapshot", SourceDigest: "sha256-v1:d", RecordedAt: 2, CaptureStartedAt: 1, CaptureEndedAt: 2, BaseRevision: "base", HeadRevision: "head", SnapshotDigest: "sha256-v1:d", Items: itemsFor(4, true)}
	claim := &store.ChangeRecord{SessionID: "combos", RepositoryID: "r", CheckoutID: "c", RepositoryIdentityKind: "local-sha256", CheckoutRoot: canonical, Kind: "implementation", EvidenceClass: "claimed", SourceKind: "file", SourceRef: "claim", SourceDisplay: "claim", SourceDigest: "sha256-v1:c", RecordedAt: 3, Items: itemsFor(8, false)}
	for _, record := range []*store.ChangeRecord{decl, rev, claim} {
		if err := ix.AppendChange(record); err != nil {
			t.Fatal(err)
		}
	}
	for mask := 0; mask < 16; mask++ {
		if mask&2 != 0 {
			appendFileEvent(t, ix, "combos", filepath.Join(canonical, fmt.Sprintf("combo-%02d.go", mask)), "live", int64(10+mask))
		}
	}
	v, err := Build(ix, "combos", ViewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]FileRow{}
	for _, row := range v.Repositories[0].Files {
		rows[row.Path] = row
	}
	for mask := 0; mask < 16; mask++ {
		row, ok := rows[fmt.Sprintf("combo-%02d.go", mask)]
		if mask == 0 {
			if ok {
				t.Fatalf("empty combination unexpectedly produced row: %+v", row)
			}
			continue
		}
		if !ok || (row.Planned != nil) != (mask&1 != 0) || (row.Touched != nil) != (mask&2 != 0) || (row.Changed != nil) != (mask&4 != 0) || (row.Claimed != nil) != (mask&8 != 0) {
			t.Fatalf("combination %04b collapsed: %+v", mask, row)
		}
	}

	session, err := Build(ix, "combos", ViewOptions{FileFilter: "session", FileSort: "overlap", FileLimit: 2})
	if err != nil {
		t.Fatal(err)
	}
	sr := session.Repositories[0]
	if sr.FileFilter != "session" || sr.FileSort != "overlap" || sr.FilePage.Total != 14 || sr.FilePage.Returned != 2 || len(sr.Files) != 2 || sr.Files[0].Path != "combo-15.go" {
		t.Fatalf("session overlap projection mismatch: %+v", sr)
	}
	for key, want := range map[string]int{"session_linked_known": 14, "planned_changed": 4, "touched_changed_known": 4, "claimed_changed": 4} {
		if got := sr.Counts[key]; got != want {
			t.Fatalf("count %s=%d, want %d", key, got, want)
		}
	}

	outside, err := Build(ix, "combos", ViewOptions{FileFilter: "outside-plan", FileQuery: "COMBO", FileSort: "path"})
	if err != nil {
		t.Fatal(err)
	}
	or := outside.Repositories[0]
	if or.FilePage.Total != 4 || len(or.Files) != 4 || or.Files[0].Path != "combo-04.go" {
		t.Fatalf("outside-plan projection mismatch: %+v", or)
	}
	queried, err := Build(ix, "combos", ViewOptions{FileQuery: "combo-15"})
	if err != nil || queried.Repositories[0].FilePage.Total != 1 {
		t.Fatalf("path query mismatch: view=%+v err=%v", queried, err)
	}
	for _, opt := range []ViewOptions{{FileFilter: "important"}, {FileSort: "smart"}, {FileQuery: strings.Repeat("x", 201)}} {
		if _, err := Build(ix, "combos", opt); !errors.Is(err, ErrInvalidFileView) {
			t.Fatalf("invalid file projection accepted: opt=%+v err=%v", opt, err)
		}
	}
}

func TestNewClaimSupersedesWithoutRewritingSourceHistory(t *testing.T) {
	repo := testRepo(t)
	source := filepath.Join(repo, "plan.md")
	ix := openIndex(t)
	if err := os.WriteFile(source, []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	first, err := RecordClaim(ix, "declaration", ClaimInput{SessionID: "history", RepoDir: repo, SourcePath: source, Intent: "first", Paths: []string{"base.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("second"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := RecordClaim(ix, "declaration", ClaimInput{SessionID: "history", RepoDir: repo, SourcePath: source, Intent: "second", Paths: []string{"base.txt", "new.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	if first.SourceDigest == second.SourceDigest {
		t.Fatal("source mutation did not create new identity")
	}
	recs, total, err := ix.ChangeRecordsForSession("history", 10)
	if err != nil || total != 2 || recs[1].SourceDigest != first.SourceDigest {
		t.Fatalf("prior source was rewritten: %+v total=%d err=%v", recs, total, err)
	}
	v, err := Build(ix, "history", ViewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	selected := v.Repositories[0].Selected
	if selected.DeclarationID != second.ID || selected.DeclarationSuperseded != 1 || selected.DeclarationSourceDigest != second.SourceDigest {
		t.Fatalf("latest/supersession facts wrong: %+v", selected)
	}
}

func TestRepositoriesNeverSubtractAcrossCheckouts(t *testing.T) {
	a := testRepo(t)
	b := testRepo(t)
	ix := openIndex(t)
	for n, repo := range []string{a, b} {
		canonical, _ := filepath.EvalSymlinks(repo)
		id := fmt.Sprintf("r%d", n)
		checkout := fmt.Sprintf("c%d", n)
		planned := fmt.Sprintf("planned-%d.go", n)
		changed := fmt.Sprintf("changed-%d.go", n)
		decl := &store.ChangeRecord{SessionID: "multi", RepositoryID: id, CheckoutID: checkout, RepositoryIdentityKind: "local-sha256", CheckoutRoot: canonical, Kind: "declaration", EvidenceClass: "claimed", SourceKind: "file", SourceRef: "p", SourceDisplay: "p", SourceDigest: "sha256-v1:p", RecordedAt: int64(n*10 + 1), IntentLabel: "x", Items: []store.ChangeItem{{Path: planned}}}
		rev := &store.ChangeRecord{SessionID: "multi", RepositoryID: id, CheckoutID: checkout, RepositoryIdentityKind: "local-sha256", CheckoutRoot: canonical, Kind: "revision", EvidenceClass: "observed", SourceKind: "git", SourceRef: "git:h", SourceDisplay: "Git", SourceDigest: "sha256-v1:d", RecordedAt: int64(n*10 + 2), CaptureStartedAt: 1, CaptureEndedAt: 2, BaseRevision: "b", HeadRevision: "h", SnapshotDigest: "sha256-v1:d", Items: []store.ChangeItem{{Path: changed, Layer: "worktree", Status: "M"}}}
		if err := ix.AppendChange(decl); err != nil {
			t.Fatal(err)
		}
		if err := ix.AppendChange(rev); err != nil {
			t.Fatal(err)
		}
	}
	for offset := 0; offset < 2; offset++ {
		v, err := Build(ix, "multi", ViewOptions{RepositoryOffset: offset, RepositoryLimit: 1})
		if err != nil {
			t.Fatal(err)
		}
		r := v.Repositories[0]
		if r.Counts["planned"] != 1 || r.Counts["changed"] != 1 || r.Counts["added_scope"] != 1 || r.Counts["omitted_scope"] != 1 {
			t.Fatalf("cross-checkout subtraction or merge: %+v", r)
		}
	}
}
