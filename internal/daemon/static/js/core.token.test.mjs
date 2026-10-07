import test from 'node:test';
import assert from 'node:assert/strict';

// Journey R-1: on a page opened from its #t=<token> link, the modules that read
// the API as they load (the rail's settings, the keymap) sent those reads with no
// token and were refused: the token was taken from the fragment by the entry
// module's body, which runs after every module it imports. core.js, which each of
// them imports, takes it while it is evaluated.
test('the token in the link is stored when core.js loads, so a read made by a module as it loads carries it', async () => {
  const stored = new Map();
  globalThis.localStorage = { getItem: key => stored.get(key) ?? null, setItem: (key, value) => stored.set(key, String(value)) };
  globalThis.location = { hash: '#t=tok_from-link1' };
  const sent = [];
  globalThis.fetch = async (url, options) => {
    sent.push(options.headers['X-CG-Token']);
    return { ok: true, status: 200, json: async () => ({}) };
  };
  const core = await import('./core.js?token-at-load');
  assert.equal(stored.get('cg_token'), 'tok_from-link1', 'stored by the time the module has loaded');
  await core.api('/api/console/config');
  assert.deepEqual(sent, ['tok_from-link1']);
  delete globalThis.location;
});

test('a fragment with no token stores nothing, and a destination beside the token does not hide it', async () => {
  const stored = new Map();
  globalThis.localStorage = { getItem: key => stored.get(key) ?? null, setItem: (key, value) => stored.set(key, String(value)) };
  const { storeTokenFromHash } = await import('./core.js?token-forms');
  assert.equal(storeTokenFromHash('#tab=capture&session=abc'), false);
  assert.equal(stored.size, 0);
  assert.equal(storeTokenFromHash('#tab=capture&session=abc&t=tok2'), true);
  assert.equal(stored.get('cg_token'), 'tok2');
});
