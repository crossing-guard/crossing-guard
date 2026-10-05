package sessionquery

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// ErrQuery marks a query the owner must fix. Its message quotes what he typed.
var ErrQuery = errors.New("query not understood")

func queryError(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrQuery}, args...)...)
}

// ageBound is "<7d" or ">14d": younger or older than a span.
type ageBound struct {
	Older   bool
	Seconds int64
}

// numberBound is "<20" or ">5": fewer or more than a count.
type numberBound struct {
	More  bool
	Count int
}

var ageUnits = map[byte]int64{'h': 3600, 'd': 86400, 'w': 7 * 86400}

// Parse reads a query. Terms are field:value, -field:value excludes, a value
// with spaces is quoted (title:"sprint retro"), and anything else is a search
// word. A tag's key and value may be joined by = or by : — the second is how a
// tag reads on screen, so typing what is on screen works.
func Parse(text string, limits Limits) (Query, error) {
	if limits.QueryBytes > 0 && len(text) > limits.QueryBytes {
		return Query{}, queryError("the filter is longer than %d bytes", limits.QueryBytes)
	}
	tokens, err := tokenize(text)
	if err != nil {
		return Query{}, err
	}
	if limits.Terms > 0 && len(tokens) > limits.Terms {
		return Query{}, queryError("more than %d terms", limits.Terms)
	}
	var query Query
	for _, tok := range tokens {
		if !tok.hasField {
			if tok.negated {
				return Query{}, queryError("%q — only a field can be excluded", tok.raw)
			}
			query.words = append(query.words, tok.value)
			continue
		}
		parsed, err := parseTerm(tok)
		if err != nil {
			return Query{}, err
		}
		query.terms = append(query.terms, parsed)
	}
	return query, nil
}

type token struct {
	raw      string
	negated  bool
	hasField bool
	name     string
	value    string
}

// tokenize splits on spaces outside quotes. A quote may open a whole word
// ("two words") or the value of a field (title:"two words").
func tokenize(text string) ([]token, error) {
	var tokens []token
	var raw strings.Builder
	quoted := false
	flush := func() error {
		if raw.Len() == 0 {
			return nil
		}
		tok, err := readToken(raw.String())
		raw.Reset()
		if err != nil {
			return err
		}
		tokens = append(tokens, tok)
		return nil
	}
	for _, r := range text {
		switch {
		case r == '"':
			quoted = !quoted
			raw.WriteRune(r)
		case unicode.IsSpace(r) && !quoted:
			if err := flush(); err != nil {
				return nil, err
			}
		default:
			raw.WriteRune(r)
		}
	}
	if quoted {
		return nil, queryError("%q — a quote is not closed", raw.String())
	}
	return tokens, flush()
}

func readToken(raw string) (token, error) {
	tok := token{raw: raw}
	body := raw
	if strings.HasPrefix(body, "-") && len(body) > 1 {
		tok.negated, body = true, body[1:]
	}
	if strings.HasPrefix(body, `"`) {
		tok.value = strings.Trim(body, `"`)
		return tok, emptyValue(tok)
	}
	name, value, found := strings.Cut(body, ":")
	if !found {
		tok.value = body
		return tok, nil
	}
	tok.hasField, tok.name, tok.value = true, fold(name), strings.Trim(value, `"`)
	return tok, emptyValue(tok)
}

func emptyValue(tok token) error {
	if strings.TrimSpace(tok.value) == "" {
		return queryError("%q needs a value", tok.raw)
	}
	return nil
}

func parseTerm(tok token) (term, error) {
	name, known := fields[tok.name]
	if !known {
		return term{}, queryError("%q is not a field", tok.name+":")
	}
	parsed := term{Negated: tok.negated, Field: name, Raw: tok.raw, Value: fold(tok.value)}
	switch name {
	case fieldTag, fieldMine:
		parsed.Key, parsed.Value = splitTag(parsed.Value)
		if parsed.Value == "" {
			return term{}, queryError("%q needs a value after the key", tok.raw)
		}
	case fieldTouched, fieldTagged:
		bound, err := parseAge(tok)
		if err != nil {
			return term{}, err
		}
		parsed.Age = bound
	case fieldCalls, fieldLines:
		bound, err := parseNumber(tok)
		if err != nil {
			return term{}, err
		}
		parsed.Number = bound
	case fieldOpen:
		if parsed.Value != "yes" && parsed.Value != "no" {
			return term{}, queryError("%q — write open:yes or open:no", tok.raw)
		}
	}
	return parsed, nil
}

// splitTag separates key from value at the first = or :. No separator means a
// keyless term.
func splitTag(value string) (string, string) {
	index := strings.IndexAny(value, "=:")
	if index < 0 {
		return "", value
	}
	return value[:index], value[index+1:]
}

func parseAge(tok token) (ageBound, error) {
	value := tok.value
	problem := queryError("%q — write an age like <7d or >14d (h, d or w)", tok.raw)
	if len(value) < 3 || (value[0] != '<' && value[0] != '>') {
		return ageBound{}, problem
	}
	unit, known := ageUnits[value[len(value)-1]]
	count, err := strconv.ParseInt(value[1:len(value)-1], 10, 32)
	if !known || err != nil || count <= 0 {
		return ageBound{}, problem
	}
	return ageBound{Older: value[0] == '>', Seconds: count * unit}, nil
}

// parseNumber reads a count bound: < or > and digits, nothing else. "<0" is
// refused because no count is below zero, so the term could never hold.
func parseNumber(tok token) (numberBound, error) {
	value := tok.value
	problem := queryError("%q — write a count like %s:>5 or %s:<20", tok.raw, tok.name, tok.name)
	if len(value) < 2 || (value[0] != '<' && value[0] != '>') {
		return numberBound{}, problem
	}
	for _, digit := range value[1:] {
		if digit < '0' || digit > '9' {
			return numberBound{}, problem
		}
	}
	count, err := strconv.Atoi(value[1:])
	if err != nil || (value[0] == '<' && count == 0) {
		return numberBound{}, problem
	}
	return numberBound{More: value[0] == '>', Count: count}, nil
}

// GroupBy is how a result is split into groups.
type GroupBy struct {
	Kind string // repository | runtime | none | tag-key
	Key  string // tag-key only, folded
}

const (
	GroupRepository = "repository"
	GroupRuntime    = "runtime"
	GroupNone       = "none"
	GroupTagKey     = "tag-key"
)

// ParseGroupBy reads a grouping; empty means by repository, today's rail.
func ParseGroupBy(text string) (GroupBy, error) {
	switch text {
	case "", GroupRepository:
		return GroupBy{Kind: GroupRepository}, nil
	case GroupRuntime, GroupNone:
		return GroupBy{Kind: text}, nil
	}
	if key, found := strings.CutPrefix(text, GroupTagKey+":"); found && key != "" && !strings.ContainsAny(key, ":=*\"") {
		return GroupBy{Kind: GroupTagKey, Key: fold(key)}, nil
	}
	return GroupBy{}, queryError("%q is not a grouping", text)
}

func (g GroupBy) String() string {
	if g.Kind == GroupTagKey {
		return GroupTagKey + ":" + g.Key
	}
	return g.Kind
}

// Sort orders within a group.
const (
	SortNewest  = "newest"  // most recent activity first — today's rail
	SortOldest  = "oldest"  // least recent activity first
	SortLongest = "longest" // longest in the view first
)

// ParseSort reads a sort order; empty means newest.
func ParseSort(text string) (string, error) {
	switch text {
	case "":
		return SortNewest, nil
	case SortNewest, SortOldest, SortLongest:
		return text, nil
	}
	return "", queryError("%q is not a sort order", text)
}
