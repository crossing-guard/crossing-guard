package store

// C7 durable understanding persistence. This file owns typed SQL only. Git
// capture, analyzers, reference parsing, selection policy, and presentation live
// outside store.

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// facts_state values of a complete generation.
const (
	UnderstandingFactsPresent = "present"
	UnderstandingFactsPruned  = "pruned"
)

const (
	maxUnderstandingUnits            = 10000
	maxUnderstandingEdges            = 100000
	maxUnderstandingCoverage         = 64
	maxUnderstandingDescriptorBytes  = 256 << 10
	maxUnderstandingPayloadBytes     = 64 << 20
	maxUnderstandingPathLookup       = 100
	maxSessionUnderstandingCheckouts = 100
)

// UnderstandingCheckpoint is one completed Git boundary offered to the analyzer. The
// linked change record owns its immutable source identity.
type UnderstandingCheckpoint struct {
	Checkpoint SessionCheckpoint `json:"checkpoint"`
	Change     ChangeRecord      `json:"change"`
}

// SessionUnderstandingCheckout is one checkout with completed source evidence for a
// session. It contains identity only; callers select boundaries separately.
type SessionUnderstandingCheckout struct {
	RepositoryID string `json:"repository_id"`
	CheckoutID   string `json:"checkout_id"`
	CheckoutRoot string `json:"checkout_root"`
}

type UnderstandingGeneration struct {
	ID                     int64  `json:"id"`
	RepositoryID           string `json:"repository_id"`
	CheckoutID             string `json:"checkout_id"`
	CheckoutRoot           string `json:"checkout_root"`
	Status                 string `json:"status"`
	SnapshotProtocol       string `json:"snapshot_protocol,omitempty"`
	SnapshotDigest         string `json:"snapshot_digest,omitempty"`
	BaseRevision           string `json:"base_revision,omitempty"`
	HeadRevision           string `json:"head_revision,omitempty"`
	StructuralSchema       string `json:"structural_schema,omitempty"`
	AnalyzerBundleDigest   string `json:"analyzer_bundle_digest,omitempty"`
	ConventionState        string `json:"convention_state"`
	ConventionSourceRef    string `json:"convention_source_ref,omitempty"`
	ConventionSourceDigest string `json:"convention_source_digest,omitempty"`
	StartedAt              int64  `json:"started_at"`
	EndedAt                int64  `json:"ended_at"`
	PathTotal              int    `json:"path_total"`
	UnitTotal              int    `json:"unit_total"`
	EdgeTotal              int    `json:"edge_total"`
	ErrorTotal             int    `json:"error_total"`
	LimitationCode         string `json:"limitation_code,omitempty"`
	Limitation             string `json:"limitation,omitempty"`
	// FactsState is "present" or "pruned". Retention removes a complete
	// generation's units, edges and coverage and leaves the row; a pruned row is
	// history, never a measurement of nothing.
	FactsState string                  `json:"facts_state"`
	Units      []UnderstandingUnit     `json:"units,omitempty"`
	Edges      []UnderstandingEdge     `json:"edges,omitempty"`
	Coverage   []UnderstandingCoverage `json:"coverage,omitempty"`
}

type UnderstandingUnit struct {
	Path           string `json:"path"`
	SourceHash     string `json:"source_hash"`
	Language       string `json:"language,omitempty"`
	Namespace      string `json:"namespace,omitempty"`
	DescriptorJSON string `json:"descriptor_json"`
}

type UnderstandingEdge struct {
	FromKind       string `json:"from_kind"`
	FromRef        string `json:"from_ref"`
	Relation       string `json:"relation"`
	ToKind         string `json:"to_kind"`
	ToRef          string `json:"to_ref"`
	SourcePath     string `json:"source_path,omitempty"`
	SourceLine     int    `json:"source_line,omitempty"`
	Provenance     string `json:"provenance"`
	AnalyzerID     string `json:"analyzer_id"`
	EvidenceDigest string `json:"evidence_digest,omitempty"`
}

type UnderstandingCoverage struct {
	Family     string `json:"family"`
	State      string `json:"state"`
	AnalyzerID string `json:"analyzer_id"`
	Attempted  int    `json:"attempted"`
	Produced   int    `json:"produced"`
	Errors     int    `json:"errors"`
	Unresolved int    `json:"unresolved"`
	Ambiguous  int    `json:"ambiguous"`
	Reason     string `json:"reason,omitempty"`
}

// UnderstandingDependencyValues is an exact bounded projection of distinct incoming
// package names and their source files for a selected file population.
type UnderstandingDependencyValues struct {
	DependentPackages     []string
	DependentPackageTotal int
	ReferencingFiles      []string
	ReferencingFileTotal  int
}

type understandingDependencyValueKind int

const (
	dependentPackageValue understandingDependencyValueKind = iota
	dependencySourceFileValue
)

func validateUnderstanding(g *UnderstandingGeneration) error {
	if g == nil || g.RepositoryID == "" || g.CheckoutID == "" || g.CheckoutRoot == "" {
		return fmt.Errorf("understanding generation requires repository, checkout, and root identity")
	}
	if g.Status != "complete" && g.Status != "failed" {
		return fmt.Errorf("understanding generation status must be complete or failed")
	}
	if g.Status == "complete" && (g.SnapshotProtocol == "" || g.SnapshotDigest == "" ||
		g.StructuralSchema == "" || g.AnalyzerBundleDigest == "") {
		return fmt.Errorf("complete understanding generation requires snapshot protocol/digest, structural schema, and analyzer bundle identity")
	}
	if g.ConventionState != "none" && g.ConventionState != "explicit" {
		return fmt.Errorf("understanding generation convention state must be none or explicit")
	}
	if g.Status == "failed" && (len(g.Units) != 0 || len(g.Edges) != 0 || len(g.Coverage) != 0) {
		return fmt.Errorf("failed understanding generation cannot contain facts or coverage")
	}
	if len(g.Units) > maxUnderstandingUnits || len(g.Edges) > maxUnderstandingEdges || len(g.Coverage) > maxUnderstandingCoverage {
		return fmt.Errorf("understanding generation exceeds child limits: units=%d/%d edges=%d/%d coverage=%d/%d",
			len(g.Units), maxUnderstandingUnits, len(g.Edges), maxUnderstandingEdges, len(g.Coverage), maxUnderstandingCoverage)
	}
	payloadBytes := 0
	for _, unit := range g.Units {
		if len(unit.DescriptorJSON) > maxUnderstandingDescriptorBytes {
			return fmt.Errorf("understanding descriptor for %q is %d bytes; maximum is %d", unit.Path, len(unit.DescriptorJSON), maxUnderstandingDescriptorBytes)
		}
		payloadBytes += len(unit.Path) + len(unit.SourceHash) + len(unit.Language) + len(unit.Namespace) + len(unit.DescriptorJSON)
	}
	for _, edge := range g.Edges {
		payloadBytes += len(edge.FromKind) + len(edge.FromRef) + len(edge.Relation) + len(edge.ToKind) + len(edge.ToRef) + len(edge.SourcePath) + len(edge.AnalyzerID) + len(edge.EvidenceDigest)
	}
	for _, coverage := range g.Coverage {
		if coverage.Attempted < 0 || coverage.Produced < 0 || coverage.Errors < 0 || coverage.Unresolved < 0 || coverage.Ambiguous < 0 {
			return fmt.Errorf("understanding coverage counts cannot be negative")
		}
		payloadBytes += len(coverage.Family) + len(coverage.State) + len(coverage.AnalyzerID) + len(coverage.Reason)
	}
	if payloadBytes > maxUnderstandingPayloadBytes {
		return fmt.Errorf("understanding generation payload is %d bytes; maximum is %d", payloadBytes, maxUnderstandingPayloadBytes)
	}
	return nil
}

// AppendUnderstanding appends one attempt and, for a complete
// attempt, all of its facts atomically. The caller's ID is published only after
// commit succeeds.
func (ix *Index) AppendUnderstanding(g *UnderstandingGeneration) error {
	if err := validateUnderstanding(g); err != nil {
		return err
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`INSERT INTO understanding_generation(
		repository_id,checkout_id,checkout_root,status,snapshot_protocol,snapshot_digest,
		base_revision,head_revision,structural_schema,analyzer_bundle_digest,convention_state,
		convention_source_ref,convention_source_digest,started_at,ended_at,path_total,unit_total,
		edge_total,error_total,limitation_code,limitation)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`,
		g.RepositoryID, g.CheckoutID, g.CheckoutRoot, g.Status, g.SnapshotProtocol, g.SnapshotDigest,
		g.BaseRevision, g.HeadRevision, g.StructuralSchema, g.AnalyzerBundleDigest, g.ConventionState,
		g.ConventionSourceRef, g.ConventionSourceDigest, g.StartedAt, g.EndedAt, g.PathTotal,
		len(g.Units), len(g.Edges), g.ErrorTotal, g.LimitationCode, g.Limitation)
	if err != nil {
		return err
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if inserted == 0 {
		if g.Status != "complete" {
			return fmt.Errorf("failed understanding attempt was not inserted")
		}
		var existing UnderstandingGeneration
		err := tx.QueryRow(`SELECT `+understandingGenerationCols+` FROM understanding_generation
			WHERE repository_id=? AND checkout_id=? AND snapshot_digest=? AND structural_schema=?
			AND analyzer_bundle_digest=? AND convention_state=? AND convention_source_digest=?
			AND status='complete' AND facts_state='present'`, g.RepositoryID, g.CheckoutID, g.SnapshotDigest,
			g.StructuralSchema, g.AnalyzerBundleDigest, g.ConventionState,
			g.ConventionSourceDigest).Scan(&existing.ID, &existing.RepositoryID,
			&existing.CheckoutID, &existing.CheckoutRoot, &existing.Status,
			&existing.SnapshotProtocol, &existing.SnapshotDigest, &existing.BaseRevision,
			&existing.HeadRevision, &existing.StructuralSchema,
			&existing.AnalyzerBundleDigest, &existing.ConventionState,
			&existing.ConventionSourceRef, &existing.ConventionSourceDigest,
			&existing.StartedAt, &existing.EndedAt, &existing.PathTotal,
			&existing.UnitTotal, &existing.EdgeTotal, &existing.ErrorTotal,
			&existing.LimitationCode, &existing.Limitation, &existing.FactsState)
		if err != nil {
			return fmt.Errorf("load replayed understanding generation: %w", err)
		}
		if existing.CheckoutRoot != g.CheckoutRoot || existing.SnapshotProtocol != g.SnapshotProtocol ||
			existing.BaseRevision != g.BaseRevision || existing.HeadRevision != g.HeadRevision ||
			existing.ConventionSourceRef != g.ConventionSourceRef || existing.PathTotal != g.PathTotal ||
			existing.UnitTotal != len(g.Units) || existing.EdgeTotal != len(g.Edges) ||
			existing.ErrorTotal != g.ErrorTotal {
			return fmt.Errorf("complete understanding generation identity collision")
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		g.ID, g.UnitTotal, g.EdgeTotal = existing.ID, existing.UnitTotal, existing.EdgeTotal
		g.FactsState = existing.FactsState
		return nil
	}
	generationID, err := res.LastInsertId()
	if err != nil {
		return err
	}
	for _, unit := range g.Units {
		// The descriptor text is stored once per distinct text; an unchanged file's
		// unit points at the descriptor an earlier scan stored.
		digest := UnderstandingDescriptorDigest(unit.DescriptorJSON)
		if _, err = tx.Exec(`INSERT OR IGNORE INTO understanding_descriptor(digest,descriptor_json) VALUES(?,?)`, digest, unit.DescriptorJSON); err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT INTO understanding_unit(generation_id,path,source_hash,language,namespace,descriptor_digest) VALUES(?,?,?,?,?,?)`,
			generationID, unit.Path, unit.SourceHash, unit.Language, unit.Namespace, digest); err != nil {
			return err
		}
	}
	for _, edge := range g.Edges {
		if _, err = tx.Exec(`INSERT INTO understanding_edge(generation_id,from_kind,from_ref,relation,to_kind,to_ref,source_path,source_line,provenance,analyzer_id,evidence_digest) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			generationID, edge.FromKind, edge.FromRef, edge.Relation, edge.ToKind, edge.ToRef, edge.SourcePath, edge.SourceLine, edge.Provenance, edge.AnalyzerID, edge.EvidenceDigest); err != nil {
			return err
		}
	}
	for _, coverage := range g.Coverage {
		if _, err = tx.Exec(`INSERT INTO understanding_coverage(generation_id,family,state,analyzer_id,attempted,produced,errors,unresolved,ambiguous,reason) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			generationID, coverage.Family, coverage.State, coverage.AnalyzerID, coverage.Attempted, coverage.Produced, coverage.Errors, coverage.Unresolved, coverage.Ambiguous, coverage.Reason); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	g.ID = generationID
	g.UnitTotal = len(g.Units)
	g.EdgeTotal = len(g.Edges)
	g.FactsState = UnderstandingFactsPresent
	return nil
}

const understandingGenerationCols = `id,repository_id,checkout_id,checkout_root,status,
 snapshot_protocol,snapshot_digest,base_revision,head_revision,structural_schema,
 analyzer_bundle_digest,convention_state,convention_source_ref,convention_source_digest,
 started_at,ended_at,path_total,unit_total,edge_total,error_total,limitation_code,limitation,
 facts_state`

func scanUnderstandingGenerations(rows *sql.Rows) ([]UnderstandingGeneration, error) {
	defer rows.Close()
	out := []UnderstandingGeneration{}
	for rows.Next() {
		var g UnderstandingGeneration
		if err := rows.Scan(&g.ID, &g.RepositoryID, &g.CheckoutID, &g.CheckoutRoot, &g.Status,
			&g.SnapshotProtocol, &g.SnapshotDigest, &g.BaseRevision, &g.HeadRevision, &g.StructuralSchema,
			&g.AnalyzerBundleDigest, &g.ConventionState, &g.ConventionSourceRef, &g.ConventionSourceDigest,
			&g.StartedAt, &g.EndedAt, &g.PathTotal, &g.UnitTotal, &g.EdgeTotal, &g.ErrorTotal,
			&g.LimitationCode, &g.Limitation, &g.FactsState); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (ix *Index) UnderstandingGenerationByID(id int64) (UnderstandingGeneration, bool, error) {
	rows, err := ix.db.Query(`SELECT `+understandingGenerationCols+` FROM understanding_generation WHERE id=?`, id)
	if err != nil {
		return UnderstandingGeneration{}, false, err
	}
	gens, err := scanUnderstandingGenerations(rows)
	if err != nil || len(gens) == 0 {
		return UnderstandingGeneration{}, false, err
	}
	return gens[0], true, nil
}

func (ix *Index) UnderstandingGenerations(repositoryID, checkoutID string, limit int) ([]UnderstandingGeneration, int, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	var total int
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM understanding_generation WHERE repository_id=? AND checkout_id=?`, repositoryID, checkoutID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := ix.db.Query(`SELECT `+understandingGenerationCols+` FROM understanding_generation WHERE repository_id=? AND checkout_id=? ORDER BY started_at DESC,id DESC LIMIT ?`, repositoryID, checkoutID, limit)
	if err != nil {
		return nil, 0, err
	}
	gens, err := scanUnderstandingGenerations(rows)
	return gens, total, err
}

// UnderstandingForSnapshot returns the latest attempt for an exact immutable
// source/analyzer/configuration identity. It does not silently fall back to a
// stale or differently configured generation.
func (ix *Index) UnderstandingForSnapshot(repositoryID, checkoutID, snapshotDigest, structuralSchema, analyzerBundleDigest, conventionState, conventionSourceDigest string) (UnderstandingGeneration, bool, error) {
	rows, err := ix.db.Query(`SELECT `+understandingGenerationCols+` FROM understanding_generation
		WHERE repository_id=? AND checkout_id=? AND snapshot_digest=? AND structural_schema=?
		AND analyzer_bundle_digest=? AND convention_state=? AND convention_source_digest=?
		ORDER BY started_at DESC,id DESC LIMIT 1`, repositoryID, checkoutID, snapshotDigest,
		structuralSchema, analyzerBundleDigest, conventionState, conventionSourceDigest)
	if err != nil {
		return UnderstandingGeneration{}, false, err
	}
	gens, err := scanUnderstandingGenerations(rows)
	if err != nil || len(gens) == 0 {
		return UnderstandingGeneration{}, false, err
	}
	return gens[0], true, nil
}

func normalizeUnderstandingPage(offset, limit int) (int, int) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	return offset, limit
}

func normalizeUnderstandingPaths(paths []string, subject string) ([]string, error) {
	if len(paths) > maxUnderstandingPathLookup {
		return nil, fmt.Errorf("understanding %s has %d paths; maximum is %d", subject, len(paths), maxUnderstandingPathLookup)
	}
	unique := make(map[string]bool, len(paths))
	normalized := make([]string, 0, len(paths))
	for _, path := range paths {
		if path == "" || strings.HasPrefix(path, "/") || path == ".." || strings.HasPrefix(path, "../") {
			return nil, fmt.Errorf("unsafe understanding path %q", path)
		}
		if !unique[path] {
			unique[path] = true
			normalized = append(normalized, path)
		}
	}
	sort.Strings(normalized)
	return normalized, nil
}

func (ix *Index) UnderstandingUnits(generationID int64, offset, limit int) ([]UnderstandingUnit, int, error) {
	offset, limit = normalizeUnderstandingPage(offset, limit)
	var total int
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM understanding_unit WHERE generation_id=?`, generationID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := ix.db.Query(`SELECT unit.path,unit.source_hash,unit.language,unit.namespace,descriptor.descriptor_json
		FROM understanding_unit unit JOIN understanding_descriptor descriptor ON descriptor.digest=unit.descriptor_digest
		WHERE unit.generation_id=? ORDER BY unit.path LIMIT ? OFFSET ?`, generationID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []UnderstandingUnit{}
	for rows.Next() {
		var unit UnderstandingUnit
		if err := rows.Scan(&unit.Path, &unit.SourceHash, &unit.Language, &unit.Namespace, &unit.DescriptorJSON); err != nil {
			return nil, 0, err
		}
		out = append(out, unit)
	}
	return out, total, rows.Err()
}

func (ix *Index) UnderstandingEdges(generationID int64, centerKind, centerRef string, offset, limit int) ([]UnderstandingEdge, int, error) {
	offset, limit = normalizeUnderstandingPage(offset, limit)
	where := `generation_id=?`
	args := []any{generationID}
	if centerKind != "" || centerRef != "" {
		if centerKind == "" || centerRef == "" {
			return nil, 0, fmt.Errorf("understanding edge center requires both kind and ref")
		}
		where += ` AND ((from_kind=? AND from_ref=?) OR (to_kind=? AND to_ref=?))`
		args = append(args, centerKind, centerRef, centerKind, centerRef)
	}
	var total int
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM understanding_edge WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, limit, offset)
	rows, err := ix.db.Query(`SELECT from_kind,from_ref,relation,to_kind,to_ref,source_path,source_line,provenance,analyzer_id,evidence_digest FROM understanding_edge WHERE `+where+` ORDER BY from_kind,from_ref,relation,to_kind,to_ref,source_path,source_line,analyzer_id LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []UnderstandingEdge{}
	for rows.Next() {
		var edge UnderstandingEdge
		if err := rows.Scan(&edge.FromKind, &edge.FromRef, &edge.Relation, &edge.ToKind, &edge.ToRef, &edge.SourcePath, &edge.SourceLine, &edge.Provenance, &edge.AnalyzerID, &edge.EvidenceDigest); err != nil {
			return nil, 0, err
		}
		out = append(out, edge)
	}
	return out, total, rows.Err()
}

// understandingEdgeByTargetSQL names the edge table for a lookup by its "to" side.
// The index is named because the table has no rowid: the primary key is then a
// covering index, and the planner prefers it on generation_id alone to the "to"
// index on all three columns — a scan of a whole generation's edges per lookup.
const understandingEdgeByTargetSQL = `understanding_edge INDEXED BY understanding_edge_to`

// UnderstandingCallers returns measured incoming symbol-call edges for an exact,
// bounded set of analyzer-qualified declaration identities. It does not infer calls
// from names, packages, or file references.
func (ix *Index) UnderstandingCallers(generationID int64, symbolRefs []string, offset, limit int) ([]UnderstandingEdge, int, error) {
	if len(symbolRefs) == 0 {
		return []UnderstandingEdge{}, 0, nil
	}
	if len(symbolRefs) > maxUnderstandingPathLookup {
		return nil, 0, fmt.Errorf("understanding caller lookup has %d symbols; maximum is %d", len(symbolRefs), maxUnderstandingPathLookup)
	}
	unique := make(map[string]bool, len(symbolRefs))
	normalized := make([]string, 0, len(symbolRefs))
	for _, ref := range symbolRefs {
		if strings.TrimSpace(ref) == "" {
			return nil, 0, fmt.Errorf("understanding caller lookup contains an empty symbol identity")
		}
		if !unique[ref] {
			unique[ref] = true
			normalized = append(normalized, ref)
		}
	}
	sort.Strings(normalized)
	offset, limit = normalizeUnderstandingPage(offset, limit)
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(normalized)), ",")
	baseArgs := make([]any, 0, len(normalized)+1)
	baseArgs = append(baseArgs, generationID)
	for _, ref := range normalized {
		baseArgs = append(baseArgs, ref)
	}
	where := `generation_id=? AND relation='symbol_calls_symbol' AND to_kind='symbol' AND to_ref IN (` + placeholders + `)`
	var total int
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM `+understandingEdgeByTargetSQL+` WHERE `+where, baseArgs...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args := append(append([]any(nil), baseArgs...), limit, offset)
	rows, err := ix.db.Query(`SELECT from_kind,from_ref,relation,to_kind,to_ref,source_path,source_line,provenance,analyzer_id,evidence_digest
		FROM `+understandingEdgeByTargetSQL+` WHERE `+where+`
		ORDER BY to_ref,from_ref,source_path,source_line,analyzer_id LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []UnderstandingEdge{}
	for rows.Next() {
		var edge UnderstandingEdge
		if err := rows.Scan(&edge.FromKind, &edge.FromRef, &edge.Relation, &edge.ToKind, &edge.ToRef,
			&edge.SourcePath, &edge.SourceLine, &edge.Provenance, &edge.AnalyzerID, &edge.EvidenceDigest); err != nil {
			return nil, 0, err
		}
		out = append(out, edge)
	}
	return out, total, rows.Err()
}

// UnderstandingDependentPackageValues returns exact totals and bounded distinct values
// for measured incoming package dependencies to the packages containing selected files.
func (ix *Index) UnderstandingDependentPackageValues(generationID int64, paths []string, limit int) (UnderstandingDependencyValues, error) {
	out := UnderstandingDependencyValues{DependentPackages: []string{}, ReferencingFiles: []string{}}
	if len(paths) == 0 {
		return out, nil
	}
	normalized, err := normalizeUnderstandingPaths(paths, "dependent-package lookup")
	if err != nil {
		return out, err
	}
	_, limit = normalizeUnderstandingPage(0, limit)
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(normalized)), ",")
	cte := `WITH relevant_packages AS (
		SELECT DISTINCT to_ref AS package_ref FROM understanding_edge
		WHERE generation_id=? AND relation='file_in_package' AND from_kind='file' AND from_ref IN (` + placeholders + `)
	), matches AS (
		SELECT DISTINCT d.from_kind,d.from_ref,d.relation,d.to_kind,d.to_ref,d.source_path,d.source_line,d.provenance,d.analyzer_id,d.evidence_digest
		FROM understanding_edge d INDEXED BY understanding_edge_to JOIN relevant_packages p ON p.package_ref=d.to_ref
		WHERE d.generation_id=? AND d.relation='package_depends_on' AND d.to_kind='package'
	)`
	baseArgs := make([]any, 0, len(normalized)+2)
	baseArgs = append(baseArgs, generationID)
	for _, path := range normalized {
		baseArgs = append(baseArgs, path)
	}
	baseArgs = append(baseArgs, generationID)
	out.DependentPackages, out.DependentPackageTotal, err = ix.understandingDistinctValues(
		cte, baseArgs, dependentPackageValue, limit)
	if err != nil {
		return out, err
	}
	out.ReferencingFiles, out.ReferencingFileTotal, err = ix.understandingDistinctValues(
		cte, baseArgs, dependencySourceFileValue, limit)
	return out, err
}

func (ix *Index) understandingDistinctValues(cte string, baseArgs []any,
	kind understandingDependencyValueKind, limit int) ([]string, int, error) {
	column, where, label := "", "", ""
	switch kind {
	case dependentPackageValue:
		column, label = "from_ref", "dependent packages"
	case dependencySourceFileValue:
		column, where, label = "source_path", " WHERE source_path!=''", "dependency source files"
	default:
		return nil, 0, fmt.Errorf("unknown understanding dependency value kind %d", kind)
	}
	var total int
	if err := ix.db.QueryRow(cte+` SELECT COUNT(DISTINCT `+column+`) FROM matches`+where, baseArgs...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count %s: %w", label, err)
	}
	args := append(append([]any(nil), baseArgs...), limit)
	rows, err := ix.db.Query(cte+` SELECT DISTINCT `+column+` FROM matches`+where+` ORDER BY `+column+` LIMIT ?`, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("read %s: %w", label, err)
	}
	values := []string{}
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			rows.Close()
			return nil, 0, fmt.Errorf("scan %s: %w", label, err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, 0, fmt.Errorf("iterate %s: %w", label, err)
	}
	if err := rows.Close(); err != nil {
		return nil, 0, fmt.Errorf("close %s: %w", label, err)
	}
	return values, total, nil
}

func (ix *Index) UnderstandingCoverage(generationID int64) ([]UnderstandingCoverage, error) {
	rows, err := ix.db.Query(`SELECT family,state,analyzer_id,attempted,produced,errors,unresolved,ambiguous,reason FROM understanding_coverage WHERE generation_id=? ORDER BY family,analyzer_id`, generationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UnderstandingCoverage{}
	for rows.Next() {
		var coverage UnderstandingCoverage
		if err := rows.Scan(&coverage.Family, &coverage.State, &coverage.AnalyzerID, &coverage.Attempted, &coverage.Produced, &coverage.Errors, &coverage.Unresolved, &coverage.Ambiguous, &coverage.Reason); err != nil {
			return nil, err
		}
		out = append(out, coverage)
	}
	return out, rows.Err()
}

// UnderstandingUnitsForPaths returns descriptors only for an exact bounded path set.
func (ix *Index) UnderstandingUnitsForPaths(generationID int64, paths []string) ([]UnderstandingUnit, error) {
	if len(paths) == 0 {
		return []UnderstandingUnit{}, nil
	}
	normalized, err := normalizeUnderstandingPaths(paths, "path lookup")
	if err != nil {
		return nil, err
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(normalized)), ",")
	args := make([]any, 0, len(normalized)+1)
	args = append(args, generationID)
	for _, path := range normalized {
		args = append(args, path)
	}
	rows, err := ix.db.Query(`SELECT unit.path,unit.source_hash,unit.language,unit.namespace,descriptor.descriptor_json
		FROM understanding_unit unit JOIN understanding_descriptor descriptor ON descriptor.digest=unit.descriptor_digest
		WHERE unit.generation_id=? AND unit.path IN (`+placeholders+`)
		ORDER BY path`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UnderstandingUnit{}
	for rows.Next() {
		var unit UnderstandingUnit
		if err := rows.Scan(&unit.Path, &unit.SourceHash, &unit.Language, &unit.Namespace, &unit.DescriptorJSON); err != nil {
			return nil, err
		}
		out = append(out, unit)
	}
	return out, rows.Err()
}

// UnderstandingUnitIndex returns the bounded analyzed path/source population without
// descriptor payloads. Callers use it to page changed paths before fetching details.
func (ix *Index) UnderstandingUnitIndex(generationID int64) ([]UnderstandingUnit, error) {
	rows, err := ix.db.Query(`SELECT path,source_hash,language,namespace FROM understanding_unit
		WHERE generation_id=? ORDER BY path LIMIT ?`, generationID, maxUnderstandingUnits+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UnderstandingUnit{}
	for rows.Next() {
		var unit UnderstandingUnit
		if err := rows.Scan(&unit.Path, &unit.SourceHash, &unit.Language, &unit.Namespace); err != nil {
			return nil, err
		}
		out = append(out, unit)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) > maxUnderstandingUnits {
		return nil, fmt.Errorf("understanding unit index exceeds %d rows", maxUnderstandingUnits)
	}
	return out, nil
}

// UnderstandingCheckpointByID loads one completed checkpoint and its exact source
// record. Incomplete boundaries are not analyzer inputs.
func (ix *Index) UnderstandingCheckpointByID(checkpointID int64) (UnderstandingCheckpoint, error) {
	checkpoint, err := ix.SessionCheckpointByID(checkpointID)
	if err != nil {
		return UnderstandingCheckpoint{}, err
	}
	if checkpoint.Status != "complete" || checkpoint.ChangeRecordID == 0 {
		return UnderstandingCheckpoint{}, fmt.Errorf("checkpoint %d is not complete", checkpointID)
	}
	change, err := ix.ChangeRecordByID(checkpoint.ChangeRecordID)
	if err != nil {
		return UnderstandingCheckpoint{}, err
	}
	return UnderstandingCheckpoint{Checkpoint: checkpoint, Change: change}, nil
}

// SessionUnderstandingCheckouts returns a bounded, deterministic checkout population
// without loading checkpoint or descriptor rows.
func (ix *Index) SessionUnderstandingCheckouts(sessionID string, limit int) ([]SessionUnderstandingCheckout, error) {
	if limit <= 0 || limit > maxSessionUnderstandingCheckouts {
		limit = maxSessionUnderstandingCheckouts
	}
	rows, err := ix.db.Query(`SELECT repository_id,checkout_id,checkout_root
		FROM session_checkpoint WHERE session_id=? AND status='complete'
		AND repository_id!='' AND checkout_id!='' AND checkout_root!=''
		GROUP BY repository_id,checkout_id,checkout_root
		ORDER BY repository_id,checkout_id,checkout_root LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SessionUnderstandingCheckout{}
	for rows.Next() {
		var checkout SessionUnderstandingCheckout
		if err := rows.Scan(&checkout.RepositoryID, &checkout.CheckoutID, &checkout.CheckoutRoot); err != nil {
			return nil, err
		}
		out = append(out, checkout)
	}
	return out, rows.Err()
}

// SessionUnderstandingBoundaries selects source boundaries before selecting analysis.
// Baseline never falls forward from attachment/pre-mutation to a settled checkpoint.
func (ix *Index) SessionUnderstandingBoundaries(sessionID, repositoryID, checkoutID string) (baseline SessionCheckpoint, baselineFound bool, current SessionCheckpoint, currentFound bool, err error) {
	baseline, err = scanCheckpoint(ix.db.QueryRow(`SELECT `+checkpointCols+` FROM session_checkpoint
		WHERE session_id=? AND repository_id=? AND checkout_id=? AND status='complete'
		AND kind IN ('attachment','pre-mutation') ORDER BY capture_ended_at,id LIMIT 1`,
		sessionID, repositoryID, checkoutID))
	if err != nil && err != sql.ErrNoRows {
		return baseline, false, current, false, err
	}
	baselineFound = err == nil
	current, err = scanCheckpoint(ix.db.QueryRow(`SELECT `+checkpointCols+` FROM session_checkpoint
		WHERE session_id=? AND repository_id=? AND checkout_id=? AND status='complete'
		ORDER BY capture_ended_at DESC,id DESC LIMIT 1`, sessionID, repositoryID, checkoutID))
	if err != nil && err != sql.ErrNoRows {
		return baseline, baselineFound, current, false, err
	}
	return baseline, baselineFound, current, err == nil, nil
}

// RecentCheckpointsNeedingUnderstanding returns only the newest recent checkpoint per
// repository/checkout identity. Exact complete generations that still hold their facts
// suppress work (a pruned one is history, not an answer); recent exact failures enforce
// durable backoff.
func (ix *Index) RecentCheckpointsNeedingUnderstanding(since, retryBefore int64, structuralSchema, analyzerBundleDigest string, limit int) ([]SessionCheckpoint, error) {
	if limit <= 0 || limit > 10 {
		limit = 10
	}
	rows, err := ix.db.Query(`SELECT checkpoint.id,checkpoint.runtime,checkpoint.session_id,checkpoint.scope_key,
		checkpoint.kind,checkpoint.request_id,COALESCE(checkpoint.trigger_event_id,0),
		COALESCE(checkpoint.trigger_result_id,0),checkpoint.trigger_observation_id,
		COALESCE(checkpoint.predecessor_checkpoint_id,0),checkpoint.working_directory,
		checkpoint.repository_id,checkpoint.checkout_id,checkpoint.checkout_root,
		checkpoint.status,checkpoint.boundary_class,checkpoint.requested_at,
		checkpoint.capture_started_at,checkpoint.capture_ended_at,
		checkpoint.capture_attempts,COALESCE(checkpoint.change_record_id,0),
		checkpoint.failure_kind,checkpoint.detail_digest FROM session_checkpoint checkpoint
		JOIN change_record change ON change.id=checkpoint.change_record_id
		WHERE checkpoint.status='complete' AND checkpoint.checkout_id!=''
		AND checkpoint.capture_ended_at>=?
		AND checkpoint.id=(SELECT current.id FROM session_checkpoint current
			WHERE current.status='complete' AND current.repository_id=checkpoint.repository_id
			AND current.checkout_id=checkpoint.checkout_id
			ORDER BY current.capture_ended_at DESC,current.id DESC LIMIT 1)
		AND NOT EXISTS (SELECT 1 FROM understanding_generation complete
			WHERE complete.status='complete' AND complete.repository_id=change.repository_id
			AND complete.checkout_id=change.checkout_id AND complete.snapshot_digest=change.snapshot_digest
			AND complete.structural_schema=? AND complete.analyzer_bundle_digest=?
			AND complete.convention_state='none' AND complete.convention_source_digest=''
			AND complete.facts_state='present')
		AND NOT EXISTS (SELECT 1 FROM understanding_generation failed
			WHERE failed.status='failed' AND failed.repository_id=change.repository_id
			AND failed.checkout_id=change.checkout_id AND failed.snapshot_digest=change.snapshot_digest
			AND failed.structural_schema=? AND failed.analyzer_bundle_digest=?
			AND failed.convention_state='none' AND failed.convention_source_digest=''
			AND failed.ended_at>?)
		ORDER BY checkpoint.capture_ended_at DESC,checkpoint.id DESC LIMIT ?`, since,
		structuralSchema, analyzerBundleDigest, structuralSchema, analyzerBundleDigest,
		retryBefore, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SessionCheckpoint{}
	for rows.Next() {
		var checkpoint SessionCheckpoint
		if err := rows.Scan(&checkpoint.ID, &checkpoint.Runtime, &checkpoint.SessionID, &checkpoint.ScopeKey,
			&checkpoint.Kind, &checkpoint.RequestID, &checkpoint.TriggerEventID,
			&checkpoint.TriggerResultID, &checkpoint.TriggerObservationID,
			&checkpoint.PredecessorID, &checkpoint.WorkingDirectory,
			&checkpoint.RepositoryID, &checkpoint.CheckoutID, &checkpoint.CheckoutRoot,
			&checkpoint.Status, &checkpoint.BoundaryClass, &checkpoint.RequestedAt,
			&checkpoint.CaptureStartedAt, &checkpoint.CaptureEndedAt,
			&checkpoint.CaptureAttempts, &checkpoint.ChangeRecordID,
			&checkpoint.FailureKind, &checkpoint.DetailDigest); err != nil {
			return nil, err
		}
		out = append(out, checkpoint)
	}
	return out, rows.Err()
}

// Understanding facts retention (understanding-facts-retention plan §2, §4.3).
//
// A complete generation KEEPS its facts while any of these holds:
//
//	K1  it matches (repository, checkout, snapshot digest) the baseline or the current
//	    checkpoint — exactly as SessionUnderstandingBoundaries selects them — of a
//	    session whose newest complete checkpoint ended at or after keepSince;
//	K2  it is the newest complete, present generation of its repository, checkout and
//	    convention state;
//	K3  it started at or after graceSince.
//
// The two clocks differ and the parameters carry the unit in their name:
// session_checkpoint.capture_ended_at is unix SECONDS, understanding_generation.started_at
// is unix NANOSECONDS.
//
// The rule is written twice on purpose. PrunableUnderstandingGenerations is the set
// form: one read that lists candidates and takes no write lock. It is advice.
// PruneUnderstandingFacts re-evaluates the rule for one generation inside the write
// transaction that marks it pruned, and is the decision: a checkpoint written by a
// resuming session lands either before the check (the generation is kept) or after
// the commit.

// understandingBoundarySQL is the per-generation form of K1: the checkpoints whose
// change record matches one generation's snapshot, then whether any of them is the
// baseline or the current boundary of a live session. Both steps are MATERIALIZED so
// the session subqueries run for those few checkpoints only; left to the planner they
// ran for every checkpoint in the store (23 s a prune on real data, measured
// 2026-10-03). It takes the generation id, then the K1 horizon in unix seconds.
const understandingBoundarySQL = `WITH matched AS MATERIALIZED (
		SELECT change.id FROM change_record change JOIN understanding_generation subject ON subject.id=?
		WHERE change.repository_id=subject.repository_id AND change.checkout_id=subject.checkout_id
		AND change.snapshot_digest=subject.snapshot_digest),
	boundary AS MATERIALIZED (
		SELECT checkpoint.id,checkpoint.session_id,checkpoint.repository_id,checkpoint.checkout_id
		FROM session_checkpoint checkpoint
		WHERE checkpoint.status='complete' AND checkpoint.change_record_id IN (SELECT id FROM matched)),
	kept AS MATERIALIZED (
		SELECT 1 FROM boundary
		WHERE (SELECT MAX(seen.capture_ended_at) FROM session_checkpoint seen
			WHERE seen.session_id=boundary.session_id AND seen.status='complete')>=?
		AND (boundary.id=(SELECT current.id FROM session_checkpoint current
				WHERE current.session_id=boundary.session_id AND current.repository_id=boundary.repository_id
				AND current.checkout_id=boundary.checkout_id AND current.status='complete'
				ORDER BY current.capture_ended_at DESC,current.id DESC LIMIT 1)
			OR boundary.id=(SELECT baseline.id FROM session_checkpoint baseline
				WHERE baseline.session_id=boundary.session_id AND baseline.repository_id=boundary.repository_id
				AND baseline.checkout_id=boundary.checkout_id AND baseline.status='complete'
				AND baseline.kind IN ('attachment','pre-mutation')
				ORDER BY baseline.capture_ended_at,baseline.id LIMIT 1))
		LIMIT 1)`

const understandingNewestOfCheckoutSQL = `NOT EXISTS(
	SELECT 1 FROM understanding_generation newer
	WHERE newer.repository_id=generation.repository_id AND newer.checkout_id=generation.checkout_id
	AND newer.convention_state=generation.convention_state AND newer.status='complete'
	AND newer.facts_state='present'
	AND (newer.started_at>generation.started_at
		OR (newer.started_at=generation.started_at AND newer.id>generation.id)))`

// PrunableUnderstandingGenerations lists, oldest first, complete generations whose
// facts no keep rule protects. keepSinceSeconds is K1's horizon in unix seconds;
// graceSinceNanos is K3's in unix nanoseconds.
func (ix *Index) PrunableUnderstandingGenerations(keepSinceSeconds, graceSinceNanos int64) ([]int64, error) {
	rows, err := ix.db.Query(`WITH live AS (
			SELECT session_id FROM session_checkpoint WHERE status='complete'
			GROUP BY session_id HAVING MAX(capture_ended_at)>=?),
		scoped AS (
			SELECT checkpoint.change_record_id,
				checkpoint.kind IN ('attachment','pre-mutation') AS opening,
				ROW_NUMBER() OVER (PARTITION BY checkpoint.session_id,checkpoint.repository_id,checkpoint.checkout_id
					ORDER BY checkpoint.capture_ended_at DESC,checkpoint.id DESC) AS newest_rank,
				ROW_NUMBER() OVER (PARTITION BY checkpoint.session_id,checkpoint.repository_id,checkpoint.checkout_id,
					checkpoint.kind IN ('attachment','pre-mutation')
					ORDER BY checkpoint.capture_ended_at,checkpoint.id) AS oldest_rank_in_class
			FROM session_checkpoint checkpoint JOIN live ON live.session_id=checkpoint.session_id
			WHERE checkpoint.status='complete'),
		boundary AS (
			SELECT change_record_id FROM scoped
			WHERE newest_rank=1 OR (opening AND oldest_rank_in_class=1)),
		kept AS (
			SELECT DISTINCT change.repository_id,change.checkout_id,change.snapshot_digest
			FROM change_record change JOIN boundary ON boundary.change_record_id=change.id
			WHERE change.snapshot_digest IS NOT NULL)
		SELECT generation.id FROM understanding_generation generation
		WHERE generation.status='complete' AND generation.facts_state='present'
		AND generation.started_at<?
		AND NOT `+understandingNewestOfCheckoutSQL+`
		AND NOT EXISTS(SELECT 1 FROM kept WHERE kept.repository_id=generation.repository_id
			AND kept.checkout_id=generation.checkout_id AND kept.snapshot_digest=generation.snapshot_digest)
		ORDER BY generation.started_at,generation.id`,
		keepSinceSeconds, graceSinceNanos)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// PruneUnderstandingFacts retires one complete generation's facts: in one write
// transaction it re-evaluates the keep rule and, when nothing keeps the generation,
// marks the row pruned and deletes its coverage. From that commit no reader resolves
// the generation's facts. Its unit and edge rows are unreachable garbage that
// DeletePrunedUnderstandingFacts removes in bounded transactions: the largest real
// generations (78,000 edges) took up to a second to delete in one transaction through
// this driver, too close to the busy timeout other writers wait on. It reports false,
// changing nothing, when the generation is kept, is not complete, or is already
// pruned. The generation row is never deleted.
func (ix *Index) PruneUnderstandingFacts(generationID, keepSinceSeconds, graceSinceNanos int64) (bool, error) {
	tx, err := ix.db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var prunable bool
	err = tx.QueryRow(understandingBoundarySQL+`
		SELECT generation.started_at<?
			AND NOT `+understandingNewestOfCheckoutSQL+`
			AND NOT EXISTS(SELECT 1 FROM kept)
		FROM understanding_generation generation
		WHERE generation.id=? AND generation.status='complete' AND generation.facts_state='present'`,
		generationID, keepSinceSeconds, graceSinceNanos, generationID).Scan(&prunable)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !prunable {
		return false, nil
	}
	for _, statement := range []string{
		`UPDATE understanding_generation SET facts_state='pruned' WHERE id=?`,
		`DELETE FROM understanding_coverage WHERE generation_id=?`,
	} {
		if _, err := tx.Exec(statement, generationID); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// DeletePrunedUnderstandingFacts deletes up to maxRows unit and edge rows of one
// pruned generation in one write transaction, edges first, and reports whether rows
// remain. It refuses a generation that is not pruned, so it can never remove facts a
// reader resolves.
func (ix *Index) DeletePrunedUnderstandingFacts(generationID int64, maxRows int) (bool, error) {
	if maxRows <= 0 {
		return false, fmt.Errorf("understanding facts delete needs a positive row budget")
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var state string
	err = tx.QueryRow(`SELECT facts_state FROM understanding_generation WHERE id=? AND status='complete'`, generationID).Scan(&state)
	if err == sql.ErrNoRows || (err == nil && state != UnderstandingFactsPruned) {
		return false, fmt.Errorf("understanding generation %d is not pruned; its facts are not deleted", generationID)
	}
	if err != nil {
		return false, err
	}
	budget := int64(maxRows)
	// The edge table has no rowid (schema 45), so its rows are named by the rest of
	// the primary key; the unit table by its path.
	for _, statement := range []string{
		`DELETE FROM understanding_edge WHERE generation_id=?1
			AND (from_kind,from_ref,relation,to_kind,to_ref,source_path,source_line,analyzer_id) IN (
				SELECT from_kind,from_ref,relation,to_kind,to_ref,source_path,source_line,analyzer_id
				FROM understanding_edge WHERE generation_id=?1 LIMIT ?2)`,
		`DELETE FROM understanding_unit WHERE generation_id=?1
			AND path IN (SELECT path FROM understanding_unit WHERE generation_id=?1 LIMIT ?2)`,
	} {
		if budget == 0 {
			break
		}
		result, err := tx.Exec(statement, generationID, budget)
		if err != nil {
			return false, err
		}
		deleted, err := result.RowsAffected()
		if err != nil {
			return false, err
		}
		budget -= deleted
	}
	var remain bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM understanding_edge WHERE generation_id=?1)
		OR EXISTS(SELECT 1 FROM understanding_unit WHERE generation_id=?1)`, generationID).Scan(&remain); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return remain, nil
}

// PrunedUnderstandingGenerationsWithFacts lists pruned generations that still hold
// unit or edge rows: deletes a stopped daemon left unfinished. Oldest first.
func (ix *Index) PrunedUnderstandingGenerationsWithFacts() ([]int64, error) {
	rows, err := ix.db.Query(`SELECT generation.id FROM understanding_generation generation
		WHERE generation.status='complete' AND generation.facts_state='pruned'
		AND (EXISTS(SELECT 1 FROM understanding_edge WHERE generation_id=generation.id)
			OR EXISTS(SELECT 1 FROM understanding_unit WHERE generation_id=generation.id))
		ORDER BY generation.started_at,generation.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// PrunedUnderstandingGenerationExists reports whether a complete generation of this
// exact identity had its facts pruned. A scan that appends beside one is a re-measure
// of history, which the retention counters report.
func (ix *Index) PrunedUnderstandingGenerationExists(repositoryID, checkoutID, snapshotDigest, structuralSchema, analyzerBundleDigest, conventionState, conventionSourceDigest string) (bool, error) {
	var exists bool
	err := ix.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM understanding_generation
		WHERE repository_id=? AND checkout_id=? AND snapshot_digest=? AND structural_schema=?
		AND analyzer_bundle_digest=? AND convention_state=? AND convention_source_digest=?
		AND status='complete' AND facts_state='pruned')`, repositoryID, checkoutID, snapshotDigest,
		structuralSchema, analyzerBundleDigest, conventionState, conventionSourceDigest).Scan(&exists)
	return exists, err
}
