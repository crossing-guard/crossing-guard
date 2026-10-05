package engine

import (
	"encoding/json"
	"testing"

	"crossing-guard/schemas"
)

// Every data-class value a shipped detector emits must be a rung on the ladder: a value
// off it is silently ignored by WaterMark and dropped from the wire sensitivity, which is
// how the email detector's "personal" went unnoticed (data-class-personal-ladder-plan.md).
func TestShippedDataClassValuesAreOnTheLadder(t *testing.T) {
	onLadder := map[string]bool{}
	for _, v := range WaterOrder() {
		onLadder[v] = true
	}
	for name, load := range map[string]func() ([]Detector, error){
		"default": DefaultDetectors, "structural": StructuralDetectors} {
		dets, err := load()
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range dets {
			if d.Tag.Key == DataClassKey && !onLadder[d.Tag.Value] {
				t.Errorf("%s detector %s emits data-class %q, which is not on the water-mark ladder %v",
					name, d.ID, d.Tag.Value, WaterOrder())
			}
		}
	}
}

func TestDataClassAliasesPointOntoTheLadder(t *testing.T) {
	onLadder := map[string]bool{}
	for _, v := range WaterOrder() {
		onLadder[v] = true
	}
	aliases := DataClassAliases()
	if len(aliases) == 0 {
		t.Fatal("the legacy personal spelling must stay aliased: stored events carry it")
	}
	for _, a := range aliases {
		if a.Key != DataClassKey || onLadder[a.From] || !onLadder[a.To] {
			t.Errorf("alias %+v must map an off-ladder spelling onto a rung", a)
		}
	}
	if len(sensitivityClasses) != len(WaterOrder()) {
		t.Errorf("wire sensitivity must be the ladder: %v vs %v", sensitivityClasses, WaterOrder())
	}
}

func TestWaterMarkReadsTheLegacySpelling(t *testing.T) {
	if got := WaterMark([]Tag{{Key: DataClassKey, Value: "internal"}, {Key: DataClassKey, Value: "personal"}}); got != "personal-data" {
		t.Fatalf("a stored personal tag must raise the mark to personal-data, got %q", got)
	}
	if got := WaterMark([]Tag{{Key: DataClassKey, Value: "radioactive"}}); got != "" {
		t.Fatalf("an unknown value stays off the ladder, got %q", got)
	}
}

func TestClassifyEmitsTheLadderSpellingForALegacyDocument(t *testing.T) {
	legacy := []byte(`{"detectors":[{"id":"data.email","kind":"pattern","regex":"[a-z]+@[a-z]+\\.[a-z]{2,}",
	  "tag":{"key":"data-class","value":"personal"},"coverage":{"enumerable":false}}]}`)
	dets, err := parseDetectors(legacy)
	if err != nil {
		t.Fatal(err)
	}
	tags := Classify(Event{Text: "mail alice@example.com"}, dets)
	if len(tags) != 1 || tags[0].Value != "personal-data" {
		t.Fatalf("a pinned or overlay document with the legacy spelling must emit the rung: %+v", tags)
	}
	shipped, _ := DefaultDetectors()
	if tags := Classify(Event{Text: "mail alice@example.com"}, shipped); !has(tags, DataClassKey, "personal-data") || WaterMark(tags) != "personal-data" {
		t.Fatalf("the shipped email detector must raise the mark: %+v", tags)
	}
}

func TestMatchAcrossTheLegacySpelling(t *testing.T) {
	for _, c := range []struct {
		term, tag Tag
		want      bool
	}{
		{Tag{Key: "data-class", Value: "personal"}, Tag{Key: "data-class", Value: "personal-data"}, true},
		{Tag{Key: "data-class", Value: "personal-data"}, Tag{Key: "data-class", Value: "personal"}, true},
		{Tag{Key: "session:data-class", Value: "personal-data"}, Tag{Key: "session:data-class", Value: "personal"}, true},
		{Tag{Key: "target:data-class", Value: "personal"}, Tag{Key: "target:data-class", Value: "personal-data"}, true},
		{Tag{Key: "data-class", Value: "personal"}, Tag{Key: "data-class", Value: "regulated"}, false},
		{Tag{Key: "session:data-class", Value: "personal"}, Tag{Key: "data-class", Value: "personal-data"}, false},
		{Tag{Key: "role", Value: "personal"}, Tag{Key: "role", Value: "personal-data"}, false},
	} {
		got := Match(Predicate{Tag: c.term.Key, Value: c.term.Value}, []Tag{c.tag})
		if got != c.want {
			t.Errorf("term %s=%s vs tag %s=%s: got %v, want %v", c.term.Key, c.term.Value, c.tag.Key, c.tag.Value, got, c.want)
		}
	}
}

func TestEncodeWireEventCanonicalSensitivity(t *testing.T) {
	both := `[{"key":"data-class","value":"personal","detector":"data.email","provenance":"observed","evidence":"x"},` +
		`{"key":"data-class","value":"personal-data","detector":"data.email","provenance":"observed","evidence":"y"}]`
	raw, err := EncodeWireEvent(WireEventInput{DeviceID: "dev_A", GlobalID: DeterministicTypedID("evt", "dev_A:9"), TS: 100,
		Session: "claude/s", Runtime: "claude", Verb: "read", Tool: "Read", FrozenTags: both, Origin: "live"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var ev WireEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatal(err)
	}
	if len(ev.Sensitivity) != 1 || ev.Sensitivity[0] != "personal-data" {
		t.Fatalf("one canonical sensitivity for both spellings: %v", ev.Sensitivity)
	}
	if len(ev.Payload.Tags) != 2 || ev.Payload.Tags[0].Value != "personal" {
		t.Fatalf("the recorded tag value ships as stored: %+v", ev.Payload.Tags)
	}
	if err := schemas.Validate("event.schema.json", raw); err != nil {
		t.Fatalf("wire event must validate: %v\n%s", err, raw)
	}
}

// Coverage must agree with matching: a rule term in either spelling has the email
// detector as its producer (postwork PW-1).
func TestCoverageProducersAcrossTheLegacySpelling(t *testing.T) {
	shipped, _ := DefaultDetectors()
	legacy, err := parseDetectors([]byte(`{"detectors":[{"id":"data.email","kind":"pattern","regex":"[a-z]+@[a-z]+",
	  "tag":{"key":"data-class","value":"personal"},"coverage":{"enumerable":false}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, dets := range [][]Detector{shipped, legacy} {
		for _, v := range []string{"personal", "personal-data"} {
			if prods := producersFor(Predicate{Tag: DataClassKey, Value: v}, producerSources{dets: dets, ch: channelHarvest}); len(prods) != 1 || prods[0].id != "data.email" {
				t.Errorf("data-class=%s over %s: producers %v", v, dets[len(dets)-1].ID, prods)
			}
		}
	}
	if prods := producersFor(Predicate{Tag: "role", Value: "personal-data"}, producerSources{dets: legacy, ch: channelHarvest}); len(prods) != 0 {
		t.Errorf("no aliasing outside data-class keys: %v", prods)
	}
}
