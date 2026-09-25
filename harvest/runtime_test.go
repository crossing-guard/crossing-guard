package harvest

import "testing"

type multiMatchRuntime struct{}

func (multiMatchRuntime) Name() string                        { return "multi-match-fixture" }
func (multiMatchRuntime) CanonicalID(s SessionSummary) string { return s.ThreadID }
func (multiMatchRuntime) MatchID(s SessionSummary, id string) bool {
	return s.ThreadID == id || s.ID == id
}
func (multiMatchRuntime) Collect() []fileJob {
	return []fileJob{{runtime: "multi-match-fixture", path: "/fixture/rollout-a-thread"}, {runtime: "multi-match-fixture", path: "/fixture/rollout-b-thread"}}
}
func (multiMatchRuntime) Summarize(j fileJob) (SessionSummary, *SessionUsage, map[string]*DayBucket, bool) {
	return SessionSummary{Runtime: "multi-match-fixture", ID: stem(j.path), ThreadID: "thread", Cwd: "/work/" + stem(j.path), HasTranscript: true}, nil, nil, true
}
func (multiMatchRuntime) Normalize(string) ([]CanonicalEvent, int, *SessionUsage, error) {
	return nil, 0, nil, nil
}
func (multiMatchRuntime) ThreadTitle(SessionSummary) string { return "" }

// TestCodexCanonicalIDPrefersThreadID pins the 2026-07-18 bug fix: when the
// rollout filename's trailing uuid differs from the session_meta thread id
// (resumed / multi-rollout threads), the canonical id must be the THREAD id —
// that is what the governor index and enrichment DB are keyed under. Using the
// stem suffix (the old enrichKey behavior) silently missed enrichment.
func TestCodexCanonicalIDPrefersThreadID(t *testing.T) {
	s := SessionSummary{
		Runtime:  "codex",
		ID:       "rollout-2026-07-10T19-49-06-019f4f14-1fb4-7962-a402-0b1193f2ab7d",
		ThreadID: "019f4f05-d18b-73f1-96a9-f6fd87e34a97",
	}
	if got := CanonicalID(s); got != s.ThreadID {
		t.Fatalf("codex CanonicalID = %q, want ThreadID %q (the DB key)", got, s.ThreadID)
	}
}

func TestDecorateSummaryProjectsCanonicalResumeID(t *testing.T) {
	s := SessionSummary{Runtime: "codex", ID: "rollout-file", ThreadID: "thread-exact"}
	decorateSummary(&s)
	if s.ResumeID != "thread-exact" {
		t.Fatalf("ResumeID = %q, want runtime-owned canonical identity", s.ResumeID)
	}
}

// TestCodexCanonicalIDFallsBackToStemSuffix: with no session_meta thread id,
// fall back to the trailing uuid of the rollout stem.
func TestCodexCanonicalIDFallsBackToStemSuffix(t *testing.T) {
	stem := "rollout-2026-07-10T19-49-06-019f4f14-1fb4-7962-a402-0b1193f2ab7d"
	if got, want := CanonicalID(SessionSummary{Runtime: "codex", ID: stem}), stem[len(stem)-36:]; got != want {
		t.Fatalf("codex fallback CanonicalID = %q, want %q", got, want)
	}
}

func TestClaudeCanonicalIDIsID(t *testing.T) {
	if got := CanonicalID(SessionSummary{Runtime: "claude", ID: "abc"}); got != "abc" {
		t.Fatalf("claude CanonicalID = %q, want abc", got)
	}
}

func TestMatchID(t *testing.T) {
	// 2026-08-29 identity split: the filename-suffix convenience match now
	// requires a FULL trailing uuid — sibling rollouts share a thread id, and a
	// short fragment could select an arbitrary sibling file.
	ownUUID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	s := SessionSummary{Runtime: "codex", ID: "rollout-x-" + ownUUID, ThreadID: "tid"}
	for _, id := range []string{"tid", ownUUID, "rollout-x-" + ownUUID} {
		if !MatchID(s, id) {
			t.Fatalf("codex MatchID(%q) = false, want true", id)
		}
	}
	for _, id := range []string{"nope", ownUUID[24:]} { // short suffix fragment must not match
		if MatchID(s, id) {
			t.Fatalf("MatchID(%q) matched, want false", id)
		}
	}
}

func TestCouldMatchIDRejectsOnlyImpossibleBuiltInShapes(t *testing.T) {
	uuid := "12345678-1234-1234-1234-123456789abc"
	for _, runtime := range []string{"codex", "claude"} {
		if !CouldMatchID(runtime, uuid) {
			t.Fatalf("%s rejected UUID", runtime)
		}
		if CouldMatchID(runtime, "c6-envelope-fixture") {
			t.Fatalf("%s accepted impossible built-in id shape", runtime)
		}
	}
	if !CouldMatchID("future-runtime", "any-shape") {
		t.Fatal("unknown runtime was not conservative")
	}
	if !CouldMatchID("codex", "rollout-2026-fixture") {
		t.Fatal("codex rejected rollout prefix")
	}
}

func TestFindAllReturnsEveryCollectedResumeMatchAndFindUsesNewestContract(t *testing.T) {
	prior, existed := runtimes["multi-match-fixture"]
	runtimes["multi-match-fixture"] = multiMatchRuntime{}
	t.Cleanup(func() {
		if existed {
			runtimes["multi-match-fixture"] = prior
		} else {
			delete(runtimes, "multi-match-fixture")
		}
	})
	matches := FindAll("multi-match-fixture", "thread")
	if len(matches) != 2 || matches[0].Cwd == matches[1].Cwd {
		t.Fatalf("all-match lookup lost resumed segments: %+v", matches)
	}
	if one, ok := Find("multi-match-fixture", "thread"); !ok || one.ThreadID != "thread" {
		t.Fatalf("Find did not reuse all-match identity: ok=%v summary=%+v", ok, one)
	}
}
