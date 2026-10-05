import assert from 'node:assert/strict';
import test from 'node:test';
import { loadChatModels } from './chat-capabilities.js';

globalThis.localStorage = { getItem: () => null };
const response = raw => ({ ok: true, json: async () => raw });
const fresh = id => ({ state: 'fresh', models: [{ id }] });
const deferred = () => { let resolve; const promise = new Promise(done => { resolve = done; }); return { promise, resolve }; };

test('fresh catalog is shared only while in flight, so reopened views see new models', async () => {
  const wait = deferred(); let calls = 0;
  globalThis.fetch = () => { calls++; return wait.promise; };
  const first = loadChatModels('native');
  assert.equal(loadChatModels('native'), first, 'Settings and inline-agent reads share discovery');
  wait.resolve(response(fresh('first')));
  assert.equal((await first).models[0].id, 'first');
  globalThis.fetch = async () => { calls++; return response(fresh('new')); };
  assert.equal((await loadChatModels('native')).models[0].id, 'new');
  assert.equal(calls, 2);
});

test('ordinary stale read joins background refresh exactly once and retains normalized contract', async () => {
  const requests = [];
  globalThis.fetch = async (url, options) => {
    requests.push([url, options.method || 'GET']);
    return response(requests.length === 1 ? { state: 'stale', models: [] } : { state: 'fresh', models: [{ id: 'opaque:/high', thinking_effort: { state: 'supported', choices: [{ id: 'native-x', label: 'Extra' }] } }] });
  };
  const list = await loadChatModels('native');
  assert.equal(list.models[0].effort.choices[0].id, 'native-x');
  assert.deepEqual(requests, [['/api/v1/chat/models?runtime=native', 'GET'], ['/api/v1/chat/models/refresh?runtime=native', 'POST']]);
  globalThis.fetch = async () => response({ state: 'stale', reason_code: 'timeout', models: [] });
  assert.equal((await loadChatModels('native')).reasonCode, 'timeout');
});

test('failure remains retryable without a loop or rejection for adjacent consumers', async () => {
  let calls = 0;
  globalThis.fetch = async () => { calls++; throw new Error('offline'); };
  const failed = await loadChatModels('native');
  assert.equal(failed.state, 'unavailable'); assert.equal(failed.reasonCode, 'request-failed'); assert.equal(calls, 1);
  globalThis.fetch = async (url, options) => { assert.equal(options.method, 'POST'); return response(fresh('recovered')); };
  assert.equal((await loadChatModels('native', { refresh: true })).models[0].id, 'recovered');
});

test('older GET completion never evicts a newer forced refresh', async () => {
  const old = deferred(), newer = deferred(); let calls = 0;
  globalThis.fetch = () => (++calls === 1 ? old.promise : newer.promise);
  const read = loadChatModels('native');
  const forced = loadChatModels('native', { refresh: true });
  old.resolve(response(fresh('old'))); await read;
  assert.equal(loadChatModels('native'), forced);
  assert.equal(loadChatModels('native', { refresh: true }), forced);
  newer.resolve(response(fresh('new')));
  assert.equal((await forced).models[0].id, 'new'); assert.equal(calls, 2);
});

test('overtaken stale GET joins the newer POST without recursion or a third request', async () => {
  const old = deferred(), newer = deferred(); let calls = 0;
  globalThis.fetch = () => (++calls === 1 ? old.promise : newer.promise);
  const read = loadChatModels('native'); const forced = loadChatModels('native', { refresh: true });
  old.resolve(response({ state: 'stale', models: [] })); newer.resolve(response(fresh('new')));
  assert.equal((await read).models[0].id, 'new'); await forced; assert.equal(calls, 2);
});
