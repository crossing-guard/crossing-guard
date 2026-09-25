package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionStreamConfigDefaultsWhenAbsent(t *testing.T) {
	config, origin, err := loadSessionStreamConfig(t.TempDir())
	if err != nil {
		t.Fatalf("absent configuration must be usable, got %v", err)
	}
	if origin != "builtin-default" {
		t.Fatalf("origin = %q, want builtin-default", origin)
	}
	if config != defaultSessionStreamConfig() {
		t.Fatalf("absent configuration must yield the compiled defaults")
	}
}

func TestSessionStreamConfigOverrides(t *testing.T) {
	dir := t.TempDir()
	body := `{"format_version":1,"poll_seconds":9,"quiet_seconds":90,"coalesce_ms":100,"lookback_rows":8,
"snapshot_events":50,"keepalive_seconds":20,"max_streams":4,"governance_window":10,
"client_backoff_cap_seconds":30,"client_age_tick_seconds":5}`
	if err := os.WriteFile(filepath.Join(dir, "session-stream.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	config, origin, err := loadSessionStreamConfig(dir)
	if err != nil {
		t.Fatalf("valid configuration rejected: %v", err)
	}
	if !strings.HasSuffix(origin, "session-stream.json") {
		t.Fatalf("origin = %q, want the file path", origin)
	}
	if config.PollSeconds != 9 || config.QuietSeconds != 90 || config.MaxStreams != 4 {
		t.Fatalf("operator values not applied: %+v", config)
	}
}

// A typo must be visible to the operator, not silently ignored — otherwise the
// console would run on values nobody chose.
func TestSessionStreamConfigMalformedIsVisible(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"unknown field":         `{"format_version":1,"poll_secondz":2}`,
		"wrong version":         `{"format_version":99}`,
		"non-positive":          `{"format_version":1,"poll_seconds":0}`,
		"non-positive coalesce": `{"format_version":1,"coalesce_ms":0}`,
		"trailing data":         `{"format_version":1} {"format_version":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(dir, "session-stream.json"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := loadSessionStreamConfig(dir); err == nil {
				t.Fatalf("%s must be reported, not accepted", name)
			}
		})
	}
}

// Polling faster than the harvest burst window cannot surface anything newer,
// so the floor is mechanism and the configured value cannot go below it.
func TestSessionStreamPollHasFloor(t *testing.T) {
	config := defaultSessionStreamConfig()
	config.PollSeconds = 1
	if got := config.Poll(); got != sessionStreamPollFloor {
		t.Fatalf("poll = %s, want the floor %s", got, sessionStreamPollFloor)
	}
}

// Every value the stream uses must come from the config owner. A literal in a
// consumer is the defect this owner exists to prevent.
func TestSessionStreamPolicyIsNotCompiledIntoConsumers(t *testing.T) {
	body, err := os.ReadFile("session_live_http.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{
		"liveSessionWindow", "liveSessionPoll", "liveSessionKeepalive",
		"liveGovernanceWindow", "liveSessionMaxStreams",
	} {
		if strings.Contains(string(body), banned) {
			t.Fatalf("%s is policy and must live in the session-stream configuration owner", banned)
		}
	}
}

// An operator file written for the retired settle window keeps loading: the
// key is read, ignored, and reported — never a silent fallback to defaults.
func TestSessionStreamConfigToleratesRetiredSettle(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "session-stream.json"),
		[]byte(`{"format_version":1,"settle_seconds":5,"quiet_seconds":120}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config, _, err := loadSessionStreamConfig(dir)
	if err != nil {
		t.Fatalf("retired key must be tolerated: %v", err)
	}
	if config.QuietSeconds != 120 || config.DeprecatedSettleSeconds != 0 {
		t.Fatalf("operator values not applied / retired key not cleared: %+v", config)
	}
}
