package daemon

// memory_synthesis.go — the daemon's bounded session-synthesis step
// (daemon-synthesis-v1 plan rev 2): at a session's end, IF the owner enabled
// it AND the session contains a memory-shaped signal, draft at most ONE
// pending lesson candidate through the one write owner. Deterministic: the
// body is built from recorded facts only, never generated. Propose-only:
// promote stays a human act. Skip-on-error, never auto-retried (RT-C9).
//
// The trigger reuses the audit package's own classification over the
// session's bounded event read (DS-RT1) — no second classifier exists here.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"crossing-guard/engine"
	"crossing-guard/harvest"
	"crossing-guard/memory"
	"crossing-guard/store"
)

// synthesisState is the doctor-visible counters + the UTC-day budget, persisted
// beside the store (the memory-import-state pattern). All fields are advisory
// output; the budget's correctness lives in this process's gate plus the file.
type synthesisState struct {
	Day              string `json:"day"` // UTC date the counters belong to
	Attempted        int    `json:"attempted"`
	Written          int    `json:"written"`
	SkippedNoTrigger int    `json:"skipped_no_trigger"`
	Errors           int    `json:"errors"`
	UpdatedAt        string `json:"updated_at"`
}

var (
	synthesisMu    sync.Mutex
	synthesisCount = synthesisState{}
)

// requestMemorySynthesis is the session-end lane's entry point: gated,
// budgeted, fire-and-forget. Errors inside the step are counted, never
// retried and never block the lane.
func requestMemorySynthesis(runtime, sessionID string) {
	config, _ := memoryConfig()
	if !config.Synthesis.Enabled {
		return
	}
	go func() {
		if err := runMemorySynthesis(runtime, sessionID, config); err != nil {
			log.Printf("memory synthesis: skipped (%s/%s): %v", runtime, shortSessionRef(sessionID), err)
		}
	}()
}

// runMemorySynthesis does the bounded work: budget check → trigger → draft →
// one write-owner call → counters. It returns an error only for the log line;
// the caller does nothing with it (RT-C9: counted, not retried).
func runMemorySynthesis(runtime, sessionID string, config MemoryConfig) error {
	synthesisMu.Lock()
	day := time.Now().UTC().Format("2006-01-02")
	if synthesisCount.Day != day {
		synthesisCount = synthesisState{Day: day} // DS-RT4: the UTC day rolls the budget
	}
	if synthesisCount.Written >= config.Synthesis.MaxPerDay {
		synthesisMu.Unlock()
		recordSynthesisOutcome(synthesisState{})
		return fmt.Errorf("daily budget reached (%d)", config.Synthesis.MaxPerDay)
	}
	synthesisCount.Attempted++
	synthesisMu.Unlock()
	defer func() {
		synthesisMu.Lock()
		recordSynthesisOutcomeLocked()
		synthesisMu.Unlock()
	}()

	// The deterministic id (DS-RT3): bounded, opaque, idempotent.
	id := synthesisID(runtime, sessionID)

	ix, err := store.OpenRO(indexPath())
	if err != nil {
		synthesisCount.Errors++
		return err
	}
	if _, err := ix.MemoryByID(id); err == nil {
		ix.Close()
		return nil // already synthesized (replay / second end event)
	}
	ix.Close()

	detail, err := LoadSession(runtime, sessionID)
	if err != nil || detail == nil {
		synthesisCount.Errors++
		return fmt.Errorf("session load: %v", err)
	}

	trigger, evidence := synthesisTrigger(detail)
	if !trigger {
		synthesisCount.SkippedNoTrigger++
		return nil
	}

	rec, sources := buildSynthesisProposal(id, detail, evidence)
	ix, err = store.Open(indexPath())
	if err != nil {
		synthesisCount.Errors++
		return err
	}
	defer ix.Close()
	actor := store.MemoryActor{AuthorType: "daemon", AuthorID: "synthesis", ActorSource: "daemon"}
	saved, err := ix.UpsertMemory(rec, sources, nil, actor)
	if err != nil {
		synthesisCount.Errors++
		return err
	}
	mirrorStoreRecord(saved)
	synthesisMu.Lock()
	synthesisCount.Written++
	synthesisMu.Unlock()
	return nil
}

// synthesisTrigger answers whether this session looks like it taught
// something: two or more failure-shaped signals, counted with the audit
// package's own classification (classifyTranscriptEvent) over the bounded
// event read. A signal is a tag with key `error` or `data-class=error`, and NO
// shipped detector emits either, so with the shipped library this never fires
// (audit-memory-write-fact plan RT-1; the owner decision is tracked
// separately). The memory-write branch read the session summary's
// memory-write count, which lost its only writer on 2026-07-20; it was
// retired. The evidence line names the trigger for the reviewer.
func synthesisTrigger(detail *SessionDetail) (bool, string) {
	failureSignals := 0
	for _, ev := range detail.Events {
		if ev.Kind != "tool_call" && ev.Kind != "tool_result" {
			continue
		}
		for _, t := range classifyTranscriptEvent(ev) {
			if t.Key == "error" || (t.Key == "data-class" && t.Value == "error") {
				failureSignals++
			}
		}
	}
	if failureSignals >= 2 {
		return true, fmt.Sprintf("%d failure-shaped signals", failureSignals)
	}
	return false, ""
}

// buildSynthesisProposal assembles the one deterministic lesson candidate.
// Facts only: objective, what changed (repo-relative), commands run, the
// trigger evidence — redacted before the write.
func buildSynthesisProposal(id string, detail *SessionDetail, evidence string) (store.MemoryRecord, []store.MemorySource) {
	ref := detail.Runtime + "/" + detail.ID
	repo := repositoryLabel(detail)
	files := map[string]bool{}
	commands := []string{}
	objective := ""
	for _, ev := range detail.Events {
		switch {
		case ev.Kind == "user" && objective == "":
			objective = firstLine(ev.Text, 120)
		case ev.Kind == "tool_call":
			if name, args, ok := toolCallParts(ev); ok && len(commands) < 8 {
				// DS-RT5 applies here too: the body carries the extracted
				// command (already redacted later by the secret pass) — never
				// the raw payload, which embeds absolute file_path values.
				cmd, _, _ := extractToolInput(args)
				commands = append(commands, name+" "+firstLine(cmd, 80))
			}
			if p := repoRelativePath(ev.Text, repo); p != "" && len(files) < 12 {
				files[p] = true
			}
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Objective: %s\n\n", orDash(objective))
	if len(files) > 0 {
		sorted := make([]string, 0, len(files))
		for f := range files {
			sorted = append(sorted, f)
		}
		sort.Strings(sorted)
		b.WriteString("Touched (repo-relative, capped):\n")
		for _, f := range sorted {
			fmt.Fprintf(&b, "- %s\n", f)
		}
		b.WriteString("\n")
	}
	if len(commands) > 0 {
		b.WriteString("Commands run (capped):\n")
		for _, c := range commands {
			fmt.Fprintf(&b, "- %s\n", c)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Trigger: %s.\n", evidence)
	fmt.Fprintf(&b, "Deterministic draft: promote only if this session taught a durable lesson; edit freely before promoting.\n")

	// The redaction pass (plan §2): the four secret patterns, before the write.
	dets, _ := engine.LoadLayered("")
	body, _ := engine.RedactText(b.String(), dets)
	title, _ := engine.RedactText(firstLine(detail.Title, 80)+" — lesson candidate", dets)

	rec := store.MemoryRecord{
		ID: id, Title: title, Category: "how-to", Body: body,
		Tags:   []string{"synthesized"},
		Source: "agent", Status: "pending",
		AuthorType: "daemon", AuthorID: "synthesis",
		Origin: "synthesized from session " + ref,
	}
	if repo != "" {
		// Repository identity is minted from the session's folder (team item 5
		// decision 18): one origin remote keys the draft by it; otherwise it stays weak.
		rec.ScopeType = store.MemoryScopeRepository
		rec.ScopeID, rec.RepositoryIdentity, rec.IdentityNote = mintMemoryScope(detail.Cwd, "")
	} else {
		rec.ScopeType = store.MemoryScopeUser
	}
	sources := []store.MemorySource{
		{Vendor: detail.Runtime, SessionID: detail.ID, AnchorKind: "none",
			CapturedAt: time.Now().UnixNano()},
	}
	return rec, sources
}

// synthesisID is the bounded opaque deterministic id (DS-RT3): the sha256's
// first 8 hex chars keep it inside the slug grammar without leaking the
// session's uuid into a rendered id.
func synthesisID(runtime, sessionID string) string {
	sum := sha256.Sum256([]byte(runtime + "/" + sessionID))
	return "syn-" + runtime + "-" + hex.EncodeToString(sum[:4])
}

// repositoryLabel derives the memory-scope label the way the store's records
// do: the cwd's basename. Sessions without a recorded cwd are user-scoped.
func repositoryLabel(detail *SessionDetail) string {
	if detail.Cwd == "" {
		return ""
	}
	return filepath.Base(strings.TrimSuffix(detail.Cwd, "/"))
}

// repoRelativePath extracts a quoted file_path from a tool-call payload and
// renders it repo-relative (DS-RT5): under the session's cwd, the relative
// path; outside it, the literal "{outside repository}" with no path.
func repoRelativePath(text, repo string) string {
	const pathKey = `"file_path"`
	i := strings.Index(text, pathKey)
	if i < 0 {
		return ""
	}
	rest := text[i+len(pathKey):]
	start := strings.Index(rest, `"`)
	if start < 0 {
		return ""
	}
	rest = rest[start+1:]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return ""
	}
	p := rest[:end]
	if !strings.HasPrefix(p, "/") {
		return p // already relative
	}
	if idx := strings.Index(p, "/"+repo+"/"); repo != "" && idx >= 0 {
		return p[idx+len(repo)+2:] // inside the checkout: repo-relative, root stripped
	}
	return "{outside repository}"
}

// toolCallParts splits a tool_call event into its bare name and argument text.
func toolCallParts(ev harvest.CanonicalEvent) (string, string, bool) {
	if ev.Name == "" {
		return "", "", false
	}
	return engine.BareTool(ev.Name), ev.Text, true
}

func shortSessionRef(id string) string {
	if len(id) > 12 {
		return id[:12] + "…"
	}
	return id
}

func firstLine(text string, max int) string {
	text = strings.TrimSpace(text)
	if i := strings.IndexAny(text, "\n\r"); i >= 0 {
		text = text[:i]
	}
	if len(text) > max {
		return text[:max] + "…"
	}
	return text
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none recorded)"
	}
	return s
}

// SynthesisState is the exported counters shape doctor reads; the same file
// the step persists.
type SynthesisState = synthesisState

// ReadSynthesisState loads the persisted counters for doctor; absent file
// means the step has never run (found=false), which doctor renders as off.
func ReadSynthesisState(dataDir string) (SynthesisState, bool, error) {
	raw, err := os.ReadFile(filepath.Join(dataDir, "memory-synthesis-state.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return SynthesisState{}, false, nil
		}
		return SynthesisState{}, false, err
	}
	var state SynthesisState
	if err := json.Unmarshal(raw, &state); err != nil {
		return SynthesisState{}, false, err
	}
	return state, true, nil
}

// recordSynthesisOutcome persists the counters (the import-state pattern);
// a failure to persist is logged, never fatal.
func recordSynthesisOutcome(s synthesisState) {
	synthesisMu.Lock()
	defer synthesisMu.Unlock()
	recordSynthesisOutcomeLocked()
}

func recordSynthesisOutcomeLocked() {
	state := synthesisCount
	state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	path := filepath.Join(filepath.Dir(indexPath()), "memory-synthesis-state.json")
	raw := []byte(mustJSON(state))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// mirrorStoreRecord writes one store record into the mirror (the write-through
// contract every write-owner caller keeps: synthesis drafts, pulled team records);
// best-effort, the store is truth. It never commits to git (team item 5 decision 5).
func mirrorStoreRecord(r store.MemoryRecord) {
	dir := memory.DefaultDir()
	memory.MirrorEnsureReadme(dir)
	mr := memory.RecordFromStore(r.ID, r.Status, string(r.ScopeType), r.ScopeID,
		r.Title, r.Category, r.Body, r.Tags, r.Aliases, r.Source, r.Origin,
		r.SupersededBy, r.VerifiedAt, r.VerifiedBy,
		time.Unix(0, r.CreatedAt).UTC().Format(time.RFC3339),
		time.Unix(0, r.UpdatedAt).UTC().Format(time.RFC3339))
	_ = memory.MirrorWrite(dir, mr)
}
