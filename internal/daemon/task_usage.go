package daemon

// TaskUsageTotals owns the per-task usage arithmetic (runtime-model-catalog-
// and-usage plan §2.1, S-RT2). TaskApplicationService holds no lock of its own;
// per-task state lives in collaborators that do, like TaskDeltaBuffer. This one
// turns an adapter's usage reports into additive deltas, keeps the task's
// running total, and joins a context window. It never knows a runtime's
// reporting style beyond the accumulation mode the adapter declared.

import (
	"sort"
	"sync"

	"crossing-guard/harvest"
)

// TaskUsageTotal is a task's summed usage so far. Cost holds one line per unit
// and basis; nothing sums across them.
type TaskUsageTotal struct {
	Input      *int64               `json:"input,omitempty"`
	CacheRead  *int64               `json:"cache_read,omitempty"`
	CacheWrite *int64               `json:"cache_write,omitempty"`
	Output     *int64               `json:"output,omitempty"`
	Reasoning  *int64               `json:"reasoning,omitempty"`
	Other      []harvest.TokenCount `json:"other,omitempty"`
	Cost       []harvest.CostTotal  `json:"cost,omitempty"`
}

// TaskUsageContext is the latest context occupancy and the window it is
// measured against; either may be unknown.
type TaskUsageContext struct {
	Used   *int64 `json:"used,omitempty"`
	Window *int64 `json:"window,omitempty"`
}

type taskUsageState struct {
	runtime, model string
	mode           string    // fixed by the first report
	previous       ChatUsage // the last running total, for differencing
	total          TaskUsageTotal
	context        TaskUsageContext
}

// TaskUsageTotals is safe for concurrent use.
type TaskUsageTotals struct {
	mu     sync.Mutex
	tasks  map[string]*taskUsageState
	models func(runtime, id string) (ChatModelOption, bool)
}

func NewTaskUsageTotals(models func(runtime, id string) (ChatModelOption, bool)) *TaskUsageTotals {
	return &TaskUsageTotals{tasks: map[string]*taskUsageState{}, models: models}
}

// Start registers a task with the model its request named ("" = the runtime's
// default), used for the window join when a report names none.
func (t *TaskUsageTotals) Start(taskID, runtime, model string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tasks[taskID] = &taskUsageState{runtime: runtime, model: model}
}

// Drop forgets a finished task.
func (t *TaskUsageTotals) Drop(taskID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.tasks, taskID)
}

// Apply turns one report into the event that is persisted and published:
// {type: usage, delta, turn_total, context}. It returns false, and the report
// is dropped, when the payload is not a ChatUsage, the task is unknown, the
// mode is unknown, or the mode changes mid-task.
func (t *TaskUsageTotals) Apply(taskID string, event ChatEvent) (ChatEvent, bool) {
	report, ok := event["delta"].(ChatUsage)
	if !ok {
		return nil, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	state := t.tasks[taskID]
	if state == nil || (report.Accumulation != usageAdditive && report.Accumulation != usageRunningTotal) {
		return nil, false
	}
	if state.mode == "" {
		state.mode = report.Accumulation
	} else if state.mode != report.Accumulation {
		return nil, false
	}
	delta := report
	if state.mode == usageRunningTotal {
		delta = differenceUsage(report, state.previous)
		state.previous = report
	}
	delta.Accumulation = usageAdditive
	addUsageTotal(&state.total, delta)
	if delta.ContextUsed != nil {
		state.context.Used = delta.ContextUsed
	}
	switch {
	case delta.ContextWindow != nil:
		state.context.Window = delta.ContextWindow
	case state.context.Window == nil && t.models != nil:
		model := delta.ModelID
		if model == "" {
			model = state.model
		}
		if model != "" {
			if option, found := t.models(state.runtime, model); found && option.Limits != nil {
				state.context.Window = option.Limits.ContextTokens
			}
		}
	}
	total := copyUsageTotal(state.total)
	return ChatEvent{"type": "usage", "delta": delta, "turn_total": total, "context": state.context}, true
}

// differenceUsage turns a running total into the amount added since the
// previous one (S-RT6). A figure lower than before makes that class unknown for
// this delta, never zero. Context fields are levels and pass through.
func differenceUsage(current, previous ChatUsage) ChatUsage {
	diff := func(now, before *int64) *int64 {
		switch {
		case now == nil:
			return nil
		case before == nil:
			return int64Pointer(*now)
		case *now < *before:
			return nil
		}
		return int64Pointer(*now - *before)
	}
	delta := ChatUsage{Accumulation: current.Accumulation, TokenClasses: harvest.TokenClasses{
		Input: diff(current.Input, previous.Input), CacheRead: diff(current.CacheRead, previous.CacheRead),
		CacheWrite: diff(current.CacheWrite, previous.CacheWrite), Output: diff(current.Output, previous.Output),
		Reasoning: diff(current.Reasoning, previous.Reasoning)}, ContextUsed: current.ContextUsed,
		ContextWindow: current.ContextWindow, ModelID: current.ModelID}
	before := map[string]int64{}
	for _, other := range previous.Other {
		before[other.ID] = other.Count
	}
	for _, other := range current.Other {
		if prior, seen := before[other.ID]; seen {
			if other.Count < prior {
				continue
			}
			other.Count -= prior
		}
		delta.Other = append(delta.Other, other)
	}
	if current.Cost != nil {
		switch prior := previous.Cost; {
		case prior == nil:
			copied := *current.Cost
			delta.Cost = &copied
		case prior.Unit == current.Cost.Unit && prior.Basis == current.Cost.Basis && current.Cost.Amount >= prior.Amount:
			delta.Cost = &harvest.Cost{Amount: current.Cost.Amount - prior.Amount, Unit: current.Cost.Unit, Basis: current.Cost.Basis}
		}
	}
	return delta
}

// addUsageTotal adds an additive delta. Reasoning is summed as its own figure
// and never into output, which already includes it.
func addUsageTotal(total *TaskUsageTotal, delta ChatUsage) {
	add := func(sum **int64, value *int64) {
		if value == nil {
			return
		}
		next := *value
		if *sum != nil {
			next += **sum
		}
		*sum = &next
	}
	add(&total.Input, delta.Input)
	add(&total.CacheRead, delta.CacheRead)
	add(&total.CacheWrite, delta.CacheWrite)
	add(&total.Output, delta.Output)
	add(&total.Reasoning, delta.Reasoning)
	total.Other = harvest.AddTokenCounts(total.Other, delta.Other)
	if cost := delta.Cost; cost != nil {
		for i := range total.Cost {
			if total.Cost[i].Unit == cost.Unit && total.Cost[i].Basis == cost.Basis {
				total.Cost[i].Amount += cost.Amount
				return
			}
		}
		total.Cost = append(total.Cost, harvest.CostTotal{Unit: cost.Unit, Basis: cost.Basis, Amount: cost.Amount})
		sort.Slice(total.Cost, func(i, j int) bool {
			if total.Cost[i].Unit != total.Cost[j].Unit {
				return total.Cost[i].Unit < total.Cost[j].Unit
			}
			return total.Cost[i].Basis < total.Cost[j].Basis
		})
	}
}

// copyUsageTotal detaches the published total from the state that keeps growing.
func copyUsageTotal(total TaskUsageTotal) TaskUsageTotal {
	copyPointer := func(value *int64) *int64 {
		if value == nil {
			return nil
		}
		return int64Pointer(*value)
	}
	return TaskUsageTotal{Input: copyPointer(total.Input), CacheRead: copyPointer(total.CacheRead),
		CacheWrite: copyPointer(total.CacheWrite), Output: copyPointer(total.Output),
		Reasoning: copyPointer(total.Reasoning), Other: append([]harvest.TokenCount(nil), total.Other...),
		Cost: append([]harvest.CostTotal(nil), total.Cost...)}
}
