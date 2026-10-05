package detectorselection

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveDetectorsSeparatesFreshAndCompatibilityCohorts(t *testing.T) {
	parent := t.TempDir()
	freshRoot := filepath.Join(parent, "fresh")
	fresh, err := ResolveDetectors(freshRoot, DetectorSurfaceCLI, "")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Origin != "mechanism-floor" || fresh.Selection != "unselected-baseline" ||
		fresh.Selected || len(fresh.Detectors) != 30 || len(fresh.StructuralMissing) != 0 {
		t.Fatalf("fresh detectors = %+v count=%d", fresh, len(fresh.Detectors))
	}

	legacyRoot := filepath.Join(parent, "legacy")
	if err := os.Mkdir(legacyRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy, err := ResolveDetectors(legacyRoot, DetectorSurfaceCLI, "")
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Origin != "embedded-catalog" || legacy.Selection != "legacy-implicit" ||
		legacy.Selected || len(legacy.Detectors) != 70 {
		t.Fatalf("legacy detectors = %+v count=%d", legacy, len(legacy.Detectors))
	}
}

func TestCompatibilityPreservesHistoricalSurfaceDivergence(t *testing.T) {
	root := filepath.Join(t.TempDir(), "legacy")
	if err := os.MkdirAll(filepath.Join(root, "policy"), 0o755); err != nil {
		t.Fatal(err)
	}
	rootOverlay := `{"detectors":[{"id":"risk.rm-rf","disabled":true}]}`
	policyOverlay := `{"detectors":[{"id":"phase.red-team","disabled":true}]}`
	if err := os.WriteFile(filepath.Join(root, "detectors.json"), []byte(rootOverlay), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "policy", "detectors.json"), []byte(policyOverlay), 0o600); err != nil {
		t.Fatal(err)
	}
	ledger, err := ResolveDetectors(root, DetectorSurfaceLedger, "")
	if err != nil {
		t.Fatal(err)
	}
	governor, err := ResolveDetectors(root, DetectorSurfaceGovernor, "")
	if err != nil {
		t.Fatal(err)
	}
	if ledger.Digest == governor.Digest || ledger.Path == governor.Path {
		t.Fatalf("legacy divergence hidden: ledger=%s %s governor=%s %s",
			ledger.Path, ledger.Digest, governor.Path, governor.Digest)
	}
}

func TestDetectorSelectionPinsCompleteBytesAndUnselectsToCohortBaseline(t *testing.T) {
	root := filepath.Join(t.TempDir(), "fresh")
	before, err := ResolveDetectors(root, DetectorSurfaceCLI, "")
	if err != nil {
		t.Fatal(err)
	}
	preview, err := PreviewDetectorSelection(root, DetectorSurfaceCLI, "starter", "")
	if err != nil {
		t.Fatal(err)
	}
	selected, err := SelectDetectors(DetectorSelectRequest{DataRoot: root, Surface: DetectorSurfaceCLI,
		Source: "starter", Selector: "cli", ExpectedActiveDigest: preview.ExpectedActiveDigest,
		ExpectedStateToken: preview.ExpectedStateToken, AcknowledgedDigest: preview.ProposedDigest})
	if err != nil {
		t.Fatal(err)
	}
	if !selected.Active.Selected || len(selected.Active.Detectors) != 70 || selected.Active.Digest == before.Digest {
		t.Fatalf("selection = %+v", selected.Active)
	}
	active, archive, err := UnselectDetectors(root, DetectorSurfaceCLI, selected.Active.StateToken, "cli")
	if err != nil {
		t.Fatal(err)
	}
	if active.Selected || active.Digest != before.Digest || archive == "" {
		t.Fatalf("unselect active=%+v archive=%q", active, archive)
	}
}

func TestDetectorSelectionRejectsStaleInvalidAndTamperedState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "fresh")
	preview, err := PreviewDetectorSelection(root, DetectorSurfaceCLI, "starter", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = SelectDetectors(DetectorSelectRequest{DataRoot: root, Surface: DetectorSurfaceCLI,
		Source: "starter", Selector: "cli", ExpectedActiveDigest: preview.ExpectedActiveDigest,
		ExpectedStateToken: "stale", AcknowledgedDigest: preview.ProposedDigest})
	if err == nil {
		t.Fatal("stale detector selection succeeded")
	}

	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte(`{"detectors":[{"id":"x","kind":"pattern","regex":"("},{"id":"x","kind":"source"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PreviewDetectorSelection(root, DetectorSurfaceCLI, "file", bad); err == nil {
		t.Fatal("invalid complete detector document was accepted")
	}

	selected, err := SelectDetectors(DetectorSelectRequest{DataRoot: root, Surface: DetectorSurfaceCLI,
		Source: "starter", Selector: "cli", ExpectedActiveDigest: preview.ExpectedActiveDigest,
		ExpectedStateToken: preview.ExpectedStateToken, AcknowledgedDigest: preview.ProposedDigest})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(selected.Active.Path, []byte(`{"detectors":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveDetectors(root, DetectorSurfaceCLI, ""); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("tampered selection did not fail loudly: %v", err)
	}
	token, info, err := DetectorSelectionStateToken(root, DetectorSurfaceCLI)
	if err != nil || info.Digest != selected.Active.Digest {
		t.Fatalf("tampered recovery token unavailable: token=%s info=%+v err=%v", token, info, err)
	}
	active, archive, err := UnselectDetectors(root, DetectorSurfaceCLI, token, "cli")
	if err != nil || active.Selection != "unselected-baseline" || archive == "" {
		t.Fatalf("tampered selection recovery failed: active=%+v archive=%q err=%v", active, archive, err)
	}
	reviewed, err := PreviewDetectorSelection(root, DetectorSurfaceCLI, "starter", "")
	if err != nil {
		t.Fatal(err)
	}
	reselected, err := SelectDetectors(DetectorSelectRequest{DataRoot: root, Surface: DetectorSurfaceCLI,
		Source: "starter", Selector: "cli", ExpectedActiveDigest: reviewed.ExpectedActiveDigest,
		ExpectedStateToken: reviewed.ExpectedStateToken, AcknowledgedDigest: reviewed.ProposedDigest})
	if err != nil {
		t.Fatalf("reviewed reselection after tamper recovery failed: %v", err)
	}
	if reselected.RecoveredDocumentArchive == "" || reselected.Active.Digest != reviewed.ProposedDigest {
		t.Fatalf("tampered document was not visibly archived: %+v", reselected)
	}
	archivedBytes, err := os.ReadFile(reselected.RecoveredDocumentArchive)
	if err != nil || !strings.Contains(string(archivedBytes), `"detectors":[]`) {
		t.Fatalf("invalid document archive missing tampered bytes: %s %v", archivedBytes, err)
	}
}

func TestInvocationOverlayDisplacesSelectionWithoutErasingIt(t *testing.T) {
	root := filepath.Join(t.TempDir(), "fresh")
	preview, err := PreviewDetectorSelection(root, DetectorSurfaceCLI, "baseline", "")
	if err != nil {
		t.Fatal(err)
	}
	selected, err := SelectDetectors(DetectorSelectRequest{DataRoot: root, Surface: DetectorSurfaceCLI,
		Source: "baseline", Selector: "cli", ExpectedActiveDigest: preview.ExpectedActiveDigest,
		ExpectedStateToken: preview.ExpectedStateToken, AcknowledgedDigest: preview.ProposedDigest})
	if err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(t.TempDir(), "overlay.json")
	if err := os.WriteFile(overlay, []byte(`{"detectors":[{"id":"risk.rm-rf","disabled":true}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	invocation, err := ResolveDetectors(root, DetectorSurfaceCLI, overlay)
	if err != nil {
		t.Fatal(err)
	}
	if invocation.Selection != "invocation-path" || invocation.Displaced == nil ||
		invocation.Displaced.Digest != selected.Active.Digest || len(invocation.Detectors) != 69 {
		t.Fatalf("invocation displacement = %+v count=%d", invocation, len(invocation.Detectors))
	}
	if got := invocation.Components[1].Count; got != 1 {
		t.Fatalf("invocation overlay component count=%d, want exact source count 1", got)
	}
	if again, err := ResolveDetectors(root, DetectorSurfaceCLI, ""); err != nil || again.Digest != selected.Active.Digest {
		t.Fatalf("durable selection was erased: %+v %v", again, err)
	}
}

func TestDetectorSelectionRefusesSymlinkedOwnerRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "fresh")
	if _, err := ResolveDetectors(root, DetectorSurfaceCLI, ""); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "detectors")); err != nil {
		t.Fatal(err)
	}
	if _, err := PreviewDetectorSelection(root, DetectorSurfaceCLI, "starter", ""); err != nil {
		t.Fatal(err)
	}
	preview, _ := PreviewDetectorSelection(root, DetectorSurfaceCLI, "starter", "")
	_, err := SelectDetectors(DetectorSelectRequest{DataRoot: root, Surface: DetectorSurfaceCLI,
		Source: "starter", Selector: "cli", ExpectedActiveDigest: preview.ExpectedActiveDigest,
		ExpectedStateToken: preview.ExpectedStateToken, AcknowledgedDigest: preview.ProposedDigest})
	if err == nil || !strings.Contains(err.Error(), "non-directory detector owner") {
		t.Fatalf("symlinked owner was not refused: %v", err)
	}
}
