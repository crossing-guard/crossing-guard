// Session status, browser side — a RENDERER, not a decider.
//
// The daemon computes what a session is doing and whether it needs the reader
// (internal/daemon/session_status.go) and publishes one frame per session on
// the activity item: execution, attention, attention_id, attention_source,
// since_ms. This module owns exactly two things the daemon cannot: the
// acknowledgement ledger ("has THIS reader looked", per viewer, cross-tab) and
// the mapping from a frame to the words and the dot shape a person sees.
//
// It never ranks facts, never joins tasks to approvals, never infers a state.
// If a status looks wrong, the fix is in Go; this file only draws.

const STORAGE_VERSION = 2;
const ENTRY_LIMIT = 512;
export const SESSION_ATTENTION_STORAGE_KEY = 'cg-session-attention-v2';
const ACTIVE = new Set(['queued', 'starting', 'running']);
const UNREAD = new Set(['new_result', 'new_failure', 'interrupted']);
// Three id spaces: task event ids, turn row ids, and agent-ask ids (the
// daemon's ask_id). Adding a space needs no version bump: the parse keeps
// what it knows and every merge walks this list.
const SOURCES = ['task', 'turn', 'agent'];

export function exactSessionKey(runtime, catalogID, _nativeID = '') {
  const provider = String(runtime || '').trim();
  const catalog = String(catalogID || '').trim();
  if (!provider || !catalog) return '';
  return provider + '\u0000' + catalog;
}

export function humanDuration(seconds) {
  const value = Math.max(0, Math.round(Number(seconds) || 0));
  if (value < 60) return value + 's';
  if (value < 3600) return Math.round(value / 60) + 'm';
  if (value < 86400) return Math.round(value / 3600) + 'h';
  return Math.round(value / 86400) + 'd';
}

function ageSeconds(item, now) {
  const since = Number(item?.since_ms) || 0;
  if (!since) return null;
  return Math.max(0, Math.round((now - since) / 1000));
}

// statusLabel is the ONE mapping from a frame to the words a person reads. The
// rail's dot title and the pane's header both call it, so they cannot disagree.
// progressWords is the ONE place a turn's progress becomes words. The pane
// header reads it through statusLabel from the daemon's frame; the composer's
// activity row reads it from its own live events. Same words either way.
export function progressWords(progress, tool) {
  if (progress === 'starting') return 'starting…';
  if (progress === 'thinking') return 'thinking…';
  if (progress === 'writing') return 'writing…';
  if (progress === 'tool') return 'running ' + (tool || 'a tool');
  return '';
}

// The activity line's own words (session-view plan §A6): what it says when the
// session's updates stop, and the agents link. Closed list, like the rest.
export const NOT_UPDATING = 'not updating';
export function agentsLinkWords({ review = 0, running = 0, watching = 0 } = {}) {
  if (review) return review + ' to review ›';
  if (running) return running + ' running ›';
  if (watching) return watching + ' watching ›';
  return '';
}

// askStatus is the frame's owner-attention beside the ladder: the newest
// unresolved agent ask this reader has not acknowledged, and the drafts —
// proposed replies that were not sent. Plain text only; renderers set it as
// text, never as markup.
export function askStatus(item, ledger) {
  const id = Number(item?.ask_id) || 0;
  const count = Number(item?.ask_count) || 0;
  const unseen = id > 0 && count > 0
    && !(ledger && id <= ledger.cursor(item?.runtime || '', item?.catalog_session_id || '', item?.native_session_id || '', 'agent'));
  return {
    unseen, id, count,
    text: String(item?.ask_text || ''),
    agent: String(item?.ask_agent || ''),
    drafts: Number(item?.draft_count) || 0,
    draftText: String(item?.draft_text || ''),
    unknown: item?.ask_state === 'unknown',
  };
}

// The words an ask and a draft take, on every surface (closed list).
export const ASKS_YOU = 'asks you';
export const REPLY_NOT_SENT = 'reply not sent';
// askLine is the one phrasing of an ask on every surface — rail title, card,
// header: the helper's own line, attributed to the agent by name.
export function askLine(ask) {
  return askText(ask) ? ASKS_YOU + ': ' + askText(ask) : '';
}

// askText is the line with its agent, without the lead words.
function askText(ask) {
  if (!ask?.text) return '';
  return ask.agent ? ask.text + ' (' + ask.agent + ')' : ask.text;
}

// agentNote is the quiet line under the status for an agent's words that are
// not the unseen ask: an ask the reader acknowledged but has not answered
// (it stays until the owner moves in the session), else an unsent reply. An
// unreadable ask source draws nothing here: the failure is reported once in
// the agents' own diagnostics, never as words on every session.
export function agentNote(ask) {
  if (!ask) return '';
  if (ask.count && !ask.unseen) return askLine(ask);
  if (ask.drafts) return REPLY_NOT_SENT + (ask.draftText ? ': ' + ask.draftText : '');
  return '';
}

export function statusLabel(item, now = Date.now(), seen = false) {
  const execution = item?.execution || 'unknown';
  const attention = item?.attention || 'none';
  const age = ageSeconds(item, now);
  const ago = age === null ? '' : 'last activity ' + humanDuration(age) + ' ago';
  if (attention === 'approval') {
    return { text: item?.attention_source === 'turn' ? 'needs your input' : 'approval required', age: ago, tone: 'ask' };
  }
  if (ACTIVE.has(execution)) {
    return { text: progressWords(item?.progress, item?.progress_tool) || 'working', age: ago, tone: 'active' };
  }
  if (execution === 'waiting') return { text: 'waiting for you', age: ago, tone: seen ? 'quiet' : 'ready' };
  if (execution === 'terminal') {
    if (attention === 'new_failure') return { text: 'failed', age: ago, tone: seen ? 'quiet' : 'bad' };
    if (attention === 'interrupted') return { text: 'stopped', age: ago, tone: 'quiet' };
    return { text: 'finished', age: ago, tone: seen || attention !== 'new_result' ? 'quiet' : 'ready' };
  }
  if (execution === 'idle') return { text: 'idle', age: ago, tone: 'quiet' };
  if (age !== null) return { text: 'no update for ' + humanDuration(age), age: '', tone: 'quiet' };
  return { text: 'unknown', age: '', tone: 'quiet' };
}
// needsReader says whether a rendered status is waiting on the reader: an
// approval, an agent's ask, a failure, an interruption or a result not yet
// seen; a session at rest after its turn ended; or an ask the reader saw and
// has not answered, even while the session runs. It reads the rendered status
// alone, so a board and a dot can never disagree about the frame behind them.
const NEEDS_READER = new Set(['approval', 'ask', 'failed', 'interrupted', 'new_result']);
export function needsReader(status) {
  if (!status) return false;
  if (NEEDS_READER.has(status.indicator?.kind) || status.execution === 'waiting') return true;
  return Boolean(status.ask && status.ask.count > 0 && !status.ask.unseen);
}

// renderSessionStatus turns one frame plus the reader's ledger into what the
// rail and header draw. `item` may be null (the daemon has said nothing about
// this session), which renders as unknown — never as anything stronger.
export function renderSessionStatus(item, ledger, now = Date.now()) {
  const runtime = item?.runtime || '';
  const catalogID = item?.catalog_session_id || '';
  const nativeID = item?.native_session_id || '';
  const execution = item?.execution || 'unknown';
  const presence = item?.presence || 'unknown';
  const freshness = item?.freshness || 'unknown';
  const attention = item?.attention || 'none';
  const source = item?.attention_source || '';
  const id = Number(item?.attention_id) || 0;
  const seen = UNREAD.has(attention) && id > 0 && ledger
    ? id <= ledger.cursor(runtime, catalogID, nativeID, source) : false;
  let words = statusLabel(item, now, seen);
  const ask = askStatus(item, ledger);
  // Precedence (plan §6.2): an approval or an input request, then an unseen
  // ask, then the ladder's own markers. An acknowledged ask shows the marker
  // beneath it again.
  const askShows = ask.unseen && attention !== 'approval';
  if (askShows) words = { text: ASKS_YOU, age: words.age, tone: 'ask' };
  let kind = 'none';
  if (attention === 'approval') kind = 'approval';
  else if (askShows) kind = 'ask';
  else if (ACTIVE.has(execution)) kind = 'running';
  else if (attention === 'new_failure' && !seen) kind = 'failed';
  else if (attention === 'interrupted' && !seen) kind = 'interrupted';
  else if (attention === 'new_result' && !seen) kind = 'new_result';
  else if (execution === 'unknown' && presence === 'open') kind = freshness === 'stale' ? 'native_stale' : 'native_open';
  const label = words.text.charAt(0).toUpperCase() + words.text.slice(1);
  // The explanation is about the reader's work, in the reader's words. The
  // daemon's own item.detail is NOT rendered: it describes how we know, and
  // that is our bookkeeping.
  const detail = kind === 'ask' ? askText(ask) : explain(kind, execution, presence, words.age);
  return {
    execution, presence, attention, seen, ask,
    attention_id: id, attention_source: source,
    indicator: { kind, label: kind === 'none' ? '' : label, detail },
    label, detail, age: words.age, tone: words.tone,
  };
}

// explain is the one-line reason under the status — plain speech, no
// mechanism. Empty when the label already says everything.
function explain(kind, execution, presence, age) {
  switch (kind) {
    case 'running': return age || '';
    case 'new_result': return execution === 'waiting' ? 'The reply is ready to read.' : 'New output is ready to read.';
    case 'approval': return 'It is waiting on your answer.';
    case 'failed': return 'The last turn ended in an error.';
    case 'interrupted': return 'The last turn was stopped.';
    case 'native_open': return 'Open, but whether a reply is being written is unknown.';
    case 'native_stale': return 'It was open a while ago; nothing has been seen since.';
    default: return presence === 'open' ? 'Open; nothing new has happened.' : '';
  }
}

function emptyState() { return { version: STORAGE_VERSION, baselined: false, entries: {} }; }

function validState(value) {
  if (!value || value.version !== STORAGE_VERSION || typeof value.baselined !== 'boolean'
    || !value.entries || typeof value.entries !== 'object' || Array.isArray(value.entries)) return null;
  const entries = {};
  for (const [key, entry] of Object.entries(value.entries)) {
    if (!key || !entry || typeof entry !== 'object') continue;
    const clean = { touched: Number.isFinite(Number(entry.touched)) ? Number(entry.touched) : 0 };
    for (const source of SOURCES) {
      const cursor = Number(entry[source]);
      clean[source] = Number.isSafeInteger(cursor) && cursor >= 0 ? cursor : 0;
    }
    entries[key] = clean;
  }
  return { version: STORAGE_VERSION, baselined: value.baselined, entries };
}

function emptyEntry() {
  const entry = { touched: 0 };
  for (const source of SOURCES) entry[source] = 0;
  return entry;
}

function mergeAttentionState(left, right) {
  const merged = emptyState();
  merged.baselined = Boolean(left?.baselined || right?.baselined);
  for (const state of [left, right]) {
    for (const [key, entry] of Object.entries(state?.entries || {})) {
      const previous = merged.entries[key] || emptyEntry();
      const next = { touched: Math.max(previous.touched, entry.touched || 0) };
      for (const source of SOURCES) next[source] = Math.max(previous[source] || 0, entry[source] || 0);
      merged.entries[key] = next;
    }
  }
  return merged;
}

// SessionAttentionStore is the reader's ledger: what this person has looked
// at, per session, kept in the browser and synced across tabs. It holds TWO
// cursors per session because the daemon's attention ids come from two
// unrelated integer sequences (task event ids and turn row ids) — comparing
// across them was the defect that made blue never clear, or never appear.
export class SessionAttentionStore {
  constructor({ storage = globalThis.localStorage, eventTarget = globalThis.window,
    key = SESSION_ATTENTION_STORAGE_KEY, now = () => Date.now() } = {}) {
    this.storage = storage; this.eventTarget = eventTarget; this.key = key; this.now = now;
    this.listeners = new Set(); this.state = this.read();
    this.boundStorage = event => {
      if (event?.key !== this.key) return;
      this.state = this.read(); this.emit();
    };
    this.eventTarget?.addEventListener?.('storage', this.boundStorage);
  }
  read() {
    try { return validState(JSON.parse(this.storage?.getItem?.(this.key) || 'null')) || emptyState(); }
    catch { return emptyState(); }
  }
  persist() {
    try { this.storage?.setItem?.(this.key, JSON.stringify(this.state)); } catch { /* UI preference only */ }
  }
  bound() {
    const ordered = Object.entries(this.state.entries)
      .sort((left, right) => right[1].touched - left[1].touched);
    this.state.entries = Object.fromEntries(ordered.slice(0, ENTRY_LIMIT));
  }
  // establishBaseline marks everything that finished BEFORE this reader
  // first looked as already read — for the TASK space only. Turn rows that
  // pre-date the feature are never called unread, because there is no
  // baseline to compare them to; they simply carry no cursor until acknowledged.
  // Agent asks inside the daemon's ask horizon start unseen: an ask is a
  // question still open, and owner motion resolves it daemon-side.
  establishBaseline(tasks, throughEventID) {
    if (this.state.baselined || !Number.isSafeInteger(throughEventID) || throughEventID <= 0) return false;
    const touched = this.now();
    for (const task of tasks || []) {
      if (ACTIVE.has(task.lifecycle) || !task.catalog_session_id) continue;
      const key = exactSessionKey(task.session_runtime, task.catalog_session_id, task.native_session_id);
      const cursor = Number(task.last_event_id) || 0;
      if (!key || cursor <= 0) continue;
      const previous = this.state.entries[key] || emptyEntry();
      this.state.entries[key] = { ...previous, task: Math.max(previous.task, cursor), touched };
    }
    this.state.baselined = true; this.bound(); this.persist(); this.emit(); return true;
  }
  cursor(runtime, catalogID, nativeID = '', source = 'task') {
    if (!SOURCES.includes(source)) return 0;
    return this.state.entries[exactSessionKey(runtime, catalogID, nativeID)]?.[source] || 0;
  }
  acknowledge(runtime, catalogID, nativeID, source, id) {
    const key = exactSessionKey(runtime, catalogID, nativeID); const cursor = Number(id);
    if (!key || !SOURCES.includes(source) || !Number.isSafeInteger(cursor) || cursor <= 0) return false;
    this.state = mergeAttentionState(this.state, this.read());
    if (cursor <= this.cursor(runtime, catalogID, nativeID, source)) return false;
    const previous = this.state.entries[key] || emptyEntry();
    this.state.entries[key] = { ...previous, [source]: cursor, touched: this.now() };
    this.bound(); this.persist(); this.emit(); return true;
  }
  subscribe(listener) {
    this.listeners.add(listener);
    try { listener(this.state); } catch (error) { console.error('Session attention listener failed', error); }
    return () => this.listeners.delete(listener);
  }
  emit() {
    for (const listener of this.listeners) {
      try { listener(this.state); } catch (error) { console.error('Session attention listener failed', error); }
    }
  }
  destroy() { this.eventTarget?.removeEventListener?.('storage', this.boundStorage); this.listeners.clear(); }
}
