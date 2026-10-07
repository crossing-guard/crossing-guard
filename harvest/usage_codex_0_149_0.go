package harvest

// Codex usage calls (token-usage-analytics plan §3.3), measured against codex
// 0.153–0.154 rollouts. Codex writes token_count events whose
// total_token_usage is a running total and whose last_token_usage is the one
// call that produced the event. The running total cannot be trusted as a
// total: a forked or reviewer child starts with its parent's total already in
// it, and a new process appending to a rollout restarts it (plan M2a, M2b).
// So one call per event whose total differs from the previous event's, counted
// from last_token_usage — which handles both cases alike (red-team T-RT8).
//
// A forked or spawned child also REPLAYS its parent's token_count events,
// sometimes restamped (code red-team C-1: 564 replayed calls in 9 children
// here). So a call's id is its content — its running total and last-call
// figures — scoped to the thread family (session_meta.session_id, which every
// child shares with its parent): a replay repeats its parent's id, and the
// store counts it once. Measured: no two distinct calls of one rollout share
// that content, and scoping keeps apart the 2 coincidences between families.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
)

// usage-3 carries each child's vendor-published role in the cursor state
// (session usage breakdown plan P-3); the bump re-reads every rollout once so
// completed children learn it too.
const codexUsageReaderIdentity = "codex-0.149.0/usage-3"

// codexUsageState is what a rollout's reader carries across reads: the last
// total seen, the model and effort of the current turn, and the session_meta
// facts, which only the first line states (red-team S2-11, S3-2).
type codexUsageState struct {
	PrevTotal *int64 `json:"prev_total,omitempty"`
	Model     string `json:"model,omitempty"`
	Effort    string `json:"effort,omitempty"`
	Client    string `json:"client,omitempty"`
	Parent    string `json:"parent,omitempty"`
	// Role is the child's vendor-published role (a thread-spawn agent_role, or
	// the named flavor such as "guardian"); "" for a top-level rollout.
	Role string `json:"role,omitempty"`
	// Family is the thread family every call id is scoped to.
	Family   string `json:"family,omitempty"`
	MetaSeen bool   `json:"meta_seen,omitempty"`
}

// observe folds one decoded record into the state and returns the call it
// states, if any. key is the rollout's own id, the family scope until
// session_meta names one.
func (state *codexUsageState) observe(record codexRecord, obj map[string]any, session, key string) (UsageCall, bool) {
	switch {
	case record.Envelope == "session_meta":
		state.observeMeta(record.Body)
	case record.Envelope == "turn_context":
		if model := anyString(record.Body["model"]); model != "" {
			state.Model = model
		}
		state.Effort = anyString(record.Body["effort"])
	case record.Envelope == "token_count" || (record.Envelope == "event_msg" && record.Kind == "token_count"):
		return state.observeTokenCount(record.Body, obj, session, key)
	case record.Envelope == "event_msg":
		// Some turns state their model on a task event before any turn_context.
		if model := anyString(record.Body["model"]); model != "" && state.Model == "" {
			state.Model = model
		}
	}
	return UsageCall{}, false
}

func (state *codexUsageState) observeMeta(body map[string]any) {
	if state.MetaSeen {
		return // a child replays its parent's header; the first one is its own
	}
	var meta codexFileMeta
	applyCodexSessionMetaLineage(&meta, body)
	state.MetaSeen = true
	state.Client = anyString(body["cli_version"])
	state.Parent = meta.parentThreadID
	state.Role = meta.agentRole
	state.Family = anyString(body["session_id"])
	if state.Family == "" {
		state.Family = anyString(body["id"])
	}
}

func (state *codexUsageState) observeTokenCount(body, obj map[string]any, session, key string) (UsageCall, bool) {
	info, _ := body["info"].(map[string]any)
	if info == nil {
		return UsageCall{}, false
	}
	totals, _ := info["total_token_usage"].(map[string]any)
	total := statedTokens(totals, "total_tokens")
	if total != nil && state.PrevTotal != nil && *total == *state.PrevTotal {
		return UsageCall{}, false // a repeated event, not a new call
	}
	state.PrevTotal = total
	last, _ := info["last_token_usage"].(map[string]any)
	if last == nil {
		return UsageCall{}, false
	}
	at := parseUsageTime(anyString(obj["timestamp"]))
	cached := statedTokens(last, "cached_input_tokens")
	family := state.Family
	if family == "" {
		family = key
	}
	call := UsageCall{ID: family + ":" + codexCallDigest(totals, last), Session: session, ParentSession: state.Parent,
		FirstAt: at, At: at, Model: state.Model, Effort: state.Effort, Client: state.Client,
		TokenClasses: TokenClasses{
			CacheRead:  cached,
			CacheWrite: statedTokens(last, "cache_write_input_tokens"),
			Output:     statedTokens(last, "output_tokens"),
			Reasoning:  statedTokens(last, "reasoning_output_tokens"),
		}}
	// input_tokens includes the cached input; the neutral class excludes it.
	if input := statedTokens(last, "input_tokens"); input != nil {
		fresh := *input - valueOf(cached)
		call.Input = &fresh
	}
	if window := statedTokens(info, "model_context_window"); window != nil && *window > 0 {
		call.ContextWindow = window
	}
	return AdmitUsageCall(call)
}

// codexCallDigest identifies a call by what it states: the running total and
// the last call's figures, in canonical JSON (encoding/json sorts map keys).
func codexCallDigest(totals, last map[string]any) string {
	body, err := json.Marshal([]map[string]any{totals, last})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:12])
}

// codexUsageCalls folds a whole rollout's calls, for the summary and
// transcript passes that already decode every record.
type codexUsageCalls struct {
	state        codexUsageState
	session, key string
	calls        []UsageCall
}

func newCodexUsageCalls(path string) *codexUsageCalls {
	session := stem(path)
	return &codexUsageCalls{session: session, key: codexRolloutKey(session)}
}

func (c *codexUsageCalls) observe(record codexRecord, obj map[string]any) {
	if call, ok := c.state.observe(record, obj, c.session, c.key); ok {
		c.calls = append(c.calls, call)
	}
}

// fold sums the rollout's calls for its summary and transcript. The model is
// the one the rollout states last, even when no call followed it yet: that is
// the model the session is using (the rail's model tag).
func (c *codexUsageCalls) fold() *SessionUsage {
	usage := FoldCalls(c.calls)
	if usage != nil && c.state.Model != "" {
		usage.Model = c.state.Model
	}
	return usage
}

// codexRolloutKey is a rollout's own id: the uuid ending its filename, which
// equals session_meta.id for every rollout measured (399 of 399).
func codexRolloutKey(session string) string {
	if len(session) >= uuidLen && looksLikeUUID(session[len(session)-uuidLen:]) {
		return session[len(session)-uuidLen:]
	}
	return session
}

func (codexRuntime) UsageReader() string { return codexUsageReaderIdentity }

// UsageSources lists every rollout. The key comes from the filename, so no
// rollout is opened to list it.
func (codexRuntime) UsageSources(ctx context.Context) ([]UsageSourceRef, error) {
	// Non-strict: one unreadable directory must not hide every other rollout
	// (code red-team C-9; the plan rejected runtime-wide blocking, M6).
	jobs, err := collectCodexJobs(false)
	if err != nil {
		return nil, err
	}
	refs := make([]UsageSourceRef, 0, len(jobs))
	for _, job := range jobs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := statRegular(job.path)
		if err != nil {
			continue
		}
		session := strings.TrimSuffix(filepath.Base(job.path), ".jsonl")
		key := codexRolloutKey(session)
		refs = append(refs, UsageSourceRef{Key: key, Session: session, SessionAlias: key,
			Marker: fileUsageMarker(info), Size: info.Size(), Modified: info.ModTime(),
			Born: sourceBirthTime(info), locator: job.path})
	}
	sortUsageSources(refs)
	return refs, nil
}

// ReadUsage reads one rollout from its cursor.
func (codexRuntime) ReadUsage(ctx context.Context, request UsageReadRequest) (UsageBatch, error) {
	path := request.Source.locator
	if path == "" {
		return UsageBatch{}, errUsageLocator
	}
	calls := &codexUsageCalls{session: request.Source.Session, key: request.Source.Key}
	batch, err := readFileUsage(ctx, path, request.Cursor, request.Budget, fileUsageAdapter{
		runtime: "codex",
		begin: func(carried json.RawMessage) fileUsageLine {
			if len(carried) > 0 {
				_ = json.Unmarshal(carried, &calls.state) // a malformed state restarts the counts
			}
			return func(record []byte, _ int64) {
				var obj map[string]any
				if json.Unmarshal(record, &obj) != nil {
					return
				}
				if decoded, ok := decodeCodexRecord(obj); ok {
					calls.observe(decoded, obj)
				}
			}
		},
		finish: func() ([]UsageCall, json.RawMessage, error) {
			state, err := json.Marshal(calls.state)
			return calls.calls, state, err
		},
	})
	batch.ParentSession = calls.state.Parent
	batch.Role = calls.state.Role
	return batch, err
}
