package guardcli

import "testing"

func TestClaudeConfigIsAbsentWhenHomeCannotResolve(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	t.Setenv("HOMEDRIVE", "")
	t.Setenv("HOMEPATH", "")
	if got := (claudeInstaller{}).ResolveConfig(); got != "" {
		t.Fatalf("ResolveConfig = %q, want empty when HOME cannot resolve", got)
	}
}

func TestInstallHelpSpellingsAreRecognized(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"--settings", "somewhere", "--help"}} {
		if !installHelpRequested(args) {
			t.Fatalf("install help not recognized in %q", args)
		}
	}
	if installHelpRequested([]string{"--settings", "somewhere"}) {
		t.Fatal("ordinary install arguments reported as help")
	}
}
