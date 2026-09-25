package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ncruces/go-sqlite3/driver"
	"github.com/ncruces/go-sqlite3/ext/fts5"
)

func completeUnderstanding() UnderstandingGeneration {
	return UnderstandingGeneration{
		RepositoryID:         "repo",
		CheckoutID:           "checkout",
		CheckoutRoot:         "/repo",
		Status:               "complete",
		SnapshotProtocol:     "git-tree-v2",
		SnapshotDigest:       "git-tree-v2-sha256:abc",
		BaseRevision:         "base",
		HeadRevision:         "head",
		StructuralSchema:     "codemap-v1",
		AnalyzerBundleDigest: "sha256-v1:bundle",
		ConventionState:      "none",
		StartedAt:            10,
		EndedAt:              11,
		PathTotal:            2,
		Units: []UnderstandingUnit{{
			Path:           "a.go",
			SourceHash:     "sha256-v1:source",
			Language:       "go",
			Namespace:      "example/a",
			DescriptorJSON: `{"path":"a.go"}`,
		}},
		Edges: []UnderstandingEdge{{
			FromKind: "file", FromRef: "a.go", Relation: "file_in_package",
			ToKind: "package", ToRef: "example/a", SourcePath: "a.go", SourceLine: 1,
			Provenance: "measured", AnalyzerID: "go-ast-v1", EvidenceDigest: "sha256-v1:edge",
		}},
		Coverage: []UnderstandingCoverage{{
			Family: "package_dependency", State: "complete", AnalyzerID: "go-ast-v1", Attempted: 1, Produced: 1,
		}},
	}
}

func TestUnderstandingAppendRoundTripAndExactSelection(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	g := completeUnderstanding()
	if err := ix.AppendUnderstanding(&g); err != nil {
		t.Fatal(err)
	}
	if g.ID == 0 || g.UnitTotal != 1 || g.EdgeTotal != 1 {
		t.Fatalf("committed totals were not published: %+v", g)
	}
	got, found, err := ix.UnderstandingGenerationByID(g.ID)
	if err != nil || !found || got.SnapshotDigest != g.SnapshotDigest || got.UnitTotal != 1 || got.EdgeTotal != 1 {
		t.Fatalf("generation roundtrip: found=%v got=%+v err=%v", found, got, err)
	}
	exact, found, err := ix.UnderstandingForSnapshot("repo", "checkout", g.SnapshotDigest, g.StructuralSchema, g.AnalyzerBundleDigest, "none", "")
	if err != nil || !found || exact.ID != g.ID {
		t.Fatalf("exact selection: found=%v got=%+v err=%v", found, exact, err)
	}
	if _, found, err := ix.UnderstandingForSnapshot("repo", "checkout", g.SnapshotDigest, g.StructuralSchema, "different", "none", ""); err != nil || found {
		t.Fatalf("selection fell back across analyzer identity: found=%v err=%v", found, err)
	}
	units, total, err := ix.UnderstandingUnits(g.ID, 0, 10)
	if err != nil || total != 1 || len(units) != 1 || units[0].Path != "a.go" {
		t.Fatalf("unit page: total=%d units=%+v err=%v", total, units, err)
	}
	edges, total, err := ix.UnderstandingEdges(g.ID, "file", "a.go", 0, 10)
	if err != nil || total != 1 || len(edges) != 1 || edges[0].ToRef != "example/a" {
		t.Fatalf("edge page: total=%d edges=%+v err=%v", total, edges, err)
	}
	coverage, err := ix.UnderstandingCoverage(g.ID)
	if err != nil || len(coverage) != 1 || coverage[0].Family != "package_dependency" {
		t.Fatalf("coverage: %+v err=%v", coverage, err)
	}
}

func TestUnderstandingCallersUsesExactIncomingSymbolEdges(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	g := completeUnderstanding()
	g.Edges = []UnderstandingEdge{
		{FromKind: "symbol", FromRef: "go-v1::function:Caller", Relation: "symbol_calls_symbol",
			ToKind: "symbol", ToRef: "go-v1::function:Target", SourcePath: "caller.go", SourceLine: 12,
			Provenance: "measured", AnalyzerID: "go-v1", EvidenceDigest: "sha256-v1:call"},
		{FromKind: "symbol", FromRef: "go-v1::function:Target", Relation: "symbol_calls_symbol",
			ToKind: "symbol", ToRef: "go-v1::function:Other", SourcePath: "a.go", SourceLine: 8,
			Provenance: "measured", AnalyzerID: "go-v1", EvidenceDigest: "sha256-v1:outgoing"},
		{FromKind: "file", FromRef: "caller.go", Relation: "file_references_symbol",
			ToKind: "symbol", ToRef: "go-v1::function:Target", SourcePath: "caller.go", SourceLine: 12,
			Provenance: "measured", AnalyzerID: "go-v1", EvidenceDigest: "sha256-v1:reference"},
		{FromKind: "file", FromRef: "a.go", Relation: "file_in_package", ToKind: "package", ToRef: "example/a",
			SourcePath: "a.go", SourceLine: 1, Provenance: "measured", AnalyzerID: "go-v1", EvidenceDigest: "sha256-v1:package"},
		{FromKind: "package", FromRef: "example/caller", Relation: "package_depends_on", ToKind: "package", ToRef: "example/a",
			SourcePath: "caller.go", SourceLine: 2, Provenance: "measured", AnalyzerID: "go-v1", EvidenceDigest: "sha256-v1:dependency"},
	}
	g.Coverage = []UnderstandingCoverage{{Family: "symbol_call", State: "complete", AnalyzerID: "go-v1", Attempted: 2, Produced: 2}}
	if err := ix.AppendUnderstanding(&g); err != nil {
		t.Fatal(err)
	}
	edges, total, err := ix.UnderstandingCallers(g.ID, []string{"go-v1::function:Target"}, 0, 10)
	if err != nil || total != 1 || len(edges) != 1 || edges[0].FromRef != "go-v1::function:Caller" {
		t.Fatalf("callers total=%d edges=%+v err=%v", total, edges, err)
	}
	dependencies, err := ix.UnderstandingDependentPackageValues(g.ID, []string{"a.go"}, 10)
	if err != nil || dependencies.DependentPackageTotal != 1 || len(dependencies.DependentPackages) != 1 ||
		dependencies.DependentPackages[0] != "example/caller" || dependencies.ReferencingFileTotal != 1 ||
		len(dependencies.ReferencingFiles) != 1 || dependencies.ReferencingFiles[0] != "caller.go" {
		t.Fatalf("dependent package values=%+v err=%v", dependencies, err)
	}
}

func TestUnderstandingDependentPackageValuesKeepsExactTotalsAboveValueLimit(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	g := completeUnderstanding()
	g.Edges = []UnderstandingEdge{{FromKind: "file", FromRef: "a.go", Relation: "file_in_package",
		ToKind: "package", ToRef: "example/a", SourcePath: "a.go", SourceLine: 1,
		Provenance: "measured", AnalyzerID: "fixture-v1", EvidenceDigest: "sha256-v1:package"}}
	for i := 0; i < 12; i++ {
		g.Edges = append(g.Edges, UnderstandingEdge{FromKind: "package", FromRef: fmt.Sprintf("example/dependent-%02d", i),
			Relation: "package_depends_on", ToKind: "package", ToRef: "example/a",
			SourcePath: fmt.Sprintf("dependent-%02d.go", i), SourceLine: 2,
			Provenance: "measured", AnalyzerID: "fixture-v1", EvidenceDigest: fmt.Sprintf("sha256-v1:dependency-%02d", i)})
	}
	if err := ix.AppendUnderstanding(&g); err != nil {
		t.Fatal(err)
	}
	values, err := ix.UnderstandingDependentPackageValues(g.ID, []string{"a.go"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if values.DependentPackageTotal != 12 || len(values.DependentPackages) != 10 ||
		values.ReferencingFileTotal != 12 || len(values.ReferencingFiles) != 10 {
		t.Fatalf("bounded dependency population lost totals: %+v", values)
	}
}

func TestUnderstandingCompleteReplayReturnsExistingGeneration(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	first := completeUnderstanding()
	if err := ix.AppendUnderstanding(&first); err != nil {
		t.Fatal(err)
	}
	replay := completeUnderstanding()
	replay.StartedAt, replay.EndedAt = 20, 21
	if err := ix.AppendUnderstanding(&replay); err != nil {
		t.Fatalf("same-fact replay failed: %v", err)
	}
	if replay.ID != first.ID {
		t.Fatalf("same identity appended generation %d instead of reusing %d", replay.ID, first.ID)
	}
	_, total, err := ix.UnderstandingGenerations("repo", "checkout", 10)
	if err != nil || total != 1 {
		t.Fatalf("replay population=%d err=%v", total, err)
	}
	collision := completeUnderstanding()
	collision.HeadRevision = "different-head"
	if err := ix.AppendUnderstanding(&collision); err == nil || !strings.Contains(err.Error(), "identity collision") {
		t.Fatalf("conflicting replay error=%v", err)
	}
}

func TestUnderstandingFailedAttemptHasNoFacts(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	g := UnderstandingGeneration{
		RepositoryID: "repo", CheckoutID: "checkout", CheckoutRoot: "/repo",
		Status: "failed", ConventionState: "none", StartedAt: 10, EndedAt: 12,
		LimitationCode: "source_changed", Limitation: "repository changed during scan",
	}
	if err := ix.AppendUnderstanding(&g); err != nil {
		t.Fatal(err)
	}
	if g.ID == 0 {
		t.Fatal("failed attempt was not recorded")
	}
	g.Units = []UnderstandingUnit{{Path: "a.go"}}
	if err := ix.AppendUnderstanding(&g); err == nil {
		t.Fatal("failed attempt accepted child facts")
	}
}

func TestUnderstandingDuplicateChildRollsBackAndDoesNotPublishID(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	g := completeUnderstanding()
	g.Units = append(g.Units, g.Units[0])
	if err := ix.AppendUnderstanding(&g); err == nil {
		t.Fatal("duplicate unit transaction succeeded")
	}
	if g.ID != 0 {
		t.Fatalf("failed append published uncommitted generation id %d", g.ID)
	}
	_, total, err := ix.UnderstandingGenerations("repo", "checkout", 10)
	if err != nil || total != 0 {
		t.Fatalf("failed append left partial generation: total=%d err=%v", total, err)
	}
}

func TestUnderstandingLimitsAndDatabaseConstraints(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	g := completeUnderstanding()
	incompleteIdentity := g
	incompleteIdentity.StructuralSchema = ""
	if err := ix.AppendUnderstanding(&incompleteIdentity); err == nil {
		t.Fatal("complete generation without structural identity accepted")
	}
	g.Units[0].DescriptorJSON = strings.Repeat("x", maxUnderstandingDescriptorBytes+1)
	if err := ix.AppendUnderstanding(&g); err == nil {
		t.Fatal("oversized descriptor accepted")
	}
	g = completeUnderstanding()
	g.Edges[0].Relation = "file_declares_symbol"
	g.Edges[0].ToKind = "package"
	if err := ix.AppendUnderstanding(&g); err == nil {
		t.Fatal("invalid relation shape accepted")
	}
	g = completeUnderstanding()
	g.ConventionState = "explicit"
	if err := ix.AppendUnderstanding(&g); err == nil {
		t.Fatal("explicit convention without identity accepted")
	}
}

func appendUnderstandingCheckpointFixture(t *testing.T, ix *Index, sessionID, repositoryID,
	checkoutID, kind, requestID, digest string, requestedAt, endedAt int64) SessionCheckpoint {
	t.Helper()
	boundaryClass := "settled"
	switch kind {
	case "attachment", "pre-mutation":
		boundaryClass = "direct-pre-release"
	case "closing", "timeout":
		boundaryClass = "exact-close"
	}
	tx, err := ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, _, err := tx.EnsureSessionCheckpoint(SessionCheckpoint{SessionID: sessionID,
		ScopeKey: checkoutID, Kind: kind, RequestID: requestID, WorkingDirectory: "/repo/" + checkoutID,
		RepositoryID: repositoryID, CheckoutID: checkoutID, CheckoutRoot: "/repo/" + checkoutID,
		Status: "pending", BoundaryClass: boundaryClass, RequestedAt: requestedAt})
	if err == nil {
		err = tx.Commit()
	} else {
		_ = tx.Rollback()
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, won, claimErr := ix.ClaimSessionCheckpoint(checkpoint.ID, requestedAt, 0, boundaryClass); claimErr != nil || !won {
		t.Fatalf("claim checkpoint won=%v err=%v", won, claimErr)
	}
	record := &ChangeRecord{SessionID: sessionID, RepositoryID: repositoryID,
		CheckoutID: checkoutID, RepositoryIdentityKind: "local-sha256", CheckoutRoot: "/repo/" + checkoutID,
		Kind: "revision", EvidenceClass: "observed", SourceKind: "git", SourceRef: "HEAD",
		SourceDisplay: "Git", SourceDigest: digest, RecordedAt: endedAt,
		CaptureStartedAt: requestedAt, CaptureEndedAt: endedAt, BaseRevision: "base",
		HeadRevision: "head", SnapshotDigest: digest, CaptureAttempts: 1}
	if err := ix.CompleteSessionCheckpoint(checkpoint.ID, record, endedAt); err != nil {
		t.Fatal(err)
	}
	completed, err := ix.SessionCheckpointByID(checkpoint.ID)
	if err != nil {
		t.Fatal(err)
	}
	return completed
}

func TestUnderstandingBoundaryAndRecoveryQueriesAreExactAndBounded(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	attachment := appendUnderstandingCheckpointFixture(t, ix, "session", "repo-a", "checkout-a",
		"attachment", "attachment-a", "git-tree-v2-sha256:a1", 1, 10)
	_ = appendUnderstandingCheckpointFixture(t, ix, "session", "repo-a", "checkout-a",
		"settled", "settled-a", "git-tree-v2-sha256:a2", 11, 20)
	currentA := appendUnderstandingCheckpointFixture(t, ix, "session", "repo-a", "checkout-a",
		"closing", "closing-a", "git-tree-v2-sha256:a3", 21, 30)
	currentB := appendUnderstandingCheckpointFixture(t, ix, "session", "repo-b", "checkout-b",
		"settled", "settled-b", "git-tree-v2-sha256:b1", 31, 40)

	checkouts, err := ix.SessionUnderstandingCheckouts("session", 10)
	if err != nil || len(checkouts) != 2 || checkouts[0].CheckoutID != "checkout-a" || checkouts[1].CheckoutID != "checkout-b" {
		t.Fatalf("checkout identities=%+v err=%v", checkouts, err)
	}
	baseline, found, current, currentFound, err := ix.SessionUnderstandingBoundaries("session", "repo-a", "checkout-a")
	if err != nil || !found || !currentFound || baseline.ID != attachment.ID || current.ID != currentA.ID {
		t.Fatalf("boundaries baseline=%+v/%v current=%+v/%v err=%v", baseline, found, current, currentFound, err)
	}

	complete := completeUnderstanding()
	complete.RepositoryID, complete.CheckoutID, complete.CheckoutRoot = "repo-a", "checkout-a", "/repo/checkout-a"
	complete.SnapshotDigest = "git-tree-v2-sha256:a3"
	complete.StructuralSchema, complete.AnalyzerBundleDigest = "schema", "bundle"
	if err := ix.AppendUnderstanding(&complete); err != nil {
		t.Fatal(err)
	}
	needed, err := ix.RecentCheckpointsNeedingUnderstanding(0, 5_000, "schema", "bundle", 10)
	if err != nil || len(needed) != 1 || needed[0].ID != currentB.ID {
		t.Fatalf("recovery after complete=%+v err=%v", needed, err)
	}
	failed := UnderstandingGeneration{RepositoryID: "repo-b", CheckoutID: "checkout-b",
		CheckoutRoot: "/repo/checkout-b", Status: "failed", SnapshotProtocol: "git-tree-v2",
		SnapshotDigest: "git-tree-v2-sha256:b1", StructuralSchema: "schema",
		AnalyzerBundleDigest: "bundle", ConventionState: "none", StartedAt: 9_000,
		EndedAt: 10_000, LimitationCode: "fixture", Limitation: "fixture"}
	if err := ix.AppendUnderstanding(&failed); err != nil {
		t.Fatal(err)
	}
	needed, err = ix.RecentCheckpointsNeedingUnderstanding(0, 5_000, "schema", "bundle", 10)
	if err != nil || len(needed) != 0 {
		t.Fatalf("recent failed attempt did not back off: %+v err=%v", needed, err)
	}
	needed, err = ix.RecentCheckpointsNeedingUnderstanding(0, 20_000, "schema", "bundle", 1)
	if err != nil || len(needed) != 1 || needed[0].ID != currentB.ID {
		t.Fatalf("expired failed attempt not eligible: %+v err=%v", needed, err)
	}
}

func TestUnderstandingBoundedPathLookupsValidateIdentityAndOmitDescriptorsFromIndex(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	generation := completeUnderstanding()
	if err := ix.AppendUnderstanding(&generation); err != nil {
		t.Fatal(err)
	}
	units, err := ix.UnderstandingUnitsForPaths(generation.ID, []string{"a.go", "a.go"})
	if err != nil || len(units) != 1 || units[0].DescriptorJSON == "" {
		t.Fatalf("selected units=%+v err=%v", units, err)
	}
	if _, err := ix.UnderstandingUnitsForPaths(generation.ID, []string{"../escape.go"}); err == nil {
		t.Fatal("unsafe path lookup was accepted")
	}
	index, err := ix.UnderstandingUnitIndex(generation.ID)
	if err != nil || len(index) != 1 || index[0].DescriptorJSON != "" {
		t.Fatalf("unit index=%+v err=%v", index, err)
	}
}

func TestUnderstandingGenerationHistoryTotalIsExactAbovePageLimit(t *testing.T) {
	ix, err := Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	tx, err := ix.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1005; i++ {
		if _, err := tx.Exec(`INSERT INTO understanding_generation(repository_id,checkout_id,checkout_root,status,convention_state,started_at,ended_at,limitation_code,limitation) VALUES('repo','checkout','/repo','failed','none',?,?, 'fixture','fixture')`, i, i); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	gens, total, err := ix.UnderstandingGenerations("repo", "checkout", 25)
	if err != nil || len(gens) != 25 || total != 1005 {
		t.Fatalf("history page/total: returned=%d total=%d err=%v", len(gens), total, err)
	}
}

func TestV3UpgradePreservesChangeEvidenceAndAddsUnderstanding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	r := &ChangeRecord{SessionID: "old", RepositoryID: "repo", CheckoutID: "checkout", RepositoryIdentityKind: "local-sha256", CheckoutRoot: "/repo", Kind: "implementation", EvidenceClass: "claimed", SourceKind: "file", SourceRef: "plan.md", SourceDisplay: "plan.md", SourceDigest: "sha256-v1:x", RecordedAt: 1}
	if err := ix.AppendChange(r); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.db.Exec(`DROP TRIGGER understanding_unit_complete; DROP TRIGGER understanding_edge_complete; DROP TRIGGER understanding_coverage_complete; DROP TABLE understanding_coverage; DROP TABLE understanding_edge; DROP TABLE understanding_unit; DROP TABLE understanding_generation; PRAGMA user_version=3`); err != nil {
		t.Fatal(err)
	}
	ix.Close()
	ix, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	changes, total, err := ix.ChangeRecordsForSession("old", 10)
	if err != nil || total != 1 || len(changes) != 1 {
		t.Fatalf("v3 evidence not preserved: total=%d changes=%+v err=%v", total, changes, err)
	}
	g := completeUnderstanding()
	if err := ix.AppendUnderstanding(&g); err != nil {
		t.Fatalf("v4 understanding unavailable after migration: %v", err)
	}
}

func TestV4UpgradePreservesCoverageAndAllowsResponsibilityFingerprint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	old := completeUnderstanding()
	if err := ix.AppendUnderstanding(&old); err != nil {
		t.Fatal(err)
	}
	downgradeUnderstandingCoverageToV4(t, ix)
	ix.Close()

	ix, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	coverage, err := ix.UnderstandingCoverage(old.ID)
	if err != nil || len(coverage) != 1 || coverage[0].Family != "package_dependency" {
		t.Fatalf("v4 coverage not preserved: %+v err=%v", coverage, err)
	}
	current := completeUnderstanding()
	current.StartedAt = 20
	current.EndedAt = 21
	current.SnapshotDigest = "git-tree-v2-sha256:current"
	current.Coverage = append(current.Coverage, UnderstandingCoverage{Family: "responsibility_fingerprint", State: "complete", AnalyzerID: "go-ast-v2", Attempted: 1, Produced: 1})
	if err := ix.AppendUnderstanding(&current); err != nil {
		t.Fatalf("v5 responsibility coverage unavailable: %v", err)
	}
	if _, err := ix.db.Exec(`INSERT INTO understanding_coverage(generation_id,family,state,analyzer_id) VALUES(?,?,?,?)`, current.ID, "invented_family", "unsupported", "fixture"); err == nil {
		t.Fatal("v5 coverage constraint accepted an unknown framework family")
	}
}

func TestV16UpgradeEnforcesCompleteUnderstandingIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ix.db.Exec(`DROP INDEX understanding_generation_complete_identity; PRAGMA user_version=15`); err != nil {
		t.Fatal(err)
	}
	ix.Close()
	ix, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	var version int
	if err := ix.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("schema stamp=%d want=%d err=%v", version, SchemaVersion, err)
	}
	var uniqueIndex int
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index'
		AND name='understanding_generation_complete_identity'`).Scan(&uniqueIndex); err != nil || uniqueIndex != 1 {
		t.Fatalf("v16 unique index=%d err=%v", uniqueIndex, err)
	}
	for _, name := range []string{"session_checkpoint_understanding_v16", "session_checkpoint_recovery_v16"} {
		var count int
		if err := ix.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&count); err != nil || count != 1 {
			t.Fatalf("v16 index %q count=%d err=%v", name, count, err)
		}
	}
}

func TestV16UpgradeRefusesDuplicateCompleteUnderstandingIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	first := completeUnderstanding()
	if err := ix.AppendUnderstanding(&first); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.db.Exec(`DROP INDEX understanding_generation_complete_identity`); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.db.Exec(`INSERT INTO understanding_generation(
		repository_id,checkout_id,checkout_root,status,snapshot_protocol,snapshot_digest,
		base_revision,head_revision,structural_schema,analyzer_bundle_digest,convention_state,
		started_at,ended_at,path_total,unit_total,edge_total,error_total)
		SELECT repository_id,checkout_id,checkout_root,status,snapshot_protocol,snapshot_digest,
		base_revision,head_revision,structural_schema,analyzer_bundle_digest,convention_state,
		started_at+10,ended_at+10,path_total,0,0,error_total
		FROM understanding_generation WHERE id=?`, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.db.Exec(`PRAGMA user_version=15`); err != nil {
		t.Fatal(err)
	}
	ix.Close()
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "duplicate complete generation identities") {
		t.Fatalf("duplicate v15 migration error=%v", err)
	}
}

func TestV18UpgradePreservesFactsAndAcceptsGenericCallsAndCoverageCounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	old := completeUnderstanding()
	if err := ix.AppendUnderstanding(&old); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.db.Exec(`
DROP TRIGGER understanding_coverage_complete;
ALTER TABLE understanding_coverage RENAME TO understanding_coverage_v18;
CREATE TABLE understanding_coverage(
  generation_id INTEGER NOT NULL REFERENCES understanding_generation(id) ON DELETE RESTRICT,
  family TEXT NOT NULL CHECK(family IN ('file_inventory','package_dependency','symbol_declaration','responsibility_fingerprint','symbol_call','document_reference','configuration_effect','data_effect','policy_effect','journey_effect')),
  state TEXT NOT NULL CHECK(state IN ('complete','partial','unsupported','failed')),
  analyzer_id TEXT NOT NULL,
  attempted INTEGER NOT NULL DEFAULT 0 CHECK(attempted >= 0),
  produced INTEGER NOT NULL DEFAULT 0 CHECK(produced >= 0),
  errors INTEGER NOT NULL DEFAULT 0 CHECK(errors >= 0),
  reason TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(generation_id,family,analyzer_id),
  CHECK((state='complete' AND reason='') OR state!='complete')
);
INSERT INTO understanding_coverage SELECT generation_id,family,state,analyzer_id,attempted,produced,errors,reason FROM understanding_coverage_v18;
DROP TABLE understanding_coverage_v18;
CREATE TRIGGER understanding_coverage_complete BEFORE INSERT ON understanding_coverage BEGIN
  SELECT CASE WHEN COALESCE((SELECT status FROM understanding_generation WHERE id=NEW.generation_id),'')!='complete' THEN RAISE(ABORT,'understanding coverage requires a complete generation') END;
END;
DROP TRIGGER understanding_edge_complete;
DROP INDEX understanding_edge_from;
DROP INDEX understanding_edge_to;
ALTER TABLE understanding_edge RENAME TO understanding_edge_v18;
CREATE TABLE understanding_edge(
  generation_id INTEGER NOT NULL REFERENCES understanding_generation(id) ON DELETE RESTRICT,
  from_kind TEXT NOT NULL CHECK(from_kind IN ('file','package','symbol','document','item')),
  from_ref TEXT NOT NULL,
  relation TEXT NOT NULL CHECK(relation IN ('file_in_package','package_depends_on','file_declares_symbol','document_references_file','document_defines_item','item_references_file')),
  to_kind TEXT NOT NULL CHECK(to_kind IN ('file','package','symbol','document','item')),
  to_ref TEXT NOT NULL, source_path TEXT NOT NULL DEFAULT '', source_line INTEGER NOT NULL DEFAULT 0 CHECK(source_line >= 0),
  provenance TEXT NOT NULL CHECK(provenance='measured'), analyzer_id TEXT NOT NULL, evidence_digest TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(generation_id,from_kind,from_ref,relation,to_kind,to_ref,source_path,source_line,analyzer_id),
  CHECK((relation='file_in_package' AND from_kind='file' AND to_kind='package') OR (relation='package_depends_on' AND from_kind='package' AND to_kind='package') OR (relation='file_declares_symbol' AND from_kind='file' AND to_kind='symbol') OR (relation='document_references_file' AND from_kind='document' AND to_kind='file') OR (relation='document_defines_item' AND from_kind='document' AND to_kind='item') OR (relation='item_references_file' AND from_kind='item' AND to_kind='file'))
);
INSERT INTO understanding_edge SELECT * FROM understanding_edge_v18;
DROP TABLE understanding_edge_v18;
CREATE INDEX understanding_edge_from ON understanding_edge(generation_id,from_kind,from_ref);
CREATE INDEX understanding_edge_to ON understanding_edge(generation_id,to_kind,to_ref);
CREATE TRIGGER understanding_edge_complete BEFORE INSERT ON understanding_edge BEGIN
  SELECT CASE WHEN COALESCE((SELECT status FROM understanding_generation WHERE id=NEW.generation_id),'')!='complete' THEN RAISE(ABORT,'understanding edges require a complete generation') END;
END;
PRAGMA user_version=17`); err != nil {
		t.Fatal(err)
	}
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}
	ix, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	coverage, err := ix.UnderstandingCoverage(old.ID)
	if err != nil || len(coverage) != 1 || coverage[0].Unresolved != 0 || coverage[0].Ambiguous != 0 {
		t.Fatalf("v18 coverage preservation: %+v err=%v", coverage, err)
	}
	current := completeUnderstanding()
	current.SnapshotDigest = "git-tree-v2-sha256:v18"
	current.StartedAt, current.EndedAt = 20, 21
	current.Edges = []UnderstandingEdge{{FromKind: "symbol", FromRef: "php-v1::method:A", Relation: "symbol_calls_symbol",
		ToKind: "symbol", ToRef: "php-v1::method:B", SourcePath: "a.php", SourceLine: 12,
		Provenance: "measured", AnalyzerID: "php-v1", EvidenceDigest: "sha256-v1:call"}}
	current.Coverage = []UnderstandingCoverage{{Family: "symbol_call", State: "partial", AnalyzerID: "php-v1",
		Attempted: 4, Produced: 1, Unresolved: 2, Ambiguous: 1, Reason: "dynamic calls remain"}}
	if err := ix.AppendUnderstanding(&current); err != nil {
		t.Fatal(err)
	}
	got, err := ix.UnderstandingCoverage(current.ID)
	if err != nil || len(got) != 1 || got[0].Unresolved != 2 || got[0].Ambiguous != 1 {
		t.Fatalf("v18 coverage roundtrip: %+v err=%v", got, err)
	}
}

func TestV5CoverageMigrationFailureRollsBackTableAndStamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	downgradeUnderstandingCoverageToV4(t, ix)
	ix.Close()
	migrationTestHook = func(schemaDB) error { return errors.New("injected v5 migration failure") }
	if _, err := Open(path); err == nil {
		t.Fatal("injected v5 migration succeeded")
	}
	migrationTestHook = nil
	raw, err := driver.Open("file:"+path, fts5.Register)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var version int
	if err := raw.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 4 {
		t.Fatalf("failed v5 migration stamped version=%d err=%v", version, err)
	}
	var tableSQL string
	if err := raw.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='understanding_coverage'`).Scan(&tableSQL); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(tableSQL, "responsibility_fingerprint") {
		t.Fatal("failed v5 migration retained the replacement coverage table")
	}
}

func downgradeUnderstandingCoverageToV4(t *testing.T, ix *Index) {
	t.Helper()
	_, err := ix.db.Exec(`
DROP TRIGGER understanding_coverage_complete;
ALTER TABLE understanding_coverage RENAME TO understanding_coverage_v5;
CREATE TABLE understanding_coverage(
  generation_id INTEGER NOT NULL REFERENCES understanding_generation(id) ON DELETE RESTRICT,
  family TEXT NOT NULL CHECK(family IN (
    'file_inventory','package_dependency','symbol_declaration','symbol_call',
    'document_reference','configuration_effect','data_effect','policy_effect','journey_effect'
  )),
  state TEXT NOT NULL CHECK(state IN ('complete','partial','unsupported','failed')),
  analyzer_id TEXT NOT NULL,
  attempted INTEGER NOT NULL DEFAULT 0 CHECK(attempted >= 0),
  produced INTEGER NOT NULL DEFAULT 0 CHECK(produced >= 0),
  errors INTEGER NOT NULL DEFAULT 0 CHECK(errors >= 0),
  reason TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(generation_id,family,analyzer_id),
  CHECK((state='complete' AND reason='') OR state!='complete')
);
INSERT INTO understanding_coverage(generation_id,family,state,analyzer_id,attempted,produced,errors,reason)
  SELECT generation_id,family,state,analyzer_id,attempted,produced,errors,reason FROM understanding_coverage_v5;
DROP TABLE understanding_coverage_v5;
CREATE TRIGGER understanding_coverage_complete BEFORE INSERT ON understanding_coverage
BEGIN
  SELECT CASE WHEN COALESCE((SELECT status FROM understanding_generation WHERE id=NEW.generation_id),'')!='complete'
    THEN RAISE(ABORT,'understanding coverage requires a complete generation') END;
END;
PRAGMA user_version=4`)
	if err != nil {
		t.Fatal(err)
	}
}
