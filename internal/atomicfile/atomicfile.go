// Package atomicfile replaces a file so that a reader sees either the old
// bytes or the new ones, never a torn write, and so the rename survives a crash:
// the new bytes are synced before the rename and the directory after it.
package atomicfile

import (
	"io/fs"
	"os"
	"path/filepath"
)

// Write replaces path with body at mode. The temporary file lives beside the
// target, so the rename never crosses a filesystem.
func Write(path string, body []byte, mode fs.FileMode) error {
	if err := WriteNoDirSync(path, body, mode); err != nil {
		return err
	}
	return SyncDir(filepath.Dir(path))
}

// WriteNoDirSync is Write for a caller that publishes several files and syncs
// their directory once at the end.
func WriteNoDirSync(path string, body []byte, mode fs.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".atomic-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(body); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

// SyncDir makes a rename inside dir durable.
func SyncDir(dir string) error {
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close() // preserve sync error
		return err
	}
	return directory.Close()
}
