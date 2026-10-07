import { el } from '../core.js';
import { managedProjection } from './managed-api.js';
import { loadAgents, agentWatchesSession, sessionIdentities, watchScopeIds } from './agents-api.js';
import { isModuleEnabled, revealModule } from '../pane-host.js';
import { agentsLinkWords } from '../task/session-status.js';

// mountSessionOrchestration renders the ONE agents link at the end of the
// session's activity line (session-view-and-console-preferences plan §A3):
// "N to review ›" when agent work waits on the reader, else "N running ›",
// else "N watching ›", else nothing. Clicking reveals the session.agents
// module in the workspace. The link exists only while that module can be
// revealed, so it is never a control that does nothing.
async function mountSessionOrchestration(host, runtime, session, context = {}) {
  // Every id that names this session, as the daemon published them: runs are
  // recorded under whichever identity the task row carried, so the counts
  // must read all of them (g4 plan §3). Watching compares the thread id too
  // (watchScopeIds, child-thread-identity plan D-1).
  const identities = sessionIdentities({ id: session, identities: context.identities });
  const counts = { watching: 0, running: 0, review: 0 };
  const link = el('button', 'activity-agents');
  link.type = 'button';
  // A reveal that failed hides the link until the workspace changes again, so
  // it never stays on screen as a control that does nothing.
  let failed = false;
  link.onclick = () => {
    revealModule('session.agents', { intent: 'expand-first' }).catch(() => { failed = true; paint(); });
  };
  function paint() {
    if (!host.isConnected) return;
    const words = agentsLinkWords(counts);
    link.textContent = words;
    link.classList.toggle('review', counts.review > 0);
    const show = Boolean(words) && !failed && isModuleEnabled('session.agents');
    if (show && !link.isConnected) host.appendChild(link);
    if (!show && link.isConnected) link.remove();
  }
  const onModuleState = () => {
    if (!host.isConnected) { document.removeEventListener('cg:module-state', onModuleState); return; }
    failed = false;
    paint();
  };
  document.addEventListener('cg:module-state', onModuleState);
  const [agents, projection] = await Promise.allSettled([
    loadAgents(),
    managedProjection(runtime, identities.length ? identities : session),
  ]);
  if (agents.status === 'fulfilled') {
    const scope = { runtime, cwd: String(context.cwd || ''), cwdKey: String(context.cwd_key || ''),
      sessionIds: watchScopeIds({ id: session, meta_id: context.meta_id, thread_id: context.thread_id }) };
    counts.watching = (agents.value.agents || []).filter(agent => agentWatchesSession(agent, scope)).length;
  }
  if (projection.status === 'fulfilled') {
    const runs = projection.value.runs || [];
    counts.running = runs.filter(run => ['admitted', 'running'].includes(run.state)).length;
    // The daemon's contract flag, never a list kept here (RT-11).
    counts.review = runs.filter(run => run.awaits_operator === true).length;
  }
  paint();
}
export { mountSessionOrchestration };
