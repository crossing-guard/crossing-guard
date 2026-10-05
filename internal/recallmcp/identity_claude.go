package recallmcp

import (
	"encoding/json"
	"os"
)

// Claude Code starts one server per session and passes that session's id in
// CLAUDE_CODE_SESSION_ID (measured on 2.1.280; undocumented) and its folder in
// CLAUDE_PROJECT_DIR. After /clear the process, and so the id, is the previous
// session's (recall-mcp-v1-plan §11).
type claudeIdentity struct{}

func init() { registerIdentity("claude", claudeIdentity{}) }

func (claudeIdentity) SessionID(json.RawMessage) string { return os.Getenv("CLAUDE_CODE_SESSION_ID") }

func (claudeIdentity) Place() (string, string) {
	if dir := os.Getenv("CLAUDE_PROJECT_DIR"); dir != "" {
		return dir, "claude_project_dir"
	}
	return "", ""
}
