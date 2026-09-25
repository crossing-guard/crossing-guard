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

func TestJavaScriptCompiledDefinitionMatchesManifest(t *testing.T) {
	body, err := os.ReadFile("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest codemap.AnalyzerModuleManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.ModuleID != javascriptDefinition.ModuleID || manifest.AnalyzerIdentity != javascriptDefinition.AnalyzerIdentity ||
		!reflect.DeepEqual(manifest.Languages, javascriptDefinition.Languages) ||
		!reflect.DeepEqual(manifest.Extensions, javascriptDefinition.Extensions) ||
		!reflect.DeepEqual(manifest.Capabilities, javascriptDefinition.Capabilities) {
		t.Fatalf("compiled definition drifted from manifest: manifest=%+v definition=%+v", manifest, javascriptDefinition)
	}
}

func TestJavaScriptFactsCallsCoverageAndShapes(t *testing.T) {
	source := `import thing from "./thing.js";
export function helper() { return 1; }
export class Controller {
  run() { helper(); this.local(); other.dynamic(); }
  local() { if (true) { return 2; } return 0; }
}`
	analyses, err := analyzeJavaScript([]modulekit.Source{{Path: "controller.js", Body: []byte(source)}})
	if err != nil || len(analyses) != 1 || analyses[0].Unit == nil || analyses[0].Failure != nil {
		t.Fatalf("analysis=%+v err=%v", analyses, err)
	}
	analysis := analyses[0]
	if analysis.Unit.Language != "javascript" || len(analysis.Unit.Declarations) < 4 || len(analysis.Edges) != 2 {
		t.Fatalf("JavaScript facts unit=%+v edges=%+v", analysis.Unit, analysis.Edges)
	}
	if analysis.Unresolved[codemap.CapabilitySymbolCall] == 0 {
		t.Fatalf("dynamic member call was presented as resolved: %+v", analysis)
	}
	if !reflect.DeepEqual(analysis.Unit.Exports, []string{"Controller", "helper"}) {
		t.Fatalf("class members leaked into module exports: %v", analysis.Unit.Exports)
	}
	var shape string
	for _, declaration := range analysis.Unit.Declarations {
		if declaration.Name == "local" {
			if declaration.Cyclomatic == nil || *declaration.Cyclomatic < 2 {
				t.Fatalf("local complexity=%v", declaration.Cyclomatic)
			}
			shape = declaration.BodyShapeDigest
		}
	}
	variant := []byte(`class Controller { renamed() { // comment
      if (false) { return 99; } return 7; } }`)
	changed, err := analyzeJavaScript([]modulekit.Source{{Path: "variant.js", Body: variant}})
	if err != nil || changed[0].Unit == nil || len(changed[0].Unit.Declarations) < 2 {
		t.Fatalf("variant=%+v err=%v", changed, err)
	}
	if got := changed[0].Unit.Declarations[1].BodyShapeDigest; got != shape {
		t.Fatalf("normalized JavaScript shape drifted: %s != %s", got, shape)
	}
}

func TestTypeScriptAndTSXUseDeclaredDialects(t *testing.T) {
	sources := []modulekit.Source{
		{Path: "typed.ts", Body: []byte(`export interface Item { id: string }; export function use(v: Item): string { return v.id }`)},
		{Path: "view.tsx", Body: []byte(`export const View = (p: {name: string}) => <div>{p.name}</div>`)},
	}
	analyses, err := analyzeJavaScript(sources)
	if err != nil || len(analyses) != 2 {
		t.Fatal(err)
	}
	languages := map[string]bool{}
	for _, analysis := range analyses {
		if analysis.Unit == nil || analysis.Failure != nil {
			t.Fatalf("dialect failure: %+v", analysis)
		}
		languages[analysis.Unit.Language] = true
	}
	if !languages["typescript"] || !languages["tsx"] {
		t.Fatalf("dialects=%v", languages)
	}
}

func TestJavaScriptMalformedSourceIsAnExplicitFailure(t *testing.T) {
	analyses, err := analyzeJavaScript([]modulekit.Source{{Path: "bad.js", Body: []byte("function broken(")}})
	if err != nil || len(analyses) != 1 || analyses[0].Failure == nil || analyses[0].Unit != nil {
		t.Fatalf("malformed analysis=%+v err=%v", analyses, err)
	}
}

func TestJavaScriptBoundsOneOversizedSourceWithoutFailingItsPeers(t *testing.T) {
	oversized := make([]byte, maxReferenceSourceBytes+1)
	analyses, err := analyzeJavaScript([]modulekit.Source{
		{Path: "huge.js", Body: oversized},
		{Path: "small.js", Body: []byte("export function small() { return 1 }")},
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
	if byPath["huge.js"].Failure == nil || byPath["huge.js"].Failure.Code != "source_too_large" {
		t.Fatalf("oversized source=%+v", byPath["huge.js"])
	}
	if byPath["small.js"].Unit == nil || byPath["small.js"].Failure != nil {
		t.Fatalf("bounded peer=%+v", byPath["small.js"])
	}
	unit := codemap.ModuleUnit{Path: "many.js", Language: "javascript", Declarations: make([]codemap.ModuleDeclaration, maxReferenceDeclarations+1)}
	if code, _ := boundedJavaScriptUnitFailure(unit); code != "unit_too_large" {
		t.Fatalf("declaration ceiling code=%q", code)
	}
}

func TestJavaScriptDisambiguatesScopedBindingsAndKeepsCallerEdgesJoined(t *testing.T) {
	source := []byte(`
function firstTarget() {}
function secondTarget() {}
function first() { const headers = () => firstTarget(); return headers(); }
function second() { const headers = () => secondTarget(); return headers(); }
`)
	analyses, err := analyzeJavaScript([]modulekit.Source{{Path: "scopes.js", Body: source}})
	if err != nil || len(analyses) != 1 || analyses[0].Unit == nil {
		t.Fatalf("analysis=%+v err=%v", analyses, err)
	}
	declared := map[string]bool{}
	headers := 0
	for _, declaration := range analyses[0].Unit.Declarations {
		if declared[declaration.Identity] {
			t.Fatalf("duplicate declaration identity %q", declaration.Identity)
		}
		declared[declaration.Identity] = true
		if declaration.Name == "headers" {
			headers++
			if !strings.HasSuffix(declaration.Identity, "#1") && !strings.HasSuffix(declaration.Identity, "#2") {
				t.Fatalf("colliding binding was not ordinally disambiguated: %+v", declaration)
			}
		}
	}
	if headers != 2 {
		t.Fatalf("headers declarations=%d facts=%+v", headers, analyses[0].Unit.Declarations)
	}
	for _, edge := range analyses[0].Edges {
		if edge.FromKind == "symbol" && !declared[strings.TrimPrefix(edge.FromRef, analyzerIdentity+"::")] {
			t.Fatalf("edge caller is not a declaration: %+v", edge)
		}
	}
}

func TestJavaScriptResolvesOnlyExactExportedRelativeImports(t *testing.T) {
	sources := []modulekit.Source{
		{Path: "lib/helper.ts", Body: []byte(`export function helper() { return 1 }; function hidden() { return 2 }`)},
		{Path: "app/run.ts", Body: []byte(`import { helper as invoke, hidden } from "../lib/helper";
export function run() { invoke(); hidden(); }`)},
	}
	analyses, err := analyzeJavaScript(sources)
	if err != nil || len(analyses) != 2 {
		t.Fatalf("analysis=%+v err=%v", analyses, err)
	}
	var app modulekit.Analysis
	for _, analysis := range analyses {
		if analysis.Unit != nil && analysis.Unit.Path == "app/run.ts" {
			app = analysis
		}
	}
	if app.Unit == nil || len(app.Edges) != 1 || app.Unresolved[codemap.CapabilitySymbolCall] != 1 ||
		!strings.Contains(app.Edges[0].ToRef, "function_declaration:helper") {
		t.Fatalf("relative import resolution was not exact: %+v", app)
	}
}
