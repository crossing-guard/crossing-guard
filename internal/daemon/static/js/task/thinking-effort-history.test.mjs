import test from 'node:test';
import assert from 'node:assert/strict';
import { taskForEffortAnchor } from './thinking-effort-history.js';
const session = { runtime: 'one', id: 'catalog', resume_id: 'native' };
const task = { id: 't', session_runtime: 'one', catalog_session_id: 'catalog', native_session_id: 'native', requested_settings: {}, events: [{ payload: { anchor: 'a' } }] };
test('historical anchors require exact runtime and catalog scope', () => {
  assert.equal(taskForEffortAnchor([task], 'a', session), task);
  assert.equal(taskForEffortAnchor([task], 'a', { ...session, runtime: 'two' }), null);
  assert.equal(taskForEffortAnchor([task], 'a', { ...session, id: 'other' }), null);
  assert.equal(taskForEffortAnchor([task, { ...task, id: 'duplicate' }], 'a', session), null);
  assert.equal(taskForEffortAnchor([task], 'a'), null);
});
test('uncataloged tasks are only eligible when the catalog identity is native', () => {
  const uncataloged = { ...task, catalog_session_id: '' };
  assert.equal(taskForEffortAnchor([uncataloged], 'a', session), null);
  assert.equal(taskForEffortAnchor([uncataloged], 'a', { ...session, id: 'native' }), uncataloged);
});
