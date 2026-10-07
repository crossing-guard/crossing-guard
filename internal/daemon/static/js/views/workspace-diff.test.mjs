import assert from 'node:assert/strict';
import test from 'node:test';

import { checkoutLabel, dirtyLabel, filterFiles, mergedTree, normalizeDiffPrefs, problemState, scopeLabel, scopeMenuItems } from './workspace-diff-tree.js';
import { changedTokens, foldWhitespace, pairLines, unmodifiedBefore } from '../diff/diff-viewer.js';
import { countChanges, normalizeDiffModel } from '../diff/diff-model.js';

test('the scope menu lists every scope and disables git scopes with the reason', () => {
  const open = scopeMenuItems('session', '');
  assert.deepEqual(open.map(item => item.id), ['session', 'working', 'staged', 'branch', 'compare']);
  assert.ok(open.every(item => !item.disabled));
  assert.equal(open[0].on, true);
  const closed = scopeMenuItems('working', 'No folder for this session');
  assert.equal(closed[0].disabled, false);
  assert.ok(closed.slice(1).every(item => item.disabled && item.reason === 'No folder for this session'));
  assert.equal(scopeLabel('compare', 'origin/main'), 'Against origin/main');
  assert.equal(scopeLabel('branch'), 'Branch vs base');
});

test('every §3.5 condition maps to a pane state, and only checkout conditions disable git scopes', () => {
  for (const code of ['no-recorded-folder', 'ambiguous-folder', 'not-a-repository', 'route-missing', 'review-unavailable']) {
    assert.equal(problemState({ code }).disablesGit, true, code);
  }
  for (const code of ['git-failed', 'git-timeout', 'base-ref-missing', 'unreachable', 'invalid-path', 'file-not-in-scope']) {
    assert.equal(problemState({ code }).disablesGit, false, code);
    assert.ok(problemState({ code }).title);
  }
  assert.equal(problemState(null), null);
  assert.equal(problemState({ code: 'git-failed', message: 'fatal: bad object' }).detail, 'fatal: bad object');
  assert.equal(problemState({ code: 'not-a-repository', folder: '/x/other' }).title, 'Not a git repository · /x/other');
  assert.equal(problemState({ code: 'something-new', message: 'odd' }).title, 'odd');
});

test('the merged tree folds single-child chains, lists folders first, and keeps file counts', () => {
  const files = normalizeDiffModel({ files: [
    { newPath: 'app/Services/FraudReview/FraudReviewService.php', hunks: [{ lines: [{ kind: 'add', newLine: 1, text: 'x' }] }] },
    { newPath: 'config/ai.php', added: 3, removed: 0 },
    { newPath: 'app/Services/FraudReview/Prompts/v1.md' },
    { newPath: 'README.md' },
  ] }).files;
  const tree = mergedTree(files);
  assert.deepEqual(tree.map(node => node.type === 'dir' ? `${node.depth}:${node.label}/` : `${node.depth}:${node.file.displayPath.split('/').pop()}`), [
    '0:app/Services/FraudReview/', '1:Prompts/', '2:v1.md', '1:FraudReviewService.php', '0:config/', '1:ai.php', '0:README.md',
  ]);
  assert.deepEqual(countChanges(files[0]), { added: 1, removed: 0 });
  assert.deepEqual(countChanges(files[1]), { added: 3, removed: 0 });
  // The tree counts the pane's raw rows too, before normalization adds arrays.
  assert.deepEqual(countChanges({ edits: [{ state: 'loaded', before: 'a\n', after: 'b\nc\n' }] }), { added: 2, removed: 1 });
  assert.deepEqual(countChanges({ added: 4, removed: 1 }), { added: 4, removed: 1 });
  assert.deepEqual(filterFiles(files, 'AI.PHP').map(file => file.displayPath), ['config/ai.php']);
  assert.equal(filterFiles(files, '').length, 4);
});

test('presentation preferences fall back to the configured defaults, never to a compiled value', () => {
  const defaults = { word_wrap: false, highlight_words: true, group_by_folder: true, hide_whitespace: true };
  assert.deepEqual(normalizeDiffPrefs(null, defaults), { tree: false, group: true, wrap: false, words: true, whitespace: true, side: false });
  assert.deepEqual(normalizeDiffPrefs({ wrap: true, words: 'yes', tree: true }, defaults), { tree: true, group: true, wrap: true, words: true, whitespace: true, side: false });
  assert.deepEqual(normalizeDiffPrefs({}, {}), { tree: false, group: false, wrap: false, words: false, whitespace: false, side: false });
});

test('session edits are unanchored blocks: counts come from the loaded bodies and pending edits count nothing', () => {
  const model = normalizeDiffModel({ files: [{ newPath: 'a.go', edits: [
    { kind: 'replacement', state: 'loaded', before: 'x\ny\n', after: 'z\n', resultId: 4, ordinal: 0 },
    { kind: 'content', state: 'pending', afterBytes: 12 },
    { kind: 'nonsense', state: 'weird' },
    { kind: 'diff', state: 'loaded', hunks: [{ lines: [{ kind: 'add', newLine: 3, text: 'q' }, { kind: 'delete', oldLine: 3, text: 'p' }] }] },
  ] }] });
  const file = model.files[0];
  assert.deepEqual(countChanges(file), { added: 2, removed: 3 });
  assert.equal(file.edits[3].hunks.length, 1);
  assert.equal(file.edits[1].after, null);
  assert.equal(file.edits[2].kind, 'replacement');
  assert.equal(file.edits[2].state, 'pending');
  assert.equal(model.source.kind, 'retained');
  assert.equal(normalizeDiffModel({ source: { kind: 'session' } }).source.label, 'This session');
});

test('viewer rules: unmodified runs, side-by-side pairing, whitespace folding, and word marks only on pairs', () => {
  const first = { lines: [{ kind: 'context', oldLine: 10, newLine: 10 }, { kind: 'add', oldLine: null, newLine: 11 }] };
  const second = { lines: [{ kind: 'delete', oldLine: 20, newLine: null }, { kind: 'add', oldLine: null, newLine: 21 }] };
  assert.equal(unmodifiedBefore(undefined, first), 9);
  assert.equal(unmodifiedBefore(first, second), 9);
  assert.equal(unmodifiedBefore(first, { lines: [{ kind: 'add', oldLine: null, newLine: 12 }] }), 0);
  const pairs = pairLines([{ kind: 'delete', text: 'a' }, { kind: 'delete', text: 'b' }, { kind: 'add', text: 'c' }, { kind: 'context', text: 'd' }]);
  assert.deepEqual(pairs.map(pair => [pair.old?.text ?? null, pair.new?.text ?? null]), [['a', 'c'], ['b', null], ['d', 'd']]);
  const folded = foldWhitespace([{ kind: 'delete', oldLine: 1, text: 'a  b' }, { kind: 'add', newLine: 1, text: 'a b' }, { kind: 'add', newLine: 2, text: 'new' }]);
  assert.deepEqual(folded.map(line => line.kind), ['context', 'add']);
  assert.equal(folded[0].text, 'a b');
  const tokens = changedTokens('const a = 1;', 'const b = 1;');
  assert.deepEqual(tokens.before.filter(token => token.changed).map(token => token.token), ['a']);
  assert.deepEqual(tokens.after.filter(token => token.changed).map(token => token.token), ['b']);
});

test('the checkout label and dirty counts read like the design, and say nothing when unknown', () => {
  assert.equal(checkoutLabel({ root: '/x/sample-project/', branch: 'feat/panes-host' }), 'sample-project · feat/panes-host');
  assert.equal(checkoutLabel({ root: '/x/repo', head: 'abcdef0123456' }), 'repo · abcdef0');
  assert.equal(checkoutLabel(null), '');
  assert.equal(dirtyLabel({ dirty: true, unstaged: 3, untracked: 1 }), '3 modified · 1 untracked');
  assert.equal(dirtyLabel({ staged: 1, conflicted: 2 }), '1 staged · 2 conflicted');
  assert.equal(dirtyLabel({}), '');
});
