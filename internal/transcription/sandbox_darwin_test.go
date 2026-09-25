//go:build darwin

package transcription

import (
	"strings"
	"testing"
)

func TestSandboxProfileDeniesByDefaultAndNeverGrantsNetworkOrGPU(t *testing.T) {
	profile := BuildSandboxProfile(SandboxSpec{Executable: "/opt/homebrew/bin/whisper-cli",
		ReadPaths: []string{"/tmp/models/model.bin"}, WritePaths: []string{"/tmp/work"}}, "/opt/homebrew/Cellar/whisper-cpp/1.9.2/bin/whisper-cli")
	for _, required := range []string{"(deny default)", "(deny network*)", "(allow file-map-executable)", "(allow file-read* (literal \"/\"))",
		`(allow process-exec (literal "/opt/homebrew/bin/whisper-cli") (literal "/opt/homebrew/Cellar/whisper-cpp/1.9.2/bin/whisper-cli"))`,
		`(subpath "/opt/homebrew")`, `(allow file-read* file-write* (subpath "/private/tmp/work"))`} {
		if !strings.Contains(profile, required) {
			t.Fatalf("profile missing %q:\n%s", required, profile)
		}
	}
	for _, forbidden := range []string{"iokit", "(allow default)", "(allow network", "/Users"} {
		if strings.Contains(profile, forbidden) {
			t.Fatalf("profile must not contain %q:\n%s", forbidden, profile)
		}
	}
}

func TestSandboxProfileEscapesQuotesInPaths(t *testing.T) {
	profile := BuildSandboxProfile(SandboxSpec{Executable: `/opt/x/bin/tool`, WritePaths: []string{`/tmp/we"ird`}}, `/opt/x/bin/tool`)
	if !strings.Contains(profile, `we\"ird`) {
		t.Fatalf("quote not escaped:\n%s", profile)
	}
}
