package guardcli

import "testing"

// TestAskBudgetBeatsRuntimeTimeout pins the one invariant the whole fail-closed
// design rests on: our ask budget must lapse BEFORE the runtime's own hook
// timeout, or the runtime kills the hook mid-wait and fails OPEN. STYLE §2:
// a safety invariant gets a test, not just a comment.
func TestAskBudgetBeatsRuntimeTimeout(t *testing.T) {
	if MaxAskBudget >= RuntimeHookTimeout {
		t.Fatalf("MaxAskBudget (%v) must be < RuntimeHookTimeout (%v): our fail-closed deadline must land before the runtime kills the hook and fails open",
			MaxAskBudget, RuntimeHookTimeout)
	}
	if AskClientSlack <= 0 {
		t.Fatalf("AskClientSlack (%v) must be positive so the HTTP client outlives the budget it is waiting on", AskClientSlack)
	}
}
