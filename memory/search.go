package memory

// Search (logged) + duplicate detection.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Hit struct {
	Record
	Score int
	Why   []string // which field matched which term — the alias-tuning feedback loop
}

type SearchOpts struct {
	Category   string
	Repository string
	// Tag keeps only records whose frontmatter tags include it, compared without
	// case. With an empty query every such record is a hit, newest first.
	Tag string
}

func Search(dir, query string, opts SearchOpts) []Hit {
	terms := tokenize(query)
	var hits []Hit
	for _, r := range LoadAll(dir) {
		if opts.Category != "" && r.Category != opts.Category {
			continue
		}
		if opts.Repository != "" && !SameRepositoryScope(r.Repository, opts.Repository) {
			continue
		}
		if opts.Tag != "" && !hasTag(r.Tags, opts.Tag) {
			continue
		}
		if len(terms) == 0 && opts.Tag != "" {
			hits = append(hits, Hit{r, 0, []string{"tag:" + strings.ToLower(opts.Tag)}})
			continue
		}
		score := 0
		var why []string
		title := strings.ToLower(r.Title)
		body := strings.ToLower(r.Body)
		id := strings.ToLower(r.ID)
		tags := strings.ToLower(strings.Join(r.Tags, " "))
		aliases := strings.ToLower(strings.Join(r.Aliases, " "))
		for _, t := range terms {
			switch {
			case strings.Contains(title, t) || strings.Contains(id, t):
				score += 3
				why = append(why, "title:"+t)
			case strings.Contains(aliases, t): // aliases weigh like titles
				score += 3
				why = append(why, "alias:"+t)
			case strings.Contains(tags, t):
				score += 2
				why = append(why, "tag:"+t)
			case strings.Contains(body, t):
				score++
				why = append(why, "body:"+t) // body-only match = alias candidate
			}
		}
		if score > 0 {
			hits = append(hits, Hit{r, score, why})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Updated > hits[j].Updated
	})
	return hits
}

// Similar flags likely duplicates at write time (search-before-write made
// mechanical): token overlap between the candidate's title+aliases and each
// existing record's. Advisory only.
func Similar(dir string, cand Record) []Record {
	candTok := map[string]bool{}
	for _, t := range tokenize(cand.Title + " " + strings.Join(cand.Aliases, " ")) {
		if len(t) >= 3 {
			candTok[t] = true
		}
	}
	var out []Record
	for _, r := range LoadAll(dir) {
		if r.ID == cand.ID {
			continue
		}
		overlap := 0
		for _, t := range tokenize(r.Title + " " + strings.Join(r.Aliases, " ")) {
			if candTok[t] {
				overlap++
			}
		}
		if overlap >= 2 {
			out = append(out, r)
		}
	}
	return out
}

func hasTag(tags []string, want string) bool {
	for _, tag := range tags {
		if strings.EqualFold(strings.TrimSpace(tag), strings.TrimSpace(want)) {
			return true
		}
	}
	return false
}

func tokenize(q string) []string {
	fields := strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
		return !('a' <= r && r <= 'z' || '0' <= r && r <= '9')
	})
	var out []string
	for _, f := range fields {
		if len(f) >= 2 {
			out = append(out, f)
		}
	}
	return out
}

// LogRecall appends one instrumentation row per read-path call.
// This is red-team F2 made concrete: the vector-vs-keyword decision is gated
// on THESE rows, reviewed as examples, not on an unmeasurable rate.
// Schema is normative (engine-response-to-gui §3): additive fields only.
func LogRecall(dir, op, query string, results []string, nonce ...string) {
	_ = os.MkdirAll(dir, 0o755)
	row := map[string]any{
		"ts": time.Now().UTC().Format(time.RFC3339), "op": op,
		"query": query, "count": len(results), "results": results,
	}
	if len(nonce) > 0 && nonce[0] != "" {
		row["nonce"] = nonce[0]
	}
	b, _ := json.Marshal(row)
	f, err := os.OpenFile(filepath.Join(dir, "recall-log.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}
