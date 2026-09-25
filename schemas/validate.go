// Package schemas embeds the portable JSON Schemas and validates documents against them.
//
// The validator implements the JSON Schema 2020-12 SUBSET this repository actually uses
// (keyword census, team plan §5.13) and is FAIL-CLOSED: a schema that uses any keyword
// outside that subset is a load error, so no schema can rely on semantics this engine
// would silently skip. It has no dependencies — the single-static-binary promise holds —
// and the server imports this same package, which is the schemas README's long-standing
// requirement: "the same JSON Schema engine embedded in the product". Patterns are RE2.
// The schemas stay standard 2020-12, so a full engine can replace this one later.
package schemas

import (
	"embed"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

//go:embed *.schema.json
var files embed.FS

// annotations carry no validation semantics and are accepted everywhere.
var annotations = map[string]bool{"$schema": true, "$id": true, "title": true, "description": true,
	"default": true, "$comment": true, "examples": true}

// supported is the whole validation vocabulary. Anything else is a load error.
var supported = map[string]bool{"type": true, "properties": true, "required": true,
	"additionalProperties": true, "items": true, "minItems": true, "maxItems": true,
	"uniqueItems": true, "enum": true, "const": true, "pattern": true, "minLength": true,
	"maxLength": true, "minimum": true, "maximum": true, "minProperties": true, "format": true,
	"oneOf": true, "allOf": true, "if": true, "then": true, "$ref": true, "$defs": true}

var supportedFormats = map[string]bool{"date-time": true, "date": true, "uri": true}

var (
	loadOnce sync.Once
	loaded   map[string]map[string]any
	loadErr  error
	reMu     sync.Mutex
	reCache  = map[string]*regexp.Regexp{}
)

func load() (map[string]map[string]any, error) {
	loadOnce.Do(func() {
		entries, err := files.ReadDir(".")
		if err != nil {
			loadErr = err
			return
		}
		loaded = map[string]map[string]any{}
		for _, e := range entries {
			raw, err := files.ReadFile(e.Name())
			if err != nil {
				loadErr = err
				return
			}
			root, err := parseSchema(e.Name(), raw)
			if err != nil {
				loadErr = err
				return
			}
			loaded[e.Name()] = root
		}
		for name, root := range loaded {
			if err := checkRefs(name, root, loaded); err != nil {
				loadErr = err
				return
			}
		}
	})
	return loaded, loadErr
}

// CheckAll loads every embedded schema: each must parse, use only supported keywords
// and formats, compile its patterns, and resolve every $ref.
func CheckAll() error {
	_, err := load()
	return err
}

// Names lists the embedded schema files.
func Names() []string {
	all, _ := load()
	out := make([]string, 0, len(all))
	for n := range all {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// SchemaInfo describes one embedded schema, for a contract listing. Version is the last
// segment of the schema's $id ("urn:crossing-guard:schema:event:1.0" → "1.0").
type SchemaInfo struct {
	File    string `json:"file"`
	ID      string `json:"id"`
	Version string `json:"version"`
	Title   string `json:"title"`
	Wire    bool   `json:"wire"`
}

// wireRecords are the records that cross between a device and the team server (team
// plan §5.13). common is shared definitions, not a record; the 0.1 drafts are not wire.
var wireRecords = map[string]bool{"event.schema.json": true, "memory.schema.json": true,
	"handoff.schema.json": true, "tombstone.schema.json": true,
	"device-report.schema.json": true, "bundle.schema.json": true}

// IsWire reports whether file is one of the wire records.
func IsWire(file string) bool { return wireRecords[file] }

// Describe lists every embedded schema with its identity and version, sorted by file —
// what a server needs to declare, and check, which contract versions it accepts.
func Describe() ([]SchemaInfo, error) {
	all, err := load()
	if err != nil {
		return nil, err
	}
	out := make([]SchemaInfo, 0, len(all))
	for _, file := range Names() {
		root := all[file]
		id, _ := root["$id"].(string)
		title, _ := root["title"].(string)
		version := id
		if i := strings.LastIndex(id, ":"); i >= 0 {
			version = id[i+1:]
		}
		out = append(out, SchemaInfo{File: file, ID: id, Version: version, Title: title, Wire: wireRecords[file]})
	}
	return out, nil
}

func parseSchema(name string, raw []byte) (map[string]any, error) {
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	obj, ok := root.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: schema root must be an object", name)
	}
	if err := checkKeywords(name, "#", obj); err != nil {
		return nil, err
	}
	return obj, nil
}

// checkKeywords walks SCHEMA positions only — never data positions such as enum members,
// const values, or property names — and rejects anything outside the vocabulary.
func checkKeywords(file, at string, s map[string]any) error {
	for k, v := range s {
		if annotations[k] {
			continue
		}
		if !supported[k] {
			return fmt.Errorf("%s %s: unsupported schema keyword %q — this engine is fail-closed; implement it in schemas/validate.go or remove it", file, at, k)
		}
		switch k {
		case "properties", "$defs":
			m, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("%s %s/%s: must be an object", file, at, k)
			}
			for name, sub := range m {
				subObj, ok := sub.(map[string]any)
				if !ok {
					return fmt.Errorf("%s %s/%s/%s: must be a schema object", file, at, k, name)
				}
				if err := checkKeywords(file, at+"/"+k+"/"+name, subObj); err != nil {
					return err
				}
			}
		case "items", "if", "then":
			subObj, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("%s %s/%s: must be a schema object", file, at, k)
			}
			if err := checkKeywords(file, at+"/"+k, subObj); err != nil {
				return err
			}
		case "additionalProperties":
			if subObj, ok := v.(map[string]any); ok {
				if err := checkKeywords(file, at+"/"+k, subObj); err != nil {
					return err
				}
			} else if _, ok := v.(bool); !ok {
				return fmt.Errorf("%s %s/%s: must be a boolean or a schema", file, at, k)
			}
		case "oneOf", "allOf":
			list, ok := v.([]any)
			if !ok {
				return fmt.Errorf("%s %s/%s: must be an array of schemas", file, at, k)
			}
			for i, sub := range list {
				subObj, ok := sub.(map[string]any)
				if !ok {
					return fmt.Errorf("%s %s/%s/%d: must be a schema object", file, at, k, i)
				}
				if err := checkKeywords(file, fmt.Sprintf("%s/%s/%d", at, k, i), subObj); err != nil {
					return err
				}
			}
		case "pattern":
			p, _ := v.(string)
			if _, err := compiled(p); err != nil {
				return fmt.Errorf("%s %s: pattern %q is not RE2: %w", file, at, p, err)
			}
		case "format":
			f, _ := v.(string)
			if !supportedFormats[f] {
				return fmt.Errorf("%s %s: unsupported format %q — fail-closed", file, at, f)
			}
		}
	}
	return nil
}

func checkRefs(file string, node any, all map[string]map[string]any) error {
	switch n := node.(type) {
	case map[string]any:
		if r, ok := n["$ref"].(string); ok {
			if _, _, err := resolve(r, file, all); err != nil {
				return fmt.Errorf("%s: %w", file, err)
			}
		}
		for k, v := range n {
			if k == "enum" || k == "const" || k == "default" || k == "examples" {
				continue
			}
			if err := checkRefs(file, v, all); err != nil {
				return err
			}
		}
	case []any:
		for _, v := range n {
			if err := checkRefs(file, v, all); err != nil {
				return err
			}
		}
	}
	return nil
}

func compiled(p string) (*regexp.Regexp, error) {
	reMu.Lock()
	defer reMu.Unlock()
	if re, ok := reCache[p]; ok {
		return re, nil
	}
	re, err := regexp.Compile(p)
	if err != nil {
		return nil, err
	}
	reCache[p] = re
	return re, nil
}

// resolve follows "#/$defs/x" within file or "other.schema.json#/$defs/x" across files.
func resolve(ref, file string, all map[string]map[string]any) (map[string]any, string, error) {
	target, pointer := file, ref
	if i := strings.Index(ref, "#"); i >= 0 {
		if i > 0 {
			target = ref[:i]
		}
		pointer = ref[i+1:]
	} else {
		target, pointer = ref, ""
	}
	root, ok := all[target]
	if !ok {
		return nil, "", fmt.Errorf("$ref %q: no embedded schema %q", ref, target)
	}
	var cur any = root
	for _, part := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		if part == "" {
			continue
		}
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, "", fmt.Errorf("$ref %q: %q is not an object", ref, part)
		}
		if cur, ok = m[part]; !ok {
			return nil, "", fmt.Errorf("$ref %q: %q not found in %s", ref, part, target)
		}
	}
	out, ok := cur.(map[string]any)
	if !ok {
		return nil, "", fmt.Errorf("$ref %q does not point at a schema object", ref)
	}
	return out, target, nil
}

// Validate checks doc against the named embedded schema (e.g. "event.schema.json").
// The error lists every violation with its JSON path.
func Validate(schemaFile string, doc []byte) error {
	all, err := load()
	if err != nil {
		return err
	}
	root, ok := all[schemaFile]
	if !ok {
		return fmt.Errorf("no embedded schema %q", schemaFile)
	}
	var value any
	if err := json.Unmarshal(doc, &value); err != nil {
		return fmt.Errorf("document is not JSON: %w", err)
	}
	v := &validator{all: all}
	v.check(root, schemaFile, value, "$")
	if len(v.errs) == 0 {
		return nil
	}
	return fmt.Errorf("%s: %s", schemaFile, strings.Join(v.errs, "; "))
}

type validator struct {
	all  map[string]map[string]any
	errs []string
}

func (v *validator) fail(path, format string, args ...any) {
	v.errs = append(v.errs, path+": "+fmt.Sprintf(format, args...))
}

// passes evaluates a subschema without recording its errors (oneOf, if).
func (v *validator) passes(s map[string]any, file string, value any, path string) bool {
	probe := &validator{all: v.all}
	probe.check(s, file, value, path)
	return len(probe.errs) == 0
}

func (v *validator) check(s map[string]any, file string, value any, path string) {
	if r, ok := s["$ref"].(string); ok {
		target, targetFile, err := resolve(r, file, v.all)
		if err != nil {
			v.fail(path, "%v", err)
			return
		}
		v.check(target, targetFile, value, path)
	}
	if t, ok := s["type"]; ok && !typeMatches(t, value) {
		v.fail(path, "is %s, want type %v", jsonType(value), t)
		return
	}
	if c, ok := s["const"]; ok && !reflect.DeepEqual(c, value) {
		v.fail(path, "must equal %v", c)
	}
	if e, ok := s["enum"].([]any); ok {
		found := false
		for _, member := range e {
			if reflect.DeepEqual(member, value) {
				found = true
				break
			}
		}
		if !found {
			v.fail(path, "%v is not in enum %v", value, e)
		}
	}
	switch val := value.(type) {
	case string:
		v.checkString(s, val, path)
	case float64:
		if m, ok := s["minimum"].(float64); ok && val < m {
			v.fail(path, "%v is below minimum %v", val, m)
		}
		if m, ok := s["maximum"].(float64); ok && val > m {
			v.fail(path, "%v is above maximum %v", val, m)
		}
	case []any:
		v.checkArray(s, file, val, path)
	case map[string]any:
		v.checkObject(s, file, val, path)
	}
	if list, ok := s["allOf"].([]any); ok {
		for _, sub := range list {
			v.check(sub.(map[string]any), file, value, path)
		}
	}
	if list, ok := s["oneOf"].([]any); ok {
		matches := 0
		var why []string
		for i, sub := range list {
			probe := &validator{all: v.all}
			probe.check(sub.(map[string]any), file, value, path)
			if len(probe.errs) == 0 {
				matches++
				continue
			}
			why = append(why, fmt.Sprintf("alternative %d: %s", i, strings.Join(probe.errs, ", ")))
		}
		if matches == 0 {
			// Say WHY each alternative failed: "matches 0" alone names no cause.
			v.fail(path, "matches 0 of the oneOf alternatives, want exactly 1 (%s)", strings.Join(why, " | "))
		} else if matches > 1 {
			v.fail(path, "matches %d of the oneOf alternatives, want exactly 1", matches)
		}
	}
	if cond, ok := s["if"].(map[string]any); ok {
		if then, ok := s["then"].(map[string]any); ok && v.passes(cond, file, value, path) {
			v.check(then, file, value, path)
		}
	}
}

func (v *validator) checkString(s map[string]any, val, path string) {
	n := float64(utf8.RuneCountInString(val))
	if m, ok := s["minLength"].(float64); ok && n < m {
		v.fail(path, "length %v is below minLength %v", n, m)
	}
	if m, ok := s["maxLength"].(float64); ok && n > m {
		v.fail(path, "length %v is above maxLength %v", n, m)
	}
	if p, ok := s["pattern"].(string); ok {
		if re, err := compiled(p); err == nil && !re.MatchString(val) {
			v.fail(path, "%q does not match pattern %s", val, p)
		}
	}
	if f, ok := s["format"].(string); ok {
		switch f {
		case "date-time":
			if _, err := time.Parse(time.RFC3339Nano, val); err != nil {
				v.fail(path, "%q is not an RFC 3339 date-time", val)
			}
		case "date":
			if _, err := time.Parse("2006-01-02", val); err != nil {
				v.fail(path, "%q is not a date", val)
			}
		case "uri":
			if u, err := url.Parse(val); err != nil || u.Scheme == "" {
				v.fail(path, "%q is not an absolute URI", val)
			}
		}
	}
}

func (v *validator) checkArray(s map[string]any, file string, val []any, path string) {
	if m, ok := s["minItems"].(float64); ok && float64(len(val)) < m {
		v.fail(path, "has %d items, below minItems %v", len(val), m)
	}
	if m, ok := s["maxItems"].(float64); ok && float64(len(val)) > m {
		v.fail(path, "has %d items, above maxItems %v", len(val), m)
	}
	if u, ok := s["uniqueItems"].(bool); ok && u {
		for i := range val {
			for j := i + 1; j < len(val); j++ {
				if reflect.DeepEqual(val[i], val[j]) {
					v.fail(path, "items %d and %d are not unique", i, j)
				}
			}
		}
	}
	if items, ok := s["items"].(map[string]any); ok {
		for i, item := range val {
			v.check(items, file, item, fmt.Sprintf("%s[%d]", path, i))
		}
	}
}

func (v *validator) checkObject(s map[string]any, file string, val map[string]any, path string) {
	if req, ok := s["required"].([]any); ok {
		for _, r := range req {
			name, _ := r.(string)
			if _, present := val[name]; !present {
				v.fail(path, "missing required property %q", name)
			}
		}
	}
	if m, ok := s["minProperties"].(float64); ok && float64(len(val)) < m {
		v.fail(path, "has %d properties, below minProperties %v", len(val), m)
	}
	props, _ := s["properties"].(map[string]any)
	keys := make([]string, 0, len(val))
	for k := range val {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if sub, ok := props[k].(map[string]any); ok {
			v.check(sub, file, val[k], path+"."+k)
			continue
		}
		switch ap := s["additionalProperties"].(type) {
		case bool:
			if !ap {
				v.fail(path, "additional property %q is not allowed", k)
			}
		case map[string]any:
			v.check(ap, file, val[k], path+"."+k)
		}
	}
}

func typeMatches(t any, value any) bool {
	switch tt := t.(type) {
	case string:
		return oneType(tt, value)
	case []any:
		for _, x := range tt {
			if name, ok := x.(string); ok && oneType(name, value) {
				return true
			}
		}
	}
	return false
}

func oneType(name string, value any) bool {
	switch name {
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "null":
		return value == nil
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "number":
		_, ok := value.(float64)
		return ok
	case "integer":
		f, ok := value.(float64)
		return ok && f == math.Trunc(f)
	}
	return false
}

func jsonType(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "unknown"
}
