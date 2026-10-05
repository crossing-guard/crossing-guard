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
		{Title: "checkout", Repository: "/work/example-app", Runtime: "claude", Branch: "feat/checkout-settle",
			Note: "Waiting on finance", TouchedAt: testNow - 2*day, Tags: []Tag{
				{Key: "Topic", Value: "Checkout", Owner: true, At: testNow - 2*day},
				{Key: "phase", Value: "plan", At: testNow - 3*day}}},
		{Title: "routing", Repository: "/work/example-app", Runtime: "claude", TouchedAt: testNow - 9*day, Tags: []Tag{
			{Key: "topic", Value: "order-routing", Owner: true, At: testNow - 9*day},
			{Key: "phase", Value: "plan", At: testNow - 9*day}, {Key: "fs", Value: "edit", At: testNow - 8*day}}},
		{Title: "export", Repository: "/work/example-app", Runtime: "codex", TouchedAt: testNow - 4*day, Tags: []Tag{
			{Key: "topic", Value: "order-export", Owner: true, At: testNow - 4*day},
			{Key: "phase", Value: "plan", At: testNow - 5*day}, {Key: "vcs", Value: "commit", At: testNow - 4*day}}},
		{Title: "sprint retro", Repository: "/work/sample-shop", Runtime: "claude", Note: "Designer call Thursday",
			TouchedAt: testNow - 7*day, Status: "running", Open: true, Tags: []Tag{
				{Value: "follow-up", Owner: true, At: testNow - 7*day}, {Value: "plan", At: testNow - 7*day}}},
		{Title: "bare", Repository: "/work/cart", Runtime: "opencode", TouchedAt: testNow - 20*day},
	}
}

func TestGrammarTable(t *testing.T) {
	rows := sampleRows()
	for query, want := range map[string]string{
		"":                               "checkout,routing,export,sprint retro,bare",
		"tag:phase=plan":                 "checkout,routing,export",
		"tag:phase:plan":                 "checkout,routing,export", // the on-screen spelling
		"tag:PHASE=Plan":                 "checkout,routing,export", // capitals are ignored
		"tag:topic=order*":               "routing,export",
		"tag:topic=*":                    "checkout,routing,export",
		"tag:topic:checkout":             "checkout",
		"tag:plan":                       "checkout,routing,export,sprint retro", // keyless: a value under any key
		"tag:phase":                      "",                                     // never matches a key
		"mine:plan":                      "",                                     // the owner applied no such tag
		"mine:follow-up":                 "sprint retro",
		"tag:phase=plan -tag:vcs=commit": "checkout,routing",
		"tag:phase=plan -tag:fs=edit -tag:vcs=commit": "checkout",
		"-tag:phase=plan": "sprint retro,bare",
		"tag:topic=checkout tag:topic=order-export": "checkout,export", // one key repeated: either value
		"tag:phase=plan tag:topic=order*":           "routing,export",  // two keys: both required
		"tag:plan mine:follow-up":                   "sprint retro",
		"tag:phase=plan mine:follow-up":             "",
		"-tag:vcs=commit -tag:fs=edit":              "checkout,sprint retro,bare",
		"-tag:nothing-has-this":                     "checkout,routing,export,sprint retro,bare", // excluded no-match holds
		"tag:nothing-has-this":                      "",
		"repo:example-app":                          "checkout,routing,export",
		"repo:example-app repo:cart":                "checkout,routing,export,bare", // a repeated field is either
		"-repo:example-app":                         "sprint retro,bare",
		"runtime:codex":                             "export",
		"branch:feat/":                              "checkout",
		`title:"sprint retro"`:                      "sprint retro",
		"note:designer":                             "sprint retro",
		"touched:<5d":                               "checkout,export",
		"touched:>8d":                               "routing,bare",
		"touched:>3d touched:<8d":                   "export,sprint retro", // two ages are a range, not alternatives
		"title:plan title:due":                      "",                    // two required words, not either
		"status:running status:unknown":             "checkout,routing,export,sprint retro,bare",
		"tagged:>5d":                                "routing,sprint retro",
		"status:running":                            "sprint retro",
		"status:unknown":                            "checkout,routing,export,bare", // no frame published = unknown
		"-status:running":                           "checkout,routing,export,bare",
		"open:yes":                                  "sprint retro",
		"open:no":                                   "checkout,routing,export,bare",
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
	query, err := Parse(`repo:example-app indexer "latency budget"`, testLimits)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(query.Words(), "|"); got != "indexer|latency budget" {
		t.Fatalf("words = %q", got)
	}
	for text, durable := range map[string]bool{
		"tag:phase=plan -tag:approved touched:>3d": true, "": true,
		"repo:example-app indexer": false, "status:running": false, "-open:yes": false,
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
		rows = append(rows, Row{Title: "r", Tags: []Tag{{Key: "topic", Value: "order-" + strconv.Itoa(i)}}})
	}
	for _, text := range []string{"tag:topic=order*", "-tag:topic=order*"} {
		query, err := Parse(text, testLimits)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := query.Bind(NewVocabulary(rows), testLimits, testNow); !errors.Is(err, ErrQuery) || !strings.Contains(err.Error(), "order*") {
			t.Errorf("%q: want an overflow error naming the term, got %v", text, err)
		}
	}
}

func TestInViewSinceIsTheAgeOfTheAdmittingFact(t *testing.T) {
	rows := sampleRows()
	for query, want := range map[string]int64{
		"tag:phase=plan":                 testNow - 3*day, // when the detector first saw it
		"tag:phase=plan tag:topic=*":     testNow - 2*day, // the later of two required facts
		"mine:topic=checkout":            testNow - 2*day,
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

func notesFor(t *testing.T, text string, rows []Row) []Note {
	t.Helper()
	query, err := Parse(text, testLimits)
	if err != nil {
		t.Fatalf("parse %q: %v", text, err)
	}
	return query.Notes(NewVocabulary(rows))
}

func suggestions(notes []Note) string {
	var out []string
	for _, note := range notes {
		out = append(out, note.Term+"→"+note.Suggest)
	}
	return strings.Join(out, ",")
}

// A keyless tag term is a value term. When its word is only ever a key, the
// term can never hold, and the owner almost certainly meant key=*: the board
// incident of 2026-09-29 (tag:flow over sessions tagged flow=building).
func TestNotesNameAKeylessTermWhoseWordIsOnlyAKey(t *testing.T) {
	flow := []Row{
		{Title: "a", Tags: []Tag{{Key: "flow", Value: "building", Owner: true}}},
		{Title: "b", Tags: []Tag{{Key: "Flow", Value: "review", Owner: true}, {Value: "loose"},
			{Key: "work", Value: "uncommitted"}}},
	}
	for text, want := range map[string]string{
		"tag:flow":                 "tag:flow→tag:flow=*",
		"-tag:flow":                "-tag:flow→-tag:flow=*",
		"tag:Flow":                 "tag:Flow→tag:flow=*",
		"mine:flow":                "mine:flow→mine:flow=*",
		"tag:flow repo:x tag:work": "tag:flow→tag:flow=*,tag:work→tag:work=*", // query order
		"tag:flow=x":               "",                                        // keyed
		"tag:flow=*":               "",
		"tag:fl*":                  "", // a glob is a pattern, not a mistaken key
		"tag:building":             "", // a value: the term selects something
		"tag:nothing":              "", // neither key nor value
		"tag:tag":                  "", // the keyless projection key is never offered
		"mine:work":                "", // work is a detector key; mine: cannot reach it
	} {
		if got := suggestions(notesFor(t, text, flow)); got != want {
			t.Errorf("%q: notes %q, want %q", text, got, want)
		}
	}
	withValue := append(flow, Row{Title: "c", Tags: []Tag{{Key: "stage", Value: "flow"}}})
	if got := notesFor(t, "tag:flow", withValue); len(got) != 0 {
		t.Errorf("a value flow exists, so tag:flow selects something; got %v", got)
	}
	// mine: is judged on the owner's tags only: an agent's keyless value flow
	// (projected under tag) makes tag:flow select it, but not mine:flow.
	agentValue := append(flow, Row{Title: "d", Tags: []Tag{{Value: "flow"}}})
	if got := suggestions(notesFor(t, "mine:flow tag:flow", agentValue)); got != "mine:flow→mine:flow=*" {
		t.Errorf("owner key flow + agent value flow: notes %q, want only the mine: term", got)
	}
	if query, _ := Parse("tag:flow=x mine:a* repo:x", testLimits); query.MayHaveNotes() {
		t.Error("a query with no keyless, glob-free tag term has nothing to note")
	}
	if query, _ := Parse("repo:x -mine:flow", testLimits); !query.MayHaveNotes() {
		t.Error("-mine:flow may be noted")
	}
	note := notesFor(t, "tag:flow", flow)[0]
	if !strings.Contains(note.Problem, `"flow"`) || !strings.Contains(note.Problem, "tag:flow=*") {
		t.Errorf("problem sentence %q must name the value looked for and the suggestion", note.Problem)
	}
}

// Notes are advice beside the query; they never change what it selects.
func TestNotesNeverChangeWhatAQueryMatches(t *testing.T) {
	rows := append(sampleRows(), Row{Title: "flow", Tags: []Tag{{Key: "flow", Value: "building", Owner: true}}})
	for _, text := range []string{"tag:flow", "-tag:flow", "tag:phase", "mine:topic tag:plan", "tag:flow=*"} {
		before := matching(t, text, rows)
		_ = notesFor(t, text, rows)
		if after := matching(t, text, rows); after != before {
			t.Errorf("%q: matches changed from %q to %q", text, before, after)
		}
	}
	if got := matching(t, "tag:flow", rows); got != "" {
		t.Errorf("tag:flow still means the value flow; got %q", got)
	}
}

func TestTagKeysNamesEveryKeyedTagTerm(t *testing.T) {
	query, err := Parse("tag:Flow=building -mine:Stage=done tag:plain repo:app mine:topic=*", Limits{QueryBytes: 600, Terms: 24, GlobExpansion: 200})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(query.TagKeys(), ","); got != "flow,stage,topic" {
		t.Fatalf("TagKeys = %q", got)
	}
}

func TestNumberTermsSelectByCallsAndLines(t *testing.T) {
	rows := []Row{{Title: "probe", Calls: 1, Lines: 4}, {Title: "short", Calls: 5, Lines: 60},
		{Title: "long", Calls: 40, Lines: 900}, {Title: "untracked", Calls: 0, Lines: 300}}
	for query, want := range map[string]string{
		"calls:>5":            "long",
		"calls:>0":            "probe,short,long",
		"calls:<1":            "untracked",
		"calls:>1 calls:<40":  "short",
		"-calls:<2":           "short,long",
		"lines:>50":           "short,long,untracked",
		"lines:<20":           "probe",
		"calls:>4 lines:<100": "short",
	} {
		if got := matching(t, query, rows); got != want {
			t.Errorf("%q matched %q, want %q", query, got, want)
		}
	}
}

func TestNumberTermsRefuseWhatTheyCannotDecide(t *testing.T) {
	for _, text := range []string{"calls:5", "calls:>x", "calls:>-1", "calls:>+5", "calls:<0", "lines:>", "lines:=3", "calls:>5d"} {
		_, err := Parse(text, testLimits)
		if !errors.Is(err, ErrQuery) || !strings.Contains(err.Error(), "write a count like") {
			t.Errorf("%q: %v", text, err)
		}
	}
}

// A size term holds still, so a view using it is counted and a placement
// rule may use it; it names no tag key; and a caller can tell it is there.
func TestNumberTermsAreDurableAndNameNoTag(t *testing.T) {
	query, err := Parse("calls:>5 tag:phase=plan", testLimits)
	if err != nil {
		t.Fatal(err)
	}
	if !query.Durable() || !query.HasNumberTerm() || strings.Join(query.TagKeys(), ",") != "phase" {
		t.Fatalf("durable %v, number term %v, tag keys %v", query.Durable(), query.HasNumberTerm(), query.TagKeys())
	}
	if plain, _ := Parse("tag:phase=plan touched:<7d", testLimits); plain.HasNumberTerm() {
		t.Fatal("a query without a size term reports one")
	}
}
