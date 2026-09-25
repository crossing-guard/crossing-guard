package golang

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"crossing-guard/codemap"
)

func TestAdapterIdentityCoverageAndDeclarations(t *testing.T) {
	root := t.TempDir()
	body := `package sample

const Visible = 1
var hidden = 2
type Item struct{}
func Top() {}
func (i *Item) Method() {}
`
	if err := os.WriteFile(filepath.Join(root, "sample.go"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	adapter := New()
	if adapter.Identity() != "go-ast-v3" {
		t.Fatalf("unexpected adapter identity %q", adapter.Identity())
	}
	wantCoverage := map[codemap.Capability]bool{
		codemap.CapabilityPackageDependency:         true,
		codemap.CapabilitySymbolDeclaration:         true,
		codemap.CapabilityResponsibilityFingerprint: true,
	}
	for _, capability := range adapter.Coverage() {
		delete(wantCoverage, capability)
	}
	if len(wantCoverage) != 0 {
		t.Fatalf("missing capability declarations: %v", wantCoverage)
	}
	mechanics, err := adapter.Analyze(root, "sample.go")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]codemap.Declaration{}
	for _, declaration := range mechanics.Declarations {
		got[declaration.Name] = declaration
	}
	for name, line := range map[string]int{"Visible": 3, "hidden": 4, "Item": 5, "Top": 6, "Item.Method": 7} {
		if got[name].Line != line {
			t.Errorf("declaration %q line=%d want=%d; all=%v", name, got[name].Line, line, got)
		}
		if got[name].Identity == "" || got[name].Kind == "" || got[name].EndLine < got[name].Line ||
			!strings.HasPrefix(got[name].SourceDigest, "go-source-declaration-v1-sha256:") ||
			got[name].SourceSpanLines == nil {
			t.Errorf("declaration %q omitted exact v3 facts: %+v", name, got[name])
		}
	}
	if got["Top"].Identity != "function:Top" || got["Item.Method"].Identity != "method:Item.Method" {
		t.Fatalf("function/method identity is unstable: top=%+v method=%+v", got["Top"], got["Item.Method"])
	}
	if got["Top"].Cyclomatic == nil || *got["Top"].Cyclomatic != 1 || got["Item"].Cyclomatic != nil {
		t.Fatalf("optional cyclomatic facts are wrong: top=%+v item=%+v", got["Top"], got["Item"])
	}
}

func TestDeclarationSourceDigestDetectsLiteralOnlyChangeWhileBodyShapeDoesNot(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sample.go")
	write := func(literal string) codemap.Declaration {
		t.Helper()
		body := "package sample\nfunc Value() int { return " + literal + " }\n"
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		mechanics, err := New().Analyze(root, "sample.go")
		if err != nil {
			t.Fatal(err)
		}
		return mechanics.Declarations[0]
	}
	before := write("1")
	after := write("99")
	if before.SourceDigest == after.SourceDigest {
		t.Fatal("literal-only change did not change exact declaration source digest")
	}
	if before.BodyShapeDigest != after.BodyShapeDigest {
		t.Fatalf("literal-only change unexpectedly changed structural body shape: before=%+v after=%+v", before, after)
	}
}

func TestBodyShapeFingerprintIgnoresNamesLiteralsAndFormattingButKeepsOperators(t *testing.T) {
	root := t.TempDir()
	body := `package sample

func First(value int) {
	if value > 1 { println("first") }
}

func TestAdapterRejectsDuplicateDeclarationIdentity(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "duplicate.go"), []byte("package sample\nfunc Same() {}\nfunc Same() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New().Analyze(dir, "duplicate.go"); err == nil || !strings.Contains(err.Error(), "duplicate declaration identity") {
		t.Fatalf("duplicate declaration error=%v", err)
	}
}

func TestAdapterDisambiguatesLegalRepeatedInitDeclarations(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "init.go"), []byte("package sample\nfunc init() {}\nfunc init() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mechanics, err := New().Analyze(dir, "init.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(mechanics.Declarations) != 2 || mechanics.Declarations[0].Identity != "function:init#1" || mechanics.Declarations[1].Identity != "function:init#2" {
		t.Fatalf("repeated init identities=%+v", mechanics.Declarations)
	}
}

// comment and formatting do not enter the shape
func Second(other int) { if other > 99 {
	println("second")
} }

func Different(other int) { if other < 99 { println("second") } }
`
	if err := os.WriteFile(filepath.Join(root, "shape.go"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	mechanics, err := New().Analyze(root, "shape.go")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]codemap.Declaration{}
	for _, declaration := range mechanics.Declarations {
		got[declaration.Name] = declaration
	}
	first, second, different := got["First"], got["Second"], got["Different"]
	if first.BodyShapeDigest == "" || first.BodyShapeNodes == 0 {
		t.Fatalf("body shape missing: %+v", first)
	}
	if first.BodyShapeDigest != second.BodyShapeDigest || first.BodyShapeNodes != second.BodyShapeNodes {
		t.Fatalf("names/literals/format changed shape: first=%+v second=%+v", first, second)
	}
	if different.BodyShapeDigest == first.BodyShapeDigest {
		t.Fatalf("operator change did not change shape: first=%+v different=%+v", first, different)
	}
	if !strings.HasPrefix(first.BodyShapeDigest, "go-ast-body-shape-v1-sha256:") {
		t.Fatalf("shape identity is not namespaced: %q", first.BodyShapeDigest)
	}
}
