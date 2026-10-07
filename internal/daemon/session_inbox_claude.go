package daemon

// The vendor's inbox registry facts (session-message-layer plan §5.1): where
// the live-session registry lives, what its rows carry, and which socket paths
// are vetted. This file may name the runtime; the generic resolver
// (session_inbox.go) may not. Nothing here writes: the registry is the
// runtime's own record of its own sessions.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"crossing-guard/internal/vendorpaths"
)

// inboxRegistryRow is the one registry row this runtime writes per live
// session. Field names follow the vendor's own registry; only what inbox
// resolution needs is decoded.
type inboxRegistryRow struct {
	PID                 int    `json:"pid"`
	SessionID           string `json:"sessionId"`
	Cwd                 string `json:"cwd"`
	ProcStart           string `json:"procStart"`
	Status              string `json:"status"`
	MessagingSocketPath string `json:"messagingSocketPath"`
}

// claudeVettedSocketDirs lists the only directories this resolver attests a
// socket path may live in: the vendor's own per-user socket namespaces (the
// vendor's own reply-target vetting checks the same shape). Anything else is
// unvettable and is never connected to.
func claudeVettedSocketDirs() []string {
	dirs := []string{"/tmp/cc-socks"}
	dirs = append(dirs, fmt.Sprintf("/tmp/cc-socks-%d", os.Getuid()))
	if runtime := os.Getenv("XDG_RUNTIME_DIR"); runtime != "" {
		dirs = append(dirs, filepath.Join(runtime, "cc-socks"))
	}
	return dirs
}

// claudeSocketPathVetted reports whether the path is inside one of the vetted
// directories. The path is cleaned lexically first, so a traversal shape
// (`/tmp/cc-socks/../etc`) never attests; a symlink is not followed: the
// literal path string is what is vetted, matching the vendor's own refusal
// shape.
func claudeSocketPathVetted(path string) bool {
	if path == "" {
		return false
	}
	clean := filepath.Clean(path)
	if clean != path && clean+"/" != path+"/" {
		// A path that only means itself after cleaning (traversal, doubled
		// slashes, a trailing dot) is not an inbox path.
		return false
	}
	for _, dir := range claudeVettedSocketDirs() {
		if prefix := dir + "/"; strings.HasPrefix(clean, prefix) {
			return true
		}
	}
	return false
}

// resolveClaudeSessionInbox resolves one canonical identity from the
// runtime's registry. Exact identity only: the native id, which the delivery
// host already confirmed through the catalog's alternates before the adapter
// is called. A registry row the caller did not name is not found; ambiguity is
// refused, never resolved by "most recent".
func (claudeChatDriver) ResolveSessionInbox(identity SessionIdentity) (sessionInbox, bool, string) {
	if identity.NativeID == "" {
		return sessionInbox{}, false, "registry requires the runtime's native session id"
	}
	rows, err := readInboxRegistry()
	if err != nil {
		return sessionInbox{}, false, "session registry unreadable: " + err.Error()
	}
	var matches []inboxRegistryRow
	for _, row := range rows {
		if row.SessionID == identity.NativeID {
			matches = append(matches, row)
		}
	}
	switch len(matches) {
	case 0:
		return sessionInbox{}, false, "no live inbox for this session"
	case 1:
		row := matches[0]
		if !claudeSocketPathVetted(row.MessagingSocketPath) {
			return sessionInbox{}, false, "unvettable-path"
		}
		return sessionInbox{SocketPath: row.MessagingSocketPath, RegistryPID: row.PID,
			SessionID: row.SessionID, Cwd: row.Cwd, ProcStart: row.ProcStart, Status: row.Status}, true, ""
	default:
		return sessionInbox{}, false, "ambiguous-inbox"
	}
}

// readInboxRegistry reads every registry row, skipping rows that fail to parse
// or name no socket: the registry may hold partial rows mid-write, and one bad
// row must not blind the resolver to the others.
func readInboxRegistry() ([]inboxRegistryRow, error) {
	entries, err := os.ReadDir(filepath.Join(homeDir(), filepath.FromSlash(vendorpaths.ClaudeSessionsRelative)))
	if err != nil {
		return nil, err
	}
	rows := make([]inboxRegistryRow, 0, len(entries))
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(
			homeDir(), filepath.FromSlash(vendorpaths.ClaudeSessionsRelative), entry.Name()))
		if err != nil {
			continue
		}
		var row inboxRegistryRow
		if err := json.Unmarshal(data, &row); err != nil {
			continue
		}
		if row.PID <= 0 || row.SessionID == "" || row.MessagingSocketPath == "" {
			continue
		}
		rows = append(rows, row)
	}
	return rows, nil
}
