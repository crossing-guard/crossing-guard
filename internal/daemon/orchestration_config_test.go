package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOrchestrationConfigDefaultsAndValidation(t *testing.T) {
	dir := t.TempDir()
	config, origin, err := loadOrchestrationConfig(dir)
	if err != nil || origin != "builtin-default" || config.NaturalSignal.SweepSeconds != 30 ||
		config.Delivery.MaxPendingPerSession != 3 || !config.IsCarrierKind("tool.completed") || config.IsCarrierKind("turn.ended") ||
		config.ClaimMargin() != 300*time.Millisecond {
		t.Fatalf("defaults: %+v %s %v", config, origin, err)
	}
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "orchestration.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"format_version":1,"delivery":{"ttl_seconds":60,"carrier_kinds":["turn.started"]}}`)
	config, origin, err = loadOrchestrationConfig(dir)
	if err != nil || !strings.HasSuffix(origin, "orchestration.json") || config.Delivery.TTLSeconds != 60 ||
		config.IsCarrierKind("tool.completed") || !config.IsCarrierKind("turn.started") || config.NaturalSignal.CoalesceMS != 500 {
		t.Fatalf("override: %+v %s %v", config, origin, err)
	}
	// An operator file is never silently reinterpreted: unknown keys, a
	// non-positive tunable, and turn.ended as a carrier are visible errors.
	for _, bad := range []string{
		`{"format_version":1,"delivery":{"codex_transport":"queue"}}`,
		`{"format_version":1,"natural_signal":{"coalesce_ms":0}}`,
		`{"format_version":1,"delivery":{"claim_margin_ms":0}}`,
		`{"format_version":1,"delivery":{"claim_margin_ms":1500}}`,
		`{"format_version":1,"delivery":{"carrier_kinds":["turn.ended"]}}`,
		`{"format_version":2}`,
	} {
		write(bad)
		if _, _, err := loadOrchestrationConfig(dir); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}

type deliveryOptionsFixtureDriver struct {
	managedDynamicFixtureDriver
	configured json.RawMessage
}

func (d *deliveryOptionsFixtureDriver) ConfigureDelivery(raw json.RawMessage) error {
	if strings.Contains(string(raw), "boom") {
		return errors.New("boom")
	}
	d.configured = append(json.RawMessage(nil), raw...)
	return nil
}

// runtime_options is dispatched to the registered driver by registry name;
// the generic loader never reads inside a blob and rejects names it cannot
// route (plan A2).
func TestOrchestrationRuntimeOptionsDispatchToRegisteredDriver(t *testing.T) {
	driver := &deliveryOptionsFixtureDriver{}
	original := chatDrivers
	chatDrivers = map[string]ChatDriver{"managed-fixture": driver, "plain": managedDynamicFixtureDriver{}}
	t.Cleanup(func() { chatDrivers = original })
	config := defaultOrchestrationConfig()
	config.Delivery.RuntimeOptions = map[string]json.RawMessage{"managed-fixture": json.RawMessage(`{"transport":"anything"}`)}
	if err := applyOrchestrationRuntimeOptions(config); err != nil || string(driver.configured) != `{"transport":"anything"}` {
		t.Fatalf("%v %s", err, driver.configured)
	}
	config.Delivery.RuntimeOptions = map[string]json.RawMessage{"unknown-runtime": json.RawMessage(`{}`)}
	if err := applyOrchestrationRuntimeOptions(config); err == nil || !strings.Contains(err.Error(), "unregistered") {
		t.Fatal(err)
	}
	config.Delivery.RuntimeOptions = map[string]json.RawMessage{"plain": json.RawMessage(`{}`)}
	if err := applyOrchestrationRuntimeOptions(config); err == nil || !strings.Contains(err.Error(), "no delivery options") {
		t.Fatal(err)
	}
	config.Delivery.RuntimeOptions = map[string]json.RawMessage{"managed-fixture": json.RawMessage(`{"transport":"boom"}`)}
	if err := applyOrchestrationRuntimeOptions(config); err == nil || !strings.Contains(err.Error(), "managed-fixture: boom") {
		t.Fatal(err)
	}
}

// Codex keeps queue as its default transport and validates its own option
// keys; the word "queue" lives in the adapter, never in the loader.
func TestCodexDeliveryOptionsOwnTheirVocabulary(t *testing.T) {
	t.Cleanup(func() { codexDeliveryTransport = "queue" })
	if err := (codexChatDriver{}).ConfigureDelivery(json.RawMessage(`{"transport":"hook"}`)); err != nil || codexDeliveryTransport != "hook" {
		t.Fatal(err, codexDeliveryTransport)
	}
	if !strings.Contains(codexDeliveryCapability().Boundary, "hook boundary") {
		t.Fatal(codexDeliveryCapability())
	}
	if err := (codexChatDriver{}).ConfigureDelivery(json.RawMessage(`{"transport":"carrier-pigeon"}`)); err == nil {
		t.Fatal("unknown transport accepted")
	}
	if err := (codexChatDriver{}).ConfigureDelivery(json.RawMessage(`{"mode":"x"}`)); err == nil {
		t.Fatal("unknown key accepted")
	}
	if err := (codexChatDriver{}).ConfigureDelivery(json.RawMessage(`{}`)); err != nil || codexDeliveryTransport != "queue" {
		t.Fatal(err, codexDeliveryTransport)
	}
}
