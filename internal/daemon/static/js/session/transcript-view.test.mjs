import test from 'node:test';
import assert from 'node:assert/strict';
import { rowDisplay, followingDisplay, rowLength, selectProfile, rowPeek, sizeLabel, hiddenUnits } from './transcript-view.js';

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

test('a hidden tool call and its result count as one row', () => {
  assert.equal(hiddenUnits([{ kind: 'tool_call' }, { kind: 'tool_result' }, { kind: 'user' }]), 2);
  assert.equal(hiddenUnits([{ kind: 'tool_result' }]), 1);
});
