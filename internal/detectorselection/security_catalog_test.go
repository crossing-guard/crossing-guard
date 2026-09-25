package detectorselection

import (
	"path/filepath"
	"testing"
)

func TestSecuritySelectionRoundTrip(t *testing.T) {
	root := filepath.Join(t.TempDir(), "fresh")
	preview, err := PreviewDetectorSelection(root, DetectorSurfaceCLI, "security-observe", "")
	if err != nil {
		t.Fatal(err)
	}
	result, err := SelectDetectors(DetectorSelectRequest{DataRoot: root, Surface: DetectorSurfaceCLI, Source: "security-observe", Selector: "cli", ExpectedActiveDigest: preview.ExpectedActiveDigest, ExpectedStateToken: preview.ExpectedStateToken, AcknowledgedDigest: preview.ProposedDigest})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Active.Detectors) != 35 || result.Active.SelectedSource != "security-observe" {
		t.Fatal("selection did not activate security document")
	}
	if _, err := SelectDetectors(DetectorSelectRequest{DataRoot: root, Surface: DetectorSurfaceCLI, Source: "portable-floor", Selector: "cli", ExpectedActiveDigest: preview.ExpectedActiveDigest, ExpectedStateToken: preview.ExpectedStateToken, AcknowledgedDigest: preview.ProposedDigest}); err == nil {
		t.Fatal("stale selection accepted")
	}
	active, _, err := UnselectDetectors(root, DetectorSurfaceCLI, result.Active.StateToken, "cli")
	if err != nil || len(active.Detectors) != 30 {
		t.Fatalf("unselect: %v", err)
	}
}
