package memcli

// adapter.go — cpmem's session surface over the shared harvest package
// (crossing-guard/harvest; code-organization-v1 M2 slice 2). This replaces
// the duplicate parser that lived in cpmem/harvest.go: vendor-file parsing
// now happens in exactly one place. Behavior notes vs the old copy
// (labeled, intended):
//   - codex tool_call/tool_result events are now indexed (the old codex
//     parser surfaced only user/agent messages)
//   - claude titles may come from the vendor summary line (the shared pass
//     prefers it); injected "<...>" context still never makes a title
//   - codex session ids remain the bare thread uuid when session_meta
//     carries one, falling back to the rollout filename stem

import (
	"fmt"
	"os"
	"strings"
	"time"

	"crossing-guard/harvest"
)

type Event struct {
	Vendor  string // claude | codex
	Session string
	TS      string
	Kind    string // user | assistant | tool_call | tool_result
	Text    string
}

type Session struct {
	Vendor   string
	ID       string
	Path     string
	CWD      string
	Title    string // codex thread_name or first user prompt
	Modified time.Time
	Turns    int // user-message count
	Summary  harvest.SessionSummary
}

func sessionFromSummary(s harvest.SessionSummary) Session {
	return Session{
		Vendor: s.Runtime, ID: harvest.CanonicalID(s), Path: s.Path, CWD: s.Cwd,
		Title: firstLine(s.Title, 80), Modified: s.Modified, Turns: s.UserTurns,
		Summary: s,
	}
}

func ListSessions(vendor string) []Session {
	var out []Session
	for _, s := range harvest.ScanSessions() { // newest first
		if vendor != "" && s.Runtime != vendor {
			continue
		}
		out = append(out, sessionFromSummary(s))
	}
	return out
}

// SessionEvents maps the canonical event stream onto cpmem's index surface:
// user/assistant/tool events only, tool text compacted. Display callers tolerate the
// empty result on error. Projection indexing uses internal/transcriptindex instead.
func SessionEvents(s Session) []Event {
	events, err := sessionEvents(s)
	if err != nil {
		return nil
	}
	return events
}

func sessionEvents(s Session) ([]Event, error) {
	events, _, _, err := harvest.NormalizeSummary(s.Summary, false)
	if err != nil {
		return nil, err
	}
	var out []Event
	for _, ev := range events {
		text := ev.Text
		switch ev.Kind {
		case "tool_call":
			text = strings.TrimSpace(ev.Name + " " + truncate(ev.Text, 200))
		case "tool_result":
			text = truncate(ev.Text, 200)
		case "user", "assistant":
		default:
			continue // thinking/system/summary/other are not indexed here
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		out = append(out, Event{Vendor: s.Vendor, Session: s.ID, TS: ev.Ts, Kind: ev.Kind, Text: text})
	}
	return out, nil
}

// ---------- helpers (used across cpmem) ----------

func home() string {
	h, _ := os.UserHomeDir()
	return h
}

func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncate(s, max)
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// Adapter is one vendor's memory-hook integration: install the SessionStart/Stop
// hooks and recognize the vendor's injected-context line (the doctor canary).
// Implementations live in adapter_<vendor>.go and self-register via init(), so
// the attach command and the doctor never name a vendor (ADR 0020).
type Adapter interface {
	Name() string
	ConfigFlag() string // the `attach <vendor>` config flag: "settings" | "config"
	DefaultConfigPath() string
	// Attach installs the SessionStart memory hook. wrote is false when an entry
	// for selfPath was already there: nothing was written and the entry is not
	// this call's.
	Attach(configPath, selfPath string) (wrote bool, err error)
	// Detach removes exactly the entry an Attach for selfPath wrote and nothing
	// else. removed is false when no such entry is there.
	Detach(configPath, selfPath string) (removed bool, err error)
	InjectedText(line []byte) (string, bool)

	// MemoryHookBinary returns the binary path of the installed SessionStart memory
	// hook, or "" when none is installed. Without it, doctor could only count
	// verified injections, so a vendor that was NEVER ATTACHED and one whose hook is
	// broken produced the identical alarming line — "memory is not reaching this
	// agent" — for a machine where nothing had ever been asked to reach it. The path
	// (not a bool) is what separates "not attached" from "attached to a binary that
	// no longer exists", which is the silent-stop failure on this surface too.
	MemoryHookBinary(configPath string) string
}

// MemoryIndexOutput is an optional memory adapter capability. Native envelopes
// and recognition of old, unattributed hook payloads belong to the adapter.
type MemoryIndexOutput interface {
	EncodeMemoryIndex(block string) ([]byte, error)
	MatchesLegacyMemoryHook(payload []byte) bool
}

func memoryIndexOutput(runtime string, explicit bool, payload []byte) (MemoryIndexOutput, error) {
	if explicit {
		a := adapters[runtime]
		if runtime == "" || a == nil {
			return nil, fmt.Errorf("memory index: unknown runtime %q", runtime)
		}
		encoder, ok := a.(MemoryIndexOutput)
		if !ok {
			return nil, fmt.Errorf("memory index: runtime %q has no memory output capability", runtime)
		}
		return encoder, nil
	}
	var selected MemoryIndexOutput
	for _, a := range adapters {
		encoder, ok := a.(MemoryIndexOutput)
		if !ok || !encoder.MatchesLegacyMemoryHook(payload) {
			continue
		}
		if selected != nil {
			return nil, fmt.Errorf("memory index: ambiguous legacy hook; specify --runtime")
		}
		selected = encoder
	}
	return selected, nil
}

// memoryHookBinaryFromCommand extracts the binary out of a `<path> memory index`
// hook command, the shape both adapters write. Returns "" for anything else.
// The path is NOT quoted by either writer, so everything before the verb is it.
//
// Anchored on the VERB, not the end of the string. The first version used
// HasSuffix(" memory index") — the exact end-anchoring regression the guard-hook
// parser documents (its match broke the moment `--runtime` was appended), rebuilt
// here the same afternoon it was fixed there. The first flag anyone appends to
// this command would have made doctor report every attached vendor as absent.
func memoryHookBinaryFromCommand(cmd string) string {
	fields := strings.Fields(cmd)
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == "memory" && fields[i+1] == "index" && i > 0 {
			return strings.Join(fields[:i], " ")
		}
	}
	return ""
}

var adapters = map[string]Adapter{}

func registerAdapter(a Adapter) { adapters[a.Name()] = a }

// injectedText dispatches to the vendor adapter; ("",false) for an unknown vendor.
func injectedText(vendor string, line []byte) (string, bool) {
	if a := adapters[vendor]; a != nil {
		return a.InjectedText(line)
	}
	return "", false
}
