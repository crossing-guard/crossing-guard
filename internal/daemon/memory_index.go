package daemon

// Phase 6 — memory as a governed record (governance plan "the convergence").
//
// Today memory has its own keyword search and would grow its own tag/classify
// subsystem. This folds it into the ONE model instead: each memory .md becomes a
// `memory:<id>` ENTITY, classified by the SAME detector library the live capture
// runs, with its labels folded to entity_state and its text indexed into the SAME
// FTS. The result — memory is a governed, searchable, LABELLED entity, and a policy
// can gate on its state (item 17) — with zero new machinery.
//
// A memory is a RESOURCE, not an action: indexing it establishes the entity and its
// state directly and does NOT append to the event log (which is the record of
// ACTIONS). Re-running is idempotent — it refreshes the entity and replaces the FTS
// row.

import (
	"fmt"

	"crossing-guard/engine"
	"crossing-guard/store"
)

// MemoryIndexResult summarizes one index run.
type MemoryIndexResult struct {
	Scanned  int `json:"scanned"`
	Indexed  int `json:"indexed"`
	Labelled int `json:"labelled"` // records that got at least one classification label
	Errors   int `json:"errors"`
}

// FmtMemoryIndexResult renders the run for the CLI — the package formats its own result
// (parity with FmtImportResult), so the two backfill verbs read the same.
func FmtMemoryIndexResult(r MemoryIndexResult) string {
	return fmt.Sprintf("indexed %d of %d memories (%d carry a classification label, %d errors)",
		r.Indexed, r.Scanned, r.Labelled, r.Errors)
}

// IndexAllMemory re-indexes every STORE record as a governed entity + FTS row,
// classified by the shared detectors. Since the first-class-records change the
// write owner maintains these rows transactionally on every mutation, so this
// is the RECOVERY/reconcile surface (a store migrated before classification
// existed, or an indexing bug's repair) — not a step in any write path. It
// opens its own store handle (visible to the live daemon immediately under WAL)
// and loads the same layered detector library the governor uses (R7).
func IndexAllMemory(dataDir string) (MemoryIndexResult, error) {
	var res MemoryIndexResult
	ix, dets, err := openGovernedStore(dataDir)
	if err != nil {
		return res, err
	}
	defer ix.Close()

	recs, err := ix.ListMemory("")
	if err != nil {
		return res, err
	}
	for _, r := range recs {
		res.Scanned++
		labelled, err := reindexOneMemory(ix, dets, r)
		if err != nil {
			res.Errors++
			continue
		}
		res.Indexed++
		if labelled {
			res.Labelled++
		}
	}
	return res, nil
}

// reindexOneMemory classifies one store record's content and refreshes its
// entity, state, and FTS row (idempotent; the write owner does this same work
// inside its transaction — this is the out-of-band reconcile). Returns whether
// the record carried any classification label.
func reindexOneMemory(ix *store.Index, dets []engine.Detector, r store.MemoryRecord) (bool, error) {
	id := engine.EntityID("memory", r.ID)
	state := classifyMemory(r, dets)
	if err := ix.UpsertMemoryEntity(id, r.UpdatedAt, state); err != nil {
		return false, err
	}
	if err := ix.IndexMemoryText(id, r.UpdatedAt, r.Title+"\n"+r.Body); err != nil {
		return false, err
	}
	return len(state) > 0, nil
}

// classifyMemory runs the shared library over one record's text (role
// "memory": content/secret/PII pattern detectors fire; tool-call-scoped
// source detectors do not — a memory is not a tool call).
func classifyMemory(r store.MemoryRecord, dets []engine.Detector) []store.StateRow {
	ev := engine.Event{Text: r.Title + "\n" + r.Body, Role: "memory"}
	tags := engine.Classify(ev, dets)
	state := make([]store.StateRow, 0, len(tags))
	for _, t := range tags {
		state = append(state, engine.FactFromTag(t))
	}
	return state
}
