package daemon

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"crossing-guard/internal/approvalchoice"
)

func TestChoiceActionPreservesQuestionAndConfiguredLimit(t *testing.T) {
	prompts := []approvalchoice.ChoicePrompt{{ID: "q", Text: "Label?", Header: "Title", FreeText: true, Options: []approvalchoice.ChoiceOption{{Label: "A", Description: "first"}, {Label: "B"}}}}
	action := actionFromApproval(Approval{ID: "ap_choice", Prompts: prompts})
	if !reflect.DeepEqual(action.Prompts, prompts) || !prompts[0].FreeText {
		t.Fatal("question changed during reviewer projection")
	}
	if action.MaxFreeTextBytes != activeApprovalsConfig().MaxFreeTextBytes {
		t.Fatal("reviewer did not receive configured ceiling")
	}
}

func TestChoiceAnswerRecoveryForHumanAndService(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, service := range []bool{false, true} {
		t.Run(map[bool]string{false: "human", true: "service"}[service], func(t *testing.T) {
			now := time.Now()
			hub := newApprovalsHub()
			hub.now = func() time.Time { return now }
			approval, waiter, capability := admittedResponseTestApproval(t, hub, "ap_choice", "ask", now.Add(time.Minute))
			approval.Prompts = []approvalchoice.ChoicePrompt{{ID: "q", Text: "Label?", FreeText: true, Options: []approvalchoice.ChoiceOption{{Label: "A"}, {Label: "B"}}}}
			responder := interactiveConsoleResponder
			if service {
				responder = ApprovalResponder{Kind: "service", ID: "service:choice-test"}
				var err error
				capability, err = hub.grantServiceResponder(approval.ID, responder)
				if err != nil {
					t.Fatal(err)
				}
			}
			cmd := responseTestCommand(approval.ID, responseTestID("Q"), "allow", "reviewed", now, capability)
			cmd.responder = responder
			cmd.selections = []approvalchoice.ChoiceSelection{{PromptID: "q", Values: []string{strings.Repeat("x", activeApprovalsConfig().MaxFreeTextBytes+1)}}}
			if got := hub.respond(cmd); got.kind != approvalResponseSelectionRequired {
				t.Fatalf("oversized answer=%+v", got)
			}
			if approval.Status != "pending" {
				t.Fatal("invalid answer resolved approval")
			}
			cmd.responseID = responseTestID("R")
			cmd.selections[0].Values = []string{"Custom label"}
			got := hub.respond(cmd)
			if got.kind != approvalResponseAccepted {
				t.Fatalf("typed answer=%+v", got)
			}
			select {
			case result := <-waiter:
				if result.Status != "allowed" || result.Responses[0].Selections[0].Values[0] != "Custom label" {
					t.Fatalf("result=%+v", result)
				}
			default:
				t.Fatal("waiter did not receive answer")
			}
		})
	}
}
