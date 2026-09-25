package engine

import (
	"encoding/json"
	"testing"
)

// TestDefaultLibraryCompiles pins that the shipped default parses and every
// pattern compiles — a broken embedded default would silently disarm the whole
// engine, so it is a build-time guarantee, not a runtime surprise.
func TestDefaultLibraryCompiles(t *testing.T) {
	dets, err := DefaultDetectors()
	if err != nil {
		t.Fatalf("embedded default failed to load: %v", err)
	}
	if len(dets) < 20 {
		t.Fatalf("shipped default has only %d detectors — expected the full library", len(dets))
	}
	seen := map[string]bool{}
	for _, d := range dets {
		if d.ID == "" {
			t.Error("a shipped detector has no id (the merge key)")
		}
		if seen[d.ID] {
			t.Errorf("duplicate detector id in shipped default: %s", d.ID)
		}
		seen[d.ID] = true
		// the honesty contract: a detector must declare coverage — either it is
		// enumerable, or it lists its unknowable gaps. Silent zero coverage is
		// the thing this library refuses to ship.
		if !d.Coverage.Enumerable && len(d.Coverage.Gaps) == 0 {
			t.Errorf("detector %s declares no coverage (not enumerable, no gaps)", d.ID)
		}
	}
}

// TestRoleScoping proves a role-scoped detector is inert off-role and active
// on-role — the property that lets one config serve the tool-call hook and the
// transcript audit path at once.
func TestRoleScoping(t *testing.T) {
	dets := []Detector{{
		ID: "agent.apology", Kind: "content", Roles: []string{"assistant"},
		Keywords: []string{"sorry"}, Tag: tagSpec{"agent", "apology"},
	}}
	if tags := Classify(Event{Text: "sorry about that", Role: "tool_call"}, dets); len(tags) != 0 {
		t.Fatalf("assistant-scoped detector fired on a tool_call event: %v", tags)
	}
	if tags := Classify(Event{Text: "sorry about that", Role: "assistant"}, dets); len(tags) != 1 {
		t.Fatalf("assistant-scoped detector did not fire on an assistant event: %v", tags)
	}
	// a detector with no Roles is role-agnostic (fires regardless)
	dets[0].Roles = nil
	if tags := Classify(Event{Text: "sorry", Role: "tool_call"}, dets); len(tags) != 1 {
		t.Fatalf("role-agnostic detector should fire on any role: %v", tags)
	}
}

func TestMergeByID(t *testing.T) {
	base := []Detector{
		{ID: "a", Kind: "content", Keywords: []string{"x"}, Tag: tagSpec{"k", "a"}},
		{ID: "b", Kind: "content", Keywords: []string{"y"}, Tag: tagSpec{"k", "b"}},
	}
	overlay := []Detector{
		{ID: "b", Kind: "content", Keywords: []string{"z"}, Tag: tagSpec{"k", "b2"}}, // replace
		{ID: "c", Kind: "content", Keywords: []string{"w"}, Tag: tagSpec{"k", "c"}},  // add
		{ID: "a", Disabled: true}, // remove
	}
	got := mergeByID(base, overlay)
	byID := map[string]Detector{}
	for _, d := range got {
		byID[d.ID] = d
	}
	if _, ok := byID["a"]; ok {
		t.Error("disabled id 'a' should have been removed")
	}
	if byID["b"].Tag.Value != "b2" {
		t.Errorf("id 'b' should have been replaced by the overlay, got tag %q", byID["b"].Tag.Value)
	}
	if _, ok := byID["c"]; !ok {
		t.Error("new id 'c' should have been added")
	}
}

// TestLoadLayeredNoOverlay: an empty/missing overlay path yields exactly the
// embedded default — the "never silent" contract.
func TestLoadLayeredNoOverlay(t *testing.T) {
	base, err := DefaultDetectors()
	if err != nil {
		t.Fatal(err)
	}
	got, err := LoadLayered("")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(base) {
		t.Fatalf("empty overlay should equal the default: got %d want %d", len(got), len(base))
	}
	got2, err := LoadLayered("/nonexistent/path/detectors.json")
	if err != nil {
		t.Fatalf("a missing overlay file must not error: %v", err)
	}
	if len(got2) != len(base) {
		t.Fatalf("missing overlay should equal the default: got %d want %d", len(got2), len(base))
	}
}

func TestStructuralFloorIsExactSubsetOfStarter(t *testing.T) {
	floor, err := StructuralDetectors()
	if err != nil {
		t.Fatal(err)
	}
	starter, err := DefaultDetectors()
	if err != nil {
		t.Fatal(err)
	}
	if len(floor) != 30 || len(starter) != 69 {
		t.Fatalf("migration snapshot floor=%d starter=%d, want 30/69", len(floor), len(starter))
	}
	starterByID := map[string]Detector{}
	for _, detector := range starter {
		starterByID[detector.ID] = detector
	}
	for _, detector := range floor {
		starterDetector, ok := starterByID[detector.ID]
		if !ok {
			t.Fatalf("structural detector %s absent from starter", detector.ID)
		}
		floorJSON, _ := json.Marshal(detector)
		starterJSON, _ := json.Marshal(starterDetector)
		if string(floorJSON) != string(starterJSON) {
			t.Fatalf("structural detector %s drifted from complete starter snapshot", detector.ID)
		}
	}
	for _, opinionatedID := range []string{"area.src", "area.secrets", "net.dest", "risk.rm-rf",
		"secret.private-key", "data.email", "agent.claim-done", "user.frustration", "phase.red-team"} {
		for _, detector := range floor {
			if detector.ID == opinionatedID {
				t.Fatalf("opinionated detector %s leaked into structural floor", opinionatedID)
			}
		}
	}
}
