package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"crossing-guard/analyzers/modulekit"
	"crossing-guard/codemap"
	treesitter "github.com/tree-sitter/go-tree-sitter"
	php "github.com/tree-sitter/tree-sitter-php/bindings/go"
)

const (
	analyzerIdentity             = "php-tree-sitter-v2"
	provisionalIdentitySeparator = "\x1f"
	maxReferenceSourceBytes      = 4 << 20
	maxReferenceDeclarations     = 4096
	maxReferenceUnitRecordBytes  = 2 << 20
)

type phpFile struct {
	path          string
	namespace     string
	unit          codemap.ModuleUnit
	calls         []phpCall
	parseError    bool
	failureCode   string
	failureReason string
}

type phpCall struct {
	path, namespace, caller, name, class string
	line                                 int
	dynamic                              bool
}

type phpDecl struct {
	identity, name, namespace, class, path string
}

func analyzePHP(sources []modulekit.Source) ([]modulekit.Analysis, error) {
	files := make([]phpFile, 0, len(sources))
	declarations := map[string][]phpDecl{}
	for _, source := range sources {
		if len(source.Body) > maxReferenceSourceBytes {
			files = append(files, phpFile{path: source.Path, parseError: true,
				failureCode: "source_too_large", failureReason: "source exceeds the reference analyzer byte limit"})
			continue
		}
		file, err := parsePHP(source)
		if err != nil {
			files = append(files, phpFile{path: source.Path, parseError: true})
			continue
		}
		if !file.parseError {
			file.failureCode, file.failureReason = boundedPHPUnitFailure(file.unit)
			file.parseError = file.failureCode != ""
		}
		files = append(files, file)
		for _, declaration := range file.unit.Declarations {
			class := ""
			if declaration.Kind == "method_declaration" || declaration.Kind == "property_declaration" || declaration.Kind == "const_declaration" {
				class = declarationClass(declaration.Identity)
			}
			declarations[strings.ToLower(declaration.Name)] = append(declarations[strings.ToLower(declaration.Name)],
				phpDecl{identity: declaration.Identity, name: declaration.Name, namespace: file.namespace,
					class: class, path: file.path})
		}
	}
	analyses := make([]modulekit.Analysis, 0, len(files))
	for index := range files {
		file := &files[index]
		if file.parseError {
			code, reason := file.failureCode, file.failureReason
			if code == "" {
				code, reason = "parse_failed", "Tree-sitter could not build a PHP syntax tree"
			}
			analyses = append(analyses, modulekit.Analysis{Failure: &codemap.ModuleUnitFailure{
				Path: file.path, Code: code, Reason: reason},
				Errors: allPHPCounts(1)})
			continue
		}
		analysis := modulekit.Analysis{Unit: &file.unit, Produced: map[codemap.Capability]int{
			codemap.CapabilityPackageDependency:         len(file.unit.Imports),
			codemap.CapabilitySymbolDeclaration:         len(file.unit.Declarations),
			codemap.CapabilityResponsibilityFingerprint: fingerprintCount(file.unit.Declarations)},
			Errors: map[codemap.Capability]int{}, Unresolved: map[codemap.Capability]int{},
			Ambiguous: map[codemap.Capability]int{}, Reasons: map[codemap.Capability]string{}}
		if file.parseError {
			analysis.Errors = allPHPCounts(1)
		}
		seenEdges := map[string]bool{}
		for _, call := range file.calls {
			if call.dynamic {
				analysis.Unresolved[codemap.CapabilitySymbolCall]++
				continue
			}
			candidates := declarations[strings.ToLower(call.name)]
			if call.class != "" {
				filtered := candidates[:0]
				for _, candidate := range candidates {
					if strings.EqualFold(candidate.class, call.class) && candidate.namespace == call.namespace {
						filtered = append(filtered, candidate)
					}
				}
				candidates = filtered
			} else {
				filtered := candidates[:0]
				for _, candidate := range candidates {
					if candidate.class == "" && candidate.namespace == call.namespace {
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
			to := analyzerIdentity + "::" + candidates[0].identity
			edge := codemap.ModuleEdge{ToKind: "symbol", ToRef: to, SourcePath: call.path, SourceLine: call.line}
			if call.caller == "" {
				edge.FromKind, edge.FromRef, edge.Relation = "file", call.path, "file_references_symbol"
			} else {
				edge.FromKind, edge.FromRef, edge.Relation = "symbol", analyzerIdentity+"::"+call.caller, "symbol_calls_symbol"
			}
			key := fmt.Sprintf("%s\x00%s\x00%s\x00%d", edge.FromRef, edge.ToRef, edge.SourcePath, edge.SourceLine)
			if !seenEdges[key] {
				seenEdges[key] = true
				analysis.Edges = append(analysis.Edges, edge)
			}
		}
		analysis.Produced[codemap.CapabilitySymbolCall] = len(analysis.Edges)
		if analysis.Unresolved[codemap.CapabilitySymbolCall] > 0 || analysis.Ambiguous[codemap.CapabilitySymbolCall] > 0 {
			analysis.Reasons[codemap.CapabilitySymbolCall] = "dynamic, external, or non-unique PHP calls remain unresolved"
		}
		analyses = append(analyses, analysis)
	}
	return analyses, nil
}

func boundedPHPUnitFailure(unit codemap.ModuleUnit) (string, string) {
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

func parsePHP(source modulekit.Source) (phpFile, error) {
	parser := treesitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(treesitter.NewLanguage(php.LanguagePHP())); err != nil {
		return phpFile{}, err
	}
	tree := parser.Parse(source.Body, nil)
	if tree == nil {
		return phpFile{}, fmt.Errorf("PHP parser returned no tree")
	}
	defer tree.Close()
	root := tree.RootNode()
	file := phpFile{path: source.Path, parseError: root.HasError(), unit: codemap.ModuleUnit{
		Path: source.Path, Language: "php", Kind: "source", LOC: lineCount(source.Body)}}
	file.unit.Namespace = phpNamespace(root, source.Body)
	file.namespace = file.unit.Namespace
	walkPHP(root, source.Body, file.unit.Namespace, "", "", &file)
	finalizePHPIdentities(&file)
	file.unit.Imports = uniqueSorted(file.unit.Imports)
	file.unit.Exports = uniqueSorted(file.unit.Exports)
	file.unit.Signals = uniqueSorted(file.unit.Signals)
	sort.Slice(file.unit.Declarations, func(i, j int) bool {
		return file.unit.Declarations[i].Identity < file.unit.Declarations[j].Identity
	})
	return file, nil
}

func walkPHP(node *treesitter.Node, source []byte, namespace, class, caller string, file *phpFile) {
	kind := node.Kind()
	if classKind(kind) {
		name := nodeText(node.ChildByFieldName("name"), source)
		if name != "" {
			class = name
			identity := phpIdentity(namespace, "", kind, name)
			file.unit.Declarations = append(file.unit.Declarations, declaration(node, identity, name, kind, source, false))
			file.unit.Exports = append(file.unit.Exports, name)
			file.unit.Signals = append(file.unit.Signals, kind)
		}
	}
	if kind == "function_definition" || kind == "method_declaration" {
		name := nodeText(node.ChildByFieldName("name"), source)
		if name != "" {
			identity := phpIdentity(namespace, class, kind, name)
			file.unit.Declarations = append(file.unit.Declarations, declaration(node, identity, name, kind, source, true))
			caller = provisionalIdentity(identity, node.StartByte())
			if kind == "function_definition" || publicPHP(node, source) {
				file.unit.Exports = append(file.unit.Exports, name)
			}
		}
	}
	if kind == "property_declaration" || kind == "const_declaration" {
		for _, nameNode := range descendantsOfKinds(node, map[string]bool{"variable_name": true, "const_element": true}) {
			if nameNode.Kind() == "const_element" {
				nameNode = firstNamedChildOfKind(nameNode, "name")
			}
			name := strings.TrimPrefix(nodeText(nameNode, source), "$")
			if name == "" {
				continue
			}
			identity := phpIdentity(namespace, class, kind, name)
			file.unit.Declarations = append(file.unit.Declarations, declaration(nameNode, identity, name, kind, source, false))
			if publicPHP(node, source) {
				file.unit.Exports = append(file.unit.Exports, name)
			}
		}
	}
	if kind == "function_call_expression" {
		callee := node.ChildByFieldName("function")
		name := nodeText(callee, source)
		dynamic := callee == nil || (callee.Kind() != "name" && callee.Kind() != "qualified_name")
		file.calls = append(file.calls, phpCall{path: file.path, namespace: namespace, caller: caller, name: lastPHPName(name), line: int(node.StartPosition().Row) + 1, dynamic: dynamic})
	} else if kind == "member_call_expression" || kind == "nullsafe_member_call_expression" {
		nameNode := node.ChildByFieldName("name")
		objectNode := node.ChildByFieldName("object")
		name := strings.TrimPrefix(nodeText(nameNode, source), "$")
		dynamic := nameNode == nil || (nameNode.Kind() != "name" && nameNode.Kind() != "variable_name")
		callClass := ""
		if nodeText(objectNode, source) == "$this" {
			callClass, dynamic = class, dynamic || class == ""
		} else {
			dynamic = true
		}
		file.calls = append(file.calls, phpCall{path: file.path, namespace: namespace, caller: caller, name: name, class: callClass, line: int(node.StartPosition().Row) + 1, dynamic: dynamic})
	} else if kind == "scoped_call_expression" {
		nameNode := node.ChildByFieldName("name")
		scopeNode := node.ChildByFieldName("scope")
		name, scope := nodeText(nameNode, source), nodeText(scopeNode, source)
		if scope == "self" || scope == "static" {
			scope = class
		}
		file.calls = append(file.calls, phpCall{path: file.path, namespace: namespace, caller: caller, name: name, class: lastPHPName(scope), line: int(node.StartPosition().Row) + 1, dynamic: name == "" || scope == ""})
	}
	if kind == "namespace_use_declaration" {
		for _, named := range descendantsOfKinds(node, map[string]bool{"namespace_name": true, "qualified_name": true}) {
			if parent := named.Parent(); named.Kind() == "namespace_name" && parent != nil && parent.Kind() == "qualified_name" {
				continue
			}
			if value := strings.Trim(nodeText(named, source), "\\"); value != "" {
				file.unit.Imports = append(file.unit.Imports, value)
			}
		}
	}
	for i := uint(0); i < node.NamedChildCount(); i++ {
		if child := node.NamedChild(i); child != nil {
			walkPHP(child, source, namespace, class, caller, file)
		}
	}
}

func firstNamedChildOfKind(node *treesitter.Node, kind string) *treesitter.Node {
	if node == nil {
		return nil
	}
	for i := uint(0); i < node.NamedChildCount(); i++ {
		if child := node.NamedChild(i); child != nil && child.Kind() == kind {
			return child
		}
	}
	return nil
}

func declaration(node *treesitter.Node, identity, name, kind string, source []byte, fingerprint bool) codemap.ModuleDeclaration {
	start, end := int64(node.StartByte()), int64(node.EndByte())
	row := codemap.ModuleDeclaration{Identity: provisionalIdentity(identity, node.StartByte()), Name: name, Kind: kind,
		Line: int(node.StartPosition().Row) + 1, EndLine: int(node.EndPosition().Row) + 1,
		StartByte: start, EndByte: end}
	if fingerprint {
		complexity := cyclomatic(node)
		row.Cyclomatic = &complexity
		row.BodyShapeDigest, row.BodyShapeNodes = shapeDigest(node, source, "php-tree-shape-v1")
	}
	return row
}

// finalizePHPIdentities keeps the existing identity for a unique declaration and adds
// deterministic source-order ordinals only where PHP permits distinct nested scopes to
// flatten to the same portable identity. Caller references use the same provisional key
// and are rewritten through this one mapping before any edge is emitted.
func finalizePHPIdentities(file *phpFile) {
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

func phpNamespace(root *treesitter.Node, source []byte) string {
	for _, node := range descendantsOfKinds(root, map[string]bool{"namespace_definition": true}) {
		if name := node.ChildByFieldName("name"); name != nil {
			return strings.Trim(nodeText(name, source), "\\")
		}
	}
	return ""
}

func phpIdentity(namespace, class, kind, name string) string {
	prefix := strings.Trim(namespace, "\\")
	if class != "" {
		if prefix != "" {
			prefix += "\\"
		}
		prefix += class
	}
	if prefix != "" {
		prefix += "::"
	}
	return kind + ":" + prefix + name
}

func declarationClass(identity string) string {
	value := identity
	if colon := strings.Index(value, ":"); colon >= 0 {
		value = value[colon+1:]
	}
	if split := strings.LastIndex(value, "::"); split >= 0 {
		value = value[:split]
		return lastPHPName(value)
	}
	return ""
}

func classKind(kind string) bool {
	return kind == "class_declaration" || kind == "interface_declaration" || kind == "trait_declaration" || kind == "enum_declaration"
}

func publicPHP(node *treesitter.Node, source []byte) bool {
	for i := uint(0); i < node.NamedChildCount(); i++ {
		child := node.NamedChild(i)
		if child == nil || child.Kind() != "visibility_modifier" {
			continue
		}
		visibility := strings.ToLower(strings.TrimSpace(nodeText(child, source)))
		return visibility != "private" && visibility != "protected"
	}
	return true
}

func lastPHPName(value string) string {
	value = strings.Trim(strings.TrimSpace(value), "\\")
	parts := strings.Split(value, "\\")
	return parts[len(parts)-1]
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

func allPHPCounts(value int) map[codemap.Capability]int {
	return map[codemap.Capability]int{codemap.CapabilityPackageDependency: value,
		codemap.CapabilitySymbolDeclaration: value, codemap.CapabilityResponsibilityFingerprint: value,
		codemap.CapabilitySymbolCall: value}
}

func cyclomatic(node *treesitter.Node) int {
	total := 1
	complexKinds := map[string]bool{"if_statement": true, "else_if_clause": true, "for_statement": true,
		"foreach_statement": true, "while_statement": true, "do_statement": true, "catch_clause": true,
		"case_statement": true, "conditional_expression": true, "match_conditional_expression": true}
	walkNodes(node, func(current *treesitter.Node) {
		if current.Id() != node.Id() && complexKinds[current.Kind()] {
			total++
		}
		if !current.IsNamed() && (current.Kind() == "&&" || current.Kind() == "||") {
			total++
		}
	})
	return total
}

func shapeDigest(node *treesitter.Node, source []byte, algorithm string) (string, int) {
	parts := []string{}
	ignored := map[string]bool{"comment": true, "name": true, "variable_name": true, "string": true,
		"integer": true, "float": true, "encapsed_string": true, "heredoc": true, "nowdoc": true}
	punctuation := map[string]bool{"(": true, ")": true, "{": true, "}": true, "[": true, "]": true, ",": true, ";": true, ":": true}
	walkNodes(node, func(current *treesitter.Node) {
		kind := current.Kind()
		if ignored[kind] || punctuation[kind] {
			return
		}
		if current.IsNamed() {
			parts = append(parts, kind)
		} else if strings.TrimSpace(nodeText(current, source)) != "" {
			parts = append(parts, kind)
		}
	})
	hash := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return algorithm + ":sha256:" + hex.EncodeToString(hash[:]), len(parts)
}

func walkNodes(node *treesitter.Node, visit func(*treesitter.Node)) {
	visit(node)
	for i := uint(0); i < node.ChildCount(); i++ {
		if child := node.Child(i); child != nil {
			walkNodes(child, visit)
		}
	}
}

func descendantsOfKinds(node *treesitter.Node, kinds map[string]bool) []*treesitter.Node {
	values := []*treesitter.Node{}
	walkNodes(node, func(current *treesitter.Node) {
		if current.Id() != node.Id() && kinds[current.Kind()] {
			values = append(values, current)
		}
	})
	return values
}

func nodeText(node *treesitter.Node, source []byte) string {
	if node == nil || node.EndByte() > uint(len(source)) {
		return ""
	}
	return string(source[node.StartByte():node.EndByte()])
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
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}
