package transcription

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConfigValidateAcceptsTheExampleShape(t *testing.T) {
	config := testConfig()
	if err := config.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if got := config.HintPolicyFor(testLocalBackend); !got.ProjectName || len(got.Keywords) != 1 {
		t.Fatalf("hint policy = %+v", got)
	}
	if got := config.HintPolicyFor(testCloudBackend); got.ProjectName || got.BranchName {
		t.Fatalf("cloud hint policy should default off: %+v", got)
	}
	if config.DisclosureFor(testLocalBackend) != nil || config.DisclosureFor(testCloudBackend) == nil {
		t.Fatal("disclosure ownership per backend is wrong")
	}
}

func TestConfigValidateRejectsEachSharedDefect(t *testing.T) {
	cases := map[string]func(c *Config){
		"missing selected section": func(c *Config) { delete(c.Backends, testLocalBackend) },
		"empty backend":            func(c *Config) { c.Backend = " " },
		"cloud hint not named": func(c *Config) {
			s := c.Backends[testCloudBackend]
			s.Hints.ProjectName = true
			c.Backends[testCloudBackend] = s
		},
		"empty disclosure": func(c *Config) {
			s := c.Backends[testCloudBackend]
			s.Disclosure.Text = ""
			c.Backends[testCloudBackend] = s
		},
		"overlap not shorter than window": func(c *Config) { c.Streaming.OverlapSeconds = 8 },
		"window over max":                 func(c *Config) { c.Streaming.WindowSeconds = 200 },
		"min over max":                    func(c *Config) { c.Limits.MinMS = 200000 },
		"peak out of range":               func(c *Config) { c.Limits.MinPeak = 1.5 },
		"zero concurrency":                func(c *Config) { c.Limits.MaxConcurrent = 0 },
		"zero token seconds":              func(c *Config) { c.Disclosure.TokenSeconds = 0 },
		"stereo":                          func(c *Config) { c.Audio.Channels = 2 },
		"empty binding":                   func(c *Config) { c.UI.PushToTalk = " " },
		"duplicate keyword": func(c *Config) {
			s := c.Backends[testLocalBackend]
			s.Hints.Keywords = []string{"a", "a"}
			c.Backends[testLocalBackend] = s
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			config := testConfig()
			mutate(&config)
			if err := config.Validate(); err == nil {
				t.Fatalf("%s: expected a validation error", name)
			}
		})
	}
}

func TestConfigValidateRequiresDisclosureToNameEnabledHints(t *testing.T) {
	config := testConfig()
	section := config.Backends[testCloudBackend]
	section.Hints = HintPolicy{ProjectName: true, BranchName: true, Keywords: []string{"diff"}}
	section.Disclosure = &Disclosure{Text: "Your recording, project name, branch name, and keyword list go to OpenAI.", Version: 1}
	config.Backends[testCloudBackend] = section
	if err := config.Validate(); err != nil {
		t.Fatalf("disclosure naming every hint should validate: %v", err)
	}
	section.Disclosure = &Disclosure{Text: "Your recording and project name go to OpenAI.", Version: 1}
	config.Backends[testCloudBackend] = section
	err := config.Validate()
	if err == nil || !strings.Contains(err.Error(), "branch") {
		t.Fatalf("expected the branch hint to be required in the disclosure, got %v", err)
	}
}

func TestBackendSectionSplitsSharedKeysFromAdapterSettings(t *testing.T) {
	var section BackendSection
	raw := `{"executable":"/x","hints":{"project_name":true,"branch_name":false,"keywords":["k"]},"disclosure":{"text":"t","version":2},"extra":1}`
	if err := json.Unmarshal([]byte(raw), &section); err != nil {
		t.Fatal(err)
	}
	if !section.Hints.ProjectName || section.Disclosure == nil || section.Disclosure.Version != 2 {
		t.Fatalf("shared keys = %+v", section)
	}
	var settings struct {
		Executable string `json:"executable"`
	}
	if err := section.DecodeSettings(&settings); err == nil {
		t.Fatal("unknown adapter key must fail strict decoding")
	}
	var loose struct {
		Executable string `json:"executable"`
		Extra      int    `json:"extra"`
	}
	if err := section.DecodeSettings(&loose); err != nil || loose.Executable != "/x" || loose.Extra != 1 {
		t.Fatalf("settings = %+v err=%v", loose, err)
	}
	round, err := json.Marshal(section)
	if err != nil || !strings.Contains(string(round), `"hints"`) || !strings.Contains(string(round), `"executable"`) {
		t.Fatalf("round trip = %s err=%v", round, err)
	}
}

func TestAdapterFactoriesValidateTheirOwnSettings(t *testing.T) {
	config := testConfig()
	section := config.Backends[testLocalBackend]
	section.Settings = rawSettings(map[string]any{"executable": "whisper-cli", "model_path": "/tmp/m.bin",
		"model_sha256": "abc", "language": "en", "cpu_only": true, "sandbox": "strict"})
	config.Backends[testLocalBackend] = section
	if _, err := New(config); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative executable must fail in the factory, got %v", err)
	}
	config = testConfig()
	config.Backend = testCloudBackend
	section = config.Backends[testCloudBackend]
	section.Settings = rawSettings(map[string]any{"api_key_env": "lower", "model": "gpt-transcribe", "origin": "https://api.openai.com"})
	config.Backends[testCloudBackend] = section
	if _, err := New(config); err == nil || !strings.Contains(err.Error(), "environment") {
		t.Fatalf("bad env name must fail in the factory, got %v", err)
	}
	section.Settings = rawSettings(map[string]any{"api_key_env": "CG_TEST_OPENAI_KEY", "model": "gpt-transcribe", "origin": "http://api.openai.com"})
	config.Backends[testCloudBackend] = section
	if _, err := New(config); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("http origin must fail in the factory, got %v", err)
	}
	config.Backend = "nope"
	if _, err := New(config); err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("unknown backend = %v", err)
	}
}
