// The Session views page's model (session-views-rebuild plan §3.4): a draft
// opens clean, saves the whole view, and never loses what the form hides.
import test from 'node:test';
import assert from 'node:assert/strict';
import { draftOf, showAs, commitOf, isDirty, saveState, baseState, rebase, asNewDraft, duplicateOf,
  indexRow, groupLabel, sortLabel, groupChoiceSets, sessionsNow } from './settings-view-model.js';
import { viewCommit } from './view-actions.js';

const list = () => ({ id: 'l', name: 'Waiting', query: 'tag:phase=plan', group_by: 'runtime', sort: 'longest' });
// As the daemon writes it: a board with no rules has no placement field.
const board = () => ({ id: 'b', name: 'Flow', query: 'tag:flow=*', group_by: 'tag-key:flow', sort: 'longest',
  board: { columns: ['building', 'done'], empty_columns: true } });

test('a draft opens clean: a board with no rules, a rule in another letter case, a stored prefix in another case', () => {
  assert.equal(isDirty(draftOf(board()), board()), false);
  const ruled = { ...board(), board: { columns: ['Build', 'Ship'], placement: [{ column: 'ship', query: 'tag:vcs=push' }] } };
  assert.equal(isDirty(draftOf(ruled), ruled), false);
  const spelled = { ...board(), group_by: 'Tag-Key:flow' };
  assert.equal(isDirty(draftOf(spelled), spelled), false);
  assert.equal(commitOf(draftOf(spelled)).group_by, 'Tag-Key:flow', 'an untouched grouping keeps its stored spelling');
  assert.equal(isDirty(draftOf(list()), list()), false);
  assert.equal(isDirty(draftOf(undefined), undefined), false);
});

test('switching how a view is shown and back changes nothing and loses nothing', () => {
  const stored = list();
  const draft = draftOf(stored);
  showAs(draft, 'board');
  draft.boardKey = 'stage';
  draft.view.board.columns.push('one');
  assert.equal(commitOf(draft).group_by, 'tag-key:stage');
  showAs(draft, 'list');
  assert.equal(commitOf(draft).group_by, 'runtime', 'the list keeps its own grouping');
  assert.equal(commitOf(draft).board, undefined, 'a list is saved without a board');
  assert.equal(isDirty(draft, stored), false);
  showAs(draft, 'board');
  assert.deepEqual(commitOf(draft).board.columns, ['one'], 'the board waited in the draft');

  const kept = draftOf(board());
  showAs(kept, 'list');
  showAs(kept, 'board');
  assert.equal(isDirty(kept, board()), false);
  assert.deepEqual(commitOf(kept).board.columns, ['building', 'done']);
});

test('a commit never changes the draft, and carries every stored field', () => {
  const stored = { ...board(), record_kind: 'sessions' };
  const draft = draftOf(stored);
  showAs(draft, 'list');
  const before = JSON.stringify(draft);
  const commit = viewCommit(commitOf(draft));
  assert.equal(JSON.stringify(draft), before);
  assert.equal(commit.record_kind, 'sessions');
  assert.equal(commit.id, 'b');
  for (const field of ['base', 'shownAs', 'listGroupBy', 'boardKey', 'storedGroupBy']) assert.equal(field in commit, false, field);
  const memory = { id: 'm', name: 'Notes', query: 'tag:x', record_kind: 'memory' };
  const edited = draftOf(memory);
  edited.view.sort = 'oldest';
  assert.equal(commitOf(edited).record_kind, 'memory');
});

test('Save is offered only for a changed, complete draft', () => {
  const stored = list();
  const draft = draftOf(stored);
  assert.deepEqual(saveState(draft, stored, false), { ready: false, words: 'No changes' });
  draft.view.sort = 'oldest';
  assert.equal(saveState(draft, stored, false).ready, true);
  assert.equal(saveState(draft, stored, true).ready, false, 'an unreadable entry turns saving off');
  showAs(draft, 'board');
  draft.boardKey = ' ';
  assert.equal(saveState(draft, stored, false).ready, false, 'a board needs its key');
  const fresh = draftOf(undefined);
  assert.equal(saveState(fresh, undefined, false).ready, false);
  fresh.view.name = 'New';
  fresh.view.query = 'tag:x';
  assert.deepEqual(saveState(fresh, undefined, false), { ready: true, words: 'Ready to create' });
});

test('a view changed or deleted elsewhere is noticed, and the edit can be kept over it', () => {
  const draft = draftOf(list());
  draft.view.query = 'tag:phase=build';
  assert.equal(baseState(draft, list()), '');
  const elsewhere = { ...list(), name: 'Renamed', record_kind: 'sessions', sort: 'oldest' };
  assert.equal(baseState(draft, elsewhere), 'changed');
  assert.equal(baseState(draft, undefined), 'gone');
  assert.equal(baseState(draftOf(undefined), undefined), '', 'a new view has no base to lose');
  const kept = rebase(draft, elsewhere);
  assert.equal(kept.view.query, 'tag:phase=build', 'the form fields are the draft’s');
  assert.equal(kept.view.name, 'Waiting');
  assert.equal(kept.view.record_kind, 'sessions', 'a field the form does not show is the stored one');
  assert.equal(baseState(kept, elsewhere), '');
  const orphan = asNewDraft(draft);
  assert.equal('id' in orphan.view, false);
  assert.equal(orphan.base, '');
});

test('a duplicate is the whole stored view under another name', () => {
  const stored = { ...board(), record_kind: 'sessions', board: { columns: ['a'], placement: [{ column: 'a', query: 'tag:x' }] } };
  const copy = duplicateOf(stored, 'Flow copy');
  const { id, ...rest } = stored;
  assert.equal(id, 'b');
  assert.deepEqual(copy, { ...rest, name: 'Flow copy' });
  copy.board.columns.push('b');
  assert.deepEqual(stored.board.columns, ['a'], 'the copy shares nothing with the stored view');
});

test('an index row says what a view is, and counts only what the daemon counted', () => {
  assert.deepEqual(indexRow(board(), { b: 1 }, false),
    { id: 'b', name: 'Flow', query: 'tag:flow=*', kind: 'Board', columns: 2, groupedBy: 'tag flow', count: '1', dirty: false });
  assert.equal(indexRow(list(), {}, true).count, '', 'no count from the daemon is no number');
  assert.equal(indexRow({ id: 'm', record_kind: 'memory' }, { m: 4 }, false).count, '', 'a memory view lists no sessions');
  assert.equal(groupLabel(''), 'repository');
  assert.equal(sortLabel(''), 'newest first');
});

test('the group list offers the owner’s keys apart from every other key', () => {
  const choices = [['', 'repository'], ['runtime', 'runtime'], ['tag-key:flow', 'tag key flow'], ['tag-key:vcs', 'tag key vcs']];
  const sets = groupChoiceSets(choices, [{ key: 'Flow', value: 'review' }, { value: 'approved' }]);
  assert.deepEqual(sets.basic.map(([value]) => value), ['', 'runtime']);
  assert.deepEqual(sets.yours.map(([value]) => value), ['tag-key:flow']);
  assert.deepEqual(sets.other.map(([value]) => value), ['tag-key:vcs']);
});

test('Sessions now: a board’s columns in order; a column past a cut has no number', () => {
  const whole = { match_total: 3, repository_total: 2, repositories: [{ key: 'done', total: 2 }, { key: '', total: 1 }] };
  assert.deepEqual(sessionsNow(board(), whole), { total: 3, more: 0,
    lines: [{ label: 'building', count: 0 }, { label: 'done', count: 2 }, { label: 'no flow', count: 1 }] });
  const cut = { repository_total: 5, repositories: [{ key: 'done', total: 2 }] };
  const read = sessionsNow(board(), cut);
  assert.equal(read.total, null, 'no match_total is no total');
  assert.equal(read.more, 4);
  assert.equal(read.lines[0].count, null);
  assert.deepEqual(sessionsNow(list(), { match_total: 0, repository_total: 0, repositories: [] }), { total: 0, more: 0, lines: [] });
  assert.equal(sessionsNow({ query: 'x' }, { repository_total: 1, repositories: [{ key: '/work/app', total: 4 }] }).lines[0].label, 'app');
});
