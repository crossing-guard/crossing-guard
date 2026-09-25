// Package filelock provides only cross-process advisory locking around a file path.
// Callers retain all meaning, validation, and persistence ownership.
package filelock

import (
	"errors"
	"os"
	"path/filepath"
)

func With(path string, mode os.FileMode, change func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, mode)
	if err != nil {
		return err
	}
	if err := lock.Chmod(mode); err != nil {
		return errors.Join(err, lock.Close())
	}
	if err := lockFile(lock); err != nil {
		return errors.Join(err, lock.Close())
	}
	changeErr := change()
	unlockErr := unlockFile(lock)
	closeErr := lock.Close()
	return errors.Join(changeErr, unlockErr, closeErr)
}
