package main

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"crossing-guard/analyzers/modulekit"
	"crossing-guard/codemap"
)

func TestPHPCompiledDefinitionMatchesManifest(t *testing.T) {
	body, err := os.ReadFile("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest codemap.AnalyzerModuleManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.ModuleID != phpDefinition.ModuleID || manifest.AnalyzerIdentity != phpDefinition.AnalyzerIdentity ||
		!reflect.DeepEqual(manifest.Languages, phpDefinition.Languages) ||
		!reflect.DeepEqual(manifest.Extensions, phpDefinition.Extensions) ||
		!reflect.DeepEqual(manifest.Capabilities, phpDefinition.Capabilities) {
		t.Fatalf("compiled definition drifted from manifest: manifest=%+v definition=%+v", manifest, phpDefinition)
	}
}

func TestPHPFactsCallsCoverageAndShapes(t *testing.T) {
	source := `<?php
namespace App;
use App\Support\Thing;
function helper() { return 1; }
class Controller {
  public function run() { helper(); $this->local(); $other->dynamic(); }
  private function local() { if (true) { return 2; } return 0; }
}`
	analyses, err := analyzePHP([]modulekit.Source{{Path: "Controller.php", Body: []byte(source)}})
	if err != nil || len(analyses) != 1 || analyses[0].Unit == nil || analyses[0].Failure != nil {
		t.Fatalf("analysis=%+v err=%v", analyses, err)
	}
	analysis := analyses[0]
	if analysis.Unit.Namespace != "App" || len(analysis.Unit.Declarations) < 4 || len(analysis.Edges) != 2 {
		t.Fatalf("PHP facts unit=%+v edges=%+v", analysis.Unit, analysis.Edges)
	}
	if analysis.Unresolved[codemap.CapabilitySymbolCall] == 0 {
		t.Fatalf("dynamic call was presented as resolved: %+v", analysis)
	}
	var complex, shape string
	for _, declaration := range analysis.Unit.Declarations {
		if declaration.Name == "local" {
			if declaration.Cyclomatic == nil || *declaration.Cyclomatic < 2 {
				t.Fatalf("local complexity=%v", declaration.Cyclomatic)
			}
			shape = declaration.BodyShapeDigest
			complex = declaration.Identity
		}
	}
	if complex == "" || shape == "" {
		t.Fatalf("method mechanics absent: %+v", analysis.Unit.Declarations)
	}
	variant := []byte(`<?php namespace App; class Controller { private function renamed() {
      // spelling, formatting, and literal changes do not change structure
      if (false) { return 99; } return 7; } }`)
	changed, err := analyzePHP([]modulekit.Source{{Path: "Variant.php", Body: variant}})
	if err != nil || changed[0].Unit == nil {
		t.Fatal(err)
	}
	if got := changed[0].Unit.Declarations[1].BodyShapeDigest; got != shape {
		t.Fatalf("normalized PHP shape drifted: %s != %s", got, shape)
	}
}

func TestPHPMalformedSourceIsAnExplicitFailure(t *testing.T) {
	analyses, err := analyzePHP([]modulekit.Source{{Path: "bad.php", Body: []byte("<?php function broken(")}})
	if err != nil || len(analyses) != 1 || analyses[0].Failure == nil || analyses[0].Unit != nil {
		t.Fatalf("malformed analysis=%+v err=%v", analyses, err)
	}
}

func TestPHPBoundsOneOversizedSourceWithoutFailingItsPeers(t *testing.T) {
	oversized := make([]byte, maxReferenceSourceBytes+1)
	analyses, err := analyzePHP([]modulekit.Source{
		{Path: "Huge.php", Body: oversized},
		{Path: "Small.php", Body: []byte("<?php function small() { return 1; }")},
	})
	if err != nil || len(analyses) != 2 {
		t.Fatalf("analysis=%+v err=%v", analyses, err)
	}
	byPath := map[string]modulekit.Analysis{}
	for _, analysis := range analyses {
		if analysis.Unit != nil {
			byPath[analysis.Unit.Path] = analysis
		} else if analysis.Failure != nil {
			byPath[analysis.Failure.Path] = analysis
		}
	}
	if byPath["Huge.php"].Failure == nil || byPath["Huge.php"].Failure.Code != "source_too_large" {
		t.Fatalf("oversized source=%+v", byPath["Huge.php"])
	}
	if byPath["Small.php"].Unit == nil || byPath["Small.php"].Failure != nil {
		t.Fatalf("bounded peer=%+v", byPath["Small.php"])
	}
	unit := codemap.ModuleUnit{Path: "Many.php", Language: "php", Declarations: make([]codemap.ModuleDeclaration, maxReferenceDeclarations+1)}
	if code, _ := boundedPHPUnitFailure(unit); code != "unit_too_large" {
		t.Fatalf("declaration ceiling code=%q", code)
	}
}

func TestPHPDisambiguatesNestedScopeCollisionsAndKeepsCallerEdgesJoined(t *testing.T) {
	source := []byte(`<?php
function firstTarget() {}
function secondTarget() {}
class Outer {
    private $originalOutput;
    public function run() {
        firstTarget();
        return new class {
            private $originalOutput;
            public function run() { secondTarget(); }
        };
    }
}`)
	analyses, err := analyzePHP([]modulekit.Source{{Path: "Nested.php", Body: source}})
	if err != nil || len(analyses) != 1 || analyses[0].Unit == nil {
		t.Fatalf("analysis=%+v err=%v", analyses, err)
	}
	assertUniquePHPDeclarationsAndJoinedCallers(t, analyses[0])
	collisions := map[string]int{}
	for _, declaration := range analyses[0].Unit.Declarations {
		if declaration.Name == "originalOutput" || declaration.Name == "run" {
			collisions[declaration.Name]++
			if !strings.HasSuffix(declaration.Identity, "#1") && !strings.HasSuffix(declaration.Identity, "#2") {
				t.Fatalf("colliding declaration was not ordinally disambiguated: %+v", declaration)
			}
		}
	}
	if collisions["originalOutput"] != 2 || collisions["run"] != 2 {
		t.Fatalf("collision facts=%v declarations=%+v", collisions, analyses[0].Unit.Declarations)
	}
}

func assertUniquePHPDeclarationsAndJoinedCallers(t *testing.T, analysis modulekit.Analysis) {
	t.Helper()
	declared := map[string]bool{}
	for _, declaration := range analysis.Unit.Declarations {
		if declared[declaration.Identity] {
			t.Fatalf("duplicate declaration identity %q", declaration.Identity)
		}
		declared[declaration.Identity] = true
	}
	for _, edge := range analysis.Edges {
		if edge.FromKind == "symbol" && !declared[strings.TrimPrefix(edge.FromRef, analyzerIdentity+"::")] {
			t.Fatalf("edge caller is not a declaration: %+v", edge)
		}
	}
}

func TestPHPVisibilityConstantsAndNamespacesStayExact(t *testing.T) {
	sources := []modulekit.Source{
		{Path: "one.php", Body: []byte(`<?php namespace One; class Worker {
public const READY = 1;
public function visible() { $text = "private hidden"; self::shared(); }
private function shared() {}
}`)},
		{Path: "two.php", Body: []byte(`<?php namespace Two; class Worker { private function shared() {} }`)},
	}
	analyses, err := analyzePHP(sources)
	if err != nil || len(analyses) != 2 || analyses[0].Unit == nil {
		t.Fatalf("analysis=%+v err=%v", analyses, err)
	}
	one := analyses[0]
	if len(one.Edges) != 1 || one.Ambiguous[codemap.CapabilitySymbolCall] != 0 {
		t.Fatalf("namespace-specific static call was not exact: %+v", one)
	}
	exports := map[string]bool{}
	for _, name := range one.Unit.Exports {
		exports[name] = true
	}
	if !exports["READY"] || !exports["visible"] || exports["shared"] {
		t.Fatalf("visibility/constant facts=%v declarations=%+v", one.Unit.Exports, one.Unit.Declarations)
	}
}
