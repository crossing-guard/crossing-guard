package harvest

// Claude Code adapter — interface verified against Claude Code CLI 2.1.212
// (the installed revision this adapter's parsing was measured against).
// The filename carries the newest verified interface; on
// re-verification against a newer CLI this file is renamed in place.

// Claude Code session files: ~/.claude/projects/<escaped-cwd>/<uuid>.jsonl
// lines: {type: user|assistant|summary|system|..., message:{role,content},
// cwd, timestamp}; content = string OR
// [{type:text|tool_use|tool_result|thinking, ...}].

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// claudeTitle scans for a summary line or first user text; counts lines and
// real user turns (injected "<...>" context and tool_result-only lines are
// not user turns and never make a title). It also folds the session's model
// calls in the same pass, for the rail's model, call count and context.
// claudeScan is one pass over a claude transcript. A struct, not a six-value
// return: the vendor titles pushed it past what a tuple can carry legibly.
type claudeScan struct {
	Title       string // best available — a vendor title if present, else the first prompt
	CustomTitle string // the user's own rename (highest authority)
	AITitle     string // the runtime's generated title
	Lines       int
	Usage       *SessionUsage
	Cwd         string
	UserTurns   int
}

func claudeTitle(path string) claudeScan {
	f, err := os.Open(path)
	if err != nil {
		return claudeScan{}
	}
	defer f.Close()
	// RESUME COPIES: when a session is resumed, Claude Code copies the prior
	// conversation into the NEW file, and the copied records keep their ORIGINAL
	// sessionId. Attributing by filename therefore counts the same turns and tokens
	// under two sessions (measured: 5 of 507 files, 4,758 records, ~1.8% of the
	// corpus — one session was resumed twice and forked into two files).
	//
	// Rule: a record belongs to the session whose id it CARRIES. Copied history is
	// still shown in the transcript (it is real context), but it is not counted as
	// this session's work. Anything that aggregates must use `owns`.
	own := stem(path)
	owns := func(obj map[string]any) bool { return claudeOwnsLine(obj, own) }
	sc := newLineScanner(f)
	title, firstUser, cwd, n, userTurns := "", "", "", 0, 0
	customTitle, aiTitle := "", ""
	calls := newUsageCallFolder()
	for sc.Scan() {
		var obj map[string]any
		if json.Unmarshal(sc.Bytes(), &obj) != nil {
			continue
		}
		// Count OUR lines only. Counting every line while Usage excluded copied
		// records made one scan report two inconsistent volumes for one file.
		if owns(obj) {
			n++
		}
		if cwd == "" {
			if c, ok := obj["cwd"].(string); ok {
				cwd = c
			}
		}
		switch obj["type"] {
		case "summary":
			if s, ok := obj["summary"].(string); ok && title == "" {
				title = s
			}
		// Claude Code writes its OWN titles into the transcript and rewrites them as
		// the session evolves, so the LAST record wins. Without these we fell back to
		// truncating the first user prompt — which is why sessions read like a
		// mid-sentence fragment instead of the label shown in the session list.
		case "custom-title":
			if s, ok := obj["customTitle"].(string); ok && s != "" {
				customTitle = s
			}
		case "ai-title":
			if s, ok := obj["aiTitle"].(string); ok && s != "" {
				aiTitle = s
			}
		case "user":
			if txt := firstText(obj); txt != "" && !strings.HasPrefix(txt, "<") {
				if !owns(obj) {
					continue // copied history: another session's turn
				}
				userTurns++
				if firstUser == "" {
					firstUser = txt
				}
			}
		case "assistant":
			if !owns(obj) {
				continue // copied history: its tokens were already counted at the source
			}
			if call, ok := claudeUsageCall(obj, own, ""); ok {
				calls.observe(call)
			}
		}
	}
	// Precedence: the user's own rename beats the generated title, which beats the
	// transcript summary, which beats truncating the first prompt (not a title).
	switch {
	case customTitle != "":
		title = customTitle
	case aiTitle != "":
		title = aiTitle
	case title == "":
		title = firstUser
	}
	return claudeScan{
		Title: truncate(title, titleMaxLen), CustomTitle: customTitle, AITitle: aiTitle,
		Lines: n, Usage: FoldCalls(calls.admitted()), Cwd: cwd, UserTurns: userTurns,
	}
}

func normalizeClaude(path string, caps textCaps) ([]CanonicalEvent, int, *SessionUsage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, nil, err
	}
	defer f.Close()
	return normalizeClaudeReader(path, f, caps)
}

func normalizeClaudeReader(path string, reader io.Reader, caps textCaps) ([]CanonicalEvent, int, *SessionUsage, error) {
	var events []CanonicalEvent
	calls := newUsageCallFolder()
	unparsed, seq := 0, 0
	// Same resume-copy rule as claudeTitle: copied records are EMITTED (they are real
	// context the reader wants) but never re-counted — their tokens already belong to
	// the session that produced them.
	own := stem(path)
	owns := func(obj map[string]any) bool { return claudeOwnsLine(obj, own) }
	sc := newLineScanner(reader)
	for sc.Scan() {
		var obj map[string]any
		if json.Unmarshal(sc.Bytes(), &obj) != nil {
			unparsed++
			continue
		}
		ts, _ := obj["timestamp"].(string)
		// The record uuid is the cheapest honest turn anchor claude publishes;
		// opaque to every consumer (TurnAnchorer capability).
		anchor, _ := obj["uuid"].(string)
		emit := func(kind, name, text string) {
			seq++
			events = append(events, CanonicalEvent{Seq: seq, Kind: kind, Ts: ts, Name: name, Text: text, TurnAnchor: anchor})
		}
		// emitClip records the pre-clip size so the console can fetch the rest.
		emitClip := func(kind, name, text string, n int) {
			t, full := clip(text, n)
			seq++
			events = append(events, CanonicalEvent{Seq: seq, Kind: kind, Ts: ts, Name: name, Text: t, FullLen: full, TurnAnchor: anchor})
		}
		switch obj["type"] {
		case "summary":
			emit("summary", "", anyString(obj["summary"]))
		case "system":
			sub, _ := obj["subtype"].(string)
			emitClip("system", sub, anyString(obj["content"]), caps.system)
		case "user", "assistant":
			msg, _ := obj["message"].(map[string]any)
			if msg == nil {
				unparsed++
				continue
			}
			if obj["type"] == "assistant" && owns(obj) {
				if call, ok := claudeUsageCall(obj, own, ""); ok {
					calls.observe(call)
				}
			}
			role := anyString(msg["role"])
			switch content := msg["content"].(type) {
			case string:
				emit(role, "", content)
			case []any:
				for _, item := range content {
					m, ok := item.(map[string]any)
					if !ok {
						continue
					}
					switch m["type"] {
					case "text":
						emit(role, "", anyString(m["text"]))
					case "thinking":
						emit("thinking", "", anyString(m["thinking"]))
					case "tool_use":
						input, _ := json.Marshal(m["input"])
						emitClip("tool_call", anyString(m["name"]), string(input), caps.tool)
					case "tool_result":
						emitClip("tool_result", "", anyString(m["content"]), caps.tool)
					}
				}
			default:
				unparsed++
			}
		case "attachment":
			// Context a hook added (hookSpecificOutput.additionalContext) is part of
			// what the model read, so it is a row. Every other attachment is the
			// vendor's own session chrome (reminders, listings, snapshots) and stays
			// counted as unparsed.
			attachment, _ := obj["attachment"].(map[string]any)
			if anyString(attachment["type"]) != "hook_additional_context" {
				unparsed++
				continue
			}
			emitClip("context", anyString(attachment["hookEvent"]), claudeAttachmentText(attachment["content"]), caps.tool)
		default:
			// progress, file-history-snapshot, queued-command, etc.
			unparsed++
		}
	}
	if sc.Err() != nil {
		unparsed++ // scanner stopped early (e.g. over-long line) — count, never hide
	}
	usage := FoldCalls(calls.admitted())
	if usage == nil {
		usage = &SessionUsage{} // callers read zero calls as "no telemetry observed"
	}
	return events, unparsed, usage, nil
}

// Claude writes placeholder assistant records such as model "<synthetic>" for
// local auth/API failures. They are transcript evidence, not runnable model IDs,
// and must not overwrite the last concrete model used by a resumable session.
func isConcreteClaudeModel(model string) bool {
	model = strings.TrimSpace(model)
	return model != "" && !(strings.HasPrefix(model, "<") && strings.HasSuffix(model, ">"))
}

// claudeAttachmentText joins a hook attachment's content, which Claude records
// as one string per hook output.
func claudeAttachmentText(content any) string {
	switch value := content.(type) {
	case string:
		return value
	case []any:
		parts := make([]string, 0, len(value))
		for _, item := range value {
			if text, ok := item.(string); ok {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// firstText pulls the first user-visible text out of a claude user line.
func firstText(obj map[string]any) string {
	msg, _ := obj["message"].(map[string]any)
	if msg == nil {
		return ""
	}
	switch content := msg["content"].(type) {
	case string:
		return content
	case []any:
		for _, item := range content {
			if m, ok := item.(map[string]any); ok && m["type"] == "text" {
				return anyString(m["text"])
			}
		}
	}
	return ""
}

func init() { register(claudeRuntime{}) }

// claudeRuntime — claude session identity. The filename stem IS the session id.
type claudeRuntime struct{}

func (claudeRuntime) Name() string        { return "claude" }
func (claudeRuntime) CLIRevision() string { return "2.1.212" }

// ActivityEvidence: SessionStart/first-action/SessionEnd hooks write the
// durable lifecycle facts, so every kind is hook-exact.
func (claudeRuntime) ActivityEvidence() map[string]string {
	return map[string]string{"session.started": "hook-exact", "session.active": "hook-exact",
		"session.ended": "hook-exact",
		// Turn boundaries: what this vendor CAN emit through its installed
		// hooks. The status decider never reads this — capability is judged
		// per session by the rows that actually arrived.
		"turn.started": "hook-exact", "turn.ended": "hook-exact", "input.requested": "hook-exact",
		// Session-scoped work facts (helper-session-attachment plan D1): every
		// one is a hook row on this runtime (UserPromptSubmit, PostToolUse,
		// Stop), measured on the reference host 2026-09-12.
		"session.turn-started": "hook-exact", "session.tool-completed": "hook-exact", "session.turn-ended": "hook-exact"}
}
func (claudeRuntime) ActivityProcess(command string) bool { return commandBase(command) == "claude" }
func (claudeRuntime) RepositoryGroupKey(_ SessionSummary, key string) string {
	for _, marker := range []string{"/.claude/worktrees", "/.claude-worktrees", "//claude/worktrees", "/-claude-worktrees"} {
		if i := strings.Index(key, marker); i > 0 {
			return key[:i]
		}
	}
	return key
}
func (claudeRuntime) CouldMatchID(id string) bool { return looksLikeUUID(id) }

// NativeOpen: the desktop app's resume route takes a CLI session uuid and
// imports the session if the desktop does not hold it (measured 2026-10-03,
// desktop 2.19675.0). A sidecar subagent is not a CLI session.
func (claudeRuntime) NativeOpen(s SessionSummary) (NativeOpenLink, bool) {
	if !looksLikeUUID(s.ID) || filepath.Base(filepath.Dir(s.Path)) == claudeSubagentsDirName {
		return NativeOpenLink{}, false
	}
	return NativeOpenLink{URL: "claude://resume?session=" + s.ID, App: "Claude"}, true
}
func (claudeRuntime) CanonicalID(s SessionSummary) string { return s.ID }
func (claudeRuntime) MatchID(s SessionSummary, id string) bool {
	return s.ID == id || s.ThreadID == id
}

func claudeRoot() string { return filepath.Join(home(), ".claude", "projects") }

func (claudeRuntime) Collect() []fileJob {
	jobs, _ := collectClaudeJobs(false)
	return jobs
}

func collectClaudeJobs(strict bool) ([]fileJob, error) {
	var jobs []fileJob
	projects, err := os.ReadDir(claudeRoot())
	if err != nil {
		if os.IsNotExist(err) {
			return jobs, nil
		}
		return nil, err
	}
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		dir := filepath.Join(claudeRoot(), p.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			if strict {
				return jobs, err
			}
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			info, err := f.Info()
			if err != nil {
				if strict {
					return jobs, err
				}
				continue
			}
			jobs = append(jobs, fileJob{
				runtime: "claude", path: filepath.Join(dir, f.Name()),
				project: p.Name(), mod: info.ModTime(), size: info.Size(),
			})
		}
	}
	return jobs, nil
}

func (claudeRuntime) Summarize(j fileJob) (SessionSummary, bool) {
	s := SessionSummary{Runtime: j.runtime, ID: stem(j.path), Project: j.project, Modified: j.mod, Path: j.path, HasTranscript: true}
	if s.Project == "" { // SummarizeFile builds a bare job
		s.Project = filepath.Base(filepath.Dir(j.path))
	}
	scan := claudeTitle(j.path)
	s.Title, s.Lines, s.Cwd, s.UserTurns = scan.Title, scan.Lines, scan.Cwd, scan.UserTurns
	// The project is the directory the transcript declares. Claude's escaped
	// folder name cannot be turned back into a path (a dash in a directory name
	// is indistinguishable from a separator), so without a cwd the raw folder
	// name stays as a label and is never used as a path.
	if s.Cwd != "" {
		s.Project = s.Cwd
	}
	s.CustomTitle, s.AITitle = scan.CustomTitle, scan.AITitle
	if scan.Usage != nil {
		s.Context, s.Model, s.Turns = scan.Usage.Context, scan.Usage.Model, scan.Usage.Turns
	}
	return s, s.Lines > 0
}

func (claudeRuntime) NormalizeFull(path string) ([]CanonicalEvent, int, *SessionUsage, error) {
	return normalizeClaude(path, deepCaps)
}

func (claudeRuntime) Normalize(path string) ([]CanonicalEvent, int, *SessionUsage, error) {
	return normalizeClaude(path, transcriptCaps)
}

func (claudeRuntime) NormalizeLifecycle(path string) (LifecycleBatch, error) {
	return normalizeClaudeLifecycle(path)
}

// ThreadTitle returns the title Claude Code itself authored for the session: the
// user's rename first, then the runtime's generated title. Returning "" leaves the
// fallback in place (the truncated first prompt), which markTitleSource then labels
// as "prompt" so the UI never passes it off as a real title.
func (claudeRuntime) ThreadTitle(s SessionSummary) string {
	if s.CustomTitle != "" {
		return truncate(s.CustomTitle, titleMaxLen)
	}
	if s.AITitle != "" {
		return truncate(s.AITitle, titleMaxLen)
	}
	return ""
}

func (runtime claudeRuntime) DiscoverTranscriptProjections(ctx context.Context,
	limits ProjectionReadLimits) (ProjectionDiscovery, error) {
	jobs, err := collectClaudeJobs(true)
	if err != nil {
		return ProjectionDiscovery{Sessions: []ProjectionSession{}, Limitations: []ProjectionLimitation{}},
			projectionError(ProjectionSourceUnreadable, runtime.Name(), "", 0,
				limits.normalized().MaxSourceBytes, err)
	}
	return discoverFileTranscriptProjections(ctx, runtime, jobs, limits)
}

func (runtime claudeRuntime) ReadTranscriptProjection(ctx context.Context, session ProjectionSession,
	limits ProjectionReadLimits) (ProjectionSnapshot, error) {
	if err := validateProjectionSession(runtime.Name(), session); err != nil {
		return ProjectionSnapshot{}, err
	}
	return readFileTranscriptProjection(ctx, runtime, session, limits,
		func(path string, reader io.Reader) ([]CanonicalEvent, int, *SessionUsage, error) {
			return normalizeClaudeReader(path, reader, transcriptCaps)
		})
}

func (runtime claudeRuntime) TranscriptProjectionGeneration(ctx context.Context,
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
