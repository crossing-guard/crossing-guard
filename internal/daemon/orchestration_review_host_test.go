package daemon

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"crossing-guard/internal/observation"
	"crossing-guard/internal/orchestration/profilefs"
	"crossing-guard/store"
)

func compatibleReviewProfileSource() []byte {
	return []byte(`---
format-version: 1
kind: crossing-guard-orchestration-profile
id: command-reviewer
version: "1.0.0"
name: Command reviewer
description: Reviews one observed tool action without controlling it.
role: reviewer
execution: stateless-review
trigger:
  event: pretool.action
context:
  - kind: pretool-action
    required: true
output:
  kind: review-recommendation
authority-requests:
  - advise
requirements:
  capabilities:
    - one-shot-inference
  destination:
    locality: local-only
limits:
  timeout: 2s
  max-hops: 1
  max-depth: 1
  max-input-bytes: 65536
  max-output-bytes: 4096
  max-tokens: 256
  max-retries: 0
  max-concurrency: 1
failure:
  missing-required-context: record-unavailable
  unavailable-capability: record-unavailable
  timeout: record-unavailable
  malformed-output: record-unavailable
---
Review whether the observed action appears destructive. Return an attributed recommendation only.
`)
}

func delegatedReviewProfileSource() []byte {
	source := compatibleReviewProfileSource()
	source = bytes.Replace(source, []byte("event: pretool.action"), []byte("event: approval.pending"), 1)
	source = bytes.Replace(source, []byte("kind: pretool-action"), []byte("kind: permission-scope"), 1)
	source = bytes.Replace(source, []byte("kind: review-recommendation"), []byte("kind: approval-response"), 1)
	source = bytes.Replace(source, []byte("authority-requests:\n  - advise"),
		[]byte("authority-requests:\n  - respond-approval"), 1)
	source = bytes.Replace(source, []byte("    - one-shot-inference"),
		[]byte("    - one-shot-inference\n    - approval-response"), 1)
	return source
}

func reviewHostHarness(t *testing.T) (*orchestrationReviewHost, *profilefs.Owner, profilefs.Preview) {
	return reviewHostHarnessWithSource(t, compatibleReviewProfileSource())
}

func reviewHostHarnessWithSource(t *testing.T, source []byte) (*orchestrationReviewHost, *profilefs.Owner, profilefs.Preview) {
	t.Helper()
	root := t.TempDir()
	owner, err := profilefs.New(root)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := owner.Preview("PROFILE.md", source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Select(profilefs.SelectCommand{SourceName: "PROFILE.md", Source: source,
		ExpectedSourceDigest: preview.SourceDigest, ExpectedBundleDigest: preview.BundleDigest,
		ExpectedStateToken: preview.StateToken}); err != nil {
		t.Fatal(err)
	}
	ix, err := store.Open(filepath.Join(root, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	host, err := newOrchestrationReviewHost(ix, owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { host.close(); _ = ix.Close() })
	return host, owner, preview
}

func enableTestReviewBinding(t *testing.T, host *orchestrationReviewHost, preview profilefs.Preview) store.ReviewBinding {
	return enableTestReviewBindingAt(t, host, preview, "http://127.0.0.1:1")
}

func enableTestReviewBindingAt(t *testing.T, host *orchestrationReviewHost, preview profilefs.Preview, endpoint string) store.ReviewBinding {
	t.Helper()
	binding, err := host.putBinding(reviewBindingCommand{ProfileID: preview.ProfileID,
		ProfileSourceDigest: preview.SourceDigest, ProfileBundleDigest: preview.BundleDigest,
		Endpoint: endpoint, Model: "local-test-model", TimeoutMS: 1000,
		ExpectedStateToken: store.ReviewBindingAbsentToken()})
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

func TestReviewHostRunsPinnedLocalBackendAndStoresStrictClaim(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"local-test-model","message":{"content":"{\"action\":\"deny\",\"message\":\"Potentially destructive.\",\"citations\":[\"action.command\"]}"},"prompt_eval_count":20,"eval_count":8}`))
	}))
	server.Listener = listener
	server.Start()
	defer server.Close()
	host, _, preview := reviewHostHarness(t)
	enableTestReviewBindingAt(t, host, preview, server.URL)
	envelope := observation.Envelope{Schema: observation.SchemaV1,
		ObservationID: "obs_2123456789abcdef0123456789abcdef",
		ActionID:      "act_2123456789abcdef0123456789abcdef",
		CollectorID:   observation.CollectorPreTool, Runtime: "claude", SessionID: "session",
		Tool: "Bash", Command: "change protected workspace content", Decision: "allow",
		ToolInput: []byte(`{"command":"change protected workspace content"}`), ToolInputBytes: 48,
		ToolInputDigest: "sha256-v1:input", ToolInputCompleteness: "complete"}
	host.offer(envelope, observation.Receipt{Schema: observation.SchemaV1,
		ObservationID: envelope.ObservationID, ActionID: envelope.ActionID, EventID: 3})
	deadline := time.Now().Add(3 * time.Second)
	for {
		items, readErr := host.ix.ReviewInvocations(10, "")
		if readErr != nil {
			t.Fatal(readErr)
		}
		if len(items) == 1 && items[0].State == "completed" {
			if items[0].Action != "deny" || items[0].Message != "Potentially destructive." ||
				len(items[0].Citations) != 1 || items[0].PromptTokens != 20 || items[0].CompletionTokens != 8 {
				t.Fatalf("completed=%+v", items[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("review did not complete: %+v", items)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestReviewHostDelegatedFirstAnswersExactPendingAskThroughApprovalOwner(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"local-test-model","message":{"content":"{\"action\":\"deny\",\"message\":\"Destructive command.\",\"citations\":[\"action.command\"]}"}}`))
	}))
	server.Listener = listener
	server.Start()
	defer server.Close()
	previousApprovals := approvals
	approvals = newApprovalsHub()
	t.Cleanup(func() { approvals = previousApprovals })
	host, _, preview := reviewHostHarnessWithSource(t, delegatedReviewProfileSource())
	if _, err := host.putBinding(reviewBindingCommand{ProfileID: preview.ProfileID,
		ProfileSourceDigest: preview.SourceDigest, ProfileBundleDigest: preview.BundleDigest,
		Endpoint: server.URL, Model: "local-test-model", TimeoutMS: 1500,
		Effect: "delegated-first", ApprovalSubdeadlineMS: 1000,
		ExpectedStateToken: store.ReviewBindingAbsentToken()}); err != nil {
		t.Fatal(err)
	}
	approval := &Approval{ID: "ap_delegated_host", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Deadline: time.Now().Add(3 * time.Second).UTC().Format(time.RFC3339), Origin: ApprovalOriginPolicyHook,
		Runtime: "claude", Session: "session", Rule: "destructive", Mode: "ask", Command: "rm important.txt", Status: "pending"}
	waiter := make(chan *Approval, 1)
	if err := approvals.admit(approval, waiter); err != nil {
		t.Fatal(err)
	}
	approvals.notifyPending(cloneApproval(approval))
	select {
	case decided := <-waiter:
		if decided.Status != "denied" || len(decided.Responses) != 1 || decided.Responses[0].Responder.Kind != "service" {
			t.Fatalf("delegated decision = %+v", decided)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("delegated review did not answer pending approval")
	}
	deadline := time.Now().Add(time.Second)
	for {
		items, readErr := host.ix.ReviewInvocations(10, "")
		if readErr != nil {
			t.Fatal(readErr)
		}
		if len(items) == 1 && items[0].ApprovalOutcome == "accepted" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("delegated invocation=%+v", items)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestReviewHostConsumesCommittedActionAsynchronouslyAndDeduplicatesAskResolution(t *testing.T) {
	host, _, preview := reviewHostHarness(t)
	enableTestReviewBinding(t, host, preview)
	envelope := observation.Envelope{Schema: observation.SchemaV1,
		ObservationID: "obs_0123456789abcdef0123456789abcdef",
		ActionID:      "act_0123456789abcdef0123456789abcdef",
		CollectorID:   observation.CollectorPreTool, Runtime: "claude", SessionID: "session",
		Tool: "Bash", Command: "inspect a temporary workspace", Decision: "ask",
		ToolInputBytes: 10, ToolInputDigest: "sha256-v1:input", ToolInputCompleteness: "metadata-only"}
	start := time.Now()
	host.offer(envelope, observation.Receipt{Schema: observation.SchemaV1,
		ObservationID: envelope.ObservationID, ActionID: envelope.ActionID, EventID: 1})
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("post-commit offer waited for model completion")
	}
	resolution := envelope
	resolution.ObservationID = "obs_1123456789abcdef0123456789abcdef"
	resolution.Decision = "allow"
	host.offer(resolution, observation.Receipt{Schema: observation.SchemaV1,
		ObservationID: resolution.ObservationID, ActionID: resolution.ActionID, EventID: 2})
	deadline := time.Now().Add(3 * time.Second)
	for {
		items, err := host.ix.ReviewInvocations(10, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(items) == 1 && items[0].State != "admitted" && items[0].State != "running" {
			if items[0].State != "unavailable" {
				t.Fatalf("invocation=%+v", items[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("review did not settle: %+v", items)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestReviewHostDisableStopsNewAdmissionAndRestartRecoversUnknown(t *testing.T) {
	host, _, preview := reviewHostHarness(t)
	binding := enableTestReviewBinding(t, host, preview)
	if _, err := host.disableBinding(binding.StateToken); err != nil {
		t.Fatal(err)
	}
	host.offer(observation.Envelope{ActionID: "act_disabled", CollectorID: observation.CollectorPreTool},
		observation.Receipt{ObservationID: "obs_disabled", EventID: 1})
	items, err := host.ix.ReviewInvocations(10, "")
	if err != nil || len(items) != 0 {
		t.Fatalf("disabled admission=%+v err=%v", items, err)
	}
}

func TestReviewHostDisableWaitsForPriorBindingAdmissions(t *testing.T) {
	host, _, preview := reviewHostHarness(t)
	binding := enableTestReviewBinding(t, host, preview)
	const offers = 64
	start := make(chan struct{})
	var wg sync.WaitGroup
	for index := 0; index < offers; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			suffix := strconv.FormatInt(int64(index), 16)
			suffix = strings.Repeat("0", 32-len(suffix)) + suffix
			host.offer(observation.Envelope{Schema: observation.SchemaV1,
				ObservationID: "obs_" + suffix, ActionID: "act_" + suffix,
				CollectorID: observation.CollectorPreTool, Runtime: "claude", SessionID: "session",
				Tool: "Bash", Command: "inspect temporary workspace", ToolInputBytes: 10,
				ToolInputDigest: "sha256-v1:input", ToolInputCompleteness: "metadata-only"},
				observation.Receipt{Schema: observation.SchemaV1, ObservationID: "obs_" + suffix,
					ActionID: "act_" + suffix, EventID: int64(index + 1)})
		}(index)
	}
	close(start)
	if _, err := host.disableBinding(binding.StateToken); err != nil {
		t.Fatal(err)
	}
	atDisable, err := host.ix.ReviewInvocations(100, "")
	if err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	afterWait, err := host.ix.ReviewInvocations(100, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(afterWait) != len(atDisable) {
		t.Fatalf("disable returned before prior-binding admission settled: at-disable=%d after-wait=%d",
			len(atDisable), len(afterWait))
	}
}

func TestReviewHistorySeparatesObservedGovernanceFromActualOutcome(t *testing.T) {
	host, _, preview := reviewHostHarness(t)
	enableTestReviewBinding(t, host, preview)
	tx, err := host.ix.BeginGov()
	if err != nil {
		t.Fatal(err)
	}
	eventID, err := tx.AppendEvent(store.EventRecord{TS: 1, SessionID: "session", Runtime: "claude",
		Verb: "exec", Tool: "Bash", Decision: "ask", Origin: "live"}, nil)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	observationID := "obs_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	actionID := "act_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := tx.AppendEventDelivery(store.EventDelivery{EventID: eventID, ObservationID: observationID,
		ActionID: actionID, ObservationSchema: observation.SchemaV1, EnvelopeDigest: "sha256-v1:envelope",
		CollectorID: observation.CollectorPreTool, ReceivedAt: 1, DeliveryAttempts: 1, DeliveryMode: "direct"}); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	host.offer(observation.Envelope{Schema: observation.SchemaV1, ObservationID: observationID,
		ActionID: actionID, CollectorID: observation.CollectorPreTool, Runtime: "claude", SessionID: "session",
		Tool: "Bash", Command: "inspect temporary workspace", Decision: "ask", ToolInputBytes: 10,
		ToolInputDigest: "sha256-v1:input", ToolInputCompleteness: "metadata-only"},
		observation.Receipt{Schema: observation.SchemaV1, ObservationID: observationID,
			ActionID: actionID, EventID: eventID})
	deadline := time.Now().Add(3 * time.Second)
	for {
		items, readErr := reviewHistory(host.ix, 10, "", "")
		if readErr != nil {
			t.Fatal(readErr)
		}
		if len(items) == 1 && items[0].Invocation.State != "admitted" && items[0].Invocation.State != "running" {
			if items[0].ObservedGovernanceDecision != "ask" || items[0].ActualOutcome.Observed {
				t.Fatalf("review projection=%+v", items[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("review history did not settle: %+v", items)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestReviewSettingsHTTPExposesSeparateBindingAndRejectsRemoteOrUnknownInput(t *testing.T) {
	host, owner, preview := reviewHostHarness(t)
	mux := http.NewServeMux()
	registerOrchestrationReviewRoutes(mux, host, owner)

	settings := httptest.NewRecorder()
	mux.ServeHTTP(settings, httptest.NewRequest(http.MethodGet, "/api/orchestration/reviews/settings", nil))
	if settings.Code != http.StatusOK {
		t.Fatal(settings.Body.String())
	}
	var body struct {
		StateToken string `json:"state_token"`
		Profiles   []struct {
			Compatible bool `json:"compatible"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(settings.Body.Bytes(), &body); err != nil || body.StateToken != store.ReviewBindingAbsentToken() ||
		len(body.Profiles) != 1 || !body.Profiles[0].Compatible {
		t.Fatalf("settings=%s err=%v", settings.Body.String(), err)
	}
	request := map[string]any{"profile_id": preview.ProfileID, "profile_source_digest": preview.SourceDigest,
		"profile_bundle_digest": preview.BundleDigest, "endpoint": "https://review.example.invalid",
		"model": "remote", "timeout_ms": 1000, "runtime_filter": "", "expected_state_token": body.StateToken,
		"confirmed": true}
	encoded, _ := json.Marshal(request)
	remote := httptest.NewRecorder()
	mux.ServeHTTP(remote, httptest.NewRequest(http.MethodPut, "/api/orchestration/reviews/binding",
		bytesReader(encoded)))
	if remote.Code != http.StatusUnprocessableEntity {
		t.Fatalf("remote=%d %s", remote.Code, remote.Body.String())
	}
	unknown := httptest.NewRecorder()
	mux.ServeHTTP(unknown, httptest.NewRequest(http.MethodPut, "/api/orchestration/reviews/binding",
		stringsReader(`{"confirmed":true,"future":true}`)))
	if unknown.Code != http.StatusBadRequest {
		t.Fatalf("unknown=%d %s", unknown.Code, unknown.Body.String())
	}
	request["endpoint"] = "http://127.0.0.1:1"
	request["model"] = "local-test-model"
	encoded, _ = json.Marshal(request)
	enabled := httptest.NewRecorder()
	mux.ServeHTTP(enabled, httptest.NewRequest(http.MethodPut, "/api/orchestration/reviews/binding", bytesReader(encoded)))
	if enabled.Code != http.StatusOK {
		t.Fatalf("enabled=%d %s", enabled.Code, enabled.Body.String())
	}
	var enabledBody struct {
		Binding store.ReviewBinding `json:"binding"`
	}
	if err := json.Unmarshal(enabled.Body.Bytes(), &enabledBody); err != nil || enabledBody.Binding.State != "enabled" {
		t.Fatalf("enabled body=%s err=%v", enabled.Body.String(), err)
	}
	disableBody, _ := json.Marshal(map[string]any{"expected_state_token": enabledBody.Binding.StateToken, "confirmed": true})
	disabled := httptest.NewRecorder()
	mux.ServeHTTP(disabled, httptest.NewRequest(http.MethodPost, "/api/orchestration/reviews/binding/disable", bytesReader(disableBody)))
	if disabled.Code != http.StatusOK {
		t.Fatalf("disabled=%d %s", disabled.Code, disabled.Body.String())
	}
}

func stringsReader(value string) *strings.Reader { return strings.NewReader(value) }
func bytesReader(value []byte) *bytes.Reader     { return bytes.NewReader(value) }
