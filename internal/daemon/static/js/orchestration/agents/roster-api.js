// Client for the Agents pages (agents-settings-redesign plan §4). Every call
// goes through the one orchestration error unwrap; writes carry the state
// token they read and an explicit confirmation.
import { api } from '../../core.js';
import { orchestrationApi } from '../orchestration-api-error.js';
import { bytesToBase64 } from '../profile-api.js';

const call = (path, options) => orchestrationApi(path, options, { code: 'agent_error' });
const send = (method, body) => ({ method, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
const enc = encodeURIComponent;

// tzOffsetMinutes is the browser's offset east of UTC, so day buckets follow
// the viewer's calendar.
export const tzOffsetMinutes = () => -new Date().getTimezoneOffset();

export const loadRoster = () => call('/api/orchestration/roster?tz_offset_minutes=' + tzOffsetMinutes());

export const loadAgent = id => call('/api/orchestration/roster/' + enc(id) + '?tz_offset_minutes=' + tzOffsetMinutes());

export function loadRuns(id, { place = '', outcome = '', before = '', limit = 0 } = {}) {
  const query = new URLSearchParams();
  if (place) query.set('place', place);
  if (outcome) query.set('outcome', outcome);
  if (before) query.set('before', before);
  if (limit) query.set('limit', String(limit));
  return call('/api/orchestration/roster/' + enc(id) + '/runs?' + query);
}

export const loadRevision = (id, sourceDigest, bundleDigest) => call('/api/orchestration/profiles/' + enc(id)
  + '/revision?' + new URLSearchParams({ source_digest: sourceDigest, bundle_digest: bundleDigest }));

// A save names the draft it replaces and the published version the author saw;
// the daemon checks the second only when the save starts the draft.
export const saveDraftEdit = (id, token, selectionToken, edit) => call('/api/orchestration/profiles/' + enc(id) + '/draft',
  send('PUT', { expected_state_token: token, expected_selection_token: selectionToken, edit }));

export const saveDraftSource = (id, token, selectionToken, text) => call('/api/orchestration/profiles/' + enc(id) + '/draft',
  send('PUT', { expected_state_token: token, expected_selection_token: selectionToken,
    source_base64: bytesToBase64(new TextEncoder().encode(text)) }));

export const discardDraft = (id, token) => call('/api/orchestration/profiles/' + enc(id) + '/draft',
  send('DELETE', { expected_state_token: token }));

export const publishDraft = (id, draftToken, selectionToken) => call('/api/orchestration/profiles/' + enc(id) + '/draft/publish',
  send('POST', { expected_draft_token: draftToken, expected_selection_token: selectionToken, confirmed: true }));

export const startDraft = request => call('/api/orchestration/drafts', send('POST', request));

// batch applies place changes all-or-nothing; validateOnly reports each
// change's validity without writing. A refusal is said in plain words.
export async function batch(changes, { validateOnly = false } = {}) {
  try {
    return await call('/api/orchestration/agents/batch', send('POST', { changes, validate_only: validateOnly, confirmed: true }));
  } catch (error) {
    throw batchError(error);
  }
}

// batchError names why nothing was saved: a place changed since the page read
// it (409), or the daemon refused named places (422, one problem per place).
export function batchError(error) {
  if (error?.status === 409) {
    return Object.assign(new Error('a place changed since this page loaded.'),
      { status: 409, recovery: 'Reload to see the latest, then try again.' });
  }
  let body = null;
  try { body = JSON.parse(String(error?.message || '')); } catch { return error; }
  const refused = (body?.results || []).filter(result => !result.valid);
  if (error?.status !== 422 || !refused.length) return error;
  return Object.assign(new Error('some places refused the change.'), { status: 422,
    recovery: refused.map(result => result.binding_id + ': ' + (result.problem || 'refused')).join('; ') });
}

export const newPlaceID = (profileID, projectRoot) => call('/api/orchestration/agents/binding-id',
  send('POST', { profile_id: profileID, project_root: projectRoot }));

// knownRepositories reads the launch roots the session rail knows, so a new
// place is picked from real repositories, never typed as a path.
export async function knownRepositories() {
  try {
    const rail = await api('/api/sessions?view=rail&repository_limit=500');
    const seen = new Map();
    for (const repository of rail.repositories || []) {
      const root = String(repository.launch_cwd || '');
      if (root && !seen.has(root)) seen.set(root, { root, sessions: Number(repository.total || 0) });
    }
    return [...seen.values()].sort((a, b) => a.root.localeCompare(b.root));
  } catch {
    return [];
  }
}
