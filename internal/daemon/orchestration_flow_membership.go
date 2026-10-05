package daemon

// Flow membership (orchestration-flows pilot, slice B): one evaluation over
// existing durable facts, plus the live signals that keep it current. The
// initial pass reads CURRENT rows only, emits no signals, and runs once per
// flow enablement (pass-2 C2): its position is journaled on the flow row,
// and a restart resumes from the live streams, never re-evaluating.
//
// The shared-checkout guard is a governance-family FLOOR in code (pass-2
// B3): configuration can strengthen it, never weaken or disable it. A
// session whose checkout has a live same-checkout sibling is excluded with
// the reason recorded — auto-continuing it would edit a shared tree
// (pass-1 RT-9).

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"crossing-guard/internal/sessionquery"
	"crossing-guard/store"
)

// flowMemberCandidate is one session the initial evaluation or a live tag
// signal proposes for membership.
type flowMemberCandidate struct {
	Runtime   string
	SessionID string // catalog id
	Cwd       string
	Title     string
}

// flowSharedCheckoutExcluded is the governance floor: the candidate's
// checkout has another live session on it. The session-activity presence
// owner is the one open-session truth; a candidate is only excluded when a
// DIFFERENT session is open on the same exact checkout root (its cwd read
// through the sessions projection, the one cwd owner).
func (host *orchestrationManagedHost) flowSharedCheckoutExcluded(candidate flowMemberCandidate) (bool, string) {
	if sessionActivityService() == nil {
		// Presence truth unavailable: exclude rather than guess (the
		// boundary's fail-closed posture for facts the floor needs).
		return true, "session presence is unavailable, so a shared-checkout conflict cannot be ruled out"
	}
	snapshot := sessionActivityService().Snapshot()
	for _, item := range snapshot.Items {
		if item.Presence != "open" || item.Runtime == "" {
			continue
		}
		if item.CatalogSessionID == candidate.SessionID && item.Runtime == candidate.Runtime {
			continue
		}
		sibling, found, err := host.ix.SessionByID(item.Runtime, item.CatalogSessionID)
		if err != nil || !found || sibling.CWD == "" || candidate.Cwd == "" {
			continue
		}
		if sibling.CWD == candidate.Cwd {
			return true, fmt.Sprintf("session %s/%s is open on the same checkout", item.Runtime, item.CatalogSessionID)
		}
	}
	return false, ""
}

// flowMembershipEvaluate runs ONE flow's initial evaluation over current
// durable facts: every session carrying the flow's membership tags, minus
// the governance floors. It records members with their exclusion state on
// the flow's member table and marks the flow's evaluation done. It emits no
// signals and never re-runs (the flow row's evaluation_state guards it).
func (host *orchestrationManagedHost) flowMembershipEvaluate(flow store.OrchestrationFlowRecord, now int64) error {
	if flow.EvaluationState == "done" {
		return nil // pass-2 C2: one evaluation per enablement, restart included
	}
	saved, err := loadSavedFlowByID(flow.FlowID)
	if err != nil {
		return err
	}
	if saved == nil {
		// The flow document vanished while enabled: nothing to evaluate
		// against; the disable path owns the message.
		return nil
	}
	// Membership candidates: sessions carrying the flow's membership tags.
	// The tag index is the one owner of active owner tags; the journal is
	// for change facts, not state reads.
	candidates, err := host.flowTaggedCandidates(*saved)
	if err != nil {
		return err
	}
	members := make([]store.OrchestrationFlowMember, 0, len(candidates))
	for _, candidate := range candidates {
		// Stage PLACEMENT is the stage's own membership query (postwork
		// fold: stage.membership was validated but never evaluated — dead
		// config — and entry was hardcoded to stages[0], a compiled
		// opinion). The first stage whose membership query the candidate's
		// tag/fact vocabulary matches admits it; a candidate matching no
		// stage is recorded with the reason.
		member := store.OrchestrationFlowMember{
			FlowID: flow.FlowID, Runtime: candidate.Runtime, SessionID: candidate.SessionID,
			AdmittedAt: now, UpdatedAt: now,
		}
		if excluded, reason := host.flowSharedCheckoutExcluded(candidate); excluded {
			member.Excluded = store.FlowMemberExcludedSharedCheckout
			member.ExclusionReason = reason
		} else if excluded, reason := host.flowTaskOwnedExcluded(candidate); excluded {
			member.Excluded = store.FlowMemberExcludedTaskOwned
			member.ExclusionReason = reason
		} else if stage := host.flowPlacementStage(*saved, candidate); stage != "" {
			member.Stage = stage
		} else {
			member.Excluded = store.FlowMemberExcludedNoStage
			member.ExclusionReason = "no stage's membership query matches this session's current facts"
		}
		members = append(members, member)
	}
	if err := host.ix.ReplaceFlowMembers(flow.FlowID, members, now); err != nil {
		return err
	}
	// Postwork fold (the evaluation-binds defect): evaluation-admitted
	// members BIND to their stage exactly like live-signal admissions —
	// the stalled-backlog population the pilot exists for gets the grant,
	// not just a member row.
	for _, member := range members {
		if member.Excluded != "" || member.Stage == "" {
			continue
		}
		if stage := savedStageByID(*saved, member.Stage); stage != nil {
			if err := host.flowStageAdmit(*saved, *stage, member.Runtime, member.SessionID, now); err != nil {
				host.setProblem("flow " + flow.FlowID + " stage bind failed: " + err.Error())
			}
		}
	}
	return host.ix.MarkFlowEvaluationDone(flow.FlowID, now)
}

// flowPlacementStage returns the first stage whose membership query the
// candidate's current tag/fact vocabulary matches, or "" when none holds.
// The query grammar is the owner's own (sessionquery) — placement is
// configuration, evaluated; code decides nothing about which session
// belongs where.
func (host *orchestrationManagedHost) flowPlacementStage(flow SavedFlow, candidate flowMemberCandidate) string {
	for _, stage := range flow.Stages {
		if host.flowStageMembershipHolds(stage, candidate) {
			return stage.ID
		}
	}
	return ""
}

// flowStageMembershipHolds evaluates one stage's membership query against
// one candidate. An empty query matches every candidate (the stage admits
// by flow membership alone); otherwise the query binds to the candidate's
// tag/fact vocabulary and must hold.
func (host *orchestrationManagedHost) flowStageMembershipHolds(stage FlowStage, candidate flowMemberCandidate) bool {
	if strings.TrimSpace(stage.Membership) == "" {
		return true
	}
	query, err := sessionquery.Parse(stage.Membership, flowQueryLimits())
	if err != nil {
		// Validation refuses this at author time; an unparsable query at
		// runtime admits nobody rather than everybody (fail closed).
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

// flowCandidateRow builds the one-candidate query row the stage membership
// and transition When clauses evaluate against: the candidate's own
// facts/tags only — the rail decorator's vocabulary for a single session,
// not a whole-rail scan.
func (host *orchestrationManagedHost) flowCandidateRow(candidate flowMemberCandidate) (sessionquery.Row, sessionquery.Vocabulary, error) {
	row := sessionquery.Row{Runtime: candidate.Runtime, Title: candidate.Title, TouchedAt: time.Now().Unix()}
	if rail, found, err := host.ix.SessionByID(candidate.Runtime, candidate.SessionID); err == nil && found {
		row.Repository = rail.Project
		row.Title = rail.Title
		row.TouchedAt = rail.Modified
	}
	// Owner tags (daemon-side facts only; values never reach agents —
	// invariant 3) and the settled state facets are the vocabulary.
	if tags, err := host.ix.SessionOwnerTagsFor(candidate.Runtime, candidate.SessionID, 100); err == nil {
		for _, tag := range tags {
			row.Tags = append(row.Tags, sessionquery.Tag{Key: tag.Key, Value: tag.Value, At: tag.AppliedAt, Owner: true})
		}
	}
	if facets, err := host.ix.SessionState(candidate.SessionID); err == nil {
		for _, facet := range facets {
			row.Tags = append(row.Tags, sessionquery.Tag{Key: facet.Key, Value: facet.Value, At: facet.LastSeen})
		}
	}
	return row, sessionquery.NewVocabulary([]sessionquery.Row{row}), nil
}

// savedStageByID resolves one stage of a flow document.
func savedStageByID(flow SavedFlow, stageID string) *FlowStage {
	for index, stage := range flow.Stages {
		if stage.ID == stageID {
			return &flow.Stages[index]
		}
	}
	return nil
}

// flowTaskOwnedExcluded keeps the double-fire guard's discipline on
// membership: a session the console task stream owns is not a natural-flow
// member (its turns route through task events already).
func (host *orchestrationManagedHost) flowTaskOwnedExcluded(candidate flowMemberCandidate) (bool, string) {
	if host.tasks == nil {
		return false, ""
	}
	tasks, err := host.tasks.List(candidate.Runtime, candidate.SessionID, 1)
	if err == nil && len(tasks) > 0 {
		return true, "a console task owns this session's turns"
	}
	return false, ""
}

// flowTaggedCandidates reads the active owner tags for the flow's membership
// tag grammar and resolves each carrier through the sessions projection —
// evaluation over existing rows, not a scanner (pass-1 RT-4).
func (host *orchestrationManagedHost) flowTaggedCandidates(flow SavedFlow) ([]flowMemberCandidate, error) {
	if len(flow.MembershipTags) == 0 {
		return nil, nil
	}
	// The active-tag read is bounded and fold-aware; membership tags use
	// the same key:value grammar the rail filter uses.
	tags, _, err := host.ix.AllActiveSessionOwnerTags(2000)
	if err != nil {
		return nil, fmt.Errorf("membership tag read: %w", err)
	}
	wanted := map[string]bool{}
	for _, tag := range flow.MembershipTags {
		wanted[foldOwnerTagString(tag)] = true
	}
	seen := map[string]bool{}
	var out []flowMemberCandidate
	for _, row := range tags {
		if !wanted[foldOwnerTagString(row.Key+":"+row.Value)] {
			continue
		}
		key := row.Runtime + "\x00" + row.SessionID
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, flowMemberCandidate{Runtime: row.Runtime, SessionID: row.SessionID,
			Cwd: row.Cwd, Title: row.Title})
	}
	return out, nil
}

// foldOwnerTagString folds a membership tag string the way the store folds
// tag parts; the flow document's membership_tags use the same key:value
// shape the owner types in the rail.
func foldOwnerTagString(tag string) string {
	folded := make([]rune, 0, len(tag))
	for _, r := range tag {
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		folded = append(folded, r)
	}
	return string(folded)
}

// loadSavedFlowByID resolves one enabled flow's current configuration from
// the flows document; nil means the id is no longer in the file.
func loadSavedFlowByID(flowID string) (*SavedFlow, error) {
	document := loadFlows(sessionViewsDataDir())
	for _, flow := range document.Flows {
		if flow.ID == flowID {
			saved := flow
			return &saved, nil
		}
	}
	return nil, nil
}

// flowEnablementSync is called when flows.json changes (and once at host
// start): every flow document whose id is absent from the enabled store rows
// and present in the file gains an enabled row with a pending evaluation;
// every enabled row whose id left the file is disabled. This is the ONLY
// enablement path — one evaluation per enablement lives here.
func (host *orchestrationManagedHost) flowEnablementSync(now int64) error {
	document := loadFlows(sessionViewsDataDir())
	enabled, err := host.ix.OrchestrationFlowRecords()
	if err != nil {
		return fmt.Errorf("flow records read: %w", err)
	}
	enabledIDs := map[string]bool{}
	for _, record := range enabled {
		if record.State == "enabled" {
			enabledIDs[record.FlowID] = true
		}
	}
	fileIDs := map[string]bool{}
	for _, flow := range document.Flows {
		fileIDs[flow.ID] = true
		if enabledIDs[flow.ID] {
			continue
		}
		body, err := json.Marshal(flow)
		if err != nil {
			return err
		}
		if err := host.ix.EnableFlow(flow.ID, body, now); err != nil {
			return fmt.Errorf("enable flow %s: %w", flow.ID, err)
		}
	}
	for _, record := range enabled {
		if record.State == "enabled" && !fileIDs[record.FlowID] {
			if err := host.ix.DisableFlow(record.FlowID, now); err != nil {
				return fmt.Errorf("disable flow %s: %w", record.FlowID, err)
			}
		}
	}
	// Evaluate every enabled flow whose evaluation is still pending; on
	// restart the flow row's evaluation_state is already done, so this
	// never re-runs (pass-2 C2).
	pending, err := host.ix.OrchestrationFlowRecords()
	if err != nil {
		return err
	}
	for _, record := range pending {
		if record.State != "enabled" || record.EvaluationState == "done" {
			continue
		}
		if err := host.flowMembershipEvaluate(record, time.Now().Unix()); err != nil {
			// One flow's evaluation failure does not block the others; the
			// host problem surface reports it on the next pass.
			host.setProblem("flow " + record.FlowID + " membership evaluation failed: " + err.Error())
			continue
		}
	}
	return nil
}
