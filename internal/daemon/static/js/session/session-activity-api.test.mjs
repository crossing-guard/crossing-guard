import test from 'node:test';
import assert from 'node:assert/strict';

globalThis.localStorage = { getItem: () => 'tok' };
const { fetchSessionActivitySnapshot } = await import('./session-activity-api.js');

// The event stream client skips an opening snapshot only on HTTP 503, so the
// thrown error must carry the status (degraded-surfaces-state-the-reason plan §2.2).
test('a failed activity snapshot throws with the HTTP status and the daemon text', async () => {
  globalThis.fetch = async () => new Response('native session activity unavailable', { status: 503 });
  await assert.rejects(fetchSessionActivitySnapshot(), error => {
    assert.equal(error.status, 503);
    assert.match(error.message, /native session activity unavailable/);
    return true;
  });
});
