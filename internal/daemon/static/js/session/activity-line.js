// The activity line: ONE readout of what a session is doing, pinned directly
// above the reply area (session-view-and-console-preferences plan §A2).
//
// It draws from four inputs and decides between them in a fixed order:
//   1. attention — an approval or input request (from the live frame, or from
//      the rail item when live is unavailable) always wins, even mid-turn;
//      an agent's unseen ask comes next (escalation-delivery plan §6.2);
//   2. lane      — the composer's own words while a console-owned turn streams;
//   3. live      — the open session's status frame, once one has arrived;
//   4. rail      — the rail's item for this session, before the first live
//      frame and while live is unavailable. It is also the only source of
//      presence and freshness (open / open a while ago), which the live frame
//      does not refresh.
// Every word comes from task/session-status.js. Nothing here narrates how the
// console knows: live loss adds "· not updating" and never replaces the words.

import { el } from '../core.js';
import { renderSessionStatus, progressWords, NOT_UPDATING, askLine, agentNote } from '../task/session-status.js';

const PRESENCE_KINDS = new Set(['native_open', 'native_stale']);

// decideActivity is pure over its inputs. `lane` is null or
// { progress, tool, startedAt }; `live` and `rail` are daemon status items.
export function decideActivity({ lane = null, live = null, liveUnavailable = false, rail = null, following = true }, ledger, now = Date.now()) {
  const railStatus = rail ? renderSessionStatus(rail, ledger, now) : null;
  if (!following) {
    // An approval the rail still knows about outranks "not updating".
    if (railStatus?.attention === 'approval') return { kind: 'approval', text: lower(railStatus.label), age: '· ' + NOT_UPDATING, source: railStatus };
    return { kind: 'none', text: NOT_UPDATING, age: '', source: null };
  }
  const liveStatus = live && !liveUnavailable ? renderSessionStatus(live, ledger, now) : null;
  let status = liveStatus || railStatus;
  if (liveStatus && liveStatus.indicator.kind === 'none' && railStatus && PRESENCE_KINDS.has(railStatus.indicator.kind)) {
    status = railStatus;
  }
  const suffix = liveUnavailable ? '· ' + NOT_UPDATING : '';
  if (status?.attention === 'approval') {
    return { kind: 'approval', text: lower(status.label), age: join(status.age, suffix), source: status };
  }
  // An agent's unseen ask outranks everything but an approval (plan §6.2):
  // the helper's own line, attributed, as plain text.
  if (status?.indicator.kind === 'ask') {
    return { kind: 'ask', text: askLine(status.ask), age: suffix, source: status };
  }
  if (lane) {
    const words = progressWords(lane.progress, lane.tool) || 'working';
    const elapsed = lane.startedAt ? Math.max(0, Math.round((now - lane.startedAt) / 1000)) + 's' : '';
    return { kind: 'running', text: words, age: join(elapsed, suffix), source: status };
  }
  if (!status) return { kind: 'none', text: '', age: suffix, source: null };
  return { kind: status.indicator.kind, text: lower(status.label), age: join(status.age, suffix), source: status, draft: draftLine(status) };
}

// draftLine is the quiet note beside the status: an acknowledged ask, an
// unsent reply, or unreadable agent lines. No dot; the agent's text as text.
function draftLine(status) {
  return agentNote(status?.ask);
}

function lower(label) {
  const text = String(label || '');
  return text.charAt(0).toLowerCase() + text.slice(1);
}

function join(age, suffix) {
  return [age, suffix].filter(Boolean).join(' ');
}

// createActivityLine owns one line's DOM. The owner feeds it inputs; it
// re-renders on each, and on the age tick. `linkHost` is where the owner may
// mount a trailing link (the agents link); the line never fills it itself.
export function createActivityLine({ ledger = null, ageTickSeconds = 15 } = {}) {
  const root = el('div', 'activity-line');
  root.setAttribute('role', 'status');
  root.setAttribute('aria-live', 'polite');
  const dot = el('span', 'session-status-dot');
  dot.setAttribute('aria-hidden', 'true');
  const words = el('span', 'activity-words');
  const age = el('span', 'activity-age');
  const draft = el('span', 'activity-draft');
  const linkHost = el('span', 'activity-link');
  root.append(dot, words, age, draft, linkHost);
  const inputs = { lane: null, live: null, liveUnavailable: false, rail: null, following: true };
  const listeners = new Set();
  let current = decideActivity(inputs, ledger);
  let timer = null;
  let disposed = false;

  function render() {
    current = decideActivity(inputs, ledger);
    dot.className = 'session-status-dot ' + current.kind;
    dot.hidden = current.kind === 'none';
    words.textContent = current.text;
    age.textContent = current.age;
    draft.textContent = current.draft || '';
    draft.hidden = !current.draft;
    root.dataset.kind = current.kind;
    for (const listener of listeners) listener(current);
    return current;
  }
  function tick() {
    if (!root.isConnected && timer) { clearInterval(timer); timer = null; return; }
    render();
  }
  function schedule(seconds) {
    if (timer) clearInterval(timer);
    timer = null;
    if (disposed) return;
    timer = setInterval(tick, Math.max(1, seconds) * 1000);
  }
  schedule(ageTickSeconds);
  render();

  return {
    root,
    linkHost,
    // A console-owned turn's progress: { progress, tool } or 'starting'; null ends it.
    setLane(next) {
      if (!next) { inputs.lane = null; schedule(ageTickSeconds); return render(); }
      const startedAt = inputs.lane?.startedAt || Date.now();
      inputs.lane = next === 'starting' ? { progress: 'starting', startedAt } : { ...next, startedAt };
      schedule(1); // elapsed seconds count while a turn streams
      return render();
    },
    setLive(frame) { inputs.live = frame || null; return render(); },
    setLiveUnavailable(unavailable) { inputs.liveUnavailable = !!unavailable; return render(); },
    setRail(item) { inputs.rail = item || null; return render(); },
    setFollowing(following) { inputs.following = following !== false; return render(); },
    setAgeTick(seconds) { if (!inputs.lane) schedule(seconds); },
    // The status the line is showing now, for acknowledging exactly what the
    // reader saw.
    current() { return current; },
    onChange(listener) { listeners.add(listener); return () => listeners.delete(listener); },
    dispose() { disposed = true; if (timer) clearInterval(timer); timer = null; listeners.clear(); },
  };
}
