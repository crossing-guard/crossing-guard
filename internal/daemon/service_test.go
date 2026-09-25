package daemon

import (
	"strings"
	"testing"
)

// TestLaunchdPlistKeepsDaemonAlive pins the two properties the product depends on:
// the service starts at login and is restarted if it exits. Without both, "always-on
// capture" is a claim the software cannot keep — live state simply stops.
//
// This tests the generated plist only. The launchctl bootstrap path is deliberately
// NOT exercised here: it registers a real user service, which is a deploy action, not
// something a test suite should do to a developer's machine. That path is verified at
// deploy by checking the service is loaded and the daemon comes back after a kill.
func TestLaunchdPlistKeepsDaemonAlive(t *testing.T) {
	p := launchdPlist("/usr/local/bin/crossing-guard", "/Users/x/.crossing-guard", "127.0.0.1:7788", "/Users/x/.crossing-guard/daemon.log")

	for _, want := range []string{
		"<key>RunAtLoad</key><true/>",   // survives logout/reboot
		"<key>KeepAlive</key><true/>",   // survives a crash
		"/usr/local/bin/crossing-guard", // the resolved binary, not a bare name on PATH
		"<string>serve</string>",        // actually runs the daemon
		"/Users/x/.crossing-guard",      // pinned data dir, not a default that could drift
		launchdLabel,                    // stable label so re-install replaces, not duplicates
		"127.0.0.1:7788",                // port PINNED: an ad-hoc squatter must not bind-fail us forever
	} {
		if !strings.Contains(p, want) {
			t.Errorf("launchd plist missing %q\n---\n%s", want, p)
		}
	}
}
