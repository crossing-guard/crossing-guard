package engine

import (
	_ "embed"
	"os"
)

// detectorsDefault is the standard detection library, shipped inside the binary.
// It is the base layer; a user overlay (LoadLayered) merges on top of it by id.
// Editing this file changes what ships; editing the user overlay changes one
// install without a rebuild.
//
//go:embed detectors.default.json
var detectorsDefault []byte

// detectorsStructural is the mechanism-first baseline. Its definitions are repeated
// in the complete starter snapshot deliberately and parity-tested: selecting a complete
// snapshot must pin every active detector byte, while the unselected floor remains a
// separately reviewable framework asset.
//
//go:embed detectors.structural.json
var detectorsStructural []byte

// DefaultDetectors returns the embedded standard library, compiled. This is what
// runs when no user overlay is present — the engine is never silent by default.
func DefaultDetectors() ([]Detector, error) {
	return parseDetectors(detectorsDefault)
}

// DefaultDetectorsJSON returns a copy of the raw embedded standard-library JSON —
// used by `crossing-guard detectors init` to materialize an editable overlay seeded from the
// shipped default.
func DefaultDetectorsJSON() []byte {
	out := make([]byte, len(detectorsDefault))
	copy(out, detectorsDefault)
	return out
}

// StructuralDetectors returns only the portable, non-policy action/tool floor.
func StructuralDetectors() ([]Detector, error) {
	return parseDetectors(detectorsStructural)
}

// StructuralDetectorsJSON returns a copy of the framework floor document.
func StructuralDetectorsJSON() []byte {
	out := make([]byte, len(detectorsStructural))
	copy(out, detectorsStructural)
	return out
}

// LoadLayered returns the embedded standard library merged with a user overlay.
//
//   - a user detector whose id matches a shipped one REPLACES it (edit in place),
//   - a user detector with a new id is ADDED,
//   - "disabled": true on an id REMOVES that detector from the effective set.
//
// An absent overlay file is not an error: the caller gets the embedded default
// (that is the "ships with the code, editable when you want" contract). A present
// but malformed overlay IS an error — a broken edit should be loud, not silently
// ignored back to the default.
func LoadLayered(userPath string) ([]Detector, error) {
	base, err := DefaultDetectors()
	if err != nil {
		return nil, err
	}
	if userPath == "" {
		return base, nil
	}
	b, err := os.ReadFile(userPath)
	if err != nil {
		if os.IsNotExist(err) {
			return base, nil // no overlay yet — ship the default
		}
		return nil, err
	}
	overlay, err := parseDetectors(b)
	if err != nil {
		return nil, err
	}
	return mergeByID(base, overlay), nil
}

// mergeByID overlays user detectors onto the base, keyed by id. Base order is
// preserved; replacements keep the base slot; new ids append in overlay order;
// a Disabled entry drops the id from the result. The engine's dedup is by tag
// identity, but the CONFIG's identity is the detector id, so that is the merge key.
func mergeByID(base, overlay []Detector) []Detector {
	pos := make(map[string]int, len(base))
	out := make([]Detector, len(base))
	copy(out, base)
	for i, d := range out {
		pos[d.ID] = i
	}
	disabled := map[string]bool{}
	for _, d := range overlay {
		if d.Disabled {
			disabled[d.ID] = true
			continue
		}
		if i, ok := pos[d.ID]; ok {
			out[i] = d // replace in place
			continue
		}
		pos[d.ID] = len(out)
		out = append(out, d)
	}
	if len(disabled) == 0 {
		return out
	}
	kept := out[:0]
	for _, d := range out {
		if !disabled[d.ID] {
			kept = append(kept, d)
		}
	}
	return kept
}

// securityObserveDetectors is an opt-in complete document, not a default overlay.
//
//go:embed detectors.security-observe.json
var securityObserveDetectors []byte

// SecurityObserveDetectors returns portable facts plus heuristic credential and
// destination facts for the separately selected security observation rulebook.
func SecurityObserveDetectors() ([]Detector, error) {
	return parseDetectors(securityObserveDetectors)
}
