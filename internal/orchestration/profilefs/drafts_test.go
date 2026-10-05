package profilefs

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func selectForTest(t *testing.T, owner *Owner, source []byte) SelectResult {
	t.Helper()
	preview, err := owner.Preview("PROFILE.md", source)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.Select(commandFromPreview(source, preview))
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestDraftLifecycleCreateEditDiscard(t *testing.T) {
	owner, _ := newTestOwner(t)
	if _, found, err := owner.Draft("design-helper"); found || err != nil {
		t.Fatalf("empty store has a draft: %v %v", found, err)
	}
	source := validHelperV2Source()
	saved, err := owner.PutDraft(DraftCommand{ProfileID: "design-helper", Source: source,
		ExpectedStateToken: DraftAbsentToken("design-helper")})
	if err != nil {
		t.Fatal(err)
	}
	if saved.Problem != nil || saved.Normalized == nil || saved.SourceDigest == "" || saved.Source != string(source) {
		t.Fatalf("saved draft = %+v", saved)
	}
	if _, err := owner.PutDraft(DraftCommand{ProfileID: "design-helper", Source: source,
		ExpectedStateToken: DraftAbsentToken("design-helper")}); ProblemCode(err) != "state_conflict" {
		t.Fatalf("a second create must conflict, got %v", err)
	}
	invalid := []byte(strings.Replace(string(source), "event: task.completed", "event: nothing.real", 1))
	edited, err := owner.PutDraft(DraftCommand{ProfileID: "design-helper", Source: invalid, ExpectedStateToken: saved.StateToken})
	if err != nil {
		t.Fatalf("an invalid draft is still stored: %v", err)
	}
	if edited.Problem == nil || edited.Normalized != nil || edited.SourceDigest != "" {
		t.Fatalf("invalid draft must carry its problem only: %+v", edited)
	}
	drafts, err := owner.Drafts()
	if err != nil || len(drafts) != 1 || drafts[0].ProfileID != "design-helper" {
		t.Fatalf("drafts = %+v, %v", drafts, err)
	}
	if err := owner.DiscardDraft("design-helper", saved.StateToken); ProblemCode(err) != "state_conflict" {
		t.Fatalf("a stale discard must conflict, got %v", err)
	}
	if err := owner.DiscardDraft("design-helper", edited.StateToken); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := owner.Draft("design-helper"); found {
		t.Fatal("discarded draft still readable")
	}
}

func TestDraftRejectsForeignIDAndBadBytes(t *testing.T) {
	owner, _ := newTestOwner(t)
	if _, err := owner.PutDraft(DraftCommand{ProfileID: "other-agent", Source: validHelperV2Source(),
		ExpectedStateToken: DraftAbsentToken("other-agent")}); ProblemCode(err) != "invalid_profile" {
		t.Fatalf("a valid document with another id must be refused, got %v", err)
	}
	if _, err := owner.PutDraft(DraftCommand{ProfileID: "design-helper", Source: []byte("a\x00b"),
		ExpectedStateToken: DraftAbsentToken("design-helper")}); ProblemCode(err) != "invalid_profile" {
		t.Fatalf("NUL bytes must be refused, got %v", err)
	}
}

func TestPublishDraftSelectsAndRemovesAtomically(t *testing.T) {
	owner, _ := newTestOwner(t)
	draft, err := owner.PutDraft(DraftCommand{ProfileID: "design-helper", Source: validHelperV2Source(),
		ExpectedStateToken: DraftAbsentToken("design-helper")})
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.PublishDraft("design-helper", draft.StateToken, absentStateToken("design-helper"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.Detail.Current.Version != "1.0.0" || result.Detail.StateToken == "" {
		t.Fatalf("publish = %+v", result)
	}
	if _, found, _ := owner.Draft("design-helper"); found {
		t.Fatal("published draft was not removed")
	}
	// Edit on top of the published version, then publish again.
	next, err := ApplyEdit(validHelperV2Source(), ProfileEdit{Version: strPtr("1.1.0"), Description: strPtr("Second.")})
	if err != nil {
		t.Fatal(err)
	}
	second, err := owner.PutDraft(DraftCommand{ProfileID: "design-helper", Source: next,
		BaseSourceDigest: result.Detail.Current.SourceDigest, BaseBundleDigest: result.Detail.Current.BundleDigest,
		ExpectedStateToken: DraftAbsentToken("design-helper")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.PublishDraft("design-helper", second.StateToken, "stale-token", nil); ProblemCode(err) != "state_conflict" {
		t.Fatalf("a stale selection token must conflict, got %v", err)
	}
	published, err := owner.PublishDraft("design-helper", second.StateToken, result.Detail.StateToken, nil)
	if err != nil || published.Detail.Current.Version != "1.1.0" || len(published.Detail.History) != 1 {
		t.Fatalf("second publish = %+v, %v", published, err)
	}
}

func TestPublishDraftRefusesMovedBaseAndTakenVersion(t *testing.T) {
	owner, _ := newTestOwner(t)
	first := selectForTest(t, owner, validHelperV2Source())
	// A draft started from nothing, while a version is already selected: its base moved.
	moved, err := owner.PutDraft(DraftCommand{ProfileID: "design-helper", Source: validHelperV2Source(),
		ExpectedStateToken: DraftAbsentToken("design-helper")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.PublishDraft("design-helper", moved.StateToken, first.Detail.StateToken, nil); ProblemCode(err) != "state_conflict" {
		t.Fatalf("a draft whose base moved must conflict, got %v", err)
	}
	if err := owner.DiscardDraft("design-helper", moved.StateToken); err != nil {
		t.Fatal(err)
	}
	// Same version number, different bytes.
	clash, err := ApplyEdit(validHelperV2Source(), ProfileEdit{Description: strPtr("Different bytes, same version.")})
	if err != nil {
		t.Fatal(err)
	}
	taken, err := owner.PutDraft(DraftCommand{ProfileID: "design-helper", Source: clash,
		BaseSourceDigest: first.Detail.Current.SourceDigest, BaseBundleDigest: first.Detail.Current.BundleDigest,
		ExpectedStateToken: DraftAbsentToken("design-helper")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.PublishDraft("design-helper", taken.StateToken, first.Detail.StateToken, nil); ProblemCode(err) != "version_taken" {
		t.Fatalf("a taken version must be refused, got %v", err)
	}
	// An invalid draft cannot publish.
	broken, err := owner.PutDraft(DraftCommand{ProfileID: "design-helper",
		Source: []byte(strings.Replace(string(clash), "event: task.completed", "event: nothing.real", 1)), ExpectedStateToken: taken.StateToken})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.PublishDraft("design-helper", broken.StateToken, first.Detail.StateToken, nil); err == nil {
		t.Fatal("an invalid draft must not publish")
	}
}

func TestSelectRefusesToEvictAPinnedRevision(t *testing.T) {
	owner, _ := newTestOwner(t)
	var oldest RevisionRef
	for index := 0; index <= maxHistory; index++ {
		source := validReviewerSource(fmt.Sprintf("1.0.%d", index), fmt.Sprintf("Revision %d.\n", index))
		result := selectForTest(t, owner, source)
		if index == 0 {
			oldest = RevisionRef{SourceDigest: result.Detail.Current.SourceDigest, BundleDigest: result.Detail.Current.BundleDigest, Holder: "repo-one"}
		}
	}
	// History is now full: current + 50. The next selection would evict the oldest.
	source := validReviewerSource("2.0.0", "Next.\n")
	preview, err := owner.Preview("PROFILE.md", source)
	if err != nil {
		t.Fatal(err)
	}
	command := commandFromPreview(source, preview)
	command.Pins = func() ([]RevisionRef, error) { return []RevisionRef{oldest}, nil }
	if _, err := owner.Select(command); ProblemCode(err) != "pinned_revision" || !strings.Contains(err.Error(), "repo-one") {
		t.Fatalf("evicting a pinned revision must be refused and name its place, got %v", err)
	}
	command.Pins = func() ([]RevisionRef, error) { return nil, errors.New("store unreadable") }
	if _, err := owner.Select(command); ProblemCode(err) != "pins_unavailable" {
		t.Fatalf("unreadable pins must refuse the trim, got %v", err)
	}
	command.Pins = func() ([]RevisionRef, error) { return nil, nil }
	if _, err := owner.Select(command); err != nil {
		t.Fatalf("an unpinned oldest revision may be trimmed: %v", err)
	}
}
