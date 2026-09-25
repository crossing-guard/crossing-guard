package harvest

// OpenCode adapter — interface verified against OpenCode CLI 1.18.0
// (passive SQLite store read; the live plugin lane is owned by
// internal/guardcli/install_opencode.go).
// The filename carries the newest verified interface; on
// re-verification against a newer CLI this file is renamed in place.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/ncruces/go-sqlite3/driver"
)

const opencodeRuntimeName = "opencode"

type opencodeRuntime struct{}

func init() { register(opencodeRuntime{}) }

func (opencodeRuntime) Name() string        { return opencodeRuntimeName }
func (opencodeRuntime) CLIRevision() string { return "1.18.0" }

// ActivityEvidence: OpenCode has no lifecycle hooks — start/active facts are
// presence-derived, and there is NO end fact in v1 (red-team B3): its absence
// here is the honest statement, never a guessed closure.
func (opencodeRuntime) ActivityEvidence() map[string]string {
	// Session-scoped work facts (helper-session-attachment plan D1): the
	// plugin's tool.execute.after writes live post-tool rows; session.idle is
	// installed probe-gated, so turn-ended is published as such; there is no
	// turn-started hook.
	return map[string]string{"session.started": "presence-derived", "session.active": "presence-derived",
		"session.tool-completed": "hook-exact", "session.turn-ended": "probe-gated"}
}
func (opencodeRuntime) CanonicalID(s SessionSummary) string      { return s.ID }
func (opencodeRuntime) MatchID(s SessionSummary, id string) bool { return s.ID == id }
func (opencodeRuntime) CouldMatchID(id string) bool              { return strings.HasPrefix(id, "ses_") }
func (opencodeRuntime) Collect() []fileJob                       { return nil }
func (opencodeRuntime) Summarize(fileJob) (SessionSummary, *SessionUsage, map[string]*DayBucket, bool) {
	return SessionSummary{}, nil, nil, false
}
func (opencodeRuntime) Normalize(string) ([]CanonicalEvent, int, *SessionUsage, error) {
	return nil, 0, nil, fmt.Errorf("OpenCode sessions require a logical session reference")
}
func (opencodeRuntime) ThreadTitle(SessionSummary) string { return "" }

func opencodeDataDir() string {
	if value := strings.TrimSpace(os.Getenv("OPENCODE_DATA_HOME")); value != "" {
		return value
	}
	if path, err := exec.LookPath("opencode"); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if output, err := exec.CommandContext(ctx, path, "debug", "paths").Output(); err == nil {
			for _, line := range strings.Split(string(output), "\n") {
				fields := strings.Fields(line)
				if len(fields) >= 2 && fields[0] == "data" {
					return strings.TrimSpace(strings.TrimPrefix(line, "data"))
				}
			}
		}
	}
	if value := os.Getenv("XDG_DATA_HOME"); value != "" {
		return filepath.Join(value, "opencode")
	}
	if runtime.GOOS == "windows" {
		if value := os.Getenv("LOCALAPPDATA"); value != "" {
			return filepath.Join(value, "opencode")
		}
	}
	if h := home(); h != "" {
		return filepath.Join(h, ".local", "share", "opencode")
	}
	return ""
}

func opencodeDBPath() string {
	if dir := opencodeDataDir(); dir != "" {
		return filepath.Join(dir, "opencode.db")
	}
	return ""
}

type opencodeModel struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
}

func parseOpenCodeModel(raw string) opencodeModel {
	var model opencodeModel
	_ = json.Unmarshal([]byte(raw), &model)
	return model
}

func unixMilli(value int64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(value)
}

func openCodeMarker(updated int64, version, model string, parts int) string {
	return strconv.FormatInt(updated, 10) + ":" + strconv.Itoa(parts) + ":" + version + ":" + model
}

type openCodeProjectionRecord struct {
	Record           SessionRecord
	SourceBytes      int64
	GenerationMarker string
}

func (opencodeRuntime) ListSessionRecords() ([]SessionRecord, error) {
	projectionRecords, err := listOpenCodeProjectionRecords(context.Background(), opencodeDBPath(), "", false)
	if err != nil {
		return nil, err
	}
	records := make([]SessionRecord, 0, len(projectionRecords))
	for _, record := range projectionRecords {
		records = append(records, record.Record)
	}
	return records, nil
}

func listOpenCodeProjectionRecords(ctx context.Context, path, sessionID string,
	includeSourceBytes bool) ([]openCodeProjectionRecord, error) {
	if path == "" {
		return []openCodeProjectionRecord{}, nil
	}
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		if err == nil || os.IsNotExist(err) {
			return []openCodeProjectionRecord{}, nil
		}
		return nil, err
	}
	db, err := driver.Open("file:" + path + "?mode=ro&_pragma=busy_timeout(100)&_pragma=query_only(1)")
	if err != nil {
		return nil, fmt.Errorf("open OpenCode database read-only: %w", err)
	}
	defer db.Close()
	query := `SELECT s.id,COALESCE(s.parent_id,''),s.directory,s.title,s.version,COALESCE(s.model,''),
		s.tokens_input,s.tokens_output,s.tokens_reasoning,s.tokens_cache_read,s.tokens_cache_write,
		s.time_created,s.time_updated,
		(SELECT count(*) FROM message m WHERE m.session_id=s.id AND json_extract(m.data,'$.role')='assistant'),
		(SELECT count(*) FROM message m WHERE m.session_id=s.id AND json_extract(m.data,'$.role')='user'),
		(SELECT count(*) FROM part p WHERE p.session_id=s.id),`
	if includeSourceBytes {
		query += `COALESCE((SELECT sum(length(m.data)+length(p.data)) FROM message m
			JOIN part p ON p.message_id=m.id WHERE p.session_id=s.id),0),
			COALESCE((SELECT max(p.time_updated) FROM part p WHERE p.session_id=s.id),0)`
	} else {
		query += `0,0`
	}
	query += ` FROM session s`
	args := []any{}
	if sessionID != "" {
		query += ` WHERE s.id=?`
		args = append(args, sessionID)
	}
	query += ` ORDER BY s.time_updated DESC,s.id`
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query OpenCode sessions: %w", err)
	}
	defer rows.Close()
	records := []openCodeProjectionRecord{}
	for rows.Next() {
		var id, parent, directory, title, version, modelRaw string
		var input, output, reasoning, cacheRead, cacheWrite, created, updated, sourceBytes, partUpdated int64
		var turns, userTurns, parts int
		if err := rows.Scan(&id, &parent, &directory, &title, &version, &modelRaw,
			&input, &output, &reasoning, &cacheRead, &cacheWrite, &created, &updated,
			&turns, &userTurns, &parts, &sourceBytes, &partUpdated); err != nil {
			return nil, err
		}
		model := parseOpenCodeModel(modelRaw)
		usage := &SessionUsage{Model: model.ID, Turns: turns, InputTokens: input,
			OutputTokens: output, CacheRead: cacheRead, CacheCreate: cacheWrite}
		finishUsage(usage)
		ref := SessionRef{Runtime: opencodeRuntimeName, ID: id, Source: path, Segment: id,
			UpdateMarker: openCodeMarker(updated, version, modelRaw, parts)}
		sum := SessionSummary{Runtime: opencodeRuntimeName, ID: id, ParentID: parent,
			Project: filepath.Base(directory), Title: truncate(title, titleMaxLen), TitleSource: "vendor",
			Modified: unixMilli(updated), Lines: parts, Path: path, SourceRef: path,
			SourceSegment: id, UpdateMarker: ref.UpdateMarker, Cwd: directory, Model: model.ID,
			Provider: model.ProviderID, ActivityStatus: "unknown", Turns: turns, UserTurns: userTurns, HasTranscript: parts > 0}
		records = append(records, openCodeProjectionRecord{Record: SessionRecord{
			Ref: ref, Summary: sum, Usage: usage}, SourceBytes: sourceBytes,
			GenerationMarker: ref.UpdateMarker + ":" + strconv.FormatInt(partUpdated, 10) +
				":" + strconv.FormatInt(sourceBytes, 10)})
		_ = created
		_ = reasoning
	}
	return records, rows.Err()
}

type openCodeMessage struct {
	Role string `json:"role"`
	Time struct {
		Created int64 `json:"created"`
	} `json:"time"`
}

type openCodePart struct {
	Type   string          `json:"type"`
	Text   string          `json:"text"`
	Tool   string          `json:"tool"`
	CallID string          `json:"callID"`
	State  json.RawMessage `json:"state"`
}

type openCodeToolState struct {
	Status   string          `json:"status"`
	Input    json.RawMessage `json:"input"`
	Output   string          `json:"output"`
	Error    string          `json:"error"`
	Metadata json.RawMessage `json:"metadata"`
	Time     struct {
		Start int64 `json:"start"`
		End   int64 `json:"end"`
	} `json:"time"`
}

type openCodeLifecycleCursor struct {
	Time  int64  `json:"time"`
	ID    string `json:"id"`
	Count int64  `json:"count"`
}

func (opencodeRuntime) LifecycleActionCollectionContracts() []LifecycleActionCollectionContract {
	return []LifecycleActionCollectionContract{{Runtime: opencodeRuntimeName, ParserVersion: 1,
		ResultSourceKind: "vendor-transcript", ResultNativeCallKind: "call_id"}}
}

func openCodeEffects(tool string, input json.RawMessage, retain bool) []LifecycleEffect {
	var args map[string]any
	if json.Unmarshal(input, &args) != nil {
		return nil
	}
	path, _ := args["filePath"].(string)
	if path == "" {
		path, _ = args["file_path"].(string)
	}
	if path == "" {
		if strings.EqualFold(tool, "apply_patch") {
			patch, _ := args["patchText"].(string)
			if patch == "" {
				patch, _ = args["command"].(string)
			}
			return openCodePatchEffects(patch)
		}
		return nil
	}
	effect := LifecycleEffect{Ordinal: 0, RawIdentity: path, EvidenceSource: "vendor-tool-input",
		SourceField: "state.input.filePath", Completeness: "metadata-only", DiffCompleteness: "unavailable"}
	switch strings.ToLower(tool) {
	case "edit":
		effect.Operation = "update"
		before, _ := args["oldString"].(string)
		after, _ := args["newString"].(string)
		effect.BeforeBytes, effect.AfterBytes = len(before), len(after)
		effect.BeforeDigest, effect.AfterDigest = lifecycleDigest([]byte(before)), lifecycleDigest([]byte(after))
		if retain {
			effect.ReplacementBefore, effect.ReplacementAfter = []byte(before), []byte(after)
			effect.Completeness = "partial"
		}
	case "write":
		effect.Operation = "write"
		content, _ := args["content"].(string)
		effect.ContentBytes, effect.ContentDigest = len(content), lifecycleDigest([]byte(content))
		if retain {
			effect.ContentPayload, effect.Completeness = []byte(content), "complete"
		}
	default:
		return nil
	}
	return []LifecycleEffect{effect}
}

func openCodePatchEffects(patch string) []LifecycleEffect {
	effects := []LifecycleEffect{}
	for _, line := range strings.Split(patch, "\n") {
		if strings.HasPrefix(line, "*** Move to: ") {
			if len(effects) > 0 && effects[len(effects)-1].Operation == "update" {
				effects[len(effects)-1].Operation = "move"
				effects[len(effects)-1].MoveTarget = strings.TrimSpace(strings.TrimPrefix(line, "*** Move to: "))
			}
			continue
		}
		var path, operation string
		for _, header := range []struct{ prefix, operation string }{
			{"*** Add File: ", "write"}, {"*** Update File: ", "update"},
			{"*** Delete File: ", "delete"},
		} {
			if strings.HasPrefix(line, header.prefix) {
				path, operation = strings.TrimSpace(strings.TrimPrefix(line, header.prefix)), header.operation
				break
			}
		}
		if path == "" {
			continue
		}
		effects = append(effects, LifecycleEffect{Ordinal: len(effects), RawIdentity: path,
			Operation: operation, EvidenceSource: "vendor-tool-input",
			SourceField: "state.input.patchText", Completeness: "partial", DiffCompleteness: "unavailable"})
	}
	return effects
}

func (opencodeRuntime) NormalizeLogicalLifecycle(request LogicalLifecycleReadRequest) (LogicalLifecyclePage, error) {
	maxRecords := request.MaxRecords
	if maxRecords <= 0 || maxRecords > DefaultLifecycleReadRecords {
		maxRecords = DefaultLifecycleReadRecords
	}
	var cursor openCodeLifecycleCursor
	if len(request.Cursor) > 0 {
		if len(request.Cursor) > MaxLifecycleStateBytes || json.Unmarshal(request.Cursor, &cursor) != nil {
			return LogicalLifecyclePage{}, fmt.Errorf("invalid OpenCode lifecycle cursor")
		}
	}
	db, err := driver.Open("file:" + request.Source + "?mode=ro&_pragma=busy_timeout(100)&_pragma=query_only(1)")
	if err != nil {
		return LogicalLifecyclePage{}, err
	}
	defer db.Close()
	var updated int64
	var partCount int
	if err := db.QueryRow(`SELECT time_updated,(SELECT count(*) FROM part WHERE session_id=?)
		FROM session WHERE id=?`, request.Segment, request.Segment).Scan(&updated, &partCount); err != nil {
		return LogicalLifecyclePage{}, err
	}
	var schemaVersion int
	_ = db.QueryRow(`PRAGMA user_version`).Scan(&schemaVersion)
	generation := "opencode-sqlite-v1:" + strconv.Itoa(schemaVersion)
	updateMarker := strconv.FormatInt(updated, 10) + ":" + strconv.Itoa(partCount)
	rows, err := db.Query(`SELECT id,time_created,time_updated,data FROM part WHERE session_id=?
		AND json_extract(data,'$.type')='tool'
		AND json_extract(data,'$.state.status') IN ('completed','error')
		AND (time_updated>? OR (time_updated=? AND id>?))
		ORDER BY time_updated,id LIMIT ?`, request.Segment, cursor.Time, cursor.Time, cursor.ID, maxRecords+1)
	if err != nil {
		return LogicalLifecyclePage{}, err
	}
	defer rows.Close()
	batch := LifecycleBatch{Runtime: opencodeRuntimeName, CanonicalSessionID: request.SessionID,
		SourceSegmentID: request.Segment, SourcePath: request.Source}
	processed := 0
	bytesInspected := int64(0)
	for rows.Next() {
		var id, raw string
		var created, changed int64
		if err := rows.Scan(&id, &created, &changed, &raw); err != nil {
			return LogicalLifecyclePage{}, err
		}
		bytesInspected += int64(len(raw))
		if processed >= maxRecords {
			batch.Continuation = true
			break
		}
		var part openCodePart
		if json.Unmarshal([]byte(raw), &part) != nil {
			batch.MalformedRecords++
			cursor = openCodeLifecycleCursor{Time: changed, ID: id, Count: cursor.Count + 1}
			processed++
			continue
		}
		var state openCodeToolState
		if json.Unmarshal(part.State, &state) != nil {
			batch.MalformedRecords++
			cursor = openCodeLifecycleCursor{Time: changed, ID: id, Count: cursor.Count + 1}
			processed++
			continue
		}
		sequence := strconv.FormatInt(created, 10) + ":" + id
		batch.Actions = append(batch.Actions, newTranscriptAction(opencodeRuntimeName,
			request.SessionID, request.SessionID, request.Segment, request.Source, sequence+":action",
			part.CallID, "call_id", part.Tool, opencodeTimestamp(state.Time.Start),
			json.RawMessage(state.Input), []byte(raw)))
		if state.Status == "completed" || state.Status == "error" {
			stateName, errorClass := "success", ""
			result := state.Output
			if state.Status == "error" {
				stateName, errorClass, result = "failure", "runtime-reported", state.Error
			}
			rawResult, decoded := resultPayloadMeta(result)
			item := newTranscriptResult(opencodeRuntimeName, request.SessionID, request.SessionID,
				request.Segment, "vendor-transcript", request.Source, sequence+":result", part.CallID,
				"call_id", part.Tool, stateName, errorClass, opencodeTimestamp(state.Time.End),
				[]byte(raw), rawResult, decoded)
			item.Effects = openCodeEffects(part.Tool, state.Input, request.RetainEffectBodies)
			batch.Results = append(batch.Results, item)
		}
		cursor = openCodeLifecycleCursor{Time: changed, ID: id, Count: cursor.Count + 1}
		processed++
	}
	batch.RecordsDecoded, batch.NextSourceLine, batch.NextOffset = processed, cursor.Count, cursor.Count
	batch.BytesInspected = bytesInspected
	cursorBody, _ := json.Marshal(cursor)
	batch.ParserState = cursorBody
	batch.PeakRetainedBytes = int64(len(cursorBody))
	return LogicalLifecyclePage{Batch: batch, Generation: generation, UpdateMarker: updateMarker, Cursor: cursorBody,
		Continuation: batch.Continuation}, rows.Err()
}

func opencodeTimestamp(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339Nano)
}

func clipOpenCode(text string, cap int) (string, int) { return clip(text, cap) }

func (opencodeRuntime) NormalizeSession(ref SessionRef, full bool) ([]CanonicalEvent, int, *SessionUsage, error) {
	events, unparsed, usage, _, err := normalizeOpenCodeSession(context.Background(), ref, full, 0)
	return events, unparsed, usage, err
}

func normalizeOpenCodeSession(ctx context.Context, ref SessionRef, full bool,
	maxSourceBytes int64) ([]CanonicalEvent, int, *SessionUsage, int64, error) {
	db, err := driver.Open("file:" + ref.Source + "?mode=ro&_pragma=busy_timeout(100)&_pragma=query_only(1)")
	if err != nil {
		return nil, 0, nil, 0, err
	}
	defer db.Close()
	var modelRaw string
	var input, output, cacheRead, cacheWrite int64
	var turns int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(model,''),tokens_input,tokens_output,tokens_cache_read,tokens_cache_write,
		(SELECT count(*) FROM message WHERE session_id=? AND json_extract(data,'$.role')='assistant')
		FROM session WHERE id=?`, ref.Segment, ref.Segment).Scan(&modelRaw, &input, &output, &cacheRead, &cacheWrite, &turns); err != nil {
		return nil, 0, nil, 0, err
	}
	var sourceBytes int64
	if maxSourceBytes > 0 {
		if err := db.QueryRowContext(ctx, `SELECT COALESCE(sum(length(m.data)+length(p.data)),0)
			FROM message m JOIN part p ON p.message_id=m.id WHERE p.session_id=?`, ref.Segment).
			Scan(&sourceBytes); err != nil {
			return nil, 0, nil, 0, err
		}
		if sourceBytes > maxSourceBytes {
			return nil, 0, nil, sourceBytes, projectionError(ProjectionSourceTooLarge,
				opencodeRuntimeName, ref.ID, sourceBytes, maxSourceBytes, nil)
		}
	}
	model := parseOpenCodeModel(modelRaw)
	usage := &SessionUsage{Model: model.ID, Turns: turns, InputTokens: input, OutputTokens: output,
		CacheRead: cacheRead, CacheCreate: cacheWrite}
	finishUsage(usage)
	rows, err := db.QueryContext(ctx, `SELECT m.data,p.data,p.time_created,p.id FROM message m
		JOIN part p ON p.message_id=m.id WHERE m.session_id=? ORDER BY p.time_created,p.id`, ref.Segment)
	if err != nil {
		return nil, 0, nil, sourceBytes, err
	}
	defer rows.Close()
	events := []CanonicalEvent{}
	unparsed, seq := 0, 0
	toolCap, metaCap, thinkingCap := transcriptCaps.tool, transcriptCaps.meta, transcriptCaps.thinking
	if full {
		toolCap, metaCap, thinkingCap = deepCaps.tool, deepCaps.meta, deepCaps.thinking
	}
	add := func(kind, ts, name, text string, cap int) {
		clipped, fullLen := clipOpenCode(text, cap)
		events = append(events, CanonicalEvent{Seq: seq, Kind: kind, Ts: ts, Name: name, Text: clipped, FullLen: fullLen})
		seq++
	}
	for rows.Next() {
		var messageRaw, partRaw string
		var created int64
		var partID string
		if err := rows.Scan(&messageRaw, &partRaw, &created, &partID); err != nil {
			return nil, unparsed, usage, sourceBytes, err
		}
		var message openCodeMessage
		var part openCodePart
		if json.Unmarshal([]byte(messageRaw), &message) != nil || json.Unmarshal([]byte(partRaw), &part) != nil {
			unparsed++
			continue
		}
		ts := opencodeTimestamp(created)
		switch part.Type {
		case "text":
			kind := message.Role
			if kind != "user" && kind != "assistant" {
				kind = "other"
			}
			add(kind, ts, "", part.Text, metaCap)
		case "reasoning":
			add("thinking", ts, "", part.Text, thinkingCap)
		case "tool":
			var state openCodeToolState
			_ = json.Unmarshal(part.State, &state)
			add("tool_call", opencodeTimestamp(state.Time.Start), part.Tool, string(state.Input), toolCap)
			result := state.Output
			if state.Error != "" {
				result = state.Error
			}
			if result == "" && len(state.Metadata) > 0 && string(state.Metadata) != "null" {
				result = string(state.Metadata)
			}
			add("tool_result", opencodeTimestamp(state.Time.End), part.Tool, result, toolCap)
		case "step-start", "step-finish":
			add("other", ts, part.Type, partID, metaCap)
		default:
			unparsed++
			add("other", ts, part.Type, partRaw, metaCap)
		}
	}
	return events, unparsed, usage, sourceBytes, rows.Err()
}

func openCodeProjectionSession(runtime opencodeRuntime, source openCodeProjectionRecord) ProjectionSession {
	record := source.Record
	summary := cacheSessionRecord(record)
	decorateSummary(&summary)
	segment := ProjectionSegment{ID: record.Ref.Segment, SourceRef: record.Ref.Source,
		UpdateMarker: source.GenerationMarker, Modified: summary.Modified,
		SourceBytes: source.SourceBytes, Summary: summary}
	return aggregateProjectionSession(runtime, runtime.CanonicalID(summary), []ProjectionSegment{segment})
}

func (runtime opencodeRuntime) DiscoverTranscriptProjections(ctx context.Context,
	limits ProjectionReadLimits) (ProjectionDiscovery, error) {
	limits = limits.normalized()
	records, err := listOpenCodeProjectionRecords(ctx, opencodeDBPath(), "", true)
	if err != nil {
		kind := ProjectionSourceUnreadable
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			kind = ProjectionReadCancelled
		}
		return ProjectionDiscovery{Sessions: []ProjectionSession{}, Limitations: []ProjectionLimitation{}},
			projectionError(kind, runtime.Name(), "", 0, limits.MaxSourceBytes, err)
	}
	out := ProjectionDiscovery{Sessions: make([]ProjectionSession, 0, len(records)),
		Limitations: []ProjectionLimitation{}, Complete: true}
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return out, projectionError(ProjectionReadCancelled, runtime.Name(), "", 0,
				limits.MaxSourceBytes, err)
		}
		session := openCodeProjectionSession(runtime, record)
		out.Sessions = append(out.Sessions, session)
		if session.SourceBytes > limits.MaxSourceBytes {
			out.Complete = false
			out.Limitations = append(out.Limitations, ProjectionLimitation{Kind: ProjectionSourceTooLarge,
				Runtime: runtime.Name(), SessionID: session.ID, ObservedBytes: session.SourceBytes,
				LimitBytes: limits.MaxSourceBytes})
		}
	}
	return out, nil
}

func (runtime opencodeRuntime) ReadTranscriptProjection(ctx context.Context, session ProjectionSession,
	limits ProjectionReadLimits) (ProjectionSnapshot, error) {
	if err := validateProjectionSession(runtime.Name(), session); err != nil {
		return ProjectionSnapshot{}, err
	}
	limits = limits.normalized()
	before, err := runtime.TranscriptProjectionGeneration(ctx, session, limits)
	if err != nil {
		return ProjectionSnapshot{}, err
	}
	if before != session.Generation {
		return ProjectionSnapshot{}, projectionError(ProjectionSourceMutated, session.Runtime,
			session.ID, session.SourceBytes, limits.MaxSourceBytes, nil)
	}
	segment := session.Segments[0]
	ref := SessionRef{Runtime: runtime.Name(), ID: session.ID, Source: segment.SourceRef,
		Segment: segment.ID, UpdateMarker: segment.UpdateMarker}
	events, unparsed, usage, sourceBytes, err := normalizeOpenCodeSession(ctx, ref, false,
		limits.MaxSourceBytes)
	if err != nil {
		if _, ok := projectionLimitation(err); ok {
			return ProjectionSnapshot{}, err
		}
		kind := ProjectionSourceUnreadable
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			kind = ProjectionReadCancelled
		}
		return ProjectionSnapshot{}, projectionError(kind, session.Runtime, session.ID,
			sourceBytes, limits.MaxSourceBytes, err)
	}
	snapshot := ProjectionSnapshot{Session: session, Events: make([]ProjectionEvent, 0, len(events)),
		Generation: before, SourceBytes: sourceBytes, Unparsed: unparsed, Usage: usage}
	for _, event := range events {
		snapshot.Events = append(snapshot.Events, ProjectionEvent{Ordinal: len(snapshot.Events),
			Lineage:   projectionEventLineage(session.Runtime, session.ID, segment.ID, event.Seq),
			SegmentID: segment.ID, Event: event})
	}
	after, err := runtime.TranscriptProjectionGeneration(ctx, session, limits)
	if err != nil {
		return ProjectionSnapshot{}, err
	}
	if after != snapshot.Generation {
		return ProjectionSnapshot{}, projectionError(ProjectionSourceMutated, session.Runtime,
			session.ID, sourceBytes, limits.MaxSourceBytes, nil)
	}
	return snapshot, nil
}

func (runtime opencodeRuntime) TranscriptProjectionGeneration(ctx context.Context,
	session ProjectionSession, limits ProjectionReadLimits) (string, error) {
	if err := validateProjectionSession(runtime.Name(), session); err != nil {
		return "", err
	}
	limits = limits.normalized()
	records, err := listOpenCodeProjectionRecords(ctx, session.Segments[0].SourceRef, session.ID, true)
	if err != nil {
		kind := ProjectionSourceUnreadable
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			kind = ProjectionReadCancelled
		}
		return "", projectionError(kind, session.Runtime, session.ID, 0, limits.MaxSourceBytes, err)
	}
	if len(records) != 1 {
		return "", projectionError(ProjectionSourceMutated, session.Runtime, session.ID, 0,
			limits.MaxSourceBytes, nil)
	}
	current := openCodeProjectionSession(runtime, records[0])
	if current.SourceBytes > limits.MaxSourceBytes {
		return "", projectionError(ProjectionSourceTooLarge, session.Runtime, session.ID,
			current.SourceBytes, limits.MaxSourceBytes, nil)
	}
	return current.Generation, nil
}
