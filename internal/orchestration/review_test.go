package orchestration

import (
	"context"
	"crossing-guard/internal/approvalchoice"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeRunner struct {
	response RunResponse
	err      error
	request  RunRequest
}

func (runner *fakeRunner) Run(_ context.Context, request RunRequest) (RunResponse, error) {
	runner.request = request
	return runner.response, runner.err
}

type fakeLifecycle struct {
	started    bool
	completion Completion
}

func (life *fakeLifecycle) MarkRunning(string, int64) error { life.started = true; return nil }
func (life *fakeLifecycle) Complete(_ string, completion Completion) error {
	life.completion = completion
	return nil
}

func testProfile() Profile {
	return Profile{ID: "reviewer", Instructions: "Review whether the observed command is destructive.",
		Timeout: time.Second, MaxInputBytes: 8192, MaxOutputBytes: 1024, MaxTokens: 128, MaxConcurrency: 1}
}

func testAction() Action {
	return Action{ActionID: "act_0123456789abcdef0123456789abcdef", ObservationID: "obs_0123456789abcdef0123456789abcdef",
		EventID: 7, Runtime: "claude", SessionID: "s", Tool: "Bash", Command: "remove the build cache recursively",
		ToolInput:      json.RawMessage(`{"command":"remove the build cache recursively","future":{"mode":"careful"}}`),
		ToolInputBytes: 32, ToolInputDigest: "sha256-v1:x", ToolInputCompleteness: "complete"}
}

func TestBuildRequestMarksActionUntrustedAndPinsDeterministicDigest(t *testing.T) {
	first, digest, err := BuildRequest(testProfile(), testAction())
	if err != nil {
		t.Fatal(err)
	}
	second, secondDigest, err := BuildRequest(testProfile(), testAction())
	if err != nil || digest != secondDigest || string(first.Schema) != string(second.Schema) {
		t.Fatalf("digest/request unstable: %q %q err=%v", digest, secondDigest, err)
	}
	if len(first.Messages) != 2 || !strings.Contains(first.Messages[0].Content, "report-only") ||
		!strings.Contains(first.Messages[1].Content, "Untrusted observed action JSON") ||
		!strings.Contains(first.Messages[1].Content, "remove the build cache recursively") ||
		!strings.Contains(first.Messages[1].Content, `"future":{"mode":"careful"}`) {
		t.Fatalf("request=%+v", first)
	}
}

func TestActionDigestIgnoresAskLifecycleButNotToolEvidence(t *testing.T) {
	action := testAction()
	first, err := ActionDigest(action)
	if err != nil {
		t.Fatal(err)
	}
	action.ObservationID = "obs_resolution"
	action.EventID = 99
	action.ObservedDecision = "allow"
	resolution, err := ActionDigest(action)
	if err != nil || resolution != first {
		t.Fatalf("lifecycle changed digest: %q %q err=%v", first, resolution, err)
	}
	action.Command = "different observed action"
	changed, _ := ActionDigest(action)
	if changed == first {
		t.Fatal("different tool evidence reused action digest")
	}
}

func TestReviewerClaimRejectsExtraFieldsUnknownAndDuplicateCitations(t *testing.T) {
	labels := FactLabels(testAction())
	valid, err := DecodeAgentClaim([]byte(`{"action":"deny","message":"Potentially destructive.","citations":["action.command"]}`), "reviewer", 1024, labels, nil, nil)
	if err != nil || valid.Action != "deny" {
		t.Fatalf("valid=%+v err=%v", valid, err)
	}
	for _, raw := range []string{
		`{"action":"deny","message":"x","citations":[],"verdict":true}`,
		`{"action":"maybe","message":"x","citations":[]}`,
		`{"action":"deny","message":"x","citations":["permission.granted"]}`,
		`{"action":"deny","message":"x","citations":["action.command","action.command"]}`,
		`{"action":"deny","message":"","citations":[]}`,
		`{"action":"deny","message":"x","citations":[],"stage_id":"s"}`,
		`{"action":"deny","message":"x","citations":[],"tags":["invented"]}`,
	} {
		if _, err := DecodeAgentClaim([]byte(raw), "reviewer", 1024, labels, nil, nil); err == nil {
			t.Fatalf("invalid claim accepted: %s", raw)
		}
	}
}

func TestExecuteStoresAttributedClaimAndClassifiedFailures(t *testing.T) {
	runner := &fakeRunner{response: RunResponse{Content: []byte(`{"action":"abstain","message":"Insufficient facts.","citations":["action.tool"]}`), RequestBytes: 10, ResponseBytes: 20}}
	life := &fakeLifecycle{}
	service, _ := NewService(runner, life)
	if err := service.Execute(context.Background(), "inv_1", testProfile(), testAction()); err != nil {
		t.Fatal(err)
	}
	if !life.started || life.completion.State != "completed" || life.completion.Claim == nil || life.completion.Claim.Action != "abstain" {
		t.Fatalf("completion=%+v", life.completion)
	}

	runner.err = &RunError{Kind: "timed_out"}
	runner.response = RunResponse{}
	life.completion = Completion{}
	if err := service.Execute(context.Background(), "inv_2", testProfile(), testAction()); err != nil {
		t.Fatal(err)
	}
	if life.completion.State != "timed_out" || life.completion.ErrorClass != "deadline_exceeded" {
		t.Fatalf("timeout=%+v", life.completion)
	}
}

func TestServiceRequiresPortsAndRespectsInputLimit(t *testing.T) {
	if _, err := NewService(nil, &fakeLifecycle{}); err == nil {
		t.Fatal("nil runner accepted")
	}
	profile := testProfile()
	profile.MaxInputBytes = 8
	if _, _, err := BuildRequest(profile, testAction()); err == nil {
		t.Fatal("oversized request accepted")
	}
	runner := &fakeRunner{err: errors.New("unavailable")}
	life := &fakeLifecycle{}
	service, _ := NewService(runner, life)
	if err := service.Execute(context.Background(), "inv", profile, testAction()); err != nil {
		t.Fatal(err)
	}
	if life.completion.State != "unavailable" || runner.request.Messages != nil {
		t.Fatalf("input limit completion=%+v request=%+v", life.completion, runner.request)
	}
}

func TestChoiceReviewExecutesTypedAnswerWithinQuestionLimits(t *testing.T) {
	for _, tc := range []struct {
		name  string
		free  bool
		limit int
		value string
		valid bool
	}{
		{"typed allowed", true, 20, "Custom", true}, {"option only", false, 20, "Custom", false},
		{"over limit", true, 3, "Custom", false}, {"offered", false, 0, "A", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			action := testAction()
			action.Prompts = []approvalchoice.ChoicePrompt{{ID: "q", Text: "Label?", FreeText: tc.free, Options: []approvalchoice.ChoiceOption{{Label: "A"}, {Label: "B"}}}}
			action.MaxFreeTextBytes = tc.limit
			raw, _ := json.Marshal(map[string]any{"action": "allow", "message": "Answer supplied", "citations": []string{}, "selections": []approvalchoice.ChoiceSelection{{PromptID: "q", Values: []string{tc.value}}}})
			runner := &fakeRunner{response: RunResponse{Content: raw}}
			life := &fakeLifecycle{}
			service, err := NewService(runner, life)
			if err != nil {
				t.Fatal(err)
			}
			if err = service.Execute(context.Background(), "inv", testProfile(), action); err != nil {
				t.Fatal(err)
			}
			if (life.completion.State == "completed") != tc.valid {
				t.Fatalf("completion=%+v", life.completion)
			}
			if tc.valid && life.completion.Claim.Selections[0].Values[0] != tc.value {
				t.Fatal("answer changed")
			}
			if tc.limit > 0 && !strings.Contains(runner.request.Messages[1].Content, `"max_free_text_bytes":`) {
				t.Fatal("model did not see ceiling")
			}
			first, _ := ActionDigest(action)
			action.MaxFreeTextBytes++
			second, _ := ActionDigest(action)
			if first == second {
				t.Fatal("limit absent from digest")
			}
		})
	}
}
