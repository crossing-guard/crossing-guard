package daemon

// skills.go — Skills surface v0: inventory + coverage matrix, READ-ONLY.
//
// Current scope:
// panes 1+2 only (inventory, coverage matrix); adopt-inbox and editor deferred;
// all writes deferred (the "one-click fix" renders as a shown diff to copy).
//
// Epistemology grades on every cell (design contract: chips carry their
// source): [fs] file exists, [config] derived from vendor config parse,
// [probed] a live enumeration seam confirmed it. Probes are EXPLICIT
// (POST /api/skills/probe) because codex app-server is a seconds-scale spawn —
// never on page load.
//
// DEBT (flagged in the review): this is consoleprobe's third inline adapter
// (after harvest + frontmatter). At the cp merge, skills scanning becomes one
// shared library.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// skillRoot is a skills directory tagged with its scope and vendor class.
type skillRoot struct{ dir, scope, class string }

// SkillsProvider is one vendor's skills behavior: which dirs it reads, how it
// grades a skill's coverage, its vendor-specific lints, and its live-probe seam.
// Implementations live in skills_<vendor>.go and self-register via init(); the
// generic scan/probe never names a vendor (ADR 0020).
type SkillsProvider interface {
	Name() string
	Roots(home, repo string) []skillRoot
	Coverage(sk *Skill) CoverageCell
	Lint(se *SkillEntry, dirName string) []string // vendor-specific lints only
	Manages(dir string) bool                      // e.g. codex .system skills
	CanProbe() bool
	Probe() *ProbeInfo
}

var skillsProviders = map[string]SkillsProvider{}

func registerSkillsProvider(p SkillsProvider) { skillsProviders[p.Name()] = p }

// skillsProviderList returns providers in stable (sorted) order.
func skillsProviderList() []SkillsProvider {
	names := make([]string, 0, len(skillsProviders))
	for n := range skillsProviders {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]SkillsProvider, len(names))
	for i, n := range names {
		out[i] = skillsProviders[n]
	}
	return out
}

// discoveryRoots is the deduped union of every provider's read dirs (a skill
// discovered once, tagged by class; coverage cross-references classes).
func discoveryRoots(repo string) []skillRoot {
	h := homeDir()
	seen := map[string]bool{}
	var out []skillRoot
	for _, p := range skillsProviderList() {
		for _, r := range p.Roots(h, repo) {
			if !seen[r.dir] {
				seen[r.dir] = true
				out = append(out, r)
			}
		}
	}
	return out
}

type SkillEntry struct {
	Path          string            `json:"path"`
	Scope         string            `json:"scope"`     // user | repo
	DirClass      string            `json:"dir_class"` // agents | claude | opencode | codex
	Symlink       bool              `json:"symlink"`
	Target        string            `json:"target,omitempty"`
	Hash          string            `json:"hash"`
	DescLen       int               `json:"desc_len"`
	BodyBytes     int               `json:"body_bytes"`
	Files         int               `json:"files"`
	Frontmatter   map[string]string `json:"frontmatter"`
	ExtraFields   []string          `json:"extra_fields,omitempty"` // beyond the spec-5
	Lints         []string          `json:"lints,omitempty"`
	VendorManaged bool              `json:"vendor_managed"`
}

type CoverageCell struct {
	State string `json:"state"` // visible|hidden-by-deny|disabled|dropped-invalid|would-reject|not-synced|vendor-managed|not-installed
	Why   string `json:"why,omitempty"`
	Grade string `json:"grade"`         // fs|config|probed
	Fix   string `json:"fix,omitempty"` // shown as copyable diff, never auto-applied in v0
}

type Skill struct {
	Name         string                  `json:"name"`
	Description  string                  `json:"description"`
	AllowedTools []string                `json:"allowed_tools,omitempty"` // A GRANT, not a restriction
	Entries      []SkillEntry            `json:"entries"`
	Drift        bool                    `json:"drift"` // same name, differing content hashes
	Coverage     map[string]CoverageCell `json:"coverage"`
}

type SkillsReport struct {
	GeneratedAt string                `json:"generated_at"`
	Skills      []Skill               `json:"skills"`
	Providers   []SkillsProviderInfo  `json:"providers"`
	Probes      map[string]*ProbeInfo `json:"probes"`
	RepoDir     string                `json:"repo_dir,omitempty"`
}

// SkillsProviderInfo is the registry's browser-safe enumeration, including
// providers whose inventory is empty and those without a native probe seam.
type SkillsProviderInfo struct {
	Runtime  string `json:"runtime"`
	CanProbe bool   `json:"can_probe"`
}

type ProbeInfo struct {
	OK    bool     `json:"ok"`
	At    string   `json:"at"`
	Err   string   `json:"err,omitempty"`
	Names []string `json:"names,omitempty"` // enumerated skill names
	Extra []string `json:"extra,omitempty"` // enumerated but not found on fs scan
}

var probeCache = struct {
	sync.Mutex
	m map[string]*ProbeInfo
}{m: map[string]*ProbeInfo{}}

// specFields per the Agent Skills open spec — Codex's validator accepts exactly these.
var specFields = map[string]bool{"name": true, "description": true, "license": true, "allowed-tools": true, "metadata": true}

var nameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func scanSkills(repo string) *SkillsReport {
	byName := map[string]*Skill{}
	for _, root := range discoveryRoots(repo) {
		entries, err := os.ReadDir(root.dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() && e.Type()&os.ModeSymlink == 0 {
				continue
			}
			dir := filepath.Join(root.dir, e.Name())
			mdPath := filepath.Join(dir, "SKILL.md")
			raw, err := os.ReadFile(mdPath)
			if err != nil {
				continue
			}
			fm, body := splitFrontmatter(string(raw))
			se := SkillEntry{
				Path: dir, Scope: root.scope, DirClass: root.class,
				Frontmatter: fm, BodyBytes: len(body),
			}
			if p := skillsProviders[root.class]; p != nil {
				se.VendorManaged = p.Manages(dir)
			}
			if li, err := os.Lstat(dir); err == nil && li.Mode()&os.ModeSymlink != 0 {
				se.Symlink = true
				if t, err := filepath.EvalSymlinks(dir); err == nil {
					se.Target = t
				}
			}
			sum := sha256.Sum256(raw)
			se.Hash = hex.EncodeToString(sum[:4])
			se.DescLen = len(fm["description"])
			se.Files = countFiles(dir)
			// generic lints (vendor-agnostic); vendor-specific lints come from
			// each provider below.
			name := fm["name"]
			if name == "" {
				se.Lints = append(se.Lints, "missing name in frontmatter")
			} else if !nameRe.MatchString(name) {
				se.Lints = append(se.Lints, "name not lowercase-hyphen: "+name)
			}
			if fm["description"] == "" {
				se.Lints = append(se.Lints, "missing description (the trigger surface)")
			}
			for k, v := range fm {
				if !specFields[k] {
					se.ExtraFields = append(se.ExtraFields, k)
				}
				if yamlSuspect(v) {
					se.Lints = append(se.Lints, fmt.Sprintf("%s: %q looks like invalid YAML (two-bracket trap) — quote it", k, truncate(v, 40)))
				}
			}
			sort.Strings(se.ExtraFields) // deterministic output (fm iteration is random)
			for _, p := range skillsProviderList() {
				se.Lints = append(se.Lints, p.Lint(&se, e.Name())...)
			}

			key := name
			if key == "" {
				key = e.Name()
			}
			sk := byName[key]
			if sk == nil {
				sk = &Skill{Name: key, Description: fm["description"], Coverage: map[string]CoverageCell{}}
				if at := fm["allowed-tools"]; at != "" {
					for _, t := range strings.Split(strings.Trim(at, "[]"), ",") {
						if t = strings.TrimSpace(strings.Trim(strings.TrimSpace(t), `"'`)); t != "" {
							sk.AllowedTools = append(sk.AllowedTools, t)
						}
					}
				}
				byName[key] = sk
			}
			sk.Entries = append(sk.Entries, se)
		}
	}

	report := &SkillsReport{GeneratedAt: time.Now().UTC().Format(time.RFC3339), RepoDir: repo}
	for _, p := range skillsProviderList() {
		report.Providers = append(report.Providers, SkillsProviderInfo{Runtime: p.Name(), CanProbe: p.CanProbe()})
	}
	for _, sk := range byName {
		// drift: same name, more than one distinct hash among non-symlink entries
		hashes := map[string]bool{}
		for _, e := range sk.Entries {
			hashes[e.Hash] = true
		}
		sk.Drift = len(hashes) > 1
		for _, p := range skillsProviderList() {
			sk.Coverage[p.Name()] = p.Coverage(sk)
		}
		report.Skills = append(report.Skills, *sk)
	}
	// stable order: name
	for i := 0; i < len(report.Skills); i++ {
		for j := i + 1; j < len(report.Skills); j++ {
			if report.Skills[j].Name < report.Skills[i].Name {
				report.Skills[i], report.Skills[j] = report.Skills[j], report.Skills[i]
			}
		}
	}
	probeCache.Lock()
	report.Probes = map[string]*ProbeInfo{}
	for k, v := range probeCache.m {
		report.Probes[k] = v
	}
	probeCache.Unlock()
	overlayProbes(report)
	return report
}

func yamlSuspect(v string) bool {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, `"`) || strings.HasPrefix(v, "'") {
		return false // quoted = valid YAML string
	}
	return strings.Contains(v, "] [") || (strings.HasPrefix(v, "[") && !strings.HasSuffix(v, "]"))
}

func countFiles(dir string) int {
	n := 0
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}

func entriesIn(sk *Skill, classes ...string) []SkillEntry {
	var out []SkillEntry
	for _, e := range sk.Entries {
		for _, c := range classes {
			if e.DirClass == c {
				out = append(out, e)
			}
		}
	}
	return out
}

// --- probes (explicit, cached) ---

func overlayProbes(r *SkillsReport) {
	for runtime, pi := range r.Probes {
		if pi == nil || !pi.OK {
			continue
		}
		enumerated := map[string]bool{}
		for _, n := range pi.Names {
			enumerated[n] = true
		}
		known := map[string]bool{}
		for i := range r.Skills {
			known[r.Skills[i].Name] = true
			cell := r.Skills[i].Coverage[runtime]
			if enumerated[r.Skills[i].Name] {
				switch cell.State {
				case "visible", "not-synced":
					cell.State, cell.Grade = "visible", "probed"
					cell.Why = "enumerated live at " + pi.At
				case "would-reject":
					// Live data beats the config model: the reject applies to
					// the INSTALLER path; runtime discovery tolerates extras.
					// (Learned by probe 2026-07-16 — the epistemology working.)
					cell.State, cell.Grade = "visible", "probed"
					cell.Why = "enumerated live despite non-spec fields — installer validation would still reject a re-install"
				}
			} else if cell.State == "visible" {
				cell.State, cell.Grade = "dropped-invalid", "probed"
				cell.Why = "config said visible but live enumeration did NOT list it — the silent-loss case"
			}
			r.Skills[i].Coverage[runtime] = cell
		}
		// enumerated but not on the fs scan (e.g. vendor .system skills)
		pi.Extra = nil
		for _, n := range pi.Names {
			if !known[n] {
				pi.Extra = append(pi.Extra, n)
			}
		}
	}
}

func handleSkills(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, scanSkills(r.URL.Query().Get("repo")))
}

func handleSkillsProbe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Runtime string `json:"runtime"`
		Repo    string `json:"repo"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p := skillsProviders[req.Runtime]
	if p == nil || !p.CanProbe() {
		http.Error(w, "probe not available for this runtime", http.StatusBadRequest)
		return
	}
	pi := p.Probe()
	probeCache.Lock()
	probeCache.m[req.Runtime] = pi
	probeCache.Unlock()
	writeJSON(w, scanSkills(req.Repo))
}

func collectNames(v any, pi *ProbeInfo) {
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case []any:
			for _, item := range t {
				walk(item)
			}
		case map[string]any:
			if n, ok := t["name"].(string); ok {
				pi.Names = append(pi.Names, n)
				return
			}
			for _, val := range t {
				walk(val)
			}
		}
	}
	walk(v)
}
