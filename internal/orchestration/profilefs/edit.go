package profilefs

// Structured authoring edits (agents-settings-redesign plan §5). The console
// edits a PROFILE.md through named fields; this owner applies them to the
// authored bytes through the YAML node tree, so keys the edit does not name
// keep their order, style and comments, and the result is re-validated by the
// same Parse every import uses. The browser never writes YAML.

import (
	"bytes"
	"embed"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed templates/*.PROFILE.md
var profileTemplates embed.FS

// ProfileEdit names the authored fields the console may change. A nil field
// is left untouched; an empty optional value removes its key; a required key
// is never removed.
type ProfileEdit struct {
	Name         *string            `json:"name,omitempty"`
	Description  *string            `json:"description,omitempty"`
	Version      *string            `json:"version,omitempty"`
	TriggerEvent *string            `json:"trigger_event,omitempty"`
	Context      *[]ContextEdit     `json:"context,omitempty"`
	Instructions *string            `json:"instructions,omitempty"`
	ReplyShape   *string            `json:"reply_shape,omitempty"`
	Stages       *map[string]string `json:"stages,omitempty"`
	Authority    *[]string          `json:"authority_requests,omitempty"`
	MayTag       *[]string          `json:"may_tag,omitempty"`
	Locality     *string            `json:"locality,omitempty"`
}

// ContextEdit is one context request as the console edits it. A selector the
// authored item already carries is kept when the kind is unchanged.
type ContextEdit struct {
	Kind     string `json:"kind"`
	Required bool   `json:"required,omitempty"`
	MaxBytes int    `json:"max_bytes,omitempty"`
}

// NewProfile is the identity a new agent starts with.
type NewProfile struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Type        string `json:"type"`
}

// topLevelOrder is the version-1 schema's key order, used only to place a key
// the source does not have yet.
var topLevelOrder = []string{
	"format-version", "kind", "id", "version", "name", "description", "type", "role", "execution",
	"priority", "may-tag", "await-annotations", "trigger", "stages", "context", "output",
	"authority-requests", "allowed-profiles", "reply-shape", "requirements", "limits", "failure",
	"tags", "presentation", "license", "provenance",
}

// ApplyEdit applies edit to exact authored bytes and returns the new bytes.
// The error is a *Problem: nil means the result parses and is publishable; a
// validation problem still returns the edited bytes so a draft can hold work
// in progress. Only a structural failure returns nil bytes.
func ApplyEdit(source []byte, edit ProfileEdit) ([]byte, error) {
	frontmatter, body, err := splitProfile(source)
	if err != nil {
		return nil, err
	}
	root, err := decodeYAMLNode(frontmatter)
	if err != nil {
		return nil, err
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) != 1 || root.Content[0].Kind != yaml.MappingNode {
		return nil, invalid("frontmatter", "Use one YAML mapping as profile frontmatter.")
	}
	baseline, err := encodeProfile(root, body)
	if err != nil {
		return nil, err
	}
	top := root.Content[0]
	applyScalarEdits(top, edit)
	if edit.TriggerEvent != nil {
		setScalar(ensureMapping(top, "trigger"), "event", *edit.TriggerEvent)
	}
	if edit.Context != nil {
		setNode(top, "context", contextNode(mappingValue(top, "context"), *edit.Context))
	}
	if edit.Stages != nil {
		setOptional(top, "stages", stagesNode(*edit.Stages))
	}
	if edit.Authority != nil {
		setNode(top, "authority-requests", sequenceNode(*edit.Authority, true))
	}
	if edit.MayTag != nil {
		setOptional(top, "may-tag", sequenceNode(*edit.MayTag, false))
	}
	if edit.Locality != nil {
		setLocality(top, *edit.Locality)
	}
	if edit.Instructions != nil {
		body = []byte(strings.TrimRight(*edit.Instructions, "\n") + "\n")
	}
	out, err := encodeProfile(root, body)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(out, baseline) {
		// Nothing changed: keep the author's exact bytes, layout included.
		out = append([]byte(nil), source...)
	}
	_, parseErr := Parse("PROFILE.md", out)
	return out, parseErr
}

// NewSource builds a new agent's first PROFILE.md from the shipped template
// for its kind, with the given identity at version 1.0.0.
func NewSource(profile NewProfile) ([]byte, error) {
	if !profileIDPattern.MatchString(profile.ID) {
		return nil, invalid("id", "Use a lowercase profile ID up to 64 characters.")
	}
	template, err := profileTemplates.ReadFile("templates/" + profile.Type + ".PROFILE.md")
	if err != nil {
		return nil, invalid("type", "Choose reviewer, follower, or helper.")
	}
	return rewriteIdentity(template, profile.ID, profile.Name, profile.Description)
}

// Duplicate rewrites another agent's exact bytes under a new identity at
// version 1.0.0; everything else is kept as authored.
func Duplicate(source []byte, id, name, description string) ([]byte, error) {
	if !profileIDPattern.MatchString(id) {
		return nil, invalid("id", "Use a lowercase profile ID up to 64 characters.")
	}
	return rewriteIdentity(source, id, name, description)
}

func rewriteIdentity(source []byte, id, name, description string) ([]byte, error) {
	frontmatter, body, err := splitProfile(source)
	if err != nil {
		return nil, err
	}
	root, err := decodeYAMLNode(frontmatter)
	if err != nil {
		return nil, err
	}
	top := root.Content[0]
	version := "1.0.0"
	setScalar(top, "id", id)
	setScalar(top, "version", version)
	applyScalarEdits(top, ProfileEdit{Name: &name, Description: &description})
	out, err := encodeProfile(root, body)
	if err != nil {
		return nil, err
	}
	if _, err := Parse("PROFILE.md", out); err != nil {
		return nil, err
	}
	return out, nil
}

// NextMinorVersion returns the next minor SemVer release of version
// ("1.4.0" → "1.5.0"); an unparseable version yields "1.0.0".
func NextMinorVersion(version string) string {
	core := version
	if cut := strings.IndexAny(core, "-+"); cut >= 0 {
		core = core[:cut]
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return "1.0.0"
	}
	major, errMajor := strconv.Atoi(parts[0])
	minor, errMinor := strconv.Atoi(parts[1])
	if errMajor != nil || errMinor != nil {
		return "1.0.0"
	}
	return fmt.Sprintf("%d.%d.0", major, minor+1)
}

func applyScalarEdits(top *yaml.Node, edit ProfileEdit) {
	for _, field := range []struct {
		key   string
		value *string
	}{{"name", edit.Name}, {"description", edit.Description}, {"version", edit.Version}} {
		if field.value != nil {
			setScalar(top, field.key, strings.TrimSpace(*field.value))
		}
	}
	if edit.ReplyShape != nil {
		if shape := strings.TrimSpace(*edit.ReplyShape); shape == "" {
			removeKey(top, "reply-shape")
		} else {
			setScalar(top, "reply-shape", shape)
		}
	}
}

func encodeProfile(root *yaml.Node, body []byte) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := yaml.NewEncoder(&buffer)
	encoder.SetIndent(2)
	if err := encoder.Encode(root); err != nil {
		return nil, invalid("frontmatter", "The edited profile could not be written as YAML.")
	}
	if err := encoder.Close(); err != nil {
		return nil, invalid("frontmatter", "The edited profile could not be written as YAML.")
	}
	out := make([]byte, 0, buffer.Len()+len(body)+8)
	out = append(out, "---\n"...)
	out = append(out, buffer.Bytes()...)
	out = append(out, "---\n"...)
	out = append(out, body...)
	return out, nil
}

// setScalar sets key to a plain string scalar, inserting the key at its
// schema position when absent. Styles are cleared so the encoder quotes a
// value that would otherwise read back as a number or boolean.
func setScalar(mapping *yaml.Node, key, value string) {
	node := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
	if strings.Contains(value, "\n") {
		node.Style = yaml.LiteralStyle
	}
	if existing := mappingValue(mapping, key); existing != nil && existing.Kind == yaml.ScalarNode {
		node.HeadComment, node.LineComment, node.FootComment = existing.HeadComment, existing.LineComment, existing.FootComment
		if node.Style == 0 && existing.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle) != 0 {
			node.Style = existing.Style
		}
	}
	setNode(mapping, key, node)
}

func setNode(mapping *yaml.Node, key string, value *yaml.Node) {
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			mapping.Content[index+1] = value
			return
		}
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	at := insertionIndex(mapping, key)
	mapping.Content = append(mapping.Content[:at], append([]*yaml.Node{keyNode, value}, mapping.Content[at:]...)...)
}

// setOptional sets value, or removes the key when value is nil.
func setOptional(mapping *yaml.Node, key string, value *yaml.Node) {
	if value == nil {
		removeKey(mapping, key)
		return
	}
	setNode(mapping, key, value)
}

func removeKey(mapping *yaml.Node, key string) {
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			mapping.Content = append(mapping.Content[:index], mapping.Content[index+2:]...)
			return
		}
	}
}

// insertionIndex places a new top-level key after the nearest existing key
// that precedes it in the schema order; unknown or nested keys append.
func insertionIndex(mapping *yaml.Node, key string) int {
	if mappingValue(mapping, "format-version") == nil {
		return len(mapping.Content)
	}
	position := -1
	for index, name := range topLevelOrder {
		if name == key {
			position = index
			break
		}
	}
	if position < 0 {
		return len(mapping.Content)
	}
	after := -1
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		for order := 0; order < position; order++ {
			if mapping.Content[index].Value == topLevelOrder[order] {
				after = index + 2
			}
		}
	}
	if after < 0 {
		return 0
	}
	return after
}

func ensureMapping(mapping *yaml.Node, key string) *yaml.Node {
	if existing := mappingValue(mapping, key); existing != nil && existing.Kind == yaml.MappingNode {
		return existing
	}
	node := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	setNode(mapping, key, node)
	return node
}

func sequenceNode(values []string, keepEmpty bool) *yaml.Node {
	cleaned := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			cleaned = append(cleaned, value)
		}
	}
	if len(cleaned) == 0 && !keepEmpty {
		return nil
	}
	node := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	if len(cleaned) == 0 {
		node.Style = yaml.FlowStyle
	}
	for _, value := range cleaned {
		node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
	}
	return node
}

func stagesNode(stages map[string]string) *yaml.Node {
	keys := make([]string, 0, len(stages))
	for key, prompt := range stages {
		if strings.TrimSpace(key) != "" && strings.TrimSpace(prompt) != "" {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	sort.Strings(keys)
	node := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, key := range keys {
		value := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: strings.TrimRight(stages[key], "\n") + "\n", Style: yaml.LiteralStyle}
		node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
	}
	return node
}

// contextNode rebuilds the context sequence, reusing an authored item (and
// its selector and comments) when its kind survives the edit.
func contextNode(existing *yaml.Node, items []ContextEdit) *yaml.Node {
	byKind := map[string]*yaml.Node{}
	if existing != nil && existing.Kind == yaml.SequenceNode {
		for _, item := range existing.Content {
			if kind := mappingValue(item, "kind"); kind != nil && item.Kind == yaml.MappingNode {
				byKind[kind.Value] = item
			}
		}
	}
	node := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, item := range items {
		kind := strings.TrimSpace(item.Kind)
		if kind == "" {
			continue
		}
		entry := byKind[kind]
		if entry == nil {
			entry = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			setNode(entry, "kind", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: kind})
		}
		if item.Required {
			setNode(entry, "required", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"})
		} else {
			removeKey(entry, "required")
		}
		if item.MaxBytes > 0 {
			setNode(entry, "max-bytes", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(item.MaxBytes)})
		} else {
			removeKey(entry, "max-bytes")
		}
		node.Content = append(node.Content, entry)
	}
	return node
}

func setLocality(top *yaml.Node, locality string) {
	requirements := ensureMapping(top, "requirements")
	locality = strings.TrimSpace(locality)
	if locality == "" {
		destination := mappingValue(requirements, "destination")
		if destination != nil && destination.Kind == yaml.MappingNode {
			removeKey(destination, "locality")
			if len(destination.Content) == 0 {
				removeKey(requirements, "destination")
			}
		}
		return
	}
	setScalar(ensureMapping(requirements, "destination"), "locality", locality)
}
