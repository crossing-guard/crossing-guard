import assert from 'node:assert/strict';
import test from 'node:test';

import { lineNavigation, navigableDiffPath, normalizeDiffModel } from './diff-model.js';

test('navigation accepts only safe relative new-file paths and visible new lines', () => {
  assert.equal(navigableDiffPath('src/app.js'), 'src/app.js');
  for (const unsafe of ['', '/dev/null', '/etc/passwd', '../secret', 'a/../../secret', 'a\0b']) {
    assert.equal(navigableDiffPath(unsafe), '');
  }
  const file = { newPath: 'src/app.js' };
  assert.deepEqual(lineNavigation(file, { kind: 'add', newLine: 12 }), { path: 'src/app.js', line: 12 });
  assert.deepEqual(lineNavigation(file, { kind: 'context', newLine: 4 }), { path: 'src/app.js', line: 4 });
  assert.equal(lineNavigation(file, { kind: 'delete', oldLine: 4 }), null);
  assert.equal(lineNavigation({ newPath: '../bad' }, { kind: 'add', newLine: 1 }), null);
});

test('normalization strips malformed presentation authority', () => {
  const model = normalizeDiffModel({
    source: { kind: 'anything', label: 5 },
    files: [{ newPath: '../bad', kind: 'invented', hunks: [{ lines: [{ kind: 'add', newLine: -1, text: 9 }] }] }],
    parseGaps: ['gap', 7],
  });
  assert.equal(model.source.kind, 'retained');
  assert.equal(model.files[0].navigationPath, '');
  assert.equal(model.files[0].kind, 'unknown');
  assert.equal(model.files[0].hunks[0].lines[0].newLine, null);
  assert.equal(model.files[0].hunks[0].lines[0].text, '');
  assert.deepEqual(model.parseGaps, ['gap']);
});
