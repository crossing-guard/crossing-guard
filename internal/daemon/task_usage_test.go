package daemon

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"crossing-guard/harvest"
)

func count(v int64) *int64 { return &v }

func applyUsage(t *testing.T, totals *TaskUsageTotals, task string, usage ChatUsage) (ChatUsage, TaskUsageTotal, TaskUsageContext) {
	t.Helper()
	event, ok := totals.Apply(task, usageEvent(usage))
	if !ok {
		t.Fatalf("report refused: %+v", usage)
	}
	return event["delta"].(ChatUsage), event["turn_total"].(TaskUsageTotal), event["context"].(TaskUsageContext)
}

// fakealpha reports per step: additive; an other class and a non-USD unit.
func TestTaskUsageAdditiveSumsClassesOtherAndCostPerUnit(t *testing.T) {
	totals := NewTaskUsageTotals(nil)
	totals.Start("t1", "fakealpha", "")
	step := ChatUsage{Accumulation: usageAdditive,
		TokenClasses: harvest.TokenClasses{Input: count(10), Output: count(5), Reasoning: count(2)},
		Other:        []harvest.TokenCount{{ID: "audio-in", Label: "Audio in", Side: harvest.TokenSideInput, Count: 7}},
		Cost:         &harvest.Cost{Amount: 1.5, Unit: "credits", Basis: harvest.CostBasisRuntime}}
	applyUsage(t, totals, "t1", step)
	step.Cost = &harvest.Cost{Amount: 0.25, Unit: "USD", Basis: harvest.CostBasisRuntime}
	_, total, _ := applyUsage(t, totals, "t1", step)
	if *total.Input != 20 || *total.Output != 10 || *total.Reasoning != 4 || total.CacheRead != nil {
		t.Fatalf("classes: %+v (an unstated class stays unknown, never 0)", total)
	}
	if len(total.Other) != 1 || total.Other[0].Count != 14 {
		t.Fatalf("other: %+v", total.Other)
	}
	if len(total.Cost) != 2 || total.Cost[0].Unit != "USD" || total.Cost[1].Amount != 1.5 {
		t.Fatalf("cost must stay one line per unit: %+v", total.Cost)
	}
}

// fakebeta reports running totals within the task, states its own window, and
// has no cost.
func TestTaskUsageRunningTotalsAreDifferencedUnderTheRules(t *testing.T) {
	totals := NewTaskUsageTotals(nil)
	totals.Start("t1", "fakebeta", "")
	applyUsage(t, totals, "t1", ChatUsage{Accumulation: usageRunningTotal, TokenClasses: harvest.TokenClasses{Input: count(100), Output: count(10)}, ContextUsed: count(100), ContextWindow: count(4000)})
	delta, total, ctx := applyUsage(t, totals, "t1", ChatUsage{Accumulation: usageRunningTotal, TokenClasses: harvest.TokenClasses{Input: count(150), Output: count(8)}, ContextUsed: count(160), ContextWindow: count(4000)})
	if *delta.Input != 50 || delta.Output != nil || delta.Accumulation != usageAdditive {
		t.Fatalf("a lower running figure must make that class unknown, never 0 or negative: %+v", delta)
	}
	if *total.Input != 150 || *total.Output != 10 || *ctx.Used != 160 || *ctx.Window != 4000 || total.Cost != nil {
		t.Fatalf("total=%+v ctx=%+v", total, ctx)
	}
	if _, ok := totals.Apply("t1", usageEvent(ChatUsage{Accumulation: usageAdditive, TokenClasses: harvest.TokenClasses{Input: count(1)}})); ok {
		t.Fatal("a task's accumulation mode must not change mid-task")
	}
	if _, ok := totals.Apply("t1", ChatEvent{"type": "usage", "delta": map[string]any{"input": 5}}); ok {
		t.Fatal("an untyped payload must be refused")
	}
	if _, ok := totals.Apply("unknown-task", usageEvent(ChatUsage{Accumulation: usageAdditive})); ok {
		t.Fatal("an unknown task must be refused")
	}
}

func TestTaskUsageWindowJoinsTheRequestedModelWhenTheReportNamesNone(t *testing.T) {
	models := func(runtime, id string) (ChatModelOption, bool) {
		if runtime == "fakealpha" && id == "a|b::c" {
			return ChatModelOption{ID: id, Limits: &ChatModelLimits{ContextTokens: count(8000)}}, true
		}
		return ChatModelOption{}, false
	}
	totals := NewTaskUsageTotals(models)
	totals.Start("t1", "fakealpha", "a|b::c")
	_, _, ctx := applyUsage(t, totals, "t1", ChatUsage{Accumulation: usageAdditive, ContextUsed: count(10)})
	if ctx.Window == nil || *ctx.Window != 8000 {
		t.Fatalf("window: %+v", ctx)
	}
	totals.Start("t2", "fakealpha", "")
	if _, _, ctx := applyUsage(t, totals, "t2", ChatUsage{Accumulation: usageAdditive, ContextUsed: count(10)}); ctx.Window != nil {
		t.Fatalf("no model named anywhere: the window is unknown, not guessed: %+v", ctx)
	}
}

func TestTaskUsageIsSafeUnderConcurrentReports(t *testing.T) {
	totals := NewTaskUsageTotals(nil)
	totals.Start("t1", "fakealpha", "")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			totals.Apply("t1", usageEvent(ChatUsage{Accumulation: usageAdditive, TokenClasses: harvest.TokenClasses{Input: count(1)}}))
		}()
	}
	wg.Wait()
	event, _ := totals.Apply("t1", usageEvent(ChatUsage{Accumulation: usageAdditive, TokenClasses: harvest.TokenClasses{Input: count(0)}}))
	if total := event["turn_total"].(TaskUsageTotal); *total.Input != 50 {
		t.Fatalf("input = %d", *total.Input)
	}
	totals.Drop("t1")
	if _, ok := totals.Apply("t1", usageEvent(ChatUsage{Accumulation: usageAdditive})); ok {
		t.Fatal("a dropped task must be forgotten")
	}
}

func TestUsageEventPayloadIsNeutralJSON(t *testing.T) {
	totals := NewTaskUsageTotals(nil)
	totals.Start("t1", "fakealpha", "")
	event, _ := totals.Apply("t1", usageEvent(ChatUsage{Accumulation: usageAdditive, TokenClasses: harvest.TokenClasses{CacheWrite: count(3)}}))
	encoded, _ := json.Marshal(event)
	body := string(encoded)
	for _, want := range []string{`"type":"usage"`, `"cache_write":3`, `"turn_total"`, `"context"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("%s missing %s", body, want)
		}
	}
}

// ChatUsage embeds harvest.TokenClasses (token-usage-analytics plan §3.2); the
// usage.delta payload's JSON must not change with it.
func TestChatUsageJSONIsUnchangedByTheSharedClasses(t *testing.T) {
	body, err := json.Marshal(ChatUsage{Accumulation: usageAdditive,
		TokenClasses: harvest.TokenClasses{Input: count(1), CacheRead: count(2), CacheWrite: count(3),
			Output: count(4), Reasoning: count(5)}, ContextUsed: count(6), ModelID: "m"})
	if err != nil {
		t.Fatal(err)
	}
	const golden = `{"accumulation":"additive","input":1,"cache_read":2,"cache_write":3,"output":4,"reasoning":5,"context_used":6,"model_id":"m"}`
	if string(body) != golden {
		t.Fatalf("usage.delta JSON changed:\n got %s\nwant %s", body, golden)
	}
}
