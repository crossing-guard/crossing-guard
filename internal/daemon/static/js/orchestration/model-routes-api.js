// Client for the model route API (team rest-of-release plan §5.5): list,
// preview, select (create or edit) and delete. A refusal keeps the places the
// daemon named, so a sheet can list them.
import { api } from '../core.js';
import { normalizeOrchestrationError } from './orchestration-api-error.js';

const send = (method, body) => ({ method, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });

// routeError is the typed refusal with the places it names.
export function routeError(error) {
  const normalized = normalizeOrchestrationError(error, { code: 'route_error' });
  let places = [];
  try { places = JSON.parse(String(error?.message || ''))?.error?.places || []; } catch { places = []; }
  if (normalized !== error) normalized.places = places.map(String);
  return normalized;
}

async function call(path, options) {
  try {
    return await api(path, options);
  } catch (error) {
    throw routeError(error);
  }
}

export const loadModelRoutes = () => call('/api/model-routes');

export const previewModelRoute = draft => call('/api/model-routes/preview', send('POST', draft));

// selectModelRoute writes what a preview showed: its digest and state token go
// back unchanged, with the confirmation.
export const selectModelRoute = (draft, preview) => call('/api/model-routes/select', send('POST', {
  ...draft, preview_digest: preview.preview_digest, expected_state_token: preview.state_token, confirmed: true }));

export const deleteModelRoute = route => call('/api/model-routes/' + encodeURIComponent(route.route_id),
  send('DELETE', { expected_state_token: route.state_token, confirmed: true }));
