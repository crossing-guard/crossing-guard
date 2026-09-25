package changeenv

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"crossing-guard/codemap"
	"crossing-guard/store"
)

var ErrImpactCenterNotFound = errors.New("impact center not found")

type ImpactGeneration struct {
	ID                     int64  `json:"id"`
	SnapshotProtocol       string `json:"snapshot_protocol"`
	SnapshotDigest         string `json:"snapshot_digest"`
	StructuralSchema       string `json:"structural_schema"`
	AnalyzerBundleDigest   string `json:"analyzer_bundle_digest"`
	ConventionState        string `json:"convention_state"`
	ConventionSourceRef    string `json:"convention_source_ref,omitempty"`
	ConventionSourceDigest string `json:"convention_source_digest,omitempty"`
	ProducedAt             int64  `json:"produced_at"`
}

type ImpactNode struct {
	Kind       string `json:"kind"`
	Ref        string `json:"ref"`
	Direct     bool   `json:"direct"`
	Provenance string `json:"provenance"`
	SourceID   int64  `json:"source_id"`
}

type ImpactEdge struct {
	FromKind   string `json:"from_kind"`
	FromRef    string `json:"from_ref"`
	Relation   string `json:"relation"`
	ToKind     string `json:"to_kind"`
	ToRef      string `json:"to_ref"`
	Provenance string `json:"provenance"`
	SourcePath string `json:"source_path,omitempty"`
	SourceLine int    `json:"source_line,omitempty"`
	AnalyzerID string `json:"analyzer_id"`
	Evidence   string `json:"evidence"`
}

type ImpactView struct {
	State               string                            `json:"state"`
	Reason              string                            `json:"reason,omitempty"`
	Generation          *ImpactGeneration                 `json:"generation,omitempty"`
	Center              *ImpactNode                       `json:"center,omitempty"`
	NodePage            Page                              `json:"node_page"`
	Nodes               []ImpactNode                      `json:"nodes"`
	EdgePage            Page                              `json:"edge_page"`
	Edges               []ImpactEdge                      `json:"edges"`
	Coverage            []store.UnderstandingCoverage     `json:"coverage"`
	CandidateState      string                            `json:"candidate_state"`
	CandidateReason     string                            `json:"candidate_reason,omitempty"`
	CandidateAnalyzerID string                            `json:"candidate_analyzer_id,omitempty"`
	CandidatePage       Page                              `json:"candidate_page"`
	Candidates          []codemap.ResponsibilityCandidate `json:"candidates"`
}

func unavailableImpact(state, reason string) ImpactView {
	return ImpactView{State: state, Reason: reason, Nodes: []ImpactNode{}, Edges: []ImpactEdge{}, Coverage: []store.UnderstandingCoverage{}, NodePage: page(0, 0, 100), EdgePage: page(0, 0, 100), CandidateState: "unavailable", CandidateReason: "exact understanding generation unavailable", CandidatePage: page(0, 0, 100), Candidates: []codemap.ResponsibilityCandidate{}}
}

func buildImpact(ix *store.Index, repositoryID, checkoutID string, revision *store.ChangeRecord, opt ViewOptions) (ImpactView, error) {
	if revision == nil {
		return unavailableImpact("unavailable", "no Git revision snapshot was captured"), nil
	}
	if !strings.HasPrefix(revision.SnapshotDigest, "git-tree-v2-sha256:") {
		return unavailableImpact("stale_source", "revision uses historical git-scope-v1 identity; rebuild the snapshot and understanding generation"), nil
	}
	if opt.AnalyzerBundleDigest == "" {
		return unavailableImpact("unavailable", "current analyzer bundle identity was not supplied"), nil
	}
	generation, found, err := ix.UnderstandingForSnapshot(repositoryID, checkoutID,
		revision.SnapshotDigest, codemap.StructuralSchema, opt.AnalyzerBundleDigest, "none", "")
	if err != nil {
		return unavailableImpact("failed", "understanding lookup failed: "+err.Error()), nil
	}
	if !found {
		return classifyMissingImpact(ix, repositoryID, checkoutID, revision.SnapshotDigest, opt.AnalyzerBundleDigest), nil
	}
	if generation.Status != "complete" {
		return unavailableImpact("failed", generation.LimitationCode+": "+generation.Limitation), nil
	}
	allEdges, err := loadGenerationEdges(ix, generation.ID)
	if err != nil {
		return unavailableImpact("failed", "understanding edges unavailable: "+err.Error()), nil
	}
	direct := map[string]bool{}
	for _, item := range revision.Items {
		if item.Path != "" {
			direct[item.Path] = true
		}
	}
	knownNodes := map[string]ImpactNode{}
	for path := range direct {
		knownNodes["file\x00"+path] = ImpactNode{Kind: "file", Ref: path, Direct: true, Provenance: "observed", SourceID: revision.ID}
	}
	for _, edge := range allEdges {
		for _, endpoint := range [][2]string{{edge.FromKind, edge.FromRef}, {edge.ToKind, edge.ToRef}} {
			key := endpoint[0] + "\x00" + endpoint[1]
			if _, ok := knownNodes[key]; !ok {
				knownNodes[key] = ImpactNode{Kind: endpoint[0], Ref: endpoint[1], Provenance: "measured", SourceID: generation.ID}
			}
		}
	}
	var center *ImpactNode
	if opt.ImpactCenterKind != "" && opt.ImpactCenterRef != "" {
		resolved, ok := knownNodes[opt.ImpactCenterKind+"\x00"+opt.ImpactCenterRef]
		if !ok {
			return ImpactView{}, fmt.Errorf("%w: %s:%s", ErrImpactCenterNotFound, opt.ImpactCenterKind, opt.ImpactCenterRef)
		}
		center = &resolved
	}
	directPackages := map[string]bool{}
	for _, edge := range allEdges {
		if edge.Relation == "file_in_package" && direct[edge.FromRef] {
			directPackages[edge.ToRef] = true
		}
	}
	selected := make([]store.UnderstandingEdge, 0)
	for _, edge := range allEdges {
		atDirect := edge.FromKind == "file" && direct[edge.FromRef] || edge.ToKind == "file" && direct[edge.ToRef]
		reversePackageDependent := edge.Relation == "package_depends_on" && directPackages[edge.ToRef]
		atCenter := opt.ImpactCenterKind != "" && ((edge.FromKind == opt.ImpactCenterKind && edge.FromRef == opt.ImpactCenterRef) || (edge.ToKind == opt.ImpactCenterKind && edge.ToRef == opt.ImpactCenterRef))
		if atDirect || reversePackageDependent || atCenter {
			selected = append(selected, edge)
		}
	}
	nodeMap := map[string]ImpactNode{}
	for path := range direct {
		nodeMap["file\x00"+path] = ImpactNode{Kind: "file", Ref: path, Direct: true, Provenance: "observed", SourceID: revision.ID}
	}
	for _, edge := range selected {
		for _, endpoint := range [][2]string{{edge.FromKind, edge.FromRef}, {edge.ToKind, edge.ToRef}} {
			key := endpoint[0] + "\x00" + endpoint[1]
			if existing, ok := nodeMap[key]; !ok || !existing.Direct {
				nodeMap[key] = ImpactNode{Kind: endpoint[0], Ref: endpoint[1], Direct: endpoint[0] == "file" && direct[endpoint[1]], Provenance: "measured", SourceID: generation.ID}
			}
		}
	}
	nodes := make([]ImpactNode, 0, len(nodeMap))
	for _, node := range nodeMap {
		nodes = append(nodes, node)
	}
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Direct != nodes[j].Direct {
			return nodes[i].Direct
		}
		if nodes[i].Kind != nodes[j].Kind {
			return nodes[i].Kind < nodes[j].Kind
		}
		return nodes[i].Ref < nodes[j].Ref
	})
	edges := make([]ImpactEdge, 0, len(selected))
	for _, edge := range selected {
		edges = append(edges, ImpactEdge{FromKind: edge.FromKind, FromRef: edge.FromRef, Relation: edge.Relation, ToKind: edge.ToKind, ToRef: edge.ToRef, Provenance: edge.Provenance, SourcePath: edge.SourcePath, SourceLine: edge.SourceLine, AnalyzerID: edge.AnalyzerID, Evidence: edge.EvidenceDigest})
	}
	coverage, err := ix.UnderstandingCoverage(generation.ID)
	if err != nil {
		return unavailableImpact("failed", "understanding coverage unavailable: "+err.Error()), nil
	}
	state := "exact"
	for _, row := range coverage {
		if row.State == "partial" || row.State == "failed" {
			state = "partial"
			break
		}
	}
	limit := opt.ImpactLimit
	if limit <= 0 {
		limit = 100
	} else if limit > 100 {
		limit = 100
	}
	nodePageValues, nodePage := slice(nodes, opt.ImpactNodeOffset, limit)
	edgePageValues, edgePage := slice(edges, opt.ImpactEdgeOffset, limit)
	view := ImpactView{State: state, Generation: &ImpactGeneration{ID: generation.ID, SnapshotProtocol: generation.SnapshotProtocol, SnapshotDigest: generation.SnapshotDigest, StructuralSchema: generation.StructuralSchema, AnalyzerBundleDigest: generation.AnalyzerBundleDigest, ConventionState: generation.ConventionState, ConventionSourceRef: generation.ConventionSourceRef, ConventionSourceDigest: generation.ConventionSourceDigest, ProducedAt: generation.EndedAt}, Center: center, NodePage: nodePage, Nodes: nodePageValues, EdgePage: edgePage, Edges: edgePageValues, Coverage: coverage, Candidates: []codemap.ResponsibilityCandidate{}}
	buildImpactCandidates(ix, generation, center, coverage, opt, &view)
	return view, nil
}

func buildImpactCandidates(ix *store.Index, generation store.UnderstandingGeneration, center *ImpactNode, coverage []store.UnderstandingCoverage, opt ViewOptions, view *ImpactView) {
	limit := opt.ImpactCandidateLimit
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	view.CandidatePage = page(0, opt.ImpactCandidateOffset, limit)
	if center == nil || center.Kind != "file" {
		view.CandidateState = "unavailable"
		view.CandidateReason = "file center required"
		return
	}
	units, err := loadGenerationUnits(ix, generation.ID)
	if err != nil {
		view.CandidateState = "failed"
		view.CandidateReason = "understanding units unavailable: " + err.Error()
		return
	}
	stored := make([]codemap.StoredResponsibilityUnit, 0, len(units))
	for _, unit := range units {
		stored = append(stored, codemap.StoredResponsibilityUnit{Path: unit.Path, DescriptorJSON: unit.DescriptorJSON})
	}
	coverageFacts := make([]codemap.ResponsibilityCoverageFact, 0, len(coverage))
	for _, fact := range coverage {
		coverageFacts = append(coverageFacts, codemap.ResponsibilityCoverageFact{Family: fact.Family, State: fact.State, AnalyzerID: fact.AnalyzerID, Reason: fact.Reason})
	}
	result := codemap.ProjectResponsibility(center.Ref, stored, coverageFacts, opt.ImpactCandidateOffset, limit)
	view.CandidateState = result.State
	view.CandidateReason = result.Reason
	view.CandidateAnalyzerID = result.AnalyzerID
	view.Candidates = result.Rows
	if result.Page != nil {
		view.CandidatePage = page(result.Page.Total, result.Page.Offset, result.Page.Limit)
	}
}

func loadGenerationUnits(ix *store.Index, generationID int64) ([]store.UnderstandingUnit, error) {
	out := []store.UnderstandingUnit{}
	for offset := 0; ; offset += 1000 {
		units, total, err := ix.UnderstandingUnits(generationID, offset, 1000)
		if err != nil {
			return nil, err
		}
		out = append(out, units...)
		if len(out) >= total {
			return out, nil
		}
	}
}

func loadGenerationEdges(ix *store.Index, generationID int64) ([]store.UnderstandingEdge, error) {
	out := []store.UnderstandingEdge{}
	for offset := 0; ; offset += 1000 {
		edges, total, err := ix.UnderstandingEdges(generationID, "", "", offset, 1000)
		if err != nil {
			return nil, err
		}
		out = append(out, edges...)
		if len(out) >= total {
			return out, nil
		}
	}
}

func classifyMissingImpact(ix *store.Index, repositoryID, checkoutID, snapshotDigest, analyzerBundle string) ImpactView {
	generations, _, err := ix.UnderstandingGenerations(repositoryID, checkoutID, 1000)
	if err != nil {
		return unavailableImpact("failed", "understanding history unavailable: "+err.Error())
	}
	if len(generations) == 0 {
		return unavailableImpact("unavailable", "no understanding generation exists for this checkout")
	}
	for _, generation := range generations {
		if generation.SnapshotDigest == snapshotDigest && generation.AnalyzerBundleDigest == analyzerBundle && generation.ConventionState == "explicit" {
			return unavailableImpact("unavailable", "only an explicit-convention generation exists; rebuild without conventions for session impact")
		}
	}
	for _, generation := range generations {
		if generation.SnapshotDigest == snapshotDigest && generation.ConventionState == "none" && generation.AnalyzerBundleDigest != analyzerBundle {
			return unavailableImpact("stale_analyzer", "source matches but the compiled analyzer bundle changed; rebuild understanding")
		}
	}
	for _, generation := range generations {
		if generation.AnalyzerBundleDigest == analyzerBundle && generation.ConventionState == "none" && generation.SnapshotDigest != snapshotDigest {
			return unavailableImpact("stale_source", "understanding source does not match the selected revision; rebuild snapshot and understanding")
		}
	}
	return unavailableImpact("unavailable", fmt.Sprintf("no structural generation matches snapshot %s and analyzer bundle %s", snapshotDigest, analyzerBundle))
}
