package changeenv

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"crossing-guard/store"
)

var ErrResponseMetadataOverBudget = errors.New("response_metadata_over_budget")
var ErrInvalidFileView = errors.New("invalid_file_view")

type Mark struct {
	Class    string `json:"class"`
	SourceID int64  `json:"source_id,omitempty"`
	Lineage  string `json:"lineage,omitempty"`
	Events   int    `json:"source_event_count,omitempty"`
}
type FileRow struct {
	Path         string   `json:"path"`
	EvidenceKey  string   `json:"evidence_key,omitempty"`
	OldPath      string   `json:"old_path,omitempty"`
	StatusLayers []string `json:"status_layers,omitempty"`
	Planned      *Mark    `json:"planned,omitempty"`
	Touched      *Mark    `json:"touched,omitempty"`
	Changed      *Mark    `json:"changed,omitempty"`
	Claimed      *Mark    `json:"claimed,omitempty"`
	evidencePath string
}

// FileEvidenceIdentity is the exact Change-row identity carried by a drill-down key.
type FileEvidenceIdentity struct {
	RepositoryID string `json:"repository_id"`
	CheckoutID   string `json:"checkout_id"`
	Path         string `json:"path"`
	AbsolutePath string `json:"absolute_path"`
}

func encodeFileEvidenceKey(identity FileEvidenceIdentity) (string, error) {
	encoded, err := json.Marshal(identity)
	if err != nil {
		return "", fmt.Errorf("encode file evidence identity: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

// DecodeFileEvidenceKey decodes and validates a server-issued Change-row identity.
func DecodeFileEvidenceKey(key string) (FileEvidenceIdentity, error) {
	var identity FileEvidenceIdentity
	decoded, err := base64.RawURLEncoding.DecodeString(key)
	if err != nil || len(decoded) > 16<<10 {
		return identity, ErrInvalidFileView
	}
	if err := json.Unmarshal(decoded, &identity); err != nil {
		return identity, ErrInvalidFileView
	}
	if identity.RepositoryID == "" || identity.CheckoutID == "" || identity.Path == "" || identity.AbsolutePath == "" || outside(identity.Path) {
		return FileEvidenceIdentity{}, ErrInvalidFileView
	}
	return identity, nil
}

type Page struct {
	Total      int  `json:"total"`
	Returned   int  `json:"returned"`
	Offset     int  `json:"offset"`
	Limit      int  `json:"limit"`
	NextOffset *int `json:"next_offset,omitempty"`
	Exact      bool `json:"exact"`
}
type PathPage struct {
	Page
	Paths []string `json:"paths"`
}
type Divergence struct {
	Available bool     `json:"available"`
	Reason    string   `json:"reason,omitempty"`
	Added     PathPage `json:"added"`
	Omitted   PathPage `json:"omitted"`
}
type Verification struct {
	Name               string `json:"name"`
	NameClass          string `json:"name_class"`
	Boundary           string `json:"boundary"`
	Result             string `json:"result"`
	ResultClass        string `json:"result_class"`
	SourceID           int64  `json:"source_id"`
	StartedAt          int64  `json:"started_at"`
	EndedAt            int64  `json:"ended_at"`
	ExecutableIdentity string `json:"executable_identity,omitempty"`
	Termination        string `json:"termination,omitempty"`
}
type Gap struct {
	Code   string `json:"code"`
	Reason string `json:"reason"`
	Count  int    `json:"count,omitempty"`
}
type PathRelation struct {
	State  string `json:"state"`
	Root   string `json:"root,omitempty"`
	Reason string `json:"reason,omitempty"`
}
type CheckoutMatch struct {
	RepositoryID    string `json:"repository_id"`
	CheckoutID      string `json:"checkout_id"`
	CheckoutDisplay string `json:"checkout_display"`
	Selected        bool   `json:"selected"`
}
type ObservedTarget struct {
	Identity         string          `json:"identity"`
	Path             string          `json:"path,omitempty"`
	DisplayPath      string          `json:"display_path"`
	Lineage          string          `json:"lineage"`
	TargetSource     string          `json:"target_source"`
	Events           int             `json:"events"`
	Resolution       string          `json:"resolution"`
	ResolutionReason string          `json:"resolution_reason,omitempty"`
	Scope            string          `json:"scope"`
	WorkingDirectory PathRelation    `json:"working_directory"`
	TemporaryRoot    PathRelation    `json:"temporary_root"`
	CheckoutMatches  []CheckoutMatch `json:"checkout_matches,omitempty"`
}
type Selection struct {
	DeclarationID              int64  `json:"declaration_id,omitempty"`
	DeclarationSourceRef       string `json:"declaration_source_ref,omitempty"`
	DeclarationSourceDigest    string `json:"declaration_source_digest,omitempty"`
	DeclarationRecordedAt      int64  `json:"declaration_recorded_at,omitempty"`
	DeclarationSuperseded      int    `json:"declaration_superseded"`
	IntentLabel                string `json:"intent_label,omitempty"`
	RevisionID                 int64  `json:"revision_id,omitempty"`
	RevisionSourceRef          string `json:"revision_source_ref,omitempty"`
	RevisionSourceDigest       string `json:"revision_source_digest,omitempty"`
	RevisionCapturedAt         int64  `json:"revision_captured_at,omitempty"`
	RevisionCaptureStartedAt   int64  `json:"revision_capture_started_at,omitempty"`
	RevisionCaptureAttempts    int    `json:"revision_capture_attempts,omitempty"`
	RevisionSuperseded         int    `json:"revision_superseded"`
	RevisionNonAtomic          bool   `json:"revision_non_atomic"`
	RevisionSparseCheckout     bool   `json:"revision_sparse_checkout"`
	ImplementationID           int64  `json:"implementation_id,omitempty"`
	ImplementationSourceRef    string `json:"implementation_source_ref,omitempty"`
	ImplementationSourceDigest string `json:"implementation_source_digest,omitempty"`
	ImplementationRecordedAt   int64  `json:"implementation_recorded_at,omitempty"`
	ImplementationSuperseded   int    `json:"implementation_superseded"`
}
type RepositoryView struct {
	RepositoryID     string          `json:"repository_id"`
	CheckoutID       string          `json:"checkout_id"`
	IdentityKind     string          `json:"identity_kind"`
	CheckoutDisplay  string          `json:"checkout_display"`
	Portability      string          `json:"portability"`
	Files            []FileRow       `json:"files"`
	FilePage         Page            `json:"file_page"`
	FileFilter       string          `json:"file_filter"`
	FileQuery        string          `json:"file_query,omitempty"`
	FileSort         string          `json:"file_sort"`
	Divergence       Divergence      `json:"divergence"`
	Verification     []Verification  `json:"verification"`
	VerificationPage Page            `json:"verification_page"`
	Gaps             []Gap           `json:"gaps"`
	Counts           map[string]int  `json:"counts"`
	Completeness     map[string]bool `json:"completeness"`
	Selected         Selection       `json:"selected"`
	Downstream       ImpactView      `json:"downstream"`
}
type SessionView struct {
	Available       bool             `json:"available"`
	Partial         bool             `json:"partial"`
	Reason          string           `json:"reason,omitempty"`
	RepositoryCount int              `json:"repository_count"`
	RepositoryPage  Page             `json:"repository_page"`
	Repositories    []RepositoryView `json:"repositories"`
	History         Page             `json:"history"`
	ObservedTouches []ObservedTarget `json:"observed_touches,omitempty"`
	ObservedPage    Page             `json:"observed_page"`
	ObservedScope   string           `json:"observed_scope"`
	ObservedCounts  map[string]int   `json:"observed_scope_counts,omitempty"`
	ObservedExact   bool             `json:"observed_scope_counts_exact"`
	Gaps            []Gap            `json:"gaps,omitempty"`
	TouchCount      int              `json:"touch_count,omitempty"`
	GovernedEvents  int              `json:"governed_events,omitempty"`
	NonFileEvents   int              `json:"non_file_events,omitempty"`
}
type ViewOptions struct {
	RepositoryOffset, RepositoryLimit, FileOffset, FileLimit, AddedOffset, OmittedOffset, VerificationOffset int
	ObservedOffset, ObservedLimit                                                                            int
	ImpactNodeOffset, ImpactEdgeOffset, ImpactLimit, ImpactCandidateOffset, ImpactCandidateLimit             int
	ImpactCenterKind, ImpactCenterRef, AnalyzerBundleDigest                                                  string
	FileFilter, FileQuery, FileSort, ObservedScope                                                           string
	SessionRoots                                                                                             []string
}

func normalizeFileView(opt *ViewOptions) error {
	opt.FileFilter = strings.TrimSpace(opt.FileFilter)
	if opt.FileFilter == "" {
		opt.FileFilter = "all"
	}
	switch opt.FileFilter {
	case "all", "session", "outside-plan", "planned", "touched", "claimed", "changed":
	default:
		return fmt.Errorf("%w: unsupported change_filter %q", ErrInvalidFileView, opt.FileFilter)
	}
	opt.FileSort = strings.TrimSpace(opt.FileSort)
	if opt.FileSort == "" {
		opt.FileSort = "path"
	}
	if opt.FileSort != "path" && opt.FileSort != "overlap" {
		return fmt.Errorf("%w: unsupported change_sort %q", ErrInvalidFileView, opt.FileSort)
	}
	opt.FileQuery = strings.TrimSpace(opt.FileQuery)
	if len(opt.FileQuery) > 200 {
		return fmt.Errorf("%w: change_query exceeds 200 bytes", ErrInvalidFileView)
	}
	opt.ObservedScope = strings.TrimSpace(opt.ObservedScope)
	if opt.ObservedScope == "" {
		opt.ObservedScope = "all"
	}
	switch opt.ObservedScope {
	case "all", "selected-checkout", "other-checkout", "ambiguous-checkout", "working-directory", "temporary", "outside-identified-roots", "unresolved", "outside-selected":
	default:
		return fmt.Errorf("%w: unsupported observed_scope %q", ErrInvalidFileView, opt.ObservedScope)
	}
	return nil
}

func clamp(v, def, max int) int {
	if v < 0 {
		return 0
	}
	if v == 0 {
		return def
	}
	if v > max {
		return max
	}
	return v
}
func page(total, off, lim int) Page {
	if off < 0 {
		off = 0
	}
	if off > total {
		off = total
	}
	p := Page{Total: total, Offset: off, Limit: lim, Exact: off == 0 && off+lim >= total}
	end := off + lim
	if end > total {
		end = total
	}
	p.Returned = end - off
	if end < total {
		p.NextOffset = &end
	}
	return p
}
func slice[T any](v []T, off, lim int) ([]T, Page) {
	p := page(len(v), off, lim)
	end := p.Offset + p.Returned
	return v[p.Offset:end], p
}

type checkoutRoot struct {
	repositoryID, checkoutID, display, root string
}

type touchPlacement struct {
	view       ObservedTarget
	matchRoots []checkoutRoot
}

func uniqueCanonicalRoot(roots []string, absent, conflicting string) PathRelation {
	unique := map[string]bool{}
	for _, root := range roots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		canonical, err := resolveAllowMissing(root)
		if err != nil {
			return PathRelation{State: "unavailable", Reason: err.Error()}
		}
		unique[canonical] = true
	}
	if len(unique) == 0 {
		return PathRelation{State: "unavailable", Reason: absent}
	}
	if len(unique) > 1 {
		return PathRelation{State: "unavailable", Reason: conflicting}
	}
	for root := range unique {
		return PathRelation{State: "available", Root: root}
	}
	return PathRelation{State: "unavailable", Reason: absent}
}

func relationTo(root PathRelation, path string) PathRelation {
	if root.State != "available" {
		return root
	}
	rel, err := filepath.Rel(root.Root, path)
	if err != nil {
		return PathRelation{State: "unavailable", Root: root.Root, Reason: err.Error()}
	}
	if outside(rel) {
		return PathRelation{State: "outside", Root: root.Root}
	}
	return PathRelation{State: "inside", Root: root.Root}
}

// placeTouches is the one path-placement calculation used by both the observed-target
// view and repository T joins. It derives only root relationships; it does not infer a
// repository, role, feature, or resulting revision change.
func placeTouches(touches []store.SessionFileTouch, sessionRoots []string, checkoutRoots []checkoutRoot, selectedCheckoutID string) []touchPlacement {
	workingRoot := uniqueCanonicalRoot(sessionRoots, "no recorded working directory", "matching session segments report different working directories")
	temporaryCandidates := []string{os.TempDir()}
	if filepath.Separator == '/' {
		temporaryCandidates = append(temporaryCandidates, "/tmp", "/var/tmp")
	}
	temporaryRoots := []string{}
	seenTemporaryRoot := map[string]bool{}
	for _, candidate := range temporaryCandidates {
		canonical, err := resolveAllowMissing(candidate)
		if err == nil && !seenTemporaryRoot[canonical] {
			seenTemporaryRoot[canonical] = true
			temporaryRoots = append(temporaryRoots, canonical)
		}
	}
	sort.Slice(temporaryRoots, func(i, j int) bool { return len(temporaryRoots[i]) > len(temporaryRoots[j]) })
	out := make([]touchPlacement, 0, len(touches))
	for _, touch := range touches {
		rawPath := strings.TrimPrefix(touch.Identity, "file:")
		view := ObservedTarget{Identity: touch.Identity, DisplayPath: rawPath, Lineage: touch.Lineage, TargetSource: touch.TargetSource, Events: touch.Events, Resolution: "unresolved", Scope: "unresolved", WorkingDirectory: PathRelation{State: "unavailable", Reason: "target path unresolved"}, TemporaryRoot: PathRelation{State: "unavailable", Reason: "target path unresolved"}}
		canonical, err := resolveAllowMissing(rawPath)
		if err != nil {
			view.ResolutionReason = err.Error()
			out = append(out, touchPlacement{view: view})
			continue
		}
		view.Path, view.DisplayPath, view.Resolution = canonical, canonical, "resolved"
		view.WorkingDirectory = relationTo(workingRoot, canonical)
		view.TemporaryRoot = PathRelation{State: "unavailable", Reason: "no canonical host temporary roots available"}
		if len(temporaryRoots) > 0 {
			view.TemporaryRoot = PathRelation{State: "outside", Reason: fmt.Sprintf("checked %d canonical host temporary roots", len(temporaryRoots))}
			for _, root := range temporaryRoots {
				relation := relationTo(PathRelation{State: "available", Root: root}, canonical)
				if relation.State == "inside" {
					view.TemporaryRoot = relation
					break
				}
			}
		}
		placement := touchPlacement{view: view}
		for _, checkout := range checkoutRoots {
			rel, relErr := filepath.Rel(checkout.root, canonical)
			if relErr != nil || outside(rel) {
				continue
			}
			placement.matchRoots = append(placement.matchRoots, checkout)
			view.CheckoutMatches = append(view.CheckoutMatches, CheckoutMatch{RepositoryID: checkout.repositoryID, CheckoutID: checkout.checkoutID, CheckoutDisplay: checkout.display, Selected: checkout.repositoryID+"\x00"+checkout.checkoutID == selectedCheckoutID})
		}
		placement.view.CheckoutMatches = view.CheckoutMatches
		switch {
		case len(placement.matchRoots) > 1:
			placement.view.Scope = "ambiguous-checkout"
		case len(placement.matchRoots) == 1 && placement.matchRoots[0].repositoryID+"\x00"+placement.matchRoots[0].checkoutID == selectedCheckoutID:
			placement.view.Scope = "selected-checkout"
			if rel, relErr := filepath.Rel(placement.matchRoots[0].root, canonical); relErr == nil {
				placement.view.DisplayPath = filepath.ToSlash(rel)
			}
		case len(placement.matchRoots) == 1:
			placement.view.Scope = "other-checkout"
			if rel, relErr := filepath.Rel(placement.matchRoots[0].root, canonical); relErr == nil {
				placement.view.DisplayPath = filepath.ToSlash(rel)
			}
		case placement.view.WorkingDirectory.State == "inside":
			placement.view.Scope = "working-directory"
			if rel, relErr := filepath.Rel(placement.view.WorkingDirectory.Root, canonical); relErr == nil {
				placement.view.DisplayPath = filepath.ToSlash(rel)
			}
		case placement.view.TemporaryRoot.State == "inside":
			placement.view.Scope = "temporary"
		default:
			placement.view.Scope = "outside-identified-roots"
		}
		out = append(out, placement)
	}
	return out
}

func observedProjection(placements []touchPlacement, population store.SessionTouchPopulation, opt ViewOptions) ([]ObservedTarget, Page, map[string]int, bool) {
	counts := map[string]int{}
	filtered := make([]ObservedTarget, 0, len(placements))
	for _, placement := range placements {
		counts[placement.view.Scope]++
		matches := opt.ObservedScope == "all" || opt.ObservedScope == placement.view.Scope || (opt.ObservedScope == "outside-selected" && placement.view.Scope != "selected-checkout")
		if matches {
			filtered = append(filtered, placement.view)
		}
	}
	rows, p := slice(filtered, opt.ObservedOffset, clamp(opt.ObservedLimit, 200, 200))
	return rows, p, counts, !population.Partial
}

func sessionGaps(population store.SessionTouchPopulation) []Gap {
	gaps := []Gap{
		{Code: "revision_missing", Reason: "no Git revision snapshot captured"},
		{Code: "impact_missing", Reason: "no current repository understanding generation is linked"},
	}
	if population.EventTotal == 0 {
		gaps = append(gaps, Gap{Code: "touch_capture_missing", Reason: "no governed action events were captured; file touches are unavailable"})
	}
	if population.Partial {
		gaps = append(gaps, Gap{Code: "touches_partial", Reason: "touch set exceeded framework limit", Count: population.DistinctFiles - len(population.Touches)})
	}
	return gaps
}

func Build(ix *store.Index, sessionID string, opt ViewOptions) (SessionView, error) {
	if err := normalizeFileView(&opt); err != nil {
		return SessionView{}, err
	}
	touchPopulation, err := ix.SessionFileTouches(sessionID, 10000)
	if err != nil {
		return SessionView{}, err
	}
	recs, total, err := ix.ChangeRecordsForSession(sessionID, 1000)
	if err != nil {
		return SessionView{}, err
	}
	if len(recs) == 0 {
		reason := "no Git revision snapshot captured; optional explicit annotations are absent"
		if touchPopulation.DistinctFiles == 0 {
			reason += "; no exact file touches captured"
		}
		placements := placeTouches(touchPopulation.Touches, opt.SessionRoots, nil, "")
		observed, observedPage, observedCounts, observedExact := observedProjection(placements, touchPopulation, opt)
		out := SessionView{Reason: reason, Partial: touchPopulation.Partial, Gaps: sessionGaps(touchPopulation),
			ObservedTouches: observed, ObservedPage: observedPage, ObservedScope: opt.ObservedScope, ObservedCounts: observedCounts, ObservedExact: observedExact,
			TouchCount: touchPopulation.DistinctFiles, GovernedEvents: touchPopulation.EventTotal, NonFileEvents: touchPopulation.NonFileEvents}
		if err := FitResponse(&out, 2<<20); err != nil {
			return SessionView{}, err
		}
		return out, nil
	}
	type key struct{ repo, checkout string }
	groups := map[key][]store.ChangeRecord{}
	for _, r := range recs {
		k := key{r.RepositoryID, r.CheckoutID}
		groups[k] = append(groups[k], r)
	}
	keys := make([]key, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].repo == keys[j].repo {
			return keys[i].checkout < keys[j].checkout
		}
		return keys[i].repo < keys[j].repo
	})
	ro := opt.RepositoryOffset
	rl := clamp(opt.RepositoryLimit, 1, 1)
	selectedKeys, rp := slice(keys, ro, rl)
	partialReasons := []string{}
	if total > len(recs) {
		partialReasons = append(partialReasons, fmt.Sprintf("change history limited to newest %d of %d records", len(recs), total))
	}
	if touchPopulation.Partial {
		partialReasons = append(partialReasons, fmt.Sprintf("file touches limited to %d of %d distinct targets", len(touchPopulation.Touches), touchPopulation.DistinctFiles))
	}
	out := SessionView{Available: true, Partial: len(partialReasons) > 0, Reason: strings.Join(partialReasons, "; "), RepositoryCount: len(keys), RepositoryPage: rp, History: page(total, 0, 1000), TouchCount: touchPopulation.DistinctFiles, GovernedEvents: touchPopulation.EventTotal, NonFileEvents: touchPopulation.NonFileEvents}
	checkoutRoots := make([]checkoutRoot, 0, len(keys))
	for _, k := range keys {
		root := groups[k][0].CheckoutRoot
		if canonical, resolveErr := resolveAllowMissing(root); resolveErr == nil {
			root = canonical
		}
		checkoutRoots = append(checkoutRoots, checkoutRoot{repositoryID: k.repo, checkoutID: k.checkout, display: filepath.Base(root), root: root})
	}
	selectedCheckoutID := ""
	if len(selectedKeys) == 1 {
		selectedCheckoutID = selectedKeys[0].repo + "\x00" + selectedKeys[0].checkout
	}
	placements := placeTouches(touchPopulation.Touches, opt.SessionRoots, checkoutRoots, selectedCheckoutID)
	out.ObservedTouches, out.ObservedPage, out.ObservedCounts, out.ObservedExact = observedProjection(placements, touchPopulation, opt)
	out.ObservedScope = opt.ObservedScope
	for _, k := range selectedKeys {
		rv, err := buildRepository(ix, groups[k], touchPopulation, placements, opt)
		if err != nil {
			return SessionView{}, err
		}
		out.Repositories = append(out.Repositories, rv)
	}
	if err := FitResponse(&out, 2<<20); err != nil {
		return SessionView{}, err
	}
	return out, nil
}

// FitResponse reduces only returned pages, preserving exact totals and next offsets.
// Callers may pass the remaining budget of a larger response envelope.
func FitResponse(v *SessionView, max int) error {
	for {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if len(b) <= max {
			return nil
		}
		type candidate struct {
			n   int
			cut func()
		}
		reducible := func(n int) int {
			if n > 1 {
				return n
			}
			return 0
		}
		cs := []candidate{{reducible(len(v.ObservedTouches)), func() {
			v.ObservedTouches = v.ObservedTouches[:len(v.ObservedTouches)/2]
			adjustPage(&v.ObservedPage, len(v.ObservedTouches))
		}}}
		if len(v.Repositories) > 0 {
			r := &v.Repositories[0]
			cs = append(cs,
				candidate{reducible(len(r.Downstream.Candidates)), func() {
					r.Downstream.Candidates = r.Downstream.Candidates[:len(r.Downstream.Candidates)/2]
					adjustPage(&r.Downstream.CandidatePage, len(r.Downstream.Candidates))
				}},
				candidate{reducible(len(r.Downstream.Nodes)), func() {
					r.Downstream.Nodes = r.Downstream.Nodes[:len(r.Downstream.Nodes)/2]
					adjustPage(&r.Downstream.NodePage, len(r.Downstream.Nodes))
				}},
				candidate{reducible(len(r.Downstream.Edges)), func() {
					r.Downstream.Edges = r.Downstream.Edges[:len(r.Downstream.Edges)/2]
					adjustPage(&r.Downstream.EdgePage, len(r.Downstream.Edges))
				}},
				candidate{reducible(len(r.Files)), func() { r.Files = r.Files[:len(r.Files)/2]; adjustPage(&r.FilePage, len(r.Files)) }},
				candidate{reducible(len(r.Divergence.Added.Paths)), func() {
					r.Divergence.Added.Paths = r.Divergence.Added.Paths[:len(r.Divergence.Added.Paths)/2]
					adjustPage(&r.Divergence.Added.Page, len(r.Divergence.Added.Paths))
				}},
				candidate{reducible(len(r.Divergence.Omitted.Paths)), func() {
					r.Divergence.Omitted.Paths = r.Divergence.Omitted.Paths[:len(r.Divergence.Omitted.Paths)/2]
					adjustPage(&r.Divergence.Omitted.Page, len(r.Divergence.Omitted.Paths))
				}},
				candidate{reducible(len(r.Verification)), func() {
					r.Verification = r.Verification[:len(r.Verification)/2]
					adjustPage(&r.VerificationPage, len(r.Verification))
				}})
		}
		best := 0
		for i := 1; i < len(cs); i++ {
			if cs[i].n > cs[best].n {
				best = i
			}
		}
		if cs[best].n == 0 {
			return ErrResponseMetadataOverBudget
		}
		cs[best].cut()
	}
}
func adjustPage(p *Page, n int) {
	p.Returned = n
	p.Exact = p.Offset == 0 && p.Offset+n >= p.Total
	p.NextOffset = nil
	if p.Offset+n < p.Total {
		x := p.Offset + n
		p.NextOffset = &x
	}
}

func latest(rs []store.ChangeRecord, kind string) *store.ChangeRecord {
	var best *store.ChangeRecord
	for i := range rs {
		r := &rs[i]
		if r.Kind != kind {
			continue
		}
		if best == nil || r.RecordedAt > best.RecordedAt || (r.RecordedAt == best.RecordedAt && r.ID > best.ID) {
			best = r
		}
	}
	return best
}
func buildRepository(ix *store.Index, rs []store.ChangeRecord, touchPopulation store.SessionTouchPopulation, placements []touchPlacement, opt ViewOptions) (RepositoryView, error) {
	r0 := rs[0]
	rv := RepositoryView{RepositoryID: r0.RepositoryID, CheckoutID: r0.CheckoutID, IdentityKind: r0.RepositoryIdentityKind, CheckoutDisplay: filepath.Base(r0.CheckoutRoot), Portability: map[bool]string{true: "portable-remote", false: "local-only"}[r0.RepositoryIdentityKind == "remote-sha256"], Counts: map[string]int{}, Completeness: map[string]bool{}, FileFilter: opt.FileFilter, FileQuery: opt.FileQuery, FileSort: opt.FileSort}
	p, d, c := latest(rs, "declaration"), latest(rs, "revision"), latest(rs, "implementation")
	if p != nil {
		rv.Selected.DeclarationID = p.ID
		rv.Selected.DeclarationSourceRef = p.SourceRef
		rv.Selected.DeclarationSourceDigest = p.SourceDigest
		rv.Selected.DeclarationRecordedAt = p.RecordedAt
		rv.Selected.DeclarationSuperseded = countRecords(rs, p.Kind) - 1
		rv.Selected.IntentLabel = p.IntentLabel
	}
	if d != nil {
		rv.Selected.RevisionID = d.ID
		rv.Selected.RevisionSourceRef = d.SourceRef
		rv.Selected.RevisionSourceDigest = d.SourceDigest
		rv.Selected.RevisionCapturedAt = d.CaptureEndedAt
		rv.Selected.RevisionCaptureStartedAt = d.CaptureStartedAt
		rv.Selected.RevisionCaptureAttempts = d.CaptureAttempts
		rv.Selected.RevisionSuperseded = countRecords(rs, d.Kind) - 1
		rv.Selected.RevisionNonAtomic = d.NonAtomic
		rv.Selected.RevisionSparseCheckout = d.SparseCheckout
	} else {
		rv.Gaps = append(rv.Gaps, Gap{"revision_missing", "no Git revision snapshot captured", 0})
	}
	if c != nil {
		rv.Selected.ImplementationID = c.ID
		rv.Selected.ImplementationSourceRef = c.SourceRef
		rv.Selected.ImplementationSourceDigest = c.SourceDigest
		rv.Selected.ImplementationRecordedAt = c.RecordedAt
		rv.Selected.ImplementationSuperseded = countRecords(rs, c.Kind) - 1
	}
	var err error
	rv.Downstream, err = buildImpact(ix, r0.RepositoryID, r0.CheckoutID, d, opt)
	if err != nil {
		return RepositoryView{}, err
	}
	rows := map[string]*FileRow{}
	ensure := func(path string) *FileRow {
		if rows[path] == nil {
			rows[path] = &FileRow{Path: path}
		}
		return rows[path]
	}
	set := func(r *store.ChangeRecord, which string) {
		if r == nil {
			return
		}
		seen := map[string]bool{}
		for _, i := range r.Items {
			if seen[i.Path] {
				continue
			}
			seen[i.Path] = true
			row := ensure(i.Path)
			m := &Mark{Class: map[string]string{"revision": "observed"}[r.Kind], SourceID: r.ID}
			if m.Class == "" {
				m.Class = "claimed"
			}
			switch which {
			case "p":
				row.Planned = m
			case "d":
				row.Changed = m
				row.OldPath = i.OldPath
				row.StatusLayers = append(row.StatusLayers, i.Layer+":"+i.Status)
			case "c":
				row.Claimed = m
			}
		}
	}
	set(p, "p")
	set(d, "d")
	set(c, "c")
	matched, ambiguous, unmatched := 0, 0, 0
	for _, placement := range placements {
		if placement.view.Resolution != "resolved" {
			unmatched++
			continue
		}
		matchCount := len(placement.matchRoots)
		currentRoot := ""
		for _, root := range placement.matchRoots {
			if root.repositoryID == r0.RepositoryID && root.checkoutID == r0.CheckoutID {
				currentRoot = root.root
				break
			}
		}
		if matchCount == 0 {
			unmatched++
		}
		if currentRoot == "" {
			continue
		}
		if matchCount != 1 {
			ambiguous++
			continue
		}
		matched++
		rel, err := filepath.Rel(currentRoot, placement.view.Path)
		if err != nil || outside(rel) {
			unmatched++
			matched--
			continue
		}
		row := ensure(filepath.ToSlash(rel))
		row.evidencePath = placement.view.Path
		row.Touched = &Mark{Class: "observed", Lineage: placement.view.Lineage, Events: placement.view.Events}
	}
	if touchPopulation.EventTotal == 0 {
		rv.Gaps = append(rv.Gaps, Gap{"touch_capture_missing", "no governed action events were captured; file touches are unavailable", 0})
	}
	if touchPopulation.Partial {
		rv.Gaps = append(rv.Gaps, Gap{"touches_partial", "touch set exceeded framework limit", touchPopulation.DistinctFiles - len(touchPopulation.Touches)})
	}
	if ambiguous > 0 {
		rv.Gaps = append(rv.Gaps, Gap{"ambiguous_touches", "observed file touches matched more than one captured checkout root", ambiguous})
	}
	if unmatched > 0 {
		rv.Gaps = append(rv.Gaps, Gap{"unmatched_touches", "observed file touches did not match any captured checkout root", unmatched})
	}
	all := make([]FileRow, 0, len(rows))
	added, omitted := []string{}, []string{}
	for _, r := range rows {
		if r.evidencePath == "" {
			r.evidencePath = filepath.Join(r0.CheckoutRoot, filepath.FromSlash(r.Path))
		}
		var keyErr error
		r.EvidenceKey, keyErr = encodeFileEvidenceKey(FileEvidenceIdentity{RepositoryID: r0.RepositoryID,
			CheckoutID: r0.CheckoutID, Path: r.Path, AbsolutePath: r.evidencePath})
		if keyErr != nil {
			return RepositoryView{}, keyErr
		}
		sort.Strings(r.StatusLayers)
		all = append(all, *r)
		if r.Changed != nil && r.Planned == nil {
			added = append(added, r.Path)
		}
		if r.Planned != nil && r.Changed == nil {
			omitted = append(omitted, r.Path)
		}
	}
	sort.Strings(added)
	sort.Strings(omitted)
	rv.Counts = map[string]int{"file_union": len(all), "planned": count(all, func(r FileRow) bool { return r.Planned != nil }), "touched_total": matched, "touched_returned": matched, "events_total": touchPopulation.EventTotal, "non_file_events": touchPopulation.NonFileEvents, "explicit_declarations": countRecords(rs, "declaration"), "implementation_claims": countRecords(rs, "implementation"), "changed": count(all, func(r FileRow) bool { return r.Changed != nil }), "claimed": count(all, func(r FileRow) bool { return r.Claimed != nil }), "added_scope": len(added), "omitted_scope": len(omitted), "unmatched_touches": unmatched,
		"session_linked_known":  count(all, func(r FileRow) bool { return r.Planned != nil || r.Touched != nil || r.Claimed != nil }),
		"planned_changed":       count(all, func(r FileRow) bool { return r.Planned != nil && r.Changed != nil }),
		"touched_changed_known": count(all, func(r FileRow) bool { return r.Touched != nil && r.Changed != nil }),
		"claimed_changed":       count(all, func(r FileRow) bool { return r.Claimed != nil && r.Changed != nil })}
	rv.Completeness = map[string]bool{"planned": p != nil, "touched": touchPopulation.EventTotal > 0 && !touchPopulation.Partial && ambiguous == 0 && unmatched == 0, "changed": d != nil, "claimed": c != nil, "divergence": p != nil && d != nil}
	filtered := make([]FileRow, 0, len(all))
	query := strings.ToLower(opt.FileQuery)
	for _, row := range all {
		matches := map[string]bool{
			"all": true, "session": row.Planned != nil || row.Touched != nil || row.Claimed != nil,
			"outside-plan": row.Changed != nil && row.Planned == nil, "planned": row.Planned != nil,
			"touched": row.Touched != nil, "claimed": row.Claimed != nil, "changed": row.Changed != nil,
		}[opt.FileFilter]
		if matches && (query == "" || strings.Contains(strings.ToLower(row.Path), query)) {
			filtered = append(filtered, row)
		}
	}
	overlap := func(r FileRow) int {
		n := 0
		if r.Planned != nil {
			n++
		}
		if r.Touched != nil {
			n++
		}
		if r.Claimed != nil {
			n++
		}
		return n
	}
	sort.Slice(filtered, func(i, j int) bool {
		if opt.FileSort == "overlap" {
			if a, b := overlap(filtered[i]), overlap(filtered[j]); a != b {
				return a > b
			}
			for _, present := range []func(FileRow) bool{
				func(r FileRow) bool { return r.Planned != nil }, func(r FileRow) bool { return r.Touched != nil },
				func(r FileRow) bool { return r.Claimed != nil }, func(r FileRow) bool { return r.Changed != nil },
			} {
				if a, b := present(filtered[i]), present(filtered[j]); a != b {
					return a
				}
			}
		}
		return filtered[i].Path < filtered[j].Path
	})
	rv.Files, rv.FilePage = slice(filtered, opt.FileOffset, clamp(opt.FileLimit, 200, 200))
	rv.Counts["touched_returned"] = count(rv.Files, func(r FileRow) bool { return r.Touched != nil })
	rv.Divergence.Available = rv.Completeness["divergence"]
	if !rv.Divergence.Available {
		rv.Divergence.Reason = "declaration and complete revision evidence are required"
	}
	rv.Divergence.Added.Paths, rv.Divergence.Added.Page = slice(added, opt.AddedOffset, 200)
	rv.Divergence.Omitted.Paths, rv.Divergence.Omitted.Page = slice(omitted, opt.OmittedOffset, 200)
	vs := []Verification{}
	for _, r := range rs {
		if r.Kind == "verification" {
			vs = append(vs, Verification{Name: r.VerificationName, NameClass: "claimed", Boundary: r.VerificationBoundary, Result: r.VerificationResult, ResultClass: r.EvidenceClass, SourceID: r.ID, StartedAt: r.CaptureStartedAt, EndedAt: r.CaptureEndedAt, ExecutableIdentity: r.ExecutableDigest, Termination: r.Termination})
		}
	}
	rv.Counts["process_witnesses"] = len(vs)
	rv.Verification, rv.VerificationPage = slice(vs, opt.VerificationOffset, 200)
	return rv, nil
}
func count(v []FileRow, f func(FileRow) bool) int {
	n := 0
	for _, r := range v {
		if f(r) {
			n++
		}
	}
	return n
}

func countRecords(v []store.ChangeRecord, kind string) int {
	n := 0
	for _, r := range v {
		if r.Kind == kind {
			n++
		}
	}
	return n
}

func Metadata(ix *store.Index, sessionID string) (runtime, title string, recordID int64, err error) {
	recs, _, err := ix.ChangeRecordsForSession(sessionID, 1000)
	if err != nil {
		return "", "", 0, err
	}
	for i := 0; i < len(recs); i++ {
		if runtime == "" && strings.TrimSpace(recs[i].SessionRuntimeClaim) != "" {
			runtime = recs[i].SessionRuntimeClaim
			recordID = recs[i].ID
		}
		if title == "" && strings.TrimSpace(recs[i].SessionTitleClaim) != "" {
			title = recs[i].SessionTitleClaim
			if recordID == 0 {
				recordID = recs[i].ID
			}
		}
	}
	return
}

func Summary(v SessionView) string { return fmt.Sprintf("%d repositories", v.RepositoryCount) }
