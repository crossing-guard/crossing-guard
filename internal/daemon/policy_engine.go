package daemon

// Policy v1a — the human-editable, testable policy surface over the REAL
// enforcement evaluator.
//
// One-evaluator rule (Q3), kept by IMPORT since M4 slice C (ADR 0018): every
// check and every validation calls the same guardcli code the hook runs —
// in-process, no subprocess, no output parsing — so console answers and
// enforcement can't diverge.
//
// Honest limits of v1a (rendered in the UI): writes are direct-to-file with
// engine-side validation; the loosen-confirm dialog is v1b; labels are
// coarse config-grade stubs (best-effort, never failclosed).

import (
	"bufio"
	"crossing-guard/engine"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"crossing-guard/internal/guardcli"
	"crossing-guard/internal/rulebook"
)

func handlePolicyRulesGet(w http.ResponseWriter, r *http.Request) {
	loaded, err := rulebook.LoadDocument()
	if err != nil {
		http.Error(w, "active rulebook unreadable: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"path": loaded.Path, "doc": json.RawMessage(loaded.Raw), "origin": loaded.Origin,
		"selection": loaded.Selection, "digest": loaded.Digest, "active": loaded.Active,
		"state_token":              loaded.StateToken,
		"available_starter_digest": loaded.AvailableStarterDigest, "selected": loaded.Selected,
		"selector": loaded.Selector, "selected_at": loaded.SelectedAt,
		"selected_source": loaded.SelectedSource, "selected_source_ref": loaded.SelectedSourceRef,
		"compatibility": loaded.Compatibility, "displaced_selection": loaded.Displaced,
		"displaced_selection_error": loaded.DisplacedError,
		"install_cohort":            loaded.InstallCohort,
		"label":                     "best-effort [config] — regex guards on shell commands; hook coverage not canary-probed",
	})
}

// handleRetiredPolicyGrid keeps the former overlapping endpoint explicit during the
// compatibility window. The destination grid never compiled to enforcement rules.
func handleRetiredPolicyGrid(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "destination policy grid retired; use /api/v1/policy/rules for active enforcement rules", http.StatusGone)
}

// handlePolicyRulesPut writes the rules file AFTER the engine validates the
// candidate (parse + guard-regex compile — the same code that loads rules at
// hook time). The daemon does not judge validity itself.
func handlePolicyRulesPut(w http.ResponseWriter, r *http.Request) {
	var doc json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&doc); err != nil {
		http.Error(w, "body must be the rules JSON document: "+err.Error(), http.StatusBadRequest)
		return
	}
	pretty := prettyJSON(doc)
	result, err := rulebook.SaveExpected(pretty, strings.Trim(r.Header.Get("If-Match"), `"`), "console")
	if err != nil {
		var invalid *rulebook.InvalidDocumentError
		var conflict *rulebook.ConflictError
		var invocation *rulebook.InvocationMutationError
		if errors.As(err, &invalid) {
			http.Error(w, "engine rejected the rules: "+err.Error(), http.StatusUnprocessableEntity)
			return
		}
		if errors.As(err, &conflict) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if errors.As(err, &invocation) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp := map[string]any{"saved": true, "path": result.Document.Path,
		"digest": result.Document.Digest, "selection": result.Document.Selection,
		"note": "engine-validated; loosen-confirm dialog is a v1b item — this write path trusts the human at the console"}
	if result.Backup != "" {
		resp["backup"] = result.Backup
	}
	writeJSON(w, resp)
}

func handlePolicySelectionPreview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source string `json:"source"`
		Path   string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	preview, err := rulebook.PreviewSelection(req.Source, req.Path)
	if err != nil {
		var invalid *rulebook.InvalidDocumentError
		if errors.As(err, &invalid) {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, preview)
}

func handlePolicySelection(w http.ResponseWriter, r *http.Request) {
	var req rulebook.SelectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.Selector = "console"
	result, err := rulebook.Select(req)
	if err != nil {
		var conflict *rulebook.ConflictError
		var invalid *rulebook.InvalidDocumentError
		var invocation *rulebook.InvocationMutationError
		switch {
		case errors.As(err, &conflict):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.As(err, &invalid):
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		case errors.As(err, &invocation):
			http.Error(w, err.Error(), http.StatusConflict)
		default:
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
		return
	}
	writeJSON(w, result)
}

func handlePolicySelectionRollback(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ExpectedStateToken string `json:"expected_state_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	active, archive, err := rulebook.RollbackLegacy(req.ExpectedStateToken, "console")
	if err != nil {
		var conflict *rulebook.ConflictError
		if errors.As(err, &conflict) {
			http.Error(w, err.Error(), http.StatusConflict)
		} else {
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
		return
	}
	writeJSON(w, map[string]any{"active": active, "archive": archive})
}

// governorDetectors is the detector set a daemon-side dry run classifies with: the
// governor's own, never the hook's install profile (which a scratch daemon must not
// read or create). nil when no governor is configured: the dry run then decides over
// the invocation's own facts and counts the rest as undecided.
func governorDetectors() []engine.Detector {
	if governor == nil {
		return nil
	}
	return governor.dets
}

func handlePolicyCheck(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Command string `json:"command"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Command) == "" {
		http.Error(w, "command required", http.StatusBadRequest)
		return
	}
	v, unjudged, err := guardcli.CheckCommandStatic(req.Command, governorDetectors())
	if err != nil {
		http.Error(w, "engine error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	res := map[string]any{"decision": v.Decision,
		// provenance: WHICH evaluator answered — the imported hook code (ADR 0025:
		// one evaluator, compiled into both the hook and the daemon).
		"evaluator": "in-process guardcli (the same engine evaluator the hook runs)"}
	if v.Rule != "" {
		res["rule"] = v.Rule
	}
	// A dry run has no session state, so it cannot judge a rule over session:/target:/
	// agent: facts — the daemon's stateful tier decides those at run time. Say how many
	// deny/ask ones there are, rather than let an ALLOW read as the whole answer.
	if unjudged.StateRules > 0 {
		res["state_rules_not_evaluated"] = unjudged.StateRules
	}
	// A command preview names no tool, path or destination: a deny/ask rule that reads
	// one could not be decided here. It did not fire, and the live call may differ.
	if unjudged.Undecided > 0 {
		res["rules_undecided"] = unjudged.Undecided
	}
	// raw keeps the CLI line shape the UI already renders
	switch v.Decision {
	case "allow":
		res["raw"] = fmt.Sprintf("allow    (no rule matched)   %q", req.Command)
	case "ask":
		res["raw"] = fmt.Sprintf("ASK      %s   %q", v.Rule, req.Command)
	case "deny":
		res["raw"] = fmt.Sprintf("DENY     %s   %q", v.Rule, req.Command)
	}
	writeJSON(w, res)
}

// handlePolicyDecisions tails the engine's decision log (JSONL). Location
// per engine today: /tmp/cp.log (relocation to ~/.crossing-guard/policy/ is
// an accepted ask — read both, prefer the new home once it exists).
func handlePolicyDecisions(w http.ResponseWriter, r *http.Request) {
	paths := []string{
		filepath.Join(homeDir(), ".crossing-guard", "policy", "decisions.jsonl"),
		"/tmp/cp.log",
	}
	var src string
	var lines []map[string]any
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		src = p
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			var obj map[string]any
			if json.Unmarshal(sc.Bytes(), &obj) == nil {
				lines = append(lines, obj)
			}
		}
		f.Close()
		break
	}
	// newest first, cap 200
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	if len(lines) > 200 {
		lines = lines[:200]
	}
	if lines == nil {
		lines = []map[string]any{}
	}
	writeJSON(w, map[string]any{"source": src, "decisions": lines,
		"note": "engine log; /tmp does not survive reboot — relocation is an accepted engine ask"})
}

func prettyJSON(raw json.RawMessage) []byte {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return raw
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return raw
	}
	return append(b, '\n')
}
