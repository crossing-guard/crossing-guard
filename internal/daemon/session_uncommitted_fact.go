package daemon

// The uncommitted-work fact (orchestration-flows pilot, slice A's last
// piece; pass-1 RT-8): at checkpoint settle, when the completed change
// record shows file edits but the head revision did not move past the base
// revision, the session "settled with uncommitted edits". This file authors
// that fact on the monitoring side ONLY:
//
// - it is a folded session_state row with its own detector name
//   (`checkpoint-settle`), the same shape every detector fact uses — the
//   state fold owner (store/govern.go UpsertSessionState) is unchanged. Its
//   coordinates are the store's declared direct fact FactUncommittedWork, the
//   catalog the coverage compiler reads (state-producer-declarations plan D-2);
// - it carries NO flow, stage, membership or configuration concept — the
//   monitoring layer does not know flows exist (the layer's non-mixing
//   contract, cross-session-orchestration-layer.md §1.1);
// - the judgment is "edits with no covering commit": the record's items
//   exist AND head == base ("HEAD" snapshot semantics — the changeenv
//   capture uses Base: "HEAD", so a commit DURING the session moves head
//   past base and the fact correctly does not fire);
// - it is NOT the folded-forever `vcs=commit` facet (pass-1 RT-8): a
//   commit-then-edit session is not WIP by this fact until the NEXT settle
//   observes it, which is the honest boundary of settle-time evidence.
//
// Consumers (the flow follow-up surface, the owner's saved views) read it
// through AllSessionStateFacets like any other facet.

import (
	"fmt"

	"crossing-guard/store"
)

// recordUncommittedWorkFact folds the WIP fact onto the session when a
// completed checkpoint's change record shows edits without a covering
// commit. Called from the settle path after the checkpoint completes; a
// nil/empty record or a failed capture writes nothing. The evidence sample
// names the change record id so a later read can cite the exact snapshot.
func recordUncommittedWorkFact(ix *store.Index, sessionID string, record *store.ChangeRecord, now int64, reportFailure func(error)) {
	if ix == nil || record == nil || sessionID == "" {
		return
	}
	// Only a completed record with observed items is WIP evidence; a record
	// with no items captured nothing (an empty tree settle).
	if len(record.Items) == 0 {
		return
	}
	// No covering commit: the head revision equals the base revision. The
	// changeenv capture bases at HEAD, so a session whose work was committed
	// mid-session shows head past base and is not WIP at this settle.
	if record.HeadRevision != "" && record.BaseRevision != "" && record.HeadRevision != record.BaseRevision {
		return
	}
	if record.HeadRevision == "" && record.BaseRevision == "" {
		// No revision evidence at all: the capture could not compare, so the
		// fact is not authored — unknown is not WIP.
		return
	}
	// The fact is the store's declared catalog entry: its coordinates, its detector name
	// (the evidence source, carried as the fold's provenance and the facet's detector
	// column) and its coverage gaps have one owner. The evidence names the change
	// record so a later read can cite the exact snapshot.
	if err := ix.UpsertSessionStateDirect(sessionID, store.FactUncommittedWork, fmt.Sprintf("change_record=%d", record.ID), now); err != nil && reportFailure != nil {
		reportFailure(err)
	}
}
