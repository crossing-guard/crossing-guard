package daemon

// Audit over REAL harvested sessions (P4 @ harvest reach) — the console's center of
// gravity per the redesign: author a policy, run it across every real session, and see
// which sessions it flags, keyed to the session itself. Post-hoc DETECT/REPORT only
// (harvest reach, design §8) — never a live block; the UI must say so.
//
// It reuses the shared crossing-guard/engine (Classify + Predicate.Match + FiredTags) —
// the SAME code the hook and ledger use — over consoleprobe's own harvested events.

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"crossing-guard/engine"
	"crossing-guard/harvest"
	"crossing-guard/internal/rulebook"
	"crossing-guard/store"
)

var (
	auditDetectors     []engine.Detector
	auditDetectorKinds map[string]string
	auditMigrationErr  error
)

func initAudit(dets []engine.Detector) error {
	auditDetectors = dets
	transcriptFactsMu.Lock() // facts cached under the old detectors are stale now
	transcriptFactsCache = map[[16]byte][]string{}
	transcriptFactsMu.Unlock()
	auditDetectorKinds = map[string]string{}
	for _, detector := range dets {
		auditDetectorKinds[detector.ID] = detector.Kind
	}
	return nil
}

var auditShellTools = map[string]bool{"Bash": true, "shell": true, "local_shell": true, "exec_command": true}

// extractToolInput pulls the command/path/url channels out of a tool_call's input
// JSON (the input rode along in the event Text per harvest.go, truncated to 600 bytes).
// If the JSON does not parse — which happens whenever the input exceeds 600 bytes and is
// cut mid-string — we FALL BACK to the raw text as the command channel, so a long
// destructive command (heredoc, big `curl|sh`, multi-flag `rm`) still matches the
// command detectors on its prefix. Returning empty here instead would make the audit
// surface report the most dangerous sessions as clean.
func extractToolInput(text string) (cmd, path, url string) {
	var in map[string]any
	if json.Unmarshal([]byte(text), &in) != nil {
		return text, "", "" // truncated/invalid → scan the raw prefix, don't go blind
	}
	switch c := in["command"].(type) {
	case string:
		cmd = c
	case []any:
		var toks []string
		for _, e := range c {
			if s, ok := e.(string); ok {
				toks = append(toks, s)
			}
		}
		cmd = strings.Join(toks, " ")
	}
	if p, ok := in["file_path"].(string); ok {
		path = p
	} else if p, ok := in["notebook_path"].(string); ok {
		path = p
	}
	if u, ok := in["url"].(string); ok {
		url = u
	}
	return cmd, path, url
}

// sessionTags accumulates the deterministic tag set for one real session: classify
// every tool call by source (tool identity) + shaped patterns in its input. Deduped —
// a session-scoped set. (A computed `memory-access=write` fact used to be added
// here from the session summary's memory-write count; that count lost its only
// writer on 2026-07-20 and the fact was retired — audit-memory-write-fact plan.
// Detector `memory.write` emits the live `memory=write` fact.)
func sessionTags(detail *SessionDetail) []engine.Tag {
	seen := map[string]bool{}
	var tags []engine.Tag
	add := func(t engine.Tag) {
		k := t.Key + "|" + t.Value + "|" + t.Detector
		if !seen[k] {
			seen[k] = true
			tags = append(tags, t)
		}
	}
	for _, ev := range detail.Events {
		for _, t := range classifyTranscriptEvent(ev) {
			add(t)
		}
	}
	return withSessionScope(tags)
}

// classifyTranscriptEvent runs the audit detectors over one transcript row.
// Role scoping: tool_call events feed the tool/command/file detectors;
// assistant/user/thinking/tool_result feed the chat-behavior and content
// detectors. The engine skips a detector whose declared Roles exclude this
// event's role, so one library serves every channel. The shell-tool map is an
// existing compiled exception, kept as it is (session-view plan §B1).
func classifyTranscriptEvent(ev harvest.CanonicalEvent) []engine.Tag {
	e := engine.Event{Role: ev.Kind}
	if ev.Kind == "tool_call" {
		e.Tool = engine.BareTool(ev.Name)
		cmd, path, url := extractToolInput(ev.Text)
		e.Path, e.Destination = path, url
		if auditShellTools[e.Tool] {
			e.Text = cmd // command channel — command patterns match the command, not prose
		} else {
			e.Text = ev.Text // raw input so content detectors still scan write bodies
		}
	} else {
		e.Text = ev.Text
	}
	return engine.Classify(e, auditDetectors)
}

// withTranscriptFacts is the GET /api/session row shape: every row, with the
// facts of each tool_call row.
func withTranscriptFacts(events []harvest.CanonicalEvent) []liveEvent {
	out := make([]liveEvent, len(events))
	for i, event := range events {
		out[i] = liveEvent{CanonicalEvent: event}
	}
	addTranscriptFacts(out)
	return out
}

// addTranscriptFacts sets the key:value facts of each tool_call row, deduped
// and in detector order. Classification is the whole detector library per row,
// so rows are spread over the CPUs and each row's facts are cached by a digest
// of its tool and input: re-opening a session costs lookups, not regexes.
// (Measured 2026-09-23 on a 9,538-row session: ~650 ms serial and uncached.)
func addTranscriptFacts(events []liveEvent) {
	var calls []int
	for i := range events {
		if events[i].Kind == "tool_call" {
			calls = append(calls, i)
		}
	}
	workers := runtime.GOMAXPROCS(0)
	if workers > len(calls) {
		workers = len(calls)
	}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for n := w; n < len(calls); n += workers {
				events[calls[n]].Facts = transcriptFacts(events[calls[n]].CanonicalEvent)
			}
		}(w)
	}
	wg.Wait()
}

// transcriptFactsCacheLimit bounds the facts cache; past it the cache starts
// over. A fixed bound, not a preference: it caps memory, not behaviour.
const transcriptFactsCacheLimit = 100_000

var (
	transcriptFactsMu    sync.Mutex
	transcriptFactsCache = map[[16]byte][]string{}
)

func transcriptFacts(event harvest.CanonicalEvent) []string {
	sum := sha256.Sum256([]byte(event.Name + "\x00" + event.Text))
	var key [16]byte
	copy(key[:], sum[:16])
	transcriptFactsMu.Lock()
	cached, ok := transcriptFactsCache[key]
	transcriptFactsMu.Unlock()
	if ok {
		return cached
	}
	var facts []string
	seen := map[string]bool{}
	for _, tag := range classifyTranscriptEvent(event) {
		fact := tag.Key + ":" + tag.Value
		if !seen[fact] {
			seen[fact] = true
			facts = append(facts, fact)
		}
	}
	transcriptFactsMu.Lock()
	if len(transcriptFactsCache) >= transcriptFactsCacheLimit {
		transcriptFactsCache = map[[16]byte][]string{}
	}
	transcriptFactsCache[key] = facts
	transcriptFactsMu.Unlock()
	return facts
}

// withSessionScope re-exposes every accumulated tag under the `session:` prefix,
// in ADDITION to its bare form.
//
// This is what lets ONE rule run in both the dry-run (here) and the live gate
// (decide.go). The live path namespaces a session's FOLDED state as
// `session:<key>` and evaluates a rule at the moment of one action; the dry-run
// has no "current action", so a session's whole accumulated set IS its state.
// Exposing it as `session:*` makes `not: session:area=docs` mean the honest
// whole-session thing — "this session never touched a doc" — so a stateful rule
// authored once reads correctly in both places instead of needing two spellings.
//
// Bare tags stay, so the existing command/source audit rules are untouched.
func withSessionScope(tags []engine.Tag) []engine.Tag {
	scoped := make([]engine.Tag, 0, len(tags)*2)
	for _, t := range tags {
		scoped = append(scoped, t)
		st := t
		st.Key = sessionPrefix + t.Key
		scoped = append(scoped, st)
	}
	return scoped
}

// The audit's agent_state values: whether a session's agent:* keys joined its
// dry-run tag set.
const (
	auditAgentRead        = "read"
	auditAgentUnavailable = "unavailable"
	auditAgentNotNeeded   = "not-needed" // no armed rule reads agent:*, nothing was read
)

// withAgentTags adds the session's live agent:* keys to its dry-run tags when an
// armed rule reads them. The keys join the evaluation set only: they are not
// session:-scoped copies and never feed the watermark or the observed tag list.
// The error, returned with auditAgentUnavailable, says why the keys were unread.
func withAgentTags(rules []engine.Rule, tags []engine.Tag, row SessionSummary, now int64) ([]engine.Tag, string, error) {
	needed := false
	for _, r := range rules {
		if engine.ReferencesKey(r.If, isAgentKey) {
			needed = true
			break
		}
	}
	if !needed {
		return tags, auditAgentNotNeeded, nil
	}
	agentTags, err := auditAgentTagsRead(row, now)
	if err != nil {
		return tags, auditAgentUnavailable, err
	}
	out := make([]engine.Tag, 0, len(tags)+len(agentTags))
	return append(append(out, tags...), agentTags...), auditAgentRead, nil
}

// auditAgentTagsRead is swapped by tests to count reads.
var auditAgentTagsRead = auditAgentTags

// auditAgentTags reads one harvested session's live agent:* keys in the shape the
// live gate evaluates (DecideStateful): Key = agent key, Value = stored provenance.
//
// It looks under every id in candidateIDs — the lookup set watching uses — with no
// belongsTo filter: the gate looks keys up by whatever id the hook sent, and for a
// Codex child rollout that is the parent thread's id, so the child's actions were
// decided over the parent's keys.
//
// The row read is capped for display; ActiveOrchestrationTagKeys replaces it here
// once it lands (plan §9).
func auditAgentTags(row SessionSummary, now int64) ([]engine.Tag, error) {
	if governor == nil || governor.ix == nil {
		return nil, fmt.Errorf("agent tags unreadable: no store")
	}
	seen := map[string]bool{}
	var out []engine.Tag
	for _, id := range candidateIDs(row) {
		rows, err := governor.ix.ActiveOrchestrationTags(id, now)
		if err != nil {
			return nil, fmt.Errorf("agent tags for %s: %w", id, err)
		}
		for _, tag := range rows {
			if seen[tag.AgentKey] {
				continue
			}
			seen[tag.AgentKey] = true
			out = append(out, engine.Tag{Key: tag.AgentKey, Value: tag.Provenance,
				Provenance: engine.Provenance(tag.Provenance), Scope: "session"})
		}
	}
	return out, nil
}

// absentTerms lists the terms of each satisfied `not:` whose tag matched nothing —
// what a finding that fired by absence was missing. It walks the predicate in
// engine.Match's branch order, so it never names a term from a branch Match
// ignored. A term that matched nothing may still name a present tag whose value
// did not match, so a value term reads `tag=value` and a pattern term `tag~/re/`.
// A term the audit cannot read (unknown) is never listed: it was not found absent, it
// was not judged.
func absentTerms(p engine.Predicate, tags []engine.Tag, unknown engine.Unknown) []string {
	var out []string
	seen := map[string]bool{}
	var walk func(engine.Predicate)
	walk = func(p engine.Predicate) {
		switch {
		case len(p.All) > 0:
			for _, c := range p.All {
				walk(c)
			}
		case len(p.Any) > 0:
			for _, c := range p.Any {
				walk(c)
			}
		case p.Not != nil && engine.Judge(*p.Not, tags, unknown) == engine.No:
			for _, term := range engine.Terms(*p.Not) {
				name := term.Tag
				if term.Value != "" {
					name += "=" + term.Value
				} else if term.Matches != "" {
					name += "~/" + term.Matches + "/"
				}
				if !seen[name] && engine.Judge(term, tags, unknown) == engine.No {
					seen[name] = true
					out = append(out, name)
				}
			}
		}
	}
	walk(p)
	return out
}

// AuditFinding is one (rule, session) hit, with the tags that fired.
type AuditFinding struct {
	Rule     string         `json:"rule"`
	Severity string         `json:"severity"`
	Message  string         `json:"message"`
	Session  SessionSummary `json:"session"`
	Fired    []AuditTag     `json:"fired"`
	// Absent names the negated terms that held because nothing matched them —
	// the evidence of a `not:` finding, which has no tag to show.
	Absent []string `json:"absent,omitempty"`
}

// AuditTag decorates the canonical engine tag with detector kind for truthful
// presentation. Kind does not participate in matching and is not added to the
// engine schema merely to satisfy one report view.
type AuditTag struct {
	engine.Tag
	DetectorKind string `json:"detector_kind"`
}

// handleAuditRules returns the current rule set + the honest reach label.
func handleAuditRules(w http.ResponseWriter, r *http.Request) {
	rules, loaded, ok := auditPolicyRules(w)
	if !ok {
		return
	}
	writeJSON(w, map[string]any{
		"rules": rules, "path": loaded.Path, "source": loaded.Origin,
		"selection": loaded.Selection, "digest": loaded.Digest, "active": loaded.Active,
		"not_fully_audited": notFullyAudited(rules, auditBlindSpot()),
		"reach":             "harvest — DETECT/REPORT only (post-hoc); never a live block (design §8)",
	})
}

// handleAuditRun evaluates the policy over every harvested session. Synchronous full
// scan (reads each session's events); fine behind a manual Run button.
func handleAuditRun(w http.ResponseWriter, r *http.Request) {
	if auditDetectors == nil {
		http.Error(w, "audit not configured (no detectors) — set -detectors", http.StatusServiceUnavailable)
		return
	}
	live, _, ok := auditPolicyRules(w)
	if !ok {
		return
	}
	start := nowMillis()
	now := time.Now().Unix()
	sessions := ScanSessions()
	var findings []AuditFinding
	evaluated, withTags, failed, agentUnavailable := 0, 0, 0, 0
	if r.Context().Err() != nil {
		return
	}
	// The LIVE policy dry-run — "what would the rules I have actually ARMED have
	// done across my history". This is the loop the
	// governance thesis needs: author a rule once, see its real-world impact, and
	// it is the same rule the hook enforces. Loaded once, not per session.
	blind := auditBlindSpot()
	for _, s := range sessions {
		if r.Context().Err() != nil {
			return
		}
		detail, err := LoadSession(s.Runtime, s.ID)
		if err != nil {
			failed++
			continue
		}
		evaluated++
		tags := sessionTags(detail)
		if len(tags) > 0 {
			withTags++
		}
		evalTags, agentState, agentErr := withAgentTags(live, tags, s, now)
		if agentState == auditAgentUnavailable {
			if agentUnavailable == 0 { // one line per run, not one per session
				log.Printf("audit: agent: rules skipped where agent tags are unreadable: %v", agentErr)
			}
			agentUnavailable++
		}
		findings = append(findings, livePolicyFindings(live, evalTags, auditUnknownFor(blind, agentState), s)...)
	}
	if findings == nil {
		findings = []AuditFinding{}
	}
	writeJSON(w, map[string]any{
		"findings": findings, "evaluated": evaluated, "with_tags": withTags,
		"total": len(sessions), "failed": failed, "elapsed_ms": nowMillis() - start,
		"live_rules": len(live), "agent_unavailable": agentUnavailable,
		"rules_not_fully_audited": len(notFullyAudited(live, blind)),
		"reach": "harvest — post-hoc findings on past sessions; NOT blocks. A session " +
			"with no finding is a WEAK negative (no declared detector matched), not a clean bill.",
	})
}

func isAgentKey(key string) bool { return strings.HasPrefix(key, engine.AgentStatePrefix) }

// auditBlindSpot is what the audit can never read, whatever the session: a single
// invocation (command, tool), a target, and the session facts the daemon writes live
// only. It restates the harvest channel of the coverage labeler (engine.UnknownAtHarvest)
// over this daemon's declared state producers.
//
// Only the daemon's direct session facts can be live-only, and that catalog is compiled
// in: no store read is needed (agent claims are harvest producers — the audit reads them).
func auditBlindSpot() engine.Unknown {
	return engine.UnknownAtHarvest(store.StateProducersFor(nil, 0))
}

// auditUnknownFor adds the agent:* keys to the blind spot for a session whose keys could
// not be read.
func auditUnknownFor(blind engine.Unknown, agentState string) engine.Unknown {
	if agentState == auditAgentUnavailable {
		return engine.AnyUnknown(blind, engine.UnknownAgent)
	}
	return blind
}

// notFullyAudited lists the armed rules that read a fact the audit cannot see. Such a
// rule still reports through any branch the audit can decide; what it cannot decide is
// never a finding, and this list is how the audit says so.
func notFullyAudited(rules []engine.Rule, blind engine.Unknown) []string {
	out := []string{}
	for _, r := range rules {
		if engine.AnyTerm(r.If, blind) {
			out = append(out, r.ID)
		}
	}
	return out
}

// livePolicyRules returns the rules the user has actually armed — the same file the
// hook enforces — or nil if none can be read. A dry-run over the live policy must
// use the live policy, not a copy, or it proves nothing about what ships.
func livePolicyRules() ([]engine.Rule, *rulebook.LoadedDocument, error) {
	loaded, err := rulebook.LoadDocument()
	if err != nil {
		return nil, nil, err
	}
	if loaded.Policy == nil {
		return nil, loaded, nil
	}
	return statefulRules(loaded.Policy).Rules, loaded, nil
}

// auditPolicyRules is the single readiness/load boundary shared by list, bulk run,
// and per-session exposure. A failed legacy migration cannot masquerade as zero rules.
func auditPolicyRules(w http.ResponseWriter) ([]engine.Rule, *rulebook.LoadedDocument, bool) {
	if auditMigrationErr != nil {
		http.Error(w, "legacy audit-rule migration unresolved: "+auditMigrationErr.Error(),
			http.StatusServiceUnavailable)
		return nil, nil, false
	}
	rules, loaded, err := livePolicyRules()
	if err != nil {
		http.Error(w, "active rulebook unreadable: "+err.Error(), http.StatusInternalServerError)
		return nil, nil, false
	}
	return rules, loaded, true
}

// livePolicyFindings evaluates the armed rules over one session's dry-run tag set.
//
// rules is already the statefulRules subset: a bare command-regex rule is about a
// single action, not a property of a whole session, and must not be replayed over an
// aggregate history. unknown names what the audit cannot read for this session (no
// single invocation, no target, live-only session facts, and agent:* keys when they
// could not be read): a rule decided by such a term is left undecided and is never a
// finding — a missing key must not turn `not: agent:x` or `not: target:x` true.
func livePolicyFindings(rules []engine.Rule, tags []engine.Tag, unknown engine.Unknown, s SessionSummary) []AuditFinding {
	var out []AuditFinding
	for _, r := range rules {
		if engine.Judge(r.If, tags, unknown) == engine.Yes {
			severity := r.Severity
			if severity == "" {
				severity = severityForAction(r.Action)
			}
			out = append(out, AuditFinding{
				Rule:     r.ID + " (armed)",
				Severity: severity,
				Message:  firstNonEmpty(r.Message, r.Intent, "armed policy rule"),
				Session:  s,
				Fired:    auditTags(engine.FiredTags(r.If, tags)),
				Absent:   absentTerms(r.If, tags, unknown),
			})
		}
	}
	return out
}

func auditTags(tags []engine.Tag) []AuditTag {
	out := make([]AuditTag, 0, len(tags))
	for _, tag := range tags {
		kind := auditDetectorKinds[tag.Detector]
		if tag.Provenance == provenanceModelClaimed {
			kind = provenanceModelClaimed // an agent's claim, not an unknown detector
		}
		if kind == "" {
			kind = "unknown"
		}
		out = append(out, AuditTag{Tag: tag, DetectorKind: kind})
	}
	return out
}

// severityForAction maps an armed rule's action onto the audit severity vocabulary
// the UI already colours.
func severityForAction(action string) string {
	switch action {
	case "deny":
		return "high"
	case "ask", "redact":
		return "medium"
	default:
		return "low"
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func nowMillis() int64 { return time.Now().UnixMilli() }

// handleAuditSession computes ONE session's exposure: its accumulated tags, water
// mark, and which policy rules it trips — so the session detail can show WHY the
// audit flagged it, right next to the transcript.
func handleAuditSession(w http.ResponseWriter, r *http.Request) {
	if auditDetectors == nil {
		http.Error(w, "audit not configured", http.StatusServiceUnavailable)
		return
	}
	live, _, ok := auditPolicyRules(w)
	if !ok {
		return
	}
	runtime := r.URL.Query().Get("runtime")
	id := r.URL.Query().Get("id")
	detail, err := LoadSession(runtime, id)
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	tags := sessionTags(detail)
	evalTags, agentState, agentErr := withAgentTags(live, tags, detail.SessionSummary, time.Now().Unix())
	if agentErr != nil {
		log.Printf("audit: agent: rules skipped for %s: %v", id, agentErr)
	}
	blind := auditBlindSpot()
	matched := livePolicyFindings(live, evalTags, auditUnknownFor(blind, agentState), detail.SessionSummary)
	if tags == nil {
		tags = []engine.Tag{}
	}
	writeJSON(w, map[string]any{
		"watermark": engine.WaterMark(tags), "tags": tags, "matched": matched,
		"weak_negative": len(tags) == 0, "agent_state": agentState,
		"rules_not_fully_audited": len(notFullyAudited(live, blind)),
		"reach":                   "post-hoc harvest — what we OBSERVED this session touch; a weak negative is not a clean bill",
	})
}
