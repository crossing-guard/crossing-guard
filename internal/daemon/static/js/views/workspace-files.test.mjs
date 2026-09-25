import assert from 'node:assert/strict';
import test from 'node:test';

import { emptyTreeState, fileProblemState, fileState, filterRows, listingState, nextFocus, normalizeEntry, rememberListing, toggleDir, truncationCopy, visibleRows } from './workspace-files-tree.js';

const listing = (dir, entries, extra = {}) => ({ dir, entries, ...extra });

test('the tree flattens expanded directories depth-first in the daemon\'s order', () => {
  const state = emptyTreeState();
  rememberListing(state, listing('', [{ name: 'a', path: 'a', kind: 'dir' }, { name: 'z.txt', path: 'z.txt', kind: 'file', size: 3 }]));
  rememberListing(state, listing('a', [{ name: 'b', path: 'a/b', kind: 'dir', untracked: true }, { name: 'f.go', path: 'a/f.go', kind: 'file' }]));
  assert.deepEqual(visibleRows(state).map(row => row.entry.path), ['a', 'z.txt']);
  assert.equal(toggleDir(state, 'a'), true);
  assert.deepEqual(visibleRows(state).map(row => `${row.depth}:${row.entry.path}`), ['0:a', '1:a/b', '1:a/f.go', '0:z.txt']);
  state.changed.add('a/f.go');
  assert.equal(visibleRows(state).find(row => row.entry.path === 'a/f.go').changed, true);
  assert.equal(toggleDir(state, 'a'), false);
});

test('a filter keeps matches and their ancestors', () => {
  const state = emptyTreeState();
  rememberListing(state, listing('', [{ name: 'a', path: 'a', kind: 'dir' }, { name: 'c', path: 'c', kind: 'dir' }]));
  rememberListing(state, listing('a', [{ name: 'deep.go', path: 'a/deep.go', kind: 'file' }]));
  rememberListing(state, listing('c', [{ name: 'other.go', path: 'c/other.go', kind: 'file' }]));
  toggleDir(state, 'a'); toggleDir(state, 'c');
  assert.deepEqual(filterRows(visibleRows(state), 'DEEP').map(row => row.entry.path), ['a', 'a/deep.go']);
  assert.equal(filterRows(visibleRows(state), '').length, 4);
});

test('empty listings carry their reason and truncation is counted', () => {
  const state = emptyTreeState();
  rememberListing(state, listing('vendor', [], { state: 'ignored' }));
  rememberListing(state, listing('gone', [], { state: 'not-in-checkout' }));
  rememberListing(state, listing('sub', [], { state: 'nested-repository' }));
  rememberListing(state, listing('big', [{ name: 'x', path: 'big/x', kind: 'file' }], { truncated: true, dropped: 12 }));
  assert.equal(listingState(state.states.get('vendor')), 'Ignored by git');
  assert.equal(listingState(state.states.get('gone')), 'Not in the checkout');
  assert.match(listingState(state.states.get('sub')), /separate repository/);
  assert.equal(truncationCopy(state.states.get('big')), '12 more entries not shown');
  assert.equal(truncationCopy(state.states.get('vendor')), '');
});

test('entries normalize to known kinds and file states read like the design', () => {
  assert.equal(normalizeEntry({ name: 'x', kind: 'invented' }).kind, 'file');
  assert.equal(normalizeEntry({ name: 'x', kind: 'nested-repository' }).kind, 'nested-repository');
  assert.equal(fileState({ kind: 'binary', size: 2048 }), 'Binary · 2.0 KB');
  assert.equal(fileState({ kind: 'symlink', target: '/etc/hosts' }), 'Symlink → /etc/hosts');
  assert.equal(fileState({ kind: 'text', size: 3 * 1024 * 1024, bytes: 1048576, truncated: true }), 'First 1.0 MB of 3.0 MB');
  assert.equal(fileState({ kind: 'text', size: 12, truncated: false }), '12 B');
});

test('the keyboard rule follows the ARIA tree pattern', () => {
  const state = emptyTreeState();
  rememberListing(state, listing('', [{ name: 'a', path: 'a', kind: 'dir' }, { name: 'z.txt', path: 'z.txt', kind: 'file' }]));
  rememberListing(state, listing('a', [{ name: 'f.go', path: 'a/f.go', kind: 'file' }]));
  let rows = visibleRows(state);
  assert.deepEqual(nextFocus(rows, 0, 'ArrowRight'), { index: 0, toggle: true }); // closed dir: open it
  toggleDir(state, 'a'); rows = visibleRows(state);
  assert.deepEqual(nextFocus(rows, 0, 'ArrowRight'), { index: 1, toggle: false }); // open dir: step in
  assert.deepEqual(nextFocus(rows, 1, 'ArrowLeft'), { index: 0, toggle: false }); // child: to parent
  assert.deepEqual(nextFocus(rows, 0, 'ArrowLeft'), { index: 0, toggle: true }); // open dir: close it
  assert.deepEqual(nextFocus(rows, 2, 'ArrowDown'), { index: 2, toggle: false });
  assert.deepEqual(nextFocus(rows, 2, 'Home'), { index: 0, toggle: false });
  assert.deepEqual(nextFocus(rows, 0, 'End'), { index: 2, toggle: false });
  assert.equal(nextFocus(rows, 0, 'x'), null);
});

test('the reader\'s own problem codes have their own copy and a refused request is named', () => {
  assert.equal(fileProblemState({ code: 'unsupported-platform' }).title, 'File reading is not available on this platform');
  assert.equal(fileProblemState({ code: 'file-not-in-tree', message: 'x' }).title, 'Not in this checkout');
  assert.equal(fileProblemState({ code: 'invalid_request', message: 'dir must be…' }).detail, 'dir must be…');
  assert.equal(fileProblemState({ code: 'no-recorded-folder' }).title, 'No folder for this session');
});
