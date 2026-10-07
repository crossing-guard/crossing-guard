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
import { el, refAnchor, SEV_CHIP, api } from '../core.js';
import { provider } from '../infopanel.js';
import * as refs from '../refs.js';
import { loadAgents, disableAgent, attachAgentToSession, loadSessionTags, loadRelatedSessions, loadGroupNotes, postGroupNote, retractGroupNote, agentWatchesSession, sessionIdentities, watchScopeIds, bindingProblem } from './agents-api.js';
import { managedSettings, managedProjection, sendFollowerDraft, actOnManagedRun, resumeWithCorrection } from './managed-api.js';
import { loadProfile } from './profile-api.js';
import { loadChatCapabilities } from '../chat-capabilities.js';
import { routePicker, loadRouteChoices } from './agents/route-picker.js';
import { ROUTE_FAMILY_MANAGED } from './agents/route-model.js';
import { buildAgentTurnIndex, publishAgentTurnIndex } from './agent-turn-decorators.js';
import { takeModuleIntent } from '../pane-host.js';
import { REPLY_NOT_SENT } from '../task/session-status.js';
import { loadRelatedHandoffs } from '../handoff/handoff-session.js';

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

const UNFINISHED_STATES = new Set(['failed', 'suppressed', 'deferred', 'unknown']);

// newerUnfinishedByBinding finds, per binding, the newest run that did not
// complete (failed, refused, deferred, lost) and is NEWER than the claim the
// panel shows — the fact that the agent stopped working since its last
// success (managed-turn-profile-limits plan §4.8). Runs arrive newest-first.
export function newerUnfinishedByBinding(runs = [], shown = latestRunByBinding(runs)) {
  const newer = new Map();
  const reachedShown = new Set();
  for (const run of runs) {
    if (!run || !run.binding_id || reachedShown.has(run.binding_id)) continue;
    if (run === shown.get(run.binding_id)) { reachedShown.add(run.binding_id); continue; }
    if (UNFINISHED_STATES.has(run.state) && !newer.has(run.binding_id)) newer.set(run.binding_id, run);
  }
  return newer;
}

// runOutcomeText is one unfinished run's class and the daemon's recovery text.
export function runOutcomeText(run = {}) {
  if (!UNFINISHED_STATES.has(run.state)) return '';
  const label = String(run.error_class || run.state).replaceAll('_', ' ');
  return run.recovery ? label + ': ' + String(run.recovery) : label;
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
  // A proposed reply the daemon did not carry to the session is a draft for
  // the owner (escalation-delivery plan §6.1) — the class is the daemon's.
  if (run.attention_class === 'draft' && run.detail?.delivery) {
    const why = run.detail.delivery.detail ? ' · ' + String(run.detail.delivery.detail) : '';
    return REPLY_NOT_SENT.charAt(0).toUpperCase() + REPLY_NOT_SENT.slice(1) + why;
  }
  if (run.action !== 'send_message') return '';
  const receipt = run.detail?.delivery;
  if (!receipt) return 'Message proposed · automatic delivery was not requested.';
  // "delivered" means handed to the session's own boundary (its hook or
  // plugin carried it); vendor consumption is never asserted from here.
  const labels = {
    accepted: 'Message queued · consumption is not confirmed.',
    delivered: 'Delivered to the session\'s boundary · consumption is not separately confirmed.',
    expired: 'Message expired · no boundary arrived in time; not delivered.',
    not_requested: 'Message proposed · automatic delivery was not requested.',
    pending: 'Delivery unconfirmed · intent recorded; no receipt. Do not retry automatically.',
    unavailable: 'Message delivery unavailable.',
    unknown: 'Delivery outcome unknown · do not retry automatically.',
  };
  // A socket post is not queued anywhere: the tier replaces the accepted
  // label (session-message-layer plan §5.6, confirming pass C-1).
  if (receipt.tier === 'socket-post' && receipt.state === 'accepted') {
    labels.accepted = 'Sent to the session\'s inbox as a peer message · the vendor\'s inbound controls may hold or drop it; consumption is not confirmed.';
  }
  const boundary = receipt.boundary && receipt.state === 'accepted' ? ' Boundary: ' + String(receipt.boundary) + '.' : '';
  return (labels[receipt.state] || labels.unknown) + boundary + (receipt.detail ? ' ' + String(receipt.detail) : '');
}

// One label family beside the delivery receipts (session-message-cross-vendor
// plan §4/§7): an agent-initiated invocation record's own states. `delivered`
// is reachable only through the boundary handoff; a socket target's terminal
// success stays `accepted` — transport success is never consumption.
export function invocationStatusText(invocation = {}) {
  const labels = {
    pending: 'Waiting for the session\'s boundary · consumption is not confirmed.',
    accepted: 'Sent to the session\'s inbox · the vendor\'s inbound controls may hold or drop it; consumption is not confirmed.',
    delivered: 'Delivered to the session\'s boundary · consumption is not separately confirmed.',
    expired: 'Invocation expired · no boundary arrived in time; not delivered.',
    unavailable: 'Delivery unavailable.',
    unknown: 'Delivery outcome unknown · do not retry automatically.',
    refused: 'Refused before sending.',
  };
  return labels[invocation.state] || labels.unknown;
}

// invocationCard renders one agent-initiated send beside the delivery receipts
// (postwork PW-7): who sent to whom, the terminal label, and the invocation id
// the asking agent can cite. Trust posture (plan RT-6): the sender line is
// attribution — what the record and the receiver were told.
export function invocationCard(invocation = {}) {
  const card = el('div', 'agent-invocation-card');
  const head = el('div', 'row');
  head.append(
    el('strong', '', 'Send to session'),
    el('span', 'sub', ' ' + String(invocation.caller_runtime || '?') + '/' + String(invocation.caller_native_id || '?')
      + ' → ' + String(invocation.target_runtime || '?') + '/' + String(invocation.target_native_id || invocation.target_catalog_id || '?')
      + (invocation.invocation_id ? ' · ' + String(invocation.invocation_id) : '')),
  );
  card.appendChild(head);
  card.appendChild(el('div', 'sub agent-invocation-status', invocationStatusText(invocation)
    + (invocation.detail ? ' ' + String(invocation.detail) : '')));
  return card;
}

// invocationCards renders the ledger rows that belong to this session — as the
// caller or the target — newest first, capped. A fetch failure renders
// nothing: the panel's own content never blocks on this read.
export function invocationCards(invocations = [], identities = []) {
  const ids = new Set(identities.map(String));
  const mine = invocations.filter(item => ids.has(String(item.caller_native_id))
    || ids.has(String(item.target_native_id)) || ids.has(String(item.target_catalog_id))
    || ids.has(String(item.caller_canonical_id)) || ids.has(String(item.target_canonical_id)));
  return mine.slice(0, 10).map(invocationCard);
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

// outcomeNotes are the facts a claim card states about an agent that is not
// producing claims: its binding cannot run here, the shown run did not
// complete, or a newer turn did not (plan §4.8).
function outcomeNotes(agent, run, laterRun) {
  const notes = [];
  const problem = bindingProblem(agent);
  if (problem) notes.push(el('div', 'banner', problem));
  if (run && UNFINISHED_STATES.has(run.state)) notes.push(el('div', 'sub agent-run-outcome', runOutcomeText(run)));
  if (laterRun) notes.push(el('div', 'sub agent-run-outcome', 'Last turn · ' + runOutcomeText(laterRun)));
  return notes;
}

function claimCard(agent, run, ctx, laterRun = null) {
  const card = el('article', 'agent-claim-card');
  // Attribution header: agent name / type / priority + goal line.
  const head = el('div', 'row agent-claim-head');
  head.append(el('strong', '', '⚖ ' + String(agent?.profile_name || agent?.profile_id || run.profile_id || 'agent')),
    el('span', 'chip', String(agent?.role || run.role || 'agent')),
    el('span', 'sub', 'priority ' + String(agent?.priority ?? '—')));
  card.appendChild(head);
  if (agent?.profile_description) card.appendChild(el('div', 'sub', String(agent.profile_description)));
  card.append(...outcomeNotes(agent, run, laterRun));
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
  // A reply the daemon did not send is the owner's to use: copy it into the
  // session. Sending it from here is the attended-delivery plan's work.
  if (run.attention_class === 'draft' && run.action !== 'draft_reply' && run.message) {
    const copy = el('button', 'btn', 'Copy reply');
    copy.onclick = async () => {
      try { await navigator.clipboard.writeText(String(run.message)); copy.textContent = 'Copied ✓'; }
      catch (error) { card.prepend(el('div', 'banner', String(error?.message || error))); }
    };
    card.appendChild(copy);
  }
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

// appendRunWarnings adds the strip's two "this will not work" chips: a binding the
// daemon says cannot run here, and a newer refused or failed turn than the claim shown.
function appendRunWarnings(summary, agent, laterRun) {
  const problem = bindingProblem(agent);
  if (problem) {
    const chip = el('span', 'chip st-disputed', 'cannot run here');
    chip.title = problem;
    summary.appendChild(chip);
  }
  if (laterRun) {
    const chip = el('span', 'chip st-disputed', 'last turn: ' + String(laterRun.error_class || laterRun.state).replaceAll('_', ' '));
    chip.title = runOutcomeText(laterRun);
    summary.appendChild(chip);
  }
}

function agentStripRow(agent, run, counters, expandHost, ctx, projectionFailed = false, helperGroup = null, laterRun = null) {
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
  appendRunWarnings(summary, agent, laterRun);
  if (counters && (counters.cycles || counters.replies)) {
    const counter = el('span', 'agent-strip-counter', 'cycle ' + counters.cycles + ' · ' + counters.replies + ' repl' + (counters.replies === 1 ? 'y' : 'ies'));
    counter.title = 'Loop counters from recorded relationships. Token budgets bound the next admission; spend is not reported by this projection.';
    summary.appendChild(counter);
  }
  row.appendChild(summary);
  if (helperGroup) {
    // ONE helper session per source session (schema 30): the row links it
    // and counts its turns; the rail folds that session under this one. The
    // link sits beside the summary button, never inside it: a link inside the
    // button toggled the row as it navigated.
    row.appendChild(sessionLinkNode(String(helperGroup.helper_runtime || ''),
      String(helperGroup.helper_native_session_id || ''), helperSessionLabel(helperGroup), 'agent-strip-helper-session'));
  }
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
    card = claimCard(agent, run, ctx, laterRun);
    summary.setAttribute('aria-expanded', 'true');
    expandHost.appendChild(card);
  };
  summary.onclick = expand;
  row.cgExpand = () => { if (!card) expand(); };
  return row;
}

// attachAgentControl renders the Attach-agent flow for one live session
// (natural-session plan B-GUI): choose an imported follower/helper and the
// model route it runs on, by name; scope = THIS session (watch_natural). Saves
// through the binding PUT with the daemon's absent token (G-4 CAS discipline).
function attachAgentControl(ctx, identities) {
  const host = el('div', 'agent-attach');
  const status = el('span', 'sub', '');
  const open = el('button', 'btn', 'Attach agent');
  open.title = 'Attach an imported follower or helper to THIS session (watches its natural lifecycle).';
  open.onclick = async () => {
    open.disabled = true;
    status.textContent = 'Loading agents…';
    let choices;
    try {
      choices = await attachChoices();
    } catch (error) {
      open.disabled = false;
      status.textContent = 'Agent list unavailable: ' + String(error?.message || error);
      return;
    }
    if (!choices.profiles.length) {
      open.disabled = false;
      status.textContent = 'No follower or helper is waiting to be attached. Import one in Settings → Agents first.';
      return;
    }
    status.textContent = '';
    open.replaceWith(attachForm(ctx, identities, choices, () => { host.replaceChildren(open, status); open.disabled = false; status.textContent = ''; }));
  };
  host.append(open, status);
  return host;
}

// attachChoices reads what the form offers: the imported follower and helper
// profiles that have no agent yet — the daemon publishes a creation token for
// exactly those — and the model routes an agent of that kind can run on.
async function attachChoices() {
  const settings = await managedSettings();
  const tokens = settings?.absent_state_tokens || {};
  const profiles = (settings?.profiles || []).filter(option => option?.compatible && tokens['agent-' + option.profile_id]);
  let capabilities = [];
  try { capabilities = await loadChatCapabilities(); } catch { capabilities = []; }
  let routes = [];
  try { routes = await loadRouteChoices(ROUTE_FAMILY_MANAGED, capabilities); } catch { routes = []; }
  return { profiles, tokens, routes };
}

// attachForm is the inline form: the agent, its model route by name (team
// rest-of-release plan §5.5 — what a route resolves to is on Settings → Models;
// a device with no route for this kind of agent says so and offers Create
// route), and the two actions on their own row.
function attachForm(ctx, identities, choices, onCancel) {
  const form = el('div', 'agent-attach-form');
  const pick = el('select');
  pick.setAttribute('aria-label', 'Agent to attach');
  choices.profiles.forEach(option => {
    const item = el('option');
    item.value = option.profile_id;
    item.textContent = String(option.name || option.profile_id) + (option.agent_type ? ' (' + option.agent_type + ')' : '');
    pick.appendChild(item);
  });
  const agent = el('label', 'agents-field');
  agent.append(el('span', 'agents-field-label', 'Agent'), pick);
  const route = routePicker(choices.routes, {});
  const status = el('div', 'sub');
  const save = el('button', 'btn primary', 'Attach to this session');
  save.disabled = route.empty;
  const cancel = el('button', 'btn', 'Cancel');
  cancel.onclick = onCancel;
  const actions = el('div', 'row');
  actions.append(save, cancel);
  form.append(agent, route.node, actions, status);
  save.onclick = async () => {
    save.disabled = true;
    try {
      await attachChosen(ctx, identities, choices, pick.value, route.value());
      form.replaceWith(el('div', 'banner', 'Agent attached. Its run appears here when the session reaches a signal it selects.'));
    } catch (error) {
      save.disabled = false;
      status.textContent = 'Not attached: ' + String(error?.message || error)
        + (error?.code === 'state_conflict' ? ' — the binding changed elsewhere; check Settings → Agents.' : '');
    }
  };
  return form;
}

// attachChosen writes the session-scoped binding for the chosen profile.
async function attachChosen(ctx, identities, choices, profileID, chosen) {
  const selection = ctx.selection || {};
  const option = choices.profiles.find(item => item.profile_id === profileID);
  if (!option) throw new Error('that profile is no longer available');
  if (!chosen) throw new Error('create a model route first');
  const bindingID = 'agent-' + option.profile_id;
  await attachAgentToSession({
    binding_id: bindingID,
    profile_id: option.profile_id,
    profile_source_digest: option.source_digest,
    profile_bundle_digest: option.bundle_digest,
    project_root: String(selection.cwd || ''),
    scope_runtime: String(selection.runtime || ''),
    scope_session: identities[0] || String(selection.id || ''),
    route_id: chosen.route_id,
    mode: chosen.mode,
    granted_authority: [],
    watch_natural: true,
  }, choices.tokens[bindingID]);
}

async function renderAgentStrip(ctx, box) {
  const selection = ctx.selection || {};
  const runtime = String(selection.runtime || '');
  const sessionID = String(selection.id || '');
  const cwd = String(selection.cwd || '');
  // Every id that names this session, as the daemon published them: runs and
  // tags are recorded under whichever identity the task row carried, so a
  // single-id query missed runs for any session whose identities diverge —
  // resumed codex threads above all (g4 plan §3; parent verification
  // limitation #3). A subagent's thread id names its parent and is not one of
  // them; watching still compares it (watchScopeIds, child-thread-identity D-1).
  const identities = sessionIdentities(selection);
  box.replaceChildren(el('div', 'sub', 'Checking which agents watch this session…'));
  const [agentsResult, projectionResult, tagsResult, relatedResult, invocationsResult, handoffsResult] = await Promise.allSettled([
    loadAgents(),
    identities.length ? managedProjection(runtime, identities) : Promise.reject(new Error('session identity unavailable')),
    identities.length ? loadSessionTags(identities) : Promise.reject(new Error('session identity unavailable')),
    loadRelatedSessions(runtime, sessionID),
    api('/api/session-message/invocations?limit=100'),
    loadChatCapabilities().catch(() => []).then(capabilities =>
      loadRelatedHandoffs(selection, name => capabilities.find(item => item.runtime === name)?.displayName || name)),
  ]);
  if (!box.isConnected) return;
  const render = () => { if (box.isConnected) renderAgentStrip(ctx, box); };
  box.replaceChildren();
  const strip = el('div', 'agent-strip');
  strip._reRender = render;
  box.appendChild(strip);
  activeStrip = strip;

  const invocations = invocationsResult.status === 'fulfilled'
    ? (invocationsResult.value?.invocations || []) : [];

  const projection = projectionResult.status === 'fulfilled' ? projectionResult.value : null;
  if (projection) publishAgentTurnIndex(runtime + '/' + sessionID, buildAgentTurnIndex(projection));
  if (renderAgentRows(strip, ctx, { runtime, cwd, identities, agentsResult, projectionResult, projection })) {
    appendSessionTags(strip, tagsResult);
  }
  // Related sessions are the session's own facts: they render whether or not
  // an agent watches it, and whether or not the agent reads succeeded.
  const related = relatedSessionsSection(relatedResult, handoffsResult.status === 'fulfilled' ? handoffsResult.value : []);
  if (related) strip.appendChild(related);
}

// renderAgentRows draws who watches this session and their runs. True when
// the full rows rendered; the session-tag row belongs only there.
function renderAgentRows(strip, ctx, { runtime, cwd, identities, agentsResult, projectionResult, projection }) {
  if (agentsResult.status === 'rejected' && projectionResult.status === 'rejected') {
    strip.appendChild(el('div', 'sub', 'Agent status unavailable: ' + String(agentsResult.reason?.message || agentsResult.reason)));
    return false;
  }
  const agents = agentsResult.status === 'fulfilled' ? (agentsResult.value.agents || []) : [];
  const watching = agents.filter(agent => agentWatchesSession(agent, { runtime, sessionIds: watchScopeIds(ctx.selection || {}), cwd, cwdKey: String((ctx.selection || {}).cwd_key || '') }));
  const runs = projection?.runs || [];
  const relationships = projection?.relationships || [];
  const groups = projection?.groups || [];
  const latest = latestRunByBinding(runs);
  const later = newerUnfinishedByBinding(runs, latest);

  const projectionFailed = projectionResult.status === 'rejected';
  if (!watching.length && !runs.length) {
    const empty = el('div', 'sub agent-strip-empty', 'No agents watch this session.');
    const attach = attachAgentControl(ctx, identities);
    strip.append(empty, attach);
    // A failed runs READ is not evidence of no runs: historical runs from
    // disabled/legacy bindings may exist but be unrenderable right now
    // (postwork SF-4).
    if (projectionFailed) strip.appendChild(el('div', 'chip st-disputed', 'run status unavailable — reload to retry'));
    return false;
  }

  const attachBar = el('div', 'row');
  attachBar.appendChild(attachAgentControl(ctx, identities));
  strip.appendChild(attachBar);

  const expandHost = el('div', 'agent-strip-expansion');
  const seenBindings = new Set();
  const rowFor = (agent, bindingID, failed) => {
    const groupIDs = new Set(runs.filter(item => item.binding_id === bindingID).map(item => item.group_id));
    return agentStripRow(agent, latest.get(bindingID) || null, groupCounters(relationships, groupIDs), expandHost, ctx, failed,
      helperSessionForBinding(groups, bindingID, identities), later.get(bindingID) || null);
  };
  for (const agent of watching) {
    seenBindings.add(agent.binding_id);
    strip.appendChild(rowFor(agent, agent.binding_id, projectionFailed));
  }
  // Runs from bindings that no longer watch (disabled, historical role names)
  // still render — history is not hidden by today's scope.
  for (const bindingID of latest.keys()) {
    if (seenBindings.has(bindingID)) continue;
    strip.appendChild(rowFor(agents.find(item => item.binding_id === bindingID) || null, bindingID, false));
  }
  strip.appendChild(expandHost);
  return true;
}

function appendSessionTags(strip, tagsResult) {
  if (tagsResult.status !== 'fulfilled' || !Array.isArray(tagsResult.value.tags) || !tagsResult.value.tags.length) return;
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

// sessionLinkNode is the one way this module links a session. app.js opens a
// plain click in place through the one session-open path; a modified click
// keeps the real href, so a new tab boots on that session. It never carries
// the ref-ok class: that click belongs to the reference model, which swallowed
// every session link. Without both identity halves it is text, not a link.
function sessionLinkNode(runtime, id, label, extraClass = '') {
  if (!runtime || !id) return el('span', 'agent-related-id' + (extraClass ? ' ' + extraClass : ''), label);
  const link = el('a', 'ref session-link' + (extraClass ? ' ' + extraClass : ''), label);
  link.href = '/?runtime=' + encodeURIComponent(runtime) + '&session=' + encodeURIComponent(id);
  link.dataset.sessionRuntime = runtime;
  link.dataset.sessionId = id;
  link.title = id;
  return link;
}

// relatedRowView is one Related-sessions row as the strip shows it. A row
// links only when the daemon says its id can name a session (`openable`); an
// id that cannot, such as a native subagent's agent id, is text, never a link
// that fails. Observed and caused rows are never merged here (plan §4).
export function relatedRowView(row = {}) {
  const runtime = String(row.runtime || '');
  const sessionId = String(row.session_id || '');
  const runs = Number(row.runs || 0), replies = Number(row.replies || 0), cycle = Number(row.cycle || 0);
  const parts = [String(row.kind || '')];
  if (row.direction) parts.push(String(row.direction));
  if (row.role) parts.push(String(row.role));
  if (row.provenance === 'caused' && row.state) parts.push(String(row.state));
  if (cycle) parts.push('cycle ' + cycle);
  if (runs > 1) parts.push(runs + ' runs');
  if (replies) parts.push(replies + (replies === 1 ? ' reply' : ' replies'));
  const idLabel = sessionId ? (runtime ? runtime + ' \u00b7 ' : '') + sessionId.slice(0, 14) + '\u2026' : '';
  return {
    provenance: String(row.provenance || ''),
    text: parts.join(' \u00b7 '),
    description: String(row.description || ''),
    link: row.openable === true && runtime && sessionId ? { runtime, id: sessionId, label: idLabel } : null,
    idLabel,
    sessionId,
    label: sessionId ? '' : String(row.label || ''),
    unresolved: !!row.unresolved,
  };
}

// relatedRowsTree orders rows for display: a descendant sits right after the
// row it was spawned by (its `via`), one indent per hop. A descendant whose
// launcher is not listed stays where the daemon put it; no row is ever dropped,
// not even one caught in a cycle of vias.
export function relatedRowsTree(rows = []) {
  const listed = new Set(rows.map(row => String(row?.session_id || '')).filter(Boolean));
  const under = new Map();
  const top = [];
  for (const row of rows) {
    const via = String(row?.via || '');
    if (via && listed.has(via) && via !== String(row?.session_id || '')) {
      if (!under.has(via)) under.set(via, []);
      under.get(via).push(row);
    } else top.push(row);
  }
  const out = [];
  const placed = new Set();
  const visit = (row, depth) => {
    if (placed.has(row)) return;
    placed.add(row);
    out.push({ row, depth });
    const id = String(row?.session_id || '');
    const nested = id ? under.get(id) : null;
    if (!nested) return;
    under.delete(id);
    for (const child of nested) visit(child, depth + 1);
  };
  for (const row of top) visit(row, 0);
  for (const row of rows) visit(row, 0);
  return out;
}

function relatedRowNode(view, depth) {
  const line = el('div', 'agent-related-row');
  if (depth) {
    line.classList.add('agent-related-nested');
    line.style.marginInlineStart = (depth * 1.25) + 'em';
  }
  line.appendChild(el('span', 'chip cl-' + (view.provenance === 'caused' ? 'claimed' : 'observed'), view.provenance));
  line.appendChild(el('span', '', view.text));
  if (view.description) {
    const description = el('span', 'agent-related-desc', view.description);
    description.title = view.description;
    line.appendChild(description);
  }
  if (view.link) line.appendChild(sessionLinkNode(view.link.runtime, view.link.id, view.link.label));
  else if (view.idLabel) {
    const id = el('span', 'agent-related-id', view.idLabel);
    id.title = view.sessionId;
    line.appendChild(id);
  } else if (view.label) {
    const chip = el('span', 'chip', view.label);
    if (view.unresolved) chip.title = 'unresolved reference';
    line.appendChild(chip);
  }
  if (view.unresolved && view.sessionId) line.appendChild(el('span', 'chip', 'unresolved'));
  return line;
}

// relatedSectionModel takes the settled read. A failed read, or a partial one
// (the daemon's coverage note), says so; neither looks like "none". Null only
// when the read succeeded, found nothing and missed nothing.
export function relatedSectionModel(result) {
  if (result?.status !== 'fulfilled') {
    return { unavailable: 'Related sessions unavailable: ' + String(result?.reason?.message || result?.reason || 'unknown error') };
  }
  const rows = Array.isArray(result.value?.related) ? result.value.related : [];
  const coverage = String(result.value?.coverage || '');
  if (!rows.length && !coverage) return null;
  return { rows: relatedRowsTree(rows).map(({ row, depth }) => ({ view: relatedRowView(row), depth })), coverage };
}

// handoffs are the rows a handoff adds: the session this one continues, or who
// continued this one. They are on another person's device, so they are text.
function relatedSessionsSection(result, handoffs = []) {
  const model = relatedSectionModel(result);
  if (!model && !handoffs.length) return null;
  const section = el('div', 'agent-related-sessions');
  section.appendChild(el('strong', 'sub', 'Related sessions'));
  for (const handoff of handoffs) {
    const line = el('div', 'agent-related-row agent-related-handoff');
    line.append(el('span', 'agent-related-id', handoff.label), el('span', 'agent-related-desc', handoff.description));
    section.appendChild(line);
  }
  if (!model) return section;
  if (model.unavailable) {
    section.appendChild(el('div', 'sub', model.unavailable));
    return section;
  }
  for (const { view, depth } of model.rows) section.appendChild(relatedRowNode(view, depth));
  if (model.coverage) section.appendChild(el('div', 'sub agent-related-coverage', model.coverage));
  return section;
}

provider({
  id: 'session.agents', zone: 'header', order: -10, title: 'Agents',
  paneModule: { placement: 'deck' },
  match: ctx => ctx.surface === 'session' && !!ctx.selection,
  render: async (ctx, box) => { await renderAgentStrip(ctx, box); consumeRevealIntent(); },
});

// The activity line's agents link reveals this module with the intent
// "expand-first". The strip may render before or after the reveal resolves, so
// both paths consume the one pending intent, and only one of them acts on it.
function consumeRevealIntent() {
  if (!activeStrip?.isConnected) return;
  if (takeModuleIntent('session.agents') !== 'expand-first') return;
  activeStrip.scrollIntoView({ block: 'nearest' });
  activeStrip.querySelector('.agent-strip-row')?.cgExpand?.();
}

if (typeof document !== 'undefined') {
  document.addEventListener('cg:module-revealed', event => {
    if (event.detail?.id === 'session.agents') consumeRevealIntent();
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
