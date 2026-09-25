package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeViewProfile(t *testing.T, dataDir, name, body string) string {
	t.Helper()
	dir := viewProfilesDir(dataDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func profileByID(profiles []ResolvedViewProfile, id string) (ResolvedViewProfile, bool) {
	for _, profile := range profiles {
		if profile.ID == id {
			return profile, true
		}
	}
	return ResolvedViewProfile{}, false
}

// The built-in modules decode under the same strict loader as installed ones.
func TestBuiltinViewProfilesLoad(t *testing.T) {
	profiles, rejected := LoadViewProfiles(t.TempDir())
	if len(rejected) != 0 {
		t.Fatalf("built-ins rejected: %+v", rejected)
	}
	for _, id := range []string{"full", "agent-review"} {
		profile, ok := profileByID(profiles, id)
		if !ok || profile.Origin != "builtin" || profile.Name == "" {
			t.Fatalf("built-in %s: %+v %v", id, profile, ok)
		}
	}
	review, _ := profileByID(profiles, "agent-review")
	if len(review.Rules) == 0 || review.Rules[0].Match.Kind != "user" || review.Rules[0].Match.MinChars != 2000 || review.Rules[0].Display != "collapse" {
		t.Fatalf("agent-review rules: %+v", review.Rules)
	}
}

// An installed module replaces the built-in of its id whole and a new id adds
// one; a file that fails is reported and skipped, and the built-in stays.
func TestInstalledViewProfilesReplaceAddAndReject(t *testing.T) {
	dataDir := t.TempDir()
	replaced := writeViewProfile(t, dataDir, "agent-review.json",
		`{"format_version":1,"id":"agent-review","name":"Mine","rules":[{"match":{"kind":"tool_call"},"display":"hide"}]}`)
	added := writeViewProfile(t, dataDir, "quiet.json", `{"format_version":1,"id":"quiet","name":"Quiet"}`)
	bad := map[string]string{
		"full.json":     `{"format_version":1,"id":"other","name":"Wrong file"}`,
		"display.json":  `{"format_version":1,"id":"display","name":"x","rules":[{"match":{},"display":"shrink"}]}`,
		"kind.json":     `{"format_version":1,"id":"kind","name":"x","rules":[{"match":{"kind":"attachment"},"display":"hide"}]}`,
		"unknown.json":  `{"format_version":1,"id":"unknown","name":"x","colour":"red"}`,
		"version.json":  `{"format_version":2,"id":"version","name":"x"}`,
		"trailing.json": `{"format_version":1,"id":"trailing","name":"x"} {}`,
		"noname.json":   `{"format_version":1,"id":"noname"}`,
	}
	for name, body := range bad {
		writeViewProfile(t, dataDir, name, body)
	}
	profiles, rejected := LoadViewProfiles(dataDir)
	review, _ := profileByID(profiles, "agent-review")
	if review.Origin != replaced || review.Name != "Mine" || len(review.Rules) != 1 || review.Rules[0].Display != "hide" {
		t.Fatalf("replacement: %+v", review)
	}
	quiet, ok := profileByID(profiles, "quiet")
	if !ok || quiet.Origin != added || quiet.Rules == nil {
		t.Fatalf("added module: %+v %v", quiet, ok)
	}
	if full, _ := profileByID(profiles, "full"); full.Origin != "builtin" {
		t.Fatalf("a failed replacement must leave the built-in in use: %+v", full)
	}
	if len(rejected) != len(bad) {
		t.Fatalf("rejected %d of %d: %+v", len(rejected), len(bad), rejected)
	}
	for _, rejection := range rejected {
		if rejection.Path == "" || rejection.Error == "" {
			t.Fatalf("a rejection names its file and reason: %+v", rejection)
		}
	}
	if len(profiles) != 3 {
		t.Fatalf("modules: %+v", profiles)
	}
}

// console.json selects modules by id: every id must resolve and every role
// must be an orchestration role; a file's role entries add to the defaults'.
func TestConsoleTranscriptSelection(t *testing.T) {
	dataDir := t.TempDir()
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(consoleConfigPath(dataDir), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	defaults := defaultConsoleConfig().Transcript
	if defaults.DefaultProfile != "full" || defaults.RoleProfiles["helper"] != "agent-review" || defaults.RoleProfiles["follower"] != "agent-review" {
		t.Fatalf("defaults: %+v", defaults)
	}
	writeViewProfile(t, dataDir, "quiet.json", `{"format_version":1,"id":"quiet","name":"Quiet"}`)
	write(`{"format_version":1,"transcript":{"role_profiles":{"reviewer":"quiet"}}}`)
	config, _, err := loadConsoleConfig(dataDir)
	if err != nil || config.Transcript.RoleProfiles["reviewer"] != "quiet" || config.Transcript.RoleProfiles["helper"] != "agent-review" {
		t.Fatalf("installed selection: %+v %v", config.Transcript, err)
	}
	for body, want := range map[string]string{
		`{"format_version":1,"transcript":{"default_profile":"missing"}}`:           "default_profile",
		`{"format_version":1,"transcript":{"role_profiles":{"helper":"missing"}}}`:  "role_profiles.helper",
		`{"format_version":1,"transcript":{"role_profiles":{"boss":"full"}}}`:       "role must be",
		`{"format_version":1,"transcript":{"default_profile":""}}`:                  "default_profile must be set",
		`{"format_version":1,"transcript":{"default_profile":"full","colour":"x"}}`: "unknown field",
	} {
		write(body)
		if _, _, err := loadConsoleConfig(dataDir); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: want %q, got %v", body, want, err)
		}
	}
}

// The route publishes every resolved module with its origin, and each request
// reads the directory again, so a dropped file needs no restart.
func TestConsoleViewProfilesRoute(t *testing.T) {
	dataDir := t.TempDir()
	previous := resolvedIndexPath
	setIndexPath(dataDir)
	defer func() { resolvedIndexPath = previous }()
	read := func() viewProfilesResponse {
		t.Helper()
		recorder := httptest.NewRecorder()
		handleConsoleViewProfiles(recorder, httptest.NewRequest(http.MethodGet, "/api/console/view-profiles", nil))
		var body viewProfilesResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("%v: %s", err, recorder.Body.String())
		}
		return body
	}
	if body := read(); len(body.Profiles) != 2 || body.Rejected == nil {
		t.Fatalf("built-ins only: %+v", body)
	}
	writeViewProfile(t, dataDir, "quiet.json", `{"format_version":1,"id":"quiet","name":"Quiet"}`)
	writeViewProfile(t, dataDir, "broken.json", `{`)
	body := read()
	if _, ok := profileByID(body.Profiles, "quiet"); !ok || len(body.Rejected) != 1 {
		t.Fatalf("after dropping files: %+v", body)
	}
}
