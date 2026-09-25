// SessionLiveClient — the open session's live view (natural-session plan,
// B-GUI). Transcript deltas append through the existing transcript renderer;
// governance deltas feed the strip/action refresh callbacks; the turn state
// and identity frames are rendered, never inferred. The honest-latency
// contract (H5) holds: updates arrive at bounded scan cadence, and an
// unavailable feed is surfaced, not hidden behind a frozen view.
//
// Transport: the tab's ONE multiplexed event stream (event-stream-client.js).
// This module owns which session is followed and how its frames are handled;
// it opens no connection of its own — that is what kept the browser under its
// six-socket cap.

let transport = null;

// attachLiveTransport gives this module the tab's event-stream client. app.js
// calls it once at boot; views never touch the transport directly.
export function attachLiveTransport(client) { transport = client; }

export class SessionLiveClient {
  constructor() {
    this.generation = 0;
    this.lastSeq = 0;
    this.onEvents = null;   // (events[]) => void — new canonical events
    this.onGovernance = null; // (actions[]) => void — new governed actions
    this.onStatus = null;  // (status: 'live' | 'unavailable', detail?) => void
    this.onTurnState = null;
    this.onIdentity = null;
  }

  subscribe(runtime, id, handlers = {}) {
    this.generation++;
    this.lastSeq = 0;
    this.onEvents = handlers.onEvents || null;
    this.onGovernance = handlers.onGovernance || null;
    this.onStatus = handlers.onStatus || null;
    this.onTurnState = handlers.onTurnState || null;
    this.onIdentity = handlers.onIdentity || null;
    if (!runtime || !id) { transport?.setSubject('', '', null); return; }
    transport?.setSubject(runtime, id, this);
  }

  // handle receives one frame of the session feed from the transport.
  handle(event, payload) {
    if (event === 'snapshot') {
      const events = Array.isArray(payload?.events) ? payload.events : [];
      if (events.length) this.lastSeq = events[events.length - 1].seq || 0;
      if (this.onStatus) this.onStatus('live', payload);
      return;
    }
    if (event === 'events') {
      const events = Array.isArray(payload?.events) ? payload.events : [];
      if (events.length) this.lastSeq = events[events.length - 1].seq || this.lastSeq;
      if (this.onEvents) this.onEvents(events);
      return;
    }
    if (event === 'governance') {
      if (this.onGovernance) this.onGovernance(Array.isArray(payload?.actions) ? payload.actions : []);
      return;
    }
    if (event === 'state') {
      // Whether the model is working or waiting is decided by the daemon; the
      // browser only renders the answer.
      if (this.onTurnState) this.onTurnState(payload);
      return;
    }
    if (event === 'identity') {
      // The session we were asked to follow is no longer resolvable. This is
      // NOT an ending — we simply cannot see it any more, and saying "ended"
      // would claim a completion nobody observed.
      if (this.onIdentity) this.onIdentity(payload);
      return;
    }
    if (event === 'unavailable') {
      if (this.onStatus) this.onStatus('unavailable', { snapshotAt: Date.now(), error: payload?.error });
    }
  }

  // drop is called by the transport when the connection is lost; the loop
  // retries, and the view says so instead of freezing.
  drop() {
    if (this.onStatus) this.onStatus('unavailable', { retrying: true });
  }

  close() {
    this.generation++;
    this.lastSeq = 0;
    this.onEvents = null;
    this.onGovernance = null;
    this.onStatus = null;
    this.onTurnState = null;
    this.onIdentity = null;
    transport?.setSubject('', '', null);
  }
}
