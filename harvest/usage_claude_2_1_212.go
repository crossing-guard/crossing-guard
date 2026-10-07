package harvest

// Claude Code usage calls (token-usage-analytics plan §3.3), measured against
// CLI 2.1.2xx transcripts. One assistant API response is written as one line
// per content block, all sharing message.id; top-level files repeat identical
// usage on each, while subagent files write streaming snapshots whose LAST
// line is complete. So one call per id, and the last line of an id wins.
//
// Ownership: a top-level line belongs to the session whose id it carries
// (resume copies keep their original sessionId; see claudeTitle). A subagent
// line carries its PARENT's sessionId, so it belongs when that id is the
// directory it sits in; the agent is named by the file (red-team T-RT17).

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const claudeUsageReaderIdentity = "claude-2.1.212/usage-1"

// Claude's two cache lifetimes, stated per call under usage.cache_creation.
// The ids are this adapter's; the framework only sums them within cache-write.
var claudeCacheWriteParts = []struct{ field, id, label string }{
	{"ephemeral_5m_input_tokens", "cache-5m", "5-minute cache"},
	{"ephemeral_1h_input_tokens", "cache-1h", "1-hour cache"},
}

// claudeUsageCall maps one owned assistant line to a call. ok is false for a
// line that states no usage, has no id at all, or names no concrete model
// (the "<synthetic>" placeholders are transcript evidence, not model calls).
func claudeUsageCall(obj map[string]any, session, agent string) (UsageCall, bool) {
	if anyString(obj["type"]) != "assistant" {
		return UsageCall{}, false
	}
	message, _ := obj["message"].(map[string]any)
	usage, _ := message["usage"].(map[string]any)
	id, model := anyString(message["id"]), anyString(message["model"])
	if id == "" {
		// Every transcript measured states message.id; without one, the line's
		// own uuid keeps each line one call rather than dropping it.
		if uuid := anyString(obj["uuid"]); uuid != "" {
			id = session + ":" + uuid
		}
	}
	if usage == nil || id == "" || !isConcreteClaudeModel(model) {
		return UsageCall{}, false
	}
	at := parseUsageTime(anyString(obj["timestamp"]))
	call := UsageCall{ID: id, Session: session, Agent: agent, FirstAt: at, At: at, Model: model,
		Effort: anyString(obj["effort"]), Client: anyString(obj["version"]),
		TokenClasses: TokenClasses{
			Input:      statedTokens(usage, "input_tokens"),
			CacheRead:  statedTokens(usage, "cache_read_input_tokens"),
			CacheWrite: statedTokens(usage, "cache_creation_input_tokens"),
			Output:     statedTokens(usage, "output_tokens"),
		}}
	if details, ok := usage["output_tokens_details"].(map[string]any); ok {
		call.Reasoning = statedTokens(details, "thinking_tokens")
	}
	if lifetimes, ok := usage["cache_creation"].(map[string]any); ok {
		for _, part := range claudeCacheWriteParts {
			if count := statedTokens(lifetimes, part.field); count != nil {
				call.Parts = append(call.Parts, TokenPart{ID: part.id, Label: part.label,
					Of: TokenClassCacheWrite, Count: *count})
			}
		}
	}
	return call, true
}

// claudeOwnsLine is the top-level ownership rule shared by every Claude pass:
// a line with no sessionId predates resume copies and is the file's own.
func claudeOwnsLine(obj map[string]any, session string) bool {
	id, _ := obj["sessionId"].(string)
	return id == "" || id == session
}

func (claudeRuntime) UsageReader() string { return claudeUsageReaderIdentity }

// UsageSources lists every top-level transcript and every subagent file. A
// subagent file belongs to the session named by its directory.
func (claudeRuntime) UsageSources(ctx context.Context) ([]UsageSourceRef, error) {
	root := claudeRoot()
	projects, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return []UsageSourceRef{}, nil
		}
		return nil, err
	}
	refs := []UsageSourceRef{}
	roles := claudeRoleCache.pass()
	defer roles.done()
	for _, project := range projects {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !project.IsDir() {
			continue
		}
		dir := filepath.Join(root, project.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			switch {
			case entry.IsDir():
				for _, child := range claudeSubagentFiles(filepath.Join(dir, entry.Name())) {
					refs = appendClaudeUsageRef(refs, root, child.path, entry.Name())
					if last := len(refs) - 1; last >= 0 && refs[last].locator == child.path {
						refs[last].Role = roles.role(child.metaPath)
					}
				}
			case strings.HasSuffix(entry.Name(), ".jsonl"):
				refs = appendClaudeUsageRef(refs, root, filepath.Join(dir, entry.Name()),
					strings.TrimSuffix(entry.Name(), ".jsonl"))
			}
		}
	}
	sortUsageSources(refs)
	return refs, nil
}

func appendClaudeUsageRef(refs []UsageSourceRef, root, path, session string) []UsageSourceRef {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return refs
	}
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return refs
	}
	return append(refs, UsageSourceRef{Key: usageSourceKey(relative), Session: session,
		Marker: fileUsageMarker(info), Size: info.Size(), Modified: info.ModTime(),
		Born: sourceBirthTime(info), locator: path})
}

// claudeRoleCache keeps each subagent sidecar's role by its stat, so a listing
// pass parses only new or changed sidecars (session usage breakdown plan
// §5.1). Each pass keeps only the entries it named, so it needs no bound.
var claudeRoleCache = &claudeSidecarRoles{entries: map[string]claudeSidecarRole{}}

type claudeSidecarRole struct {
	size     int64
	modified time.Time
	role     string
}

type claudeSidecarRoles struct {
	mu      sync.Mutex
	entries map[string]claudeSidecarRole
	seen    map[string]claudeSidecarRole
}

// pass starts one listing pass; done drops every entry the pass did not name.
func (c *claudeSidecarRoles) pass() *claudeSidecarRoles {
	c.mu.Lock()
	c.seen = map[string]claudeSidecarRole{}
	return c
}

func (c *claudeSidecarRoles) done() {
	c.entries, c.seen = c.seen, nil
	c.mu.Unlock()
}

// role is the sidecar's agentType, or "" when it is absent, unreadable or
// states none. It is called only between pass and done.
func (c *claudeSidecarRoles) role(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	if entry, ok := c.entries[path]; ok && entry.size == info.Size() && entry.modified.Equal(info.ModTime()) {
		c.seen[path] = entry
		return entry.role
	}
	entry := claudeSidecarRole{size: info.Size(), modified: info.ModTime()}
	if meta, ok := readClaudeSubagentMeta(path); ok {
		entry.role = meta.AgentType
	}
	c.seen[path] = entry
	return entry.role
}

// ReadUsage reads one transcript or subagent file from its cursor. Claude
// states every fact per line, so no state is carried between reads.
func (claudeRuntime) ReadUsage(ctx context.Context, request UsageReadRequest) (UsageBatch, error) {
	path := request.Source.locator
	if path == "" {
		return UsageBatch{}, errUsageLocator
	}
	session, agent := request.Source.Session, claudeSubagentID(path)
	folder := newUsageCallFolder()
	return readFileUsage(ctx, path, request.Cursor, request.Budget, fileUsageAdapter{
		runtime: "claude",
		begin: func(json.RawMessage) fileUsageLine {
			return func(record []byte, _ int64) {
				var obj map[string]any
				if json.Unmarshal(record, &obj) != nil {
					return
				}
				if !claudeOwnsLine(obj, session) {
					return
				}
				if call, ok := claudeUsageCall(obj, session, agent); ok {
					folder.observe(call)
				}
			}
		},
		finish: func() ([]UsageCall, json.RawMessage, error) { return folder.admitted(), nil, nil },
	})
}

// claudeSubagentID is the agent id a subagent file names, or "" for a
// top-level transcript.
func claudeSubagentID(path string) string {
	if filepath.Base(filepath.Dir(path)) != claudeSubagentsDirName {
		return ""
	}
	name := filepath.Base(path)
	return strings.TrimSuffix(strings.TrimPrefix(name, "agent-"), ".jsonl")
}

// statedTokens reads a count the runtime stated; nil when absent or not a
// non-negative number.
func statedTokens(fields map[string]any, key string) *int64 {
	value, ok := fields[key].(float64)
	if !ok || value < 0 || value != value {
		return nil
	}
	count := int64(value)
	return &count
}

// parseUsageTime reads a vendor timestamp; zero when absent or malformed.
func parseUsageTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}
