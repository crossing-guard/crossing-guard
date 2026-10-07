import test from 'node:test';
import assert from 'node:assert/strict';

// Journey C-2: the Handoffs group stopped being read once the person left Sessions —
// the refresh returned before scheduling the next one when no rail was mounted — and
// stayed stale afterwards. With no rail on the page the lists are still read on the
// cadence, and a rail mounted later is painted from the last read at once.
test('the handoff lists are read on the cadence with no rail mounted, and a rail mounted later is painted at once', async t => {
  t.mock.timers.enable({ apis: ['setTimeout', 'Date'] });
  const reads = [];
  const hosts = [];
  const node = () => ({ children: [], className: '', dataset: {}, classList: { toggle() {}, add() {}, remove() {} },
    append(...items) { this.children.push(...items); }, appendChild(item) { this.children.push(item); return item; },
    replaceChildren(...items) { this.children = items; }, setAttribute() {} });
  globalThis.document = { visibilityState: 'visible', createElement: node, addEventListener() {}, dispatchEvent() {},
    querySelectorAll: selector => (selector === '.handoff-rail' ? hosts : []) };
  globalThis.localStorage = { getItem: () => '' };
  globalThis.fetch = async url => {
    reads.push(String(url));
    return { ok: true, status: 200, headers: { get: () => 'application/json' },
      json: async () => ({ received: [{ id: 'hnd_1', state: 'received', title: 'Carry on', created_at: '2026-10-04T10:00:00Z' }], sent: [] }),
      text: async () => '' };
  };
  const rail = await import('./handoff-rail.js');
  rail.configureHandoffRail({ countRefreshMs: 1000 });
  const settle = async () => { for (let i = 0; i < 10; i++) await Promise.resolve(); };
  for (let tick = 0; tick < 3; tick++) { t.mock.timers.tick(1000); await settle(); }
  const listReads = reads.filter(url => url.includes('/team/handoffs'));
  assert.ok(listReads.length >= 3, 'the lists were read ' + listReads.length + ' times in three cadences with no rail mounted');

  const side = node();
  const host = rail.mountHandoffRail(side);
  assert.ok(host.children.length > 0, 'a rail mounted after a read is painted before its own read returns');
});

// Journey R-1: the cadence came from one read made as the page loaded. On a page
// opened from its #t=<token> link that read was refused, nothing said so, and the
// group was never read again until the page was reloaded. The cadence is asked for
// again with every read of the group until one answers.
test('a cadence read that fails is asked again with the next read of the group, and the cadence then runs', async t => {
  t.mock.timers.enable({ apis: ['setTimeout', 'Date'] });
  const reads = [];
  const hosts = [];
  const node = () => ({ children: [], className: '', dataset: {}, classList: { toggle() {}, add() {}, remove() {} },
    append(...items) { this.children.push(...items); }, appendChild(item) { this.children.push(item); return item; },
    replaceChildren(...items) { this.children = items; }, setAttribute() {} });
  globalThis.document = { visibilityState: 'visible', createElement: node, addEventListener() {}, dispatchEvent() {},
    querySelectorAll: selector => (selector === '.handoff-rail' ? hosts : []) };
  globalThis.localStorage = { getItem: () => '' };
  globalThis.fetch = async url => {
    reads.push(String(url));
    return { ok: true, status: 200, headers: { get: () => 'application/json' }, json: async () => ({ received: [], sent: [] }), text: async () => '' };
  };
  const warnings = [];
  t.mock.method(console, 'warn', (...words) => warnings.push(words.join(' ')));
  const settle = async () => { for (let i = 0; i < 10; i++) await Promise.resolve(); };
  const listReads = () => reads.filter(url => url.includes('/team/handoffs')).length;

  const rail = await import('./handoff-rail.js?cadence-retry');
  let asked = 0;
  rail.configureHandoffRail({ readSettings: async () => {
    asked++;
    if (asked === 1) { const refused = new Error('unauthorized'); refused.status = 401; throw refused; }
    return { countRefreshMs: 1000 };
  } });
  await settle();
  assert.equal(asked, 1);
  assert.equal(warnings.length, 1, 'the failed read is said, not swallowed');
  t.mock.timers.tick(5000); await settle();
  assert.equal(listReads(), 0, 'with no cadence nothing is scheduled');

  rail.mountHandoffRail(node()); // the rail is drawn: the group is read, and the cadence asked for again
  await settle();
  assert.equal(asked, 2);
  assert.equal(listReads(), 1);
  for (let tick = 0; tick < 3; tick++) { t.mock.timers.tick(1000); await settle(); }
  assert.ok(listReads() >= 4, 'the group was read ' + listReads() + ' times: once on mount, then on the cadence');
  assert.equal(asked, 2, 'a cadence that answered is not asked for again');
});
