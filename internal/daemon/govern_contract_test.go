package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The console probe treats non-200 as daemon failure. A nil governor therefore has
// a deliberate 200 + configured:false contract on both support endpoints: degraded
// capture is not the same state as an unreachable daemon.
func TestNilGovernorSupportEndpointsRemainReachable(t *testing.T) {
	prior := governor
	governor = nil
	t.Cleanup(func() { governor = prior })

	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"health", handleGovernHealth},
		{"runtimes", handleGovernRuntimes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.handler(rec, httptest.NewRequest(http.MethodGet, "/api/govern/"+tc.name, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: nil governor is degraded, not unreachable", rec.Code)
			}
			var body struct {
				Configured bool `json:"configured"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Configured {
				t.Error("configured = true, want false for nil governor")
			}
		})
	}
}
