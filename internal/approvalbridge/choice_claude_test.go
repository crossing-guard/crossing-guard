package approvalbridge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"crossing-guard/internal/approvalchoice"
)

const twoOptionQuestion = `{"questions":[{"question":"Which label?","header":"Label",` +
	`"options":[{"label":"Sessions today","description":"today-scoped"},` +
	`{"label":"Daily sessions","description":"a rate"}],"multiSelect":false}]}`

// askQuestion drives one question-tool call through the bridge and returns the
// permission decision, plus whatever the requester was asked for.
func askQuestion(t *testing.T, input string, answer func(ApprovalRequest) (ApprovalResult, error)) (
	map[string]any, ApprovalRequest) {
	t.Helper()
	seen := make(chan ApprovalRequest, 1)
	h := newMCPHarness(t, RequesterFunc(func(_ context.Context, request ApprovalRequest) (ApprovalResult, error) {
		seen <- request
		return answer(request)
	}))
	h.send(t, map[string]any{"jsonrpc": "2.0", "id": "q-call", "method": "tools/call",
		"params": map[string]any{"name": ToolName, "arguments": map[string]any{
			"tool_name": questionToolName, "tool_use_id": "toolu_q", "input": json.RawMessage(input)}}})
	decision := permissionDecision(t, h.receive(t))
	return decision, <-seen
}

func TestQuestionOptionsReachTheApproverAsPrompts(t *testing.T) {
	_, request := askQuestion(t, twoOptionQuestion, func(ApprovalRequest) (ApprovalResult, error) {
		return ApprovalResult{Decision: "denied"}, nil
	})
	if len(request.Prompts) != 1 {
		t.Fatalf("carried %d prompts, want 1", len(request.Prompts))
	}
	prompt := request.Prompts[0]
	if prompt.Text != "Which label?" || prompt.Header != "Label" || prompt.Multi {
		t.Fatalf("prompt lost its question: %#v", prompt)
	}
	if len(prompt.Options) != 2 || prompt.Options[0].Label != "Sessions today" ||
		prompt.Options[1].Description != "a rate" {
		t.Fatalf("prompt lost its options: %#v", prompt.Options)
	}
	if !prompt.FreeText {
		t.Fatal("a question that accepts a typed answer must say so")
	}
}

func TestAllowedAnswerReachesTheRuntimeAsAnAnswer(t *testing.T) {
	decision, _ := askQuestion(t, twoOptionQuestion, func(request ApprovalRequest) (ApprovalResult, error) {
		return ApprovalResult{Decision: "allowed", PromptsCompleteness: "complete",
			Selections: []approvalchoice.ChoiceSelection{
				{PromptID: request.Prompts[0].ID, Values: []string{"Sessions today"}}}}, nil
	})
	if decision["behavior"] != "allow" {
		t.Fatalf("decision = %#v", decision)
	}
	updated := decision["updatedInput"].(map[string]any)
	answers, ok := updated["answers"].(map[string]any)
	if !ok {
		t.Fatalf("an allowed answer produced no answers: %#v", updated)
	}
	if answers["Which label?"] != "Sessions today" {
		t.Fatalf("answers = %#v", answers)
	}
	if _, kept := updated["questions"]; !kept {
		t.Fatal("the original questions must travel back with the answer")
	}
}

func TestMultiSelectAnswerIsJoined(t *testing.T) {
	multi := `{"questions":[{"question":"Which sections?","options":[{"label":"Intro"},` +
		`{"label":"Body"},{"label":"End"}],"multiSelect":true}]}`
	decision, _ := askQuestion(t, multi, func(request ApprovalRequest) (ApprovalResult, error) {
		return ApprovalResult{Decision: "allowed", PromptsCompleteness: "complete",
			Selections: []approvalchoice.ChoiceSelection{
				{PromptID: request.Prompts[0].ID, Values: []string{"Intro", "End"}}}}, nil
	})
	answers := decision["updatedInput"].(map[string]any)["answers"].(map[string]any)
	if answers["Which sections?"] != "Intro, End" {
		t.Fatalf("answers = %#v", answers)
	}
}

func TestTypedAnswerTravelsAsItsOwnText(t *testing.T) {
	decision, _ := askQuestion(t, twoOptionQuestion, func(request ApprovalRequest) (ApprovalResult, error) {
		return ApprovalResult{Decision: "allowed", PromptsCompleteness: "complete",
			Selections: []approvalchoice.ChoiceSelection{
				{PromptID: request.Prompts[0].ID, Values: []string{"Sessions this week"}}}}, nil
	})
	answers := decision["updatedInput"].(map[string]any)["answers"].(map[string]any)
	if answers["Which label?"] != "Sessions this week" {
		t.Fatalf("a typed answer was not delivered verbatim: %#v", answers)
	}
}

// The session must never be left to infer that nobody answered when the real reason
// is that nobody was shown the question.
func TestDroppedOptionsTellTheSessionWhy(t *testing.T) {
	decision, _ := askQuestion(t, twoOptionQuestion, func(ApprovalRequest) (ApprovalResult, error) {
		return ApprovalResult{Decision: "allowed", PromptsCompleteness: "truncated"}, nil
	})
	updated := decision["updatedInput"].(map[string]any)
	if _, answered := updated["answers"]; answered {
		t.Fatal("dropped options must not produce an answer")
	}
	note, ok := updated["response"].(string)
	if !ok || !strings.Contains(note, "never saw these options") {
		t.Fatalf("response = %#v", updated["response"])
	}
}

func TestUnusableAnswerIsRefusedRatherThanDelivered(t *testing.T) {
	decision, _ := askQuestion(t, twoOptionQuestion, func(ApprovalRequest) (ApprovalResult, error) {
		// An answer aimed at a question this call never asked can only come from
		// a defect above; the bridge holds the only exact copy of the call.
		return ApprovalResult{Decision: "allowed", PromptsCompleteness: "complete",
			Selections: []approvalchoice.ChoiceSelection{
				{PromptID: "q7", Values: []string{"Sessions today"}}}}, nil
	})
	updated := decision["updatedInput"].(map[string]any)
	if _, answered := updated["answers"]; answered {
		t.Fatal("an answer outside the offered options must not be delivered")
	}
	if note, _ := updated["response"].(string); !strings.Contains(note, "usable answer") {
		t.Fatalf("response = %#v", updated["response"])
	}
}

func TestOrdinaryToolAllowStillEchoesItsInput(t *testing.T) {
	h := newMCPHarness(t, RequesterFunc(func(context.Context, ApprovalRequest) (ApprovalResult, error) {
		return ApprovalResult{Decision: "allowed"}, nil
	}))
	h.send(t, map[string]any{"jsonrpc": "2.0", "id": "b-call", "method": "tools/call",
		"params": map[string]any{"name": ToolName, "arguments": map[string]any{
			"tool_name": "Bash", "tool_use_id": "toolu_b",
			"input": map[string]any{"command": "printf hello"}}}})
	updated := permissionDecision(t, h.receive(t))["updatedInput"].(map[string]any)
	if len(updated) != 1 || updated["command"] != "printf hello" {
		t.Fatalf("a call with no questions must be echoed unchanged: %#v", updated)
	}
}

func TestFailureNamesTheBoundaryThatBroke(t *testing.T) {
	h := newMCPHarness(t, RequesterFunc(func(context.Context, ApprovalRequest) (ApprovalResult, error) {
		return ApprovalResult{}, errors.New("approval daemon unavailable")
	}))
	h.send(t, map[string]any{"jsonrpc": "2.0", "id": "f-call", "method": "tools/call",
		"params": map[string]any{"name": ToolName, "arguments": map[string]any{
			"tool_name": "Bash", "tool_use_id": "toolu_f", "input": map[string]any{"command": "ls"}}}})
	decision := permissionDecision(t, h.receive(t))
	message, _ := decision["message"].(string)
	if decision["behavior"] != "deny" || !strings.Contains(message, "no approvals service is running") {
		t.Fatalf("a failure must name its cause, got %#v", decision)
	}
}

func TestErrorClassesStayFreeOfOperatorDetail(t *testing.T) {
	for _, testCase := range []struct{ err, want string }{
		{"approval daemon unavailable", "no approvals service is running"},
		{"approval daemon refused request: HTTP 401", "the approvals service refused the request"},
		{"approval daemon returned an invalid response", "the approvals service answered unreadably"},
		{"approval request failed: dial tcp 127.0.0.1:7788: connection refused",
			"the approvals service could not be reached"},
	} {
		got := approvalErrorClass(errors.New(testCase.err))
		if got != testCase.want {
			t.Fatalf("class for %q = %q, want %q", testCase.err, got, testCase.want)
		}
		for _, leak := range []string{"127.0.0.1", "7788", "HTTP 401"} {
			if strings.Contains(got, leak) {
				t.Fatalf("class %q leaks %q", got, leak)
			}
		}
	}
}
