package detectorselection

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const stateKeyDetector = `{"id":"custom.state","kind":"source","match":{"tool":["Bash"]},` +
	`"tag":{"key":"session:uncommitted","value":"x"},"coverage":{"enumerable":true}}`

func wantStateNamespaceErr(t *testing.T, via string, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "state namespace") || !strings.Contains(err.Error(), "custom.state") {
		t.Fatalf("%s: err=%v, want the state-namespace rejection naming custom.state", via, err)
	}
}

// TestSelectionRejectsStateNamespaceKey: preview (and so select) of a file and an
// invocation overlay both refuse a state-namespace key.
func TestSelectionRejectsStateNamespaceKey(t *testing.T) {
	root := filepath.Join(t.TempDir(), "fresh")
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte(`{"detectors":[`+stateKeyDetector+`]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := PreviewDetectorSelection(root, DetectorSurfaceCLI, "file", bad)
	wantStateNamespaceErr(t, "preview", err)
	_, err = ResolveDetectors(root, DetectorSurfaceCLI, bad)
	wantStateNamespaceErr(t, "invocation overlay", err)
}

// TestDurableSelectionWithStateKeyFailsAndUnselectRecovers models an install that
// selected such a document before the rejection existed: resolution fails loudly
// (no silent fallback) and `unselect` is the recovery.
func TestDurableSelectionWithStateKeyFailsAndUnselectRecovers(t *testing.T) {
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
	// Write the pre-upgrade bytes through the store's own write path with a matching record.
	var doc struct {
		Detectors []json.RawMessage `json:"detectors"`
	}
	if err := json.Unmarshal(selected.Active.Raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc.Detectors = append(doc.Detectors, json.RawMessage(stateKeyDetector))
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	digest := detectorDigest(raw)
	if _, _, err := writeDetectorDocument(root, raw, digest); err != nil {
		t.Fatal(err)
	}
	record, err := readDetectorSelection(root)
	if err != nil || record == nil {
		t.Fatalf("selection record: %+v %v", record, err)
	}
	record.Digest = digest
	if err := writeDetectorSelection(root, *record); err != nil {
		t.Fatal(err)
	}

	_, err = ResolveDetectors(root, DetectorSurfaceCLI, "")
	wantStateNamespaceErr(t, "durable selection", err)
	_, err = PreviewDetectorSelection(root, DetectorSurfaceCLI, "baseline", "")
	wantStateNamespaceErr(t, "preview over an invalid active selection", err)

	token, _, err := DetectorSelectionStateToken(root, DetectorSurfaceCLI)
	if err != nil {
		t.Fatal(err)
	}
	active, archive, err := UnselectDetectors(root, DetectorSurfaceCLI, token, "cli")
	if err != nil || active.Selection != "unselected-baseline" || archive == "" {
		t.Fatalf("unselect recovery: active=%+v archive=%q err=%v", active, archive, err)
	}
}
