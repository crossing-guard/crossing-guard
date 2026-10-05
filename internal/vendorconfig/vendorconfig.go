// Package vendorconfig owns reversible byte-level replacement of vendor-owned
// configuration files. It deliberately knows nothing about hook schemas, vendors,
// consent, or attachment semantics; those remain with their existing adapters.
package vendorconfig

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Snapshot is the exact state a caller parsed before preparing a replacement.
// Exists distinguishes a missing file from a present empty one.
type Snapshot struct {
	Data   []byte
	Mode   fs.FileMode
	Exists bool
}

// Read captures a regular config file without following symlinks. Replacing a
// symlink would change config topology; callers must explicitly pass its target.
func Read(path string) (Snapshot, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Snapshot{}, nil
	}
	if err != nil {
		return Snapshot{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return Snapshot{}, fmt.Errorf("%s is a symlink; pass its resolved target explicitly", path)
	}
	if !info.Mode().IsRegular() {
		return Snapshot{}, fmt.Errorf("%s is not a regular file", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Data: raw, Mode: info.Mode().Perm(), Exists: true}, nil
}

// Replace atomically installs after only if the file still matches before. An
// existing file first receives a private immediate-prior recovery copy. The
// match is checked again just before the rename, which narrows but cannot
// close the window in which another writer (a vendor rewriting its own file)
// could change it: a check-then-rename is not a lock.
func Replace(path string, before Snapshot, after []byte) (string, error) {
	if before.Exists && bytes.Equal(before.Data, after) {
		return "", nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	current, err := Read(path)
	if err != nil {
		return "", err
	}
	if current.Exists != before.Exists || current.Mode != before.Mode ||
		!bytes.Equal(current.Data, before.Data) {
		return "", fmt.Errorf("%s changed while Crossing Guard was preparing its edit; refusing to overwrite it", path)
	}

	backup := ""
	if before.Exists {
		backup = path + ".crossing-guard.bak"
		if info, err := os.Lstat(backup); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("backup path %s is a symlink; refusing to replace it", backup)
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		if err := writeAtomic(backup, before.Data, 0o600); err != nil {
			return "", fmt.Errorf("preserve immediate-prior config at %s: %w", backup, err)
		}
	}

	mode := before.Mode
	if !before.Exists {
		mode = 0o644
	}
	unchanged := func() error {
		latest, err := Read(path)
		if err != nil {
			return err
		}
		if latest.Exists != before.Exists || latest.Mode != before.Mode || !bytes.Equal(latest.Data, before.Data) {
			return fmt.Errorf("%s changed while Crossing Guard was preparing its edit; refusing to overwrite it", path)
		}
		return nil
	}
	if err := writeAtomicChecked(path, after, mode, unchanged); err != nil {
		return "", fmt.Errorf("replace vendor config %s: %w", path, err)
	}
	return backup, nil
}

func writeAtomic(path string, raw []byte, mode fs.FileMode) error {
	return writeAtomicChecked(path, raw, mode, nil)
}

// writeAtomicChecked is writeAtomic with a last check run after the temp file
// is synced and immediately before the rename.
func writeAtomicChecked(path string, raw []byte, mode fs.FileMode, beforeRename func() error) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".crossing-guard-config-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(mode); err != nil {
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
	if beforeRename != nil {
		if err := beforeRename(); err != nil {
			return err
		}
	}
	return os.Rename(tmpPath, path)
}
