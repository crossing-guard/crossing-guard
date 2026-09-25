package taskinput

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type fileSystem struct{ root string }

func openFileSystem(root string) (fileSystem, error) {
	root = filepath.Clean(root)
	if !filepath.IsAbs(root) {
		return fileSystem{}, fmt.Errorf("task input root must be absolute")
	}
	if info, err := os.Lstat(root); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fileSystem{}, fmt.Errorf("task input root must be a real directory")
		}
	} else if !os.IsNotExist(err) {
		return fileSystem{}, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fileSystem{}, fmt.Errorf("create task input root: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return fileSystem{}, fmt.Errorf("protect task input root: %w", err)
	}
	for _, name := range []string{"scopes", "incoming"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o700); err != nil {
			return fileSystem{}, err
		}
	}
	return fileSystem{root: root}, nil
}

func (fs fileSystem) scopeDir(scopeID string) string {
	return filepath.Join(fs.root, "scopes", scopeID)
}
func (fs fileSystem) manifestPath(scopeID string) string {
	return filepath.Join(fs.scopeDir(scopeID), "manifest.json")
}
func (fs fileSystem) originalPath(scopeID, inputID string) string {
	return filepath.Join(fs.scopeDir(scopeID), "original", inputID)
}
func (fs fileSystem) preparedPath(scopeID, inputID, extension string) string {
	return filepath.Join(fs.scopeDir(scopeID), "prepared", inputID+extension)
}

func (fs fileSystem) createScopeDirs(scopeID string) error {
	for _, name := range []string{"", "original", "prepared"} {
		if err := os.MkdirAll(filepath.Join(fs.scopeDir(scopeID), name), 0o700); err != nil {
			return err
		}
	}
	return nil
}

func (fs fileSystem) tempFile() (*os.File, error) {
	file, err := os.CreateTemp(filepath.Join(fs.root, "incoming"), "input-*")
	if err == nil {
		err = file.Chmod(0o600)
	}
	if err != nil && file != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
	}
	return file, err
}

func (fs fileSystem) writeManifest(record scopeRecord) error {
	raw, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	temp, err := os.CreateTemp(fs.scopeDir(record.ID), ".manifest-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(raw); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(name, fs.manifestPath(record.ID))
}

func (fs fileSystem) readManifest(scopeID string) (scopeRecord, error) {
	raw, err := os.ReadFile(fs.manifestPath(scopeID))
	if err != nil {
		return scopeRecord{}, err
	}
	var record scopeRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return scopeRecord{}, fmt.Errorf("decode scope manifest: %w", err)
	}
	if record.ID != scopeID {
		return scopeRecord{}, fmt.Errorf("scope manifest identity mismatch")
	}
	return record, nil
}
