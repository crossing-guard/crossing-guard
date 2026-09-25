package codemap

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

const (
	maxResponsibilityUnits  = 10000
	maxResponsibilityPage   = 100
	maxResponsibilityDetail = 50
	maxStructuralCenters    = 100
)

// ResponsibilityUnit is the persisted input to the pure comparison function.
// It deliberately contains no policy verdict or repository configuration.
type ResponsibilityUnit struct {
	Path       string
	Descriptor UnitDescriptor
}

// StoredResponsibilityUnit is the portable durable form consumed by the
// responsibility projection. Store adapters remain responsible for loading it.
type StoredResponsibilityUnit struct {
	Path           string
	DescriptorJSON string
}

// ResponsibilityCoverageFact is the small coverage vocabulary needed by the
// projection. It intentionally does not import a persistence model.
type ResponsibilityCoverageFact struct {
	Family     string
	State      string
	AnalyzerID string
	Reason     string
}

// ResponsibilityResult is a factual projection, not a duplicate judgment.
type ResponsibilityResult struct {
	State      string
	Reason     string
	AnalyzerID string
	Page       *ResponsibilityPage
	Rows       []ResponsibilityCandidate
}

type ResponsibilityDetail struct {
	Total    int      `json:"total"`
	Returned int      `json:"returned"`
	Values   []string `json:"values"`
	Exact    bool     `json:"exact"`
}

type ResponsibilityCount struct {
	Total    int  `json:"total"`
	Returned int  `json:"returned"`
	Exact    bool `json:"exact"`
}

type ResponsibilityBodyShape struct {
	Digest            string               `json:"digest"`
	NodeCount         int                  `json:"node_count"`
	LeftDeclarations  ResponsibilityDetail `json:"left_declarations"`
	RightDeclarations ResponsibilityDetail `json:"right_declarations"`
}

// ResponsibilityCandidate records the exact facts which caused two units to
// be returned. Shared dependencies are supporting context and never a trigger.
type ResponsibilityCandidate struct {
	LeftPath                   string                    `json:"left_path"`
	RightPath                  string                    `json:"right_path"`
	Language                   string                    `json:"language"`
	AnalyzerID                 string                    `json:"analyzer_id"`
	SharedDeclarations         ResponsibilityDetail      `json:"shared_declarations"`
	SharedBodyShapes           ResponsibilityCount       `json:"shared_body_shapes"`
	BodyShapes                 []ResponsibilityBodyShape `json:"body_shapes"`
	SharedInternalDependencies ResponsibilityDetail      `json:"shared_internal_dependencies"`
	SharedExternalDependencies ResponsibilityDetail      `json:"shared_external_dependencies"`
}

type ResponsibilityPage struct {
	Offset int `json:"offset"`
	Limit  int `json:"limit"`
	Total  int `json:"total"`
}

// StructuralMatchChange is one exact body-shape relationship added or removed for a
// selected center. It deliberately does not name the relationship a duplicate.
type StructuralMatchChange struct {
	OtherPath string `json:"other_path"`
	Digest    string `json:"digest"`
	NodeCount int    `json:"node_count"`
	State     string `json:"state"`
}

// StructuralMatchChanges is a bounded centered set difference. Work is linear in the
// two unit populations; it never constructs an all-pairs graph.
type StructuralMatchChanges struct {
	State      string                  `json:"state"`
	Reason     string                  `json:"reason,omitempty"`
	AnalyzerID string                  `json:"analyzer_id,omitempty"`
	Page       ResponsibilityPage      `json:"page"`
	Rows       []StructuralMatchChange `json:"rows"`
}

// StructuralMatchPopulationRow is one canonical file-pair/body-shape relationship
// added or removed around the selected changed-file population.
type StructuralMatchPopulationRow struct {
	CenterPath string `json:"center_path"`
	OtherPath  string `json:"other_path"`
	Digest     string `json:"digest"`
	NodeCount  int    `json:"node_count"`
	State      string `json:"state"`
}

// StructuralMatchPopulationChanges is the bounded set difference for several exact
// changed centers. A row is still a measured body-shape relationship, never a
// duplicate judgment.
type StructuralMatchPopulationChanges struct {
	State  string                         `json:"state"`
	Reason string                         `json:"reason,omitempty"`
	Page   ResponsibilityPage             `json:"page"`
	Rows   []StructuralMatchPopulationRow `json:"rows"`
}

// CompareStructuralMatches compares exact body-shape relationships around one file.
// Both populations must provide compatible responsibility coverage.
func CompareStructuralMatches(centerPath string, before, after []StoredResponsibilityUnit,
	beforeCoverage, afterCoverage []ResponsibilityCoverageFact, offset, limit int) StructuralMatchChanges {
	result := StructuralMatchChanges{Rows: []StructuralMatchChange{}}
	beforeSet, beforeAnalyzer, beforeState, beforeReason, err := centeredStructuralMatchSet(centerPath, before, beforeCoverage)
	if err != nil {
		result.State, result.Reason = "failed", err.Error()
		return result
	}
	afterSet, afterAnalyzer, afterState, afterReason, err := centeredStructuralMatchSet(centerPath, after, afterCoverage)
	if err != nil {
		result.State, result.Reason = "failed", err.Error()
		return result
	}
	if beforeAnalyzer == "" && afterAnalyzer == "" {
		result.State = "unavailable"
		result.Reason = "centered file has no analyzed unit in either snapshot"
		return result
	}
	if beforeAnalyzer != "" && afterAnalyzer != "" && beforeAnalyzer != afterAnalyzer {
		result.State = "failed"
		result.Reason = "structural match comparison requires one exact analyzer identity"
		return result
	}
	result.AnalyzerID = afterAnalyzer
	if result.AnalyzerID == "" {
		result.AnalyzerID = beforeAnalyzer
	}
	if beforeState == "unsupported" || afterState == "unsupported" {
		result.State = "unsupported"
		result.Reason = strings.TrimSpace(beforeReason + " " + afterReason)
		return result
	}
	result.State = "exact"
	if beforeState == "partial" || afterState == "partial" {
		result.State = "partial"
		result.Reason = strings.TrimSpace(beforeReason + " " + afterReason)
	}
	seen := map[string]bool{}
	for key, beforeFact := range beforeSet {
		seen[key] = true
		if _, found := afterSet[key]; !found {
			beforeFact.State = "removed"
			result.Rows = append(result.Rows, beforeFact)
		}
	}
	for key, afterFact := range afterSet {
		if !seen[key] {
			afterFact.State = "added"
			result.Rows = append(result.Rows, afterFact)
		}
	}
	sort.Slice(result.Rows, func(i, j int) bool {
		if result.Rows[i].OtherPath != result.Rows[j].OtherPath {
			return result.Rows[i].OtherPath < result.Rows[j].OtherPath
		}
		if result.Rows[i].Digest != result.Rows[j].Digest {
			return result.Rows[i].Digest < result.Rows[j].Digest
		}
		return result.Rows[i].State < result.Rows[j].State
	})
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 || limit > maxResponsibilityPage {
		limit = maxResponsibilityPage
	}
	result.Page = ResponsibilityPage{Offset: offset, Limit: limit, Total: len(result.Rows)}
	if offset >= len(result.Rows) {
		result.Rows = []StructuralMatchChange{}
		return result
	}
	end := offset + limit
	if end > len(result.Rows) {
		end = len(result.Rows)
	}
	result.Rows = result.Rows[offset:end]
	return result
}

// CompareStructuralMatchPopulation decodes each generation once, then compares
// centered relationships for an exact bounded changed-path population.
func CompareStructuralMatchPopulation(centerPaths []string, before, after []StoredResponsibilityUnit,
	beforeCoverage, afterCoverage []ResponsibilityCoverageFact, offset, limit int) StructuralMatchPopulationChanges {
	result := StructuralMatchPopulationChanges{State: "exact", Rows: []StructuralMatchPopulationRow{}}
	centers := uniqueSorted(centerPaths)
	if len(centers) > maxStructuralCenters {
		result.State, result.Reason = "failed", fmt.Sprintf("structural center population has %d paths; maximum is %d", len(centers), maxStructuralCenters)
		return result
	}
	beforeUnits, err := decodeResponsibilityPopulation(before)
	if err != nil {
		result.State, result.Reason = "failed", err.Error()
		return result
	}
	afterUnits, err := decodeResponsibilityPopulation(after)
	if err != nil {
		result.State, result.Reason = "failed", err.Error()
		return result
	}
	beforeRows, afterRows, state, reason, err := structuralPopulationSets(centers, beforeUnits, afterUnits,
		beforeCoverage, afterCoverage)
	if err != nil {
		result.State, result.Reason = "failed", err.Error()
		return result
	}
	result.State, result.Reason = state, reason
	result.Rows = structuralPopulationDifference(beforeRows, afterRows)
	result.Page, result.Rows = pageStructuralPopulation(result.Rows, offset, limit)
	return result
}

func structuralPopulationSets(centers []string, beforeUnits, afterUnits []ResponsibilityUnit,
	beforeCoverage, afterCoverage []ResponsibilityCoverageFact) (map[string]StructuralMatchPopulationRow,
	map[string]StructuralMatchPopulationRow, string, string, error) {
	beforeRows := map[string]StructuralMatchPopulationRow{}
	afterRows := map[string]StructuralMatchPopulationRow{}
	hasExact, hasIncomplete := false, false
	reasons := []string{}
	for _, center := range centers {
		beforeSet, beforeAnalyzer, beforeState, beforeReason, setErr := centeredStructuralMatchSetDecoded(center, beforeUnits, beforeCoverage)
		if setErr != nil {
			return nil, nil, "", "", setErr
		}
		afterSet, afterAnalyzer, afterState, afterReason, setErr := centeredStructuralMatchSetDecoded(center, afterUnits, afterCoverage)
		if setErr != nil {
			return nil, nil, "", "", setErr
		}
		if beforeAnalyzer != "" && afterAnalyzer != "" && beforeAnalyzer != afterAnalyzer {
			return nil, nil, "", "", fmt.Errorf("structural match comparison requires one exact analyzer identity per center")
		}
		if beforeAnalyzer == "" && afterAnalyzer == "" {
			continue
		}
		if beforeState == "unsupported" || afterState == "unsupported" {
			hasIncomplete = true
			reasons = append(reasons, strings.TrimSpace(beforeReason+" "+afterReason))
			continue
		}
		hasExact = true
		if beforeState == "partial" || afterState == "partial" {
			hasIncomplete = true
			reasons = append(reasons, strings.TrimSpace(beforeReason+" "+afterReason))
		}
		addStructuralPopulationRows(beforeRows, center, beforeSet)
		addStructuralPopulationRows(afterRows, center, afterSet)
	}
	state := "exact"
	if !hasExact && hasIncomplete {
		state = "unsupported"
	} else if hasIncomplete {
		state = "partial"
	}
	return beforeRows, afterRows, state, strings.TrimSpace(strings.Join(uniqueSorted(reasons), "; ")), nil
}

func addStructuralPopulationRows(destination map[string]StructuralMatchPopulationRow, center string,
	rows map[string]StructuralMatchChange) {
	for _, row := range rows {
		left, right := center, row.OtherPath
		if right < left {
			left, right = right, left
		}
		destination[left+"\x00"+right+"\x00"+row.Digest] = StructuralMatchPopulationRow{CenterPath: left,
			OtherPath: right, Digest: row.Digest, NodeCount: row.NodeCount}
	}
}

func structuralPopulationDifference(beforeRows, afterRows map[string]StructuralMatchPopulationRow) []StructuralMatchPopulationRow {
	rows := []StructuralMatchPopulationRow{}
	for key, row := range beforeRows {
		if _, found := afterRows[key]; !found {
			row.State = "removed"
			rows = append(rows, row)
		}
	}
	for key, row := range afterRows {
		if _, found := beforeRows[key]; !found {
			row.State = "added"
			rows = append(rows, row)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		left, right := rows[i], rows[j]
		if left.CenterPath != right.CenterPath {
			return left.CenterPath < right.CenterPath
		}
		if left.OtherPath != right.OtherPath {
			return left.OtherPath < right.OtherPath
		}
		if left.Digest != right.Digest {
			return left.Digest < right.Digest
		}
		return left.State < right.State
	})
	return rows
}

func pageStructuralPopulation(rows []StructuralMatchPopulationRow, offset, limit int) (ResponsibilityPage,
	[]StructuralMatchPopulationRow) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 || limit > maxResponsibilityPage {
		limit = maxResponsibilityPage
	}
	page := ResponsibilityPage{Offset: offset, Limit: limit, Total: len(rows)}
	if offset >= len(rows) {
		return page, []StructuralMatchPopulationRow{}
	}
	end := offset + limit
	if end > len(rows) {
		end = len(rows)
	}
	return page, rows[offset:end]
}

func centeredStructuralMatchSet(centerPath string, stored []StoredResponsibilityUnit,
	coverage []ResponsibilityCoverageFact) (map[string]StructuralMatchChange, string, string, string, error) {
	decoded, err := decodeResponsibilityPopulation(stored)
	if err != nil {
		return nil, "", "", "", err
	}
	return centeredStructuralMatchSetDecoded(centerPath, decoded, coverage)
}

func decodeResponsibilityPopulation(stored []StoredResponsibilityUnit) ([]ResponsibilityUnit, error) {
	if len(stored) > maxResponsibilityUnits {
		return nil, fmt.Errorf("responsibility comparison has %d units; maximum is %d", len(stored), maxResponsibilityUnits)
	}
	decoded := make([]ResponsibilityUnit, 0, len(stored))
	seenPaths := map[string]bool{}
	for _, item := range stored {
		unit, err := DecodeResponsibilityUnit(item.Path, item.DescriptorJSON)
		if err != nil {
			return nil, err
		}
		if seenPaths[unit.Path] {
			return nil, fmt.Errorf("responsibility comparison contains duplicate path %q", unit.Path)
		}
		seenPaths[unit.Path] = true
		decoded = append(decoded, unit)
	}
	return decoded, nil
}

func centeredStructuralMatchSetDecoded(centerPath string, decoded []ResponsibilityUnit,
	coverage []ResponsibilityCoverageFact) (map[string]StructuralMatchChange, string, string, string, error) {
	centerIndex := -1
	for index := range decoded {
		if decoded[index].Path == centerPath {
			centerIndex = index
			break
		}
	}
	if centerIndex < 0 {
		return map[string]StructuralMatchChange{}, "", "absent", "", nil
	}
	center := decoded[centerIndex]
	analyzerID := center.Descriptor.Identity.AnalyzerID
	coverageState, coverageReason := "", ""
	for _, fact := range coverage {
		if fact.Family == string(CapabilityResponsibilityFingerprint) && fact.AnalyzerID == analyzerID {
			coverageState, coverageReason = fact.State, fact.Reason
			break
		}
	}
	if coverageState == "" {
		return nil, "", "", "", fmt.Errorf("responsibility coverage is missing for centered analyzer %s", analyzerID)
	}
	if coverageState != "complete" && coverageState != "partial" && coverageState != "unsupported" {
		return nil, "", "", "", fmt.Errorf("responsibility fingerprint producer reported %s", coverageState)
	}
	out := map[string]StructuralMatchChange{}
	if coverageState == "unsupported" {
		return out, analyzerID, coverageState, coverageReason, nil
	}
	for _, right := range decoded {
		if right.Path == center.Path || right.Descriptor.Identity.AnalyzerID != analyzerID {
			continue
		}
		shapes, err := intersectBodyShapes(center, right)
		if err != nil {
			return nil, "", "", "", err
		}
		for _, shape := range shapes {
			key := right.Path + "\x00" + shape.Digest
			out[key] = StructuralMatchChange{OtherPath: right.Path, Digest: shape.Digest, NodeCount: shape.NodeCount}
		}
	}
	return out, analyzerID, coverageState, coverageReason, nil
}

// DecodeResponsibilityUnit is the single validation boundary for descriptors
// read from durable storage.
func DecodeResponsibilityUnit(storedPath, descriptorJSON string) (ResponsibilityUnit, error) {
	if err := validateResponsibilityPath(storedPath); err != nil {
		return ResponsibilityUnit{}, err
	}
	var descriptor UnitDescriptor
	if err := json.Unmarshal([]byte(descriptorJSON), &descriptor); err != nil {
		return ResponsibilityUnit{}, fmt.Errorf("decode responsibility descriptor for %s: %w", storedPath, err)
	}
	unit := ResponsibilityUnit{Path: storedPath, Descriptor: descriptor}
	if err := validateResponsibilityUnit(unit); err != nil {
		return ResponsibilityUnit{}, err
	}
	return unit, nil
}

// ProjectResponsibility is the single owner for translating durable
// descriptors and coverage into a centered responsibility-candidate page.
func ProjectResponsibility(centerPath string, stored []StoredResponsibilityUnit, coverage []ResponsibilityCoverageFact, offset, limit int) ResponsibilityResult {
	result := ResponsibilityResult{Rows: []ResponsibilityCandidate{}}
	centerIndex := -1
	for i := range stored {
		if stored[i].Path == centerPath {
			centerIndex = i
			break
		}
	}
	if centerIndex < 0 {
		result.State = "unavailable"
		result.Reason = "centered file has no analyzed unit"
		return result
	}
	center, err := DecodeResponsibilityUnit(stored[centerIndex].Path, stored[centerIndex].DescriptorJSON)
	if err != nil {
		result.State = "failed"
		result.Reason = err.Error()
		return result
	}
	result.AnalyzerID = center.Descriptor.Identity.AnalyzerID
	var selected *ResponsibilityCoverageFact
	for i := range coverage {
		if coverage[i].Family == string(CapabilityResponsibilityFingerprint) && coverage[i].AnalyzerID == result.AnalyzerID {
			selected = &coverage[i]
			break
		}
	}
	if selected == nil {
		result.State = "failed"
		result.Reason = "responsibility coverage is missing for centered analyzer " + result.AnalyzerID
		return result
	}
	if selected.State == "unsupported" || selected.State == "failed" {
		result.State = selected.State
		result.Reason = selected.Reason
		if result.Reason == "" {
			result.Reason = "responsibility fingerprint producer reported " + selected.State
		}
		return result
	}
	if selected.State != "complete" && selected.State != "partial" {
		result.State = "failed"
		result.Reason = "responsibility fingerprint producer reported unknown state " + selected.State
		return result
	}
	decoded := make([]ResponsibilityUnit, 0, len(stored))
	for _, item := range stored {
		unit, decodeErr := DecodeResponsibilityUnit(item.Path, item.DescriptorJSON)
		if decodeErr != nil {
			result.State = "failed"
			result.Reason = decodeErr.Error()
			return result
		}
		decoded = append(decoded, unit)
	}
	rows, page, err := ResponsibilityCandidates(center, decoded, offset, limit)
	if err != nil {
		result.State = "failed"
		result.Reason = err.Error()
		return result
	}
	result.State = "exact"
	if selected.State == "partial" {
		result.State = "partial"
		result.Reason = selected.Reason
		if result.Reason == "" {
			result.Reason = "one or more claimed files could not be analyzed"
		}
	}
	result.Page = &page
	result.Rows = rows
	return result
}

// ResponsibilityCandidates compares one center with a bounded set of persisted
// units. It returns path-ordered facts; it does not rank, score, or judge them.
func ResponsibilityCandidates(center ResponsibilityUnit, units []ResponsibilityUnit, offset, limit int) ([]ResponsibilityCandidate, ResponsibilityPage, error) {
	if len(units) > maxResponsibilityUnits {
		return nil, ResponsibilityPage{}, fmt.Errorf("responsibility comparison has %d units; maximum is %d", len(units), maxResponsibilityUnits)
	}
	if err := validateResponsibilityUnit(center); err != nil {
		return nil, ResponsibilityPage{}, fmt.Errorf("center: %w", err)
	}
	seen := make(map[string]bool, len(units))
	centerMatches := 0
	for _, unit := range units {
		if err := validateResponsibilityUnit(unit); err != nil {
			return nil, ResponsibilityPage{}, err
		}
		if seen[unit.Path] {
			return nil, ResponsibilityPage{}, fmt.Errorf("responsibility comparison contains duplicate path %q", unit.Path)
		}
		seen[unit.Path] = true
		if unit.Path == center.Path {
			centerMatches++
			if !sameResponsibilityUnit(center, unit) {
				return nil, ResponsibilityPage{}, fmt.Errorf("center %q disagrees with comparison population", center.Path)
			}
		}
	}
	if centerMatches != 1 {
		return nil, ResponsibilityPage{}, fmt.Errorf("center %q is absent from comparison population", center.Path)
	}

	rows := make([]ResponsibilityCandidate, 0)
	for _, right := range units {
		if right.Path == center.Path || right.Descriptor.Identity.AnalyzerID != center.Descriptor.Identity.AnalyzerID {
			continue
		}
		sharedDeclarations := intersectStrings(declarationNames(center), declarationNames(right))
		bodyShapes, err := intersectBodyShapes(center, right)
		if err != nil {
			return nil, ResponsibilityPage{}, err
		}
		if len(sharedDeclarations) == 0 && len(bodyShapes) == 0 {
			continue
		}
		bodyShapeTotal := len(bodyShapes)
		if len(bodyShapes) > maxResponsibilityDetail {
			bodyShapes = bodyShapes[:maxResponsibilityDetail]
		}
		rows = append(rows, ResponsibilityCandidate{
			LeftPath: center.Path, RightPath: right.Path,
			Language: center.Descriptor.Identity.Language, AnalyzerID: center.Descriptor.Identity.AnalyzerID,
			SharedDeclarations:         detail(sharedDeclarations),
			SharedBodyShapes:           ResponsibilityCount{Total: bodyShapeTotal, Returned: len(bodyShapes), Exact: len(bodyShapes) == bodyShapeTotal},
			BodyShapes:                 bodyShapes,
			SharedInternalDependencies: detail(intersectStrings(center.Descriptor.Structure.Imports, right.Descriptor.Structure.Imports)),
			SharedExternalDependencies: detail(intersectStrings(center.Descriptor.Structure.External, right.Descriptor.Structure.External)),
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].RightPath < rows[j].RightPath })
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 || limit > maxResponsibilityPage {
		limit = maxResponsibilityPage
	}
	if offset > len(rows) {
		offset = len(rows)
	}
	page := ResponsibilityPage{Offset: offset, Limit: limit, Total: len(rows)}
	if offset >= len(rows) {
		return []ResponsibilityCandidate{}, page, nil
	}
	end := offset + limit
	if end > len(rows) {
		end = len(rows)
	}
	return rows[offset:end], page, nil
}

func validateResponsibilityPath(path string) error {
	normalized := filepath.ToSlash(strings.TrimPrefix(path, "./"))
	if path == "" || normalized != path || filepath.IsAbs(path) || path == ".." || strings.HasPrefix(path, "../") {
		return fmt.Errorf("unsafe responsibility path %q", path)
	}
	return nil
}

func validateResponsibilityUnit(unit ResponsibilityUnit) error {
	if err := validateResponsibilityPath(unit.Path); err != nil {
		return err
	}
	if unit.Descriptor.Identity.Path != unit.Path {
		return fmt.Errorf("stored path %q disagrees with descriptor path %q", unit.Path, unit.Descriptor.Identity.Path)
	}
	if unit.Descriptor.Identity.AnalyzerID == "" {
		return fmt.Errorf("responsibility descriptor for %s has no analyzer identity", unit.Path)
	}
	shapeNodes := map[string]int{}
	for _, declaration := range unit.Descriptor.Symbol.Declarations {
		if declaration.Name == "" {
			return fmt.Errorf("responsibility descriptor for %s contains an unnamed declaration", unit.Path)
		}
		if (declaration.BodyShapeDigest == "") != (declaration.BodyShapeNodes == 0) || declaration.BodyShapeNodes < 0 {
			return fmt.Errorf("responsibility descriptor for %s contains malformed body shape for %s", unit.Path, declaration.Name)
		}
		if declaration.BodyShapeDigest != "" {
			if nodes, ok := shapeNodes[declaration.BodyShapeDigest]; ok && nodes != declaration.BodyShapeNodes {
				return fmt.Errorf("responsibility descriptor for %s has conflicting node counts for body-shape digest %q", unit.Path, declaration.BodyShapeDigest)
			}
			shapeNodes[declaration.BodyShapeDigest] = declaration.BodyShapeNodes
		}
	}
	return nil
}

func sameResponsibilityUnit(left, right ResponsibilityUnit) bool {
	leftJSON, leftErr := json.Marshal(left.Descriptor)
	rightJSON, rightErr := json.Marshal(right.Descriptor)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func declarationNames(unit ResponsibilityUnit) []string {
	out := make([]string, 0, len(unit.Descriptor.Symbol.Declarations))
	for _, declaration := range unit.Descriptor.Symbol.Declarations {
		out = append(out, declaration.Name)
	}
	return out
}

type bodyShapeSet struct {
	nodes int
	names []string
}

func bodyShapes(unit ResponsibilityUnit) map[string]bodyShapeSet {
	out := map[string]bodyShapeSet{}
	for _, declaration := range unit.Descriptor.Symbol.Declarations {
		if declaration.BodyShapeDigest == "" {
			continue
		}
		shape := out[declaration.BodyShapeDigest]
		shape.nodes = declaration.BodyShapeNodes
		shape.names = append(shape.names, declaration.Name)
		out[declaration.BodyShapeDigest] = shape
	}
	return out
}

func intersectBodyShapes(left, right ResponsibilityUnit) ([]ResponsibilityBodyShape, error) {
	leftShapes, rightShapes := bodyShapes(left), bodyShapes(right)
	digests := make([]string, 0)
	for digest := range leftShapes {
		if _, ok := rightShapes[digest]; ok {
			digests = append(digests, digest)
		}
	}
	sort.Strings(digests)
	out := make([]ResponsibilityBodyShape, 0, len(digests))
	for _, digest := range digests {
		if leftShapes[digest].nodes != rightShapes[digest].nodes {
			return nil, fmt.Errorf("body-shape digest %q has conflicting node counts %d and %d", digest, leftShapes[digest].nodes, rightShapes[digest].nodes)
		}
		out = append(out, ResponsibilityBodyShape{Digest: digest, NodeCount: leftShapes[digest].nodes, LeftDeclarations: detail(leftShapes[digest].names), RightDeclarations: detail(rightShapes[digest].names)})
	}
	return out, nil
}

func intersectStrings(left, right []string) []string {
	rightSet := make(map[string]bool, len(right))
	for _, value := range right {
		if value != "" {
			rightSet[value] = true
		}
	}
	seen := map[string]bool{}
	out := make([]string, 0)
	for _, value := range left {
		if value != "" && rightSet[value] && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func detail(values []string) ResponsibilityDetail {
	values = uniqueSorted(values)
	total := len(values)
	if len(values) > maxResponsibilityDetail {
		values = values[:maxResponsibilityDetail]
	}
	return ResponsibilityDetail{Total: total, Returned: len(values), Values: values, Exact: len(values) == total}
}

func uniqueSorted(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}
