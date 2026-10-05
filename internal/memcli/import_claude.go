package memcli

// import.go — bring the existing curated silos into the store.
//
// AUTO-PROMOTION POLICY (provenance-tiered, ADR 0013 D4 refinement):
//   Tier A  already-curated stores (Claude auto-memory topic files)
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
// Importing from a remote memory service is a daemon job, not built yet.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"crossing-guard/internal/changeenv"
	"crossing-guard/memory"
	"crossing-guard/store"
)

type ImportStats struct {
	Projects, Files, Imported, Skipped, Tombstoned int
}

func importClaudeMemory(dir string, onlyProject string, toPending bool) (ImportStats, error) {
	st, _, err := ImportAutoMemory(dir, onlyProject, toPending)
	return st, err
}

// ImportAutoMemory is the one importer of a vendor's auto-memory topic files
// (today: Claude Code's per-project memory directories) into the STORE,
// through the one write owner (first-class-records plan §3.3 — record + entity
// + classification + FTS + outbox in one transaction), shared by the CLI verbs
// and the daemon (daemon-memory-import plan D1). It returns the ids it wrote.
// Contract: never deletes; rewrites an imported record only when the upstream
// file's Updated is newer (local edits to imported records are not preserved);
// honours tombstones (the store's, which the migration owns).
func ImportAutoMemory(dir string, onlyProject string, toPending bool) (ImportStats, []string, error) {
	var st ImportStats
	var written []string
	projDirs, _ := filepath.Glob(filepath.Join(home(), ".claude", "projects", "*", "memory"))
	ix, err := store.Open(indexDB())
	if err != nil {
		return st, nil, err
	}
	defer ix.Close()
	dets := memDetectors()
	actor := store.MemoryActor{AuthorType: "user", AuthorID: "daemon-import", ActorSource: "daemon"}
	status := "active"
	if toPending {
		status = "pending"
	}
	roots, _ := ix.CheckoutRoots(identityUpgradeRoots()) // best-effort: without them every import stays weak
	for _, pd := range projDirs {
		repo := repoFromEscapedProject(filepath.Base(filepath.Dir(pd)))
		if onlyProject != "" && !memory.SameRepositoryScope(repo, onlyProject) {
			continue
		}
		// Repository identity is minted at import when the vendor's project directory
		// names a checkout this device has seen and that checkout has one origin remote
		// (team item 5 decision 18); otherwise the record stays weak, by label.
		scopeID, identity, note := importRepositoryScope(filepath.Base(filepath.Dir(pd)), repo, roots)
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
			if ix.MemoryTombstoned(rec.ID) {
				st.Tombstoned++
				continue
			}
			// idempotent: existing record only updated when the upstream file
			// is newer than the stored copy (keeps local edits unless upstream
			// moved)
			if old, err := ix.MemoryByID(rec.ID); err == nil {
				// An import never writes over a team record: the vendor's file would
				// become a revision of the team's record, unreviewed (PW-H1).
				if store.MemoryIsTeamRecord(old) || old.ShareState == "shared" || old.UpdatedAt >= parseOrZero(rec.Updated) {
					st.Skipped++
					continue
				}
			}
			sr := store.MemoryRecord{
				ID: rec.ID, Status: status,
				ScopeType: store.MemoryScopeRepository, ScopeID: scopeID,
				RepositoryIdentity: identity, IdentityNote: note,
				Title: rec.Title, Category: rec.Category, Body: rec.Body,
				Tags: rec.Tags, Aliases: rec.Aliases, Source: rec.Source, Origin: rec.Origin,
				AuthorType: "user", AuthorID: "import",
			}
			if sr.Source == "" {
				sr.Source = "import"
			}
			saved, err := ix.UpsertMemory(sr, nil, dets, actor)
			if err != nil {
				fmt.Fprintf(os.Stderr, "skip %s: %v\n", base, err)
				st.Skipped++
				continue
			}
			mirrorRecord(saved)
			st.Imported++
			written = append(written, rec.ID)
		}
	}
	return st, written, nil
}

// parseOrZero parses an RFC3339 record timestamp to unix nanos, 0 when absent.
func parseOrZero(ts string) int64 {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return 0
	}
	return t.UnixNano()
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

// claudeProjectEscape is how the vendor names a project directory after its folder:
// every character outside [A-Za-z0-9] becomes '-'.
var claudeProjectEscape = regexp.MustCompile(`[^A-Za-z0-9]`)

// importRepositoryScope finds the checkout an escaped project directory names among the
// roots this device's sessions recorded, and mints the repository identity from it. With
// no such checkout, or no single origin remote, the label stays the weak scope.
func importRepositoryScope(escaped, label string, roots []string) (scopeID, identity, note string) {
	for _, root := range roots {
		if claudeProjectEscape.ReplaceAllString(root, "-") != escaped {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), identityResolveTimeout())
		id, kind, why := changeenv.MemoryRepositoryScope(ctx, root, "")
		cancel()
		if kind == "remote-sha256" {
			return id, kind, ""
		}
		return label, "weak", why
	}
	return label, "weak", "imported by project name; no checkout of it has been seen on this device"
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
	// No git commit: the mirror directory also holds records pulled from a team, and a
	// whole-directory commit would put their bodies into a history a later deletion
	// cannot reach (team item 5 decision 5, R2-H9). An existing .git there is legacy
	// residue from before memory moved into the store; nothing writes to it any more.
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
