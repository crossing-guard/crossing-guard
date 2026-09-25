package guardcli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"crossing-guard/internal/observation"
)

// Every hook boundary that can carry a helper message states the moment it
// stops waiting, so the daemon claims messages only while the reply can still
// be read (delivery-claim-on-reply plan D2).
func TestHookSendersStateTheirDeliveryDeadline(t *testing.T) {
	seen := map[string]time.Time{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadline, ok := observation.ParseHookDeadline(r.Header.Get(observation.HookDeadlineHeader))
		if !ok {
			t.Errorf("%s: no deadline header", r.URL.Path)
		}
		seen[r.URL.Path] = deadline
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/session-turn/v1"):
			_ = json.NewEncoder(w).Encode(observation.SessionTurnReceipt{Schema: observation.SessionTurnSchemaV1, ObservationID: "trn_deadline"})
		case strings.HasSuffix(r.URL.Path, "/observe/v1"):
			_ = json.NewEncoder(w).Encode(observation.Receipt{Schema: observation.SchemaV1, ObservationID: "obs_deadline", EventID: 1})
		case strings.HasSuffix(r.URL.Path, "/result/v1"):
			_ = json.NewEncoder(w).Encode(observation.ResultReceipt{Schema: observation.ResultSchemaV1, ObservationID: "res_deadline", ResultID: 1})
		}
	}))
	defer server.Close()
	addr := strings.TrimPrefix(server.URL, "http://")
	before := time.Now()
	if _, err := sendSessionTurn(addr, "", observation.SessionTurnEnvelope{ObservationID: "trn_deadline"}); err != nil {
		t.Fatal(err)
	}
	if _, err := sendObservation(addr, "", observation.Envelope{ObservationID: "obs_deadline"}); err != nil {
		t.Fatal(err)
	}
	if _, err := sendResult(addr, "", observation.ResultEnvelope{ObservationID: "res_deadline"}); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	if len(seen) != 3 {
		t.Fatalf("all three boundaries must be seen: %v", seen)
	}
	for path, deadline := range seen {
		if deadline.Before(before.Add(observation.HookDeliveryBudget).Add(-time.Millisecond)) ||
			deadline.After(after.Add(observation.HookDeliveryBudget).Add(time.Millisecond)) {
			t.Fatalf("%s deadline %s is not send time plus the hook budget", path, deadline)
		}
	}
	if _, ok := observation.ParseHookDeadline(""); ok {
		t.Fatal("absent header must read as no deadline")
	}
	if _, ok := observation.ParseHookDeadline("soon"); ok {
		t.Fatal("malformed header must read as no deadline")
	}
}
