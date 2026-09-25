package daemon

// Audit over REAL harvested sessions (P4 @ harvest reach) — the console's center of
// gravity per the redesign: author a policy, run it across every real session, and see
// which sessions it flags, keyed to the session itself. Post-hoc DETECT/REPORT only
// (harvest reach, design §8) — never a live block; the UI must say so.
//
// It reuses the shared crossing-guard/engine (Classify + Predicate.Match + FiredTags) —
// the SAME code the hook and ledger use — over consoleprobe's own harvested events.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"crossing-guard/engine"
	"crossing-guard/internal/rulebook"
)

var (
	auditDetectors     []engine.Detector
	auditDetectorKinds map[string]string
	auditMigrationErr  error
)

func initAudit(dets []engine.Detector) error {
	auditDetectors = dets
	auditDetectorKinds = map[string]string{"builtin:index": "source"}
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
// every tool call by source (tool identity) + shaped patterns in its input, plus the
// already-indexed memory-write fact. Deduped — a session-scoped set.
func sessionTags(detail *SessionDetail, memWrites int) []engine.Tag {
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
		// Role scoping: tool_call events feed the tool/command/file detectors;
		// assistant/user/thinking/tool_result feed the chat-behavior and content
		// detectors. The engine skips a detector whose declared Roles exclude this
		// event's role, so one library serves every channel.
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
		for _, t := range engine.Classify(e, auditDetectors) {
			add(t)
		}
	}
	if memWrites > 0 {
		add(engine.Tag{Key: "memory-access", Value: "write", Detector: "builtin:index",
			Provenance: engine.Observed, Scope: "session",
			Evidence: fmt.Sprintf("%d memory write(s)", memWrites)})
	}
	return withSessionScope(tags)
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

// AuditFinding is one (rule, session) hit, with the tags that fired.
type AuditFinding struct {
	Rule     string         `json:"rule"`
	Severity string         `json:"severity"`
	Message  string         `json:"message"`
	Session  SessionSummary `json:"session"`
	Fired    []AuditTag     `json:"fired"`
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
		"reach": "harvest — DETECT/REPORT only (post-hoc); never a live block (design §8)",
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
	sessions := ScanSessions()
	var findings []AuditFinding
	evaluated, withTags, failed := 0, 0, 0
	if r.Context().Err() != nil {
		return
	}
	// The LIVE policy dry-run — "what would the rules I have actually ARMED have
	// done across my history". This is the loop the
	// governance thesis needs: author a rule once, see its real-world impact, and
	// it is the same rule the hook enforces. Loaded once, not per session.
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
		tags := sessionTags(detail, s.MemWrites)
		if len(tags) > 0 {
			withTags++
		}
		findings = append(findings, livePolicyFindings(live, tags, s)...)
	}
	if findings == nil {
		findings = []AuditFinding{}
	}
	writeJSON(w, map[string]any{
		"findings": findings, "evaluated": evaluated, "with_tags": withTags,
		"total": len(sessions), "failed": failed, "elapsed_ms": nowMillis() - start,
		"live_rules": len(live),
		"reach": "harvest — post-hoc findings on past sessions; NOT blocks. A session " +
			"with no finding is a WEAK negative (no declared detector matched), not a clean bill.",
	})
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
// aggregate history.
func livePolicyFindings(rules []engine.Rule, tags []engine.Tag, s SessionSummary) []AuditFinding {
	var out []AuditFinding
	for _, r := range rules {
		if engine.Match(r.If, tags) {
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
			})
		}
	}
	return out
}

func auditTags(tags []engine.Tag) []AuditTag {
	out := make([]AuditTag, 0, len(tags))
	for _, tag := range tags {
		kind := auditDetectorKinds[tag.Detector]
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
	tags := sessionTags(detail, detail.MemWrites)
	matched := livePolicyFindings(live, tags, detail.SessionSummary)
	if tags == nil {
		tags = []engine.Tag{}
	}
	writeJSON(w, map[string]any{
		"watermark": engine.WaterMark(tags), "tags": tags, "matched": matched,
		"weak_negative": len(tags) == 0,
		"reach":         "post-hoc harvest — what we OBSERVED this session touch; a weak negative is not a clean bill",
	})
}
