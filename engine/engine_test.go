package engine

import (
	"slices"
	"testing"
)

// These tests PORT the Python testbed probes into Go, so the shared engine is
// proven to match detect.py / audit.py / checkpoint.py behaviour — the parity the
// whole port exists to guarantee.

func dets(t *testing.T) []Detector {
	d, err := LoadDetectors("testdata/detectors.json")
	if err != nil {
		t.Fatalf("load detectors: %v", err)
	}
	return d
}

func has(tags []Tag, key, val string) bool {
	for _, t := range tags {
		if t.Key == key && t.Value == val {
			return true
		}
	}
	return false
}

// P-BI: content-classification is BLIND to business intel (no shape); source
// classification tags the same sensitivity deterministically. The product thesis.
func TestPBI_ContentBlindSourceSees(t *testing.T) {
	d := dets(t)
	bi := `Q3 board memo — CONFIDENTIAL. Gross margin on SKU-1001 fell to 30% ` +
		`after the supplier raised unit cost to $10.00; re-sourcing to protect ` +
		`the line. Price floor stays at $20.00. Do not share outside the board.`
	if got := Classify(Event{Text: bi}, d); len(got) != 0 {
		t.Fatalf("content scan of business intel should find NOTHING, got %v", got)
	}
	// same sensitivity, by SOURCE:
	if tags := Classify(Event{Tool: "listPayments"}, d); !has(tags, "data-class", "regulated") {
		t.Fatalf("finance tool should tag regulated by source, got %v", tags)
	}
	if tags := Classify(Event{Tool: "getSupplierPrice"}, d); !has(tags, "data-class", "internal") {
		t.Fatalf("vendor-cost tool should tag internal by source, got %v", tags)
	}
}

// Source classification catches ALL of a customer record; content only the shaped bits.
func TestSourceBeatsContentOnPII(t *testing.T) {
	d := dets(t)
	rec := "Order 111: John A. Smith, 4820 Example Ave, jsmith@example.com, CUS-40028871."
	content := Classify(Event{Text: rec}, d)
	if !has(content, "data-class", "personal-data") {
		t.Fatalf("content should catch the SHAPED pii (email/CUS-id), got %v", content)
	}
	// source nails it with zero content inspection:
	if src := Classify(Event{Tool: "getCustomerAddress"}, d); !has(src, "data-class", "personal-data") {
		t.Fatalf("getCustomerAddress should tag personal-data by source, got %v", src)
	}
}

// Destination is fail-safe: unlisted → external.
func TestDestinationFailSafe(t *testing.T) {
	d := dets(t)
	if tags := Classify(Event{Destination: "https://random-saas.com/up"}, d); !has(tags, "destination-class", "external") {
		t.Fatalf("unlisted destination must be external, got %v", tags)
	}
	if tags := Classify(Event{Destination: "https://inventory.example.internal/x"}, d); !has(tags, "destination-class", "in-house") {
		t.Fatalf("allowlisted host must be in-house, got %v", tags)
	}
}

// Water mark is the monotonic max data-class; absence is a weak negative ("").
func TestWaterMarkMonotonic(t *testing.T) {
	d := dets(t)
	var tags []Tag
	tags = append(tags, Classify(Event{Tool: "getProduct"}, d)...) // public
	if WaterMark(tags) != "public" {
		t.Fatalf("want public, got %q", WaterMark(tags))
	}
	tags = append(tags, Classify(Event{Tool: "getSupplierPrice"}, d)...)   // internal
	tags = append(tags, Classify(Event{Tool: "getCustomerAddress"}, d)...) // personal-data
	if WaterMark(tags) != "personal-data" {
		t.Fatalf("want personal-data after PII, got %q", WaterMark(tags))
	}
	// a later low-class read must NOT lower the mark
	tags = append(tags, Classify(Event{Tool: "getProduct"}, d)...)
	if WaterMark(tags) != "personal-data" {
		t.Fatalf("mark must be monotonic, got %q", WaterMark(tags))
	}
	if WaterMark(nil) != "" {
		t.Fatalf("no tags → weak negative (empty), got %q", WaterMark(nil))
	}
}

// Compound predicate: a two-term rule does NOT fire on one term (IF THIS AND THIS).
func TestCompoundPredicate(t *testing.T) {
	pii := Tag{Key: "data-class", Value: "personal-data"}
	ep := Tag{Key: "endpoint-class", Value: "non-compliant"}
	p := Predicate{All: []Predicate{{Tag: "data-class", Value: "personal-data"},
		{Tag: "endpoint-class", Value: "non-compliant"}}}
	if Match(p, []Tag{pii}) {
		t.Fatal("two-term rule must NOT fire on one term")
	}
	if !Match(p, []Tag{pii, ep}) {
		t.Fatal("two-term rule must fire when both present")
	}
}

// Checkpoint ladder + capability honesty: hard-block greys override; route is greyed
// without a gateway and live with one (design §7/§8).
func TestCheckpointLadderAndCapabilities(t *testing.T) {
	pol, err := LoadPolicy("testdata/policy.json")
	if err != nil {
		t.Fatalf("load policy: %v", err)
	}
	// hard-block: credential material, override must be greyed (non-overridable)
	cred := []Tag{{Key: "data-class", Value: "credential-material"}}
	d := Decide(cred, pol)
	if d.Decision != "block" || d.Mode != HardBlock {
		t.Fatalf("credential → hard-block, got %s/%s", d.Decision, d.Mode)
	}
	for _, m := range d.Menu {
		if m.Action == "confirm-and-record" && m.Live {
			t.Fatal("hard-block must NOT offer a live override")
		}
	}
	// confirm-and-record: PII → non-compliant endpoint; route greyed w/o gateway
	pii := []Tag{{Key: "data-class", Value: "personal-data"},
		{Key: "endpoint-class", Value: "non-compliant"}}
	d = Decide(pii, pol)
	if d.Decision != "block" || d.Mode != ConfirmAndRecord {
		t.Fatalf("PII+endpoint → confirm-and-record block, got %s/%s", d.Decision, d.Mode)
	}
	if routeLive(d.Menu) {
		t.Fatal("route must be greyed with no gateway")
	}
	// same policy, gateway wired → route goes live
	pol.Capabilities.GatewayAvailable = true
	d = Decide(pii, pol)
	if !routeLive(d.Menu) {
		t.Fatal("route must be LIVE once a gateway is available")
	}
}

func routeLive(menu []Resolution) bool {
	for _, m := range menu {
		if m.Action == "route" && m.Live {
			return true
		}
	}
	return false
}

// A rule leaning on a content detector carries UNKNOWABLE coverage; a source map is
// enumerable. The tag itself never carries a score.
func TestCoverageIsOnDetectorNotTag(t *testing.T) {
	d := dets(t)
	src := Classify(Event{Tool: "getCustomerAddress"}, d)
	if len(src) == 0 || !src[0].Coverage.Enumerable {
		t.Fatalf("source map coverage should be enumerable, got %+v", src)
	}
	// AKIA key pattern is declared unknowable
	key := Classify(Event{Text: "AKIAIOSFODNN7EXAMPLE"}, d)
	if len(key) == 0 {
		t.Fatal("aws key should be detected")
	}
	if key[0].Coverage.Enumerable {
		t.Fatal("aws-key pattern coverage must be declared UNKNOWABLE")
	}
}

// Gates is the one "does this mode stop the action" predicate every tier reads.
func TestModeGates(t *testing.T) {
	for mode, want := range map[Mode]bool{HardBlock: true, ConfirmAndRecord: true,
		WarnAndProceed: false, SilentLog: false, "": false, "hard-blok": false} {
		if got := mode.Gates(); got != want {
			t.Errorf("Mode(%q).Gates() = %v, want %v", mode, got, want)
		}
	}
}

func TestFiredWarnsNamesEveryWarnRuleOnce(t *testing.T) {
	always := Predicate{Not: &Predicate{Tag: "absent"}}
	pol := &Policy{Rules: []Rule{
		{ID: "w1", Mode: WarnAndProceed, If: always},
		{ID: "quiet", Mode: SilentLog, If: always},
		{ID: "w2", Mode: WarnAndProceed, If: always},
		{ID: "w1", Mode: WarnAndProceed, If: always}, // same id from another layer
	}}
	d := Decide(nil, pol)
	if got := FiredWarns(d, pol); !slices.Equal(got, []string{"w1", "w2"}) {
		t.Fatalf("FiredWarns = %v, want [w1 w2]", got)
	}
	pol.Rules = append(pol.Rules, Rule{ID: "stop", Mode: HardBlock, If: always})
	if got := FiredWarns(Decide(nil, pol), pol); got != nil {
		t.Fatalf("a gating decision reported warns: %v", got)
	}
}
