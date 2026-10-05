package memcli

// memory_store.go — the CLI's bridge from the historical file verbs to the ONE
// write owner (memory-first-class-records plan §3.3). The store is truth; the
// files directory is a write-through mirror maintained after commit. The WRITE
// verbs (and doctor) open the same store the daemon resolves, run through
// store.UpsertMemory/Promote/Reject/Delete, then mirror. The READ verbs do not
// open it at all: they read through the daemon (memory_daemon.go).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"crossing-guard/engine"
	"crossing-guard/internal/changeenv"
	"crossing-guard/internal/detectorselection"
	"crossing-guard/internal/teamlink"
	"crossing-guard/memory"
	"crossing-guard/store"
)

// memStore is the CLI's handle for direct store use (promote/reject/delete).
func memStore() (*store.Index, error) {
	ix, err := store.Open(indexDB())
	if err != nil {
		return nil, err
	}
	return ix, nil
}

// memDetectors resolves the same layered detector library the live governor uses
// (R7 — a memory classifies exactly as the same text would in a tool call).
func memDetectors() []engine.Detector {
	dir := home()
	loaded, err := detectorselection.ResolveDetectors(
		filepath.Join(dir, ".crossing-guard"),
		detectorselection.DetectorSurfaceGovernor, os.Getenv("CG_DETECTORS"))
	if err != nil {
		return nil
	}
	return loaded.Detectors
}

// linkedOrganizationID is the organization this device is linked to (team.json beside
// the store). An unlinked device has no organization scope to write to.
func linkedOrganizationID() (string, error) {
	doc, _, err := teamlink.Load(filepath.Dir(indexDB()))
	if err != nil {
		return "", fmt.Errorf("team.json could not be read: %w", err)
	}
	if !doc.Linked() || doc.Organization.ID == "" {
		return "", fmt.Errorf("--scope organization needs a linked team: this device is not linked (crossing-guard link <server>)")
	}
	return doc.Organization.ID, nil
}

// mintRepositoryScope resolves the current folder for a repository-scoped record
// (changeenv.MemoryRepositoryScope), bounded so a slow git never hangs the verb.
func mintRepositoryScope(label string) (scopeID, identity, note string) {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), identityResolveTimeout())
	defer cancel()
	return changeenv.MemoryRepositoryScope(ctx, cwd, label)
}

// teamTunables is team.json beside the store, or its embedded default document on a
// device that has never linked (or whose file cannot be read): the memory doors' bounds
// live there, never in this package.
func teamTunables() teamlink.Document {
	doc, _, err := teamlink.Load(filepath.Dir(indexDB()))
	if err != nil {
		doc, _ = teamlink.Default() // the embedded document always decodes (teamlink's own test)
	}
	return doc
}

// identityResolveTimeout is team.json's identity_resolve_timeout: how long one
// repository resolution may take.
func identityResolveTimeout() time.Duration { return teamTunables().IdentityResolveTimeout.Duration }

// identityUpgradeRoots is team.json's identity_upgrade_roots: how many seen checkout
// roots a door considers when it mints a repository identity.
func identityUpgradeRoots() int { return teamTunables().IdentityUpgradeRoots }

// cliActor is the write owner's actor for every CLI verb.
func cliActor() store.MemoryActor {
	who := os.Getenv("USER")
	if who == "" {
		who = "local"
	}
	return store.MemoryActor{AuthorType: "user", AuthorID: who, ActorSource: "cli"}
}

// upsertThroughStore runs one write through the owner and mirrors after commit.
// It returns the stored record.
func upsertThroughStore(rec store.MemoryRecord, sources []store.MemorySource) (store.MemoryRecord, error) {
	return writeThroughStore(rec, sources, (*store.Index).UpsertMemory)
}

// narrowThroughStore is the explicit narrowing to user scope (`--scope user`): a record
// the team holds is detached into a copy of the user's own (team item 5, O-11).
func narrowThroughStore(rec store.MemoryRecord, sources []store.MemorySource) (store.MemoryRecord, error) {
	return writeThroughStore(rec, sources, (*store.Index).NarrowMemoryToUser)
}

// reviseThroughStore writes an edit of a record the verb READ at revision expected: when
// the record has moved since (a teammate's revision landed, another door wrote), the
// save is refused and nothing is written — never merged (team item 5 decision 13b).
func reviseThroughStore(rec store.MemoryRecord, expected int64) (store.MemoryRecord, error) {
	return writeThroughStore(rec, nil, func(ix *store.Index, r store.MemoryRecord, sources []store.MemorySource, dets []engine.Detector, actor store.MemoryActor) (store.MemoryRecord, error) {
		return ix.ReviseMemoryAt(r, expected, sources, dets, actor)
	})
}

// createThroughStore is upsertThroughStore for a NEW record: the owner refuses
// (store.ErrMemoryExists) rather than edit when the id is taken.
func createThroughStore(rec store.MemoryRecord, sources []store.MemorySource) (store.MemoryRecord, error) {
	return writeThroughStore(rec, sources, (*store.Index).CreateMemory)
}

func writeThroughStore(rec store.MemoryRecord, sources []store.MemorySource,
	write func(*store.Index, store.MemoryRecord, []store.MemorySource, []engine.Detector, store.MemoryActor) (store.MemoryRecord, error)) (store.MemoryRecord, error) {
	ix, err := memStore()
	if err != nil {
		return rec, err
	}
	defer ix.Close()
	saved, err := write(ix, rec, sources, memDetectors(), cliActor())
	if err != nil {
		return saved, err
	}
	mirrorRecord(saved)
	return saved, nil
}

// mirrorRecord writes the store record back into the mirror directory
// (best-effort: a mirror failure never fails the store mutation).
func mirrorRecord(r store.MemoryRecord) {
	dir := memory.DefaultDir()
	memory.MirrorEnsureReadme(dir)
	mr := memory.RecordFromStore(r.ID, r.Status, string(r.ScopeType), r.ScopeID,
		r.Title, r.Category, r.Body, r.Tags, r.Aliases, r.Source, r.Origin,
		r.SupersededBy, r.VerifiedAt, r.VerifiedBy,
		fmtMemoryTime(r.CreatedAt), fmtMemoryTime(r.UpdatedAt))
	_ = memory.MirrorWrite(dir, mr)
}

// mirrorRemove deletes one record's mirror file (best-effort).
func mirrorRemove(id string) {
	_ = memory.MirrorDelete(memory.DefaultDir(), id)
}

// fmtMemoryTime renders store nanos as the RFC3339 string the mirror's
// frontmatter timestamps carry (the legacy files' format).
func fmtMemoryTime(nanos int64) string {
	return time.Unix(0, nanos).UTC().Format(time.RFC3339)
}

// memGetRecord reads one record from the store (pending included, disclosed)
// for the write verbs that edit it; `memory get` reads through the daemon.
func memGetRecord(id string) (store.MemoryRecord, []store.MemorySource, error) {
	ix, err := store.OpenRO(indexDB())
	if err != nil {
		return store.MemoryRecord{}, nil, err
	}
	defer ix.Close()
	r, err := ix.MemoryByID(id)
	if err != nil {
		return r, nil, err
	}
	sources, err := ix.MemorySources(id)
	if err != nil {
		return r, nil, err
	}
	return r, sources, nil
}

// memListRecords lists records by status ("active", "pending", "rejected", "" = all)
// for doctor; `memory list` and `memory index` read through the daemon.
func memListRecords(status string) ([]store.MemoryRecord, error) {
	ix, err := store.OpenRO(indexDB())
	if err != nil {
		return nil, err
	}
	defer ix.Close()
	return ix.ListMemory(status)
}

// memMigrateFiles is the one-time file→store migration (plan §3.4), callable from
// the daemon startup path and a CLI verb. Idempotent by the owner's skip-present
// rule; archives nothing by itself (the daemon/CLI wrapper does the rename).
func memMigrateFiles() (memory.MigrationStats, int, error) {
	dir := memory.DefaultDir()
	recs, tombstones, st, err := memory.MigrateFilesToStore(dir)
	if err != nil {
		return st, 0, err
	}
	ix, err := memStore()
	if err != nil {
		return st, 0, err
	}
	defer ix.Close()
	dets := memDetectors()
	actor := cliActor()
	imported := 0
	for _, sr := range recs {
		if ix.MemoryTombstoned(sr.ID) {
			continue
		}
		if _, err := ix.MemoryByID(sr.ID); err == nil {
			continue // present: skip (idempotence)
		}
		rec := store.MemoryRecord{
			ID: sr.ID, Status: sr.Status,
			ScopeType: store.MemoryScopeType(sr.ScopeType), ScopeID: sr.ScopeID,
			RepositoryIdentity: sr.RepositoryIdentity,
			Title:              sr.Title, Category: sr.Category, Body: sr.Body,
			Tags: sr.Tags, Aliases: sr.Aliases, Source: sr.Source, Origin: sr.Origin,
			SupersededBy: sr.SupersededBy, VerifiedAt: sr.VerifiedAt, VerifiedBy: sr.VerifiedBy,
			RejectReason: sr.RejectReason,
			// RT-C3/RT-4: the wire identity is minted at first store write —
			// the UpsertMemory owner mints global_id; the author is required
			// (empty is invalid on the wire), minted here from the actor.
			AuthorType: actor.AuthorType, AuthorID: actor.AuthorID,
			CreatedAt: sr.CreatedAt.UnixNano(), UpdatedAt: sr.UpdatedAt.UnixNano(),
		}
		if rec.Source == "" || (rec.Source != "human" && rec.Source != "agent" && rec.Source != "import" && rec.Source != "harvest") {
			// A legacy file whose source fell outside the enum (observed once:
			// a path pasted into the field by a hand edit) still migrates —
			// recorded in origin, never silently dropped.
			rec.Origin = strings.TrimSpace(rec.Origin + "; legacy source " + rec.Source + " recorded, import assumed")
			rec.Source = "import"
		}
		if _, err := ix.UpsertMemory(rec, nil, dets, actor); err != nil {
			st.Errors++
			continue
		}
		imported++
	}
	for _, id := range tombstones {
		if !ix.MemoryTombstoned(id) {
			if err := ix.InsertMemoryTombstone(id, actor.AuthorID); err != nil {
				return st, imported, err
			}
		}
	}
	// Mirror the whole store once after migration.
	for _, r := range recs {
		if got, err := ix.MemoryByID(r.ID); err == nil {
			mirrorRecord(got)
		}
	}
	memory.MirrorEnsureReadme(dir)
	return st, imported, nil
}

var _ = fmt.Sprintf
var _ = time.Now
