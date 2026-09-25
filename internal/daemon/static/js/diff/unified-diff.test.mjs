import assert from 'node:assert/strict';
import test from 'node:test';

import { parseUnifiedDiff } from './unified-diff.js';

test('parses retained multi-file text and preserves old/new line numbers', () => {
  const model = parseUnifiedDiff([
    'diff --git a/src/app.js b/src/app.js',
    'index 1111111..2222222 100644',
    '--- a/src/app.js',
    '+++ b/src/app.js',
    '@@ -2,3 +2,4 @@ function run() {',
    ' keep();',
    '-oldValue();',
    '+newValue();',
    '+more();',
    '--- looks like a file header but is deleted content',
    '+++ looks like a file header but is added content',
    ' tail();',
    'diff --git "a/docs/a b.md" "b/docs/a b.md"',
    '--- "a/docs/a b.md"',
    '+++ "b/docs/a b.md"',
    '@@ -1 +1 @@',
    '-old',
    '+new',
    '\\ No newline at end of file',
  ].join('\n'));

  assert.equal(model.source.kind, 'retained');
  assert.equal(model.files.length, 2);
  assert.equal(model.files[0].navigationPath, 'src/app.js');
  assert.deepEqual(model.files[0].hunks[0].lines.slice(0, 4).map(line => [line.kind, line.oldLine, line.newLine]), [
    ['context', 2, 2],
    ['delete', 3, null],
    ['add', null, 3],
    ['add', null, 4],
  ]);
  assert.equal(model.files[1].newPath, 'docs/a b.md');
  assert.equal(model.files[1].hunks[0].lines.at(-1).kind, 'meta');
  assert.deepEqual(model.parseGaps, []);
});

test('parses a plain unified diff without a git boundary and ignores final newline', () => {
  const model = parseUnifiedDiff('--- a/plain.txt\n+++ b/plain.txt\n@@ -1 +1 @@\n-old\n+new\n');
  assert.equal(model.files.length, 1);
  assert.equal(model.files[0].newPath, 'plain.txt');
  assert.deepEqual(model.parseGaps, []);
});

test('decodes Git UTF-8 octal path quoting for safe navigation', () => {
  const model = parseUnifiedDiff([
    'diff --git "a/\\346\\226\\207.txt" "b/\\346\\226\\207.txt"',
    '--- "a/\\346\\226\\207.txt"',
    '+++ "b/\\346\\226\\207.txt"',
    '@@ -1 +1 @@',
    '-old',
    '+new',
  ].join('\n'));
  assert.equal(model.files[0].newPath, '文.txt');
  assert.equal(model.files[0].navigationPath, '文.txt');
});

test('reports binary summaries without inventing hunks', () => {
  const model = parseUnifiedDiff([
    'diff --git a/image.png b/image.png',
    'new file mode 100644',
    'index 0000000..1111111',
    'Binary files /dev/null and b/image.png differ',
  ].join('\n'));
  assert.equal(model.files[0].kind, 'binary');
  assert.equal(model.files[0].hunks.length, 0);
  assert.match(model.files[0].summary, /Binary files/);
});

test('malformed retained text stays visibly available as raw fallback', () => {
  const raw = 'provider emitted something that is not a patch';
  const model = parseUnifiedDiff(raw);
  assert.equal(model.files.length, 0);
  assert.equal(model.rawFallback, raw);
  assert.ok(model.parseGaps.length >= 1);
});

// A runtime that reports one file per change has no reason to repeat the path
// inside the text. Codex does exactly that, and until this was handled every
// such body rendered as nothing at all.
const HEADERLESS = [
  '@@ -139,2 +139,15 @@',
  ' ',
  '+### F14 - a finding',
  '+',
  ' ## Final judgment',
].join('\n');

test('a headerless body takes its path from the caller and renders as one file', () => {
  const model = parseUnifiedDiff(HEADERLESS, {}, { path: 'docs/design/plan.md' });
  assert.equal(model.files.length, 1);
  assert.equal(model.files[0].displayPath, 'docs/design/plan.md');
  assert.equal(model.files[0].navigationPath, 'docs/design/plan.md');
  assert.equal(model.files[0].hunks.length, 1);
  assert.equal(model.parseGaps.length, 0);
  assert.equal(model.rawFallback, '');
  const added = model.files[0].hunks[0].lines.filter(line => line.kind === 'add');
  assert.equal(added.length, 2);
  assert.equal(added[0].newLine, 140);
});

test('the same body without a caller path still renders its captured text', () => {
  const model = parseUnifiedDiff(HEADERLESS);
  assert.equal(model.files.length, 0);
  assert.equal(model.rawFallback, HEADERLESS);
});

// The failure this test exists for: seeding a file before parsing would send
// every line of a non-diff body into that file's metadata, join them with
// separators, and leave the captured text empty — showing the reader less than
// before the change.
test('a caller path never swallows a body that is not a diff', () => {
  const raw = 'provider emitted something that is not a patch\nsecond line\n';
  const model = parseUnifiedDiff(raw, {}, { path: 'docs/design/plan.md' });
  assert.equal(model.files.length, 0);
  assert.equal(model.rawFallback, raw);
});

test('a body with its own headers ignores the caller path', () => {
  const model = parseUnifiedDiff([
    'diff --git a/real/path.js b/real/path.js',
    '@@ -1,1 +1,2 @@',
    ' keep();',
    '+added();',
  ].join('\n'), {}, { path: 'wrong/path.js' });
  assert.equal(model.files.length, 1);
  assert.equal(model.files[0].navigationPath, 'real/path.js');
});

test('a deleted file renders its diff but offers no navigation', () => {
  const model = parseUnifiedDiff(HEADERLESS, {}, { path: 'docs/design/plan.md', newPath: '/dev/null' });
  assert.equal(model.files.length, 1);
  assert.equal(model.files[0].displayPath, 'docs/design/plan.md');
  assert.equal(model.files[0].navigationPath, '');
});

test('a moved file renders its diff but offers no navigation, because the new path was not retained', () => {
  const model = parseUnifiedDiff(HEADERLESS, {}, { path: 'docs/design/plan.md', newPath: '' });
  assert.equal(model.files.length, 1);
  assert.equal(model.files[0].navigationPath, '');
});

test('a caller path is refused when the body does not open on a hunk', () => {
  const raw = 'Some prose.\n@@ -1,1 +1,2 @@\n keep();\n+added();\n';
  const model = parseUnifiedDiff(raw, {}, { path: 'docs/design/plan.md' });
  assert.equal(model.files.length, 0);
  assert.equal(model.rawFallback, raw);
});

test('an unreadable body reports one fact, not one per line', () => {
  const raw = Array.from({ length: 2000 }, (_, index) => `line ${index}`).join('\n');
  const model = parseUnifiedDiff(raw);
  assert.equal(model.files.length, 0);
  assert.equal(model.parseGaps.length, 2);
  assert.equal(model.rawFallback, raw);
});
