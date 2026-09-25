// SessionEventStore: the open session's conversation. These tests pin the two
// things that went wrong before — a turn drawn twice, and a turn deleted by an
// over-eager dedupe — plus the rule that a session never becomes a task.
import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { SessionEventStore, describeTurnState, humanDuration } from './session-event-store.js';

test('events append once, in sequence order, and repeats are ignored', () => {
  const store = new SessionEventStore();
  const first = store.accept([{ seq: 1, kind: 'user' }, { seq: 2, kind: 'assistant' }]);
  assert.equal(first.length, 2);
  const repeat = store.accept([{ seq: 2, kind: 'assistant' }, { seq: 3, kind: 'tool_call' }]);
  assert.deepEqual(repeat.map(e => e.seq), [3], 'an event already seen must not append twice');
  assert.equal(store.events.length, 3);
});

test('a turn already drawn live is not drawn again from the harvested copy', () => {
  const store = new SessionEventStore();
  store.markRenderedLive('turn-abc');
  const fresh = store.accept([
    { seq: 1, kind: 'assistant', turn_anchor: 'turn-abc' },
    { seq: 2, kind: 'assistant', turn_anchor: 'turn-def' },
  ]);
  assert.deepEqual(fresh.map(e => e.seq), [2], 'the live-drawn turn must be suppressed exactly once');
});

test('an event with no anchor is always shown — a visible duplicate beats a deleted turn', () => {
  const store = new SessionEventStore();
  store.markRenderedLive('turn-abc');
  assert.equal(store.accept([{ seq: 1, kind: 'assistant' }]).length, 1, 'unidentifiable turns must never be dropped');
});

test('an empty anchor is not treated as a match', () => {
  const store = new SessionEventStore();
  store.markRenderedLive('');
  assert.equal(store.accept([{ seq: 1, kind: 'assistant', turn_anchor: '' }]).length, 1);
});

test('a prompt the composer sent is skipped once, by exact text only', () => {
  const store = new SessionEventStore();
  store.markPromptRenderedLive('be careful its only september 2nd');
  const first = store.accept([
    { seq: 1, kind: 'user', text: 'be careful its only september 2nd' },
    { seq: 2, kind: 'user', text: 'be careful its only september 2nd!' },
  ]);
  assert.deepEqual(first.map(e => e.seq), [2], 'exact text is consumed; a near match is not');
  const again = store.accept([{ seq: 3, kind: 'user', text: 'be careful its only september 2nd' }]);
  assert.equal(again.length, 1, 'the match is consumed once; the same prompt sent later is a new turn');
});

test('reset clears the conversation so rows never leak across sessions', () => {
  const store = new SessionEventStore();
  store.accept([{ seq: 5, kind: 'user' }]);
  store.markRenderedLive('turn-abc');
  store.setTurnState({ execution: 'waiting' });
  store.reset();
  assert.equal(store.events.length, 0);
  assert.equal(store.lastSeq, 0);
  assert.equal(store.turnState, null);
  assert.equal(store.accept([{ seq: 1, kind: 'assistant', turn_anchor: 'turn-abc' }]).length, 1);
});

test('the store never invents a task, an owner, or a control', () => {
  const store = new SessionEventStore();
  store.accept([{ seq: 1, kind: 'assistant' }]);
  const serialized = JSON.stringify(store.events.concat([store.turnState]));
  for (const forbidden of ['task_id', 'ownership', 'controllable', 'lifecycle']) {
    assert.equal(serialized.includes(forbidden), false,
      'a watched session must never be projected as owned, stoppable work');
  }
});

test('turn state renders as plain answers about the work', () => {
  const now = Date.parse('2026-09-01T12:00:00Z');
  const since = now - 30_000;
  assert.equal(describeTurnState({ execution: 'running', since_ms: since }, now).text, 'working');
  assert.equal(describeTurnState({ execution: 'waiting', since_ms: since }, now).text, 'waiting for you');
  assert.equal(describeTurnState({ execution: 'waiting', attention: 'approval', attention_source: 'turn', since_ms: since }, now).text, 'needs your input');
  assert.equal(describeTurnState({ execution: 'unknown', since_ms: now - 600_000 }, now).text, 'no update for 10m');
  assert.equal(describeTurnState({ execution: 'idle', since_ms: since }, now).text, 'idle');
  assert.equal(describeTurnState(null, now).text, 'unknown');
  assert.equal(describeTurnState({ execution: 'running', since_ms: since }, now).age,
    'last activity 30s ago', 'the age must always be available');
});

test('durations read like a person would say them', () => {
  assert.equal(humanDuration(45), '45s');
  assert.equal(humanDuration(90), '2m');
  assert.equal(humanDuration(7200), '2h');
});

// Show, don't tell: the pane may describe the user's work, never our
// record-keeping. This is the rule that produced the worst regression in this
// feature's history, so it is enforced rather than remembered.
test('no bookkeeping vocabulary can reach the screen', () => {
  const source = readFileSync(new URL('./session-event-store.js', import.meta.url), 'utf8');
  const rendered = source.split('\n')
    .filter(line => !line.trim().startsWith('//'))
    .join('\n');
  for (const word of ['projection', 'superseded', 'harvested', 'reconcil', 'overlay', 'anchor']) {
    const inUserString = new RegExp("(['\"`])[^'\"`\\n]*" + word + "[^'\"`\\n]*\\1", 'i');
    assert.equal(inUserString.test(rendered), false,
      'the word "' + word + '" is our bookkeeping and must not appear in a rendered string');
  }
});
