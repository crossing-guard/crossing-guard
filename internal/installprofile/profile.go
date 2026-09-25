// Package installprofile owns the create-once framework fact that distinguishes a
// data root which predates the mechanism-first cutover from one created by it. It
// deliberately knows nothing about rules, detectors, packs, or runtime attachment.
package installprofile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"crossing-guard/internal/filelock"
)

const FormatVersion = 1

type Cohort string

const (
	Compatibility  Cohort = "pre-c5c-compatibility"
	MechanismFirst Cohort = "c5c-mechanism-first"
)

type Profile struct {
	FormatVersion int    `json:"format_version"`
	Cohort        Cohort `json:"cohort"`
	Basis         string `json:"basis"`
	FirstSeenAt   string `json:"first_seen_at"`
}

type Result struct {
	Profile Profile
	Path    string
	Created bool
}

func Path(root string) string { return filepath.Join(root, "installation.json") }

func bootstrapLockPath(root string) string {
	clean := filepath.Clean(root)
	return filepath.Join(filepath.Dir(clean), "."+filepath.Base(clean)+".installation.lock")
}

// Ensure classifies and records one operational data root. The adjacent lock is
// intentional: creating a lock inside root would destroy the absent/present fact
// before it was observed.
func Ensure(root string) (Result, error) {
	if root == "" {
		return Result{}, errors.New("installation data root is required")
	}
	root = filepath.Clean(root)
	var result Result
	err := filelock.With(bootstrapLockPath(root), 0o600, func() error {
		profile, err := read(Path(root))
		if err == nil {
			result = Result{Profile: profile, Path: Path(root)}
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}

		cohort, basis, err := classifyRoot(root)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(root, 0o755); err != nil {
			return fmt.Errorf("create installation data root: %w", err)
		}
		profile = Profile{FormatVersion: FormatVersion, Cohort: cohort, Basis: basis,
			FirstSeenAt: time.Now().UTC().Format(time.RFC3339Nano)}
		if err := write(Path(root), profile); err != nil {
			return err
		}
		result = Result{Profile: profile, Path: Path(root), Created: true}
		return nil
	})
	return result, err
}

// Preview reports what Ensure would create without touching the filesystem.
func Preview(root string) (Profile, error) {
	if root == "" {
		return Profile{}, errors.New("installation data root is required")
	}
	if profile, err := read(Path(root)); err == nil {
		return profile, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Profile{}, err
	}
	cohort, basis, err := classifyRoot(filepath.Clean(root))
	return Profile{FormatVersion: FormatVersion, Cohort: cohort, Basis: basis}, err
}

func classifyRoot(root string) (Cohort, string, error) {
	info, err := os.Lstat(root)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return MechanismFirst, "data-root-created-by-c5c", nil
	case err != nil:
		return "", "", fmt.Errorf("inspect installation data root: %w", err)
	case info.Mode()&os.ModeSymlink != 0:
		return "", "", fmt.Errorf("installation data root must not be a symlink: %s", root)
	case !info.IsDir():
		return "", "", fmt.Errorf("installation data root is not a directory: %s", root)
	default:
		return Compatibility, "data-root-preexisting", nil
	}
}

func read(path string) (Profile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Profile{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var profile Profile
	if err := decoder.Decode(&profile); err != nil {
		return Profile{}, fmt.Errorf("parse installation profile %s: %w", path, err)
	}
	if err := requireEOF(decoder); err != nil {
		return Profile{}, fmt.Errorf("parse installation profile %s: %w", path, err)
	}
	if err := validate(profile); err != nil {
		return Profile{}, fmt.Errorf("invalid installation profile %s: %w", path, err)
	}
	return profile, nil
}

func requireEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("trailing JSON value")
}

func validate(profile Profile) error {
	if profile.FormatVersion != FormatVersion {
		return fmt.Errorf("format_version=%d, want %d", profile.FormatVersion, FormatVersion)
	}
	if profile.Cohort != Compatibility && profile.Cohort != MechanismFirst {
		return fmt.Errorf("unknown cohort %q", profile.Cohort)
	}
	validBasis := profile.Basis == "data-root-preexisting" || profile.Basis == "data-root-created-by-c5c"
	if !validBasis {
		return fmt.Errorf("unknown basis %q", profile.Basis)
	}
	if (profile.Cohort == Compatibility) != (profile.Basis == "data-root-preexisting") {
		return errors.New("cohort and basis disagree")
	}
	if _, err := time.Parse(time.RFC3339Nano, profile.FirstSeenAt); err != nil {
		return fmt.Errorf("invalid first_seen_at: %w", err)
	}
	return nil
}

func write(path string, profile Profile) error {
	raw, err := json.MarshalIndent(profile, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".installation-*")
	if err != nil {
		return fmt.Errorf("create installation profile temp file: %w", err)
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
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("install installation profile: %w", err)
	}
	return nil
}
