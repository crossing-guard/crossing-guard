package analyzermodule

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const packageDirectoryMode = 0o700

// Root returns the one analyzer lifecycle root for a Crossing Guard data directory.
func Root(dataDir string) string { return filepath.Join(dataDir, "analyzers") }

func packagesRoot(dataDir string) string { return filepath.Join(Root(dataDir), "packages") }

func packageDirectory(dataDir, digest string) string {
	return filepath.Join(packagesRoot(dataDir), strings.ReplaceAll(digest, ":", "-"))
}

// Remove deletes one exact unselected installed package. Selection must be changed
// first so removal cannot silently alter the active assembly on restart.
func Remove(dataDir, reference string) error {
	installed, err := Find(dataDir, reference)
	if err != nil {
		return err
	}
	selection, err := LoadSelection(dataDir)
	if err != nil {
		return err
	}
	for _, selected := range selection.Modules {
		if selected.PackageDigest == installed.PackageDigest {
			return fmt.Errorf("analyzer package %s is selected; deselect it before removal", reference)
		}
	}
	if packageDirectory(dataDir, installed.PackageDigest) != installed.Dir {
		return fmt.Errorf("analyzer removal requires exact content-addressed storage")
	}
	if err := os.RemoveAll(installed.Dir); err != nil {
		return fmt.Errorf("remove analyzer package: %w", err)
	}
	return nil
}

// Install copies one validated foreign package into immutable content-addressed local
// storage. It does not execute or select the package.
func Install(dataDir, sourceDirectory string) (Package, error) {
	source, err := Inspect(sourceDirectory)
	if err != nil {
		return Package{}, err
	}
	root := packagesRoot(dataDir)
	if err := os.MkdirAll(root, packageDirectoryMode); err != nil {
		return Package{}, fmt.Errorf("create analyzer package root: %w", err)
	}
	if err := os.Chmod(Root(dataDir), packageDirectoryMode); err != nil {
		return Package{}, fmt.Errorf("protect analyzer lifecycle root: %w", err)
	}
	if err := os.Chmod(root, packageDirectoryMode); err != nil {
		return Package{}, fmt.Errorf("protect analyzer package root: %w", err)
	}
	destination := packageDirectory(dataDir, source.PackageDigest)
	if installed, err := Inspect(destination); err == nil {
		if installed.PackageDigest != source.PackageDigest {
			return Package{}, fmt.Errorf("installed analyzer digest directory contains different bytes")
		}
		return installed, nil
	} else if !errors.Is(err, fs.ErrNotExist) && pathExists(destination) {
		return Package{}, fmt.Errorf("inspect existing analyzer package: %w", err)
	}
	temporary, err := os.MkdirTemp(root, ".install-*")
	if err != nil {
		return Package{}, fmt.Errorf("create analyzer install staging directory: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(temporary)
		}
	}()
	if err := os.Chmod(temporary, packageDirectoryMode); err != nil {
		return Package{}, fmt.Errorf("protect analyzer staging directory: %w", err)
	}
	if err := copyPackage(source.Dir, temporary); err != nil {
		return Package{}, err
	}
	staged, err := Inspect(temporary)
	if err != nil {
		return Package{}, fmt.Errorf("validate staged analyzer package: %w", err)
	}
	if staged.PackageDigest != source.PackageDigest || staged.EntrypointDigest != source.EntrypointDigest {
		return Package{}, fmt.Errorf("staged analyzer package digest changed during copy")
	}
	if err := syncDirectory(temporary); err != nil {
		return Package{}, fmt.Errorf("sync staged analyzer package: %w", err)
	}
	if err := os.Rename(temporary, destination); err != nil {
		if installed, inspectErr := Inspect(destination); inspectErr == nil && installed.PackageDigest == source.PackageDigest {
			return installed, nil
		}
		return Package{}, fmt.Errorf("publish analyzer package: %w", err)
	}
	cleanup = false
	if err := syncDirectory(root); err != nil {
		return Package{}, fmt.Errorf("sync analyzer package catalog: %w", err)
	}
	return Inspect(destination)
}

// List returns every valid installed package plus explicit errors for corrupt entries.
func List(dataDir string) ([]Package, []error) {
	entries, err := os.ReadDir(packagesRoot(dataDir))
	if errors.Is(err, fs.ErrNotExist) {
		return []Package{}, []error{}
	}
	if err != nil {
		return []Package{}, []error{fmt.Errorf("read analyzer package catalog: %w", err)}
	}
	packages := []Package{}
	errorsFound := []error{}
	for _, entry := range entries {
		path := filepath.Join(packagesRoot(dataDir), entry.Name())
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			errorsFound = append(errorsFound, fmt.Errorf("analyzer catalog entry is not a directory: %s", path))
			continue
		}
		installed, inspectErr := Inspect(path)
		if inspectErr != nil {
			errorsFound = append(errorsFound, fmt.Errorf("inspect analyzer catalog entry %s: %w", path, inspectErr))
			continue
		}
		if packageDirectory(dataDir, installed.PackageDigest) != path {
			errorsFound = append(errorsFound, fmt.Errorf("analyzer package is stored under the wrong digest path: %s", path))
			continue
		}
		packages = append(packages, installed)
	}
	sort.Slice(packages, func(i, j int) bool {
		if packages[i].Manifest.ModuleID != packages[j].Manifest.ModuleID {
			return packages[i].Manifest.ModuleID < packages[j].Manifest.ModuleID
		}
		return packages[i].PackageDigest < packages[j].PackageDigest
	})
	return packages, errorsFound
}

// Find returns one exact installed module reference in module-id@package-digest form.
func Find(dataDir, reference string) (Package, error) {
	moduleID, digest, ok := strings.Cut(strings.TrimSpace(reference), "@")
	if !ok || moduleID == "" || digest == "" {
		return Package{}, fmt.Errorf("analyzer reference must be module-id@sha256-v1:<digest>")
	}
	installed, err := Inspect(packageDirectory(dataDir, digest))
	if err != nil {
		return Package{}, fmt.Errorf("find analyzer package: %w", err)
	}
	if installed.Manifest.ModuleID != moduleID || installed.PackageDigest != digest {
		return Package{}, fmt.Errorf("analyzer reference does not match installed package")
	}
	return installed, nil
}

func copyPackage(source, destination string) error {
	return filepath.WalkDir(source, func(path string, item fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == source {
			return nil
		}
		rel, err := filepath.Rel(source, path)
		if err != nil || !safeRelativePath(filepath.ToSlash(rel)) {
			return fmt.Errorf("copy analyzer package unsafe path %s", path)
		}
		info, err := item.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("copy analyzer package unsupported file %s", path)
		}
		target := filepath.Join(destination, rel)
		if info.IsDir() {
			return os.Mkdir(target, packageDirectoryMode)
		}
		mode := fs.FileMode(0o600)
		if info.Mode().Perm()&0o111 != 0 {
			mode = 0o700
		}
		return copyRegularFile(path, target, mode)
	})
}

func copyRegularFile(source, destination string, mode fs.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open analyzer package source: %w", err)
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		_ = input.Close() // preserve destination creation error
		return fmt.Errorf("create analyzer package destination: %w", err)
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return fmt.Errorf("copy analyzer package file: %w", err)
	}
	if err := output.Sync(); err != nil {
		_ = output.Close()
		return fmt.Errorf("sync analyzer package file: %w", err)
	}
	if err := output.Close(); err != nil {
		_ = input.Close() // preserve output close error
		return fmt.Errorf("close analyzer package file: %w", err)
	}
	if err := input.Close(); err != nil {
		return fmt.Errorf("close analyzer package source: %w", err)
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close() // preserve sync error
		return err
	}
	return directory.Close()
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil || !errors.Is(err, fs.ErrNotExist)
}
