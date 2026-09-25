// Agent-shaped orchestration routes (redesign plan §3.5 API compatibility step).
//
// Backend contract consumed here (built in parallel; a Go payload-shape
// contract test pins it):
//   GET  /api/orchestration/agents
//        → { agents:[{ binding_id, state, role('reviewer'|'follower'|'helper'),
//            priority, declared_tags:[], limits:{ max_total, max_active,
//            max_hops, loop_budget, max_group_tokens, max_agent_tokens },
//            scope_runtime, scope_session, project_root, profile_id, runtime,
//            mode, granted_authority:[], auto_action, state_token,
//            profile_name, profile_description, prompt_excerpt }],
//            defaults:{...resolved}, signals:[{kind,source,terminal,description}] }
//   PUT  /api/orchestration/agents/{binding}          (CAS via state_token)
//   POST /api/orchestration/agents/{binding}/disable
//   GET  /api/orchestration/tags?session_id=X → { tags:[{tag, agent_key,
//            binding_id, applied_at, expires_at}] }
//   POST /api/orchestration/groups/{group}/notes  { body }
//   GET  /api/orchestration/groups/{group}/notes
//   POST /api/orchestration/notes/{note}/retract
import { orchestrationApi } from './orchestration-api-error.js';

const agentsApi = (path, options) => orchestrationApi(path, options, { code: 'agent_error' });

const loadAgents = () => agentsApi('/api/orchestration/agents');

// saveAgent updates one agent binding. CAS: callers pass the state token they
// read (mirrors the managed binding PUT shape).
const saveAgent = (bindingID, input, expectedStateToken) => agentsApi(
  '/api/orchestration/agents/' + encodeURIComponent(bindingID), {
    method: 'PUT', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ ...input, expected_state_token: expectedStateToken, confirmed: true }),
  });

// Provider-outage reroute (provider-outage plan Slice C): preview names each
// parked run's outcome; apply carries the preview token so drift between the
// two is refused server-side instead of silently applied.
const rerouteParkedPreview = (runtime, model) => agentsApi(
  '/api/orchestration/managed/provider-reroute/preview', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ runtime, model }),
  });

const rerouteParkedApply = (runtime, model, previewToken) => agentsApi(
  '/api/orchestration/managed/provider-reroute', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ runtime, model, preview_token: previewToken, confirmed: true }),
  });

const disableAgent = (bindingID, expectedStateToken) => agentsApi(
  '/api/orchestration/agents/' + encodeURIComponent(bindingID) + '/disable', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ expected_state_token: expectedStateToken, confirmed: true }),
  });

const loadRelatedSessions = (runtime, sessionID) => agentsApi(
  '/api/session/related?runtime=' + encodeURIComponent(runtime) + '&id=' + encodeURIComponent(sessionID));
// loadSessionTags accepts one session identity or a list of exact alternates
// (tags are written under the group's catalog-else-native id — the same
// identity class the projection join matches; g4 plan §3).
const loadSessionTags = sessionID => {
  const ids = sessionIdentities(Array.isArray(sessionID) ? { alternates: sessionID } : { id: sessionID });
  return agentsApi('/api/orchestration/tags?' + ids.map(id => 'session_id=' + encodeURIComponent(id)).join('&'));
};

// sessionIdentities returns the deduped exact identity alternates of one
// session selection: artifact id, vendor meta id, vendor thread id (g4 plan
// §3 — three forms, never guessed, never fuzzy).
function sessionIdentities(selection = {}) {
  const raw = Array.isArray(selection.alternates)
    ? selection.alternates
    : [selection.id, selection.meta_id, selection.thread_id];
  return [...new Set(raw.map(id => String(id || '').trim()).filter(Boolean))];
}

// attachAgentToSession creates a session-scoped natural-watching binding for
// one imported follower/helper profile (natural-session plan B-GUI): scope
// pinned to THIS session, watch_natural true, through the same PUT/CAS path
// the settings page uses. The daemon's absent-token discipline (G-4 §2.3)
// guards collisions: binding id `agent-<profile_id>`, created only under the
// daemon-published absent token for that id.
const attachAgentToSession = (input, absentToken) => agentsApi(
  '/api/orchestration/agents/' + encodeURIComponent(input.binding_id), {
    method: 'PUT', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ ...input, expected_state_token: absentToken, confirmed: true }),
  });

const loadGroupNotes = groupID => agentsApi(
  '/api/orchestration/groups/' + encodeURIComponent(groupID) + '/notes');

const postGroupNote = (groupID, body) => agentsApi(
  '/api/orchestration/groups/' + encodeURIComponent(groupID) + '/notes', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ body, confirmed: true }),
  });

const retractGroupNote = noteID => agentsApi(
  '/api/orchestration/notes/' + encodeURIComponent(noteID) + '/retract', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ confirmed: true }),
  });

// agentWatchesSession reports whether an ENABLED agent binding's scope covers
// a session, mirroring the host's trigger-time bindingScopeMatches exactly
// (g4 plan §5.9 — one "watching" semantic; anything looser is a lie about
// what will actually trigger):
//   scope_runtime  — must equal the session's runtime when set
//   scope_session  — must equal ANY exact identity alternate when set
//                    (artifact id, vendor meta id, vendor thread id)
//   project_root   — must equal the session's cwd EXACTLY when set (the host
//                    matches the task's working directory by cleaned
//                    equality, so a subdirectory session never triggers)
// Never a heuristic: no title/timestamp matching, no prefix containment.
function agentWatchesSession(agent, { runtime = '', sessionId = '', sessionIds = null, cwd = '' } = {}) {
  if (!agent || agent.state !== 'enabled') return false;
  if (agent.scope_runtime && agent.scope_runtime !== runtime) return false;
  const ids = sessionIdentities({ alternates: Array.isArray(sessionIds) ? sessionIds : [sessionId] });
  if (agent.scope_session && !ids.includes(agent.scope_session)) return false;
  const trimSlash = value => value.length > 1 && value.endsWith('/') ? value.slice(0, -1) : value;
  const root = trimSlash(String(agent.project_root || ''));
  if (root && trimSlash(String(cwd || '')) !== root) return false;
  return true;
}

export { loadAgents, saveAgent, disableAgent, rerouteParkedPreview, rerouteParkedApply, attachAgentToSession, loadSessionTags, loadRelatedSessions, loadGroupNotes, postGroupNote, retractGroupNote, agentWatchesSession, sessionIdentities };
