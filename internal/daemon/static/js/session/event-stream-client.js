// EventStreamClient — the ONE long-lived connection a console tab holds.
//
// A browser allows six connections per origin across every tab of the profile.
// Four separate streams per tab (approvals, runtime tasks, session activity,
// live session) starved the tab's own reads as soon as a second tab existed.
// This client multiplexes every feed over GET /api/events/stream, keeps each
// feed's projection store and vocabulary unchanged (the envelope's payload is
// the legacy body verbatim), and owns the one reconnect/backoff loop.
//
// Transport is fetch + readSSE like every other console stream — NOT the
// browser EventSource API, which cannot send the X-CG-Token header.
import { readSSE } from '../task/task-stream.js';
import { cpHeaders, apiPath } from '../core.js';

const wait = ms => new Promise(resolve => setTimeout(resolve, ms));

export function openEventStream(params, signal) {
  const query = new URLSearchParams();
  query.set('tasks', String(params.tasks || 0));
  query.set('activity', String(params.activity || 0));
  if (params.runtime && params.id) { query.set('runtime', params.runtime); query.set('id', params.id); }
  if (params.clientID) query.set('client_id', params.clientID);
  return fetch(apiPath('/api/events/stream?' + query), { headers: cpHeaders(), signal }).then(response => {
    if (!response.ok) throw new Error('Event stream failed: HTTP ' + response.status);
    return response;
  });
}

export class EventStreamClient {
  // snapshots: { tasks(signal) -> {tasks, through_event_id}, activity(signal) -> snapshot }
  // backoff: { base } — the cap arrives from the daemon in the hello frame.
  constructor({ taskStore, activityStore, approvalStore, snapshots, open = openEventStream,
    notify = () => {}, clientID = '', backoff = {} }) {
    this.taskStore = taskStore; this.activityStore = activityStore; this.approvalStore = approvalStore;
    this.snapshots = snapshots; this.open = open; this.notify = notify; this.clientID = clientID;
    this.backoff = { base: 250, cap: 5000, ...backoff };
    this.stopped = true; this.controller = null; this.attempt = null;
    this.subject = { runtime: '', id: '' }; this.live = null; this.subjectChanged = false;
  }

  start() {
    if (!this.stopped) return;
    this.stopped = false; this.controller = new AbortController();
    void this.run(this.controller);
  }

  stop() {
    this.stopped = true;
    this.attempt?.abort(); this.controller?.abort();
    this.controller = null; this.attempt = null;
  }

  // setSubject selects the live session this connection follows. `live` is
  // the handler object ({ handle(event, payload), drop() }); null clears it.
  // Changing the subject reconnects at once with every other cursor kept.
  setSubject(runtime, id, live) {
    this.subject = { runtime: runtime || '', id: id || '' };
    this.live = live || null;
    if (this.stopped || !this.attempt) return;
    this.subjectChanged = true;
    this.attempt.abort();
  }

  async run(controller) {
    let delay = this.backoff.base;
    const active = () => !this.stopped && this.controller === controller && !controller.signal.aborted;
    while (active()) {
      const attempt = new AbortController(); this.attempt = attempt;
      const onAbort = () => attempt.abort();
      controller.signal.addEventListener('abort', onAbort, { once: true });
      try {
        const outcome = await this.connect(attempt.signal);
        if (!active()) return;
        if (outcome === 'reset') { delay = this.backoff.base; continue; }
        throw new Error('Event stream disconnected');
      } catch (error) {
        if (!active()) return;
        if (this.subjectChanged) { this.subjectChanged = false; delay = this.backoff.base; continue; }
        if (error?.name === 'AbortError') return;
        this.live?.drop?.();
        this.notify({ state: 'reconnecting', detail: String(error) });
        await wait(delay); delay = Math.min(delay * 2, this.backoff.cap);
      } finally {
        controller.signal.removeEventListener('abort', onAbort);
      }
    }
  }

  async connect(signal) {
    if (this.taskStore.cursor() === 0) await this.resnapshot('tasks', signal);
    await this.resnapshot('activity', signal);
    const response = await this.open({
      tasks: this.taskStore.cursor(), activity: this.activityStore.cursor(),
      runtime: this.subject.runtime, id: this.subject.id, clientID: this.clientID,
    }, signal);
    this.notify({ state: 'connected', detail: '' });
    const resets = new Set();
    await readSSE(response.body, (value, meta) => this.dispatch(value, meta, resets),
      malformed => this.notify({ state: 'degraded', detail: 'Skipped a malformed event.', malformed }));
    for (const feed of resets) await this.resnapshot(feed, signal);
    return resets.size ? 'reset' : 'closed';
  }

  async resnapshot(feed, signal) {
    if (feed === 'tasks') {
      const data = await this.snapshots.tasks(signal);
      this.taskStore.snapshot(data.tasks || [], data.through_event_id || 0);
    } else if (feed === 'activity') {
      this.activityStore.snapshot(await this.snapshots.activity(signal));
    }
  }

  dispatch(value, meta, resets) {
    const feed = value?.feed || meta.event;
    const event = value?.event || '';
    const payload = value?.payload;
    if (feed === 'stream') this.applyHello(event, payload);
    else if (feed === 'approvals') this.applyApproval(event, payload);
    else if (feed === 'tasks') this.applyTask(event, payload, resets);
    else if (feed === 'activity') this.applyActivity(event, payload, resets);
    else if (feed === 'session') this.live?.handle?.(event, payload);
  }

  applyHello(event, payload) {
    if (event !== 'hello') return;
    if (Number.isFinite(payload?.backoff_cap_seconds) && payload.backoff_cap_seconds > 0) {
      this.backoff.cap = payload.backoff_cap_seconds * 1000;
    }
  }

  applyApproval(event, payload) {
    const result = this.approvalStore.apply(event === 'snapshot'
      ? { type: 'snapshot', pending: payload?.pending || [], history: payload?.history || [] }
      : { type: 'approval', kind: payload?.kind, approval: payload?.approval });
    if (result.error) throw new Error(result.error);
  }

  applyTask(event, payload, resets) {
    if (event === 'reset' || payload?.type === 'reset') { resets.add('tasks'); return; }
    if (event === 'unavailable') { this.notify({ state: 'degraded', detail: payload?.error || 'Task updates unavailable.' }); return; }
    const result = this.taskStore.apply(payload);
    if (result.gap || result.error) throw new Error('Task stream cursor gap');
  }

  applyActivity(event, payload, resets) {
    if (event === 'unavailable') { this.notify({ state: 'degraded', detail: payload?.error || 'Session activity unavailable.' }); return; }
    if (event === 'reset') resets.add('activity');
    const result = this.activityStore.snapshot(payload);
    if (result.error) throw new Error(result.error);
  }
}
