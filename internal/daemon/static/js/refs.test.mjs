import assert from 'node:assert/strict';

globalThis.localStorage = { getItem: () => '' };

let releaseSlow;
const slow = new Promise(resolve => { releaseSlow = resolve; });
globalThis.fetch = async raw => {
  const url = new URL(String(raw), 'http://crossing-guard.test');
  const root = url.searchParams.get('cwd');
  if (root === '/slow') await slow;
  const paths = root === '/alpha' ? ['src/alpha.js']
    : root === '/beta' ? ['src/beta.js']
      : ['src/slow.js'];
  return { ok: true, json: async () => ({ paths, ids: {}, docs: [], root }) };
};

const refs = await import('./refs.js');

const alpha = refs.forProject('/alpha');
const beta = refs.forProject('/beta');
await Promise.all([alpha.start(), beta.start()]);

assert.equal(refs.projectRoot(), null, 'preparing a project must not mutate committed root');
assert.equal(alpha.resolve('src/alpha.js').state, 'resolved');
assert.equal(beta.resolve('src/beta.js').state, 'resolved');
assert.equal(alpha.resolve('src/beta.js').state, 'missing');

alpha.activate();
assert.equal(refs.projectRoot(), '/alpha');
assert.equal(refs.resolve('src/alpha.js').state, 'resolved');

const slowScope = refs.forProject('/slow');
const slowLoad = slowScope.start();
assert.equal(slowScope.resolve('src/slow.js').state, 'indexing');
assert.equal(refs.projectRoot(), '/alpha', 'a pending project must not replace committed root');
releaseSlow();
await slowLoad;
assert.equal(slowScope.resolve('src/slow.js').state, 'resolved');
assert.equal(refs.projectRoot(), '/alpha', 'a completed stale load must not replace committed root');

console.log('refs: all pass');
