package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"crossing-guard/harvest"
	"crossing-guard/internal/orchestration"
	"crossing-guard/store"
)

// managedContextReader adapts existing resource owners to the orchestration port.
// It owns no lifecycle, persistence, memory search or event subscription.
type managedContextReader struct {
	ix    *store.Index
	tasks *TaskApplicationService
}

func (reader *managedContextReader) ReadContext(ctx context.Context, request orchestration.ContextRequest) (orchestration.ContextEnvelope, error) {
	result := orchestration.ContextEnvelope{Source: request.Source, Items: []orchestration.PromptContext{}}
	result.Source.FinalMessage = ""
	// The transcript cutoff is per request (one reader serves every
	// goroutine): the pinned value comes in on the source and the value
	// supplied leaves on the envelope.
	transcriptSeq := request.Source.TranscriptSeq
	selections := append([]orchestration.ContextSelection{{Kind: "operator.group_notes", MaxBytes: 8192}}, request.Selections...)
	finalSelected := false
	for _, selection := range selections {
		finalSelected = finalSelected || selection.Kind == "task.final-response"
	}
	if !finalSelected {
		selections = append(selections, orchestration.ContextSelection{Kind: "task.final-response", MaxBytes: 262144})
	}
	for _, selection := range selections {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		coverage := orchestration.ContextCoverage{Kind: selection.Kind, State: "empty", ReadAt: time.Now().UnixMilli()}
		label, body, detail, seq, err := reader.readSelection(request, selection)
		if seq > 0 {
			transcriptSeq = seq
		}
		coverage.Detail = detail
		if err != nil {
			coverage.State, coverage.Detail = "unavailable", err.Error()
		} else {
			var truncated bool
			empty := body == ""
			if selection.Kind == "task.messages" {
				var window struct {
					Omitted int64             `json:"omitted_events"`
					Events  []json.RawMessage `json:"events"`
				}
				_ = json.Unmarshal([]byte(body), &window)
				truncated = window.Omitted > 0
				empty = len(window.Events) == 0 && window.Omitted == 0
			} else {
				body, truncated = boundedContextText(body, selection.MaxBytes)
			}
			if !empty {
				coverage.State = "supplied"
			}
			if truncated {
				coverage.State = "truncated"
			}
			if selection.Kind == "task.final-response" {
				result.Source.FinalMessage = body
			} else if body != "" {
				result.Items = append(result.Items, orchestration.PromptContext{Label: label, Body: body})
			}
		}
		result.Coverage = append(result.Coverage, coverage)
		if err != nil && selection.Required {
			return result, fmt.Errorf("required %s: %w", selection.Kind, err)
		}
	}
	result.TranscriptSeq = transcriptSeq
	return result, nil
}

// Bound text by its encoded JSON-string size, retaining complete UTF-8 runes.
func boundedContextText(body string, maxBytes int) (string, bool) {
	encoded, _ := json.Marshal(body)
	if len(encoded) <= maxBytes {
		return body, false
	}
	runes := []rune(body)
	low, high := 0, len(runes)
	for low < high {
		mid := (low + high + 1) / 2
		encoded, _ = json.Marshal(string(runes[:mid]))
		if len(encoded) <= maxBytes {
			low = mid
		} else {
			high = mid - 1
		}
	}
	return string(runes[:low]), body != ""
}

func (reader *managedContextReader) readSelection(request orchestration.ContextRequest, selection orchestration.ContextSelection) (label, body, detail string, transcriptSeq int64, err error) {
	detail = "read-current at read_at; may change on retry"
	if selection.MaxBytes < 2 {
		return "", "", "", 0, errors.New("context byte limit cannot contain an encoded string")
	}
	if selection.Selector != "" {
		return "", "", "", 0, errors.New("named context selector unavailable: " + selection.Selector)
	}
	switch selection.Kind {
	case "session.messages":
		// The session transcript through the harvest owner, on BOTH paths
		// (plan D4): a console task and a terminal session supply the same
		// shape, user turns included. Addressed by the group root identity —
		// the same address delivery uses — never by task ownership.
		label = "context.session_messages"
		body, detail, transcriptSeq, err = reader.sessionMessagesContext(request, selection.MaxBytes)
	case "task.final-response", "task.messages":
		detail = fmt.Sprintf("observed task events through sequence %d; pinned across retries", request.Source.Sequence)
		if reader.tasks == nil {
			err = errors.New("task context unavailable")
			break
		}
		if selection.Kind == "task.final-response" {
			body, err = reader.tasks.taskMessageThrough(request.Source.TaskID, request.Source.Sequence)
		} else {
			label = "context.task_messages"
			body, err = reader.tasks.taskMessagesContext(request.Source.TaskID, request.Source.Sequence, selection.MaxBytes)
		}
	case "operator.group_notes":
		label = "operator.group_notes"
		notes, readErr := reader.ix.ManagedGroupNotes(request.GroupID, false)
		err = readErr
		for _, note := range notes {
			body += "- " + note.Body + "\n"
		}
	case "prior-claims":
		label = "context.prior_claims"
		detail += "; at most 3 matching completed claims in newest 200 global runs"
		runs, readErr := reader.ix.ManagedRuns(200)
		err = readErr
		count := 0
		for _, run := range runs {
			if run.GroupID != request.GroupID || run.State != "completed" || strings.TrimSpace(run.Message) == "" {
				continue
			}
			body += "[" + run.Action + "] " + run.Message + "\n"
			count++
			if count == 3 {
				break
			}
		}
	case "session.tags":
		label = "session.tags"
		tags, readErr := reader.ix.ActiveOrchestrationTags(request.SessionID, time.Now().Unix())
		err = readErr
		for _, tag := range tags {
			body += "- " + tag.Tag + " (" + tag.AgentKey + ")\n"
		}
	default:
		err = errors.New("context provider unavailable: " + selection.Kind)
	}
	return
}

// sessionMessagesContext reads the newest transcript events of the source
// session, bounded by the configured tail and the selection's encoded byte
// limit. The cutoff is pinned on first read: the highest Seq supplied is
// recorded on the run (source_transcript_seq) and a retry truncates to it, so
// a relaunched helper never sees events after its trigger (red-team H5).
func (reader *managedContextReader) sessionMessagesContext(request orchestration.ContextRequest, maxBytes int) (string, string, int64, error) {
	runtime, id := request.Source.Runtime, request.Source.CatalogSessionID
	if id == "" {
		id = request.Source.NativeSessionID
	}
	if runtime == "" || id == "" {
		return "", "", 0, errors.New("session transcript identity unavailable")
	}
	started := time.Now()
	detailSession, err := LoadSession(runtime, id)
	if err != nil {
		return "", "", 0, fmt.Errorf("session transcript unavailable: %w", err)
	}
	body, highest, omitted, since, err := boundTranscriptEvents(detailSession.Events, request.Source.TranscriptSeq,
		request.Source.TranscriptSince, orchestrationConfig().Context.TranscriptTailEvents, maxBytes)
	detail := fmt.Sprintf("transcript read at read_at (%dms); the vendor file may lag the hook; newest %d events kept, %d older omitted; pinned through seq %d",
		time.Since(started).Milliseconds(), orchestrationConfig().Context.TranscriptTailEvents, omitted, highest)
	switch {
	case request.Source.TranscriptSince > 0 && since > 0:
		detail += fmt.Sprintf("; continuation delta after seq %d", since)
	case request.Source.TranscriptSince > 0:
		detail += fmt.Sprintf("; continuation delta unavailable (since %d is at or past the transcript head) — configured tail supplied instead", request.Source.TranscriptSince)
	}
	return body, detail, highest, err
}

// boundTranscriptEvents is the pure half of session.messages: keep the
// conversation kinds at or before the pinned sequence (zero pins nothing and
// the highest sequence supplied becomes the pin), then the newest tail, then
// shrink to the encoded byte limit oldest-first. Coverage metadata always
// survives; an unsatisfiable limit is an error, never an invented body.
// A continuation turn (sinceSeq > 0) drops events at or below sinceSeq — the
// helper session already saw them — unless that leaves nothing at all (the
// vendor file lags the hook, or a re-parse renumbered the transcript): then
// the pinned tail is supplied and the applied lower bound reports zero.
func boundTranscriptEvents(events []harvest.CanonicalEvent, pinSeq, sinceSeq int64, tail, maxBytes int) (body string, highest int64, omitted int, appliedSince int64, err error) {
	if pinSeq > 0 {
		end := 0
		for end < len(events) && int64(events[end].Seq) <= pinSeq {
			end++
		}
		events = events[:end]
	}
	if len(events) > 0 {
		highest = int64(events[len(events)-1].Seq)
	}
	if sinceSeq > 0 && sinceSeq < highest {
		start := 0
		for start < len(events) && int64(events[start].Seq) <= sinceSeq {
			start++
		}
		events = events[start:]
		appliedSince = sinceSeq
	}
	rows := []map[string]any{}
	for _, event := range events {
		switch event.Kind {
		case "user", "assistant", "tool_call", "tool_result":
			rows = append(rows, map[string]any{"seq": event.Seq, "kind": event.Kind, "name": event.Name, "text": event.Text, "full_len": event.FullLen})
		}
	}
	if tail > 0 && len(rows) > tail {
		omitted = len(rows) - tail
		rows = rows[omitted:]
	}
	for {
		encodedBody, marshalErr := json.Marshal(map[string]any{"coverage": "session transcript events (user, assistant, tool calls and results) through the pinned sequence; payloads are clipped by the harvest owner and disclose full_len",
			"through_seq": highest, "omitted_events": omitted, "events": rows})
		if marshalErr != nil {
			return "", highest, omitted, appliedSince, marshalErr
		}
		encoded, marshalErr := json.Marshal(string(encodedBody))
		if marshalErr != nil {
			return "", highest, omitted, appliedSince, marshalErr
		}
		if len(encoded) <= maxBytes {
			return string(encodedBody), highest, omitted, appliedSince, nil
		}
		if len(rows) == 0 {
			return "", highest, omitted, appliedSince, errors.New("session.messages byte limit cannot contain coverage metadata")
		}
		rows = rows[1:]
		omitted++
	}
}

// sourceTranscriptSeq is the pinned transcript cutoff a first read recorded.
func sourceTranscriptSeq(detail map[string]any) int64 {
	switch value := detail["source_transcript_seq"].(type) {
	case int64:
		return value
	case float64:
		return int64(value)
	case int:
		return int64(value)
	}
	return 0
}

// sourceSequence is the admitted event cutoff, not the source task's current head.
func sourceSequence(detail map[string]any) int64 {
	switch value := detail["source_sequence"].(type) {
	case int64:
		return value
	case float64:
		return int64(value)
	case int:
		return int64(value)
	}
	return 0
}
