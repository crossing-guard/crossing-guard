package codemap

import (
	"strings"
	"testing"
)

func measuredInt(value int) *int { return &value }

func comparisonDescriptor(path, analyzer, hash string, declarations ...Declaration) *UnitDescriptor {
	return &UnitDescriptor{Identity: Identity{Path: path, AnalyzerID: analyzer}, SourceHash: hash,
		Symbol: Symbol{Declarations: declarations}}
}

func comparisonDeclaration(identity, name, digest string, line, end, span int, cyclomatic *int) Declaration {
	return Declaration{Identity: identity, Name: name, Kind: "function", SourceDigest: digest,
		Line: line, EndLine: end, SourceSpanLines: measuredInt(span), Cyclomatic: cyclomatic}
}

func TestCompareUnitsReportsExactDeclarationStatesAndMetrics(t *testing.T) {
	before := comparisonDescriptor("sample.fixture", "fixture-v3", "sha256-v1:before",
		comparisonDeclaration("function:Changed", "Changed", "source:before", 2, 4, 3, measuredInt(2)),
		comparisonDeclaration("function:Moved", "Moved", "source:same", 8, 9, 2, measuredInt(1)),
		comparisonDeclaration("function:Removed", "Removed", "source:removed", 12, 12, 1, measuredInt(1)))
	after := comparisonDescriptor("sample.fixture", "fixture-v3", "sha256-v1:after",
		comparisonDeclaration("function:Added", "Added", "source:added", 2, 2, 1, measuredInt(1)),
		comparisonDeclaration("function:Changed", "Changed", "source:after", 5, 10, 6, measuredInt(4)),
		comparisonDeclaration("function:Moved", "Moved", "source:same", 14, 15, 2, measuredInt(1)))

	change, err := CompareUnits(before, after)
	if err != nil {
		t.Fatal(err)
	}
	if change.State != "modified" || change.Path != "sample.fixture" || len(change.Declarations) != 4 {
		t.Fatalf("unit change=%+v", change)
	}
	byIdentity := map[string]DeclarationChange{}
	for _, declaration := range change.Declarations {
		byIdentity[declaration.Identity] = declaration
	}
	if byIdentity["function:Added"].State != "added" || byIdentity["function:Removed"].State != "removed" || byIdentity["function:Moved"].State != "moved" {
		t.Fatalf("declaration states=%+v", byIdentity)
	}
	changed := byIdentity["function:Changed"]
	if changed.State != "modified" || changed.SourceSpanLines.Delta == nil || *changed.SourceSpanLines.Delta != 3 ||
		changed.Cyclomatic.Delta == nil || *changed.Cyclomatic.Delta != 2 {
		t.Fatalf("changed declaration metrics=%+v", changed)
	}
}

func TestCompareUnitsRejectsMixedAndIncompleteIdentity(t *testing.T) {
	declaration := comparisonDeclaration("function:One", "One", "source:one", 1, 1, 1, nil)
	before := comparisonDescriptor("sample.fixture", "fixture-v3", "sha256-v1:before", declaration)
	after := comparisonDescriptor("sample.fixture", "fixture-v4", "sha256-v1:after", declaration)
	if _, err := CompareUnits(before, after); err == nil || !strings.Contains(err.Error(), "analyzer identity") {
		t.Fatalf("mixed analyzer error=%v", err)
	}
	after.Identity.AnalyzerID = before.Identity.AnalyzerID
	after.Symbol.Declarations = append(after.Symbol.Declarations, declaration)
	if _, err := CompareUnits(before, after); err == nil || !strings.Contains(err.Error(), "duplicate declaration identity") {
		t.Fatalf("duplicate declaration error=%v", err)
	}
	after.Symbol.Declarations = []Declaration{{Name: "legacy"}}
	if _, err := CompareUnits(before, after); err == nil || !strings.Contains(err.Error(), "incomplete declaration") {
		t.Fatalf("legacy declaration error=%v", err)
	}
}

func TestCompareUnitsSeparatesUnavailableMetricsFromZero(t *testing.T) {
	before := comparisonDescriptor("sample.fixture", "fixture-v3", "sha256-v1:before",
		comparisonDeclaration("value:One", "One", "source:before", 1, 1, 1, nil))
	after := comparisonDescriptor("sample.fixture", "fixture-v3", "sha256-v1:after",
		comparisonDeclaration("value:One", "One", "source:after", 1, 1, 1, measuredInt(0)))
	change, err := CompareUnits(before, after)
	if err != nil {
		t.Fatal(err)
	}
	metric := change.Declarations[0].Cyclomatic
	if metric.Before != nil || metric.After == nil || *metric.After != 0 || metric.Delta != nil {
		t.Fatalf("optional metric collapsed unavailable into zero: %+v", metric)
	}
}
