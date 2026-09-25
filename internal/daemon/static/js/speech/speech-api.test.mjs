import assert from 'node:assert/strict';
import { test } from 'node:test';

globalThis.localStorage ??= { getItem: () => 'token-123', setItem() {}, removeItem() {} };
const api = await import('./speech-api.js');

function fakeFetch(handler) {
  const calls = [];
  const impl = async (path, options = {}) => {
    calls.push({ path, options });
    const result = await handler(path, options);
    return result;
  };
  return { impl, calls };
}

const jsonResponse = (body, status = 200) => ({ ok: status < 400, status, json: async () => body, text: async () => JSON.stringify(body) });

test('every call uses the canonical /api/v1 surface and carries the console token', async () => {
  const { impl, calls } = fakeFetch(async () => jsonResponse({ id: 'dict_1', sample_rate: 16000 }, 201));
  const created = await api.createDictation({ cwd: '/tmp/project' }, impl);
  assert.equal(created.id, 'dict_1');
  assert.equal(calls[0].path, '/api/v1/speech/dictations');
  assert.equal(calls[0].options.headers['X-CG-Token'], 'token-123');
  await api.sendFrame('dict_1', 3, 6000, new Uint8Array(4), impl);
  assert.equal(calls[1].path, '/api/v1/speech/dictations/dict_1/frames?seq=3&offset=6000');
  assert.equal(calls[1].options.headers['Content-Type'], 'application/octet-stream');
  await api.finishDictation('dict_1', 'release', impl);
  assert.equal(JSON.parse(calls[2].options.body).reason, 'release');
  await api.cancelDictation('dict_1', impl);
  assert.equal(calls[3].options.method, 'DELETE');
  await api.confirmDisclosure('openai-batch', 1, impl);
  assert.equal(calls[4].path, '/api/v1/speech-disclosures');
  assert.deepEqual(JSON.parse(calls[4].options.body), { backend_id: 'openai-batch', operation: 'transcription', disclosure_version: 1 });
});

test('structured daemon errors keep their code and message', async () => {
  const { impl } = fakeFetch(async () => jsonResponse({ code: 'busy', message: 'finish the current dictation first' }, 409));
  await assert.rejects(api.createDictation({}, impl), error => error.code === 'busy' && error.status === 409 && /finish the current/.test(error.message));
});

test('the dictation stream delivers parsed named events until the daemon closes it', async () => {
  const encoder = new TextEncoder();
  const frames = ['event: partial\ndata: {"kind":"partial","committed":"rebase the","tail":"branch"}\n\n',
    'event: ended\ndata: {"kind":"ended","state":"final","text":"rebase the branch"}\n\n'];
  let index = 0;
  const body = { getReader: () => ({
    read: async () => index < frames.length ? { done: false, value: encoder.encode(frames[index++]) } : { done: true, value: undefined },
    cancel: async () => {},
  }) };
  const { impl } = fakeFetch(async () => ({ ok: true, status: 200, body }));
  const seen = [];
  await api.openDictationStream('dict_1', event => seen.push(event), undefined, impl);
  assert.deepEqual(seen.map(event => event.kind), ['partial', 'ended']);
  assert.equal(seen[1].text, 'rebase the branch');
});
