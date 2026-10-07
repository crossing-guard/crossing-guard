package engine

import (
	"encoding/json"
	"regexp"
	"strings"
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
	// 70 since data.credential (owner ruling D-6, stateful-rule-coverage plan): the
	// legacy starter changes only by a recorded owner ruling.
	if len(floor) != 30 || len(starter) != 70 {
		t.Fatalf("migration snapshot floor=%d starter=%d, want 30/70", len(floor), len(starter))
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
		"secret.private-key", "data.email", "data.credential", "agent.claim-done", "user.frustration", "phase.red-team"} {
		for _, detector := range floor {
			if detector.ID == opinionatedID {
				t.Fatalf("opinionated detector %s leaked into structural floor", opinionatedID)
			}
		}
	}
}

// secretSamples holds one positive sample per shipped secret detector. A new secret
// detector without a sample fails TestCredentialProducerCoversEverySecretDetector, which
// is the point: it must be folded into data.credential too.
var secretSamples = map[string]string{
	"secret.aws-key":     "export AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE",
	"secret.private-key": "-----BEGIN RSA PRIVATE KEY-----",
	"secret.gh-token":    "token ghp_" + strings.Repeat("a", 36) + " end",
	"secret.bearer":      "curl -H 'authorization: Bearer abcdefghij0123'",
}

// data.credential is the data-class=credential-material producer, and its regex is the
// secret detectors' regexes as alternatives. Two guards keep them from drifting: each
// secret regex must appear verbatim as one alternative (a (?i)X flag as a (?i:X) group),
// and each secret detector's sample must fire both it and data.credential. The first
// catches a broadened secret regex whose old sample still passes; the second catches an
// alternative that no longer matches what it claims to.
func TestCredentialProducerCoversEverySecretDetector(t *testing.T) {
	dets, err := DefaultDetectors()
	if err != nil {
		t.Fatal(err)
	}
	var cred *Detector
	for i := range dets {
		if dets[i].ID == "data.credential" {
			cred = &dets[i]
		}
	}
	if cred == nil || cred.Tag.Key != "data-class" || cred.Tag.Value != "credential-material" ||
		cred.Scope != "resource" || len(cred.Roles) != 0 || cred.Coverage.Enumerable {
		t.Fatalf("data.credential missing or misdeclared: %+v", cred)
	}
	// Its gaps carry every secret detector's gaps, so the per-term coverage report for a
	// data-class term is never narrower than the secret terms it summarizes.
	credGaps := map[string]bool{}
	for _, g := range cred.Coverage.Gaps {
		credGaps[g] = true
	}
	for _, d := range dets {
		if d.Tag.Key != "secret" {
			continue
		}
		for _, g := range d.Coverage.Gaps {
			if !credGaps[g] {
				t.Errorf("data.credential gaps miss %s gap %q", d.ID, g)
			}
		}
	}
	alternatives := map[string]bool{}
	for _, alt := range strings.Split(cred.Regex, ")|(") {
		alt = strings.TrimSuffix(strings.TrimPrefix(alt, "("), ")")
		alternatives[alt] = true
	}
	secrets := 0
	for _, d := range dets {
		if d.Tag.Key != "secret" {
			continue
		}
		secrets++
		want := "?:" + d.Regex
		if flagged, ok := strings.CutPrefix(d.Regex, "(?i)"); ok {
			want = "?i:" + flagged
		}
		if !alternatives[want] {
			t.Errorf("%s regex %q is not an alternative of data.credential (want %q)", d.ID, d.Regex, want)
		}
		sample, ok := secretSamples[d.ID]
		if !ok {
			t.Errorf("%s has no sample; add one and fold it into data.credential", d.ID)
			continue
		}
		tags := Classify(Event{Text: sample, Role: "tool_call"}, dets)
		if !hasTag(tags, "secret", d.Tag.Value) || !hasTag(tags, "data-class", "credential-material") {
			t.Errorf("%s sample %q: tags %v, want secret=%s and data-class=credential-material",
				d.ID, sample, tags, d.Tag.Value)
		}
	}
	if secrets != len(alternatives) {
		t.Errorf("data.credential has %d alternatives for %d secret detectors", len(alternatives), secrets)
	}
	// The case flag must not leak out of the bearer group into the other alternatives.
	if regexp.MustCompile(cred.Regex).MatchString("akiaabcdefghijklmnop") {
		t.Error("data.credential matched a lowercase AWS key: the (?i) flag leaked")
	}
}

// With the starter, a credential sample now raises the water mark to the ladder's top
// rung, which no shipped detector could reach before data.credential.
func TestCredentialSampleRaisesTheWaterMark(t *testing.T) {
	dets, err := DefaultDetectors()
	if err != nil {
		t.Fatal(err)
	}
	tags := Classify(Event{Text: secretSamples["secret.aws-key"], Role: "tool_call"}, dets)
	if got := WaterMark(tags); got != "credential-material" {
		t.Fatalf("water mark %q, want credential-material (tags %v)", got, tags)
	}
	for _, tag := range tags {
		if tag.Key == "data-class" && strings.Contains(tag.Evidence, "AKIA") {
			t.Fatalf("data.credential stored raw evidence %q", tag.Evidence)
		}
	}
}

func hasTag(tags []Tag, key, value string) bool {
	for _, t := range tags {
		if t.Key == key && t.Value == value {
			return true
		}
	}
	return false
}
