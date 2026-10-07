package daemon

// Owner attention (escalation-delivery plan §6): a helper's line for the
// owner — an ask, or a reply it proposed that was not sent — rides the
// session's status frame beside the attention ladder. The session-status
// decider stays the one Needs-you owner; this file is its peripheral input.
//
// The class of a claim is the claim contract's answer (ClaimAttention in
// internal/orchestration/claim.go); nothing here keeps a list of actions.
// An ask or draft stays until the owner moves in the session (a turn start
// the daemon did not cause and that is not a sub-agent re-entry) or the
// reader acknowledges it. A failure to read the source degrades only the ask
// fields: the approval and turn ladders keep their answer (RT-4).

import (
	"errors"
	"log"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"crossing-guard/harvest"
	"crossing-guard/internal/orchestration"
	"crossing-guard/internal/sessionactivity"
	"crossing-guard/store"
)

// managedHostRef is the process's managed orchestration host, for readers
// outside the host (the status fold). Nil when the host is unavailable.
var managedHostRef atomic.Pointer[orchestrationManagedHost]

// managedHostResolved is set once the daemon's attempt to open the managed
// host finished, either way. Before that, a nil host means "not ready yet"
// (the sampler starts first), not "no asks" (independent red-team G15).
var managedHostResolved atomic.Bool

func orchestrationManagedHostService() *orchestrationManagedHost { return managedHostRef.Load() }

// setOrchestrationManagedHostService publishes the open attempt's outcome;
// nil after an attempt means the host is unavailable.
func setOrchestrationManagedHostService(host *orchestrationManagedHost) {
	managedHostRef.Store(host)
	managedHostResolved.Store(true)
}

var errOwnerAttentionNotReady = errors.New("the managed orchestration host is still opening")

// ownerAttention is what the frame carries about helper lines for the owner.
type ownerAttention struct {
	AskID      int64
	AskText    string
	AskAgent   string
	AskCount   int
	DraftCount int
	DraftText  string
	State      string // "" or "unknown"
}

// ownerAttentionBatch is one sampler pass's read of attention-bearing claims
// across every session, so a pass costs one query (RT-12).
type ownerAttentionBatch struct {
	runs   []store.OwnerAttentionRun
	err    error
	absent bool
	// byIdentity indexes runs by runtime + each root id, built on first use,
	// so a pass over many items never rescans every run.
	byIdentity map[string][]int
	// candidates are the sessions this pass added only for their agent lines.
	candidates map[string]bool
	// notReady: the managed host has not finished opening; asks are unknown
	// for this pass, and nothing is logged as a failure.
	notReady bool
}

func identityKey(runtime, id string) string { return runtime + "\x00" + id }

func ownerAttentionSince(now time.Time) int64 {
	return now.Add(-sessionStreamConfig().AskHorizon()).Unix()
}

// readOwnerAttentionBatch reads every attention-bearing claim inside the ask
// horizon, bounded by the rail's own size times the fold's lookback.
func readOwnerAttentionBatch(now time.Time) *ownerAttentionBatch {
	host := orchestrationManagedHostService()
	if host == nil && !managedHostResolved.Load() {
		return &ownerAttentionBatch{notReady: true}
	}
	if host == nil {
		logSessionStatusFailure("owner-attention", "", errOwnerAttentionAbsent)
		return &ownerAttentionBatch{absent: true}
	}
	limit := sessionActivityConfig().MaxRailSessions * sessionStreamConfig().LookbackRows
	runs, err := host.ix.OwnerAttentionRuns(ownerAttentionSince(now), orchestration.OwnerAttentionActions(),
		orchestration.CarriedDeliveryStates(), limit)
	return &ownerAttentionBatch{runs: runs, err: err}
}

var errOwnerAttentionAbsent = errors.New("the managed orchestration host is unavailable; no agent asks are read")

// runsFor returns the batch's runs whose group root is this session, newest
// first (the batch's own order).
func (batch *ownerAttentionBatch) runsFor(runtime, catalogID, nativeID string) []store.OwnerAttentionRun {
	if batch.byIdentity == nil {
		batch.byIdentity = map[string][]int{}
		for index, run := range batch.runs {
			ids := []string{run.CatalogID}
			if run.NativeID != run.CatalogID {
				ids = append(ids, run.NativeID)
			}
			for _, id := range ids {
				if id != "" {
					key := identityKey(run.Runtime, id)
					batch.byIdentity[key] = append(batch.byIdentity[key], index)
				}
			}
		}
	}
	seen := map[int]bool{}
	var indexes []int
	for _, id := range []string{catalogID, nativeID} {
		if id == "" {
			continue
		}
		for _, index := range batch.byIdentity[identityKey(runtime, id)] {
			if !seen[index] {
				seen[index] = true
				indexes = append(indexes, index)
			}
		}
	}
	sort.Ints(indexes)
	out := make([]store.OwnerAttentionRun, 0, len(indexes))
	for _, index := range indexes {
		out = append(out, batch.runs[index])
	}
	return out
}

// readOwnerAttentionFor is the single-session read an event refold uses.
func readOwnerAttentionFor(now time.Time, runtime, catalogID, nativeID string) ([]store.OwnerAttentionRun, bool, error) {
	host := orchestrationManagedHostService()
	if host == nil && !managedHostResolved.Load() {
		return nil, false, errOwnerAttentionNotReady
	}
	if host == nil {
		return nil, true, nil
	}
	ids := []string{}
	for _, id := range []string{catalogID, nativeID} {
		if id != "" && (len(ids) == 0 || ids[0] != id) {
			ids = append(ids, id)
		}
	}
	runs, err := host.ix.OwnerAttentionFor(runtime, ids, ownerAttentionSince(now),
		orchestration.OwnerAttentionActions(), orchestration.CarriedDeliveryStates(),
		sessionActivityConfig().MaxRailSessions*sessionStreamConfig().LookbackRows)
	return runs, false, err
}

// foldOwnerAttention decides one session's unresolved asks and drafts. turns
// are the session's newest turn rows (newest first) and tasks its runtime
// tasks, both already read by the status fold; readErr is either read's
// error. Resolution fails closed: without both reads the ask is unknown and
// never resolved.
func foldOwnerAttention(now time.Time, runtime, catalogID, nativeID string, batch *ownerAttentionBatch,
	turns []store.SessionTurnObservation, readErr error, tasks []RuntimeTask) ownerAttention {
	var runs []store.OwnerAttentionRun
	var absent bool
	var err error
	if batch != nil && batch.notReady {
		return ownerAttention{State: "unknown"}
	}
	if batch != nil {
		runs, absent, err = batch.runsFor(runtime, catalogID, nativeID), batch.absent, batch.err
	} else {
		runs, absent, err = readOwnerAttentionFor(now, runtime, catalogID, nativeID)
	}
	if absent {
		return ownerAttention{}
	}
	if err == nil {
		err = readErr
	}
	if errors.Is(err, errOwnerAttentionNotReady) {
		return ownerAttention{State: "unknown"}
	}
	if err != nil {
		// Surfaced once in the managed host's own problem (the Agents
		// settings' diagnostics), never as words on every session (J3).
		if logSessionStatusFailure("owner-attention:"+runtime, nativeID, err) {
			if host := orchestrationManagedHostService(); host != nil {
				host.setProblem("Agent asks could not be read for a session: " + err.Error())
			}
		}
		return ownerAttention{State: "unknown"}
	}
	if len(runs) == 0 {
		return ownerAttention{}
	}
	motionMS := ownerMotionMS(now, turns, tasks)
	config := sessionStreamConfig().Attention
	out := ownerAttention{}
	// runs are newest first: the first unresolved of each class is its newest.
	for _, run := range runs {
		class, _ := orchestration.ClaimAttention(run.Action, run.DeliveryState)
		if class == "" || motionMS > lifecycleInstantMS(run.CompletedAt) {
			continue
		}
		switch class {
		case orchestration.AttentionAsk:
			if out.AskCount == 0 {
				out.AskID, out.AskText, out.AskAgent = run.SettledSeq, cutAttentionLine(run.Message, config.AskLineMaxChars), agentDisplayName(run.ProfileID)
			} else if run.SettledSeq > out.AskID {
				out.AskID = run.SettledSeq
			}
			out.AskCount++
		case orchestration.AttentionDraft:
			if out.DraftCount == 0 {
				out.DraftText = cutAttentionLine(run.Message, config.AskLineMaxChars)
			}
			out.DraftCount++
		}
	}
	return out
}

// ownerMotionMS is the daemon-clock instant of the newest turn start the
// owner caused, 0 when there is none (§6.3). A start inside the window of a
// runtime task some managed run launched is the daemon's own; a start right
// after a sub-agent ended is the vendor re-entering that result.
func ownerMotionMS(now time.Time, turns []store.SessionTurnObservation, tasks []RuntimeTask) int64 {
	reentryMS := int64(sessionStreamConfig().Attention.SubagentReentryMS)
	settleMS := int64(orchestrationConfig().NaturalSignal.TaskSettleMS)
	childTask := map[string]bool{}
	checked := map[string]bool{}
	host := orchestrationManagedHostService()
	daemonCaused := func(atMS int64) bool {
		for _, task := range tasks {
			end := now.UnixMilli()
			if isTerminalLifecycle(task.Lifecycle) || task.Lifecycle == TaskUnknown {
				end = task.UpdatedAt
			}
			if atMS < task.CreatedAt || atMS > end+settleMS {
				continue
			}
			if !checked[task.ID] && host != nil {
				checked[task.ID] = true
				// A failed lookup counts as the daemon's own turn: the safe
				// direction keeps the ask (plan §6.3).
				_, found, err := host.ix.ManagedRunByChildTask(task.ID)
				childTask[task.ID] = err != nil || found
			}
			if childTask[task.ID] {
				return true
			}
		}
		return false
	}
	for index, turn := range turns {
		if turn.Kind != "turn.started" {
			continue
		}
		if index+1 < len(turns) {
			previous := turns[index+1]
			if previous.Kind == "subagent.ended" && turn.ReceivedAtMS-previous.ReceivedAtMS <= reentryMS {
				continue
			}
		}
		if daemonCaused(turn.ReceivedAtMS) {
			continue
		}
		return turn.ReceivedAtMS
	}
	return 0
}

// cutAttentionLine bounds a helper's line to max characters, on a rune
// boundary, and marks a cut. The text stays plain: renderers set it as text.
func cutAttentionLine(text string, maxChars int) string {
	text = strings.TrimSpace(text)
	if maxChars <= 0 || utf8.RuneCountInString(text) <= maxChars {
		return text
	}
	runes := []rune(text)
	return strings.TrimSpace(string(runes[:maxChars])) + "…"
}

// mergeOwnerAttentionCandidates keeps a session that holds a classed ask or
// draft on the rail after it went quiet (RT-3): such a session is added as a
// candidate even when no presence lane or recent fact holds it. Candidates
// are bounded by max_rail_sessions on their own, so a rail full of presence
// items never crowds an ask out (independent red-team G10), and each is
// folded once, by the pass's decoration; pruneResolvedAttentionCandidates
// then drops the ones whose lines were all resolved (G9).
func mergeOwnerAttentionCandidates(now time.Time, sessions []SessionSummary, items []sessionactivity.Item, batch *ownerAttentionBatch) []sessionactivity.Item {
	if batch == nil || batch.absent || batch.err != nil || len(batch.runs) == 0 {
		return items
	}
	maxRail := sessionActivityConfig().MaxRailSessions
	seen := map[string]bool{}
	for _, item := range items {
		seen[presenceIdentity(item.Runtime, item.CatalogSessionID, item.NativeSessionID)] = true
	}
	quiet := sessionStreamConfig().Quiet()
	catalog := map[string]*SessionSummary{}
	for index := range sessions {
		session := &sessions[index]
		for _, id := range sessionIdentities(*session) {
			if id != "" && catalog[identityKey(session.Runtime, id)] == nil {
				catalog[identityKey(session.Runtime, id)] = session
			}
		}
	}
	batch.candidates = map[string]bool{}
	for _, run := range batch.runs {
		if len(batch.candidates) >= maxRail {
			break
		}
		if class, _ := orchestration.ClaimAttention(run.Action, run.DeliveryState); class == "" {
			continue
		}
		resolved := catalog[identityKey(run.Runtime, run.CatalogID)]
		if resolved == nil {
			resolved = catalog[identityKey(run.Runtime, run.NativeID)]
		}
		if resolved == nil {
			continue
		}
		nativeID := harvest.CanonicalID(*resolved)
		key := presenceIdentity(resolved.Runtime, resolved.ID, nativeID)
		if seen[key] {
			continue
		}
		seen[key] = true
		batch.candidates[key] = true
		items = append(items, sessionactivity.Item{
			Runtime: resolved.Runtime, CatalogSessionID: resolved.ID, NativeSessionID: nativeID,
			Presence: "unknown", Execution: "unknown", Evidence: "native_protocol", Freshness: "live",
			Authority: "observed", ObservedAt: now, ExpiresAt: now.Add(quiet),
			Detail: "An agent line is waiting for the owner; whether the session is open is unknown.",
		})
	}
	return items
}

// pruneResolvedAttentionCandidates drops the candidates this pass added whose
// decorated frame carries no unresolved ask or draft and no unknown ask
// state: the owner already moved on in them.
func pruneResolvedAttentionCandidates(items []sessionactivity.Item, batch *ownerAttentionBatch) []sessionactivity.Item {
	if batch == nil || len(batch.candidates) == 0 {
		return items
	}
	kept := items[:0]
	for _, item := range items {
		candidate := batch.candidates[presenceIdentity(item.Runtime, item.CatalogSessionID, item.NativeSessionID)]
		if candidate && item.AskCount == 0 && item.DraftCount == 0 && item.AskState == "" {
			continue
		}
		kept = append(kept, item)
	}
	return kept
}

// notifyOwnerAsk raises the one OS notification an ask gets, from the settle
// path only (§7): never from the fold, the sampler, a restart or a replay —
// finishManagedChild reaches here only for a claim this process just stored.
func (host *orchestrationManagedHost) notifyOwnerAsk(run store.ManagedRun, message string) {
	if !sessionStreamConfig().Attention.AskNotify {
		return
	}
	text := agentDisplayName(run.ProfileID) + ": " + cutAttentionLine(message, sessionStreamConfig().Attention.AskLineMaxChars)
	routed := approvalAttention.Notify(text)
	logOwnerAskNotification(run.RunID, routed)
}

// logOwnerAskNotification records the router's decision. The desktop
// notifier may still stand down (CG_NOTIFY=off), and the line says so.
func logOwnerAskNotification(runID string, routed bool) {
	if routed {
		suffix := ""
		if os.Getenv("CG_NOTIFY") == "off" {
			suffix = " (the desktop notifier is off: CG_NOTIFY=off)"
		}
		log.Printf("owner ask %s: routed to the desktop notifier%s", runID, suffix)
		return
	}
	log.Printf("owner ask %s: no notification (a console is visible and focused)", runID)
}

// publishRootStatus refolds the status of the session a group is rooted at,
// so a settled ask or draft reaches the frame within the coalesce window.
func (host *orchestrationManagedHost) publishRootStatus(groupID string) {
	group, found, err := host.ix.ManagedGroup(groupID)
	if err != nil || !found {
		return
	}
	nativeID := group.RootNativeSessionID
	if nativeID == "" {
		nativeID = group.RootCatalogSessionID
	}
	sessionStatusRefold(group.RootRuntime, nativeID)
}

// agentDisplayName is the name the owner gave the agent in its profile, the
// profile id when the profile cannot be read (independent red-team J2).
func agentDisplayName(profileID string) string {
	host := orchestrationManagedHostService()
	if host == nil || host.profiles == nil {
		return profileID
	}
	detail, err := host.profiles.Get(profileID)
	if err != nil || detail.Normalized == nil || strings.TrimSpace(detail.Normalized.Name) == "" {
		return profileID
	}
	return detail.Normalized.Name
}
