package recallmcp

import (
	"encoding/json"
	"os"
	"sort"
)

// CallerIdentity is what one runtime tells its MCP server about the session
// calling a tool (recall-mcp-v1-plan §3.6). Each runtime registers one from its
// own file; this file names none.
type CallerIdentity interface {
	// SessionID names the calling session from the process environment and the
	// call's params._meta, or "" when the runtime does not say.
	SessionID(meta json.RawMessage) string
	// Place is the caller's working folder when the runtime names one, with a
	// label for where it came from; "" when it does not.
	Place() (dir, source string)
}

var identities = map[string]CallerIdentity{}

func registerIdentity(runtime string, identity CallerIdentity) { identities[runtime] = identity }

// Runtimes lists the runtimes the server can run for.
func Runtimes() []string {
	names := make([]string, 0, len(identities))
	for name := range identities {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// callerPlace is the runtime's named folder, else the server process's own.
func callerPlace(identity CallerIdentity) (string, string) {
	if dir, source := identity.Place(); dir != "" {
		return dir, source
	}
	if cwd, err := os.Getwd(); err == nil {
		return cwd, "process_cwd"
	}
	return "", ""
}

// metaString reads one string field of a JSON object, "" when absent.
func metaString(raw json.RawMessage, path ...string) string {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	for _, key := range path {
		object, ok := value.(map[string]any)
		if !ok {
			return ""
		}
		value = object[key]
	}
	text, _ := value.(string)
	return text
}
