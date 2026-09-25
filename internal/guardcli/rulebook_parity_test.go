package guardcli

import (
	"os"
	"path/filepath"
	"testing"
)

// This stays in guardcli deliberately: rulebook unit tests prove documents compile;
// this proves a migrated active document still reaches the hook's verdict mapping.
func TestMigratedRulebookReachesCommandVerdict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules.json")
	raw := `{"rules":[{"id":"legacy-ask","action":"ask","guards":[{"name":"g","pattern":"touch-me"}]}]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CG_RULES", path)
	verdict, err := CheckCommand("please touch-me")
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Decision != "ask" || verdict.Rule != "legacy-ask" {
		t.Fatalf("verdict = %+v, want migrated legacy-ask", verdict)
	}
}
