package rulebook

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"crossing-guard/ruledoc"
)

// InvalidDocumentError is the portable authored-policy rejection; see ruledoc.
type InvalidDocumentError = ruledoc.InvalidDocumentError

// Save preserves the current activation mode. It exists for internal migrations that
// already loaded the active base; interactive callers use SaveExpected for stale-write
// protection. A legacy edit remains legacy and never manufactures selection consent.
func Save(raw []byte) (backup string, err error) {
	active, err := LoadDocument()
	if err != nil {
		return "", err
	}
	result, err := SaveExpected(raw, active.StateToken, "migration")
	return result.Backup, err
}

// SaveExpected validates and saves only if the active digest is still the one edited.
func SaveExpected(raw []byte, expectedStateToken, selector string) (SaveResult, error) {
	var result SaveResult
	err := withSaveMutationLock(func() error {
		if err := ruledoc.Validate(raw); err != nil {
			return &InvalidDocumentError{Err: err}
		}
		active, err := LoadDocument()
		if err != nil {
			return err
		}
		if selector == "console" && active.Selection == "invocation-path" {
			return &InvocationMutationError{Path: active.Path}
		}
		if expectedStateToken != active.StateToken {
			return &ConflictError{Expected: expectedStateToken, Actual: active.StateToken}
		}
		if active.Origin == "selected-user" {
			digest := ruledoc.ContentDigest(raw)
			if _, err := writeImmutableDocument(raw, digest); err != nil {
				return err
			}
			record, err := readSelection()
			if err != nil {
				return err
			}
			if record == nil || record.Digest != active.Digest {
				actual := "none"
				if record != nil {
					actual = record.Digest
				}
				return &ConflictError{Expected: active.Digest, Actual: actual}
			}
			record.Digest = digest
			record.Source = "edited"
			record.SourceRef = active.Digest
			if selector == "cli" || selector == "console" {
				record.Selector = selector
			}
			record.SelectedAt = time.Now().UTC().Format(time.RFC3339Nano)
			if err := writeSelection(*record); err != nil {
				return err
			}
			loaded, err := LoadDocument()
			result = SaveResult{Document: loaded}
			return err
		}
		backup, err := saveDirect(active.Path, raw)
		if err != nil {
			result.Backup = backup
			return err
		}
		loaded, err := LoadDocument()
		result = SaveResult{Backup: backup, Document: loaded}
		return err
	})
	return result, err
}

func saveDirect(path string, raw []byte) (backup string, err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if previous, readErr := os.ReadFile(path); readErr == nil {
		candidate := path + ".bak"
		if writeErr := os.WriteFile(candidate, previous, 0o600); writeErr != nil {
			return "", fmt.Errorf("preserve active-rules backup: %w", writeErr)
		}
		if chmodErr := os.Chmod(candidate, 0o600); chmodErr != nil {
			return "", fmt.Errorf("protect active-rules backup: %w", chmodErr)
		}
		backup = candidate
	} else if !errors.Is(readErr, fs.ErrNotExist) {
		return "", fmt.Errorf("read active rules for backup: %w", readErr)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return backup, err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return backup, err
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return backup, err
	}
	if err := tmp.Close(); err != nil {
		return backup, err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return backup, fmt.Errorf("replace active rules: %w", err)
	}
	return backup, nil
}
