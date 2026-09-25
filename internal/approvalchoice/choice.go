// Package approvalchoice owns the shared question/answer format and structural validation.
package approvalchoice

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// maxChoicePromptIDBytes bounds an adapter-minted prompt identity. No pattern is
// imposed: an adapter may mint sequential ids or carry its runtime's own.
const maxChoicePromptIDBytes = 128

// ChoiceOption is one answer an approver may select.
type ChoiceOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// ChoicePrompt is one question carried on a held approval. Header and per-option
// descriptions are optional display sugar; no validator requires them.
type ChoicePrompt struct {
	ID       string         `json:"id"`
	Text     string         `json:"text"`
	Header   string         `json:"header,omitempty"`
	Options  []ChoiceOption `json:"options"`
	Multi    bool           `json:"multi,omitempty"`
	FreeText bool           `json:"free_text,omitempty"`
}

// ChoiceSelection is one prompt's answer. Values are option labels, except where the
// prompt allows free text, in which case one unlisted value is also accepted.
type ChoiceSelection struct {
	PromptID string   `json:"prompt_id"`
	Values   []string `json:"values"`
}

// ChoiceLimits are the approval owner's configured ceilings. They travel to this
// validator rather than being compiled here, so the operator's document decides.
type ChoiceLimits struct {
	MaxPrompts       int
	MaxOptions       int
	MaxTextBytes     int
	MaxFreeTextBytes int
	MaxWireBytes     int
}

// CheckPromptShape rejects prompts that are structurally impossible to answer or to
// display. A failure here means the adapter sent something malformed, which is a bug
// in the adapter and must surface as a refused request — never as a silent drop.
// Configured ceilings are NOT checked here; see PromptsWithinLimits.
func CheckPromptShape(prompts []ChoicePrompt) error {
	seen := make(map[string]bool, len(prompts))
	for index, prompt := range prompts {
		if err := checkOnePromptShape(prompt); err != nil {
			return fmt.Errorf("prompt %d: %w", index, err)
		}
		if seen[prompt.ID] {
			return fmt.Errorf("prompt %d: duplicate prompt id", index)
		}
		seen[prompt.ID] = true
	}
	return nil
}

func checkOnePromptShape(prompt ChoicePrompt) error {
	if strings.TrimSpace(prompt.ID) == "" || len(prompt.ID) > maxChoicePromptIDBytes ||
		!utf8.ValidString(prompt.ID) {
		return errors.New("prompt id is empty, oversized, or not valid UTF-8")
	}
	if strings.TrimSpace(prompt.Text) == "" || !utf8.ValidString(prompt.Text) {
		return errors.New("prompt text is empty or not valid UTF-8")
	}
	if len(prompt.Options) < 2 {
		return errors.New("a prompt needs at least two options")
	}
	labels := make(map[string]bool, len(prompt.Options))
	for _, option := range prompt.Options {
		if strings.TrimSpace(option.Label) == "" || !utf8.ValidString(option.Label) {
			return errors.New("option label is empty or not valid UTF-8")
		}
		if labels[option.Label] {
			return errors.New("duplicate option label")
		}
		labels[option.Label] = true
	}
	return nil
}

// PromptsWithinLimits reports whether these prompts fit the operator's configured
// ceilings. Over-ceiling prompts are dropped and the approval is marked truncated;
// they never turn a question into a denial. Free text is not a ceiling here: an
// operator who disallows typed answers gets option-only prompts, not dropped ones.
func PromptsWithinLimits(prompts []ChoicePrompt, limits ChoiceLimits) bool {
	if len(prompts) > limits.MaxPrompts {
		return false
	}
	for _, prompt := range prompts {
		if len(prompt.Options) > limits.MaxOptions {
			return false
		}
		if len(prompt.Text) > limits.MaxTextBytes || len(prompt.Header) > limits.MaxTextBytes {
			return false
		}
		for _, option := range prompt.Options {
			if len(option.Label) > limits.MaxTextBytes || len(option.Description) > limits.MaxTextBytes {
				return false
			}
		}
	}
	return true
}

// ValidateSelections is the one rule for "may this answer become operative". The
// approval owner calls it before recording a response; the bridge calls it again
// before building the runtime reply, because the bridge is the only party holding the
// exact original tool input.
//
// Every prompt must be answered exactly once. A value must be one of that prompt's
// option labels, unless the prompt allows free text, in which case a single unlisted
// bounded value is accepted instead.
func ValidateSelections(prompts []ChoicePrompt, selections []ChoiceSelection, limits ChoiceLimits) error {
	if len(prompts) == 0 {
		if len(selections) != 0 {
			return errors.New("this approval carries no questions to answer")
		}
		return nil
	}
	if len(selections) != len(prompts) {
		return errors.New("answer every question exactly once")
	}
	byPrompt := make(map[string]ChoiceSelection, len(selections))
	for _, selection := range selections {
		if _, duplicate := byPrompt[selection.PromptID]; duplicate {
			return errors.New("one question was answered twice")
		}
		byPrompt[selection.PromptID] = selection
	}
	for _, prompt := range prompts {
		selection, answered := byPrompt[prompt.ID]
		if !answered {
			return fmt.Errorf("question %q was not answered", prompt.ID)
		}
		if err := validateOneSelection(prompt, selection, limits); err != nil {
			return fmt.Errorf("question %q: %w", prompt.ID, err)
		}
	}
	return nil
}

func validateOneSelection(prompt ChoicePrompt, selection ChoiceSelection, limits ChoiceLimits) error {
	if len(selection.Values) == 0 {
		return errors.New("no answer was given")
	}
	if !prompt.Multi && len(selection.Values) != 1 {
		return errors.New("this question takes exactly one answer")
	}
	if len(selection.Values) > len(prompt.Options)+1 {
		return errors.New("more answers than this question has options")
	}
	labels := make(map[string]bool, len(prompt.Options))
	for _, option := range prompt.Options {
		labels[option.Label] = true
	}
	seen := make(map[string]bool, len(selection.Values))
	freeUsed := false
	for _, value := range selection.Values {
		if seen[value] {
			return errors.New("the same answer was given twice")
		}
		seen[value] = true
		if labels[value] {
			continue
		}
		if !prompt.FreeText {
			return errors.New("that answer is not one of the offered options")
		}
		if freeUsed {
			return errors.New("only one typed answer is allowed")
		}
		if strings.TrimSpace(value) == "" || !utf8.ValidString(value) ||
			len(value) > limits.MaxFreeTextBytes {
			return errors.New("the typed answer is empty, oversized, or not valid UTF-8")
		}
		freeUsed = true
	}
	return nil
}
