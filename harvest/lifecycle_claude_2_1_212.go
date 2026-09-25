package harvest

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

func proposedClaudeEffects(tool string, input map[string]any) []LifecycleEffect {
	path := anyString(input["file_path"])
	if path == "" {
		return nil
	}
	switch tool {
	case "Edit":
		before, after := []byte(anyString(input["old_string"])), []byte(anyString(input["new_string"]))
		return []LifecycleEffect{{RawIdentity: path, Operation: "update", EvidenceSource: "derived-input",
			SourceField: "tool_input.file_path", Completeness: "partial", ReplaceAll: input["replace_all"] == true,
			ReplacementBefore: before, ReplacementAfter: after, BeforeBytes: len(before), AfterBytes: len(after), BeforeDigest: lifecycleDigest(before),
			AfterDigest: lifecycleDigest(after), DiffCompleteness: "unavailable"}}
	case "Write":
		content := []byte(anyString(input["content"]))
		return []LifecycleEffect{{RawIdentity: path, Operation: "write", EvidenceSource: "derived-input",
			SourceField: "tool_input.file_path", Completeness: "complete", ContentPayload: content,
			ContentBytes: len(content), ContentDigest: lifecycleDigest(content), DiffCompleteness: "unavailable"}}
	}
	return nil
}

func normalizeClaudeLifecycle(path string) (LifecycleBatch, error) {
	return normalizeClaudeLifecycleIncremental(LifecycleReadRequest{Path: path, MaxBytes: 1<<62 - 1, MaxRecords: int(^uint(0) >> 1)})
}

func normalizeClaudeLifecycleIncremental(request LifecycleReadRequest) (LifecycleBatch, error) {
	segment := strings.TrimSuffix(filepath.Base(request.Path), filepath.Ext(request.Path))
	request.Runtime = "claude"
	request.SourceSegmentID = segment
	state, err := decodeLifecycleState(request.State)
	if err != nil {
		return LifecycleBatch{}, err
	}
	reader, err := newLifecycleRecordReader(request, state.Discard)
	if err != nil {
		return LifecycleBatch{}, err
	}
	defer func() { _ = reader.Close() }()
	path := request.Path
	batch := LifecycleBatch{Runtime: "claude", CanonicalSessionID: segment, SourceSegmentID: segment, SourcePath: path}
	for {
		record, ok, readErr := reader.Next()
		if readErr != nil {
			return LifecycleBatch{}, readErr
		}
		if !ok {
			break
		}
		var obj map[string]any
		if json.Unmarshal(record, &obj) != nil {
			batch.MalformedRecords++
			continue
		}
		supplied := anyString(obj["sessionId"])
		msg, _ := obj["message"].(map[string]any)
		items, _ := msg["content"].([]any)
		for ordinal, item := range items {
			m, _ := item.(map[string]any)
			if m == nil {
				continue
			}
			switch anyString(m["type"]) {
			case "tool_use":
				id, tool := anyString(m["id"]), anyString(m["name"])
				input, _ := m["input"].(map[string]any)
				rememberLifecycleCall(&state, id, lifecycleCallState{Tool: tool, Session: supplied,
					Effects: proposedClaudeEffects(tool, input)})
			case "tool_result":
				id := anyString(m["tool_use_id"])
				call := state.Calls[id]
				completionState, errorClass := "success", ""
				if m["is_error"] == true {
					completionState, errorClass = "failure", "runtime-reported"
				}
				raw, decoded := resultPayloadMeta(m["content"])
				r := newTranscriptResult("claude", batch.CanonicalSessionID, supplied, segment,
					"vendor-transcript", path, sourceSequence(int(reader.sourceLine), ordinal), id, "tool_use_id", call.Tool,
					completionState, errorClass, anyString(obj["timestamp"]), record, raw, decoded)
				r.Effects = append(r.Effects, call.Effects...)
				for i := range r.Effects {
					r.Effects[i].Ordinal = i
				}
				batch.Results = append(batch.Results, r)
				forgetLifecycleCall(&state, id)
			}
		}
	}
	if !request.RetainEffectBodies {
		for id, call := range state.Calls {
			for index := range call.Effects {
				call.Effects[index].ReplacementBefore = nil
				call.Effects[index].ReplacementAfter = nil
				call.Effects[index].ContentPayload = nil
				call.Effects[index].DiffPayload = nil
			}
			state.Calls[id] = call
		}
	}
	state.Discard = reader.discard
	priorCalls := make(map[string]lifecycleCallState, len(state.Calls))
	for id, call := range state.Calls {
		priorCalls[id] = call
	}
	stateBody, order, evicted, err := boundedLifecycleState(state.Calls, state.Order,
		batch.CanonicalSessionID, state.Discard)
	if err != nil {
		return LifecycleBatch{}, err
	}
	for _, id := range evicted {
		batch.EvictedCalls = append(batch.EvictedCalls, newEvictedCallObservation("claude",
			batch.CanonicalSessionID, segment, path, request.SourceGeneration, id, priorCalls[id]))
	}
	state.Order = order
	batch.ParserState = stateBody
	if err := reader.finish(&batch); err != nil {
		return LifecycleBatch{}, err
	}
	return batch, nil
}

func (claudeRuntime) NormalizeLifecycleIncremental(request LifecycleReadRequest) (LifecycleBatch, error) {
	return normalizeClaudeLifecycleIncremental(request)
}
