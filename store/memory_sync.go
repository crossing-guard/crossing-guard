package store

// Memory by scope across the team — the store half. The memory write
// owner (memory.go) stays the one writer: a pulled revision lands through writeMemoryTx's
// landing mode, a push answer settles here in ONE transaction with its outbox
// acknowledgement, and the drain's encoder reads the QUEUED revision, never the record's
// current state. This file is exported (not internal to the daemon) so the server's
// real-Postgres simulations drive the same code a device runs.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"crossing-guard/engine"
	"crossing-guard/teamwire"
)

// MemorySyncEffects is what a sync transition changed on this device: records the caller
// re-mirrors (landed) or removes from the mirror (removed, by slug), and counts.
type MemorySyncEffects struct {
	Landed    []MemoryRecord
	Removed   []string
	Held      int
	Ignored   int
	Conflicts int
	Discarded int // local edits a pulled deletion displaced (each kept as a conflict copy)
	Shadowed  int
	Aliased   int
	// Unlandable counts pulled rows this device could not land (a record its own rules
	// refuse); each is skipped so it cannot hold the rows behind it (PW-M7).
	Unlandable int
	// DeletionsRefused counts deletions the team refused; the record returns by pull.
	DeletionsRefused int
}

func (e *MemorySyncEffects) add(o MemorySyncEffects) {
	e.Landed = append(e.Landed, o.Landed...)
	e.Removed = append(e.Removed, o.Removed...)
	e.Held += o.Held
	e.Ignored += o.Ignored
	e.Conflicts += o.Conflicts
	e.Discarded += o.Discarded
	e.Shadowed += o.Shadowed
	e.Aliased += o.Aliased
	e.Unlandable += o.Unlandable
	e.DeletionsRefused += o.DeletionsRefused
}

// MemoryByGlobalID loads one record by its wire id. ok is false when none exists.
func (ix *Index) MemoryByGlobalID(globalID string) (MemoryRecord, bool, error) {
	return memoryByGlobalIDTx(ix.db, globalID)
}

type queryRower interface {
	QueryRow(query string, args ...any) *sql.Row
}

func memoryByGlobalIDTx(q queryRower, globalID string) (MemoryRecord, bool, error) {
	r, err := scanMemoryRecord(q.QueryRow(`SELECT `+memoryRecordCols+` FROM memory_record WHERE global_id=?`, globalID))
	if err == sql.ErrNoRows {
		return r, false, nil
	}
	return r, err == nil, err
}

// ---------- the drain's encoder ----------

// MemoryPushRecord renders one memory or tombstone outbox row for the wire, or names why
// it cannot leave (decisions 1, 13a, 14):
//   - a memory row naming no revision (the pre-44 backlog) is not_shareable — it is never
//     encoded from current state (K-6);
//   - the record must still pass the gate NOW (state may have changed since enqueue:
//     share withdrawn, scope narrowed, status changed): otherwise not_shareable;
//   - the QUEUED revision is read by (global id, revision); missing is source_missing;
//   - the body is made portable and redacted (criterion 8), the base is the record's
//     pushed_hash, and the result is frozen on the row in its own transaction BEFORE the
//     request leaves, so a retry is byte-identical whatever changes (Q-4).
//
// orgID is the organization this device is linked to. A tombstone row carries its body
// from enqueue.
func (ix *Index) MemoryPushRecord(row OutboxRow, dets []engine.Detector, orgID string) (teamwire.PushRecord, string, error) {
	switch row.Kind {
	case OutboxTombstone:
		if row.SentWireBody == "" {
			return teamwire.PushRecord{}, teamwire.CodeSourceMissing, nil
		}
		var t teamwire.Tombstone
		if err := json.Unmarshal([]byte(row.SentWireBody), &t); err != nil || t.ID == "" {
			return teamwire.PushRecord{}, "encode_failed", nil
		}
		body := []byte(row.SentWireBody)
		return teamwire.PushRecord{Kind: teamwire.KindTombstone, ID: t.ID, ContentHash: teamwire.ContentHash(body), Body: body}, "", nil
	case OutboxMemory:
	default:
		return teamwire.PushRecord{}, "unknown_kind", nil
	}
	if row.Revision <= 0 {
		return teamwire.PushRecord{}, teamwire.CodeNotShareable, nil
	}
	cur, ok, err := ix.MemoryByGlobalID(row.GlobalID)
	if err != nil {
		return teamwire.PushRecord{}, "", err
	}
	if !ok {
		return teamwire.PushRecord{}, teamwire.CodeSourceMissing, nil
	}
	if !MemoryShareable(cur) {
		return teamwire.PushRecord{}, teamwire.CodeNotShareable, nil
	}
	// An organization record travels only to ITS organization: one left from an earlier
	// link to another team is refused here, before any body is built (PW-M3).
	if cur.ScopeType == MemoryScopeOrganization && cur.ScopeID != orgID {
		return teamwire.PushRecord{}, teamwire.CodeNotShareable, nil
	}
	body := row.SentWireBody
	if body == "" {
		var snap string
		var revAt int64
		err := ix.db.QueryRow(`SELECT body, created_at FROM memory_revision WHERE global_id=? AND revision=?`, row.GlobalID, row.Revision).Scan(&snap, &revAt)
		if err == sql.ErrNoRows {
			return teamwire.PushRecord{}, teamwire.CodeSourceMissing, nil
		}
		if err != nil {
			return teamwire.PushRecord{}, "", err
		}
		var shape memoryWireShape
		if err := json.Unmarshal([]byte(snap), &shape); err != nil {
			return teamwire.PushRecord{}, "encode_failed", nil
		}
		rec := wireFromSnapshot(shape, cur, revAt, dets)
		rec.BaseContentHash = cur.PushedHash
		rec.ContentHash = teamwire.MemoryWireHash(rec)
		raw, err := json.Marshal(rec)
		if err != nil {
			return teamwire.PushRecord{}, "", err
		}
		// The freeze takes only a row still waiting to be sent (FR-5): a deletion or a
		// narrowing in another process may have acknowledged it between the gate read
		// above and this write, and a row acknowledged there must never be sent.
		if _, err := ix.db.Exec(`UPDATE sync_outbox SET sent_wire_body=?, sent_wire_hash=? WHERE seq=? AND sent_wire_body='' AND acked_at IS NULL`,
			string(raw), rec.ContentHash, row.Seq); err != nil {
			return teamwire.PushRecord{}, "", err
		}
		switch err := ix.db.QueryRow(`SELECT sent_wire_body FROM sync_outbox WHERE seq=? AND acked_at IS NULL`, row.Seq).Scan(&body); {
		case err == sql.ErrNoRows:
			return teamwire.PushRecord{}, teamwire.CodeSuperseded, nil
		case err != nil:
			return teamwire.PushRecord{}, "", err
		}
		if body == "" {
			return teamwire.PushRecord{}, teamwire.CodeSuperseded, nil
		}
	}
	return teamwire.PushRecord{Kind: teamwire.KindMemory, ID: teamwire.MemoryPushID(row.GlobalID, row.Revision),
		ContentHash: teamwire.ContentHash([]byte(body)), Body: json.RawMessage(body)}, "", nil
}

// wireFromSnapshot builds the wire record from a revision snapshot: the snapshot's
// content, made portable (paths) and redacted (the shipped secret patterns), under the
// team's slug (decision 15's alias maps back), with the record's identity and creation
// time from current state.
func wireFromSnapshot(shape memoryWireShape, cur MemoryRecord, revAt int64, dets []engine.Detector) teamwire.MemoryRecord {
	clean := func(s string) string {
		t, _ := engine.RedactText(engine.PortableText(s, ""), dets)
		return t
	}
	list := func(vs []string) []string {
		out, seen := []string{}, map[string]bool{}
		for _, v := range vs {
			if c := clean(v); c != "" && !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
		return out
	}
	slug := shape.Slug
	if cur.WireSlug != "" {
		slug = cur.WireSlug
	}
	rec := teamwire.MemoryRecord{SchemaVersion: teamwire.MemorySchemaVersion, ID: shape.ID, Slug: slug, Revision: shape.Revision,
		Scope: teamwire.MemoryScope{Type: shape.ScopeType, ID: shape.ScopeID},
		Title: clean(shape.Title), Category: shape.Category, Body: clean(shape.Body),
		Tags: list(shape.Tags), Aliases: list(shape.Aliases), Source: shape.Source, Origin: clean(shape.Origin),
		SupersededBy: optionalString(shape.SupersededBy), VerifiedAt: wireTime(shape.VerifiedAt), VerifiedBy: optionalString(clean(shape.VerifiedBy)),
		CreatedAt: time.Unix(0, cur.CreatedAt).UTC().Format(time.RFC3339), UpdatedAt: time.Unix(0, revAt).UTC().Format(time.RFC3339),
		// The author of record is the member the server authenticates (decision 6), so the
		// wire carries no local account name and no session id: a fixed value (CR-14).
		Author: teamwire.Actor{Type: WireActorType(shape.AuthorType), ID: teamwire.WireAuthorID}}
	if shape.ScopeType == string(MemoryScopeRepository) {
		rec.RepositoryIdentity = cur.RepositoryIdentity
	}
	return rec
}

func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// wireTime renders a local verification stamp as the wire's date-time, or nil when it is
// not a time (a free-text legacy stamp stays on the device).
func wireTime(s string) *string {
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			out := t.UTC().Format(time.RFC3339)
			return &out
		}
	}
	return nil
}

// ---------- push settlement (decision 13b) ----------

// MemorySettle is one memory or tombstone row's answer. Code is accepted, duplicate,
// stale_base, or the refusal code (the server's, or this device's own: not_shareable,
// source_missing, internal_exhausted, …).
type MemorySettle struct {
	Seq            int64
	Code           string
	ServerRevision int64
	Current        *teamwire.PulledMemory // stale_base: the revision the server holds
}

// SettleMemoryPush applies one answer in ONE transaction with the row's acknowledgement,
// so no crash leaves the sync state and the outbox disagreeing (K-3):
//   - accepted / duplicate: pushed_hash = the frozen wire hash, server_revision = the
//     answered number, synced_projection_hash = the SENT revision's projection;
//   - stale_base: the local current body becomes a conflict copy, every later queued
//     memory row of the record is acknowledged superseded (never a tombstone row — a
//     local delete still propagates), and the server's carried revision LANDS, resetting
//     the sync state (K-1: no pull needed to recover);
//   - any other answer: acknowledgement only; the sync state does not move.
//
// Then, for every outcome, a held pulled revision re-applies through decision 5's rules,
// after the carried revision (S-4), so a held revision no newer is discarded by rule 1.
// A settle for a row already acknowledged, or for a record that no longer exists, writes
// nothing beyond the acknowledgement — it can never re-create a deleted record (S-5).
func (ix *Index) SettleMemoryPush(s MemorySettle, at int64) (MemorySyncEffects, error) {
	var eff MemorySyncEffects
	g, err := ix.BeginGov()
	if err != nil {
		return eff, err
	}
	defer func() { _ = g.Rollback() }()
	var kind, globalID, sentHash string
	var revision int64
	var acked sql.NullInt64
	err = g.QueryRow(`SELECT record_kind, global_id, revision, sent_wire_hash, acked_at FROM sync_outbox WHERE seq=?`, s.Seq).
		Scan(&kind, &globalID, &revision, &sentHash, &acked)
	if err == sql.ErrNoRows || (err == nil && acked.Valid) {
		return eff, nil
	}
	if err != nil {
		return eff, err
	}
	// The frozen wire body has done its work once the row is settled: it is blanked with
	// the acknowledgement (PW-M2). What a deletion erases on this device is the record, its
	// revisions, its frozen wire bodies, its search rows and its held slot; conflict copies
	// are KEPT by design (R2-M5a), and a legacy git history in the mirror is not rewritten.
	if _, err := g.Exec(`UPDATE sync_outbox SET acked_at=?, ack_code=?, sent_wire_body='' WHERE seq=? AND acked_at IS NULL`, at, s.Code, s.Seq); err != nil {
		return eff, err
	}
	if kind == OutboxTombstone && scopeOf(g, s.Seq) == teamwire.TombstoneMemory && !MemoryDeletionAccepted(s.Code) {
		// The team did not take this deletion — refused (not this member's to delete),
		// or never delivered (the server kept failing to store it, it could not be
		// encoded): the team may still hold the record, so this device must not stay
		// without it behind a tombstone that blocks every pull. The local tombstone goes
		// and the pull starts over — landing is idempotent, and whatever the team holds
		// comes back (PW-M5, CR-3: one predicate for the recovery and for the count).
		if _, err := g.Exec(`DELETE FROM memory_tombstone WHERE global_id=? AND origin='local'`, globalID); err != nil {
			return eff, err
		}
		if err := restartMemoryPullTx(g); err != nil {
			return eff, err
		}
		eff.DeletionsRefused++
		return eff, g.Commit()
	}
	rec, ok, err := memoryByGlobalIDTx(g, globalID)
	if err != nil {
		return eff, err
	}
	if kind != OutboxMemory || !ok {
		return eff, g.Commit()
	}
	switch s.Code {
	case teamwire.StatusAccepted, teamwire.StatusDuplicate:
		projection := ""
		var snap string
		switch err := g.QueryRow(`SELECT body FROM memory_revision WHERE global_id=? AND revision=?`, globalID, revision).Scan(&snap); {
		case err == nil:
			var shape memoryWireShape
			if json.Unmarshal([]byte(snap), &shape) == nil {
				projection = MemoryProjectionHash(memoryFromSnapshot(shape))
			}
		case err != sql.ErrNoRows:
			return eff, err
		}
		if _, err := g.Exec(`UPDATE memory_record SET pushed_hash=?, server_revision=?, synced_projection_hash=? WHERE global_id=?`,
			sentHash, s.ServerRevision, projection, globalID); err != nil {
			return eff, err
		}
		if _, err := g.Exec(`UPDATE memory_revision SET server_revision=? WHERE global_id=? AND revision=?`, s.ServerRevision, globalID, revision); err != nil {
			return eff, err
		}
	case teamwire.CodeStaleBase:
		by := ""
		if s.Current != nil {
			by = authorName(*s.Current)
		}
		if err := saveConflictTx(g, rec, "stale_base", by); err != nil {
			return eff, err
		}
		eff.Conflicts++
		if _, err := g.Exec(`UPDATE sync_outbox SET acked_at=?, ack_code=? WHERE global_id=? AND record_kind=? AND acked_at IS NULL AND seq > ?`,
			at, teamwire.CodeSuperseded, globalID, OutboxMemory, s.Seq); err != nil {
			return eff, err
		}
		if s.Current != nil && s.Current.WireHash != rec.PushedHash {
			landed, err := landPulledTx(g, *s.Current, landDirect)
			if err != nil {
				return eff, err
			}
			if landed.Unlandable > 0 {
				// This build cannot land the team's current revision (a newer build's
				// record). The base still moves to it, or every later push would answer
				// stale_base forever; the local content stays, diverged (CR-4).
				if _, err := g.Exec(`UPDATE memory_record SET pushed_hash=?, server_revision=? WHERE global_id=?`,
					s.Current.WireHash, s.Current.ServerRevision, globalID); err != nil {
					return eff, err
				}
			}
			eff.add(landed)
		} else if s.Current == nil {
			// The server holds no revision for a base this device named: the next edit is
			// a first appearance.
			if _, err := g.Exec(`UPDATE memory_record SET pushed_hash='', server_revision=0, synced_projection_hash='' WHERE global_id=?`, globalID); err != nil {
				return eff, err
			}
		}
	}
	held, err := reapplyHeldTx(g, globalID)
	if err != nil {
		return eff, err
	}
	eff.add(held)
	return eff, g.Commit()
}

// scopeOf reads an outbox row's scope (” when unreadable).
func scopeOf(g *GovTx, seq int64) string {
	var scope string
	_ = g.QueryRow(`SELECT scope FROM sync_outbox WHERE seq=?`, seq).Scan(&scope)
	return scope
}

// MemoryDeletionAccepted is the ONE test of whether the team took a memory deletion: an
// accepting answer, or this device's own supersede. Every other settled answer — a
// refusal by name, an exhausted retry, a row that could not be sent — means it did not,
// and is both recovered from and counted by this same predicate (CR-3).
func MemoryDeletionAccepted(code string) bool {
	return code == teamwire.StatusAccepted || code == teamwire.StatusDuplicate || code == teamwire.CodeSuperseded
}

func authorName(p teamwire.PulledMemory) string {
	if p.AuthorName != "" {
		return p.AuthorName
	}
	return p.AuthorUserID
}

// memoryFromSnapshot turns a revision snapshot back into the local record fields the
// projection covers.
func memoryFromSnapshot(s memoryWireShape) MemoryRecord {
	return MemoryRecord{ID: s.Slug, GlobalID: s.ID, Revision: s.Revision, ScopeType: MemoryScopeType(s.ScopeType), ScopeID: s.ScopeID,
		Title: s.Title, Category: s.Category, Body: s.Body, Tags: nonNilStrings(s.Tags), Aliases: nonNilStrings(s.Aliases),
		Source: s.Source, Origin: s.Origin, SupersededBy: s.SupersededBy, VerifiedAt: s.VerifiedAt, VerifiedBy: s.VerifiedBy,
		AuthorType: s.AuthorType, AuthorID: s.AuthorID}
}

func saveConflictTx(g *GovTx, r MemoryRecord, reason, by string) error {
	body, err := json.Marshal(memorySnapshot(r))
	if err != nil {
		return err
	}
	_, err = g.Exec(`INSERT INTO memory_conflict(global_id,record_id,reason,local_revision,body,by_author,created_at) VALUES(?,?,?,?,?,?,?)`,
		r.GlobalID, r.ID, reason, r.Revision, string(body), by, time.Now().UnixNano())
	return err
}

// reapplyHeldTx clears a record's held pulled revision and runs it through decision 5's
// rules — the step every settle ends with, so no answer type can strand it (Q-2).
func reapplyHeldTx(g *GovTx, globalID string) (MemorySyncEffects, error) {
	var raw string
	err := g.QueryRow(`SELECT held_wire_record FROM memory_record WHERE global_id=?`, globalID).Scan(&raw)
	if err == sql.ErrNoRows || (err == nil && raw == "") {
		return MemorySyncEffects{}, nil
	}
	if err != nil {
		return MemorySyncEffects{}, err
	}
	if _, err := g.Exec(`UPDATE memory_record SET held_wire_record='', held_server_revision=0 WHERE global_id=?`, globalID); err != nil {
		return MemorySyncEffects{}, err
	}
	var held teamwire.PulledMemory
	if err := json.Unmarshal([]byte(raw), &held); err != nil {
		return MemorySyncEffects{Unlandable: 1}, nil // cleared above; it cannot wedge the record
	}
	return landPulledTx(g, held, landRules)
}

// ---------- landing pulled records (decision 5) ----------

type landMode int

const (
	landRules  landMode = iota // decision 5 rules 1–4
	landDirect                 // a stale_base answer's carried revision: rule 1, then land
)

// ApplyPulledMemory lands one pulled record revision in its own transaction.
func (ix *Index) ApplyPulledMemory(p teamwire.PulledMemory) (MemorySyncEffects, error) {
	return ix.ApplyPulledRows([]teamwire.PullRow{{Kind: teamwire.KindMemory, Memory: &p}}, 1, "")
}

// ApplyPulledTombstone applies one pulled deletion in its own transaction.
func (ix *Index) ApplyPulledTombstone(t teamwire.PulledTombstone) (MemorySyncEffects, error) {
	return ix.ApplyPulledRows([]teamwire.PullRow{{Kind: teamwire.KindTombstone, Tombstone: &t}}, 1, "")
}

// ErrLinkChanged refuses landing a page pulled under a link that is no longer this
// device's: the device unlinked, or is linked to another organization.
var ErrLinkChanged = errors.New("the team link changed; the pulled page was not landed")

// ApplyPulledRows applies a page's rows in order, batch records per SQLite transaction
// (pull_apply_batch; never a whole page in one, R2-L3). It never enqueues. orgID names
// the organization the page was pulled from: each transaction checks, INSIDE itself, that
// the device is still linked to it, so a page in flight across an unlink lands nothing
// (CR-10). "" skips the check (a caller that is not a pull job).
func (ix *Index) ApplyPulledRows(rows []teamwire.PullRow, batch int, orgID string) (MemorySyncEffects, error) {
	var eff MemorySyncEffects
	if batch <= 0 {
		batch = 1
	}
	for start := 0; start < len(rows); start += batch {
		end := min(start+batch, len(rows))
		g, err := ix.BeginGov()
		if err != nil {
			return eff, err
		}
		if orgID != "" {
			if linked, err := linkedOrganizationTx(g); err != nil || linked != orgID {
				_ = g.Rollback()
				if err == nil {
					err = ErrLinkChanged
				}
				return eff, err
			}
		}
		var chunk MemorySyncEffects
		for _, row := range rows[start:end] {
			var one MemorySyncEffects
			switch {
			case row.Memory != nil:
				one, err = landPulledTx(g, *row.Memory, landRules)
			case row.Tombstone != nil:
				one, err = applyPulledTombstoneTx(g, *row.Tombstone)
			}
			if err != nil {
				_ = g.Rollback()
				return eff, err
			}
			chunk.add(one)
		}
		if err := g.Commit(); err != nil {
			return eff, err
		}
		eff.add(chunk)
	}
	return eff, nil
}

// landPulledTx is decision 5's landing rule, in order:
//  1. a server revision no newer than the record's is ignored (a lagging page, K-7);
//     so is a revision whose wire hash is the record's base (its own push, pulled back:
//     decision 13c's idempotency);
//  2. a record with an unacked memory or tombstone row HOLDS the revision (a newer held
//     revision replaces an older); every settle re-applies it (Q-2, S-5);
//  3. a DIVERGED record — its current projection differs from the synced one: a pending,
//     rejected or otherwise unsent local edit — keeps its body as a conflict copy;
//  4. land, through the one write's landing mode.
//
// A record deleted on this device is never re-created by a pull.
func landPulledTx(g *GovTx, p teamwire.PulledMemory, mode landMode) (MemorySyncEffects, error) {
	var eff MemorySyncEffects
	w := p.Record
	var tombstoned int
	if err := g.QueryRow(`SELECT count(*) FROM memory_tombstone WHERE global_id=?`, w.ID).Scan(&tombstoned); err != nil {
		return eff, err
	}
	if tombstoned > 0 {
		eff.Ignored++
		return eff, nil
	}
	rec, exists, err := memoryByGlobalIDTx(g, w.ID)
	if err != nil {
		return eff, err
	}
	incoming := memoryFromWire(w)
	// A row this device's own rules refuse (the wire schema is looser than the store:
	// a blank title, a slug outside the id grammar) is skipped and counted, never an
	// error: one bad row must not hold every row behind it, deletions included (PW-M7).
	probe := incoming
	probe.AuthorType, probe.AuthorID = "user", "team"
	if !memoryIDPattern.MatchString(w.Slug) || ValidateMemoryRecord(probe) != nil {
		eff.Unlandable++
		return eff, nil
	}
	land := memoryLanding{wire: p}
	if exists {
		// Rule 1. A stale_base answer's carried revision is the server's CURRENT one by
		// definition, so it lands whenever its hash differs from this device's base,
		// whatever the revision numbers say (PW-L4).
		same := p.WireHash != "" && p.WireHash == rec.PushedHash
		if same || (mode == landRules && p.ServerRevision <= rec.ServerRevision) {
			eff.Ignored++
			return eff, nil
		}
		if mode == landRules {
			n, err := unackedMemoryRows(g, w.ID)
			if err != nil {
				return eff, err
			}
			if n > 0 {
				raw, err := json.Marshal(p)
				if err != nil {
					return eff, err
				}
				if _, err := g.Exec(`UPDATE memory_record SET held_wire_record=?, held_server_revision=? WHERE global_id=? AND held_server_revision < ?`,
					string(raw), p.ServerRevision, w.ID, p.ServerRevision); err != nil {
					return eff, err
				}
				eff.Held++
				return eff, nil
			}
			incoming.ID = rec.ID
			if current := MemoryProjectionHash(rec); current != rec.SyncedProjectionHash && current != MemoryProjectionHash(incoming) {
				if err := saveConflictTx(g, rec, "pulled_over_edit", authorName(p)); err != nil {
					return eff, err
				}
				eff.Conflicts++
			}
		}
		incoming.ID = rec.ID
		incoming.CreatedAt = rec.CreatedAt
	} else {
		local, wireSlug, collision, err := resolvePulledSlugTx(g, w.Slug, w.ID)
		if err != nil {
			return eff, err
		}
		incoming.ID = local
		land.wireSlug, land.collision = wireSlug, collision
		switch collision {
		case "shadowed":
			eff.Shadowed++
		case "alias":
			eff.Aliased++
		}
	}
	incoming.AuthorType, incoming.AuthorID = "user", p.AuthorUserID
	if incoming.AuthorID == "" {
		incoming.AuthorID = w.Author.ID
	}
	saved, err := writeMemoryTx(g, incoming, nil, nil, MemoryActor{AuthorType: "user", AuthorID: incoming.AuthorID, ActorSource: "team"},
		memoryWrite{mode: memoryUpsert, landing: &land})
	if err != nil {
		return eff, err
	}
	eff.Landed = append(eff.Landed, saved)
	return eff, nil
}

// memoryFromWire is the local record a wire revision lands as (its slug is resolved by
// the caller; its author is the server-stamped one).
func memoryFromWire(w teamwire.MemoryRecord) MemoryRecord {
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	r := MemoryRecord{ID: w.Slug, GlobalID: w.ID, Status: "active",
		ScopeType: MemoryScopeType(w.Scope.Type), ScopeID: w.Scope.ID, RepositoryIdentity: w.RepositoryIdentity,
		Title: w.Title, Category: w.Category, Body: w.Body, Tags: nonNilStrings(w.Tags), Aliases: nonNilStrings(w.Aliases),
		Source: w.Source, Origin: w.Origin, SupersededBy: deref(w.SupersededBy), VerifiedAt: deref(w.VerifiedAt), VerifiedBy: deref(w.VerifiedBy)}
	if r.RepositoryIdentity == "" {
		r.RepositoryIdentity = "weak"
	}
	if t, err := time.Parse(time.RFC3339, w.CreatedAt); err == nil {
		r.CreatedAt = t.UnixNano()
	}
	return r
}

// resolvePulledSlugTx keeps a team slug or, when a local record already holds it, lands
// under a local-only alias "<slug>-<8 chars of the global id>" that the encoder maps back
// (decision 15). Colliding with a USER record also shadows the pulled one from recall.
func resolvePulledSlugTx(g *GovTx, slug, globalID string) (local, wireSlug, collision string, err error) {
	var scope string
	err = g.QueryRow(`SELECT scope_type FROM memory_record WHERE id=?`, slug).Scan(&scope)
	if err == sql.ErrNoRows {
		return slug, "", "", nil
	}
	if err != nil {
		return "", "", "", err
	}
	collision = "alias"
	if scope == string(MemoryScopeUser) {
		collision = "shadowed"
	}
	local, err = freeAliasTx(g, slug, globalID)
	return local, slug, collision, err
}

// freeAliasTx is a local id no record holds, derived from a taken one:
// "<slug>-<8 chars of the global id>", numbered when even that is taken.
func freeAliasTx(g *GovTx, slug, globalID string) (string, error) {
	suffix := strings.ToLower(globalID)
	if len(suffix) > 8 {
		suffix = suffix[len(suffix)-8:]
	}
	base := slug
	if len(base) > 88 {
		base = base[:88]
	}
	base = strings.TrimRight(base, "-")
	for i := 0; ; i++ {
		candidate := base + "-" + suffix
		if i > 0 {
			candidate = fmt.Sprintf("%s-%s-%d", base, suffix, i)
		}
		var n int
		if err := g.QueryRow(`SELECT count(*) FROM memory_record WHERE id=?`, candidate).Scan(&n); err != nil {
			return "", err
		}
		if n == 0 {
			return candidate, nil
		}
	}
}

// applyPulledTombstoneTx applies a teammate's deletion at once (tombstones do not wait
// on rule 2, decision 8): an unacked or unsent local edit is kept as a conflict copy
// ("edit discarded: deleted by …", R2-M5a), the record's queued memory rows are
// acknowledged superseded, the record leaves store and index, and — O-7 — its revision
// rows are erased. The tombstone is recorded so no later pull re-creates the record.
func applyPulledTombstoneTx(g *GovTx, t teamwire.PulledTombstone) (MemorySyncEffects, error) {
	var eff MemorySyncEffects
	if t.Tombstone.RecordType != teamwire.TombstoneMemory {
		eff.Ignored++ // session-content tombstones are never pulled (C-8); defensive
		return eff, nil
	}
	globalID := t.Tombstone.RecordID
	by := t.DeletedByName
	if by == "" {
		by = t.DeletedByUserID
	}
	rec, exists, err := memoryByGlobalIDTx(g, globalID)
	if err != nil {
		return eff, err
	}
	now := time.Now().UnixNano()
	slug, scopeType, scopeID := "", "", ""
	if exists {
		slug, scopeType, scopeID = rec.ID, string(rec.ScopeType), rec.ScopeID
		n, err := unackedMemoryRows(g, globalID)
		if err != nil {
			return eff, err
		}
		if n > 0 || MemoryProjectionHash(rec) != rec.SyncedProjectionHash {
			if err := saveConflictTx(g, rec, "deleted", by); err != nil {
				return eff, err
			}
			eff.Discarded++
		}
		if _, err := g.Exec(`UPDATE sync_outbox SET acked_at=?, ack_code=? WHERE global_id=? AND record_kind=? AND acked_at IS NULL`,
			now/int64(time.Second), teamwire.CodeSuperseded, globalID, OutboxMemory); err != nil {
			return eff, err
		}
		if err := removeMemoryRecordTx(g, rec); err != nil {
			return eff, err
		}
		eff.Removed = append(eff.Removed, rec.ID)
	}
	if _, err := g.Exec(`DELETE FROM memory_revision WHERE global_id=?`, globalID); err != nil {
		return eff, err
	}
	if err := eraseSentBodiesTx(g, globalID); err != nil {
		return eff, err
	}
	deletedAt := now
	if at, err := time.Parse(time.RFC3339, t.Tombstone.DeletedAt); err == nil {
		deletedAt = at.UnixNano()
	}
	_, err = g.Exec(`INSERT INTO memory_tombstone(global_id,id,deleted_at,deleted_by,prior_content_hash,scope_type,scope_id,reason_class,origin)
		VALUES(?,?,?,?,?,?,?,?,'pulled') ON CONFLICT(global_id) DO NOTHING`,
		globalID, slug, deletedAt, by, t.Tombstone.PriorContentHash, scopeType, scopeID, t.Tombstone.ReasonClass)
	return eff, err
}

// ---------- the pull cursor ----------

// SyncCursor reads a pull cursor (the existing sync_cursor table; one per scope, e.g.
// "memory"). Zero when none is recorded.
func (ix *Index) SyncCursor(scope string) (int64, error) {
	var c string
	err := ix.db.QueryRow(`SELECT cursor FROM sync_cursor WHERE scope=?`, scope).Scan(&c)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var n int64
	_, err = fmt.Sscan(c, &n)
	return n, err
}

// SetSyncCursor advances a pull cursor; it never moves backwards.
func (ix *Index) SetSyncCursor(scope string, cursor, at int64) error {
	old, err := ix.SyncCursor(scope)
	if err != nil {
		return err
	}
	if cursor <= old {
		return nil
	}
	_, err = ix.db.Exec(`INSERT INTO sync_cursor(scope,cursor,updated_at) VALUES(?,?,?)
		ON CONFLICT(scope) DO UPDATE SET cursor=excluded.cursor, updated_at=excluded.updated_at`, scope, fmt.Sprint(cursor), at)
	return err
}

// pullEpochScope is the sync_cursor row counting how many times the memory pull was
// told to start over. A missing cursor row reads as 0, so the cursor's value alone cannot
// tell "reset" from "never pulled": a tick that began at 0 would pass a compare-and-set
// made after a reset. The epoch moves on every reset, and the advance compares it (FR-3).
const pullEpochScope = "pull-epoch"

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func bumpPullEpoch(x execer) error {
	_, err := x.Exec(`INSERT INTO sync_cursor(scope,cursor,updated_at) VALUES(?, '1', 0)
		ON CONFLICT(scope) DO UPDATE SET cursor = CAST(CAST(cursor AS INTEGER) + 1 AS TEXT)`, pullEpochScope)
	return err
}

// restartMemoryPullTx forgets every memory pull cursor and moves the epoch, so the next
// pull starts from the beginning and no tick already in flight records its position.
func restartMemoryPullTx(x execer) error {
	if _, err := x.Exec(`DELETE FROM sync_cursor WHERE scope LIKE 'memory%'`); err != nil {
		return err
	}
	return bumpPullEpoch(x)
}

// MemoryPullEpoch reads the reset epoch a pull tick must hand back to AdvanceSyncCursor.
// Read it BEFORE the cursor.
func (ix *Index) MemoryPullEpoch() (int64, error) { return ix.SyncCursor(pullEpochScope) }

// ClearSyncCursor forgets a pull cursor, so the next pull starts from the beginning; it
// counts as a reset for any tick in flight.
func (ix *Index) ClearSyncCursor(scope string) error {
	tx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM sync_cursor WHERE scope=?`, scope); err != nil {
		return err
	}
	if err := bumpPullEpoch(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// AdvanceSyncCursor moves a pull cursor from the value the caller read to a later one, as
// one compare-and-set over the cursor AND the reset epoch the caller read before it: when
// the stored cursor is no longer `from`, or the pull was restarted since — a deletion the
// team did not take, an unlink, a build change — nothing is written and moved is false,
// so a tick already in flight can never put an old position back, even one that started
// from the beginning (CR-2, FR-3).
func (ix *Index) AdvanceSyncCursor(scope string, from, to, epoch, at int64) (moved bool, err error) {
	if to <= from {
		return false, nil
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	read := func(scope string) (int64, error) {
		var raw string
		var n int64
		switch err := tx.QueryRow(`SELECT cursor FROM sync_cursor WHERE scope=?`, scope).Scan(&raw); {
		case err == sql.ErrNoRows:
			return 0, nil
		case err != nil:
			return 0, err
		}
		_, err := fmt.Sscan(raw, &n)
		return n, err
	}
	cur, err := read(scope)
	if err != nil {
		return false, err
	}
	now, err := read(pullEpochScope)
	if err != nil {
		return false, err
	}
	if cur != from || now != epoch {
		return false, nil
	}
	if _, err := tx.Exec(`INSERT INTO sync_cursor(scope,cursor,updated_at) VALUES(?,?,?)
		ON CONFLICT(scope) DO UPDATE SET cursor=excluded.cursor, updated_at=excluded.updated_at`, scope, fmt.Sprint(to), at); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ---------- the link this device's sync state belongs to ----------

// linkScope is the sync_cursor row naming the organization this device is linked to.
// The store keeps it so that what belongs to a link — acknowledged rows, a pulled page —
// can be told from what belonged to an earlier one, inside a transaction.
const linkScope = "link"

// SetLinkedOrganization records the organization this device is linked to, and gives
// back to this link the acknowledgements it made before an unlink: a relink to the SAME
// organization keeps the device's row on the server (the key rotates), so what that
// server accepted from this device is still there and still this link's (CR-1).
func (ix *Index) SetLinkedOrganization(orgID string, at int64) error {
	tx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`INSERT INTO sync_cursor(scope,cursor,updated_at) VALUES(?,?,?)
		ON CONFLICT(scope) DO UPDATE SET cursor=excluded.cursor, updated_at=excluded.updated_at`, linkScope, orgID, at); err != nil {
		return err
	}
	prefix := priorLinkPrefix(orgID)
	if _, err := tx.Exec(`UPDATE sync_outbox SET ack_code = substr(ack_code, ?) WHERE acked_at IS NOT NULL AND substr(ack_code, 1, ?) = ?`,
		len(prefix)+1, len(prefix), prefix); err != nil {
		return err
	}
	return tx.Commit()
}

// LinkedOrganization reads the organization this device is linked to ("" when none).
func (ix *Index) LinkedOrganization() (string, error) { return linkedOrganizationTx(ix.db) }

func linkedOrganizationTx(q queryRower) (string, error) {
	var org string
	err := q.QueryRow(`SELECT cursor FROM sync_cursor WHERE scope=?`, linkScope).Scan(&org)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return org, err
}

// priorLinkPrefix marks an acknowledgement made under a link that has ended.
func priorLinkPrefix(orgID string) string { return "prior:" + orgID + ":" }

// ---------- sharing pre-existing records (decision 14) ----------

// MemoryShareCandidates lists active records at a scope that can travel which are not yet
// shared — the records created before linking, and every identity upgrade. Records
// imported from another tool (source "import") and records received from a team are
// listed separately: the bulk action excludes them until each is reviewed individually.
//
// orgID is the linked organization: an organization record of ANOTHER organization — one
// written under an earlier link — is no candidate at all; it cannot travel here (CR-8).
func (ix *Index) MemoryShareCandidates(orgID string) (bulk, imported []MemoryRecord, err error) {
	rows, err := ix.db.Query(`SELECT `+memoryRecordCols+` FROM memory_record WHERE share_state='unshared' AND status='active'
		AND ((scope_type='organization' AND scope_id=?) OR (scope_type='repository' AND repository_identity='remote-sha256')) ORDER BY updated_at DESC`, orgID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanMemoryRecord(rows)
		if err != nil {
			return nil, nil, err
		}
		// Individual review only: an imported record, and a record received from a team
		// (after an unlink it may be another organization's, PW-M3).
		if r.Source == "import" || r.SyncOrigin == "pulled" {
			imported = append(imported, r)
		} else {
			bulk = append(bulk, r)
		}
	}
	return bulk, imported, rows.Err()
}

// ErrMemoryNotShareable refuses sharing a record whose scope or status cannot travel.
var ErrMemoryNotShareable = errors.New("memory record cannot be shared")

// ShareMemory is the explicit, counted share action (decision 14): ids nil shares every
// bulk candidate (never an imported record); named ids share those records, imported
// ones included (the individual review). Each newly shared record enqueues its current
// revision. Returns how many were shared.
func (ix *Index) ShareMemory(ids []string, orgID string) (int, error) {
	var targets []MemoryRecord
	if ids == nil {
		bulk, _, err := ix.MemoryShareCandidates(orgID)
		if err != nil {
			return 0, err
		}
		targets = bulk
	} else {
		for _, id := range ids {
			r, err := ix.MemoryByID(id)
			if err == sql.ErrNoRows {
				return 0, fmt.Errorf("%w: %s", ErrMemoryNotFound, id)
			}
			if err != nil {
				return 0, err
			}
			if r.Status != "active" || !memoryScopeCanTravel(r) {
				return 0, fmt.Errorf("%w: %s is %s at %s scope (%s identity)", ErrMemoryNotShareable, id, r.Status, r.ScopeType, r.RepositoryIdentity)
			}
			if r.ScopeType == MemoryScopeOrganization && r.ScopeID != orgID {
				return 0, fmt.Errorf("%w: %s belongs to another organization", ErrMemoryNotShareable, id)
			}
			if r.ShareState != "shared" {
				targets = append(targets, r)
			}
		}
	}
	g, err := ix.BeginGov()
	if err != nil {
		return 0, err
	}
	defer func() { _ = g.Rollback() }()
	for _, r := range targets {
		if _, err := g.Exec(`UPDATE memory_record SET share_state='shared' WHERE global_id=?`, r.GlobalID); err != nil {
			return 0, err
		}
		r.ShareState = "shared"
		if err := enqueueMemoryOutbox(g, r); err != nil {
			return 0, err
		}
	}
	return len(targets), g.Commit()
}

// ---------- reading the sync state ----------

// MemoryConflict is one conflict copy.
type MemoryConflict struct {
	ID            int64  `json:"id"`
	GlobalID      string `json:"global_id"`
	RecordID      string `json:"record_id"`
	Reason        string `json:"reason"` // stale_base | pulled_over_edit | deleted
	LocalRevision int64  `json:"local_revision"`
	Title         string `json:"title"`
	Body          string `json:"body"`
	ByAuthor      string `json:"by_author"`
	CreatedAt     int64  `json:"created_at"`
}

// MemoryConflicts lists conflict copies, newest first; globalID "" lists all.
func (ix *Index) MemoryConflicts(globalID string, limit int) ([]MemoryConflict, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("conflict copies: a positive limit is required (team.json conflicts_page_max)")
	}
	q := `SELECT conflict_id, global_id, record_id, reason, local_revision, body, by_author, created_at FROM memory_conflict`
	args := []any{}
	if globalID != "" {
		q += ` WHERE global_id=?`
		args = append(args, globalID)
	}
	q += ` ORDER BY conflict_id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := ix.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemoryConflict
	for rows.Next() {
		var c MemoryConflict
		var body string
		if err := rows.Scan(&c.ID, &c.GlobalID, &c.RecordID, &c.Reason, &c.LocalRevision, &body, &c.ByAuthor, &c.CreatedAt); err != nil {
			return nil, err
		}
		var shape memoryWireShape
		if json.Unmarshal([]byte(body), &shape) == nil {
			c.Title, c.Body = shape.Title, shape.Body
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// MemorySyncSummary is the device's memory sync state in counts, for the console and
// doctor.
type MemorySyncSummary struct {
	// Shared counts records THIS device shared; Pulled the records that came from the
	// team. A pulled record is held as shared too, and is counted under Pulled only.
	Shared           int `json:"shared"`
	Pulled           int `json:"pulled"`
	Shadowed         int `json:"shadowed"`
	Aliased          int `json:"aliased"`
	Held             int `json:"held"`
	Conflicts        int `json:"conflicts"`
	DiscardedEdits   int `json:"discarded_edits"`
	WeakRepository   int `json:"weak_repository"`
	ShareCandidates  int `json:"share_candidates"`
	ImportCandidates int `json:"import_candidates"`
	NotShareable     int `json:"not_shareable"`
	Deleted          int `json:"deleted"`
	DeletedByTeam    int `json:"deleted_by_team"`
	DeletionsRefused int `json:"deletions_refused"` // deletions the team did not take (refused or never delivered); what the team holds returns
}

// MemorySyncSummaryCounts reads the summary. orgID is the linked organization (share
// candidates are counted for it, CR-8). notShareableSince (Unix seconds) bounds the
// not_shareable count to rows refused since then, so the one-off backlog a device
// carried into schema 44 ages out of the line instead of standing in it for good (CR-7).
func (ix *Index) MemorySyncSummaryCounts(orgID string, notShareableSince int64) (MemorySyncSummary, error) {
	var s MemorySyncSummary
	err := ix.db.QueryRow(`SELECT
		COALESCE(SUM(share_state='shared' AND sync_origin!='pulled'),0), COALESCE(SUM(sync_origin='pulled'),0),
		COALESCE(SUM(collision='shadowed'),0), COALESCE(SUM(collision='alias'),0), COALESCE(SUM(held_server_revision>0),0),
		COALESCE(SUM(scope_type='repository' AND repository_identity='weak'),0),
		COALESCE(SUM(share_state='unshared' AND status='active' AND source!='import' AND sync_origin!='pulled' AND ((scope_type='organization' AND scope_id=?1) OR (scope_type='repository' AND repository_identity='remote-sha256'))),0),
		COALESCE(SUM(share_state='unshared' AND status='active' AND (source='import' OR sync_origin='pulled') AND ((scope_type='organization' AND scope_id=?1) OR (scope_type='repository' AND repository_identity='remote-sha256'))),0)
		FROM memory_record`, orgID).Scan(&s.Shared, &s.Pulled, &s.Shadowed, &s.Aliased, &s.Held, &s.WeakRepository, &s.ShareCandidates, &s.ImportCandidates)
	if err != nil {
		return s, err
	}
	if err := ix.db.QueryRow(`SELECT COALESCE(SUM(reason!='deleted'),0), COALESCE(SUM(reason='deleted'),0) FROM memory_conflict`).Scan(&s.Conflicts, &s.DiscardedEdits); err != nil {
		return s, err
	}
	if err := ix.db.QueryRow(`SELECT count(*) FROM sync_outbox WHERE record_kind=? AND ack_code=? AND acked_at >= ?`,
		OutboxMemory, teamwire.CodeNotShareable, notShareableSince).Scan(&s.NotShareable); err != nil {
		return s, err
	}
	// The same predicate the settle recovers by (MemoryDeletionAccepted), over this
	// link's acknowledgements only (an earlier link's carry the prior-link mark).
	if err := ix.db.QueryRow(`SELECT count(*) FROM sync_outbox WHERE record_kind=? AND scope=? AND acked_at IS NOT NULL
		AND ack_code NOT IN ('', ?, ?, ?) AND ack_code NOT LIKE 'prior:%'`,
		OutboxTombstone, teamwire.TombstoneMemory, teamwire.StatusAccepted, teamwire.StatusDuplicate, teamwire.CodeSuperseded).Scan(&s.DeletionsRefused); err != nil {
		return s, err
	}
	err = ix.db.QueryRow(`SELECT COALESCE(SUM(origin='local'),0), COALESCE(SUM(origin='pulled'),0) FROM memory_tombstone`).Scan(&s.Deleted, &s.DeletedByTeam)
	return s, err
}

// MemoryDeletionSent is one deletion this device sent and the server accepted.
type MemoryDeletionSent struct {
	GlobalID  string `json:"global_id"`
	Slug      string `json:"slug"`
	DeletedAt int64  `json:"deleted_at"`
}

// MemoryDeletionsSent lists this device's accepted memory deletions, newest first — the
// ones whose reach Settings → Team verifies.
func (ix *Index) MemoryDeletionsSent(limit int) ([]MemoryDeletionSent, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("deletions: a positive limit is required (team.json deletions_verify_max)")
	}
	rows, err := ix.db.Query(`SELECT t.global_id, t.id, t.deleted_at FROM memory_tombstone t
		JOIN sync_outbox o ON o.global_id = t.global_id AND o.record_kind = ? AND o.ack_code IN (?, ?)
		WHERE t.origin = 'local' ORDER BY t.deleted_at DESC LIMIT ?`, OutboxTombstone, teamwire.StatusAccepted, teamwire.StatusDuplicate, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemoryDeletionSent
	for rows.Next() {
		var d MemoryDeletionSent
		if err := rows.Scan(&d.GlobalID, &d.Slug, &d.DeletedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---------- deleting session content already sent (decision 9) ----------

// ErrNothingSent refuses a content deletion for a session the server holds nothing of
// from this device.
var ErrNothingSent = errors.New("this device sent no content for that session")

// contentScopes are the outbox scope spellings of one content key ("<runtime>/<native>"):
// content rows carry the event's stored session id, which may or may not be prefixed.
func contentScopes(key string) (string, string) {
	native := key
	if i := strings.Index(key, "/"); i >= 0 {
		native = key[i+1:]
	}
	return native, key
}

// contentRowsOfSession is the WHERE fragment selecting one session's content rows: the
// prefixed spelling matches outright; the bare native id matches only where this
// device's events for that id are this runtime's, so one runtime's bare id is never
// read as another's session (the three-identities rule, PW-L10). Arguments: full key,
// native id, native id, runtime.
const contentRowsOfSession = `(scope = ? OR (scope = ? AND EXISTS (SELECT 1 FROM event e WHERE e.session_id = ? AND e.runtime = ?)))`

func contentRuntime(key string) string {
	if i := strings.Index(key, "/"); i >= 0 {
		return key[:i]
	}
	return ""
}

// ContentSent counts this device's content chunks for a session the server accepted and
// no later deletion has covered — what "delete what was sent" would erase. The button
// renders from these rows (decision 9).
func (ix *Index) ContentSent(key string) (int, error) {
	native, full := contentScopes(key)
	var n int
	err := ix.db.QueryRow(`SELECT count(*) FROM sync_outbox WHERE record_kind=? AND `+contentRowsOfSession+` AND ack_code IN (?, ?)
		AND seq > COALESCE((SELECT MAX(seq) FROM sync_outbox WHERE record_kind=? AND scope=?), 0)`,
		OutboxContent, full, native, native, contentRuntime(key), teamwire.StatusAccepted, teamwire.StatusDuplicate, OutboxTombstone, teamwire.TombstoneSessionContent+":"+key).Scan(&n)
	return n, err
}

// EnqueueContentTombstone is "delete what was sent" (decision 9, O-3): one tombstone with
// record_type session_content naming the WIRE session id, prior_content_hash = sha256
// over the sorted sent body hashes of the session's accepted content rows (C-3), and
// cleanup ["cache"] — and, in the same transaction, the session's consent is withdrawn,
// so content stops flowing after the deletion. Returns the chunks it covers.
func (ix *Index) EnqueueContentTombstone(key, wireSessionID string, at int64) (int, error) {
	native, full := contentScopes(key)
	tx, err := ix.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.Query(`SELECT sent_body_hash FROM sync_outbox WHERE record_kind=? AND `+contentRowsOfSession+` AND ack_code IN (?, ?) AND sent_body_hash != ''
		AND seq > COALESCE((SELECT MAX(seq) FROM sync_outbox WHERE record_kind=? AND scope=?), 0)`,
		OutboxContent, full, native, native, contentRuntime(key), teamwire.StatusAccepted, teamwire.StatusDuplicate, OutboxTombstone, teamwire.TombstoneSessionContent+":"+key)
	if err != nil {
		return 0, err
	}
	var hashes []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return 0, err
		}
		hashes = append(hashes, h)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if len(hashes) == 0 {
		return 0, ErrNothingSent
	}
	sort.Strings(hashes)
	sum := sha256.Sum256([]byte(strings.Join(hashes, "\n")))
	prior := "sha256:" + hex.EncodeToString(sum[:])
	tomb := teamwire.Tombstone{SchemaVersion: teamwire.TombstoneSchemaVersion, ID: engine.NewTypedID("tmb"), RecordID: wireSessionID,
		RecordType: teamwire.TombstoneSessionContent, DeletedAt: time.Unix(at, 0).UTC().Format(time.RFC3339),
		DeletedBy: teamwire.Actor{Type: "user", ID: teamwire.WireAuthorID}, ReasonClass: "user-request", PriorContentHash: prior,
		RequiredProjectionCleanup: []string{"cache"}}
	body, err := json.Marshal(tomb)
	if err != nil {
		return 0, err
	}
	if err := enqueueOutboxRow(tx, outboxInsert{kind: OutboxTombstone, globalID: tomb.ID, contentHash: prior,
		scope: teamwire.TombstoneSessionContent + ":" + key, at: at, sentWireBody: string(body), sentWireHash: teamwire.ContentHash(body)}); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM sync_content_optin WHERE session_id = ?`, key); err != nil {
		return 0, err
	}
	return len(hashes), tx.Commit()
}

// ---------- repository identity (decision 18) ----------

// CheckoutRoots lists distinct checkout roots this device's sessions recorded, newest
// first — the candidates the guarded identity upgrade resolves.
func (ix *Index) CheckoutRoots(limit int) ([]string, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("checkout roots: a positive limit is required (team.json identity_upgrade_roots)")
	}
	rows, err := ix.db.Query(`SELECT checkout_root FROM session_checkpoint WHERE checkout_root != ''
		GROUP BY checkout_root ORDER BY MAX(id) DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var root string
		if err := rows.Scan(&root); err != nil {
			return nil, err
		}
		out = append(out, root)
	}
	return out, rows.Err()
}

// SetMemoryIdentityNote records why a repository record's identity stays weak (the
// console names it, decision 18). It is not a revision: the content did not change.
func (ix *Index) SetMemoryIdentityNote(id, note string) error {
	_, err := ix.db.Exec(`UPDATE memory_record SET identity_note=? WHERE id=?`, note, id)
	return err
}

// MemoryRefusedByTeam reports whether code is the team server refusing a revision for
// what it IS — who sent it, its scope, its name, a deleted id — as opposed to an answer
// that settles the record (accepted, duplicate, stale_base, which lands the team's
// revision) or a row this device never sent (not_shareable, superseded).
func MemoryRefusedByTeam(code string) bool {
	switch code {
	case teamwire.CodeOrganizationScopeAdmin, teamwire.CodeForeignOrganization, teamwire.CodeSlugImmutable,
		teamwire.CodeTombstoned, teamwire.CodeInvalidRecord, teamwire.CodeHashMismatch, teamwire.StatusConflict:
		return true
	}
	return false
}

// MemoryRefusals answers, per record global id, the code the team refused that record's
// LATEST queued revision with — nothing for a record whose latest revision is still
// waiting, was taken, or belongs to an ended link (FR-6). It is what a record says about
// itself: "the team did not take this edit", and why.
func (ix *Index) MemoryRefusals() (map[string]string, error) {
	rows, err := ix.db.Query(`SELECT o.global_id, o.ack_code FROM sync_outbox o
		WHERE o.record_kind=? AND o.acked_at IS NOT NULL
		  AND o.seq = (SELECT MAX(seq) FROM sync_outbox WHERE record_kind=? AND global_id=o.global_id)`, OutboxMemory, OutboxMemory)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var gid, code string
		if err := rows.Scan(&gid, &code); err != nil {
			return nil, err
		}
		if MemoryRefusedByTeam(code) {
			out[gid] = code
		}
	}
	return out, rows.Err()
}

// MemoryRefusal is MemoryRefusals for one record (” when its latest revision was not refused).
func (ix *Index) MemoryRefusal(globalID string) (string, error) {
	var code string
	var acked sql.NullInt64
	err := ix.db.QueryRow(`SELECT ack_code, acked_at FROM sync_outbox WHERE record_kind=? AND global_id=? ORDER BY seq DESC LIMIT 1`,
		OutboxMemory, globalID).Scan(&code, &acked)
	if err == sql.ErrNoRows || (err == nil && (!acked.Valid || !MemoryRefusedByTeam(code))) {
		return "", nil
	}
	return code, err
}

// ErrMemoryNotTeam is TakeTeamVersion's refusal for a record the team does not hold.
var ErrMemoryNotTeam = errors.New("the team does not hold this record")

// ErrMemoryInFlight is TakeTeamVersion's refusal while a revision of the record is
// still waiting for the team's answer.
var ErrMemoryInFlight = errors.New("a revision of this record is still being sent")

// TakeTeamVersion gives up this device's edit of a team record and asks for the team's
// current revision back (FR-6): the record's base is forgotten and the memory pull starts
// over, so the next pull lands what the team holds through decision 5's rules — and, by
// rule 3, keeps the local body as a conflict copy first. Nothing is sent, and nothing is
// restored from local history. Until that pull lands, the record still shows the local body.
func (ix *Index) TakeTeamVersion(id string) error {
	g, err := ix.BeginGov()
	if err != nil {
		return err
	}
	defer func() { _ = g.Rollback() }()
	r, ok, err := memoryByIDTx(g, id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s", ErrMemoryNotFound, id)
	}
	linked, err := deviceLinked(g)
	if err != nil {
		return err
	}
	if !linked || !MemoryIsTeamRecord(r) {
		return fmt.Errorf("%w: %s", ErrMemoryNotTeam, id)
	}
	n, err := unackedMemoryRows(g, r.GlobalID)
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("%w: %s", ErrMemoryInFlight, id)
	}
	if _, err := g.Exec(`UPDATE memory_record SET pushed_hash='', server_revision=0, sync_origin='pulled' WHERE global_id=?`, r.GlobalID); err != nil {
		return err
	}
	if err := restartMemoryPullTx(g); err != nil {
		return err
	}
	return g.Commit()
}

// MemoryConflictCounts counts conflict copies per record global id.
func (ix *Index) MemoryConflictCounts() (map[string]int, error) {
	rows, err := ix.db.Query(`SELECT global_id, count(*) FROM memory_conflict GROUP BY global_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var gid string
		var n int
		if err := rows.Scan(&gid, &n); err != nil {
			return nil, err
		}
		out[gid] = n
	}
	return out, rows.Err()
}
