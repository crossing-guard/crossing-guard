package daemon

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"crossing-guard/harvest"
)

type ChatEvent map[string]any

// Usage accumulation modes (runtime-model-catalog-and-usage design §4.4). An
// adapter declares which one its runtime's reports follow; the framework does
// the arithmetic, so no adapter keeps state and no browser sums anything.
const (
	// usageAdditive: summing every report of a task gives the task's total.
	usageAdditive = "additive"
	// usageRunningTotal: each report is the task's total so far.
	usageRunningTotal = "running-total-within-task"
)

// ChatUsage is the one typed usage payload an adapter emits, as
// ChatEvent{"type": "usage", "delta": ChatUsage{...}}. Token classes are the
// well-known disjoint set (input excludes cache; output includes reasoning;
// reasoning is informational and never added to a total). Every field is
// optional: nil means the runtime did not state it.
type ChatUsage struct {
	Accumulation string `json:"accumulation"`
	// The class set is harvest's, shared with recorded calls (catalog design
	// S-RT12); embedding keeps this payload's JSON unchanged.
	harvest.TokenClasses
	Other         []harvest.TokenCount `json:"other,omitempty"`
	Cost          *harvest.Cost        `json:"cost,omitempty"`
	ContextUsed   *int64               `json:"context_used,omitempty"`   // a level: latest wins
	ContextWindow *int64               `json:"context_window,omitempty"` // a level; wins over the model list
	ModelID       string               `json:"model_id,omitempty"`       // opaque; empty when unknown or spanning models
}

func usageEvent(delta ChatUsage) ChatEvent { return ChatEvent{"type": "usage", "delta": delta} }

func int64Pointer(value int64) *int64 { return &value }

// statedCount reads a non-negative JSON number an adapter found in its
// runtime's event; anything else is "not stated" (nil).
func statedCount(value any) *int64 {
	number, ok := value.(float64)
	if !ok || number < 0 || number != number {
		return nil
	}
	return int64Pointer(int64(number))
}

// statedAmount reads a non-negative JSON number as an amount.
func statedAmount(value any) (float64, bool) {
	number, ok := value.(float64)
	return number, ok && number >= 0 && number == number
}

// sumStated adds stated counts; the sum is stated if any part is.
func sumStated(values ...*int64) *int64 {
	var total *int64
	for _, value := range values {
		if value == nil {
			continue
		}
		if total == nil {
			total = int64Pointer(0)
		}
		*total += *value
	}
	return total
}

func taskEventKind(event ChatEvent) string {
	switch anyString(event["type"]) {
	case "session", "spawn", "stderr":
		return "task.activity"
	case "delta":
		return "message.delta"
	case "text":
		return "message.completed"
	case "thinking_delta":
		return "reasoning.delta"
	case "thinking":
		return "reasoning.completed"
	case "tool":
		return "tool.started"
	case "tool_result":
		return "tool.completed"
	case "auth_required":
		return "coverage.gap"
	case "result":
		return "task.activity"
	case "usage":
		return "usage.delta"
	default:
		return "coverage.gap"
	}
}

func taskRequestDigest(req ChatRequest) string {
	req.IdempotencyKey = ""
	req.SessionEffortToken = ""
	encoded, _ := json.Marshal(req)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func newTaskID() string {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		panic(fmt.Sprintf("generate runtime task id: %v", err))
	}
	return "task_" + hex.EncodeToString(value)
}
