import test from 'node:test';
import assert from 'node:assert/strict';

// Journey W-5: every "Open again…" started the composer at the first mode and the
// default model. The mode and model the last Open in a runtime was sent with are kept
// for the life of the page, per runtime.
test('an Open starts from the mode and model the last Open in that runtime was sent with', async () => {
  globalThis.document = { addEventListener() {}, dispatchEvent() {} };
  const { startOpenComposer, takeOpenComposer } = await import('./handoff-composer.js');
  const open = (runtime, ticket) => ({ handoffId: 'hnd_1', ticketId: ticket, runtime, runtimeLabel: runtime, cwd: '/work/repo', title: 'Carry on', from: 'Maya', local: false });

  startOpenComposer(open('runtime-a', 'tkt_1'));
  const first = takeOpenComposer();
  assert.equal(first.choice(), null, 'the first Open has nothing to start from');
  first.sent({ mode: 'accept-edits', model: 'model-large' });

  startOpenComposer(open('runtime-a', 'tkt_2'));
  const again = takeOpenComposer();
  assert.deepEqual(again.choice(), { mode: 'accept-edits', model: 'model-large' });
  again.sent({ mode: 'read-only', model: '' });

  startOpenComposer(open('runtime-b', 'tkt_3'));
  const other = takeOpenComposer();
  assert.equal(other.choice(), null, 'another runtime keeps its own choice');
  other.sent();

  startOpenComposer(open('runtime-a', 'tkt_4'));
  assert.deepEqual(takeOpenComposer().choice(), { mode: 'read-only', model: '' }, 'the newest send wins');
});

// Journey W-6: the live chat stayed headed "New session" after its first prompt was
// sent. Once sent, it is named for the handoff it continues.
test('an Open names its chat for the handoff it continues', async () => {
  const { startOpenComposer, takeOpenComposer } = await import('./handoff-composer.js');
  startOpenComposer({ handoffId: 'hnd_2', ticketId: 'tkt_9', runtime: 'runtime-a', cwd: '/work/repo', title: 'Rules 4–6 remain', from: 'Maya Okafor', local: false });
  assert.deepEqual(takeOpenComposer().heading(), { title: 'Rules 4–6 remain', continues: 'continues “Rules 4–6 remain” from Maya Okafor' });
  startOpenComposer({ handoffId: 'hnd_3', ticketId: 'tkt_10', runtime: 'runtime-a', cwd: '/work/repo', title: 'Carry on', from: '', local: true });
  assert.deepEqual(takeOpenComposer().heading(), { title: 'Carry on', continues: 'continues “Carry on” from an earlier session on this device' });
});
