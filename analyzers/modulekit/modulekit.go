// Package modulekit is an optional author-side helper for the public analyzer stream
// protocol. Core scan, store, and daemon packages never import it. A third-party module
// may emit the documented NDJSON directly instead.
package modulekit

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"crossing-guard/codemap"
)

// Definition is the compiled identity of a native reference module.
type Definition struct {
	ModuleID         string
	AnalyzerIdentity string
	Languages        []string
	Extensions       []string
	Capabilities     []codemap.Capability
}

// Analysis contains mechanics for one exact source plus accepted fact counts and
// honest limitations. The protocol helper aggregates these into one coverage record.
type Analysis struct {
	Unit       *codemap.ModuleUnit
	Failure    *codemap.ModuleUnitFailure
	Edges      []codemap.ModuleEdge
	Produced   map[codemap.Capability]int
	Errors     map[codemap.Capability]int
	Unresolved map[codemap.Capability]int
	Ambiguous  map[codemap.Capability]int
	Reasons    map[codemap.Capability]string
}

// Source is one source file already verified against the core-supplied digest.
type Source struct {
	Path string
	Body []byte
}

// AnalyzeFunc analyzes the complete claimed source set once so a module can resolve
// exact relationships across files. Returned analyses must contain one path each.
type AnalyzeFunc func([]Source) ([]Analysis, error)

// Main owns one complete stdin/stdout protocol invocation and returns a process exit
// code. It writes source-free diagnostics to stderr and never logs source content.
func Main(definition Definition, analyze AnalyzeFunc) int {
	request, err := readRequest(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "analyzer protocol:", err)
		return 1
	}
	encoder := json.NewEncoder(os.Stdout)
	emit := func(record codemap.AnalyzerOutputRecord) error { return encoder.Encode(record) }
	identity := codemap.AnalyzerOutputRecord{Type: "analysis_start", ProtocolVersion: codemap.AnalyzerProtocolV1,
		RequestID: request.start.RequestID, ModuleID: definition.ModuleID, AnalyzerIdentity: definition.AnalyzerIdentity}
	if err := emit(identity); err != nil {
		return 1
	}
	coverage := map[codemap.Capability]*codemap.ModuleCoverage{}
	for _, capability := range definition.Capabilities {
		coverage[capability] = &codemap.ModuleCoverage{Family: capability, State: "complete", Attempted: len(request.paths)}
	}
	unitTotal, edgeTotal, failureTotal := 0, 0, 0
	verifiedSources := make([]Source, 0, len(request.paths))
	for _, fact := range request.paths {
		source, readErr := os.ReadFile(filepath.Join(request.start.CheckoutRoot, filepath.FromSlash(fact.Path)))
		if readErr == nil {
			hash := sha256.Sum256(source)
			if "sha256-v1:"+hex.EncodeToString(hash[:]) != fact.SourceHash || int64(len(source)) != fact.Bytes {
				readErr = fmt.Errorf("source identity changed")
			}
		}
		if readErr != nil {
			if err := emit(codemap.AnalyzerOutputRecord{Type: "unit_failure", Failure: &codemap.ModuleUnitFailure{
				Path: fact.Path, Code: "source_unavailable", Reason: readErr.Error()}}); err != nil {
				return 1
			}
			failureTotal++
			for _, row := range coverage {
				row.Errors++
			}
			continue
		}
		verifiedSources = append(verifiedSources, Source{Path: fact.Path, Body: source})
	}
	analyses, analyzeErr := analyze(verifiedSources)
	if analyzeErr != nil {
		fmt.Fprintln(os.Stderr, "analyzer batch:", analyzeErr)
		return 1
	}
	sort.Slice(analyses, func(i, j int) bool { return analysisPath(analyses[i]) < analysisPath(analyses[j]) })
	verified := make(map[string]bool, len(verifiedSources))
	for _, source := range verifiedSources {
		verified[source.Path] = true
	}
	returned := make(map[string]bool, len(analyses))
	for _, analysis := range analyses {
		path := analysisPath(analysis)
		if !verified[path] || returned[path] || (analysis.Unit != nil && analysis.Failure != nil) {
			fmt.Fprintln(os.Stderr, "analyzer batch: results must contain exactly one unit or failure for each verified source")
			return 1
		}
		returned[path] = true
		if analysis.Failure != nil {
			if err := emit(codemap.AnalyzerOutputRecord{Type: "unit_failure", Failure: analysis.Failure}); err != nil {
				return 1
			}
			failureTotal++
		}
		if analysis.Unit != nil {
			if err := emit(codemap.AnalyzerOutputRecord{Type: "unit", Unit: analysis.Unit}); err != nil {
				return 1
			}
			unitTotal++
		}
		if analysis.Unit == nil && analysis.Failure == nil {
			fmt.Fprintln(os.Stderr, "analyzer batch: result has neither unit nor failure")
			return 1
		}
		sort.Slice(analysis.Edges, func(i, j int) bool {
			left, right := analysis.Edges[i], analysis.Edges[j]
			return edgeKey(left) < edgeKey(right)
		})
		for index := range analysis.Edges {
			if err := emit(codemap.AnalyzerOutputRecord{Type: "edge", Edge: &analysis.Edges[index]}); err != nil {
				return 1
			}
			edgeTotal++
		}
		for family, count := range analysis.Produced {
			if coverage[family] != nil {
				coverage[family].Produced += count
			}
		}
		for family, count := range analysis.Errors {
			if coverage[family] != nil {
				coverage[family].Errors += count
			}
		}
		for family, count := range analysis.Unresolved {
			if coverage[family] != nil {
				coverage[family].Unresolved += count
			}
		}
		for family, count := range analysis.Ambiguous {
			if coverage[family] != nil {
				coverage[family].Ambiguous += count
			}
		}
		for family, reason := range analysis.Reasons {
			if coverage[family] != nil && coverage[family].Reason == "" {
				coverage[family].Reason = reason
			}
		}
	}
	if len(returned) != len(verified) {
		fmt.Fprintln(os.Stderr, "analyzer batch: result omitted one or more verified sources")
		return 1
	}
	for _, capability := range definition.Capabilities {
		row := coverage[capability]
		if row.Errors > 0 || row.Unresolved > 0 || row.Ambiguous > 0 {
			row.State = "partial"
			if row.Reason == "" {
				row.Reason = "one or more source facts could not be parsed or resolved"
			}
		}
		if err := emit(codemap.AnalyzerOutputRecord{Type: "coverage", Coverage: row}); err != nil {
			return 1
		}
	}
	identity.Type = "analysis_end"
	identity.UnitTotal, identity.EdgeTotal, identity.FailureTotal = unitTotal, edgeTotal, failureTotal
	if err := emit(identity); err != nil {
		return 1
	}
	return 0
}

func analysisPath(analysis Analysis) string {
	if analysis.Unit != nil {
		return analysis.Unit.Path
	}
	if analysis.Failure != nil {
		return analysis.Failure.Path
	}
	return ""
}

type request struct {
	start codemap.AnalyzerScanStart
	paths []codemap.AnalyzerPath
}

func readRequest(reader io.Reader) (request, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	var out request
	ended := false
	seen := map[string]bool{}
	for scanner.Scan() {
		decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
		decoder.DisallowUnknownFields()
		var record codemap.AnalyzerInputRecord
		if err := decoder.Decode(&record); err != nil {
			return request{}, err
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return request{}, fmt.Errorf("one JSON value is required per line")
		}
		switch record.Type {
		case "scan_start":
			if out.start.RequestID != "" || record.Start == nil || record.Start.ProtocolVersion != codemap.AnalyzerProtocolV1 {
				return request{}, fmt.Errorf("invalid scan_start")
			}
			out.start = *record.Start
		case "path":
			if out.start.RequestID == "" || ended || record.Path == nil || seen[record.Path.Path] {
				return request{}, fmt.Errorf("invalid path record")
			}
			seen[record.Path.Path] = true
			out.paths = append(out.paths, *record.Path)
		case "scan_end":
			if out.start.RequestID == "" || ended || record.Start != nil || record.Path != nil {
				return request{}, fmt.Errorf("invalid scan_end")
			}
			ended = true
		default:
			return request{}, fmt.Errorf("unknown input record %q", record.Type)
		}
	}
	if err := scanner.Err(); err != nil {
		return request{}, err
	}
	if out.start.RequestID == "" || !ended || len(out.paths) > out.start.PathLimit ||
		strings.TrimSpace(out.start.CheckoutRoot) == "" || strings.TrimSpace(out.start.SnapshotDigest) == "" {
		return request{}, fmt.Errorf("incomplete or over-limit analyzer request")
	}
	return out, nil
}

func edgeKey(edge codemap.ModuleEdge) string {
	return edge.FromKind + "\x00" + edge.FromRef + "\x00" + edge.Relation + "\x00" + edge.ToKind + "\x00" + edge.ToRef + fmt.Sprintf("\x00%09d", edge.SourceLine)
}
