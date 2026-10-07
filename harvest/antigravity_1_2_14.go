package harvest

// Antigravity CLI 1.2.14 transcript reader. The native hook lane owns decisions;
// this reader owns only measured on-disk session discovery, text, and tool calls.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

const antigravityRuntimeName = "antigravity"
const antigravityUntitled = "Untitled Antigravity session"
const antigravityProjectionRevision = ":parser-tool-calls-v2"

type antigravityRuntime struct{}

func init() { register(antigravityRuntime{}) }

func (antigravityRuntime) Name() string                             { return antigravityRuntimeName }
func (antigravityRuntime) CLIRevision() string                      { return "1.2.14" }
func (antigravityRuntime) CanonicalID(s SessionSummary) string      { return s.ID }
func (antigravityRuntime) MatchID(s SessionSummary, id string) bool { return s.ID == id }
func (antigravityRuntime) ThreadTitle(SessionSummary) string        { return "" }

// First-action fallback and fullyIdle Stop are measured hook facts. Neither
// proves an explicit session start/end or a user-turn-start boundary.
func (antigravityRuntime) ActivityEvidence() map[string]string {
	return map[string]string{"session.started": "hook-exact", "session.tool-completed": "hook-exact",
		"turn.ended": "hook-exact", "session.turn-ended": "hook-exact"}
}

func (antigravityRuntime) ActivityProcess(command string) bool { return commandBase(command) == "agy" }

func antigravityBrainRoot() string {
	return filepath.Join(home(), ".gemini", "antigravity-cli", "brain")
}

func antigravityUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, c := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if c < '0' || c > '9' {
			if c < 'a' || c > 'f' {
				return false
			}
		}
	}
	return true
}

// antigravitySource accepts only a direct, regular transcript beneath the
// measured CLI brain layout. It never follows a symlink supplied by that tree.
func antigravitySource(path string) (string, os.FileInfo, bool) {
	root := antigravityBrainRoot()
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", nil, false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 4 || !antigravityUUID(parts[0]) || parts[1] != ".system_generated" ||
		parts[2] != "logs" || parts[3] != "transcript_full.jsonl" {
		return "", nil, false
	}
	for _, dir := range []string{root, filepath.Join(root, parts[0]),
		filepath.Join(root, parts[0], parts[1]), filepath.Join(root, parts[0], parts[1], parts[2])} {
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() {
			return "", nil, false
		}
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", nil, false
	}
	return parts[0], info, true
}

func (antigravityRuntime) Collect() []fileJob {
	root := antigravityBrainRoot()
	children, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	jobs := make([]fileJob, 0, len(children))
	for _, child := range children {
		if !child.IsDir() || !antigravityUUID(child.Name()) {
			continue
		}
		path := filepath.Join(root, child.Name(), ".system_generated", "logs", "transcript_full.jsonl")
		_, info, ok := antigravitySource(path)
		if ok {
			jobs = append(jobs, fileJob{runtime: antigravityRuntimeName, path: path,
				mod: info.ModTime(), size: info.Size()})
		}
	}
	return jobs
}

type antigravityLine struct {
	Source    string          `json:"source"`
	Type      string          `json:"type"`
	Status    string          `json:"status"`
	CreatedAt string          `json:"created_at"`
	Content   json.RawMessage `json:"content"`
	ToolCalls json.RawMessage `json:"tool_calls"`
}

func openAntigravitySource(path string) (*os.File, string, error) {
	id, before, ok := antigravitySource(path)
	if !ok {
		return nil, "", errors.New("antigravity transcript is outside the measured CLI layout")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		_ = f.Close()
		return nil, "", errors.New("antigravity transcript changed during open")
	}
	return f, id, nil
}

func antigravityUserRequest(content string) string {
	const open, close = "<USER_REQUEST>", "</USER_REQUEST>"
	start := strings.Index(content, open)
	if start < 0 {
		return ""
	}
	content = content[start+len(open):]
	end := strings.Index(content, close)
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(content[:end])
}

func antigravityText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return ""
	}
	return text
}

func (antigravityRuntime) Summarize(job fileJob) (SessionSummary, bool) {
	f, id, err := openAntigravitySource(job.path)
	if err != nil {
		return SessionSummary{}, false
	}
	defer f.Close()
	sum := SessionSummary{Runtime: antigravityRuntimeName, ID: id, Path: job.path,
		Modified: job.mod, HasTranscript: true, Title: antigravityUntitled}
	scan := newLineScanner(f)
	for scan.Scan() {
		sum.Lines++
		var row antigravityLine
		if json.Unmarshal(scan.Bytes(), &row) != nil || row.Status != "DONE" ||
			row.Source != "USER_EXPLICIT" || row.Type != "USER_INPUT" {
			continue
		}
		sum.UserTurns++
		if sum.TitleSource == "" {
			if request := antigravityUserRequest(antigravityText(row.Content)); request != "" {
				sum.Title, sum.TitleSource = truncate(request, titleMaxLen), "prompt"
			}
		}
	}
	return sum, sum.Lines > 0
}

func normalizeAntigravity(path string, caps textCaps) ([]CanonicalEvent, int, error) {
	f, _, err := openAntigravitySource(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	return normalizeAntigravityReader(f, caps)
}

func normalizeAntigravityReader(reader io.Reader, caps textCaps) ([]CanonicalEvent, int, error) {
	events := []CanonicalEvent{}
	unparsed := 0
	emit := func(kind, name, content, timestamp string, budget int) {
		text, full := clip(content, budget)
		events = append(events, CanonicalEvent{Seq: len(events) + 1, Kind: kind,
			Name: name, Ts: timestamp, Text: text, FullLen: full})
	}
	unknown := func(timestamp, description string) {
		unparsed++
		emit("other", "", description, timestamp, caps.meta)
	}
	scan := newLineScanner(reader)
	for scan.Scan() {
		var row antigravityLine
		if json.Unmarshal(scan.Bytes(), &row) != nil {
			unknown("", "Unparsed Antigravity record")
			continue
		}
		if row.Status == "DONE" && row.Source == "MODEL" && row.Type == "PLANNER_RESPONSE" &&
			len(row.ToolCalls) > 0 {
			emitted := false
			if response := strings.TrimSpace(antigravityText(row.Content)); response != "" {
				emit("assistant", "", response, row.CreatedAt, caps.tool)
				emitted = true
			}
			var calls []json.RawMessage
			if err := json.Unmarshal(row.ToolCalls, &calls); err != nil {
				unknown(row.CreatedAt, "Unparsed Antigravity tool calls")
				continue
			}
			for _, raw := range calls {
				var call struct {
					Name string          `json:"name"`
					Args json.RawMessage `json:"args"`
				}
				callErr := json.Unmarshal(raw, &call)
				args := bytes.TrimSpace(call.Args)
				if callErr != nil || !antigravityToolName(call.Name) ||
					len(args) == 0 || args[0] != '{' {
					unknown(row.CreatedAt, "Unparsed Antigravity tool call")
					continue
				}
				emit("tool_call", call.Name, string(call.Args), row.CreatedAt, caps.tool)
				emitted = true
			}
			if !emitted && len(calls) == 0 {
				unknown(row.CreatedAt, "Unsupported Antigravity record")
			}
			continue
		}
		kind, content := "other", "Unsupported Antigravity record"
		switch {
		case row.Status == "DONE" && row.Source == "USER_EXPLICIT" && row.Type == "USER_INPUT":
			if request := antigravityUserRequest(antigravityText(row.Content)); request != "" {
				kind, content = "user", request
			}
		case row.Status == "DONE" && row.Source == "MODEL" && row.Type == "PLANNER_RESPONSE":
			if response := strings.TrimSpace(antigravityText(row.Content)); response != "" {
				kind, content = "assistant", response
			}
		}
		if kind == "other" {
			unknown(row.CreatedAt, content)
			continue
		}
		budget := caps.meta
		if kind == "user" || kind == "assistant" {
			budget = caps.tool
		}
		emit(kind, "", content, row.CreatedAt, budget)
	}
	if err := scan.Err(); err != nil {
		return events, unparsed, err
	}
	return events, unparsed, nil
}

func antigravityToolName(name string) bool {
	if name == "" || len(name) > 128 || strings.TrimSpace(name) != name {
		return false
	}
	return !strings.ContainsFunc(name, unicode.IsControl)
}

func (antigravityRuntime) Normalize(path string) ([]CanonicalEvent, int, *SessionUsage, error) {
	events, unparsed, err := normalizeAntigravity(path, transcriptCaps)
	return events, unparsed, nil, err
}

var _ Runtime = antigravityRuntime{}
var _ TranscriptProjectionSource = antigravityRuntime{}

func (runtime antigravityRuntime) DiscoverTranscriptProjections(ctx context.Context,
	limits ProjectionReadLimits) (ProjectionDiscovery, error) {
	root := antigravityBrainRoot()
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return ProjectionDiscovery{Sessions: []ProjectionSession{}, Complete: true,
			Limitations: []ProjectionLimitation{}}, nil
	}
	if err != nil || !info.IsDir() {
		return ProjectionDiscovery{Sessions: []ProjectionSession{}, Complete: false,
			Limitations: []ProjectionLimitation{{Kind: ProjectionSourceUnreadable,
				Runtime: runtime.Name()}}}, nil
	}
	children, err := os.ReadDir(root)
	if err != nil {
		return ProjectionDiscovery{Sessions: []ProjectionSession{}, Complete: false,
			Limitations: []ProjectionLimitation{{Kind: ProjectionSourceUnreadable,
				Runtime: runtime.Name()}}}, nil
	}
	unsafe := false
	for _, child := range children {
		if err := ctx.Err(); err != nil {
			return ProjectionDiscovery{}, projectionError(ProjectionReadCancelled, runtime.Name(), "",
				0, limits.normalized().MaxSourceBytes, err)
		}
		if !antigravityUUID(child.Name()) {
			continue
		}
		path := filepath.Join(root, child.Name())
		childInfo, childErr := os.Lstat(path)
		if childErr != nil || !childInfo.IsDir() {
			unsafe = true
			continue
		}
		for _, part := range []string{".system_generated", "logs", "transcript_full.jsonl"} {
			path = filepath.Join(path, part)
			item, statErr := os.Lstat(path)
			if errors.Is(statErr, os.ErrNotExist) {
				break
			}
			if statErr != nil || (!item.IsDir() && part != "transcript_full.jsonl") ||
				(part == "transcript_full.jsonl" && !item.Mode().IsRegular()) {
				unsafe = true
				break
			}
		}
	}
	out, err := discoverFileTranscriptProjections(ctx, runtime, runtime.Collect(), limits)
	for index := range out.Sessions {
		for segment := range out.Sessions[index].Segments {
			out.Sessions[index].Segments[segment].UpdateMarker += antigravityProjectionRevision
		}
		out.Sessions[index].Generation = projectionGeneration(out.Sessions[index])
	}
	if unsafe {
		out.Complete = false
		out.Limitations = append(out.Limitations, ProjectionLimitation{
			Kind: ProjectionSourceUnreadable, Runtime: runtime.Name()})
	}
	return out, err
}

func antigravityProjectionSource(session ProjectionSession) (string, error) {
	if err := validateProjectionSession(antigravityRuntimeName, session); err != nil {
		return "", err
	}
	if !antigravityUUID(session.ID) || len(session.Segments) != 1 ||
		session.Segments[0].ID != session.ID || session.Summary.ID != session.ID {
		return "", errors.New("invalid antigravity projection identity")
	}
	path := filepath.Join(antigravityBrainRoot(), session.ID, ".system_generated", "logs", "transcript_full.jsonl")
	if session.Segments[0].SourceRef != path {
		return "", errors.New("invalid antigravity projection source")
	}
	return path, nil
}

func (runtime antigravityRuntime) refreshProjection(ctx context.Context, session ProjectionSession,
	limits ProjectionReadLimits) (ProjectionSession, error) {
	limits = limits.normalized()
	if err := ctx.Err(); err != nil {
		return ProjectionSession{}, projectionError(ProjectionReadCancelled, runtime.Name(), session.ID,
			0, limits.MaxSourceBytes, err)
	}
	path, err := antigravityProjectionSource(session)
	if err != nil {
		return ProjectionSession{}, err
	}
	f, id, err := openAntigravitySource(path)
	if err != nil {
		return ProjectionSession{}, projectionError(ProjectionSourceUnreadable, runtime.Name(), session.ID,
			0, limits.MaxSourceBytes, err)
	}
	info, statErr := f.Stat()
	closeErr := f.Close()
	if statErr != nil || closeErr != nil || id != session.ID {
		return ProjectionSession{}, projectionError(ProjectionSourceUnreadable, runtime.Name(), session.ID,
			0, limits.MaxSourceBytes, errors.Join(statErr, closeErr))
	}
	if info.Size() > limits.MaxSourceBytes {
		return ProjectionSession{}, projectionError(ProjectionSourceTooLarge, runtime.Name(), session.ID,
			info.Size(), limits.MaxSourceBytes, nil)
	}
	current := session
	current.Segments = append([]ProjectionSegment(nil), session.Segments...)
	current.Segments[0].Modified = info.ModTime()
	current.Segments[0].SourceBytes = info.Size()
	current.Segments[0].UpdateMarker = fileProjectionMarker(info.ModTime(), info.Size()) +
		antigravityProjectionRevision
	current.Modified, current.SourceBytes = info.ModTime(), info.Size()
	current.Generation = projectionGeneration(current)
	return current, nil
}

func (runtime antigravityRuntime) TranscriptProjectionGeneration(ctx context.Context,
	session ProjectionSession, limits ProjectionReadLimits) (string, error) {
	current, err := runtime.refreshProjection(ctx, session, limits)
	if err != nil {
		return "", err
	}
	return current.Generation, nil
}

func (runtime antigravityRuntime) ReadTranscriptProjection(ctx context.Context,
	session ProjectionSession, limits ProjectionReadLimits) (ProjectionSnapshot, error) {
	current, err := runtime.refreshProjection(ctx, session, limits)
	if err != nil {
		return ProjectionSnapshot{}, err
	}
	limits = limits.normalized()
	if current.Generation != session.Generation {
		return ProjectionSnapshot{}, projectionError(ProjectionSourceMutated, runtime.Name(), session.ID,
			current.SourceBytes, limits.MaxSourceBytes, nil)
	}
	f, _, err := openAntigravitySource(session.Segments[0].SourceRef)
	if err != nil {
		return ProjectionSnapshot{}, projectionError(ProjectionSourceUnreadable, runtime.Name(), session.ID,
			current.SourceBytes, limits.MaxSourceBytes, err)
	}
	reader := &contextReader{ctx: ctx, r: io.NewSectionReader(f, 0, current.SourceBytes)}
	events, unparsed, readErr := normalizeAntigravityReader(reader, transcriptCaps)
	opened, statErr := f.Stat()
	closeErr := f.Close()
	if readErr == nil {
		readErr = errors.Join(reader.err, statErr, closeErr)
	}
	if readErr != nil {
		kind := ProjectionSourceUnreadable
		if errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) {
			kind = ProjectionReadCancelled
		}
		return ProjectionSnapshot{}, projectionError(kind, runtime.Name(), session.ID,
			current.SourceBytes, limits.MaxSourceBytes, readErr)
	}
	if opened.Size() != current.SourceBytes ||
		fileProjectionMarker(opened.ModTime(), opened.Size())+antigravityProjectionRevision !=
			current.Segments[0].UpdateMarker {
		return ProjectionSnapshot{}, projectionError(ProjectionSourceMutated, runtime.Name(), session.ID,
			opened.Size(), limits.MaxSourceBytes, nil)
	}
	after, err := runtime.refreshProjection(ctx, session, limits)
	if err != nil {
		return ProjectionSnapshot{}, err
	}
	if after.Generation != current.Generation {
		return ProjectionSnapshot{}, projectionError(ProjectionSourceMutated, runtime.Name(), session.ID,
			after.SourceBytes, limits.MaxSourceBytes, nil)
	}
	snapshot := ProjectionSnapshot{Session: session, Generation: current.Generation,
		SourceBytes: current.SourceBytes, Unparsed: unparsed, Events: make([]ProjectionEvent, 0, len(events))}
	for _, event := range events {
		snapshot.Events = append(snapshot.Events, ProjectionEvent{Ordinal: len(snapshot.Events),
			Lineage:   projectionEventLineage(runtime.Name(), session.ID, session.ID, event.Seq),
			SegmentID: session.ID, Event: event})
	}
	return snapshot, nil
}
