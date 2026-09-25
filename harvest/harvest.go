// Package harvest reads vendor session sources at rest (file-backed or logical)
// and maps them to canonical summaries and events. READ-ONLY over vendor-owned
// storage. This is the one parser set (ADR 0018 / code-organization-v1 M2):
// extracted from the consoleprobe implementation, which superseded the
// earlier cpmem copy.
//
// Honest-coverage rule: anything we cannot confidently map becomes
// kind="other"/unparsed and is COUNTED, never hidden.
//
// Note on placement: this lives at crossing-guard/harvest (public) rather than
// internal/harvest until the experiment modules merge into the root module
// at M4 — Go's internal-visibility rule would bar them from importing it.
package harvest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// titleMaxLen bounds a derived session title (display budget; applied at
	// every place a title is built, so it lives in one place).
	titleMaxLen = 90
	// scanConcurrency caps the parallel session-file scan fan-out.
	scanConcurrency = 16
)

type SessionSummary struct {
	Runtime        string    `json:"runtime"`
	ID             string    `json:"id"`
	ResumeID       string    `json:"resume_id,omitempty"` // runtime-owned canonical native resume handle
	Project        string    `json:"project"`
	Title          string    `json:"title"`
	Modified       time.Time `json:"modified"`
	Lines          int       `json:"lines"`
	Path           string    `json:"path"`
	SourceRef      string    `json:"source_ref,omitempty"`
	SourceSegment  string    `json:"source_segment,omitempty"`
	UpdateMarker   string    `json:"-"`
	Provider       string    `json:"provider,omitempty"`
	ParentID       string    `json:"parent_id,omitempty"`
	ActivityStatus string    `json:"activity_status,omitempty"` // unknown when this source cannot prove exact per-session liveness
	Context        int64     `json:"context,omitempty"`         // last-turn context occupancy (handoff radar)
	Cwd            string    `json:"cwd,omitempty"`             // real working dir from the session file (resume needs it; Project is display-mangled for dashed dirs)
	// RepositoryKey is the one cross-stack grouping identity for the Sessions rail.
	// It normalizes the macOS /private alias and folds vendor worktrees exactly once;
	// display labels and paging must consume this value rather than re-resolve cwd.
	RepositoryKey string `json:"repository_key"`
	Model         string `json:"model,omitempty"` // last model observed in the session
	Turns         int    `json:"turns,omitempty"` // assistant turns with usage telemetry
	// governance-index tags (branch/commits/PRs/memory-writes) — canonical
	// record fields; merged by the daemon after a pure scan
	Branch    string   `json:"branch,omitempty"`
	Commits   int      `json:"commits,omitempty"`
	PRs       []string `json:"prs,omitempty"`
	MemWrites int      `json:"mem_writes,omitempty"`
	// identity capture (canonicalization is the ADR 0009 decision — until
	// then both identifiers are carried): ID above is the filename stem;
	// ThreadID is codex's session_meta.session_id (bare uuid)
	ThreadID string `json:"thread_id,omitempty"`
	// Vendor-authored titles, carried separately from Title so the display layer
	// can tell a REAL title from a truncated first prompt. TitleSource declares
	// which one Title came from: "custom" (the user renamed the session), "ai"
	// (the runtime's generated title), "thread" (codex thread name), or "prompt"
	// (fallback — the first user message, NOT a title; render it as such).
	CustomTitle       string `json:"custom_title,omitempty"`
	AITitle           string `json:"ai_title,omitempty"`
	TitleSource       string `json:"title_source,omitempty"`
	HasTranscript     bool   `json:"has_transcript"`
	HasCapture        bool   `json:"has_capture,omitempty"`
	HasChangeEvidence bool   `json:"has_change_evidence,omitempty"`
	// UserTurns counts real user messages (injected "<...>" context and
	// tool_result-only lines excluded) — distinct from Turns, which counts
	// assistant turns carrying usage telemetry
	UserTurns int `json:"user_turns,omitempty"`
	// Native lineage, OBSERVED provenance only — the vendor's own file states
	// the relationship (native-session-lineage-plan §6). MetaID is the
	// artifact's own vendor id where the store separates artifact identity
	// from thread identity (codex session_meta.id); the remaining fields are
	// empty unless the vendor published them — nothing here is inferred.
	MetaID          string `json:"meta_id,omitempty"`
	LineageKind     string `json:"lineage_kind,omitempty"`  // native-subagent | native-thread-spawn | ""
	LineageDepth    int    `json:"lineage_depth,omitempty"` // the runtime's own reported depth, else 0
	LineageRole     string `json:"lineage_role,omitempty"`
	LineageNickname string `json:"lineage_nickname,omitempty"`
}

type CanonicalEvent struct {
	Seq  int    `json:"seq"`
	Kind string `json:"kind"` // user|assistant|thinking|tool_call|tool_result|summary|system|context|other; context is text a hook added to the conversation
	Ts   string `json:"ts,omitempty"`
	Name string `json:"name,omitempty"` // tool name for tool_call
	Text string `json:"text,omitempty"`
	// FullLen is the payload's length BEFORE clipping, in bytes; zero when
	// nothing was cut. A transcript keeps payloads short so a 1,300-event
	// session stays openable, but "short" silently discarded the thing a
	// reviewer came for: the code an agent wrote. Declaring the real size means
	// the console can say how much it is not showing and go fetch the rest
	// (EventText) instead of presenting a fragment as the whole.
	FullLen int `json:"full_len,omitempty"`
	// TurnAnchor is the opaque vendor-composed identity of the turn this event
	// belongs to (codex turn_id, claude record uuid) — populated where the
	// vendor states one, empty when unknown. Generic code compares anchors
	// only for equality; the TurnAnchorer capability owns composition.
	TurnAnchor string `json:"turn_anchor,omitempty"`
}

// textCaps bounds how much of each payload kind a parse keeps.
//
// It is a parameter rather than a constant because the same parser serves two
// callers with opposite needs: rendering a whole transcript, where hundreds of
// full tool payloads would be tens of megabytes, and expanding ONE chip, where
// a fragment is useless. Same code path, two budgets.
type textCaps struct{ tool, system, thinking, meta int }

// transcriptCaps is the whole-session budget: enough to recognise an event,
// not enough to review it. deepCaps is the single-event budget.
var (
	transcriptCaps = textCaps{tool: 2000, system: 500, thinking: 400, meta: 400}
	deepCaps       = textCaps{tool: eventTextMax, system: eventTextMax, thinking: eventTextMax, meta: eventTextMax}
)

// eventTextMax bounds even a deep read. A tool result can be an entire file;
// past a megabyte the answer is "open it in your editor", not a bigger panel.
const eventTextMax = 1 << 20

// SessionUsage aggregates token/cost telemetry the vendors already write to
// disk. Claude: message.usage on every assistant line (context occupancy =
// last input+cache_read+cache_creation). Codex: token_count event_msg carries
// total_token_usage + model_context_window explicitly. All observed, not
// inferred — absent fields stay zero and the UI must label them unknown.
type SessionUsage struct {
	Model         string  `json:"model,omitempty"`
	Turns         int     `json:"turns"`          // assistant messages with usage
	InputTokens   int64   `json:"input_tokens"`   // cumulative non-cache input
	OutputTokens  int64   `json:"output_tokens"`  // cumulative output
	CacheRead     int64   `json:"cache_read"`     // cumulative cache reads
	CacheCreate   int64   `json:"cache_create"`   // cumulative cache writes
	Context       int64   `json:"context"`        // last-turn context occupancy (tokens)
	ContextWindow int64   `json:"context_window"` // model window if the vendor states it (codex); else 0 = unknown
	CacheHitRate  float64 `json:"cache_hit_rate"` // cumulative cache_read / (cache_read+input+cache_create)
}

type SessionDetail struct {
	SessionSummary
	Events   []CanonicalEvent `json:"events"`
	Unparsed int              `json:"unparsed"` // honest coverage: lines we could not map
	Usage    *SessionUsage    `json:"usage,omitempty"`
}

func home() string { h, _ := os.UserHomeDir(); return h }

// Summary cache: reading every JSONL line on every request costs seconds at
// ~550 sessions (measured 5-8s/request). Cache per file keyed on mtime+size;
// only changed files are re-read. The product's SQLite store makes this moot.
type cacheEntry struct {
	mod    time.Time
	size   int64
	sum    SessionSummary
	usage  *SessionUsage         // nil when the file carries no telemetry
	days   map[string]*DayBucket // per-day usage buckets (key: YYYY-MM-DD)
	marker string
}

// DayBucket aggregates observed token telemetry for one calendar day.
type DayBucket struct {
	Input       int64 `json:"input"`
	Output      int64 `json:"output"`
	CacheRead   int64 `json:"cache_read"`
	CacheCreate int64 `json:"cache_create"`
	Turns       int   `json:"turns"`
}

var summaryCache sync.Map // path -> cacheEntry

type fileJob struct {
	runtime string
	path    string
	project string
	mod     time.Time
	size    int64
}

// ScanSessions lists sessions from both runtimes, newest first. Pure over
// the vendor files: governance-index enrichment is merged by the caller.
func ScanSessions() []SessionSummary {
	var jobs []fileJob
	out := []SessionSummary{}
	for _, rt := range Runtimes() {
		if source, ok := rt.(SessionSource); ok {
			records, err := source.ListSessionRecords()
			if err != nil {
				continue
			}
			for _, record := range records {
				out = append(out, cacheSessionRecord(record))
			}
			continue
		}
		jobs = append(jobs, rt.Collect()...)
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, scanConcurrency)
	for _, j := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(j fileJob) {
			defer wg.Done()
			defer func() { <-sem }()
			rt := runtimeFor(j.runtime)
			if rt == nil {
				return
			}
			sum, ok := summaryForJob(rt, j)
			if !ok {
				return
			}
			mu.Lock()
			out = append(out, sum)
			mu.Unlock()
		}(j)
	}
	wg.Wait()
	// title overlays (e.g. codex's user-curated thread name), applied after the
	// cache because the thread-name index changes independently of rollouts
	for i := range out {
		decorateSummary(&out[i])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out
}

func logicalCacheKey(ref SessionRef) string {
	return ref.Runtime + "\x00" + ref.Source + "\x00" + ref.Segment
}

func summaryCacheKey(s SessionSummary) string {
	if s.SourceRef != "" {
		return logicalCacheKey(SessionRef{Runtime: s.Runtime, Source: s.SourceRef, Segment: s.SourceSegment})
	}
	return s.Path
}

func cacheSessionRecord(record SessionRecord) SessionSummary {
	sum := record.Summary
	if sum.Runtime == "" {
		sum.Runtime = record.Ref.Runtime
	}
	if sum.ID == "" {
		sum.ID = record.Ref.ID
	}
	sum.SourceRef, sum.SourceSegment, sum.UpdateMarker = record.Ref.Source, record.Ref.Segment, record.Ref.UpdateMarker
	if sum.Path == "" {
		sum.Path = record.Ref.Source
	}
	sum.RepositoryKey = RepositoryGroupKey(sum)
	key := logicalCacheKey(record.Ref)
	if cached, ok := summaryCache.Load(key); ok && cached.(cacheEntry).marker == record.Ref.UpdateMarker {
		return cached.(cacheEntry).sum
	}
	summaryCache.Store(key, cacheEntry{sum: sum, usage: record.Usage, days: record.Days, marker: record.Ref.UpdateMarker})
	return sum
}

func summaryForJob(rt Runtime, j fileJob) (SessionSummary, bool) {
	if cached, ok := summaryCache.Load(j.path); ok {
		entry := cached.(cacheEntry)
		if entry.mod.Equal(j.mod) && entry.size == j.size {
			return entry.sum, true
		}
	}
	sum, usage, days, ok := rt.Summarize(j)
	if !ok {
		return SessionSummary{}, false
	}
	sum.RepositoryKey = RepositoryGroupKey(sum)
	summaryCache.Store(j.path, cacheEntry{mod: j.mod, size: j.size, sum: sum, usage: usage, days: days})
	return sum, true
}

func decorateSummary(sum *SessionSummary) {
	// Older entries in the in-process summary cache can predate a presentation-only
	// projection change during tests. Keep the derived identity total and deterministic.
	if sum.RepositoryKey == "" {
		sum.RepositoryKey = RepositoryGroupKey(*sum)
	}
	sum.ResumeID = CanonicalID(*sum)
	// ResumeHandle capability: the native resume handle when it differs from
	// canonical identity (codex children resume their parent thread).
	if handle, ok := runtimeFor(sum.Runtime).(ResumeHandle); ok {
		if native := handle.ResumeID(*sum); native != "" {
			sum.ResumeID = native
		}
	}
	if rt := runtimeFor(sum.Runtime); rt != nil {
		if title := rt.ThreadTitle(*sum); title != "" {
			sum.Title = title
		}
	}
	markTitleSource(sum)
}

// NoProjectKey is the explicit weak-identity bucket used when neither a real cwd nor a
// vendor project identity is available.
const NoProjectKey = "(no project)"

// RepositoryGroupKey is the ONE repository grouping resolver for harvested sessions.
// It preserves the previously shipped browser behavior: real cwd first, vendor project
// fallback, the macOS /private alias, and Claude worktree folding into the parent.
func RepositoryGroupKey(s SessionSummary) string {
	key := s.Cwd
	if key == "" {
		key = s.Project
	}
	if strings.HasPrefix(key, "/private/") {
		key = strings.TrimPrefix(key, "/private")
	}
	if owner, ok := runtimeFor(s.Runtime).(RepositoryGrouper); ok {
		key = owner.RepositoryGroupKey(s, key)
	}
	if key == "" {
		return NoProjectKey
	}
	return key
}

// stem is a session file's id: its basename without the .jsonl suffix.
func stem(path string) string { return strings.TrimSuffix(filepath.Base(path), ".jsonl") }

// SessionFiles lists one runtime's session file paths (discovery only; the
// incremental indexer walks these with its own watermarks).
func SessionFiles(runtime string) []string {
	rt := runtimeFor(runtime)
	if rt == nil {
		return nil
	}
	var paths []string
	for _, j := range rt.Collect() {
		paths = append(paths, j.path)
	}
	return paths
}

func LogicalSessionRecords(runtime string) ([]SessionRecord, bool, error) {
	source, ok := sessionSource(runtime)
	if !ok {
		return nil, false, nil
	}
	records, err := source.ListSessionRecords()
	if err == nil {
		for index := range records {
			records[index].Summary = cacheSessionRecord(records[index])
		}
	}
	return records, true, err
}

// SummarizeFile summarizes a single session file (cache-aware), for callers
// doing per-file incremental work. Same semantics as ScanSessions, including
// the codex thread-name title overlay.
func SummarizeFile(runtime, path string) (SessionSummary, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return SessionSummary{}, false
	}
	rt := runtimeFor(runtime)
	if rt == nil {
		return SessionSummary{}, false
	}
	j := fileJob{runtime: runtime, path: path, mod: info.ModTime(), size: info.Size()}
	var sum SessionSummary
	if c, ok := summaryCache.Load(path); ok {
		if e := c.(cacheEntry); e.mod.Equal(j.mod) && e.size == j.size {
			sum = e.sum
		}
	}
	if sum.Path == "" {
		s, usage, days, ok := rt.Summarize(j)
		if !ok {
			return SessionSummary{}, false
		}
		summaryCache.Store(path, cacheEntry{mod: j.mod, size: j.size, sum: s, usage: usage, days: days})
		sum = s
	}
	if t := rt.ThreadTitle(sum); t != "" {
		sum.Title = t
	}
	markTitleSource(&sum)
	return sum, true
}

// markTitleSource records WHERE Title came from, so the UI never presents a
// truncated first prompt as though it were a real session title. Absence of a
// vendor title is a fact worth showing, not one to paper over.
func markTitleSource(s *SessionSummary) {
	if s.TitleSource == "vendor" {
		return
	}
	switch {
	case s.CustomTitle != "" && s.Title == truncate(s.CustomTitle, titleMaxLen):
		s.TitleSource = "custom"
	case s.AITitle != "" && s.Title == truncate(s.AITitle, titleMaxLen):
		s.TitleSource = "ai"
	case s.ThreadID != "" && s.CustomTitle == "" && s.AITitle == "":
		s.TitleSource = "thread" // codex thread name overlay (or its own fallback)
	default:
		s.TitleSource = "prompt" // the first user message — NOT an authored title
	}
}

// FindAll resolves every source file belonging to one runtime/session identity. Codex
// thread ids can name several resumed rollout files. Exact/suffix filename candidates
// serve the common path through the existing summary cache; a full scan is fallback-only
// for identities an adapter cannot encode in its filename.
//
// Newest-wins contract (Find, and every daemon caller that takes matches[0]):
// MatchID owns membership, so a thread-id query returns only the thread's own
// segments — a subagent child rollout, however new, is not a match and can
// never be handed back as the thread's primary (codex MatchID).
func FindAll(runtime, id string) []SessionSummary {
	matches := []SessionSummary{}
	if records, supported, err := LogicalSessionRecords(runtime); supported {
		if err != nil {
			return matches
		}
		for _, record := range records {
			if MatchID(record.Summary, id) {
				matches = append(matches, record.Summary)
			}
		}
		sort.Slice(matches, func(i, j int) bool { return matches[i].Modified.After(matches[j].Modified) })
		return matches
	}
	// Exact/suffix filename candidates cover the common Claude id and Codex rollout/
	// thread-id shapes without parsing every transcript. The full scan remains the
	// fallback for resumed Codex threads whose canonical id is not in the filename.
	if rt := runtimeFor(runtime); rt != nil {
		for _, job := range rt.Collect() {
			candidateID := stem(job.path)
			if candidateID != id && !strings.HasSuffix(candidateID, id) {
				continue
			}
			sum, ok := summaryForJob(rt, job)
			if !ok {
				continue
			}
			decorateSummary(&sum)
			if MatchID(sum, id) {
				matches = append(matches, sum)
			}
		}
	}
	if len(matches) > 0 {
		sort.Slice(matches, func(i, j int) bool { return matches[i].Modified.After(matches[j].Modified) })
		return matches
	}
	for _, s := range ScanSessions() {
		if s.Runtime == runtime && MatchID(s, id) {
			matches = append(matches, s)
		}
	}
	return matches
}

// Find resolves the newest matching session summary. Callers that derive facts across a
// resumed session must use FindAll rather than silently choosing one segment.
func Find(runtime, id string) (SessionSummary, bool) {
	matches := FindAll(runtime, id)
	if len(matches) > 0 {
		return matches[0], true
	}
	return SessionSummary{}, false
}

// Normalize reads one session file into canonical events. A zero-turn usage
// means no telemetry was observed; callers should render "unknown", and
// Load already nils it out.
func Normalize(runtime, path string) ([]CanonicalEvent, int, *SessionUsage, error) {
	rt := runtimeFor(runtime)
	if rt == nil {
		return nil, 0, nil, fmt.Errorf("unknown runtime %q", runtime)
	}
	return rt.Normalize(path)
}

func NormalizeSummary(s SessionSummary, full bool) ([]CanonicalEvent, int, *SessionUsage, error) {
	if source, ok := sessionSource(s.Runtime); ok && s.SourceRef != "" {
		return source.NormalizeSession(SessionRef{Runtime: s.Runtime, ID: s.ID, Source: s.SourceRef,
			Segment: s.SourceSegment, UpdateMarker: s.UpdateMarker}, full)
	}
	if full {
		if deep, ok := runtimeFor(s.Runtime).(DeepNormalizer); ok {
			return deep.NormalizeFull(s.Path)
		}
	}
	return Normalize(s.Runtime, s.Path)
}

// Load resolves and normalizes one session.
func Load(runtime, id string) (*SessionDetail, error) {
	s, ok := Find(runtime, id)
	if !ok {
		return nil, fmt.Errorf("session not found: %s/%s", runtime, id)
	}
	d := &SessionDetail{SessionSummary: s}
	var err error
	d.Events, d.Unparsed, d.Usage, err = NormalizeSummary(s, false)
	if d.Usage != nil && d.Usage.Turns == 0 {
		d.Usage = nil
	}
	return d, err
}

// prettyProject converts the escaped cwd dir name back to something readable.
func prettyProject(escaped string) string {
	s := strings.TrimPrefix(escaped, "-")
	return "/" + strings.ReplaceAll(s, "-", "/")
}

// --- helpers ---

func newLineScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024) // sessions have huge lines
	return sc
}

// asInt64 tolerates the float64 that encoding/json produces for numbers.
func asInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}

// finishUsage derives the cache-hit rate.
func finishUsage(u *SessionUsage) {
	if denom := u.CacheRead + u.InputTokens + u.CacheCreate; denom > 0 {
		u.CacheHitRate = float64(u.CacheRead) / float64(denom)
	}
}

func anyString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case nil:
		return ""
	case []any, map[string]any:
		return compactJSON(s)
	default:
		return fmt.Sprintf("%v", s)
	}
}

func compactJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// clip is truncate that also reports the ORIGINAL size, so a caller can record
// how much it dropped instead of leaving the reader to guess whether an
// ellipsis means "a bit more" or "another twenty kilobytes".
func clip(s string, n int) (string, int) {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s, 0
	}
	return s[:n] + "…", len(s)
}
