package profilefs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newTestOwner(t *testing.T) (*Owner, string) {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	owner, err := New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	owner.now = func() time.Time { return time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC) }
	return owner, dataDir
}

func TestPreviewIsFilesystemNoWriteAndSelectIsExactAndInspectable(t *testing.T) {
	owner, dataDir := newTestOwner(t)
	source := validReviewerSource("1.0.0", "Review without session history.\n")
	preview, err := owner.Preview("PROFILE.md", source)
	if err != nil {
		t.Fatal(err)
	}
	if preview.RuntimeEffects || preview.SelectionState != "not_selected" || preview.AuthorityGranted {
		t.Fatalf("preview = %+v", preview)
	}
	if _, err := os.Lstat(filepath.Join(dataDir, "orchestration")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preview wrote profile state: %v", err)
	}
	result, err := owner.Select(SelectCommand{SourceName: "PROFILE.md", Source: source,
		ExpectedSourceDigest: preview.SourceDigest, ExpectedBundleDigest: preview.BundleDigest,
		ExpectedStateToken: preview.StateToken})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.Detail.SelectionState != "selected_inert" || result.Detail.RuntimeEffects ||
		result.Detail.Source != string(source) || result.Detail.Integrity != "verified" {
		t.Fatalf("select result = %+v", result)
	}
	if result.Detail.Current.SelectedBy != "authenticated-local-client" {
		t.Fatalf("selector attribution = %q", result.Detail.Current.SelectedBy)
	}
	for _, forbidden := range []string{"bindings", "groups", "invocations", "tasks", "events", "approvals"} {
		if _, err := os.Lstat(filepath.Join(dataDir, "orchestration", forbidden)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("selection created deferred artifact %s: %v", forbidden, err)
		}
	}
	list, err := owner.List()
	if err != nil || len(list.Profiles) != 1 || list.Profiles[0].SelectionState != "selected_inert" ||
		list.Profiles[0].Integrity != "verified" || list.RuntimeEffects {
		t.Fatalf("list = %+v, %v", list, err)
	}
	detail, err := owner.Get("command-reviewer")
	if err != nil || detail.Normalized == nil || detail.Manifest == nil || len(detail.History) != 0 {
		t.Fatalf("detail = %+v, %v", detail, err)
	}
	idempotent, err := owner.Select(SelectCommand{SourceName: "PROFILE.md", Source: source,
		ExpectedSourceDigest: preview.SourceDigest, ExpectedBundleDigest: preview.BundleDigest,
		ExpectedStateToken: result.Detail.CurrentStateTokenForTest(t, owner)})
	if err != nil || idempotent.Changed || len(idempotent.Detail.History) != 0 {
		t.Fatalf("idempotent = %+v, %v", idempotent, err)
	}
}

func (detail Detail) CurrentStateTokenForTest(t *testing.T, owner *Owner) string {
	t.Helper()
	record, err := owner.readSelection(detail.ProfileID)
	if err != nil || record == nil {
		t.Fatalf("read selected token: %+v, %v", record, err)
	}
	return record.StateToken
}

func TestSelectRejectsStaleAndConcurrentWritersAndKeepsHistory(t *testing.T) {
	owner, _ := newTestOwner(t)
	base := validReviewerSource("1.0.0", "First instructions.\n")
	first, err := owner.Preview("PROFILE.md", base)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := owner.Select(commandFromPreview(base, first))
	if err != nil {
		t.Fatal(err)
	}
	secondSource := validReviewerSource("1.1.0", "Second instructions.\n")
	thirdSource := validReviewerSource("1.2.0", "Third instructions.\n")
	second, err := owner.Preview("PROFILE.md", secondSource)
	if err != nil {
		t.Fatal(err)
	}
	third, err := owner.Preview("PROFILE.md", thirdSource)
	if err != nil {
		t.Fatal(err)
	}
	if second.StateToken != third.StateToken || second.StateToken == first.StateToken {
		t.Fatal("preview tokens did not reflect current per-profile state")
	}
	next, err := owner.Select(commandFromPreview(secondSource, second))
	if err != nil || len(next.Detail.History) != 1 || next.Detail.History[0].Version != "1.0.0" {
		t.Fatalf("second selection = %+v, %v (initial=%+v)", next, err, selected)
	}
	if _, err := owner.Select(commandFromPreview(thirdSource, third)); ProblemCode(err) != "state_conflict" {
		t.Fatalf("stale selection error = %v", err)
	}

	fourthSource := validReviewerSource("1.3.0", "Fourth instructions.\n")
	fifthSource := validReviewerSource("1.4.0", "Fifth instructions.\n")
	fourth, _ := owner.Preview("PROFILE.md", fourthSource)
	fifth, _ := owner.Preview("PROFILE.md", fifthSource)
	errs := make(chan error, 2)
	var wait sync.WaitGroup
	for _, item := range []struct {
		source  []byte
		preview Preview
	}{{fourthSource, fourth}, {fifthSource, fifth}} {
		wait.Add(1)
		go func(source []byte, preview Preview) {
			defer wait.Done()
			_, selectErr := owner.Select(commandFromPreview(source, preview))
			errs <- selectErr
		}(item.source, item.preview)
	}
	wait.Wait()
	close(errs)
	successes, conflicts := 0, 0
	for selectErr := range errs {
		if selectErr == nil {
			successes++
		} else if ProblemCode(selectErr) == "state_conflict" {
			conflicts++
		} else {
			t.Fatalf("unexpected concurrent error: %v", selectErr)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent results success=%d conflict=%d", successes, conflicts)
	}
}

func TestTamperedImmutableStateIsVisibleAndCannotBeOverwritten(t *testing.T) {
	owner, _ := newTestOwner(t)
	source := validReviewerSource("1.0.0", "Original instructions.\n")
	preview, _ := owner.Preview("PROFILE.md", source)
	if _, err := owner.Select(commandFromPreview(source, preview)); err != nil {
		t.Fatal(err)
	}
	dir, _ := owner.documentDirectory(preview.BundleDigest, preview.SourceDigest)
	if err := os.WriteFile(filepath.Join(dir, "PROFILE.md"), validReviewerSource("1.0.0", "Tampered instructions.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	detail, err := owner.Get("command-reviewer")
	if err != nil || detail.Integrity != "needs_attention" || detail.Problem == nil || detail.Source != "" {
		t.Fatalf("tampered detail = %+v, %v", detail, err)
	}
	list, err := owner.List()
	if err != nil || len(list.Profiles) != 1 || list.Profiles[0].Integrity != "needs_attention" {
		t.Fatalf("tampered list = %+v, %v", list, err)
	}
	update := validReviewerSource("1.1.0", "Clean update.\n")
	updatePreview, err := owner.Preview("PROFILE.md", update)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Select(commandFromPreview(update, updatePreview)); ProblemCode(err) != "integrity_conflict" {
		t.Fatalf("tampered state overwrite error = %v", err)
	}
}

func TestSelectionRefusesSymlinkedOwnerPath(t *testing.T) {
	owner, dataDir := newTestOwner(t)
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(dataDir, "orchestration")); err != nil {
		t.Fatal(err)
	}
	source := validReviewerSource("1.0.0", "Review.\n")
	document, err := Parse("PROFILE.md", source)
	if err != nil {
		t.Fatal(err)
	}
	command := SelectCommand{SourceName: "PROFILE.md", Source: source, ExpectedSourceDigest: document.SourceDigest,
		ExpectedBundleDigest: document.BundleDigest, ExpectedStateToken: absentStateToken(document.Profile.ID)}
	if _, err := owner.Select(command); ProblemCode(err) != "storage_error" {
		t.Fatalf("symlink selection error = %v", err)
	}
}

func TestReadRefusesSymlinkedSelectionAndDocumentComponents(t *testing.T) {
	owner, _ := newTestOwner(t)
	source := validReviewerSource("1.0.0", "Review.\n")
	preview, _ := owner.Preview("PROFILE.md", source)
	if _, err := owner.Select(commandFromPreview(source, preview)); err != nil {
		t.Fatal(err)
	}
	selectionDir := filepath.Join(owner.root, "selections")
	selectionSaved := filepath.Join(owner.root, "selections-saved")
	if err := os.Rename(selectionDir, selectionSaved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(selectionSaved, selectionDir); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Get("command-reviewer"); ProblemCode(err) != "integrity_conflict" {
		t.Fatalf("symlinked selection read error = %v", err)
	}
	if err := os.Rename(selectionDir, filepath.Join(owner.root, "selection-link-saved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(selectionSaved, selectionDir); err != nil {
		t.Fatal(err)
	}
	documentDir, _ := owner.documentDirectory(preview.BundleDigest, preview.SourceDigest)
	documentSaved := documentDir + "-saved"
	if err := os.Rename(documentDir, documentSaved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(documentSaved, documentDir); err != nil {
		t.Fatal(err)
	}
	detail, err := owner.Get("command-reviewer")
	if err != nil || detail.Integrity != "needs_attention" {
		t.Fatalf("symlinked document detail = %+v, %v", detail, err)
	}
}

func TestSelectionHistoryIsBoundedToFiftyRevisions(t *testing.T) {
	owner, _ := newTestOwner(t)
	for version := 0; version < maxHistory+3; version++ {
		source := validReviewerSource(fmt.Sprintf("1.%d.0", version), fmt.Sprintf("Revision %d.\n", version))
		preview, err := owner.Preview("PROFILE.md", source)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := owner.Select(commandFromPreview(source, preview)); err != nil {
			t.Fatal(err)
		}
	}
	detail, err := owner.Get("command-reviewer")
	if err != nil || len(detail.History) != maxHistory || detail.History[0].Version != "1.51.0" ||
		detail.History[maxHistory-1].Version != "1.2.0" {
		t.Fatalf("bounded history len=%d first=%q last=%q err=%v", len(detail.History),
			detail.History[0].Version, detail.History[len(detail.History)-1].Version, err)
	}
}

func TestGetRevisionReadsCurrentAndHistoricalExactBytesWithoutActivation(t *testing.T) {
	owner, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	firstSource := validReviewerSource("1.0.0", "First exact instructions.\n")
	firstPreview, err := owner.Preview("PROFILE.md", firstSource)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Select(SelectCommand{SourceName: "PROFILE.md", Source: firstSource,
		ExpectedSourceDigest: firstPreview.SourceDigest, ExpectedBundleDigest: firstPreview.BundleDigest,
		ExpectedStateToken: firstPreview.StateToken}); err != nil {
		t.Fatal(err)
	}
	secondSource := validReviewerSource("1.1.0", "Second exact instructions.\n")
	secondPreview, err := owner.Preview("PROFILE.md", secondSource)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Select(SelectCommand{SourceName: "PROFILE.md", Source: secondSource,
		ExpectedSourceDigest: secondPreview.SourceDigest, ExpectedBundleDigest: secondPreview.BundleDigest,
		ExpectedStateToken: secondPreview.StateToken}); err != nil {
		t.Fatal(err)
	}

	historical, err := owner.GetRevision(firstPreview.ProfileID, firstPreview.SourceDigest, firstPreview.BundleDigest)
	if err != nil {
		t.Fatal(err)
	}
	if historical.Current.Version != "1.0.0" || historical.Source != string(firstSource) ||
		historical.RuntimeEffects || historical.Integrity != "verified" {
		t.Fatalf("historical revision=%+v", historical)
	}
	current, err := owner.GetRevision(secondPreview.ProfileID, secondPreview.SourceDigest, secondPreview.BundleDigest)
	if err != nil || current.Current.Version != "1.1.0" {
		t.Fatalf("current=%+v err=%v", current, err)
	}
	if _, err := owner.GetRevision(firstPreview.ProfileID, secondPreview.SourceDigest, firstPreview.BundleDigest); ProblemCode(err) != "not_found" {
		t.Fatalf("mixed revision identity err=%v", err)
	}
}

func commandFromPreview(source []byte, preview Preview) SelectCommand {
	return SelectCommand{SourceName: "PROFILE.md", Source: source, ExpectedSourceDigest: preview.SourceDigest,
		ExpectedBundleDigest: preview.BundleDigest, ExpectedStateToken: preview.StateToken}
}
