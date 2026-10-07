package harvest

import (
	"net/url"
	"strings"
	"testing"
)

type multiMatchRuntime struct{}

func (multiMatchRuntime) Name() string                        { return "multi-match-fixture" }
func (multiMatchRuntime) CanonicalID(s SessionSummary) string { return s.ThreadID }
func (multiMatchRuntime) MatchID(s SessionSummary, id string) bool {
	return s.ThreadID == id || s.ID == id
}
func (multiMatchRuntime) Collect() []fileJob {
	return []fileJob{{runtime: "multi-match-fixture", path: "/fixture/rollout-a-thread"}, {runtime: "multi-match-fixture", path: "/fixture/rollout-b-thread"}}
}
func (multiMatchRuntime) Summarize(j fileJob) (SessionSummary, bool) {
	return SessionSummary{Runtime: "multi-match-fixture", ID: stem(j.path), ThreadID: "thread", Cwd: "/work/" + stem(j.path), HasTranscript: true}, true
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
		ID:       "rollout-2026-07-10T19-49-06-abcdef00-aaaa-7bbb-8ccc-000000000003",
		ThreadID: "abcdef00-aaaa-7bbb-8ccc-000000000001",
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
	stem := "rollout-2026-07-10T19-49-06-abcdef00-aaaa-7bbb-8ccc-000000000003"
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

// countingRuntime counts its listings and summaries, registered only from
// TestFindListedListsOnceAndSummarizesOnlyCandidates.
type countingRuntime struct {
	multiMatchRuntime
	collects, summaries *int
}

func (countingRuntime) Name() string { return "counting-fixture" }
func (r countingRuntime) Collect() []fileJob {
	*r.collects++
	return []fileJob{{runtime: "counting-fixture", path: "/fixture/exact-id.jsonl"},
		{runtime: "counting-fixture", path: "/fixture/rollout-2026-thread-7"},
		{runtime: "counting-fixture", path: "/fixture/unrelated"}}
}
func (r countingRuntime) Summarize(j fileJob) (SessionSummary, bool) {
	*r.summaries++
	return SessionSummary{Runtime: "counting-fixture", ID: stem(j.path), ThreadID: strings.TrimPrefix(stem(j.path), "rollout-2026-")}, true
}

// Many ids cost one listing, and only files whose names carry an id are
// read; an id with no file is absent rather than a full scan (the Usage
// pane's members, session usage breakdown code red-team C-15).
func TestFindListedListsOnceAndSummarizesOnlyCandidates(t *testing.T) {
	collects, summaries := 0, 0
	runtimes["counting-fixture"] = countingRuntime{collects: &collects, summaries: &summaries}
	t.Cleanup(func() { delete(runtimes, "counting-fixture") })
	found := FindListed("counting-fixture", []string{"exact-id", "thread-7", "gone"})
	if collects != 1 || summaries != 2 {
		t.Fatalf("listings %d, summaries %d; want 1 and 2", collects, summaries)
	}
	if found["exact-id"].ID != "exact-id" || found["thread-7"].ID != "rollout-2026-thread-7" {
		t.Fatalf("found %+v", found)
	}
	if _, ok := found["gone"]; ok || len(found) != 2 {
		t.Fatalf("an id with no file must be absent: %+v", found)
	}
	if len(FindListed("counting-fixture", nil)) != 0 || collects != 1 {
		t.Fatal("no ids must not list")
	}
}

// TestNativeOpenLinks: each adapter that ships a desktop app composes the link
// from a shape-checked id; a subagent or an id the app would refuse has none.
func TestNativeOpenLinks(t *testing.T) {
	const id = "abcdef00-aaaa-4bbb-8ccc-000000000013"
	for _, tc := range []struct {
		name    string
		session SessionSummary
		url     string
		app     string
	}{
		{"claude session", SessionSummary{Runtime: "claude", ID: id, Path: "/p/proj/" + id + ".jsonl"},
			"claude://resume?session=" + id, "Claude"},
		{"claude subagent sidecar", SessionSummary{Runtime: "claude", ID: id,
			Path: "/p/proj/parent/" + claudeSubagentsDirName + "/agent-" + id + ".jsonl"}, "", ""},
		{"claude non-uuid id", SessionSummary{Runtime: "claude", ID: "agent-a1b2", Path: "/p/proj/agent-a1b2.jsonl"}, "", ""},
		{"codex thread", SessionSummary{Runtime: "codex", ID: "rollout-x", ThreadID: id}, "codex://threads/" + id, "Codex"},
		{"codex stem suffix", SessionSummary{Runtime: "codex", ID: "rollout-2026-10-03T13-28-32-" + id},
			"codex://threads/" + id, "Codex"},
		{"codex subagent opens nothing", SessionSummary{Runtime: "codex", ID: "rollout-child", ThreadID: id,
			MetaID: "0199aaaa-0000-7000-8000-000000000001", LineageKind: "native-subagent"}, "", ""},
		{"codex uppercase thread", SessionSummary{Runtime: "codex", ID: "rollout-x", ThreadID: strings.ToUpper(id)}, "", ""},
		{"codex non-uuid thread", SessionSummary{Runtime: "codex", ID: "r", ThreadID: "thread-exact"}, "", ""},
		{"opencode", SessionSummary{Runtime: "opencode", ID: "ses_abc"}, "", ""},
		{"unknown runtime", SessionSummary{Runtime: "nope", ID: id}, "", ""},
	} {
		s := tc.session
		s.NativeOpen = NativeOpenLink{URL: "stale://x", App: "stale"}
		decorateSummary(&s)
		if s.NativeOpen.URL != tc.url || s.NativeOpen.App != tc.app {
			t.Errorf("%s: NativeOpen = %+v, want url %q app %q", tc.name, s.NativeOpen, tc.url, tc.app)
		}
	}
}

// TestNativeOpenLinksNeverLeaveTheMachine: the console hands this URL to the
// operating system, so no registered adapter may publish a web, file or script
// scheme, and every adapter with the capability must produce a parseable URL.
func TestNativeOpenLinksNeverLeaveTheMachine(t *testing.T) {
	const id = "abcdef00-aaaa-4bbb-8ccc-000000000013"
	for _, name := range RuntimeNames() {
		opener, ok := runtimeFor(name).(NativeOpener)
		if !ok {
			continue
		}
		link, ok := opener.NativeOpen(SessionSummary{Runtime: name, ID: id, ThreadID: id, Path: "/p/proj/" + id + ".jsonl"})
		if !ok {
			t.Fatalf("%s: no link for a plain top-level session", name)
		}
		parsed, err := url.Parse(link.URL)
		if err != nil || parsed.Scheme == "" || link.App == "" {
			t.Fatalf("%s: link %+v does not parse to a scheme and an app name (%v)", name, link, err)
		}
		switch parsed.Scheme {
		case "http", "https", "file", "javascript", "data":
			t.Fatalf("%s: link scheme %q is not a desktop-app scheme", name, parsed.Scheme)
		}
	}
}
