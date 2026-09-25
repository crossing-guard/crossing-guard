import { orchestrationApi } from './orchestration-api-error.js';

// Managed-lane calls now share the one orchestration error unwrap (IMPL-10):
// typed `{error:{message,code,field,recovery}}` bodies become normalized
// Errors instead of raw JSON strings in banners.
const managedApi = (path, options) => orchestrationApi(path, options, { code: 'managed_error' });

const managedSettings = () => managedApi('/api/orchestration/managed/settings');
// managedProjection accepts one session identity or a list of exact
// alternates (artifact id, vendor meta id, vendor thread id — g4 plan §3):
// runs are recorded under whichever identity the task row carried, so the
// daemon matches ANY supplied value. A string still works (defensive: a
// stray string caller must never degrade to per-character params).
const managedProjection = (runtime = '', session = '') => {
  const query = new URLSearchParams(); if (runtime) query.set('runtime', runtime);
  const ids = [...new Set((Array.isArray(session) ? session : [session]).map(id => String(id || '').trim()).filter(Boolean))];
  for (const id of ids) query.append('session', id);
  return managedApi('/api/orchestration/managed' + (query.size ? '?' + query : ''));
};
// managedProjectionAll reads every managed run regardless of source scope —
// the Agents roster's run-history source. No filter params: the projection
// is unfiltered exactly when no runtime/session values are sent (the old
// `?scope=all` decoration claimed a backend contract that never existed —
// postwork SF-6).
const managedProjectionAll = () => managedApi('/api/orchestration/managed');
const sendFollowerDraft = (run, message) => managedApi('/api/orchestration/managed/runs/' + encodeURIComponent(run) + '/send', {
  method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ message, confirmed: true }),
});
const actOnManagedRun = run => managedApi('/api/orchestration/managed/runs/' + encodeURIComponent(run) + '/act', {
  method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ confirmed: true }),
});
const resumeWithCorrection = (run, message) => managedApi('/api/orchestration/managed/runs/' + encodeURIComponent(run) + '/resume', {
  method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ message, confirmed: true }),
});

export { managedSettings, managedProjection, managedProjectionAll, sendFollowerDraft, actOnManagedRun, resumeWithCorrection };
