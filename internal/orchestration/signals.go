package orchestration

// The signal catalog is the one published vocabulary of signal kinds the lower
// layers actually emit. Profiles map selectors to prompts against THIS catalog at
// binding time; no stage or workflow vocabulary may be compiled anywhere else
// (owner rule, 2026-08-29). Adding a signal kind means the emitting owner
// publishes it here with its source; nothing else changes.
//
// A catalog entry says what the signal IS. Whether a live pump currently serves
// it is reported separately by the daemon (per-runtime capability/parity data),
// so an authored profile naming a real-but-unserved signal is honestly
// "incompatible here", never silently ignored.

type Signal struct {
	// Kind is the canonical selector string profiles use.
	Kind string `json:"kind"`
	// Source names the owning lower-layer stream.
	Source string `json:"source"`
	// Terminal marks end-of-work signals eligible for helper reply actions.
	// Message delivery and interruption have separate capability/grant checks.
	Terminal bool `json:"terminal"`
	// Description is operator-facing copy for the Agents surface.
	Description string `json:"description"`
	// Superseded names the session-scoped kind that reports the same fact for
	// every session (owned or natural). A wildcard selector skips a superseded
	// kind so one fact fires a binding once; explicit selection still works.
	Superseded string `json:"superseded,omitempty"`
}

// SignalCatalog lists every published signal kind. Order is display order.
func SignalCatalog() []Signal {
	return []Signal{
		{Kind: "task.completed", Source: "task-events", Terminal: true, Superseded: "session.turn-ended",
			Description: "A managed task finished and returned its final response (managed task stream only; session.turn-ended reports the same boundary for every session)."},
		{Kind: "task.failed", Source: "task-events", Terminal: true,
			Description: "A managed task ended in failure; the failure is the fact, never a pretend success."},
		{Kind: "task.interrupted", Source: "task-events", Terminal: true,
			Description: "A managed task was interrupted before completion."},
		{Kind: "task.unknown", Source: "task-events", Terminal: true,
			Description: "A managed task's outcome could not be determined (for example after a restart)."},
		{Kind: "task.message-completed", Source: "task-events", Terminal: false,
			Description: "One assistant message inside a running managed task completed (managed task stream only; no natural-session equivalent is served)."},
		{Kind: "task.tool-completed", Source: "task-events", Terminal: false, Superseded: "session.tool-completed",
			Description: "One tool call inside a running managed task completed (managed task stream only; session.tool-completed reports the same fact for every session)."},
		{Kind: "approval.pending", Source: "approvals", Terminal: false,
			Description: "Governance holds an ask and a delegated review may recommend within its subdeadline."},
		{Kind: "session.started", Source: "session-activity", Terminal: false,
			Description: "A session began under this binding's scope."},
		{Kind: "session.active", Source: "session-activity", Terminal: false,
			Description: "A natural session is already open and a natural-watching binding began matching it (bootstrap for mid-session attach)."},
		{Kind: "session.ended", Source: "session-activity", Terminal: false,
			Description: "A session under this binding's scope recorded an explicit end. Non-terminal by design: helper auto-reply stays suppressed off natural sessions (Slice D owns any change)."},
		// Session-scoped work facts (helper-session-attachment plan D1): the
		// same kind whether the daemon launched the session or the user did.
		// The task pump and the hook-row emitter both publish them; which
		// runtimes serve each natural row is published per runtime, never
		// compiled here.
		{Kind: "session.turn-started", Source: "session-activity", Terminal: false,
			Description: "A turn began in a session under this binding's scope (the user submitted a prompt, or a managed task started)."},
		{Kind: "session.tool-completed", Source: "session-activity", Terminal: false,
			Description: "One tool call completed in a session under this binding's scope — the pause where a helper's context lands next."},
		{Kind: "session.turn-ended", Source: "session-activity", Terminal: true,
			Description: "A turn ended in a session under this binding's scope (the agent handed back, or a managed task completed). Terminal: a helper may act; whether a reply can land is a delivery capability, not a helper concern."},
	}
}

// SupersededSignal reports the session-scoped kind that supersedes kind for
// wildcard selection, or "" when kind stands on its own.
func SupersededSignal(kind string) string {
	for _, signal := range SignalCatalog() {
		if signal.Kind == kind {
			return signal.Superseded
		}
	}
	return ""
}

// KnownSignal reports whether kind names a published signal.
func KnownSignal(kind string) bool {
	for _, signal := range SignalCatalog() {
		if signal.Kind == kind {
			return true
		}
	}
	return false
}

// TerminalSignal reports whether kind is a published terminal signal.
func TerminalSignal(kind string) bool {
	for _, signal := range SignalCatalog() {
		if signal.Kind == kind {
			return signal.Terminal
		}
	}
	return false
}

// SignalForTaskEvent maps the task-event pump's historical event strings onto
// catalog kinds. The duplicated vocabulary (profile-side "task.message-completed"
// vs event-side "message.completed") was a recorded defect; the pump now speaks
// catalog kinds through this one translation and the old strings never spread.
func SignalForTaskEvent(event string) string {
	switch event {
	case "completed":
		return "task.completed"
	case "failed":
		return "task.failed"
	case "interrupted":
		return "task.interrupted"
	case "unknown":
		return "task.unknown"
	case "message.completed":
		return "task.message-completed"
	case "tool.completed":
		return "task.tool-completed"
	}
	if KnownSignal(event) {
		return event
	}
	return ""
}

// SessionSignalForTaskEvent maps a managed task stream event onto the
// session-scoped kind that reports the same fact for every session
// (helper-session-attachment plan D3). A managed task is exactly one turn, so
// its start and completion are that turn's boundaries. "" means the event is
// not a session-scoped fact.
func SessionSignalForTaskEvent(event string) string {
	switch event {
	case "task.started":
		return "session.turn-started"
	case "tool.completed":
		return "session.tool-completed"
	case "completed", "task.completed":
		return "session.turn-ended"
	}
	return ""
}
