// Package memory is the vendor-neutral memory store + injection index
// (ADR 0013; extracted from experiments/cpmem — code-organization-v1's
// memory-package promotion, on the way to internal/memory at M4).
//
// Record model (per red-team of the memory reference, 2026-07-14):
//   - one TOPIC-DOSSIER per file, updated in place (not atomic facts)
//   - frontmatter carries identity + provenance + ALIASES (write-time alias
//     expansion = the cheap lexical substitute for vector search)
//   - every read path is LOGGED to recall-log.jsonl (instrumentation is a
//     store requirement, not an optional probe — misses are invisible)
//
// Store layout (files-first, git-ignorable, greppable — the design both
// vendors' native memory systems converged on):
//
//	<dir>/<id>.md            one dossier per record
//	<dir>/recall-log.jsonl   every search/get/index call + outcome
package memory

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type Record struct {
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	Category   string   `json:"category"`
	Repository string   `json:"repository"`
	Tags       []string `json:"tags"`
	Aliases    []string `json:"aliases"`
	Source     string   `json:"source"`           // human | agent | import | harvest
	Origin     string   `json:"origin,omitempty"` // structured import provenance, e.g. claude-auto-memory:<path>
	Superseded string   `json:"superseded_by,omitempty"`
	VerifiedAt string   `json:"verified_at,omitempty"` // dated attestation (v1.1: split fields,
	VerifiedBy string   `json:"verified_by,omitempty"` //  one fact per field — GUI R3)
	Created    string   `json:"created"`
	Updated    string   `json:"updated"`
	Format     int      `json:"format"` // record format version; current = 1
	Body       string   `json:"-"`
	Path       string   `json:"-"`
	Pending    bool     `json:"pending,omitempty"` // lives in pending/, never projected; GUI inbox flag (R7)
	// Scope survives into recall (team item 5 decision 12; invariant 6: scope is a
	// field). Store records carry all three; a legacy mirror file has only Repository
	// and reads as a weak repository record. Shadowed marks a pulled team record whose
	// name a local user record already holds (decision 15): never recalled.
	ScopeType          string `json:"scope_type,omitempty"` // user | repository | organization
	ScopeID            string `json:"scope_id,omitempty"`
	RepositoryIdentity string `json:"repository_identity,omitempty"` // weak | remote-sha256
	Shadowed           bool   `json:"-"`
}

// Categories is the record-category enum (ADR 0013 D1 frontmatter).
var Categories = map[string]bool{
	"convention": true, "gotcha": true, "bug-fix": true, "architecture": true,
	"how-to": true, "incident": true, "preference": true, "business-rule": true,
	"tech-stack": true, "note": true,
}

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,99}$`)

func Validate(r Record) error {
	if !slugRe.MatchString(r.ID) {
		return fmt.Errorf("id must be kebab-case slug: %q", r.ID)
	}
	if strings.TrimSpace(r.Title) == "" {
		return fmt.Errorf("title required")
	}
	if !Categories[r.Category] {
		return fmt.Errorf("category %q not in enum (convention|gotcha|bug-fix|architecture|how-to|incident|preference|business-rule|tech-stack|note)", r.Category)
	}
	return nil
}

// DefaultDir is the store location: $CG_MEMORY_DIR (legacy $CPMEM_DIR) or
// ~/.crossing-guard/memory.
func DefaultDir() string {
	for _, n := range []string{"CG_MEMORY_DIR", "CPMEM_DIR"} {
		if d := os.Getenv(n); d != "" {
			return d
		}
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".crossing-guard", "memory")
}

// ---------- read/write ----------

func Load(dir string) []Record {
	files, _ := filepath.Glob(filepath.Join(dir, "*.md"))
	var out []Record
	for _, f := range files {
		if r, err := Read(f); err == nil {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated > out[j].Updated })
	return out
}

// LoadAll = store records + pending/ proposals (marked, never projected).
func LoadAll(dir string) []Record {
	recs := Load(dir)
	files, _ := filepath.Glob(filepath.Join(dir, "pending", "*.md"))
	for _, f := range files {
		if r, err := Read(f); err == nil {
			r.Pending = true
			recs = append(recs, r)
		}
	}
	return recs
}

func Read(path string) (Record, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Record{}, err
	}
	r := Record{Path: path}
	s := string(raw)
	if !strings.HasPrefix(s, "---\n") {
		return Record{}, fmt.Errorf("%s: missing frontmatter", path)
	}
	rest := s[4:]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		return Record{}, fmt.Errorf("%s: unterminated frontmatter", path)
	}
	r.Body = strings.TrimSpace(rest[end+5:])
	for _, line := range strings.Split(rest[:end], "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "id":
			r.ID = v
		case "title":
			r.Title = v
		case "category":
			r.Category = v
		case "repository":
			r.Repository = v
		case "source":
			r.Source = v
		case "origin":
			r.Origin = v
		case "superseded_by":
			r.Superseded = v
		case "format":
			fmt.Sscanf(v, "%d", &r.Format)
		case "verified_at":
			r.VerifiedAt = v
		case "verified_by":
			r.VerifiedBy = v
		case "verified": // legacy compound field — normalize on read (v1.0 → v1.1)
			if at, by, ok := strings.Cut(v, " by "); ok {
				r.VerifiedAt, r.VerifiedBy = strings.TrimSpace(at), strings.TrimSpace(by)
			} else {
				r.VerifiedAt = v
			}
		case "created":
			r.Created = v
		case "updated":
			r.Updated = v
		case "tags":
			r.Tags = SplitCSV(v)
		case "aliases":
			r.Aliases = SplitCSV(v)
		}
	}
	if r.ID == "" {
		r.ID = strings.TrimSuffix(filepath.Base(path), ".md")
	}
	if r.Tags == nil {
		r.Tags = []string{} // JSON contract: arrays, never null
	}
	if r.Aliases == nil {
		r.Aliases = []string{}
	}
	return r, nil
}

// scalar flattens a frontmatter value to one line. Without this, a crafted
// Title like "x\nsuperseded_by: y" injects frontmatter keys on re-read —
// which can silently drop the record from the injection index (red-team
// finding; agent-authored pending records make this a reachable path).
func scalar(v string) string {
	v = strings.ReplaceAll(v, "\r", " ")
	v = strings.ReplaceAll(v, "\n", " ")
	return strings.TrimSpace(v)
}

func scalarList(vs []string) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		if v = scalar(strings.ReplaceAll(v, ",", " ")); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func Write(dir string, r Record) error {
	if !slugRe.MatchString(r.ID) {
		return fmt.Errorf("id must be kebab-case slug: %q", r.ID)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "id: %s\n", r.ID)
	fmt.Fprintf(&b, "title: %s\n", scalar(r.Title))
	fmt.Fprintf(&b, "category: %s\n", scalar(r.Category))
	if r.Repository != "" {
		fmt.Fprintf(&b, "repository: %s\n", scalar(r.Repository))
	}
	if len(r.Tags) > 0 {
		fmt.Fprintf(&b, "tags: %s\n", strings.Join(scalarList(r.Tags), ", "))
	}
	if len(r.Aliases) > 0 {
		fmt.Fprintf(&b, "aliases: %s\n", strings.Join(scalarList(r.Aliases), ", "))
	}
	fmt.Fprintf(&b, "source: %s\n", scalar(r.Source))
	if r.Origin != "" {
		fmt.Fprintf(&b, "origin: %s\n", scalar(r.Origin))
	}
	if r.Superseded != "" {
		fmt.Fprintf(&b, "superseded_by: %s\n", scalar(r.Superseded))
	}
	if r.VerifiedAt != "" {
		fmt.Fprintf(&b, "verified_at: %s\n", scalar(r.VerifiedAt))
	}
	if r.VerifiedBy != "" {
		fmt.Fprintf(&b, "verified_by: %s\n", scalar(r.VerifiedBy))
	}
	fmt.Fprintf(&b, "created: %s\nupdated: %s\n", scalar(r.Created), scalar(r.Updated))
	b.WriteString("format: 1\n")
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimSpace(r.Body))
	b.WriteString("\n")
	return os.WriteFile(filepath.Join(dir, r.ID+".md"), []byte(b.String()), 0o644)
}

// ---------- shared small helpers ----------

func run(dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	return cmd.Run()
}

func ShortDate(ts string) string {
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ts
}

func SplitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
