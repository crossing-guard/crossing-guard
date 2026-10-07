package daemon

// Natural-session signal emitter (natural-session plan, Slice B; extended by
// the helper-session-attachment plan D2). The durable rows hooks already
// append — session-activity (SessionStart / first-action / SessionEnd),
// session-turn (turn.started / turn.ended) and live post-tool results — are
// the ONE evidence source; this file only TRANSLATES them onto the published
// catalog kinds and routes them through the same binding machinery console
// task events use. It creates no scanner, no second lifecycle vocabulary, and
// no vendor branch: which runtime writes which row is published per runtime
// by the harvest adapters, never decided here.
//
// End-fact sourcing is per runtime (red-team B3): only hook-exact closure rows
// ("end" entry kind) fire session.ended; presence decay NEVER masquerades as
// an end. OpenCode emits no natural end fact in v1.
//
// Double-fire guard (red-team M10): a session the console task stream owns
// stays task-owned — its natural signals are suppressed for the task's whole
// lifetime, including the resume window.

import (
	"errors"
	"fmt"
	"hash/crc32"
	"log"
	"strings"
	"time"

	"crossing-guard/store"
)

// The durable emitter positions, one per row family (append-only rowids,
// matching the pump's pattern). Each is also the run-key producer qualifier.
const (
	naturalActivityStreamKind  = "natural-session-activity-v1"
	naturalTurnStreamKind      = "natural-session-turn-v1"
	naturalResultStreamKind    = "natural-result-v1"
	naturalBootstrapStreamKind = "natural-session-bootstrap-v1"
)

// naturalSessionSignal is the typed fact one translated row carries.
type naturalSessionSignal struct {
	Signal  string // published catalog kind
	Runtime string
	// CatalogSessionID and NativeSessionID carry the session's identity
	// alternates; scope matching offers both. NativeSessionID is the hook's
	// own session id; CatalogSessionID resolves it through the catalog owner
	// and falls back to the hook id.
	CatalogSessionID string
	NativeSessionID  string
	ProjectRoot      string // the sessions row's cwd as recorded, "" when it had none
	Producer         string // durable stream kind the row came from
	EventRowID       int64  // that stream's rowid — idempotency anchor
	At               int64  // daemon-clock milliseconds
	Payload          map[string]any
}

// errNaturalSettle stops a translation pass before a row inside the launch
// settle window (red-team M6): a session the daemon may have just launched
// has hook rows before its task records a native id, so the row waits for
// the next pass rather than firing off the natural path.
var errNaturalSettle = errors.New("natural row inside task settle window")

// signalForNaturalActivity maps one durable activity row's evidence class
// onto the catalog. Entry kinds follow the session-start/closure collection
// contracts: "first-action"/explicit opens start, "end" ends.
func signalForNaturalActivity(row store.SessionActivityObservation) (string, bool) {
	switch row.EntryKind {
	case "first-action", "start":
		return "session.started", true
	case "end":
		return "session.ended", true
	}
	return "", false
}

// naturalIdentityCache memoizes one pass's identity resolution: resolving a
// hook id through the catalog is a filesystem scan, and one busy session can
// contribute most of a pass's rows.
type naturalIdentityCache map[string][2]string

// resolve maps one hook session id onto both identity alternates plus the
// exact root the sessions row recorded. A known catalog id (turn rows store
// the one ingest resolved) is used as-is.
func (cache naturalIdentityCache) resolve(ix *store.Index, runtime, hookSessionID, knownCatalogID string) (catalogID, root string) {
	key := runtime + "\x00" + hookSessionID
	if hit, ok := cache[key]; ok {
		return hit[0], hit[1]
	}
	catalogID = knownCatalogID
	if catalogID == "" {
		catalogID = resolveCatalogSessionID(runtime, hookSessionID)
	}
	if catalogID == "" {
		catalogID = hookSessionID
	}
	if session, found, err := ix.SessionByID(runtime, hookSessionID); err == nil && found {
		root = session.CWD
	}
	cache[key] = [2]string{catalogID, root}
	return catalogID, root
}

// emitNaturalSessionSignals translates durable activity rows after the stored
// position, routing each onto the binding machinery. Bounded, resumable, and
// quiet when nothing matches — the same resilience contract as the task pump.
func emitNaturalSessionSignals(ix *store.Index, route func(naturalSessionSignal) error) error {
	position, err := ix.OrchestrationStreamPosition(naturalActivityStreamKind)
	if err != nil {
		return fmt.Errorf("natural signal position: %w", err)
	}
	rows, last, err := ix.SessionActivityAfter(position, 200)
	if err != nil {
		return fmt.Errorf("natural signal read: %w", err)
	}
	identities := naturalIdentityCache{}
	for _, row := range rows {
		// Only natural-lifecycle evidence translates; runtime-less rows and
		// replay quarantine artifacts carry other entry kinds and stay inert.
		signal, ok := signalForNaturalActivity(row)
		if !ok || row.Runtime == "" || row.SessionID == "" {
			continue
		}
		catalogID, root := identities.resolve(ix, row.Runtime, row.SessionID, "")
		err := route(naturalSessionSignal{Signal: signal, Runtime: row.Runtime,
			CatalogSessionID: catalogID, NativeSessionID: row.SessionID,
			ProjectRoot: root, Producer: naturalActivityStreamKind, EventRowID: row.RowID,
			At: row.ReceivedAt * 1000})
		if errors.Is(err, errNaturalSettle) {
			last = row.RowID - 1
			break
		}
		if err != nil {
			return fmt.Errorf("route natural %s for %s/%s: %w", signal, row.Runtime, row.SessionID, err)
		}
	}
	return advanceNaturalPosition(ix, naturalActivityStreamKind, position, last)
}

// signalForNaturalTurn maps one durable turn row onto the catalog: the two
// turn boundaries every session has. Attention and subagent kinds stay inert.
func signalForNaturalTurn(row store.SessionTurnObservation) (string, bool) {
	switch row.Kind {
	case "turn.started":
		return "session.turn-started", true
	case "turn.ended":
		return "session.turn-ended", true
	}
	return "", false
}

// emitNaturalTurnSignals translates durable session-turn rows (plan D2).
func emitNaturalTurnSignals(ix *store.Index, route func(naturalSessionSignal) error) error {
	position, err := ix.OrchestrationStreamPosition(naturalTurnStreamKind)
	if err != nil {
		return fmt.Errorf("natural turn position: %w", err)
	}
	rows, last, err := ix.SessionTurnsAfter(position, 200)
	if err != nil {
		return fmt.Errorf("natural turn read: %w", err)
	}
	identities := naturalIdentityCache{}
	for _, row := range rows {
		signal, ok := signalForNaturalTurn(row)
		if !ok || row.Runtime == "" || row.SessionID == "" {
			continue
		}
		catalogID, root := identities.resolve(ix, row.Runtime, row.SessionID, row.CatalogSessionID)
		err := route(naturalSessionSignal{Signal: signal, Runtime: row.Runtime,
			CatalogSessionID: catalogID, NativeSessionID: row.SessionID,
			ProjectRoot: root, Producer: naturalTurnStreamKind, EventRowID: row.RowID,
			At: row.ReceivedAtMS})
		if errors.Is(err, errNaturalSettle) {
			last = row.RowID - 1
			break
		}
		if err != nil {
			return fmt.Errorf("route natural %s for %s/%s: %w", signal, row.Runtime, row.SessionID, err)
		}
	}
	return advanceNaturalPosition(ix, naturalTurnStreamKind, position, last)
}

// emitNaturalResultSignals translates live post-tool completions (plan D2):
// the pause every helper's next context lands on. The reader already filters
// to hook-delivered rows, so transcript-derived rows never fire twice.
func emitNaturalResultSignals(ix *store.Index, route func(naturalSessionSignal) error) error {
	position, err := ix.OrchestrationStreamPosition(naturalResultStreamKind)
	if err != nil {
		return fmt.Errorf("natural result position: %w", err)
	}
	rows, last, err := ix.ResultObservationsAfter(position, 200)
	if err != nil {
		return fmt.Errorf("natural result read: %w", err)
	}
	identities := naturalIdentityCache{}
	for _, row := range rows {
		if row.Runtime == "" || row.SessionID == "" {
			continue
		}
		catalogID, root := identities.resolve(ix, row.Runtime, row.SessionID, "")
		at := row.ReceivedAt * 1000
		if at == 0 {
			at = row.CompletedAt * 1000
		}
		err := route(naturalSessionSignal{Signal: "session.tool-completed", Runtime: row.Runtime,
			CatalogSessionID: catalogID, NativeSessionID: row.SessionID,
			ProjectRoot: root, Producer: naturalResultStreamKind, EventRowID: row.ID,
			At: at, Payload: map[string]any{"result_id": row.ID, "name": row.Tool}})
		if errors.Is(err, errNaturalSettle) {
			last = row.ID - 1
			break
		}
		if err != nil {
			return fmt.Errorf("route natural session.tool-completed for %s/%s: %w", row.Runtime, row.SessionID, err)
		}
	}
	return advanceNaturalPosition(ix, naturalResultStreamKind, position, last)
}

func advanceNaturalPosition(ix *store.Index, kind string, position, last int64) error {
	if last <= position {
		return nil
	}
	if err := ix.PutOrchestrationStreamPosition(kind, last, time.Now().Unix()); err != nil {
		return fmt.Errorf("advance %s position: %w", kind, err)
	}
	return nil
}

// sessionIsTaskOwned reports whether the console task stream owns this session
// identity — the double-fire guard (red-team M10). A task-owned session's
// lifecycle signals route through task events only; natural emission is
// suppressed for the task's whole lifetime including the resume window.
func (host *orchestrationManagedHost) sessionIsTaskOwned(signal naturalSessionSignal) bool {
	if host.tasks == nil {
		return false
	}
	tasks, err := host.tasks.List(signal.Runtime, signal.CatalogSessionID, 1)
	if err == nil && len(tasks) > 0 {
		return true
	}
	if signal.NativeSessionID != "" && signal.NativeSessionID != signal.CatalogSessionID {
		tasks, err = host.tasks.List(signal.Runtime, signal.NativeSessionID, 1)
		if err == nil && len(tasks) > 0 {
			return true
		}
	}
	return false
}

// routeNaturalSignal runs one natural signal through every enabled
// natural-watching binding's selector map with the same arbitration,
// annotators-before-actors hold, and budget discipline the task path uses.
// session.turn-ended IS terminal here; what keeps a helper from resuming a
// session the daemon does not own is the capability answer in
// finishManagedChild (sourceSessionTaskOwned → attended_session, plan D6),
// never terminality.
func (host *orchestrationManagedHost) routeNaturalSignal(signal naturalSessionSignal) error {
	// Double-fire guard: task-owned sessions stay task-owned (M10).
	if host.sessionIsTaskOwned(signal) {
		return nil
	}
	// Launch window (M6): a row this fresh may belong to a task whose native
	// id is not recorded yet; wait for the next pass before deciding it is
	// natural. Bootstrap signals carry no row time and are never settled.
	if signal.At > 0 && time.Since(time.UnixMilli(signal.At)) < orchestrationConfig().TaskSettle() {
		return errNaturalSettle
	}
	bindings, err := host.ix.ManagedBindings(true)
	if err != nil {
		return err
	}
	matches := []matchedAgent{}
	folder := newFolderScope(signal.ProjectRoot)
	for _, binding := range bindings {
		if !binding.WatchNatural || !naturalScopeMatches(binding, signal, folder) {
			continue
		}
		if match, ok := host.selectBinding(binding, signal.Signal); ok {
			matches = append(matches, match)
		}
	}
	if len(matches) == 0 {
		return nil
	}
	act, deferredMatches, winner := arbitrateAgents(matches)
	// The natural-session pseudo task/event: the group and run idempotency
	// keys anchor to the durable activity rowid so one lifecycle row fires a
	// binding exactly once, restart included.
	task := RuntimeTask{ID: naturalTaskID(signal), Runtime: signal.Runtime,
		CatalogSessionID: signal.CatalogSessionID, NativeSessionID: signal.NativeSessionID,
		WorkingDirectory: signal.ProjectRoot}
	event := TaskEvent{Producer: signal.Producer, EventID: signal.EventRowID, TaskID: task.ID,
		Kind: "natural." + signal.Signal, OccurredAt: signal.At, Payload: signal.Payload}
	for _, match := range deferredMatches {
		if err := host.recordDeferredRun(match, winner, "priority_deferred",
			"Helper "+winner+" holds higher priority for this signal; raise this agent's priority or disable the winner to let it act.",
			task, event, signal.Signal, nil); err != nil {
			return err
		}
	}
	passiveMatched := false
	for _, match := range act {
		if match.compiled.AgentType() == "follower" {
			passiveMatched = true
			if err := host.launchNaturalAgentRun(match, task, event, signal); err != nil {
				return err
			}
		}
	}
	for _, match := range act {
		if match.compiled.AgentType() == "follower" {
			continue
		}
		if passiveMatched && match.compiled.AwaitAnnotations != "never" {
			key := heldKey(task.ID, event.EventID)
			host.mu.Lock()
			if host.held == nil {
				host.held = map[string][]heldLaunch{}
			}
			host.held[key] = append(host.held[key], heldLaunch{match: match, task: task, event: event, signal: signal.Signal})
			host.mu.Unlock()
			continue
		}
		if err := host.launchNaturalAgentRun(match, task, event, signal); err != nil {
			return err
		}
	}
	if passiveMatched {
		host.maybeReleaseHeld(task.ID, event.EventID)
	}
	return nil
}

// naturalTaskID anchors group identity to one natural session (not one row):
// every signal for the same session shares the group, so loop counters and
// budgets behave exactly as the console path's do.
func naturalTaskID(signal naturalSessionSignal) string {
	id := signal.CatalogSessionID
	if id == "" {
		id = signal.NativeSessionID
	}
	return "natural:" + signal.Runtime + ":" + id
}

// naturalScopeMatches is the consent gate: watch_natural opted in (checked by
// the caller), runtime scope matched, session scope matched when pinned, and
// the session working in the binding's EXACT folder under any spelling — a
// subdirectory or cwd-less session never triggers (the same rule the task path
// enforces). folder is signal.ProjectRoot's scope.
func naturalScopeMatches(binding store.ManagedBinding, signal naturalSessionSignal, folder *folderScope) bool {
	if binding.ScopeRuntime != "" && binding.ScopeRuntime != signal.Runtime {
		return false
	}
	if binding.ScopeSession != "" && binding.ScopeSession != signal.CatalogSessionID &&
		(signal.NativeSessionID == "" || binding.ScopeSession != signal.NativeSessionID) {
		return false
	}
	return folder.matchesRoot(binding.ProjectRoot)
}

// launchNaturalAgentRun admits and launches one binding's run off a natural
// signal through the same group/admission machinery; the prompt builder and
// every claim/annotator path are shared with the task stream.
func (host *orchestrationManagedHost) launchNaturalAgentRun(match matchedAgent, task RuntimeTask, event TaskEvent, signal naturalSessionSignal) error {
	// The pairing group is looked up by the session identity the synthetic
	// task carries (plan D1), exactly as the task path does.
	if err := host.launchAgentRun(match, task, event, signal.Signal, nil); err != nil {
		// A suppressed admission is terminal and honest, never a pump error.
		var suppressed errManagedSuppressed
		if errors.As(err, &suppressed) {
			log.Printf("natural signal: binding %s admission suppressed (%s): %s",
				match.binding.BindingID, suppressed.class, suppressed.recovery)
			return nil
		}
		return err
	}
	return nil
}

// emitSessionActiveBootstrap fires the `session.active` catalog signal for
// every currently-open session the freshly saved binding matches (red-team
// H4). The session-activity service is the one presence owner: its open
// items ARE the open sessions. Idempotency: the run key embeds the binding's
// state token, so one save fires once. The token excludes the update time, so
// turning a place off and on again with nothing else changed restores the same
// token and admits nothing new; the sessions it already joined keep going.
func (host *orchestrationManagedHost) emitSessionActiveBootstrap(binding store.ManagedBinding) {
	if sessionActivityService() == nil {
		return
	}
	snapshot := sessionActivityService().Snapshot()
	for _, item := range snapshot.Items {
		if item.Presence != "open" || item.Runtime == "" {
			continue
		}
		// At stays 0: a bootstrap carries no row time and is never settled.
		signal := naturalSessionSignal{Signal: "session.active", Runtime: item.Runtime,
			CatalogSessionID: item.CatalogSessionID, NativeSessionID: item.NativeSessionID,
			ProjectRoot: ""}
		if item.CatalogSessionID == "" {
			continue
		}
		// Exact-root scoping needs the session's cwd; the activity item does
		// not carry it, so the sessions row is the one owner to ask.
		if session, found, err := host.ix.SessionByID(item.Runtime, item.CatalogSessionID); err == nil && found {
			signal.ProjectRoot = session.CWD
		}
		if !naturalScopeMatches(binding, signal, newFolderScope(signal.ProjectRoot)) || host.sessionIsTaskOwned(signal) {
			continue
		}
		if err := host.routeNaturalSignalToBinding(binding, signal); err != nil {
			log.Printf("session.active bootstrap for binding %s failed (%s/%s): %v",
				binding.BindingID, item.Runtime, item.CatalogSessionID, err)
		}
	}
}

// routeNaturalSignalToBinding routes one bootstrap signal through a single
// freshly saved binding (the selector/arbitration machinery runs for that
// binding alone — it was just validated, and multi-binding arbitration would
// re-fire peers on every save).
func (host *orchestrationManagedHost) routeNaturalSignalToBinding(binding store.ManagedBinding, signal naturalSessionSignal) error {
	detail, err := host.profiles.GetRevision(binding.ProfileID, binding.ProfileSourceDigest, binding.ProfileBundleDigest)
	if err != nil || detail.Normalized == nil {
		return errors.New("profile revision unavailable")
	}
	compiled := *detail.Normalized
	if validateManagedProfile(compiled) != nil {
		return errors.New("profile is not a managed orchestration agent")
	}
	stagePrompt, selected := profileSignalSelectors(compiled)[signal.Signal]
	if !selected {
		return nil
	}
	match := matchedAgent{binding: binding, compiled: compiled, stagePrompt: stagePrompt}
	task := RuntimeTask{ID: naturalTaskID(signal), Runtime: signal.Runtime,
		CatalogSessionID: signal.CatalogSessionID, NativeSessionID: signal.NativeSessionID,
		WorkingDirectory: signal.ProjectRoot}
	event := TaskEvent{Producer: naturalBootstrapStreamKind, EventID: naturalBootstrapEventID(binding), TaskID: task.ID, Kind: "natural.session.active"}
	if compiled.AgentType() == "follower" {
		return host.launchNaturalAgentRun(match, task, event, signal)
	}
	return host.launchNaturalAgentRun(match, task, event, signal)
}

// naturalBootstrapEventID derives a stable per-binding event anchor for the
// bootstrap fire: the binding's state token means one save = one event id, and
// the run idempotency deduplicates restarts.
func naturalBootstrapEventID(binding store.ManagedBinding) int64 {
	h := crc32.ChecksumIEEE([]byte(binding.StateToken))
	return int64(h)
}

// bootstrapNaturalStreamPositions gives each natural stream a position row at
// its table's current head the first time this host meets it. Without it a
// new stream kind starts at zero and REPLAYS the whole table as fresh signals:
// on 2026-09-12 the installed daemon fired a memory helper on every historical
// tool result in scope (2,992 admissions, 15 launched) the minute a binding went
// live. History written before anyone subscribed is not a signal.
func bootstrapNaturalStreamPositions(ix *store.Index) error {
	now := time.Now().Unix()
	for _, stream := range []struct {
		kind string
		head func() (int64, error)
	}{
		{naturalActivityStreamKind, ix.SessionActivityHead},
		{naturalTurnStreamKind, ix.SessionTurnHead},
		{naturalResultStreamKind, ix.ResultObservationHead},
		{naturalTagStreamKind, bootstrapTagJournalHead(ix)},
		{naturalUncommittedStreamKind, ix.UncommittedWorkFacetHead},
	} {
		head, err := stream.head()
		if err != nil {
			return fmt.Errorf("%s head: %w", stream.kind, err)
		}
		created, err := ix.EnsureOrchestrationStreamPosition(stream.kind, head, now)
		if err != nil {
			return fmt.Errorf("%s bootstrap: %w", stream.kind, err)
		}
		if created && head > 0 {
			log.Printf("natural signal stream %s starts at head %d (%d earlier rows are history, not signals)", stream.kind, head, head)
		}
	}
	return nil
}

// emitNaturalSignalsOnce is the emitter-cadence entry point: one bounded,
// durable-positioned translation pass per row family. Loud on failure, never
// fatal to the caller — transient store contention retries once in-process
// (the pump's drain discipline), and anything still failing records the
// problem for the next pass, which resumes from the same durable position (no
// signal is lost: a position only advances after successful routing).
//
// It reports whether a pass stopped on a row inside the settle window, so the
// loop can re-arm for exactly that window instead of waiting for the next
// ingest nudge or safety sweep (a held row is typically the LAST row before a
// pause — the one a helper most wants).
func (host *orchestrationManagedHost) emitNaturalSignalsOnce() (held bool) {
	// Delivery expiry rides this cadence whether or not a pass below fails.
	defer host.expireDeliveriesOnce()
	passes := []struct {
		name string
		run  func(*store.Index, func(naturalSessionSignal) error) error
	}{
		{"activity", emitNaturalSessionSignals},
		{"turn", emitNaturalTurnSignals},
		{"result", emitNaturalResultSignals},
		{"tag", emitTagChangeSignals},
		{"uncommitted", emitUncommittedWorkSignals},
	}
	for _, pass := range passes {
		route := func(signal naturalSessionSignal) error {
			err := host.routeNaturalSignal(signal)
			if errors.Is(err, errNaturalSettle) {
				held = true
			}
			// Flow consumers ride the same pass (orchestration-flows):
			// tag signals drive membership; transitions evaluate after
			// membership so a freshly admitted member can move on the same
			// signal; the flow's journal position advances only after its
			// membership write.
			host.flowSignalConsumer(signal)
			host.flowSignalTransitionConsumer(signal)
			return err
		}
		err := pass.run(host.ix, route)
		if err != nil && strings.Contains(err.Error(), "database is locked") {
			time.Sleep(100 * time.Millisecond)
			err = pass.run(host.ix, route)
		}
		if err != nil {
			host.setProblem("Natural-session " + pass.name + " signal emission is failing: " + err.Error())
			return held
		}
	}
	// Flow enablement/evaluation cadence (orchestration-flows slice B): the
	// emitter's sweep is the one cadence the flow mechanisms ride —
	// enablement sync, the one-per-enablement evaluation, and nothing else.
	if err := host.flowEnablementSync(time.Now().Unix()); err != nil {
		host.setProblem("Flow enablement sync is failing: " + err.Error())
	}
	host.sweepStuckSessionMessageInvocations()
	return held
}

// requestNaturalSignalEmit is the ingest-side nudge (plan D2): a hook row was
// just written, so the emitter runs after the coalesce window instead of
// waiting for its safety sweep. Non-blocking; a pending nudge absorbs repeats.
func requestNaturalSignalEmit() {
	host := managedHost
	if host == nil || host.nudge == nil {
		return
	}
	select {
	case host.nudge <- struct{}{}:
	default:
	}
}

// runNaturalSignalEmitter is the emitter's own loop: its safety sweep and the
// coalesced ingest nudge, both configuration (plan D8). It replaces the ride on
// the lifecycle coordinator's sweep, which also paces parked-run relaunch.
func (host *orchestrationManagedHost) runNaturalSignalEmitter() {
	defer host.wg.Done()
	config := orchestrationConfig()
	sweep, coalesce := config.Sweep(), config.Coalesce()
	if sweep <= 0 || coalesce <= 0 {
		// Never trust a swapped or half-loaded value with a ticker.
		fallback := defaultOrchestrationConfig()
		sweep, coalesce = fallback.Sweep(), fallback.Coalesce()
	}
	ticker := time.NewTicker(sweep)
	defer ticker.Stop()
	settle := config.TaskSettle()
	if settle <= 0 {
		settle = defaultOrchestrationConfig().TaskSettle()
	}
	// rearm fires once after a pass held a row inside the settle window.
	var rearm <-chan time.Time
	run := func() {
		if host.emitNaturalSignalsOnce() {
			rearm = time.After(settle + coalesce)
		} else {
			rearm = nil
		}
	}
	for {
		select {
		case <-host.ctx.Done():
			return
		case <-ticker.C:
			run()
		case <-rearm:
			run()
		case <-host.nudge:
			select {
			case <-host.ctx.Done():
				return
			case <-time.After(coalesce):
			}
			run()
		}
	}
}
