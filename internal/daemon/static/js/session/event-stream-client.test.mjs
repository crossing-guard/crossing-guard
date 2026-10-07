import test from 'node:test';
import assert from 'node:assert/strict';

globalThis.localStorage = { getItem: () => 'tok' };
const { EventStreamClient } = await import('./event-stream-client.js');

const frame = (feed, event, payload, cursor) => {
  const envelope = { feed, event, payload };
  if (cursor !== undefined) envelope.cursor = cursor;
  return `event: ${feed}\ndata: ${JSON.stringify(envelope)}\n\n`;
};
const sseBody = frames => new ReadableStream({
  start(controller) {
    for (const item of frames) controller.enqueue(new TextEncoder().encode(item));
    controller.close();
  },
});
// A held stream behaves like a real fetch body: it never closes on its own and
// rejects the pending read when the request's signal aborts.
const held = signal => new ReadableStream({
  start(controller) {
    if (!signal) return;
    signal.addEventListener('abort', () => controller.error(Object.assign(new Error('aborted'), { name: 'AbortError' })), { once: true });
  },
});
const flush = (ms = 15) => new Promise(resolve => setTimeout(resolve, ms));

function fakeStores() {
  const calls = { task: [], activity: [], approval: [] };
  let taskCursor = 0, activityCursor = 0;
  return {
    calls,
    taskStore: { cursor: () => taskCursor, snapshot: (tasks, through) => { taskCursor = through; calls.task.push({ snapshot: through }); },
      apply: event => { calls.task.push(event); taskCursor = event.event_id || taskCursor; return {}; } },
    activityStore: { cursor: () => activityCursor, snapshot: value => { activityCursor = value?.generation || activityCursor; calls.activity.push(value); return {}; } },
    approvalStore: { apply: event => { calls.approval.push(event); return {}; } },
    snapshots: { tasks: async () => ({ tasks: [], through_event_id: taskCursor }), activity: async () => ({ generation: activityCursor, items: [] }) },
  };
}

test('one connection fans every feed out to its own store with the payload bytes untouched', async () => {
  const stores = fakeStores();
  const opened = [];
  const client = new EventStreamClient({ ...stores, open: async (params, signal) => { opened.push(params); return { body: held(signal), ok: true }; } });
  const taskPayload = { schema_version: 1, event_id: 4, kind: 'task.started', payload: { text: 'héllo' } };
  const activityPayload = { generation: 9, items: [{ runtime: 'x' }] };
  client.start();
  await flush();
  const resets = new Set();
  client.dispatch({ feed: 'stream', event: 'hello', payload: { backoff_cap_seconds: 7 } }, { event: 'stream' }, resets);
  client.dispatch({ feed: 'approvals', event: 'snapshot', payload: { pending: [{ id: 'a' }], history: [] } }, { event: 'approvals' }, resets);
  client.dispatch({ feed: 'approvals', event: 'approval', payload: { kind: 'decided', approval: { id: 'a' } } }, { event: 'approvals' }, resets);
  client.dispatch({ feed: 'tasks', event: 'task', cursor: 4, payload: taskPayload }, { event: 'tasks' }, resets);
  client.dispatch({ feed: 'activity', event: 'activity', cursor: 9, payload: activityPayload }, { event: 'activity' }, resets);
  client.stop();
  assert.equal(client.backoff.cap, 7000, 'the daemon publishes the backoff cap; the browser does not compile it in');
  assert.deepEqual(stores.calls.approval[0], { type: 'snapshot', pending: [{ id: 'a' }], history: [] });
  assert.deepEqual(stores.calls.approval[1], { type: 'approval', kind: 'decided', approval: { id: 'a' } });
  assert.deepEqual(stores.calls.task.at(-1), taskPayload, 'task events reach the store exactly as the legacy route sent them');
  assert.deepEqual(stores.calls.activity.at(-1), activityPayload);
  assert.equal(resets.size, 0);
  assert.equal(opened.length, 1);
  assert.equal(opened[0].tasks, 0);
  assert.equal(opened[0].clientID, '');
});

test('a reset for one feed re-snapshots that feed only, then reconnects with the cursors', async () => {
  const stores = fakeStores();
  stores.taskStore.snapshot([], 12);
  stores.activityStore.snapshot({ generation: 3 });
  stores.calls.task.length = 0; stores.calls.activity.length = 0;
  const opened = [];
  const bodies = [sseBody([frame('tasks', 'reset', { type: 'reset', through_event_id: 40 }, 40)])];
  let taskSnapshots = 0, activitySnapshots = 0;
  stores.snapshots.tasks = async () => { taskSnapshots++; return { tasks: [], through_event_id: 40 }; };
  stores.snapshots.activity = async () => { activitySnapshots++; return { generation: 3, items: [] }; };
  const client = new EventStreamClient({ ...stores, open: async (params, signal) => { opened.push({ ...params }); return { body: bodies.shift() || held(signal) }; } });
  client.start();
  await flush(40);
  client.stop();
  assert.equal(opened.length, 2, 'the reset ends the stream and the client reconnects');
  assert.equal(opened[0].tasks, 12);
  assert.equal(opened[1].tasks, 40, 'the reconnect carries the reset cursor');
  assert.equal(opened[1].activity, 3, 'the activity cursor is kept across a task reset');
  assert.equal(taskSnapshots, 1, 'only the reset feed re-snapshots');
});

test('changing the live subject reconnects at once and routes session frames to the handler', async () => {
  const stores = fakeStores();
  const opened = [];
  const aborted = [];
  const client = new EventStreamClient({ ...stores, open: async (params, signal) => {
    opened.push({ ...params }); signal.addEventListener('abort', () => aborted.push(params.id));
    return { body: held(signal) };
  } });
  const seen = [];
  const live = { handle: (event, payload) => seen.push([event, payload]), drop: () => seen.push(['drop']) };
  client.start();
  await flush();
  client.setSubject('claude', 'ses-1', live);
  await flush(30);
  client.dispatch({ feed: 'session', event: 'state', payload: { execution: 'running' } }, { event: 'session' }, new Set());
  const abortedBeforeStop = [...aborted];
  client.stop();
  assert.equal(opened.length, 2);
  assert.equal(opened[0].id, '');
  assert.equal(opened[1].id, 'ses-1');
  assert.equal(opened[1].runtime, 'claude');
  assert.deepEqual(abortedBeforeStop, [''], 'the subject change aborts the old attempt, not the loop');
  assert.deepEqual(seen, [['state', { execution: 'running' }]]);
});

test('a dropped connection reports reconnecting once, tells the live handler, and backs off', async () => {
  const stores = fakeStores();
  const states = [];
  let attempts = 0;
  const client = new EventStreamClient({ ...stores, notify: detail => states.push(detail.state), backoff: { base: 5 },
    open: async (_, signal) => { attempts++; if (attempts === 1) return { body: sseBody([]) }; return { body: held(signal) }; } });
  const dropped = [];
  client.setSubject('claude', 'ses-2', { handle: () => {}, drop: () => dropped.push(1) });
  client.start();
  await flush(60);
  client.stop();
  assert.deepEqual(states.slice(0, 3), ['connected', 'reconnecting', 'connected']);
  assert.equal(dropped.length, 1, 'the live view learns the connection was lost');
  assert.equal(attempts, 2);
});

test('a task cursor gap throws so the loop re-snapshots instead of rendering a hole', async () => {
  const stores = fakeStores();
  stores.taskStore.apply = () => ({ gap: true });
  const client = new EventStreamClient({ ...stores, open: async (_, signal) => ({ body: held(signal) }) });
  assert.throws(() => client.dispatch({ feed: 'tasks', event: 'task', payload: { event_id: 9 } }, { event: 'tasks' }, new Set()), /cursor gap/);
});

// A stream that delivers its frames and then stays open, like a daemon whose
// absent feed sent its `unavailable` frame and ended quietly.
const framesThenHeld = (frames, signal) => new ReadableStream({
  start(controller) {
    for (const item of frames) controller.enqueue(new TextEncoder().encode(item));
    signal?.addEventListener('abort', () => controller.error(Object.assign(new Error('aborted'), { name: 'AbortError' })), { once: true });
  },
});
const httpError = status => Object.assign(new Error('HTTP ' + status), { status });
const taskReason = 'runtime task service unavailable: store schema is v99';

// Degraded start (degraded-surfaces-state-the-reason plan §2.2): a feed whose
// service is absent answers its opening snapshot with 503. The stream must open
// anyway, so approvals and activity stay live and the feed's own frame names why.
test('a 503 opening task snapshot opens the stream once and ends degraded with the reason', async () => {
  const stores = fakeStores();
  stores.snapshots.tasks = async () => { throw httpError(503); };
  const states = [];
  let attempts = 0;
  const client = new EventStreamClient({ ...stores, notify: d => states.push([d.state, d.detail]), backoff: { base: 2 },
    open: async (_, signal) => { attempts++; return { body: framesThenHeld([
      frame('approvals', 'snapshot', { pending: [], history: [] }),
      frame('tasks', 'unavailable', { error: taskReason }),
    ], signal) }; } });
  client.start();
  await flush(60);
  client.stop();
  assert.equal(attempts, 1, 'no retry loop against an absent feed');
  assert.deepEqual(states, [['connected', ''], ['degraded', taskReason]]);
  assert.equal(stores.calls.approval.length, 1, 'approvals still arrive');
});

test('a 503 opening activity snapshot opens the stream the same way', async () => {
  const stores = fakeStores();
  stores.snapshots.activity = async () => { throw httpError(503); };
  const states = [];
  let attempts = 0;
  const client = new EventStreamClient({ ...stores, notify: d => states.push(d.state), backoff: { base: 2 },
    open: async (_, signal) => { attempts++; return { body: framesThenHeld([frame('activity', 'unavailable', { error: 'x' })], signal) }; } });
  client.start();
  await flush(60);
  client.stop();
  assert.equal(attempts, 1);
  assert.deepEqual(states, ['connected', 'degraded']);
});

test('any other opening snapshot failure still reconnects with backoff', async () => {
  const stores = fakeStores();
  let snapshots = 0;
  stores.snapshots.tasks = async () => { snapshots++; throw httpError(500); };
  const states = [];
  let attempts = 0;
  const client = new EventStreamClient({ ...stores, notify: d => states.push(d.state), backoff: { base: 2 },
    open: async (_, signal) => { attempts++; return { body: held(signal) }; } });
  client.start();
  await flush(40);
  client.stop();
  assert.equal(attempts, 0, 'a 500 is not an absent feed');
  assert.ok(snapshots >= 2, 'the snapshot is retried');
  assert.equal(states[0], 'reconnecting');
});

test('a 503 re-snapshot after a reset backs off instead of reopening at once', async () => {
  const stores = fakeStores();
  let snapshots = 0;
  stores.snapshots.tasks = async () => { snapshots++; if (snapshots === 1) return { tasks: [], through_event_id: 5 }; throw httpError(503); };
  const states = [];
  let attempts = 0;
  const client = new EventStreamClient({ ...stores, notify: d => states.push(d.state), backoff: { base: 200 },
    open: async (_, signal) => { attempts++; return { body: attempts === 1 ? sseBody([frame('tasks', 'reset', { type: 'reset' })]) : held(signal) }; } });
  client.start();
  await flush(60);
  client.stop();
  assert.equal(attempts, 1, 'no immediate reopen while the backoff runs');
  assert.deepEqual(states, ['connected', 'reconnecting']);
});

test('a subject change on a degraded feed reconnects once and ends degraded again', async () => {
  const stores = fakeStores();
  stores.snapshots.tasks = async () => { throw httpError(503); };
  const states = [];
  let attempts = 0;
  const client = new EventStreamClient({ ...stores, notify: d => states.push(d.state), backoff: { base: 2 },
    open: async (_, signal) => { attempts++; return { body: framesThenHeld([frame('tasks', 'unavailable', { error: taskReason })], signal) }; } });
  client.start();
  await flush(20);
  client.setSubject('runtime-a', 'ses-3', { handle: () => {}, drop: () => {} });
  await flush(40);
  client.stop();
  assert.equal(attempts, 2);
  assert.deepEqual(states, ['connected', 'degraded', 'connected', 'degraded']);
});
