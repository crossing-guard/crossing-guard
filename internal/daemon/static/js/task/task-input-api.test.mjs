import assert from 'node:assert/strict';
import test from 'node:test';

import { listTaskInputs, removeTaskInput, stageTaskInput, TaskInputHTTPError } from './task-input-api.js';

Object.defineProperty(globalThis, 'localStorage', {
  configurable: true, value: { getItem() { return ''; } },
});

test('task input API carries opaque scope only in the private header', async () => {
  const originalFetch = globalThis.fetch;
  const calls = [];
  globalThis.fetch = async (url, options = {}) => {
    calls.push({ url, options });
    return new Response(JSON.stringify({ scope: { id: 'scope_1', inputs: [] }, input: { id: 'input_1' } }),
      { status: 200, headers: { 'Content-Type': 'application/json' } });
  };
  try {
    const file = new File(['hello'], 'note.txt', { type: 'text/plain' });
    await stageTaskInput(file, 'picker', 'scope_secret');
    await listTaskInputs('scope_secret');
    await removeTaskInput('scope_secret', 'input_1');
    assert.equal(calls.length, 3);
    for (const call of calls) {
      assert.equal(call.options.headers['X-CG-Input-Scope'], 'scope_secret');
      assert.doesNotMatch(call.url, /scope_secret/);
    }
    assert.equal(calls[0].options.body.get('source'), 'picker');
  } finally { globalThis.fetch = originalFetch; }
});

test('task input API preserves structured server errors', async () => {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async () => new Response(JSON.stringify({ code: 'animated_image', message: 'animated images are not supported' }),
    { status: 400, headers: { 'Content-Type': 'application/json' } });
  try {
    await assert.rejects(() => listTaskInputs('scope_1'), error =>
      error instanceof TaskInputHTTPError && error.code === 'animated_image' && /animated images/.test(error.message));
  } finally { globalThis.fetch = originalFetch; }
});
