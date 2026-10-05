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

const NOOP = Object.freeze({ type: 'noop', events: [] });
const PENDING = Object.freeze({ type: 'pending', events: [] });

// Provider transcripts sometimes persist closing metadata after the assistant
// answer it describes. Keep every canonical row and its identity, but present a
// trailing metadata suffix before the answer so the conversation still ends in
// the answer. User rows delimit exchanges; tool and conversational rows never
// move. This consumes normalized kinds and contains no runtime-specific branch.
function orderConversationEvents(events) {
  const source = Array.isArray(events) ? events : [];
  const ordered = [];
  const appendExchange = exchange => {
    let assistant = -1;
    for (let i = exchange.length - 1; i >= 0; i--) {
      if (exchange[i]?.kind === 'assistant') { assistant = i; break; }
    }
    if (assistant < 0 || assistant === exchange.length - 1) {
      ordered.push(...exchange);
      return;
    }
    let suffix = exchange.length;
    while (suffix > assistant + 1 && exchange[suffix - 1]?.kind === 'other') suffix--;
    if (suffix !== assistant + 1) {
      ordered.push(...exchange);
      return;
    }
    ordered.push(...exchange.slice(0, assistant), ...exchange.slice(suffix), exchange[assistant]);
  };
  let start = 0;
  for (let i = 1; i < source.length; i++) {
    if (source[i]?.kind !== 'user') continue;
    appendExchange(source.slice(start, i));
    start = i;
  }
  appendExchange(source.slice(start));
  return ordered;
}

export class SessionEventStore {
  constructor({ maxPending = 500 } = {}) {
    this.events = [];
    this.lastSeq = 0;
    this.maxPending = maxPending;
    this.renderedAnchors = new Set();
    this.renderedPrompts = []; // exact prompt texts the composer sent, each consumed once
    this.ownedTurns = new Set(); // composer turns drawing live in this view
    this.overlay = null; // temporary rows drawn by the composer over the canonical tail
    this.turnState = null;
  }

  // reset drops everything for a new session selection. The store never
  // carries rows across sessions — a stale row in the wrong conversation is a
  // worse failure than an empty pane.
  reset(seed = []) {
    this.events = orderConversationEvents(seed);
    this.lastSeq = this.events.reduce((last, event) => Math.max(last, Number(event?.seq) || 0), 0);
    this.renderedAnchors.clear();
    this.renderedPrompts = [];
    this.ownedTurns.clear();
    this.overlay = null;
    this.turnState = null;
  }

  // markRenderedLive records that a turn was already painted from the live
  // composer lane. Called with whatever anchor that lane can supply; an empty
  // anchor is ignored rather than guessed at, because a wrong anchor would
  // silently delete somebody's turn.
  markRenderedLive(anchor) {
    if (anchor) this.renderedAnchors.add(String(anchor));
  }

  // markLiveRow keeps the provider-neutral row the composer painted. Its
  // newest exact record identity becomes the catch-up barrier once every
  // overlapping owned turn has ended. Process completion alone is not proof
  // that the native transcript has reached this row.
  markLiveRow(row) {
    if (!this.overlay || !row || typeof row !== 'object') return NOOP;
    const copy = { ...row };
    this.overlay.live.push(copy);
    if (this.overlay.live.length > this.maxPending) {
      this.overlay.live = this.overlay.live.slice(-this.maxPending);
      this.overlay.overflowed = true;
    }
    if (copy.turn_anchor) {
      const anchor = String(copy.turn_anchor);
      this.renderedAnchors.add(anchor);
      this.overlay.barrierAnchor = anchor;
      this.overlay.barrierRow = copy;
    }
    return PENDING;
  }

  // markPromptRenderedLive records a prompt the composer drew. The vendor's
  // stream carries no identity for the prompt record, so this is the ONE
  // text-keyed match in the store: exact equality, consumed by the first
  // harvested user event that matches, never a similarity guess.
  markPromptRenderedLive(text) {
    if (typeof text === 'string' && text.length) this.renderedPrompts.push(text);
  }

  // beginOwnedTurn records that the composer in this view is drawing a turn
  // live. A vendor can store a record before its stream announces it, so
  // until the turn ends the harvested copies are held rather than drawn.
  beginOwnedTurn(id) {
    if (!id) return NOOP;
    const key = String(id);
    if (this.ownedTurns.has(key)) return NOOP;
    if (!this.ownedTurns.size && !this.overlay) {
      this.overlay = {
        boundary: this.events.length,
        boundarySeq: this.lastSeq,
        live: [],
        native: [],
        barrierAnchor: '',
        barrierRow: null,
        ended: false,
        overflowed: false,
        refreshRequested: false,
      };
    }
    if (this.overlay) this.overlay.ended = false;
    this.ownedTurns.add(key);
    return { type: 'boundary', boundary: this.overlay?.boundary ?? this.events.length, events: [] };
  }

  // endOwnedTurn closes process ownership only. Native rows remain pending
  // until the newest exact live record identity arrives from the transcript.
  endOwnedTurn(id) {
    const key = String(id || '');
    if (!this.ownedTurns.has(key)) return NOOP;
    this.ownedTurns.delete(key);
    if (this.ownedTurns.size || !this.overlay) return PENDING;
    this.overlay.ended = true;
    return this.#reconcile();
  }

  // #boundPending keeps the pending native tail within maxPending, and asks for
  // one refresh the first time rows had to be dropped.
  #boundPending() {
    if (this.overlay.native.length <= this.maxPending) return PENDING;
    this.overlay.native = this.overlay.native.slice(-this.maxPending);
    this.overlay.overflowed = true;
    if (this.overlay.refreshRequested) return PENDING;
    this.overlay.refreshRequested = true;
    return { type: 'refresh', events: [] };
  }

  // accept returns a view effect. Outside an owned turn, native rows append as
  // before. During and after an owned turn they remain pending until the exact
  // final live anchor proves the native transcript has caught up, at which
  // point the complete native span replaces only the temporary tail.
  accept(incoming) {
    const fresh = [];
    for (const event of Array.isArray(incoming) ? incoming : []) {
      const seq = Number(event?.seq) || 0;
      if (this.overlay && seq && seq > this.overlay.boundarySeq) {
        const at = this.overlay.native.findIndex(row => (Number(row?.seq) || 0) === seq);
        if (at >= 0) {
          this.overlay.native[at] = event;
          continue;
        }
      }
      if (seq && seq <= this.lastSeq) continue;
      if (seq > this.lastSeq) this.lastSeq = seq;
      fresh.push(event);
    }
    if (!fresh.length) return this.overlay ? this.#reconcile() : NOOP;
    if (this.overlay) {
      this.overlay.native.push(...fresh);
      const effect = this.#reconcile();
      if (effect.type !== 'pending') return effect;
      return this.#boundPending();
    }

    const append = fresh.filter(event => !this.#alreadyDrawn(event));
    if (!append.length) return NOOP;
    const before = this.events;
    const ordered = orderConversationEvents(before.concat(append));
    let boundary = 0;
    while (boundary < before.length && ordered[boundary] === before[boundary]) boundary++;
    this.events = ordered;
    if (boundary === before.length) return { type: 'append', events: ordered.slice(boundary) };
    return { type: 'replace-tail', boundary, events: ordered.slice(boundary) };
  }

  // A bounded recovery snapshot may replace pending deltas only when it
  // contains the same exact barrier. Otherwise the temporary live answer stays
  // visible and the uncertain native tail remains uncommitted.
  acceptSnapshot(snapshot) {
    if (!this.overlay || !Array.isArray(snapshot)) return NOOP;
    const overlay = this.overlay;
    const nativeTail = snapshot.filter(event => (Number(event?.seq) || 0) > overlay.boundarySeq);
    const barrier = overlay.ended && this.#hasBarrier(nativeTail, overlay);
    if (!barrier) return PENDING;
    this.events = orderConversationEvents(snapshot);
    this.lastSeq = snapshot.reduce((last, event) => Math.max(last, Number(event?.seq) || 0), 0);
    this.overlay = null;
    this.renderedPrompts = [];
    return { type: 'replace-tail', boundary: overlay.boundary, events: this.events.slice(overlay.boundary) };
  }

  #alreadyDrawn(event) {
    const anchor = event?.turn_anchor;
    if (anchor && this.renderedAnchors.has(String(anchor))) return true;
    if (event?.kind === 'user' && this.renderedPrompts.length) {
      const at = this.renderedPrompts.indexOf(String(event.text || ''));
      if (at >= 0) { this.renderedPrompts.splice(at, 1); return true; }
    }
    return false;
  }

  #reconcile() {
    const overlay = this.overlay;
    if (!overlay || !overlay.ended || !overlay.barrierAnchor || overlay.overflowed) return PENDING;
    if (!this.#hasBarrier(overlay.native, overlay)) return PENDING;
    const canonical = [...overlay.native].sort((a, b) => (Number(a?.seq) || 0) - (Number(b?.seq) || 0));
    const ordered = orderConversationEvents(canonical);
    this.events = this.events.slice(0, overlay.boundary).concat(ordered);
    this.overlay = null;
    this.renderedPrompts = [];
    return { type: 'replace-tail', boundary: overlay.boundary, events: ordered };
  }

  #hasBarrier(events, overlay) {
    const required = (overlay?.live || []).filter(row => row?.turn_anchor);
    if (!required.length || !overlay.barrierAnchor) return false;
    const matched = required.every(live => events.some(event => {
      if (String(event?.turn_anchor || '') !== String(live.turn_anchor)) return false;
      if (String(event?.kind || '') !== String(live.kind || '')) return false;
      return true;
    }));
    if (!matched) return false;
    const frontier = overlay.barrierRow;
    if (frontier?.kind !== 'assistant' || !frontier.text) return true;
    return events.some(event => String(event?.turn_anchor || '') === overlay.barrierAnchor
      && event?.kind === 'assistant' && String(event?.text || '') === frontier.text);
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
export { orderConversationEvents };

// Done 2026-09-02: the composer reports each block it drew by the vendor's
// record identity (the daemon forwards it as `anchor` on the owned stream),
// and each prompt it sent by exact text; the harvested copies are skipped
// here. Vendors whose owned stream carries no record identity still draw
// their composer turns twice, and that is stated, not hidden.
