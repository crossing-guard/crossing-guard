// Context generations prevent late reads/writes in one session from changing
// another composer destination. Saves serialize and admission uses their token.
export function createEffortState({ transport, changed = () => {}, active = () => true, schedule = callback => setTimeout(callback, 3000), cancel = clearTimeout }) {
  let generation = 0, context = {}, key = '', selection = null, token = '', busy = false, error = '', pendingIdentity = false;
  const drafts = new Map();
  let retryTimer, identityRetries = 0;
  const stopRetry = () => { if (retryTimer != null) cancel(retryTimer); retryTimer = null; };
  function retryIdentity() {
    stopRetry();
    if (!pendingIdentity || error || identityRetries >= 40 || !active()) return;
    retryTimer = schedule(() => { retryTimer = null; if (active()) { identityRetries++; void load(); } });
  }
  const snapshot = () => ({ generation, context: { ...context }, selection: selection && { ...selection }, token, busy, error, pendingIdentity });
  const emit = () => changed(snapshot());
  const draftKey = () => JSON.stringify([context.runtime, context.model]);
  async function load(adopt = false) {
    stopRetry();
    const epoch = ++generation, savedDraft = adopt || pendingIdentity ? selection : null;
    pendingIdentity = Boolean(savedDraft && context.session_id);
    busy = Boolean(context.session_id && context.model); error = ''; token = '';
    selection = context.session_id ? savedDraft : (drafts.get(draftKey()) || null);
    emit();
    if (!busy) return;
    try {
      const saved = await transport(context);
      if (epoch !== generation) return;
      token = saved.token; pendingIdentity = false;
      selection = token === '0' ? null : saved.thinking_effort;
      busy = false;
      if (savedDraft && token === '0') { await choose(savedDraft); return; }
    } catch (failure) {
      if (epoch !== generation) return;
      if (failure.code !== 'identity_pending' || !pendingIdentity) error = failure.detail || failure.message;
      busy = false;
    }
    emit(); retryIdentity();
  }
  async function choose(value) {
    if (busy) return;
    const epoch = ++generation;
    selection = value && { ...value }; error = '';
    if (!context.session_id || pendingIdentity) { drafts.set(draftKey(), selection); emit(); retryIdentity(); return; }
    busy = true; emit();
    try {
      const saved = await transport(context, selection, token);
      if (epoch !== generation) return;
      token = saved.token; selection = saved.thinking_effort;
    } catch (failure) {
      if (epoch !== generation) return;
      error = failure.detail || failure.message;
    }
    busy = false; emit();
  }
  return {
    snapshot, choose, retry: () => choose(selection), refuse(failure, epoch) { if (epoch !== generation) return false; error = failure.detail || failure.message; emit(); return true; }, reload: () => load(),
    setContext(next) {
      const nextKey = JSON.stringify([next.runtime, next.session_id, next.model]);
      if (nextKey === key) return;
      const adopt = !context.session_id && next.session_id && next.runtime === context.runtime && next.model === context.model;
      stopRetry(); identityRetries = 0; pendingIdentity = false;
      context = { ...next }; key = nextKey;
      void load(adopt);
    },
    request({ legacy = false } = {}) {
      if (busy || error) throw new Error(error || 'Wait for thinking effort to finish saving.');
      if (!selection) return token && !legacy ? { thinking_effort: { kind: 'inherit' }, session_effort_token: token } : {};
      return { thinking_effort: { ...selection }, ...(token && !pendingIdentity ? { session_effort_token: token } : {}) };
    },
  };
}
