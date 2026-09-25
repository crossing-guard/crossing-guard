package rulebook

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func isolatedRulebook(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CG_RULES", "")
	if err := os.Mkdir(filepath.Join(home, ".crossing-guard"), 0o755); err != nil {
		t.Fatal(err)
	}
	return home
}

func selectPreview(t *testing.T, source, path, selector string) SelectionResult {
	t.Helper()
	preview, err := PreviewSelection(source, path)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Select(SelectRequest{Source: source, Path: path, Selector: selector,
		ExpectedActiveDigest: preview.ExpectedActiveDigest, ExpectedStateToken: preview.ExpectedStateToken,
		AcknowledgedDigest: preview.ProposedDigest})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestExplicitStarterSelectionPinsImmutableValidatedBytes(t *testing.T) {
	home := isolatedRulebook(t)
	before, err := LoadDocument()
	if err != nil {
		t.Fatal(err)
	}
	if before.Selected || before.Selection != "legacy-implicit" {
		t.Fatalf("unexpected initial state: %+v", before)
	}
	result := selectPreview(t, "starter", "", "cli")
	if !result.Active.Selected || result.Active.Selection != "explicit-user" || result.Active.Origin != "selected-user" {
		t.Fatalf("selection not active: %+v", result.Active)
	}
	if result.Active.Digest != before.Digest || result.DurableSelection.Digest != before.Digest {
		t.Fatalf("starter selection changed bytes: before=%s result=%+v", before.Digest, result)
	}
	if result.DurableSelection.SourceRef != "embedded starter catalog" ||
		result.Active.SelectedSource != "starter" ||
		result.Active.SelectedSourceRef != "embedded starter catalog" {
		t.Fatalf("selected source provenance was not preserved: %+v", result)
	}
	root := filepath.Join(home, ".crossing-guard", "policy", "rulebook")
	for _, path := range []string{root, filepath.Join(root, "documents")} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("owner directory mode for %s = %v err=%v", path, info.Mode().Perm(), err)
		}
	}
	for _, path := range []string{filepath.Join(root, "selection.json"), result.Active.Path} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("selected file mode for %s = %v err=%v", path, info.Mode().Perm(), err)
		}
	}
}

func TestSelectedSaveIsCompareAndSwapAndAdvancesDigest(t *testing.T) {
	isolatedRulebook(t)
	selected := selectPreview(t, "none", "", "console")
	oldState := selected.Active.StateToken
	raw := []byte(`{"rules":[{"id":"new","action":"deny","if":{"tag":"command","matches":"x"}}]}`)
	saved, err := SaveExpected(raw, oldState, "console")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Backup != "" || saved.Document.StateToken == oldState || saved.Document.Origin != "selected-user" {
		t.Fatalf("selected save did not advance immutable selection: %+v", saved)
	}
	if saved.Document.SelectedSource != "edited" || saved.Document.SelectedSourceRef != selected.Active.Digest {
		t.Fatalf("selected edit source provenance = %+v", saved.Document)
	}
	if _, err := SaveExpected([]byte(`{"rules":[]}`), oldState, "console"); err == nil {
		t.Fatal("stale selected save was accepted")
	} else {
		var conflict *ConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("stale save error = %T %v", err, err)
		}
	}
}

func TestConcurrentSelectionsCannotBothReplaceReviewedBase(t *testing.T) {
	isolatedRulebook(t)
	starter, err := PreviewSelection("starter", "")
	if err != nil {
		t.Fatal(err)
	}
	none, err := PreviewSelection("none", "")
	if err != nil {
		t.Fatal(err)
	}
	requests := []SelectRequest{
		{Source: "starter", Selector: "cli", ExpectedStateToken: starter.ExpectedStateToken,
			ExpectedActiveDigest: starter.ExpectedActiveDigest, AcknowledgedDigest: starter.ProposedDigest},
		{Source: "none", Selector: "console", ExpectedStateToken: none.ExpectedStateToken,
			ExpectedActiveDigest: none.ExpectedActiveDigest, AcknowledgedDigest: none.ProposedDigest},
	}
	errs := make(chan error, len(requests))
	for _, request := range requests {
		request := request
		go func() { _, err := Select(request); errs <- err }()
	}
	successes, conflicts := 0, 0
	for range requests {
		err := <-errs
		if err == nil {
			successes++
			continue
		}
		var conflict *ConflictError
		if errors.As(err, &conflict) {
			conflicts++
		} else {
			t.Fatalf("unexpected concurrent selection error: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent result successes=%d conflicts=%d", successes, conflicts)
	}
}

func TestSelectionRejectsMismatchedReviewedActiveDigest(t *testing.T) {
	isolatedRulebook(t)
	preview, err := PreviewSelection("none", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = Select(SelectRequest{Source: "none", Selector: "console",
		ExpectedActiveDigest: "sha256:" + strings.Repeat("0", 64),
		ExpectedStateToken:   preview.ExpectedStateToken, AcknowledgedDigest: preview.ProposedDigest})
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("mismatched reviewed digest error = %T %v", err, err)
	}
}

func TestInvocationSelectionDisplacesButDoesNotEraseDurableSelection(t *testing.T) {
	home := isolatedRulebook(t)
	durable := selectPreview(t, "none", "", "cli")
	path := filepath.Join(home, "invocation.json")
	raw := []byte(`{"rules":[{"id":"invocation","action":"allow","if":{"tag":"command","matches":"x"}}]}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_RULES", path)
	loaded, err := LoadDocument()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Selection != "invocation-path" || loaded.Selector != "environment" || loaded.Displaced == nil ||
		loaded.Displaced.Digest != durable.DurableSelection.Digest {
		t.Fatalf("invocation displacement hidden: %+v", loaded)
	}
}

func TestTamperedSelectionFailsLoudlyThenRollbackArchivesRecord(t *testing.T) {
	home := isolatedRulebook(t)
	selected := selectPreview(t, "starter", "", "cli")
	if err := os.WriteFile(selected.Active.Path, []byte(`{"rules":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDocument(); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("tampered selection did not fail loudly: %v", err)
	}
	active, archive, err := RollbackLegacy(selected.Active.StateToken, "cli")
	if err != nil {
		t.Fatal(err)
	}
	if active.Selection != "legacy-implicit" || archive == "" {
		t.Fatalf("rollback result = %+v archive=%q", active, archive)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatalf("rollback evidence missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".crossing-guard", "policy", "rulebook", "selection.json")); !os.IsNotExist(err) {
		t.Fatalf("active selection record still present: %v", err)
	}
}

func TestSelectionRefusesDigestCollisionAndSymlinkedOwnerPaths(t *testing.T) {
	home := isolatedRulebook(t)
	preview, err := PreviewSelection("starter", "")
	if err != nil {
		t.Fatal(err)
	}
	path, err := selectedDocumentPath(preview.ProposedDigest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("collision"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Select(SelectRequest{Source: "starter", Selector: "cli",
		ExpectedActiveDigest: preview.ExpectedActiveDigest, ExpectedStateToken: preview.ExpectedStateToken,
		AcknowledgedDigest: preview.ProposedDigest})
	if err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("digest collision not rejected: %v", err)
	}

	if runtime.GOOS == "windows" {
		return
	}
	other := t.TempDir()
	root := filepath.Join(home, ".crossing-guard", "policy", "rulebook")
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, root); err != nil {
		t.Fatal(err)
	}
	_, err = Select(SelectRequest{Source: "starter", Selector: "cli",
		ExpectedActiveDigest: preview.ExpectedActiveDigest, ExpectedStateToken: preview.ExpectedStateToken,
		AcknowledgedDigest: preview.ProposedDigest})
	if err == nil || !strings.Contains(err.Error(), "non-directory rulebook owner path") {
		t.Fatalf("symlinked owner path not rejected: %v", err)
	}
}

func TestMissingInvocationFileDoesNotHideDurableSelection(t *testing.T) {
	home := isolatedRulebook(t)
	selectPreview(t, "none", "", "cli")
	t.Setenv("CG_RULES", filepath.Join(home, "missing.json"))
	if _, err := LoadDocument(); err == nil || !strings.Contains(err.Error(), "refusing to hide durable selection") {
		t.Fatalf("missing invocation silently fell back: %v", err)
	}
}

func TestMalformedSelectionCandidateRejectedBeforeWrite(t *testing.T) {
	home := isolatedRulebook(t)
	path := filepath.Join(home, "bad.json")
	if err := os.WriteFile(path, []byte(`{"rules":[{"id":"bad","action":"dney","if":{}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PreviewSelection("file", path); err == nil {
		t.Fatal("malformed imported selection accepted")
	}
	if _, err := os.Stat(filepath.Join(home, ".crossing-guard", "policy", "rulebook", "selection.json")); !os.IsNotExist(err) {
		t.Fatalf("rejected selection wrote a record: %v", err)
	}
}

func TestRollbackPreviewMatchesCohortUnselect(t *testing.T) {
	for _, tc := range []struct {
		name   string
		legacy bool
		custom bool
		count  int
	}{
		{"fresh", false, false, 0},
		{"legacy embedded", true, false, 8},
		{"legacy custom", true, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("CG_RULES", "")
			root := filepath.Join(home, ".crossing-guard")
			if tc.legacy {
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if tc.custom {
				path := filepath.Join(root, "policy", "rules.json")
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(`{"rules":[{"id":"custom","action":"ask","if":{"tag":"command","matches":"example"}}]}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := LoadDocument()
			if err != nil {
				t.Fatal(err)
			}
			selectPreview(t, "security-observe", "", "console")
			preview, err := PreviewSelection("legacy", "")
			if err != nil {
				t.Fatal(err)
			}
			if preview.RuleCount != tc.count || preview.ProposedDigest != before.Digest {
				t.Fatalf("rollback preview differs from original baseline: count=%d want=%d digest=%s want=%s", preview.RuleCount, tc.count, preview.ProposedDigest, before.Digest)
			}
			after, archive, err := Unselect(preview.ExpectedStateToken, "console")
			if err != nil {
				t.Fatal(err)
			}
			if archive == "" || after.Selected || after.Digest != preview.ProposedDigest || len(after.Policy.Rules) != preview.RuleCount {
				t.Fatalf("rollback disagrees with preview: archive=%s after=%+v preview=%+v", archive, after, preview)
			}
		})
	}
}
