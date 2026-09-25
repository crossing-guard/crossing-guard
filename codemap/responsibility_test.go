package codemap

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func responsibilityFixture(path, analyzer string, declarations []Declaration, internal, external []string) ResponsibilityUnit {
	return ResponsibilityUnit{Path: path, Descriptor: UnitDescriptor{
		Identity:  Identity{Path: path, Language: "fixture", AnalyzerID: analyzer},
		Symbol:    Symbol{Declarations: declarations},
		Structure: Structure{Imports: internal, External: external},
	}}
}

func storedResponsibilityFixture(t *testing.T, unit ResponsibilityUnit) StoredResponsibilityUnit {
	t.Helper()
	body, err := json.Marshal(unit.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	return StoredResponsibilityUnit{Path: unit.Path, DescriptorJSON: string(body)}
}

func TestProjectResponsibilityOwnsPortableStateAndPaging(t *testing.T) {
	center := responsibilityFixture("center.fixture", "fixture-v1", []Declaration{{Name: "Same"}}, nil, nil)
	other := responsibilityFixture("other.fixture", "fixture-v1", []Declaration{{Name: "Same"}}, nil, nil)
	stored := []StoredResponsibilityUnit{storedResponsibilityFixture(t, center), storedResponsibilityFixture(t, other)}
	coverage := []ResponsibilityCoverageFact{{Family: string(CapabilityResponsibilityFingerprint), State: "complete", AnalyzerID: "fixture-v1"}}

	exact := ProjectResponsibility("center.fixture", stored, coverage, 0, 1)
	if exact.State != "exact" || exact.AnalyzerID != "fixture-v1" || exact.Page == nil || exact.Page.Total != 1 || len(exact.Rows) != 1 {
		t.Fatalf("exact projection=%+v", exact)
	}
	partialCoverage := append([]ResponsibilityCoverageFact(nil), coverage...)
	partialCoverage[0].State, partialCoverage[0].Reason = "partial", "one source failed"
	partial := ProjectResponsibility("center.fixture", stored, partialCoverage, 1, 1)
	if partial.State != "partial" || partial.Reason != "one source failed" || partial.Page == nil || partial.Page.Offset != 1 || len(partial.Rows) != 0 {
		t.Fatalf("partial projection=%+v", partial)
	}
	unsupportedCoverage := append([]ResponsibilityCoverageFact(nil), coverage...)
	unsupportedCoverage[0].State = "unsupported"
	unsupported := ProjectResponsibility("center.fixture", stored, unsupportedCoverage, 0, 1)
	if unsupported.State != "unsupported" || unsupported.Reason == "" || unsupported.Page != nil {
		t.Fatalf("unsupported projection=%+v", unsupported)
	}
	missingCoverage := ProjectResponsibility("center.fixture", stored, nil, 0, 1)
	if missingCoverage.State != "failed" || !strings.Contains(missingCoverage.Reason, "coverage is missing") {
		t.Fatalf("missing coverage projection=%+v", missingCoverage)
	}
	missingCenter := ProjectResponsibility("missing.fixture", stored, coverage, 0, 1)
	if missingCenter.State != "unavailable" || missingCenter.AnalyzerID != "" {
		t.Fatalf("missing center projection=%+v", missingCenter)
	}
	malformed := append([]StoredResponsibilityUnit(nil), stored...)
	malformed[1].DescriptorJSON = `{bad`
	failed := ProjectResponsibility("center.fixture", malformed, coverage, 0, 1)
	if failed.State != "failed" || !strings.Contains(failed.Reason, "decode responsibility descriptor") {
		t.Fatalf("malformed projection=%+v", failed)
	}
}

func TestResponsibilityCandidatesReturnExactFactsInPathOrder(t *testing.T) {
	center := responsibilityFixture("center.fixture", "fixture-v1", []Declaration{
		{Name: "SharedName", Line: 1},
		{Name: "CenterOnly", Line: 2, BodyShapeDigest: "fixture-shape-v1:shared", BodyShapeNodes: 9},
	}, []string{"internal/shared", "internal/left"}, []string{"external/shared"})
	units := []ResponsibilityUnit{
		responsibilityFixture("z.fixture", "fixture-v1", []Declaration{{Name: "Other", BodyShapeDigest: "fixture-shape-v1:shared", BodyShapeNodes: 9}}, []string{"internal/shared"}, []string{"external/shared"}),
		center,
		responsibilityFixture("a.fixture", "fixture-v1", []Declaration{{Name: "SharedName"}}, []string{"internal/shared"}, nil),
		responsibilityFixture("dependency-only.fixture", "fixture-v1", []Declaration{{Name: "NoMatch"}}, []string{"internal/shared"}, []string{"external/shared"}),
		responsibilityFixture("other-analyzer.fixture", "fixture-v2", []Declaration{{Name: "SharedName", BodyShapeDigest: "fixture-shape-v1:shared", BodyShapeNodes: 9}}, nil, nil),
	}

	rows, page, err := ResponsibilityCandidates(center, units, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || len(rows) != 2 || rows[0].RightPath != "a.fixture" || rows[1].RightPath != "z.fixture" {
		t.Fatalf("candidate population/order=%+v page=%+v", rows, page)
	}
	if rows[0].SharedDeclarations.Total != 1 || rows[0].SharedDeclarations.Values[0] != "SharedName" {
		t.Fatalf("exact declaration basis missing: %+v", rows[0])
	}
	if rows[1].SharedBodyShapes.Total != 1 || len(rows[1].BodyShapes) != 1 || rows[1].BodyShapes[0].Digest != "fixture-shape-v1:shared" {
		t.Fatalf("body-shape basis missing: %+v", rows[1])
	}
	if rows[1].SharedInternalDependencies.Total != 1 || rows[1].SharedExternalDependencies.Total != 1 {
		t.Fatalf("supporting dependency facts missing: %+v", rows[1])
	}
}

func TestResponsibilityCandidatesExposeCapsAndCanonicalizePaging(t *testing.T) {
	leftDeclarations := make([]Declaration, 0, 60)
	rightDeclarations := make([]Declaration, 0, 60)
	for i := 0; i < 60; i++ {
		name := fmt.Sprintf("Shared%02d", i)
		shape := fmt.Sprintf("fixture-shape-v1:%02d", i)
		leftDeclarations = append(leftDeclarations, Declaration{Name: name, BodyShapeDigest: shape, BodyShapeNodes: i + 1})
		rightDeclarations = append(rightDeclarations, Declaration{Name: name, BodyShapeDigest: shape, BodyShapeNodes: i + 1})
	}
	center := responsibilityFixture("center.fixture", "fixture-v1", leftDeclarations, nil, nil)
	other := responsibilityFixture("other.fixture", "fixture-v1", rightDeclarations, nil, nil)
	rows, page, err := ResponsibilityCandidates(center, []ResponsibilityUnit{center, other}, -7, 999)
	if err != nil {
		t.Fatal(err)
	}
	if page.Offset != 0 || page.Limit != 100 || page.Total != 1 || len(rows) != 1 {
		t.Fatalf("hostile page not canonicalized: %+v rows=%d", page, len(rows))
	}
	set := rows[0].SharedDeclarations
	if set.Total != 60 || set.Returned != 50 || set.Exact || len(set.Values) != 50 {
		t.Fatalf("detail cap hidden: %+v", set)
	}
	if shapes := rows[0].SharedBodyShapes; shapes.Total != 60 || shapes.Returned != 50 || shapes.Exact || len(rows[0].BodyShapes) != 50 {
		t.Fatalf("body-shape cap hidden: %+v rows=%d", shapes, len(rows[0].BodyShapes))
	}
	rows, page, err = ResponsibilityCandidates(center, []ResponsibilityUnit{center, other}, 999, 1)
	if err != nil || len(rows) != 0 || page.Offset != 1 || page.Total != 1 {
		t.Fatalf("past-end page not canonicalized: page=%+v rows=%d err=%v", page, len(rows), err)
	}
}

func TestDecodeResponsibilityUnitOwnsPersistedValidation(t *testing.T) {
	descriptor := UnitDescriptor{Identity: Identity{Path: "right.fixture", Language: "fixture", AnalyzerID: "fixture-v1"}}
	body, _ := json.Marshal(descriptor)
	if _, err := DecodeResponsibilityUnit("left.fixture", string(body)); err == nil || !strings.Contains(err.Error(), "disagrees") {
		t.Fatalf("path mismatch error=%v", err)
	}
	descriptor.Identity.Path = "left.fixture"
	descriptor.Identity.AnalyzerID = ""
	body, _ = json.Marshal(descriptor)
	if _, err := DecodeResponsibilityUnit("left.fixture", string(body)); err == nil || !strings.Contains(err.Error(), "analyzer") {
		t.Fatalf("missing analyzer error=%v", err)
	}
	if _, err := DecodeResponsibilityUnit("../escape.fixture", `{}`); err == nil {
		t.Fatal("unsafe store path decoded")
	}
}

func TestResponsibilityCandidatesRejectDuplicatePathsAndMalformedShapes(t *testing.T) {
	center := responsibilityFixture("center.fixture", "fixture-v1", []Declaration{{Name: "Same", BodyShapeDigest: "shape", BodyShapeNodes: 2}}, nil, nil)
	duplicate := responsibilityFixture("center.fixture", "fixture-v1", []Declaration{{Name: "Same"}}, nil, nil)
	if _, _, err := ResponsibilityCandidates(center, []ResponsibilityUnit{center, duplicate}, 0, 10); err == nil {
		t.Fatal("duplicate paths were accepted")
	}
	bad := responsibilityFixture("bad.fixture", "fixture-v1", []Declaration{{Name: "Same", BodyShapeDigest: "shape", BodyShapeNodes: 0}}, nil, nil)
	if _, _, err := ResponsibilityCandidates(center, []ResponsibilityUnit{center, bad}, 0, 10); err == nil {
		t.Fatal("malformed shape fact was accepted")
	}
	conflict := responsibilityFixture("conflict.fixture", "fixture-v1", []Declaration{{Name: "Same", BodyShapeDigest: "shape", BodyShapeNodes: 7}}, nil, nil)
	if _, _, err := ResponsibilityCandidates(center, []ResponsibilityUnit{center, conflict}, 0, 10); err == nil || !strings.Contains(err.Error(), "node count") {
		t.Fatalf("conflicting shape node counts error=%v", err)
	}
	if _, _, err := ResponsibilityCandidates(center, []ResponsibilityUnit{duplicate}, 0, 10); err == nil || !strings.Contains(err.Error(), "disagrees") {
		t.Fatalf("mismatched center error=%v", err)
	}
	if _, _, err := ResponsibilityCandidates(center, []ResponsibilityUnit{responsibilityFixture("other.fixture", "fixture-v1", []Declaration{{Name: "Same"}}, nil, nil)}, 0, 10); err == nil || !strings.Contains(err.Error(), "absent") {
		t.Fatalf("absent center error=%v", err)
	}
	within := responsibilityFixture("within.fixture", "fixture-v1", []Declaration{{Name: "A", BodyShapeDigest: "shape", BodyShapeNodes: 2}, {Name: "B", BodyShapeDigest: "shape", BodyShapeNodes: 3}}, nil, nil)
	if _, _, err := ResponsibilityCandidates(within, []ResponsibilityUnit{within}, 0, 10); err == nil || !strings.Contains(err.Error(), "conflicting node counts") {
		t.Fatalf("within-unit conflicting shape error=%v", err)
	}
}

func TestApplyMechanicsCanonicalizesPersistedDeclarations(t *testing.T) {
	descriptor := &UnitDescriptor{}
	NewDescriber(".", nil).applyMechanics(descriptor, &Mechanics{Declarations: []Declaration{{Name: "z", Line: 1}, {Name: "a", Line: 9}, {Name: "a", Line: 2}}})
	got := descriptor.Symbol.Declarations
	if len(got) != 3 || got[0].Name != "a" || got[0].Line != 2 || got[1].Name != "a" || got[1].Line != 9 || got[2].Name != "z" {
		t.Fatalf("declaration order=%+v", got)
	}
}

func TestCompareStructuralMatchesReportsExactCenteredSetDifference(t *testing.T) {
	coverage := []ResponsibilityCoverageFact{{Family: string(CapabilityResponsibilityFingerprint), State: "complete", AnalyzerID: "fixture-v1"}}
	shape := Declaration{Name: "Center", BodyShapeDigest: "fixture-shape-v1:shared", BodyShapeNodes: 7}
	centerBefore := responsibilityFixture("center.fixture", "fixture-v1", []Declaration{shape}, nil, nil)
	centerAfter := responsibilityFixture("center.fixture", "fixture-v1", []Declaration{shape}, nil, nil)
	oldMatch := responsibilityFixture("old.fixture", "fixture-v1", []Declaration{{Name: "Old", BodyShapeDigest: shape.BodyShapeDigest, BodyShapeNodes: 7}}, nil, nil)
	oldChanged := responsibilityFixture("old.fixture", "fixture-v1", []Declaration{{Name: "Old", BodyShapeDigest: "fixture-shape-v1:other", BodyShapeNodes: 3}}, nil, nil)
	newBefore := responsibilityFixture("new.fixture", "fixture-v1", []Declaration{{Name: "New"}}, nil, nil)
	newMatch := responsibilityFixture("new.fixture", "fixture-v1", []Declaration{{Name: "New", BodyShapeDigest: shape.BodyShapeDigest, BodyShapeNodes: 7}}, nil, nil)
	before := []StoredResponsibilityUnit{storedResponsibilityFixture(t, centerBefore), storedResponsibilityFixture(t, oldMatch), storedResponsibilityFixture(t, newBefore)}
	after := []StoredResponsibilityUnit{storedResponsibilityFixture(t, centerAfter), storedResponsibilityFixture(t, oldChanged), storedResponsibilityFixture(t, newMatch)}

	result := CompareStructuralMatches("center.fixture", before, after, coverage, coverage, 0, 1)
	if result.State != "exact" || result.Page.Total != 2 || result.Page.Limit != 1 || len(result.Rows) != 1 || result.Rows[0].OtherPath != "new.fixture" || result.Rows[0].State != "added" {
		t.Fatalf("first structural page=%+v", result)
	}
	second := CompareStructuralMatches("center.fixture", before, after, coverage, coverage, 1, 1)
	if len(second.Rows) != 1 || second.Rows[0].OtherPath != "old.fixture" || second.Rows[0].State != "removed" {
		t.Fatalf("second structural page=%+v", second)
	}
}

func TestCompareStructuralMatchPopulationReturnsMeasuredChangedCenters(t *testing.T) {
	coverage := []ResponsibilityCoverageFact{{Family: string(CapabilityResponsibilityFingerprint), State: "complete", AnalyzerID: "fixture-v1"}}
	shape := Declaration{Name: "Center", BodyShapeDigest: "fixture-shape-v1:shared", BodyShapeNodes: 7}
	centerBefore := responsibilityFixture("center.fixture", "fixture-v1", []Declaration{shape}, nil, nil)
	centerAfter := responsibilityFixture("center.fixture", "fixture-v1", []Declaration{shape}, nil, nil)
	oldMatch := responsibilityFixture("old.fixture", "fixture-v1", []Declaration{{Name: "Old", BodyShapeDigest: shape.BodyShapeDigest, BodyShapeNodes: 7}}, nil, nil)
	oldChanged := responsibilityFixture("old.fixture", "fixture-v1", []Declaration{{Name: "Old"}}, nil, nil)
	newBefore := responsibilityFixture("new.fixture", "fixture-v1", []Declaration{{Name: "New"}}, nil, nil)
	newMatch := responsibilityFixture("new.fixture", "fixture-v1", []Declaration{{Name: "New", BodyShapeDigest: shape.BodyShapeDigest, BodyShapeNodes: 7}}, nil, nil)
	before := []StoredResponsibilityUnit{storedResponsibilityFixture(t, centerBefore), storedResponsibilityFixture(t, oldMatch), storedResponsibilityFixture(t, newBefore)}
	after := []StoredResponsibilityUnit{storedResponsibilityFixture(t, centerAfter), storedResponsibilityFixture(t, oldChanged), storedResponsibilityFixture(t, newMatch)}

	result := CompareStructuralMatchPopulation([]string{"center.fixture"}, before, after, coverage, coverage, 0, 10)
	if result.State != "exact" || result.Page.Total != 2 || len(result.Rows) != 2 ||
		result.Rows[0].OtherPath != "new.fixture" || result.Rows[0].State != "added" ||
		result.Rows[1].OtherPath != "old.fixture" || result.Rows[1].State != "removed" {
		t.Fatalf("structural population=%+v", result)
	}
}

func TestCompareStructuralMatchesIsBoundedAtTenThousandUnits(t *testing.T) {
	coverage := []ResponsibilityCoverageFact{{Family: string(CapabilityResponsibilityFingerprint), State: "complete", AnalyzerID: "fixture-v1"}}
	shape := Declaration{Name: "Center", BodyShapeDigest: "fixture-shape-v1:shared", BodyShapeNodes: 2}
	before := make([]StoredResponsibilityUnit, 0, maxResponsibilityUnits)
	after := make([]StoredResponsibilityUnit, 0, maxResponsibilityUnits)
	before = append(before, storedResponsibilityFixture(t, responsibilityFixture("center.fixture", "fixture-v1", []Declaration{shape}, nil, nil)))
	after = append(after, storedResponsibilityFixture(t, responsibilityFixture("center.fixture", "fixture-v1", []Declaration{shape}, nil, nil)))
	for index := 1; index < maxResponsibilityUnits; index++ {
		path := fmt.Sprintf("unit-%05d.fixture", index)
		beforeDeclaration := Declaration{Name: "Other"}
		afterDeclaration := Declaration{Name: "Other"}
		if index == maxResponsibilityUnits-1 {
			afterDeclaration = Declaration{Name: "Other", BodyShapeDigest: shape.BodyShapeDigest, BodyShapeNodes: 2}
		}
		before = append(before, storedResponsibilityFixture(t, responsibilityFixture(path, "fixture-v1", []Declaration{beforeDeclaration}, nil, nil)))
		after = append(after, storedResponsibilityFixture(t, responsibilityFixture(path, "fixture-v1", []Declaration{afterDeclaration}, nil, nil)))
	}
	result := CompareStructuralMatches("center.fixture", before, after, coverage, coverage, 0, 100)
	if result.State != "exact" || result.Page.Total != 1 || len(result.Rows) != 1 || result.Rows[0].State != "added" {
		t.Fatalf("bounded structural comparison=%+v", result)
	}
	over := append(append([]StoredResponsibilityUnit(nil), after...), storedResponsibilityFixture(t, responsibilityFixture("over.fixture", "fixture-v1", nil, nil, nil)))
	if got := CompareStructuralMatches("center.fixture", before, over, coverage, coverage, 0, 100); got.State != "failed" || !strings.Contains(got.Reason, "maximum") {
		t.Fatalf("over-limit structural comparison=%+v", got)
	}
}
