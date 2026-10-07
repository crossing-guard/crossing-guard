package orchestration

import "crossing-guard/profiledoc"

// The signal catalog lives in the top-level profiledoc package, because a portable
// profile's trigger is validated against it wherever the profile is parsed (team
// rest-of-release plan §4.1 decision 2). These names keep every caller unchanged.

// Signal is one published signal kind.
type Signal = profiledoc.Signal

// SignalCatalog lists every published signal kind. Order is display order.
func SignalCatalog() []Signal { return profiledoc.SignalCatalog() }

// SupersededSignal reports the session-scoped kind that supersedes kind for
// wildcard selection, or "" when kind stands on its own.
func SupersededSignal(kind string) string { return profiledoc.SupersededSignal(kind) }

// KnownSignal reports whether kind names a published signal.
func KnownSignal(kind string) bool { return profiledoc.KnownSignal(kind) }

// TerminalSignal reports whether kind is a published terminal signal.
func TerminalSignal(kind string) bool { return profiledoc.TerminalSignal(kind) }

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
// (helper-session-attachment plan D3). A managed task is exactly one turn. It
// starts at the task's first session frame, the event where the runtime
// reports the session it runs in: at spawn a new session has no identity yet,
// so context, delivery and grouping would address nothing (managed turn start
// identity plan D1). Which event is the first frame depends on the payload and
// the task's own stream, so the caller decides and passes firstSessionFrame;
// this function only names the kinds. "" means the event is not a
// session-scoped fact.
func SessionSignalForTaskEvent(event string, firstSessionFrame bool) string {
	if firstSessionFrame {
		return "session.turn-started"
	}
	switch event {
	case "tool.completed":
		return "session.tool-completed"
	case "completed", "task.completed":
		return "session.turn-ended"
	}
	return ""
}

// TriggerLabel is the plain name of a trigger event: a catalog signal's label,
// or the review lane's pre-tool action (reviewed one action at a time, not a
// catalog signal).
func TriggerLabel(event string) string {
	if event == "pretool.action" {
		return "A tool is about to run"
	}
	for _, signal := range SignalCatalog() {
		if signal.Kind == event {
			return signal.Label
		}
	}
	return event
}
