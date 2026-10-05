import { test } from 'node:test';
import assert from 'node:assert/strict';
import { diffLines, diffStats } from './text-diff.js';

test('identical text is all same', () => {
  const result = diffLines('a\nb', 'a\nb');
  assert.deepEqual(result.lines.map(line => line.op), ['same', 'same']);
  assert.equal(result.truncated, false);
});

test('one changed line is a del then an add between shared context', () => {
  const result = diffLines('one\ntwo\nthree', 'one\nTWO\nthree');
  assert.deepEqual(result.lines, [
    { op: 'same', text: 'one' }, { op: 'del', text: 'two' }, { op: 'add', text: 'TWO' }, { op: 'same', text: 'three' },
  ]);
  assert.deepEqual(diffStats(result), { added: 1, removed: 1 });
});

test('insertions and deletions in the middle keep the common lines', () => {
  const result = diffLines('a\nb\nc\nd', 'a\nc\nx\nd');
  assert.deepEqual(result.lines.map(line => line.op + ':' + line.text),
    ['same:a', 'del:b', 'same:c', 'add:x', 'same:d']);
});

test('empty inputs and CRLF are handled', () => {
  assert.deepEqual(diffLines('', '').lines, []);
  assert.deepEqual(diffLines('', 'x').lines, [{ op: 'add', text: 'x' }]);
  assert.deepEqual(diffLines('a\r\nb', 'a\nb').lines.map(line => line.op), ['same', 'same']);
});

test('an oversized middle degrades to a block replacement and says so', () => {
  const before = Array.from({ length: 50 }, (_, i) => 'x' + i).join('\n');
  const after = Array.from({ length: 50 }, (_, i) => 'y' + i).join('\n');
  const result = diffLines(before, after, { maxCells: 100 });
  assert.equal(result.truncated, true);
  assert.deepEqual(diffStats(result), { added: 50, removed: 50 });
});
