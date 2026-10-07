package daemon

// The bounded continue grant (orchestration-flows pilot, slice C; owner D-1
// confirmed): a flow stage's helper may deliver ONE continue-class reply on
// session.turn-ended — the "on finish" hand-back — and nothing else. This
// file is the delivery-path property that enforces it.
//
// The separation of concerns is exact (pass-2 B1): the flow CONFIGURATION
// declares the reply class — a bounded shape with a name, max bytes, no
// tool calls, no approval vocabulary — and delivery code checks CONFORMANCE
// to that declared shape only. No blessed "continue" word exists here; the
// word "continue" never appears in a comparison in this file. Approvals and
// permissions are untouched: a permission prompt routes through the
// approval owner whatever this grant says.
//
// The loop ceiling is daemon-enforced and durable (schema 38's folded-state
// ceiling row): max deliveries per member, a deadline, and a no-op detector
// (the same reply digest twice with no intervening work fact). A breach is
// a first-class failure kind — the completed run gains
// error_class=flow_ceiling_breach so the roster attention lane counts it
// directly, not only through the all-failed threshold (pass-2 C1).

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"crossing-guard/store"
)

// flowGrantForRun resolves the flow grant that governs one run's delivery,
// if any: an ARMED flow binding whose profile's fire-on is this run's
// signal, on the run's source session. A nil grant with a nil error means no
// flow is involved; a read failure is an error, never "no flow" — the
// caller then refuses the delivery (fail closed). The flow id travels with
// the grant so dry-run receipts and ceiling rows land on the right flow.
func (host *orchestrationManagedHost) flowGrantForRun(run store.ManagedRun) (*flowGrant, error) {
	signal, _ := run.Detail["signal"].(string)
	if signal != "session.turn-ended" {
		return nil, nil
	}
	group, found, err := host.ix.ManagedGroup(run.GroupID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("the run's session group is unavailable")
	}
	records, err := host.ix.OrchestrationFlowRecords()
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		if record.State != "enabled" {
			continue
		}
		flow, err := loadSavedFlowByID(record.FlowID)
		if err != nil {
			return nil, err
		}
		if flow == nil {
			continue // the flow left flows.json; the next enablement sync disables it
		}
		bindings, err := host.ix.FlowBindings(record.FlowID, group.RootRuntime, group.RootCatalogSessionID)
		if err != nil {
			return nil, err
		}
		for _, binding := range bindings {
			if !binding.Armed || binding.InertReason != "" {
				continue
			}
			for _, stage := range flow.Stages {
				if stage.ID != binding.Stage {
					continue
				}
				for _, profile := range stage.Profiles {
					if profile.ProfileID == binding.ProfileID {
						return &flowGrant{FlowID: record.FlowID, Stage: stage.ID, Profile: profile}, nil
					}
				}
			}
		}
	}
	return nil, nil
}

// flowGrant is the resolved governing grant: the flow, the stage and the
// profile declaration the delivery must conform to.
type flowGrant struct {
	FlowID  string
	Stage   string
	Profile FlowStageProfile
}

// flowGrantRefused is the conformance check: does the helper's reply conform
// to the config-declared shape? Refusal reasons are recorded, never sent.
// The check is structural over the DECLARED class only: byte bound, and the
// configurable fenced-block rule. It never matches words — postwork fold:
// the compiled English approval word list is GONE, replaced by
// flowPendingApprovalFloor, a structural observed-state check that delivers
// nothing at all while the session holds a pending approval. Word-matching
// was both an opinion smuggled into framework code and weaker than the
// state check (a reply containing "the plan allows retries" is not an
// approval answer; an actual pending approval is).
func flowGrantRefused(grant FlowStageProfile, reply string) string {
	if grant.ReplyClass == "" {
		return "no_reply_class_declared"
	}
	// The reply rides the attribution wrapper; conformance bounds the
	// payload the helper produced, so the wrapper is measured but the
	// class bound applies to the payload alone.
	if grant.ReplyClassMaxBytes() > 0 && len([]byte(reply)) > grant.ReplyClassMaxBytes() {
		return "reply_exceeds_declared_max_bytes"
	}
	// Fenced code blocks are refused when the class says so (the default
	// stays refuse: a continue reply carrying code fences is the shape of a
	// tool-injection attempt). The owner may relax it per class — his call,
	// recorded in his configuration, not compiled here.
	if grant.RefuseFencedBlocks && strings.Contains(reply, "```") {
		return "reply_carries_fenced_block"
	}
	return ""
}

// flowPendingApprovalFloor is the structural approval protection: while the
// member session holds a pending approval, the grant delivers NOTHING —
// the approval owner alone resolves an ask (plan §5 invariant 5). This is
// observed governance state, not guessed intent from prose: no word list,
// no heuristics, and a false positive is impossible because an absent
// approval cannot be misread.
func (host *orchestrationManagedHost) flowPendingApprovalFloor(runtime, catalogID, nativeID string) bool {
	// `approvals` is the one approvals owner (its package-level hub); the
	// floor reads observed pending state, it never writes.
	return approvals.pendingForSession(runtime, catalogID, nativeID, nil) > 0
}

// ReplyClassMaxBytes resolves the declared byte bound; zero means the
// validation floor (flow config validation already refuses a class without
// a bound — the class always declares one).
func (p FlowStageProfile) ReplyClassMaxBytes() int {
	if p.ReplyClassBytes > 0 {
		return p.ReplyClassBytes
	}
	return 0
}

// flowCeilingMissing is the refusal for a grant with no counter row.
const flowCeilingMissing = "flow_ceiling_missing"

// flowCeilingCheck runs before an armed grant delivers: it folds the durable
// counter, detects no-op repeats, and enforces the deadline. It counts
// nothing — the caller counts a delivery only once it started — and a read
// failure is an error, never a breach. A breach comes back as the refusal;
// completeFlowGrantBreach records it on the run.
func (host *orchestrationManagedHost) flowCeilingCheck(grant flowGrant, runtime, sessionID, reply string, now int64) (refused string, err error) {
	ceiling, found, err := host.ix.FlowCeiling(grant.FlowID, runtime, sessionID)
	if err != nil {
		return "", err
	}
	if !found {
		// No counter row: the arm path always creates one, so its absence
		// means the grant was never armed through the stage path — refuse
		// rather than deliver uncounted.
		return flowCeilingMissing, nil
	}
	// Breached stays breached until the owner re-arms by re-tagging.
	if ceiling.Breached == 1 {
		return "flow_ceiling_breach", nil
	}
	if grant.Profile.MaxDeliveries > 0 && ceiling.Deliveries >= grant.Profile.MaxDeliveries {
		if err := host.ix.MarkFlowCeilingBreached(grant.FlowID, runtime, sessionID, now); err != nil {
			return "", err
		}
		return "flow_ceiling_breach", nil
	}
	if grant.Profile.DeadlineSeconds > 0 && ceiling.FirstDeliveryAt > 0 && now-ceiling.FirstDeliveryAt > grant.Profile.DeadlineSeconds {
		if err := host.ix.MarkFlowCeilingBreached(grant.FlowID, runtime, sessionID, now); err != nil {
			return "", err
		}
		return "flow_ceiling_breach", nil
	}
	// No-op detector: the same reply digest again means the session
	// produced no new work between continues (plan §1 slice C).
	if ceiling.LastReplyDigest == flowReplyDigest(reply) && ceiling.Deliveries > 0 {
		if err := host.ix.MarkFlowCeilingBreached(grant.FlowID, runtime, sessionID, now); err != nil {
			return "", err
		}
		return "flow_ceiling_noop_breach", nil
	}
	return "", nil
}

// flowReplyDigest folds one reply for the no-op detector.
func flowReplyDigest(reply string) string {
	sum := sha256.Sum256([]byte(reply))
	return hex.EncodeToString(sum[:8])
}

// completeFlowGrantBreach is the breach outcome. The claim is already stored
// as a completed run when the ceiling is checked, so the breach is recorded ON
// that run: the ceiling detail merged in, and the first-class error class the
// roster attention lane counts (pass-2 C1; escalation-delivery plan RT-1a —
// completing it a second time was refused and the breach was never recorded).
func (host *orchestrationManagedHost) completeFlowGrantBreach(run store.ManagedRun, refusal string) error {
	if err := host.ix.MergeManagedRunDetail(run.RunID, map[string]any{"flow_ceiling": refusal, "recovery": flowCeilingRecovery}); err != nil {
		return err
	}
	return host.ix.SetManagedRunErrorClass(run.RunID, "flow_ceiling_breach", flowCeilingRecovery)
}

// flowCeilingRecovery is the one recovery a breach carries.
const flowCeilingRecovery = "The stage's helper reached its configured delivery ceiling, so no further automatic reply is sent. Review the session's work, then re-tag it to re-arm the grant."

// flowDryRunRecord records what an armed-but-dry-run grant WOULD have done
// (slice D): a receipt row, never a delivery.
func (host *orchestrationManagedHost) flowDryRunRecord(flowID, runtime, sessionID, kind string, detail map[string]any, now int64) error {
	return host.ix.RecordFlowDryRun(flowID, runtime, sessionID, kind, detail, now)
}
