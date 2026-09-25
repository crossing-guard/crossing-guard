package approvalbridge

// Claude's question tool, translated. This file is the only place that knows the
// name of that tool, the shape of its arguments, or how it expects an answer back.
// Everything above it — the wire, the approvals inbox, the browser, the delegated
// reviewer — speaks the generic choice-prompt vocabulary in internal/approvalchoice.
//
// Translation is deliberately lossy in one direction: an attribute Claude sends that
// the generic vocabulary does not name is dropped rather than smuggled through, so a
// second runtime's question surface cannot quietly become Claude-shaped.

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"crossing-guard/internal/approvalchoice"
)

// questionToolName is Claude's clarifying-question tool. A call to any other tool
// carries no prompts and travels the ordinary allow/deny path unchanged.
const questionToolName = "AskUserQuestion"

// maxBridgeFreeTextBytes is this process's own hard bound on a typed answer. The
// operator's real ceiling lives in the daemon's approvals document; the bridge cannot
// read it, so it enforces only the vocabulary's maximum as a sanity bound.
const maxBridgeFreeTextBytes = 8192

// claudeQuestions is the argument shape of the question tool. Only the fields the
// generic vocabulary can carry are declared; anything else Claude sends is ignored.
type claudeQuestions struct {
	Questions []struct {
		Question    string `json:"question"`
		Header      string `json:"header"`
		MultiSelect bool   `json:"multiSelect"`
		Options     []struct {
			Label       string `json:"label"`
			Description string `json:"description"`
		} `json:"options"`
	} `json:"questions"`
}

// projectChoicePrompts turns a question-tool call into generic prompts. It also
// returns each prompt's original question text, because Claude keys its answers by
// that text and only this process should know that. A non-question tool, or a call
// whose questions do not parse, yields no prompts and the ordinary path.
func projectChoicePrompts(toolName string, toolInput json.RawMessage) ([]approvalchoice.ChoicePrompt, []string) {
	if toolName != questionToolName {
		return nil, nil
	}
	var parsed claudeQuestions
	if json.Unmarshal(toolInput, &parsed) != nil || len(parsed.Questions) == 0 {
		return nil, nil
	}
	prompts := make([]approvalchoice.ChoicePrompt, 0, len(parsed.Questions))
	texts := make([]string, 0, len(parsed.Questions))
	for index, question := range parsed.Questions {
		options := make([]approvalchoice.ChoiceOption, 0, len(question.Options))
		for _, option := range question.Options {
			options = append(options, approvalchoice.ChoiceOption{
				Label: option.Label, Description: option.Description})
		}
		prompts = append(prompts, approvalchoice.ChoicePrompt{
			ID:      "q" + strconv.Itoa(index),
			Text:    question.Question,
			Header:  question.Header,
			Options: options,
			Multi:   question.MultiSelect,
			// Claude's own UI always offers a typed alternative, so every
			// question accepts free text.
			FreeText: true,
		})
		texts = append(texts, question.Question)
	}
	if approvalchoice.CheckPromptShape(prompts) != nil {
		return nil, nil
	}
	return prompts, texts
}

// answeredInput builds the permission result's updated input for an allowed call.
//
// Three cases, all honest: a call with no questions is echoed exactly as today; an
// answered call carries the approver's choices in the tool's own answers map; and a
// call whose questions never reached the approver says so in the tool's own freeform
// reply slot, so the session learns why no answer came back instead of being told
// only that nobody answered.
func answeredInput(toolInput json.RawMessage, prompts []approvalchoice.ChoicePrompt,
	questionTexts []string, result ApprovalResult) json.RawMessage {
	if len(prompts) == 0 {
		return toolInput
	}
	if note := unansweredNote(prompts, questionTexts, result); note != "" {
		return questionEnvelope(toolInput, "response", note)
	}
	answers := map[string]string{}
	for index, prompt := range prompts {
		for _, selection := range result.Selections {
			if selection.PromptID == prompt.ID {
				answers[questionTexts[index]] = strings.Join(selection.Values, ", ")
			}
		}
	}
	return questionEnvelope(toolInput, "answers", answers)
}

// unansweredNote returns the text to send when questions were asked but no usable
// answer came back, or "" when the selections are good. The bridge re-validates
// rather than trusting the inbox, because it alone still holds the exact call.
func unansweredNote(prompts []approvalchoice.ChoicePrompt, questionTexts []string,
	result ApprovalResult) string {
	if len(questionTexts) != len(prompts) {
		return "Crossing Guard could not match the approver's answers to these questions. Ask the user directly."
	}
	if result.PromptsCompleteness == "truncated" {
		return "The approver never saw these options: they exceed the approvals inbox limits. Ask the user directly, or ask a shorter question."
	}
	limits := approvalchoice.ChoiceLimits{MaxFreeTextBytes: maxBridgeFreeTextBytes}
	if err := approvalchoice.ValidateSelections(prompts, result.Selections, limits); err != nil {
		return "The approval was granted without a usable answer to these questions. Ask the user directly."
	}
	return ""
}

// questionEnvelope rebuilds the tool input with one added field. The original
// questions array is preserved byte for byte because the tool requires it back.
func questionEnvelope(toolInput json.RawMessage, field string, value any) json.RawMessage {
	var original map[string]json.RawMessage
	if json.Unmarshal(toolInput, &original) != nil {
		return toolInput
	}
	questions, ok := original["questions"]
	if !ok {
		return toolInput
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return toolInput
	}
	envelope, err := json.Marshal(map[string]json.RawMessage{
		"questions": questions,
		field:       encoded,
	})
	if err != nil {
		return toolInput
	}
	return envelope
}

// approvalErrorClass names the boundary that failed, in words a session can act on,
// without leaking an address, a token, or an operator's filesystem layout.
func approvalErrorClass(err error) string {
	if err == nil {
		return "unknown"
	}
	message := err.Error()
	for _, known := range []struct{ needle, class string }{
		{"approval daemon unavailable", "no approvals service is running"},
		{"refused request", "the approvals service refused the request"},
		{"invalid response", "the approvals service answered unreadably"},
		{"encoding failed", "the request could not be encoded"},
		{"creation failed", "the request could not be built"},
		{"request failed", "the approvals service could not be reached"},
	} {
		if strings.Contains(message, known.needle) {
			return known.class
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "the request was cancelled before an answer arrived"
	}
	return "unrecognized failure"
}
