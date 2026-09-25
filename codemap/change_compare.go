package codemap

import (
	"fmt"
	"sort"
)

const maxUnitChangeDeclarations = 4096

// IntegerChange is one optional measured integer before and after a source change.
// Delta is present only when both sides are measured.
type IntegerChange struct {
	Before *int `json:"before,omitempty"`
	After  *int `json:"after,omitempty"`
	Delta  *int `json:"delta,omitempty"`
}

// DeclarationChange is an exact adapter-produced declaration comparison. State is
// mechanical: added, removed, modified, moved, or unchanged.
type DeclarationChange struct {
	Identity        string        `json:"identity"`
	BeforeName      string        `json:"before_name,omitempty"`
	AfterName       string        `json:"after_name,omitempty"`
	Kind            string        `json:"kind,omitempty"`
	State           string        `json:"state"`
	BeforeLine      int           `json:"before_line,omitempty"`
	AfterLine       int           `json:"after_line,omitempty"`
	BeforeEndLine   int           `json:"before_end_line,omitempty"`
	AfterEndLine    int           `json:"after_end_line,omitempty"`
	SourceSpanLines IntegerChange `json:"source_span_lines"`
	Cyclomatic      IntegerChange `json:"cyclomatic"`
}

// UnitChange is a pure comparison of at most two exact unit descriptors. It contains
// no threshold, score, role judgment, or causal attribution.
type UnitChange struct {
	Path             string              `json:"path"`
	State            string              `json:"state"`
	BeforeSourceHash string              `json:"before_source_hash,omitempty"`
	AfterSourceHash  string              `json:"after_source_hash,omitempty"`
	Declarations     []DeclarationChange `json:"declarations"`
}

// CompareUnits compares one path across two descriptors produced by the same adapter.
// A nil side represents an added or removed analyzed unit.
func CompareUnits(before, after *UnitDescriptor) (UnitChange, error) {
	if before == nil && after == nil {
		return UnitChange{}, fmt.Errorf("unit comparison requires at least one descriptor")
	}
	path := descriptorPath(before, after)
	if before != nil && after != nil {
		if before.Identity.Path != after.Identity.Path {
			return UnitChange{}, fmt.Errorf("unit comparison path mismatch %q != %q", before.Identity.Path, after.Identity.Path)
		}
		if before.Identity.AnalyzerID == "" || before.Identity.AnalyzerID != after.Identity.AnalyzerID {
			return UnitChange{}, fmt.Errorf("unit comparison for %s requires one exact analyzer identity", path)
		}
	}
	beforeDeclarations, err := indexedDeclarations(path, before)
	if err != nil {
		return UnitChange{}, err
	}
	afterDeclarations, err := indexedDeclarations(path, after)
	if err != nil {
		return UnitChange{}, err
	}
	identities := make([]string, 0, len(beforeDeclarations)+len(afterDeclarations))
	seen := map[string]bool{}
	for identity := range beforeDeclarations {
		seen[identity] = true
		identities = append(identities, identity)
	}
	for identity := range afterDeclarations {
		if !seen[identity] {
			identities = append(identities, identity)
		}
	}
	sort.Strings(identities)
	change := UnitChange{Path: path, State: unitState(before, after), Declarations: make([]DeclarationChange, 0, len(identities))}
	if before != nil {
		change.BeforeSourceHash = before.SourceHash
	}
	if after != nil {
		change.AfterSourceHash = after.SourceHash
	}
	for _, identity := range identities {
		beforeDeclaration, beforeOK := beforeDeclarations[identity]
		afterDeclaration, afterOK := afterDeclarations[identity]
		change.Declarations = append(change.Declarations, compareDeclaration(identity, beforeDeclaration, beforeOK, afterDeclaration, afterOK))
	}
	return change, nil
}

func descriptorPath(before, after *UnitDescriptor) string {
	if before != nil {
		return before.Identity.Path
	}
	return after.Identity.Path
}

func indexedDeclarations(path string, descriptor *UnitDescriptor) (map[string]Declaration, error) {
	out := map[string]Declaration{}
	if descriptor == nil {
		return out, nil
	}
	if descriptor.Identity.Path == "" || descriptor.Identity.AnalyzerID == "" {
		return nil, fmt.Errorf("unit comparison descriptor for %q has incomplete identity", path)
	}
	if len(descriptor.Symbol.Declarations) > maxUnitChangeDeclarations {
		return nil, fmt.Errorf("unit comparison for %s has %d declarations; maximum is %d", path, len(descriptor.Symbol.Declarations), maxUnitChangeDeclarations)
	}
	for _, declaration := range descriptor.Symbol.Declarations {
		if declaration.Identity == "" || declaration.Name == "" || declaration.Kind == "" || declaration.SourceDigest == "" {
			return nil, fmt.Errorf("unit comparison for %s contains an incomplete declaration fact", path)
		}
		if _, duplicate := out[declaration.Identity]; duplicate {
			return nil, fmt.Errorf("unit comparison for %s contains duplicate declaration identity %q", path, declaration.Identity)
		}
		if declaration.SourceSpanLines != nil && *declaration.SourceSpanLines <= 0 {
			return nil, fmt.Errorf("unit comparison for %s contains invalid source span for %q", path, declaration.Identity)
		}
		if declaration.Cyclomatic != nil && *declaration.Cyclomatic < 0 {
			return nil, fmt.Errorf("unit comparison for %s contains invalid cyclomatic fact for %q", path, declaration.Identity)
		}
		out[declaration.Identity] = declaration
	}
	return out, nil
}

func unitState(before, after *UnitDescriptor) string {
	if before == nil {
		return "added"
	}
	if after == nil {
		return "removed"
	}
	if before.SourceHash == after.SourceHash {
		return "unchanged"
	}
	return "modified"
}

func compareDeclaration(identity string, before Declaration, beforeOK bool, after Declaration, afterOK bool) DeclarationChange {
	change := DeclarationChange{Identity: identity, BeforeName: before.Name, AfterName: after.Name,
		Kind: after.Kind, BeforeLine: before.Line, AfterLine: after.Line,
		BeforeEndLine: before.EndLine, AfterEndLine: after.EndLine,
		SourceSpanLines: integerChange(before.SourceSpanLines, after.SourceSpanLines),
		Cyclomatic:      integerChange(before.Cyclomatic, after.Cyclomatic)}
	if !afterOK {
		change.Kind = before.Kind
		change.State = "removed"
		return change
	}
	if !beforeOK {
		change.State = "added"
		return change
	}
	if before.SourceDigest != after.SourceDigest {
		change.State = "modified"
		return change
	}
	if before.Line != after.Line || before.EndLine != after.EndLine {
		change.State = "moved"
		return change
	}
	change.State = "unchanged"
	return change
}

func integerChange(before, after *int) IntegerChange {
	change := IntegerChange{Before: copyInteger(before), After: copyInteger(after)}
	if before != nil && after != nil {
		delta := *after - *before
		change.Delta = &delta
	}
	return change
}

func copyInteger(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
