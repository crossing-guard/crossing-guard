package daemon

import (
	"fmt"
	"os"
	"testing"

	"crossing-guard/store"
)

// realHomeOptIn is the environment variable of the one test that reads the person's
// real session stores on purpose (TestGovernorReplayReal). When it is set
// the package's tests keep the home directory they were started with.
const realHomeOptIn = "CG_REPLAY_REAL"

// vendorRootOverrides are the variables that point a runtime's store somewhere other
// than under the home directory. A developer's exported value would lead a test back
// to real data, so the package starts without them; a test that needs one sets it.
var vendorRootOverrides = []string{"CODEX_HOME", "OPENCODE_DATA_HOME", "OPENCODE_CONFIG_DIR", "XDG_DATA_HOME", "XDG_CONFIG_HOME"}

// TestMain gives the package's tests a home directory of their own: an empty one.
//
// Without it, a test that does not set HOME itself runs against the machine's real
// one. The session scan (ScanSessions, reached from every session and entity report
// through sessionTitles) then parses every Claude Code and Codex transcript the
// developer has ever written — measured 2026-10-04 on a machine with 4,098 transcripts
// (6.8 GB): 334 s under -race for the first test that scans, and about 11 s for each
// one after it. That cost grows with the developer's own use and has nothing to do
// with what the tests assert. It also means tests read a person's real transcripts,
// and code that falls back to the default data directory reads the installed one.
//
// A test that sets HOME (t.Setenv) is unaffected. No test in the package shells out to
// the Go tool, and every test that commits with git names its own identity, so nothing
// here needs the real home's caches or git configuration.
//
// It also seeds new stores (store.SeedFreshStores). Nearly every test here opens a new
// store, some two or three, and under -race each one spent about 2.4 s creating the
// schema and climbing the migration ladder — most of the package's wall time. A seeded
// open starts from a copy of that ladder's own output and still runs everything Open
// runs over an existing store. A path that already holds a file is opened as it is, so
// the tests that prepare an older, busy or unreadable store are unaffected. The ladder
// itself is proved in the store package, which does not seed.
//
// A re-executed copy of this binary that is a `serve` child (serveStartupHelperEnv) gets
// neither: it is the package's only real boot on a new data directory, so it climbs the
// ladder itself and keeps the home its parent gave it.
func TestMain(m *testing.M) {
	if os.Getenv(serveStartupHelperEnv) != "" {
		os.Exit(m.Run())
	}
	stop, err := store.SeedFreshStores()
	if err != nil {
		fmt.Fprintf(os.Stderr, "the tests' store seed could not be made: %v\n", err)
		os.Exit(1)
	}
	testMainArranged = "the store seed"
	code := runWithOwnHome(m)
	stop()
	os.Exit(code)
}

// testMainArranged names what TestMain arranged for this process, or is empty when it
// arranged nothing. The serve child reads it to prove it boots unarranged.
var testMainArranged string

func runWithOwnHome(m *testing.M) int {
	if os.Getenv(realHomeOptIn) != "" {
		return m.Run()
	}
	home, err := os.MkdirTemp("", "crossing-guard-daemon-test-home-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "the tests' own home directory could not be made: %v\n", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(home) }()
	if err := os.Setenv("HOME", home); err != nil {
		fmt.Fprintf(os.Stderr, "HOME could not be set for the tests: %v\n", err)
		return 1
	}
	for _, name := range vendorRootOverrides {
		if err := os.Unsetenv(name); err != nil {
			fmt.Fprintf(os.Stderr, "%s could not be cleared for the tests: %v\n", name, err)
			return 1
		}
	}
	return m.Run()
}
