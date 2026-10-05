package daemon

// Turn progress: what the model is doing INSIDE one open turn, refined from a
// running execution. It is a transient, time-ordered reading of the block
// boundaries the vendor already wrote to its transcript — not content (that
// is the transcript's), not a durable fact (that is the turn table's and
// session_status.go's), not transport (that is the stream's).
//
// Placement (in-turn-progress plan §B.2): a sibling of the status decider,
// pure over boundaries, composed by the live stream for the open session
// only. It never touches the store and never reaches the rail; a test pins
// the first, the fold's call sites pin the second. Vendor record names never
// appear here — the harvest adapters have already turned them into canonical
// kinds and RFC 3339 stamps.

import (
	"time"

	"crossing-guard/harvest"
)

const (
	progressThinking = "thinking"
	progressWriting  = "writing"
	progressTool     = "tool"
)

// turnBoundary is one completed block of the open turn.
type turnBoundary struct {
	Kind  string // user | thinking | assistant | tool_call | tool_result
	AtMS  int64  // vendor record stamp in ms; 0 when the vendor gave none
	Index int    // position in the transcript window; the in-record tie-break
	Name  string // tool name for tool_call
}

// turnProgress is the decided refinement; Progress "" means absent.
type turnProgress struct {
	Progress string
	Tool     string
}

// turnBoundaries reads the newest `window` canonical events and keeps the
// blocks of the OPEN turn: the last user prompt and everything after it. A
// summary (compaction) resets the turn — only blocks after it count. Kinds
// outside the five are ignored.
func turnBoundaries(events []harvest.CanonicalEvent, window int) []turnBoundary {
	if window <= 0 || len(events) == 0 {
		return nil
	}
	first := 0
	if len(events) > window {
		first = len(events) - window
	}
	var out []turnBoundary
	for i := first; i < len(events); i++ {
		event := events[i]
		switch event.Kind {
		case "user":
			out = out[:0] // a new prompt opens a new turn
			out = append(out, turnBoundary{Kind: event.Kind, AtMS: eventTimeMS(event.Ts), Index: i})
		case "summary":
			out = out[:0] // compaction: nothing before it describes the open turn
		case "thinking", "assistant", "tool_call", "tool_result":
			out = append(out, turnBoundary{Kind: event.Kind, AtMS: eventTimeMS(event.Ts), Index: i, Name: event.Name})
		}
	}
	return out
}

// eventTimeMS parses the canonical RFC 3339 stamp adapters place on events.
// An absent or unparseable stamp is 0, which the rules treat as "no clock".
func eventTimeMS(ts string) int64 {
	if ts == "" {
		return 0
	}
	if at, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		return at.UnixMilli()
	}
	if at, err := time.Parse(time.RFC3339, ts); err == nil {
		return at.UnixMilli()
	}
	return 0
}

// decideTurnProgress applies the ordered, total rule list of plan §B.3.
//
//  1. execution is not running            → absent (progress refines running only)
//  2. attention is approval               → absent (the ask renders alone)
//  3. a tool call has no result yet       → tool, named by the OLDEST outstanding call
//  4. newest block is user or tool_result → thinking
//  5. newest block is thinking/assistant  → writing
//  6. newest block older than quiet       → absent (execution decays with it)
//
// Rule 6 is checked before 3–5 so a stale open call cannot read as running.
func decideTurnProgress(execution, attention string, boundaries []turnBoundary, now time.Time, quiet time.Duration) turnProgress {
	if execution != "running" || attention == "approval" || len(boundaries) == 0 {
		return turnProgress{}
	}
	newest := boundaries[len(boundaries)-1]
	// Decay reads the newest STAMPED boundary; a turn with no stamps at all
	// has no clock and makes no claim.
	newestStamp := int64(0)
	for _, boundary := range boundaries {
		if boundary.AtMS > newestStamp {
			newestStamp = boundary.AtMS
		}
	}
	if newestStamp == 0 || (quiet > 0 && now.UnixMilli()-newestStamp > quiet.Milliseconds()) {
		return turnProgress{}
	}
	// Canonical events carry no call id, so calls and results pair by ORDER:
	// the k-th result is taken to answer the k-th call. That is an assumption,
	// not a fact — with results returning out of order the named tool can be
	// one that already finished. It is stated here so nobody reads it as more.
	var calls []turnBoundary
	results := 0
	for _, boundary := range boundaries {
		switch boundary.Kind {
		case "tool_call":
			calls = append(calls, boundary)
		case "tool_result":
			results++
		}
	}
	if results < len(calls) {
		return turnProgress{Progress: progressTool, Tool: calls[results].Name}
	}
	switch newest.Kind {
	case "user", "tool_result":
		return turnProgress{Progress: progressThinking}
	case "thinking", "assistant":
		return turnProgress{Progress: progressWriting}
	}
	return turnProgress{}
}

// liveEvent is the live stream's wire shape for one canonical event: the
// event itself plus the transient thought duration, which is computed from
// boundary stamps and never stored or indexed.
type liveEvent struct {
	harvest.CanonicalEvent
	ThoughtMS int64 `json:"thought_ms,omitempty"`
	// Facts are the detector facts (key:value) a tool_call row classifies as.
	// Set on GET /api/session rows and on stream deltas only; snapshot rows
	// carry none, because the client reads them for their sequence alone.
	Facts []string `json:"facts,omitempty"`
}

// annotateThoughts attaches "thought for" durations. For each thinking block,
// the duration is its stamp minus the previous block's stamp. When the vendor
// supplied thinking text the duration sits on that event; when it did not
// (a signature-only block the renderer never shows) it sits on the next
// content block, so it renders where the thought sat. Durations under
// minMS are omitted rather than shown as "0s".
func annotateThoughts(events []harvest.CanonicalEvent, minMS int64) []liveEvent {
	out := make([]liveEvent, len(events))
	for i, event := range events {
		out[i] = liveEvent{CanonicalEvent: event}
	}
	pending := int64(0)
	for i, event := range events {
		if event.Kind == "user" || event.Kind == "summary" {
			pending = 0 // a thought never crosses into the next turn or past a compaction
			continue
		}
		if event.Kind == "thinking" && i > 0 {
			at, prev := eventTimeMS(event.Ts), eventTimeMS(events[i-1].Ts)
			if at > 0 && prev > 0 && at-prev >= minMS {
				if event.Text != "" {
					out[i].ThoughtMS = at - prev
				} else {
					pending = at - prev
				}
			}
			continue
		}
		if pending > 0 && (event.Kind == "assistant" || event.Kind == "tool_call") {
			out[i].ThoughtMS = pending
			pending = 0
		}
	}
	return out
}
