package changeenv

import (
	"path/filepath"
	"testing"

	"crossing-guard/store"
)

// pruneForTest prunes one generation with a horizon no session and no generation
// in these small-clock fixtures can meet, so only "newest of its checkout" keeps one.
func pruneForTest(t *testing.T, ix *store.Index, generationID int64) {
	t.Helper()
	pruned, err := ix.PruneUnderstandingFacts(generationID, 1<<40, 1<<62)
	if err != nil || !pruned {
		t.Fatalf("prune generation %d: pruned=%v err=%v", generationID, pruned, err)
	}
}

func TestBuildCodeChangesNamesRetentionForAPrunedBaseline(t *testing.T) {
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	appendCodeCheckpoint(t, ix, "session", "attachment", "baseline", "git-tree-v2-sha256:baseline", 1, 2)
	appendCodeCheckpoint(t, ix, "session", "settled", "current", "git-tree-v2-sha256:current", 3, 4)
	baseline := appendCodeGeneration(t, ix, "git-tree-v2-sha256:baseline", "baseline", false)
	appendCodeGeneration(t, ix, "git-tree-v2-sha256:current", "current", true)
	pruneForTest(t, ix, baseline.ID)

	changes, err := BuildCodeChanges(ix, "session", CodeChangeOptions{AnalyzerBundleDigest: "bundle"})
	if err != nil {
		t.Fatal(err)
	}
	if changes.State != "baseline_unavailable" || changes.Reason != factsPrunedReason ||
		changes.Aggregates.DeclarationChanges != 0 {
		t.Fatalf("pruned baseline rendered as a comparison: state=%q reason=%q aggregates=%+v", changes.State, changes.Reason, changes.Aggregates)
	}
	if changes.Current == nil || changes.Current.FactsState != store.UnderstandingFactsPresent {
		t.Fatalf("current boundary lost its facts state: %+v", changes.Current)
	}
	statuses, err := CodeChangeStatuses(ix, "session", "bundle")
	if err != nil || len(statuses) != 1 || statuses[0].State != "baseline_unavailable" || statuses[0].Reason != factsPrunedReason {
		t.Fatalf("status summary: %+v err=%v", statuses, err)
	}
}

func TestBuildCodeChangesNamesRetentionForAPrunedCurrentAndIntermediate(t *testing.T) {
	ix, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	appendCodeCheckpoint(t, ix, "session", "attachment", "baseline", "git-tree-v2-sha256:baseline", 1, 2)
	middle := appendCodeCheckpoint(t, ix, "session", "settled", "middle", "git-tree-v2-sha256:middle", 3, 4)
	appendCodeCheckpoint(t, ix, "session", "settled", "current", "git-tree-v2-sha256:current", 5, 6)
	appendCodeGeneration(t, ix, "git-tree-v2-sha256:baseline", "baseline", false)
	middleGeneration := appendCodeGeneration(t, ix, "git-tree-v2-sha256:middle", "middle", false)
	current := appendCodeGeneration(t, ix, "git-tree-v2-sha256:current", "current", true)

	// An intermediate checkpoint named by id answers with the retention reason.
	pruneForTest(t, ix, middleGeneration.ID)
	named, err := BuildCodeChanges(ix, "session", CodeChangeOptions{AnalyzerBundleDigest: "bundle", CheckpointID: middle.ID})
	if err != nil {
		t.Fatal(err)
	}
	if named.State != "unavailable" || named.Reason != factsPrunedReason || len(named.Files) != 0 {
		t.Fatalf("pruned intermediate: state=%q reason=%q files=%d", named.State, named.Reason, len(named.Files))
	}
	// The session's own current view is untouched by that.
	if whole, err := BuildCodeChanges(ix, "session", CodeChangeOptions{AnalyzerBundleDigest: "bundle"}); err != nil || whole.State != "exact" {
		t.Fatalf("session view state=%q err=%v", whole.State, err)
	}
	// A later scan of the checkout makes the session's current generation prunable.
	appendCodeGeneration(t, ix, "git-tree-v2-sha256:later", "later", false)
	pruneForTest(t, ix, current.ID)
	gone, err := BuildCodeChanges(ix, "session", CodeChangeOptions{AnalyzerBundleDigest: "bundle"})
	if err != nil {
		t.Fatal(err)
	}
	if gone.State != "unavailable" || gone.Reason != factsPrunedReason || len(gone.Files) != 0 {
		t.Fatalf("pruned current: state=%q reason=%q files=%d", gone.State, gone.Reason, len(gone.Files))
	}
}

func TestImpactNamesRetentionForAPrunedGeneration(t *testing.T) {
	index := impactIndex(t)
	digest := "git-tree-v2-sha256:exact"
	revision := appendImpactRevision(t, index, "session", digest, "changed.go")
	generation := appendImpactGeneration(t, index, digest, "bundle", "none", "complete")
	appendImpactGeneration(t, index, "git-tree-v2-sha256:later", "bundle", "none", "complete")
	pruneForTest(t, index, generation.ID)
	view, err := buildImpact(index, "repo", "checkout", revision, ViewOptions{AnalyzerBundleDigest: "bundle", ImpactLimit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if view.State != "unavailable" || view.Reason != factsPrunedReason || view.Generation != nil || len(view.Edges) != 0 {
		t.Fatalf("pruned generation rendered as measured: %+v", view)
	}
}
