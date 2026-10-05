import test from 'node:test';
import assert from 'node:assert/strict';
import { moveOrder, confirmDeleteText, renameSettle, recoverySuffix, viewCommit, groupByChoices } from './view-actions.js';

test('moveOrder moves a view one step and keeps every id', () => {
  const order = ['a', 'b', 'c'];
  assert.deepEqual(moveOrder(order, 'b', -1), ['b', 'a', 'c']);
  assert.deepEqual(moveOrder(order, 'b', 1), ['a', 'c', 'b']);
  assert.deepEqual(order, ['a', 'b', 'c']); // the input is never mutated
});

test('moveOrder refuses a move that would fall outside the list', () => {
  assert.equal(moveOrder(['a', 'b'], 'a', -1), null);
  assert.equal(moveOrder(['a', 'b'], 'b', 1), null);
  assert.equal(moveOrder(['a'], 'missing', 1), null);
});

test('the delete sentence promises the same thing everywhere', () => {
  assert.equal(confirmDeleteText('Awaiting approval'),
    'Delete the view \u201cAwaiting approval\u201d? No session is changed.');
  assert.equal(confirmDeleteText(''), 'Delete the view \u201cunnamed\u201d? No session is changed.');
});

test('a rename commits only a changed, non-empty name', () => {
  assert.deepEqual(renameSettle('Old', 'New'), { commit: true, name: 'New' });
  assert.deepEqual(renameSettle('Old', '  Old  '), { commit: false, name: 'Old' });
  assert.deepEqual(renameSettle('Old', ''), { commit: false, name: '' });
  assert.deepEqual(renameSettle('Old', '   '), { commit: false, name: '' });
});

test('a refusal suffix promises only what actually survived', () => {
  assert.equal(recoverySuffix(400), ' \u2014 the edit is kept; fix it and save again.');
  assert.equal(recoverySuffix(409), ' \u2014 the list was reloaded from the file; repeat the change.');
  assert.equal(recoverySuffix(0), '');
  assert.equal(recoverySuffix(undefined), '');
});
// The regression (settings-views-full-view-save plan §0): the daemon's PUT
// replaces the stored view, so a commit that leaves the board out deletes it.
test('saving a board view keeps its board', () => {
  const board = { columns: ['building', 'review', 'waiting-you', 'done'], empty_columns: true };
  const view = { id: 'v1', name: 'Flow: opted-in', query: 'tag:flow=*', group_by: 'tag-key:flow', sort: 'oldest', board };
  const commit = viewCommit(view);
  assert.deepEqual(commit.board, board);
  assert.equal(commit.group_by, 'tag-key:flow');
  assert.deepEqual(commit, view);
});

test('a commit keeps every field, trims the name, and adds no defaults', () => {
  const commit = viewCommit({ id: 'm1', name: '  Notes  ', query: 'tag:topic=x', record_kind: 'memory' });
  assert.deepEqual(commit, { id: 'm1', name: 'Notes', query: 'tag:topic=x', record_kind: 'memory' });
});

test('group choices: mechanical first, then each known tag key once, lowercased', () => {
  const known = [{ key: 'Flow', value: 'building' }, { key: 'flow', value: 'review' }, { value: 'approved' }, { key: 'phase', value: 'plan' }];
  assert.deepEqual(groupByChoices(known), [['', 'repository'], ['runtime', 'runtime'], ['none', 'none'],
    ['tag-key:flow', 'tag key flow'], ['tag-key:phase', 'tag key phase']]);
});

test('a stored grouping is always offered, in its own spelling, never twice', () => {
  const values = choices => choices.map(([value]) => value);
  assert.deepEqual(groupByChoices([], 'tag-key:flow').at(-1), ['tag-key:flow', 'tag key flow']);
  assert.deepEqual(values(groupByChoices([], 'repository')), ['repository', 'runtime', 'none']);
  assert.deepEqual(values(groupByChoices([{ key: 'flow' }], 'tag-key:Flow')), ['', 'runtime', 'none', 'tag-key:Flow']);
  assert.deepEqual(values(groupByChoices([{ key: 'flow' }], '')), ['', 'runtime', 'none', 'tag-key:flow']);
  assert.deepEqual(groupByChoices([], 'TAG-KEY:gone').at(-1), ['TAG-KEY:gone', 'tag key gone']);
});

// A board view whose key left the vocabulary, regrouped to runtime and refused:
// the stored grouping is still a choice, so the owner can switch back.
test('the stored grouping stays offered beside an edited one', () => {
  const values = groupByChoices([], 'tag-key:gone', 'runtime').map(([value]) => value);
  assert.deepEqual(values, ['', 'runtime', 'none', 'tag-key:gone']);
  assert.deepEqual(groupByChoices([], undefined, '').map(([value]) => value), ['', 'runtime', 'none']);
});

// The filter bar's Group button cycles through the same list, with
// 'repository' as its own spelling of the default.
test('the filter bar cycles repository, runtime, none, then the tag keys', () => {
  const cycle = groupByChoices([{ key: 'flow' }]).map(([value]) => value || 'repository');
  assert.deepEqual(cycle, ['repository', 'runtime', 'none', 'tag-key:flow']);
});
