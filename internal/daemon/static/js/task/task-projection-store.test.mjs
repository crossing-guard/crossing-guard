import { TaskProjectionStore, reduceTaskProjection, taskEventsToTranscript } from './task-projection-store.js';

let failures = 0;
const check = (name, condition) => {
  if (!condition) { failures++; console.error('FAIL', name); }
};

const event = (eventID, taskID, sequence, catalogID, nativeID) => ({
  schema_version: 1, event_id: eventID, task_id: taskID, sequence,
  session_runtime: 'codex', catalog_session_id: catalogID,
  native_session_id: nativeID, kind: 'task.started', observed_at: eventID,
  payload: {},
});

const store = new TaskProjectionStore();
store.snapshot([], 0);
store.apply(event(1, 'parent-task', 1, 'parent-rollout', 'shared-thread'));
store.apply(event(2, 'child-task', 1, 'child-rollout', 'shared-thread'));

check('catalog identity selects only the exact rail row',
  store.latestForCatalogSession('codex', 'parent-rollout')?.id === 'parent-task');
check('shared native identity does not cross-paint catalogued tasks',
  store.latestUncatalogedForNativeSession('codex', 'shared-thread') === null);

store.apply(event(3, 'new-task', 1, '', 'new-thread'));
check('new unharvested task may use its exact native identity',
  store.latestUncatalogedForNativeSession('codex', 'new-thread')?.id === 'new-task');
check('uncataloged native task does not cross-paint a different catalog identity',
  store.visibleForSession('codex', 'rollout-new-thread', 'new-thread').length === 0);
check('native fallback is allowed when the catalog uses that exact identity',
  store.visibleForSession('codex', 'new-thread', 'new-thread')[0]?.id === 'new-task');

const gap = reduceTaskProjection(store.state, event(5, 'later', 1, '', 'later'));
check('global cursor gaps are rejected', gap.gap === true && store.cursor() === 3);

store.apply(event(4, 'active-task', 1, 'shared-catalog', 'shared-native'));
store.apply({ ...event(5, 'completed-task', 1, 'shared-catalog', 'shared-native'), kind: 'task.completed' });
check('active work stays visible ahead of newer terminal history',
  store.visibleForSession('codex', 'shared-catalog', 'shared-native')[0]?.id === 'active-task');

const listenerErrors = [];
let healthyListenerCalls = 0;
const isolated = new TaskProjectionStore(error => listenerErrors.push(error));
isolated.subscribe(() => { throw new Error('view failed'); });
isolated.subscribe(() => { healthyListenerCalls++; });
isolated.apply(event(1, 'isolated-task', 1, '', 'isolated-native'));
check('one listener cannot stop cursor progress or other listeners',
  isolated.cursor() === 1 && healthyListenerCalls === 2 && listenerErrors.length === 2);

const many = Array.from({ length: 520 }, (_, index) => ({
  id: 'terminal-' + index, lifecycle: 'completed', updated_at: index, events: [],
}));
isolated.snapshot(many, 520);
check('terminal task projection is bounded', isolated.state.tasks.size === 512);

const emptyReasoning = taskEventsToTranscript([
  { kind: 'reasoning.delta', payload: { text: '' }, occurred_at: 1 },
  { kind: 'reasoning.completed', payload: { text: ' \n ' }, occurred_at: 2 },
]);
check('persisted empty reasoning does not create a transcript row', emptyReasoning.length === 0);

const accumulatedReasoning = taskEventsToTranscript([
  { kind: 'reasoning.delta', payload: { text: '  inspect' }, occurred_at: 3 },
  { kind: 'reasoning.delta', payload: { text: ' evidence  ' }, occurred_at: 4 },
  { kind: 'reasoning.completed', payload: { text: '' }, occurred_at: 5 },
]);
check('empty completion preserves accumulated visible reasoning',
  accumulatedReasoning.length === 1
  && accumulatedReasoning[0].kind === 'thinking'
  && accumulatedReasoning[0].text === '  inspect evidence  ');

if (failures) process.exit(1);
console.log('task-projection-store: all pass');
