package modelroute

// File discipline for the route directory. It is profilefs's discipline restated — that
// package's helpers are unexported, and a route owner that reached into it would tie two
// owners' storage together: no symlinked component, owner-only modes, strict bounded
// JSON, and a rename as the only way a reader ever sees new bytes.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ensureRoot creates <dataDir>/models/routes one component at a time. The data
// directory itself must already exist and be a direct directory.
func (owner *Owner) ensureRoot() error {
	info, err := os.Lstat(owner.dataDir)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("daemon data root is not a direct directory")
	}
	if err := ensureDirectoryComponent(filepath.Join(owner.dataDir, "models")); err != nil {
		return err
	}
	return ensureDirectoryComponent(owner.root)
}

func ensureDirectoryComponent(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.Mkdir(path, 0o700); err != nil {
			return err
		}
		return os.Chmod(path, 0o700)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("model route owner component is not a direct directory")
	}
	return nil
}

// checkDirectoryChain verifies that every component from the data directory down to
// path is a direct directory. A missing component is fs.ErrNotExist.
func (owner *Owner) checkDirectoryChain(path string) error {
	relative, err := filepath.Rel(owner.dataDir, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return errors.New("model route path escaped its data root")
	}
	current := owner.dataDir
	parts := []string{""}
	if relative != "." {
		parts = append(parts, strings.Split(relative, string(filepath.Separator))...)
	}
	for _, part := range parts {
		if part != "" {
			current = filepath.Join(current, part)
		}
		info, statErr := os.Lstat(current)
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("model route owner component is not a direct directory")
		}
	}
	return nil
}

func readRegularFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("model route path is not a direct regular file")
	}
	if info.Size() > limit {
		return nil, errors.New("model route file exceeds its storage bound")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(io.LimitReader(file, limit+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if int64(len(body)) > limit {
		return nil, errors.New("model route file exceeds its storage bound")
	}
	return body, nil
}

func writeNewFile(path string, body []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// atomicReplace writes body to path through a synced temporary file in dir and a
// rename, so a reader sees the old bytes or the new ones, never a mix.
func atomicReplace(dir, path string, body []byte, pattern string) error {
	temporary, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
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
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return syncDirectory(dir)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return err
	}
	return directory.Close()
}

func decodeStrictJSON(body []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}
