// Client for the handoff routes (loopback reference: "Handoff between members",
// "Opening a handoff, and memory recall", "Choosing a folder"). A refusal is
// the daemon's `{error, code}`; it is thrown with both, so a surface words the
// code and falls back to the sentence.
import { api, cpHeaders, apiPath } from '../core.js';

const json = (method, body) => ({ method, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body ?? {}) });

// refusalOf turns a thrown response into { message, code, status }.
export function refusalOf(error) {
  const raw = String(error?.message || error || '');
  let parsed = null;
  try { parsed = JSON.parse(raw); } catch { parsed = null; }
  const refusal = new Error(String(parsed?.error || parsed?.message || raw || 'The request failed'));
  refusal.code = String(parsed?.code || '');
  refusal.status = error?.status || 0;
  return refusal;
}

async function call(path, options) {
  try {
    return await api(path, options);
  } catch (error) {
    throw refusalOf(error);
  }
}

const at = id => '/api/team/handoffs/' + encodeURIComponent(id);

export const loadDraft = session => call('/api/handoff/generate?runtime=' + encodeURIComponent(session.runtime) + '&id=' + encodeURIComponent(session.id));
export const loadMembers = () => call('/api/team/members');
export const loadHandoffs = () => call('/api/team/handoffs');
export const loadHandoff = id => call(at(id));
export const sendHandoff = body => call('/api/team/handoffs/send', json('POST', body));
export const declineHandoff = id => call(at(id) + '/decline', json('POST'));
export const withdrawHandoff = id => call(at(id) + '/withdraw', json('POST'));
export const closeHandoff = id => call(at(id) + '/close', json('POST'));
export const loadOpenOptions = id => call(at(id) + '/open-options');
export const openHandoff = (id, runtime, checkoutRoot) => call(at(id) + '/open', json('POST', { runtime, checkout_root: checkoutRoot }));
export const cancelOpen = (id, ticket) => call(at(id) + '/open/' + encodeURIComponent(ticket) + '/cancel', json('POST'));
export const deliverAgain = (id, ticket) => call(at(id) + '/deliver-again', json('POST', { ticket }));
export const loadSessionHandoff = (runtime, nativeID) => call('/api/team/handoffs/session?runtime=' + encodeURIComponent(runtime) + '&native_id=' + encodeURIComponent(nativeID));
export const loadRecall = () => call('/api/memory/attach');
export const setRecall = (runtime, action) => call('/api/memory/attach', json('POST', { runtime, action, consent: true }));
export const loadAdoptions = () => call('/api/team/layers');
export const chooseFolder = start => call('/api/folder/choose', json('POST', { start: start || '' }));

// cancelOpenOnLeave cancels a waiting Open while the page is going away: the
// request outlives the page, which an ordinary one may not.
export function cancelOpenOnLeave(id, ticket) {
  try {
    void fetch(apiPath(at(id) + '/open/' + encodeURIComponent(ticket) + '/cancel'),
      { method: 'POST', keepalive: true, headers: { ...cpHeaders(), 'Content-Type': 'application/json' }, body: '{}' });
  } catch { /* the page is closing; the ticket stays waiting and offers nothing */ }
}
