package memory

// Tombstones + pending lifecycle (ADR 0013 D4/D7) + the git-backed history.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// GitCommit makes the store a local git repo (Codex-validated pattern):
// auto-init on first write, one commit per mutation. History/undo/provenance
// for free. Local-only — never pushed; see operations doc §3 for the
// deletion-vs-history governance caveat. No-op if git is unavailable.
func GitCommit(dir, msg string) {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if run(dir, "git", "init", "-q") != nil {
			return
		}
	}
	if run(dir, "git", "add", "-A") != nil {
		return
	}
	_ = run(dir, "git", "commit", "-q", "-m", msg, "--no-gpg-sign")
}

// AppendTombstone records a deleted slug so harvest/synthesis can never
// resurrect it. One slug per line in <dir>/tombstones.
func AppendTombstone(dir, id string) error {
	f, err := os.OpenFile(filepath.Join(dir, "tombstones"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, id)
	return err
}

func IsTombstoned(dir, id string) bool {
	raw, err := os.ReadFile(filepath.Join(dir, "tombstones"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == id {
			return true
		}
	}
	return false
}

// Promote moves pending/<id>.md into the store (the human act — the ONLY
// door from proposal to injectable record).
func Promote(dir, id string) error {
	src := filepath.Join(dir, "pending", id+".md")
	r, err := Read(src)
	if err != nil {
		return err
	}
	if IsTombstoned(dir, r.ID) {
		return fmt.Errorf("%s is tombstoned (deleted before); un-tombstone deliberately by editing the tombstones file", r.ID)
	}
	// A proposal must never silently clobber a curated store record — that
	// would defeat the human gate (red-team finding). Merge deliberately:
	// edit the existing record, or delete it first.
	if _, err := os.Stat(filepath.Join(dir, r.ID+".md")); err == nil {
		return fmt.Errorf("store already has %s — refusing to overwrite from pending; edit the existing record or delete it first", r.ID)
	}
	if err := Validate(r); err != nil {
		return fmt.Errorf("won't promote invalid record: %w (fix with: edit)", err)
	}
	r.Updated = time.Now().UTC().Format(time.RFC3339)
	if err := Write(dir, r); err != nil {
		return err
	}
	if err := os.Remove(src); err != nil {
		return err
	}
	GitCommit(dir, "promote("+r.ID+")")
	return nil
}

// Reject moves pending/<id>.md to rejected/ with a reason stamped in —
// retained as synthesis training data (internal API §3), never projected.
func Reject(dir, id, reason string) error {
	src := filepath.Join(dir, "pending", id+".md")
	r, err := Read(src)
	if err != nil {
		return err
	}
	r.Body = "REJECTED " + time.Now().UTC().Format("2006-01-02") + ": " + reason + "\n\n" + r.Body
	if err := Write(filepath.Join(dir, "rejected"), r); err != nil {
		return err
	}
	if err := os.Remove(src); err != nil {
		return err
	}
	GitCommit(dir, "reject("+id+")")
	return nil
}

// Delete removes a store record and tombstones the slug. Honest language
// contract (ADR 0013 D7): removed from store and index; RETAINED in local
// git history until purge exists.
func Delete(dir, id string) error {
	path := filepath.Join(dir, id+".md")
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("no such record: %s", id)
	}
	// Tombstone BEFORE removing: if the remove then fails, a tombstoned
	// record still on disk is recoverable; a removed record with no
	// tombstone can be resurrected by harvest — the invariant tombstones
	// exist to prevent (red-team finding).
	if err := AppendTombstone(dir, id); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	GitCommit(dir, "delete("+id+") + tombstone")
	return nil
}
