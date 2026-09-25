package harvest

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

func (codexRuntime) LifecycleAliasRepairContracts() []LifecycleAliasRepairContract {
	return []LifecycleAliasRepairContract{{Runtime: "codex", ResultSourceKind: "vendor-patch-result",
		ResultNativeCallKind: "patch_call_id", AliasNativeCallKind: "tool_use_id",
		Tool: "apply_patch", Algorithm: "codex-patch-exec-id-v1"}}
}

func (codexRuntime) LifecycleActionCollectionContracts() []LifecycleActionCollectionContract {
	return []LifecycleActionCollectionContract{{Runtime: "codex", ParserVersion: 1,
		ResultSourceKind: "vendor-transcript", ResultNativeCallKind: "call_id"}}
}

func codexOperation(kind string) string {
	switch strings.ToLower(kind) {
	case "add", "added", "create", "created":
		return "create"
	case "update", "updated", "modify", "modified":
		return "update"
	case "delete", "deleted", "remove", "removed":
		return "delete"
	case "move", "moved", "rename", "renamed":
		return "move"
	default:
		return "unknown"
	}
}

func codexPatchEffects(changes any) []LifecycleEffect {
	items, _ := changes.([]any)
	if byPath, ok := changes.(map[string]any); ok {
		paths := make([]string, 0, len(byPath))
		for path := range byPath {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		items = make([]any, 0, len(paths))
		for _, path := range paths {
			item, _ := byPath[path].(map[string]any)
			copyItem := map[string]any{"path": path}
			for key, value := range item {
				copyItem[key] = value
			}
			items = append(items, copyItem)
		}
	}
	out := make([]LifecycleEffect, 0, len(items))
	for ordinal, item := range items {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		path := anyString(m["path"])
		if path == "" {
			path = anyString(m["file_path"])
		}
		diff := []byte(anyString(m["diff"]))
		if len(diff) == 0 {
			diff = []byte(anyString(m["unified_diff"]))
		}
		content := []byte(anyString(m["content"]))
		kind := anyString(m["kind"])
		if kind == "" {
			kind = anyString(m["type"])
		}
		e := LifecycleEffect{Ordinal: ordinal, RawIdentity: path, Operation: codexOperation(kind),
			MoveTarget: anyString(m["move_path"]), EvidenceSource: "runtime-result", SourceField: "changes",
			Completeness: "complete", DiffCompleteness: "unavailable"}
		if len(diff) > 0 {
			e.DiffPayload, e.DiffBytes, e.DiffDigest, e.DiffCompleteness = diff, len(diff), lifecycleDigest(diff), "complete"
		}
		if len(content) > 0 {
			e.ContentPayload, e.ContentBytes, e.ContentDigest = content, len(content), lifecycleDigest(content)
		}
		out = append(out, e)
	}
	return out
}

func normalizeCodexLifecycle(path string) (LifecycleBatch, error) {
	return normalizeCodexLifecycleIncremental(LifecycleReadRequest{Path: path, MaxBytes: 1<<62 - 1, MaxRecords: int(^uint(0) >> 1)})
}

func normalizeCodexLifecycleIncremental(request LifecycleReadRequest) (LifecycleBatch, error) {
	segment := strings.TrimSuffix(filepath.Base(request.Path), filepath.Ext(request.Path))
	request.Runtime = "codex"
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
	batch := LifecycleBatch{Runtime: "codex", CanonicalSessionID: state.Canonical,
		SourceSegmentID: segment, SourcePath: path}
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
		recordView, decoded := decodeCodexRecord(obj)
		if !decoded {
			continue
		}
		containerType, itype, inner := recordView.Envelope, recordView.Kind, recordView.Body
		if containerType == "session_meta" || itype == "session_meta" {
			batch.CanonicalSessionID = anyString(inner["session_id"])
			if batch.CanonicalSessionID == "" {
				batch.CanonicalSessionID = anyString(inner["id"])
			}
			state.Canonical = batch.CanonicalSessionID
			continue
		}
		if containerType == "response_item" {
			switch itype {
			case "function_call", "local_shell_call", "custom_tool_call":
				id := anyString(inner["call_id"])
				if id == "" {
					id = anyString(inner["id"])
				}
				tool := anyString(inner["name"])
				if tool == "" {
					tool = itype
				}
				var input any
				switch itype {
				case "function_call":
					input = inner["arguments"]
				case "custom_tool_call":
					input = inner["input"]
				case "local_shell_call":
					input = inner["action"]
				}
				batch.Actions = append(batch.Actions, newTranscriptAction("codex", batch.CanonicalSessionID,
					"", segment, path, sourceSequence(int(reader.sourceLine), 0), id, "call_id", tool,
					anyString(obj["timestamp"]), input, record))
				rememberLifecycleCall(&state, id, lifecycleCallState{Tool: tool})
			case "function_call_output", "custom_tool_call_output", "local_shell_call_output":
				id := anyString(inner["call_id"])
				if id == "" {
					id = anyString(inner["id"])
				}
				raw, decoded := resultPayloadMeta(inner["output"])
				completionState, errorClass := "success", ""
				if strings.EqualFold(anyString(inner["status"]), "failed") || inner["is_error"] == true {
					completionState, errorClass = "failure", "runtime-reported"
				}
				batch.Results = append(batch.Results, newTranscriptResult("codex", batch.CanonicalSessionID, "", segment,
					"vendor-transcript", path, sourceSequence(int(reader.sourceLine), 0), id, "call_id", state.Calls[id].Tool,
					completionState, errorClass, anyString(obj["timestamp"]), record, raw, decoded))
				forgetLifecycleCall(&state, id)
			}
		}
		if containerType == "event_msg" && itype == "patch_apply_end" {
			id := anyString(inner["call_id"])
			if id == "" {
				id = anyString(inner["id"])
			}
			completionState, errorClass := "success", ""
			if inner["success"] == false || strings.EqualFold(anyString(inner["status"]), "failed") {
				completionState, errorClass = "failure", "runtime-reported"
			}
			raw, decoded := resultPayloadMeta(inner["stdout"])
			r := newTranscriptResult("codex", batch.CanonicalSessionID, "", segment, "vendor-patch-result",
				path, sourceSequence(int(reader.sourceLine), 0), id, "patch_call_id", "apply_patch", completionState, errorClass,
				anyString(obj["timestamp"]), record, raw, decoded)
			if id != "" {
				r.NativeCallAliases = []NativeCallAlias{{NativeCallKind: "tool_use_id",
					NativeCallID: id, Algorithm: "codex-patch-exec-id-v1"}}
			}
			stdout, stderr := []byte(anyString(inner["stdout"])), []byte(anyString(inner["stderr"]))
			r.StdoutBytes, r.StdoutDigest = len(stdout), lifecycleDigest(stdout)
			r.StderrBytes, r.StderrDigest = len(stderr), lifecycleDigest(stderr)
			r.Effects = codexPatchEffects(inner["changes"])
			batch.Results = append(batch.Results, r)
			forgetLifecycleCall(&state, id)
		}
	}
	if batch.CanonicalSessionID == "" {
		batch.CanonicalSessionID = segment
	}
	state.Canonical = batch.CanonicalSessionID
	state.Discard = reader.discard
	priorCalls := make(map[string]lifecycleCallState, len(state.Calls))
	for id, call := range state.Calls {
		priorCalls[id] = call
	}
	stateBody, order, evicted, err := boundedLifecycleState(state.Calls, state.Order,
		state.Canonical, state.Discard)
	if err != nil {
		return LifecycleBatch{}, fmt.Errorf("encode codex lifecycle state: %w", err)
	}
	for _, id := range evicted {
		batch.EvictedCalls = append(batch.EvictedCalls, newEvictedCallObservation("codex",
			batch.CanonicalSessionID, segment, path, request.SourceGeneration, id, priorCalls[id]))
	}
	state.Order = order
	batch.ParserState = stateBody
	if err := reader.finish(&batch); err != nil {
		return LifecycleBatch{}, err
	}
	return batch, nil
}

func (codexRuntime) NormalizeLifecycleIncremental(request LifecycleReadRequest) (LifecycleBatch, error) {
	return normalizeCodexLifecycleIncremental(request)
}
