package refindex

// Reference index: turns a token in a transcript — a repo path, a `file:line`, a
// D-id, an ADR id, a URL — into a target the console can open, together with the
// STATE of that resolution (console-and-info-panel §5).
//
// The state is the whole point. A reference in an old session often points at
// something that has since moved, so the console must be able to say "referenced,
// no longer in the tree" instead of rendering a link that 404s (INV — honest
// misses). Recognition only flags a token; resolution is what earns a link.
//
// Read-only by contract: nothing here writes to the store or the tree. This
// package owns parsing and resolution over either a bounded walk or an explicit
// manifest. The daemon owns its ephemeral cache; durable generation persistence
// belongs to store and the understanding orchestrator.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxWalk = 20000

var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "__pycache__": true,
	"dist": true, "build": true, ".next": true, ".venv": true,
}

// RefState is how a token resolved. Every miss carries a distinguishable state so
// the console can render the REASON rather than a dead link (§5 honesty rule).
type RefState string

const (
	// RefResolved — the target exists in the working tree now. Clickable.
	RefResolved RefState = "resolved"
	// RefMissing — no such path, and git has never heard of it either. Dim.
	RefMissing RefState = "missing"
	// RefStale — git knows this path but the working tree does not: moved or
	// deleted since the session that mentioned it. Struck through, not a link.
	RefStale RefState = "stale"
	// RefUnresolved — an id shaped like ours (D5, ADR 25) with no definition. Dim.
	RefUnresolved RefState = "unresolved"
	// RefExternal — an http(s) URL. Opens in a tab; nothing to resolve locally.
	RefExternal RefState = "external"
)

// RefKind is what sort of thing a token turned out to name.
type RefKind string

const (
	RefKindPath RefKind = "path"
	RefKindDoc  RefKind = "doc"
	RefKindID   RefKind = "id"
	RefKindURL  RefKind = "url"
)

// RefDef is one definition site of an id — the line that DECLARES D18, not a
// line that merely mentions it (see refIDDefPattern).
type RefDef struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// RefTarget is the resolved reference the console opens and the panel matches on.
type RefTarget struct {
	Token string   `json:"token"`
	Kind  RefKind  `json:"kind"`
	State RefState `json:"state"`
	Path  string   `json:"path,omitempty"`
	Line  int      `json:"line,omitempty"`
	URL   string   `json:"url,omitempty"`
	// Reason states WHY a non-resolved token did not resolve. Never empty for a
	// miss: an unexplained miss is the dead-link failure this index exists to
	// prevent.
	Reason string `json:"reason,omitempty"`
	// Defs carries every definition site found for an id. More than one is
	// surfaced rather than guessed (impl-plan R10).
	Defs []RefDef `json:"defs,omitempty"`
	// Subjects are file anchors an id's own text happens to carry. Many items
	// carry none, so this is often empty — never promise subjects that aren't
	// there (§5).
	Subjects []string `json:"subjects,omitempty"`
}

// RefManifest is the whole index, served once per console load so the client can
// resolve synchronously while rendering a transcript (impl-plan Step 1.2).
type RefManifest struct {
	Root    string              `json:"root"`
	Paths   []string            `json:"paths"`
	Docs    []string            `json:"docs"`
	IDs     map[string][]RefDef `json:"ids"`
	BuiltAt time.Time           `json:"built_at"`
	// Truncated reports that the walk hit its bound and the path set is partial.
	// A partial index must say so: silently short manifests would turn real files
	// into "missing" (INV — count, don't hide).
	Truncated bool `json:"truncated"`
}

// refIndex is the in-memory form: sets for O(1) membership during resolution.
type Index struct {
	root  string
	paths map[string]bool
	docs  []string
	ids   map[string][]RefDef
	// mentions are the INCOMING edges — "which doc line points at this token" —
	// keyed by the token as written. Backlinks (§7 L2) read this; resolution does
	// not, because a mention is not a definition (R10).
	mentions  map[string][]RefDef
	ambiguous int
	builtAt   time.Time
	truncated bool
}

func (idx *Index) Root() string { return idx.root }

// Backlinks returns the deterministic bounded doc lines that reference target.
func (idx *Index) Backlinks(target string, limit int) (refs []RefDef, total int) {
	if limit <= 0 {
		limit = 50
	}
	seen := map[string]bool{}
	wanted := normalizeRefID(target)
	for key, defs := range idx.mentions {
		if !mentionMatchesTarget(normalizeRefID(key), wanted) {
			continue
		}
		for _, def := range defs {
			if def.Path == target {
				continue
			}
			identity := def.Path + ":" + strconv.Itoa(def.Line)
			if seen[identity] {
				continue
			}
			seen[identity] = true
			refs = append(refs, def)
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Path != refs[j].Path {
			return refs[i].Path < refs[j].Path
		}
		return refs[i].Line < refs[j].Line
	})
	total = len(refs)
	if len(refs) > limit {
		refs = refs[:limit]
	}
	return refs, total
}

func mentionMatchesTarget(key, target string) bool {
	return key == target || strings.HasSuffix(target, "/"+key) || strings.HasSuffix(key, "/"+target)
}

type Coverage struct {
	State     string `json:"state"`
	Attempted int    `json:"attempted"`
	Produced  int    `json:"produced"`
	Errors    int    `json:"errors"`
	Ambiguous int    `json:"ambiguous"`
	Reason    string `json:"reason,omitempty"`
}

type ManifestFacts struct {
	Root     string              `json:"root"`
	Paths    []string            `json:"paths"`
	Docs     []string            `json:"docs"`
	IDs      map[string][]RefDef `json:"ids"`
	Mentions map[string][]RefDef `json:"mentions"`
	Coverage Coverage            `json:"coverage"`
}

// BuildManifest parses references over exactly the caller-supplied source
// population. No walk, extension allowlist, or implicit project configuration
// can widen this durable input.
func BuildManifest(root string, paths []string) (ManifestFacts, error) {
	root, err := projectRoot(root)
	if err != nil {
		return ManifestFacts{}, err
	}
	if len(paths) > maxWalk {
		return ManifestFacts{}, fmt.Errorf("%w: manifest exceeds %d paths", ErrRefRequest, maxWalk)
	}
	idx := &Index{root: root, paths: map[string]bool{}, ids: map[string][]RefDef{}, mentions: map[string][]RefDef{}, builtAt: time.Now()}
	for _, path := range paths {
		path = filepath.ToSlash(strings.TrimPrefix(path, "./"))
		clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
		if path == "" || filepath.IsAbs(path) || clean != path || path == ".." || strings.HasPrefix(path, "../") {
			return ManifestFacts{}, fmt.Errorf("%w: unsafe manifest path %q", ErrRefRequest, path)
		}
		idx.paths[path] = true
	}
	allPaths := make([]string, 0, len(idx.paths))
	for path := range idx.paths {
		allPaths = append(allPaths, path)
		if strings.HasSuffix(strings.ToLower(path), ".md") {
			idx.docs = append(idx.docs, path)
		}
	}
	sort.Strings(allPaths)
	sort.Strings(idx.docs)
	coverage := Coverage{Attempted: len(idx.docs)}
	var reasons []string
	for _, doc := range idx.docs {
		if err := idx.scanDoc(root, doc); err != nil {
			coverage.Errors++
			reasons = append(reasons, doc+": "+err.Error())
			continue
		}
		coverage.Produced++
	}
	idx.collectADRDefs()
	coverage.Ambiguous = idx.ambiguous
	if coverage.Errors == 0 && coverage.Ambiguous == 0 {
		coverage.State = "complete"
	} else {
		coverage.State = "partial"
		if coverage.Ambiguous > 0 {
			reasons = append(reasons, strconv.Itoa(coverage.Ambiguous)+" path references matched multiple manifest files")
		}
		coverage.Reason = strings.Join(reasons, "; ")
	}
	return ManifestFacts{Root: root, Paths: allPaths, Docs: append([]string(nil), idx.docs...), IDs: idx.ids, Mentions: idx.mentions, Coverage: coverage}, nil
}

// Manifest returns a detached sorted view suitable for the daemon response.
func (idx *Index) Manifest() *RefManifest {
	paths := make([]string, 0, len(idx.paths))
	for p := range idx.paths {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return &RefManifest{
		Root: idx.root, Paths: paths, Docs: idx.docs, IDs: idx.ids,
		BuiltAt: idx.builtAt, Truncated: idx.truncated,
	}
}

// refDocMaxBytes bounds a served doc. Our design docs run tens of KB; a cap keeps
// a pathological file from becoming the console's problem.
const refDocMaxBytes = 1 << 20

// RefDoc is one markdown document, served for the in-console viewer (§8).
type RefDoc struct {
	Path      string `json:"path"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
}

// ReadRefDoc returns a markdown file from inside the project root.
//
// Two guards, both load-bearing: the resolved path must stay INSIDE the root
// (otherwise `../../.ssh/id_rsa` is a file read served over HTTP), and it must be
// a .md file (the viewer renders prose; code goes to the editor). The console is
// loopback-only and token-guarded, but a path-traversal read is not something to
// leave to the network boundary.
func ReadRefDoc(root, rel string) (*RefDoc, error) {
	abs, err := projectRoot(root)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(strings.ToLower(rel), ".md") {
		return nil, fmt.Errorf("%w: only .md documents are served (got %q)", ErrRefRequest, rel)
	}
	full := filepath.Join(abs, filepath.FromSlash(rel))
	clean, err := filepath.Abs(full)
	if err != nil {
		return nil, fmt.Errorf("%w: bad path %q: %v", ErrRefRequest, rel, err)
	}
	if clean != abs && !strings.HasPrefix(clean, abs+string(filepath.Separator)) {
		return nil, fmt.Errorf("%w: path escapes the project root", ErrRefRequest)
	}
	body, err := os.ReadFile(clean)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot read %q: %v", ErrRefRequest, rel, err)
	}
	doc := &RefDoc{Path: filepath.ToSlash(rel)}
	if len(body) > refDocMaxBytes {
		body, doc.Truncated = body[:refDocMaxBytes], true
	}
	doc.Text = string(body)
	return doc, nil
}

// Resolve is the single server-side resolution authority for a built index.
func (idx *Index) Resolve(token string) *RefTarget { return idx.resolve(token) }

// projectRoot validates the root a caller asked for. An index is per-PROJECT: the
// console always knows the session's cwd, so an empty or nonsensical root is a
// caller bug, not something to paper over. Walking from "/" once produced a
// 15,000-entry index of the whole machine — expensive and meaningless — so the
// filesystem root is refused explicitly rather than silently accepted.
func projectRoot(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", fmt.Errorf("%w: cwd is required (the index is per-project)", ErrRefRequest)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("%w: bad cwd %q: %v", ErrRefRequest, root, err)
	}
	if abs == string(filepath.Separator) {
		return "", fmt.Errorf("%w: refusing to index the filesystem root", ErrRefRequest)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("%w: cwd %q is not readable: %v", ErrRefRequest, abs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: cwd %q is not a directory", ErrRefRequest, abs)
	}
	return abs, nil
}

// ProjectRoot exposes the shared root validation to hosts that also serve
// related read-only project features.
func ProjectRoot(root string) (string, error) { return projectRoot(root) }

// ErrRefRequest marks a caller mistake (missing or unusable cwd) so the handlers
// answer 400 rather than 500 — a client that asked wrongly should be told so.
var ErrRefRequest = errors.New("ref index")

// buildRefIndex walks the tree once, collecting the path set, the .md corpus, and
// the id definitions. Bounds and skip-list match SearchFiles so the two agree on
// what "a repo file" means.
func Build(root string) (*Index, error) {
	root, err := projectRoot(root)
	if err != nil {
		return nil, err
	}
	idx := &Index{
		root:     root,
		paths:    map[string]bool{},
		ids:      map[string][]RefDef{},
		mentions: map[string][]RefDef{},
		builtAt:  time.Now(),
	}
	visited := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable subtree is not a reason to have no index
		}
		visited++
		if visited > maxWalk {
			idx.truncated = true
			return filepath.SkipAll
		}
		if d.IsDir() {
			if skipDirs[d.Name()] || (strings.HasPrefix(d.Name(), ".") && path != root) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		idx.paths[rel] = true
		if strings.HasSuffix(strings.ToLower(rel), ".md") {
			idx.docs = append(idx.docs, rel)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ref index: walking %s: %w", root, err)
	}
	sort.Strings(idx.docs)
	for _, doc := range idx.docs {
		_ = idx.scanDoc(root, doc)
	}
	idx.collectADRDefs()
	return idx, nil
}

// refIDDefPattern matches a D-id DEFINITION — the bold declaration a tracker doc
// uses to introduce an item (`| **D18** | …`). A bare `D18` in prose is a
// MENTION, and resolving an id to a mention is the bug this pattern exists to
// avoid: ci-requirements.md says "D18/item 8" while D18 is defined in
// v1-remaining-work.md (impl-plan R10). Definition by pattern, never by a
// hardcoded list of tracker filenames.
var refIDDefPattern = regexp.MustCompile(`\*\*(D\d+)\*\*`)

// refSubjectPattern matches a file:line anchor inside an item's own text, e.g.
// `service.go:40`. These are the doc→code links D-items already carry by hand;
// many items carry none.
var refSubjectPattern = regexp.MustCompile(`\b([A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)*\.[A-Za-z0-9_.-]+)(?::(\d+))?\b`)

// refIDMentionPattern matches an id ANYWHERE — definition or prose. Used only for
// backlinks ("what points here"), never for resolution.
var refIDMentionPattern = regexp.MustCompile(`\b(D\d+|ADR\s*\d+)\b`)

// scanDoc reads one markdown file for both id definitions and outgoing mentions.
// Errors are skipped, not fatal: one unreadable doc must not cost the whole index.
func (idx *Index) scanDoc(root, rel string) error {
	f, err := os.Open(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return err
	}
	body, err := io.ReadAll(io.LimitReader(f, refDocMaxBytes+1))
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if len(body) > refDocMaxBytes {
		return fmt.Errorf("document %q exceeds %d bytes", rel, refDocMaxBytes)
	}
	for n, line := range strings.Split(string(body), "\n") {
		def := RefDef{Path: rel, Line: n + 1, Text: strings.TrimSpace(line)}
		for _, m := range refIDDefPattern.FindAllStringSubmatch(line, -1) {
			idx.ids[m[1]] = append(idx.ids[m[1]], def)
		}
		idx.collectMentions(def, line)
	}
	return nil
}

// collectMentions records the outgoing edges on one line: ids it names and file
// paths it names. A doc that merely mentions D18 is a BACKLINK to D18, which is
// exactly the "what else points here" question (§7 L2) — and exactly why it must
// not be mistaken for a definition.
func (idx *Index) collectMentions(def RefDef, line string) {
	for _, m := range refIDMentionPattern.FindAllStringSubmatch(line, -1) {
		key := normalizeRefID(strings.ReplaceAll(m[1], "  ", " "))
		idx.mentions[key] = append(idx.mentions[key], def)
	}
	for _, m := range refSubjectPattern.FindAllStringSubmatch(line, -1) {
		if path, ok := idx.uniquePath(m[1]); ok {
			idx.mentions[path] = append(idx.mentions[path], def)
		} else if len(idx.suffixMatches(m[1])) > 1 {
			idx.ambiguous++
		}
	}
}

func (idx *Index) uniquePath(candidate string) (string, bool) {
	candidate = filepath.ToSlash(strings.TrimPrefix(candidate, "./"))
	if idx.paths[candidate] {
		return candidate, true
	}
	hits := idx.suffixMatches(candidate)
	if len(hits) == 1 {
		return hits[0], true
	}
	return "", false
}

// adrFilePattern matches the ADR filename convention docs/adr/0025-title.md,
// where the NUMBER IN THE FILENAME is the definition — there is no prose
// declaration to grep for.
var adrFilePattern = regexp.MustCompile(`(?:^|/)adr/(\d+)-[^/]*\.md$`)

// collectADRDefs derives ADR definitions from the doc set's filenames.
func (idx *Index) collectADRDefs() {
	for _, doc := range idx.docs {
		m := adrFilePattern.FindStringSubmatch(doc)
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		id := normalizeRefID("ADR " + strconv.Itoa(n))
		idx.ids[id] = append(idx.ids[id], RefDef{Path: doc, Line: 1, Text: doc})
	}
}

// normalizeRefID folds the spellings a human writes ("ADR 0025", "ADR25",
// "adr 25") onto one key, so lookup does not depend on how the id was typed.
func normalizeRefID(raw string) string {
	s := strings.TrimSpace(raw)
	if m := regexp.MustCompile(`(?i)^adr\s*0*(\d+)$`).FindStringSubmatch(s); m != nil {
		return "ADR " + m[1]
	}
	if m := regexp.MustCompile(`(?i)^(d)(\d+)$`).FindStringSubmatch(s); m != nil {
		return "D" + m[2]
	}
	return s
}

// fileLinePattern splits a `path:line` token into its parts.
var fileLinePattern = regexp.MustCompile(`^(.+?):(\d+)$`)

// resolve is the single decision point: token in, target-with-state out.
func (idx *Index) resolve(token string) *RefTarget {
	token = strings.TrimSpace(token)
	t := &RefTarget{Token: token}
	if token == "" {
		t.State = RefUnresolved
		t.Reason = "empty reference"
		return t
	}
	if strings.HasPrefix(token, "http://") || strings.HasPrefix(token, "https://") {
		t.Kind, t.State, t.URL = RefKindURL, RefExternal, token
		return t
	}
	if id := normalizeRefID(token); idx.isIDShaped(id) {
		return idx.resolveID(t, id)
	}
	return idx.resolvePath(t, token)
}

// isIDShaped reports whether a token looks like one of our ids at all. Shape is
// not existence — an id-shaped token with no definition resolves UNRESOLVED with
// a reason, which is different from "not an id".
func (idx *Index) isIDShaped(id string) bool {
	return regexp.MustCompile(`^(D\d+|ADR \d+)$`).MatchString(id)
}

// resolveID looks an id up in the definition map. More than one definition is
// surfaced, never silently narrowed (R10).
func (idx *Index) resolveID(t *RefTarget, id string) *RefTarget {
	t.Kind = RefKindID
	defs := idx.ids[id]
	if len(defs) == 0 {
		t.State = RefUnresolved
		t.Reason = id + " is not defined in any doc in this repo"
		return t
	}
	t.State = RefResolved
	t.Defs = defs
	t.Path, t.Line = defs[0].Path, defs[0].Line
	t.Subjects = idx.subjectsOf(defs)
	return t
}

// subjectsOf extracts the file anchors an item's own definition text carries.
// Only anchors that exist in the tree are returned — promising a subject file
// that isn't there is the same dead link in a different coat.
func (idx *Index) subjectsOf(defs []RefDef) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range defs {
		for _, m := range refSubjectPattern.FindAllStringSubmatch(d.Text, -1) {
			if p, ok := idx.uniquePath(m[1]); ok && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}

// resolvePath resolves a repo path, with or without a :line suffix.
func (idx *Index) resolvePath(t *RefTarget, token string) *RefTarget {
	path := strings.TrimPrefix(token, "./")
	if m := fileLinePattern.FindStringSubmatch(path); m != nil {
		if n, err := strconv.Atoi(m[2]); err == nil {
			path, t.Line = m[1], n
		}
	}
	path = filepath.ToSlash(path)
	// An absolute path inside this project names the same file as its
	// repo-relative form; a transcript writes both.
	if rel, ok := strings.CutPrefix(path, idx.root+"/"); ok {
		path = rel
	}
	path = strings.TrimLeft(path, "/")
	t.Path = path
	t.Kind = RefKindPath
	if strings.HasSuffix(path, ".md") {
		t.Kind = RefKindDoc
	}
	if idx.paths[path] {
		t.State = RefResolved
		return t
	}
	switch hits := idx.suffixMatches(path); len(hits) {
	case 0:
		return idx.markAbsent(t, path)
	case 1:
		t.Path, t.State = hits[0], RefResolved
		return t
	default:
		// Ambiguous: guessing between two files is how a link goes to the wrong
		// place. Say which candidates matched — "no file at this path" would be a
		// lie, since several files match it.
		t.State = RefUnresolved
		t.Reason = "ambiguous: " + strconv.Itoa(len(hits)) + " files match this name (" +
			strings.Join(hits, ", ") + ")"
		t.Defs = suffixCandidates(hits, t.Line)
		return t
	}
}

// suffixMatches returns every tree path ending with the partial path a transcript
// often writes — a bare filename ("govern.go"), or a tail like "js/app.js" for
// "internal/daemon/static/js/app.js".
func (idx *Index) suffixMatches(path string) []string {
	var hits []string
	for p := range idx.paths {
		if strings.HasSuffix(p, "/"+path) {
			hits = append(hits, p)
		}
	}
	sort.Strings(hits)
	return hits
}

// suffixCandidates presents ambiguous matches as pickable targets, so the reader
// chooses instead of the index guessing.
func suffixCandidates(hits []string, line int) []RefDef {
	defs := make([]RefDef, 0, len(hits))
	for _, h := range hits {
		defs = append(defs, RefDef{Path: h, Line: line, Text: h})
	}
	return defs
}

// markAbsent decides between MISSING and STALE by asking git whether the path
// ever existed. That distinction is the difference between "you mistyped this"
// and "this moved since the session you are reading" — the single most useful
// thing to say about an old transcript's references.
func (idx *Index) markAbsent(t *RefTarget, path string) *RefTarget {
	known, checked := gitKnowsPath(idx.root, path)
	switch {
	case !checked:
		t.State = RefMissing
		t.Reason = "no file at this path (git history unavailable, so a move could not be ruled out)"
	case known:
		t.State = RefStale
		t.Reason = "referenced, no longer in the tree"
	default:
		t.State = RefMissing
		t.Reason = "no file at this path"
	}
	return t
}

// gitPathCache memoises history lookups: an unresolved path repeated across a
// transcript should cost one subprocess, not one per mention.
var (
	gitPathCacheMu sync.Mutex
	gitPathCache   = map[string]bool{}
)

// gitKnowsPath reports whether git has any history for path. The second return is
// false when git could not be consulted at all — the caller must not report a
// confident "missing" it did not earn.
func gitKnowsPath(root, path string) (known, checked bool) {
	key := root + "\x00" + path
	gitPathCacheMu.Lock()
	if v, ok := gitPathCache[key]; ok {
		gitPathCacheMu.Unlock()
		return v, true
	}
	gitPathCacheMu.Unlock()

	cmd := exec.Command("git", "log", "--oneline", "-1", "--all", "--", path)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return false, false // not a repo, or no git — stay honest about not knowing
	}
	known = len(strings.TrimSpace(string(out))) > 0
	gitPathCacheMu.Lock()
	gitPathCache[key] = known
	gitPathCacheMu.Unlock()
	return known, true
}
