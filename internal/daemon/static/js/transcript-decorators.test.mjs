import test from 'node:test';
import assert from 'node:assert/strict';
import { registerTranscriptDecorator, applyTranscriptDecorators, registeredTranscriptDecorators, clearTranscriptDecorators } from './transcript-decorators.js';

const fakeRow = () => ({ dataset: {} });

test('decorators run in registered order, lower order first', () => {
  clearTranscriptDecorators();
  const calls = [];
  registerTranscriptDecorator(() => calls.push('late'), 20);
  registerTranscriptDecorator(() => calls.push('early'), 10);
  registerTranscriptDecorator(() => calls.push('late-2'), 20);
  applyTranscriptDecorators({ kind: 'assistant' }, fakeRow(), {});
  assert.deepEqual(calls, ['early', 'late', 'late-2']);
  assert.equal(registeredTranscriptDecorators().length, 3);
});

test('a row is decorated at most once', () => {
  clearTranscriptDecorators();
  let count = 0;
  registerTranscriptDecorator(() => count++);
  const row = fakeRow();
  applyTranscriptDecorators({ kind: 'user' }, row, {});
  applyTranscriptDecorators({ kind: 'user' }, row, {}); // merged tool_result path re-touch
  assert.equal(count, 1);
  assert.equal(row.dataset.cgDecorated, '1');
});

test('decorator context and event are passed through', () => {
  clearTranscriptDecorators();
  let seen = null;
  registerTranscriptDecorator((ev, rowEl, ctx) => { seen = { ev, ctx }; });
  const ev = { kind: 'tool_call', text: 'x' };
  applyTranscriptDecorators(ev, fakeRow(), { kind: 'tool_call', projectRoot: '/repo' });
  assert.equal(seen.ev, ev);
  assert.equal(seen.ctx.projectRoot, '/repo');
});

test('a throwing decorator never breaks the chain', () => {
  clearTranscriptDecorators();
  const calls = [];
  registerTranscriptDecorator(() => { throw new Error('boom'); }, 0);
  registerTranscriptDecorator(() => calls.push('survivor'), 1);
  applyTranscriptDecorators({ kind: 'assistant' }, fakeRow(), {});
  assert.deepEqual(calls, ['survivor']);
});

test('missing event or row is a no-op', () => {
  clearTranscriptDecorators();
  let count = 0;
  registerTranscriptDecorator(() => count++);
  applyTranscriptDecorators(null, fakeRow(), {});
  applyTranscriptDecorators({ kind: 'user' }, null, {});
  assert.equal(count, 0);
  clearTranscriptDecorators();
});
