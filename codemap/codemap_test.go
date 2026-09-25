package codemap

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fakeAnalyzer is a stand-in language adapter. Its existence in the test is the
// point: core is exercised WITHOUT any real language package, proving the port
// is what core depends on rather than any particular adapter.
type fakeAnalyzer struct {
	lang string
	id   string
	exts []string
	out  *Mechanics
	err  error
}

func (f *fakeAnalyzer) Language() string     { return f.lang }
func (f *fakeAnalyzer) Extensions() []string { return f.exts }
func (f *fakeAnalyzer) Identity() string {
	if f.id != "" {
		return f.id
	}
	return f.lang + "-test-v1"
}
func (f *fakeAnalyzer) Coverage() []Capability {
	return []Capability{CapabilityPackageDependency, CapabilitySymbolDeclaration}
}
func (f *fakeAnalyzer) Analyze(root, rel string) (*Mechanics, error) {
	return f.out, f.err
}

func TestFactoryResolvesByExtension(t *testing.T) {
	analyzers = map[string]Analyzer{} // isolate from other tests
	Register(&fakeAnalyzer{lang: "toy", exts: []string{".toy"}, out: &Mechanics{}})

	if _, ok := analyzerFor("a/b/c.toy"); !ok {
		t.Fatal("registered adapter did not claim its own extension")
	}
	if _, ok := analyzerFor("a/b/c.other"); ok {
		t.Fatal("an adapter claimed an extension it never declared")
	}
	if _, ok := analyzerFor("Makefile"); ok {
		t.Fatal("an extensionless file matched an adapter")
	}
}

// An empty registry must be an honest, non-fatal state: most projects contain
// files nobody analyzes.
func TestEmptyRegistryIsHonestNotFatal(t *testing.T) {
	analyzers = map[string]Analyzer{}
	if got := Languages(); len(got) != 0 {
		t.Fatalf("expected no languages bound, got %v", got)
	}
	err := (&ErrNoAnalyzer{Path: "x.toy", Registered: Languages()}).Error()
	if err == "" {
		t.Fatal("ErrNoAnalyzer must explain itself")
	}
}

func TestRoleIsUnknownWithAReasonWhenNothingMatches(t *testing.T) {
	cfg := &Config{Name: "test-config", Rules: []RoleRule{
		{Name: "never", Role: "service", PathGlob: []string{"nowhere/**"}},
	}}
	got := assignRole(cfg, "src/thing.toy", &Mechanics{}, 0)
	if got.Source != RoleUnknown {
		t.Fatalf("unmatched unit got role %q from %q — a role must never be invented", got.Layer, got.Source)
	}
	if got.Why == "" {
		t.Fatal("an unknown role must say why")
	}
}

// With no config at all, core must still answer — and must say that the silence
// is a missing config, not a property of the code.
func TestNoConfigSaysSo(t *testing.T) {
	got := assignRole(nil, "a.toy", &Mechanics{}, 0)
	if got.Source != RoleUnknown || got.Why == "" {
		t.Fatalf("missing config must produce an explained unknown, got %+v", got)
	}
}

func TestFirstMatchingRuleWinsAndIsNamed(t *testing.T) {
	cfg := &Config{Name: "c", Rules: []RoleRule{
		{Name: "specific", Role: "entrypoint", PathGlob: []string{"cmd/**"}, SignalsAny: []string{"func:main"}},
		{Name: "general", Role: "service", PathGlob: []string{"cmd/**"}},
	}}
	got := assignRole(cfg, "cmd/app/main.toy", &Mechanics{Signals: []string{"func:main"}}, 0)
	if got.Rule != "specific" || got.Layer != "entrypoint" {
		t.Fatalf("order must decide specificity; got rule=%q layer=%q", got.Rule, got.Layer)
	}
	if got.Source != RoleFromConvention {
		t.Fatalf("a config rule assigned this, so source must be convention; got %q", got.Source)
	}
}

func TestGlobMatching(t *testing.T) {
	cases := []struct {
		glob, path string
		want       bool
	}{
		{"cmd/**", "cmd/app/main.go", true},
		{"cmd/**", "internal/app/main.go", false},
		{"**/static/**", "internal/daemon/static/js/app.js", true},
		{"store/**", "store/index.go", true},
		{"store/**", "engine/index.go", false},
	}
	for _, c := range cases {
		if got := matchGlob(c.glob, c.path); got != c.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", c.glob, c.path, got, c.want)
		}
	}
}

// Provenance must never be silently absent for a value the panel will show.
func TestDescriptorTagsProvenance(t *testing.T) {
	d := &UnitDescriptor{}
	d.tag("metrics.loc", Measured)
	if d.Provenance["metrics.loc"] != Measured {
		t.Fatal("tag did not record provenance")
	}
	if d.Provenance["role.layer"] != "" {
		t.Fatal("an untagged field must have NO provenance rather than a default one")
	}
}

func TestAnalyzerBundleDigestIsDeterministicAndIdentitySensitive(t *testing.T) {
	analyzers = map[string]Analyzer{}
	Register(&fakeAnalyzer{lang: "zeta", exts: []string{".z", ".zz"}, out: &Mechanics{}})
	Register(&fakeAnalyzer{lang: "alpha", exts: []string{".a"}, out: &Mechanics{}})
	first := AnalyzerBundleDigest()
	analyzers = map[string]Analyzer{}
	Register(&fakeAnalyzer{lang: "alpha", exts: []string{".a"}, out: &Mechanics{}})
	Register(&fakeAnalyzer{lang: "zeta", exts: []string{".zz", ".z"}, out: &Mechanics{}})
	if second := AnalyzerBundleDigest(); second != first {
		t.Fatalf("registration or extension order changed bundle identity: %q != %q", second, first)
	}
	if !strings.HasPrefix(first, "sha256-v1:") {
		t.Fatalf("bundle digest is not self-describing: %q", first)
	}
	Register(&fakeAnalyzer{lang: "zeta", id: "zeta-test-v2", exts: []string{".zz", ".z"}, out: &Mechanics{}})
	if changed := AnalyzerBundleDigest(); changed == first {
		t.Fatal("adapter identity change did not change bundle identity")
	}
}

func TestAnalyzeManifestUsesOnlyExplicitPathsAndReportsUnsupported(t *testing.T) {
	analyzers = map[string]Analyzer{}
	Register(&fakeAnalyzer{lang: "toy", exts: []string{".toy"}, out: &Mechanics{
		Namespace: "pkg/a", Imports: []string{"pkg/b", "pkg/b"},
		Declarations: []Declaration{{Name: "Thing", Line: 3}}, LOC: 2,
	}})
	root := t.TempDir()
	for path, body := range map[string]string{
		"a.toy":            "one\ntwo\n",
		"not-selected.toy": "must not be analyzed\n",
		"notes.txt":        "unsupported but explicitly present\n",
	} {
		if err := os.WriteFile(filepath.Join(root, path), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	result, err := NewDescriber(root, nil).AnalyzeManifest([]string{"notes.txt", "a.toy", "a.toy"})
	if err != nil {
		t.Fatal(err)
	}
	if result.PathTotal != 2 || len(result.Units) != 1 || result.Units[0].Path != "a.toy" {
		t.Fatalf("manifest widened or failed to deduplicate: %+v", result)
	}
	if len(result.Failures) != 1 || result.Failures[0].Path != "notes.txt" || result.Failures[0].Code != "unsupported_analyzer" {
		t.Fatalf("unsupported explicit path was hidden: %+v", result.Failures)
	}
	wantRelations := []string{"file_declares_symbol", "file_in_package", "package_depends_on"}
	gotRelations := make([]string, 0, len(result.Edges))
	for _, edge := range result.Edges {
		gotRelations = append(gotRelations, edge.Relation)
	}
	if !reflect.DeepEqual(gotRelations, wantRelations) {
		t.Fatalf("structural edges are not deterministic: got %v want %v", gotRelations, wantRelations)
	}
	if len(result.Coverage) != 4 || result.Coverage[0].State != "complete" || result.Coverage[1].Family != CapabilityResponsibilityFingerprint || result.Coverage[1].State != "unsupported" || result.Coverage[2].Family != CapabilitySymbolCall || result.Coverage[2].State != "unsupported" || result.Coverage[3].State != "complete" {
		t.Fatalf("adapter coverage was not explicit: %+v", result.Coverage)
	}
	if result.Units[0].Descriptor.Identity.AnalyzerID != "toy-test-v1" {
		t.Fatalf("descriptor lost exact analyzer identity: %+v", result.Units[0].Descriptor.Identity)
	}
	if result.Units[0].Descriptor.Identity.Path != "a.toy" || result.Units[0].Descriptor.Provenance["identity.path"] != Measured {
		t.Fatalf("descriptor path is not a measured fact: %+v", result.Units[0].Descriptor)
	}
	if hash := result.Units[0].Descriptor.SourceHash; !strings.HasPrefix(hash, "sha256-v1:") || len(hash) != len("sha256-v1:")+64 {
		t.Fatalf("durable descriptor retained a truncated source hash: %q", hash)
	}
	if notes := strings.Join(result.Units[0].Descriptor.Notes, "\n"); !strings.Contains(notes, "bounded manifest git history unavailable") {
		t.Fatalf("history failure was not an explicit descriptor limitation: %q", notes)
	}
}

func TestAnalyzeManifestBatchesCurrentPathEvolutionWithoutWidening(t *testing.T) {
	analyzers = map[string]Analyzer{}
	Register(&fakeAnalyzer{lang: "toy", exts: []string{".toy"}, out: &Mechanics{Namespace: "pkg", LOC: 10}})
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, path), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	write("a.toy", "first\n")
	write("b.toy", "only\n")
	write("outside.toy", "not selected\n")
	git("add", ".")
	git("-c", "user.name=CrossingGuard", "-c", "user.email=crossing-guard@example.invalid", "commit", "-q", "-m", "first")
	write("a.toy", "second\n")
	git("add", "a.toy")
	git("-c", "user.name=CrossingGuard", "-c", "user.email=crossing-guard@example.invalid", "commit", "-q", "-m", "second")
	write("new.toy", "untracked\n")

	result, err := NewDescriber(root, nil).AnalyzeManifest([]string{"new.toy", "b.toy", "a.toy"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Units) != 3 {
		t.Fatalf("unexpected units: %+v", result.Units)
	}
	byPath := map[string]*UnitDescriptor{}
	for _, unit := range result.Units {
		byPath[unit.Path] = unit.Descriptor
	}
	if byPath["a.toy"].Evolution.Commits != 2 || byPath["b.toy"].Evolution.Commits != 1 {
		t.Fatalf("batched commit counts are wrong: a=%+v b=%+v", byPath["a.toy"].Evolution, byPath["b.toy"].Evolution)
	}
	if byPath["a.toy"].Evolution.FirstSeen == "" || byPath["a.toy"].Evolution.LastSeen == "" {
		t.Fatalf("batched timestamps are missing: %+v", byPath["a.toy"].Evolution)
	}
	if !byPath["new.toy"].Evolution.IsNew || byPath["new.toy"].Provenance["evolution.is_new"] != Measured {
		t.Fatalf("untracked path was not measured as new: %+v", byPath["new.toy"])
	}
	for path, descriptor := range byPath {
		if notes := strings.Join(descriptor.Notes, "\n"); !strings.Contains(notes, "current-path history; renames are not followed") {
			t.Fatalf("%s hid manifest history semantics: %q", path, notes)
		}
	}
	if _, widened := byPath["outside.toy"]; widened {
		t.Fatal("batched history widened beyond the explicit manifest")
	}
}

func TestEvolutionKeepsMeasuredValuesWithoutHotspotJudgment(t *testing.T) {
	descriptor := &UnitDescriptor{Metrics: Metrics{LOC: 201}}
	(&Describer{}).applyEvolutionObservation(descriptor, evolutionObservation{
		available: true,
		evolution: Evolution{Commits: 12, FirstSeen: "2026-01-01", LastSeen: "2026-08-15"},
	})
	if descriptor.Evolution.Commits != 12 || descriptor.Metrics.LOC != 201 {
		t.Fatalf("measured values changed: %+v", descriptor)
	}
	if descriptor.Evolution.Hotspot != "" {
		t.Fatalf("framework derived an unselected hotspot judgment: %+v", descriptor.Evolution)
	}
	if _, tagged := descriptor.Provenance["evolution.hotspot"]; tagged {
		t.Fatalf("framework published hotspot provenance: %+v", descriptor.Provenance)
	}
	legacy := &UnitDescriptor{Metrics: Metrics{LOC: 201}, Evolution: Evolution{Hotspot: "high"}}
	if strings.Contains(legacy.Summary(), "hotspot") {
		t.Fatalf("legacy compatibility value leaked into the summary: %q", legacy.Summary())
	}
}

func TestManifestHistoryOutputWriterRefusesOverflow(t *testing.T) {
	w := &limitedOutput{max: 3}
	if _, err := w.Write([]byte("four")); err == nil {
		t.Fatal("manifest history output exceeded its bound without refusal")
	}
	if w.buf.Len() != 0 {
		t.Fatalf("overflow retained partial history bytes: %d", w.buf.Len())
	}
}

func TestAnalyzeManifestReportsClaimedAnalyzerFailureAsPartial(t *testing.T) {
	analyzers = map[string]Analyzer{}
	Register(&fakeAnalyzer{lang: "toy", exts: []string{".toy"}, err: errors.New("fixture parse failure")})
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.toy"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := NewDescriber(root, nil).AnalyzeManifest([]string{"a.toy"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Failures) != 1 || result.Failures[0].Code != "analysis_failed" {
		t.Fatalf("analyzer failure was hidden: %+v", result.Failures)
	}
	if len(result.Coverage) != 4 || result.Coverage[0].State != "partial" || result.Coverage[1].State != "unsupported" || result.Coverage[2].State != "unsupported" || result.Coverage[3].State != "partial" {
		t.Fatalf("failed claimed paths became absence: %+v", result.Coverage)
	}
}

func TestAnalyzeManifestCountsResponsibilityFingerprints(t *testing.T) {
	analyzers = map[string]Analyzer{}
	Register(&responsibilityAnalyzer{fakeAnalyzer: fakeAnalyzer{lang: "shape", exts: []string{".shape"}, out: &Mechanics{Declarations: []Declaration{
		{Name: "One", BodyShapeDigest: "shape-v1:a", BodyShapeNodes: 3},
		{Name: "Two"},
		{Name: "Three", BodyShapeDigest: "shape-v1:b", BodyShapeNodes: 4},
	}}}})
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.shape"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := NewDescriber(root, nil).AnalyzeManifest([]string{"a.shape"})
	if err != nil {
		t.Fatal(err)
	}
	for _, coverage := range result.Coverage {
		if coverage.Family == CapabilityResponsibilityFingerprint {
			if coverage.State != "complete" || coverage.Attempted != 1 || coverage.Produced != 2 || coverage.Errors != 0 {
				t.Fatalf("responsibility coverage=%+v", coverage)
			}
			return
		}
	}
	t.Fatal("responsibility coverage row missing")
}

type responsibilityAnalyzer struct{ fakeAnalyzer }

func (r *responsibilityAnalyzer) Coverage() []Capability {
	return append(r.fakeAnalyzer.Coverage(), CapabilityResponsibilityFingerprint)
}
