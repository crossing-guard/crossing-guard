import test from 'node:test';
import assert from 'node:assert/strict';
import { rowDisplay, followingDisplay, rowLength, selectProfile, rowPeek, sizeLabel, createCallPairing,
  hiddenActivityKind, hiddenActivityLabel } from './transcript-view.js';

const agentReview = {
  id: 'agent-review',
  rules: [
    { match: { kind: 'user', min_chars: 2000 }, display: 'collapse' },
    { match: { kind: 'context' }, display: 'collapse' },
  ],
};

test('the first matching rule decides and an unmatched row is shown', () => {
  const profile = { id: 'p', rules: [
    { match: { kind: 'tool_call', tool: 'Bash' }, display: 'hide' },
    { match: { kind: 'tool_call' }, display: 'collapse' },
  ] };
  assert.equal(rowDisplay({ kind: 'tool_call', name: 'Bash' }, profile), 'hide');
  assert.equal(rowDisplay({ kind: 'tool_call', name: 'Read' }, profile), 'collapse');
  assert.equal(rowDisplay({ kind: 'assistant', text: 'done' }, profile), 'show');
});

test('min_chars counts the whole row, including what the transcript clipped', () => {
  assert.equal(rowDisplay({ kind: 'user', text: 'x'.repeat(1999) }, agentReview), 'show');
  assert.equal(rowDisplay({ kind: 'user', text: 'x'.repeat(2000) }, agentReview), 'collapse');
  assert.equal(rowDisplay({ kind: 'user', text: 'short', full_len: 19107 }, agentReview), 'collapse');
  assert.equal(rowLength({ text: 'abc', full_len: 0 }), 3);
});

test('an empty match holds for every row, and no profile shows everything', () => {
  assert.equal(rowDisplay({ kind: 'thinking' }, { rules: [{ match: {}, display: 'hide' }] }), 'hide');
  assert.equal(rowDisplay({ kind: 'user', text: 'x'.repeat(5000) }, null), 'show');
  assert.equal(rowDisplay({ kind: 'user' }, { id: 'empty', rules: [] }), 'show');
});

test('a tool result goes wherever its call went', () => {
  const hideResults = { rules: [{ match: { kind: 'tool_result' }, display: 'hide' }] };
  assert.equal(followingDisplay({ kind: 'tool_result' }, hideResults, 'shown'), 'show');
  assert.equal(followingDisplay({ kind: 'tool_result' }, null, 'hidden'), 'hide');
  assert.equal(followingDisplay({ kind: 'tool_result' }, hideResults, ''), 'hide', 'an orphan result follows the rules');
});

test('a session opens with its role module, else the default, else none', () => {
  const profiles = [{ id: 'full' }, agentReview];
  const selection = { default_profile: 'full', role_profiles: { helper: 'agent-review', follower: 'gone' } };
  assert.equal(selectProfile(profiles, selection, 'helper').id, 'agent-review');
  assert.equal(selectProfile(profiles, selection, '').id, 'full');
  assert.equal(selectProfile(profiles, selection, 'follower').id, 'full', 'a module that no longer resolves falls back');
  assert.equal(selectProfile([], selection, 'helper'), null);
});

test('a collapsed row shows its first line and its size', () => {
  assert.equal(rowPeek('\n\n  You are a helper.\nmore'), 'You are a helper.');
  assert.equal(rowPeek('x'.repeat(200), 10), 'xxxxxxxxx…');
  assert.equal(sizeLabel(812), '812 chars');
  assert.equal(sizeLabel(1900), '1.9K chars');
  assert.equal(sizeLabel(19107), '19K chars');
});

// The sessions view's loop, without a DOM: each result goes where the call it
// answers went, and a spoken turn ends every wait.
function pairTranscript(events, profile) {
  const calls = createCallPairing(), rows = [], unanswered = [];
  for (const event of events) {
    if (event.kind === 'user' || event.kind === 'assistant') unanswered.push(...calls.close().map(call => call.id));
    const answers = event.kind === 'tool_result';
    const display = followingDisplay(event, profile, answers ? calls.peek()?.display || '' : '');
    const call = answers ? calls.result() : null;
    if (event.kind === 'tool_call') calls.call({ id: event.id, display: display === 'hide' ? 'hidden' : 'shown' });
    rows.push(event.kind + ':' + display + (call ? '>' + call.id : ''));
  }
  return { rows, unanswered, waiting: calls.close().map(call => call.id) };
}

const hideCalls = { rules: [{ match: { kind: 'tool_call' }, display: 'hide' }] };

test('calls fired together each keep their own result, and no result leaks into the conversation', () => {
  const paired = pairTranscript([
    { kind: 'tool_call', id: 'a' }, { kind: 'tool_call', id: 'b' },
    { kind: 'tool_result' }, { kind: 'tool_result' }, { kind: 'assistant' },
  ], hideCalls);
  assert.deepEqual(paired.rows, ['tool_call:hide', 'tool_call:hide', 'tool_result:hide>a', 'tool_result:hide>b', 'assistant:show']);
  assert.deepEqual(paired.unanswered, []);
});

test('a result follows its own call when one call is hidden and the next is shown', () => {
  const mixed = { rules: [{ match: { kind: 'tool_call', tool: 'Bash' }, display: 'hide' }] };
  const paired = pairTranscript([
    { kind: 'tool_call', id: 'a', name: 'Bash' }, { kind: 'tool_call', id: 'b', name: 'Read' },
    { kind: 'tool_result' }, { kind: 'tool_result' },
  ], mixed);
  assert.deepEqual(paired.rows.slice(2), ['tool_result:hide>a', 'tool_result:show>b']);
});

test('a spoken turn ends the wait, so an interrupted call never claims a later result', () => {
  const paired = pairTranscript([
    { kind: 'tool_call', id: 'a' }, { kind: 'user' },
    { kind: 'tool_call', id: 'b' }, { kind: 'tool_result' }, { kind: 'tool_result' },
  ], hideCalls);
  assert.deepEqual(paired.unanswered, ['a']);
  assert.deepEqual(paired.rows.slice(3), ['tool_result:hide>b', 'tool_result:show']);
  assert.deepEqual(paired.waiting, []);
});

test('fact matches the detector facts a tool row carries; max_chars bounds length', () => {
  const profile = { id: 'p', rules: [
    { match: { fact: 'exec:run' }, display: 'hide' },
    { match: { kind: 'assistant', max_chars: 10 }, display: 'collapse' },
  ] };
  assert.equal(rowDisplay({ kind: 'tool_call', name: 'bash', facts: ['exec:run'] }, profile), 'hide');
  assert.equal(rowDisplay({ kind: 'tool_call', name: 'Grep', facts: ['search:code'] }, profile), 'show');
  assert.equal(rowDisplay({ kind: 'tool_call', name: 'Bash' }, profile), 'show', 'a row without facts never matches a fact rule');
  assert.equal(rowDisplay({ kind: 'assistant', text: 'short' }, profile), 'collapse');
  assert.equal(rowDisplay({ kind: 'assistant', text: 'x'.repeat(11) }, profile), 'show');
});

test('hidden activity names commands only with positive execution evidence', () => {
  assert.equal(hiddenActivityKind({ kind: 'tool_call', facts: ['exec:run'] }), 'command');
  assert.equal(hiddenActivityKind({ kind: 'tool_call', facts: ['search:code'] }), 'tool');
  assert.equal(hiddenActivityKind({ kind: 'tool_call', name: 'Bash' }), 'tool');
  assert.equal(hiddenActivityKind({ kind: 'thinking' }), 'thinking');
  assert.equal(hiddenActivityKind({ kind: 'context' }), 'context');
  assert.equal(hiddenActivityKind({ kind: 'tool_result' }), 'result');
  assert.equal(hiddenActivityKind({ kind: 'system' }), 'other');
  assert.equal(hiddenActivityLabel('command', 1, true), 'Running a command');
  assert.equal(hiddenActivityLabel('command', 1), 'Ran a command');
  assert.equal(hiddenActivityLabel('command', 3), 'Ran 3 commands');
  assert.equal(hiddenActivityLabel('tool', 2), 'Used 2 tools');
});
