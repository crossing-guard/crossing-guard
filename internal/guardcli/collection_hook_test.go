package guardcli

import "testing"

func TestCollectionHookDispatchHasNoDecisionSurface(t *testing.T) {
	called := ""
	decision, reason := "sentinel", "sentinel"
	emit := collectionHookEmitters{
		action: func(_ hookInput, gotDecision, gotReason string) {
			called, decision, reason = "action", gotDecision, gotReason
		},
		result: func(hookInput) { called = "result" },
		entry:  func(hookInput) { called = "entry" },
		close:  func(hookInput) { called = "close" },
	}
	dispatchCollectionHook(hookInput{HookEventName: "PreToolUse"}, emit)
	if called != "action" || decision != "" || reason != "" {
		t.Fatalf("collection before-tool dispatch = %q decision=%q reason=%q", called, decision, reason)
	}
}

func TestCollectionHookDispatchMapsOnlyCollectionPhases(t *testing.T) {
	for event, want := range map[string]string{
		"SessionStart": "entry", "PostToolUse": "result",
		"PostToolUseFailure": "result", "SessionEnd": "close",
	} {
		t.Run(event, func(t *testing.T) {
			called := ""
			emit := collectionHookEmitters{
				action: func(hookInput, string, string) { called = "action" },
				result: func(hookInput) { called = "result" },
				entry:  func(hookInput) { called = "entry" },
				close:  func(hookInput) { called = "close" },
			}
			dispatchCollectionHook(hookInput{HookEventName: event}, emit)
			if called != want {
				t.Fatalf("dispatch = %q, want %q", called, want)
			}
		})
	}
}
