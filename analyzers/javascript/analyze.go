package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"crossing-guard/analyzers/modulekit"
	"crossing-guard/codemap"
	treesitter "github.com/tree-sitter/go-tree-sitter"
	javascript "github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

const (
	analyzerIdentity             = "javascript-tree-sitter-v2"
	provisionalIdentitySeparator = "\x1f"
	maxReferenceSourceBytes      = 4 << 20
	maxReferenceDeclarations     = 4096
	maxReferenceUnitRecordBytes  = 2 << 20
)

type jsFile struct {
	path          string
	unit          codemap.ModuleUnit
	calls         []jsCall
	bindings      map[string]jsImportBinding
	parseError    bool
	failureCode   string
	failureReason string
}

type jsCall struct {
	path, caller, name, class string
	line                      int
	dynamic                   bool
}

type jsDecl struct{ identity, name, class, path string }

type jsImportBinding struct{ source, imported string }

func analyzeJavaScript(sources []modulekit.Source) ([]modulekit.Analysis, error) {
	files := make([]jsFile, 0, len(sources))
	declarations := map[string][]jsDecl{}
	knownPaths := map[string]bool{}
	exported := map[string]bool{}
	for _, source := range sources {
		knownPaths[source.Path] = true
		if len(source.Body) > maxReferenceSourceBytes {
			files = append(files, jsFile{path: source.Path, parseError: true,
				failureCode: "source_too_large", failureReason: "source exceeds the reference analyzer byte limit"})
			continue
		}
		file, err := parseJavaScript(source)
		if err != nil {
			files = append(files, jsFile{path: source.Path, parseError: true})
			continue
		}
		if !file.parseError {
			file.failureCode, file.failureReason = boundedJavaScriptUnitFailure(file.unit)
			file.parseError = file.failureCode != ""
		}
		files = append(files, file)
		for _, name := range file.unit.Exports {
			exported[file.path+"\x00"+name] = true
		}
		for _, declaration := range file.unit.Declarations {
			declarations[file.path+"\x00"+declaration.Name] = append(declarations[file.path+"\x00"+declaration.Name],
				jsDecl{identity: declaration.Identity, name: declaration.Name,
					class: declarationClass(declaration.Identity), path: file.path})
		}
	}
	analyses := make([]modulekit.Analysis, 0, len(files))
	for index := range files {
		file := &files[index]
		if file.parseError {
			code, reason := file.failureCode, file.failureReason
			if code == "" {
				code, reason = "parse_failed", "Tree-sitter could not build a JavaScript/TypeScript syntax tree"
			}
			analyses = append(analyses, modulekit.Analysis{Failure: &codemap.ModuleUnitFailure{Path: file.path,
				Code: code, Reason: reason}, Errors: allJSCounts(1)})
			continue
		}
		analysis := modulekit.Analysis{Unit: &file.unit, Produced: map[codemap.Capability]int{
			codemap.CapabilityPackageDependency: len(file.unit.Imports), codemap.CapabilitySymbolDeclaration: len(file.unit.Declarations),
			codemap.CapabilityResponsibilityFingerprint: fingerprintCount(file.unit.Declarations)},
			Errors: map[codemap.Capability]int{}, Unresolved: map[codemap.Capability]int{},
			Ambiguous: map[codemap.Capability]int{}, Reasons: map[codemap.Capability]string{}}
		seen := map[string]bool{}
		for _, call := range file.calls {
			if call.dynamic {
				analysis.Unresolved[codemap.CapabilitySymbolCall]++
				continue
			}
			candidates := declarations[call.path+"\x00"+call.name]
			if len(candidates) == 0 {
				if binding, ok := file.bindings[call.name]; ok {
					if target := resolveJSImportTarget(call.path, binding.source, knownPaths); target != "" &&
						exported[target+"\x00"+binding.imported] {
						candidates = declarations[target+"\x00"+binding.imported]
					}
				}
			}
			if call.class != "" {
				filtered := candidates[:0]
				for _, candidate := range candidates {
					if candidate.class == call.class {
						filtered = append(filtered, candidate)
					}
				}
				candidates = filtered
			}
			if len(candidates) == 0 {
				analysis.Unresolved[codemap.CapabilitySymbolCall]++
				continue
			}
			if len(candidates) > 1 {
				analysis.Ambiguous[codemap.CapabilitySymbolCall]++
				continue
			}
			edge := codemap.ModuleEdge{ToKind: "symbol", ToRef: analyzerIdentity + "::" + candidates[0].identity,
				SourcePath: call.path, SourceLine: call.line}
			if call.caller == "" {
				edge.FromKind, edge.FromRef, edge.Relation = "file", call.path, "file_references_symbol"
			} else {
				edge.FromKind, edge.FromRef, edge.Relation = "symbol", analyzerIdentity+"::"+call.caller, "symbol_calls_symbol"
			}
			key := fmt.Sprintf("%s\x00%s\x00%d", edge.FromRef, edge.ToRef, edge.SourceLine)
			if !seen[key] {
				seen[key] = true
				analysis.Edges = append(analysis.Edges, edge)
			}
		}
		analysis.Produced[codemap.CapabilitySymbolCall] = len(analysis.Edges)
		if analysis.Unresolved[codemap.CapabilitySymbolCall] > 0 || analysis.Ambiguous[codemap.CapabilitySymbolCall] > 0 {
			analysis.Reasons[codemap.CapabilitySymbolCall] = "external, member, optional, or non-unique calls remain unresolved"
		}
		analyses = append(analyses, analysis)
	}
	return analyses, nil
}

func boundedJavaScriptUnitFailure(unit codemap.ModuleUnit) (string, string) {
	if len(unit.Declarations) > maxReferenceDeclarations {
		return "unit_too_large", "unit exceeds the reference analyzer declaration limit"
	}
	record, err := json.Marshal(codemap.AnalyzerOutputRecord{Type: "unit", Unit: &unit})
	if err != nil {
		return "unit_encode_failed", "unit could not be encoded"
	}
	if len(record)+1 > maxReferenceUnitRecordBytes {
		return "unit_too_large", "unit exceeds the reference analyzer record limit"
	}
	return "", ""
}

func parseJavaScript(source modulekit.Source) (jsFile, error) {
	parser := treesitter.NewParser()
	defer parser.Close()
	language, languageID, err := jsLanguage(source.Path)
	if err != nil {
		return jsFile{}, err
	}
	if err := parser.SetLanguage(language); err != nil {
		return jsFile{}, err
	}
	tree := parser.Parse(source.Body, nil)
	if tree == nil {
		return jsFile{}, fmt.Errorf("JavaScript parser returned no tree")
	}
	defer tree.Close()
	root := tree.RootNode()
	file := jsFile{path: source.Path, parseError: root.HasError(), bindings: map[string]jsImportBinding{}, unit: codemap.ModuleUnit{
		Path: source.Path, Language: languageID, Kind: "module", Namespace: source.Path, LOC: lineCount(source.Body)}}
	walkJS(root, source.Body, "", "", false, &file)
	finalizeJavaScriptIdentities(&file)
	file.unit.Imports = uniqueSorted(file.unit.Imports)
	file.unit.Exports = uniqueSorted(file.unit.Exports)
	file.unit.Signals = uniqueSorted(file.unit.Signals)
	sort.Slice(file.unit.Declarations, func(i, j int) bool { return file.unit.Declarations[i].Identity < file.unit.Declarations[j].Identity })
	return file, nil
}

func jsLanguage(path string) (*treesitter.Language, string, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".ts":
		return treesitter.NewLanguage(typescript.LanguageTypescript()), "typescript", nil
	case ".tsx":
		return treesitter.NewLanguage(typescript.LanguageTSX()), "tsx", nil
	case ".js", ".jsx", ".mjs", ".cjs":
		return treesitter.NewLanguage(javascript.Language()), "javascript", nil
	default:
		return nil, "", fmt.Errorf("unsupported JavaScript extension")
	}
}

func walkJS(node *treesitter.Node, source []byte, class, caller string, exported bool, file *jsFile) {
	kind := node.Kind()
	if kind == "export_statement" {
		exported = true
	}
	if kind == "class_declaration" || kind == "interface_declaration" || kind == "type_alias_declaration" {
		name := nodeText(node.ChildByFieldName("name"), source)
		if name != "" {
			identity := jsIdentity(class, kind, name)
			file.unit.Declarations = append(file.unit.Declarations, jsDeclaration(node, identity, name, kind, source, false))
			class = name
			if exported {
				file.unit.Exports = append(file.unit.Exports, name)
			}
			file.unit.Signals = append(file.unit.Signals, kind)
		}
	}
	if kind == "function_declaration" || kind == "method_definition" {
		name := nodeText(node.ChildByFieldName("name"), source)
		if name != "" {
			identity := jsIdentity(class, kind, name)
			file.unit.Declarations = append(file.unit.Declarations, jsDeclaration(node, identity, name, kind, source, true))
			caller = provisionalIdentity(identity, node.StartByte())
			if exported {
				file.unit.Exports = append(file.unit.Exports, name)
			}
		}
	}
	if kind == "variable_declarator" {
		nameNode, valueNode := node.ChildByFieldName("name"), node.ChildByFieldName("value")
		name := nodeText(nameNode, source)
		if nameNode != nil && nameNode.Kind() == "identifier" && name != "" {
			declarationKind, fingerprint := "value", false
			if valueNode != nil && (valueNode.Kind() == "arrow_function" || valueNode.Kind() == "function_expression") {
				declarationKind, fingerprint = "function", true
			}
			if caller == "" || fingerprint {
				identity := jsIdentity(class, declarationKind, name)
				file.unit.Declarations = append(file.unit.Declarations, jsDeclaration(node, identity, name, declarationKind, source, fingerprint))
				if fingerprint {
					caller = provisionalIdentity(identity, node.StartByte())
				}
				if exported {
					file.unit.Exports = append(file.unit.Exports, name)
				}
			}
		}
	}
	if kind == "import_statement" {
		if sourceNode := node.ChildByFieldName("source"); sourceNode != nil {
			specifier := trimQuote(nodeText(sourceNode, source))
			file.unit.Imports = append(file.unit.Imports, specifier)
			for _, imported := range descendantsOfJSKind(node, "import_specifier") {
				name := nodeText(imported.ChildByFieldName("name"), source)
				local := nodeText(imported.ChildByFieldName("alias"), source)
				if local == "" {
					local = name
				}
				if name != "" && local != "" {
					file.bindings[local] = jsImportBinding{source: specifier, imported: name}
				}
			}
		}
	}
	if kind == "call_expression" {
		function := node.ChildByFieldName("function")
		name, dynamic, callClass := "", true, ""
		if function != nil && function.Kind() == "identifier" {
			name, dynamic = nodeText(function, source), false
		} else if function != nil && function.Kind() == "member_expression" {
			object, property := function.ChildByFieldName("object"), function.ChildByFieldName("property")
			if object != nil && property != nil && object.Kind() == "this" && property.Kind() == "property_identifier" {
				name, callClass, dynamic = nodeText(property, source), class, class == ""
			}
		}
		file.calls = append(file.calls, jsCall{path: file.path, caller: caller, name: name, class: callClass,
			line: int(node.StartPosition().Row) + 1, dynamic: dynamic})
		if function != nil && function.Kind() == "identifier" && nodeText(function, source) == "require" {
			if arguments := node.ChildByFieldName("arguments"); arguments != nil && arguments.NamedChildCount() == 1 {
				file.unit.Imports = append(file.unit.Imports, trimQuote(nodeText(arguments.NamedChild(0), source)))
			}
		}
	}
	descendantExported := exported
	if kind == "class_declaration" || kind == "interface_declaration" || kind == "type_alias_declaration" ||
		kind == "function_declaration" || kind == "method_definition" || kind == "variable_declarator" {
		descendantExported = false
	}
	for i := uint(0); i < node.NamedChildCount(); i++ {
		if child := node.NamedChild(i); child != nil {
			walkJS(child, source, class, caller, descendantExported, file)
		}
	}
}

func jsDeclaration(node *treesitter.Node, identity, name, kind string, source []byte, fingerprint bool) codemap.ModuleDeclaration {
	declaration := codemap.ModuleDeclaration{Identity: provisionalIdentity(identity, node.StartByte()), Name: name, Kind: kind,
		Line: int(node.StartPosition().Row) + 1, EndLine: int(node.EndPosition().Row) + 1,
		StartByte: int64(node.StartByte()), EndByte: int64(node.EndByte())}
	if fingerprint {
		complexity := cyclomatic(node)
		declaration.Cyclomatic = &complexity
		declaration.BodyShapeDigest, declaration.BodyShapeNodes = shapeDigest(node, source)
	}
	return declaration
}

// finalizeJavaScriptIdentities keeps unique declaration identities stable while
// disambiguating legal same-name bindings from different scopes. Calls retain a private
// provisional declaration key during the syntax walk and are rewritten to the same
// final identity before exact edges are resolved.
func finalizeJavaScriptIdentities(file *jsFile) {
	groups := map[string][]int{}
	for index, row := range file.unit.Declarations {
		base, _ := splitProvisionalIdentity(row.Identity)
		groups[base] = append(groups[base], index)
	}
	final := map[string]string{}
	for base, indexes := range groups {
		sort.Slice(indexes, func(i, j int) bool {
			return file.unit.Declarations[indexes[i]].StartByte < file.unit.Declarations[indexes[j]].StartByte
		})
		for ordinal, index := range indexes {
			identity := base
			if len(indexes) > 1 {
				identity += "#" + strconv.Itoa(ordinal+1)
			}
			provisional := file.unit.Declarations[index].Identity
			final[provisional] = identity
			file.unit.Declarations[index].Identity = identity
		}
	}
	for index := range file.calls {
		if identity := final[file.calls[index].caller]; identity != "" {
			file.calls[index].caller = identity
		}
	}
}

func provisionalIdentity(base string, startByte uint) string {
	return base + provisionalIdentitySeparator + strconv.FormatUint(uint64(startByte), 10)
}

func splitProvisionalIdentity(identity string) (string, string) {
	if index := strings.LastIndex(identity, provisionalIdentitySeparator); index >= 0 {
		return identity[:index], identity[index+len(provisionalIdentitySeparator):]
	}
	return identity, ""
}

func jsIdentity(class, kind, name string) string {
	if class != "" {
		return kind + ":" + class + "::" + name
	}
	return kind + ":" + name
}

func declarationClass(identity string) string {
	value := identity
	if colon := strings.Index(value, ":"); colon >= 0 {
		value = value[colon+1:]
	}
	if split := strings.LastIndex(value, "::"); split >= 0 {
		return value[:split]
	}
	return ""
}

func cyclomatic(node *treesitter.Node) int {
	total := 1
	complexKinds := map[string]bool{"if_statement": true, "for_statement": true, "for_in_statement": true,
		"while_statement": true, "do_statement": true, "catch_clause": true, "switch_case": true,
		"ternary_expression": true}
	walkNodes(node, func(current *treesitter.Node) {
		if current.Id() != node.Id() && complexKinds[current.Kind()] {
			total++
		}
		if !current.IsNamed() && (current.Kind() == "&&" || current.Kind() == "||" || current.Kind() == "??") {
			total++
		}
	})
	return total
}

func shapeDigest(node *treesitter.Node, source []byte) (string, int) {
	ignored := map[string]bool{"comment": true, "identifier": true, "property_identifier": true,
		"string": true, "template_string": true, "number": true, "regex": true, "true": true, "false": true, "null": true}
	punctuation := map[string]bool{"(": true, ")": true, "{": true, "}": true, "[": true, "]": true, ",": true, ";": true, ":": true}
	parts := []string{}
	walkNodes(node, func(current *treesitter.Node) {
		kind := current.Kind()
		if ignored[kind] || punctuation[kind] {
			return
		}
		if current.IsNamed() || strings.TrimSpace(nodeText(current, source)) != "" {
			parts = append(parts, kind)
		}
	})
	hash := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "javascript-tree-shape-v1:sha256:" + hex.EncodeToString(hash[:]), len(parts)
}

func walkNodes(node *treesitter.Node, visit func(*treesitter.Node)) {
	visit(node)
	for i := uint(0); i < node.ChildCount(); i++ {
		if child := node.Child(i); child != nil {
			walkNodes(child, visit)
		}
	}
}

func nodeText(node *treesitter.Node, source []byte) string {
	if node == nil || node.EndByte() > uint(len(source)) {
		return ""
	}
	return string(source[node.StartByte():node.EndByte()])
}

func trimQuote(value string) string { return strings.Trim(strings.TrimSpace(value), "'\"`") }

func descendantsOfJSKind(node *treesitter.Node, kind string) []*treesitter.Node {
	values := []*treesitter.Node{}
	walkNodes(node, func(current *treesitter.Node) {
		if current.Id() != node.Id() && current.Kind() == kind {
			values = append(values, current)
		}
	})
	return values
}

func resolveJSImportTarget(fromPath, specifier string, known map[string]bool) string {
	if !strings.HasPrefix(specifier, "./") && !strings.HasPrefix(specifier, "../") {
		return ""
	}
	base := filepath.ToSlash(filepath.Clean(filepath.Join(filepath.Dir(fromPath), specifier)))
	if base == "." || base == ".." || strings.HasPrefix(base, "../") {
		return ""
	}
	candidates := []string{base}
	if filepath.Ext(base) == "" {
		for _, extension := range []string{".js", ".jsx", ".mjs", ".cjs", ".ts", ".tsx"} {
			candidates = append(candidates, base+extension)
		}
		for _, extension := range []string{".js", ".jsx", ".mjs", ".cjs", ".ts", ".tsx"} {
			candidates = append(candidates, base+"/index"+extension)
		}
	}
	for _, candidate := range candidates {
		if known[candidate] {
			return candidate
		}
	}
	return ""
}

func lineCount(source []byte) int {
	if len(source) == 0 {
		return 0
	}
	return strings.Count(string(source), "\n") + 1
}

func uniqueSorted(values []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func fingerprintCount(declarations []codemap.ModuleDeclaration) int {
	total := 0
	for _, declaration := range declarations {
		if declaration.BodyShapeDigest != "" {
			total++
		}
	}
	return total
}

func allJSCounts(value int) map[codemap.Capability]int {
	return map[codemap.Capability]int{codemap.CapabilityPackageDependency: value,
		codemap.CapabilitySymbolDeclaration: value, codemap.CapabilityResponsibilityFingerprint: value,
		codemap.CapabilitySymbolCall: value}
}
