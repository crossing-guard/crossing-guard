package daemon

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every browser caller of agentWatchesSession must hand it the session's
// cwd_key; one that forgets silently falls back to comparing spellings and
// says "nobody is watching" a /private/tmp session whose place was saved as
// /tmp (place-root-folder-identity plan §3.3, red-team PR-3b).
func TestWatchingCallersPassTheSessionFolderKey(t *testing.T) {
	callers := map[string]bool{}
	err := filepath.WalkDir("static/js", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".js") || strings.HasSuffix(path, "agents-api.js") {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		calls := strings.Count(string(body), "agentWatchesSession(")
		if calls == 0 {
			return nil
		}
		callers[filepath.Base(path)] = true
		// Every call needs its own cwdKey argument, built from cwd_key.
		if passes := strings.Count(string(body), "cwdKey:"); passes < calls || !strings.Contains(string(body), "cwd_key") {
			t.Errorf("%s calls agentWatchesSession %d times but passes the session's cwd_key as cwdKey %d times", path, calls, passes)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"session-orchestration.js", "session-agents-panel.js"} {
		if !callers[want] {
			t.Errorf("expected caller %s no longer calls agentWatchesSession; update this contract", want)
		}
	}
	// The header's context comes from the session detail in sessions.js.
	sessions, err := os.ReadFile("static/js/views/sessions.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sessions), "cwd_key: String(d.cwd_key || '')") {
		t.Error("sessions.js no longer hands the session detail's cwd_key to the header's watching check")
	}
}
