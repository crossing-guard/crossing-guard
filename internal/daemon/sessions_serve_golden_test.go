package daemon

import (
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"crossing-guard/store"
)

// The rail's three response shapes are pinned byte for byte. Session
// organization (tags, filters, saved views) extends this handler, and its
// first invariant is that a request carrying none of the new parameters, over
// sessions carrying no tags, answers exactly as it did before. A struct-level
// assertion cannot prove that: a new field without omitempty, a changed
// embedding, or a reordered key all pass one and break the browser.
//
// Regenerate deliberately with UPDATE_SESSIONS_GOLDEN=1 and review the diff.
func TestSessionsResponsesAreByteStable(t *testing.T) {
	restore := agentSessionParentsRead
	t.Cleanup(func() { agentSessionParentsRead = restore })
	agentSessionParentsRead = func(ids []string) map[string]store.AgentSessionParent {
		return map[string]store.AgentSessionParent{
			"agent-1": {Role: "follower", RootRuntime: "codex", RootNativeSessionID: "thread-p"},
			"orphan":  {Role: "helper", RootRuntime: "codex", RootNativeSessionID: "absent-parent"},
		}
	}

	base := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	parent := sessionFixture("/work/alpha", "codex", "parent", base)
	parent.MetaID, parent.ThreadID = "parent-meta", "thread-p"
	parent.Title, parent.Branch, parent.Model = "Parent thread", "feat/x", "gpt-5"
	agent := sessionFixture("/work/alpha", "codex", "agent-1", base.Add(-time.Minute))
	orphan := sessionFixture("/work/alpha", "codex", "orphan", base.Add(-2*time.Minute))
	rows := []SessionSummary{parent, agent, orphan}
	for i := 0; i < 17; i++ {
		row := sessionFixture("/work/alpha", "claude", string(rune('a'+i)), base.Add(-time.Duration(i+3)*time.Minute))
		row.Title = "Alpha " + string(rune('A'+i))
		rows = append(rows, row)
	}
	beta := sessionFixture("/work/beta", "claude", "beta-1", base.Add(-time.Hour))
	beta.Title, beta.Turns, beta.UserTurns = "Beta one", 12, 4
	loose := sessionFixture("", "opencode", "loose-1", base.Add(-2*time.Hour))
	loose.RepositoryKey = ""
	rows = append(rows, beta, loose)

	scan := func() []SessionSummary { return append([]SessionSummary(nil), rows...) }
	presence := openFixture(rows[5], beta)
	openSet := func(time.Time) presenceOpenSet { return presence }

	cases := []struct{ name, target string }{
		{"bare_array", "/api/sessions"},
		{"rail", "/api/sessions?view=rail&repository_limit=200"},
		{"repository_all", "/api/sessions?view=repository&repository=/work/alpha&mode=all&offset=0&limit=15"},
		{"repository_all_page2", "/api/sessions?view=repository&repository=/work/alpha&mode=all&offset=15&limit=15"},
		{"repository_open", "/api/sessions?view=repository&repository=/work/alpha&mode=open&offset=0&limit=15"},
		{"repository_selected_child", "/api/sessions?view=repository&repository=/work/alpha&mode=all&offset=0&limit=15&selected_runtime=codex&selected_id=agent-1"},
		{"repository_selected_far", "/api/sessions?view=repository&repository=/work/alpha&mode=all&offset=0&limit=15&selected_runtime=claude&selected_id=q"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handleSessionsWith(rec, httptest.NewRequest("GET", tc.target, nil), scan, openSet)
			if rec.Code != 200 {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			path := filepath.Join("testdata", "sessions_golden", tc.name+".json")
			if os.Getenv("UPDATE_SESSIONS_GOLDEN") == "1" {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, rec.Body.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("missing golden (run with UPDATE_SESSIONS_GOLDEN=1): %v", err)
			}
			if !bytes.Equal(rec.Body.Bytes(), want) {
				t.Fatalf("response bytes changed for %s\n got: %s\nwant: %s", tc.target, rec.Body.Bytes(), want)
			}
		})
	}
}
