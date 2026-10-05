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
	releaseSeen := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-CG-Token") != "fixture-token" {
			http.Error(w, "wrong endpoint", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/api/v1/approvals/grant/revoke" {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			releaseSeen <- body["grant_token"]
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path != "/api/v1/approvals/request" {
			http.Error(w, "wrong endpoint", http.StatusNotFound)
			return
		}
		var wire approvalWireRequest
		if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		requestSeen <- RuntimeApprovalRequest{Runtime: wire.Runtime, TaskID: wire.TaskID,
			ToolCallID: wire.ToolCallID, ToolName: wire.ToolName, Action: wire.Action,
			Targets: wire.Targets, ApprovalReason: wire.ApprovalReason,
			OfferExactRunGrant: wire.OfferExactRunGrant, GrantToken: wire.GrantToken}
		_ = json.NewEncoder(w).Encode(RuntimeApprovalResult{Decision: "allowed", GrantID: "run_exact", GrantToken: wire.GrantToken})
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
		ToolName: "FixtureTool", Timeout: time.Second, Action: "Change fixture",
		Targets: []string{"fixture.txt"}, ApprovalReason: "Fixture requires approval.",
		OfferExactRunGrant: true, GrantToken: "arg_" + strings.Repeat("A", 26),
	})
	if err != nil || result.Decision != "allowed" || result.GrantID != "run_exact" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	request := <-requestSeen
	if request.Runtime != "fixture-runtime" || request.TaskID != "task_fixture" ||
		request.ToolCallID != "call_fixture" || request.ToolName != "FixtureTool" ||
		request.Action != "Change fixture" || len(request.Targets) != 1 ||
		request.Targets[0] != "fixture.txt" || request.ApprovalReason != "Fixture requires approval." ||
		!request.OfferExactRunGrant || request.GrantToken == "" {
		t.Fatalf("request=%+v", request)
	}
	if err := ReleaseRuntimeApprovalGrantFromDataDir(context.Background(), dataDir, request.GrantToken); err != nil {
		t.Fatal(err)
	}
	if released := <-releaseSeen; released != request.GrantToken {
		t.Fatalf("released token = %q", released)
	}
}
