import test from 'node:test';
import assert from 'node:assert/strict';
import { createEffortState } from './thinking-effort-state.js';
const tick = () => new Promise(resolve => setImmediate(resolve));
const high = { kind: 'level', value: 'opaque-high' };
const low = { kind: 'level', value: 'opaque-low' };
const context = { runtime: 'fixture', session_id: 'one', model: 'model' };

test('saved selection blocks send until acknowledged and reload restores it', async () => {
  let saved = { token: '0', thinking_effort: { kind: 'inherit' } }, complete;
  const state = createEffortState({ transport: async (_, value, token) => {
    if (!value) return saved;
    assert.equal(token, saved.token);
    await new Promise(resolve => { complete = resolve; });
    saved = { token: '1', thinking_effort: value }; return saved;
  } });
  state.setContext(context); await tick();
  assert.deepEqual(state.request(), { thinking_effort: { kind: 'inherit' }, session_effort_token: '0' });
  const writing = state.choose(high);
  assert.throws(() => state.request(), /Wait/);
  complete(); await writing;
  assert.deepEqual(state.request(), { thinking_effort: high, session_effort_token: '1' });
  await state.reload(); assert.deepEqual(state.snapshot().selection, high);
});

test('late reads cannot overwrite another session or model', async () => {
  const pending = [];
  const state = createEffortState({ transport: ctx => new Promise(resolve => pending.push({ ctx, resolve })) });
  state.setContext(context); state.setContext({ ...context, session_id: 'two' });
  pending[1].resolve({ token: '2', thinking_effort: low }); await tick();
  pending[0].resolve({ token: '1', thinking_effort: high }); await tick();
  assert.deepEqual(state.request(), { thinking_effort: low, session_effort_token: '2' });
});

test('stale save retains pending choice and blocks until explicit reload', async () => {
  const state = createEffortState({ transport: async (_, value) => {
    if (value) throw new Error('Changed in another tab');
    return { token: '2', thinking_effort: low };
  } });
  state.setContext(context); await tick(); await state.choose(high);
  assert.deepEqual(state.snapshot().selection, high);
  assert.throws(() => state.request(), /another tab/);
  await state.reload(); assert.deepEqual(state.request().thinking_effort, low);
});

test('new chat retains draft until canonical identity exists, then saves it', async () => {
  let known = false, saved = false;
  const state = createEffortState({ transport: async (_, value) => {
    if (!known) throw Object.assign(new Error('Not harvested'), { code: 'identity_pending' });
    if (value) { saved = true; return { token: '1', thinking_effort: value }; }
    return { token: '0', thinking_effort: { kind: 'inherit' } };
  } });
  state.setContext({ ...context, session_id: '' }); await state.choose(high);
  state.setContext(context); await tick();
  assert.equal(state.snapshot().pendingIdentity, true);
  assert.deepEqual(state.request(), { thinking_effort: high });
  known = true; await state.reload();
  assert.equal(saved, true); assert.equal(state.request().session_effort_token, '1');
});

test('legacy extra args keep omission only when no saved explicit selection exists', async () => {
  const state = createEffortState({ transport: async () => ({ token: '0', thinking_effort: { kind: 'inherit' } }) });
  state.setContext(context); await tick();
  assert.deepEqual(state.request({ legacy: true }), {});
  assert.equal(state.request().session_effort_token, '0');
});

test('late refusal cannot poison a different model context', async () => {
  const state = createEffortState({ transport: async () => ({ token: '0', thinking_effort: { kind: 'inherit' } }) });
  state.setContext(context); await tick();
  const epoch = state.snapshot().generation;
  state.setContext({ ...context, model: 'second' }); await tick();
  assert.equal(state.refuse(new Error('stale old request'), epoch), false);
  assert.doesNotThrow(() => state.request());
});

test('pending identity adoption never carries an effort into another model', async () => {
  const state = createEffortState({ transport: async ctx => {
    if (ctx.model === 'model') throw Object.assign(new Error('pending'), { code: 'identity_pending' });
    return { token: '0', thinking_effort: { kind: 'inherit' } };
  } });
  state.setContext({ ...context, session_id: '' }); await state.choose(high);
  state.setContext(context); await tick();
  state.setContext({ ...context, model: 'second' }); await tick();
  assert.equal(state.snapshot().selection, null);
  assert.equal(state.snapshot().pendingIdentity, false);
});

test('late refusal cannot poison a later effort selection in the same context', async () => {
  for (const session_id of ['', 'one']) {
    const state = createEffortState({ transport: async (_, value) => ({ token: value ? '2' : '1', thinking_effort: value || high }) });
    state.setContext({ ...context, session_id }); await tick(); await state.choose(high);
    const epoch = state.snapshot().generation;
    await state.choose(low);
    assert.equal(state.refuse(new Error('stale prior send'), epoch), false);
    assert.deepEqual(state.request().thinking_effort, low);
  }
});

test('delayed new-session identity automatically saves without a second send', async () => {
  let known = false, callback, saved;
  const state = createEffortState({ schedule: fn => { callback = fn; return 1; }, cancel: () => { callback = null; }, transport: async (_, value) => {
    if (!known) throw Object.assign(new Error('pending'), { code: 'identity_pending' });
    if (value) saved = value;
    return { token: value ? '1' : '0', thinking_effort: value || { kind: 'inherit' } };
  } });
  state.setContext({ ...context, session_id: '' }); await state.choose(high);
  state.setContext(context); await tick();
  assert.equal(state.snapshot().pendingIdentity, true);
  known = true; callback(); await tick();
  assert.deepEqual(saved, high); assert.equal(state.request().session_effort_token, '1');
  assert.equal(state.snapshot().pendingIdentity, false);
});
