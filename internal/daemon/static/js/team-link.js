import { api } from './core.js';

// The team link, as the daemon reports it (GET /api/team). Everything shown in
// Settings → Team comes from here; the console never talks to the team server itself
// (ADR 0022: no capability the local API does not expose).

async function teamApi(path, options) {
  try {
    return await api(path, options);
  } catch (error) {
    try {
      const payload = JSON.parse(String(error?.message || ''));
      if (typeof payload?.error === 'string' && payload.error.trim()) {
        const normalized = new Error(payload.error.trim());
        normalized.status = error.status;
        throw normalized;
      }
    } catch (parseError) {
      if (parseError instanceof SyntaxError) throw error;
      throw parseError;
    }
    throw error;
  }
}

function loadTeamStatus() {
  return teamApi('/api/team');
}

function startTeamLink(server, name) {
  return teamApi('/api/team/link', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ server, name }) });
}

function unlinkTeam() {
  return teamApi('/api/team/unlink', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' });
}

// The layers surface (item 3c): the verified offer, the adoptions, and the two
// explicit local actions — availability is not activation.
function loadTeamLayers() {
  return teamApi('/api/team/layers');
}
// Adopt carries the state token of the offer as it was drawn (the route requires it), so
// an offer that moved since is refused. Un-adopt names the organization for a record of
// one this device is no longer linked to. Re-pin carries the PRESENTED fingerprint the
// person compared, and says the console asked.
function adoptLayer(scope, digest, stateToken = '') {
  const body = { scope, digest, state_token: stateToken, surface: 'console' };
  return teamApi('/api/team/layers/adopt', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
}
function unadoptLayer(scope, organizationId = '') {
  const body = organizationId ? { scope, organization_id: organizationId } : { scope };
  return teamApi('/api/team/layers/unadopt', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
}
function repinOrgKey(fingerprint) {
  return teamApi('/api/team/org-key/repin', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ fingerprint, surface: 'console' }) });
}
// Session content consent (D-12): one session's captured tool inputs, forward only.
function setSessionContent(runtime, session, enabled) {
  return teamApi('/api/team/content', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ runtime, session, enabled }) });
}
// Team memory (item 5). Sharing is one explicit action: every reviewed candidate (no ids),
// or named records one by one. Deletion reach is asked of the server through the daemon.
function shareTeamMemory(ids) {
  return teamApi('/api/team/memory/share', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(ids ? { ids } : {}) });
}
// Give up this device's edit of a team record and bring the team's revision back.
function takeTeamVersion(id) {
  return teamApi('/api/team/memory/take-team-version', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ id }) });
}
function loadTeamMemoryDeletions() {
  return teamApi('/api/team/memory/deletions');
}
// What the server holds of one session's content from this device, and its deletion —
// which also turns sharing off for the session.
function loadSentContent(runtime, session) {
  return teamApi('/api/team/content/sent?runtime=' + encodeURIComponent(runtime) + '&session=' + encodeURIComponent(session));
}
function deleteSentContent(runtime, session) {
  return teamApi('/api/team/content', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ runtime, session, enabled: false, delete_sent: true }) });
}
export { loadTeamStatus, startTeamLink, unlinkTeam, loadTeamLayers, adoptLayer, unadoptLayer, repinOrgKey, setSessionContent, takeTeamVersion,
  shareTeamMemory, loadTeamMemoryDeletions, loadSentContent, deleteSentContent };
