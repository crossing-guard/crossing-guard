package rulebook

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"crossing-guard/engine"
	"crossing-guard/internal/filelock"
	"crossing-guard/ruledoc"
)

// This file contains only the rulebook selection owner's persistence mechanics.
// Selection meaning and orchestration remain in selection.go.
func rulebookRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	root := filepath.Join(home, ".crossing-guard", "policy", "rulebook")
	current := home
	for _, part := range []string{".crossing-guard", "policy", "rulebook"} {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, fs.ErrNotExist) {
			break
		}
		if statErr != nil {
			return "", statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("refusing non-directory rulebook owner path %s", current)
		}
	}
	return root, nil
}

func selectionPath() (string, error) {
	root, err := rulebookRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "selection.json"), nil
}

func selectedDocumentPath(digest string) (string, error) {
	if !digestPattern.MatchString(digest) {
		return "", fmt.Errorf("invalid rulebook digest %q", digest)
	}
	root, err := rulebookRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "documents", strings.Replace(digest, ":", "-", 1)+".json"), nil
}

func readSelection() (*selectionRecord, error) {
	path, err := selectionPath()
	if err != nil {
		return nil, err
	}
	raw, err := readRegularFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read rulebook selection: %w", err)
	}
	var record selectionRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, fmt.Errorf("parse rulebook selection: %w", err)
	}
	if record.FormatVersion != selectionFormatVersion || record.ArtifactType != "rulebook" ||
		record.OriginLayer != "user" || !digestPattern.MatchString(record.Digest) ||
		strings.TrimSpace(record.Source) == "" || strings.TrimSpace(record.Selector) == "" {
		return nil, fmt.Errorf("invalid rulebook selection record")
	}
	if _, err := time.Parse(time.RFC3339Nano, record.SelectedAt); err != nil {
		return nil, fmt.Errorf("invalid rulebook selection timestamp: %w", err)
	}
	return &record, nil
}

func readSelected(record *selectionRecord) ([]byte, *engine.Policy, string, error) {
	path, err := selectedDocumentPath(record.Digest)
	if err != nil {
		return nil, nil, "", err
	}
	raw, err := readRegularFile(path)
	if err != nil {
		return nil, nil, path, fmt.Errorf("read selected rulebook document: %w", err)
	}
	if actual := ruledoc.ContentDigest(raw); actual != record.Digest {
		return nil, nil, path, fmt.Errorf("selected rulebook digest mismatch: record %s, content %s", record.Digest, actual)
	}
	policy, err := ruledoc.Parse(raw)
	if err != nil {
		return nil, nil, path, fmt.Errorf("validate selected rulebook: %w", err)
	}
	return raw, policy, path, nil
}

func readRegularFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("refusing symlinked rulebook path %s", path)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("rulebook path is not a regular file: %s", path)
	}
	return os.ReadFile(path)
}

func ensureOwnedDir(path string) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("refusing non-directory rulebook owner path %s", path)
		}
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

func writeImmutableDocument(raw []byte, digest string) (string, error) {
	path, err := selectedDocumentPath(digest)
	if err != nil {
		return "", err
	}
	root, err := rulebookRoot()
	if err != nil {
		return "", err
	}
	if err := ensureOwnedDir(root); err != nil {
		return "", err
	}
	dir := filepath.Dir(path)
	if err := ensureOwnedDir(dir); err != nil {
		return "", err
	}
	if existing, err := readRegularFile(path); err == nil {
		if !bytes.Equal(existing, raw) || ruledoc.ContentDigest(existing) != digest {
			return "", fmt.Errorf("rulebook digest filename collision at %s", path)
		}
		return path, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".document-*.tmp")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		if existing, readErr := readRegularFile(path); readErr == nil && bytes.Equal(existing, raw) {
			return path, nil
		}
		return "", fmt.Errorf("install immutable rulebook document: %w", err)
	}
	return path, nil
}

func writeSelection(record selectionRecord) error {
	root, err := rulebookRoot()
	if err != nil {
		return err
	}
	if err := ensureOwnedDir(root); err != nil {
		return err
	}
	path := filepath.Join(root, "selection.json")
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing symlinked rulebook selection %s", path)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	raw, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(root, ".selection-*.tmp")
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

func withMutationLock(change func() error) error {
	root, err := rulebookRoot()
	if err != nil {
		return err
	}
	if err := ensureOwnedDir(root); err != nil {
		return err
	}
	return filelock.With(filepath.Join(root, "mutation.lock"), 0o600, func() error {
		if path := os.Getenv("CG_RULES"); path != "" {
			return filelock.With(path+".lock", 0o600, change)
		}
		return change()
	})
}

func withSaveMutationLock(change func() error) error {
	if path := os.Getenv("CG_RULES"); path != "" {
		return filelock.With(path+".lock", 0o600, change)
	}
	return withMutationLock(change)
}
