package main

import (
	"strings"
	"testing"

	"crossing-guard/internal/platform"
)

func TestParseOpenArgs(t *testing.T) {
	for _, args := range [][]string{{"--open"}, {"-o"}} {
		open, err := parseOpenArgs("test", args)
		if err != nil || !open {
			t.Fatalf("parseOpenArgs(%v) = %v, %v; want true, nil", args, open, err)
		}
	}
	if _, err := parseOpenArgs("test", []string{"--bogus"}); err == nil {
		t.Error("unknown option must be a usage error")
	}
	if _, err := parseOpenArgs("test", []string{"positional"}); err == nil {
		t.Error("unexpected positional argument must be a usage error")
	}
}

func TestParseInitArgs(t *testing.T) {
	dry, yes, recall, err := parseInitArgs([]string{"-n", "--yes"})
	if err != nil || !dry || !yes || recall != "" {
		t.Fatalf("parseInitArgs = (%v, %v, %q, %v), want true, true, \"\", nil", dry, yes, recall, err)
	}
	if _, _, _, err := parseInitArgs([]string{"--dry-run=true", "extra"}); err == nil {
		t.Error("unexpected positional argument must be a usage error")
	}
	if _, _, recall, _ := parseInitArgs([]string{"--recall"}); recall != "yes" {
		t.Errorf("--recall = %q", recall)
	}
	if _, _, recall, _ := parseInitArgs([]string{"--no-recall"}); recall != "no" {
		t.Errorf("--no-recall = %q", recall)
	}
	if _, _, _, err := parseInitArgs([]string{"--recall", "--no-recall"}); err != errRecallConflict {
		t.Errorf("--recall --no-recall = %v, want a usage error", err)
	}
}

func TestInitPlatformWarningUsesCapabilityRecord(t *testing.T) {
	if warning := initPlatformWarning(platform.For("darwin")); warning != "" {
		t.Fatalf("demonstrated platform warned: %q", warning)
	}
	for _, goos := range []string{"linux", "windows"} {
		support := platform.For(goos)
		warning := initPlatformWarning(support)
		for _, want := range []string{goos, "Stateful enforcement will NOT arm", string(support.Service), support.Note} {
			if !strings.Contains(warning, want) {
				t.Errorf("%s warning missing %q: %s", goos, want, warning)
			}
		}
	}
}

func TestParseChangeRootKeepsDataSelectionExplicit(t *testing.T) {
	data, sub, rest, err := parseDataSubcommand("change", []string{"--data", "/tmp/evidence", "show", "--session", "s"})
	if err != nil || data != "/tmp/evidence" || sub != "show" || strings.Join(rest, " ") != "--session s" {
		t.Fatalf("parse change root=(%q,%q,%v,%v)", data, sub, rest, err)
	}
	for _, args := range [][]string{{"--data"}, {"--data="}, {}} {
		if _, _, _, err := parseDataSubcommand("change", args); err == nil {
			t.Fatalf("invalid root args accepted: %v", args)
		}
	}
}

func TestParseUnderstandUsesTheSameExplicitDataSelection(t *testing.T) {
	data, sub, rest, err := parseDataSubcommand("understand", []string{"--data=/tmp/facts", "scan", "--repo", "/repo"})
	if err != nil || data != "/tmp/facts" || sub != "scan" || strings.Join(rest, " ") != "--repo /repo" {
		t.Fatalf("parse understand root=(%q,%q,%v,%v)", data, sub, rest, err)
	}
}
