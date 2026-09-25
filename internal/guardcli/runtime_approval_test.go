package guardcli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRuntimeApprovalUsesExactDaemonDataDirectory(t *testing.T) {
	requestSeen := make(chan RuntimeApprovalRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/approvals/request" || r.Header.Get("X-CG-Token") != "fixture-token" {
			http.Error(w, "wrong endpoint", http.StatusUnauthorized)
			return
		}
		var wire approvalWireRequest
		if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		requestSeen <- RuntimeApprovalRequest{Runtime: wire.Runtime, TaskID: wire.TaskID,
			ToolCallID: wire.ToolCallID, ToolName: wire.ToolName}
		_ = json.NewEncoder(w).Encode(RuntimeApprovalResult{Decision: "allowed"})
	}))
	defer server.Close()

	dataDir := t.TempDir()
	addr := strings.TrimPrefix(server.URL, "http://")
	if err := os.WriteFile(filepath.Join(dataDir, "daemon-addr"), []byte(addr), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "api-token"), []byte("fixture-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := RequestRuntimeApprovalFromDataDir(context.Background(), dataDir, RuntimeApprovalRequest{
		Runtime: "fixture-runtime", TaskID: "task_fixture", ToolCallID: "call_fixture",
		ToolName: "FixtureTool", Timeout: time.Second,
	})
	if err != nil || result.Decision != "allowed" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	request := <-requestSeen
	if request.Runtime != "fixture-runtime" || request.TaskID != "task_fixture" ||
		request.ToolCallID != "call_fixture" || request.ToolName != "FixtureTool" {
		t.Fatalf("request=%+v", request)
	}
}
