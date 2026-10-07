package harvest

// The neutral usage vocabulary (runtime-model-catalog-and-usage design §4.1,
// §4.3; token-usage-analytics plan §3.2). Adapters map their runtime's own
// fields into it; nothing here knows a runtime, a currency, or a price. The
// well-known token classes are disjoint: input excludes cache, output includes
// reasoning, and reasoning is informational only — never added to a total.

import (
	"sort"
	"time"
)

// Well-known token class ids. They name the classes of TokenClasses and are
// what a TokenPart's Of refers to.
const (
	TokenClassInput      = "input"
	TokenClassCacheRead  = "cache-read"
	TokenClassCacheWrite = "cache-write"
	TokenClassOutput     = "output"
	TokenClassReasoning  = "reasoning"
)

// TokenClasses is the disjoint well-known class set, nil when the runtime did
// not state a class. ChatUsage embeds it too, so live and recorded usage share
// one type (catalog design S-RT12).
type TokenClasses struct {
	Input      *int64 `json:"input,omitempty"`
	CacheRead  *int64 `json:"cache_read,omitempty"`
	CacheWrite *int64 `json:"cache_write,omitempty"`
	Output     *int64 `json:"output,omitempty"`
	// Reasoning is the part of Output spent on reasoning — never added.
	Reasoning *int64 `json:"reasoning,omitempty"`
}

// UsageCall is one model call as a runtime stated it (plan §3.2). Counts are
// nil when not stated. Every id and label is opaque to the framework.
type UsageCall struct {
	ID            string // unique within the runtime; required
	Session       string // the session whose agent made the call
	ParentSession string // set when that session is itself delegated by another
	Agent         string // delegated-agent id inside Session; "" = its own agent
	// FirstAt is when the first line of this call appeared in its source; At is
	// when the call completed. FirstAt orders copies of one call (plan §3.5).
	FirstAt, At time.Time
	Model       string // opaque; "" = not stated
	Effort      string // requested reasoning effort, opaque; "" = not stated
	Client      string // opaque client version; "" = not stated
	TokenClasses
	ContextWindow *int64       // a level, never summed
	Parts         []TokenPart  // informational parts of one class, never added
	Other         []TokenCount // additive classes outside the well-known set
	Cost          *Cost
}

// Delegated reports whether another agent's work made this call: an agent
// inside the session, or a session that has a parent.
func (c UsageCall) Delegated() bool { return c.Agent != "" || c.ParentSession != "" }

// TokenPart is a labelled part of one well-known class, such as one cache
// lifetime of cache-write. The framework sums parts by ID within Of and never
// adds them to a total.
type TokenPart struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Of    string `json:"of"`
	Count int64  `json:"count"`
}

// AdmitUsageCall applies the framework's call rules (plan §3.2) and reports
// whether the call is recorded at all:
//   - a call whose four classes are all zero or unstated is not a call (the
//     all-zero placeholder lines runtimes write before the real one);
//   - a call with no completion time cannot be placed in time or ordered
//     against its copies, so it is not recorded (code red-team C-8);
//   - reasoning larger than output cannot be a part of it, so it is unknown.
func AdmitUsageCall(call UsageCall) (UsageCall, bool) {
	if call.At.IsZero() {
		return call, false
	}
	counted := false
	for _, value := range []*int64{call.Input, call.CacheRead, call.CacheWrite, call.Output} {
		if value != nil && *value != 0 {
			counted = true
		}
	}
	if !counted {
		return call, false
	}
	if call.Reasoning != nil && call.Output != nil && *call.Reasoning > *call.Output {
		call.Reasoning = nil
	}
	return call, true
}

// FoldCalls sums calls into one SessionUsage: every class where stated, other
// classes by id, cost within one unit and basis, and the levels (model,
// context, window) from the latest call. Nil when there are no calls.
func FoldCalls(calls []UsageCall) *SessionUsage {
	if len(calls) == 0 {
		return nil
	}
	ordered := append([]UsageCall(nil), calls...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].At.Before(ordered[j].At) })
	usage := &SessionUsage{}
	mixedCost := false
	for _, call := range ordered {
		usage.Turns++
		usage.InputTokens += valueOf(call.Input)
		usage.CacheRead += valueOf(call.CacheRead)
		usage.CacheCreate += valueOf(call.CacheWrite)
		usage.OutputTokens += valueOf(call.Output)
		if call.Reasoning != nil {
			sum := *call.Reasoning
			if usage.Reasoning != nil {
				sum += *usage.Reasoning
			}
			usage.Reasoning = &sum
			usage.ReasoningStatedCalls++
		}
		usage.Other = AddTokenCounts(usage.Other, append([]TokenCount(nil), call.Other...))
		addSessionCost(usage, call.Cost, &mixedCost)
		if call.Model != "" {
			usage.Model = call.Model
		}
		usage.Context = valueOf(call.Input) + valueOf(call.CacheRead) + valueOf(call.CacheWrite)
		if call.ContextWindow != nil {
			usage.ContextWindow = *call.ContextWindow
		}
	}
	finishUsage(usage)
	return usage
}

func valueOf(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

// Cost bases. A runtime-stated cost is what the runtime computed and reported;
// it is not an invoice. An estimated cost is derived by the framework and is
// never summed with a runtime-stated one.
const (
	CostBasisRuntime   = "runtime"
	CostBasisEstimated = "estimated"
)

// Token sides for classes outside the well-known set.
const (
	TokenSideInput  = "input"
	TokenSideOutput = "output"
)

// Cost is an amount in an opaque unit code (an ISO 4217 code or any unit the
// runtime states) with the basis it was produced on. Absent cost is nil.
type Cost struct {
	Amount float64 `json:"amount"`
	Unit   string  `json:"unit"`
	Basis  string  `json:"basis"`
}

// TokenCount is one runtime-specific class outside the well-known set. The
// framework sums these by ID and renders Label; it never interprets the ID.
type TokenCount struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Side  string `json:"side"`
	Count int64  `json:"count"`
}

// CostTotal is one summed cost line of a report: one unit, one basis.
type CostTotal struct {
	Unit   string  `json:"unit"`
	Basis  string  `json:"basis"`
	Amount float64 `json:"amount"`
}

// CostCoverage says how many sessions with telemetry stated a cost at all.
type CostCoverage struct {
	WithCost    int `json:"with_cost"`
	WithoutCost int `json:"without_cost"`
}

// AddTokenCounts merges other-class counts by ID; the first label and side seen
// for an ID win. The result is sorted by ID so output is deterministic.
func AddTokenCounts(total, next []TokenCount) []TokenCount {
	if len(next) == 0 {
		return total
	}
	index := make(map[string]int, len(total))
	for i, count := range total {
		index[count.ID] = i
	}
	for _, count := range next {
		if i, ok := index[count.ID]; ok {
			total[i].Count += count.Count
			continue
		}
		index[count.ID] = len(total)
		total = append(total, count)
	}
	sort.Slice(total, func(i, j int) bool { return total[i].ID < total[j].ID })
	return total
}

// addUsageCounts adds next's additive counts into total. Context, window and
// model are levels, not amounts, and are left to the caller.
func addUsageCounts(total, next *SessionUsage) {
	total.Turns += next.Turns
	total.InputTokens += next.InputTokens
	total.OutputTokens += next.OutputTokens
	total.CacheRead += next.CacheRead
	total.CacheCreate += next.CacheCreate
	if next.Reasoning != nil {
		sum := *next.Reasoning
		if total.Reasoning != nil {
			sum += *total.Reasoning
		}
		total.Reasoning = &sum
	}
	total.Other = AddTokenCounts(total.Other, next.Other)
}

// addSessionCost adds a cost to a session-level cost. Two costs in different
// units or bases cannot become one number, so the session cost becomes
// unknown (nil) and mixed reports true.
func addSessionCost(total *SessionUsage, next *Cost, mixed *bool) {
	if next == nil || *mixed {
		return
	}
	if total.Cost == nil {
		copied := *next
		total.Cost = &copied
		return
	}
	if total.Cost.Unit != next.Unit || total.Cost.Basis != next.Basis {
		total.Cost, *mixed = nil, true
		return
	}
	total.Cost.Amount += next.Amount
}
