package daemon

// Owner-tag change signal translator (orchestration-flows pilot, slice A).
// The append-only session_owner_tag_change journal (schema 38) is the ONE
// evidence source for the two tag catalog kinds; this file only TRANSLATES
// journal rows onto the published kinds and routes them through the same
// binding machinery the natural streams use. It creates no scanner, no
// second vocabulary, and observes no mutable session_owner_tag row (pass-1
// RT-2): removals, renames and purges are journal facts because the journal
// write paths append them.
//
// Source parity is daemon-side (pass-1 RT-3): both kinds are always served
// when this translator runs; the capability surface reports that through the
// same per-runtime reporting shape, with the source named owner-tag-journal.
//
// The journal row carries the tag value the owner wrote. That value is user
// content and NEVER reaches agent context here: the routed signal's payload
// carries only the change class and the identity alternates. Binding
// selectors match on the kind, never on tag text; flow membership
// configuration decides what a tag means (plan §5 invariant 3/9).

import (
	"fmt"

	"crossing-guard/store"
)

const naturalTagStreamKind = "owner-tag-journal-v1"

// emitTagChangeSignals translates journal rows after the stored position,
// routing each onto the binding machinery. Bounded, resumable, quiet when
// nothing matches — the same resilience contract as the other streams.
func emitTagChangeSignals(ix *store.Index, route func(naturalSessionSignal) error) error {
	position, err := ix.OrchestrationStreamPosition(naturalTagStreamKind)
	if err != nil {
		return fmt.Errorf("tag signal position: %w", err)
	}
	rows, truncated, err := ix.SessionOwnerTagChangesAfter(position, 200)
	if err != nil {
		return fmt.Errorf("tag signal read: %w", err)
	}
	_ = truncated
	identities := naturalIdentityCache{}
	var last int64 = position
	for _, row := range rows {
		signal, ok := signalForTagChange(row)
		if !ok {
			continue
		}
		catalogID, root := identities.resolve(ix, row.Runtime, row.SessionID, "")
		err := route(naturalSessionSignal{Signal: signal, Runtime: row.Runtime,
			CatalogSessionID: catalogID, NativeSessionID: row.SessionID,
			ProjectRoot: root, Producer: naturalTagStreamKind, EventRowID: row.ChangeID,
			At: row.ChangedAt * 1000})
		if err != nil {
			return fmt.Errorf("route %s for %s/%s: %w", signal, row.Runtime, row.SessionID, err)
		}
		last = row.ChangeID
	}
	return advanceNaturalPosition(ix, naturalTagStreamKind, position, last)
}

// signalForTagChange maps one journal row onto the catalog. Both change
// classes are facts about the owner's own organization act.
func signalForTagChange(row store.SessionOwnerTagChange) (string, bool) {
	switch row.Change {
	case "applied":
		return "session.tag-applied", true
	case "removed":
		return "session.tag-removed", true
	}
	return "", false
}

// bootstrapTagJournalHead adapts the tag journal's head to the bootstrap
// table's function shape; bootstrapNaturalStreamPositions seeds the position
// at head through the same Ensure path as every other stream — history is
// not a signal.
func bootstrapTagJournalHead(ix *store.Index) func() (int64, error) {
	return ix.SessionOwnerTagChangeHead
}

// emitUncommittedWorkSignals translates the settle-authored uncommitted-work
// facts after the stored position. The folded session_state row is the ONE
// evidence source; the fact's (session, last_seen) pair anchors idempotency:
// one row can only fire once per last_seen advance, and the fold's MIN/MAX
// window makes a re-settle that still observes uncommitted work advance
// last_seen — which is a NEW fact (more work may have landed), not a replay.
const naturalUncommittedStreamKind = "uncommitted-work-facts-v1"

func emitUncommittedWorkSignals(ix *store.Index, route func(naturalSessionSignal) error) error {
	position, err := ix.OrchestrationStreamPosition(naturalUncommittedStreamKind)
	if err != nil {
		return fmt.Errorf("uncommitted signal position: %w", err)
	}
	// The facets read is bounded and fold-ordered by last_seen; the position
	// is a (last_seen, session) composite folded into one cursor the same way
	// the other streams use rowids: strictly newer facts only.
	rows, truncated, err := ix.UncommittedWorkFacetsAfter(position, 200)
	if err != nil {
		return fmt.Errorf("uncommitted signal read: %w", err)
	}
	_ = truncated
	identities := naturalIdentityCache{}
	var last int64 = position
	for _, row := range rows {
		catalogID, root := identities.resolve(ix, row.Runtime, row.SessionID, "")
		err := route(naturalSessionSignal{Signal: "session.uncommitted-work", Runtime: row.Runtime,
			CatalogSessionID: catalogID, NativeSessionID: row.SessionID,
			ProjectRoot: root, Producer: naturalUncommittedStreamKind, EventRowID: row.FactAt,
			At:      row.FactAt * 1000,
			Payload: map[string]any{"detector": store.FactUncommittedWork.Detector()}})
		if err != nil {
			return fmt.Errorf("route session.uncommitted-work for %s/%s: %w", row.Runtime, row.SessionID, err)
		}
		last = row.FactAt
	}
	return advanceNaturalPosition(ix, naturalUncommittedStreamKind, position, last)
}
