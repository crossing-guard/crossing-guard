package guardcli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"crossing-guard/engine"
	"crossing-guard/internal/collectionconfig"
	"crossing-guard/internal/observation"
)

func decodedResultBytes(raw json.RawMessage) []byte {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []byte(text)
	}
	return append([]byte(nil), raw...)
}

func resultStreamMeta(raw json.RawMessage, field string) (int, string) {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return 0, ""
	}
	value, found := object[field]
	if !found {
		return 0, ""
	}
	var stream string
	if json.Unmarshal(value, &stream) != nil {
		return 0, ""
	}
	body := []byte(stream)
	return len(body), observation.DigestBytes(body)
}

func retainEffectBody(mode collectionconfig.Mode, retained *int, body []byte) []byte {
	if mode == collectionconfig.MetadataOnly || len(body) == 0 || *retained+len(body) > observation.MaxRetainedInput {
		return nil
	}
	*retained += len(body)
	return append([]byte(nil), body...)
}

func directResultEffects(in hookInput, mode collectionconfig.Mode, retained *int) []observation.ResultEffect {
	tool := engine.BareTool(in.ToolName)
	path := in.ToolInput.FilePath
	if path == "" {
		return nil
	}
	switch tool {
	case "Edit":
		before, after := []byte(in.ToolInput.OldString), []byte(in.ToolInput.NewString)
		effect := observation.ResultEffect{Ordinal: 0, RawIdentity: path, Operation: "update",
			EvidenceSource: "derived-input", SourceField: "tool_input.file_path", Completeness: "partial",
			ReplaceAll: in.ToolInput.ReplaceAll, BeforeBytes: len(before), BeforeDigest: observation.DigestBytes(before),
			AfterBytes: len(after), AfterDigest: observation.DigestBytes(after), DiffCompleteness: "unavailable"}
		effect.BeforePayload = retainEffectBody(mode, retained, before)
		effect.AfterPayload = retainEffectBody(mode, retained, after)
		if len(effect.BeforePayload)+len(effect.AfterPayload) == 0 {
			effect.Completeness = "metadata-only"
		}
		return []observation.ResultEffect{effect}
	case "Write":
		content := []byte(in.ToolInput.Content)
		effect := observation.ResultEffect{Ordinal: 0, RawIdentity: path, Operation: "write",
			EvidenceSource: "derived-input", SourceField: "tool_input.file_path", Completeness: "complete",
			ContentBytes: len(content), ContentDigest: observation.DigestBytes(content), DiffCompleteness: "unavailable"}
		effect.ContentPayload = retainEffectBody(mode, retained, content)
		if len(effect.ContentPayload) == 0 && len(content) > 0 {
			effect.Completeness = "metadata-only"
		}
		return []observation.ResultEffect{effect}
	}
	return nil
}

func buildResultEnvelope(in hookInput) (observation.ResultEnvelope, error) {
	id, err := newRecordID("res_")
	if err != nil {
		return observation.ResultEnvelope{}, err
	}
	nativeID, nativeKind := in.ToolUseID, "tool_use_id"
	if nativeID == "" {
		nativeID, nativeKind = in.CallID, "call_id"
		if nativeID == "" {
			nativeKind = ""
		}
	}
	raw := append([]byte(nil), in.RawToolResponse...)
	decoded := decodedResultBytes(raw)
	stdoutBytes, stdoutDigest := resultStreamMeta(raw, "stdout")
	stderrBytes, stderrDigest := resultStreamMeta(raw, "stderr")
	state, errorClass := "success", ""
	if in.ToolIsError || in.HookEventName == "PostToolUseFailure" {
		state, errorClass = "failure", "runtime-reported"
	}
	segment := strings.TrimSuffix(filepath.Base(in.TranscriptPath), filepath.Ext(in.TranscriptPath))
	now := time.Now().Unix()
	loaded, err := collectionconfig.Load(dataDir())
	if err != nil {
		return observation.ResultEnvelope{}, err
	}
	retained := 0
	envelope := observation.ResultEnvelope{Schema: observation.ResultSchemaV1, ObservationID: id,
		CollectorID: observation.CollectorPostTool, CollectorVersion: collectorVersion(), Runtime: in.Runtime,
		SessionID: in.SessionID, SuppliedSessionID: in.SessionID, SourceSegmentID: segment,
		HookEventName: in.HookEventName, TranscriptPath: in.TranscriptPath, Cwd: in.Cwd, NativeCallID: nativeID,
		NativeCallKind: nativeKind, Tool: in.ToolName, State: state, ErrorClass: errorClass,
		CompletedAt: now, DurationMS: in.DurationMS, MediaType: observation.InputMediaTypeJSON, RawEnvelopeBytes: in.RawEnvelopeBytes,
		RawFieldBytes: len(raw), DecodedBytes: len(decoded), RetainedBytes: 0,
		ExternalLocatorBytes: len(in.TranscriptPath), PayloadDigest: observation.DigestBytes(decoded),
		Completeness: "metadata-only", StdoutBytes: stdoutBytes, StdoutDigest: stdoutDigest,
		StderrBytes: stderrBytes, StderrDigest: stderrDigest, QueuedAt: now,
		DeliveryAttempts: 1, DeliveryMode: "direct", Carrier: in.Carrier}
	envelope.Effects = directResultEffects(in, loaded.Document.ResultPayloadMode, &retained)
	if loaded.Document.ResultPayloadMode == collectionconfig.CompleteBounded && len(decoded) > 0 && retained+len(decoded) <= observation.MaxRetainedInput {
		envelope.Payload = append([]byte(nil), decoded...)
		envelope.RetainedBytes = len(envelope.Payload)
		envelope.Completeness = "complete"
	}
	return envelope, nil
}

func spoolResult(e observation.ResultEnvelope) (string, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	return spoolRecord(e.ObservationID, b)
}

func sendResult(addr, token string, e observation.ResultEnvelope) (observation.ResultReceipt, error) {
	var receipt observation.ResultReceipt
	body, err := json.Marshal(e)
	if err != nil {
		return receipt, err
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/api/govern/result/v1", bytes.NewReader(body))
	if err != nil {
		return receipt, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-CG-Token", token)
	}
	req.Header.Set(observation.HookDeadlineHeader, observation.HookDeadlineValue(time.Now()))
	resp, err := (&http.Client{Timeout: observation.HookDeliveryBudget}).Do(req)
	if err != nil {
		return receipt, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return receipt, fmt.Errorf("result rejected (%s): %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&receipt); err != nil {
		return receipt, err
	}
	if receipt.Schema != observation.ResultSchemaV1 || receipt.ObservationID != e.ObservationID || receipt.ResultID == 0 {
		return receipt, fmt.Errorf("result acknowledgement did not confirm observation identity")
	}
	return receipt, nil
}

func emitResult(in hookInput) []observation.Delivery {
	if os.Getenv("CG_OBSERVE") == "0" {
		return nil
	}
	envelope, err := buildResultEnvelope(in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[crossing-guard] result envelope failed:", err)
		return nil
	}
	spoolPath, err := spoolResult(envelope)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[crossing-guard] result spool failed:", err)
		return nil
	}
	addr, token, ok := daemonEndpoint()
	if !ok {
		return nil
	}
	receipt, err := sendResult(addr, token, envelope)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[crossing-guard] result pending:", err)
		return nil
	}
	if err := os.Remove(spoolPath); err == nil {
		_ = syncObservationSpoolDir(filepath.Dir(spoolPath))
	}
	return receipt.Deliveries
}
