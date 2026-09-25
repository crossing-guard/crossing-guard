package approvalchoice

import "testing"

type choiceCase struct {
	name       string
	prompts    []ChoicePrompt
	selections []ChoiceSelection
	valid      bool
}

func choiceCases() []choiceCase {
	single := []ChoicePrompt{{ID: "q0", Text: "Which label?", Options: []ChoiceOption{
		{Label: "Sessions today"}, {Label: "Daily sessions"}}}}
	multi := []ChoicePrompt{{ID: "q0", Text: "Which sections?", Multi: true,
		Options: []ChoiceOption{{Label: "Intro"}, {Label: "Body"}, {Label: "End"}}}}
	free := []ChoicePrompt{{ID: "q0", Text: "Which label?", FreeText: true,
		Options: []ChoiceOption{{Label: "A"}, {Label: "B"}}}}
	two := []ChoicePrompt{
		{ID: "q0", Text: "First?", Options: []ChoiceOption{{Label: "A"}, {Label: "B"}}},
		{ID: "q1", Text: "Second?", Options: []ChoiceOption{{Label: "C"}, {Label: "D"}}},
	}
	pick := func(id string, values ...string) ChoiceSelection {
		return ChoiceSelection{PromptID: id, Values: values}
	}
	return []choiceCase{
		{"no questions, no answer", nil, nil, true},
		{"no questions but an answer", nil, []ChoiceSelection{pick("q0", "A")}, false},
		{"one offered label", single, []ChoiceSelection{pick("q0", "Sessions today")}, true},
		{"a label that was never offered", single, []ChoiceSelection{pick("q0", "Sessions / day")}, false},
		{"unanswered question", single, nil, false},
		{"empty values", single, []ChoiceSelection{pick("q0")}, false},
		{"two answers to a single-choice question", single,
			[]ChoiceSelection{pick("q0", "Sessions today", "Daily sessions")}, false},
		{"answer aimed at an unknown question", single, []ChoiceSelection{pick("q7", "Sessions today")}, false},
		{"multi-select takes several", multi, []ChoiceSelection{pick("q0", "Intro", "End")}, true},
		{"multi-select repeats itself", multi, []ChoiceSelection{pick("q0", "Intro", "Intro")}, false},
		{"free text where invited", free, []ChoiceSelection{pick("q0", "something else")}, true},
		{"two typed answers", free, []ChoiceSelection{{PromptID: "q0", Values: []string{"one", "two"}}}, false},
		{"blank typed answer", free, []ChoiceSelection{pick("q0", "   ")}, false},
		{"both questions answered", two,
			[]ChoiceSelection{pick("q0", "A"), pick("q1", "C")}, true},
		{"only one of two answered", two, []ChoiceSelection{pick("q0", "A")}, false},
		{"same question answered twice", two,
			[]ChoiceSelection{pick("q0", "A"), pick("q0", "B")}, false},
	}
}

func TestSelections(t *testing.T) {
	for _, tc := range choiceCases() {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSelections(tc.prompts, tc.selections, ChoiceLimits{MaxFreeTextBytes: 2048})
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}

func TestTypedAnswerLimits(t *testing.T) {
	prompts := []ChoicePrompt{{ID: "q", Text: "Label?", FreeText: true, Options: []ChoiceOption{{Label: "A"}, {Label: "B"}}}}
	for _, tc := range []struct {
		name, value string
		limit       int
		valid       bool
	}{
		{"boundary", "é", 2, true}, {"bytes not runes", "é", 1, false}, {"disabled", "custom", 0, false},
		{"offered still works", "A", 0, true}, {"invalid utf8", string([]byte{255}), 20, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSelections(prompts, []ChoiceSelection{{PromptID: "q", Values: []string{tc.value}}}, ChoiceLimits{MaxFreeTextBytes: tc.limit})
			if (err == nil) != tc.valid {
				t.Fatalf("err=%v want valid=%v", err, tc.valid)
			}
		})
	}
	if err := CheckPromptShape(prompts); err != nil {
		t.Fatal(err)
	}
	if PromptsWithinLimits(prompts, ChoiceLimits{MaxPrompts: 1, MaxOptions: 2, MaxTextBytes: 2}) {
		t.Fatal("oversized prompt accepted")
	}
	prompts[0].Options[1].Label = "A"
	if CheckPromptShape(prompts) == nil {
		t.Fatal("duplicate option label accepted")
	}
}
