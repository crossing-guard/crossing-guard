import assert from 'node:assert/strict';
import test from 'node:test';

import { capturedTextVisibility } from './diff-viewer.js';

const model = (files, rawFallback) => ({ files, rawFallback });
const withHunks = count => ({ hunks: Array.from({ length: count }, () => ({ lines: [] })) });

test('when nothing could be rendered, the captured text is the answer and is shown', () => {
  assert.equal(capturedTextVisibility(model([], 'captured text')), 'shown');
  assert.equal(capturedTextVisibility(model([{ hunks: [] }], 'captured text')), 'shown');
});

test('when a diff rendered, the captured text is a second copy and stays behind a disclosure', () => {
  assert.equal(capturedTextVisibility(model([withHunks(1)], 'captured text')), 'collapsed');
  assert.equal(capturedTextVisibility(model([withHunks(2), { hunks: [] }], 'captured text')), 'collapsed');
});

test('with no captured text there is nothing to reveal', () => {
  assert.equal(capturedTextVisibility(model([withHunks(1)], '')), 'none');
  assert.equal(capturedTextVisibility(model([], '')), 'none');
  assert.equal(capturedTextVisibility(undefined), 'none');
});
