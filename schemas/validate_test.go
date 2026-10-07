package schemas

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestEveryEmbeddedSchemaLoadsFailClosed(t *testing.T) {
	if err := CheckAll(); err != nil {
		t.Fatalf("an embedded schema uses something this engine does not implement: %v", err)
	}
	if len(Names()) < 11 {
		t.Fatalf("expected the full schema set, got %v", Names())
	}
}

func TestUnsupportedKeywordIsALoadErrorNotASilentSkip(t *testing.T) {
	for _, tc := range []struct{ name, schema, want string }{
		{"not", `{"type":"object","not":{"required":["x"]}}`, `"not"`},
		{"patternProperties", `{"type":"object","patternProperties":{"^x":{"type":"string"}}}`, `"patternProperties"`},
		{"nested", `{"type":"object","properties":{"a":{"type":"array","contains":{"type":"string"}}}}`, `"contains"`},
		{"format", `{"type":"string","format":"email"}`, `unsupported format "email"`},
		{"pattern", `{"type":"string","pattern":"^(?!/)"}`, "not RE2"},
	} {
		_, err := parseSchema(tc.name+".schema.json", []byte(tc.schema))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want a load error containing %s, got %v", tc.name, tc.want, err)
		}
	}
	// Data positions are not schema positions: a property NAMED like a keyword is fine.
	if _, err := parseSchema("ok.schema.json", []byte(`{"type":"object","properties":{"not":{"type":"string"},"contains":{"enum":["if","else"]}}}`)); err != nil {
		t.Fatalf("property names and enum members are data, not keywords: %v", err)
	}
}

// schemaFor picks the schema whose base name is the longest prefix of the fixture name.
func schemaFor(fixture string) string {
	best := ""
	for _, n := range Names() {
		base := strings.TrimSuffix(n, ".schema.json")
		if strings.HasPrefix(fixture, base) && len(base) > len(strings.TrimSuffix(best, ".schema.json")) {
			best = n
		}
	}
	return best
}

func fixtures(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("fixtures", dir))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// The README has asked for this since 2026-07-10: every fixture validated by the engine
// embedded in the product. Until this test, nothing had ever validated any of them.
func TestValidFixturesConform(t *testing.T) {
	names := fixtures(t, "valid")
	if len(names) < 11 {
		t.Fatalf("expected the pre-existing and the wire fixtures, got %v", names)
	}
	for _, name := range names {
		schema := schemaFor(name)
		if schema == "" {
			t.Errorf("%s: no schema matches this fixture name", name)
			continue
		}
		doc, err := os.ReadFile(filepath.Join("fixtures", "valid", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := Validate(schema, doc); err != nil {
			t.Errorf("valid/%s against %s: %v", name, schema, err)
		}
	}
}

func TestInvalidFixturesAreRejectedForTheirStatedReason(t *testing.T) {
	for _, name := range fixtures(t, "invalid") {
		schema := schemaFor(name)
		if schema == "" {
			t.Errorf("%s: no schema matches this fixture name", name)
			continue
		}
		doc, err := os.ReadFile(filepath.Join("fixtures", "invalid", name))
		if err != nil {
			t.Fatal(err)
		}
		verr := Validate(schema, doc)
		if verr == nil {
			t.Errorf("invalid/%s was ACCEPTED by %s", name, schema)
			continue
		}
		expected, err := os.ReadFile(filepath.Join("fixtures", "invalid", strings.TrimSuffix(name, ".json")+".expected.txt"))
		if err != nil {
			t.Errorf("invalid/%s has no .expected.txt", name)
			continue
		}
		first := strings.SplitN(string(expected), "\n", 2)[0]
		if want, ok := strings.CutPrefix(first, "error-contains: "); ok && !strings.Contains(verr.Error(), want) {
			t.Errorf("invalid/%s: rejected, but not for the stated reason\n want substring: %s\n got: %v", name, want, verr)
		}
	}
}

func TestKeywordSemantics(t *testing.T) {
	ok := func(schema, doc string) {
		t.Helper()
		loadSynthetic(t, schema)
		if err := Validate("synthetic.schema.json", []byte(doc)); err != nil {
			t.Errorf("schema %s should accept %s: %v", schema, doc, err)
		}
	}
	bad := func(schema, doc, want string) {
		t.Helper()
		loadSynthetic(t, schema)
		err := Validate("synthetic.schema.json", []byte(doc))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("schema %s should reject %s with %q, got %v", schema, doc, want, err)
		}
	}
	ok(`{"type":"integer","minimum":1}`, `3`)
	bad(`{"type":"integer"}`, `3.5`, "want type")
	bad(`{"type":"integer","minimum":1}`, `0`, "below minimum")
	bad(`{"type":"string","maxLength":2}`, `"abc"`, "above maxLength")
	ok(`{"type":"string","format":"date-time"}`, `"2026-09-17T14:03:11.5Z"`)
	bad(`{"type":"string","format":"date-time"}`, `"yesterday"`, "RFC 3339")
	bad(`{"type":"array","uniqueItems":true}`, `[1,2,1]`, "not unique")
	bad(`{"type":"array","minItems":1}`, `[]`, "below minItems")
	ok(`{"oneOf":[{"type":"string"},{"type":"null"}]}`, `null`)
	bad(`{"oneOf":[{"type":"string"},{"type":"string","minLength":1}]}`, `"x"`, "matches 2 of the oneOf")
	ok(`{"enum":["a",null]}`, `null`)
	bad(`{"enum":["a",null]}`, `"b"`, "not in enum")
	bad(`{"type":"object","additionalProperties":false,"properties":{"a":{}}}`, `{"b":1}`, `additional property "b"`)
	bad(`{"type":"object","required":["a"]}`, `{}`, `missing required property "a"`)
	bad(`{"type":"object","minProperties":1}`, `{}`, "below minProperties")
	ok(`{"type":"object","if":{"properties":{"k":{"const":"x"}},"required":["k"]},"then":{"required":["v"]}}`, `{"k":"y"}`)
	bad(`{"type":"object","if":{"properties":{"k":{"const":"x"}},"required":["k"]},"then":{"required":["v"]}}`, `{"k":"x"}`, `missing required property "v"`)
	bad(`{"allOf":[{"type":"string"},{"minLength":3}]}`, `"ab"`, "below minLength")
}

// loadSynthetic swaps one schema into the loaded set for keyword-level tests.
func loadSynthetic(t *testing.T, schema string) {
	t.Helper()
	all, err := load()
	if err != nil {
		t.Fatal(err)
	}
	root, err := parseSchema("synthetic.schema.json", []byte(schema))
	if err != nil {
		t.Fatal(err)
	}
	all["synthetic.schema.json"] = root
	t.Cleanup(func() { delete(all, "synthetic.schema.json") })
}

func TestDescribeListsTheWireRecordsAtTheirVersions(t *testing.T) {
	infos, err := Describe()
	if err != nil {
		t.Fatal(err)
	}
	wire := map[string]string{}
	for _, in := range infos {
		if in.ID == "" || in.Version == "" || in.Title == "" {
			t.Errorf("%s: incomplete identity %+v", in.File, in)
		}
		if in.Wire != IsWire(in.File) {
			t.Errorf("%s: Wire flag disagrees with IsWire", in.File)
		}
		if in.Wire {
			wire[in.File] = in.Version
		}
	}
	if wire["memory.schema.json"] != "1.1" { // team item 5: base_content_hash (decision 13b)
		t.Errorf("memory.schema.json: want wire version 1.1, got %q", wire["memory.schema.json"])
	}
	// Team rest-of-release: bundle 1.1 (caps; accepts 1.0 documents) and handoff 1.1
	// (title, typed recipient, remaining list, agent references, excerpt).
	for _, file := range []string{"bundle.schema.json", "handoff.schema.json"} {
		if wire[file] != "1.1" {
			t.Errorf("%s: want wire version 1.1, got %q", file, wire[file])
		}
	}
	for _, file := range []string{"event.schema.json", "handoff-receipt.schema.json", "tombstone.schema.json", "device-report.schema.json", "session.schema.json", "session-checkpoint-fact.schema.json", "session-content.schema.json"} {
		if wire[file] != "1.0" {
			t.Errorf("%s: want wire version 1.0, got %q", file, wire[file])
		}
	}
	if len(wire) != 10 || IsWire("common.schema.json") || IsWire("rule.schema.json") {
		t.Fatalf("exactly ten wire records; common and the drafts are not: %v", wire)
	}
}

// A server answers a device with WHERE and WHICH RULE, never the value: the offending
// value of an absolute path is a home directory, and the offending value of a leaked
// credential is the credential.
func TestViolationsNeverQuoteTheDocument(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("fixtures", "invalid", "event-absolute-target.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Violations("event.schema.json", doc)
	if err != nil || len(got) == 0 {
		t.Fatalf("want violations, got %v err=%v", got, err)
	}
	sawPattern := false
	for _, v := range got {
		if strings.Contains(v.Path, "/Users/") || strings.Contains(v.Keyword, "/Users/") || strings.Contains(v.Keyword, "secret") {
			t.Fatalf("a violation quoted the document: %+v", v)
		}
		if v.Path == "$.payload.target.path" && strings.HasSuffix(v.Keyword, "pattern") {
			sawPattern = true
		}
	}
	if !sawPattern {
		t.Fatalf("the cause must still be findable — path and keyword: %+v", got)
	}
	// The human-facing form still quotes, because a developer at a terminal needs it.
	if verr := Validate("event.schema.json", doc); verr == nil || !strings.Contains(verr.Error(), "/Users/") {
		t.Fatalf("Validate keeps its developer-facing detail: %v", verr)
	}
	if got, err := Violations("device-report.schema.json", []byte(`{"unexpected-key-name":1}`)); err != nil || len(got) == 0 {
		t.Fatalf("want violations: %v %v", got, err)
	} else {
		for _, v := range got {
			if strings.Contains(v.Path+v.Keyword, "unexpected-key-name") {
				t.Fatalf("a submitted key name is document content too: %+v", v)
			}
		}
	}
	if got, err := Violations("device-report.schema.json", []byte(`not json, maybe a secret`)); err == nil || got != nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("a parse failure must not echo the body: %v %v", got, err)
	}
}
