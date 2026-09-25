package understanding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"crossing-guard/codemap"
	"crossing-guard/codemap/adapters/golang"
	"crossing-guard/internal/changeenv"
	"crossing-guard/store"
)

func init() { golang.Register() }

func testAnalyzerAssembly(t *testing.T) *codemap.AnalyzerAssembly {
	t.Helper()
	assembly, err := codemap.RegisteredAnalyzerAssembly()
	if err != nil {
		t.Fatal(err)
	}
	return assembly
}

func testRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(rel, body string) {
		t.Helper()
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q")
	write("go.mod", "module example.test/project\n\ngo 1.26\n")
	write("a/a.go", "package a\n\nimport \"example.test/project/b\"\n\ntype Item struct{}\nfunc Use() {}\n")
	write("b/b.go", "package b\n\nfunc Called() {}\n")
	write("docs/work.md", "| **D7** | update a/a.go |\n")
	run("add", ".")
	run("-c", "user.name=CrossingGuard", "-c", "user.email=crossing-guard@example.invalid", "commit", "-q", "-m", "fixture")
	return root
}

func openStore(t *testing.T) *store.Index {
	t.Helper()
	index, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	return index
}

func TestScanAppendsExactStructuralAndReferenceGeneration(t *testing.T) {
	root := testRepository(t)
	index := openStore(t)
	revision, err := changeenv.RecordSnapshot(index, changeenv.SnapshotInput{SessionID: "journey", RepoDir: root, Base: "HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := Scan(context.Background(), index, ScanInput{RepoDir: root, Base: "HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Recorded || result.Generation == nil || result.Generation.Status != "complete" || !strings.HasPrefix(result.Generation.SnapshotDigest, "git-tree-v2-sha256:") {
		t.Fatalf("complete generation not recorded: %+v", result)
	}
	if result.Generation.StructuralSchema != "codemap-structural-v3" {
		t.Fatalf("generation retained stale structural schema: %q", result.Generation.StructuralSchema)
	}
	if revision.SnapshotDigest != result.Generation.SnapshotDigest {
		t.Fatalf("stable compiled journey could not join exact source: revision=%s understanding=%s", revision.SnapshotDigest, result.Generation.SnapshotDigest)
	}
	if result.Generation.ConventionState != "none" || result.Generation.ConventionSourceRef != "" {
		t.Fatalf("implicit configuration entered structural scan: %+v", result.Generation)
	}
	relations := map[string]int{}
	for _, edge := range result.Generation.Edges {
		relations[edge.Relation]++
		if edge.ToKind == "file" && edge.ToRef == "D7" {
			t.Fatalf("item id was misclassified as a file edge: %+v", edge)
		}
		if edge.Provenance != "measured" || edge.AnalyzerID == "" || edge.EvidenceDigest == "" {
			t.Fatalf("edge lacks inspectable evidence identity: %+v", edge)
		}
	}
	if result.Generation.ErrorTotal != 0 {
		t.Fatalf("unsupported file types inflated error total: %d", result.Generation.ErrorTotal)
	}
	for _, relation := range []string{"file_in_package", "package_depends_on", "file_declares_symbol", "document_references_file", "document_defines_item", "item_references_file"} {
		if relations[relation] == 0 {
			t.Errorf("missing relation %s: %v", relation, relations)
		}
	}
	coverage := map[string]string{}
	for _, row := range result.Generation.Coverage {
		coverage[row.Family] = row.State
	}
	if coverage["package_dependency"] != "complete" || coverage["symbol_declaration"] != "complete" || coverage["responsibility_fingerprint"] != "complete" || coverage["document_reference"] != "complete" {
		t.Fatalf("known coverage missing: %v", coverage)
	}
	for _, family := range []string{"symbol_call", "configuration_effect", "data_effect", "policy_effect", "journey_effect"} {
		if coverage[family] != "unsupported" {
			t.Errorf("unsupported family %s was hidden: %v", family, coverage)
		}
	}
	stored, found, err := index.UnderstandingGenerationByID(result.Generation.ID)
	if err != nil || !found || stored.EdgeTotal != len(result.Generation.Edges) {
		t.Fatalf("stored generation mismatch: found=%v stored=%+v err=%v", found, stored, err)
	}
	units, total, err := index.UnderstandingUnits(result.Generation.ID, 0, 100)
	if err != nil || total != len(result.Generation.Units) {
		t.Fatalf("stored units: total=%d units=%d err=%v", total, len(units), err)
	}
	decoded := make([]codemap.ResponsibilityUnit, 0, len(units))
	var center codemap.ResponsibilityUnit
	for _, unit := range units {
		decodedUnit, err := codemap.DecodeResponsibilityUnit(unit.Path, unit.DescriptorJSON)
		if err != nil {
			t.Fatal(err)
		}
		decoded = append(decoded, decodedUnit)
		if unit.Path == "a/a.go" {
			center = decodedUnit
			if decodedUnit.Descriptor.Identity.AnalyzerID != "go-ast-v3" {
				t.Fatalf("stored descriptor analyzer=%q", decodedUnit.Descriptor.Identity.AnalyzerID)
			}
			body, _ := json.Marshal(decodedUnit.Descriptor)
			if strings.Contains(string(body), "func Use") || !strings.Contains(string(body), "go-ast-body-shape-v1-sha256:") {
				t.Fatalf("descriptor persisted raw body or lost shape digest: %s", body)
			}
		}
	}
	candidates, page, err := codemap.ResponsibilityCandidates(center, decoded, 0, 100)
	if err != nil || page.Total == 0 || len(candidates) == 0 || candidates[0].RightPath != "b/b.go" || candidates[0].SharedBodyShapes.Total == 0 {
		t.Fatalf("persisted shape facts not reproducible: page=%+v candidates=%+v err=%v", page, candidates, err)
	}
}

func TestScanRetriesThenRecordsFailedAttemptWithoutFacts(t *testing.T) {
	root := testRepository(t)
	index := openStore(t)
	untracked := filepath.Join(root, "changing.go")
	if err := os.WriteFile(untracked, []byte("package changing\n"), 0600); err != nil {
		t.Fatal(err)
	}
	mutations := 0
	result, err := Scan(context.Background(), index, ScanInput{
		RepoDir: root, Base: "HEAD",
		BetweenCapture: func(attempt int) {
			mutations++
			_ = os.WriteFile(untracked, []byte(fmt.Sprintf("package changing\n// %d\n", mutations)), 0600)
		},
	})
	if err == nil || !strings.Contains(err.Error(), "changed during analysis") {
		t.Fatalf("unstable analysis accepted: result=%+v err=%v", result, err)
	}
	if !result.Recorded || result.Generation.Status != "failed" || result.Generation.LimitationCode != "source_changed" {
		t.Fatalf("failed attempt not explicit: %+v", result)
	}
	units, total, err := index.UnderstandingUnits(result.Generation.ID, 0, 10)
	if err != nil || total != 0 || len(units) != 0 {
		t.Fatalf("failed attempt retained facts: total=%d units=%+v err=%v", total, units, err)
	}
}

func TestExpectedSnapshotScansOnceAndRefusesSupersededSource(t *testing.T) {
	root := testRepository(t)
	index := openStore(t)
	record, err := changeenv.CaptureSnapshotRecord(context.Background(), changeenv.SnapshotInput{
		SessionID: "expected", RepoDir: root, Base: "HEAD",
	})
	if err != nil {
		t.Fatal(err)
	}
	expected := &SnapshotExpectation{RepositoryID: record.RepositoryID, CheckoutID: record.CheckoutID,
		CheckoutRoot: record.CheckoutRoot, SnapshotProtocol: changeenv.GitTreeProtocol,
		SnapshotDigest: record.SnapshotDigest, BaseRevision: record.BaseRevision,
		HeadRevision: record.HeadRevision}
	result, err := Scan(context.Background(), index, ScanInput{RepoDir: root,
		Base: record.BaseRevision, Expected: expected})
	if err != nil || result.Outcome != "complete" || result.Generation == nil ||
		result.Generation.SnapshotDigest != record.SnapshotDigest {
		t.Fatalf("expected scan result=%+v err=%v", result, err)
	}

	if err := os.WriteFile(filepath.Join(root, "a", "a.go"), []byte("package a\nfunc Changed() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err = Scan(context.Background(), index, ScanInput{RepoDir: root,
		Base: record.BaseRevision, Expected: expected})
	if !errors.Is(err, ErrSnapshotSuperseded) || result.Outcome != "superseded" || result.Recorded {
		t.Fatalf("pre-analysis supersession result=%+v err=%v", result, err)
	}
	_, total, err := index.UnderstandingGenerations(record.RepositoryID, record.CheckoutID, 10)
	if err != nil || total != 1 {
		t.Fatalf("supersession appended generation: total=%d err=%v", total, err)
	}
}

func TestExpectedSnapshotDoesNotRetryAfterDuringAnalysisChange(t *testing.T) {
	root := testRepository(t)
	index := openStore(t)
	record, err := changeenv.CaptureSnapshotRecord(context.Background(), changeenv.SnapshotInput{
		SessionID: "expected-race", RepoDir: root, Base: "HEAD",
	})
	if err != nil {
		t.Fatal(err)
	}
	expected := &SnapshotExpectation{RepositoryID: record.RepositoryID, CheckoutID: record.CheckoutID,
		CheckoutRoot: record.CheckoutRoot, SnapshotProtocol: changeenv.GitTreeProtocol,
		SnapshotDigest: record.SnapshotDigest, BaseRevision: record.BaseRevision,
		HeadRevision: record.HeadRevision}
	attempts := 0
	result, err := Scan(context.Background(), index, ScanInput{RepoDir: root,
		Base: record.BaseRevision, Expected: expected, BetweenCapture: func(attempt int) {
			attempts++
			_ = os.WriteFile(filepath.Join(root, "during.go"), []byte("package during\n"), 0600)
		}})
	if !errors.Is(err, ErrSnapshotSuperseded) || result.Outcome != "superseded" || attempts != 1 {
		t.Fatalf("during-analysis supersession attempts=%d result=%+v err=%v", attempts, result, err)
	}
	_, total, err := index.UnderstandingGenerations(record.RepositoryID, record.CheckoutID, 10)
	if err != nil || total != 0 {
		t.Fatalf("superseded attempt persisted: total=%d err=%v", total, err)
	}
}

func TestExpectedSnapshotCancellationBeforeObservationDoesNotPersistIdentitylessFailure(t *testing.T) {
	root := testRepository(t)
	index := openStore(t)
	record, err := changeenv.CaptureSnapshotRecord(context.Background(), changeenv.SnapshotInput{
		SessionID: "expected-cancel", RepoDir: root, Base: "HEAD",
	})
	if err != nil {
		t.Fatal(err)
	}
	expected := &SnapshotExpectation{RepositoryID: record.RepositoryID, CheckoutID: record.CheckoutID,
		CheckoutRoot: record.CheckoutRoot, SnapshotProtocol: changeenv.GitTreeProtocol,
		SnapshotDigest: record.SnapshotDigest, BaseRevision: record.BaseRevision,
		HeadRevision: record.HeadRevision}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := Scan(ctx, index, ScanInput{RepoDir: root, Base: record.BaseRevision, Expected: expected})
	if !errors.Is(err, context.Canceled) || result.Recorded || result.Outcome != "failed" {
		t.Fatalf("cancelled expected scan result=%+v err=%v", result, err)
	}
	_, total, err := index.UnderstandingGenerations(record.RepositoryID, record.CheckoutID, 10)
	if err != nil || total != 0 {
		t.Fatalf("pre-observation cancellation persisted failure: total=%d err=%v", total, err)
	}
}

func TestAutomaticFailureBeforeObservationRetainsExpectedIdentityForBackoff(t *testing.T) {
	root := testRepository(t)
	index := openStore(t)
	record, err := changeenv.CaptureSnapshotRecord(context.Background(), changeenv.SnapshotInput{
		SessionID: "expected-failure", RepoDir: root, Base: "HEAD",
	})
	if err != nil {
		t.Fatal(err)
	}
	expected := &SnapshotExpectation{RepositoryID: record.RepositoryID, CheckoutID: record.CheckoutID,
		CheckoutRoot: record.CheckoutRoot, SnapshotProtocol: changeenv.GitTreeProtocol,
		SnapshotDigest: record.SnapshotDigest, BaseRevision: record.BaseRevision,
		HeadRevision: record.HeadRevision}
	result, err := appendFailed(index, changeenv.Repository{ID: record.RepositoryID,
		CheckoutID: record.CheckoutID, Root: record.CheckoutRoot}, ScanInput{Expected: expected, Assembly: testAnalyzerAssembly(t)}, nil,
		1, "none", "", "", "source_observation_failed", errors.New("fixture observation failure"))
	if err == nil || !result.Recorded || result.Generation == nil {
		t.Fatalf("expected failure result=%+v err=%v", result, err)
	}
	stored, found, lookupErr := index.UnderstandingForSnapshot(record.RepositoryID,
		record.CheckoutID, record.SnapshotDigest, codemap.StructuralSchema,
		testAnalyzerAssembly(t).Digest(), "none", "")
	if lookupErr != nil || !found || stored.Status != "failed" ||
		stored.SnapshotDigest != record.SnapshotDigest || stored.BaseRevision != record.BaseRevision ||
		stored.HeadRevision != record.HeadRevision {
		t.Fatalf("durable expected failure found=%v generation=%+v err=%v", found, stored, lookupErr)
	}
}

func TestAutomaticRepositoryResolutionFailureRetainsExpectedIdentityForBackoff(t *testing.T) {
	index := openStore(t)
	missingRoot := filepath.Join(t.TempDir(), "missing-checkout")
	expected := &SnapshotExpectation{RepositoryID: "repo", CheckoutID: "checkout",
		CheckoutRoot: missingRoot, SnapshotProtocol: changeenv.GitTreeProtocol,
		SnapshotDigest: "git-tree-v2-sha256:missing", BaseRevision: "base", HeadRevision: "head"}
	result, err := Scan(context.Background(), index, ScanInput{RepoDir: missingRoot,
		Base: expected.BaseRevision, Expected: expected})
	if err == nil || !result.Recorded || result.Generation == nil ||
		result.Generation.LimitationCode != "repository_resolution_failed" {
		t.Fatalf("resolution failure result=%+v err=%v", result, err)
	}
	stored, found, lookupErr := index.UnderstandingForSnapshot(expected.RepositoryID,
		expected.CheckoutID, expected.SnapshotDigest, codemap.StructuralSchema,
		testAnalyzerAssembly(t).Digest(), "none", "")
	if lookupErr != nil || !found || stored.Status != "failed" || stored.CheckoutRoot != missingRoot {
		t.Fatalf("durable resolution failure found=%v generation=%+v err=%v", found, stored, lookupErr)
	}
}

func TestScanRecordsMalformedExplicitConfigurationWithoutFallback(t *testing.T) {
	root := testRepository(t)
	configuration := filepath.Join(root, "conventions.json")
	if err := os.WriteFile(configuration, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	index := openStore(t)
	result, err := Scan(context.Background(), index, ScanInput{RepoDir: root, Base: "HEAD", Conventions: configuration})
	if err == nil || !strings.Contains(err.Error(), "convention config") {
		t.Fatalf("malformed explicit configuration accepted: result=%+v err=%v", result, err)
	}
	if !result.Recorded || result.Generation.Status != "failed" || result.Generation.ConventionState != "explicit" || result.Generation.ConventionSourceRef == "" || result.Generation.ConventionSourceDigest == "" {
		t.Fatalf("explicit configuration failure lost identity or fell back: %+v", result)
	}
}
