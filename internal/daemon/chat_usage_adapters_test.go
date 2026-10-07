package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crossing-guard/harvest"
)

// A real Claude Code 2.1.212 headless result (plan §4 step 0, row c), with the
// reply text and ids replaced.
func TestClaudeResultBecomesOneAdditiveUsageDelta(t *testing.T) {
	raw, err := os.ReadFile("testdata/claude_2_1_212_result.json")
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	// Stream assistant lines repeat (measured) and carry usage; none may count.
	assistant := map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant",
		"content": []any{}, "usage": map[string]any{"input_tokens": float64(10), "output_tokens": float64(1)}}}
	for _, event := range append(claudeChatDriver{}.ProjectEvent(assistant), claudeChatDriver{}.ProjectEvent(assistant)...) {
		if event["type"] == "usage" {
			t.Fatal("an assistant stream line was counted as usage")
		}
	}
	events := claudeChatDriver{}.ProjectEvent(result)
	if len(events) != 2 || events[0]["type"] != "usage" || events[1]["type"] != "result" || events[1]["cost"] != nil {
		t.Fatalf("events: %+v", events)
	}
	usage := events[0]["delta"].(ChatUsage)
	if usage.Accumulation != usageAdditive || *usage.Input != 18 || *usage.CacheWrite != 43397 ||
		*usage.CacheRead != 37114 || *usage.Output != 369 || *usage.Reasoning != 215 {
		t.Fatalf("classes: %+v (thinking is inside output)", usage)
	}
	if usage.ContextWindow == nil || *usage.ContextWindow != 200000 || usage.ContextUsed == nil {
		t.Fatalf("context: used=%v window=%v", usage.ContextUsed, usage.ContextWindow)
	}
	if usage.Cost == nil || usage.Cost.Unit != "USD" || usage.Cost.Basis != harvest.CostBasisRuntime || usage.Cost.Amount <= 0 {
		t.Fatalf("cost: %+v", usage.Cost)
	}
	result["is_error"] = true
	if events := (claudeChatDriver{}).ProjectEvent(result); len(events) != 1 || events[0]["type"] != "result" {
		t.Fatalf("a failed result must state no cost or usage: %+v", events)
	}
}

func TestCodexTurnCompletedStatesNoLiveUsage(t *testing.T) {
	events := codexChatDriver{}.ProjectEvent(map[string]any{"type": "turn.completed",
		"usage": map[string]any{"input_tokens": float64(63176), "output_tokens": float64(233)}})
	encoded, _ := json.Marshal(events)
	if len(events) != 1 || strings.Contains(string(encoded), "63176") {
		t.Fatalf("D-7: a thread running total must not reach the task as a turn figure: %s", encoded)
	}
}

// A fake app-server answering initialize and two model/list pages.
func TestCodexDiscoversModelsThroughTheBoundedSession(t *testing.T) {
	bin := t.TempDir()
	script := `#!/bin/sh
while read line; do
  case "$line" in
    *'"initialize"'*) echo '{"id":1,"result":{}}' ;;
    *'"cursor"'*) echo '{"id":3,"result":{"data":[{"id":"m-hidden","hidden":true},{"id":"m-b","displayName":"B","inputModalities":["text","image"]}],"nextCursor":null}}' ;;
    *'"model/list"'*) echo '{"method":"note","params":{}}'; echo '{"id":2,"result":{"data":[{"id":"m-a","displayName":"A"},{"id":"m-default","displayName":"Default one","isDefault":true,"inputModalities":["text"]}],"nextCursor":"c2"}}' ;;
  esac
done
`
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runner := boundedRunner{ctx: ctx, workDir: filepath.Join(t.TempDir(), "work"), maxBytes: 1 << 20, waitDelay: time.Second}
	discovery, err := codexChatDriver{}.DiscoverChatModels(ctx, ChatModelEnv{Run: runner.Run, Session: runner.Session})
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, model := range discovery.Models {
		ids = append(ids, model.ID)
		for _, kind := range model.Inputs {
			if kind == "image" {
				t.Fatalf("S-RT5: Codex refuses images on a named model, so %s must not advertise them", model.ID)
			}
		}
	}
	if strings.Join(ids, ",") != "m-default,m-a,m-b" {
		t.Fatalf("ids = %v (default first, hidden skipped, both pages)", ids)
	}
}
