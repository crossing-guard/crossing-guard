package analyzermodule

import (
	"crossing-guard/internal/atomicfile"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const selectionFormatV1 = "crossing-guard-analyzer-selection-v1"
const maxSelectedModules = 8

// SelectedModule pins one exact installed module package.
type SelectedModule struct {
	ModuleID      string `json:"module_id"`
	PackageDigest string `json:"package_digest"`
}

// Selection is the one user-scope executable analyzer selection.
type Selection struct {
	FormatVersion string           `json:"format_version"`
	SelectedAt    string           `json:"selected_at"`
	SelectedBy    string           `json:"selected_by"`
	Modules       []SelectedModule `json:"modules"`
}

// LoadSelection reads exact selected digests. Missing selection is an empty valid state;
// malformed selection is an error and never falls back to another process module.
func LoadSelection(dataDir string) (Selection, error) {
	path := filepath.Join(Root(dataDir), "selection.json")
	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Selection{FormatVersion: selectionFormatV1, Modules: []SelectedModule{}}, nil
	}
	if err != nil {
		return Selection{}, fmt.Errorf("open analyzer selection: %w", err)
	}
	decoder := json.NewDecoder(io.LimitReader(file, maxManifestBytes+1))
	decoder.DisallowUnknownFields()
	var selection Selection
	if err := decoder.Decode(&selection); err != nil {
		_ = file.Close() // preserve decode error
		return selection, fmt.Errorf("decode analyzer selection: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		_ = file.Close() // preserve decode error
		return selection, fmt.Errorf("decode analyzer selection: %w", err)
	}
	if err := file.Close(); err != nil {
		return selection, fmt.Errorf("close analyzer selection: %w", err)
	}
	if err := validateSelection(selection); err != nil {
		return selection, err
	}
	return selection, nil
}

// Select adds or explicitly replaces one module and atomically pins exact bytes.
func Select(dataDir string, installed Package, replaceModuleID, selectedBy string, now time.Time) (Selection, error) {
	verified, err := Inspect(installed.Dir)
	if err != nil || verified.PackageDigest != installed.PackageDigest {
		return Selection{}, fmt.Errorf("select analyzer package requires exact installed bytes")
	}
	if packageDirectory(dataDir, installed.PackageDigest) != installed.Dir {
		return Selection{}, fmt.Errorf("select analyzer package requires content-addressed installed storage")
	}
	selection, err := LoadSelection(dataDir)
	if err != nil {
		return Selection{}, err
	}
	next := make([]SelectedModule, 0, len(selection.Modules)+1)
	foundReplace := replaceModuleID == ""
	for _, current := range selection.Modules {
		if current.ModuleID == installed.Manifest.ModuleID || current.ModuleID == replaceModuleID {
			if current.ModuleID == replaceModuleID {
				foundReplace = true
			}
			continue
		}
		next = append(next, current)
	}
	if !foundReplace {
		return Selection{}, fmt.Errorf("selected analyzer replacement %q was not active", replaceModuleID)
	}
	next = append(next, SelectedModule{ModuleID: installed.Manifest.ModuleID,
		PackageDigest: installed.PackageDigest})
	resolved, err := resolveModules(dataDir, next)
	if err != nil {
		return Selection{}, err
	}
	if err := validateClaimConflicts(resolved); err != nil {
		return Selection{}, err
	}
	sort.Slice(next, func(i, j int) bool { return next[i].ModuleID < next[j].ModuleID })
	selection = Selection{FormatVersion: selectionFormatV1, SelectedAt: now.UTC().Format(time.RFC3339Nano),
		SelectedBy: strings.TrimSpace(selectedBy), Modules: next}
	if selection.SelectedBy == "" {
		return Selection{}, fmt.Errorf("analyzer selection requires an attributed selector")
	}
	if err := writeSelection(dataDir, selection); err != nil {
		return Selection{}, err
	}
	return selection, nil
}

// Deselect removes one exact selected module without deleting installed bytes.
func Deselect(dataDir, moduleID, selectedBy string, now time.Time) (Selection, error) {
	selection, err := LoadSelection(dataDir)
	if err != nil {
		return Selection{}, err
	}
	next := make([]SelectedModule, 0, len(selection.Modules))
	found := false
	for _, current := range selection.Modules {
		if current.ModuleID == moduleID {
			found = true
			continue
		}
		next = append(next, current)
	}
	if !found {
		return Selection{}, fmt.Errorf("analyzer module %q is not selected", moduleID)
	}
	selection = Selection{FormatVersion: selectionFormatV1, SelectedAt: now.UTC().Format(time.RFC3339Nano),
		SelectedBy: strings.TrimSpace(selectedBy), Modules: next}
	if selection.SelectedBy == "" {
		return Selection{}, fmt.Errorf("analyzer deselection requires an attributed selector")
	}
	if err := writeSelection(dataDir, selection); err != nil {
		return Selection{}, err
	}
	return selection, nil
}

// ResolveSelected validates all selected package bytes and deterministic claims without
// executing a module.
func ResolveSelected(dataDir string) ([]Package, error) {
	selection, err := LoadSelection(dataDir)
	if err != nil {
		return nil, err
	}
	packages, err := resolveModules(dataDir, selection.Modules)
	if err != nil {
		return nil, err
	}
	if err := validateClaimConflicts(packages); err != nil {
		return nil, err
	}
	return packages, nil
}

func resolveModules(dataDir string, selected []SelectedModule) ([]Package, error) {
	packages := make([]Package, 0, len(selected))
	seen := map[string]bool{}
	for _, item := range selected {
		if seen[item.ModuleID] || !strings.HasPrefix(item.PackageDigest, "sha256-v1:") {
			return nil, fmt.Errorf("analyzer selection contains duplicate or invalid module %q", item.ModuleID)
		}
		seen[item.ModuleID] = true
		installed, err := Inspect(packageDirectory(dataDir, item.PackageDigest))
		if err != nil {
			return nil, fmt.Errorf("resolve selected analyzer %s: %w", item.ModuleID, err)
		}
		if installed.Manifest.ModuleID != item.ModuleID || installed.PackageDigest != item.PackageDigest {
			return nil, fmt.Errorf("selected analyzer %s identity/digest does not match installed package", item.ModuleID)
		}
		packages = append(packages, installed)
	}
	sort.Slice(packages, func(i, j int) bool {
		return packages[i].Manifest.ModuleID < packages[j].Manifest.ModuleID
	})
	return packages, nil
}

func validateClaimConflicts(packages []Package) error {
	claimed := map[string]string{}
	for _, installed := range packages {
		for _, extension := range installed.Manifest.Extensions {
			if owner := claimed[extension]; owner != "" {
				return fmt.Errorf("analyzer modules %s and %s both claim extension %s; explicit replacement is required",
					owner, installed.Manifest.ModuleID, extension)
			}
			claimed[extension] = installed.Manifest.ModuleID
		}
	}
	return nil
}

func validateSelection(selection Selection) error {
	if selection.FormatVersion != selectionFormatV1 {
		return fmt.Errorf("unsupported analyzer selection format %q", selection.FormatVersion)
	}
	if len(selection.Modules) > maxSelectedModules {
		return fmt.Errorf("analyzer selection exceeds %d modules", maxSelectedModules)
	}
	if len(selection.Modules) > 0 && (selection.SelectedAt == "" || selection.SelectedBy == "") {
		return fmt.Errorf("analyzer selection requires selector and time")
	}
	if selection.SelectedAt != "" {
		if _, err := time.Parse(time.RFC3339Nano, selection.SelectedAt); err != nil {
			return fmt.Errorf("analyzer selection has invalid time")
		}
	}
	seen := map[string]bool{}
	for _, item := range selection.Modules {
		if !manifestID.MatchString(item.ModuleID) || !strings.HasPrefix(item.PackageDigest, "sha256-v1:") || seen[item.ModuleID] {
			return fmt.Errorf("analyzer selection contains invalid module %q", item.ModuleID)
		}
		seen[item.ModuleID] = true
	}
	return nil
}

func writeSelection(dataDir string, selection Selection) error {
	if err := validateSelection(selection); err != nil {
		return err
	}
	root := Root(dataDir)
	if err := os.MkdirAll(root, packageDirectoryMode); err != nil {
		return fmt.Errorf("create analyzer lifecycle root: %w", err)
	}
	body, err := json.MarshalIndent(selection, "", "  ")
	if err != nil {
		return fmt.Errorf("encode analyzer selection: %w", err)
	}
	body = append(body, '\n')
	temporary, err := os.CreateTemp(root, ".selection-*")
	if err != nil {
		return fmt.Errorf("create analyzer selection staging file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("protect analyzer selection staging file: %w", err)
	}
	if _, err := temporary.Write(body); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write analyzer selection staging file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync analyzer selection staging file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close analyzer selection staging file: %w", err)
	}
	selectionPath := filepath.Join(root, "selection.json")
	previousPath := filepath.Join(root, "selection.previous.json")
	if current, err := os.ReadFile(selectionPath); err == nil {
		if err := atomicfile.WriteNoDirSync(previousPath, current, 0o600); err != nil {
			return fmt.Errorf("preserve previous analyzer selection: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read previous analyzer selection: %w", err)
	}
	if err := os.Rename(temporaryPath, selectionPath); err != nil {
		return fmt.Errorf("publish analyzer selection: %w", err)
	}
	return atomicfile.SyncDir(root)
}
