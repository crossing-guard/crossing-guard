package detectorselection

import (
	"bytes"
	"crossing-guard/engine"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"crossing-guard/internal/installprofile"
)

type DetectorSurface string

const (
	DetectorSurfaceCLI      DetectorSurface = "cli-hook-governor"
	DetectorSurfaceLedger   DetectorSurface = "daemon-ledger-audit"
	DetectorSurfaceGovernor DetectorSurface = "daemon-governor"
)

type DetectorSelectionInfo struct {
	Digest     string `json:"digest"`
	Source     string `json:"source"`
	SourceRef  string `json:"source_ref,omitempty"`
	Selector   string `json:"selector"`
	SelectedAt string `json:"selected_at"`
}

type detectorSelectionRecord struct {
	FormatVersion int    `json:"format_version"`
	ArtifactType  string `json:"artifact_type"`
	OriginLayer   string `json:"origin_layer"`
	DetectorSelectionInfo
}

type DetectorComponent struct {
	Role   string `json:"role"`
	Origin string `json:"origin"`
	Path   string `json:"path,omitempty"`
	Digest string `json:"digest"`
	Count  int    `json:"count"`
}

type LoadedDetectors struct {
	Detectors              []engine.Detector      `json:"-"`
	Raw                    []byte                 `json:"-"`
	Path                   string                 `json:"path,omitempty"`
	Origin                 string                 `json:"origin"`
	Selection              string                 `json:"selection"`
	Digest                 string                 `json:"digest"`
	DetectorCount          int                    `json:"detector_count"`
	StateToken             string                 `json:"state_token"`
	Active                 bool                   `json:"active"`
	Selected               bool                   `json:"selected"`
	Selector               string                 `json:"selector,omitempty"`
	SelectedAt             string                 `json:"selected_at,omitempty"`
	SelectedSource         string                 `json:"selected_source,omitempty"`
	SelectedSourceRef      string                 `json:"selected_source_ref,omitempty"`
	InstallCohort          string                 `json:"install_cohort"`
	InstallProfileCreated  bool                   `json:"install_profile_created,omitempty"`
	Surface                DetectorSurface        `json:"surface"`
	AvailableStarterDigest string                 `json:"available_starter_digest"`
	StructuralPresent      []string               `json:"structural_present"`
	StructuralMissing      []string               `json:"structural_missing"`
	Components             []DetectorComponent    `json:"components"`
	Displaced              *DetectorSelectionInfo `json:"displaced_selection,omitempty"`
	DisplacedError         string                 `json:"displaced_selection_error,omitempty"`
}

type DetectorPreview struct {
	Source               string   `json:"source"`
	SourcePath           string   `json:"source_path,omitempty"`
	Surface              string   `json:"surface"`
	ExpectedActiveDigest string   `json:"expected_active_digest"`
	ExpectedStateToken   string   `json:"expected_state_token"`
	ProposedDigest       string   `json:"proposed_digest"`
	Added                []string `json:"added"`
	Removed              []string `json:"removed"`
	Replaced             []string `json:"replaced"`
	DetectorCount        int      `json:"detector_count"`
	SemanticReach        string   `json:"semantic_reach"`
	raw                  []byte
}

type DetectorSelectRequest struct {
	DataRoot             string          `json:"-"`
	Surface              DetectorSurface `json:"surface"`
	Source               string          `json:"source"`
	Path                 string          `json:"path,omitempty"`
	Selector             string          `json:"selector"`
	ExpectedActiveDigest string          `json:"expected_active_digest"`
	ExpectedStateToken   string          `json:"expected_state_token"`
	AcknowledgedDigest   string          `json:"acknowledged_digest"`
}

type DetectorSelectionResult struct {
	Preview                  DetectorPreview       `json:"preview"`
	DurableSelection         DetectorSelectionInfo `json:"durable_selection"`
	Active                   *LoadedDetectors      `json:"active"`
	RecoveredDocumentArchive string                `json:"recovered_document_archive,omitempty"`
}

type DetectorConflictError struct{ Field, Expected, Actual string }

func (e *DetectorConflictError) Error() string {
	field := e.Field
	if field == "" {
		field = "state"
	}
	return fmt.Sprintf("active detector %s changed: expected %s, now %s; reload and review", field, e.Expected, e.Actual)
}

func PreviewDetectorSelection(dataRoot string, surface DetectorSurface, source, path string) (DetectorPreview, error) {
	active, err := ResolveDetectors(dataRoot, surface, "")
	if err != nil {
		return DetectorPreview{}, err
	}
	raw, sourcePath, err := detectorCandidate(active, source, path)
	if err != nil {
		return DetectorPreview{}, err
	}
	candidate, err := parseSelectedDetectorDocument(raw)
	if err != nil {
		return DetectorPreview{}, err
	}
	canonical, err := encodeDetectorDocument(candidate)
	if err != nil {
		return DetectorPreview{}, err
	}
	added, removed, replaced := detectorDiff(active.Detectors, candidate)
	reach := "not enumerable — patterns require representative real inputs"
	if source == "security-observe" {
		reach += "; credential patterns can miss secrets; unlisted destinations are external; detector selection alone selects no rules and blocks no egress"
	}
	return DetectorPreview{Source: source, SourcePath: sourcePath, Surface: string(surface),
		ExpectedActiveDigest: active.Digest, ExpectedStateToken: active.StateToken,
		ProposedDigest: detectorDigest(canonical), Added: added, Removed: removed, Replaced: replaced,
		DetectorCount: len(candidate), SemanticReach: reach, raw: canonical}, nil
}

func detectorCandidate(active *LoadedDetectors, source, path string) ([]byte, string, error) {
	switch source {
	case "current":
		return append([]byte(nil), active.Raw...), active.Path, nil
	case "starter", "legacy":
		detectors, err := engine.DefaultDetectors()
		raw, encodeErr := encodeDetectorDocument(detectors)
		if source == "legacy" {
			return raw, "embedded legacy compatibility catalog", errors.Join(err, encodeErr)
		}
		return raw, "embedded starter catalog", errors.Join(err, encodeErr)
	case "baseline", "portable-floor":
		detectors, err := engine.StructuralDetectors()
		raw, encodeErr := encodeDetectorDocument(detectors)
		return raw, "embedded structural baseline", errors.Join(err, encodeErr)
	case "security-observe":
		detectors, err := engine.SecurityObserveDetectors()
		raw, encodeErr := encodeDetectorDocument(detectors)
		return raw, "embedded security observation catalog", errors.Join(err, encodeErr)
	case "file":
		if strings.TrimSpace(path) == "" {
			return nil, "", errors.New("detector selection file path is required")
		}
		raw, err := readDetectorRegularFile(path)
		return raw, path, err
	default:
		return nil, "", fmt.Errorf("unknown detector selection source %q", source)
	}
}

func SelectDetectors(request DetectorSelectRequest) (DetectorSelectionResult, error) {
	if request.Selector != "cli" && request.Selector != "console" {
		return DetectorSelectionResult{}, errors.New("selector must be cli or console")
	}
	var result DetectorSelectionResult
	err := withDetectorLock(request.DataRoot, func() error {
		preview, err := PreviewDetectorSelection(request.DataRoot, request.Surface, request.Source, request.Path)
		if err != nil {
			return err
		}
		if request.ExpectedActiveDigest != preview.ExpectedActiveDigest {
			return &DetectorConflictError{Field: "digest", Expected: request.ExpectedActiveDigest, Actual: preview.ExpectedActiveDigest}
		}
		if request.ExpectedStateToken != preview.ExpectedStateToken {
			return &DetectorConflictError{Field: "state token", Expected: request.ExpectedStateToken, Actual: preview.ExpectedStateToken}
		}
		if request.AcknowledgedDigest != preview.ProposedDigest {
			return fmt.Errorf("acknowledged detector digest %q does not match proposed %s", request.AcknowledgedDigest, preview.ProposedDigest)
		}
		_, recoveredArchive, err := writeDetectorDocument(request.DataRoot, preview.raw, preview.ProposedDigest)
		if err != nil {
			return err
		}
		info := DetectorSelectionInfo{Digest: preview.ProposedDigest, Source: request.Source,
			SourceRef: preview.SourcePath, Selector: request.Selector,
			SelectedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		record := detectorSelectionRecord{FormatVersion: detectorSelectionFormat,
			ArtifactType: "detectors", OriginLayer: "user", DetectorSelectionInfo: info}
		if err := writeDetectorSelection(request.DataRoot, record); err != nil {
			return err
		}
		active, err := ResolveDetectors(request.DataRoot, request.Surface, "")
		if err != nil {
			return fmt.Errorf("detector selection written but active proof failed: %w", err)
		}
		result = DetectorSelectionResult{Preview: preview, DurableSelection: info, Active: active,
			RecoveredDocumentArchive: recoveredArchive}
		return nil
	})
	return result, err
}

func UnselectDetectors(dataRoot string, surface DetectorSurface, expectedStateToken, selector string) (*LoadedDetectors, string, error) {
	if selector != "cli" && selector != "console" {
		return nil, "", errors.New("selector must be cli or console")
	}
	var active *LoadedDetectors
	var archive string
	err := withDetectorLock(dataRoot, func() error {
		currentToken, _, err := DetectorSelectionStateToken(dataRoot, surface)
		if err != nil {
			return err
		}
		if currentToken != expectedStateToken {
			return &DetectorConflictError{Field: "state token", Expected: expectedStateToken, Actual: currentToken}
		}
		history := filepath.Join(detectorOwnerRoot(dataRoot), "history")
		if err := ensureDetectorDir(history); err != nil {
			return err
		}
		stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
		archive = filepath.Join(history, "selection-"+stamp+"-"+selector+".json")
		if err := os.Rename(detectorSelectionPath(dataRoot), archive); err != nil {
			return err
		}
		active, err = ResolveDetectors(dataRoot, surface, "")
		return err
	})
	return active, archive, err
}

// DetectorSelectionStateToken returns the durable selection's compare-and-swap token
// without opening its selected document. That makes explicit unselect possible when the
// selected bytes are missing or tampered; it never makes those bytes active.
func DetectorSelectionStateToken(dataRoot string, surface DetectorSurface) (string, *DetectorSelectionInfo, error) {
	record, err := readDetectorSelection(dataRoot)
	if err != nil {
		return "", nil, err
	}
	if record == nil {
		return "", nil, errors.New("no durable detector selection to unselect")
	}
	profile, err := installprofile.Ensure(dataRoot)
	if err != nil {
		return "", nil, err
	}
	doc := stampDetectorState(&LoadedDetectors{Origin: "selected-user", Selection: "explicit-user",
		Digest: record.Digest, Selected: true, Selector: record.Selector, SelectedAt: record.SelectedAt,
		SelectedSource: record.Source, SelectedSourceRef: record.SourceRef,
		InstallCohort: string(profile.Profile.Cohort), Surface: surface})
	info := record.DetectorSelectionInfo
	return doc.StateToken, &info, nil
}

func detectorDiff(before, after []engine.Detector) (added, removed, replaced []string) {
	beforeByID, afterByID := map[string]engine.Detector{}, map[string]engine.Detector{}
	for _, detector := range before {
		beforeByID[detector.ID] = detector
	}
	for _, detector := range after {
		afterByID[detector.ID] = detector
	}
	for id, old := range beforeByID {
		newDetector, ok := afterByID[id]
		if !ok {
			removed = append(removed, id)
			continue
		}
		oldRaw, _ := json.Marshal(old)
		newRaw, _ := json.Marshal(newDetector)
		if !bytes.Equal(oldRaw, newRaw) {
			replaced = append(replaced, id)
		}
	}
	for id := range afterByID {
		if _, ok := beforeByID[id]; !ok {
			added = append(added, id)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(replaced)
	return
}
