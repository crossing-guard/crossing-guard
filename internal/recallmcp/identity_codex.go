package recallmcp

import "encoding/json"

// Codex may serve several threads from one server process and scrubs the
// server's environment, but every tools/call carries the thread in
// params._meta (measured on 0.154.0-alpha.6.2): threadId, and the same id as
// x-codex-turn-metadata.session_id. The thread id is the rollout's native id.
type codexIdentity struct{}

func init() { registerIdentity("codex", codexIdentity{}) }

func (codexIdentity) SessionID(meta json.RawMessage) string {
	if id := metaString(meta, "threadId"); id != "" {
		return id
	}
	return metaString(meta, "x-codex-turn-metadata", "session_id")
}

func (codexIdentity) Place() (string, string) { return "", "" }
