package harvest

// Codex adapter — interface verified against Codex CLI 0.149.0-alpha.4.3
// (measured lanes, both in this ONE file: the current flat rollout format
// and the legacy nested response-item format).
// The filename carries the newest verified interface; on
// re-verification against a newer CLI this file is renamed in place.

// Codex session files: ~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl
// lines: {timestamp, type|payload:{type}, payload:{...}} — the discriminator
// lives on payload.type in the nested generation and on the TOP-LEVEL type
// in the flatter one; both are handled everywhere below.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// codexFileMeta is one rollout file's summary-pass yield.
type codexFileMeta struct {
	title     string
	cwd       string
	threadID  string // session_meta.session_id — the bare thread uuid
	lines     int
	userTurns int
	// observed lineage (session_meta) — extraction lives in graph_codex.go
	metaSeen       bool   // first session_meta wins: children replay the parent's header
	metaID         string // session_meta.id — the rollout's OWN id
	parentThreadID string // set only when source declares a subagent
	lineageKind    string // "" | native-thread-spawn | native-subagent
	lineageDepth   int
	agentRole      string
	agentNickname  string
}

// codexMeta collects title/cwd/identity/usage from one pass. Usage is folded
// from the rollout's calls (usage_codex_0_149_0.go), never from the running
// total Codex writes.
func codexMeta(path string) (meta codexFileMeta, usage *SessionUsage) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := newLineScanner(f)
	calls := newCodexUsageCalls(path)
	for sc.Scan() {
		meta.lines++
		var obj map[string]any
		if json.Unmarshal(sc.Bytes(), &obj) != nil {
			continue
		}
		record, ok := decodeCodexRecord(obj)
		if !ok {
			continue
		}
		calls.observe(record, obj)
		switch record.Envelope {
		case "session_meta":
			if c, ok := record.Body["cwd"].(string); ok {
				meta.cwd = c
			}
			if id := anyString(record.Body["session_id"]); id != "" {
				meta.threadID = id
			} else if id := anyString(record.Body["id"]); id != "" {
				meta.threadID = id
			}
			applyCodexSessionMetaLineage(&meta, record.Body)
		case "event_msg":
			switch record.Kind {
			case "item_completed":
				item, _ := record.Body["item"].(map[string]any)
				switch anyString(item["type"]) {
				case "UserMessage":
					meta.userTurns++
					if meta.title == "" {
						meta.title = extractCodexContent(item["content"])
					}
				}
			case "user_message":
				meta.userTurns++
				if meta.title == "" {
					meta.title = anyString(record.Body["message"])
				}
			}
		case "user_message": // flatter variants observed in some versions
			meta.userTurns++
			if meta.title == "" {
				meta.title = anyString(record.Body["message"])
			}
		}
	}
	usage = calls.fold()
	return
}

// codexThreadNames maps thread uuid -> user-curated thread name from
// ~/.codex/session_index.jsonl, memoized on the index file's mtime+size.
var threadNamesMemo struct {
	sync.Mutex
	mod   int64
	size  int64
	names map[string]string
}

func codexThreadNames() map[string]string {
	path := filepath.Join(home(), ".codex", "session_index.jsonl")
	st, err := os.Stat(path)
	if err != nil {
		return map[string]string{}
	}
	threadNamesMemo.Lock()
	defer threadNamesMemo.Unlock()
	if threadNamesMemo.names != nil && threadNamesMemo.mod == st.ModTime().Unix() && threadNamesMemo.size == st.Size() {
		return threadNamesMemo.names
	}
	names := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return names
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		var row struct {
			ID   string `json:"id"`
			Name string `json:"thread_name"`
		}
		if json.Unmarshal(sc.Bytes(), &row) == nil && row.ID != "" {
			names[row.ID] = row.Name
		}
	}
	threadNamesMemo.mod, threadNamesMemo.size, threadNamesMemo.names = st.ModTime().Unix(), st.Size(), names
	return names
}

func normalizeCodex(path string, caps textCaps) ([]CanonicalEvent, int, *SessionUsage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, nil, err
	}
	defer f.Close()
	return normalizeCodexReader(f, caps)
}

func normalizeCodexReader(reader io.Reader, caps textCaps) ([]CanonicalEvent, int, *SessionUsage, error) {
	var events []CanonicalEvent
	calls := &codexUsageCalls{}
	unparsed, seq := 0, 0
	flatProtocolObserved := false
	flatTranscriptObserved := false
	sc := newLineScanner(reader)
	for sc.Scan() {
		var obj map[string]any
		if json.Unmarshal(sc.Bytes(), &obj) != nil {
			unparsed++
			continue
		}
		ts, _ := obj["timestamp"].(string)
		turnAnchor := "" // assigned after decode; the emit closures read it at call time
		emit := func(kind, name, text string) {
			seq++
			events = append(events, CanonicalEvent{Seq: seq, Kind: kind, Ts: ts, Name: name, Text: text, TurnAnchor: turnAnchor})
		}
		// emitClip records the pre-clip size so the console can fetch the rest.
		emitClip := func(kind, name, text string, n int) {
			t, full := clip(text, n)
			seq++
			events = append(events, CanonicalEvent{Seq: seq, Kind: kind, Ts: ts, Name: name, Text: t, FullLen: full, TurnAnchor: turnAnchor})
		}
		record, ok := decodeCodexRecord(obj)
		if !ok {
			unparsed++
			continue
		}
		turnAnchor = codexTurnAnchorFromRecord(record)
		calls.observe(record, obj)

		if record.Flat {
			switch record.Envelope {
			case "session_meta", "world_state", "inter_agent_communication_metadata":
				// Current Codex writes raw protocol records beside its presentation lane.
				// They are known evidence, not missing transcript lines, and emitting them
				// would duplicate items and expose application-injected context.
			case "response_item":
				flatProtocolObserved = true
			case "turn_context":
				// Model and effort are call facts; calls.observe above reads them.
			case "compacted":
				if text := codexPublicText(record.Body); text != "" {
					emitClip("summary", "", text, caps.system)
				}
			case "event_msg":
				switch record.Kind {
				case "item_completed":
					flatTranscriptObserved = true
					item, itemOK := record.Body["item"].(map[string]any)
					if !itemOK || item == nil {
						unparsed++
						break
					}
					drafts, known := codexPresentationItem(item)
					if !known {
						unparsed++
						break
					}
					for _, draft := range drafts {
						switch draft.Kind {
						case "user", "assistant":
							emit(draft.Kind, draft.Name, draft.Text)
						case "thinking":
							emitClip(draft.Kind, draft.Name, draft.Text, caps.thinking)
						case "summary", "system":
							emitClip(draft.Kind, draft.Name, draft.Text, caps.system)
						default:
							emitClip(draft.Kind, draft.Name, draft.Text, caps.tool)
						}
					}
				case "user_message":
					flatTranscriptObserved = true
					emit("user", "", anyString(record.Body["message"]))
				case "agent_message":
					flatTranscriptObserved = true
					emit("assistant", "", anyString(record.Body["message"]))
				case "agent_reasoning":
					flatTranscriptObserved = true
					if text := anyString(record.Body["text"]); strings.TrimSpace(text) != "" {
						emitClip("thinking", "", text, caps.thinking)
					}
				case "token_count", "task_started", "task_complete", "thread_settings_applied", "turn_aborted":
					// Usage facts; calls.observe above folds them.
				default:
					unparsed++
				}
			case "user_message":
				flatTranscriptObserved = true
				emit("user", "", anyString(record.Body["message"]))
			case "agent_message":
				flatTranscriptObserved = true
				emit("assistant", "", anyString(record.Body["message"]))
			case "agent_reasoning":
				flatTranscriptObserved = true
				if text := anyString(record.Body["text"]); strings.TrimSpace(text) != "" {
					emitClip("thinking", "", text, caps.thinking)
				}
			case "token_count":
				// Usage facts; calls.observe above folds them.
			default:
				unparsed++
			}
			continue
		}

		// Compatibility lane for the older nested generation.
		switch record.Envelope {
		case "session_meta":
		case "event_msg", "user_message", "agent_message", "token_count", "agent_reasoning":
			kind := record.Kind
			if record.Envelope != "event_msg" {
				kind = record.Envelope
			}
			switch kind {
			case "user_message":
				emit("user", "", anyString(record.Body["message"]))
			case "agent_message":
				emit("assistant", "", anyString(record.Body["message"]))
			case "agent_reasoning":
				if text := anyString(record.Body["text"]); strings.TrimSpace(text) != "" {
					emitClip("thinking", "", text, caps.thinking)
				}
			case "token_count":
				// Usage facts; calls.observe above folds them.
			case "task_started", "task_complete":
			default:
				unparsed++
			}
		case "response_item":
			switch record.Kind {
			case "message":
				role := anyString(record.Body["role"])
				if role == "user" || role == "assistant" {
					emit(role, "", extractCodexContent(record.Body["content"]))
				}
			case "function_call", "local_shell_call", "custom_tool_call":
				name := anyString(record.Body["name"])
				if name == "" {
					name = record.Kind
				}
				emitClip("tool_call", name,
					codexSelectedJSON(record.Body, "arguments", "input", "action"), caps.tool)
			case "function_call_output", "custom_tool_call_output", "local_shell_call_output":
				emitClip("tool_result", "", codexSelectedJSON(record.Body, "output", "status", "is_error"), caps.tool)
			case "reasoning":
				if text := codexPublicText(record.Body); text != "" {
					emitClip("thinking", "", text, caps.thinking)
				}
			default:
				unparsed++
			}
		case "turn_context":
			// Model and effort are call facts; calls.observe above reads them.
		case "compacted":
			if text := codexPublicText(record.Body); text != "" {
				emitClip("summary", "", text, caps.system)
			}
		default:
			unparsed++
		}
	}
	if sc.Err() != nil {
		unparsed++ // scanner stopped early (e.g. over-long line) — count, never hide
	}
	if flatProtocolObserved && !flatTranscriptObserved {
		// A raw protocol lane is intentionally not safe transcript input. If Codex
		// stops supplying every supported presentation lane, surface one honest
		// compatibility gap rather than returning a silently empty conversation.
		unparsed++
	}
	return events, unparsed, calls.fold(), nil
}

type codexEventDraft struct {
	Kind string
	Name string
	Text string
}

func codexPresentationItem(item map[string]any) ([]codexEventDraft, bool) {
	switch anyString(item["type"]) {
	case "UserMessage":
		return []codexEventDraft{{Kind: "user", Text: extractCodexContent(item["content"])}}, true
	case "AgentMessage":
		return []codexEventDraft{{Kind: "assistant", Text: extractCodexContent(item["content"])}}, true
	case "Reasoning":
		if text := codexPublicText(item); text != "" {
			return []codexEventDraft{{Kind: "thinking", Text: text}}, true
		}
		return nil, true
	case "CommandExecution":
		return codexToolDrafts("Bash", item,
			[]string{"command", "cwd", "parsed_cmd"}, []string{"status", "stdout", "stderr"}), true
	case "FileChange":
		return codexToolDrafts("apply_patch", item,
			[]string{"changes"}, []string{"status"}), true
	case "McpToolCall":
		name := joinCodexToolName(anyString(item["server"]), anyString(item["tool"]))
		return codexToolDrafts(name, item,
			[]string{"arguments"}, []string{"status", "result", "error"}), true
	case "DynamicToolCall":
		name := joinCodexToolName(anyString(item["namespace"]), anyString(item["tool"]))
		return codexToolDrafts(name, item,
			[]string{"arguments"}, []string{"status", "content_items", "success", "duration"}), true
	case "CollabAgentToolCall":
		name := anyString(item["tool"])
		if name == "" {
			name = "agent"
		}
		return codexToolDrafts(name, item,
			[]string{"sender_thread_id", "receiver_thread_ids", "receiver_agents"},
			[]string{"status", "agents_states"}), true
	case "SubAgentActivity":
		return codexToolDrafts("agent", item,
			[]string{"kind", "agent_path", "agent_thread_id"}, []string{"status"}), true
	case "ImageView":
		return codexToolDrafts("view_image", item, []string{"path"}, []string{"status"}), true
	case "Extension":
		name := anyString(item["kind"])
		if name == "" {
			name = "extension"
		}
		return codexToolDrafts(name, item,
			[]string{"query", "action"}, []string{"status", "results"}), true
	case "ContextCompaction":
		if text := codexPublicText(item); text != "" {
			return []codexEventDraft{{Kind: "summary", Text: text}}, true
		}
		return nil, true
	default:
		return nil, false
	}
}

func codexToolDrafts(name string, item map[string]any, callKeys, resultKeys []string) []codexEventDraft {
	if name == "" {
		name = "tool"
	}
	drafts := []codexEventDraft{{Kind: "tool_call", Name: name,
		Text: codexSelectedJSON(item, callKeys...)}}
	// item_completed is itself the provider's completion boundary. Emit the
	// adjacent result even when the provider supplies no public output so the
	// generic renderer closes this tool before the next item.
	drafts = append(drafts, codexEventDraft{Kind: "tool_result",
		Text: codexSelectedJSON(item, resultKeys...)})
	return drafts
}

func joinCodexToolName(scope, name string) string {
	if scope == "" {
		return name
	}
	if name == "" {
		return scope
	}
	return scope + "." + name
}

func codexSelectedJSON(source map[string]any, keys ...string) string {
	selected := make(map[string]any, len(keys))
	for _, key := range keys {
		value, ok := source[key]
		if !ok || !codexValuePresent(value) {
			continue
		}
		selected[key] = value
	}
	if len(selected) == 0 {
		return ""
	}
	return compactJSON(selected)
}

func codexValuePresent(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(typed) != ""
	case []any:
		return len(typed) > 0
	case map[string]any:
		return len(typed) > 0
	default:
		return true
	}
}

func codexPublicText(source map[string]any) string {
	for _, key := range []string{"summary_text", "summary", "content", "text"} {
		if text := strings.TrimSpace(extractCodexContent(source[key])); text != "" {
			return text
		}
	}
	return ""
}

// extractCodexContent flattens content arrays like [{type:input_text,text}].
func extractCodexContent(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, item := range c {
			if text, ok := item.(string); ok && text != "" {
				parts = append(parts, text)
				continue
			}
			if m, ok := item.(map[string]any); ok {
				if t := anyString(m["text"]); t != "" {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return anyString(v)
}

// uuidLen is the length of a canonical UUID (a codex thread id).
const uuidLen = 36

func init() { register(codexRuntime{}) }

// codexRuntime — codex session identity.
type codexRuntime struct{}

func (codexRuntime) Name() string { return "codex" }

// CLIRevision: the measured revision this adapter was verified against
// (both lanes — flat 0.149 and legacy nested — live in this one file).
func (codexRuntime) CLIRevision() string { return "0.149.0-alpha.4.3" }

// ActivityEvidence: the installed lifecycle hooks write the durable facts, so
// every kind is hook-exact. The session-scoped work facts (session.turn-*,
// session.tool-completed) are deliberately ABSENT: on the reference host the
// Codex hook lane has produced PreToolUse rows only (2026-09-12); a kind joins
// this map when its firing canary records rows (helper-session-attachment
// plan B2), never before.
func (codexRuntime) ActivityEvidence() map[string]string {
	return map[string]string{"session.started": "hook-exact", "session.active": "hook-exact",
		"session.ended": "hook-exact"}
}
func (codexRuntime) ActivityProcess(command string) bool {
	base := commandBase(command)
	return base == "codex" || strings.HasPrefix(base, "codex-")
}
func (codexRuntime) CouldMatchID(id string) bool {
	return strings.HasPrefix(id, "rollout-") || looksLikeUUID(id) || (len(id) > uuidLen && looksLikeUUID(id[len(id)-uuidLen:]))
}

// CanonicalID prefers the session_meta thread id — the key the governor index
// and enrichment DB are keyed under (see daemon/harvest.go, daemon/enrich.go).
// It falls back to the rollout filename's trailing uuid ONLY when session_meta
// carried none.
//
// Bug fix 2026-07-18: the enrichment lookup previously used the stem suffix
// UNCONDITIONALLY, so ~9% of codex sessions — where the rollout filename's uuid
// differs from the thread id (resumed / multi-rollout threads) — silently
// missed their enrichment. Pinned by TestCodexCanonicalIDPrefersThreadID.
//
// Exception 2026-08-29: subagent rollouts key by their OWN session_meta.id —
// see the identity-split comment in the body.
func (codexRuntime) CanonicalID(s SessionSummary) string {
	// Identity split (orchestration-agents-redesign-plan §4, lineage plan §7):
	// a subagent rollout's session_meta.session_id is the PARENT thread's id,
	// so keying children by ThreadID folded every child onto the parent (six
	// rail rows sharing one resume handle, enrichment overwrites). A rollout
	// whose source declares a subagent — thread_spawn AND guardian alike — is
	// its OWN artifact, keyed by its own session_meta.id. The parent thread
	// stays the resume affordance via the ResumeHandle capability.
	if s.LineageKind != "" && s.MetaID != "" {
		return s.MetaID
	}
	if s.ThreadID != "" {
		return s.ThreadID
	}
	if len(s.ID) > uuidLen {
		return s.ID[len(s.ID)-uuidLen:]
	}
	return s.ID
}

func (codexRuntime) MatchID(s SessionSummary, id string) bool {
	if s.ID == id || (s.MetaID != "" && s.MetaID == id) {
		return true
	}
	// A subagent rollout carries the parent's thread id in session_meta.
	// A thread-id query names the THREAD — its primary and resumed segments —
	// so it must never match a child: FindAll/Find resolve "newest wins", and
	// a guardian child is often the newest file, which handed the child back
	// as the thread's primary. Children answer only to their own ids above.
	if s.ThreadID == id {
		return s.LineageKind == ""
	}
	// Filename-suffix convenience: only a FULL trailing uuid may match, so a
	// short fragment cannot select an arbitrary sibling rollout.
	return looksLikeUUID(id) && strings.HasSuffix(s.ID, id)
}

func codexRoot() string { return filepath.Join(home(), ".codex", "sessions") }

func (codexRuntime) Collect() []fileJob {
	jobs, _ := collectCodexJobs(false)
	return jobs
}

func collectCodexJobs(strict bool) ([]fileJob, error) {
	var jobs []fileJob
	err := filepath.WalkDir(codexRoot(), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if strict {
				return err
			}
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if strict {
				return err
			}
			return nil
		}
		jobs = append(jobs, fileJob{runtime: "codex", path: path, mod: info.ModTime(), size: info.Size()})
		return nil
	})
	if os.IsNotExist(err) {
		return jobs, nil
	}
	return jobs, err
}

func (codexRuntime) Summarize(j fileJob) (SessionSummary, bool) {
	s := SessionSummary{Runtime: j.runtime, ID: stem(j.path), Project: j.project, Modified: j.mod, Path: j.path, HasTranscript: true}
	m, usage := codexMeta(j.path)
	s.Title = truncate(m.title, titleMaxLen)
	s.Lines = m.lines
	s.ThreadID = m.threadID
	s.UserTurns = m.userTurns
	applyCodexLineageSummary(&s, m)
	if m.cwd != "" {
		s.Project = m.cwd
		s.Cwd = m.cwd
	}
	if usage != nil {
		s.Context, s.Model, s.Turns = usage.Context, usage.Model, usage.Turns
	}
	return s, s.Lines > 0
}

func (codexRuntime) NormalizeFull(path string) ([]CanonicalEvent, int, *SessionUsage, error) {
	return normalizeCodex(path, deepCaps)
}

func (codexRuntime) Normalize(path string) ([]CanonicalEvent, int, *SessionUsage, error) {
	return normalizeCodex(path, transcriptCaps)
}

func (codexRuntime) NormalizeLifecycle(path string) (LifecycleBatch, error) {
	return normalizeCodexLifecycle(path)
}

func (codexRuntime) ThreadTitle(s SessionSummary) string {
	if n := codexThreadNames()[s.ThreadID]; n != "" {
		return truncate(n, titleMaxLen)
	}
	return ""
}

func (runtime codexRuntime) DiscoverTranscriptProjections(ctx context.Context,
	limits ProjectionReadLimits) (ProjectionDiscovery, error) {
	jobs, err := collectCodexJobs(true)
	if err != nil {
		return ProjectionDiscovery{Sessions: []ProjectionSession{}, Limitations: []ProjectionLimitation{}},
			projectionError(ProjectionSourceUnreadable, runtime.Name(), "", 0,
				limits.normalized().MaxSourceBytes, err)
	}
	return discoverFileTranscriptProjections(ctx, runtime, jobs, limits)
}

func (runtime codexRuntime) ReadTranscriptProjection(ctx context.Context, session ProjectionSession,
	limits ProjectionReadLimits) (ProjectionSnapshot, error) {
	if err := validateProjectionSession(runtime.Name(), session); err != nil {
		return ProjectionSnapshot{}, err
	}
	return readFileTranscriptProjection(ctx, runtime, session, limits,
		func(_ string, reader io.Reader) ([]CanonicalEvent, int, *SessionUsage, error) {
			return normalizeCodexReader(reader, transcriptCaps)
		})
}

func (runtime codexRuntime) TranscriptProjectionGeneration(ctx context.Context,
	session ProjectionSession, limits ProjectionReadLimits) (string, error) {
	if err := validateProjectionSession(runtime.Name(), session); err != nil {
		return "", err
	}
	current, err := refreshFileProjectionSession(ctx, runtime, session, limits)
	if err != nil {
		return "", err
	}
	return current.Generation, nil
}
