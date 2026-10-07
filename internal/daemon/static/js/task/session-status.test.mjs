import { needsReader, renderSessionStatus, statusLabel, progressWords, exactSessionKey, SessionAttentionStore, agentsLinkWords, NOT_UPDATING, ASKS_YOU, REPLY_NOT_SENT } from './session-status.js';
import { readFileSync } from 'node:fs';

let failures = 0;
const check = (name, condition) => {
  if (!condition) { failures++; console.error('FAIL', name); }
};

const NOW = 1_800_000_000_000;
const frame = (extra = {}) => ({
  runtime: 'codex', catalog_session_id: 'catalog', native_session_id: 'native',
  presence: 'unknown', execution: 'unknown', evidence: 'none', freshness: 'unknown', authority: 'none',
  attention: 'none', attention_id: 0, attention_source: '', since_ms: NOW - 30_000, ...extra,
});

class MemoryStorage {
  constructor(raw = null) { this.raw = raw; this.writes = 0; }
  getItem() { return this.raw; }
  setItem(_key, value) { this.raw = value; this.writes++; }
}
class MemoryEvents {
  constructor() { this.listeners = new Set(); }
  addEventListener(type, listener) { if (type === 'storage') this.listeners.add(listener); }
  removeEventListener(type, listener) { if (type === 'storage') this.listeners.delete(listener); }
  dispatch(event) { for (const listener of this.listeners) listener(event); }
}
const ledgerOf = (storage = new MemoryStorage(), events = new MemoryEvents()) =>
  new SessionAttentionStore({ storage, eventTarget: events, now: () => NOW });

check('session key uses exact catalog identity', exactSessionKey('codex', 'rollout', 'shared-thread') === 'codex' + '\u0000' + 'rollout');
check('missing catalog identity cannot create attention key', exactSessionKey('codex', '', 'native') === '');

// ---- the renderer draws what the daemon decided; it decides nothing ----
const ledger = ledgerOf();
const running = renderSessionStatus(frame({ execution: 'running', authority: 'owned' }), ledger, NOW);
check('running renders the ring and the word working', running.indicator.kind === 'running' && running.label === 'Working');

const handedBack = renderSessionStatus(frame({ execution: 'waiting', authority: 'observed',
  attention: 'new_result', attention_id: 37, attention_source: 'turn' }), ledger, NOW);
check('an unread hand-back is blue and says waiting for you',
  handedBack.indicator.kind === 'new_result' && handedBack.label === 'Waiting for you' && !handedBack.seen);

ledger.acknowledge('codex', 'catalog', 'native', 'turn', 37);
const seenHandBack = renderSessionStatus(frame({ execution: 'waiting', authority: 'observed',
  attention: 'new_result', attention_id: 37, attention_source: 'turn' }), ledger, NOW);
check('once seen the dot empties but the words stay true',
  seenHandBack.indicator.kind === 'none' && seenHandBack.label === 'Waiting for you' && seenHandBack.seen);

// ---- what is waiting on the reader (board-in-flight plan §2.2) ----
check('a running session does not need the reader', !needsReader(running));
check('an unread hand-back needs the reader', needsReader(handedBack));
check('a hand-back the reader saw and has not answered still does', needsReader(seenHandBack));
check('no status at all needs nobody', !needsReader(undefined) && !needsReader(renderSessionStatus(frame(), ledger, NOW)));

const vendorPrompt = renderSessionStatus(frame({ execution: 'waiting', attention: 'approval', attention_source: 'turn', attention_id: 40 }), ledger, NOW);
check('a vendor permission prompt is amber and asks for input',
  vendorPrompt.indicator.kind === 'approval' && vendorPrompt.label === 'Needs your input');
const ourApproval = renderSessionStatus(frame({ execution: 'running', attention: 'approval', attention_source: 'task' }), ledger, NOW);
check('our own approval is amber and outranks the ring',
  ourApproval.indicator.kind === 'approval' && ourApproval.label === 'Approval required');

check('an approval needs the reader, whoever asked', needsReader(vendorPrompt) && needsReader(ourApproval));

const ownedDone = renderSessionStatus(frame({ execution: 'terminal', authority: 'owned',
  attention: 'new_result', attention_id: 41000, attention_source: 'task' }), ledger, NOW);
check('an owned completion keeps its blue dot', ownedDone.indicator.kind === 'new_result' && ownedDone.label === 'Finished');
const ownedFailed = renderSessionStatus(frame({ execution: 'terminal', authority: 'owned',
  attention: 'new_failure', attention_id: 41001, attention_source: 'task' }), ledger, NOW);
check('an unread failure is the red diamond', ownedFailed.indicator.kind === 'failed' && ownedFailed.label === 'Failed');

const open = renderSessionStatus(frame({ presence: 'open', freshness: 'live', authority: 'observed', since_ms: 0 }), ledger, NOW);
check('open with nothing observed is the quiet green outline, never running',
  open.indicator.kind === 'native_open' && open.label === 'Unknown');
const stale = renderSessionStatus(frame({ presence: 'open', freshness: 'stale', authority: 'observed', since_ms: 0 }), ledger, NOW);
check('expired presence is dashed, distinct from open', stale.indicator.kind === 'native_stale');
const silence = renderSessionStatus(frame({ presence: 'open', freshness: 'live', authority: 'observed', since_ms: NOW - 7 * 60_000 }), ledger, NOW);
check('silence is reported as silence with its duration',
  silence.label === 'No update for 7m' && silence.indicator.kind === 'native_open');
const nothing = renderSessionStatus(null, ledger, NOW);
check('no frame at all is unknown with an empty dot', nothing.indicator.kind === 'none' && nothing.label === 'Unknown');
check('idle is quiet', renderSessionStatus(frame({ execution: 'idle', authority: 'observed' }), ledger, NOW).indicator.kind === 'none');
check('the daemon\'s own detail never reaches the screen',
  !renderSessionStatus(frame({ presence: 'open', freshness: 'live', authority: 'observed', since_ms: 0,
    detail: 'Lifecycle hooks saw this session start' }), ledger, NOW).detail.includes('hook'));

// ---- rail and header read one labeler ----
check('header words match the rail label', statusLabel(frame({ execution: 'waiting' }), NOW).text === 'waiting for you'
  && renderSessionStatus(frame({ execution: 'waiting' }), ledger, NOW).label === 'Waiting for you');
check('age is always available', statusLabel(frame({ execution: 'running' }), NOW).age === 'last activity 30s ago');

// ---- the ledger: two cursors, two spaces ----
const two = ledgerOf();
check('baseline seeds the task space only', two.establishBaseline([
  { lifecycle: 'completed', session_runtime: 'codex', catalog_session_id: 'catalog', native_session_id: 'native', last_event_id: 41000 },
], 41000) && two.cursor('codex', 'catalog', 'native', 'task') === 41000 && two.cursor('codex', 'catalog', 'native', 'turn') === 0);
check('baseline is established once', !two.establishBaseline([], 41001));
const turnAfterBaseline = renderSessionStatus(frame({ execution: 'waiting', attention: 'new_result', attention_id: 37, attention_source: 'turn' }), two, NOW);
check('a turn id far below the task cursor is still unread — different space', turnAfterBaseline.indicator.kind === 'new_result');
check('acknowledging a turn never moves the task cursor', two.acknowledge('codex', 'catalog', 'native', 'turn', 37)
  && two.cursor('codex', 'catalog', 'native', 'turn') === 37 && two.cursor('codex', 'catalog', 'native', 'task') === 41000);
check('a later task event is still unread after a huge turn id', (() => {
  two.acknowledge('codex', 'catalog', 'native', 'turn', 90_000);
  return renderSessionStatus(frame({ execution: 'terminal', attention: 'new_failure', attention_id: 41002, attention_source: 'task' }), two, NOW).indicator.kind === 'failed';
})());
check('an unknown source is refused', !two.acknowledge('codex', 'catalog', 'native', 'clock', 5) && two.cursor('codex', 'catalog', 'native', 'clock') === 0);
check('acknowledgement never moves backwards', !two.acknowledge('codex', 'catalog', 'native', 'turn', 10));

// ---- cross-tab, corruption, bounds ----
const shared = new MemoryStorage(); const bus = new MemoryEvents();
const tabA = ledgerOf(shared, bus); const tabB = ledgerOf(shared, bus);
tabA.acknowledge('codex', 'catalog', 'native', 'turn', 12);
bus.dispatch({ key: 'cg-session-attention-v2' });
check('a second tab sees the acknowledgement', tabB.cursor('codex', 'catalog', 'native', 'turn') === 12);
const corrupt = ledgerOf(new MemoryStorage('{bad'), new MemoryEvents());
check('corrupt persistence resets safely', corrupt.cursor('codex', 'catalog', 'native', 'turn') === 0 && corrupt.state.baselined === false);
const oldVersion = ledgerOf(new MemoryStorage(JSON.stringify({ version: 1, baselined: true, entries: { ['codex' + '\u0000' + 'catalog']: { cursor: 41000, touched: 1 } } })), new MemoryEvents());
check('a v1 ledger (one cursor) is not read as v2 — it re-baselines instead of comparing across spaces',
  oldVersion.state.baselined === false && oldVersion.cursor('codex', 'catalog', 'native', 'task') === 0);
const bounded = ledgerOf();
bounded.establishBaseline([], 1);
for (let index = 1; index <= 520; index++) bounded.acknowledge('codex', 'session-' + index, '', 'turn', index);
check('acknowledgement state is bounded', Object.keys(bounded.state.entries).length === 512);

// ---- show, don't tell ----
const source = readFileSync(new URL('./session-status.js', import.meta.url), 'utf8')
  .split('\n').filter(line => !line.trim().startsWith('//')).join('\n');
for (const word of ['projection', 'superseded', 'harvested', 'reconcil', 'overlay', 'anchor', 'derived', 'evidence class', 'lifecycle hook', 'vendor process', 'hook ']) {
  const inUserString = new RegExp("(['\"`])[^'\"`\\n]*" + word + "[^'\"`\\n]*\\1", 'i');
  check('no bookkeeping word "' + word + '" in a rendered string', !inUserString.test(source));
}
// Provider branches are checked across all browser code by
// scripts/vendor-lint-js.mjs (runtime-model-catalog-and-usage plan §2.5).

tabA.destroy(); tabB.destroy(); corrupt.destroy(); bounded.destroy(); two.destroy(); ledger.destroy(); oldVersion.destroy();

// Progress words: the ONE place a turn's progress becomes words, for every lane.
{
  const since = NOW - 5_000;
  check('progress: thinking', progressWords('thinking') === 'thinking…');
  check('progress: writing', progressWords('writing') === 'writing…');
  check('progress: tool named', progressWords('tool', 'Bash') === 'running Bash');
  check('progress: tool unnamed', progressWords('tool') === 'running a tool');
  check('progress: none is empty', progressWords('') === '');
  check('header: thinking', statusLabel(frame({ execution: 'running', progress: 'thinking', since_ms: since }), NOW).text === 'thinking…');
  check('header: running tool', statusLabel(frame({ execution: 'running', progress: 'tool', progress_tool: 'Grep', since_ms: since }), NOW).text === 'running Grep');
  check('header: plain working without progress', statusLabel(frame({ execution: 'running', since_ms: since }), NOW).text === 'working');
  check('header: an ask outranks progress', statusLabel(frame({ execution: 'running', progress: 'tool', progress_tool: 'Bash', attention: 'approval', attention_source: 'turn', since_ms: since }), NOW).text === 'needs your input');
  check('header: progress never leaks past running', statusLabel(frame({ execution: 'waiting', progress: 'writing', since_ms: since }), NOW).text === 'waiting for you');
}

// The activity line's own words (session-view plan §A6): closed list, no mechanism.
{
  check('lane start word', progressWords('starting') === 'starting…');
  check('agents link: review outranks running outranks watching',
    agentsLinkWords({ review: 1, running: 2, watching: 3 }) === '1 to review ›'
    && agentsLinkWords({ running: 2, watching: 3 }) === '2 running ›'
    && agentsLinkWords({ watching: 3 }) === '3 watching ›' && agentsLinkWords({}) === '');
  const words = [NOT_UPDATING, progressWords('starting'), agentsLinkWords({ review: 1 }), agentsLinkWords({ running: 1 }), agentsLinkWords({ watching: 1 })];
  check('activity-line words carry no mechanism', words.every(word => !/\b(feed|snapshot|stream|frame|resolvable|hook|harvest|lane|boundary|module|provider)\b/i.test(word)));
}

// Agent asks and drafts ride beside the ladder (escalation-delivery plan §6.2,
// §6.4): approval and input outrank an ask; an unseen ask outranks the
// markers; acknowledging it shows the marker beneath; the agent source joins
// the ledger with no version bump.
{
  const asking = frame({ execution: 'waiting', attention: 'new_result', attention_id: 9, attention_source: 'turn',
    ask_id: 41, ask_count: 2, ask_text: 'The owner needs to pick <b>A</b>', ask_agent: 'continue-helper' });
  const store = ledgerOf();
  const shown = renderSessionStatus(asking, store, NOW);
  check('an unseen ask says asks you and outranks the hand-back', shown.indicator.kind === 'ask' && shown.label === 'Asks you'
    && shown.indicator.detail === 'The owner needs to pick <b>A</b> (continue-helper)' && shown.ask.count === 2);
  check('approval outranks an ask', renderSessionStatus({ ...asking, attention: 'approval', attention_source: 'task' }, store, NOW).indicator.kind === 'approval');
  check('input request outranks an ask', renderSessionStatus({ ...asking, attention: 'approval', attention_source: 'turn' }, store, NOW).label === 'Needs your input');
  check('acknowledging the ask takes the agent space', store.acknowledge(asking.runtime, 'catalog', 'native', 'agent', 41) === true);
  const after = renderSessionStatus(asking, store, NOW);
  check('an acknowledged ask shows the marker beneath', after.indicator.kind === 'new_result' && after.ask.unseen === false);
  check('the ask id is its own space', store.cursor(asking.runtime, 'catalog', 'native', 'turn') === 0 && store.cursor(asking.runtime, 'catalog', 'native', 'agent') === 41);
  check('a newer ask shows again', renderSessionStatus({ ...asking, ask_id: 42 }, store, NOW).indicator.kind === 'ask');
  // A v2 ledger written before the agent source existed keeps its cursors.
  const legacy = new MemoryStorage(JSON.stringify({ version: 2, baselined: true, entries: {
    [exactSessionKey(asking.runtime, 'catalog')]: { task: 5, turn: 7, touched: 1 } } }));
  const upgraded = ledgerOf(legacy);
  check('a ledger without the agent space loads without a reset', upgraded.cursor(asking.runtime, 'catalog', 'native', 'turn') === 7
    && upgraded.cursor(asking.runtime, 'catalog', 'native', 'agent') === 0);
  const drafted = renderSessionStatus(frame({ draft_count: 1, draft_text: 'Go ahead.' }), store, NOW);
  check('a draft raises no dot', drafted.indicator.kind === 'none' && drafted.ask.drafts === 1 && drafted.ask.draftText === 'Go ahead.');
  check('an unknown ask source is carried, never drawn as an ask', renderSessionStatus(frame({ ask_state: 'unknown' }), store, NOW).ask.unknown === true);
  check('a draft alone does not need the reader', !needsReader(drafted));
  const unanswered = renderSessionStatus(frame({ execution: 'running', ask_count: 1, ask_id: 99, ask_text: 'Pick one.' }), store, NOW);
  check('an unseen ask needs the reader while the session runs', unanswered.indicator.kind === 'ask' && needsReader(unanswered));
  store.acknowledge(frame().runtime, 'catalog', 'native', 'agent', 99);
  const acknowledged = renderSessionStatus(frame({ execution: 'running', ask_count: 1, ask_id: 99, ask_text: 'Pick one.' }), store, NOW);
  check('an ask the reader saw and has not answered still does, under the running dot',
    acknowledged.indicator.kind === 'running' && !acknowledged.ask.unseen && needsReader(acknowledged));
  check('ask words are a closed list', ASKS_YOU === 'asks you' && REPLY_NOT_SENT === 'reply not sent');
}

if (failures) process.exit(1);
console.log('session-status: all pass');
