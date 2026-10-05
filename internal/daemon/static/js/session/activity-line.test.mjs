import test from 'node:test';
import assert from 'node:assert/strict';
import { decideActivity } from './activity-line.js';

const NOW = 1_800_000_000_000;
const ledger = { cursor: () => 0 };
const frame = extra => ({ runtime: 'runtime-a', catalog_session_id: 's', since_ms: NOW - 5000, ...extra });

test('an approval always wins, even while the composer lane is streaming', () => {
  const shown = decideActivity({ lane: { progress: 'tool', tool: 'Bash', startedAt: NOW - 3000 },
    live: frame({ execution: 'running', attention: 'approval', attention_source: 'task' }) }, ledger, NOW);
  assert.equal(shown.kind, 'approval');
  assert.equal(shown.text, 'approval required');
});

test('the lane decides the words during an owned turn; live and lane interleave without flicker', () => {
  const lane = { progress: 'tool', tool: 'Bash', startedAt: NOW - 12000 };
  const live = frame({ execution: 'running', progress: 'thinking' });
  const first = decideActivity({ lane, live }, ledger, NOW);
  const second = decideActivity({ lane, live: frame({ execution: 'running', progress: 'writing' }) }, ledger, NOW);
  assert.equal(first.text, 'running Bash');
  assert.equal(second.text, 'running Bash', 'a polled frame never overrides the lane mid-turn');
  assert.equal(first.age, '12s');
  assert.equal(decideActivity({ lane: { progress: 'starting', startedAt: NOW } }, ledger, NOW).text, 'starting…');
});

test('before the first live frame the rail decides; live then takes over', () => {
  const rail = frame({ execution: 'waiting' });
  assert.equal(decideActivity({ rail }, ledger, NOW).text, 'waiting for you');
  const shown = decideActivity({ rail, live: frame({ execution: 'running', progress: 'thinking' }) }, ledger, NOW);
  assert.equal(shown.text, 'thinking…');
});

test('live loss keeps the rail words, including a pending approval, and adds the suffix', () => {
  const rail = frame({ execution: 'running', attention: 'approval', attention_source: 'task' });
  const shown = decideActivity({ rail, live: frame({ execution: 'running' }), liveUnavailable: true }, ledger, NOW);
  assert.equal(shown.text, 'approval required');
  assert.match(shown.age, /· not updating$/);
});

test('presence and freshness come from the rail when the live frame has nothing to say', () => {
  const rail = frame({ execution: 'unknown', presence: 'open', freshness: 'stale' });
  const shown = decideActivity({ rail, live: frame({ execution: 'unknown' }) }, ledger, NOW);
  assert.equal(shown.kind, 'native_stale');
});

test('a session no longer followed says so in closed-list words', () => {
  assert.equal(decideActivity({ following: false }, ledger, NOW).text, 'not updating');
});

test('the words carry no mechanism', () => {
  const banned = /\b(feed|snapshot|stream|frame|resolvable|hook|harvest|lane|boundary)\b/i;
  for (const inputs of [{ following: false }, { rail: frame({ execution: 'waiting' }), liveUnavailable: true },
    { lane: { progress: 'starting', startedAt: NOW } }]) {
    const shown = decideActivity(inputs, ledger, NOW);
    assert.doesNotMatch(shown.text + ' ' + shown.age, banned);
  }
});

test('acknowledgement reads the status the line is showing', () => {
  const live = frame({ execution: 'waiting', attention: 'new_result', attention_source: 'turn', attention_id: 42 });
  const shown = decideActivity({ live }, ledger, NOW);
  assert.equal(shown.source.attention_id, 42);
  assert.equal(shown.source.attention_source, 'turn');
});

test('a pending approval outranks "not updating", and the lane keeps the suffix', () => {
  const rail = frame({ execution: 'running', attention: 'approval', attention_source: 'task' });
  const lost = decideActivity({ rail, following: false }, ledger, NOW);
  assert.equal(lost.kind, 'approval');
  assert.match(lost.age, /not updating/);
  const lane = decideActivity({ lane: { progress: 'tool', tool: 'Bash', startedAt: NOW - 1000 }, liveUnavailable: true }, ledger, NOW);
  assert.match(lane.age, /· not updating$/);
});

test('standalone Chat: an approval on the followed session beats the lane', () => {
  const shown = decideActivity({ lane: { progress: 'tool', tool: 'Bash', startedAt: NOW },
    rail: frame({ execution: 'running', attention: 'approval', attention_source: 'turn' }) }, ledger, NOW);
  assert.equal(shown.text, 'needs your input');
});

// An agent's unseen ask outranks everything but an approval, and reads as the
// helper's own line (escalation-delivery plan §6.2).
test('an unseen ask shows the helper line; an approval still wins', () => {
  const asking = frame({ execution: 'waiting', ask_id: 3, ask_count: 1, ask_text: 'Pick A or B.', ask_agent: 'helper-x' });
  const shown = decideActivity({ lane: { progress: 'tool', tool: 'Bash', startedAt: NOW }, rail: asking }, ledger, NOW);
  assert.equal(shown.kind, 'ask');
  assert.equal(shown.text, 'asks you: Pick A or B. (helper-x)');
  const approval = decideActivity({ rail: { ...asking, attention: 'approval', attention_source: 'task' } }, ledger, NOW);
  assert.equal(approval.kind, 'approval');
  const seen = decideActivity({ rail: asking }, { cursor: (_r, _c, _n, source) => (source === 'agent' ? 3 : 0) }, NOW);
  assert.equal(seen.kind, 'none');
  assert.equal(seen.text, 'waiting for you');
});

test('an unsent proposed reply is a quiet note beside the status', () => {
  const shown = decideActivity({ rail: frame({ execution: 'waiting', draft_count: 1, draft_text: 'Go ahead.' }) }, ledger, NOW);
  assert.equal(shown.text, 'waiting for you');
  assert.equal(shown.draft, 'reply not sent: Go ahead.');
  assert.equal(decideActivity({ rail: frame({ execution: 'waiting' }) }, ledger, NOW).draft, '');
});

test('an acknowledged, unanswered ask stays as a quiet note; unreadable agent lines draw nothing', () => {
  const asking = frame({ execution: 'waiting', ask_id: 3, ask_count: 1, ask_text: 'Pick A or B.' });
  const seen = decideActivity({ rail: asking }, { cursor: (_r, _c, _n, source) => (source === 'agent' ? 3 : 0) }, NOW);
  assert.equal(seen.kind, 'none');
  assert.equal(seen.draft, 'asks you: Pick A or B.');
  assert.equal(decideActivity({ rail: frame({ execution: 'waiting', ask_state: 'unknown' }) }, ledger, NOW).draft, '');
});
