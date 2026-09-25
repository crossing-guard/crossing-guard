package detectorselection

import (
	"bytes"
	"crossing-guard/engine"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"crossing-guard/internal/filelock"
)

const detectorSelectionFormat = 1

var detectorDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func detectorDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("sha256:%x", sum)
}

func detectorOwnerRoot(dataRoot string) string { return filepath.Join(dataRoot, "detectors") }
func detectorSelectionPath(dataRoot string) string {
	return filepath.Join(detectorOwnerRoot(dataRoot), "selection.json")
}
func detectorDocumentPath(dataRoot, digest string) (string, error) {
	if !detectorDigestPattern.MatchString(digest) {
		return "", fmt.Errorf("invalid detector digest %q", digest)
	}
	return filepath.Join(detectorOwnerRoot(dataRoot), "documents", strings.Replace(digest, ":", "-", 1)+".json"), nil
}

func detectorCompatibilityPath(dataRoot string, surface DetectorSurface) string {
	if surface == DetectorSurfaceLedger {
		return filepath.Join(dataRoot, "detectors.json")
	}
	return filepath.Join(dataRoot, "policy", "detectors.json")
}

func encodeDetectorDocument(detectors []engine.Detector) ([]byte, error) {
	raw, err := json.MarshalIndent(struct {
		Detectors []engine.Detector `json:"detectors"`
	}{Detectors: detectors}, "", "  ")
	return append(raw, '\n'), err
}

func parseSelectedDetectorDocument(raw []byte) ([]engine.Detector, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var document struct {
		Comment   string            `json:"_comment,omitempty"`
		Detectors []engine.Detector `json:"detectors"`
	}
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("trailing JSON value")
		}
		return nil, err
	}
	var fields struct {
		Detectors []map[string]json.RawMessage `json:"detectors"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for i := range document.Detectors {
		detector := &document.Detectors[i]
		if i >= len(fields.Detectors) || fields.Detectors[i]["coverage"] == nil {
			return nil, fmt.Errorf("detector %s must declare coverage", detector.ID)
		}
		if strings.TrimSpace(detector.ID) == "" || seen[detector.ID] {
			return nil, fmt.Errorf("detector id is empty or duplicated: %q", detector.ID)
		}
		seen[detector.ID] = true
		if detector.Disabled {
			return nil, fmt.Errorf("complete selected detector %s cannot be disabled", detector.ID)
		}
		if detector.Evidence != "" && detector.Evidence != "raw" {
			return nil, fmt.Errorf("detector %s has invalid evidence mode %q", detector.ID, detector.Evidence)
		}
		switch detector.Kind {
		case "source":
			if len(detector.Match.Tool)+len(detector.Match.SourcePrefix)+len(detector.Match.PathPrefix) == 0 {
				return nil, fmt.Errorf("source detector %s has no match", detector.ID)
			}
		case "destination":
		case "pattern":
			if detector.Regex == "" {
				return nil, fmt.Errorf("pattern detector %s has no regex", detector.ID)
			}
		case "content":
			if len(detector.Keywords) == 0 {
				return nil, fmt.Errorf("content detector %s has no keywords", detector.ID)
			}
		default:
			return nil, fmt.Errorf("detector %s has unknown kind %q", detector.ID, detector.Kind)
		}
		if detector.Kind != "destination" && (detector.Tag.Key == "" || detector.Tag.Value == "") {
			return nil, fmt.Errorf("detector %s must declare a non-empty tag", detector.ID)
		}
	}
	if err := engine.CompileDetectors(document.Detectors); err != nil {
		return nil, err
	}
	return document.Detectors, nil
}

func withDetectorLock(dataRoot string, change func() error) error {
	if err := ensureDetectorOwnerRoot(dataRoot); err != nil {
		return err
	}
	return filelock.With(filepath.Join(detectorOwnerRoot(dataRoot), "selection.lock"), 0o600, change)
}

func ensureDetectorOwnerRoot(dataRoot string) error {
	root := detectorOwnerRoot(dataRoot)
	info, err := os.Lstat(root)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.Mkdir(root, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		info, err = os.Lstat(root)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("refusing non-directory detector owner path %s", root)
	}
	return os.Chmod(root, 0o700)
}

func readDetectorSelection(dataRoot string) (*detectorSelectionRecord, error) {
	raw, err := readDetectorRegularFile(detectorSelectionPath(dataRoot))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var record detectorSelectionRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, err
	}
	if record.FormatVersion != detectorSelectionFormat || record.ArtifactType != "detectors" ||
		record.OriginLayer != "user" || !detectorDigestPattern.MatchString(record.Digest) ||
		record.Source == "" || (record.Selector != "cli" && record.Selector != "console") {
		return nil, errors.New("invalid detector selection record")
	}
	if _, err := time.Parse(time.RFC3339Nano, record.SelectedAt); err != nil {
		return nil, err
	}
	return &record, nil
}

func readSelectedDetectors(dataRoot string, record *detectorSelectionRecord) ([]byte, []engine.Detector, string, error) {
	path, err := detectorDocumentPath(dataRoot, record.Digest)
	if err != nil {
		return nil, nil, "", err
	}
	raw, err := readDetectorRegularFile(path)
	if err != nil {
		return nil, nil, path, err
	}
	if digest := detectorDigest(raw); digest != record.Digest {
		return nil, nil, path, fmt.Errorf("selected detector digest mismatch: record %s, content %s", record.Digest, digest)
	}
	detectors, err := parseSelectedDetectorDocument(raw)
	return raw, detectors, path, err
}

func readDetectorRegularFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("refusing non-regular detector file %s", path)
	}
	return os.ReadFile(path)
}

func ensureDetectorDir(path string) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("refusing non-directory detector owner path %s", path)
		}
		return os.Chmod(path, 0o700)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

func writeDetectorDocument(dataRoot string, raw []byte, digest string) (string, string, error) {
	path, err := detectorDocumentPath(dataRoot, digest)
	if err != nil {
		return "", "", err
	}
	if err := ensureDetectorDir(filepath.Dir(path)); err != nil {
		return "", "", err
	}
	recoveredArchive := ""
	if existing, err := readDetectorRegularFile(path); err == nil {
		if !bytes.Equal(existing, raw) {
			history := filepath.Join(detectorOwnerRoot(dataRoot), "history")
			if err := ensureDetectorDir(history); err != nil {
				return "", "", err
			}
			stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
			recoveredArchive = filepath.Join(history, "invalid-document-"+stamp+"-"+filepath.Base(path))
			if err := os.Rename(path, recoveredArchive); err != nil {
				return "", "", fmt.Errorf("archive invalid detector document: %w", err)
			}
		} else {
			return path, "", nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", "", err
	}
	if err := writeDetectorAtomic(path, raw); err != nil {
		return "", recoveredArchive, err
	}
	return path, recoveredArchive, nil
}

func writeDetectorSelection(dataRoot string, record detectorSelectionRecord) error {
	if err := ensureDetectorDir(detectorOwnerRoot(dataRoot)); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return writeDetectorAtomic(detectorSelectionPath(dataRoot), append(raw, '\n'))
}

func writeDetectorAtomic(path string, raw []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".detectors-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func fileDigest(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "unavailable"
	}
	return detectorDigest(raw)
}
