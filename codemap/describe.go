package codemap

// Assembling a descriptor: adapter mechanics + config role + git evolution,
// cached for display (console-and-info-panel §5 scope boundary — compute and
// cache to DISPLAY here; durable materialisation into the governor store and any
// policy over it belong to console-understanding-graph.md, not this package).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Describer answers descriptor questions for one project root.
type Describer struct {
	root     string
	cfg      *Config
	assembly *AnalyzerAssembly

	mu    sync.Mutex
	cache map[string]*UnitDescriptor // cacheKey -> descriptor
	// The project graph: a unit's Ca, and its PACKAGE's type counts, cannot be
	// known from the unit alone.
	graph    *projectGraph
	graphAt  time.Time
	graphErr error
}

// pkgFacts aggregates a package. Martin's A, I and D are defined over a
// PACKAGE, not a file: a file with no type declarations has no abstractness,
// while the package it belongs to certainly does. Computing them per-file
// reported A=0 for `main.go` whose package declares 61 types — a category
// error dressed up as a measurement.
type pkgFacts struct {
	abstractTypes int
	totalTypes    int
	imports       map[string]bool // distinct internal packages this package uses
}

type projectGraph struct {
	fanIn map[string]int       // namespace -> how many packages import it (Ca)
	pkg   map[string]*pkgFacts // namespace -> aggregated facts
}

// NewDescriber binds a project root to a convention config. A nil config is
// legal and honest: roles then report "unknown, no config loaded" rather than
// being invented.
func NewDescriber(root string, cfg *Config) *Describer {
	return &Describer{root: root, cfg: cfg, cache: map[string]*UnitDescriptor{}}
}

// NewDescriberWithAssembly binds one immutable analyzer assembly to every descriptor and
// manifest read. Production hosts use this constructor so module replacement cannot
// diverge scan and file-detail facts.
func NewDescriberWithAssembly(root string, cfg *Config, assembly *AnalyzerAssembly) *Describer {
	return &Describer{root: root, cfg: cfg, assembly: assembly, cache: map[string]*UnitDescriptor{}}
}

func (d *Describer) analyzerAssembly() (*AnalyzerAssembly, error) {
	if d.assembly != nil {
		return d.assembly, nil
	}
	return RegisteredAnalyzerAssembly()
}

// fanInTTL bounds how long the project-wide reverse import graph is reused.
//
// Building it parses every unit in the project — measured at ~1.6s on this repo
// — so a short TTL means a reader browsing references pays that repeatedly. Ca
// only moves when an import statement changes, which is far rarer than reading,
// so this is deliberately long: a slightly stale caller COUNT is a much smaller
// cost than a panel that stalls every half minute. Per-file descriptors are
// unaffected — they key on content hash and refresh the moment a file changes.
const fanInTTL = 5 * time.Minute

// Describe returns the descriptor for one unit.
func (d *Describer) Describe(rel string) (*UnitDescriptor, error) {
	rel = filepath.ToSlash(strings.TrimPrefix(rel, "./"))
	hash, err := d.sourceHash(rel)
	if err != nil {
		return nil, err
	}
	key := rel + "@" + hash

	d.mu.Lock()
	if c, ok := d.cache[key]; ok {
		d.mu.Unlock()
		return c, nil
	}
	d.mu.Unlock()

	desc, err := d.build(rel, hash)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.cache[key] = desc
	d.mu.Unlock()
	return desc, nil
}

// sourceHash keys the cache on the CONTENT of the working tree, not on the git
// sha.
//
// The design (§5) says to key by (path, git-sha), but §10 sells live,
// session-scoped churn — "is this new, how edited THIS session" — as the thing
// git-history tools structurally cannot do. Those two pull opposite ways:
// uncommitted edits all share HEAD's sha, so a git-sha key would serve a stale
// descriptor for precisely the file the agent just changed, which is the case
// the feature exists for. Content hashing costs one read of a file we are about
// to analyze anyway.
func (d *Describer) sourceHash(rel string) (string, error) {
	body, err := os.ReadFile(filepath.Join(d.root, filepath.FromSlash(rel)))
	if err != nil {
		return "", fmt.Errorf("codemap: cannot read %s: %w", rel, err)
	}
	sum := sha256.Sum256(body)
	return "sha256-v1:" + hex.EncodeToString(sum[:]), nil
}

// build assembles one descriptor from its three independent sources.
func (d *Describer) build(rel, _ string) (*UnitDescriptor, error) {
	assembly, err := d.analyzerAssembly()
	if err != nil {
		return nil, err
	}
	provider := assembly.providerFor(rel)
	if compatibility, ok := provider.(*inProcessProvider); ok {
		hash, hashErr := d.sourceHash(rel)
		if hashErr != nil {
			return nil, hashErr
		}
		mechanics, analyzeErr := compatibility.analyzer.Analyze(d.root, rel)
		if analyzeErr != nil {
			return nil, fmt.Errorf("codemap: analyzing %s: %w", rel, analyzeErr)
		}
		return d.assemble(rel, hash, compatibility.analyzer.Language(), compatibility.analyzer.Identity(),
			mechanics, d.projectGraph(), nil), nil
	}
	result, err := d.AnalyzeManifestContext(context.Background(), []string{rel})
	if err != nil {
		return nil, err
	}
	if len(result.Units) == 0 {
		if len(result.Failures) == 1 && result.Failures[0].Code != "unsupported_analyzer" {
			return nil, fmt.Errorf("codemap: analyzing %s: %s", rel, result.Failures[0].Reason)
		}
		return nil, &ErrNoAnalyzer{Path: rel, Registered: assembly.Languages()}
	}
	return result.Units[0].Descriptor, nil
}

// assemble is the one descriptor assembly path used by single-file display and
// explicit-manifest durable analysis.
func (d *Describer) assemble(rel, hash, language, analyzerID string, mech *Mechanics, g *projectGraph, evolution *evolutionObservation) *UnitDescriptor {
	desc := &UnitDescriptor{SourceHash: hash}
	desc.Identity.Path = rel
	desc.Identity.Language = language
	desc.Identity.AnalyzerID = analyzerID
	desc.tag("identity.path", Measured)
	desc.tag("identity.language", Measured)
	desc.tag("identity.analyzer_id", Measured)
	d.applyMechanics(desc, mech)

	fanIn := 0
	if g != nil {
		fanIn = g.fanIn[desc.Identity.Namespace]
	}
	desc.Structure.FanIn = fanIn
	desc.tag("structure.fan_in", Measured)
	d.applyCoupling(desc, g, desc.Identity.Namespace, fanIn)

	// L4 role — inferred by definition: a rule judged it, it was not measured.
	desc.Role = assignRole(d.cfg, rel, mech, fanIn)
	if desc.Role.Source == RoleUnknown {
		desc.note(desc.Role.Why)
	} else {
		desc.tag("role.layer", Inferred)
	}

	if evolution == nil {
		d.applyEvolution(desc, rel)
	} else {
		d.applyEvolutionObservation(desc, *evolution)
	}
	return desc
}

func (d *Describer) applyMechanics(desc *UnitDescriptor, m *Mechanics) {
	desc.Identity.Namespace = m.Namespace
	desc.Symbol.Kind = m.Kind
	desc.Symbol.Exports = m.Exports
	desc.Symbol.Declarations = append([]Declaration(nil), m.Declarations...)
	sort.Slice(desc.Symbol.Declarations, func(i, j int) bool {
		a, b := desc.Symbol.Declarations[i], desc.Symbol.Declarations[j]
		if a.Identity != b.Identity {
			return a.Identity < b.Identity
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.BodyShapeDigest != b.BodyShapeDigest {
			return a.BodyShapeDigest < b.BodyShapeDigest
		}
		return a.BodyShapeNodes < b.BodyShapeNodes
	})
	desc.Structure.Imports = m.Imports
	desc.Structure.External = m.External
	desc.Structure.FanOut = len(m.Imports)
	desc.Metrics.LOC = m.LOC
	desc.Metrics.Cyclomatic = m.Cyclomatic
	for _, f := range []string{"identity.namespace", "symbol.exports", "symbol.declarations", "structure.imports",
		"structure.fan_out", "metrics.loc"} {
		desc.tag(f, Measured)
	}
	if m.Cyclomatic > 0 {
		desc.tag("metrics.cyclomatic", Measured)
	}
}

// applyCoupling computes Martin's A/I/D over the unit's PACKAGE — the scope the
// metrics are actually defined for. Arithmetic over numbers the adapter
// reported, so it stays in core: no language knowledge is involved.
func (d *Describer) applyCoupling(desc *UnitDescriptor, g *projectGraph, ns string, fanIn int) {
	if g == nil {
		desc.note("project graph unavailable, so package coupling was not computed")
		return
	}
	facts := g.pkg[ns]
	if facts == nil {
		desc.note("package " + ns + " was not aggregated, so A/I/D were not computed")
		return
	}
	ce, ca := float64(len(facts.imports)), float64(fanIn)
	if ce+ca > 0 {
		desc.Metrics.Instability = ce / (ce + ca)
		desc.tag("metrics.instability", Measured)
	}
	if facts.totalTypes > 0 {
		desc.Metrics.Abstractness = float64(facts.abstractTypes) / float64(facts.totalTypes)
		desc.tag("metrics.abstractness", Measured)
		desc.Metrics.Distance = abs(desc.Metrics.Abstractness + desc.Metrics.Instability - 1)
		desc.tag("metrics.distance", Measured)
	} else {
		desc.note("package " + ns + " declares no types, so abstractness and " +
			"distance-from-main-sequence are undefined for it")
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// applyEvolution reads git. Language-blind: history does not care what the file
// is written in.
func (d *Describer) applyEvolution(desc *UnitDescriptor, rel string) {
	out, err := d.git("log", "--follow", "--format=%h|%aI", "--", rel)
	if err != nil {
		d.applyEvolutionObservation(desc, evolutionObservation{limitation: "git history unavailable, so age and churn are unknown: " + err.Error()})
		return
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 1 && lines[0] == "" {
		d.applyEvolutionObservation(desc, evolutionObservation{available: true})
		return
	}
	observation := evolutionObservation{available: true}
	observation.evolution.Commits = len(lines)
	if _, ts, ok := strings.Cut(lines[0], "|"); ok {
		observation.evolution.LastSeen = ts
	}
	if _, ts, ok := strings.Cut(lines[len(lines)-1], "|"); ok {
		observation.evolution.FirstSeen = ts
	}
	d.applyEvolutionObservation(desc, observation)
}

type evolutionObservation struct {
	evolution   Evolution
	available   bool
	currentPath bool
	limitation  string
}

// applyEvolutionObservation is the one owner of evolution fields and provenance
// for both single-file and explicit-manifest descriptors.
func (d *Describer) applyEvolutionObservation(desc *UnitDescriptor, observation evolutionObservation) {
	if !observation.available {
		desc.note(observation.limitation)
		return
	}
	desc.Evolution = observation.evolution
	if desc.Evolution.Commits == 0 {
		desc.Evolution.IsNew = true
		desc.tag("evolution.is_new", Measured)
		desc.note("no commit touches this path yet — new or untracked")
		if observation.currentPath {
			desc.note("manifest evolution uses current-path history; renames are not followed")
		}
		return
	}
	for _, f := range []string{"evolution.commits", "evolution.last_seen", "evolution.first_seen"} {
		desc.tag(f, Measured)
	}
	if observation.currentPath {
		desc.note("manifest evolution uses current-path history; renames are not followed")
	}
}

// projectGraph returns the cached project-wide graph, rebuilding past the TTL.
func (d *Describer) projectGraph() *projectGraph {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.graph == nil || time.Since(d.graphAt) > fanInTTL {
		d.graph, d.graphErr = d.buildGraph()
		d.graphAt = time.Now()
	}
	if d.graphErr != nil {
		return nil
	}
	return d.graph
}

// buildGraph walks every unit an analyzer claims, counting incoming imports and
// aggregating each package's type counts.
func (d *Describer) buildGraph() (*projectGraph, error) {
	g := &projectGraph{fanIn: map[string]int{}, pkg: map[string]*pkgFacts{}}
	counts := g.fanIn
	err := filepath.WalkDir(d.root, func(p string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			name := entry.Name()
			if name == ".git" || name == "node_modules" || name == "vendor" ||
				(strings.HasPrefix(name, ".") && p != d.root) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(d.root, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		a, ok := analyzerFor(rel)
		if !ok {
			return nil
		}
		m, err := a.Analyze(d.root, rel)
		if err != nil {
			return nil
		}
		facts := g.pkg[m.Namespace]
		if facts == nil {
			facts = &pkgFacts{imports: map[string]bool{}}
			g.pkg[m.Namespace] = facts
		}
		facts.abstractTypes += m.AbstractTypes
		facts.totalTypes += m.TotalTypes

		seen := map[string]bool{}
		for _, imp := range m.Imports {
			facts.imports[imp] = true
			if !seen[imp] {
				seen[imp] = true
				counts[imp]++
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return g, nil
}

func (d *Describer) git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = d.root
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

const manifestHistoryOutputLimit = 16 << 20

type limitedOutput struct {
	buf bytes.Buffer
	max int
}

func (w *limitedOutput) Write(p []byte) (int, error) {
	if w.buf.Len()+len(p) > w.max {
		return 0, fmt.Errorf("git history output exceeds %d bytes", w.max)
	}
	return w.buf.Write(p)
}

func (d *Describer) gitLimited(limit int, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = d.root
	out := &limitedOutput{max: limit}
	cmd.Stdout = out
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return out.buf.Bytes(), nil
}

// Summary is a one-line place statement for the panel header.
func (d *UnitDescriptor) Summary() string {
	parts := []string{}
	if d.Role.Layer != "" {
		parts = append(parts, d.Role.Layer)
	}
	if d.Identity.Namespace != "" {
		parts = append(parts, d.Identity.Namespace)
	}
	if d.Metrics.LOC > 0 {
		parts = append(parts, strconv.Itoa(d.Metrics.LOC)+" LOC")
	}
	return strings.Join(parts, " · ")
}
