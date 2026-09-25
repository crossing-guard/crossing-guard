// Right-column agent strip + in-place agent panel (GUI design §2, G-1 = A).
//
// ONE new info-panel provider: `session.agents`, header zone, order below
// every existing provider's 0 so the strip renders ABOVE the session-evidence
// header and the primary tab row — the pinned band the owner asked for. No
// existing provider is modified; the three-view (Changes/Effects/Plan)
// contract stays intact below it.
//
// Data: /api/orchestration/agents filtered by scope to THIS session, plus the
// managed projection filtered to this session (runs, relationships, claims).
// The provider also publishes the client-side agent-turn index that the
// transcript decorators consume (agent-turn-decorators.js).
//
// No raw HTML injection anywhere in this file: claim text and refs are
// model-authored input, and ref anchors are built with the DOM refAnchor()
// helper (core.js) so the orchestration contract-test discipline stays true.
import { el, refAnchor, SEV_CHIP } from '../core.js';
import { provider } from '../infopanel.js';
import * as refs from '../refs.js';
import { loadAgents, disableAgent, attachAgentToSession, loadSessionTags, loadRelatedSessions, loadGroupNotes, postGroupNote, retractGroupNote, agentWatchesSession, sessionIdentities } from './agents-api.js';
import { managedProjection, sendFollowerDraft, actOnManagedRun, resumeWithCorrection } from './managed-api.js';
import { loadProfile } from './profile-api.js';
import { loadChatCapabilities } from '../chat-capabilities.js';
import { buildAgentTurnIndex, publishAgentTurnIndex } from './agent-turn-decorators.js';

/* ---------- pure view-model helpers (unit-tested) ---------- */

// Prefer the newest completed claim in the API’s newest-first order.
export function latestRunByBinding(runs = []) {
  const latest = new Map();
  for (const run of runs) {
    if (!run || !run.binding_id) continue;
    const current = latest.get(run.binding_id);
    if (!current || (run.state === 'completed' && current.state !== 'completed')) latest.set(run.binding_id, run);
  }
  return latest;
}

// groupCounters derives the honest loop counters from relationship rows for
// one set of group ids: highest recorded cycle and how many reply turns exist.
// Token SPEND is not in this projection, so it is not invented here — only
// budgets (limits) render, labeled as budgets.
// helperSessionForBinding picks the pairing group whose helper session
// serves THIS source session (schema 30): the binding's newest group whose
// root identity is one of the session's identities. Null when no turn has
// reported a helper session yet — the row then says nothing about it.
export function helperSessionForBinding(groups = [], bindingID = '', identities = []) {
  const ids = new Set(identities.map(String));
  let best = null;
  for (const group of groups) {
    if (String(group?.binding_id || '') !== String(bindingID)) continue;
    if (!String(group?.helper_native_session_id || '')) continue;
    if (ids.size && !ids.has(String(group.root_catalog_session_id || '')) && !ids.has(String(group.root_native_session_id || ''))) continue;
    if (!best || Number(group.updated_at || 0) > Number(best.updated_at || 0)) best = group;
  }
  return best;
}

export function helperSessionLabel(group = {}) {
  const turns = Number(group?.helper_turns || 0);
  let text = 'helper session \u00b7 ' + turns + ' turn' + (turns === 1 ? '' : 's');
  if (Number(group?.pending_event_id || 0)) text += ' \u00b7 1 waiting';
  const dropped = Number(group?.pending_dropped || 0);
  if (dropped) text += ' \u00b7 ' + dropped + ' dropped';
  return text;
}

export function groupCounters(relationships = [], groupIDs = new Set()) {
  let cycles = 0, replies = 0;
  for (const rel of relationships) {
    if (!groupIDs.has(rel.group_id)) continue;
    if (Number.isFinite(rel.cycle)) cycles = Math.max(cycles, rel.cycle);
    if (rel.reply_task_id) replies++;
  }
  return { cycles, replies };
}

// findingRefModel classifies one claim finding ref for rendering.
//   resolution 'resolved' (or a resolved_path) → a real anchor
//   any other stated resolution → flagged "unverified reference"
//   no stated resolution → ask the injected project-root resolver; still
//     unverified when it cannot answer. Never a link that 404s.
export function findingRefModel(ref = {}, resolve = null) {
  const kind = String(ref.kind || 'file');
  const line = Number(ref.line) || 0;
  const path = String(ref.path || '');
  const label = (path || String(ref.anchor || ref.session || 'reference')) + (line ? ':' + line : '');
  if (kind === 'session') {
    return { kind, label: 'session ' + String(ref.session || 'unknown'),
      state: ref.resolution === 'resolved' ? 'resolved-session' : 'unverified',
      reason: ref.resolution === 'resolved' ? '' : 'session reference ' + String(ref.resolution || 'unresolved') };
  }
  if (kind === 'event') {
    return { kind, label: 'event ' + String(ref.anchor || 'unknown') + (ref.session ? ' · ' + ref.session : ''),
      state: ref.resolution === 'resolved' ? 'resolved-session' : 'unverified',
      reason: ref.resolution === 'resolved' ? '' : 'event reference ' + String(ref.resolution || 'unresolved') };
  }
  if (ref.resolution === 'resolved' || ref.resolved_path) {
    return { kind, state: 'resolved', path: String(ref.resolved_path || path), line, label };
  }
  if (ref.resolution) {
    return { kind, state: 'unverified', label, reason: String(ref.resolution) };
  }
  if (resolve && path) {
    let target = null;
    try { target = resolve(path + (line ? ':' + line : '')); } catch { target = null; }
    if (target?.state === 'resolved') return { kind, state: 'resolved', path: target.path, line: target.line || line, label };
    return { kind, state: 'unverified', label, reason: String(target?.reason || target?.state || 'unresolved') };
  }
  return { kind, state: 'unverified', label, reason: 'resolution not reported' };
}

// unresolvedRefCount surfaces the strip-level "n unverified" flag.
export function unresolvedRefCount(findings = []) {
  let count = 0;
  for (const finding of findings) {
    for (const ref of finding?.refs || []) {
      if (findingRefModel(ref).state === 'unverified') count++;
    }
  }
  return count;
}

// Completion is a claim outcome; the separate receipt describes delivery only.
export function deliveryStatusText(run = {}) {
  if (run.action !== 'send_message') return '';
  const receipt = run.detail?.delivery;
  if (!receipt) return 'Message proposed · automatic delivery was not requested.';
  // "delivered" means handed to the session's own boundary (its hook or
  // plugin carried it); vendor consumption is never asserted from here.
  const labels = {
    accepted: 'Message queued · consumption is not confirmed.',
    delivered: 'Delivered to the session\'s boundary · consumption is not separately confirmed.',
    expired: 'Message expired · no boundary arrived in time; not delivered.',
    pending: 'Delivery unconfirmed · intent recorded; no receipt. Do not retry automatically.',
    unavailable: 'Message delivery unavailable.',
    unknown: 'Delivery outcome unknown · do not retry automatically.',
  };
  const boundary = receipt.boundary && receipt.state === 'accepted' ? ' Boundary: ' + String(receipt.boundary) + '.' : '';
  return (labels[receipt.state] || labels.unknown) + boundary + (receipt.detail ? ' ' + String(receipt.detail) : '');
}

/* ---------- DOM builders ---------- */

function findingRefNode(model) {
  if (model.state === 'resolved') {
    const anchor = refAnchor(model.path, model.label);
    if (model.line) anchor.dataset.refLine = String(model.line);
    return anchor;
  }
  const wrap = el('span', 'agent-ref-unverified');
  wrap.appendChild(el('span', 'ref ref-miss', model.label));
  const flag = el('span', 'chip st-disputed agent-ref-flag', 'unverified reference');
  if (model.reason) flag.title = model.reason;
  wrap.appendChild(flag);
  return wrap;
}

function claimFindings(run, scope) {
  const findings = Array.isArray(run.detail?.findings) ? run.detail.findings : [];
  const host = el('div', 'agent-claim-findings');
  for (const finding of findings) {
    const row = el('div', 'agent-claim-finding');
    row.appendChild(el('span', 'chip ' + (SEV_CHIP[finding.severity] || 'st-draft'), String(finding.severity || 'note')));
    row.appendChild(el('span', 'agent-claim-statement', String(finding.statement || '')));
    const refList = el('span', 'agent-claim-refs');
    for (const ref of finding.refs || []) refList.appendChild(findingRefNode(findingRefModel(ref, scope?.resolve)));
    if (refList.childNodes.length) row.appendChild(refList);
    host.appendChild(row);
  }
  return { host, count: findings.length };
}

function stateChip(run) {
  const state = String(run?.state || 'unknown').replaceAll('_', ' ');
  const cls = run?.state === 'failed' ? 'st-disputed' : (['admitted', 'running'].includes(run?.state) ? 'st-stale' : '');
  return el('span', 'chip ' + cls, state);
}

/* ---------- the expanded claim card ---------- */

function claimCard(agent, run, ctx) {
  const card = el('article', 'agent-claim-card');
  // Attribution header: agent name / type / priority + goal line.
  const head = el('div', 'row agent-claim-head');
  head.append(el('strong', '', '⚖ ' + String(agent?.profile_name || agent?.profile_id || run.profile_id || 'agent')),
    el('span', 'chip', String(agent?.role || run.role || 'agent')),
    el('span', 'sub', 'priority ' + String(agent?.priority ?? '—')));
  card.appendChild(head);
  if (agent?.profile_description) card.appendChild(el('div', 'sub', String(agent.profile_description)));
  if (agent?.prompt_excerpt) {
    const prompt = document.createElement('details');
    prompt.className = 'agent-prompt';
    const summary = document.createElement('summary');
    summary.textContent = 'Prompt · ' + String(agent.prompt_excerpt).slice(0, 80) + '…';
    prompt.appendChild(summary);
    const body = el('pre', 'agent-prompt-body', 'Loading full prompt…');
    prompt.appendChild(body);
    let loaded = false;
    prompt.addEventListener('toggle', () => {
      if (!prompt.open || loaded) return;
      loaded = true;
      loadProfile(agent.profile_id).then(detail => { body.textContent = String(detail.source || agent.prompt_excerpt); })
        .catch(error => { body.textContent = 'Full prompt unavailable: ' + String(error?.message || error) + '\n\nExcerpt:\n' + String(agent.prompt_excerpt); });
    });
    card.appendChild(prompt);
  }
  if (!run) {
    card.appendChild(el('div', 'sub', 'Watching · no run for this session yet.'));
    return card;
  }
  // Verdict + typed findings with real references, bound to the HELPER's
  // project root — never the currently-open session's (GUI design §5.1).
  const scope = refs.forProject(String(run.project_root || ctx.selection?.cwd || ''));
  void scope.start();
  const verdictRow = el('div', 'row agent-claim-verdict');
  verdictRow.append(stateChip(run), el('strong', '', String(run.detail?.verdict || run.message || 'No claim message.')));
  card.appendChild(verdictRow);
  const delivery = deliveryStatusText(run);
  if (delivery) card.appendChild(el('div', 'sub agent-delivery-status', delivery));
  const findings = claimFindings(run, scope);
  if (findings.count) {
    card.appendChild(findings.host);
    const unresolved = unresolvedRefCount(run.detail?.findings || []);
    if (unresolved) card.appendChild(el('div', 'sub agent-claim-unresolved', unresolved + ' unverified reference' + (unresolved === 1 ? '' : 's') + ' — flagged above, never linked.'));
  } else if (run.citations?.length) {
    // v1 claim (plain message + string citations) — renders forever.
    card.appendChild(el('div', 'sub', 'Cited supplied facts: ' + run.citations.map(String).join(' · ')));
  }
  const tags = Array.isArray(run.detail?.tags) ? run.detail.tags : [];
  if (tags.length) {
    const tagRow = el('div', 'agent-claim-tags');
    tagRow.appendChild(el('span', 'sub', 'tags applied: '));
    tags.forEach(tag => tagRow.appendChild(el('span', 'chip cl-observed', String(tag))));
    card.appendChild(tagRow);
  }
  // Evidence trail — only when the run detail actually reports it; an
  // unreported trail is omitted, not invented.
  const filesRead = Number(run.detail?.files_read);
  const commandsRun = Number(run.detail?.commands_run);
  if (Number.isFinite(filesRead) && Number.isFinite(commandsRun) && (filesRead || commandsRun)) {
    card.appendChild(el('div', 'sub agent-evidence-trail', 'read ' + filesRead + ' file' + (filesRead === 1 ? '' : 's') + ' · ran ' + commandsRun + ' command' + (commandsRun === 1 ? '' : 's')));
  }
  // Draft reply keeps the existing Edit & send path (/send route).
  if (run.state === 'completed' && run.action === 'draft_reply') {
    const editor = document.createElement('textarea');
    editor.rows = 4; editor.value = String(run.message || '');
    editor.setAttribute('aria-label', 'Editable agent reply draft');
    const send = el('button', 'btn primary', 'Edit & send to exact parent');
    send.onclick = async () => {
      send.disabled = true;
      try { await sendFollowerDraft(run.run_id, editor.value); send.textContent = 'Reply task admitted ✓'; }
      catch (error) { send.disabled = false; card.prepend(el('div', 'banner', String(error?.message || error))); }
    };
    card.append(editor, send);
  }
  // Manual confirmation for a non-auto helper's pending action claim
  // (launch_profile / request_interrupt) through the existing owners.
  if (run.state === 'completed' && ['launch_profile', 'request_interrupt'].includes(run.action)) {
    const act = el('button', 'btn primary', run.action === 'launch_profile' ? 'Launch pinned child' : 'Interrupt exact task');
    act.onclick = async () => {
      act.disabled = true;
      try { await actOnManagedRun(run.run_id); act.textContent = 'Requested through task owner ✓'; }
      catch (error) { act.disabled = false; card.prepend(el('div', 'banner', String(error?.message || error))); }
    };
    card.appendChild(act);
  }
  // Corrective resume of the finished source task, operator-edited.
  if (run.state === 'completed') {
    const correction = document.createElement('details');
    const summary = document.createElement('summary');
    summary.textContent = 'Resume with correction';
    const editor = document.createElement('textarea');
    editor.rows = 4; editor.value = String(run.message || '');
    editor.setAttribute('aria-label', 'Editable corrective resume');
    const resume = el('button', 'btn primary', 'Start a new corrective turn');
    resume.onclick = async () => {
      resume.disabled = true;
      try { await resumeWithCorrection(run.run_id, editor.value); resume.textContent = 'Corrective task admitted ✓'; }
      catch (error) { resume.disabled = false; correction.prepend(el('div', 'banner', String(error?.message || error))); }
    };
    correction.append(summary, editor, resume);
    card.appendChild(correction);
  }
  // No "Re-run now": no such API exists yet — a dead button would be a lie.
  card.appendChild(feedbackBox(run));
  card.appendChild(groupNotesSection(run));
  return card;
}

// feedbackBox — two honest scopes (plan §7): a group note that shapes THIS
// session's future reviews, and "always", which is the real profile-revision
// path (re-import PROFILE.md in Settings) — no fake in-place revision editing.
function feedbackBox(run) {
  const box = el('div', 'agent-feedback');
  box.appendChild(el('strong', '', 'Feedback to this agent'));
  const input = document.createElement('textarea');
  input.rows = 3; input.placeholder = 'What should future reviews of this session know?';
  input.setAttribute('aria-label', 'Feedback for future agent reviews');
  const actions = el('div', 'row');
  const sessionScope = el('button', 'btn primary', "Apply to this session's future reviews");
  sessionScope.onclick = async () => {
    if (!input.value.trim() || !run.group_id) return;
    sessionScope.disabled = true;
    try {
      await postGroupNote(run.group_id, input.value.trim());
      sessionScope.textContent = 'Note recorded ✓';
      input.value = '';
      setTimeout(() => { sessionScope.textContent = "Apply to this session's future reviews"; sessionScope.disabled = false; }, 1200);
    } catch (error) {
      sessionScope.disabled = false;
      box.prepend(el('div', 'banner', String(error?.message || error)));
    }
  };
  const always = el('button', 'btn', 'Always → edit PROFILE.md and re-import');
  always.title = 'Permanent changes go through the profile import flow so every revision stays reviewed and pinned.';
  always.onclick = () => {
    document.dispatchEvent(new CustomEvent('cg:settings-subpage', { detail: 'agents' }));
    document.dispatchEvent(new CustomEvent('cg:nav', { detail: 'settings' }));
  };
  actions.append(sessionScope, always);
  box.append(input, actions,
    el('div', 'sub', 'Session notes are injected into future prompts for this source session and are visible in the context manifest. Feedback edits future inputs only.'));
  return box;
}

function groupNotesSection(run) {
  const section = el('div', 'agent-group-notes');
  if (!run.group_id) return section;
  const details = document.createElement('details');
  const summary = document.createElement('summary');
  summary.textContent = 'Group notes';
  const body = el('div', 'agent-group-notes-body');
  details.append(summary, body);
  let loaded = false;
  details.addEventListener('toggle', async () => {
    if (!details.open || loaded) return;
    loaded = true;
    body.appendChild(el('div', 'sub', 'Loading notes…'));
    try {
      const response = await loadGroupNotes(run.group_id);
      if (!body.isConnected) return;
      body.replaceChildren();
      const notes = Array.isArray(response.notes) ? response.notes : [];
      if (!notes.length) { body.appendChild(el('div', 'sub', 'No notes for this group.')); return; }
      for (const note of notes) {
        const row = el('div', 'agent-group-note' + (note.retracted_at ? ' retracted' : ''));
        row.appendChild(el('span', '', String(note.body || '')));
        if (note.retracted_at) row.appendChild(el('span', 'chip', 'retracted'));
        else {
          const retract = el('button', 'btn', 'Retract');
          retract.onclick = async () => {
            retract.disabled = true;
            try { await retractGroupNote(note.note_id || note.id); row.classList.add('retracted'); retract.replaceWith(el('span', 'chip', 'retracted')); }
            catch (error) { retract.disabled = false; body.prepend(el('div', 'banner', String(error?.message || error))); }
          };
          row.appendChild(retract);
        }
        body.appendChild(row);
      }
    } catch (error) {
      if (body.isConnected) body.replaceChildren(el('div', 'banner', 'Notes unavailable: ' + String(error?.message || error)));
    }
  });
  section.appendChild(details);
  return section;
}

/* ---------- the strip ---------- */

let activeStrip = null; // the currently-rendered strip element, for cg:agents-panel-open

function agentStripRow(agent, run, counters, expandHost, ctx, projectionFailed = false, helperGroup = null) {
  const row = el('div', 'agent-strip-row');
  const summary = el('button', 'agent-strip-summary');
  summary.type = 'button';
  summary.setAttribute('aria-expanded', 'false');
  summary.append(el('span', 'agent-strip-name', '⚖ ' + String(agent?.profile_name || agent?.profile_id || run?.profile_id || 'agent')),
    el('span', 'chip', String(agent?.role || run?.role || 'agent')));
  if (run) {
    summary.appendChild(stateChip(run));
    const verdict = String(run.detail?.verdict || '');
    if (verdict) summary.appendChild(el('span', 'agent-strip-verdict', verdict));
    const unresolved = unresolvedRefCount(run.detail?.findings || []);
    if (unresolved) summary.appendChild(el('span', 'chip st-disputed', unresolved + ' unverified'));
  } else if (projectionFailed) {
    // The runs READ failed — "no runs yet" would be a lie about recorded state.
    summary.appendChild(el('span', 'chip st-disputed', 'run status unavailable'));
  } else {
    summary.appendChild(el('span', 'sub', 'watching · no runs yet'));
  }
  if (counters && (counters.cycles || counters.replies)) {
    const counter = el('span', 'agent-strip-counter', 'cycle ' + counters.cycles + ' · ' + counters.replies + ' repl' + (counters.replies === 1 ? 'y' : 'ies'));
    counter.title = 'Loop counters from recorded relationships. Token budgets bound the next admission; spend is not reported by this projection.';
    summary.appendChild(counter);
  }
  if (helperGroup) {
    // ONE helper session per source session (schema 30): the row links it
    // and counts its turns; the rail folds that session under this one.
    const open = el('a', 'ref ref-ok agent-strip-helper-session', helperSessionLabel(helperGroup));
    open.href = '/?runtime=' + encodeURIComponent(String(helperGroup.helper_runtime || ''))
      + '&session=' + encodeURIComponent(String(helperGroup.helper_native_session_id || ''));
    open.title = String(helperGroup.helper_native_session_id || '');
    summary.appendChild(open);
  }
  row.appendChild(summary);
  // G-3: Disable rides the strip header, wired to the agent disable route.
  if (agent?.state === 'enabled' && agent.binding_id) {
    const disable = el('button', 'btn agent-strip-disable', 'Disable');
    disable.title = 'Disable new triggers for this agent (existing runs are untouched).';
    disable.onclick = async event => {
      event.stopPropagation();
      disable.disabled = true;
      try { await disableAgent(agent.binding_id, agent.state_token); disable.replaceWith(el('span', 'chip', 'disabled ✓')); }
      catch (error) { disable.disabled = false; row.appendChild(el('div', 'banner', String(error?.message || error))); }
    };
    row.appendChild(disable);
  }
  let card = null;
  const expand = () => {
    if (card) { card.remove(); card = null; summary.setAttribute('aria-expanded', 'false'); return; }
    card = claimCard(agent, run, ctx);
    summary.setAttribute('aria-expanded', 'true');
    expandHost.appendChild(card);
  };
  summary.onclick = expand;
  row.cgExpand = () => { if (!card) expand(); };
  return row;
}

// attachAgentControl renders the Attach-agent flow for one live session
// (natural-session plan B-GUI): choose an imported follower/helper, accept
// its default runtime route or override model/runtime inline, scope = THIS
// session (watch_natural). Saves through the binding PUT with the daemon's
// absent token (G-4 CAS discipline). The model picker is the honest preset
// list from the runtime's published capabilities plus Custom… (M12) — it is
// labeled as presets, never a fake installed-models list.
function attachAgentControl(ctx, identities) {
  const selection = ctx.selection || {};
  const runtime = String(selection.runtime || '');
  const sessionID = String(selection.id || '');
  const cwd = String(selection.cwd || '');
  const host = el('div', 'agent-attach');
  const status = el('span', 'sub', '');
  const open = el('button', 'btn', 'Attach agent');
  open.title = 'Attach an imported follower or helper to THIS session (watches its natural lifecycle).';
  open.onclick = async () => {
    open.disabled = true;
    status.textContent = 'Loading agents…';
    let payload;
    try {
      payload = await loadAgents();
    } catch (error) {
      open.disabled = false;
      status.textContent = 'Agent list unavailable: ' + String(error?.message || error);
      return;
    }
    const agents = payload?.agents || [];
    const options = (payload?.profiles || []).filter(option =>
      option?.compatible && ['follower', 'helper'].includes(String(option.agent_type || '')));
    const absent = payload?.absent_state_tokens || {};
    const unbound = options.filter(option => !agents.some(a => a.binding_id === 'agent-' + option.profile_id));
    if (!unbound.length) {
      open.disabled = false;
      status.textContent = 'No unbound follower/helper profiles. Import one in Settings → Agents first.';
      return;
    }
    // Replace the button with the inline form.
    const form = el('div', 'agent-attach-form');
    const pick = el('select');
    pick.setAttribute('aria-label', 'Agent to attach');
    unbound.forEach(option => {
      const item = el('option');
      item.value = option.profile_id;
      item.textContent = String(option.name || option.profile_id) + ' · ' + String(option.agent_type || 'agent');
      pick.appendChild(item);
    });
    form.appendChild(pick);
    // Runtime route + model picker (presets + custom, honestly labeled).
    const routeRow = el('div', 'row');
    const runtimeSelect = el('select');
    runtimeSelect.setAttribute('aria-label', 'Agent runtime');
    const modelSelect = el('select');
    modelSelect.setAttribute('aria-label', 'Agent model');
    const syncRoute = capabilities => {
      runtimeSelect.replaceChildren();
      const list = (capabilities || []).filter(item => item.can_start
        && (item.modes || []).some(mode => mode.risk === 'normal'));
      list.forEach(item => {
        const option = el('option');
        option.value = item.runtime;
        option.textContent = item.display_name;
        runtimeSelect.appendChild(option);
      });
      const syncModels = () => {
        modelSelect.replaceChildren();
        const capability = list.find(item => item.runtime === runtimeSelect.value);
        const defaultOption = el('option');
        defaultOption.value = '';
        defaultOption.textContent = 'Default model';
        modelSelect.appendChild(defaultOption);
        (capability?.models || []).forEach(model => {
          const option = el('option');
          option.value = model.id;
          option.textContent = model.label + (model.custom ? '' : ' · preset');
          modelSelect.appendChild(option);
        });
        const custom = el('option');
        custom.value = 'custom';
        custom.textContent = 'Custom…';
        modelSelect.appendChild(custom);
      };
      runtimeSelect.onchange = syncModels;
      syncModels();
    };
    let capabilities = [];
    try { capabilities = await loadChatCapabilities(); } catch { capabilities = []; }
    syncRoute(capabilities);
    routeRow.append(runtimeSelect, modelSelect);
    form.appendChild(routeRow);
    const customInput = el('input');
    customInput.placeholder = 'exact provider/model';
    customInput.style.width = '200px';
    customInput.classList.add('hidden');
    const modelRow = el('div', 'row');
    modelRow.appendChild(customInput);
    form.appendChild(modelRow);
    modelSelect.onchange = () => {
      customInput.classList.toggle('hidden', modelSelect.value !== 'custom');
    };
    const save = el('button', 'btn primary', 'Attach to this session');
    const cancel = el('button', 'btn', 'Cancel');
    const actions = el('div', 'row');
    actions.append(save, cancel, status);
    form.appendChild(actions);
    open.replaceWith(form);
    cancel.onclick = () => { form.replaceWith(open); open.disabled = false; status.textContent = ''; };
    save.onclick = async () => {
      save.disabled = true;
      const option = unbound.find(item => item.profile_id === pick.value);
      if (!option) { save.disabled = false; status.textContent = 'Profile unavailable.'; return; }
      const bindingID = 'agent-' + option.profile_id;
      const token = absent[bindingID];
      if (!token) { save.disabled = false; status.textContent = 'No creation token for this agent; manage it in Settings → Agents.'; return; }
      const scope = identities[0] || sessionID;
      try {
        await attachAgentToSession({
          binding_id: bindingID,
          profile_id: option.profile_id,
          profile_source_digest: option.source_digest,
          profile_bundle_digest: option.bundle_digest,
          project_root: cwd,
          scope_runtime: runtime,
          scope_session: scope,
          runtime: runtimeSelect.value,
          model: modelSelect.value === 'custom' ? customInput.value.trim() : modelSelect.value,
          mode: '',
          granted_authority: [],
          watch_natural: true,
        }, token);
        status.textContent = 'Attached ✓ — watching this session.';
        form.replaceWith(el('div', 'banner', 'Agent attached. Its run appears here when the session reaches a signal it selects.'));
      } catch (error) {
        save.disabled = false;
        status.textContent = 'Not attached: ' + String(error?.message || error)
          + (error?.code === 'state_conflict' ? ' — the binding changed elsewhere; check Settings → Agents.' : '');
      }
    };
  };
  host.append(open, status);
  return host;
}

async function renderAgentStrip(ctx, box) {
  const selection = ctx.selection || {};
  const runtime = String(selection.runtime || '');
  const sessionID = String(selection.id || '');
  const cwd = String(selection.cwd || '');
  // All exact identity alternates of this session (artifact id, vendor meta
  // id, vendor thread id): runs and tags are recorded under whichever
  // identity the task row carried, so a single-id query missed runs for any
  // session whose identities diverge — resumed codex threads above all
  // (g4 plan §3; parent verification limitation #3).
  const identities = sessionIdentities(selection);
  box.replaceChildren(el('div', 'sub', 'Checking which agents watch this session…'));
  const [agentsResult, projectionResult, tagsResult, relatedResult] = await Promise.allSettled([
    loadAgents(),
    identities.length ? managedProjection(runtime, identities) : Promise.reject(new Error('session identity unavailable')),
    identities.length ? loadSessionTags(identities) : Promise.reject(new Error('session identity unavailable')),
    loadRelatedSessions(runtime, sessionID),
  ]);
  if (!box.isConnected) return;
  const render = () => { if (box.isConnected) renderAgentStrip(ctx, box); };
  box.replaceChildren();
  const strip = el('div', 'agent-strip');
  strip._reRender = render;
  box.appendChild(strip);
  activeStrip = strip;

  const projection = projectionResult.status === 'fulfilled' ? projectionResult.value : null;
  if (projection) publishAgentTurnIndex(runtime + '/' + sessionID, buildAgentTurnIndex(projection));

  if (agentsResult.status === 'rejected' && projectionResult.status === 'rejected') {
    strip.appendChild(el('div', 'sub', 'Agent status unavailable: ' + String(agentsResult.reason?.message || agentsResult.reason)));
    return;
  }
  const agents = agentsResult.status === 'fulfilled' ? (agentsResult.value.agents || []) : [];
  const watching = agents.filter(agent => agentWatchesSession(agent, { runtime, sessionIds: identities, cwd }));
  const runs = projection?.runs || [];
  const relationships = projection?.relationships || [];
  const groups = projection?.groups || [];
  const latest = latestRunByBinding(runs);

  const projectionFailed = projectionResult.status === 'rejected';
  if (!watching.length && !runs.length) {
    const empty = el('div', 'sub agent-strip-empty', 'No agents watch this session.');
    const attach = attachAgentControl(ctx, identities);
    strip.append(empty, attach);
    // A failed runs READ is not evidence of no runs: historical runs from
    // disabled/legacy bindings may exist but be unrenderable right now
    // (postwork SF-4).
    if (projectionFailed) strip.appendChild(el('div', 'chip st-disputed', 'run status unavailable — reload to retry'));
    return;
  }

  const attachBar = el('div', 'row');
  attachBar.appendChild(attachAgentControl(ctx, identities));
  strip.appendChild(attachBar);

  const expandHost = el('div', 'agent-strip-expansion');
  const seenBindings = new Set();
  for (const agent of watching) {
    seenBindings.add(agent.binding_id);
    const run = latest.get(agent.binding_id) || null;
    const groupIDs = new Set(runs.filter(item => item.binding_id === agent.binding_id).map(item => item.group_id));
    strip.appendChild(agentStripRow(agent, run, groupCounters(relationships, groupIDs), expandHost, ctx, projectionFailed,
      helperSessionForBinding(groups, agent.binding_id, identities)));
  }
  // Runs from bindings that no longer watch (disabled, historical role names)
  // still render — history is not hidden by today's scope.
  for (const [bindingID, run] of latest) {
    if (seenBindings.has(bindingID)) continue;
    const agent = agents.find(item => item.binding_id === bindingID) || null;
    const groupIDs = new Set(runs.filter(item => item.binding_id === bindingID).map(item => item.group_id));
    strip.appendChild(agentStripRow(agent, run, groupCounters(relationships, groupIDs), expandHost, ctx, false,
      helperSessionForBinding(groups, bindingID, identities)));
  }
  strip.appendChild(expandHost);

  if (tagsResult.status === 'fulfilled' && Array.isArray(tagsResult.value.tags) && tagsResult.value.tags.length) {
    const tagRow = el('div', 'agent-strip-tags');
    tagRow.appendChild(el('span', 'sub', 'session tags: '));
    for (const item of tagsResult.value.tags) {
      const chip = el('span', 'chip cl-observed', String(item.tag || ''));
      chip.title = 'applied by ' + String(item.agent_key || item.binding_id || 'unknown agent')
        + (item.expires_at ? ' · expires ' + item.expires_at : '');
      tagRow.appendChild(chip);
    }
    strip.appendChild(tagRow);
  }

  if (relatedResult.status === 'fulfilled') {
    const section = relatedSessionsSection(relatedResult.value);
    if (section) strip.appendChild(section);
  }
}

// One provenance-labeled related-sessions presentation: observed native edges
// and caused agent arcs, never merged (plan §4). Rows navigate to the session.
function relatedSessionModel(row = {}) {
  return {
    kind: String(row.kind || ''),
    provenance: String(row.provenance || ''),
    direction: String(row.direction || ''),
    runtime: String(row.runtime || ''),
    sessionId: String(row.session_id || ''),
    label: String(row.label || ''),
    role: String(row.role || ''),
    state: String(row.state || ''),
    cycle: Number(row.cycle || 0),
    unresolved: !!row.unresolved,
  };
}

function relatedSessionsSection(payload) {
  const rows = Array.isArray(payload?.related) ? payload.related.map(relatedSessionModel) : [];
  if (!rows.length) return null;
  const section = el('div', 'agent-related-sessions');
  section.appendChild(el('strong', 'sub', 'Related sessions'));
  for (const row of rows) {
    const line = el('div', 'agent-related-row');
    line.appendChild(el('span', 'chip cl-' + (row.provenance === 'caused' ? 'claimed' : 'observed'), row.provenance));
    let text = row.kind + (row.direction ? ' \u00b7 ' + row.direction : '');
    if (row.role) text += ' \u00b7 ' + row.role;
    if (row.cycle) text += ' \u00b7 cycle ' + row.cycle;
    line.appendChild(el('span', '', text));
    if (row.sessionId) {
      const open = el('a', 'ref ref-ok', (row.runtime ? row.runtime + ' \u00b7 ' : '') + row.sessionId.slice(0, 14) + '\u2026');
      open.href = '/?runtime=' + encodeURIComponent(row.runtime) + '&session=' + encodeURIComponent(row.sessionId);
      line.appendChild(open);
    } else if (row.label) {
      const chip = el('span', 'chip', row.label);
      if (row.unresolved) chip.title = 'unresolved reference';
      line.appendChild(chip);
    }
    if (row.unresolved && row.sessionId) line.appendChild(el('span', 'chip', 'unresolved'));
    section.appendChild(line);
  }
  return section;
}

provider({
  id: 'session.agents', zone: 'header', order: -10, title: 'Agents',
  paneModule: { placement: 'deck' },
  match: ctx => ctx.surface === 'session' && !!ctx.selection,
  render: (ctx, box) => renderAgentStrip(ctx, box),
});

// "⚖ n watching" and the session-header Agents button open this panel — the
// one managed-work surface.
if (typeof document !== 'undefined') {
  document.addEventListener('cg:agents-panel-open', () => {
    if (!activeStrip?.isConnected) return;
    activeStrip.scrollIntoView({ block: 'nearest' });
    activeStrip.querySelector('.agent-strip-row')?.cgExpand?.();
  });
  // Live-run refresh (natural-session plan B-GUI): the live session view
  // dispatches this event when its governance delta channel reports new
  // activity for the open session; the strip re-renders its run rows from the
  // durable projection. No polling loop of its own.
  document.addEventListener('cg:session-activity-delta', () => {
    const strip = activeStrip;
    if (!strip?.isConnected) return;
    const render = strip._reRender;
    if (typeof render === 'function') render();
  });
}
