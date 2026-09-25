// Package golang is a LANGUAGE ADAPTER: an interface to Go for the codemap
// port (console-and-info-panel §6).
//
// This package may name the language it adapts — that is the whole point of the
// boundary, and it is the most any source file is allowed to say. It must NEVER
// name a framework, and codemap core must never import it: binding happens
// through the factory, so deleting this package leaves core building and the
// console working with one fewer language.
//
// It reports MECHANICS ONLY — package, exports, imports, size, complexity. It
// has no concept of "controller" or "service"; those live in a convention
// config, which is data. That separation is what lets one adapter serve any
// number of conventions.
//
// It exists to prove the seam, not because Go is special. A second adapter is
// added the same way and changes nothing here.
package golang

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"crossing-guard/codemap"
)

// Adapter implements codemap.Analyzer for Go source.
type Adapter struct {
	// module is the go.mod module path, used to tell an internal import from a
	// third-party one. Resolved lazily per root.
	//
	// The mutex is not optional. One adapter is registered process-wide and
	// Analyze runs on HTTP handler goroutines, so two concurrent descriptor
	// requests raced this map — and a concurrent map read/write in Go is a
	// FATAL runtime error, not a recoverable one. Two reference clicks, or two
	// console tabs, were enough to take the daemon down.
	mu      sync.RWMutex
	modules map[string]string
}

// New returns an adapter ready to register.
func New() *Adapter { return &Adapter{modules: map[string]string{}} }

// Register binds this adapter into the codemap factory. A host calls this; core
// never does.
func Register() { codemap.Register(New()) }

func (a *Adapter) Language() string     { return "go" }
func (a *Adapter) Extensions() []string { return []string{".go"} }
func (a *Adapter) Identity() string     { return "go-ast-v3" }
func (a *Adapter) Coverage() []codemap.Capability {
	return []codemap.Capability{codemap.CapabilityPackageDependency, codemap.CapabilitySymbolDeclaration, codemap.CapabilityResponsibilityFingerprint}
}

// Analyze parses one file and reports what it mechanically contains.
func (a *Adapter) Analyze(root, rel string) (*codemap.Mechanics, error) {
	full := filepath.Join(root, filepath.FromSlash(rel))
	body, err := os.ReadFile(full)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	// Parse tolerantly: a file that does not compile still has a shape worth
	// reporting, and refusing to describe it would make the panel go blank
	// exactly when someone is mid-edit.
	file, err := parser.ParseFile(fset, full, body, parser.SkipObjectResolution)
	if err != nil && file == nil {
		return nil, err
	}

	m := &codemap.Mechanics{
		Namespace: a.namespaceOf(root, rel, file),
		Kind:      "File",
		LOC:       countLines(body),
	}
	if file == nil {
		return m, nil
	}
	module := a.moduleOf(root)
	for _, imp := range file.Imports {
		p, e := strconv.Unquote(imp.Path.Value)
		if e != nil {
			continue
		}
		if module != "" && (p == module || strings.HasPrefix(p, module+"/")) {
			m.Imports = append(m.Imports, strings.TrimPrefix(strings.TrimPrefix(p, module), "/"))
		} else {
			m.External = append(m.External, p)
		}
	}
	if err := a.collectDecls(body, fset, file, m); err != nil {
		return nil, err
	}
	m.Signals = a.signals(rel, file, m)
	return m, nil
}

// collectDecls gathers the public surface, type counts for abstractness, and a
// cheap cyclomatic proxy.
const (
	maxDeclarationsPerUnit = 4096
	maxBodyShapeNodes      = 1_000_000
)

func (a *Adapter) collectDecls(source []byte, fset *token.FileSet, file *ast.File, m *codemap.Mechanics) error {
	bodyNodes := 0
	for _, d := range file.Decls {
		switch decl := d.(type) {
		case *ast.FuncDecl:
			kind := "function"
			name := decl.Name.Name
			if decl.Recv != nil && len(decl.Recv.List) > 0 {
				if receiver := receiverName(decl.Recv.List[0].Type); receiver != "" {
					name = receiver + "." + name
					kind = "method"
				}
			}
			declaration, err := declarationFact(source, fset, kind, name, decl.Pos(), decl.End())
			if err != nil {
				return err
			}
			complexity := complexityOf(decl)
			declaration.Cyclomatic = &complexity
			if decl.Body != nil {
				digest, nodes, err := bodyShape(decl.Body, maxBodyShapeNodes-bodyNodes)
				if err != nil {
					return fmt.Errorf("responsibility fingerprint for %s: %w", name, err)
				}
				bodyNodes += nodes
				declaration.BodyShapeDigest = digest
				declaration.BodyShapeNodes = nodes
			}
			m.Declarations = append(m.Declarations, declaration)
			if decl.Name.IsExported() {
				m.Exports = append(m.Exports, decl.Name.Name)
			}
			m.Cyclomatic += complexity
		case *ast.GenDecl:
			for _, spec := range decl.Specs {
				switch typed := spec.(type) {
				case *ast.TypeSpec:
					declaration, err := declarationFact(source, fset, "type", typed.Name.Name, typed.Pos(), typed.End())
					if err != nil {
						return err
					}
					m.Declarations = append(m.Declarations, declaration)
					m.TotalTypes++
					// An interface is the language's abstract type, which is what
					// Martin's A counts.
					if _, isIface := typed.Type.(*ast.InterfaceType); isIface {
						m.AbstractTypes++
					}
					if typed.Name.IsExported() {
						m.Exports = append(m.Exports, typed.Name.Name)
					}
				case *ast.ValueSpec:
					for _, name := range typed.Names {
						declaration, err := declarationFact(source, fset, "value", name.Name, typed.Pos(), typed.End())
						if err != nil {
							return err
						}
						m.Declarations = append(m.Declarations, declaration)
						if name.IsExported() {
							m.Exports = append(m.Exports, name.Name)
						}
					}
				}
			}
		}
		if len(m.Declarations) > maxDeclarationsPerUnit {
			return fmt.Errorf("unit declares more than %d symbols", maxDeclarationsPerUnit)
		}
	}
	identityTotals := make(map[string]int, len(m.Declarations))
	for _, declaration := range m.Declarations {
		identityTotals[declaration.Identity]++
	}
	identityOrdinals := map[string]int{}
	for index := range m.Declarations {
		identity := m.Declarations[index].Identity
		if identityTotals[identity] <= 1 {
			continue
		}
		if identity != "function:init" && identity != "value:_" {
			return fmt.Errorf("unit contains duplicate declaration identity %q", identity)
		}
		identityOrdinals[identity]++
		m.Declarations[index].Identity = fmt.Sprintf("%s#%d", identity, identityOrdinals[identity])
	}
	seenIdentities := make(map[string]bool, len(m.Declarations))
	for _, declaration := range m.Declarations {
		if declaration.Identity == "" {
			return fmt.Errorf("declaration %q has no stable identity", declaration.Name)
		}
		if seenIdentities[declaration.Identity] {
			return fmt.Errorf("unit contains duplicate declaration identity %q", declaration.Identity)
		}
		seenIdentities[declaration.Identity] = true
	}
	return nil
}

func declarationFact(source []byte, fset *token.FileSet, kind, name string, start, end token.Pos) (codemap.Declaration, error) {
	startPosition := fset.PositionFor(start, false)
	endPosition := fset.PositionFor(end, false)
	if startPosition.Offset < 0 || endPosition.Offset < startPosition.Offset || endPosition.Offset > len(source) {
		return codemap.Declaration{}, fmt.Errorf("declaration %s has invalid source range %d:%d of %d", name, startPosition.Offset, endPosition.Offset, len(source))
	}
	span := endPosition.Line - startPosition.Line + 1
	sum := sha256.Sum256(source[startPosition.Offset:endPosition.Offset])
	return codemap.Declaration{
		Identity: kind + ":" + name, Name: name, Kind: kind, Line: startPosition.Line,
		EndLine: endPosition.Line, SourceDigest: "go-source-declaration-v1-sha256:" + hex.EncodeToString(sum[:]),
		SourceSpanLines: &span,
	}, nil
}

// bodyShape hashes only syntax structure and operators. Identifier spelling,
// literal values, positions, and formatting never enter the digest.
func bodyShape(body *ast.BlockStmt, nodeBudget int) (string, int, error) {
	if nodeBudget <= 0 {
		return "", 0, fmt.Errorf("unit exceeds %d body syntax nodes", maxBodyShapeNodes)
	}
	h := sha256.New()
	nodes := 0
	overflow := false
	ast.Inspect(body, func(node ast.Node) bool {
		if overflow {
			return false
		}
		if node == nil {
			_, _ = h.Write([]byte{')'})
			return true
		}
		nodes++
		if nodes > nodeBudget {
			overflow = true
			return false
		}
		_, _ = fmt.Fprintf(h, "(%T", node)
		switch typed := node.(type) {
		case *ast.BinaryExpr:
			_, _ = fmt.Fprintf(h, ":%s", typed.Op)
		case *ast.UnaryExpr:
			_, _ = fmt.Fprintf(h, ":%s", typed.Op)
		case *ast.AssignStmt:
			_, _ = fmt.Fprintf(h, ":%s", typed.Tok)
		case *ast.IncDecStmt:
			_, _ = fmt.Fprintf(h, ":%s", typed.Tok)
		case *ast.BranchStmt:
			_, _ = fmt.Fprintf(h, ":%s", typed.Tok)
		case *ast.RangeStmt:
			_, _ = fmt.Fprintf(h, ":%s", typed.Tok)
		}
		return true
	})
	if overflow {
		return "", 0, fmt.Errorf("unit exceeds %d body syntax nodes", maxBodyShapeNodes)
	}
	return "go-ast-body-shape-v1-sha256:" + hex.EncodeToString(h.Sum(nil)), nodes, nil
}

func receiverName(expr ast.Expr) string {
	switch typed := expr.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.StarExpr:
		return receiverName(typed.X)
	case *ast.IndexExpr:
		return receiverName(typed.X)
	case *ast.IndexListExpr:
		return receiverName(typed.X)
	default:
		return ""
	}
}

// signals are opaque strings a convention config may match on. The adapter says
// WHAT IS THERE; it does not say what any of it means.
func (a *Adapter) signals(rel string, file *ast.File, m *codemap.Mechanics) []string {
	var out []string
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fn.Name.Name == "main" && fn.Recv == nil {
			out = append(out, "func:main")
		}
		if fn.Name.Name == "init" && fn.Recv == nil {
			out = append(out, "func:init")
		}
		if strings.HasPrefix(fn.Name.Name, "Test") && fn.Recv == nil {
			out = append(out, "func:test")
		}
	}
	if strings.HasSuffix(rel, "_test.go") {
		out = append(out, "file:test")
	}
	// A handler-shaped signature is a mechanical fact; whether that makes the
	// file a "controller" is the config's call, not ours.
	if containsAny(m.External, []string{"net/http"}) {
		out = append(out, "imports:http")
	}
	return out
}

// namespaceOf reports the unit's package identity as <dir>#<package>, which is
// what an importer names.
func (a *Adapter) namespaceOf(root, rel string, file *ast.File) string {
	dir := filepath.ToSlash(filepath.Dir(rel))
	if dir == "." {
		dir = ""
	}
	if file != nil && file.Name != nil && dir == "" {
		return file.Name.Name
	}
	return dir
}

// moduleOf reads the module path from go.mod, so an internal import can be told
// from a third-party one.
func (a *Adapter) moduleOf(root string) string {
	a.mu.RLock()
	m, ok := a.modules[root]
	a.mu.RUnlock()
	if ok {
		return m
	}
	mod := ""
	if body, err := os.ReadFile(filepath.Join(root, "go.mod")); err == nil {
		for _, line := range strings.Split(string(body), "\n") {
			if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
				mod = strings.TrimSpace(rest)
				break
			}
		}
	}
	a.mu.Lock()
	a.modules[root] = mod
	a.mu.Unlock()
	return mod
}

// complexityOf is a branch count — the standard cyclomatic proxy. Named as a
// proxy rather than presented as gocyclo's exact number.
func complexityOf(fn *ast.FuncDecl) int {
	n := 1
	ast.Inspect(fn, func(node ast.Node) bool {
		switch n2 := node.(type) {
		case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt, *ast.CaseClause, *ast.CommClause:
			n++
		case *ast.BinaryExpr:
			if n2.Op == token.LAND || n2.Op == token.LOR {
				n++
			}
		}
		return true
	})
	return n
}

func countLines(body []byte) int {
	if len(body) == 0 {
		return 0
	}
	return strings.Count(string(body), "\n") + 1
}

func containsAny(hay, needles []string) bool {
	for _, h := range hay {
		for _, n := range needles {
			if h == n || strings.HasPrefix(h, n+"/") {
				return true
			}
		}
	}
	return false
}
