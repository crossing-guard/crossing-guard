import { api } from '../core.js';
import { organization, adoptViews } from './organization-state.js';

const send = (method, path, body) => api(path, { method, body: JSON.stringify(body) });

export const loadSessionTags = session =>
  api('/api/session-tags?runtime=' + encodeURIComponent(session.runtime) + '&id=' + encodeURIComponent(session.id));

export const changeSessionTags = (sessions, change) =>
  send('POST', '/api/session-tags', { sessions: sessions.map(s => ({ runtime: s.runtime, id: s.id })), ...change });

export const renameSessionTag = (from, to) => send('POST', '/api/session-tags/rename', { from, to });
export const purgeSessionTag = tag => send('POST', '/api/session-tags/purge', { tag });
export const loadTagVocabulary = () => api('/api/session-tags/vocabulary');

export const saveSessionNote = (session, text) =>
  send('PUT', '/api/session-notes', { session: { runtime: session.runtime, id: session.id }, text });

export async function loadViews() {
  adoptViews(await api('/api/session-views'));
  return organization.views;
}

// Every view write presents the token of the list it was made against and
// adopts the list the daemon sends back.
export async function createView(view) {
  adoptViews(await send('POST', '/api/session-views', { state_token: organization.stateToken, view }));
}

export async function updateView(view) {
  adoptViews(await send('PUT', '/api/session-views/' + encodeURIComponent(view.id), { state_token: organization.stateToken, view }));
}

export async function deleteView(id) {
  const path = '/api/session-views/' + encodeURIComponent(id) + '?state_token=' + encodeURIComponent(organization.stateToken);
  adoptViews(await api(path, { method: 'DELETE' }));
}

export async function reorderViews(order) {
  adoptViews(await send('PUT', '/api/session-views/order', { state_token: organization.stateToken, order }));
}

export const loadViewCounts = () => api('/api/sessions?view=rail&repository_limit=1&counts=1');
