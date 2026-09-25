import { reportApprovalPresence } from './approval-api.js';

// A presence report is sent only when visibility or focus CHANGES. The tab's
// open event stream is its lease (the daemon attaches the same client id), so
// there is no heartbeat: a periodic POST held one of the browser's six
// per-origin connections every tick and starved the console under load.
// A send still must never outlive a reasonable wait, so a stuck daemon cannot
// hold a connection either.
const SEND_TIMEOUT_MS = 4000;

function newClientID() {
  if (globalThis.crypto?.randomUUID) return 'tab_' + globalThis.crypto.randomUUID();
  return 'tab_' + Date.now().toString(36) + '_' + Math.random().toString(36).slice(2);
}

export class ApprovalAttentionClient {
  constructor(report = reportApprovalPresence) {
    this.report = report; this.clientID = newClientID(); this.started = false; this.inFlight = false;
    this.lastSent = null; // the state the daemon last acknowledged
    this.boundSend = () => { void this.send(); };
  }
  start() {
    if (this.started) return;
    this.started = true;
    document.addEventListener('visibilitychange', this.boundSend);
    window.addEventListener('focus', this.boundSend);
    window.addEventListener('blur', this.boundSend);
    this.boundSend();
  }
  stop() {
    if (!this.started) return;
    this.started = false;
    document.removeEventListener('visibilitychange', this.boundSend);
    window.removeEventListener('focus', this.boundSend);
    window.removeEventListener('blur', this.boundSend);
  }
  current() {
    const visible = document.visibilityState === 'visible';
    return { visible, focused: visible && document.hasFocus() };
  }
  async send() {
    if (this.inFlight) return; // a send is outstanding; the next change is the retry
    const state = this.current();
    if (this.lastSent && this.lastSent.visible === state.visible && this.lastSent.focused === state.focused) return;
    this.inFlight = true;
    const abort = new AbortController();
    const expiry = setTimeout(() => abort.abort(), SEND_TIMEOUT_MS);
    try {
      await this.report({ client_id: this.clientID, ...state }, abort.signal);
      this.lastSent = state;
    } catch { /* not acknowledged: the next change resends; the stream lease still holds */
    } finally {
      clearTimeout(expiry);
      this.inFlight = false;
    }
  }
}
