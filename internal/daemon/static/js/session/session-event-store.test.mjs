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
  assert.deepEqual(first.events.map(e => e.seq), [1, 2]);
  const repeat = store.accept([{ seq: 2, kind: 'assistant' }, { seq: 3, kind: 'tool_call' }]);
  assert.deepEqual(repeat.events.map(e => e.seq), [3], 'an event already seen must not append twice');
  assert.equal(store.events.length, 3);
});

test('a turn already drawn live is not drawn again from the harvested copy', () => {
  const store = new SessionEventStore();
  store.markRenderedLive('turn-abc');
  const fresh = store.accept([
    { seq: 1, kind: 'assistant', turn_anchor: 'turn-abc' },
    { seq: 2, kind: 'assistant', turn_anchor: 'turn-def' },
  ]);
  assert.deepEqual(fresh.events.map(e => e.seq), [2], 'the live-drawn turn must be suppressed exactly once');
});

test('an event with no anchor is always shown — a visible duplicate beats a deleted turn', () => {
  const store = new SessionEventStore();
  store.markRenderedLive('turn-abc');
  assert.equal(store.accept([{ seq: 1, kind: 'assistant' }]).events.length, 1, 'unidentifiable turns must never be dropped');
});

test('an empty anchor is not treated as a match', () => {
  const store = new SessionEventStore();
  store.markRenderedLive('');
  assert.equal(store.accept([{ seq: 1, kind: 'assistant', turn_anchor: '' }]).events.length, 1);
});

test('a prompt the composer sent is skipped once, by exact text only', () => {
  const store = new SessionEventStore();
  store.markPromptRenderedLive('be careful its only september 2nd');
  const first = store.accept([
    { seq: 1, kind: 'user', text: 'be careful its only september 2nd' },
    { seq: 2, kind: 'user', text: 'be careful its only september 2nd!' },
  ]);
  assert.deepEqual(first.events.map(e => e.seq), [2], 'exact text is consumed; a near match is not');
  const again = store.accept([{ seq: 3, kind: 'user', text: 'be careful its only september 2nd' }]);
  assert.equal(again.events.length, 1, 'the match is consumed once; the same prompt sent later is a new turn');
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
  assert.equal(store.accept([{ seq: 1, kind: 'assistant', turn_anchor: 'turn-abc' }]).events.length, 1);
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

test('held native rows replace the live tail only after the exact final anchor arrives', () => {
  const store = new SessionEventStore();
  store.reset([{ seq: 9, kind: 'assistant', text: 'earlier' }]);
  store.beginOwnedTurn('turn-1');
  store.markPromptRenderedLive('ask');
  store.markLiveRow({ kind: 'user', text: 'ask' });
  store.markLiveRow({ kind: 'assistant', text: 'final', turn_anchor: 'prt_reply' });
  const held = store.accept([
    { seq: 10, kind: 'user', text: 'ask' },
    { seq: 11, kind: 'other', name: 'step-start' },
    { seq: 12, kind: 'other', name: 'patch' },
  ]);
  assert.equal(held.type, 'pending');
  assert.equal(store.endOwnedTurn('turn-1').type, 'pending', 'task completion is not native catch-up');
  const reconciled = store.accept([
    { seq: 13, kind: 'other', name: 'step-finish' },
    { seq: 14, kind: 'assistant', text: 'final', turn_anchor: 'prt_reply' },
  ]);
  assert.equal(reconciled.type, 'replace-tail');
  assert.equal(reconciled.boundary, 1);
  assert.deepEqual(reconciled.events.map(e => e.seq), [10, 11, 12, 13, 14]);
  assert.equal(store.events.at(-1).text, 'final', 'the final answer remains after older system rows');
  assert.equal(store.events.filter(e => e.turn_anchor === 'prt_reply').length, 1);
});

test('native closing metadata is presented before the final answer on reload', () => {
  const store = new SessionEventStore();
  store.reset([
    { seq: 1, kind: 'user', text: 'question' },
    { seq: 2, kind: 'other', name: 'step-start' },
    { seq: 3, kind: 'assistant', text: 'answer', turn_anchor: 'final' },
    { seq: 4, kind: 'other', name: 'step-finish' },
    { seq: 5, kind: 'other', name: 'patch' },
  ]);
  assert.deepEqual(store.events.map(event => event.seq), [1, 2, 4, 5, 3]);
  assert.equal(store.events.at(-1).text, 'answer');
});

test('late closing metadata reorders the smallest canonical tail', () => {
  const store = new SessionEventStore();
  store.reset([{ seq: 1, kind: 'user' }, { seq: 2, kind: 'assistant', text: 'answer' }]);
  const effect = store.accept([
    { seq: 3, kind: 'other', name: 'step-finish' },
    { seq: 4, kind: 'other', name: 'patch' },
  ]);
  assert.equal(effect.type, 'replace-tail');
  assert.equal(effect.boundary, 1);
  assert.deepEqual(effect.events.map(event => event.seq), [3, 4, 2]);
  assert.deepEqual(store.events.map(event => event.seq), [1, 3, 4, 2]);
});

test('an exact final anchor waits for its same-sequence body revision', () => {
  const store = new SessionEventStore();
  store.beginOwnedTurn('turn');
  store.markLiveRow({ kind: 'assistant', text: 'complete answer', turn_anchor: 'final' });
  store.endOwnedTurn('turn');
  assert.equal(store.accept([
    { seq: 1, kind: 'assistant', text: '', turn_anchor: 'final' },
    { seq: 2, kind: 'other', name: 'step-finish' },
  ]).type, 'pending', 'an identified but empty native row is not caught up');
  const effect = store.accept([
    { seq: 1, kind: 'assistant', text: 'complete answer', turn_anchor: 'final' },
  ]);
  assert.equal(effect.type, 'replace-tail');
  assert.deepEqual(effect.events.map(event => [event.seq, event.kind, event.text || '']), [
    [2, 'other', ''], [1, 'assistant', 'complete answer'],
  ]);
});

test('a shared tool anchor cannot reconcile the wrong row kind', () => {
  const store = new SessionEventStore();
  store.beginOwnedTurn('turn');
  store.markLiveRow({ kind: 'tool_result', text: 'result', turn_anchor: 'tool' });
  store.endOwnedTurn('turn');
  assert.equal(store.accept([
    { seq: 1, kind: 'tool_call', text: 'input', turn_anchor: 'tool' },
  ]).type, 'pending');
  assert.equal(store.accept([
    { seq: 2, kind: 'tool_result', text: 'result', turn_anchor: 'tool' },
  ]).type, 'replace-tail');
});

test('historical thinking clipping does not block a complete final assistant', () => {
  const store = new SessionEventStore();
  store.beginOwnedTurn('turn');
  store.markLiveRow({ kind: 'thinking', text: 'a long live thought', turn_anchor: 'thought' });
  store.markLiveRow({ kind: 'assistant', text: 'answer', turn_anchor: 'final' });
  store.endOwnedTurn('turn');
  assert.equal(store.accept([
    { seq: 1, kind: 'thinking', text: 'a long', turn_anchor: 'thought' },
    { seq: 2, kind: 'assistant', text: 'answer', turn_anchor: 'final' },
  ]).type, 'replace-tail');
});

test('tool and conversational rows are never moved as closing metadata', () => {
  const store = new SessionEventStore();
  store.reset([
    { seq: 1, kind: 'user' },
    { seq: 2, kind: 'assistant', text: 'first' },
    { seq: 3, kind: 'tool_call' },
    { seq: 4, kind: 'tool_result' },
    { seq: 5, kind: 'assistant', text: 'final' },
  ]);
  assert.deepEqual(store.events.map(event => event.seq), [1, 2, 3, 4, 5]);
});

test('one turn ending does not release another turn still drawing', () => {
  const store = new SessionEventStore();
  store.beginOwnedTurn('turn-a');
  store.beginOwnedTurn('turn-b');
  store.accept([{ seq: 1, kind: 'other' }]);
  store.markLiveRow({ kind: 'assistant', text: 'done', turn_anchor: 'final' });
  assert.equal(store.endOwnedTurn('turn-a').type, 'pending', 'turn-b still owns the view');
  assert.equal(store.endOwnedTurn('turn-b').type, 'pending', 'the exact native barrier is still absent');
  assert.equal(store.endOwnedTurn('turn-unknown').type, 'noop', 'an unknown end is inert');
  assert.equal(store.accept([{ seq: 2, kind: 'assistant', text: 'done', turn_anchor: 'final' }]).type, 'replace-tail');
});

test('reset drops holds and held rows with the session', () => {
  const store = new SessionEventStore();
  store.beginOwnedTurn('turn-a');
  store.accept([{ seq: 1, kind: 'assistant' }]);
  store.reset();
  assert.equal(store.ownedTurns.size, 0);
  assert.deepEqual(store.accept([{ seq: 1, kind: 'assistant' }]).events.map(e => e.seq), [1], 'a new session draws at once');
  assert.equal(store.endOwnedTurn('turn-a').type, 'noop', 'a late end from the old session releases nothing');
});

test('an earlier text anchor cannot reconcile a later final text block', () => {
  const store = new SessionEventStore();
  store.beginOwnedTurn('turn');
  store.markLiveRow({ kind: 'assistant', text: 'first', turn_anchor: 'a1' });
  store.markLiveRow({ kind: 'tool_call', name: 'Read', turn_anchor: 'tool' });
  store.markLiveRow({ kind: 'assistant', text: 'last', turn_anchor: 'a2' });
  store.accept([{ seq: 1, kind: 'assistant', text: 'first', turn_anchor: 'a1' }, { seq: 2, kind: 'tool_call', turn_anchor: 'tool' }]);
  assert.equal(store.endOwnedTurn('turn').type, 'pending');
  assert.equal(store.accept([{ seq: 3, kind: 'assistant', text: 'last', turn_anchor: 'a2' }]).type, 'replace-tail');
});

test('missing final anchor preserves the live answer and bounds pending native rows', () => {
  const store = new SessionEventStore({ maxPending: 2 });
  store.beginOwnedTurn('turn');
  store.markLiveRow({ kind: 'assistant', text: 'visible', turn_anchor: 'missing' });
  store.endOwnedTurn('turn');
  const overflow = store.accept([{ seq: 1, kind: 'other' }, { seq: 2, kind: 'other' }, { seq: 3, kind: 'other' }]);
  assert.equal(overflow.type, 'refresh');
  assert.equal(store.overlay.live[0].text, 'visible');
  assert.equal(store.overlay.native.length, 2);
  const barrierDelta = store.accept([{ seq: 4, kind: 'assistant', text: 'visible', turn_anchor: 'missing' }]);
  assert.equal(barrierDelta.type, 'pending', 'a truncated delta span cannot be committed');
  const recovered = store.acceptSnapshot([
    { seq: 1, kind: 'other' }, { seq: 2, kind: 'other' },
    { seq: 3, kind: 'other' }, { seq: 4, kind: 'assistant', text: 'visible', turn_anchor: 'missing' },
  ]);
  assert.equal(recovered.type, 'replace-tail');
  assert.deepEqual(recovered.events.map(event => event.seq), [1, 2, 3, 4]);
});

test('a new owned turn cannot erase an earlier unresolved live answer', () => {
  const store = new SessionEventStore();
  store.beginOwnedTurn('first');
  store.markLiveRow({ kind: 'assistant', turn_anchor: 'first-final' });
  store.endOwnedTurn('first');
  store.beginOwnedTurn('second');
  store.markLiveRow({ kind: 'assistant', turn_anchor: 'second-final' });
  assert.equal(store.accept([{ seq: 1, kind: 'assistant', turn_anchor: 'second-final' }]).type, 'pending');
  assert.equal(store.endOwnedTurn('second').type, 'pending');
  assert.equal(store.accept([{ seq: 2, kind: 'assistant', turn_anchor: 'first-final' }]).type, 'replace-tail');
});
