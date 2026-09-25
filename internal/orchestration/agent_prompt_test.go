package orchestration

import (
	"strings"
	"testing"
)

func TestBuildAgentPromptSuppliesLabeledContextManifest(t *testing.T) {
	profile := AgentPromptProfile{ID: "design-helper", Type: "helper",
		Instructions: "Review the completed turn.", MaxInputBytes: 8192, MaxOutputBytes: 1024,
		DeclaredTags: []string{"needs-review"}}
	source := ManagedSource{TaskID: "task_1", Runtime: "codex", ProjectRoot: "/repo",
		Lifecycle: "completed", FinalMessage: "Done.", EventID: 7}
	extras := []PromptContext{{Label: "operator.group_notes", Body: "- prefer ADR 0028\n"}}
	prompt, labels, err := BuildAgentPrompt(profile, source, "task.completed", extras)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"task.completed", "operator.group_notes", "prefer ADR 0028",
		"untrusted data", "needs-review", "supplied_fact_labels"} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("prompt lost %q:\n%s", required, prompt)
		}
	}
	want := []string{"source.task", "source.lifecycle", "source.final_message", "operator.group_notes"}
	if len(labels) != len(want) {
		t.Fatalf("labels = %v", labels)
	}
	for index, label := range want {
		if labels[index] != label {
			t.Fatalf("labels = %v", labels)
		}
	}
}

func TestBuildAgentPromptRefusesDuplicateLabelsAndOversize(t *testing.T) {
	profile := AgentPromptProfile{ID: "p", Type: "helper", Instructions: "Go.",
		MaxInputBytes: 1024, MaxOutputBytes: 256}
	duplicate := []PromptContext{{Label: "context.a", Body: "x"}, {Label: "context.a", Body: "y"}}
	if _, _, err := BuildAgentPrompt(profile, ManagedSource{}, "task.completed", duplicate); err == nil {
		t.Fatal("duplicate context label was accepted")
	}
	oversized := []PromptContext{{Label: "context.big", Body: strings.Repeat("b", 4096)}}
	if _, _, err := BuildAgentPrompt(profile, ManagedSource{}, "task.completed", oversized); err == nil {
		t.Fatal("prompt over the profile input limit was accepted")
	}
}
