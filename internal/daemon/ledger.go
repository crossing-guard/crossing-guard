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

	"crossing-guard/engine"
	"crossing-guard/internal/rulebook"
)

var (
	liveLedger      *engine.Ledger // nil until initLedger succeeds (config present)
	engineDetectors []engine.Detector
	enginePolicy    *engine.Policy
)

// initLedger loads detectors + policy and opens the daemon-owned ledger. Non-fatal:
// if config is absent the daemon still serves harvest/memory/policy; the ledger routes
// report 503 so the UI can render "live governance not configured" honestly.
func initLedger(ledgerPath string, dets []engine.Detector, polPath string) error {
	// ADR 0025's convergence, applied to the file layout rather than the format:
	// policy-engine.json and policy/rules.json are the SAME format, so a separate
	// engine-ledger policy is a second copy of the rules you enforce — free to drift,
	// and it did (the compiled coverage report described a file the hook never read).
	// Absent, we fall back to the ACTIVE enforcement rules, which is what the
	// coverage report should have been describing all along: what you actually
	// enforce. A present file still wins, so an existing deploy is unchanged.
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
	liveLedger, engineDetectors, enginePolicy = l, dets, pol
	return nil
}

// GET /api/policy/coverage — the compiled honest label per rule
// (P-COMPILE-2): boundary = detection-coverage ∧ enforcement-reach, computed
// mechanically. The console renders FROM this — no shield where there's a
// watch (gui-design §6).
func handlePolicyCoverage(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{
		"legacy": map[string]any{
			"rules": "rules.json guards",
			"label": "best-effort [config] — regex guards on shell-command text; coverage not canary-probed; a hard guarantee needs a non-bypassable backstop",
		},
	}
	if enginePolicy == nil {
		out["engine"] = nil
		out["note"] = "engine policy not configured — only the legacy regex layer is active"
		writeJSON(w, out)
		return
	}
	out["engine"] = engine.CompileBoundaries(enginePolicy, engineDetectors, engine.ReachStop)
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
