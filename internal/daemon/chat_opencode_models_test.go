package daemon

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"crossing-guard/harvest"
)

// The fixture is a real `opencode models --verbose --pure` capture (1.18.0,
// 2026-09-24) trimmed to one model per provider/price/image combination, with
// every api, headers and options value replaced by a SENTINEL string.
func TestOpenCodeParsesVerboseModelsWithoutSecretFields(t *testing.T) {
	raw, err := os.ReadFile("testdata/opencode_1_18_0_models_verbose.txt")
	if err != nil {
		t.Fatal(err)
	}
	models, rejected, err := parseOpenCodeVerboseModels(raw)
	if err != nil || rejected != 0 || len(models) != 7 {
		t.Fatalf("models=%d rejected=%d err=%v", len(models), rejected, err)
	}
	encoded, _ := json.Marshal(models)
	if strings.Contains(string(encoded), "SENTINEL") {
		t.Fatalf("a secret-bearing vendor field reached the neutral entries: %s", encoded)
	}
	byID := map[string]ChatModelOption{}
	for _, model := range models {
		if err := validateChatModelOption(model, true, defaultChatModelBounds); err != nil {
			t.Fatalf("%s: %v", model.ID, err)
		}
		byID[model.ID] = model
	}
	local := byID["ollama/qwen2.5-coder:7b"]
	if local.Group != "ollama" || local.Limits != nil {
		t.Fatalf("a zero limit must be unknown, not 0: %+v", local)
	}
	if local.Price == nil || local.Price.Rates[0].Amount != 0 || local.Price.Unit != "USD" {
		t.Fatalf("D-6: a stated zero price is shown as stated: %+v", local.Price)
	}
	free := byID["opencode/big-pickle"]
	if free.Limits == nil || free.Limits.ContextTokens == nil || *free.Limits.ContextTokens != 200000 {
		t.Fatalf("stated context limit lost: %+v", free.Limits)
	}
}

func TestOpenCodeVerboseParserRefusesDriftAndCountsBadObjects(t *testing.T) {
	if _, _, err := parseOpenCodeVerboseModels([]byte("Some banner text\n")); modelDiscoveryReason(err) != modelReasonUnparseable {
		t.Fatalf("header drift must fail loudly: %v", err)
	}
	if _, _, err := parseOpenCodeVerboseModels([]byte("a/b\n{\n  \"id\": \"b\"\n")); modelDiscoveryReason(err) != modelReasonUnparseable {
		t.Fatalf("an unterminated object must fail: %v", err)
	}
	mixed := "p/good\n{\n  \"id\": \"good\",\n  \"providerID\": \"p\",\n  \"name\": \"Good\"\n}\n" +
		"p/liar\n{\n  \"id\": \"other\",\n  \"providerID\": \"p\"\n}\n"
	models, rejected, err := parseOpenCodeVerboseModels([]byte(mixed))
	if err != nil || len(models) != 1 || rejected != 1 || models[0].ID != "p/good" {
		t.Fatalf("models=%+v rejected=%d err=%v", models, rejected, err)
	}
}

// Measured 2026-09-23: two steps whose tokens sum to the session totals, and
// reasoning counted apart from output.
func TestOpenCodeStepUsageIsAdditiveAndFoldsReasoningIntoOutput(t *testing.T) {
	step := map[string]any{"type": "step_finish", "part": map[string]any{"cost": 0.0165,
		"tokens": map[string]any{"total": float64(9000), "input": float64(371), "output": float64(14),
			"reasoning": float64(100), "cache": map[string]any{"read": float64(8448), "write": float64(67)}}}}
	events := openCodeChatDriver{}.ProjectEvent(step)
	if len(events) != 1 || events[0]["type"] != "usage" {
		t.Fatalf("step_finish must become one usage event: %+v", events)
	}
	usage := events[0]["delta"].(ChatUsage)
	if usage.Accumulation != usageAdditive || *usage.Output != 114 || *usage.Reasoning != 100 ||
		*usage.Input != 371 || *usage.ContextUsed != 371+8448+67 || usage.ModelID != "" {
		t.Fatalf("mapping: %+v", usage)
	}
	if usage.Cost == nil || usage.Cost.Amount != 0.0165 || usage.Cost.Basis != harvest.CostBasisRuntime {
		t.Fatalf("cost: %+v", usage.Cost)
	}
}
