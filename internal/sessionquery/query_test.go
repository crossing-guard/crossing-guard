package sessionquery

import (
	"errors"
	"go/parser"
	gotoken "go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

var testLimits = Limits{QueryBytes: 400, Terms: 12, GlobExpansion: 8}

const day = int64(86400)
const testNow = 100 * day

func mustBind(t *testing.T, text string, rows []Row) Bound {
	t.Helper()
	query, err := Parse(text, testLimits)
	if err != nil {
		t.Fatalf("parse %q: %v", text, err)
	}
	bound, err := query.Bind(NewVocabulary(rows), testLimits, testNow)
	if err != nil {
		t.Fatalf("bind %q: %v", text, err)
	}
	return bound
}

func matching(t *testing.T, text string, rows []Row) string {
	t.Helper()
	bound := mustBind(t, text, rows)
	var titles []string
	for _, row := range rows {
		if bound.Matches(row) {
			titles = append(titles, row.Title)
		}
	}
	return strings.Join(titles, ",")
}

func sampleRows() []Row {
	return []Row{
		{Title: "walmart", Repository: "/work/example-app", Runtime: "claude", Branch: "feat/walmart-settle",
			Note: "Waiting on finance", TouchedAt: testNow - 2*day, Tags: []Tag{
				{Key: "Topic", Value: "Walmart", Owner: true, At: testNow - 2*day},
				{Key: "phase", Value: "plan", At: testNow - 3*day}}},
		{Title: "routing", Repository: "/work/example-app", Runtime: "claude", TouchedAt: testNow - 9*day, Tags: []Tag{
			{Key: "topic", Value: "amazon-routing", Owner: true, At: testNow - 9*day},
			{Key: "phase", Value: "plan", At: testNow - 9*day}, {Key: "fs", Value: "edit", At: testNow - 8*day}}},
		{Title: "fba", Repository: "/work/example-app", Runtime: "codex", TouchedAt: testNow - 4*day, Tags: []Tag{
			{Key: "topic", Value: "amazon-fba", Owner: true, At: testNow - 4*day},
			{Key: "phase", Value: "plan", At: testNow - 5*day}, {Key: "vcs", Value: "commit", At: testNow - 4*day}}},
		{Title: "due diligence", Repository: "/work/sample-shop", Runtime: "claude", Note: "Lawyer call Thursday",
			TouchedAt: testNow - 7*day, Status: "running", Open: true, Tags: []Tag{
				{Value: "follow-up", Owner: true, At: testNow - 7*day}, {Value: "plan", At: testNow - 7*day}}},
		{Title: "bare", Repository: "/work/cart", Runtime: "opencode", TouchedAt: testNow - 20*day},
	}
}

func TestGrammarTable(t *testing.T) {
	rows := sampleRows()
	for query, want := range map[string]string{
		"":                               "walmart,routing,fba,due diligence,bare",
		"tag:phase=plan":                 "walmart,routing,fba",
		"tag:phase:plan":                 "walmart,routing,fba", // the on-screen spelling
		"tag:PHASE=Plan":                 "walmart,routing,fba", // capitals are ignored
		"tag:topic=amazon*":              "routing,fba",
		"tag:topic=*":                    "walmart,routing,fba",
		"tag:topic:walmart":              "walmart",
		"tag:plan":                       "walmart,routing,fba,due diligence", // keyless: a value under any key
		"tag:phase":                      "",                                  // never matches a key
		"mine:plan":                      "",                                  // the owner applied no such tag
		"mine:follow-up":                 "due diligence",
		"tag:phase=plan -tag:vcs=commit": "walmart,routing",
		"tag:phase=plan -tag:fs=edit -tag:vcs=commit": "walmart",
		"-tag:phase=plan":                        "due diligence,bare",
		"tag:topic=walmart tag:topic=amazon-fba": "walmart,fba", // one key repeated: either value
		"tag:phase=plan tag:topic=amazon*":       "routing,fba", // two keys: both required
		"tag:plan mine:follow-up":                "due diligence",
		"tag:phase=plan mine:follow-up":          "",
		"-tag:vcs=commit -tag:fs=edit":           "walmart,due diligence,bare",
		"-tag:nothing-has-this":                  "walmart,routing,fba,due diligence,bare", // excluded no-match holds
		"tag:nothing-has-this":                   "",
		"repo:example-app":                       "walmart,routing,fba",
		"repo:example-app repo:cart":             "walmart,routing,fba,bare", // a repeated field is either
		"-repo:example-app":                      "due diligence,bare",
		"runtime:codex":                          "fba",
		"branch:feat/":                           "walmart",
		`title:"due diligence"`:                  "due diligence",
		"note:lawyer":                            "due diligence",
		"touched:<5d":                            "walmart,fba",
		"touched:>8d":                            "routing,bare",
		"touched:>3d touched:<8d":                "fba,due diligence", // two ages are a range, not alternatives
		"title:plan title:due":                   "",                  // two required words, not either
		"status:running status:unknown":          "walmart,routing,fba,due diligence,bare",
		"tagged:>5d":                             "routing,due diligence",
		"status:running":                         "due diligence",
		"status:unknown":                         "walmart,routing,fba,bare", // no frame published = unknown
		"-status:running":                        "walmart,routing,fba,bare",
		"open:yes":                               "due diligence",
		"open:no":                                "walmart,routing,fba,bare",
	} {
		if got := matching(t, query, rows); got != want {
			t.Errorf("%q\n got: %s\nwant: %s", query, got, want)
		}
	}
}

func TestMalformedQueriesNameWhatWasTyped(t *testing.T) {
	for query, mention := range map[string]string{
		"bogus:x":                `"bogus:"`,
		"tag:":                   `"tag:"`,
		"tag:topic=":             `"tag:topic="`,
		"touched:soon":           `"touched:soon"`,
		"touched:<0d":            `"touched:<0d"`,
		"open:maybe":             `"open:maybe"`,
		"-loose":                 `"-loose"`,
		`title:"unclosed`:        "quote",
		strings.Repeat("x ", 13): "more than 12 terms",
		strings.Repeat("y", 401): "longer than 400 bytes",
	} {
		_, err := Parse(query, testLimits)
		if !errors.Is(err, ErrQuery) || !strings.Contains(err.Error(), mention) {
			t.Errorf("%.30q: want an ErrQuery mentioning %s, got %v", query, mention, err)
		}
	}
}

func TestWordsAreKeptForTheSearchLegAndMakeAQueryUndurable(t *testing.T) {
	query, err := Parse(`repo:example-app repricer "margin floor"`, testLimits)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(query.Words(), "|"); got != "repricer|margin floor" {
		t.Fatalf("words = %q", got)
	}
	for text, durable := range map[string]bool{
		"tag:phase=plan -tag:approved touched:>3d": true, "": true,
		"repo:example-app repricer": false, "status:running": false, "-open:yes": false,
	} {
		query, err := Parse(text, testLimits)
		if err != nil || query.Durable() != durable {
			t.Errorf("%q durable=%v err=%v, want %v", text, query.Durable(), err, durable)
		}
	}
}

// engine.Match reads an empty term value as "any value of this key". A tag
// with an empty value must therefore never become a leaf, or one odd facet
// would turn a precise term into a wildcard.
func TestEmptyTagValuesNeverWidenATerm(t *testing.T) {
	rows := []Row{
		{Title: "odd", Tags: []Tag{{Key: "phase", Value: ""}}},
		{Title: "plan", Tags: []Tag{{Key: "phase", Value: "plan"}}},
		{Title: "red", Tags: []Tag{{Key: "phase", Value: "red-team"}}},
	}
	if got := matching(t, "tag:phase=plan", rows); got != "plan" {
		t.Fatalf("got %q", got)
	}
	if got := matching(t, "tag:phase=*", rows); got != "plan,red" {
		t.Fatalf("a valueless tag is not a tag: got %q", got)
	}
}

// Truncating an expansion would make an excluded glob true for the sessions
// whose value was cut. Overflow is an error that names the term.
func TestGlobOverflowIsAnErrorNotATruncation(t *testing.T) {
	var rows []Row
	for i := 0; i < testLimits.GlobExpansion+1; i++ {
		rows = append(rows, Row{Title: "r", Tags: []Tag{{Key: "topic", Value: "amazon-" + strconv.Itoa(i)}}})
	}
	for _, text := range []string{"tag:topic=amazon*", "-tag:topic=amazon*"} {
		query, err := Parse(text, testLimits)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := query.Bind(NewVocabulary(rows), testLimits, testNow); !errors.Is(err, ErrQuery) || !strings.Contains(err.Error(), "amazon*") {
			t.Errorf("%q: want an overflow error naming the term, got %v", text, err)
		}
	}
}

func TestInViewSinceIsTheAgeOfTheAdmittingFact(t *testing.T) {
	rows := sampleRows()
	for query, want := range map[string]int64{
		"tag:phase=plan":                 testNow - 3*day, // when the detector first saw it
		"tag:phase=plan tag:topic=*":     testNow - 2*day, // the later of two required facts
		"mine:topic=walmart":             testNow - 2*day,
		"repo:example-app":               testNow - 2*day, // no tag term: last activity
		"tag:phase=plan -tag:vcs=commit": testNow - 3*day, // an exclusion admits nothing
	} {
		if got := mustBind(t, query, rows).InViewSince(rows[0]); got != want {
			t.Errorf("%q: since=%d want %d", query, got, want)
		}
	}
}

func TestGroupByAndSortAreValidated(t *testing.T) {
	for text, want := range map[string]string{"": "repository", "repository": "repository", "runtime": "runtime",
		"none": "none", "tag-key:Topic": "tag-key:topic"} {
		group, err := ParseGroupBy(text)
		if err != nil || group.String() != want {
			t.Errorf("group %q = %q %v", text, group.String(), err)
		}
	}
	for _, bad := range []string{"tag-key:", "tag-key:a:b", "folder", "tag-key:x*"} {
		if _, err := ParseGroupBy(bad); !errors.Is(err, ErrQuery) {
			t.Errorf("group %q accepted", bad)
		}
	}
	for text, want := range map[string]string{"": SortNewest, "oldest": SortOldest, "longest": SortLongest} {
		if got, err := ParseSort(text); err != nil || got != want {
			t.Errorf("sort %q = %q %v", text, got, err)
		}
	}
	if _, err := ParseSort("priority"); !errors.Is(err, ErrQuery) {
		t.Error("unknown sort accepted")
	}
}

// One predicate evaluator (ADR 0025): this package states tag terms and lets
// engine.Match decide them. It must not grow a matcher of its own, and it must
// stay importable without a daemon.
func TestPackageDecidesTagTermsOnlyThroughEngineMatch(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	callsMatch := false
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(gotoken.NewFileSet(), filepath.Join(".", name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range file.Imports {
			path := strings.Trim(spec.Path.Value, `"`)
			if strings.HasPrefix(path, "crossing-guard/") && path != "crossing-guard/engine" {
				t.Errorf("%s imports %s; this package may import engine only", name, path)
			}
			if path == "regexp" {
				t.Errorf("%s imports regexp; globs expand to exact leaves, there is no pattern matching of tags here", name)
			}
		}
		source, _ := os.ReadFile(name)
		if strings.Contains(string(source), "engine.Match(") {
			callsMatch = true
		}
		if strings.Contains(string(source), "Matches:") {
			t.Errorf("%s builds a regex predicate; engine's pattern cache is unbounded and shared with governance", name)
		}
	}
	if !callsMatch {
		t.Fatal("no call to engine.Match: tag terms must be decided by the one evaluator")
	}
}
