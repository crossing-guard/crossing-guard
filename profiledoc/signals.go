package profiledoc

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
	// Label is the short plain name the Agents pages show ("A turn starts").
	Label string `json:"label"`
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
		{Kind: "task.completed", Label: "A console task finishes", Source: "task-events", Terminal: true, Superseded: "session.turn-ended",
			Description: "A managed task finished and returned its final response (managed task stream only; session.turn-ended reports the same boundary for every session)."},
		{Kind: "task.failed", Label: "A console task fails", Source: "task-events", Terminal: true,
			Description: "A managed task ended in failure; the failure is the fact, never a pretend success."},
		{Kind: "task.interrupted", Label: "A console task is interrupted", Source: "task-events", Terminal: true,
			Description: "A managed task was interrupted before completion."},
		{Kind: "task.unknown", Label: "A console task ends in an unknown state", Source: "task-events", Terminal: true,
			Description: "A managed task's outcome could not be determined (for example after a restart)."},
		{Kind: "task.message-completed", Label: "A console task finishes a message", Source: "task-events", Terminal: false,
			Description: "One assistant message inside a running managed task completed (managed task stream only; no natural-session equivalent is served)."},
		{Kind: "task.tool-completed", Label: "A console task finishes a tool call", Source: "task-events", Terminal: false, Superseded: "session.tool-completed",
			Description: "One tool call inside a running managed task completed (managed task stream only; session.tool-completed reports the same fact for every session)."},
		{Kind: "approval.pending", Label: "An approval is waiting", Source: "approvals", Terminal: false,
			Description: "Governance holds an ask and a delegated review may recommend within its subdeadline."},
		{Kind: "session.started", Label: "A session starts", Source: "session-activity", Terminal: false,
			Description: "A session began under this binding's scope."},
		{Kind: "session.active", Label: "An open session is attached", Source: "session-activity", Terminal: false,
			Description: "A natural session is already open and a natural-watching binding began matching it (bootstrap for mid-session attach)."},
		{Kind: "session.ended", Label: "A session ends", Source: "session-activity", Terminal: false,
			Description: "A session under this binding's scope recorded an explicit end. Non-terminal by design: helper auto-reply stays suppressed off natural sessions (Slice D owns any change)."},
		// Session-scoped work facts (helper-session-attachment plan D1): the
		// same kind whether the daemon launched the session or the user did.
		// The task pump and the hook-row emitter both publish them; which
		// runtimes serve each natural row is published per runtime, never
		// compiled here.
		{Kind: "session.turn-started", Label: "A turn starts", Source: "session-activity", Terminal: false,
			Description: "A turn began in a session under this binding's scope (the user submitted a prompt, or a managed task's runtime reported the session it runs in; a task that never reaches its session fires none)."},
		{Kind: "session.tool-completed", Label: "A tool call finishes", Source: "session-activity", Terminal: false,
			Description: "One tool call completed in a session under this binding's scope — the pause where a helper's context lands next."},
		{Kind: "session.turn-ended", Label: "A turn ends and hands back", Source: "session-activity", Terminal: true,
			Description: "A turn ended in a session under this binding's scope (the agent handed back, or a managed task completed). Terminal: a helper may act; whether a reply can land is a delivery capability, not a helper concern."},
		// Daemon-owned fact streams (orchestration-flows pilot, pass-1 RT-3
		// contract amendment): the source is a daemon-owned durable row
		// family, not a runtime-emitted stream, so per-runtime service
		// parity means TRANSLATOR AVAILABILITY, reported through the same
		// capability surface with the source named daemon-side. The facts
		// are generic: the journal records every owner tag change for every
		// session regardless of any flow, and which tags matter is decided
		// by configuration, never here.
		{Kind: "session.tag-applied", Label: "An owner tag is applied", Source: "owner-tag-journal", Terminal: false,
			Description: "The owner applied a tag to a session (journal fact; daemon-side source, parity is translator availability)."},
		{Kind: "session.tag-removed", Label: "An owner tag is removed", Source: "owner-tag-journal", Terminal: false,
			Description: "The owner removed a tag from a session — by retract, rename or purge (journal fact; daemon-side source, parity is translator availability)."},
		{Kind: "session.uncommitted-work", Label: "A session settles with uncommitted edits", Source: "checkpoint-settle", Terminal: false,
			Description: "A settled checkpoint observed file edits with no covering commit (monitoring fact authored at settle; daemon-side source, parity is translator availability). Not the folded vcs=commit facet."},
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
