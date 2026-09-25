const exactKey = (runtime, catalogID) => String(runtime || '') + '\u0000' + String(catalogID || '');

export class SessionActivityStore {
  constructor({ now = () => Date.now(), onListenerError = error => console.error('Session activity listener failed', error) } = {}) {
    this.now = now; this.onListenerError = onListenerError;
    this.state = { generation: 0, capability: { status: 'unavailable', detail: 'Activity snapshot has not connected.' }, items: new Map() };
    this.listeners = new Set(); this.expiryTimer = null;
  }
  snapshot(value) {
    if (!value || value.schema_version !== 1 || !Number.isSafeInteger(Number(value.generation))) {
      return { error: 'unsupported session activity snapshot' };
    }
    const items = new Map();
    for (const item of value.items || []) {
      if (!item?.runtime || !item?.catalog_session_id) continue;
      items.set(exactKey(item.runtime, item.catalog_session_id), { ...item });
    }
    this.state = {
      generation: Number(value.generation),
      capability: value.capability || { status: 'unavailable', detail: 'Activity capability missing.' },
      observed_at: value.observed_at || '',
      items,
    };
    this.scheduleExpiry();
    this.emit();
    return { state: this.state };
  }
  activity(runtime, catalogID) {
    const item = this.state.items.get(exactKey(runtime, catalogID));
    if (!item) return null;
    const expires = Date.parse(item.expires_at || '');
    if (Number.isFinite(expires) && this.now() > expires) {
      return { ...item, freshness: 'stale', detail: 'The last native activity observation expired; current turn state is unknown.' };
    }
    return { ...item };
  }
  cursor() { return this.state.generation; }
  capability() { return { ...this.state.capability }; }
  scheduleExpiry() {
    clearTimeout(this.expiryTimer);
    const expiries = [...this.state.items.values()]
      .map(item => Date.parse(item.expires_at || '')).filter(Number.isFinite);
    if (!expiries.length) { this.expiryTimer = null; return; }
    const delay = Math.max(0, Math.min(...expiries) - this.now() + 5);
    this.expiryTimer = setTimeout(() => { this.expiryTimer = null; this.emit(); }, delay);
    this.expiryTimer?.unref?.();
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
}

export const sessionActivityStore = new SessionActivityStore();
