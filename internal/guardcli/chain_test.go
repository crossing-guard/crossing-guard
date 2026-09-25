package guardcli

import (
	"strings"
	"testing"

	"crossing-guard/engine"
)

func TestDaemonSourceLabelNeverClaimsAHeldTailItDidNotCheck(t *testing.T) {
	if l := daemonSourceLabel(engine.ChainReport{Status: "none"}); !strings.Contains(l, "internal consistency only") {
		t.Fatalf("no anchor → must say so: %q", l)
	}
	held := true
	if l := daemonSourceLabel(engine.ChainReport{Status: "verified", TailMatchesHeld: &held}); !strings.Contains(l, "held anchor") {
		t.Fatalf("anchor checked → may say so: %q", l)
	}
}
