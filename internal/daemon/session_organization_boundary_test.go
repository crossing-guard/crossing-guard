package daemon

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func organizationRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func shippedGoFiles(t *testing.T, dirs ...string) []string {
	t.Helper()
	root := organizationRepositoryRoot(t)
	var files []string
	for _, dir := range dirs {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if !entry.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return files
}

// Views and tags are the owner's. The framework ships the mechanism and must
// never ship an opinion: no code path may construct a saved view, apply an
// owner tag, or write an owner note from text that is compiled in. This walks
// the syntax tree rather than grepping, so a handler that passes the request's
// own value through is fine, and a string literal or a string constant declared
// in the same file — however the call is formatted — is not. It does not follow
// a local variable initialised from a literal, a constant declared in another
// file, or a field assigned after construction; review owns those.
func TestNoShippedCodeAuthorsAViewATagOrANote(t *testing.T) {
	authoring := map[string]bool{"ApplySessionOwnerTags": true, "RetractSessionOwnerTags": true,
		"PutSessionOwnerNote": true, "RenameSessionOwnerTag": true, "writeSessionViews": true}
	for _, path := range shippedGoFiles(t, "internal", "store", "engine", "ruledoc", "harvest", "cmd") {
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		constants := packageStringConstants(file)
		ast.Inspect(file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.CompositeLit:
				if name := typeName(n.Type); name == "SavedSessionView" || name == "SessionOwnerTagValue" {
					if literal := firstCompiledText(n, constants); literal != "" {
						t.Errorf("%s: a %s is built from compiled-in text %s — views and tags are the owner's, never code", path, name, literal)
					}
				}
			case *ast.CallExpr:
				if authoring[calledName(n)] {
					for _, arg := range n.Args {
						if literal := firstCompiledText(arg, constants); literal != "" {
							t.Errorf("%s: %s is called with compiled-in text %s", path, calledName(n), literal)
						}
					}
				}
			}
			return true
		})
	}
}

func packageStringConstants(file *ast.File) map[string]bool {
	out := map[string]bool{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value := spec.(*ast.ValueSpec)
			for index, name := range value.Names {
				if index < len(value.Values) {
					if lit, ok := value.Values[index].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						out[name.Name] = true
					}
				}
			}
		}
	}
	return out
}

func firstCompiledText(node ast.Node, constants map[string]bool) string {
	found := ""
	ast.Inspect(node, func(n ast.Node) bool {
		if found != "" {
			return false
		}
		switch v := n.(type) {
		case *ast.BasicLit:
			if v.Kind == token.STRING && v.Value != `""` {
				found = v.Value
			}
		case *ast.Ident:
			if constants[v.Name] {
				found = v.Name
			}
		}
		return true
	})
	return found
}

func typeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return t.Sel.Name
	}
	return ""
}

func calledName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	}
	return ""
}

// Nothing the binary embeds may define a saved view. The three embedded sets
// are named, and "defines a view" means a JSON document with a top-level
// "views" key — a transcript view profile (its own, different thing) has none.
func TestNoEmbeddedAssetDefinesASessionView(t *testing.T) {
	required := map[string]bool{"view-profiles": true, "engine": true, "ruledoc": true}
	check := func(set string, files fs.FS, pattern string) {
		matches, err := fs.Glob(files, pattern)
		if err != nil {
			t.Fatal(err)
		}
		// A set that matches nothing is a guard that moved out from under this
		// test (the rule files once lived in internal/rulebook); it must not pass.
		if len(matches) == 0 && required[set] {
			t.Errorf("%s: no file matches %q — the embedded set moved; point this test at where it went", set, pattern)
		}
		for _, name := range matches {
			raw, err := fs.ReadFile(files, name)
			if err != nil {
				t.Fatal(err)
			}
			var document map[string]json.RawMessage
			if json.Unmarshal(raw, &document) != nil {
				continue
			}
			if _, defines := document["views"]; defines {
				t.Errorf("%s:%s defines session views; the builtin default is the empty list", set, name)
			}
		}
	}
	check("static", staticFS, "static/*.json")
	check("static", staticFS, "static/*/*.json")
	check("view-profiles", builtinViewProfiles, "view-profiles/*.json")
	root := organizationRepositoryRoot(t)
	check("engine", os.DirFS(filepath.Join(root, "engine")), "*.json")
	check("ruledoc", os.DirFS(filepath.Join(root, "ruledoc")), "*.json")
	for _, name := range []string{"session-views.json", "session_views.json"} {
		_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err == nil && !entry.IsDir() && entry.Name() == name && !strings.Contains(path, "testdata") {
				t.Errorf("%s exists in the repository; no views file may ship, not even as an example", path)
			}
			return nil
		})
	}
}

// Owner tags organize; they never govern and never reach an agent. The
// boundary is structural: the rule evaluator's input and the agent context
// builder may not name anything that reads them.
func TestOwnerTagsNeverReachGovernanceOrAgents(t *testing.T) {
	root := organizationRepositoryRoot(t)
	forbidden := []string{"SessionOwnerTag", "SessionOwnerNote", "sessionTagSnapshot", "sessionTagsRead",
		"decorateSessions", "session_owner_tag", "session_owner_note", "ownerRememberedSession"}
	guarded := []string{"internal/daemon/decide.go", "internal/daemon/orchestration_context.go",
		"internal/daemon/govern.go", "internal/orchestration/agent_prompt.go"}
	for _, relative := range guarded {
		source, err := os.ReadFile(filepath.Join(root, relative))
		if err != nil {
			t.Fatalf("%s: %v (a guarded file moved; move the guard with it)", relative, err)
		}
		for _, name := range forbidden {
			if strings.Contains(string(source), name) {
				t.Errorf("%s mentions %s: owner tags must not reach the rule evaluator or an agent's context", relative, name)
			}
		}
	}
	for _, path := range shippedGoFiles(t, "engine", "ruledoc", "internal/orchestration", "internal/rulebook", "internal/detectorselection") {
		source, _ := os.ReadFile(path)
		for _, name := range forbidden {
			if strings.Contains(string(source), name) {
				t.Errorf("%s mentions %s", path, name)
			}
		}
	}
}

// The evaluator decides from the action's own tags, the session's folded
// detector state and the agent-claimed tags. An owner tag — even one spelled
// exactly like a rule input — must be in neither of the two stores it reads,
// which is what makes TestOwnerTagsNeverReachGovernanceOrAgents sufficient.
func TestOwnerTagNeverEntersTheStoresTheEvaluatorReads(t *testing.T) {
	f := newOrganizationFixture(t, organizationRows()...)
	f.tag("risk:rm-rf", f.rows[0])
	f.tag("secret", f.rows[0])
	state, err := f.ix.SessionState("walmart")
	if err != nil || len(state) != 0 {
		t.Fatalf("an owner tag entered the evaluator's folded session state: %+v %v", state, err)
	}
	agentTags, err := f.ix.ActiveOrchestrationTags("walmart", 1)
	if err != nil || len(agentTags) != 0 {
		t.Fatalf("an owner tag appeared among agent tags, which the agent-tag fact and the session.tags context read: %+v %v", agentTags, err)
	}
	facets, _, err := f.ix.AllSessionStateFacets(100)
	if err != nil || len(facets) != 0 {
		t.Fatalf("facets = %+v %v", facets, err)
	}
}
