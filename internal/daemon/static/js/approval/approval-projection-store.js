const HISTORY_LIMIT = 100;

const byDeadline = (left, right) => String(left.deadline || '').localeCompare(String(right.deadline || ''))
  || String(left.id).localeCompare(String(right.id));
const byDecisionNewest = (left, right) => String(right.decided_at || right.created_at || '')
  .localeCompare(String(left.decided_at || left.created_at || '')) || String(right.id).localeCompare(String(left.id));

export function reduceApprovalProjection(state, event) {
  if (!event || typeof event !== 'object') return { state, error: 'invalid approval event' };
  if (event.type === 'snapshot') {
    const pending = new Map(); const history = new Map();
    for (const approval of event.pending || []) if (approval?.id) pending.set(approval.id, { ...approval });
    for (const approval of event.history || []) if (approval?.id) history.set(approval.id, { ...approval });
    return { state: { pending, history } };
  }
  if (event.type !== 'approval' || !event.approval?.id) return { state, error: 'unsupported approval event' };
  const approval = { ...event.approval };
  const pending = new Map(state.pending); const history = new Map(state.history);
  if (event.kind === 'pending' && approval.status === 'pending') {
    pending.set(approval.id, approval); history.delete(approval.id);
  } else if (event.kind === 'decided' || approval.status !== 'pending') {
    pending.delete(approval.id); history.set(approval.id, approval);
    const ordered = [...history.values()].sort(byDecisionNewest).slice(0, HISTORY_LIMIT);
    history.clear(); for (const item of ordered) history.set(item.id, item);
  } else {
    return { state, error: 'unsupported approval transition' };
  }
  return { state: { pending, history } };
}

export class ApprovalProjectionStore {
  constructor(onListenerError = error => console.error('Approval projection listener failed', error)) {
    this.state = { pending: new Map(), history: new Map() };
    this.listeners = new Set(); this.onListenerError = onListenerError;
  }
  snapshot(value) { this.apply({ type: 'snapshot', pending: value?.pending || [], history: value?.history || [] }); }
  apply(event) {
    const result = reduceApprovalProjection(this.state, event);
    if (result.error) return result;
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
  pending() { return [...this.state.pending.values()].sort(byDeadline); }
  history() { return [...this.state.history.values()].sort(byDecisionNewest); }
}

export const approvalProjectionStore = new ApprovalProjectionStore();
