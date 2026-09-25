package guardcli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// TestHeldCallIsRecordedBeforeBlocking pins the regression the D19 staging fix
// introduced. The hook holds on the approvals inbox for up to MaxAskBudget (55s)
// waiting on a human; with the observation staged until a decision existed, a hook
// killed while held flushed nothing and the action vanished from the log — strictly
// worse than the empty decision it replaced, and on the ONE path where the event log
// is the only possible record. The hold must be recorded BEFORE we block.
func TestHeldCallIsRecordedBeforeBlocking(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var mu sync.Mutex
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		got = append(got, body)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	t.Setenv("CG_GOVERN", srv.Listener.Addr().String())
	t.Setenv("CG_GOVERN_TOKEN", "t")
	t.Setenv("CG_OBSERVE", "1")

	var in hookInput
	in.SessionID, in.ToolName = "s-held", "Bash"
	observeAttempt(in)

	// This is the moment before we block on a human. If the process dies here, the
	// log must already know the call was held.
	observeAsk("held for human: rule git-force-push")

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("held call not recorded before blocking: %d observations", len(got))
	}
	if got[0]["decision"] != "ask" {
		t.Fatalf("held call recorded with decision=%v, want \"ask\"", got[0]["decision"])
	}
	if got[0]["session"] != "s-held" {
		t.Fatalf("wrong session: %v", got[0]["session"])
	}
	pending, err := pendingObservations()
	if err != nil || len(pending) != 1 {
		t.Fatalf("legacy 204 incorrectly acknowledged v1 evidence: pending=%v err=%v", pending, err)
	}
}
