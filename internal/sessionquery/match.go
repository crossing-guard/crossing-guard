package sessionquery

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"crossing-guard/engine"
)

// Vocabulary is every distinct projected (key, value) present on the rows of
// one request. A value glob is expanded against it, so a term can only ever
// name tags that exist — there is no pattern language at match time.
type Vocabulary struct {
	pairs []tagPair
	// ownerPairs are the projected pairs the owner himself has applied. Only
	// Notes reads them: a mine: term can reach nothing else.
	ownerPairs map[tagPair]bool
}

// tagPair is a projected (key, value). engine.Tag carries a slice and so
// cannot key a map.
type tagPair struct{ Key, Value string }

// NewVocabulary collects the tags of the given rows.
func NewVocabulary(rows []Row) Vocabulary {
	seen := map[tagPair]struct{}{}
	ownerPairs := map[tagPair]bool{}
	for _, row := range rows {
		for _, tag := range projectTags(row.Tags, false) {
			seen[tagPair{tag.Key, tag.Value}] = struct{}{}
		}
		for _, tag := range projectTags(row.Tags, true) {
			ownerPairs[tagPair{tag.Key, tag.Value}] = true
		}
	}
	pairs := make([]tagPair, 0, len(seen))
	for pair := range seen {
		pairs = append(pairs, pair)
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].Key != pairs[j].Key {
			return pairs[i].Key < pairs[j].Key
		}
		return pairs[i].Value < pairs[j].Value
	})
	return Vocabulary{pairs: pairs, ownerPairs: ownerPairs}
}

// boundTerm is a term ready to test: tag terms carry their predicate.
type boundTerm struct {
	term
	predicate engine.Predicate
	decidable bool // false: the term names no existing tag, so it is simply false
}

// Bound is a query bound to one request's vocabulary and clock.
type Bound struct {
	groups [][]boundTerm // terms sharing a field and polarity; any one may hold
	now    int64
}

// Bind prepares the query for matching. It fails, naming the term, if a glob
// would expand past the limit: truncating instead would make an excluded glob
// wrongly true for the sessions whose value was cut.
func (q Query) Bind(vocabulary Vocabulary, limits Limits, now int64) (Bound, error) {
	bound := Bound{now: now}
	index := map[string]int{}
	for _, t := range q.terms {
		prepared := boundTerm{term: t}
		if t.Field == fieldTag || t.Field == fieldMine {
			predicate, decidable, err := tagPredicate(t, vocabulary, limits)
			if err != nil {
				return Bound{}, err
			}
			prepared.predicate, prepared.decidable = predicate, decidable
		}
		key := groupKey(t)
		at, seen := index[key]
		if !seen {
			at = len(bound.groups)
			index[key] = at
			bound.groups = append(bound.groups, nil)
		}
		bound.groups[at] = append(bound.groups[at], prepared)
	}
	return bound, nil
}

// groupKey decides which terms are alternatives. A session has exactly one
// repository, runtime, status and open-ness, so repeating such a field can only
// mean "either". Everything else is required term by term: two tag terms are
// both required — unless they name the same key, where "topic is a or b" is the
// only useful reading — and two ages are a range ("older than 7d" AND "younger
// than 14d"; read as alternatives they would match every session).
func groupKey(t term) string {
	key := string(t.Field)
	switch t.Field {
	case fieldRepo, fieldRuntime, fieldStatus, fieldOpen:
	case fieldTag, fieldMine:
		if t.Key == "" {
			key += "\x00" + t.Raw
		} else {
			key += "\x00" + t.Key
		}
	default:
		key += "\x00" + t.Raw
	}
	if t.Negated {
		return "-" + key
	}
	return key
}

// tagPredicate expands one tag term into exact leaves. A keyed term matches
// values under that key; a keyless term matches a value under any key, never a
// key. "key=*" is the bare key term. No leaf carries an empty value, because
// engine.Match reads an empty term value as "any value".
func tagPredicate(t term, vocabulary Vocabulary, limits Limits) (engine.Predicate, bool, error) {
	if t.Key != "" && t.Value == "*" {
		return engine.Predicate{Tag: t.Key}, true, nil
	}
	var leaves []engine.Predicate
	for _, pair := range vocabulary.pairs {
		if t.Key != "" && pair.Key != t.Key {
			continue
		}
		if pair.Value == "" || !globMatch(t.Value, pair.Value) {
			continue
		}
		leaves = append(leaves, engine.Predicate{Tag: pair.Key, Value: pair.Value})
		if limits.GlobExpansion > 0 && len(leaves) > limits.GlobExpansion {
			return engine.Predicate{}, false, queryError("%q matches more than %d tags — narrow it", t.Raw, limits.GlobExpansion)
		}
	}
	if len(leaves) == 0 {
		return engine.Predicate{}, false, nil
	}
	return engine.Predicate{Any: leaves}, true, nil
}

// globMatch matches a folded pattern whose only metacharacter is *.
func globMatch(pattern, text string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == text
	}
	if !strings.HasPrefix(text, parts[0]) {
		return false
	}
	text = text[len(parts[0]):]
	last := parts[len(parts)-1]
	for _, part := range parts[1 : len(parts)-1] {
		at := strings.Index(text, part)
		if at < 0 {
			return false
		}
		text = text[at+len(part):]
	}
	return strings.HasSuffix(text, last)
}

// Matches reports whether the row satisfies every group of terms. Within a
// group — the same field, the same polarity — any one term is enough; an
// excluded group holds when none of its terms does.
func (b Bound) Matches(row Row) bool {
	all, owner := projectTags(row.Tags, false), projectTags(row.Tags, true)
	for _, group := range b.groups {
		held := false
		for _, t := range group {
			if b.holds(t, row, all, owner) {
				held = true
				break
			}
		}
		if held == group[0].Negated {
			return false
		}
	}
	return true
}

func (b Bound) holds(t boundTerm, row Row, all, owner []engine.Tag) bool {
	switch t.Field {
	case fieldTag:
		return t.decidable && engine.Match(t.predicate, all)
	case fieldMine:
		return t.decidable && engine.Match(t.predicate, owner)
	case fieldRepo:
		return strings.Contains(fold(row.Repository), t.Value)
	case fieldRuntime:
		return fold(row.Runtime) == t.Value
	case fieldBranch:
		return strings.Contains(fold(row.Branch), t.Value)
	case fieldTitle:
		return strings.Contains(fold(row.Title), t.Value)
	case fieldNote:
		return strings.Contains(fold(row.Note), t.Value)
	case fieldStatus:
		return statusWord(row.Status) == t.Value
	case fieldOpen:
		return row.Open == (t.Value == "yes")
	case fieldTouched:
		return t.Age.holds(b.now, row.TouchedAt)
	case fieldTagged:
		return taggedWithin(t.Age, b.now, row.Tags)
	case fieldCalls:
		return t.Number.holds(row.Calls)
	case fieldLines:
		return t.Number.holds(row.Lines)
	}
	return false
}

func (n numberBound) holds(count int) bool {
	if n.More {
		return count > n.Count
	}
	return count < n.Count
}

// statusWord gives a session the decider never published a frame for the word
// the decider itself uses for that: unknown. There is no third state.
func statusWord(status string) string {
	if status == "" {
		return "unknown"
	}
	return fold(status)
}

func (a ageBound) holds(now, at int64) bool {
	if at <= 0 {
		return false
	}
	age := now - at
	if a.Older {
		return age > a.Seconds
	}
	return age < a.Seconds
}

func taggedWithin(bound ageBound, now int64, tags []Tag) bool {
	for _, tag := range tags {
		if tag.Owner && bound.holds(now, tag.At) {
			return true
		}
	}
	return false
}

// InViewSince is when the row came to satisfy the query's tag terms: for each
// included tag term, the earliest matching tag; across terms, the latest of
// those, since every term must hold. With no included tag term it is the row's
// last activity. It is the named start of the age a view shows and sorts by.
func (b Bound) InViewSince(row Row) int64 {
	since := int64(0)
	for _, group := range b.groups {
		if group[0].Negated || (group[0].Field != fieldTag && group[0].Field != fieldMine) {
			continue
		}
		if earliest := earliestMatch(group, row.Tags); earliest > since {
			since = earliest
		}
	}
	if since == 0 {
		return row.TouchedAt
	}
	return since
}

func earliestMatch(group []boundTerm, tags []Tag) int64 {
	earliest := int64(0)
	for _, tag := range tags {
		if tag.At <= 0 || tag.Value == "" {
			continue
		}
		one := projectTags([]Tag{tag}, false)
		for _, t := range group {
			if t.Field == fieldMine && !tag.Owner {
				continue
			}
			if t.decidable && engine.Match(t.predicate, one) && (earliest == 0 || tag.At < earliest) {
				earliest = tag.At
			}
		}
	}
	return earliest
}

// Note is advice about one term of a query that parses and binds but, over
// the tags that exist, can only ever be false because it was probably meant
// as something else. A note never changes what the query matches.
type Note struct {
	Term    string `json:"term"`    // the term exactly as typed
	Suggest string `json:"suggest"` // the term the owner probably meant
	Problem string `json:"problem"` // one sentence, shown beside the query
}

// Notes reports each keyless tag term whose word is a tag key and never a tag
// value. A keyless term matches a value under any key and never a key, so
// tag:flow over sessions tagged flow=building selects nothing; tag:flow=* is
// the key term. A mine: term is judged on the owner's own tags only, because
// that is all mine: can reach: an agent's value flow does not make mine:flow
// select anything, and a detector's key flow is not a key mine: can name.
// The projection key of keyless tags is never offered: the owner never wrote
// it. Notes come back in query order.
func (q Query) Notes(vocabulary Vocabulary) []Note {
	var notes []Note
	for _, t := range q.terms {
		if !noteable(t) {
			continue
		}
		ownerOnly := t.Field == fieldMine
		if t.Value == KeylessTagKey || vocabulary.has(ownerOnly, func(p tagPair) bool { return p.Value == t.Value }) ||
			!vocabulary.has(ownerOnly, func(p tagPair) bool { return p.Key == t.Value }) {
			continue
		}
		notes = append(notes, keyTermNote(t))
	}
	return notes
}

// MayHaveNotes reports a query with a term Notes could speak about, so a
// caller can skip building a vocabulary for one that has none.
func (q Query) MayHaveNotes() bool {
	for _, t := range q.terms {
		if noteable(t) {
			return true
		}
	}
	return false
}

// noteable is a keyless, glob-free tag: or mine: term. A glob is a pattern,
// never a mistaken key.
func noteable(t term) bool {
	return (t.Field == fieldTag || t.Field == fieldMine) && t.Key == "" && !strings.Contains(t.Value, "*")
}

// has reports whether any pair (the owner's only, when asked) satisfies the
// test. An exact value test is what tagPredicate's expansion of a glob-free
// term reduces to (empty values are never pairs), without its overflow error.
func (v Vocabulary) has(ownerOnly bool, test func(tagPair) bool) bool {
	if ownerOnly {
		for pair := range v.ownerPairs {
			if test(pair) {
				return true
			}
		}
		return false
	}
	for _, pair := range v.pairs {
		if test(pair) {
			return true
		}
	}
	return false
}

func keyTermNote(t term) Note {
	prefix := ""
	if t.Negated {
		prefix = "-"
	}
	value := t.Value + "=*"
	if strings.ContainsFunc(value, unicode.IsSpace) {
		value = `"` + value + `"`
	}
	suggest := prefix + string(t.Field) + ":" + value
	whose, which := "the tag value", "is a tag key"
	if t.Field == fieldMine {
		whose, which = "your tag value", "is one of your tag keys"
	}
	return Note{Term: t.Raw, Suggest: suggest, Problem: fmt.Sprintf(
		"%s looks for %s %q, and no session has one. %s %s: write %s for any %s tag.",
		t.Raw, whose, t.Value, t.Value, which, suggest, t.Value)}
}
