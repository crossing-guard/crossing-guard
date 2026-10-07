package main

import (
	"regexp"
	"testing"

	"crossing-guard/engine"
)

// The demo only stages a canary its rules would hard-deny: an ask would hang in the
// inbox, and a warn proceeds in the hook, so neither is a deny to demonstrate.
func TestCanaryWouldBeDeniedOnlyByAHardDeny(t *testing.T) {
	canary := engine.Predicate{Tag: engine.CommandTagKey, Matches: regexp.QuoteMeta(canaryCommand)}
	for mode, want := range map[engine.Mode]bool{engine.HardBlock: true,
		engine.ConfirmAndRecord: false, engine.WarnAndProceed: false, engine.SilentLog: false} {
		pol := &engine.Policy{Rules: []engine.Rule{{ID: "canary", Mode: mode, If: canary}}}
		if got := canaryWouldBeDenied(pol); got != want {
			t.Errorf("%s canary rule: canaryWouldBeDenied=%v, want %v", mode, got, want)
		}
	}
	if canaryWouldBeDenied(&engine.Policy{}) {
		t.Error("no rules: canary reported denied")
	}
}
