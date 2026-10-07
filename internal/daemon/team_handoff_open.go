package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"crossing-guard/engine"
	"crossing-guard/internal/guardcli"
	"crossing-guard/internal/observation"
	"crossing-guard/store"
	"crossing-guard/teamwire"
)

// Opening a handoff (team rest-of-release plan §6.3–§6.5). A handoff is opened only by
// Open in the console: Open writes a ticket; the person's first prompt launches an
// ordinary console task whose process alone carries the ticket; the lifecycle hook of
// the session that process starts copies it into its session entry, which claims the
// ticket; the brief is armed for that session and handed over at its next prompt, by
// the existing carrier. A session the person starts in a terminal never picks one up.
//
// This file is the daemon half of that: admitting and recording the launch, watching
// the launched task for its first session frame and its end, the claim inside the
// session-entry ingest, the confirmation after the carrier's reply, and the re-arm
// sweep. The rows are the store's (store/handoff_open.go).

// handoffPromptKind is the observation kind the brief rides: the prompt event, in the
// framework's vocabulary (§6.5 "Branch chosen").
const handoffPromptKind = "turn.started"

// handoffCarrierKind reports whether a boundary of this kind, from this runtime, may
// carry a handoff's brief: only a kind at which the runtime's hook can tell a nested
// call from the session's own (K-1). On Codex that is the prompt kind alone, so a
// Codex tool event never carries a brief, from a child or from the parent.
func handoffCarrierKind(runtime, kind string) bool {
	return slices.Contains(guardcli.NestedCallKinds(runtime), kind)
}

// handoffOpenRuntimes lists the runtimes a handoff can be opened in: those the console
// can launch and whose hook can tell a nested call at the prompt kind. A runtime
// whose hook cannot is not offered (OD-7b: OpenCode in this release).
func handoffOpenRuntimes() []string {
	out := []string{}
	for name := range chatDrivers {
		if handoffCarrierKind(name, handoffPromptKind) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// handoffBriefPolicy is the delivery table's own policy, from orchestration.json.
func handoffBriefPolicy() store.HandoffBriefPolicy {
	config := orchestrationConfig()
	return store.HandoffBriefPolicy{TTLSeconds: int64(config.Delivery.TTLSeconds), MaxPending: config.Delivery.MaxPendingPerSession}
}

// handoffLaunchError refuses a send that carries a ticket. Code is the data; the
// sentence is for a person.
type handoffLaunchError struct {
	Code    string
	Message string
}

func (e *handoffLaunchError) Error() string { return e.Message }

// handoffLaunchSentences words the refusal of a ticketed send.
var handoffLaunchSentences = map[string]string{
	store.HandoffCodeTicketNotFound:  "this handoff was not opened here; open it again",
	store.HandoffCodeTicketCancelled: "this open was cancelled; open the handoff again",
	store.HandoffCodeTicketUsed:      "this open already started its session; send from that session, or open the handoff again",
	store.HandoffCodeTicketRuntime:   "this handoff was opened for another runtime; open it again in this one",
	teamwire.CodeHandoffWithdrawn:    "the sender withdrew this handoff",
	teamwire.CodeHandoffDeclined:     "this handoff was declined",
	teamwire.CodeHandoffClosed:       "this handoff was closed",
	teamwire.CodeHandoffExpired:      "this handoff expired",
}

const (
	handoffCodeUnavailable   = "handoff_open_unavailable"
	handoffCodeNewSession    = "ticket_needs_new_session"
	handoffCodeRuntimeNotOff = "runtime_not_offered"
)

func handoffLaunchRefused(refusal *store.HandoffRefusal) *handoffLaunchError {
	message := handoffLaunchSentences[refusal.Code]
	if message == "" {
		message = "this handoff cannot be opened now"
	}
	return &handoffLaunchError{Code: refusal.Code, Message: message}
}

// prepareHandoffLaunch completes a ticketed send before the task service validates it:
// the session starts in the ticket's folder unless the request names one. It refuses
// nothing but a ticket it cannot read; admission is admitHandoffLaunch's.
func prepareHandoffLaunch(req ChatRequest) (ChatRequest, error) {
	if governor == nil || governor.ix == nil {
		return req, &handoffLaunchError{Code: handoffCodeUnavailable, Message: "handoffs cannot be opened: " + governorUnavailable()}
	}
	if req.SessionID != "" || req.CatalogSessionID != "" {
		return req, &handoffLaunchError{Code: handoffCodeNewSession, Message: "a handoff opens a new session; it cannot be sent into an existing one"}
	}
	ticket, found, err := governor.ix.HandoffOpenByTicket(req.HandoffTicket)
	if err != nil {
		return req, fmt.Errorf("the open could not be read: %w", err)
	}
	if !found {
		return req, handoffLaunchRefused(&store.HandoffRefusal{Code: store.HandoffCodeTicketNotFound})
	}
	if strings.TrimSpace(req.Cwd) == "" && req.WorkspaceSelectionID == "" {
		req.Cwd = ticket.CheckoutRoot
	}
	return req, nil
}

// admitHandoffLaunch is the read-only check of a ticketed send, before any task is
// minted: the ticket is waiting, its runtime matches, and its handoff has not ended.
func admitHandoffLaunch(req ChatRequest) error {
	refusal, err := governor.ix.HandoffOpenLaunchRefusal(req.HandoffTicket, req.Runtime)
	if err != nil {
		return fmt.Errorf("the open could not be read: %w", err)
	}
	if refusal != nil {
		return handoffLaunchRefused(refusal)
	}
	return nil
}

// recordHandoffLaunch names the launched task on the ticket — the authoritative
// admission, in one transaction — before that task's process is scheduled, and starts
// watching the task.
func recordHandoffLaunch(req ChatRequest, taskID string) error {
	if _, err := governor.ix.LaunchHandoffOpen(req.HandoffTicket, req.Runtime, taskID); err != nil {
		var refusal *store.HandoffRefusal
		if errors.As(err, &refusal) {
			return handoffLaunchRefused(refusal)
		}
		return fmt.Errorf("the open could not be recorded: %w", err)
	}
	handoffOpens.watch(taskID, req.HandoffTicket)
	return nil
}

// handoffOpenOwner watches the tasks that Opens launched. The task service has no end
// callback for another owner (plan §17.3), so this subscribes to its events; the
// subscription is a wake-up, and every sweep re-reads the tasks themselves, so a
// dropped event costs time and never the outcome.
type handoffOpenOwner struct {
	mu      sync.Mutex
	watched map[string]string // launched task id → ticket id
	running bool
}

var handoffOpens = &handoffOpenOwner{watched: map[string]string{}}

func (o *handoffOpenOwner) watch(taskID, ticketID string) {
	o.mu.Lock()
	o.watched[taskID] = ticketID
	o.mu.Unlock()
}

func (o *handoffOpenOwner) unwatch(taskID string) {
	o.mu.Lock()
	delete(o.watched, taskID)
	o.mu.Unlock()
}

func (o *handoffOpenOwner) ticketOf(taskID string) (string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	ticketID, ok := o.watched[taskID]
	return ticketID, ok
}

// start begins following the task service's events. It is called once, after the task
// service and the governor exist; without a task service nothing is launched and
// there is nothing to follow.
func (o *handoffOpenOwner) start() {
	o.mu.Lock()
	if o.running || runtimeTasks == nil {
		o.mu.Unlock()
		return
	}
	o.running = true
	o.mu.Unlock()
	go o.follow(runtimeTasks)
}

// handoffFollowRetry is daemon.json's handoff.follow_retry: the pause before the event
// subscription is opened again after it closed (a slow subscriber is disconnected by
// the hub) or could not be opened. Read at each pause, so a changed file is followed.
func handoffFollowRetry() time.Duration {
	config, _ := consoleConfig()
	return config.Handoff.FollowRetry.Duration
}

func (o *handoffOpenOwner) follow(tasks *TaskApplicationService) {
	for {
		watermark, err := tasks.Watermark()
		if err != nil {
			time.Sleep(handoffFollowRetry())
			continue
		}
		subscription, _, _, err := tasks.SubscribeAll(watermark)
		if err != nil {
			time.Sleep(handoffFollowRetry())
			continue
		}
		// Whatever happened while no subscription was open is read from the tasks.
		o.reconcile(tasks)
		for event := range subscription.Events {
			o.taskEvent(tasks, event)
		}
		tasks.Unsubscribe(subscription.ID)
		time.Sleep(handoffFollowRetry())
	}
}

// taskEvent handles one event of a watched task: its session frame, or its end.
func (o *handoffOpenOwner) taskEvent(tasks *TaskApplicationService, event TaskEvent) {
	ticketID, watched := o.ticketOf(event.TaskID)
	if !watched {
		return
	}
	if anyString(event.Payload["type"]) == "session" {
		o.frame(ticketID, anyString(event.Payload["id"]))
		return
	}
	if lifecycle, isTask := strings.CutPrefix(event.Kind, "task."); isTask && terminalTaskLifecycle(TaskLifecycle(lifecycle)) {
		if task, found, err := tasks.Task(event.TaskID); err == nil && found {
			o.settle(ticketID, task)
		}
	}
}

// frame records the session a launched task's first session frame named and moves a
// claim made by any other session to it (§6.4 rule 3).
func (o *handoffOpenOwner) frame(ticketID, nativeID string) {
	if nativeID == "" || governor == nil || governor.ix == nil {
		return
	}
	frame, err := governor.ix.RecordHandoffOpenFrame(ticketID, nativeID, time.Now().Unix(), handoffBriefPolicy())
	if err != nil {
		log.Printf("handoff open %s: the launched session could not be recorded: %v", ticketID, err)
		return
	}
	if frame.Moved {
		log.Printf("handoff open %s: a session other than the launched one held the claim; it is the launched session's now", ticketID)
	}
	if frame.Receipt && team != nil {
		team.pushSoon()
	}
}

// settle reads a launched task and applies what it says to its ticket: the session
// frame if the ticket has none yet, and — when the task has ended and no session
// claimed the ticket — the cancellation, in the one of §6.3's two ways the frame
// tells apart.
func (o *handoffOpenOwner) settle(ticketID string, task RuntimeTask) {
	if governor == nil || governor.ix == nil {
		return
	}
	if task.NativeSessionID != "" {
		o.frame(ticketID, task.NativeSessionID)
	}
	if !terminalTaskLifecycle(task.Lifecycle) {
		return
	}
	o.unwatch(task.ID)
	reason, detail := store.HandoffOpenHookDidNotRun, ""
	if task.NativeSessionID == "" {
		reason, detail = store.HandoffOpenRuntimeNotStarted, strings.TrimSpace(task.ErrorText)
	}
	ended, err := governor.ix.EndHandoffOpenLaunch(ticketID, reason, detail, time.Now().Unix())
	if err != nil {
		log.Printf("handoff open %s: the launched turn's end could not be recorded: %v", ticketID, err)
		return
	}
	if ended {
		log.Printf("handoff open %s: the launched turn ended without a claim (%s)", ticketID, reason)
	}
}

// reconcile reads every launched ticket's task: the path that needs no event. A task
// the service no longer holds can never claim, and is settled as never started.
func (o *handoffOpenOwner) reconcile(tasks *TaskApplicationService) {
	if governor == nil || governor.ix == nil || tasks == nil {
		return
	}
	launched, err := governor.ix.HandoffOpensLaunched()
	if err != nil {
		log.Printf("handoff opens: the launched tickets could not be read: %v", err)
		return
	}
	for _, ticket := range launched {
		task, found, err := tasks.Task(ticket.LaunchedTask)
		if err != nil {
			continue
		}
		if !found {
			task = RuntimeTask{ID: ticket.LaunchedTask, Lifecycle: TaskUnknown, ErrorText: "the launched task is no longer recorded"}
		}
		if !terminalTaskLifecycle(task.Lifecycle) {
			o.watch(ticket.LaunchedTask, ticket.TicketID)
		}
		o.settle(ticket.TicketID, task)
	}
}

// sweep is the handoff owner's pass on the lifecycle coordinator's sweep (§6.5, plan
// §17.2): launched tickets are read against their tasks, and every claimed ticket's
// brief is re-armed, expired or given up as the store's sweep decides. It runs with no
// agent turned on and with no task service. startup is true for the pass at daemon
// start, when no reply of this process can be in flight.
func (o *handoffOpenOwner) sweep(startup bool) {
	if governor == nil || governor.ix == nil {
		return
	}
	o.reconcile(runtimeTasks)
	config, _ := consoleConfig()
	in := store.HandoffBriefSweep{Now: time.Now().Unix(), Policy: handoffBriefPolicy(),
		BriefWaitSeconds: int64(config.Handoff.BriefWait.Duration / time.Second)}
	if !startup {
		// A reply is written within the hook's delivery budget; a row handed over
		// longer ago than that with no confirmation lost it.
		in.HandedOverGraceSeconds = int64(observation.HookDeliveryBudget/time.Second) + 1
	}
	effects, err := governor.ix.SweepHandoffBriefs(in)
	if err != nil {
		log.Printf("handoff briefs: the re-arm sweep failed: %v", err)
		return
	}
	if effects != (store.HandoffBriefSweepEffects{}) {
		log.Printf("handoff briefs: armed=%d expired=%d gave_up=%d waiting=%d", effects.Armed, effects.Expired, effects.GaveUp, effects.Waiting)
	}
}

// handoffEntryStep is what one session entry asks of the handoff owner, decided
// before the ingest takes its write lock: a claim, with the brief already built, or a
// re-arm after a compaction of a session that holds a ticket.
type handoffEntryStep struct {
	claim *store.HandoffClaim
	rearm bool
}

// validHandoffTicket reports whether a value copied from a process environment has a
// ticket id's shape. Anything else claims nothing and is not an error: the entry is
// evidence either way.
func validHandoffTicket(value string) bool {
	return len(value) <= observation.MaxHandoffTicketBytes && engine.IsTypedID(value) &&
		strings.HasPrefix(value, teamwire.TicketIDPrefix+"_")
}

// prepareHandoffEntry reads what the claim needs before the ingest's transaction: the
// ticket, its handoff and the brief built from the document (§6.4, §6.5). Only an
// entry that carries a ticket id claims (rule 1); a compaction entry that carries a
// claimed session's id asks for the brief once more (rule 5). Every other entry costs
// nothing here.
//
// A read that FAILS claims nothing: a claim made without the document would hold the
// ticket with an empty brief that nothing can arm afterwards. The ticket stays as it
// was, so the same session's entry, replayed, claims it once the store reads again. A
// ticket or handoff that is simply not there is not a failure: the claim answers it.
//
// ctx is the ingest's: what is read here for the brief's closing line stops when the
// ingest's budget does.
func prepareHandoffEntry(ctx context.Context, g *Governor, entry observation.SessionEntryEnvelope) handoffEntryStep {
	if g == nil || g.ix == nil {
		return handoffEntryStep{}
	}
	if entry.HandoffTicket == "" {
		if entry.EntryKind != "context-compact" {
			return handoffEntryStep{}
		}
		holds, err := g.ix.HasClaimedHandoffOpen(entry.Runtime, entry.SessionID)
		if err != nil {
			log.Printf("handoff: the claimed tickets of %s/%s could not be read: %v", entry.Runtime, entry.SessionID, err)
		}
		return handoffEntryStep{rearm: holds}
	}
	if !validHandoffTicket(entry.HandoffTicket) {
		return handoffEntryStep{}
	}
	claim := &store.HandoffClaim{TicketID: entry.HandoffTicket, Runtime: entry.Runtime, NativeID: entry.SessionID,
		Transcript: entry.TranscriptPath, Policy: handoffBriefPolicy()}
	ticket, found, err := g.ix.HandoffOpenByTicket(entry.HandoffTicket)
	if err != nil {
		return handoffClaimUnread(entry, "its ticket", err)
	}
	if !found {
		return handoffEntryStep{claim: claim} // the claim itself answers unknown_ticket
	}
	h, found, err := g.ix.HandoffByID(ticket.OrganizationID, ticket.HandoffID)
	if err != nil {
		return handoffClaimUnread(entry, "its handoff", err)
	}
	if !found {
		return handoffEntryStep{claim: claim}
	}
	rec, held, err := h.Document()
	if err != nil {
		return handoffClaimUnread(entry, "the handoff's document", err)
	}
	if held {
		claim.BriefText = buildHandoffBrief(h, rec, handoffSharedMemoryCount(ctx, g.ix, ticket.CheckoutRoot), handoffInjectMaxBytes())
	}
	return handoffEntryStep{claim: claim}
}

// handoffClaimUnread is the step of an entry whose claim could not be prepared: no
// claim, and a line naming what could not be read — ids only.
func handoffClaimUnread(entry observation.SessionEntryEnvelope, what string, err error) handoffEntryStep {
	log.Printf("handoff: session entry of %s carried ticket %s and claimed nothing: %s could not be read (%v); the ticket is still claimable",
		entry.Runtime, entry.HandoffTicket, what, err)
	return handoffEntryStep{}
}

// applyHandoffEntryTx is the step inside the ingest's first transaction (§6.4 rule 6:
// before the checkpoint capture that follows). created is whether this entry is new: a
// duplicate compaction entry asks for nothing again.
func applyHandoffEntryTx(g *Governor, tx *store.GovTx, step handoffEntryStep, entry observation.SessionEntryEnvelope, created bool, at int64) (store.HandoffClaimResult, error) {
	switch {
	case step.claim != nil:
		step.claim.At = at
		return g.ix.ClaimHandoffTicketTx(tx, *step.claim)
	case step.rearm && created:
		_, err := g.ix.RearmHandoffBriefsForSessionTx(tx, entry.Runtime, entry.SessionID, at, handoffBriefPolicy())
		return store.HandoffClaimResult{}, err
	}
	return store.HandoffClaimResult{}, nil
}

// afterHandoffEntry runs once the ingest's transaction committed: the started receipt
// leaves at once, and the claim is chained as an event by ids only.
func afterHandoffEntry(result store.HandoffClaimResult, entry observation.SessionEntryEnvelope) {
	switch result.Outcome {
	case "", store.HandoffClaimDuplicate:
		return
	case store.HandoffClaimClaimed:
		if team != nil {
			team.linkEvent("team.handoff.started", "handoff "+result.HandoffID+" started in "+entry.Runtime)
			if result.Receipt {
				team.pushSoon()
			}
		}
	default:
		// A refused claim is worth a line: it is how a spawned process that inherited
		// the ticket, or a stale ticket, shows up. Ids and the code only.
		log.Printf("handoff: session entry of %s carried ticket %s and claimed nothing (%s)", entry.Runtime, entry.HandoffTicket, result.Outcome)
	}
}

// confirmHandoffBrief is the third settle branch of a session_delivery hand-over
// (§6.5 "Confirmation: the reply owns the claim"): it runs after the carrier wrote its
// reply to the hook without error, as its own transaction. Only the first confirmation
// of a ticket marks the handoff opened and enqueues the opened receipt.
func confirmHandoffBrief(ix *store.Index, ticketID string, record store.SessionDelivery) {
	confirmed, err := ix.ConfirmHandoffBrief(ticketID, record.Runtime, record.NativeSessionID, time.Now().Unix())
	if err != nil {
		// The brief was handed over; the sweep re-arms it and the next prompt
		// confirms. It may be delivered twice; it is never dropped.
		log.Printf("handoff brief %s was handed to %s but could not be confirmed: %v", record.DeliveryID, record.DeliveredKind, err)
		return
	}
	if !confirmed.First || team == nil {
		return
	}
	team.linkEvent("team.handoff.opened", "handoff "+confirmed.HandoffID+" brief delivered")
	if confirmed.Receipt {
		team.pushSoon()
	}
}
