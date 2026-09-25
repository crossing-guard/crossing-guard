package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func openOwnerTagTestIndex(t *testing.T) *Index {
	t.Helper()
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	return ix
}

func ownerTarget(runtime, id string) SessionOwnerTarget {
	return SessionOwnerTarget{Runtime: runtime, SessionID: id, Title: "Title of " + id,
		Repository: "/work/alpha", Cwd: "/work/alpha", TouchedAt: 100}
}

func activeOwnerTags(t *testing.T, ix *Index) []SessionOwnerTag {
	t.Helper()
	tags, truncated, err := ix.AllActiveSessionOwnerTags(1000)
	if err != nil || truncated {
		t.Fatalf("read tags: truncated=%v err=%v", truncated, err)
	}
	return tags
}

// A fresh store holds no tags, no notes and no suggestions: the framework
// supplies the mechanism and nothing to put in it.
func TestFreshStoreHasNoOwnerTagsNotesOrVocabulary(t *testing.T) {
	ix := openOwnerTagTestIndex(t)
	if tags := activeOwnerTags(t, ix); len(tags) != 0 {
		t.Fatalf("fresh store has tags: %+v", tags)
	}
	vocabulary, err := ix.SessionOwnerTagVocabulary(100)
	if err != nil || len(vocabulary) != 0 {
		t.Fatalf("fresh store has vocabulary: %+v %v", vocabulary, err)
	}
	notes, _, err := ix.AllSessionOwnerNotes(100)
	if err != nil || len(notes) != 0 {
		t.Fatalf("fresh store has notes: %+v %v", notes, err)
	}
}

func TestOwnerTagApplyIsIdempotentCasePreservingAndCaseBlind(t *testing.T) {
	ix := openOwnerTagTestIndex(t)
	session := ownerTarget("claude", "s1")
	first := SessionOwnerTagValue{Key: "Topic", Value: "Amazon-Routing"}
	if err := ix.ApplySessionOwnerTags([]SessionOwnerTarget{session}, []SessionOwnerTagValue{first}, 10); err != nil {
		t.Fatal(err)
	}
	// The same tag in another case, on the same and on another session, is the
	// same tag, spelled as first written; re-applying refreshes what is
	// remembered about the session and adds no row.
	again := SessionOwnerTagValue{Key: "topic", Value: "AMAZON-ROUTING"}
	renamed := session
	renamed.Title = "Renamed since"
	other := ownerTarget("codex", "s2")
	if err := ix.ApplySessionOwnerTags([]SessionOwnerTarget{renamed, other}, []SessionOwnerTagValue{again}, 20); err != nil {
		t.Fatal(err)
	}
	tags := activeOwnerTags(t, ix)
	if len(tags) != 2 {
		t.Fatalf("want one row per session, got %+v", tags)
	}
	for _, tag := range tags {
		if tag.Key != "Topic" || tag.Value != "Amazon-Routing" {
			t.Fatalf("first spelling not kept: %+v", tag)
		}
		if tag.SessionID == "s1" && (tag.Title != "Renamed since" || tag.AppliedAt != 10) {
			t.Fatalf("re-apply must refresh the snapshot and keep applied_at: %+v", tag)
		}
	}
	// Ǆ/ǆ fold under Go's Unicode folding and not under SQLite's ASCII lower().
	if err := ix.ApplySessionOwnerTags([]SessionOwnerTarget{session}, []SessionOwnerTagValue{{Value: "ÉTÉ"}}, 30); err != nil {
		t.Fatal(err)
	}
	if err := ix.ApplySessionOwnerTags([]SessionOwnerTarget{session}, []SessionOwnerTagValue{{Value: "été"}}, 31); err != nil {
		t.Fatal(err)
	}
	if got := len(activeOwnerTags(t, ix)); got != 3 {
		t.Fatalf("non-ASCII case variants must be one tag, have %d rows", got)
	}
}

func TestOwnerTagRetractReapplyWithinOneSecond(t *testing.T) {
	ix := openOwnerTagTestIndex(t)
	session, tag := []SessionOwnerTarget{ownerTarget("claude", "s1")}, []SessionOwnerTagValue{{Value: "approved"}}
	for round := 0; round < 3; round++ {
		if err := ix.ApplySessionOwnerTags(session, tag, 50); err != nil {
			t.Fatalf("round %d apply: %v", round, err)
		}
		if err := ix.RetractSessionOwnerTags(session, tag, 50); err != nil {
			t.Fatalf("round %d retract: %v", round, err)
		}
	}
	if tags := activeOwnerTags(t, ix); len(tags) != 0 {
		t.Fatalf("retracted tag still active: %+v", tags)
	}
	// Taking off a tag the session does not carry is not an error.
	if err := ix.RetractSessionOwnerTags(session, []SessionOwnerTagValue{{Value: "never-applied"}}, 60); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerTagBulkApplyIsAllOrNothing(t *testing.T) {
	ix := openOwnerTagTestIndex(t)
	targets := []SessionOwnerTarget{ownerTarget("claude", "s1"), {Runtime: "claude"}, ownerTarget("claude", "s3")}
	err := ix.ApplySessionOwnerTags(targets, []SessionOwnerTagValue{{Value: "follow-up"}}, 10)
	if !errors.Is(err, ErrSessionOwnerTagInvalid) {
		t.Fatalf("incomplete identity must be refused: %v", err)
	}
	if tags := activeOwnerTags(t, ix); len(tags) != 0 {
		t.Fatalf("a refused bulk write left rows behind: %+v", tags)
	}
}

func TestOwnerTagValidationRefusesWhatTheGrammarCannotExpress(t *testing.T) {
	for _, bad := range []SessionOwnerTagValue{
		{Value: ""}, {Key: "tag", Value: "x"}, {Key: "TAG", Value: "x"}, {Key: "a:b", Value: "x"}, {Value: "a:b"},
		{Value: "a=b"}, {Value: "star*"}, {Value: `quo"te`}, {Value: " lead"}, {Value: "trail "},
		{Value: "new\nline"}, {Value: strings.Repeat("x", SessionOwnerTagPartMax+1)},
		{Key: strings.Repeat("k", SessionOwnerTagPartMax+1), Value: "x"},
	} {
		if err := ValidateSessionOwnerTag(bad); !errors.Is(err, ErrSessionOwnerTagInvalid) {
			t.Errorf("%+v accepted: %v", bad, err)
		}
	}
	for _, good := range []SessionOwnerTagValue{
		{Value: "approved"}, {Value: "needs review"}, {Key: "topic", Value: "amazon-routing"},
		{Key: "sprint", Value: "38"}, {Value: strings.Repeat("x", SessionOwnerTagPartMax)},
	} {
		if err := ValidateSessionOwnerTag(good); err != nil {
			t.Errorf("%+v refused: %v", good, err)
		}
	}
}

func TestOwnerTagRenameKeepsAgeAndMergesAndPurgeDeletes(t *testing.T) {
	ix := openOwnerTagTestIndex(t)
	a, b := ownerTarget("claude", "a"), ownerTarget("claude", "b")
	typo, fixed := SessionOwnerTagValue{Value: "aproved"}, SessionOwnerTagValue{Value: "approved"}
	if err := ix.ApplySessionOwnerTags([]SessionOwnerTarget{a, b}, []SessionOwnerTagValue{typo}, 10); err != nil {
		t.Fatal(err)
	}
	if err := ix.ApplySessionOwnerTags([]SessionOwnerTarget{b}, []SessionOwnerTagValue{fixed}, 40); err != nil {
		t.Fatal(err)
	}
	changed, err := ix.RenameSessionOwnerTag(typo, fixed, 99)
	if err != nil || changed != 2 {
		t.Fatalf("rename changed=%d err=%v", changed, err)
	}
	tags := activeOwnerTags(t, ix)
	if len(tags) != 2 {
		t.Fatalf("a session that already had the new tag must end with one, got %+v", tags)
	}
	for _, tag := range tags {
		if tag.Value != "approved" {
			t.Fatalf("old tag survives: %+v", tag)
		}
		if tag.SessionID == "a" && tag.AppliedAt != 10 {
			t.Fatalf("rename must keep how long the session has carried it: %+v", tag)
		}
	}
	removed, err := ix.PurgeSessionOwnerTag(SessionOwnerTagValue{Value: "APPROVED"})
	if err != nil || removed == 0 {
		t.Fatalf("purge removed=%d err=%v", removed, err)
	}
	var left int
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM session_owner_tag WHERE value_fold='approved'`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("purge is a delete, history included: left=%d err=%v", left, err)
	}
}

func TestOwnerTagVocabularyOrdersByRecentUse(t *testing.T) {
	ix := openOwnerTagTestIndex(t)
	a, b := ownerTarget("claude", "a"), ownerTarget("claude", "b")
	for at, tag := range []SessionOwnerTagValue{{Value: "old"}, {Key: "topic", Value: "walmart"}, {Value: "recent"}} {
		if err := ix.ApplySessionOwnerTags([]SessionOwnerTarget{a}, []SessionOwnerTagValue{tag}, int64(10+at)); err != nil {
			t.Fatal(err)
		}
	}
	if err := ix.ApplySessionOwnerTags([]SessionOwnerTarget{b}, []SessionOwnerTagValue{{Value: "old"}}, 5); err != nil {
		t.Fatal(err)
	}
	vocabulary, err := ix.SessionOwnerTagVocabulary(2)
	if err != nil || len(vocabulary) != 2 || vocabulary[0].Value != "recent" || vocabulary[1].Value != "walmart" || vocabulary[1].Key != "topic" {
		t.Fatalf("vocabulary=%+v err=%v", vocabulary, err)
	}
	all, _ := ix.SessionOwnerTagVocabulary(10)
	if len(all) != 3 || all[2].Value != "old" || all[2].Sessions != 2 {
		t.Fatalf("usage counts: %+v", all)
	}
}

func TestOwnerNoteReplacesClearsAndIsBounded(t *testing.T) {
	ix := openOwnerTagTestIndex(t)
	session := ownerTarget("claude", "s1")
	for _, text := range []string{"lawyer call Thursday", "  waiting on finance  "} {
		if err := ix.PutSessionOwnerNote(session, text, 10); err != nil {
			t.Fatal(err)
		}
	}
	notes, _, err := ix.AllSessionOwnerNotes(10)
	if err != nil || len(notes) != 1 || notes[0].Text != "waiting on finance" {
		t.Fatalf("a note is one replaceable, trimmed line: %+v %v", notes, err)
	}
	if err := ix.PutSessionOwnerNote(session, strings.Repeat("x", SessionOwnerNoteMax+1), 11); !errors.Is(err, ErrSessionOwnerTagInvalid) {
		t.Fatalf("over-long note accepted: %v", err)
	}
	if err := ix.PutSessionOwnerNote(session, "", 12); err != nil {
		t.Fatal(err)
	}
	if notes, _, _ := ix.AllSessionOwnerNotes(10); len(notes) != 0 {
		t.Fatalf("empty text clears the note: %+v", notes)
	}
}

func TestOwnerTagReadsReportTruncationInsteadOfHidingIt(t *testing.T) {
	ix := openOwnerTagTestIndex(t)
	for i := 0; i < 3; i++ {
		target := ownerTarget("claude", fmt.Sprintf("s%d", i))
		if err := ix.ApplySessionOwnerTags([]SessionOwnerTarget{target}, []SessionOwnerTagValue{{Value: "x"}}, int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	tags, truncated, err := ix.AllActiveSessionOwnerTags(2)
	if err != nil || !truncated || len(tags) != 2 {
		t.Fatalf("tags=%d truncated=%v err=%v", len(tags), truncated, err)
	}
	if _, truncated, _ := ix.AllActiveSessionOwnerTags(3); truncated {
		t.Fatal("an exact fit is not a truncation")
	}
}

// A session the owner tagged keeps its catalogue row and its indexed text
// through a clean orphan sweep; an untagged one is still swept, and the
// reported count is only what was actually removed.
func TestOrphanSweepSparesOwnerTaggedSessions(t *testing.T) {
	ix := openOwnerTagTestIndex(t)
	for _, id := range []string{"tagged", "untagged", "retracted"} {
		if _, err := ix.ReplaceTranscriptProjection(projectionFixture("claude", id, "g1", "Title "+id, id+" pelican transcript", projectionTestTime)); err != nil {
			t.Fatal(err)
		}
	}
	tag := []SessionOwnerTagValue{{Value: "follow-up"}}
	if err := ix.ApplySessionOwnerTags([]SessionOwnerTarget{ownerTarget("claude", "tagged"), ownerTarget("claude", "retracted")}, tag, 10); err != nil {
		t.Fatal(err)
	}
	if err := ix.RetractSessionOwnerTags([]SessionOwnerTarget{ownerTarget("claude", "retracted")}, tag, 11); err != nil {
		t.Fatal(err)
	}
	removed, err := ix.RemoveTranscriptProjectionOrphans(nil)
	if err != nil || removed != 2 {
		t.Fatalf("removed=%d err=%v (the tagged session must not be counted)", removed, err)
	}
	row, found, err := ix.SessionByID("claude", "tagged")
	if err != nil || !found || row.Title != "Title tagged" {
		t.Fatalf("tagged session's catalogue row was blanked: %+v found=%v err=%v", row, found, err)
	}
	docs, err := ix.TranscriptDocuments("claude", "tagged", 100)
	if err != nil || len(docs) == 0 {
		t.Fatalf("tagged session's kept text is gone: %+v %v", docs, err)
	}
	if hits, _ := ix.SearchEvents("untagged pelican", 10); len(hits) != 0 {
		t.Fatalf("untagged session must still be swept: %+v", hits)
	}
	if docs, _ := ix.TranscriptDocuments("claude", "retracted", 100); len(docs) != 0 {
		t.Fatalf("a retracted tag keeps nothing: %+v", docs)
	}

	// The exemption is bounded when a limit is set, most recently tagged first.
	ix.SetOwnerKeptSessionsLimit(1)
	for _, id := range []string{"older", "newer"} {
		if _, err := ix.ReplaceTranscriptProjection(projectionFixture("claude", id, "g1", id, id+" heron", projectionTestTime)); err != nil {
			t.Fatal(err)
		}
	}
	_ = ix.RetractSessionOwnerTags([]SessionOwnerTarget{ownerTarget("claude", "tagged")}, tag, 12)
	_ = ix.ApplySessionOwnerTags([]SessionOwnerTarget{ownerTarget("claude", "older")}, tag, 20)
	_ = ix.ApplySessionOwnerTags([]SessionOwnerTarget{ownerTarget("claude", "newer")}, tag, 30)
	if _, err := ix.RemoveTranscriptProjectionOrphans(nil); err != nil {
		t.Fatal(err)
	}
	if docs, _ := ix.TranscriptDocuments("claude", "newer", 10); len(docs) == 0 {
		t.Fatal("the most recently tagged session is inside the bound")
	}
	if docs, _ := ix.TranscriptDocuments("claude", "older", 10); len(docs) != 0 {
		t.Fatal("beyond the bound the exemption stops")
	}
}

// Filtering a global top-N afterwards loses a session that ranks below N
// globally; confining the match to the candidate set finds it. The answer is in
// sessions, so a few long sessions cannot crowd the others out, and it says
// when more matched than it returned.
func TestSessionsMatchingTextFindsWhatGlobalRankingBuriesAndCountsSessions(t *testing.T) {
	ix := openOwnerTagTestIndex(t)
	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("noisy-%d", i)
		if _, err := ix.ReplaceTranscriptProjection(projectionFixture("claude", id, "g1", id,
			"repricer repricer repricer repricer margin", projectionTestTime)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ix.ReplaceTranscriptProjection(projectionFixture("claude", "wanted", "g1", "wanted",
		"a long passage about many unrelated things that mentions the repricer exactly once near its end", projectionTestTime)); err != nil {
		t.Fatal(err)
	}
	global, err := ix.SearchEvents("repricer", 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range global {
		if hit.SessionID == "wanted" {
			t.Fatal("the fixture no longer buries the wanted session in the global top 3, so this test proves nothing; strengthen the noise")
		}
	}
	confined, more, err := ix.SessionsMatchingText("repricer", []string{"wanted", "absent"}, 3)
	if err != nil || more || len(confined) != 1 || confined[0] != (TranscriptProjectionKey{Runtime: "claude", SessionID: "wanted"}) {
		t.Fatalf("confined=%+v more=%v err=%v", confined, more, err)
	}
	ids := []string{"wanted"}
	for i := 0; i < 8; i++ {
		ids = append(ids, fmt.Sprintf("noisy-%d", i))
	}
	sessions, more, err := ix.SessionsMatchingText("repricer", ids, 4)
	if err != nil || !more || len(sessions) != 4 {
		t.Fatalf("nine sessions match and four were asked for: got %d more=%v err=%v", len(sessions), more, err)
	}
	seen := map[TranscriptProjectionKey]bool{}
	for _, key := range sessions {
		if seen[key] {
			t.Fatalf("a session was returned twice — the answer is in events, not sessions: %+v", sessions)
		}
		seen[key] = true
	}
	if none, _, err := ix.SessionsMatchingText("repricer", nil, 3); err != nil || len(none) != 0 {
		t.Fatalf("an empty set matches nothing: %+v %v", none, err)
	}
}

// "Stored as first written" has to survive a rename: into a tag that already
// exists (its spelling wins), and a rename that only changes capitals (every
// row takes the new spelling, and later applies use it).
func TestOwnerTagRenameKeepsOneSpelling(t *testing.T) {
	ix := openOwnerTagTestIndex(t)
	a, b := ownerTarget("claude", "a"), ownerTarget("claude", "b")
	apply := func(target SessionOwnerTarget, value string, at int64) {
		t.Helper()
		if err := ix.ApplySessionOwnerTags([]SessionOwnerTarget{target}, []SessionOwnerTagValue{{Value: value}}, at); err != nil {
			t.Fatal(err)
		}
	}
	apply(a, "approved", 10)
	apply(b, "aproved", 20)
	if _, err := ix.RenameSessionOwnerTag(SessionOwnerTagValue{Value: "aproved"}, SessionOwnerTagValue{Value: "APPROVED"}, 30); err != nil {
		t.Fatal(err)
	}
	for _, tag := range activeOwnerTags(t, ix) {
		if tag.Value != "approved" {
			t.Fatalf("renaming into an existing tag must take its spelling: %+v", tag)
		}
	}
	changed, err := ix.RenameSessionOwnerTag(SessionOwnerTagValue{Value: "approved"}, SessionOwnerTagValue{Value: "Approved"}, 40)
	if err != nil || changed == 0 {
		t.Fatalf("case-only rename changed=%d err=%v", changed, err)
	}
	apply(ownerTarget("claude", "c"), "approved", 50)
	tags := activeOwnerTags(t, ix)
	if len(tags) != 3 {
		t.Fatalf("a case-only rename must not add or drop rows: %+v", tags)
	}
	for _, tag := range tags {
		if tag.Value != "Approved" {
			t.Fatalf("after a case-only rename every row, and every later apply, uses the new spelling: %+v", tag)
		}
		if tag.SessionID == "a" && tag.AppliedAt != 10 {
			t.Fatalf("a case-only rename must keep how long the session has carried the tag: %+v", tag)
		}
	}
	vocabulary, err := ix.SessionOwnerTagVocabulary(10)
	if err != nil || len(vocabulary) != 1 || vocabulary[0].Value != "Approved" || vocabulary[0].Sessions != 3 {
		t.Fatalf("vocabulary=%+v err=%v", vocabulary, err)
	}
}

// Taking a tag off and putting one on is one transaction: an unstorable tag in
// either list means nothing is written.
func TestOwnerTagMixedChangeIsOneTransaction(t *testing.T) {
	ix := openOwnerTagTestIndex(t)
	session := []SessionOwnerTarget{ownerTarget("claude", "s1")}
	if err := ix.ApplySessionOwnerTags(session, []SessionOwnerTagValue{{Value: "planning"}}, 10); err != nil {
		t.Fatal(err)
	}
	err := ix.ChangeSessionOwnerTags(session, []SessionOwnerTagValue{{Value: "bad*"}}, []SessionOwnerTagValue{{Value: "planning"}}, 20)
	if !errors.Is(err, ErrSessionOwnerTagInvalid) {
		t.Fatalf("err=%v", err)
	}
	if tags := activeOwnerTags(t, ix); len(tags) != 1 || tags[0].Value != "planning" {
		t.Fatalf("a refused change wrote something: %+v", tags)
	}
	if err := ix.ChangeSessionOwnerTags(session, []SessionOwnerTagValue{{Value: "approved"}}, []SessionOwnerTagValue{{Value: "planning"}}, 30); err != nil {
		t.Fatal(err)
	}
	if tags := activeOwnerTags(t, ix); len(tags) != 1 || tags[0].Value != "approved" {
		t.Fatalf("tags=%+v", tags)
	}
	remembered, found, err := ix.RememberedSessionOwnerTag("claude", "s1")
	if err != nil || !found || remembered.Title != "Title of s1" {
		t.Fatalf("remembered=%+v found=%v err=%v", remembered, found, err)
	}
	if _, found, _ := ix.RememberedSessionOwnerTag("codex", "s1"); found {
		t.Fatal("a session is remembered per runtime")
	}
}

func TestOwnerTagSchemaIsAdditiveOnAnOlderStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ix.db.Exec(`DROP TABLE session_owner_tag; DROP TABLE session_owner_note; PRAGMA user_version=31;`); err != nil {
		t.Fatal(err)
	}
	_ = ix.Close()
	for pass := 0; pass < 2; pass++ {
		ix, err = Open(path)
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		var tables, indexes, version int
		_ = ix.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('session_owner_tag','session_owner_note')`).Scan(&tables)
		_ = ix.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='index' AND name IN ('session_owner_tag_active','session_owner_tag_fold')`).Scan(&indexes)
		_ = ix.db.QueryRow(`PRAGMA user_version`).Scan(&version)
		if tables != 2 || indexes != 2 || version != SchemaVersion {
			t.Fatalf("pass %d: tables=%d indexes=%d version=%d", pass, tables, indexes, version)
		}
		if tags := activeOwnerTags(t, ix); len(tags) != 0 {
			t.Fatalf("pass %d: migration must not seed anything: %+v", pass, tags)
		}
		_ = ix.Close()
	}
}
