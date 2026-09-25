package detectorselection

import (
	"crossing-guard/engine"
	"fmt"
	"os"
	"strings"

	"crossing-guard/internal/installprofile"
)

func ResolveDetectors(dataRoot string, surface DetectorSurface, invocationPath string) (*LoadedDetectors, error) {
	profile, err := installprofile.Ensure(dataRoot)
	if err != nil {
		return nil, err
	}
	starter, err := engine.DefaultDetectors()
	if err != nil {
		return nil, err
	}
	starterRaw, err := encodeDetectorDocument(starter)
	if err != nil {
		return nil, err
	}
	starterDigest := detectorDigest(starterRaw)
	profileCreated := profile.Created

	if invocationPath != "" {
		if _, err := os.Stat(invocationPath); err != nil {
			return nil, fmt.Errorf("detector invocation overlay %s: %w", invocationPath, err)
		}
		detectors, err := engine.LoadLayered(invocationPath)
		if err != nil {
			return nil, err
		}
		raw, err := encodeDetectorDocument(detectors)
		if err != nil {
			return nil, err
		}
		doc := &LoadedDetectors{Detectors: detectors, Raw: raw, Path: invocationPath,
			Origin: "invocation-overlay", Selection: "invocation-path", Digest: detectorDigest(raw),
			Active: true, Selected: true, Selector: "environment-or-flag", Surface: surface,
			InstallCohort: string(profile.Profile.Cohort), AvailableStarterDigest: starterDigest,
			InstallProfileCreated: profileCreated,
			Components: []DetectorComponent{{Role: "starter", Origin: "embedded-catalog", Digest: starterDigest, Count: len(starter)},
				{Role: "overlay", Origin: "invocation-file", Path: invocationPath, Digest: fileDigest(invocationPath), Count: detectorFileCount(invocationPath)}},
		}
		if record, recordErr := readDetectorSelection(dataRoot); recordErr != nil {
			doc.DisplacedError = recordErr.Error()
		} else if record != nil {
			info := record.DetectorSelectionInfo
			doc.Displaced = &info
		}
		return stampDetectorState(doc), nil
	}

	record, err := readDetectorSelection(dataRoot)
	if err != nil {
		return nil, err
	}
	if record != nil {
		raw, detectors, path, err := readSelectedDetectors(dataRoot, record)
		if err != nil {
			return nil, err
		}
		return stampDetectorState(&LoadedDetectors{Detectors: detectors, Raw: raw, Path: path,
			Origin: "selected-user", Selection: "explicit-user", Digest: record.Digest,
			Active: true, Selected: true, Selector: record.Selector, SelectedAt: record.SelectedAt,
			SelectedSource: record.Source, SelectedSourceRef: record.SourceRef,
			InstallCohort: string(profile.Profile.Cohort), Surface: surface,
			InstallProfileCreated:  profileCreated,
			AvailableStarterDigest: starterDigest,
			Components: []DetectorComponent{{Role: "selected-complete", Origin: "selected-user", Path: path,
				Digest: record.Digest, Count: len(detectors)}}}), nil
	}

	if profile.Profile.Cohort == installprofile.MechanismFirst {
		detectors, err := engine.StructuralDetectors()
		if err != nil {
			return nil, err
		}
		raw, err := encodeDetectorDocument(detectors)
		if err != nil {
			return nil, err
		}
		digest := detectorDigest(raw)
		return stampDetectorState(&LoadedDetectors{Detectors: detectors, Raw: raw,
			Origin: "mechanism-floor", Selection: "unselected-baseline", Digest: digest,
			Active: true, Selected: false, InstallCohort: string(profile.Profile.Cohort), Surface: surface,
			InstallProfileCreated:  profileCreated,
			AvailableStarterDigest: starterDigest,
			Components:             []DetectorComponent{{Role: "structural-floor", Origin: "embedded-framework", Digest: digest, Count: len(detectors)}}}), nil
	}

	overlayPath := detectorCompatibilityPath(dataRoot, surface)
	detectors, err := engine.LoadLayered(overlayPath)
	if err != nil {
		return nil, err
	}
	raw, err := encodeDetectorDocument(detectors)
	if err != nil {
		return nil, err
	}
	origin := "embedded-catalog"
	selection := "legacy-implicit"
	components := []DetectorComponent{{Role: "starter", Origin: origin, Digest: starterDigest, Count: len(starter)}}
	if info, statErr := os.Stat(overlayPath); statErr == nil && info.Mode().IsRegular() {
		origin = "compatibility-overlay"
		selection = "legacy-path-precedence"
		components = append(components, DetectorComponent{Role: "overlay", Origin: "legacy-file",
			Path: overlayPath, Digest: fileDigest(overlayPath), Count: detectorFileCount(overlayPath)})
	}
	return stampDetectorState(&LoadedDetectors{Detectors: detectors, Raw: raw, Path: overlayPath,
		Origin: origin, Selection: selection, Digest: detectorDigest(raw), Active: true,
		Selected: false, InstallCohort: string(profile.Profile.Cohort), Surface: surface,
		InstallProfileCreated:  profileCreated,
		AvailableStarterDigest: starterDigest, Components: components}), nil
}

func detectorFileCount(path string) int {
	detectors, err := engine.LoadDetectors(path)
	if err != nil {
		return 0
	}
	return len(detectors)
}

func stampDetectorState(doc *LoadedDetectors) *LoadedDetectors {
	doc.DetectorCount = len(doc.Detectors)
	floor, _ := engine.StructuralDetectors()
	present := map[string]bool{}
	for _, detector := range doc.Detectors {
		present[detector.ID] = true
	}
	for _, detector := range floor {
		if present[detector.ID] {
			doc.StructuralPresent = append(doc.StructuralPresent, detector.ID)
		} else {
			doc.StructuralMissing = append(doc.StructuralMissing, detector.ID)
		}
	}
	displaced := ""
	if doc.Displaced != nil {
		displaced = doc.Displaced.Digest + "|" + doc.Displaced.Source + "|" + doc.Displaced.SelectedAt
	}
	doc.StateToken = detectorDigest([]byte(strings.Join([]string{doc.Origin, doc.Selection,
		doc.Digest, doc.Selector, doc.SelectedAt, doc.SelectedSource, doc.SelectedSourceRef,
		doc.InstallCohort, string(doc.Surface), displaced, doc.DisplacedError}, "|")))
	return doc
}
