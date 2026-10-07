package daemon

// The live, tamper-evident session ledger — folded INTO the console daemon so one
// process owns both halves of the design's dual-use ledger (§5): harvest/findability
// (post-hoc, the existing session browser) AND live governance (daemon-owned,
// tamper-EVIDENT — append-only + hash-chained, not tamper-PROOF; the status endpoint
// states the same honest ceiling). It reuses the shared crossing-guard/engine package — the SAME source the
// `cp` hook compiles — so the one-evaluator rule holds by construction: console reads,
// hook writes, both classify+decide through identical code (guarded by engine's parity
// tests), not a re-implementation.

import (
	"encoding/json"
	"net/http"
	"time"

	"crossing-guard/engine"
	"crossing-guard/internal/rulebook"
	"crossing-guard/store"
)

var (
	liveLedger   *engine.Ledger // nil until initLedger succeeds (config present)
	enginePolicy *engine.Policy
)

// initLedger loads detectors + policy and opens the daemon-owned ledger. Non-fatal:
// if config is absent the daemon still serves harvest/memory/policy; the ledger routes
// report 503 so the UI can render "live governance not configured" honestly.
func initLedger(ledgerPath string, dets []engine.Detector, polPath string) error {
	// ADR 0025's convergence, applied to the file layout rather than the format:
	// policy-engine.json and policy/rules.json are the SAME format, so a separate
	// engine-ledger policy is a second copy of the rules you enforce — free to drift.
	// Absent, the dev ledger decides over the ACTIVE enforcement rules; a present file
	// still wins for the ledger. The coverage report no longer reads either: it labels
	// the rulebook itself (handlePolicyCoverage).
	supplement, err := rulebook.LoadInvocationPolicyPath(polPath, "daemon-invocation-or-compatibility")
	if err != nil {
		// errors.Is, NOT os.IsNotExist: the latter does not unwrap %w chains, so the
		// day LoadPolicy wraps its ReadFile error (house style tells it to), the
		// absent-file branch would silently die and every install would regress to
		// "ledger unavailable". The fallback's whole contract hangs on this check.
		return err
	}
	var pol *engine.Policy
	if supplement.Available {
		pol = supplement.Policy
	} else {
		if pol, err = rulebook.Load(); err != nil {
			return err
		}
	}
	l, err := engine.NewLedger(ledgerPath, dets, pol)
	if err != nil {
		return err
	}
	liveLedger, enginePolicy = l, pol
	return nil
}

// daemonPolicyPath is the daemon's own compatibility policy file (--policy, the
// daemon's $CG_POLICY, <data>/policy-engine.json). Only the dev ledger reads it.
var daemonPolicyPath string

// GET /api/policy/coverage — the compiled honest label per rule
// (P-COMPILE-2): boundary = detection-coverage ∧ enforcement-reach, computed
// mechanically. It labels the user rulebook as loaded now — the set the stateful tier
// reloads on every decision and the hook's standalone tier loads — classified with the
// governor's detectors, at the stateful tier's reach on this host (stateful-tier reach
// plan D-5). Team layers are per checkout and the invocation file per hook
// environment; neither is visible here, so the daemon's own compatibility file is
// reported, never labeled.
func handlePolicyCoverage(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{
		"note": "the user rulebook only: team layers are per checkout — `crossing-guard coverage` in a checkout labels them. " +
			"The hook's engine tier loads this rulebook only when a hook's own invocation file exists with no rules or is the rulebook itself; " +
			"the daemon cannot see that file, so a rule only that tier could fire is UNVERIFIED here.",
	}
	if daemonPolicyPath != "" {
		inv, err := rulebook.LoadInvocationPolicyPath(daemonPolicyPath, "daemon-invocation-or-compatibility")
		if err != nil || inv.Available {
			c := map[string]any{"path": daemonPolicyPath, "labeled": false,
				"note": "read by the dev ledger only; a hook loads the invocation file its own environment resolves, which the daemon cannot see"}
			if err != nil {
				c["error"] = err.Error()
			} else {
				c["origin"] = inv.Origin
			}
			out["compatibility"] = c
		}
	}
	doc, err := rulebook.LoadDocument()
	if err != nil {
		out["engine"], out["error"] = nil, "rulebook: "+err.Error()
		writeJSON(w, out)
		return
	}
	out["rulebook"] = map[string]any{"path": doc.Path, "origin": doc.Origin, "digest": doc.Digest}
	pol := doc.Policy
	if pol == nil {
		pol = &engine.Policy{}
	}
	// Model-claim producers come from this daemon's own store. Without a governor no
	// stateful tier runs, so state terms are labeled at its "not running" reach.
	// HookEngine stays EngineUnknown: whether a hook's engine tier loads this rulebook is
	// decided by that hook's invocation file, which the daemon cannot see.
	set := engine.LiveRuleSet{Kind: engine.SetUser, Tier: engine.StatefulDown}
	var dets []engine.Detector
	sp := store.StateProducersFor(nil, 0)
	switch {
	case governor != nil:
		dets, sp = governor.dets, store.StateProducersFor(governor.ix, time.Now().Unix())
		set.Tier = engine.StatefulUnarmed
		if governor.platform.StatefulEnforcementReady() {
			set.Tier = engine.StatefulArmed
		}
	case policyDetectorRuntime.governor != nil:
		dets = policyDetectorRuntime.governor.Detectors
		sp.AgentClaimsNote = "the daemon's governance store is not open"
	default:
		// Never label with an empty detector set: every detector-backed rule would read
		// a false INERT.
		out["engine"], out["error"] = nil, "no governor detector set resolved"
		writeJSON(w, out)
		return
	}
	out["engine"] = engine.CompileLiveBoundaries(pol, dets, sp, set)
	writeJSON(w, out)
}

func ledgerReady(w http.ResponseWriter) bool {
	if liveLedger == nil {
		http.Error(w, "live ledger not configured (no detectors/policy) — "+
			"harvest + policy still available", http.StatusServiceUnavailable)
		return false
	}
	return true
}

// POST /api/ledger/observe — the hook feed: classify an event, append to the chain,
// decide over the whole SESSION. Returns the Observation (tags, session tags, water
// mark, session-scoped decision).
func handleLedgerObserve(w http.ResponseWriter, r *http.Request) {
	if !ledgerReady(w) {
		return
	}
	var in struct {
		Session     string `json:"session"`
		Tool        string `json:"tool"`
		Path        string `json:"path"`
		Destination string `json:"destination"`
		Text        string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if in.Session == "" {
		in.Session = "unknown"
	}
	obs := liveLedger.Observe(in.Session, engine.Event{Tool: in.Tool, Path: in.Path,
		Destination: in.Destination, Text: in.Text})
	writeJSON(w, obs)
}

// GET /api/ledger/verify?session= — walk the on-disk chain vs the HELD anchor.
func handleLedgerVerify(w http.ResponseWriter, r *http.Request) {
	if !ledgerReady(w) {
		return
	}
	ok, detail := liveLedger.Verify(r.URL.Query().Get("session"))
	code := 200
	if !ok {
		code = 409
	}
	w.WriteHeader(code)
	writeJSON(w, map[string]any{"verified": ok, "detail": detail})
}

// GET /api/ledger/status — sessions held + the honest ceiling (for the UI banner).
func handleLedgerStatus(w http.ResponseWriter, r *http.Request) {
	if liveLedger == nil {
		writeJSON(w, map[string]any{"configured": false,
			"note": "live governance not configured; harvest + policy available"})
		return
	}
	writeJSON(w, map[string]any{"configured": true,
		"sessions_held":  liveLedger.SessionsHeld(),
		"anchor":         "HELD in daemon memory (out of same-user file reach)",
		"honest_ceiling": "tamper-EVIDENT vs in-session agent; not tamper-proof"})
}
