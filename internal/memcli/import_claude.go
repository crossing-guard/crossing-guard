package memcli

// import.go — bring the existing curated silos into the store.
//
// AUTO-PROMOTION POLICY (provenance-tiered, ADR 0013 D4 refinement):
//   Tier A  already-curated stores (Claude auto-memory topic files, OMS bank)
//           → import DIRECTLY into the store (source: import, origin recorded).
//           The human curation happened upstream; re-reviewing hundreds of
//           records one-by-one would kill the import. --pending overrides.
//   Tier B  raw-session synthesis (future harvest propose) → ALWAYS pending.
//   Tier C  quick notes → ALWAYS pending.
// Tombstones are respected in every tier: deleted slugs stay deleted.
//
// Claude auto-memory: ~/.claude/projects/<escaped-cwd>/memory/*.md — one
// topic file per dossier (MEMORY.md is the vendor's injected index, skipped;
// the files may carry their own YAML frontmatter, parsed leniently).
// OMS/Typesense import needs a Passport token against the REST proxy — that
// is a daemon job (`import oms`), not built yet; noted in usage.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"crossing-guard/memory"
)

type ImportStats struct {
	Projects, Files, Imported, Skipped, Tombstoned int
}

func importClaudeMemory(dir string, onlyProject string, toPending bool) (ImportStats, error) {
	st, _, err := ImportAutoMemory(dir, onlyProject, toPending)
	return st, err
}

// ImportAutoMemory is the one importer of a vendor's auto-memory topic files (today: Claude Code's per-project memory directories) into a
// store, shared by the CLI verbs and the daemon (daemon-memory-import plan D1). It
// returns the ids it wrote so a caller can index exactly those records. Contract:
// never deletes; rewrites an imported record only when the upstream file's Updated
// is newer (local edits to imported records are not preserved); honours tombstones.
func ImportAutoMemory(dir string, onlyProject string, toPending bool) (ImportStats, []string, error) {
	var st ImportStats
	var written []string
	projDirs, _ := filepath.Glob(filepath.Join(home(), ".claude", "projects", "*", "memory"))
	targetDir := dir
	if toPending {
		targetDir = filepath.Join(dir, "pending")
	}
	for _, pd := range projDirs {
		repo := repoFromEscapedProject(filepath.Base(filepath.Dir(pd)))
		if onlyProject != "" && !memory.SameRepositoryScope(repo, onlyProject) {
			continue
		}
		st.Projects++
		files, _ := filepath.Glob(filepath.Join(pd, "*.md"))
		for _, f := range files {
			base := filepath.Base(f)
			if base == "MEMORY.md" { // the vendor's index, not a dossier
				continue
			}
			st.Files++
			rec, ok := recordFromClaudeMemoryFile(f, repo)
			if !ok {
				st.Skipped++
				continue
			}
			if isTombstoned(dir, rec.ID) {
				st.Tombstoned++
				continue
			}
			// idempotent: existing imported record only updated when source
			// file is newer than our copy (keeps local edits unless upstream moved)
			if old, err := readRecord(filepath.Join(dir, rec.ID+".md")); err == nil {
				rec.Created = old.Created
				if old.Updated >= rec.Updated {
					st.Skipped++
					continue
				}
			}
			if err := writeRecord(targetDir, rec); err != nil {
				fmt.Fprintf(os.Stderr, "skip %s: %v\n", base, err)
				st.Skipped++
				continue
			}
			st.Imported++
			if !toPending {
				written = append(written, rec.ID)
			}
		}
	}
	if st.Imported > 0 {
		mode := "store (tier-A auto-promote)"
		if toPending {
			mode = "pending/"
		}
		gitCommit(dir, fmt.Sprintf("import claude-memory: %d records into %s", st.Imported, mode))
	}
	return st, written, nil
}

// repoFromEscapedProject maps "-Users-alice-Documents-Sites-my-repo"
// → "my-repo". Escaping is lossy ("/"→"-"), so take the segment after
// the LAST known workspace marker; fall back to the whole trimmed name.
func repoFromEscapedProject(escaped string) string {
	for _, marker := range []string{"-Documents-Sites-", "-Sites-"} {
		if i := strings.LastIndex(escaped, marker); i >= 0 {
			if r := escaped[i+len(marker):]; r != "" {
				return strings.ToLower(r)
			}
		}
	}
	return strings.ToLower(strings.Trim(escaped, "-"))
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	s = nonSlug.ReplaceAllString(strings.ToLower(s), "-")
	s = strings.Trim(s, "-")
	if len(s) > 100 {
		s = strings.Trim(s[:100], "-")
	}
	return s
}

// recordFromClaudeMemoryFile converts one auto-memory topic file. Lenient on
// the vendor-side frontmatter (name:/description:/type:) — used when present,
// body kept verbatim below an origin header.
func recordFromClaudeMemoryFile(path, repo string) (Record, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Record{}, false
	}
	fi, _ := os.Stat(path)
	content := string(raw)
	title, category := "", ""

	if strings.HasPrefix(content, "---\n") {
		if end := strings.Index(content[4:], "\n---\n"); end >= 0 {
			front := content[4 : 4+end]
			content = strings.TrimSpace(content[4+end+5:])
			for _, line := range strings.Split(front, "\n") {
				k, v, _ := strings.Cut(line, ":")
				v = strings.Trim(strings.TrimSpace(v), `"`)
				switch strings.TrimSpace(k) {
				case "description":
					title = v
				case "type":
					if categories[v] {
						category = v
					}
				}
			}
		}
	}
	if title == "" { // first heading, else first non-empty line
		for _, line := range strings.Split(content, "\n") {
			line = strings.TrimSpace(strings.TrimLeft(line, "# "))
			if line != "" {
				title = truncate(line, 160)
				break
			}
		}
	}
	if strings.TrimSpace(content) == "" || title == "" {
		return Record{}, false
	}
	base := strings.TrimSuffix(filepath.Base(path), ".md")
	if category == "" {
		category = guessCategory(base + " " + title)
	}
	mod := time.Now().UTC()
	if fi != nil {
		mod = fi.ModTime().UTC()
	}
	return Record{
		ID:         importID(repo, base),
		Title:      title,
		Category:   category,
		Repository: repo,
		Tags:       []string{"imported", "claude-auto-memory"},
		Aliases:    []string{}, // not yet curated — alias pass is follow-up work
		Source:     "import",
		Origin:     "claude-auto-memory:" + path, // structured provenance (format v1.1)
		Created:    mod.Format(time.RFC3339),
		Updated:    mod.Format(time.RFC3339),
		Body:       content,
	}, true
}

// importID prefixes the repo for uniqueness WITHOUT stuttering when the
// source filename already carries it (v1.1 punch-list #4:
// "private-ai-crossing-guard-private-ai-crossing-guard-direction").
func importID(repo, base string) string {
	rslug, bslug := slugify(repo), slugify(base)
	if bslug == rslug || strings.HasPrefix(bslug, rslug+"-") {
		return bslug
	}
	return slugify(rslug + "-" + bslug)
}

// ---------- one-shot migration to format v1.1 ----------

var originComment = regexp.MustCompile(`(?s)^<!-- imported from (\S+) \([0-9-]+\)[^>]*-->\s*`)

type MigrateStats struct{ Scanned, Rewritten, Renamed int }

// migrateStore rewrites every record (store + pending + rejected) into the
// current format: adds format:1, lifts the legacy origin HTML comment into
// frontmatter, splits legacy verified (readRecord normalizes on read), and
// collapses stuttered import ids. Idempotent; one git commit.
func migrateStore(dir string) (MigrateStats, error) {
	var st MigrateStats
	for _, sub := range []string{"", "pending", "rejected"} {
		d := filepath.Join(dir, sub)
		files, _ := filepath.Glob(filepath.Join(d, "*.md"))
		for _, f := range files {
			st.Scanned++
			r, err := readRecord(f)
			if err != nil {
				fmt.Fprintf(os.Stderr, "migrate skip %s: %v\n", f, err)
				continue
			}
			if r.Origin == "" {
				if m := originComment.FindStringSubmatch(r.Body); m != nil {
					r.Origin = "claude-auto-memory:" + m[1]
					r.Body = originComment.ReplaceAllString(r.Body, "")
				}
			}
			newID := r.ID
			if r.Repository != "" {
				rslug := slugify(r.Repository)
				if strings.HasPrefix(newID, rslug+"-"+rslug) {
					newID = strings.TrimPrefix(newID, rslug+"-")
				}
			}
			renamed := newID != r.ID
			r.ID = newID
			if err := writeRecord(d, r); err != nil {
				fmt.Fprintf(os.Stderr, "migrate write %s: %v\n", r.ID, err)
				continue
			}
			if renamed {
				_ = os.Remove(f)
				st.Renamed++
			}
			st.Rewritten++
		}
	}
	gitCommit(dir, fmt.Sprintf("migrate(format v1.1): %d rewritten, %d ids de-stuttered", st.Rewritten, st.Renamed))
	return st, nil
}

func guessCategory(hint string) string {
	h := strings.ToLower(hint)
	switch {
	case strings.Contains(h, "incident") || strings.Contains(h, "outage"):
		return "incident"
	case strings.Contains(h, "gotcha") || strings.Contains(h, "warning") || strings.Contains(h, "pitfall"):
		return "gotcha"
	case strings.Contains(h, "bug") || strings.Contains(h, "fix"):
		return "bug-fix"
	case strings.Contains(h, "convention") || strings.Contains(h, "standard") || strings.Contains(h, "style"):
		return "convention"
	case strings.Contains(h, "architecture") || strings.Contains(h, "schema") || strings.Contains(h, "model"):
		return "architecture"
	case strings.Contains(h, "prefer"):
		return "preference"
	default:
		return "how-to"
	}
}
