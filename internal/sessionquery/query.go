// Package sessionquery is the one grammar the console uses to filter, search
// and group sessions, and the one validator for a saved view's query.
//
// It carries no vocabulary. The field names below are syntax; every tag, key,
// repository or word a query mentions is the caller's data. It decides no tag
// term itself: a tag term becomes an engine.Predicate of exact leaves and
// engine.Match decides it (ADR 0025 — one predicate evaluator). What is new
// here is the text form, the row-field filters, and the expansion of a value
// glob into exact leaves over the tags that actually exist in one request.
//
// The package imports engine and nothing else of ours, so both the list filter
// and the saved-view validator can use it without a daemon.
package sessionquery

import (
	"strings"

	"crossing-guard/engine"
)

// KeylessTagKey is the key a keyless tag is projected under. The store refuses
// it as an owner key, so a projected keyless tag can never be mistaken for one.
const KeylessTagKey = "tag"

// Limits bounds what one query may cost. They come from configuration.
type Limits struct {
	QueryBytes    int
	Terms         int
	GlobExpansion int
}

// Tag is one tag on a row, from any source. Owner marks the owner's own.
type Tag struct {
	Key   string
	Value string
	Owner bool
	// At is when the tag became true of the session: when the owner applied
	// it, when an agent claimed it, or when a detector first saw it.
	At int64
}

// Row is a session as the grammar sees it. The daemon fills it; this package
// never reaches for a session itself.
type Row struct {
	Repository string
	Runtime    string
	Branch     string
	Title      string
	Note       string
	Status     string // the status decider's execution word; empty means unknown
	Open       bool
	TouchedAt  int64
	// Calls is the session's model calls with usage records, the number the
	// rail shows; Lines is the runtime's own transcript length. Both are zero
	// for a session whose transcript the scan did not return.
	Calls int
	Lines int
	Tags  []Tag
}

// field is one name the grammar understands to the left of a colon.
type field string

const (
	fieldTag     field = "tag"
	fieldMine    field = "mine"
	fieldRepo    field = "repo"
	fieldRuntime field = "runtime"
	fieldBranch  field = "branch"
	fieldTitle   field = "title"
	fieldNote    field = "note"
	fieldTouched field = "touched"
	fieldTagged  field = "tagged"
	fieldStatus  field = "status"
	fieldOpen    field = "open"
	fieldCalls   field = "calls"
	fieldLines   field = "lines"
)

var fields = map[string]field{
	"tag": fieldTag, "mine": fieldMine, "repo": fieldRepo, "runtime": fieldRuntime,
	"branch": fieldBranch, "title": fieldTitle, "note": fieldNote,
	"touched": fieldTouched, "tagged": fieldTagged, "status": fieldStatus, "open": fieldOpen,
	"calls": fieldCalls, "lines": fieldLines,
}

// term is one parsed field:value, as written.
type term struct {
	Negated bool
	Field   field
	Raw     string // the term exactly as typed, for error messages
	Value   string // folded
	Key     string // tag terms only, folded; empty for a keyless term
	Age     ageBound
	Number  numberBound
}

// Query is a parsed query. It is inert until bound to the tags of a request.
type Query struct {
	terms []term
	words []string
}

// Words are the plain search words, in order, for the transcript search leg.
func (q Query) Words() []string { return append([]string(nil), q.words...) }

// Empty reports a query that filters nothing and searches nothing.
func (q Query) Empty() bool { return len(q.terms) == 0 && len(q.words) == 0 }

// Durable reports whether the query's membership holds still: no live status,
// no open-ness, no ranked text search. Only a durable query may show a count,
// because a count that decays with a time window or is capped by ranking lies.
func (q Query) Durable() bool {
	if len(q.words) > 0 {
		return false
	}
	for _, t := range q.terms {
		if t.Field == fieldStatus || t.Field == fieldOpen {
			return false
		}
	}
	return true
}

// HasNumberTerm reports a query that asks about a session's size (calls: or
// lines:). A caller that evaluates queries over rows it builds without those
// numbers must refuse such a query: every row would read zero.
func (q Query) HasNumberTerm() bool {
	for _, t := range q.terms {
		if t.Field == fieldCalls || t.Field == fieldLines {
			return true
		}
	}
	return false
}

// TagKeys are the folded keys the query's tag terms name, in order: every
// tag: or mine: term that has a key, excluded terms included. A keyless term
// names no key and is not listed.
func (q Query) TagKeys() []string {
	var keys []string
	for _, t := range q.terms {
		if (t.Field == fieldTag || t.Field == fieldMine) && t.Key != "" {
			keys = append(keys, t.Key)
		}
	}
	return keys
}

func fold(text string) string { return strings.ToLower(text) }

// projectTags turns a row's tags into the slice engine.Match evaluates: keys
// and values folded, keyless tags under KeylessTagKey. ownerOnly keeps only
// the owner's. Empty values are dropped: to engine.Match an empty term value
// means "any value", so an empty tag value must never become a leaf or a tag.
func projectTags(tags []Tag, ownerOnly bool) []engine.Tag {
	out := make([]engine.Tag, 0, len(tags))
	for _, tag := range tags {
		if tag.Value == "" || (ownerOnly && !tag.Owner) {
			continue
		}
		out = append(out, engine.Tag{Key: projectedKey(tag.Key), Value: fold(tag.Value)})
	}
	return out
}

func projectedKey(key string) string {
	if key == "" {
		return KeylessTagKey
	}
	return fold(key)
}
