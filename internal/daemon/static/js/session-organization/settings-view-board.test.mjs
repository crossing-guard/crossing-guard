// The board section's model (board-observed-columns plan §2.6): the card's
// copy is its own, a column rename takes its rules with it, and a column a
// rule names cannot be removed.
import test from 'node:test';
import assert from 'node:assert/strict';
import { copyView, boardKey, setBoard, renameColumn, columnInUse, moveItem } from './settings-view-board.js';

const saved = () => ({ id: 'v', name: 'Stages', query: 'touched:<7d', group_by: 'tag-key:Stage',
  board: { columns: ['Build', 'Ship'], empty_columns: true, placement: [{ column: 'ship', query: 'tag:vcs=push' }] } });

test('a card edits its own copy of the board, never the saved view', () => {
  const view = saved();
  const copy = copyView(view);
  copy.board.columns.push('Done');
  copy.board.placement[0].query = 'tag:vcs=commit';
  copy.board.placement.push({ column: 'Done', query: 'tag:fs=edit' });
  assert.deepEqual(view, saved());
  assert.deepEqual(copyView({ id: 'l', query: 'x' }), { id: 'l', query: 'x' }, 'a list view gains no board');
});

test('the key a board writes moves under is its grouping key, as the owner spelled it', () => {
  assert.equal(boardKey(saved()), 'Stage');
  assert.equal(boardKey({ group_by: 'runtime' }), '');
  assert.equal(boardKey({}), '');
});

test('turning the board on starts it empty; turning it off keeps the grouping', () => {
  const view = { id: 'l', query: 'x', group_by: 'tag-key:topic' };
  setBoard(view, true);
  assert.deepEqual(view.board, { columns: [], empty_columns: true, placement: [] });
  setBoard(view, false);
  assert.deepEqual(view, { id: 'l', query: 'x', group_by: 'tag-key:topic' });
});

test('a card ties a rule to its column by the declared spelling', () => {
  assert.deepEqual(copyView(saved()).board.placement, [{ column: 'Ship', query: 'tag:vcs=push' }]);
});

test('renaming a column carries its rules, through an empty name and letter by letter', () => {
  const { board } = copyView(saved());
  for (const name of ['', 'S', 'Sh', 'Shipped']) renameColumn(board, 1, name);
  assert.deepEqual(board.columns, ['Build', 'Shipped']);
  assert.deepEqual(board.placement, [{ column: 'Shipped', query: 'tag:vcs=push' }]);
  renameColumn(board, 0, 'Building');
  assert.deepEqual(board.placement, [{ column: 'Shipped', query: 'tag:vcs=push' }], 'other columns\' rules are left alone');
});

test('typing a name through another column\'s name never takes that column\'s rules', () => {
  const board = { columns: ['review', 're'], placement: [{ column: 'review', query: 'tag:phase=red-team' }] };
  for (const name of ['rev', 'revi', 'revie', 'review', 'review-2']) renameColumn(board, 1, name);
  assert.deepEqual(board.columns, ['review', 'review-2']);
  assert.deepEqual(board.placement, [{ column: 'review', query: 'tag:phase=red-team' }]);
});

test('a column a rule names is in use; one no rule names is not', () => {
  const { board } = copyView(saved());
  assert.equal(columnInUse(board, 1), true);
  assert.equal(columnInUse(board, 0), false);
});

test('moving an entry past either end of its list does nothing', () => {
  const list = ['a', 'b', 'c'];
  moveItem(list, 0, -1);
  moveItem(list, 2, 1);
  assert.deepEqual(list, ['a', 'b', 'c']);
  moveItem(list, 0, 1);
  assert.deepEqual(list, ['b', 'a', 'c']);
});
