package daemon

// Store: JSON-file persistence for memory records and notes. The real product uses
// SQLite (ADR 0003); a probe uses files.
// Memory records follow a SIMPLIFIED cut of schemas/memory.schema.json —
// enough to exercise the review/manage UI, not schema-complete.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"crossing-guard/memory"
	"crossing-guard/store"
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
	// Revision is the record's revision as read. An edit sends it back; a save made
	// against an older revision is refused (409), never merged (team item 5, 13b).
	Revision int64 `json:"revision,omitempty"`
	// Team is the record's team-sync facts (team item 5); nil for a record that is
	// neither at a shareable scope nor from a teammate.
	Team *MemoryTeam `json:"team,omitempty"`
}

// MemoryTeam is one record's standing with the linked team, as facts.
type MemoryTeam struct {
	GlobalID       string `json:"global_id"`
	ScopeType      string `json:"scope_type"`
	Shared         bool   `json:"shared"`
	CanTravel      bool   `json:"can_travel"`              // organization, or repository identified by its remote
	IdentityNote   string `json:"identity_note,omitempty"` // why a repository record stays on this device
	Origin         string `json:"origin"`                  // local | pulled
	Author         string `json:"author,omitempty"`        // the authenticated author of the last landed team revision
	ServerRevision int64  `json:"server_revision,omitempty"`
	InSync         bool   `json:"in_sync"`                 // this device's content equals the last synced revision
	RejectedHere   bool   `json:"rejected_here,omitempty"` // rejected on this device only; the team's version is unchanged
	Collision      string `json:"collision,omitempty"`     // alias | shadowed
	WireSlug       string `json:"wire_slug,omitempty"`     // the team's name when this id is a local alias
	Conflicts      int    `json:"conflicts,omitempty"`     // conflict copies kept for this record
	Imported       bool   `json:"imported,omitempty"`      // source import: shared one by one, never in bulk
	// Refused is the code the team refused this record's latest revision with: the edit
	// on this device was not taken and is not the team's (FR-6). Empty otherwise.
	Refused string `json:"refused,omitempty"`
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

func (s *Store) ListMemories() ([]Memory, error) {
	// ONE console-visible memory store (RT-8: the probe memories.json merge is
	// retired; sandbox records go through the write owner with an origin label).
	return readControlPlaneMemories(false)
}

// readControlPlaneMemories maps the STORE onto the console vocabulary
// (response-to-gui §2): exactly four states, verified rendered as age. Since
// the first-class-records change this reads index.sqlite (the one read surface
// rule, plan §3.5) — no file walking. A store that cannot be read is an error,
// never an empty list; one not created yet is empty
// (memory-store-unavailable-reads plan §2).
func readControlPlaneMemories(pendingOnly bool) ([]Memory, error) {
	out := []Memory{}
	ix, err := openIndexForRead()
	if errors.Is(err, errIndexNotCreated) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	defer ix.Close()
	status := "active"
	if pendingOnly {
		status = "pending"
	}
	recs, err := ix.ListMemory(status)
	if err != nil {
		return nil, err
	}
	conflicts, err := ix.MemoryConflictCounts()
	if err != nil {
		return nil, err
	}
	refusals, err := ix.MemoryRefusals()
	if err != nil {
		return nil, err
	}
	for _, r := range recs {
		m := mapStoreMemoryRecord(r)
		if m.Team != nil {
			m.Team.Conflicts, m.Team.Refused = conflicts[r.GlobalID], refusals[r.GlobalID]
		}
		out = append(out, m)
	}
	if pendingOnly {
		return out, nil
	}
	// A team record rejected on this device stays visible: the team's version is
	// unchanged, and it returns when a teammate changes it or it is promoted again (O-9).
	rejected, err := ix.ListMemory("rejected")
	if err != nil {
		return nil, err
	}
	for _, r := range rejected {
		if r.ShareState != "shared" && r.SyncOrigin != "pulled" {
			continue
		}
		m := mapStoreMemoryRecord(r)
		m.Status = "rejected"
		if m.Team != nil {
			m.Team.Conflicts, m.Team.Refused = conflicts[r.GlobalID], refusals[r.GlobalID]
		}
		out = append(out, m)
	}
	return out, nil
}

func mapStoreMemoryRecord(r store.MemoryRecord) Memory {
	verified := r.VerifiedAt
	if verified != "" && r.VerifiedBy != "" {
		verified += " by " + r.VerifiedBy
	}
	m := Memory{
		ID: r.ID, Claim: r.Title, Scope: r.ScopeID,
		Store: "crossing-guard", Grade: "pkg",
		Aliases: r.Aliases, Tags: r.Tags,
		Verified:  verified,
		CreatedAt: time.Unix(0, r.CreatedAt).UTC().Format(time.RFC3339),
		UpdatedAt: time.Unix(0, r.UpdatedAt).UTC().Format(time.RFC3339),
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
	case r.Status == "pending":
		m.Status = "pending"
		m.Pending = true
	case r.SupersededBy != "":
		m.Status = "superseded"
	case verified != "":
		m.Status = "verified"
	default:
		m.Status = "active"
	}
	if r.Source != "" {
		m.Sources = []string{r.Source}
	}
	m.Revision = r.Revision
	canTravel := r.ScopeType == store.MemoryScopeOrganization || (r.ScopeType == store.MemoryScopeRepository && r.RepositoryIdentity == "remote-sha256")
	if r.ScopeType != store.MemoryScopeUser || r.SyncOrigin == "pulled" {
		m.Team = &MemoryTeam{GlobalID: r.GlobalID, ScopeType: string(r.ScopeType), Shared: r.ShareState == "shared", CanTravel: canTravel,
			IdentityNote: r.IdentityNote, Origin: r.SyncOrigin, Author: r.TeamAuthor, ServerRevision: r.ServerRevision,
			InSync:       store.MemoryProjectionHash(r) == r.SyncedProjectionHash,
			RejectedHere: r.Status == "rejected" && (r.ShareState == "shared" || r.SyncOrigin == "pulled"),
			Collision:    r.Collision, WireSlug: r.WireSlug, Imported: r.Source == "import"}
		if !canTravel && m.Team.IdentityNote == "" && r.ScopeType == store.MemoryScopeRepository {
			m.Team.IdentityNote = "the repository is known only by its folder name"
		}
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

// errMemoryNotFound is the one answer that means "no such record": an unknown
// or path-like id. Every other failure is the store's, and the routes say so
// (503) instead of reporting a missing record.
var errMemoryNotFound = errors.New("memory record not found")

// GetCpmemMemory fetches ONE full record (incl. dossier body), keeping the
// console's response shape. The read is recall-logged like every store read
// path (instrumentation requirement), under the caller's label (recallLabel).
// Store-backed since the first-class-records change — the id guard stays
// because the response map is built from it.
func GetCpmemMemory(id, via string) (map[string]any, error) {
	// ids are kebab-case slugs; anything path-like is a traversal attempt
	if id == "" || strings.ContainsAny(id, "/\\.") {
		return nil, fmt.Errorf("record %q: %w", id, errMemoryNotFound)
	}
	ix, err := openIndexForRead()
	if errors.Is(err, errIndexNotCreated) {
		return nil, fmt.Errorf("record %q: %w", id, errMemoryNotFound)
	}
	if err != nil {
		return nil, err
	}
	defer ix.Close()
	r, err := ix.MemoryByID(id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("record %q: %w", id, errMemoryNotFound)
	}
	if err != nil {
		return nil, err
	}
	sources, err := ix.MemorySources(id)
	if err != nil {
		return nil, err
	}
	memory.LogRecall(memory.DefaultDir(), recallLabel(via, "get"), id, []string{r.ID})
	rec := memoryRecordMap(r)
	if sources == nil {
		sources = []store.MemorySource{}
	}
	rec["sources"] = sources
	return rec, nil
}

// ListMemoryRecordMaps lists every record in one status as the same map
// GetCpmemMemory returns, minus sources (one query for the list, not one per
// record). status is one of the store's three; the caller validates it.
func ListMemoryRecordMaps(status string) ([]map[string]any, error) {
	ix, err := openIndexForRead()
	if errors.Is(err, errIndexNotCreated) {
		return []map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer ix.Close()
	recs, err := ix.ListMemory(status)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(recs))
	for _, r := range recs {
		out = append(out, memoryRecordMap(r))
	}
	return out, nil
}

// memoryRecordMap is the one record shape /api/memory/record and
// /api/memory/records share, so the single and list reads cannot drift.
func memoryRecordMap(r store.MemoryRecord) map[string]any {
	tags, aliases := r.Tags, r.Aliases
	if tags == nil {
		tags = []string{}
	}
	if aliases == nil {
		aliases = []string{}
	}
	return map[string]any{
		"id": r.ID, "title": r.Title, "category": r.Category,
		"scope_type": string(r.ScopeType), "repository": r.ScopeID,
		"tags": tags, "aliases": aliases,
		"source": r.Source, "origin": r.Origin,
		"superseded_by": r.SupersededBy,
		"verified_at":   r.VerifiedAt, "verified_by": r.VerifiedBy,
		"created": time.Unix(0, r.CreatedAt).UTC().Format(time.RFC3339),
		"updated": time.Unix(0, r.UpdatedAt).UTC().Format(time.RFC3339),
		"format":  1, "body": r.Body, "pending": r.Status == "pending",
		"revision": r.Revision, "status": r.Status,
		// Scope and team sync facts (team item 5): recall filters by scope_id and
		// repository_identity; the console states where a record came from and whether
		// this device's copy matches the team's.
		"global_id": r.GlobalID, "scope_id": r.ScopeID, "repository_identity": r.RepositoryIdentity,
		"share_state": r.ShareState, "sync_origin": r.SyncOrigin, "team_author": r.TeamAuthor,
		"server_revision": r.ServerRevision, "in_sync": store.MemoryProjectionHash(r) == r.SyncedProjectionHash,
		"collision": r.Collision, "wire_slug": r.WireSlug, "identity_note": r.IdentityNote,
		"reject_reason": r.RejectReason,
	}
}

// ListPendingMemories returns the engine's proposal inbox (pending/).
// Engine contract: the console is the ONLY promote surface beyond the CLI.
func ListPendingMemories() ([]Memory, error) {
	return readControlPlaneMemories(true)
}

// PromoteMemory / RejectMemory delegate the human act to the STORE write owner
// (plan §3.3) — mutations stay store-owned; the console never writes records
// through any other path, and the mirror is maintained after commit.
func PromoteMemory(id string) (string, error) {
	ix, err := store.Open(indexPath())
	if err != nil {
		return "", err
	}
	defer ix.Close()
	if _, err := ix.PromoteMemory(id, store.MemoryActor{AuthorType: "user", AuthorID: consoleActor(), ActorSource: "console"}); err != nil {
		return "", err
	}
	return "promoted " + id + " into the store", nil
}

func RejectMemory(id, reason string) (string, error) {
	if reason == "" {
		reason = "rejected via console (no reason given)"
	}
	ix, err := store.Open(indexPath())
	if err != nil {
		return "", err
	}
	defer ix.Close()
	if _, err := ix.RejectMemory(id, reason, store.MemoryActor{AuthorType: "user", AuthorID: consoleActor(), ActorSource: "console"}); err != nil {
		return "", err
	}
	return "rejected " + id + " (retained with its history)", nil
}

// consoleActor names the console's acting user for revision rows.
func consoleActor() string {
	who := os.Getenv("USER")
	if who == "" {
		who = "console"
	}
	return who
}

// UpsertMemory is the console's write path through the ONE store owner
// (memory-first-class-records plan §5.3/§6: Crossing Guard records are
// console-writable; the probe `memories.json` store is retired). New records
// are drafted `pending` — the human gate (ADR 0013 D4): the console never
// silently injects what a person has not promoted.
//
// Without an id it CREATES: a freshly minted id through the create-only write,
// so it can never land on an existing record (memory-create-identity plan D-1).
// With an id it EDITS that existing record (D-3): fields the request does not
// carry are kept, and the edit is re-drafted pending as ADR 0013's addendum
// says. Record tags/aliases are carried and validated, never dropped (D-2).
func (s *Store) UpsertMemory(m Memory) (Memory, error) {
	if m.Claim == "" {
		return m, fmt.Errorf("%w: claim is required", store.ErrMemoryInvalid)
	}
	ix, err := store.Open(indexPath())
	if err != nil {
		return m, err
	}
	defer ix.Close()
	who := consoleActor()
	actor := store.MemoryActor{AuthorType: "user", AuthorID: who, ActorSource: "console"}

	var saved store.MemoryRecord
	if m.ID == "" {
		saved, err = ix.CreateMemory(store.MemoryRecord{
			ID: newConsoleMemoryID(), Title: m.Claim, Body: m.Body,
			Tags: m.Tags, Aliases: m.Aliases,
			Category: "note", Source: "human",
			Status: "pending", ScopeType: store.MemoryScopeUser,
			AuthorType: "user", AuthorID: who,
		}, nil, nil, actor)
	} else {
		saved, err = editConsoleMemory(ix, m, actor)
	}
	if err != nil {
		return m, err
	}
	return mapStoreMemoryRecord(saved), nil
}

// editConsoleMemory revises one existing record from a console request:
// title always, body when non-empty, tags/aliases when the request carries a
// non-null array (absent and null keep; [] clears). Only the labels the
// request carries are validated — a kept label is the record's, not the
// request's. Category, scope, source, origin, verified stamps and the author
// of record are the stored record's. The write refuses inside its transaction
// if the record was deleted after this read.
func editConsoleMemory(ix *store.Index, m Memory, actor store.MemoryActor) (store.MemoryRecord, error) {
	if err := store.ValidateMemoryLabels(m.Tags, m.Aliases); err != nil {
		return store.MemoryRecord{}, err
	}
	rec, err := ix.MemoryByID(m.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return rec, fmt.Errorf("record %q: %w", m.ID, errMemoryNotFound)
	}
	if err != nil {
		return rec, err
	}
	rec.Title = m.Claim
	if m.Body != "" {
		rec.Body = m.Body
	}
	if m.Tags != nil {
		rec.Tags = m.Tags
	}
	if m.Aliases != nil {
		rec.Aliases = m.Aliases
	}
	// Re-drafted pending: a rejection's reason no longer describes it.
	rec.Status, rec.RejectReason = "pending", ""
	// A team record is edited against the revision that was read, always: without one the
	// save could re-write an older body over a teammate's landed revision (13b, PW-M8).
	if m.Revision <= 0 && store.MemoryIsTeamRecord(rec) {
		return rec, fmt.Errorf("%w: an edit of a team record carries the revision it read", store.ErrMemoryStale)
	}
	var saved store.MemoryRecord
	if m.Revision > 0 {
		// The console sends the revision it displayed: a save over a revision that has
		// since moved (a teammate's landed edit) is refused, never merged (13b).
		saved, err = ix.ReviseMemoryAt(rec, m.Revision, nil, nil, actor)
	} else {
		saved, err = ix.ReviseMemory(rec, nil, nil, actor)
	}
	if errors.Is(err, store.ErrMemoryNotFound) {
		return saved, fmt.Errorf("record %q: %w", m.ID, errMemoryNotFound)
	}
	return saved, err
}

// newConsoleMemoryID mints a console-created record's id (a var so a test can
// force a collision and prove the create refuses instead of editing).
var newConsoleMemoryID = func() string { return store.NewMemoryID("note") }

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
