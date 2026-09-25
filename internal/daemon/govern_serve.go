package daemon

// Serve-side of the live governor (plan Phase 1b): the daemon owns a read-write
// handle on the one index and exposes an observe endpoint the hook feeds. Observe
// is write-through today; the in-memory hot cache + async batching is the latency
// work deferred until we deploy the widened matcher and measure under real load.

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"crossing-guard/engine"
	"crossing-guard/harvest"
	"crossing-guard/internal/analyzerhost"
	"crossing-guard/internal/changeenv"
	"crossing-guard/internal/collectionconfig"
	"crossing-guard/store"
)

var governor *Governor // nil until initGovernor succeeds

const (
	sessionTraceDefaultLimit = 100
	sessionTraceMaxLimit     = 200
	sessionCaptureLimit      = 200
)

// initGovernor opens the writable index and loads the layered detector library.
// Non-fatal: absent, the daemon still serves everything else and observe returns 503.
func initGovernor(dataDir string, dets []engine.Detector) error {
	loaded, err := collectionconfig.Load(dataDir)
	if err != nil {
		return err
	}
	ix, err := store.Open(indexPath())
	if err != nil {
		return err
	}
	governor = NewGovernor(ix, dets)
	host, hostErr := analyzerhost.Build(dataDir)
	if hostErr != nil {
		_ = ix.Close()
		return hostErr
	}
	governor.analyzerAssembly = host.Assembly
	if host.SelectionError != nil {
		governor.analyzerSelectionError = host.SelectionError.Error()
		log.Printf("analyzer modules inactive: %v", host.SelectionError)
	}
	governor.resultPayloadMode = loaded.Document.ResultPayloadMode
	governor.collectionConfigOrigin = loaded.Origin
	startObservationReplay(dataDir, governor)
	startLifecycleReconciliation(governor)
	startUnderstandingScheduling(governor)
	recoverPendingCheckpoints(governor)
	startIdleTimeoutScheduler(governor)
	return nil
}

// POST /api/govern/observe — the hook feed. Classify + append + fold, no decision
// (observe-only, Phase 1b). Kept lean: the hook fires this best-effort on the tool
// path, so it must return fast.
func handleGovernObserve(w http.ResponseWriter, r *http.Request) {
	if governor == nil {
		http.Error(w, "governor not configured", http.StatusServiceUnavailable)
		return
	}
	var o Observation
	if err := json.NewDecoder(r.Body).Decode(&o); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Validate before it reaches PRIMARY TRUTH. An unvalidated observe let a load
	// probe write 210 session-less rows into the append-only log — 41% of it — which
	// cannot be undone by design and had to be purged by hand. Junk is cheap to
	// reject here and permanent one line later.
	if o.SessionID == "" {
		http.Error(w, "session is required", http.StatusBadRequest)
		return
	}
	if o.Tool == "" {
		http.Error(w, "tool is required", http.StatusBadRequest)
		return
	}
	switch o.Decision {
	case "", "allow", "deny", "ask": // "" = caller reported none; recorded as unknown
	default:
		http.Error(w, "decision must be allow, deny, or ask", http.StatusBadRequest)
		return
	}
	// Runtime is a caller claim (see Observation), and it feeds a GROUP BY plus
	// every per-runtime surface — so clamp its SHAPE here even though its truth
	// cannot be checked: a misbehaving caller must not be able to spray unbounded
	// or unprintable cardinality into PRIMARY TRUTH. Deliberately not an allowlist
	// of known vendors: the installer registry is a different package's knowledge,
	// and a new vendor's events must not be rejected by a stale daemon.
	if len(o.Runtime) > 64 || !validRuntimeName(o.Runtime) {
		http.Error(w, "runtime must be a short lowercase identifier", http.StatusBadRequest)
		return
	}
	if o.TS == 0 {
		o.TS = time.Now().Unix()
	}
	// The live endpoint is live truth by definition: a hook cannot mark itself
	// imported. Only the in-process importer sets origin=imported.
	o.Origin = "live"
	if err := governor.Observe(o); err != nil {
		// The action was accepted and validated but could NOT be persisted (a full
		// disk is the canonical case). Count it: this is the single silent-loss path
		// the event log itself cannot show, because the missing row is the evidence.
		governor.observeFailures.Add(1)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// validRuntimeName clamps the shape of a claimed runtime: empty (unknown) or a
// short identifier — letters, digits, dash, underscore, dot.
func validRuntimeName(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// StatefulDecision is what /api/govern/decide returns: the stateful-tier verdict, or
// allow when nothing stateful fired. Decision is the AUTHORING vocabulary the hook
// enforces — allow | deny | ask — translated from the engine's HardBlock/ConfirmAndRecord
// here, so the wire contract is honest and the hook can route ask→override. `evaluated`
// says whether the stateful tier even ran, so the hook logs the honest reason (ADR 0025:
// never imply a session gate enforced when it was not evaluated).
type StatefulDecision struct {
	Decision  string `json:"decision"` // allow | deny | ask
	Rule      string `json:"rule,omitempty"`
	Message   string `json:"message,omitempty"`
	Reason    string `json:"reason"`
	Evaluated bool   `json:"evaluated"`
}

// POST /api/govern/decide — the stateful-tier consult (Phase 3). The hook sends the
// action; the daemon decides over live session/target state. FAIL-OPEN by contract:
// if the governor is not configured or the platform is not armed, this answers allow
// with evaluated=false, and the hook proceeds — a daemon problem never blocks a tool
// call (owner decision 2026-07-20).
func handleGovernDecide(w http.ResponseWriter, r *http.Request) {
	if governor == nil {
		writeJSON(w, StatefulDecision{Decision: "allow", Evaluated: false,
			Reason: "governor not configured — stateful tier not evaluated (fail-open)"})
		return
	}
	var o Observation
	if err := json.NewDecoder(r.Body).Decode(&o); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	d, err := governor.DecideStateful(o)
	if err != nil {
		// A rules-load error must not block the tool: report allow, unevaluated.
		writeJSON(w, StatefulDecision{Decision: "allow", Evaluated: false,
			Reason: "stateful tier errored, not evaluated (fail-open): " + err.Error()})
		return
	}
	if d == nil {
		writeJSON(w, StatefulDecision{Decision: "allow", Evaluated: true,
			Reason: "no stateful rule fired"})
		return
	}
	// Translate the engine's severity ladder to the hook's authoring vocabulary:
	// ConfirmAndRecord is an OVERRIDABLE ask (the hook routes it to askHuman); anything
	// else that reached here (HardBlock) is a hard deny. DecideStateful already dropped
	// warn/silent to nil, so those never arrive.
	wire := "deny"
	if d.Mode == engine.ConfirmAndRecord {
		wire = "ask"
	}
	writeJSON(w, StatefulDecision{Decision: wire, Rule: d.Rule, Message: d.Message,
		Evaluated: true, Reason: "stateful rule " + d.Rule + " fired over live session/target state"})
}

// GET /api/govern/health — capture liveness (D13). Answers, out loud, whether
// capture is actually happening. When the governor never opened this reports
// configured:false rather than 503, because a console needs to distinguish "the
// governor is down" from "the daemon is down" — both broke capture, differently.
func handleGovernHealth(w http.ResponseWriter, r *http.Request) {
	if governor == nil {
		writeJSON(w, GovernorHealth{Configured: false})
		return
	}
	h, err := governor.Health()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, h)
}

// GET /api/govern/runtimes — the event log grouped by the agent that produced it.
// This is what makes "is the hook FIRING" answerable per runtime: a vendor config
// proves registration, and only an event proves behaviour. `verify` polls it and
// `doctor` reports it.
func handleGovernRuntimes(w http.ResponseWriter, r *http.Request) {
	if governor == nil {
		writeJSON(w, map[string]any{"configured": false, "runtimes": []any{},
			"note": "the governor is not configured — nothing is being captured, so no runtime can be shown as firing"})
		return
	}
	stats, err := governor.ix.RuntimeStats()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"configured": true, "runtimes": stats,
		"note": "runtime is EMPTY for events captured before the hook carried it — unattributed, never inferred"})
}

// EntityTouch is one session's contact with a resource — the answer to "which
// session touched this file?", in the form a HUMAN asked it. The governance log
// keys on raw session ids, so the title is joined here; without it this endpoint
// answers in UUIDs, which is technically correct and useless.
type EntityTouch struct {
	SessionID string   `json:"session_id"`
	Title     string   `json:"title,omitempty"`
	Runtime   string   `json:"runtime,omitempty"`
	Verbs     []string `json:"verbs"`     // read | write | exec … distinct, in order seen
	Decisions []string `json:"decisions"` // distinct decisions recorded for this pair
	Events    int      `json:"events"`
	FirstSeen int64    `json:"first_seen"`
	LastSeen  int64    `json:"last_seen"`
}

// EntityReport is the whole answer for one resource: what it is, what we know
// about it, and who touched it.
type EntityReport struct {
	ID        string           `json:"id"`
	Found     bool             `json:"found"`
	Note      string           `json:"note,omitempty"` // why the answer is empty, when it is
	Entity    *store.Entity    `json:"entity,omitempty"`
	State     []store.StateRow `json:"state"`
	Touches   []EntityTouch    `json:"touches"`
	Events    int              `json:"events"`
	Truncated bool             `json:"truncated"` // hit the row cap: the answer is PARTIAL
}

// GET /api/govern/entity?id=<entity-id|path> — which sessions touched a resource.
// Accepts a bare path or url and canonicalizes it, because a caller should not have
// to know our id scheme to ask about a file they can see.
func handleGovernEntity(w http.ResponseWriter, r *http.Request) {
	if governor == nil {
		http.Error(w, "governor not configured", http.StatusServiceUnavailable)
		return
	}
	raw := strings.TrimSpace(r.URL.Query().Get("id"))
	if raw == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	rep, err := governor.EntityReport(canonicalEntityID(raw), entityEventCap)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, rep)
}

// canonicalEntityID turns what a human types into the id we store. Already-prefixed
// ids pass through; a URL becomes url:<host>; anything else is treated as a file.
func canonicalEntityID(s string) string {
	for _, p := range []string{"file:", "url:", "mcp:", "session:", "memory:", "db:"} {
		if strings.HasPrefix(s, p) {
			return s
		}
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return engine.EntityID("url", s)
	}
	return engine.EntityID("file", s)
}

type sessionSummaryResponse struct {
	Section string                 `json:"section"`
	Summary SessionEvidenceSummary `json:"summary"`
}

type sessionChangeResponse struct {
	ID      string                `json:"id"`
	Section string                `json:"section"`
	Change  changeenv.SessionView `json:"change"`
}

type sessionImpactRepository struct {
	RepositoryID    string               `json:"repository_id"`
	CheckoutID      string               `json:"checkout_id"`
	CheckoutDisplay string               `json:"checkout_display"`
	Portability     string               `json:"portability"`
	Downstream      changeenv.ImpactView `json:"downstream"`
}

type sessionImpactProjection struct {
	Available       bool                      `json:"available"`
	Partial         bool                      `json:"partial"`
	Reason          string                    `json:"reason,omitempty"`
	RepositoryCount int                       `json:"repository_count"`
	RepositoryPage  changeenv.Page            `json:"repository_page"`
	Repositories    []sessionImpactRepository `json:"repositories"`
}

type sessionImpactResponse struct {
	ID      string                  `json:"id"`
	Section string                  `json:"section"`
	Change  sessionImpactProjection `json:"change"`
}

type sessionVerifyRepository struct {
	RepositoryID     string                   `json:"repository_id"`
	CheckoutID       string                   `json:"checkout_id"`
	CheckoutDisplay  string                   `json:"checkout_display"`
	Verification     []changeenv.Verification `json:"verification"`
	VerificationPage changeenv.Page           `json:"verification_page"`
}

type sessionVerifyProjection struct {
	Available       bool                      `json:"available"`
	Partial         bool                      `json:"partial"`
	Reason          string                    `json:"reason,omitempty"`
	RepositoryCount int                       `json:"repository_count"`
	RepositoryPage  changeenv.Page            `json:"repository_page"`
	Repositories    []sessionVerifyRepository `json:"repositories"`
}

type sessionVerifyResponse struct {
	ID      string                  `json:"id"`
	Section string                  `json:"section"`
	Change  sessionVerifyProjection `json:"change"`
}

type sessionTraceResponse struct {
	ID             string                `json:"id"`
	Section        string                `json:"section"`
	Found          bool                  `json:"found"`
	Events         []store.EventRecord   `json:"events"`
	EventResources []store.EventResource `json:"event_resources"`
	EventCount     int                   `json:"event_count"`
	SnapshotID     int64                 `json:"snapshot_id"`
	NextCursor     string                `json:"next_cursor,omitempty"`
}

type sessionActionResponse struct {
	Section string              `json:"section"`
	Action  SessionActionDetail `json:"action"`
}

type sessionReachResponse struct {
	Section string       `json:"section"`
	Reach   SessionReach `json:"reach"`
}

type sessionCaptureResponse struct {
	Section string         `json:"section"`
	Capture SessionCapture `json:"capture"`
}

type sessionFileResponse struct {
	Section string            `json:"section"`
	File    SessionFileDetail `json:"file"`
}

type sessionBodyResponse struct {
	Section string      `json:"section"`
	Body    SessionBody `json:"body"`
}

type sessionCodeChangesResponse struct {
	Section     string                `json:"section"`
	CodeChanges changeenv.CodeChanges `json:"code_changes"`
}

type sessionCodeChangeFileResponse struct {
	Section string                         `json:"section"`
	File    changeenv.CodeChangeFileDetail `json:"code_change_file"`
}

func encodeTraceCursor(snapshot, ts, id int64) string {
	raw := strconv.FormatInt(snapshot, 10) + ":" + strconv.FormatInt(ts, 10) + ":" + strconv.FormatInt(id, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeTraceCursor(raw string) (snapshot, ts, id int64, err error) {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return 0, 0, 0, err
	}
	parts := strings.Split(string(decoded), ":")
	if len(parts) != 3 {
		return 0, 0, 0, errors.New("invalid trace cursor")
	}
	values := []*int64{&snapshot, &ts, &id}
	for index, part := range parts {
		value, parseErr := strconv.ParseInt(part, 10, 64)
		if parseErr != nil || value < 0 {
			return 0, 0, 0, errors.New("invalid trace cursor")
		}
		*values[index] = value
	}
	return snapshot, ts, id, nil
}

type queryGetter interface{ Get(string) string }

func sessionViewOptions(q queryGetter, sessionRoots []string) (changeenv.ViewOptions, error) {
	atoi := func(k string) int { n, _ := strconv.Atoi(q.Get(k)); return n }
	centerKind, centerRef := "", ""
	if center := q.Get("impact_center"); center != "" {
		var ok bool
		centerKind, centerRef, ok = strings.Cut(center, ":")
		if !ok || centerKind == "" || centerRef == "" {
			return changeenv.ViewOptions{}, errors.New("impact_center must be KIND:REF")
		}
	}
	impactLimit := atoi("impact_limit")
	return changeenv.ViewOptions{RepositoryOffset: atoi("repository_offset"), RepositoryLimit: atoi("repository_limit"), FileOffset: atoi("change_offset"), FileLimit: atoi("change_limit"), FileFilter: q.Get("change_filter"), FileQuery: q.Get("change_query"), FileSort: q.Get("change_sort"), ObservedOffset: atoi("observed_offset"), ObservedLimit: atoi("observed_limit"), ObservedScope: q.Get("observed_scope"), SessionRoots: sessionRoots, AddedOffset: atoi("added_offset"), OmittedOffset: atoi("omitted_offset"), VerificationOffset: atoi("verification_offset"), ImpactNodeOffset: atoi("impact_node_offset"), ImpactEdgeOffset: atoi("impact_edge_offset"), ImpactLimit: impactLimit, ImpactCandidateOffset: atoi("impact_candidate_offset"), ImpactCandidateLimit: impactLimit, ImpactCenterKind: centerKind, ImpactCenterRef: centerRef, AnalyzerBundleDigest: currentAnalyzerBundle()}, nil
}

// GET /api/govern/session?id=<session> — what we captured for one session: its
// folded state AND its events.
//
// BREAKING (2026-07-20): this returned a bare []StateRow. It now returns a
// SessionReport envelope, symmetric with /api/govern/entity. The bare array could
// not say WHY it was empty, so no console view could satisfy INV-21 from it. Done
// now rather than versioned around because the endpoint had no console consumer
// yet — `grep api/govern internal/daemon/static/` was empty on the day of the
// change. The state rows are unchanged, under the `state` key.
func handleGovernSession(w http.ResponseWriter, r *http.Request) {
	if governor == nil {
		http.Error(w, "governor not configured", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	rawID := strings.TrimSpace(q.Get("id"))
	sessionID := rawID
	sessionRoots := []string{}
	section := strings.TrimSpace(q.Get("section"))
	if runtime := strings.TrimSpace(q.Get("runtime")); runtime != "" {
		stored, err := governor.StoredSessionIdentity(rawID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if stored {
			if section == "" || section == "change" || section == "impact" || section == "verify" || section == "file" {
				roots, rootErr := governor.StoredSessionRoots(rawID)
				if rootErr != nil {
					http.Error(w, rootErr.Error(), http.StatusInternalServerError)
					return
				}
				sessionRoots = append(sessionRoots, roots...)
			}
		} else {
			matches := harvest.FindAll(runtime, rawID)
			for _, match := range matches {
				if strings.TrimSpace(match.Cwd) != "" {
					sessionRoots = append(sessionRoots, match.Cwd)
				}
			}
			if len(matches) > 0 {
				sessionID = harvest.CanonicalID(matches[0])
			}
		}
	}
	if section != "" {
		handleGovernSessionSection(w, q, sessionID, sessionRoots, section)
		return
	}
	viewOption, optionErr := sessionViewOptions(q, sessionRoots)
	if optionErr != nil {
		http.Error(w, optionErr.Error(), http.StatusBadRequest)
		return
	}
	rep, err := governor.SessionReport(sessionID, entityEventCap, viewOption)
	if err != nil {
		if errors.Is(err, changeenv.ErrImpactCenterNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
		} else if errors.Is(err, changeenv.ErrInvalidFileView) {
			http.Error(w, err.Error(), http.StatusBadRequest)
		} else if errors.Is(err, changeenv.ErrResponseMetadataOverBudget) {
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	const maxSessionResponse = 2 << 20
	changeBytes, _ := json.Marshal(changeenv.SessionView{})
	change := rep.Change
	rep.Change = changeenv.SessionView{}
	fixedBytes, marshalErr := json.Marshal(rep)
	rep.Change = change
	if marshalErr != nil {
		http.Error(w, marshalErr.Error(), http.StatusInternalServerError)
		return
	}
	changeBudget := maxSessionResponse - len(fixedBytes) + len(changeBytes)
	if changeBudget <= 0 {
		http.Error(w, changeenv.ErrResponseMetadataOverBudget.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	if err := changeenv.FitResponse(&rep.Change, changeBudget); err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	if encoded, err := json.Marshal(rep); err != nil || len(encoded) > maxSessionResponse {
		http.Error(w, changeenv.ErrResponseMetadataOverBudget.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	writeJSON(w, rep)
}

func handleGovernSessionSection(w http.ResponseWriter, q queryGetter, sessionID string, sessionRoots []string, section string) {
	switch section {
	case "summary":
		handleGovernSessionSummary(w, q, sessionID, section)
	case "trace":
		handleGovernSessionTrace(w, q, sessionID, section)
	case "action":
		handleGovernSessionAction(w, q, sessionID, section)
	case "reach":
		handleGovernSessionReach(w, sessionID, section)
	case "capture":
		handleGovernSessionCapture(w, q, sessionID, section)
	case "file":
		handleGovernSessionFile(w, q, sessionID, section)
	case "body":
		handleGovernSessionBody(w, q, sessionID, section)
	case "edits":
		handleGovernSessionEdits(w, q, sessionID, section)
	case "code-changes":
		handleGovernSessionCodeChanges(w, q, sessionID, section)
	case "code-change-file":
		handleGovernSessionCodeChangeFile(w, q, sessionID, section)
	case "statements":
		handleGovernSessionStatements(w, q, sessionID, section)
	case "checks":
		handleGovernSessionChecks(w, q, sessionID, section)
	case "change", "impact", "verify":
		handleGovernSessionChange(w, q, sessionID, sessionRoots, section)
	default:
		http.Error(w, "unsupported session section", http.StatusBadRequest)
	}
}

func sessionCodeChangeOptions(q queryGetter) (changeenv.CodeChangeOptions, error) {
	options := changeenv.CodeChangeOptions{RepositoryID: strings.TrimSpace(q.Get("repository_id")),
		CheckoutID: strings.TrimSpace(q.Get("checkout_id")), AnalyzerBundleDigest: currentAnalyzerBundle()}
	if len(options.RepositoryID) > 512 || len(options.CheckoutID) > 512 {
		return options, errors.New("repository_id and checkout_id must not exceed 512 bytes")
	}
	if raw := strings.TrimSpace(q.Get("checkpoint_id")); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 1 {
			return options, errors.New("checkpoint_id must be a positive integer")
		}
		options.CheckpointID = value
	}
	if raw := strings.TrimSpace(q.Get("code_offset")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			return options, errors.New("code_offset must be a non-negative integer")
		}
		options.Offset = value
	}
	options.Limit = 25
	if raw := strings.TrimSpace(q.Get("code_limit")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 25 {
			return options, errors.New("code_limit must be between 1 and 25")
		}
		options.Limit = value
	}
	return options, nil
}

func handleGovernSessionCodeChanges(w http.ResponseWriter, q queryGetter, sessionID, section string) {
	options, err := sessionCodeChangeOptions(q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	changes, err := governor.SessionCodeChanges(sessionID, options)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "selected checkpoint was not found for this session checkout", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, sessionCodeChangesResponse{Section: section, CodeChanges: changes})
}

func handleGovernSessionCodeChangeFile(w http.ResponseWriter, q queryGetter, sessionID, section string) {
	options, err := sessionCodeChangeOptions(q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	path := strings.TrimSpace(q.Get("path"))
	if path == "" || len(path) > 4096 {
		http.Error(w, "path is required and must not exceed 4096 bytes", http.StatusBadRequest)
		return
	}
	structuralOffset := 0
	if raw := strings.TrimSpace(q.Get("structural_offset")); raw != "" {
		structuralOffset, err = strconv.Atoi(raw)
		if err != nil || structuralOffset < 0 {
			http.Error(w, "structural_offset must be a non-negative integer", http.StatusBadRequest)
			return
		}
	}
	structuralLimit := 25
	if raw := strings.TrimSpace(q.Get("structural_limit")); raw != "" {
		structuralLimit, err = strconv.Atoi(raw)
		if err != nil || structuralLimit < 1 || structuralLimit > 100 {
			http.Error(w, "structural_limit must be between 1 and 100", http.StatusBadRequest)
			return
		}
	}
	file, err := governor.SessionCodeChangeFile(sessionID, path, options, structuralOffset, structuralLimit)
	if errors.Is(err, changeenv.ErrInvalidFileView) {
		http.Error(w, "invalid repository-relative code path", http.StatusBadRequest)
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "code-change file was not found for this session checkout", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, sessionCodeChangeFileResponse{Section: section, File: file})
}

func handleGovernSessionSummary(w http.ResponseWriter, q queryGetter, sessionID, section string) {
	summary, err := governor.SessionEvidenceSummary(sessionID, strings.TrimSpace(q.Get("runtime")))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, sessionSummaryResponse{Section: section, Summary: summary})
}

func handleGovernSessionAction(w http.ResponseWriter, q queryGetter, sessionID, section string) {
	eventID, err := strconv.ParseInt(q.Get("event_id"), 10, 64)
	if err != nil || eventID < 1 {
		http.Error(w, "event_id must be a positive integer", http.StatusBadRequest)
		return
	}
	action, err := governor.SessionAction(sessionID, eventID)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "action not found for session", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, sessionActionResponse{Section: section, Action: action})
}

func handleGovernSessionReach(w http.ResponseWriter, sessionID, section string) {
	reach, err := governor.SessionReach(sessionID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, sessionReachResponse{Section: section, Reach: reach})
}

func handleGovernSessionCapture(w http.ResponseWriter, q queryGetter, sessionID, section string) {
	capture, err := governor.SessionCapture(sessionID, strings.TrimSpace(q.Get("runtime")), sessionCaptureLimit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, sessionCaptureResponse{Section: section, Capture: capture})
}

func handleGovernSessionFile(w http.ResponseWriter, q queryGetter, sessionID, section string) {
	file, err := governor.SessionFile(sessionID, strings.TrimSpace(q.Get("file_key")))
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "file evidence not found for session", http.StatusNotFound)
		return
	}
	if errors.Is(err, changeenv.ErrInvalidFileView) {
		http.Error(w, "invalid file evidence key", http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, sessionFileResponse{Section: section, File: file})
}

// sessionEditsResponse is the typed body of section=edits.
type sessionEditsResponse struct {
	Section string                 `json:"section"`
	Edits   changeenv.SessionEdits `json:"edits"`
}

func handleGovernSessionEdits(w http.ResponseWriter, q queryGetter, sessionID, section string) {
	config, _ := consoleConfig()
	limit := config.EditsPageSize
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 {
			http.Error(w, "limit must be a positive integer", http.StatusBadRequest)
			return
		}
		if value < limit {
			limit = value
		}
	}
	offset := 0
	if raw := strings.TrimSpace(q.Get("offset")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			http.Error(w, "offset must be a non-negative integer", http.StatusBadRequest)
			return
		}
		offset = value
	}
	edits, err := governor.SessionEdits(sessionID, limit, offset)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, sessionEditsResponse{Section: section, Edits: edits})
}

func handleGovernSessionBody(w http.ResponseWriter, q queryGetter, sessionID, section string) {
	kind := strings.TrimSpace(q.Get("body_kind"))
	eventID, resultID, ordinal := int64(0), int64(0), 0
	var parseErr error
	switch kind {
	case "event_input":
		eventID, parseErr = strconv.ParseInt(q.Get("event_id"), 10, 64)
		if parseErr != nil || eventID < 1 {
			http.Error(w, "event_id must be a positive integer", http.StatusBadRequest)
			return
		}
	case "result":
		resultID, parseErr = strconv.ParseInt(q.Get("result_id"), 10, 64)
		if parseErr != nil || resultID < 1 {
			http.Error(w, "result_id must be a positive integer", http.StatusBadRequest)
			return
		}
	case "effect_content", "effect_diff", "effect_before", "effect_after":
		resultID, parseErr = strconv.ParseInt(q.Get("result_id"), 10, 64)
		if parseErr != nil || resultID < 1 {
			http.Error(w, "result_id must be a positive integer", http.StatusBadRequest)
			return
		}
		ordinal, parseErr = strconv.Atoi(q.Get("ordinal"))
		if parseErr != nil || ordinal < 0 {
			http.Error(w, "ordinal must be a non-negative integer", http.StatusBadRequest)
			return
		}
	default:
		http.Error(w, "unsupported body_kind", http.StatusBadRequest)
		return
	}
	body, err := governor.SessionBody(sessionID, kind, eventID, resultID, ordinal)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "retained textual body unavailable", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, sessionBodyResponse{Section: section, Body: body})
}

func handleGovernSessionChange(w http.ResponseWriter, q queryGetter, sessionID string, sessionRoots []string, section string) {
	viewOption, err := sessionViewOptions(q, sessionRoots)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	view, err := governor.SessionChange(sessionID, viewOption)
	if err != nil {
		if errors.Is(err, changeenv.ErrImpactCenterNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
		} else if errors.Is(err, changeenv.ErrInvalidFileView) {
			http.Error(w, err.Error(), http.StatusBadRequest)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	switch section {
	case "impact":
		writeJSON(w, sessionImpactResponse{ID: sessionID, Section: section, Change: projectSessionImpact(view)})
	case "verify":
		writeJSON(w, sessionVerifyResponse{ID: sessionID, Section: section, Change: projectSessionVerify(view)})
	default:
		writeJSON(w, sessionChangeResponse{ID: sessionID, Section: section, Change: view})
	}
}

func projectSessionImpact(view changeenv.SessionView) sessionImpactProjection {
	out := sessionImpactProjection{Available: view.Available, Partial: view.Partial,
		Reason: view.Reason, RepositoryCount: view.RepositoryCount,
		RepositoryPage: view.RepositoryPage, Repositories: []sessionImpactRepository{}}
	for _, repository := range view.Repositories {
		out.Repositories = append(out.Repositories, sessionImpactRepository{
			RepositoryID: repository.RepositoryID, CheckoutID: repository.CheckoutID,
			CheckoutDisplay: repository.CheckoutDisplay, Portability: repository.Portability,
			Downstream: repository.Downstream})
	}
	return out
}

func projectSessionVerify(view changeenv.SessionView) sessionVerifyProjection {
	out := sessionVerifyProjection{Available: view.Available, Partial: view.Partial,
		Reason: view.Reason, RepositoryCount: view.RepositoryCount,
		RepositoryPage: view.RepositoryPage, Repositories: []sessionVerifyRepository{}}
	for _, repository := range view.Repositories {
		out.Repositories = append(out.Repositories, sessionVerifyRepository{
			RepositoryID: repository.RepositoryID, CheckoutID: repository.CheckoutID,
			CheckoutDisplay: repository.CheckoutDisplay, Verification: repository.Verification,
			VerificationPage: repository.VerificationPage})
	}
	return out
}

func handleGovernSessionTrace(w http.ResponseWriter, q queryGetter, sessionID, section string) {
	limit, err := strconv.Atoi(q.Get("limit"))
	if q.Get("limit") == "" {
		limit = sessionTraceDefaultLimit
	} else if err != nil || limit < 1 || limit > sessionTraceMaxLimit {
		http.Error(w, "limit must be between 1 and "+strconv.Itoa(sessionTraceMaxLimit), http.StatusBadRequest)
		return
	}
	query := store.SessionTraceQuery{Limit: limit, Decision: strings.TrimSpace(q.Get("decision")), Origin: strings.TrimSpace(q.Get("origin")), Query: strings.TrimSpace(q.Get("query"))}
	if len(query.Query) > 200 {
		http.Error(w, "query exceeds 200 bytes", http.StatusBadRequest)
		return
	}
	if cursor := strings.TrimSpace(q.Get("cursor")); cursor != "" {
		query.SnapshotID, query.AfterTS, query.AfterID, err = decodeTraceCursor(cursor)
		if err != nil {
			http.Error(w, "invalid trace cursor", http.StatusBadRequest)
			return
		}
	}
	trace, err := governor.SessionTrace(sessionID, query)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	response := sessionTraceResponse{ID: trace.ID, Section: section, Found: trace.Found,
		Events: trace.Events, EventResources: trace.Resources, EventCount: trace.Total,
		SnapshotID: trace.Snapshot}
	if trace.HasMore {
		response.NextCursor = encodeTraceCursor(trace.Snapshot, trace.NextTS, trace.NextID)
	}
	writeJSON(w, response)
}

// GET /api/govern/sessions — sessions we hold governance rows for, newest first.
// Backs the Governance rail. Distinct from /api/sessions, which enumerates vendor
// session files and knows nothing about what was captured.
func handleGovernSessions(w http.ResponseWriter, r *http.Request) {
	if governor == nil {
		http.Error(w, "governor not configured", http.StatusServiceUnavailable)
		return
	}
	list, err := governor.GovernedSessions(governedSessionCap)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, list)
}
