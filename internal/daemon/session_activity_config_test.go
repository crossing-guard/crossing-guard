package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionActivityConfigDefaultsWhenAbsent(t *testing.T) {
	config, origin, err := loadSessionActivityConfig(t.TempDir())
	if err != nil || origin != "builtin-default" || config != defaultSessionActivityConfig() {
		t.Fatalf("config=%+v origin=%q err=%v", config, origin, err)
	}
}

func TestSessionActivityConfigOverridesAndRejects(t *testing.T) {
	dir := t.TempDir()
	body := `{"format_version":1,"sampler_interval_seconds":10,"liveness_live_seconds":60,
"liveness_recent_seconds":300,"liveness_window_seconds":1800,"liveness_horizon_seconds":86400,
"rail_keepalive_seconds":5,"turn_retention_days":7}`
	if err := os.WriteFile(filepath.Join(dir, "session-activity.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	config, origin, err := loadSessionActivityConfig(dir)
	if err != nil || !strings.HasSuffix(origin, "session-activity.json") || config.SamplerIntervalSeconds != 10 ||
		config.TurnRetentionDays != 7 || config.LivenessLiveSeconds != 60 {
		t.Fatalf("config=%+v origin=%q err=%v", config, origin, err)
	}
	for name, bad := range map[string]string{
		"unknown field":   `{"format_version":1,"sampler_interval_secondz":10}`,
		"inverted ladder": `{"format_version":1,"liveness_live_seconds":900,"liveness_recent_seconds":120}`,
		"non-positive":    `{"format_version":1,"turn_retention_days":0}`,
		"wrong version":   `{"format_version":9}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, "session-activity.json"), []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := loadSessionActivityConfig(dir); err == nil {
			t.Fatalf("%s must be reported, not accepted", name)
		}
	}
}

// The sampler and the rail stream read policy only through the owner. A
// literal duration in either is the defect this owner exists to prevent.
func TestSessionActivityPolicyIsNotCompiledIntoConsumers(t *testing.T) {
	for _, file := range []string{"session_activity.go", "session_activity_http.go"} {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, banned := range []string{"30 * time.Second", "45 * time.Second", "15 * time.Second",
			"2 * time.Minute", "15 * time.Minute", "60 * time.Minute", "7 * 24 * time.Hour",
			"hookLivenessLive", "nativeActivityTTL", "nativeActivityInterval"} {
			if strings.Contains(string(body), banned) {
				t.Fatalf("%s still compiles policy %q; it belongs in the session-activity configuration owner", file, banned)
			}
		}
	}
}
