import test from 'node:test';
import assert from 'node:assert/strict';

const { SessionLiveClient, attachLiveTransport } = await import('./session-live-client.js');

function fakeTransport() {
  const calls = [];
  return { calls, setSubject: (runtime, id, live) => calls.push({ runtime, id, live }) };
}

test('subscribe hands the subject to the tab transport instead of opening a stream', () => {
  const transport = fakeTransport();
  attachLiveTransport(transport);
  const client = new SessionLiveClient();
  client.subscribe('claude', 'ses-1', { onStatus: () => {} });
  assert.equal(transport.calls.length, 1, 'exactly one subject change per subscribe (no close-then-subscribe double reconnect)');
  assert.equal(transport.calls[0].runtime, 'claude');
  assert.equal(transport.calls[0].id, 'ses-1');
  assert.equal(transport.calls[0].live, client, 'the client itself is the frame handler');
  client.close();
  assert.deepEqual(transport.calls[1], { runtime: '', id: '', live: null });
});

test('frames dispatch to the handlers and track the last seq', () => {
  attachLiveTransport(fakeTransport());
  const client = new SessionLiveClient();
  const statuses = [], events = [], states = [], identities = [], governance = [];
  client.subscribe('claude', 'ses-1', {
    onStatus: (status, detail) => statuses.push([status, detail]),
    onEvents: list => events.push(list), onTurnState: state => states.push(state),
    onIdentity: payload => identities.push(payload), onGovernance: actions => governance.push(actions),
  });
  client.handle('snapshot', { events: [{ seq: 7 }], age_tick_seconds: 3 });
  assert.equal(client.lastSeq, 7);
  assert.equal(statuses[0][0], 'live');
  client.handle('events', { events: [{ seq: 8 }, { seq: 9 }] });
  assert.equal(client.lastSeq, 9);
  assert.deepEqual(events[0].map(e => e.seq), [8, 9]);
  client.handle('state', { execution: 'running' });
  assert.deepEqual(states, [{ execution: 'running' }]);
  client.handle('governance', { actions: [{ event_id: 1 }] });
  assert.deepEqual(governance, [[{ event_id: 1 }]]);
  client.handle('identity', { following: false });
  assert.deepEqual(identities, [{ following: false }]);
  client.handle('unavailable', { error: 'session unavailable' });
  assert.equal(statuses.at(-1)[0], 'unavailable');
  client.drop();
  assert.deepEqual(statuses.at(-1), ['unavailable', { retrying: true }]);
});

test('an empty subject clears the transport subject', () => {
  const transport = fakeTransport();
  attachLiveTransport(transport);
  new SessionLiveClient().subscribe('', '', {});
  assert.deepEqual(transport.calls[0], { runtime: '', id: '', live: null });
});
