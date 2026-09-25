package analyzermodule

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"crossing-guard/codemap"
)

const (
	maxProtocolLineBytes    = 2 << 20
	maxProtocolInputBytes   = 8 << 20
	maxProtocolOutputBytes  = 64 << 20
	maxProtocolStderrBytes  = 64 << 10
	maxProtocolRecords      = 120000
	maxProtocolReasonBytes  = 4096
	maxProtocolPaths        = 10000
	maxProtocolUnits        = 10000
	maxProtocolEdges        = 100000
	maxProtocolDeclarations = 4096
	moduleTerminationGrace  = 2 * time.Second
)

// ScanRequest is one exact bounded invocation supplied by the existing source owner.
type ScanRequest struct {
	RequestID        string
	SnapshotProtocol string
	SnapshotDigest   string
	CheckoutRoot     string
	Paths            []codemap.AnalyzerPath
}

// Result contains only facts from a clean, fully validated invocation. On any runner
// error all slices are empty; callers record failed coverage rather than partial output.
type Result struct {
	Units        []codemap.ModuleUnit
	Edges        []codemap.ModuleEdge
	Coverage     []codemap.ModuleCoverage
	Failures     []codemap.ModuleUnitFailure
	StderrBytes  int
	StderrDigest string
}

// Run executes one exact selected native module without a shell and accepts facts only
// after clean protocol completion and post-run package revalidation.
func Run(ctx context.Context, installed Package, request ScanRequest) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	verified, err := Inspect(installed.Dir)
	if err != nil || !samePackage(installed, verified) {
		return Result{}, fmt.Errorf("selected analyzer package changed before execution")
	}
	input, allowed, err := encodeInput(installed, request)
	if err != nil {
		return Result{}, err
	}
	entrypoint := filepath.Join(installed.Dir, filepath.FromSlash(installed.Manifest.Entrypoint))
	command := exec.Command(entrypoint)
	command.Dir = installed.Dir
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C", "CROSSING_GUARD_ANALYZER_PROTOCOL=" + codemap.AnalyzerProtocolV1}
	command.Stdin = bytes.NewReader(input)
	stdout := newBoundedBuffer(maxProtocolOutputBytes)
	stderr := newBoundedBuffer(maxProtocolStderrBytes)
	command.Stdout = stdout
	command.Stderr = stderr
	configureProcess(command)
	if err := command.Start(); err != nil {
		return Result{}, fmt.Errorf("start analyzer module %s: %w", installed.Manifest.ModuleID, err)
	}
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	select {
	case err = <-wait:
	case <-ctx.Done():
		terminateProcess(command.Process, moduleTerminationGrace)
		<-wait
		return Result{}, fmt.Errorf("analyzer module %s cancelled: %w", installed.Manifest.ModuleID, ctx.Err())
	}
	if err != nil {
		return Result{}, fmt.Errorf("analyzer module %s failed: %w", installed.Manifest.ModuleID, err)
	}
	if stdout.overflow {
		return Result{}, fmt.Errorf("analyzer module %s exceeded %d output bytes", installed.Manifest.ModuleID, maxProtocolOutputBytes)
	}
	if stderr.overflow {
		return Result{}, fmt.Errorf("analyzer module %s exceeded %d diagnostic bytes", installed.Manifest.ModuleID, maxProtocolStderrBytes)
	}
	postRun, inspectErr := Inspect(installed.Dir)
	if inspectErr != nil || !samePackage(installed, postRun) {
		return Result{}, fmt.Errorf("selected analyzer package changed during execution")
	}
	result, err := decodeOutput(installed, request.RequestID, allowed, stdout.Bytes())
	if err != nil {
		return Result{}, err
	}
	result.StderrBytes = stderr.total
	result.StderrDigest, err = digestReader(bytes.NewReader(stderr.Bytes()))
	if err != nil {
		return Result{}, fmt.Errorf("digest analyzer module diagnostics: %w", err)
	}
	return result, nil
}

func encodeInput(installed Package, request ScanRequest) ([]byte, map[string]codemap.AnalyzerPath, error) {
	if strings.TrimSpace(request.RequestID) == "" || strings.TrimSpace(request.SnapshotProtocol) == "" ||
		strings.TrimSpace(request.SnapshotDigest) == "" || strings.TrimSpace(request.CheckoutRoot) == "" {
		return nil, nil, fmt.Errorf("analyzer scan requires request, snapshot, and checkout identity")
	}
	if len(request.Paths) > maxProtocolPaths {
		return nil, nil, fmt.Errorf("analyzer scan has %d paths; maximum is %d", len(request.Paths), maxProtocolPaths)
	}
	extensions := map[string]bool{}
	for _, extension := range installed.Manifest.Extensions {
		extensions[extension] = true
	}
	paths := append([]codemap.AnalyzerPath(nil), request.Paths...)
	sort.Slice(paths, func(i, j int) bool { return paths[i].Path < paths[j].Path })
	allowed := map[string]codemap.AnalyzerPath{}
	var input bytes.Buffer
	encoder := json.NewEncoder(&input)
	start := codemap.AnalyzerInputRecord{Type: "scan_start", Start: &codemap.AnalyzerScanStart{
		ProtocolVersion: codemap.AnalyzerProtocolV1, RequestID: request.RequestID,
		SnapshotProtocol: request.SnapshotProtocol, SnapshotDigest: request.SnapshotDigest,
		CheckoutRoot: request.CheckoutRoot, PathLimit: maxProtocolPaths,
		UnitLimit: maxProtocolUnits, EdgeLimit: maxProtocolEdges}}
	if err := encoder.Encode(start); err != nil {
		return nil, nil, fmt.Errorf("encode analyzer scan start: %w", err)
	}
	for _, path := range paths {
		if !safeRelativePath(path.Path) || path.SourceHash == "" || path.Bytes < 0 || path.Lines < 0 {
			return nil, nil, fmt.Errorf("analyzer scan contains invalid path fact %q", path.Path)
		}
		extension := strings.ToLower(filepathExtension(path.Path))
		if !extensions[extension] {
			return nil, nil, fmt.Errorf("analyzer module %s does not claim %s", installed.Manifest.ModuleID, path.Path)
		}
		if _, duplicate := allowed[path.Path]; duplicate {
			return nil, nil, fmt.Errorf("analyzer scan contains duplicate path %q", path.Path)
		}
		allowed[path.Path] = path
		copy := path
		if err := encoder.Encode(codemap.AnalyzerInputRecord{Type: "path", Path: &copy}); err != nil {
			return nil, nil, fmt.Errorf("encode analyzer path: %w", err)
		}
	}
	if err := encoder.Encode(codemap.AnalyzerInputRecord{Type: "scan_end"}); err != nil {
		return nil, nil, fmt.Errorf("encode analyzer scan end: %w", err)
	}
	if input.Len() > maxProtocolInputBytes {
		return nil, nil, fmt.Errorf("analyzer protocol input exceeds %d bytes", maxProtocolInputBytes)
	}
	return input.Bytes(), allowed, nil
}

func decodeOutput(installed Package, requestID string, allowed map[string]codemap.AnalyzerPath, body []byte) (Result, error) {
	result := Result{Units: []codemap.ModuleUnit{}, Edges: []codemap.ModuleEdge{},
		Coverage: []codemap.ModuleCoverage{}, Failures: []codemap.ModuleUnitFailure{}}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 64<<10), maxProtocolLineBytes)
	recordNumber := 0
	started, ended := false, false
	coverageSeen := map[codemap.Capability]bool{}
	unitSeen := map[string]bool{}
	for scanner.Scan() {
		recordNumber++
		if recordNumber > maxProtocolRecords {
			return Result{}, fmt.Errorf("analyzer module %s exceeded %d protocol records", installed.Manifest.ModuleID, maxProtocolRecords)
		}
		decoder := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
		decoder.DisallowUnknownFields()
		var record codemap.AnalyzerOutputRecord
		if err := decoder.Decode(&record); err != nil {
			return Result{}, fmt.Errorf("decode analyzer module %s record %d: %w", installed.Manifest.ModuleID, recordNumber, err)
		}
		if err := requireJSONEOF(decoder); err != nil {
			return Result{}, fmt.Errorf("decode analyzer module %s record %d: %w", installed.Manifest.ModuleID, recordNumber, err)
		}
		if ended {
			return Result{}, fmt.Errorf("analyzer module %s emitted output after analysis_end", installed.Manifest.ModuleID)
		}
		switch record.Type {
		case "analysis_start":
			if started || recordNumber != 1 || record.ProtocolVersion != codemap.AnalyzerProtocolV1 ||
				record.RequestID != requestID || record.ModuleID != installed.Manifest.ModuleID ||
				record.AnalyzerIdentity != installed.Manifest.AnalyzerIdentity {
				return Result{}, fmt.Errorf("analyzer module %s emitted a mismatched analysis_start", installed.Manifest.ModuleID)
			}
			started = true
		case "unit":
			if !started || record.Unit == nil || !singlePayload(record) {
				return Result{}, fmt.Errorf("analyzer module %s emitted malformed unit record", installed.Manifest.ModuleID)
			}
			if err := validateModuleUnit(installed, allowed, *record.Unit); err != nil {
				return Result{}, err
			}
			if unitSeen[record.Unit.Path] {
				return Result{}, fmt.Errorf("analyzer module %s emitted duplicate unit %s", installed.Manifest.ModuleID, record.Unit.Path)
			}
			unitSeen[record.Unit.Path] = true
			result.Units = append(result.Units, *record.Unit)
		case "edge":
			if !started || record.Edge == nil || !singlePayload(record) {
				return Result{}, fmt.Errorf("analyzer module %s emitted malformed edge record", installed.Manifest.ModuleID)
			}
			if err := validateModuleEdge(installed, allowed, *record.Edge); err != nil {
				return Result{}, err
			}
			result.Edges = append(result.Edges, *record.Edge)
		case "coverage":
			if !started || record.Coverage == nil || !singlePayload(record) {
				return Result{}, fmt.Errorf("analyzer module %s emitted malformed coverage record", installed.Manifest.ModuleID)
			}
			if err := validateModuleCoverage(installed, *record.Coverage); err != nil {
				return Result{}, err
			}
			if coverageSeen[record.Coverage.Family] {
				return Result{}, fmt.Errorf("analyzer module %s emitted duplicate coverage %s", installed.Manifest.ModuleID, record.Coverage.Family)
			}
			coverageSeen[record.Coverage.Family] = true
			result.Coverage = append(result.Coverage, *record.Coverage)
		case "unit_failure":
			if !started || record.Failure == nil || !singlePayload(record) {
				return Result{}, fmt.Errorf("analyzer module %s emitted malformed unit_failure record", installed.Manifest.ModuleID)
			}
			if _, ok := allowed[record.Failure.Path]; !ok || !manifestID.MatchString(record.Failure.Code) ||
				len(record.Failure.Reason) > maxProtocolReasonBytes {
				return Result{}, fmt.Errorf("analyzer module %s emitted invalid failure fact", installed.Manifest.ModuleID)
			}
			result.Failures = append(result.Failures, *record.Failure)
		case "analysis_end":
			if !started || record.Unit != nil || record.Edge != nil || record.Coverage != nil || record.Failure != nil ||
				record.ProtocolVersion != codemap.AnalyzerProtocolV1 || record.RequestID != requestID ||
				record.ModuleID != installed.Manifest.ModuleID || record.AnalyzerIdentity != installed.Manifest.AnalyzerIdentity ||
				record.UnitTotal != len(result.Units) || record.EdgeTotal != len(result.Edges) ||
				record.FailureTotal != len(result.Failures) {
				return Result{}, fmt.Errorf("analyzer module %s emitted a mismatched analysis_end", installed.Manifest.ModuleID)
			}
			ended = true
		default:
			return Result{}, fmt.Errorf("analyzer module %s emitted unknown record type %q", installed.Manifest.ModuleID, record.Type)
		}
		if len(result.Units) > maxProtocolUnits || len(result.Edges) > maxProtocolEdges {
			return Result{}, fmt.Errorf("analyzer module %s exceeded unit/edge limits", installed.Manifest.ModuleID)
		}
	}
	if err := scanner.Err(); err != nil {
		return Result{}, fmt.Errorf("read analyzer module %s output: %w", installed.Manifest.ModuleID, err)
	}
	if !started || !ended {
		return Result{}, fmt.Errorf("analyzer module %s did not complete its protocol", installed.Manifest.ModuleID)
	}
	for _, capability := range installed.Manifest.Capabilities {
		if !coverageSeen[capability] {
			return Result{}, fmt.Errorf("analyzer module %s omitted claimed coverage %s", installed.Manifest.ModuleID, capability)
		}
	}
	if err := validateResolvedSymbols(installed, result); err != nil {
		return Result{}, err
	}
	return result, nil
}

func validateModuleUnit(installed Package, allowed map[string]codemap.AnalyzerPath, unit codemap.ModuleUnit) error {
	path, ok := allowed[unit.Path]
	if !ok || !contains(installed.Manifest.Languages, unit.Language) || unit.LOC < 0 {
		return fmt.Errorf("analyzer module %s emitted invalid unit %q", installed.Manifest.ModuleID, unit.Path)
	}
	if len(unit.Declarations) > maxProtocolDeclarations {
		return fmt.Errorf("analyzer module %s emitted too many declarations for %s", installed.Manifest.ModuleID, unit.Path)
	}
	seen := map[string]bool{}
	for _, declaration := range unit.Declarations {
		if declaration.Identity == "" || declaration.Name == "" || declaration.Kind == "" || seen[declaration.Identity] ||
			declaration.Line <= 0 || declaration.EndLine < declaration.Line || declaration.EndLine > path.Lines ||
			declaration.StartByte < 0 || declaration.EndByte <= declaration.StartByte || declaration.EndByte > path.Bytes ||
			(declaration.Cyclomatic != nil && *declaration.Cyclomatic < 0) || declaration.BodyShapeNodes < 0 {
			return fmt.Errorf("analyzer module %s emitted invalid declaration for %s", installed.Manifest.ModuleID, unit.Path)
		}
		if declaration.BodyShapeDigest != "" && !hasAlgorithm(installed.Manifest.BodyShapeAlgorithms, declaration.BodyShapeDigest) {
			return fmt.Errorf("analyzer module %s emitted undeclared body-shape algorithm", installed.Manifest.ModuleID)
		}
		seen[declaration.Identity] = true
	}
	return nil
}

func validateModuleEdge(installed Package, allowed map[string]codemap.AnalyzerPath, edge codemap.ModuleEdge) error {
	path, ok := allowed[edge.SourcePath]
	if !ok || edge.SourceLine < 0 || edge.SourceLine > path.Lines || edge.FromRef == "" || edge.ToRef == "" {
		return fmt.Errorf("analyzer module %s emitted invalid edge source", installed.Manifest.ModuleID)
	}
	valid := (edge.Relation == "symbol_calls_symbol" && edge.FromKind == "symbol" && edge.ToKind == "symbol") ||
		(edge.Relation == "file_references_symbol" && edge.FromKind == "file" && edge.ToKind == "symbol")
	if !valid {
		return fmt.Errorf("analyzer module %s emitted unsupported edge relation %q", installed.Manifest.ModuleID, edge.Relation)
	}
	if edge.FromKind == "file" && edge.FromRef != edge.SourcePath {
		return fmt.Errorf("analyzer module %s emitted file reference from a different path", installed.Manifest.ModuleID)
	}
	return nil
}

func validateModuleCoverage(installed Package, coverage codemap.ModuleCoverage) error {
	if !containsCapability(installed.Manifest.Capabilities, coverage.Family) ||
		(coverage.State != "complete" && coverage.State != "partial" && coverage.State != "failed") ||
		coverage.Attempted < 0 || coverage.Produced < 0 || coverage.Errors < 0 ||
		coverage.Unresolved < 0 || coverage.Ambiguous < 0 || len(coverage.Reason) > maxProtocolReasonBytes {
		return fmt.Errorf("analyzer module %s emitted invalid coverage %s", installed.Manifest.ModuleID, coverage.Family)
	}
	if coverage.State == "complete" && (coverage.Errors != 0 || coverage.Unresolved != 0 || coverage.Ambiguous != 0) {
		return fmt.Errorf("analyzer module %s called incomplete coverage complete", installed.Manifest.ModuleID)
	}
	if coverage.State == "failed" && coverage.Produced != 0 {
		return fmt.Errorf("analyzer module %s failed coverage cannot report produced facts", installed.Manifest.ModuleID)
	}
	return nil
}

func validateResolvedSymbols(installed Package, result Result) error {
	declared := map[string]bool{}
	prefix := installed.Manifest.AnalyzerIdentity + "::"
	for _, unit := range result.Units {
		for _, declaration := range unit.Declarations {
			declared[prefix+declaration.Identity] = true
		}
	}
	for _, edge := range result.Edges {
		if !strings.HasPrefix(edge.ToRef, prefix) || !declared[edge.ToRef] {
			return fmt.Errorf("analyzer module %s emitted call edge to undeclared symbol %q", installed.Manifest.ModuleID, edge.ToRef)
		}
		if edge.FromKind == "symbol" && (!strings.HasPrefix(edge.FromRef, prefix) || !declared[edge.FromRef]) {
			return fmt.Errorf("analyzer module %s emitted call edge from undeclared symbol %q", installed.Manifest.ModuleID, edge.FromRef)
		}
	}
	return nil
}

func singlePayload(record codemap.AnalyzerOutputRecord) bool {
	count := 0
	for _, present := range []bool{record.Unit != nil, record.Edge != nil, record.Coverage != nil, record.Failure != nil} {
		if present {
			count++
		}
	}
	return count == 1
}

func samePackage(left, right Package) bool {
	return left.PackageDigest == right.PackageDigest && left.EntrypointDigest == right.EntrypointDigest &&
		left.Manifest.ModuleID == right.Manifest.ModuleID && left.Manifest.AnalyzerIdentity == right.Manifest.AnalyzerIdentity
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func containsCapability(values []codemap.Capability, wanted codemap.Capability) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func hasAlgorithm(algorithms []string, digest string) bool {
	for _, algorithm := range algorithms {
		if strings.HasPrefix(digest, algorithm+":") {
			return true
		}
	}
	return false
}

type boundedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	total    int
	overflow bool
}

func newBoundedBuffer(limit int) *boundedBuffer { return &boundedBuffer{limit: limit} }

func (buffer *boundedBuffer) Write(body []byte) (int, error) {
	buffer.total += len(body)
	remaining := buffer.limit - buffer.buffer.Len()
	if remaining <= 0 {
		buffer.overflow = true
		return len(body), nil
	}
	if len(body) > remaining {
		buffer.overflow = true
		_, _ = buffer.buffer.Write(body[:remaining])
		return len(body), nil
	}
	_, _ = buffer.buffer.Write(body)
	return len(body), nil
}

func (buffer *boundedBuffer) Bytes() []byte { return buffer.buffer.Bytes() }

func filepathExtension(path string) string {
	index := strings.LastIndex(path, ".")
	if index < 0 {
		return ""
	}
	return path[index:]
}

var _ io.Writer = (*boundedBuffer)(nil)
