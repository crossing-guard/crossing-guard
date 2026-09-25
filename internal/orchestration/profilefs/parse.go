package profilefs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	maxYAMLNodes      = 1024
	maxYAMLDepth      = 16
	maxYAMLMapKeys    = 128
	maxYAMLCollection = 128
	maxYAMLScalar     = 16_384

	sourceDigestFrame = "crossing-guard-orchestration-profile-source-v1\x00"
	bundleDigestFrame = "crossing-guard-orchestration-compiled-profile-v1\x00"
)

var requiredTopLevelKeys = []string{
	"format-version", "kind", "id", "version", "name", "description", "role", "execution",
	"trigger", "context", "output", "authority-requests", "requirements", "limits", "failure",
}

// Parse validates exact authored bytes and compiles an inert normalized value.
func Parse(sourceName string, source []byte) (Document, error) {
	if sourceName != "PROFILE.md" {
		return Document{}, problem("invalid_source_name", "source_name", "The imported file name is invalid.",
			"Choose a file named exactly PROFILE.md.")
	}
	if len(source) > MaxSourceBytes {
		return Document{}, problem("source_too_large", "source", "The profile source exceeds 262144 bytes.",
			"Reduce the PROFILE.md file size and preview it again.")
	}
	if len(source) == 0 || !utf8.Valid(source) || bytes.HasPrefix(source, []byte{0xef, 0xbb, 0xbf}) || bytes.IndexByte(source, 0) >= 0 {
		return Document{}, invalid("source", "Use nonempty UTF-8 without a BOM or NUL bytes.")
	}
	frontmatter, body, err := splitProfile(source)
	if err != nil {
		return Document{}, err
	}
	if len(frontmatter) > maxFrontmatterBytes {
		return Document{}, problem("frontmatter_too_large", "frontmatter", "The YAML frontmatter exceeds 65536 bytes.",
			"Reduce the frontmatter and preview it again.")
	}
	if len(body) > maxInstructionBytes {
		return Document{}, problem("instructions_too_large", "instructions", "The Markdown instructions exceed 196608 bytes.",
			"Reduce the instructions and preview them again.")
	}
	if strings.TrimSpace(string(body)) == "" {
		return Document{}, invalid("instructions", "Add nonempty Markdown instructions after the frontmatter.")
	}

	root, err := decodeYAMLNode(frontmatter)
	if err != nil {
		return Document{}, err
	}
	if err := inspectYAML(root); err != nil {
		return Document{}, err
	}
	if err := requireStructure(root); err != nil {
		return Document{}, err
	}
	var authored authoredProfile
	decoder := yaml.NewDecoder(bytes.NewReader(frontmatter))
	decoder.KnownFields(true)
	if err := decoder.Decode(&authored); err != nil {
		return Document{}, problem("invalid_profile", "frontmatter", "The profile YAML fields or types are invalid.",
			"Review the named fields and use only the version-1 profile schema.")
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Document{}, invalid("frontmatter", "Provide exactly one YAML mapping document.")
	}
	compiled, err := compileProfile(authored, string(body))
	if err != nil {
		return Document{}, err
	}
	canonical, err := json.Marshal(compiled)
	if err != nil {
		return Document{}, storageProblem(err)
	}
	return Document{
		Source: append([]byte(nil), source...), SourceDigest: framedDigest(sourceDigestFrame, source),
		BundleDigest: framedDigest(bundleDigestFrame, canonical), Canonical: canonical, Profile: compiled,
	}, nil
}

func splitProfile(source []byte) ([]byte, []byte, error) {
	start := 0
	switch {
	case bytes.HasPrefix(source, []byte("---\n")):
		start = 4
	case bytes.HasPrefix(source, []byte("---\r\n")):
		start = 5
	default:
		return nil, nil, invalid("frontmatter", "Begin PROFILE.md at byte zero with a standalone --- line.")
	}
	for offset := start; offset < len(source); {
		newline := bytes.IndexByte(source[offset:], '\n')
		if newline < 0 {
			break
		}
		lineEnd := offset + newline
		line := source[offset:lineEnd]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		if bytes.Equal(line, []byte("---")) {
			return source[start:offset], source[lineEnd+1:], nil
		}
		offset = lineEnd + 1
	}
	return nil, nil, invalid("frontmatter", "End the YAML frontmatter with a standalone --- line.")
}

func decodeYAMLNode(frontmatter []byte) (*yaml.Node, error) {
	for _, line := range bytes.Split(frontmatter, []byte{'\n'}) {
		trimmed := bytes.TrimSpace(line)
		if bytes.HasPrefix(trimmed, []byte{'%'}) || bytes.Equal(trimmed, []byte("...")) {
			return nil, invalid("frontmatter", "YAML directives and document terminators are not supported.")
		}
	}
	decoder := yaml.NewDecoder(bytes.NewReader(frontmatter))
	var root yaml.Node
	if err := decoder.Decode(&root); err != nil {
		return nil, problem("invalid_profile", "frontmatter", "The profile YAML could not be parsed.",
			"Use the bounded version-1 YAML mapping format.")
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, invalid("frontmatter", "Provide exactly one YAML mapping document.")
	}
	return &root, nil
}

func inspectYAML(root *yaml.Node) error {
	nodes := 0
	var visit func(*yaml.Node, int) error
	visit = func(node *yaml.Node, depth int) error {
		nodes++
		if nodes > maxYAMLNodes {
			return invalid("frontmatter", "Reduce the YAML node count below 1024.")
		}
		if depth > maxYAMLDepth {
			return invalid("frontmatter", "Reduce YAML nesting to 16 levels.")
		}
		if node.Anchor != "" || node.Kind == yaml.AliasNode {
			return invalid("frontmatter", "YAML anchors and aliases are not supported.")
		}
		if node.Style&yaml.TaggedStyle != 0 {
			return invalid("frontmatter", "Explicit YAML tags are not supported.")
		}
		switch node.Kind {
		case yaml.DocumentNode:
			if len(node.Content) != 1 {
				return invalid("frontmatter", "Provide one YAML mapping document.")
			}
		case yaml.MappingNode:
			if len(node.Content)%2 != 0 || len(node.Content)/2 > maxYAMLMapKeys {
				return invalid("frontmatter", "Use mappings with at most 128 keys.")
			}
			seen := map[string]bool{}
			for index := 0; index < len(node.Content); index += 2 {
				key := node.Content[index]
				if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
					return invalid("frontmatter", "Use plain string YAML mapping keys.")
				}
				if key.Value == "<<" || key.Tag == "!!merge" {
					return invalid("frontmatter", "YAML merge keys are not supported.")
				}
				if seen[key.Value] {
					return invalid("frontmatter", "Duplicate YAML mapping keys are not allowed.")
				}
				seen[key.Value] = true
			}
		case yaml.SequenceNode:
			if len(node.Content) > maxYAMLCollection {
				return invalid("frontmatter", "Use sequences with at most 128 items.")
			}
		case yaml.ScalarNode:
			if len(node.Value) > maxYAMLScalar {
				return invalid("frontmatter", "Use YAML scalars no larger than 16384 bytes.")
			}
			if !oneOf(node.Tag, "!!str", "!!int", "!!bool", "!!null") {
				return invalid("frontmatter", "Use only string, integer, boolean, null, map, and sequence YAML values.")
			}
		default:
			return invalid("frontmatter", "This YAML node type is not supported.")
		}
		for _, child := range node.Content {
			if err := visit(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	return visit(root, 0)
}

func requireStructure(root *yaml.Node) error {
	if root.Kind != yaml.DocumentNode || len(root.Content) != 1 || root.Content[0].Kind != yaml.MappingNode {
		return invalid("frontmatter", "Use one YAML mapping as profile frontmatter.")
	}
	top := root.Content[0]
	for _, key := range requiredTopLevelKeys {
		if mappingValue(top, key) == nil {
			return invalid(key, "Add every required version-1 profile field.")
		}
	}
	requiredMappings := []struct {
		name string
		keys []string
	}{
		{"trigger", []string{"event"}}, {"output", []string{"kind"}},
		{"requirements", []string{"capabilities"}},
		{"limits", []string{"timeout", "max-hops", "max-depth"}},
		{"failure", []string{"missing-required-context", "unavailable-capability", "timeout", "malformed-output"}},
	}
	for _, required := range requiredMappings {
		node := mappingValue(top, required.name)
		if node == nil || node.Kind != yaml.MappingNode {
			return invalid(required.name, "Use a YAML mapping for this field.")
		}
		for _, key := range required.keys {
			if mappingValue(node, key) == nil {
				return invalid(required.name+"."+key, "Add every required version-1 profile field.")
			}
		}
	}
	context := mappingValue(top, "context")
	if context == nil || context.Kind != yaml.SequenceNode || len(context.Content) < 1 || len(context.Content) > 16 {
		return invalid("context", "Provide between 1 and 16 context mappings.")
	}
	for index, item := range context.Content {
		if item.Kind != yaml.MappingNode || mappingValue(item, "kind") == nil {
			return invalid("context["+decimal(index)+"].kind", "Add a kind to every context request.")
		}
	}
	if authority := mappingValue(top, "authority-requests"); authority == nil || authority.Kind != yaml.SequenceNode {
		return invalid("authority-requests", "Provide an explicit YAML sequence, which may be empty.")
	}
	return nil
}

func mappingValue(mapping *yaml.Node, name string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == name {
			return mapping.Content[index+1]
		}
	}
	return nil
}

func framedDigest(frame string, body []byte) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte(frame))
	_, _ = digest.Write(body)
	return "sha256-v1:" + hex.EncodeToString(digest.Sum(nil))
}

func decimal(value int) string {
	const digits = "0123456789"
	if value == 0 {
		return "0"
	}
	var reversed [20]byte
	index := len(reversed)
	for value > 0 {
		index--
		reversed[index] = digits[value%10]
		value /= 10
	}
	return string(reversed[index:])
}
