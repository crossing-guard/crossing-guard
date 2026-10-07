package daemon

// Flow stage lifecycle (orchestration-flows pilot, slice B): entry records
// the stage's profile bindings and arms their ceiling rows, exit releases
// them. The flow-scoped binding relation (schema 38) is a durable record
// the grant consults — the flow NEVER launches anything; the owner's
// existing managed binding does, visibly and governed, and the flow
// binding only bounds what that launch may deliver (postwork PO-7).
//
// In-flight deliveries on exit follow the claim-on-reply owner's existing
// semantics: a claimed-but-undelivered message completes or returns to
// pending; the flow records the choice on the released binding row, it
// does not invent a third path (plan §5 invariant on slice B).

import (
	"fmt"
	"time"

	"crossing-guard/internal/orchestration"
	"crossing-guard/internal/sessionquery"
	"crossing-guard/store"
)

// flowStageAdmit runs when a member enters a stage: record the stage's
// profile bindings and arm the ceiling rows. Postwork fold PO-7: the
// pass-2 "owner-place-active" dedup is REMOVED — it was self-defeating.
// The flow NEVER launches anything: launching is the owner's existing,
// visible, governed managed binding (the Agents-page place with the
// authority grants the owner ticked). The flow binding only BOUNDS what
// that launch may deliver (when: membership/stage; what: the declared
// reply shape; how much: the durable ceiling). The dedup therefore guarded
// against a double-fire that cannot happen, while disabling the bounding
// exactly when the owner's binding exists — i.e., always, in the only
// configuration where the flow has anything to bound.
func (host *orchestrationManagedHost) flowStageAdmit(flow SavedFlow, stage FlowStage, runtime, sessionID string, now int64) error {
	for _, profile := range stage.Profiles {
		binding := store.OrchestrationFlowBinding{
			FlowID: flow.ID, Stage: stage.ID, MemberRuntime: runtime, MemberSessionID: sessionID,
			ProfileID: profile.ProfileID, CreatedAt: now,
		}
		if err := host.ix.PutFlowBinding(binding, "", now); err != nil {
			return fmt.Errorf("flow %s stage %s bind %s: %w", flow.ID, stage.ID, profile.ProfileID, err)
		}
		if !profile.DryRun {
			// Armed: the grant check reads the flow configuration's ceiling
			// at delivery time. A dry-run stage stays unarmed — its receipts
			// are recorded at the delivery hook instead.
			if err := host.ix.ArmFlowBinding(flow.ID, stage.ID, runtime, sessionID, profile.ProfileID, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// flowStageExit runs when a member leaves a stage (transition, tag removal,
// flow disable): release the stage's bindings. In-flight claimed
// deliveries keep the claim-on-reply owner's semantics; the release records
// which bindings were armed at exit.
func (host *orchestrationManagedHost) flowStageExit(flowID, stage, runtime, sessionID string, now int64) error {
	return host.ix.ReleaseFlowBindings(flowID, stage, runtime, sessionID, now)
}

// flowSignalConsumer is the live tag-signal path (slice B's second half):
// the emitter loop routes tag signals through here after the normal binding
// routing, so flow membership follows tag changes in near-real time. The
// flow's own journal position advances only after the membership write
// succeeds — a crash replays the row (pass-2 C2 resumption).
func (host *orchestrationManagedHost) flowSignalConsumer(signal naturalSessionSignal) {
	if signal.Signal != "session.tag-applied" && signal.Signal != "session.tag-removed" {
		return
	}
	records, err := host.ix.OrchestrationFlowRecords()
	if err != nil {
		return
	}
	for _, record := range records {
		if record.State != "enabled" || record.EvaluationState != "done" {
			continue
		}
		host.flowFollowTagSignal(record, signal)
	}
}

// flowFollowTagSignal applies one tag signal to one flow: membership admits
// on apply (through the governance floors), releases on removal.
func (host *orchestrationManagedHost) flowFollowTagSignal(record store.OrchestrationFlowRecord, signal naturalSessionSignal) {
	flow, err := loadSavedFlowByID(record.FlowID)
	if err != nil || flow == nil {
		return
	}
	// The journal position guard: this flow has already consumed up to
	// record.JournalPosition; the emitter's row id must be newer.
	if signal.EventRowID <= record.JournalPosition {
		return
	}
	// The signal's session must carry (or have carried) one of the flow's
	// membership tags. The tag VALUE never enters this decision beyond the
	// membership grammar the owner configured — the payload the translator
	// routed carries only the change class, and the flow reads the current
	// active tags through the store's fold-aware read (values stay
	// daemon-side, plan invariant 3).
	candidates, err := host.flowTaggedCandidates(*flow)
	if err != nil {
		host.setProblem("flow " + record.FlowID + " membership read failed: " + err.Error())
		return
	}
	isMember := false
	for _, candidate := range candidates {
		if candidate.Runtime == signal.Runtime && candidate.SessionID == signal.CatalogSessionID {
			isMember = true
			break
		}
	}
	now := time.Now().Unix()
	candidate := flowMemberCandidate{Runtime: signal.Runtime, SessionID: signal.CatalogSessionID}
	switch {
	case signal.Signal == "session.tag-applied" && isMember:
		member := store.OrchestrationFlowMember{FlowID: record.FlowID, Runtime: signal.Runtime,
			SessionID: signal.CatalogSessionID, AdmittedAt: now, UpdatedAt: now}
		if excluded, reason := host.flowSharedCheckoutExcluded(candidate); excluded {
			member.Excluded = store.FlowMemberExcludedSharedCheckout
			member.ExclusionReason = reason
		} else if excluded, reason := host.flowTaskOwnedExcluded(candidate); excluded {
			member.Excluded = store.FlowMemberExcludedTaskOwned
			member.ExclusionReason = reason
		} else if stage := host.flowPlacementStage(*flow, candidate); stage != "" {
			member.Stage = stage
		} else {
			member.Excluded = store.FlowMemberExcludedNoStage
			member.ExclusionReason = "no stage's membership query matches this session's current facts"
		}
		if err := host.ix.UpsertFlowMember(member, now); err != nil {
			host.setProblem("flow " + record.FlowID + " membership write failed: " + err.Error())
			return
		}
		if member.Excluded == "" && member.Stage != "" {
			if stage := savedStageByID(*flow, member.Stage); stage != nil {
				if err := host.flowStageAdmit(*flow, *stage, signal.Runtime, signal.CatalogSessionID, now); err != nil {
					host.setProblem(err.Error())
				}
			}
		}
	case signal.Signal == "session.tag-removed" && !isMember:
		// The tag that left was this flow's membership tag: release every
		// stage the member was in (placement may have moved it), then exit.
		members, err := host.ix.FlowMembers(record.FlowID)
		if err == nil {
			for _, member := range members {
				if member.Runtime == signal.Runtime && member.SessionID == signal.CatalogSessionID && member.Stage != "" {
					_ = host.flowStageExit(record.FlowID, member.Stage, signal.Runtime, signal.CatalogSessionID, now)
				}
			}
		}
		if err := host.ix.RemoveFlowMember(record.FlowID, signal.Runtime, signal.CatalogSessionID, now); err != nil {
			host.setProblem(err.Error())
		}
	}
	// Advance the flow's journal position only after the membership write.
	if err := host.ix.AdvanceFlowJournalPosition(record.FlowID, signal.EventRowID, now); err != nil {
		host.setProblem("flow " + record.FlowID + " position write failed: " + err.Error())
	}
}

// flowSignalTransitionConsumer runs STAGE TRANSITIONS on the signals the
// emitter routed (postwork fold: transitions existed as config vocabulary
// but no runtime evaluated them — the owner's core "pop to the next
// column" shape). The rules are the stage document's own: a transition
// fires when its signal kind arrives for a member OF that stage AND its
// When tag condition holds against the member's current vocabulary. The
// move is a fact-driven stage change: old stage released, new stage
// admitted, member row updated — recorded in dry-run when the destination
// stage's profiles run dry.
func (host *orchestrationManagedHost) flowSignalTransitionConsumer(signal naturalSessionSignal) {
	if !orchestration.KnownSignal(signal.Signal) {
		return
	}
	records, err := host.ix.OrchestrationFlowRecords()
	if err != nil {
		return
	}
	for _, record := range records {
		if record.State != "enabled" {
			continue
		}
		flow, err := loadSavedFlowByID(record.FlowID)
		if err != nil || flow == nil {
			continue
		}
		members, err := host.ix.FlowMembers(record.FlowID)
		if err != nil {
			continue
		}
		for _, member := range members {
			if member.Excluded != "" || member.Stage == "" {
				continue
			}
			if member.Runtime != signal.Runtime || member.SessionID != signal.CatalogSessionID {
				continue
			}
			stage := savedStageByID(*flow, member.Stage)
			if stage == nil {
				continue
			}
			for _, transit := range stage.Transitions {
				if transit.Kind != signal.Signal {
					continue
				}
				// The When clause: the transition's tag condition must hold
				// against the member's CURRENT vocabulary (the "until a
				// red-teamed tag appears" half of the owner's rule).
				if transit.When != "" && !host.flowWhenHolds(transit.When, flowMemberCandidate{
					Runtime: member.Runtime, SessionID: member.SessionID}) {
					continue
				}
				host.flowMoveMember(record, *flow, member, transit, signal)
				break // one transition per signal; a second would need the NEW stage's rules
			}
		}
	}
}

// flowWhenHolds evaluates one transition's When tag term against a member's
// current vocabulary. Fail-closed on parse (validation catches authoring
// errors; runtime refuses to fire on garbage).
func (host *orchestrationManagedHost) flowWhenHolds(when string, candidate flowMemberCandidate) bool {
	query, err := sessionquery.Parse(when, flowQueryLimits())
	if err != nil {
		return false
	}
	row, vocabulary, err := host.flowCandidateRow(candidate)
	if err != nil {
		return false
	}
	bound, err := query.Bind(vocabulary, flowQueryLimits(), time.Now().Unix())
	if err != nil {
		return false
	}
	return bound.Matches(row)
}

// flowMoveMember executes one transition: release the old stage's
// bindings, admit the new stage's, update the member row, and record a
// dry-run transition receipt when the destination runs dry.
func (host *orchestrationManagedHost) flowMoveMember(record store.OrchestrationFlowRecord, flow SavedFlow, member store.OrchestrationFlowMember, transit FlowStageTransit, signal naturalSessionSignal) {
	now := time.Now().Unix()
	destination := savedStageByID(flow, transit.To)
	if destination == nil {
		return
	}
	if err := host.flowStageExit(record.FlowID, member.Stage, member.Runtime, member.SessionID, now); err != nil {
		host.setProblem(err.Error())
		return
	}
	moved := member
	moved.Stage = transit.To
	moved.UpdatedAt = now
	if err := host.ix.UpsertFlowMember(moved, now); err != nil {
		host.setProblem(err.Error())
		return
	}
	if err := host.flowStageAdmit(flow, *destination, member.Runtime, member.SessionID, now); err != nil {
		host.setProblem(err.Error())
		return
	}
	// Dry-run destination: the transition itself is recorded as a receipt.
	for _, profile := range destination.Profiles {
		if profile.DryRun {
			_ = host.ix.RecordFlowDryRun(record.FlowID, member.Runtime, member.SessionID, "transition",
				map[string]any{"from": member.Stage, "to": transit.To, "signal": signal.Signal,
					"when": transit.When, "run": signal.EventRowID}, now)
			break
		}
	}
}
