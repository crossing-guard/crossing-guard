import { el } from '../core.js';
import { managedProjection } from './managed-api.js';
import { loadAgents, agentWatchesSession, sessionIdentities } from './agents-api.js';

async function mountSessionOrchestration(row, runtime, session, context = {}) {
  // Every exact identity this session is known under (artifact id, vendor
  // meta id, vendor thread id): runs are recorded under whichever identity
  // the task row carried, so the header counters must read all of them
  // (g4 plan §3 — same alternates the strip uses).
  const identities = sessionIdentities({ id: session, meta_id: context.meta_id, thread_id: context.thread_id });
  const button = el('button', 'btn', 'Agents');
  // The session-agents panel (right column) is the ONE managed-work surface.
  button.onclick = () => document.dispatchEvent(new CustomEvent('cg:agents-panel-open', { detail: { runtime, session } }));
  row.appendChild(button);
  // "⚖ n watching" — which enabled agents cover this session (deterministic
  // scope match, agents-api.js). Clicking opens the right-column agent panel.
  void loadAgents().then(payload => {
    if (!row.isConnected) return;
    const scope = { runtime, sessionIds: identities, cwd: String(context.cwd || '') };
    const watching = (payload.agents || []).filter(agent => agentWatchesSession(agent, scope));
    if (!watching.length) return;
    const indicator = el('button', 'btn session-agents-watching', '⚖ ' + watching.length + ' watching');
    indicator.title = 'Agents watching this session: ' + watching.map(agent => String(agent.profile_name || agent.profile_id || agent.binding_id)).join(', ');
    indicator.onclick = () => document.dispatchEvent(new CustomEvent('cg:agents-panel-open', { detail: { runtime, session } }));
    row.appendChild(indicator);
  }).catch(() => { /* the agents route is additive; the header stays quiet without it */ });
  try { const projection = await managedProjection(runtime, identities.length ? identities : session); if (!button.isConnected) return; const active = (projection.runs || []).filter(run => ['admitted', 'running'].includes(run.state)).length; const needs = (projection.runs || []).filter(run => run.state === 'completed' && ['draft_reply', 'request_interrupt', 'launch_profile'].includes(run.action)).length; const total = (projection.runs || []).length; if (active) button.textContent = active + (active === 1 ? ' agent running' : ' agents running'); else if (needs) button.textContent = needs + ' agent item' + (needs === 1 ? '' : 's') + ' need review'; else if (total) button.textContent = total + ' agent item' + (total === 1 ? '' : 's'); }
  catch { button.title = 'Agent run status is unavailable; open to retry.'; }
}
export { mountSessionOrchestration };
