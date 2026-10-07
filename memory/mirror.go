package memory

// mirror.go — the WRITE-THROUGH MIRROR (memory-first-class-records plan §3.4,
// RT-7 folds). The store is truth; this directory is a human-greppable
// convenience the write owner maintains after every committed store mutation.
// Hand edits are DETECTED (stored content hashes), logged loudly, and adoptable
// through `memory import-file` — never silently overwritten.

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// mirrorReadmeText is maintained at the mirror root so the directory's contract
// is readable from the directory itself.
const mirrorReadmeText = `This directory is a WRITE-THROUGH MIRROR of the Crossing Guard memory store
(~/.crossing-guard/index.sqlite). The store is truth.

- Edits made here are NOT read. Use: crossing-guard memory edit <id>
- To adopt a hand edit deliberately:  crossing-guard memory import-file <path>
- Deleting a file here does not delete the record.
- Regenerated on every store mutation; drift is repaired on next write.
`

// MirrorWrite writes one record's file and records its content hash. The write
// owner calls it AFTER the store transaction commits (best-effort: a mirror
// failure never fails the store mutation; the next mutation repairs drift).
func MirrorWrite(dir string, r Record) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := Write(dir, r); err != nil {
		return err
	}
	content, err := os.ReadFile(filepath.Join(dir, r.ID+".md"))
	if err != nil {
		return err
	}
	return mirrorRecordHash(dir, r.ID, content)
}

// MirrorDelete removes one record's file and hash entry.
func MirrorDelete(dir, id string) error {
	if err := os.Remove(filepath.Join(dir, id+".md")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return mirrorRemoveHash(dir, id)
}

// MirrorHandEdits reports mirror files whose on-disk content no longer matches
// the hash the write owner recorded — the hand-edit detector (RT-7). The caller
// logs loudly and points at `memory import-file`; it never rewrites.
func MirrorHandEdits(dir string) []string {
	ledger := mirrorLoadHashes(dir)
	var out []string
	for id, want := range ledger {
		content, err := os.ReadFile(filepath.Join(dir, id+".md"))
		if err != nil {
			out = append(out, id) // deleted or unreadable: also drift
			continue
		}
		if mirrorHash(content) != want {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// MirrorEnsureReadme maintains the contract text at the mirror root.
func MirrorEnsureReadme(dir string) {
	_ = os.WriteFile(filepath.Join(dir, "README.txt"), []byte(mirrorReadmeText), 0o644)
}

// ---------- hash ledger (beside recall-log.jsonl; additive, like it) ----------

func mirrorHash(content []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(content))
}

func mirrorLoadHashes(dir string) map[string]string {
	out := map[string]string{}
	raw, err := os.ReadFile(filepath.Join(dir, "mirror-hashes.json"))
	if err != nil {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	return out
}

func mirrorSaveHashes(dir string, ledger map[string]string) error {
	raw, err := json.MarshalIndent(ledger, "", " ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "mirror-hashes.json.tmp")
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "mirror-hashes.json"))
}

func mirrorRecordHash(dir, id string, content []byte) error {
	ledger := mirrorLoadHashes(dir)
	ledger[id] = mirrorHash(content)
	return mirrorSaveHashes(dir, ledger)
}

func mirrorRemoveHash(dir, id string) error {
	ledger := mirrorLoadHashes(dir)
	if _, ok := ledger[id]; !ok {
		return nil
	}
	delete(ledger, id)
	return mirrorSaveHashes(dir, ledger)
}

// RecordFromStore converts a store-canonical record (as read through this
// package's Record shape by the CLI/daemon adapters) back into the file shape
// the mirror writes. The write owner calls this after commit.
func RecordFromStore(id, status string, scopeType, scopeID, title, category, body string,
	tags, aliases []string, source, origin, supersededBy, verifiedAt, verifiedBy,
	created, updated string) Record {
	r := Record{
		ID: id, Title: title, Category: category, Body: body,
		Tags: tags, Aliases: aliases, Source: source, Origin: origin,
		Superseded: supersededBy, VerifiedAt: verifiedAt, VerifiedBy: verifiedBy,
		Created: created, Updated: updated,
	}
	r.ScopeType, r.ScopeID = scopeType, scopeID // copied verbatim: recall decides by scope (decision 12)
	if scopeType == "repository" || scopeType == "organization" {
		r.Repository = scopeID
	}
	return r
}

// StoreStatusFromPath is not used — statuses live in the store; the mirror has
// no pending/ subdirectory any more. Kept as a named note instead of a function:
// the mirror layout is FLAT by design (plan §3.4).
var _ = time.Now
