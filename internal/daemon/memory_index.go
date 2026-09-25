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
	"crossing-guard/memory"
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

// IndexAllMemory walks the memory store and indexes every record as a governed
// entity + FTS row, classified by the shared detectors. It opens its own store handle
// (visible to the live daemon immediately under WAL) and loads the same layered
// detector library the governor uses (R7 — memory classifies exactly like everything
// else).
func IndexAllMemory(dataDir string) (MemoryIndexResult, error) {
	var res MemoryIndexResult
	// Same store + detector library as live capture (R7), resolved identically so a
	// memory classifies exactly as the same text would in a tool call.
	ix, dets, err := openGovernedStore(dataDir)
	if err != nil {
		return res, err
	}
	defer ix.Close()

	for _, r := range memory.LoadAll(memory.DefaultDir()) {
		res.Scanned++
		labelled, err := indexOneMemory(ix, dets, r)
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

// indexOneMemory classifies one record's content and writes its entity, state, and FTS
// row. Returns whether it carried any classification label. ts is the record's own
// Updated time when parseable, so re-indexing an unchanged file is stable.
func indexOneMemory(ix *store.Index, dets []engine.Detector, r memory.Record) (bool, error) {
	id := engine.EntityID("memory", r.ID)
	ts, ok := parseEventTime(r.Updated)
	if !ok {
		// Fall back to Created; if BOTH are unparseable, ts stays 0 — an explicit
		// "undated" sentinel (first_seen/last_seen=0, sorts first) rather than a
		// fabricated time. The entity is still indexed; only its window is unknown.
		ts, _ = parseEventTime(r.Created)
	}

	// Classify the memory's own text with the shared library. Role "memory" runs the
	// content/secret/PII pattern detectors (they declare no role, so they fire on any)
	// while skipping tool-call-scoped source detectors — a memory is not a tool call.
	ev := engine.Event{Text: r.Title + "\n" + r.Body, Role: "memory"}
	tags := engine.Classify(ev, dets)

	state := make([]store.StateRow, 0, len(tags))
	for _, t := range tags {
		state = append(state, engine.FactFromTag(t))
	}
	if err := ix.UpsertMemoryEntity(id, ts, state); err != nil {
		return false, err
	}
	// Text into the ONE FTS so a single search covers memory and sessions alike.
	if err := ix.IndexMemoryText(id, ts, r.Title+"\n"+r.Body); err != nil {
		return false, err
	}
	return len(tags) > 0, nil
}
