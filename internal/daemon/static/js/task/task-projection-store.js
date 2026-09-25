import { hasRenderableText } from './task-event-semantics.js';

const TERMINAL = new Set(['completed', 'interrupted', 'failed', 'unknown']);
const TASK_EVENT_LIMIT = 512;
const TASK_LIMIT = 512;

const taskNewestFirst = (left, right) => (right.updated_at || 0) - (left.updated_at || 0)
  || String(right.id).localeCompare(String(left.id));

function boundedTasks(tasks) {
  if (tasks.size <= TASK_LIMIT) return tasks;
  const next = new Map(tasks);
  const evictable = [...next.values()].filter(task => TERMINAL.has(task.lifecycle)).sort(taskNewestFirst).reverse();
  for (const task of evictable) {
    if (next.size <= TASK_LIMIT) break;
    next.delete(task.id);
  }
  return next;
}

export function reduceTaskProjection(state, event) {
  if (!event || event.schema_version !== 1 || !event.task_id) return { state, error: 'unsupported task event' };
  if (event.event_id <= state.cursor) return { state, duplicate: true };
  if (event.event_id !== state.cursor + 1) return { state, gap: true };
  const previous = state.tasks.get(event.task_id) || {
    id: event.task_id, session_runtime: event.session_runtime || '', catalog_session_id: event.catalog_session_id || '', native_session_id: event.native_session_id || '', lifecycle: 'queued',
    ownership: 'crossing-guard', observation_mode: 'stream', freshness: 'live',
    controllable: true, last_sequence: 0, events: [],
  };
  if (event.sequence <= previous.last_sequence) return { state: { ...state, cursor: event.event_id }, duplicate: true };
  if (event.sequence !== previous.last_sequence + 1) return { state, gap: true };
  const payload = event.payload || {};
  let lifecycle = previous.lifecycle;
  if (event.kind.startsWith('task.')) {
    const candidate = event.kind.slice(5);
    if (candidate === 'started') lifecycle = 'running';
    else if (candidate !== 'activity') lifecycle = candidate;
  }
  const task = {
    ...previous,
		session_runtime: event.session_runtime || previous.session_runtime,
    catalog_session_id: event.catalog_session_id || previous.catalog_session_id,
    lifecycle,
    native_session_id: payload.type === 'session' && payload.id ? payload.id : previous.native_session_id,
    last_sequence: event.sequence,
    last_event_id: event.event_id,
    updated_at: event.observed_at,
    freshness: lifecycle === 'unknown' ? 'unknown' : previous.freshness,
    controllable: lifecycle === 'unknown' ? false : previous.controllable,
    events: [...previous.events, event].slice(-TASK_EVENT_LIMIT),
  };
  const tasks = new Map(state.tasks); tasks.set(task.id, task);
  return { state: { cursor: event.event_id, tasks: boundedTasks(tasks) } };
}

export class TaskProjectionStore {
  constructor(onListenerError = error => console.error('Task projection listener failed', error)) {
    this.state = { cursor: 0, tasks: new Map() };
    this.listeners = new Set(); this.onListenerError = onListenerError;
  }
  snapshot(tasks, throughEventID) {
    const mapped = new Map();
    for (const task of tasks || []) mapped.set(task.id, { ...task, events: task.events || [] });
    this.state = { cursor: Number(throughEventID) || 0, tasks: boundedTasks(mapped) };
    this.emit();
  }
  apply(event) {
    const result = reduceTaskProjection(this.state, event);
    if (result.gap || result.error) return result;
    this.state = result.state; this.emit(); return result;
  }
  subscribe(listener) {
    this.listeners.add(listener);
    try { listener(this.state); } catch (error) { this.onListenerError(error); }
    return () => this.listeners.delete(listener);
  }
  emit() {
    for (const listener of this.listeners) {
      try { listener(this.state); } catch (error) { this.onListenerError(error); }
    }
  }
  task(id) { return this.state.tasks.get(id) || null; }
  all() { return [...this.state.tasks.values()]; }
  events(id, afterSequence = 0) { return (this.task(id)?.events || []).filter(event => event.sequence > afterSequence); }
  latestForCatalogSession(runtime, catalogID) {
    return this.tasksForCatalogSession(runtime, catalogID)[0] || null;
  }
  latestUncatalogedForNativeSession(runtime, nativeID) {
    return this.tasksForUncatalogedNativeSession(runtime, nativeID)[0] || null;
  }
  tasksForCatalogSession(runtime, catalogID) {
    return [...this.state.tasks.values()]
      .filter(task => task.session_runtime === runtime && task.catalog_session_id === catalogID)
      .sort(taskNewestFirst);
  }
  tasksForUncatalogedNativeSession(runtime, nativeID) {
    return [...this.state.tasks.values()]
      .filter(task => !task.catalog_session_id && task.session_runtime === runtime && task.native_session_id === nativeID)
      .sort(taskNewestFirst);
  }
  visibleForSession(runtime, catalogID, nativeID) {
    const matches = this.tasksForCatalogSession(runtime, catalogID);
    // A native resume handle may identify several harvested rows (Codex parent and
    // auto-review child are a measured example). Native fallback is safe only when
    // the catalog's own exact ID is the same value; otherwise wait for reconciliation.
    const tasks = matches.length ? matches
      : (catalogID === nativeID ? this.tasksForUncatalogedNativeSession(runtime, nativeID) : []);
    const active = tasks.filter(task => !TERMINAL.has(task.lifecycle));
    return active.length ? active : tasks.slice(0, 1);
  }
  terminal(id) { return TERMINAL.has(this.task(id)?.lifecycle); }
  cursor() { return this.state.cursor; }
}

export function taskEventsToTranscript(events) {
  const transcript = [];
  let message = '', reasoning = '';
  for (const event of events || []) {
    const payload = event.payload || {};
    switch (event.kind) {
      case 'message.delta': message += payload.text || ''; break;
      case 'message.completed':
        transcript.push({ kind: 'assistant', text: payload.text || message, ts: event.occurred_at });
        message = '';
        break;
      case 'reasoning.delta': reasoning += payload.text || ''; break;
      case 'reasoning.completed': {
        const text = payload.text || reasoning;
        if (hasRenderableText(text)) transcript.push({ kind: 'thinking', text, ts: event.occurred_at });
        reasoning = '';
        break;
      }
      case 'tool.started': transcript.push({ kind: 'tool_call', name: payload.name || 'tool', text: payload.text || '', ts: event.occurred_at }); break;
      case 'tool.completed': transcript.push({ kind: 'tool_result', text: payload.text || '', ts: event.occurred_at }); break;
      case 'coverage.gap': transcript.push({ kind: 'system', name: 'coverage', text: payload.text || 'Live event coverage is incomplete.', ts: event.occurred_at }); break;
    }
  }
  if (message) transcript.push({ kind: 'assistant', text: message });
  if (hasRenderableText(reasoning)) transcript.push({ kind: 'thinking', text: reasoning });
  return transcript;
}

export const taskProjectionStore = new TaskProjectionStore();
