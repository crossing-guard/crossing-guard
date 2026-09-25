package analyzermodule

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crossing-guard/codemap"
)

func TestMain(main *testing.M) {
	if os.Getenv("CROSSING_GUARD_ANALYZER_PROTOCOL") == codemap.AnalyzerProtocolV1 {
		os.Exit(runFixtureModule(os.Stdin, os.Stdout))
	}
	os.Exit(main.Run())
}

func TestInstallSelectResolveAndRunNativeModule(t *testing.T) {
	dataDir := t.TempDir()
	source := fixturePackage(t, "org.crossing-guard.fixture", ".fixture")
	installed, err := Install(dataDir, source)
	if err != nil {
		t.Fatal(err)
	}
	if installed.Dir == source || !strings.Contains(installed.Dir, filepath.Join("analyzers", "packages")) {
		t.Fatalf("installed directory=%q", installed.Dir)
	}
	if selection, err := LoadSelection(dataDir); err != nil || len(selection.Modules) != 0 {
		t.Fatalf("available module became selected: %+v err=%v", selection, err)
	}
	selection, err := Select(dataDir, installed, "", "test", time.Unix(10, 0))
	if err != nil || len(selection.Modules) != 1 {
		t.Fatalf("select=%+v err=%v", selection, err)
	}
	resolved, err := ResolveSelected(dataDir)
	if err != nil || len(resolved) != 1 || resolved[0].PackageDigest != installed.PackageDigest {
		t.Fatalf("resolve=%+v err=%v", resolved, err)
	}
	result, err := Run(t.Context(), resolved[0], ScanRequest{RequestID: "request-1",
		SnapshotProtocol: "git-tree-v2", SnapshotDigest: "git-tree-v2-sha256:fixture",
		CheckoutRoot: t.TempDir(), Paths: []codemap.AnalyzerPath{{Path: "sample.fixture",
			SourceHash: "sha256-v1:fixture", Bytes: 32, Lines: 3}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Units) != 1 || result.Units[0].Path != "sample.fixture" ||
		len(result.Units[0].Declarations) != 1 || len(result.Coverage) != 2 {
		t.Fatalf("unexpected module result: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(Root(dataDir), "selection.json")); err != nil {
		t.Fatalf("selection was not durable: %v", err)
	}
}

func TestSelectionConflictRequiresNamedReplacement(t *testing.T) {
	dataDir := t.TempDir()
	first, err := Install(dataDir, fixturePackage(t, "org.example.first", ".fixture"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Install(dataDir, fixturePackage(t, "org.example.second", ".fixture"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Select(dataDir, first, "", "test", time.Unix(10, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := Select(dataDir, second, "", "test", time.Unix(11, 0)); err == nil || !strings.Contains(err.Error(), "both claim") {
		t.Fatalf("overlapping selection error=%v", err)
	}
	selection, err := Select(dataDir, second, first.Manifest.ModuleID, "test", time.Unix(12, 0))
	if err != nil || len(selection.Modules) != 1 || selection.Modules[0].ModuleID != second.Manifest.ModuleID {
		t.Fatalf("replacement=%+v err=%v", selection, err)
	}
	previous, err := os.ReadFile(filepath.Join(Root(dataDir), "selection.previous.json"))
	if err != nil || !strings.Contains(string(previous), first.Manifest.ModuleID) {
		t.Fatalf("previous selection=%q err=%v", previous, err)
	}
}

func TestRemoveRequiresExactUnselectedPackage(t *testing.T) {
	dataDir := t.TempDir()
	installed, err := Install(dataDir, fixturePackage(t, "org.example.removal", ".remove"))
	if err != nil {
		t.Fatal(err)
	}
	reference := installed.Manifest.ModuleID + "@" + installed.PackageDigest
	if _, err := Select(dataDir, installed, "", "test", time.Unix(10, 0)); err != nil {
		t.Fatal(err)
	}
	if err := Remove(dataDir, reference); err == nil || !strings.Contains(err.Error(), "deselect") {
		t.Fatalf("selected removal error=%v", err)
	}
	if _, err := Deselect(dataDir, installed.Manifest.ModuleID, "test", time.Unix(11, 0)); err != nil {
		t.Fatal(err)
	}
	if err := Remove(dataDir, reference); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(installed.Dir); !os.IsNotExist(err) {
		t.Fatalf("removed package still exists: %v", err)
	}
}

func TestRunnerRejectsPackageMutationBeforeExecution(t *testing.T) {
	dataDir := t.TempDir()
	installed, err := Install(dataDir, fixturePackage(t, "org.example.mutation", ".fixture"))
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(installed.Dir, manifestName)
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, append(manifest, ' '), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Run(t.Context(), installed, ScanRequest{RequestID: "request-1",
		SnapshotProtocol: "git-tree-v2", SnapshotDigest: "git-tree-v2-sha256:fixture",
		CheckoutRoot: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "changed before execution") {
		t.Fatalf("mutation error=%v", err)
	}
}

func TestInspectRejectsInterpreterEntrypointAndUnknownManifestField(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "analyzer"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, directory, manifestFixture("org.example.script", ".fixture"))
	if _, err := Inspect(directory); err == nil || !strings.Contains(err.Error(), "interpreter-backed") {
		t.Fatalf("script entrypoint error=%v", err)
	}
	body, err := os.ReadFile(filepath.Join(directory, manifestName))
	if err != nil {
		t.Fatal(err)
	}
	body = []byte(strings.Replace(string(body), "\n}", ",\n  \"language_specific\": true\n}", 1))
	if err := os.WriteFile(filepath.Join(directory, manifestName), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(directory); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown manifest field error=%v", err)
	}
}

func fixturePackage(t *testing.T, moduleID, extension string) string {
	t.Helper()
	directory := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := copyRegularFile(executable, filepath.Join(directory, "analyzer"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, directory, manifestFixture(moduleID, extension))
	return directory
}

func manifestFixture(moduleID, extension string) codemap.AnalyzerModuleManifest {
	return codemap.AnalyzerModuleManifest{FormatVersion: codemap.AnalyzerManifestV1,
		ModuleID: moduleID, ModuleVersion: "v1", ProtocolVersion: codemap.AnalyzerProtocolV1,
		Entrypoint: "analyzer", AnalyzerIdentity: "fixture-analyzer-v1",
		Languages: []string{"fixture"}, Extensions: []string{extension},
		Capabilities: []codemap.Capability{codemap.CapabilitySymbolDeclaration,
			codemap.CapabilityResponsibilityFingerprint},
		BodyShapeAlgorithms: []string{"fixture-shape-v1"}}
}

func writeManifest(t *testing.T, directory string, manifest codemap.AnalyzerModuleManifest) {
	t.Helper()
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, '\n')
	if err := os.WriteFile(filepath.Join(directory, manifestName), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func runFixtureModule(input io.Reader, output io.Writer) int {
	scanner := bufio.NewScanner(input)
	var start codemap.AnalyzerScanStart
	var path *codemap.AnalyzerPath
	for scanner.Scan() {
		var record codemap.AnalyzerInputRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return 2
		}
		switch record.Type {
		case "scan_start":
			if record.Start == nil {
				return 2
			}
			start = *record.Start
		case "path":
			path = record.Path
		case "scan_end":
		default:
			return 2
		}
	}
	if err := scanner.Err(); err != nil || start.RequestID == "" {
		return 2
	}
	encoder := json.NewEncoder(output)
	identity := "fixture-analyzer-v1"
	moduleID := fixtureModuleIDFromExecutable()
	if err := encoder.Encode(codemap.AnalyzerOutputRecord{Type: "analysis_start",
		ProtocolVersion: codemap.AnalyzerProtocolV1, RequestID: start.RequestID,
		ModuleID: moduleID, AnalyzerIdentity: identity}); err != nil {
		return 2
	}
	unitTotal := 0
	if path != nil {
		unitTotal = 1
		complexity := 1
		unit := codemap.ModuleUnit{Path: path.Path, Language: "fixture", LOC: path.Lines,
			Declarations: []codemap.ModuleDeclaration{{Identity: "Example", Name: "Example",
				Kind: "function", Line: 1, EndLine: 1, StartByte: 0, EndByte: 1,
				Cyclomatic: &complexity, BodyShapeDigest: "fixture-shape-v1:abc", BodyShapeNodes: 1}}}
		if err := encoder.Encode(codemap.AnalyzerOutputRecord{Type: "unit", Unit: &unit}); err != nil {
			return 2
		}
	}
	for _, family := range []codemap.Capability{codemap.CapabilitySymbolDeclaration,
		codemap.CapabilityResponsibilityFingerprint} {
		coverage := codemap.ModuleCoverage{Family: family, State: "complete", Attempted: unitTotal, Produced: unitTotal}
		if err := encoder.Encode(codemap.AnalyzerOutputRecord{Type: "coverage", Coverage: &coverage}); err != nil {
			return 2
		}
	}
	if err := encoder.Encode(codemap.AnalyzerOutputRecord{Type: "analysis_end",
		ProtocolVersion: codemap.AnalyzerProtocolV1, RequestID: start.RequestID,
		ModuleID: moduleID, AnalyzerIdentity: identity, UnitTotal: unitTotal}); err != nil {
		return 2
	}
	return 0
}

func fixtureModuleIDFromExecutable() string {
	directory, err := os.Getwd()
	if err != nil {
		return ""
	}
	body, err := os.ReadFile(filepath.Join(directory, manifestName))
	if err != nil {
		return ""
	}
	var manifest codemap.AnalyzerModuleManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return ""
	}
	return manifest.ModuleID
}

func ExampleInspect() {
	fmt.Println("available analyzer manifests are validated without executing them")
	// Output: available analyzer manifests are validated without executing them
}

func TestPackageDigestPreservesInstalledExecutableIdentity(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "analyzer")
	if err := os.WriteFile(path, []byte("fixture\n"), 0700); err != nil {
		t.Fatal(err)
	}
	const prior = "sha256-v1:482682a129458c8750330bc1e14a474ed2ddbdda1e4fc1cfd15c84a3588e37a6"
	for _, mode := range []os.FileMode{0700, 0755, 0710, 0701} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		digest, _, _, err := digestDirectory(root)
		if err != nil || digest != prior {
			t.Errorf("mode %o digest=%s err=%v; want prior installed identity %s", mode, digest, err, prior)
		}
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	digest, _, _, err := digestDirectory(root)
	if err != nil || digest == prior {
		t.Fatalf("removed executable status: digest=%s err=%v", digest, err)
	}
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("changed\n"), 0700); err != nil {
		t.Fatal(err)
	}
	digest, _, _, err = digestDirectory(root)
	if err != nil || digest == prior {
		t.Fatalf("changed content: digest=%s err=%v", digest, err)
	}
}

func TestInstallOrdinaryExecutablePermissions(t *testing.T) {
	source := fixturePackage(t, "org.example.permissions", ".fixture")
	if err := os.Chmod(filepath.Join(source, "analyzer"), 0755); err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	installed, err := Install(data, source)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(installed.Dir, "analyzer"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("stored executable permissions=%o", info.Mode().Perm())
	}
	again, err := Install(data, source)
	if err != nil || again.PackageDigest != installed.PackageDigest || again.Dir != installed.Dir {
		t.Fatalf("reinstall=%+v err=%v", again, err)
	}
}
