package main

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"crossing-guard/internal/approvalbridge"
)

func TestTopLevelHelpClassifiesEveryDispatchedVerbOnce(t *testing.T) {
	verbs := dispatchedVerbs(t)
	verbs = append(verbs, approvalbridge.Command)

	for _, verb := range verbs {
		pattern := regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(verb) + `(?:\s|$)`)
		if count := len(pattern.FindAllStringIndex(topLevelUsage, -1)); count != 1 {
			t.Errorf("help entries for %q = %d, want exactly 1", verb, count)
		}
	}
	for _, heading := range []string{"PUBLIC ALPHA CANDIDATE — USER JOURNEY", "EXPERIMENTAL SOURCE-VISIBLE TOOLS",
		"INTERNAL / INSTALLED MACHINE INTERFACES"} {
		if !strings.Contains(topLevelUsage, heading) {
			t.Errorf("help missing stability heading %q", heading)
		}
	}
	if strings.Contains(topLevelUsage, "not built yet") {
		t.Fatal("top-level help advertises an unbuilt path")
	}
}

func dispatchedVerbs(t *testing.T) []string {
	t.Helper()
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve help test source")
	}
	body, err := os.ReadFile(filepath.Join(filepath.Dir(testFile), "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(body)
	start := strings.Index(source, "switch os.Args[1]")
	if start < 0 {
		t.Fatal("top-level dispatch switch not found")
	}
	end := strings.Index(source[start:], "\n\tdefault:")
	if end < 0 {
		t.Fatal("top-level dispatch default not found")
	}
	matches := regexp.MustCompile(`"([a-z-]+)"`).FindAllStringSubmatch(source[start:start+end], -1)
	verbs := make([]string, 0, len(matches))
	seen := map[string]bool{}
	for _, match := range matches {
		if !seen[match[1]] {
			seen[match[1]] = true
			verbs = append(verbs, match[1])
		}
	}
	sort.Strings(verbs)
	return verbs
}
