package memory

// migrate_store.go — the one-time file→store migration (memory-first-class-records
// plan §3.4). Reads the legacy files store (store + pending/ + rejected/ +
// tombstones) and produces store-canonical records for the write owner to insert.
// The store package owns the INSERT; this package owns the FILE FORMAT, so the
// parsing lives here. Idempotent by contract: the write owner skips present ids.
//
// Wire identity is minted here (RT-3/RT-4 folds): every migrated record gets a
// mem_… global id and a wire-valid author {type:'user', id:<local-user>}, and the
// minting is recorded in Origin so nothing pretends it was observed.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// StoreRecord is the store-canonical shape this package hands to the store's
// write owner. It mirrors store.MemoryRecord without importing it (the store
// imports THIS package's constants, not the reverse — the memory package stays
// dependency-free and the store stays the only writer).
type StoreRecord struct {
	ID                 string
	Status             string // active | pending | rejected
	ScopeType          string // user | repository | organization
	ScopeID            string
	RepositoryIdentity string
	Title              string
	Category           string
	Body               string
	Tags               []string
	Aliases            []string
	Source             string
	Origin             string
	SupersededBy       string
	VerifiedAt         string
	VerifiedBy         string
	CreatedAt          time.Time
	UpdatedAt          time.Time
	RejectReason       string
}

// MigrationStats summarizes one migration pass.
type MigrationStats struct {
	Store, Pending, Rejected, Tombstones int
	Errors                               int
}

// MigrateFilesToStore reads the legacy directory once and returns the records +
// tombstone ids the write owner should insert. It does not write the store and
// does not touch the directory — the caller (daemon startup or CLI verb) does the
// archive/mirror steps around it.
func MigrateFilesToStore(dir string) ([]StoreRecord, []string, MigrationStats, error) {
	var out []StoreRecord
	var tombstones []string
	var st MigrationStats

	// Tombstones first: a deleted slug stays deleted, whatever directory it is in.
	if raw, err := os.ReadFile(filepath.Join(dir, "tombstones")); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			if id := strings.TrimSpace(line); id != "" {
				tombstones = append(tombstones, id)
				st.Tombstones++
			}
		}
	}

	appendFrom := func(sub string, status string, stat *int) error {
		base := dir
		if sub != "" {
			base = filepath.Join(dir, sub)
		}
		files, _ := filepath.Glob(filepath.Join(base, "*.md"))
		for _, f := range files {
			r, err := Read(f)
			if err != nil {
				st.Errors++
				continue
			}
			if r.ID == "" {
				st.Errors++
				continue
			}
			rec, err := storeRecordFrom(r, status)
			if err != nil {
				st.Errors++
				continue
			}
			out = append(out, rec)
			*stat++
		}
		return nil
	}

	if err := appendFrom("", "active", &st.Store); err != nil {
		return nil, nil, st, err
	}
	if err := appendFrom("pending", "pending", &st.Pending); err != nil {
		return nil, nil, st, err
	}
	if err := appendFrom("rejected", "rejected", &st.Rejected); err != nil {
		return nil, nil, st, err
	}
	return out, tombstones, st, nil
}

// storeRecordFrom maps one parsed file Record onto the store shape. Scope mapping
// (team plan §5 / plan §3.4): Repository non-empty → {repository, <label>, weak}
// — a basename cannot yield a remote identity (R17); the daemon upgrades weak →
// remote-sha256 when it next resolves that repository.
func storeRecordFrom(r Record, status string) (StoreRecord, error) {
	rec := StoreRecord{
		ID: r.ID, Status: status,
		Title: r.Title, Category: r.Category, Body: r.Body,
		Tags: r.Tags, Aliases: r.Aliases,
		Source: r.Source, Origin: r.Origin,
		SupersededBy: r.Superseded, VerifiedAt: r.VerifiedAt, VerifiedBy: r.VerifiedBy,
	}
	switch {
	case r.Repository != "":
		rec.ScopeType, rec.ScopeID, rec.RepositoryIdentity = "repository", r.Repository, "weak"
	default:
		rec.ScopeType = "user"
	}
	created, errC := parseRecordTime(r.Created)
	updated, errU := parseRecordTime(r.Updated)
	if errC != nil {
		created = time.Now().UTC()
	}
	if errU != nil {
		updated = created
	}
	rec.CreatedAt, rec.UpdatedAt = created, updated
	if status == "rejected" {
		// The legacy Reject stamped the reason into the body ("REJECTED <date>: …").
		// Keep the body verbatim (history) and lift the reason into the field.
		if rest, ok := strings.CutPrefix(r.Body, "REJECTED "); ok {
			if before, _, ok := strings.Cut(rest, "\n"); ok {
				rec.RejectReason = strings.TrimSuffix(before, ":")
			} else {
				rec.RejectReason = before
			}
		}
	}
	if rec.Source == "" {
		rec.Source = "import"
	}
	// RT-3/RT-4: minted wire identity, recorded as minted, never observed.
	if rec.Origin == "" {
		rec.Origin = "migrated from files store; author+global_id minted at migration"
	} else {
		rec.Origin += "; author+global_id minted at migration"
	}
	return rec, nil
}

func parseRecordTime(ts string) (time.Time, error) {
	if ts == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	return time.Parse(time.RFC3339, ts)
}
