// Package analyzermodule owns installed executable analyzer packages, explicit
// user-scope selection, and bounded process execution. It imports the portable codemap
// protocol; codemap and understanding never import this lifecycle/process package.
package analyzermodule

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"crossing-guard/codemap"
)

const (
	manifestName          = "manifest.json"
	maxManifestBytes      = 64 << 10
	maxPackageBytes       = 256 << 20
	maxPackageFiles       = 4096
	maxManifestClaims     = 128
	maxManifestTextLength = 256
)

var manifestID = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]{0,254}[A-Za-z0-9])?$`)

// Package is one validated immutable analyzer package. Available does not mean selected
// or active; callers must load an exact selection separately.
type Package struct {
	Dir              string
	Manifest         codemap.AnalyzerModuleManifest
	PackageDigest    string
	EntrypointDigest string
}

// Inspect validates and hashes an analyzer source/package directory without executing
// its entrypoint.
func Inspect(directory string) (Package, error) {
	abs, err := filepath.Abs(strings.TrimSpace(directory))
	if err != nil || strings.TrimSpace(directory) == "" {
		return Package{}, fmt.Errorf("analyzer package requires a directory")
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return Package{}, fmt.Errorf("inspect analyzer package %s: %w", abs, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return Package{}, fmt.Errorf("analyzer package root must be a real directory: %s", abs)
	}
	manifest, err := readManifest(filepath.Join(abs, manifestName))
	if err != nil {
		return Package{}, err
	}
	if err := validateManifest(manifest); err != nil {
		return Package{}, err
	}
	packageDigest, _, _, err := digestDirectory(abs)
	if err != nil {
		return Package{}, err
	}
	entrypoint := filepath.Join(abs, filepath.FromSlash(manifest.Entrypoint))
	entryDigest, err := validateEntrypoint(abs, entrypoint)
	if err != nil {
		return Package{}, err
	}
	return Package{Dir: abs, Manifest: manifest, PackageDigest: packageDigest,
		EntrypointDigest: entryDigest}, nil
}

func readManifest(path string) (codemap.AnalyzerModuleManifest, error) {
	file, err := os.Open(path)
	if err != nil {
		return codemap.AnalyzerModuleManifest{}, fmt.Errorf("open analyzer manifest: %w", err)
	}
	reader := io.LimitReader(file, maxManifestBytes+1)
	body, err := io.ReadAll(reader)
	if err != nil {
		_ = file.Close() // preserve the read error; closing cannot make the bytes valid
		return codemap.AnalyzerModuleManifest{}, fmt.Errorf("read analyzer manifest: %w", err)
	}
	if err := file.Close(); err != nil {
		return codemap.AnalyzerModuleManifest{}, fmt.Errorf("close analyzer manifest: %w", err)
	}
	if len(body) > maxManifestBytes {
		return codemap.AnalyzerModuleManifest{}, fmt.Errorf("analyzer manifest exceeds %d bytes", maxManifestBytes)
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	var manifest codemap.AnalyzerModuleManifest
	if err := decoder.Decode(&manifest); err != nil {
		return manifest, fmt.Errorf("decode analyzer manifest: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return manifest, fmt.Errorf("decode analyzer manifest: %w", err)
	}
	return manifest, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func validateManifest(manifest codemap.AnalyzerModuleManifest) error {
	if manifest.FormatVersion != codemap.AnalyzerManifestV1 {
		return fmt.Errorf("unsupported analyzer manifest format %q", manifest.FormatVersion)
	}
	if manifest.ProtocolVersion != codemap.AnalyzerProtocolV1 {
		return fmt.Errorf("unsupported analyzer protocol %q", manifest.ProtocolVersion)
	}
	for label, value := range map[string]string{"module_id": manifest.ModuleID,
		"module_version": manifest.ModuleVersion, "analyzer_identity": manifest.AnalyzerIdentity} {
		if !manifestID.MatchString(value) || len(value) > maxManifestTextLength {
			return fmt.Errorf("analyzer manifest has invalid %s", label)
		}
	}
	if !safeRelativePath(manifest.Entrypoint) {
		return fmt.Errorf("analyzer entrypoint must be a safe package-relative path")
	}
	if err := validateStrings("language", manifest.Languages, func(value string) bool {
		return manifestID.MatchString(value)
	}); err != nil {
		return err
	}
	if err := validateStrings("extension", manifest.Extensions, func(value string) bool {
		return len(value) >= 2 && value[0] == '.' && value == strings.ToLower(value) &&
			!strings.ContainsAny(value, "/\\\x00") && len(value) <= 32
	}); err != nil {
		return err
	}
	allowed := map[codemap.Capability]bool{
		codemap.CapabilityPackageDependency: true, codemap.CapabilitySymbolDeclaration: true,
		codemap.CapabilityResponsibilityFingerprint: true, codemap.CapabilitySymbolCall: true,
	}
	seenCapabilities := map[codemap.Capability]bool{}
	if len(manifest.Capabilities) == 0 || len(manifest.Capabilities) > maxManifestClaims {
		return fmt.Errorf("analyzer manifest requires 1..%d capabilities", maxManifestClaims)
	}
	for _, capability := range manifest.Capabilities {
		if !allowed[capability] || seenCapabilities[capability] {
			return fmt.Errorf("analyzer manifest has unsupported or duplicate capability %q", capability)
		}
		seenCapabilities[capability] = true
	}
	if err := validateStrings("body-shape algorithm", manifest.BodyShapeAlgorithms, func(value string) bool {
		return manifestID.MatchString(value)
	}); err != nil && len(manifest.BodyShapeAlgorithms) != 0 {
		return err
	}
	return nil
}

func validateStrings(label string, values []string, valid func(string) bool) error {
	if len(values) == 0 || len(values) > maxManifestClaims {
		return fmt.Errorf("analyzer manifest requires 1..%d %ss", maxManifestClaims, label)
	}
	seen := map[string]bool{}
	for _, value := range values {
		if !valid(value) || len(value) > maxManifestTextLength || seen[value] {
			return fmt.Errorf("analyzer manifest has invalid or duplicate %s %q", label, value)
		}
		seen[value] = true
	}
	return nil
}

func safeRelativePath(path string) bool {
	if path == "" || filepath.IsAbs(path) || strings.ContainsRune(path, 0) || strings.Contains(path, "\\") {
		return false
	}
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	return cleaned == path && cleaned != "." && cleaned != ".." && !strings.HasPrefix(cleaned, "../")
}

func validateEntrypoint(root, path string) (string, error) {
	if !withinRoot(root, path) {
		return "", fmt.Errorf("analyzer entrypoint escapes package root")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("inspect analyzer entrypoint: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("analyzer entrypoint must be a regular executable file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open analyzer entrypoint: %w", err)
	}
	reader := bufio.NewReader(file)
	prefix, _ := reader.Peek(2)
	if string(prefix) == "#!" {
		_ = file.Close() // the protocol rejection is the actionable error
		return "", fmt.Errorf("interpreter-backed analyzer entrypoints are unsupported in protocol v1")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close() // preserve the seek error
		return "", fmt.Errorf("seek analyzer entrypoint: %w", err)
	}
	digest, err := digestReader(file)
	if err != nil {
		_ = file.Close() // preserve the read error
		return "", fmt.Errorf("hash analyzer entrypoint: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close analyzer entrypoint: %w", err)
	}
	return digest, nil
}

func digestDirectory(root string) (string, int, int64, error) {
	type entry struct {
		path string
		mode fs.FileMode
		size int64
	}
	entries := []entry{}
	var total int64
	err := filepath.WalkDir(root, func(path string, item fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		info, err := item.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("analyzer package contains unsupported file %s", path)
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || !safeRelativePath(filepath.ToSlash(rel)) {
			return fmt.Errorf("analyzer package contains unsafe path %s", path)
		}
		entries = append(entries, entry{filepath.ToSlash(rel), info.Mode().Perm(), info.Size()})
		total += info.Size()
		if len(entries) > maxPackageFiles || total > maxPackageBytes {
			return fmt.Errorf("analyzer package exceeds %d files or %d bytes", maxPackageFiles, maxPackageBytes)
		}
		return nil
	})
	if err != nil {
		return "", 0, 0, fmt.Errorf("inspect analyzer package: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	hash := sha256.New()
	var length [8]byte
	for _, item := range entries {
		binary.BigEndian.PutUint64(length[:], uint64(len(item.path)))
		_, _ = hash.Write(length[:])
		_, _ = io.WriteString(hash, item.path)
		// Match the private copy's 0700 mode while preserving existing installed digests.
		var executableMode uint64
		if item.mode&0o111 != 0 {
			executableMode = 0o100
		}
		binary.BigEndian.PutUint64(length[:], executableMode)
		_, _ = hash.Write(length[:])
		binary.BigEndian.PutUint64(length[:], uint64(item.size))
		_, _ = hash.Write(length[:])
		file, err := os.Open(filepath.Join(root, filepath.FromSlash(item.path)))
		if err != nil {
			return "", 0, 0, fmt.Errorf("hash analyzer package %s: %w", item.path, err)
		}
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if copyErr != nil {
			return "", 0, 0, fmt.Errorf("hash analyzer package %s: %w", item.path, copyErr)
		}
		if closeErr != nil {
			return "", 0, 0, fmt.Errorf("close analyzer package %s: %w", item.path, closeErr)
		}
	}
	return "sha256-v1:" + hex.EncodeToString(hash.Sum(nil)), len(entries), total, nil
}

func digestReader(reader io.Reader) (string, error) {
	hash := sha256.New()
	if _, err := io.Copy(hash, reader); err != nil {
		return "", err
	}
	return "sha256-v1:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func withinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
