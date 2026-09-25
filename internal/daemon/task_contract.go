package daemon

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

type ChatEvent map[string]any

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
	default:
		return "coverage.gap"
	}
}

func taskRequestDigest(req ChatRequest) string {
	req.IdempotencyKey = ""
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
