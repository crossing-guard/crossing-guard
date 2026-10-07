package store

// Memory as first-class records — the ONE write owner (memory-first-class-records
// plan §3.3, red-team folds RT-2/RT-3/RT-4). Every mutation of a memory record —
// CLI verb, daemon import, console write, MCP proposal, promote/reject/delete —
// goes through UpsertMemory/PromoteMemory/RejectMemory/DeleteMemory and nowhere
// else: record + revision + entity + classification + FTS + outbox change in ONE
// caller-owned GovTx, so there is no partial state and no second writer to drift.
//
// The files directory (~/.crossing-guard/memory) is a WRITE-THROUGH MIRROR the
// caller maintains after commit (memory.Mirror*), never a source of truth.

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"crossing-guard/engine"
	"crossing-guard/teamwire"
)

// MemoryScopeType mirrors common.schema.json $defs/scope for the four-state
// lifecycle a record carries locally (the wire constrains sharing to
// repository|organization; user scope never leaves the device).
type MemoryScopeType string

const (
	MemoryScopeUser         MemoryScopeType = "user"
	MemoryScopeRepository   MemoryScopeType = "repository"
	MemoryScopeOrganization MemoryScopeType = "organization"
)

// MemoryRecord is the store-canonical record (plan §3.1). Tags and aliases are
// JSON arrays in SQLite; the struct keeps Go slices and the layer converts.
type MemoryRecord struct {
	ID                 string // slug, kebab-case — unchanged vocabulary
	GlobalID           string // mem_… typed id (RT-4); minted on first write
	Status             string // active | pending | rejected
	ScopeType          MemoryScopeType
	ScopeID            string // repository label / org id; '' when scope is user
	RepositoryIdentity string // weak | remote-sha256
	Title              string
	Category           string
	Body               string
	Tags               []string
	Aliases            []string
	Source             string // human | agent | import | harvest
	Origin             string
	SupersededBy       string
	VerifiedAt         string
	VerifiedBy         string
	AuthorType         string // RT-3: wire actor {type,id}; '' is invalid on the wire
	AuthorID           string
	Revision           int64
	ContentHash        string // sha256 over the wire-shaped record
	RejectReason       string
	CreatedAt          int64 // unix nanos
	UpdatedAt          int64

	// Team sync state (schema 44, team item 5). The write owner carries these across
	// every local write; only the sync transitions (push settle, pull landing, the
	// share action) move them. Callers building a record leave them zero.
	ShareState           string // unshared | shared — only shared leaves the device (decision 14)
	SyncOrigin           string // local | pulled
	PushedHash           string // wire hash of the last accepted or landed revision: the next push's base
	ServerRevision       int64  // that revision's server number
	SyncedProjectionHash string // the content projection at that moment (decision 13)
	HeldServerRevision   int64  // a pulled revision waiting for an in-flight push to settle (decision 5 rule 2)
	WireSlug             string // the team's slug when ID is a local collision alias (decision 15)
	Collision            string // '' | alias | shadowed
	TeamAuthor           string // the authenticated author of the last landed team revision
	IdentityNote         string // why a repository record's identity is weak, when known
}

// MemorySource is one session citation (plan §4, ADR 0013 D2). Anchor kinds
// close the O2 anchor-format question: event-uuid | turn-id | line-offset |
// none (record-level).
type MemorySource struct {
	Vendor     string
	SessionID  string
	Anchor     string
	AnchorKind string
	CapturedAt int64
}

// MemoryActor names who performed a mutation, for the revision row.
type MemoryActor struct {
	AuthorType  string // human | user | daemon | agent | mcp (wire actor shape)
	AuthorID    string
	ActorSource string // the writing door: cli | daemon | console | mcp
}

// Well-known memory categories (memory.Categories is the owner of the enum;
// mirrored here as SQL constraints do not carry Go constants).
var memoryCategories = map[string]bool{
	"convention": true, "gotcha": true, "bug-fix": true, "architecture": true,
	"how-to": true, "incident": true, "preference": true, "business-rule": true,
	"tech-stack": true, "note": true,
}

var memorySources = map[string]bool{"human": true, "agent": true, "import": true, "harvest": true}

// ErrMemoryInvalid marks a refusal caused by the record itself (its shape,
// enums, author, id or labels — not its source citations), so a door can
// answer 400 and keep I/O failures apart.
var ErrMemoryInvalid = errors.New("invalid memory record")

// ErrMemoryExists is CreateMemory's refusal: the id already names a record,
// and a create never edits one.
var ErrMemoryExists = errors.New("memory record already exists")

// ErrMemoryNotFound is ReviseMemory's refusal: the id names no record, and a
// revision never creates one.
var ErrMemoryNotFound = errors.New("memory record not found")

// memoryInvalid wraps a validation error without changing its text.
type memoryInvalid struct{ err error }

func (e memoryInvalid) Error() string        { return e.err.Error() }
func (e memoryInvalid) Unwrap() error        { return e.err }
func (e memoryInvalid) Is(target error) bool { return target == ErrMemoryInvalid }

// ValidateMemoryRecord enforces the write owner's invariants: identity, enum
// membership, and the RT-3 rule that a record is wire-shaped (author present)
// before it is ever stored, so no migrated-or-new record is invalid on the wire.
func ValidateMemoryRecord(r MemoryRecord) error {
	if err := validateMemoryRecord(r); err != nil {
		return memoryInvalid{err}
	}
	return nil
}

func validateMemoryRecord(r MemoryRecord) error {
	if r.ID == "" {
		return fmt.Errorf("memory id required")
	}
	if strings.TrimSpace(r.Title) == "" {
		return fmt.Errorf("memory title required")
	}
	if !memoryCategories[r.Category] {
		return fmt.Errorf("memory category %q not in enum", r.Category)
	}
	if !memorySources[r.Source] {
		return fmt.Errorf("memory source %q not in enum (human|agent|import|harvest)", r.Source)
	}
	switch r.ScopeType {
	case MemoryScopeUser:
		if r.ScopeID != "" {
			return fmt.Errorf("user-scoped memory cannot carry a scope id")
		}
	case MemoryScopeRepository, MemoryScopeOrganization:
		if r.ScopeID == "" {
			return fmt.Errorf("%s-scoped memory requires a scope id", r.ScopeType)
		}
	default:
		return fmt.Errorf("memory scope %q not in enum (user|repository|organization)", r.ScopeType)
	}
	if r.RepositoryIdentity != "" && r.RepositoryIdentity != "weak" && r.RepositoryIdentity != "remote-sha256" {
		return fmt.Errorf("repository_identity %q not in enum (weak|remote-sha256)", r.RepositoryIdentity)
	}
	if r.AuthorType == "" || r.AuthorID == "" {
		return fmt.Errorf("memory author is required (wire actor {type,id})")
	}
	if r.Status == "" {
		r.Status = "active" // validated again below; default documented for callers
	}
	if r.Status != "active" && r.Status != "pending" && r.Status != "rejected" {
		return fmt.Errorf("memory status %q not in enum (active|pending|rejected)", r.Status)
	}
	return nil
}

// memoryWireShape is the LOCAL snapshot a revision row stores and the local content
// hash commits to: the unredacted body, the local slug, the local revision and author.
// It is not the wire record: the wire carries a redacted body and its own hash
// (teamwire.MemoryWireHash, team item 5 decision 13c), computed at first send.
type memoryWireShape struct {
	ID           string   `json:"id"`
	Slug         string   `json:"slug"`
	Revision     int64    `json:"revision"`
	ScopeType    string   `json:"scope_type"`
	ScopeID      string   `json:"scope_id"`
	Title        string   `json:"title"`
	Category     string   `json:"category"`
	Body         string   `json:"body"`
	Tags         []string `json:"tags"`
	Aliases      []string `json:"aliases"`
	Source       string   `json:"source"`
	Origin       string   `json:"origin"`
	SupersededBy string   `json:"superseded_by"`
	VerifiedAt   string   `json:"verified_at"`
	VerifiedBy   string   `json:"verified_by"`
	AuthorType   string   `json:"author_type"`
	AuthorID     string   `json:"author_id"`
}

// MemoryContentHash commits to the local record at the given revision (the revision
// chain's link). It covers the unredacted body and the local revision, so it is NOT what
// a server compares: the wire hash is teamwire.MemoryWireHash over the redacted record
// (team item 5 decision 13c), and "in sync" is judged by MemoryProjectionHash.
func MemoryContentHash(r MemoryRecord) string {
	shape := memoryWireShape{
		ID: r.GlobalID, Slug: r.ID, Revision: r.Revision,
		ScopeType: string(r.ScopeType), ScopeID: r.ScopeID,
		Title: r.Title, Category: r.Category, Body: r.Body,
		Tags: nonNilStrings(r.Tags), Aliases: nonNilStrings(r.Aliases),
		Source: r.Source, Origin: r.Origin, SupersededBy: r.SupersededBy,
		VerifiedAt: r.VerifiedAt, VerifiedBy: r.VerifiedBy,
		AuthorType: r.AuthorType, AuthorID: r.AuthorID,
	}
	b, _ := json.Marshal(shape)
	return fmt.Sprintf("sha256:%x", sha256.Sum256(b))
}

// MemoryProjectionHash covers a record's CONTENT only — global id, local slug, scope,
// title, category, body, tags, aliases, source, superseded_by — never revision, status,
// author, verification or timestamps, which change on every write (team item 5
// decision 13, Q-3). It is always computed from the local row (the local slug, the
// unredacted body), so redaction and a collision alias never make a record look
// diverged (S-3). A record is in sync when this equals its synced_projection_hash.
func MemoryProjectionHash(r MemoryRecord) string {
	b, _ := json.Marshal(struct {
		GlobalID     string   `json:"id"`
		Slug         string   `json:"slug"`
		ScopeType    string   `json:"scope_type"`
		ScopeID      string   `json:"scope_id"`
		Title        string   `json:"title"`
		Category     string   `json:"category"`
		Body         string   `json:"body"`
		Tags         []string `json:"tags"`
		Aliases      []string `json:"aliases"`
		Source       string   `json:"source"`
		SupersededBy string   `json:"superseded_by"`
	}{r.GlobalID, r.ID, string(r.ScopeType), r.ScopeID, r.Title, r.Category, r.Body,
		nonNilStrings(r.Tags), nonNilStrings(r.Aliases), r.Source, r.SupersededBy})
	return fmt.Sprintf("sha256:%x", sha256.Sum256(b))
}

// MemoryShareable is decision 14's gate: only an active, deliberately shared record at a
// scope that can travel leaves the device — organization, or repository with a
// remote-derived identity (O-4: a weak, folder-name identity never travels).
func MemoryShareable(r MemoryRecord) bool {
	if r.Status != "active" || r.ShareState != "shared" {
		return false
	}
	switch r.ScopeType {
	case MemoryScopeOrganization:
		return r.ScopeID != ""
	case MemoryScopeRepository:
		return r.RepositoryIdentity == "remote-sha256" && r.ScopeID != ""
	}
	return false
}

// memoryScopeCanTravel is the gate's scope half: a record at this scope would be
// shareable once shared and active.
func memoryScopeCanTravel(r MemoryRecord) bool {
	return r.ScopeType == MemoryScopeOrganization || (r.ScopeType == MemoryScopeRepository && r.RepositoryIdentity == "remote-sha256")
}

func nonNilStrings(vs []string) []string {
	if vs == nil {
		return []string{}
	}
	return vs
}

// ---------- read ----------

const memoryRecordCols = `id,global_id,status,scope_type,scope_id,repository_identity,
	title,category,body,tags,aliases,source,origin,superseded_by,verified_at,verified_by,
	author_type,author_id,revision,content_hash,reject_reason,created_at,updated_at,
	share_state,sync_origin,pushed_hash,server_revision,synced_projection_hash,held_server_revision,
	wire_slug,collision,team_author,identity_note`

func scanMemoryRecord(row interface{ Scan(...any) error }) (MemoryRecord, error) {
	var r MemoryRecord
	var tags, aliases string
	if err := row.Scan(&r.ID, &r.GlobalID, &r.Status, &r.ScopeType, &r.ScopeID, &r.RepositoryIdentity,
		&r.Title, &r.Category, &r.Body, &tags, &aliases, &r.Source, &r.Origin, &r.SupersededBy,
		&r.VerifiedAt, &r.VerifiedBy, &r.AuthorType, &r.AuthorID, &r.Revision, &r.ContentHash,
		&r.RejectReason, &r.CreatedAt, &r.UpdatedAt,
		&r.ShareState, &r.SyncOrigin, &r.PushedHash, &r.ServerRevision, &r.SyncedProjectionHash, &r.HeldServerRevision,
		&r.WireSlug, &r.Collision, &r.TeamAuthor, &r.IdentityNote); err != nil {
		return MemoryRecord{}, err
	}
	r.Tags = decodeJSONStrings(tags)
	r.Aliases = decodeJSONStrings(aliases)
	return r, nil
}

func decodeJSONStrings(s string) []string {
	if s == "" {
		return []string{}
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil || out == nil {
		return []string{}
	}
	return out
}

func encodeJSONStrings(vs []string) string {
	b, err := json.Marshal(nonNilStrings(vs))
	if err != nil {
		return "[]"
	}
	return string(b)
}

// MemoryByID loads one record by slug, any status.
func (ix *Index) MemoryByID(id string) (MemoryRecord, error) {
	return scanMemoryRecord(ix.db.QueryRow(
		`SELECT `+memoryRecordCols+` FROM memory_record WHERE id=?`, id))
}

// ListMemory lists records by status ("" = all), newest-updated first.
func (ix *Index) ListMemory(status string) ([]MemoryRecord, error) {
	query := `SELECT ` + memoryRecordCols + ` FROM memory_record`
	var args []any
	if status != "" {
		query += ` WHERE status=?`
		args = append(args, status)
	}
	query += ` ORDER BY updated_at DESC`
	rows, err := ix.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemoryRecord
	for rows.Next() {
		r, err := scanMemoryRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// InsertMemoryTombstone records a deleted slug without a store record (the
// migration path for legacy tombstones): keyed 'slug:<id>', since no global id exists.
func (ix *Index) InsertMemoryTombstone(id, deletedBy string) error {
	g, err := ix.BeginGov()
	if err != nil {
		return err
	}
	defer func() { _ = g.Rollback() }()
	if _, err := g.Exec(`INSERT INTO memory_tombstone(global_id,id,deleted_at,deleted_by) VALUES(?,?,?,?)
		ON CONFLICT(global_id) DO NOTHING`, "slug:"+id, id, time.Now().UnixNano(), deletedBy); err != nil {
		return err
	}
	return g.Commit()
}

// MemoryRevisionByID loads one revision snapshot by the record's CURRENT slug (the
// console's history read). Revisions are keyed by global id since schema 44; the slug
// resolves to the record first, so a re-created slug reads its own history.
func (ix *Index) MemoryRevisionByID(recordID string, revision int64) (string, int64, string, string, error) {
	var body, prevHash, changed string
	var createdAt int64
	err := ix.db.QueryRow(`SELECT v.body,v.prev_hash,v.changed,v.created_at FROM memory_revision v
		JOIN memory_record r ON r.global_id = v.global_id
		WHERE r.id=? AND v.revision=?`, recordID, revision).
		Scan(&body, &prevHash, &changed, &createdAt)
	return body, createdAt, prevHash, changed, err
}

// MemorySources returns a record's citations, oldest first.
func (ix *Index) MemorySources(recordID string) ([]MemorySource, error) {
	rows, err := ix.db.Query(`SELECT vendor,session_id,anchor,anchor_kind,captured_at
		FROM memory_source WHERE record_id=? ORDER BY captured_at`, recordID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemorySource
	for rows.Next() {
		var s MemorySource
		if err := rows.Scan(&s.Vendor, &s.SessionID, &s.Anchor, &s.AnchorKind, &s.CapturedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ---------- write: the one owner ----------

// UpsertMemory inserts or revises one record in one transaction: the record row,
// its revision snapshot (hash-chained per record), the governed entity +
// classification state, the FTS search document, and — when the record's scope
// is shareable — the sync outbox enqueue (RT-2/RT-4). dets may be nil, in which
// case classification is skipped (the migration and tests use this; the daemon
// passes the layered library so R7 holds: memory classifies like everything
// else).
func (ix *Index) UpsertMemory(r MemoryRecord, sources []MemorySource, dets []engine.Detector, actor MemoryActor) (MemoryRecord, error) {
	return ix.writeMemory(r, sources, dets, actor, memoryWrite{mode: memoryUpsert})
}

// memoryWriteMode says what writeMemory may do with the row it finds (or does
// not find) inside its transaction.
type memoryWriteMode int

const (
	memoryUpsert     memoryWriteMode = iota // insert or revise
	memoryCreateOnly                        // insert; refuse an existing id
	memoryReviseOnly                        // revise; refuse a missing id
)

// memoryIDPattern is the record slug rule the mirror enforces on write
// (memory.Validate), checked on create so a record the mirror would refuse is
// never stored.
var memoryIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,99}$`)

// NewMemoryID mints a new record id: <prefix>-<UTC second>-<12 hex>. The
// timestamp keeps ids readable and ordered; the 48 random bits make two ids
// minted in one second distinct (a clash would still fail closed in
// CreateMemory, never edit). prefix must itself be a slug.
func NewMemoryID(prefix string) string {
	var raw [6]byte
	_, _ = rand.Read(raw[:]) // crypto/rand.Read never fails (it aborts the process instead)
	return prefix + "-" + time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(raw[:])
}

// Record label bounds: the wire schema's (schemas/memory.schema.json tags /
// aliases item maxLength), plus a count cap on what one request may carry.
const (
	memoryTagMaxChars   = 100
	memoryAliasMaxChars = 200
	memoryLabelsMax     = 64
)

// ValidateMemoryLabels checks a record's content tags and aliases against the
// wire schema (non-empty, bounded, unique) and the mirror's frontmatter
// round-trip (no comma — the list separator; no control character; no
// surrounding space — frontmatter trims). Owner tags have their own grammar
// (ValidateSessionOwnerTag); these are record content.
func ValidateMemoryLabels(tags, aliases []string) error {
	check := func(kind string, vs []string, maxChars int) error {
		if len(vs) > memoryLabelsMax {
			return fmt.Errorf("memory %s: %d given, at most %d", kind, len(vs), memoryLabelsMax)
		}
		seen := make(map[string]bool, len(vs))
		for _, v := range vs {
			switch {
			case v == "":
				return fmt.Errorf("memory %s: an empty value", kind)
			case utf8.RuneCountInString(v) > maxChars:
				return fmt.Errorf("memory %s: %q is longer than %d characters", kind, labelExcerpt(v), maxChars)
			case strings.IndexFunc(v, unicode.IsControl) >= 0:
				return fmt.Errorf("memory %s: %q contains a control character", kind, labelExcerpt(v))
			case v != strings.TrimSpace(v):
				return fmt.Errorf("memory %s: %q starts or ends with a space", kind, labelExcerpt(v))
			case strings.ContainsRune(v, ','):
				return fmt.Errorf("memory %s: %q contains a comma", kind, labelExcerpt(v))
			case seen[v]:
				return fmt.Errorf("memory %s: %q is repeated", kind, labelExcerpt(v))
			}
			seen[v] = true
		}
		return nil
	}
	if err := check("tags", tags, memoryTagMaxChars); err != nil {
		return memoryInvalid{err}
	}
	if err := check("aliases", aliases, memoryAliasMaxChars); err != nil {
		return memoryInvalid{err}
	}
	return nil
}

// labelExcerpt bounds a refused label quoted back in an error, so a long value
// is not echoed whole into a response (or an agent's context).
func labelExcerpt(v string) string {
	const keep = 40
	if utf8.RuneCountInString(v) <= keep {
		return v
	}
	return string([]rune(v)[:keep]) + "…"
}

// CreateMemory writes a NEW record through the same transaction as UpsertMemory
// and refuses (ErrMemoryExists, nothing written) when the id already names a
// record: a create never edits. The existence check runs inside the write
// transaction, so two concurrent creates of one id cannot both pass it. A new
// record's id and labels are checked here — no legacy row is involved, unlike
// ValidateMemoryRecord, which also guards promote/reject of imported records.
func (ix *Index) CreateMemory(r MemoryRecord, sources []MemorySource, dets []engine.Detector, actor MemoryActor) (MemoryRecord, error) {
	if !memoryIDPattern.MatchString(r.ID) {
		return r, memoryInvalid{fmt.Errorf("memory id must be a kebab-case slug: %q", r.ID)}
	}
	if err := ValidateMemoryLabels(r.Tags, r.Aliases); err != nil {
		return r, err
	}
	return ix.writeMemory(r, sources, dets, actor, memoryWrite{mode: memoryCreateOnly})
}

// ErrMemoryStale is ReviseMemoryAt's refusal: the record moved past the revision the
// caller read. A stale save is refused, never merged (team item 5 decision 13b).
var ErrMemoryStale = errors.New("memory record changed since it was read")

// ReviseMemory is UpsertMemory for an EXISTING record: it refuses
// (ErrMemoryNotFound, nothing written) when the id names no record — checked
// inside the write transaction, so a record deleted after the caller read it
// is not recreated.
func (ix *Index) ReviseMemory(r MemoryRecord, sources []MemorySource, dets []engine.Detector, actor MemoryActor) (MemoryRecord, error) {
	return ix.writeMemory(r, sources, dets, actor, memoryWrite{mode: memoryReviseOnly})
}

// ReviseMemoryAt is ReviseMemory with an expected base: expected is the revision the
// caller read. When the stored revision is any other, nothing is written and
// ErrMemoryStale is returned — the console, CLI and MCP doors pass what they read, so a
// save made over a teammate's landed revision is refused instead of silently reverting it.
func (ix *Index) ReviseMemoryAt(r MemoryRecord, expected int64, sources []MemorySource, dets []engine.Detector, actor MemoryActor) (MemoryRecord, error) {
	if expected <= 0 {
		return r, memoryInvalid{fmt.Errorf("expected revision must be positive")}
	}
	return ix.writeMemory(r, sources, dets, actor, memoryWrite{mode: memoryReviseOnly, expected: expected})
}

// NarrowMemoryToUser is the EXPLICIT narrowing of a record to user scope (owner ruling
// O-11): on a record the team holds it writes r's content to a detached copy of the
// user's own and leaves the team's record untouched; a repeat narrowing of the same team
// record revises that same copy. On a record the team never received it is an ordinary
// write. Every other write at user scope over a team record's id — a verb that merely
// omitted its scope, an import of a mirror file — keeps the record's stored scope (FR-1).
func (ix *Index) NarrowMemoryToUser(r MemoryRecord, sources []MemorySource, dets []engine.Detector, actor MemoryActor) (MemoryRecord, error) {
	r.ScopeType, r.ScopeID, r.RepositoryIdentity, r.IdentityNote = MemoryScopeUser, "", "", ""
	return ix.writeMemory(r, sources, dets, actor, memoryWrite{mode: memoryUpsert, narrow: true})
}

// memoryWrite says what one pass of the write owner may do.
type memoryWrite struct {
	mode     memoryWriteMode
	narrow   bool           // the caller asked for user scope in so many words (NarrowMemoryToUser)
	expected int64          // > 0: the revision the caller read (ReviseMemoryAt)
	landing  *memoryLanding // non-nil: a pulled team revision lands (decision 5) — never enqueues
}

// memoryLanding is the landing mode's input: the team's revision and, for a record new
// to this device, the local alias its slug collision forced (decision 15).
type memoryLanding struct {
	wire      teamwire.PulledMemory
	wireSlug  string // the team's slug when the local id is an alias
	collision string // '' | alias | shadowed
}

func (ix *Index) writeMemory(r MemoryRecord, sources []MemorySource, dets []engine.Detector, actor MemoryActor, w memoryWrite) (MemoryRecord, error) {
	if err := ValidateMemoryRecord(r); err != nil {
		return r, err
	}
	g, err := ix.BeginGov()
	if err != nil {
		return r, err
	}
	defer func() { _ = g.Rollback() }()
	saved, err := writeMemoryTx(g, r, sources, dets, actor, w)
	if err != nil {
		return saved, err
	}
	return saved, g.Commit()
}

// writeMemoryTx is the ONE memory write, inside the caller's transaction: record row,
// sources, revision snapshot, entity + classification + FTS, and — for a local write of a
// shareable record — the outbox row. The landing mode (decision 5) is the same write with
// the team's revision as input: it appends the next LOCAL revision carrying the server's
// number, sets the sync state, and never enqueues. The sync state is the owner's to
// carry: a local write keeps whatever the last settle or landing left.
func writeMemoryTx(g *GovTx, r MemoryRecord, sources []MemorySource, dets []engine.Detector, actor MemoryActor, w memoryWrite) (MemoryRecord, error) {
	if err := ValidateMemoryRecord(r); err != nil {
		return r, err
	}
	now := time.Now().UnixNano()
	old, hasOld, err := memoryByIDTx(g, r.ID)
	if err != nil {
		return r, err
	}
	if hasOld && w.mode == memoryCreateOnly {
		return r, fmt.Errorf("%w: %s", ErrMemoryExists, r.ID)
	}
	if !hasOld && w.mode == memoryReviseOnly {
		return r, fmt.Errorf("%w: %s", ErrMemoryNotFound, r.ID)
	}
	if w.expected > 0 && hasOld && old.Revision != w.expected {
		return r, fmt.Errorf("%w: %s is at revision %d, the save was made against %d", ErrMemoryStale, r.ID, old.Revision, w.expected)
	}
	linked, err := deviceLinked(g)
	if err != nil {
		return r, err
	}
	// A local write at user scope over a record the team holds, or may hold.
	//   - Asked for in so many words (NarrowMemoryToUser): the record is DETACHED (owner
	//     ruling O-11, 2026-10-03). What is written is a record of the user's own — a new
	//     global id, a new local id, no sync state — and the team's record is not touched
	//     at all: it keeps its id, body, sync state and queued revisions, still recalls as
	//     the team's, and takes the team's later revisions and deletion. A repeat narrowing
	//     of the same team record revises the copy already detached from it.
	//   - Anything else (a verb that omitted its scope, an import): the record keeps its
	//     stored scope and the write is an ordinary revision of the team's record (FR-1).
	detachedFrom := ""
	if hasOld && w.landing == nil && r.ScopeType == MemoryScopeUser {
		team, err := memoryReachedTeamTx(g, old, linked)
		if err != nil {
			return r, err
		}
		switch {
		case team && !w.narrow:
			r.ScopeType, r.ScopeID, r.RepositoryIdentity = old.ScopeType, old.ScopeID, old.RepositoryIdentity
		case team:
			var copyID string
			switch err := g.QueryRow(`SELECT id FROM memory_record WHERE detached_from=? AND scope_type=? ORDER BY created_at LIMIT 1`,
				old.GlobalID, string(MemoryScopeUser)).Scan(&copyID); {
			case err == nil:
				r.ID = copyID
				if old, hasOld, err = memoryByIDTx(g, copyID); err != nil {
					return r, err
				}
			case err == sql.ErrNoRows:
				detached, err := detachedCopy(g, r, old)
				if err != nil {
					return r, err
				}
				r, hasOld, detachedFrom = detached, false, old.GlobalID
			default:
				return r, err
			}
		}
	}

	if hasOld {
		// Revise in place: provenance fields (created, author of record, verified
		// stamps, origin) survive unless the caller sets them; revision bumps.
		if r.CreatedAt == 0 {
			r.CreatedAt = old.CreatedAt
		}
		if r.AuthorType == "" && old.AuthorType != "" {
			r.AuthorType, r.AuthorID = old.AuthorType, old.AuthorID
		}
		if r.VerifiedAt == "" {
			r.VerifiedAt, r.VerifiedBy = old.VerifiedAt, old.VerifiedBy
		}
		if r.Origin == "" {
			r.Origin = old.Origin
		}
		r.GlobalID = old.GlobalID
		r.Revision = old.Revision + 1
		r.ShareState, r.SyncOrigin, r.PushedHash, r.ServerRevision = old.ShareState, old.SyncOrigin, old.PushedHash, old.ServerRevision
		r.SyncedProjectionHash, r.HeldServerRevision = old.SyncedProjectionHash, old.HeldServerRevision
		r.WireSlug, r.Collision, r.TeamAuthor = old.WireSlug, old.Collision, old.TeamAuthor
		if r.IdentityNote == "" && r.RepositoryIdentity == old.RepositoryIdentity {
			r.IdentityNote = old.IdentityNote
		}
		// A deliberate widening — user or weak repository to a scope that can travel —
		// on a linked device is the act of sharing (criterion 38's user → organization).
		// An identity UPGRADE is not: it keeps the record unshared (decision 14).
		if r.ShareState != "shared" && linked && memoryScopeCanTravel(r) && !memoryScopeCanTravel(old) && old.ScopeType != r.ScopeType && r.Source != "import" {
			r.ShareState = "shared"
		}
		// A record the team never received that is written at user scope leaves sharing:
		// it is private again (PW-L6). A record the team holds is handled above.
		if r.ScopeType == MemoryScopeUser && w.landing == nil {
			r.ShareState = "unshared"
		}
	} else {
		if r.CreatedAt == 0 {
			r.CreatedAt = now
		}
		if r.GlobalID == "" {
			r.GlobalID = engine.NewTypedID("mem")
		}
		// A record new to this store starts above every revision this device ever queued
		// for its global id. The push id is the global id and the LOCAL revision, and the
		// server remembers each one it accepted from this device: a record that returns
		// after a deletion the team did not take must not re-use one with other content,
		// which the server answers as tampering (FR-2). Outbox rows outlive the record.
		var queued int64
		if err := g.QueryRow(`SELECT COALESCE(MAX(revision),0) FROM sync_outbox WHERE global_id=? AND record_kind=?`, r.GlobalID, OutboxMemory).Scan(&queued); err != nil {
			return r, err
		}
		r.Revision = queued + 1
		// Created after linking at a scope that can travel: shared by default
		// (decision 14). Anything else waits for an explicit share.
		// An IMPORTED record is never shared by being written (PW-H1): the import door
		// runs unattended on session end, so its records reach the team only through
		// the individual share action — the review decision 14 requires.
		r.ShareState, r.SyncOrigin = "unshared", "local"
		if linked && memoryScopeCanTravel(r) && r.Source != "import" {
			r.ShareState = "shared"
		}
	}
	if r.Status == "" {
		r.Status = "active"
	}
	if r.RepositoryIdentity == "" {
		r.RepositoryIdentity = "weak" // the DDL's default; '' would violate the CHECK
	}
	var serverRevision int64
	if l := w.landing; l != nil {
		r.Status, r.RejectReason = "active", "" // wire records are active by construction
		if !hasOld {
			r.ShareState = "shared" // an existing unshared record stays unshared (S-2)
			r.WireSlug, r.Collision = l.wireSlug, l.collision
		}
		r.SyncOrigin = "pulled"
		r.PushedHash, r.ServerRevision = l.wire.WireHash, l.wire.ServerRevision
		r.TeamAuthor = l.wire.AuthorName
		if r.TeamAuthor == "" {
			r.TeamAuthor = l.wire.AuthorUserID
		}
		if r.HeldServerRevision <= l.wire.ServerRevision {
			r.HeldServerRevision = 0 // the held revision is not newer than what lands
		}
		serverRevision = l.wire.ServerRevision
	}
	r.UpdatedAt = now
	r.ContentHash = MemoryContentHash(r)
	if w.landing != nil {
		r.SyncedProjectionHash = MemoryProjectionHash(r)
	}

	if _, err := g.Exec(`INSERT INTO memory_record(
		id,global_id,status,scope_type,scope_id,repository_identity,title,category,body,
		tags,aliases,source,origin,superseded_by,verified_at,verified_by,author_type,author_id,
		revision,content_hash,reject_reason,created_at,updated_at,
		share_state,sync_origin,pushed_hash,server_revision,synced_projection_hash,held_server_revision,
		wire_slug,collision,team_author,identity_note)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
		  global_id=excluded.global_id, status=excluded.status,
		  scope_type=excluded.scope_type, scope_id=excluded.scope_id,
		  repository_identity=excluded.repository_identity,
		  title=excluded.title, category=excluded.category, body=excluded.body,
		  tags=excluded.tags, aliases=excluded.aliases, source=excluded.source,
		  origin=excluded.origin, superseded_by=excluded.superseded_by,
		  verified_at=excluded.verified_at, verified_by=excluded.verified_by,
		  author_type=excluded.author_type, author_id=excluded.author_id,
		  revision=excluded.revision, content_hash=excluded.content_hash,
		  reject_reason=excluded.reject_reason, updated_at=excluded.updated_at,
		  share_state=excluded.share_state, sync_origin=excluded.sync_origin,
		  pushed_hash=excluded.pushed_hash, server_revision=excluded.server_revision,
		  synced_projection_hash=excluded.synced_projection_hash,
		  held_server_revision=excluded.held_server_revision,
		  held_wire_record=CASE WHEN excluded.held_server_revision = 0 THEN '' ELSE memory_record.held_wire_record END,
		  wire_slug=excluded.wire_slug, collision=excluded.collision,
		  team_author=excluded.team_author, identity_note=excluded.identity_note`,
		r.ID, r.GlobalID, r.Status, string(r.ScopeType), r.ScopeID, r.RepositoryIdentity,
		r.Title, r.Category, r.Body, encodeJSONStrings(r.Tags), encodeJSONStrings(r.Aliases),
		r.Source, r.Origin, r.SupersededBy, r.VerifiedAt, r.VerifiedBy, r.AuthorType, r.AuthorID,
		r.Revision, r.ContentHash, r.RejectReason, r.CreatedAt, r.UpdatedAt,
		r.ShareState, r.SyncOrigin, r.PushedHash, r.ServerRevision, r.SyncedProjectionHash, r.HeldServerRevision,
		r.WireSlug, r.Collision, r.TeamAuthor, r.IdentityNote); err != nil {
		return r, err
	}

	if detachedFrom != "" {
		if _, err := g.Exec(`UPDATE memory_record SET detached_from=? WHERE id=?`, detachedFrom, r.ID); err != nil {
			return r, err
		}
	}

	if err := upsertMemorySources(g, r.ID, sources, now); err != nil {
		return r, err
	}
	if err := appendMemoryRevision(g, r, actor, now, serverRevision); err != nil {
		return r, err
	}
	if err := indexMemoryThroughTx(g, r, dets); err != nil {
		return r, err
	}
	if w.landing == nil {
		if err := enqueueMemoryOutbox(g, r); err != nil {
			return r, err
		}
	}
	return r, nil
}

// PromoteMemory moves a record to active — the human gate (ADR 0013 D4). Promote keeps
// the record's current body and clears any rejection reason (U-4); on a shared record it
// is what carries a console edit's body to the team (C-1).
func (ix *Index) PromoteMemory(id string, actor MemoryActor) (MemoryRecord, error) {
	return ix.transitionMemory(id, "active", "", actor)
}

// RejectMemory moves a record to rejected with the reason stamped as a field (not
// body-munged), status and reason in ONE transaction (U-4). On a shared record it is a
// plain local status flip (O-9): nothing is restored and nothing is enqueued — the
// gate refuses a rejected record — so the team keeps its last accepted revision.
func (ix *Index) RejectMemory(id, reason string, actor MemoryActor) (MemoryRecord, error) {
	return ix.transitionMemory(id, "rejected", reason, actor)
}

func (ix *Index) transitionMemory(id, to, reason string, actor MemoryActor) (MemoryRecord, error) {
	r, err := ix.MemoryByID(id)
	if err != nil {
		if err == sql.ErrNoRows {
			return r, fmt.Errorf("no such memory record: %s", id)
		}
		return r, err
	}
	if r.Status == to {
		return r, fmt.Errorf("memory %s is already %s", id, to)
	}
	r.Status, r.RejectReason = to, reason
	// dets=nil: transitions do not re-classify; the body did not change. The write
	// names the revision this read saw: a teammate's revision that landed in between
	// refuses the transition instead of being re-written under the new base (13b, PW-M8).
	return ix.writeMemory(r, nil, nil, actor, memoryWrite{mode: memoryReviseOnly, expected: r.Revision})
}

// MemoryDeletion says what one deletion did, so a door can tell the user the truth.
type MemoryDeletion struct {
	Team   bool // the team held the record: its revision history was erased too (O-7)
	Queued bool // a deletion for the team was queued (the device is linked)
}

// DeleteMemory removes a record and tombstones it by global id (see DeleteMemoryReport).
func (ix *Index) DeleteMemory(id string, actor MemoryActor) error {
	_, err := ix.DeleteMemoryReport(id, actor)
	return err
}

// DeleteMemoryReport removes a record and tombstones it by global id, in one transaction
// (ADR 0013 D7: delete is honest — removed from store/index/mirror). A record the team
// does not hold keeps its revision history, as the git archive did; its queued revisions
// are acknowledged superseded and nothing is sent. A TEAM record — one the team accepted
// a revision of, received from a teammate, or with a revision already frozen for sending
// (it may have reached the server) — has its revisions and frozen wire bodies erased too
// (O-7, the stated D7 exception) and, on a linked device, a tombstone enqueued so the
// deletion propagates (decision 8).
func (ix *Index) DeleteMemoryReport(id string, actor MemoryActor) (MemoryDeletion, error) {
	var done MemoryDeletion
	g, err := ix.BeginGov()
	if err != nil {
		return done, err
	}
	defer func() { _ = g.Rollback() }()
	r, err := scanMemoryRecord(g.QueryRow(`SELECT `+memoryRecordCols+` FROM memory_record WHERE id=?`, id))
	if err == sql.ErrNoRows {
		return done, fmt.Errorf("no such memory record: %s", id)
	}
	if err != nil {
		return done, err
	}
	now := time.Now().UnixNano()
	prior := r.PushedHash
	if prior == "" {
		prior = r.ContentHash
	}
	if _, err := g.Exec(`INSERT INTO memory_tombstone(global_id,id,deleted_at,deleted_by,prior_content_hash,scope_type,scope_id,reason_class,origin)
		VALUES(?,?,?,?,?,?,?,'user-request','local') ON CONFLICT(global_id) DO NOTHING`,
		r.GlobalID, id, now, actor.AuthorID, prior, string(r.ScopeType), r.ScopeID); err != nil {
		return done, err
	}
	if err := removeMemoryRecordTx(g, r); err != nil {
		return done, err
	}
	linked, err := deviceLinked(g)
	if err != nil {
		return done, err
	}
	if done.Team, err = memoryReachedTeamTx(g, r, linked); err != nil {
		return done, err
	}
	if _, err := g.Exec(`UPDATE sync_outbox SET acked_at=?, ack_code=? WHERE global_id=? AND record_kind=? AND acked_at IS NULL`,
		now/int64(time.Second), teamwire.CodeSuperseded, r.GlobalID, OutboxMemory); err != nil {
		return done, err
	}
	if done.Team {
		if _, err := g.Exec(`DELETE FROM memory_revision WHERE global_id=?`, r.GlobalID); err != nil {
			return done, err
		}
		if err := eraseSentBodiesTx(g, r.GlobalID); err != nil {
			return done, err
		}
		tomb := teamwire.Tombstone{SchemaVersion: teamwire.TombstoneSchemaVersion,
			ID: engine.DeterministicTypedID("tmb", "memory:"+r.GlobalID), RecordID: r.GlobalID, RecordType: teamwire.TombstoneMemory,
			DeletedAt: time.Unix(0, now).UTC().Format(time.RFC3339), DeletedBy: teamwire.Actor{Type: WireActorType(actor.AuthorType), ID: teamwire.WireAuthorID},
			ReasonClass: "user-request", PriorContentHash: prior,
			RequiredProjectionCleanup: []string{"local-search", "server-search", "cache"}}
		body, err := json.Marshal(tomb)
		if err != nil {
			return done, err
		}
		if done.Queued, err = deviceLinked(g); err != nil {
			return done, err
		}
		if err := enqueueOutboxRow(g, outboxInsert{kind: OutboxTombstone, globalID: r.GlobalID, contentHash: prior,
			scope: teamwire.TombstoneMemory, at: now / int64(time.Second), sentWireBody: string(body), sentWireHash: teamwire.ContentHash(body)}); err != nil {
			return done, err
		}
	}
	return done, g.Commit()
}

// MemoryIsTeamRecord reports whether the TEAM holds this record as well as this device:
// a revision of it was accepted (it has a base) or it was received from the team — and
// it is still at a scope the team has (a record narrowed to user scope is private again).
// A record merely marked shared whose first push has not been answered — a draft, a
// record written a moment ago — is not yet the team's (CR-5). Deleting a team record
// erases its history and propagates (O-7, decision 8).
func MemoryIsTeamRecord(r MemoryRecord) bool {
	return r.ScopeType != MemoryScopeUser && (r.PushedHash != "" || r.SyncOrigin == "pulled")
}

// CountTeamRepositoryMemory counts the records recall would hand a session in the
// repository whose remote-derived id is repositoryID and that the team holds too: active,
// at that repository's scope, not shadowed, and a team record by MemoryIsTeamRecord's
// rule (it has a base, or it was received). Organization and user records are not
// counted, and neither is a weak (folder-name) repository record, which never travels.
// The id is compared as recall compares scopes (memory.SameRepositoryScope: by case
// fold). An empty id counts nothing: a checkout with no single origin remote has no
// team scope.
//
// A device that is not linked has no team, so it counts nothing. An unlink clears every
// record's base and leaves the records it received where they are, as local records
// like any other (memoryReachedTeamTx); without the link test those alone would still
// be counted as the team's while this device's own shared records were not.
func (ix *Index) CountTeamRepositoryMemory(repositoryID string) (int, error) {
	if repositoryID == "" {
		return 0, nil
	}
	var count int
	err := ix.db.QueryRow(`SELECT count(*) FROM memory_record WHERE status='active' AND scope_type=?
		AND repository_identity='remote-sha256' AND scope_id=? COLLATE NOCASE AND collision != 'shadowed'
		AND (pushed_hash != '' OR sync_origin='pulled')
		AND EXISTS (SELECT 1 FROM sync_device WHERE linked = 1)`, string(MemoryScopeRepository), repositoryID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count the repository's team memory: %w", err)
	}
	return count, nil
}

// memoryByIDTx reads one record by its local id inside the caller's transaction.
func memoryByIDTx(g *GovTx, id string) (MemoryRecord, bool, error) {
	r, err := scanMemoryRecord(g.QueryRow(`SELECT `+memoryRecordCols+` FROM memory_record WHERE id=?`, id))
	if err == sql.ErrNoRows {
		return MemoryRecord{}, false, nil
	}
	return r, err == nil, err
}

// memoryReachedTeamTx reports whether the team of the CURRENT link holds r or may (FR-4):
//   - a revision of it was accepted or landed under this link (it has a base);
//   - it was received from a team and the device is linked (between a relink and the
//     first pull the base is not back yet; after an unlink a pulled record is a local
//     record like any other);
//   - a revision of it is frozen for sending and either still unanswered — it may be on
//     the server — or answered accepted or duplicate under this link. A revision the
//     server REFUSED, and one acknowledged under an ended link, do not count.
func memoryReachedTeamTx(g *GovTx, r MemoryRecord, linked bool) (bool, error) {
	if r.ScopeType == MemoryScopeUser {
		return false, nil
	}
	if r.PushedHash != "" || (r.SyncOrigin == "pulled" && linked) {
		return true, nil
	}
	var frozen int
	err := g.QueryRow(`SELECT count(*) FROM sync_outbox WHERE global_id=? AND record_kind=? AND sent_wire_hash != ''
		AND (acked_at IS NULL OR ack_code IN (?, ?))`, r.GlobalID, OutboxMemory, teamwire.StatusAccepted, teamwire.StatusDuplicate).Scan(&frozen)
	return frozen > 0, err
}

// detachedCopy is the record a narrowing of team record old to user scope writes
// instead (O-11): r's content under a new global id and a free local id derived from the
// team's, with no sync state. Provenance the caller did not set is the team record's.
func detachedCopy(g *GovTx, r, old MemoryRecord) (MemoryRecord, error) {
	r.GlobalID = engine.NewTypedID("mem")
	id, err := freeAliasTx(g, old.ID, r.GlobalID)
	if err != nil {
		return r, err
	}
	r.ID = id
	if r.AuthorType == "" {
		r.AuthorType, r.AuthorID = old.AuthorType, old.AuthorID
	}
	if r.Origin == "" {
		r.Origin = old.Origin
	}
	r.CreatedAt, r.Revision, r.ScopeID, r.RepositoryIdentity, r.IdentityNote = 0, 0, "", "", ""
	r.ShareState, r.SyncOrigin, r.PushedHash, r.ServerRevision = "", "", "", 0
	r.SyncedProjectionHash, r.HeldServerRevision = "", 0
	r.WireSlug, r.Collision, r.TeamAuthor = "", "", ""
	return r, nil
}

// eraseSentBodiesTx blanks the frozen wire bodies on a record's memory outbox rows; the
// hashes stay (they are what a later settle and the audit of what was sent need).
func eraseSentBodiesTx(g *GovTx, globalID string) error {
	_, err := g.Exec(`UPDATE sync_outbox SET sent_wire_body='' WHERE global_id=? AND record_kind=?`, globalID, OutboxMemory)
	return err
}

// removeMemoryRecordTx removes a record's row, its FTS document and its governed entity
// (sources cascade with the row). Revisions are the caller's decision (D7 / O-7).
func removeMemoryRecordTx(g *GovTx, r MemoryRecord) error {
	if _, err := g.Exec(`DELETE FROM search_document
		WHERE owner_kind='memory' AND vendor='memory' AND session_id=?`,
		engine.EntityID("memory", r.ID)); err != nil {
		return err
	}
	if _, err := g.Exec(`DELETE FROM entity WHERE id=?`, engine.EntityID("memory", r.ID)); err != nil {
		return err
	}
	_, err := g.Exec(`DELETE FROM memory_record WHERE id=?`, r.ID)
	return err
}

// WireActorType maps a local author type (human | user | daemon | agent | mcp) to the
// wire actor's enum (user | agent | service | system).
func WireActorType(local string) string {
	switch local {
	case "agent", "mcp":
		return "agent"
	case "daemon":
		return "system"
	case "service":
		return "service"
	}
	return "user"
}

// MemoryTombstoned reports whether a slug was deleted (importers must respect
// this before resurrecting anything).
func (ix *Index) MemoryTombstoned(id string) bool {
	var n int
	if err := ix.db.QueryRow(`SELECT count(*) FROM memory_tombstone WHERE id=?`, id).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// ---------- internals ----------

func upsertMemorySources(g *GovTx, recordID string, sources []MemorySource, now int64) error {
	for _, s := range sources {
		switch s.AnchorKind {
		case "":
			s.AnchorKind = "none"
		case "event-uuid", "turn-id", "line-offset", "none":
		default:
			return fmt.Errorf("memory source anchor_kind %q not in enum", s.AnchorKind)
		}
		if s.Vendor == "" || s.SessionID == "" {
			return fmt.Errorf("memory source requires vendor and session_id")
		}
		if s.AnchorKind == "none" && s.Anchor != "" {
			return fmt.Errorf("memory source anchor_kind none cannot carry an anchor")
		}
		if s.AnchorKind != "none" && s.Anchor == "" {
			return fmt.Errorf("memory source anchor_kind %s requires an anchor", s.AnchorKind)
		}
		if s.CapturedAt == 0 {
			s.CapturedAt = now
		}
		if _, err := g.Exec(`INSERT INTO memory_source(record_id,vendor,session_id,anchor,anchor_kind,captured_at)
			VALUES(?,?,?,?,?,?)
			ON CONFLICT(record_id,vendor,session_id,anchor) DO UPDATE SET captured_at=excluded.captured_at`,
			recordID, s.Vendor, s.SessionID, s.Anchor, s.AnchorKind, s.CapturedAt); err != nil {
			return err
		}
	}
	return nil
}

func memorySnapshot(r MemoryRecord) memoryWireShape {
	return memoryWireShape{
		ID: r.GlobalID, Slug: r.ID, Revision: r.Revision,
		ScopeType: string(r.ScopeType), ScopeID: r.ScopeID,
		Title: r.Title, Category: r.Category, Body: r.Body,
		Tags: nonNilStrings(r.Tags), Aliases: nonNilStrings(r.Aliases),
		Source: r.Source, Origin: r.Origin, SupersededBy: r.SupersededBy,
		VerifiedAt: r.VerifiedAt, VerifiedBy: r.VerifiedBy,
		AuthorType: r.AuthorType, AuthorID: r.AuthorID,
	}
}

// appendMemoryRevision writes the revision snapshot keyed (global_id, LOCAL revision);
// serverRevision is the team server's number for a landed revision (0 = none yet).
func appendMemoryRevision(g *GovTx, r MemoryRecord, actor MemoryActor, now, serverRevision int64) error {
	var prevHash string
	if r.Revision > 1 {
		if err := g.QueryRow(`SELECT content_hash FROM memory_revision
			WHERE global_id=? AND revision=?`, r.GlobalID, r.Revision-1).Scan(&prevHash); err != nil && err != sql.ErrNoRows {
			return err
		}
	}
	body, err := json.Marshal(memorySnapshot(r))
	if err != nil {
		return err
	}
	var server any
	if serverRevision > 0 {
		server = serverRevision
	}
	_, err = g.Exec(`INSERT INTO memory_revision(record_id,global_id,revision,server_revision,author,actor_source,
		content_hash,prev_hash,changed,body,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(global_id,revision) DO UPDATE SET
		  record_id=excluded.record_id, server_revision=excluded.server_revision,
		  author=excluded.author, actor_source=excluded.actor_source,
		  content_hash=excluded.content_hash, prev_hash=excluded.prev_hash,
		  changed=excluded.changed, body=excluded.body`,
		r.ID, r.GlobalID, r.Revision, server, actor.AuthorType+":"+actor.AuthorID, actor.ActorSource,
		r.ContentHash, prevHash, "record", string(body), now)
	return err
}

// indexMemoryThroughTx maintains the governed entity, its classification state,
// and the FTS document inside the caller's transaction (RT-2 fold: the Phase 6
// helpers gained WithTx variants; this composes them).
func indexMemoryThroughTx(g *GovTx, r MemoryRecord, dets []engine.Detector) error {
	id := engine.EntityID("memory", r.ID)
	if err := g.UpsertEntity(id, "memory", r.ID, r.UpdatedAt); err != nil {
		return err
	}
	if dets != nil {
		ev := engine.Event{Text: r.Title + "\n" + r.Body, Role: "memory"}
		for _, t := range engine.Classify(ev, dets) {
			if err := g.UpsertEntityState(id, engine.FactFromTag(t), r.UpdatedAt); err != nil {
				return err
			}
		}
	}
	return replaceOwnedSearchDocuments(g, "memory", "memory", engine.EntityID("memory", r.ID), []SearchDocument{{
		Order: 0, Timestamp: strconv.FormatInt(r.UpdatedAt, 10),
		Kind: "memory", Text: r.Title + "\n" + r.Body, Lineage: "memory:" + r.ID,
	}})
}

// enqueueMemoryOutbox adds the sync-outbox row in the SAME transaction — only for a
// record that passes decision 14's gate (active, shared, a scope that can travel), and
// naming the revision it carries, so the drain encodes THAT revision and two queued
// offline edits push as two revisions (decision 1). A write whose content equals the
// last synced projection with nothing queued enqueues nothing (U-2): re-promoting an
// unchanged record, or a status flip after a landing, has nothing to send — but an
// A→B→A edit with B still queued does send A, or the team would be left at B.
func enqueueMemoryOutbox(g *GovTx, r MemoryRecord) error {
	if !MemoryShareable(r) {
		return nil
	}
	if r.SyncedProjectionHash != "" && MemoryProjectionHash(r) == r.SyncedProjectionHash {
		n, err := unackedMemoryRows(g, r.GlobalID)
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
	}
	return enqueueOutboxRow(g, outboxInsert{kind: OutboxMemory, globalID: r.GlobalID, contentHash: r.ContentHash,
		scope: string(r.ScopeType) + ":" + r.ScopeID, at: r.UpdatedAt / int64(time.Second), revision: r.Revision})
}

// unackedMemoryRows counts a record's queued memory and tombstone rows.
func unackedMemoryRows(tx outboxTx, globalID string) (int, error) {
	var n int
	err := tx.QueryRow(`SELECT count(*) FROM sync_outbox WHERE global_id=? AND record_kind IN (?,?) AND acked_at IS NULL`,
		globalID, OutboxMemory, OutboxTombstone).Scan(&n)
	return n, err
}

// ---------- owner tags (plan §6: the session grammar, one more record kind) ----------

// MemoryOwnerTag is one active owner tag on a memory record.
type MemoryOwnerTag struct {
	RecordID  string
	Key       string
	Value     string
	AppliedAt int64
}

// MemoryOwnerTagUse is one distinct tag with how much it is used.
type MemoryOwnerTagUse struct {
	Key           string `json:"key,omitempty"`
	Value         string `json:"value"`
	Records       int    `json:"records"`
	LastAppliedAt int64  `json:"last_applied_at"`
}

// ChangeMemoryOwnerTags takes tags off and puts tags on, for one record, in
// one transaction — the same grammar, folding, validation and row shape as
// session owner tags (the shared functions in session_owner_tag.go; a second
// tag system is exactly what the plan forbids). Every tag is validated before
// anything is written.
func (ix *Index) ChangeMemoryOwnerTags(recordID string, apply, retract []SessionOwnerTagValue, now int64) error {
	if recordID == "" {
		return fmt.Errorf("memory id required")
	}
	if len(apply) == 0 && len(retract) == 0 {
		return fmt.Errorf("nothing to change")
	}
	for _, tag := range append(append([]SessionOwnerTagValue{}, apply...), retract...) {
		if err := ValidateSessionOwnerTag(tag); err != nil {
			return err
		}
	}
	g, err := ix.BeginGov()
	if err != nil {
		return err
	}
	defer func() { _ = g.Rollback() }()
	for _, tag := range retract {
		key := FoldSessionOwnerTagPart(tag.Key)
		value := FoldSessionOwnerTagPart(tag.Value)
		if _, err := g.Exec(`UPDATE memory_owner_tag SET retracted_at=?
			WHERE record_id=? AND key_fold=? AND value_fold=? AND retracted_at=0`,
			now, recordID, key, value); err != nil {
			return err
		}
	}
	for _, tag := range apply {
		id, err := newMemoryOwnerTagID()
		if err != nil {
			return err
		}
		key := FoldSessionOwnerTagPart(tag.Key)
		value := FoldSessionOwnerTagPart(tag.Value)
		if _, err := g.Exec(`INSERT INTO memory_owner_tag(tag_id,record_id,key,value,key_fold,value_fold,applied_at,retracted_at)
			VALUES(?,?,?,?,?,?,?,0)
			ON CONFLICT(record_id,key_fold,value_fold,applied_at) DO NOTHING`,
			id, recordID, tag.Key, tag.Value, key, value, now); err != nil {
			return err
		}
	}
	return g.Commit()
}

// MemoryOwnerTags returns a record's active owner tags, oldest first.
func (ix *Index) MemoryOwnerTags(recordID string) ([]MemoryOwnerTag, error) {
	rows, err := ix.db.Query(`SELECT record_id,key,value,applied_at FROM memory_owner_tag
		WHERE record_id=? AND retracted_at=0 ORDER BY applied_at`, recordID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemoryOwnerTag
	for rows.Next() {
		var t MemoryOwnerTag
		if err := rows.Scan(&t.RecordID, &t.Key, &t.Value, &t.AppliedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// MemoryOwnerTagUses lists the distinct owner tags over memory records with
// their counts — find_by_tag's vocabulary for records.
func (ix *Index) MemoryOwnerTagUses() ([]MemoryOwnerTagUse, error) {
	rows, err := ix.db.Query(`SELECT key,value,count(*),max(applied_at) FROM memory_owner_tag
		WHERE retracted_at=0 GROUP BY key,value ORDER BY max(applied_at) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemoryOwnerTagUse
	for rows.Next() {
		var u MemoryOwnerTagUse
		if err := rows.Scan(&u.Key, &u.Value, &u.Records, &u.LastAppliedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// RecordsByMemoryOwnerTag lists record ids carrying one owner tag.
func (ix *Index) RecordsByMemoryOwnerTag(key, value string) ([]string, error) {
	rows, err := ix.db.Query(`SELECT record_id FROM memory_owner_tag
		WHERE key_fold=? AND value_fold=? AND retracted_at=0 ORDER BY applied_at`,
		FoldSessionOwnerTagPart(key), FoldSessionOwnerTagPart(value))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// newMemoryOwnerTagID mirrors newSessionOwnerTagID: random, "mown_" prefix so
// memory owner tags are tellable apart from session ones in a log or export.
func newMemoryOwnerTagID() (string, error) {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "mown_" + hex.EncodeToString(raw[:]), nil
}
