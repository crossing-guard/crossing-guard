package daemon

import "testing"

func TestSearchMetadataPreservesNewestCatalogAndResumeIdentity(t *testing.T) {
	newest := SessionSummary{
		Runtime: "codex", ID: "rollout-new", ResumeID: "thread-shared", ThreadID: "thread-shared",
		Title: "newest", Project: "/work/newest",
	}
	older := SessionSummary{
		Runtime: "codex", ID: "rollout-old", ResumeID: "thread-shared", ThreadID: "thread-shared",
		Title: "older", Project: "/work/older",
	}
	metadata := searchMetadataByIdentity([]SessionSummary{newest, older})

	if got := metadata["codex/thread-shared"]; got.ID != newest.ID {
		t.Fatalf("shared native identity selected catalog %q, want newest %q", got.ID, newest.ID)
	}
	hit := enrichSearchHitIdentity(SearchHit{Runtime: "codex", ID: "thread-shared"}, metadata["codex/thread-shared"])
	if hit.ID != newest.ID || hit.ResumeID != newest.ResumeID || hit.Title != newest.Title || hit.Project != newest.Project {
		t.Fatalf("enriched search identity = %+v, want newest catalog/resume metadata", hit)
	}
	if got := metadata["codex/rollout-old"]; got.ID != older.ID {
		t.Fatalf("distinct older catalog identity was lost: %+v", got)
	}
}
