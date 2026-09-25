package daemon

// Store: JSON-file persistence for memory records and notes. The real product uses
// SQLite (ADR 0003); a probe uses files.
// Memory records follow a SIMPLIFIED cut of schemas/memory.schema.json —
// enough to exercise the review/manage UI, not schema-complete.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"crossing-guard/memory"
)

type Memory struct {
	ID             string   `json:"id"`
	Claim          string   `json:"claim"`
	Status         string   `json:"status"`         // draft|verified|stale|disputed|superseded|expired
	Classification string   `json:"classification"` // observed|inferred|user-asserted|verified
	Scope          string   `json:"scope"`          // free-form in the PoC (repo path / "global")
	Sources        []string `json:"sources"`        // session ids / URLs / "user"
	CreatedAt      string   `json:"created_at"`
	UpdatedAt      string   `json:"updated_at"`
	Store          string   `json:"store"`          // "probe" (writable JSON) | "crossing-guard" (cpmem dossiers, read-only here)
	Body           string   `json:"body,omitempty"` // dossier body (crossing-guard records)
	Grade          string   `json:"grade"`          // evidence grade: "pkg" (engine package import) | "fs" (console's own JSON)
	Aliases        []string `json:"aliases,omitempty"`
	Tags           []string `json:"tags,omitempty"`
	Verified       string   `json:"verified,omitempty"` // dated attestation "2026-07-16 by alice" — render as AGE, never a bare check
	Pending        bool     `json:"pending,omitempty"`
}

type Note struct {
	ID        string `json:"id"`
	Target    string `json:"target"` // "runtime/session-id" or "runtime/session-id#seq"
	Text      string `json:"text"`
	CreatedAt string `json:"created_at"`
}

type Store struct {
	dir string
	mu  sync.Mutex
}

func NewStore(dir string) *Store { return &Store{dir: dir} }

func (s *Store) path(name string) string { return filepath.Join(s.dir, name) }

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// --- memory ---

func (s *Store) ListMemories() []Memory {
	s.mu.Lock()
	var out []Memory
	_ = readJSON(s.path("memories.json"), &out)
	s.mu.Unlock()
	for i := range out {
		out[i].Store = "probe"
	}
	// Merge the REAL store via the memory package — the v1c import landed
	// (ADR 0018: import, never shell-out; the cpmem-subprocess contract this
	// replaced also read the retired v1.0 `verified` field, so verified
	// chips were silently empty — fixed here by mapping the v1.1 split).
	out = append(out, readControlPlaneMemories(false)...)
	if out == nil {
		out = []Memory{}
	}
	return out
}

// readControlPlaneMemories maps the engine store onto the console vocabulary
// (response-to-gui §2): exactly four states, verified rendered as age.
func readControlPlaneMemories(pendingOnly bool) []Memory {
	var out []Memory
	for _, r := range memory.LoadAll(memory.DefaultDir()) {
		if r.Pending != pendingOnly {
			continue
		}
		out = append(out, mapControlPlaneRecord(r))
	}
	return out
}

func mapControlPlaneRecord(r memory.Record) Memory {
	verified := r.VerifiedAt
	if verified != "" && r.VerifiedBy != "" {
		verified += " by " + r.VerifiedBy
	}
	m := Memory{
		ID: r.ID, Claim: r.Title, Scope: r.Repository,
		Store: "crossing-guard", Grade: "pkg",
		Aliases: r.Aliases, Tags: r.Tags,
		Verified: verified, Pending: r.Pending,
		CreatedAt: r.Created, UpdatedAt: r.Updated,
	}
	switch r.Source {
	case "human":
		m.Classification = "user-asserted"
	case "harvest":
		m.Classification = "observed"
	default:
		m.Classification = "inferred"
	}
	switch {
	case r.Pending:
		m.Status = "pending"
	case r.Superseded != "":
		m.Status = "superseded"
	case verified != "":
		m.Status = "verified"
	default:
		m.Status = "active"
	}
	if r.Source != "" {
		m.Sources = []string{r.Source}
	}
	return m
}

// Shared helpers — still used by skills.go for SKILL.md frontmatter (a
// DIFFERENT format contract than the memory store; R1's "delete the parser"
// applies to memory dossiers only, which now come via cpmem --json above).
func homeDir() string { h, _ := os.UserHomeDir(); return h }

func splitFrontmatter(s string) (map[string]string, string) {
	fm := map[string]string{}
	if !strings.HasPrefix(s, "---\n") {
		return fm, s
	}
	rest := s[4:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return fm, s
	}
	for _, line := range strings.Split(rest[:end], "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			fm[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return fm, rest[end+4:]
}

// GetCpmemMemory fetches ONE full record (incl. dossier body), mirroring the
// CLI's `memory get --json` shape and its pending/ review fallback. The read
// is recall-logged like every store read path (instrumentation requirement).
func GetCpmemMemory(id string) (map[string]any, error) {
	// ids are kebab-case slugs; anything path-like is a traversal attempt
	if id == "" || strings.ContainsAny(id, "/\\.") {
		return nil, fmt.Errorf("record %q not found", id)
	}
	dir := memory.DefaultDir()
	r, err := memory.Read(filepath.Join(dir, id+".md"))
	if err != nil { // review flow: proposals must be readable BEFORE promotion
		if pr, perr := memory.Read(filepath.Join(dir, "pending", id+".md")); perr == nil {
			pr.Pending = true
			r, err = pr, nil
		}
	}
	if err != nil {
		return nil, fmt.Errorf("record %q not found", id)
	}
	memory.LogRecall(dir, "get", id, []string{r.ID})
	raw, err := json.Marshal(struct {
		memory.Record
		Body string `json:"body"`
	}{r, r.Body})
	if err != nil {
		return nil, err
	}
	var rec map[string]any
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// ListPendingMemories returns the engine's proposal inbox (pending/).
// Engine contract: the console is the ONLY promote surface beyond the CLI.
func ListPendingMemories() []Memory {
	return readControlPlaneMemories(true)
}

// PromoteMemory / RejectMemory delegate the human act to the engine package —
// mutations stay engine-owned; the console never writes dossier files itself.
func PromoteMemory(id string) (string, error) {
	if err := memory.Promote(memory.DefaultDir(), id); err != nil {
		return "", err
	}
	return "promoted " + id + " into the store", nil
}

func RejectMemory(id, reason string) (string, error) {
	if reason == "" {
		reason = "rejected via console (no reason given)"
	}
	if err := memory.Reject(memory.DefaultDir(), id, reason); err != nil {
		return "", err
	}
	return "rejected " + id + " (retained in rejected/)", nil
}

func (s *Store) UpsertMemory(m Memory) (Memory, error) {
	if m.Claim == "" {
		return m, fmt.Errorf("claim is required")
	}
	if m.Store == "crossing-guard" {
		return m, fmt.Errorf("crossing-guard records are read-only in the console — edit via `cpmem memory`")
	}
	m.Store = "probe"
	if m.Classification == "" {
		m.Classification = "user-asserted"
	}
	if m.Status == "" {
		m.Status = "draft"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var all []Memory
	_ = readJSON(s.path("memories.json"), &all)
	if m.ID == "" {
		m.ID = fmt.Sprintf("mem_%d", time.Now().UnixNano())
		m.CreatedAt = now()
		m.UpdatedAt = m.CreatedAt
		all = append(all, m)
	} else {
		found := false
		for i := range all {
			if all[i].ID == m.ID {
				m.CreatedAt = all[i].CreatedAt
				m.UpdatedAt = now()
				all[i] = m
				found = true
				break
			}
		}
		if !found {
			return m, fmt.Errorf("memory %s not found", m.ID)
		}
	}
	return m, writeJSONFile(s.path("memories.json"), all)
}

// --- notes ---

func (s *Store) ListNotes() []Note {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Note
	_ = readJSON(s.path("notes.json"), &out)
	if out == nil {
		out = []Note{}
	}
	return out
}

func (s *Store) AddNote(n Note) (Note, error) {
	if n.Text == "" {
		return n, fmt.Errorf("text is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var all []Note
	_ = readJSON(s.path("notes.json"), &all)
	n.ID = fmt.Sprintf("note_%d", time.Now().UnixNano())
	n.CreatedAt = now()
	all = append(all, n)
	return n, writeJSONFile(s.path("notes.json"), all)
}

// --- io helpers ---

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func writeJSONFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
