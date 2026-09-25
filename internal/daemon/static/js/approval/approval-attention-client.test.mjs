import assert from 'node:assert/strict';

const listeners = { document: [], window: [] };
globalThis.document = {
  visibilityState: 'visible',
  hasFocus: () => true,
  addEventListener: (name) => listeners.document.push(name), removeEventListener: () => {},
};
globalThis.window = { addEventListener: (name) => listeners.window.push(name), removeEventListener: () => {} };

const { ApprovalAttentionClient } = await import('./approval-attention-client.js');
const reports = [];
const client = new ApprovalAttentionClient(async payload => { reports.push(payload); });
await client.send();
assert.equal(reports[0].visible, true);
assert.equal(reports[0].focused, true);
assert.match(reports[0].client_id, /^tab_/);
assert.equal(client.clientID, reports[0].client_id, 'the stream attaches under the same id the presence POST uses');

await client.send();
assert.equal(reports.length, 1, 'an unchanged state is not re-sent: the open stream is the lease, there is no heartbeat');

document.visibilityState = 'hidden';
await client.send();
assert.equal(reports[1].visible, false);
assert.equal(reports[1].focused, false);

console.log('approval attention client: PASS');

// No interval: start() sends once and then only visibility/focus events send.
client.start();
assert.equal(client.timer, undefined, 'no periodic timer exists');
assert.ok(listeners.document.includes('visibilitychange'));
assert.ok(listeners.window.includes('focus') && listeners.window.includes('blur'));

// A send while one is outstanding is dropped, so a slow daemon can never hold
// more than one presence connection per tab.
let release;
const pending = [];
const slow = new ApprovalAttentionClient((payload, signal) => {
  pending.push({ payload, signal });
  return new Promise(resolve => { release = resolve; });
});
document.visibilityState = 'visible';
const first = slow.send();
await slow.send();
assert.equal(pending.length, 1, 'a second send while one is in flight must not open another request');
release();
await first;
assert.ok(pending[0].signal, 'presence must pass an abort signal so a stuck send cannot hold a connection');
assert.equal(typeof pending[0].signal.aborted, 'boolean');

// A failed send releases the latch and is retried on the next change.
let failures = 0;
const failing = new ApprovalAttentionClient(async () => { failures++; throw new Error('daemon unreachable'); });
await failing.send();
assert.equal(failing.inFlight, false, 'a failed send must release the latch');
assert.equal(failing.lastSent, null, 'a failure is not remembered as acknowledged');
await failing.send();
assert.equal(failures, 2, 'the next event retries an unacknowledged state');

console.log('approval attention client: change-only presence is bounded: PASS');
