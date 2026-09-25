// The open session's conversation, as one ordered list of canonical events.
//
// This is deliberately NOT TaskProjectionStore. That store is keyed by task id
// and defaults everything it materializes to crossing-guard ownership with
// controls enabled — feeding a terminal-driven session through it would invent
// an owned, stoppable task for work Crossing Guard neither started nor can
// stop. Sessions are not tasks, so they get their own small store.
//
// It holds no server data beyond the rows currently rendered, owns no DOM, and
// makes no requests: the client feeds it frames, the view reads it back.

import { statusLabel, humanDuration } from '../task/session-status.js';

export class SessionEventStore {
  constructor() {
    this.events = [];
    this.lastSeq = 0;
    this.renderedAnchors = new Set();
    this.renderedPrompts = []; // exact prompt texts the composer sent, each consumed once
    this.turnState = null;
  }

  // reset drops everything for a new session selection. The store never
  // carries rows across sessions — a stale row in the wrong conversation is a
  // worse failure than an empty pane.
  reset() {
    this.events = [];
    this.lastSeq = 0;
    this.renderedAnchors.clear();
    this.renderedPrompts = [];
    this.turnState = null;
  }

  // markRenderedLive records that a turn was already painted from the live
  // composer lane. Called with whatever anchor that lane can supply; an empty
  // anchor is ignored rather than guessed at, because a wrong anchor would
  // silently delete somebody's turn.
  markRenderedLive(anchor) {
    if (anchor) this.renderedAnchors.add(String(anchor));
  }

  // markPromptRenderedLive records a prompt the composer drew. The vendor's
  // stream carries no identity for the prompt record, so this is the ONE
  // text-keyed match in the store: exact equality, consumed by the first
  // harvested user event that matches, never a similarity guess.
  markPromptRenderedLive(text) {
    if (typeof text === 'string' && text.length) this.renderedPrompts.push(text);
  }

  // accept returns only the events the view should append: new by sequence,
  // and not already drawn live. Events with no anchor are always appended —
  // when we cannot identify a turn we show it, because a visible duplicate is
  // recoverable and a deleted turn is not.
  accept(incoming) {
    const fresh = [];
    for (const event of Array.isArray(incoming) ? incoming : []) {
      const seq = Number(event?.seq) || 0;
      if (seq && seq <= this.lastSeq) continue;
      const anchor = event?.turn_anchor;
      if (anchor && this.renderedAnchors.has(String(anchor))) continue;
      if (event?.kind === 'user' && this.renderedPrompts.length) {
        const at = this.renderedPrompts.indexOf(String(event.text || ''));
        if (at >= 0) { this.renderedPrompts.splice(at, 1); continue; }
      }
      if (seq > this.lastSeq) this.lastSeq = seq;
      this.events.push(event);
      fresh.push(event);
    }
    return fresh;
  }

  setTurnState(state) {
    if (state && typeof state === 'object') this.turnState = state;
    return this.turnState;
  }
}

// describeTurnState renders the daemon's status frame for the pane header. It
// is the SAME labeler the rail uses (task/session-status.js), so the header
// and the dot can never disagree about one session.
export function describeTurnState(item, now = Date.now(), seen = false) {
  return statusLabel(item, now, seen);
}

export { humanDuration };

// Done 2026-09-02: the composer reports each block it drew by the vendor's
// record identity (the daemon forwards it as `anchor` on the owned stream),
// and each prompt it sent by exact text; the harvested copies are skipped
// here. Vendors whose owned stream carries no record identity still draw
// their composer turns twice, and that is stated, not hidden.
