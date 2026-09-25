package memcli

// attach.go — adapter install + the doctor canary.
//
// attach = write config + onboard trust + PROVE injection. Silent failure is
// the default failure mode of hook injection (probed 2026-07-14: Codex
// silently skips untrusted hooks; Codex desktop had silent regressions
// (#21639); Claude cloud runs project hooks only) — so `doctor` greps actual
// session files for the beacon the index emits, instead of trusting config.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"crossing-guard/harvest"
)

// both marks match: the current emission and the pre-rename one still
// present in existing session files (doctor checks recent sessions)
var beaconMarks = [][]byte{[]byte("crossing-guard-memory-beacon"), []byte("cp-memory-beacon")}

// ensureHookEntry adds a hook entry for event unless the command is already
// registered there. Returns true when it changed the map.
func ensureHookEntry(hooks map[string]any, event, matcher, cmd string) (bool, error) {
	rawEntries, present := hooks[event]
	entries, ok := rawEntries.([]any)
	if present && !ok {
		return false, fmt.Errorf("hooks.%s must be a JSON array; refusing to replace it", event)
	}
	for _, e := range entries {
		entry, _ := e.(map[string]any)
		inner, _ := entry["hooks"].([]any)
		for _, rawHook := range inner {
			hook, _ := rawHook.(map[string]any)
			if command, _ := hook["command"].(string); command == cmd {
				return false, nil
			}
		}
	}
	entry := map[string]any{
		"hooks": []any{map[string]any{"type": "command", "command": cmd}},
	}
	if matcher != "" {
		entry["matcher"] = matcher
	}
	hooks[event] = append(entries, entry)
	return true, nil
}

// ---------- doctor (v2: nonce-integrity, P-MEM-11 / ADR 0013 T2) ----------

// doctor answers one question per vendor: did an injection PROVABLY land in
// recent sessions? Proof = a nonce that (a) was minted+logged by a real
// `memory index` emission (recall-log) AND (b) appears in the runtime's
// injected-context POSITION — Claude: an `attachment` line with
// attachment.hookEvent=SessionStart (observed shape 2026-07-16); Codex: a
// developer-role response_item. A beacon merely QUOTED in conversation
// (assistant text, tool results) matches neither, killing the v1 false
// positive.

type VendorHealth struct {
	Vendor     string `json:"vendor"`
	Checked    int    `json:"checked"`
	Injected   int    `json:"injected"`
	Status     string `json:"status"`
	ConfigPath string `json:"config_path,omitempty"`
	// Attached separates "nothing is configured to inject memory here" from
	// "something is, and it is not working". Both used to render as the same
	// sentence — "memory is not reaching this agent" — which reads as a broken
	// system on a machine where the memory hook was simply never installed. That is
	// the same not-installed-vs-not-working conflation the guard surface had.
	Attached   bool   `json:"attached"`
	HookBinary string `json:"hook_binary,omitempty"`
	// No omitempty: false is the ALARMING value (attached to a binary that is
	// gone), and omitempty would drop the field in exactly that state — a JSON
	// consumer would see the alarm as absence.
	BinaryPresent bool `json:"binary_present"`
}

type DoctorReport struct {
	Store           string         `json:"store"`
	Records         int            `json:"records"`
	Pending         int            `json:"pending"`
	KnownNonces     int            `json:"known_nonces"`
	Vendors         []VendorHealth `json:"vendors"`
	EffectiveConfig map[string]any `json:"effective_config"`
}

func doctorReport(recent int) DoctorReport {
	dir := memoryDir()
	nonces := loggedNonces(dir)
	rep := DoctorReport{
		Store:       dir,
		Records:     len(loadRecords(dir)),
		Pending:     len(loadAllRecords(dir)) - len(loadRecords(dir)),
		KnownNonces: len(nonces),
		EffectiveConfig: map[string]any{ // GUI R8: front-ends never guess paths
			"store_dir":   dir,
			"index_db":    indexDB(),
			"index_built": indexExists(),
			"recall_log":  filepath.Join(dir, "recall-log.jsonl"),
		},
	}
	for _, vendor := range harvest.RuntimeNames() {
		sessions := ListSessions(vendor)
		if len(sessions) > recent {
			sessions = sessions[:recent]
		}
		h := VendorHealth{Vendor: vendor, Checked: len(sessions)}
		if a := adapters[vendor]; a != nil {
			h.ConfigPath = a.DefaultConfigPath()
			recorded := false
			if rec, found, err := readAttachmentRecord(vendor); err != nil {
				h.ConfigPath = ""
				h.Status = "BROKEN — " + err.Error()
			} else if found {
				h.ConfigPath = rec.ConfigPath
				recorded = true
			}
			if h.Status == "" {
				h.HookBinary = a.MemoryHookBinary(h.ConfigPath)
			}
			h.Attached = h.HookBinary != ""
			if h.Attached {
				_, err := os.Stat(h.HookBinary)
				h.BinaryPresent = err == nil
			} else if recorded {
				if _, err := os.Stat(h.ConfigPath); os.IsNotExist(err) {
					h.Status = "BROKEN — recorded memory config no longer exists: " + h.ConfigPath
				} else if err != nil {
					h.Status = "UNKNOWN — recorded memory config could not be verified: " + h.ConfigPath + " (" + err.Error() + ")"
				} else {
					h.Status = "BROKEN — recorded memory config contains no recognized injection hook: " + h.ConfigPath
				}
			}
		}
		for _, s := range sessions {
			if sessionHasInjectedNonce(s, nonces) {
				h.Injected++
			}
		}
		// The ladder, by evidence. Only the last two lines describe a FAULT; the
		// first describes a choice not yet made, and saying so is the difference
		// between a support surface and an alarm that cries wolf on a fresh install.
		//
		// Verified injection OUTRANKS the config read. The config is checked at the
		// vendor's DEFAULT path, but attach accepts a custom one — and a nonce in
		// the hook position is proof injection is happening wherever the hook
		// actually lives. Without this rung, a custom-path attach printed the
		// self-contradiction "5/5 verified → not attached" and told the user to
		// attach what was provably injecting.
		switch {
		case h.Status != "":
			// Attachment evidence failures outrank session inference. A stale or
			// corrupt record must be repaired, not hidden by an old successful nonce.
		case h.Injected > 0 && !h.Attached:
			h.Attached = true // it demonstrably is; only the default config denies it
			h.Status = "OK — verified injection (" + strconv.Itoa(h.Injected) + "/" +
				strconv.Itoa(h.Checked) + "), via a hook at a NON-DEFAULT config path"
		case !h.Attached:
			h.Status = "not attached — nothing is configured to inject memory here. " +
				"Attach with: crossing-guard attach " + vendor
		case !h.BinaryPresent:
			// binaryGone distinguishes "confirmed absent" from "could not check"
			// (EPERM etc.) — "no longer exists" is a factual claim and must not be
			// made off an unreadable directory.
			h.Status = "BROKEN — its hook runs " + h.HookBinary + ", which " + binaryGone(h.HookBinary) +
				". Re-attach with: crossing-guard attach " + vendor
		case h.Checked == 0:
			h.Status = "attached; no sessions found to verify against"
		case h.Injected == h.Checked:
			h.Status = "OK — verified injection (logged nonce in hook position)"
		case h.Injected > 0:
			h.Status = "PARTIAL — some sessions missing injection (trust gate? hook config?)"
		default:
			h.Status = "ATTACHED but NOT INJECTED — the hook is installed and no recent " +
				"session shows the beacon"
		}
		rep.Vendors = append(rep.Vendors, h)
	}
	return rep
}

// MemoryDoctor is the memory-injection half of the whole-system doctor. It stays
// here because the beacon/nonce logic is memcli's; the top-level doctor renders it
// as one section beside enforcement, capture and service health, so "is memory
// reaching my agents" is answered in the same breath as "am I governed".
func MemoryDoctor(recent int) DoctorReport { return doctorReport(recent) }

// binaryGone words the stat failure honestly: absent is a fact, anything else is
// only "could not be verified".
func binaryGone(path string) string {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return "no longer exists"
	} else if err != nil {
		return "could not be verified (" + err.Error() + ")"
	}
	return "no longer exists" // unreachable from the ladder; kept total
}

// loggedNonces returns nonces minted by real index emissions (last 1000 rows).
func loggedNonces(dir string) map[string]bool {
	nonces := map[string]bool{}
	raw, err := os.ReadFile(filepath.Join(dir, "recall-log.jsonl"))
	if err != nil {
		return nonces
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var row struct {
			Op    string `json:"op"`
			Nonce string `json:"nonce"`
		}
		if json.Unmarshal([]byte(line), &row) == nil && row.Op == "index" && row.Nonce != "" {
			nonces[row.Nonce] = true
		}
	}
	return nonces
}

func sessionHasInjectedNonce(s Session, nonces map[string]bool) bool {
	if len(nonces) == 0 {
		return false
	}
	f, err := os.Open(s.Path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 8*1024*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		marked := false
		for _, mark := range beaconMarks {
			if bytes.Contains(line, mark) {
				marked = true
				break
			}
		}
		if !marked {
			continue
		}
		text, position := injectedText(s.Vendor, line)
		if !position {
			continue // beacon quoted somewhere else — the old false positive
		}
		for n := range nonces {
			if strings.Contains(text, n) {
				return true
			}
		}
	}
	return false
}

// ---------- recall log review ----------

func showRecallLog(limit int) {
	path := filepath.Join(memoryDir(), "recall-log.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Println("no recall log yet:", path)
		return
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	for _, l := range lines {
		fmt.Println(l)
	}
}
