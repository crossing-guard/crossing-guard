// Agents page — agent-first (G-4, gui-design §8a; g4 plan §2).
//
// The roster IS the page: importing a profile creates the agent's card, and
// that card owns everything about the agent — prompt first, then deployment
// scope, runtime route, grants, and budgets as set-and-forget settings — with
// ONE primary action. The type-first deploy-form sections are gone.
// Reviewer cards ride the reviews routes (singleton binding, replace-on-
// enable stated honestly); follower/helper cards ride the agents routes with
// per-profile binding ids created under the daemon-published ABSENT token
// (never a live token — a collision must CAS-conflict, not silently rebind).
// Typed DOM nodes only (never raw HTML injection).
import { el, api, fmtTime } from '../core.js';
import { loadAgents, saveAgent, disableAgent, rerouteParkedPreview, rerouteParkedApply } from './agents-api.js';
import { loadProfiles, loadProfile } from './profile-api.js';
import { managedProjectionAll, managedSettings } from './managed-api.js';
import { loadReviewSettings, saveReviewBinding, disableReviewBinding } from './review-api.js';
import { renderProfileImporter, profileIdentitySection, profileStorageDiagnostics } from './settings-profiles.js';
import { reviewCard } from './session-reviews.js';

/* ---------- pure view-model helpers (unit-tested) ---------- */

const POWER_COPY = Object.freeze({
  'draft-reply': 'draft a reply for you to review and send',
  'reply': 'send a reply into the watched session',
  'advise': 'record advice on completed work',
  'launch-profile': 'launch one allowlisted read-only child profile',
  'send-message': 'Send an attributed message to the source session',
  'request-interrupt': 'request an exact task interruption',
  'respond-approval': 'answer an approval that governance already deferred',
});

const ROLE_COPY = Object.freeze({
  reviewer: 'Sees one event at a time, no session context. Observes and tags.',
  follower: 'Reads the session so far through bounded selectors. Observes and tags/metadata — never acts.',
  helper: 'Reads the session so far and may act with your granted capabilities — never your identity.',
});

// describePowers turns granted authority + auto_action into plain language.
export function describePowers(agent = {}) {
  const granted = Array.isArray(agent.granted_authority) ? agent.granted_authority : [];
  const powers = granted.map(name => POWER_COPY[name] || String(name).replaceAll('-', ' '));
  if (!powers.length) return 'Observe and tag only — no granted actions.';
  const auto = agent.auto_action
    ? ' Granted actions apply automatically.'
    : ' Every action waits for your review.';
  return 'May ' + powers.join('; ') + '.' + auto;
}

// BUDGET_FIELDS names the owner-editable limits and their resolved-default keys.
export const BUDGET_FIELDS = Object.freeze([
  ['max_total', 'invocations / group'],
  ['max_active', 'active helpers'],
  ['max_hops', 'hops'],
  ['loop_budget', 'loop cycles'],
  ['max_group_tokens', 'group tokens'],
  ['max_agent_tokens', 'agent tokens'],
]);

// agentRosterModel maps agent binding rows onto view models, resolving each
// budget against the shipped defaults so the card can say which value is the
// agent's own and which is inherited configuration.
// helperSessionsSummary counts one binding's helper sessions (schema 30):
// groups that adopted a vendor session, their turns, and signals waiting
// behind a running turn. Groups without a session are not sessions.
export function helperSessionsSummary(groups = [], bindingID = '') {
  const out = { sessions: 0, turns: 0, waiting: 0 };
  for (const group of groups) {
    if (String(group?.binding_id || '') !== String(bindingID)) continue;
    if (!String(group?.helper_native_session_id || '')) continue;
    out.sessions += 1;
    out.turns += Number(group.helper_turns || 0);
    if (Number(group.pending_event_id || 0)) out.waiting += 1;
  }
  return out;
}

export function helperSessionsLine(summary = { sessions: 0, turns: 0, waiting: 0 }) {
  if (!summary.sessions) return '';
  let text = summary.sessions + ' helper session' + (summary.sessions === 1 ? '' : 's') + ' \u00b7 ' + summary.turns + ' turn' + (summary.turns === 1 ? '' : 's');
  if (summary.waiting) text += ' \u00b7 ' + summary.waiting + ' waiting';
  return text;
}

export function agentRosterModel(payload = {}) {
  const defaults = payload.defaults || {};
  return (payload.agents || []).map(agent => ({
    bindingID: String(agent.binding_id || ''),
    name: String(agent.profile_name || agent.profile_id || agent.binding_id || 'agent'),
    role: String(agent.role || 'unknown'),
    roleCopy: ROLE_COPY[agent.role] || 'Unknown agent type.',
    state: String(agent.state || 'unknown'),
    priority: agent.priority ?? null,
    declaredTags: Array.isArray(agent.declared_tags) ? agent.declared_tags.map(String) : [],
    powers: describePowers(agent),
    deployment: {
      projectRoot: String(agent.project_root || ''),
      scopeRuntime: String(agent.scope_runtime || ''),
      scopeSession: String(agent.scope_session || ''),
      runtime: String(agent.runtime || ''),
      mode: String(agent.mode || ''),
    },
    budgets: BUDGET_FIELDS.map(([key, label]) => ({
      key, label,
      value: agent.limits?.[key] ?? null,
      resolvedDefault: defaults[key] ?? null,
      effective: agent.limits?.[key] ?? defaults[key] ?? null,
    })),
    profileID: String(agent.profile_id || ''),
    promptExcerpt: String(agent.prompt_excerpt || ''),
    stateToken: String(agent.state_token || ''),
    raw: agent,
  }));
}

// managedCardName resolves the display identity for a managed card whose
// profile identity may be gone (pinned revision unavailable).
function cardStateOf(binding, needsAttention) {
  if (needsAttention) return 'needs attention';
  if (!binding) return 'not deployed';
  return String(binding.state || 'unknown');
}

// agentPageModel builds the one card list from the four payloads. Pure and
// deterministic (g4 plan §2): every managed binding → deployed card; the
// review singleton → deployed reviewer card; every remaining imported profile
// → undeployed card in its lane (managed only when `agent-<profile_id>` is
// unbound AND the daemon published an absent token for it), else an
// incompatible card with the reason. Imported ≠ hidden.
export function agentPageModel(profilesPayload, managed, review, agentsPayload) {
  const cards = [];
  // Null payloads mean a READ failed — cards must say "lane state unknown",
  // never blame the profile (postwork SF-1) or claim a revision is gone
  // (postwork SF-2).
  const profilesAvailable = profilesPayload != null;
  const lanesAvailable = managed != null || review != null;
  const profiles = Array.isArray(profilesPayload?.profiles) ? profilesPayload.profiles : [];
  const managedOptions = Array.isArray(managed?.profiles) ? managed.profiles : [];
  const reviewOptions = Array.isArray(review?.profiles) ? review.profiles : [];
  const absentTokens = managed?.absent_state_tokens || {};
  const defaults = agentsPayload?.defaults || managed?.defaults || {};
  // Managed binding rows: prefer the enriched agents payload; fall back to the
  // raw settings bindings so an agents-route outage degrades, not blanks.
  const bindingRows = Array.isArray(agentsPayload?.agents) && agentsPayload.agents.length
    ? agentsPayload.agents
    : (Array.isArray(managed?.bindings) ? managed.bindings : []);
  const rosterByBinding = new Map(agentRosterModel({ agents: bindingRows, defaults }).map(model => [model.bindingID, model]));
  const coveredProfiles = new Set();
  const boundIDs = new Set();

  for (const binding of bindingRows) {
    const model = rosterByBinding.get(String(binding.binding_id || ''));
    const profile = profiles.find(item => item.profile_id === binding.profile_id) || null;
    const option = managedOptions.find(item => item.profile_id === binding.profile_id) || null;
    // Revision-gone honesty (RT-14a): no importable identity AND no enriched
    // profile name → the pinned revision is unavailable — but only when the
    // profiles read SUCCEEDED; a failed read proves nothing about revisions.
    const needsAttention = profilesAvailable && !profile && !String(binding.profile_name || '');
    coveredProfiles.add(String(binding.profile_id || ''));
    boundIDs.add(String(binding.binding_id || ''));
    cards.push({
      kind: 'managed', deployed: true,
      bindingID: model.bindingID, enableToken: model.stateToken, tokenSource: 'live',
      name: profile?.name || model.name, description: String(binding.profile_description || profile?.description || ''),
      type: model.role, state: cardStateOf(binding, needsAttention), enabled: binding.state === 'enabled',
      needsAttention, compatible: option ? !!option.compatible : true,
      reason: option?.reason || (needsAttention ? 'The pinned profile revision is unavailable. History and disable remain.' : ''),
      requestedAuthority: option?.requested_authority || (Array.isArray(binding.granted_authority) ? binding.granted_authority : []),
      profile, option, binding, model,
    });
  }

  const reviewBinding = review?.binding || null;
  if (reviewBinding) {
    const option = reviewOptions.find(item => item.profile_id === reviewBinding.profile_id) || null;
    const profile = profiles.find(item => item.profile_id === reviewBinding.profile_id) || null;
    coveredProfiles.add(String(reviewBinding.profile_id || ''));
    cards.push({
      kind: 'reviewer', deployed: true,
      bindingID: 'review-binding', enableToken: String(review?.state_token || ''), tokenSource: 'live',
      name: profile?.name || option?.name || String(reviewBinding.profile_id || 'reviewer'),
      description: String(profile?.description || ''), type: 'reviewer',
      state: reviewBinding.state === 'enabled' ? 'enabled' : 'disabled', enabled: reviewBinding.state === 'enabled',
      needsAttention: false, compatible: option ? !!option.compatible : true, reason: option?.reason || '',
      profile, option, binding: reviewBinding, model: null,
    });
  }

  for (const profile of profiles) {
    if (coveredProfiles.has(String(profile.profile_id || ''))) continue;
    const managedOption = managedOptions.find(item => item.profile_id === profile.profile_id) || null;
    const reviewOption = reviewOptions.find(item => item.profile_id === profile.profile_id) || null;
    const candidateID = 'agent-' + String(profile.profile_id || '');
    if (managedOption?.compatible && ['follower', 'helper'].includes(String(managedOption.agent_type || ''))) {
      // RT-2: never offer creation over an existing binding id, and never
      // enable through anything but the published absent token.
      const collision = boundIDs.has(candidateID);
      const absent = absentTokens[candidateID] || '';
      cards.push({
        kind: 'managed', deployed: false,
        bindingID: candidateID, enableToken: collision ? '' : absent, tokenSource: collision || !absent ? null : 'absent',
        name: profile.name || profile.profile_id, description: String(profile.description || ''),
        type: String(managedOption.agent_type), state: 'not deployed', enabled: false,
        needsAttention: false, compatible: !collision,
        reason: collision ? 'Binding id ' + candidateID + ' is already taken by another deployment; disable it first.'
          : (!absent ? 'The daemon did not publish a creation token for this profile; reload after upgrading the daemon.' : ''),
        requestedAuthority: managedOption.requested_authority || [],
        profile, option: managedOption, binding: null, model: null,
      });
    } else if (reviewOption?.compatible) {
      cards.push({
        kind: 'reviewer', deployed: false,
        bindingID: 'review-binding', enableToken: String(review?.state_token || ''), tokenSource: 'live',
        name: profile.name || profile.profile_id, description: String(profile.description || ''),
        type: 'reviewer', state: 'not deployed', enabled: false,
        needsAttention: false, compatible: true,
        reason: reviewBinding ? 'Enabling this reviewer replaces the currently enabled reviewer.' : '',
        replacesReviewer: !!reviewBinding,
        profile, option: reviewOption, binding: null, model: null,
      });
    } else {
      const reason = !lanesAvailable
        ? 'Binding lane state unavailable: the settings reads failed. Reload to retry.'
        : (managedOption?.reason || reviewOption?.reason
          || 'No binding lane accepts this profile. Re-import a corrected PROFILE.md.');
      cards.push({
        kind: managedOption ? 'managed' : 'reviewer', deployed: false,
        bindingID: '', enableToken: '', tokenSource: null,
        name: profile.name || profile.profile_id, description: String(profile.description || ''),
        type: String(managedOption?.agent_type || 'unknown'),
        state: 'not deployed', enabled: false, needsAttention: !!profile.problem,
        compatible: false, reason: String(reason),
        profile, option: managedOption || reviewOption, binding: null, model: null,
      });
    }
  }
  return cards;
}

/* ---------- page ---------- */

export async function renderAgentsPage(main) {
  main.appendChild(el('div', 'sub',
    'Every agent: what it reads, what it may do, where it watches, and what it costs. '
    + 'Importing a profile creates its card; the card owns its settings with one Enable. '
    + 'Budgets are yours — edits apply to the next admission, never the in-flight turn.'));
  const rosterHost = el('div', 'agents-roster');
  renderProfileImporter(main, async () => { await renderAgentRoster(rosterHost); });
  main.appendChild(rosterHost);
  await renderAgentRoster(rosterHost);
}

// knownRepositoryRoots reads the launch roots the session rail already knows,
// so the project-root field is a picker of real repositories, never an
// absolute-path placeholder. A plain text field remains when the read fails.
async function knownRepositoryRoots() {
  try {
    const rail = await api('/api/sessions?view=rail&repository_limit=500');
    const roots = new Set();
    for (const repository of rail.repositories || []) {
      if (repository.launch_cwd) roots.add(String(repository.launch_cwd));
    }
    return [...roots].sort();
  } catch { return []; }
}

async function renderAgentRoster(host, notice = '') {
  host.replaceChildren(el('div', 'sub', 'Loading the agent roster…'));
  const [profilesResult, managedResult, reviewResult, agentsResult, historyResult, rootsResult] =
    await Promise.allSettled([loadProfiles(), managedSettings(), loadReviewSettings(),
      loadAgents(), managedProjectionAll(), knownRepositoryRoots()]);
  if (!host.isConnected) return;
  host.replaceChildren();
  if (notice) host.appendChild(el('div', 'banner', notice));
  const profilesPayload = profilesResult.status === 'fulfilled' ? profilesResult.value : null;
  const managed = managedResult.status === 'fulfilled' ? managedResult.value : null;
  const review = reviewResult.status === 'fulfilled' ? reviewResult.value : null;
  const agents = agentsResult.status === 'fulfilled' ? agentsResult.value : null;
  const history = historyResult.status === 'fulfilled' ? historyResult.value : null;
  const roots = rootsResult.status === 'fulfilled' ? rootsResult.value : [];
  if (!profilesPayload && !managed && !review && !agents) {
    host.appendChild(el('div', 'banner', 'Agent roster unavailable: '
      + String(profilesResult.reason?.message || profilesResult.reason || 'daemon unreachable')));
    const retry = el('button', 'btn', 'Retry');
    retry.onclick = () => { void renderAgentRoster(host); };
    host.appendChild(retry);
    return;
  }
  for (const [label, result] of [['Profiles', profilesResult], ['Agent bindings', managedResult],
    ['Reviewer lane', reviewResult], ['Agent roster', agentsResult]]) {
    if (result.status === 'rejected') {
      host.appendChild(el('div', 'banner', label + ' unavailable: ' + String(result.reason?.message || result.reason)));
    }
  }
  if (managed?.problem) host.appendChild(el('div', 'banner', String(managed.problem)));

  const refresh = async note => { await renderAgentRoster(host, note); };

  // Provider outage banners (provider-outage plan Slice D): ONE actionable
  // row per tripped route — since when, the vendor's own recovery words, the
  // parked count, and the preview→confirm reroute along each run's declared
  // chain. Stale helpers become drafts, never late auto-sends.
  for (const outage of Array.isArray(managed?.provider_outages) ? managed.provider_outages : []) {
    const banner = el('div', 'banner');
    const routeName = outage.runtime + (outage.model ? ' / ' + outage.model : '');
    const since = outage.tripped_at ? new Date(outage.tripped_at * 1000).toLocaleTimeString() : '';
    banner.appendChild(el('div', '', 'Provider route ' + routeName + ' is unavailable'
      + (since ? ' since ' + since : '') + ' — ' + (outage.parked || 0) + ' run(s) parked.'
      + (outage.detail ? ' ' + outage.detail : '')));
    const reroute = el('button', 'btn', 'Reroute parked runs…');
    reroute.onclick = async () => {
      reroute.disabled = true;
      try {
        const plan = await rerouteParkedPreview(outage.runtime, outage.model || '');
        const counts = { reroute: 0, stale_draft: 0, no_route: 0 };
        for (const row of plan.decisions || []) counts[row.action] = (counts[row.action] || 0) + 1;
        const summary = counts.reroute + ' reroute along declared chains, '
          + counts.stale_draft + ' stale helper draft(s) held for you, '
          + counts.no_route + ' with no declared fallback (stay parked).';
        if (!window.confirm('Reroute parked runs on ' + routeName + '?\n' + summary)) { reroute.disabled = false; return; }
        const applied = await rerouteParkedApply(outage.runtime, outage.model || '', plan.preview_token);
        await refresh('Rerouted ' + (applied.rerouted || 0) + '; ' + (applied.stale_drafts || 0)
          + ' stale draft(s); ' + (applied.no_route || 0) + ' still parked without a fallback.');
      } catch (error) {
        reroute.disabled = false;
        banner.appendChild(el('div', 'sub', 'Reroute failed: ' + String(error?.message || error)));
      }
    };
    banner.appendChild(reroute);
    host.appendChild(banner);
  }
  const cards = agentPageModel(profilesPayload, managed, review, agents);
  if (!cards.length) {
    host.appendChild(el('div', 'sub', 'No agents. Import a PROFILE.md above — importing creates the agent’s card; selection alone stays inert.'));
  }
  const historyFailed = historyResult.status === 'rejected';
  const runsByBinding = new Map();
  for (const run of history?.runs || []) {
    if (!runsByBinding.has(run.binding_id)) runsByBinding.set(run.binding_id, []);
    runsByBinding.get(run.binding_id).push(run);
  }
  for (const card of cards) {
    // Only a DEPLOYED card owns run history; a collided undeployed card must
    // never show the other deployment's runs under this profile's name
    // (postwork SF-3).
    const cardRuns = !card.deployed ? []
      : (historyFailed ? null : (runsByBinding.get(card.bindingID) || []));
    const cardGroups = !card.deployed || historyFailed ? [] : (history?.groups || []);
    host.appendChild(card.kind === 'reviewer'
      ? reviewerAgentCard(refresh, card, review)
      : managedAgentCard(refresh, card, managed, agents, roots, cardRuns, cardGroups));
  }
  // History is never hidden: reviewer invocations render even when no
  // reviewer card carries them (e.g. binding row lost).
  if (review && !cards.some(card => card.kind === 'reviewer' && card.deployed) && (review.recent || []).length) {
    host.appendChild(reviewHistorySection(review.recent));
  }
  if (profilesPayload) host.appendChild(profileStorageDiagnostics(profilesPayload));
  if (Array.isArray(agents?.signals) && agents.signals.length) {
    const signals = document.createElement('details');
    const summary = document.createElement('summary');
    summary.textContent = 'Published signal catalog (' + agents.signals.length + ')';
    signals.appendChild(summary);
    for (const signal of agents.signals) {
      signals.appendChild(el('div', 'sub', String(signal.kind || '') + ' · ' + String(signal.source || '')
        + (signal.terminal ? ' · terminal' : '')
        + (signal.served === false ? ' · not currently served' : '')
        + ' — ' + String(signal.description || '')));
    }
    host.appendChild(signals);
  }
}

/* ---------- shared card sections ---------- */

const STATE_CHIP = Object.freeze({
  'enabled': 'st-verified', 'needs attention': 'st-disputed', 'disabled': 'st-stale', 'not deployed': '',
});

function cardHeader(card, priorityText) {
  const head = el('div', 'row');
  head.append(el('strong', '', '⚖ ' + card.name),
    el('span', 'chip', card.type),
    el('span', 'chip ' + (STATE_CHIP[card.state] || ''), card.state));
  if (priorityText) head.appendChild(el('span', 'sub', priorityText));
  return head;
}

// promptSection renders the owner direction's "prompt first": role copy,
// description, expandable full prompt, then the set-and-forget identity.
function promptSection(card) {
  const section = el('div', 'agents-card-prompt');
  section.appendChild(el('div', 'sub', ROLE_COPY[card.type] || 'Unknown agent type.'));
  if (card.description) section.appendChild(el('div', 'sub', card.description));
  const profileID = String(card.profile?.profile_id || card.binding?.profile_id || '');
  if (profileID) {
    // Prompt FIRST, readably: the bounded instruction excerpt is always
    // visible; the exact full PROFILE.md source (frontmatter included) is one
    // toggle away — not a wall of YAML by default.
    const excerpt = String(card.model?.promptExcerpt || '');
    if (excerpt) section.appendChild(el('pre', 'agent-prompt-body agent-prompt-excerpt', excerpt));
    const details = document.createElement('details');
    details.className = 'agents-roster-prompt';
    const summary = document.createElement('summary');
    summary.textContent = excerpt ? 'Full prompt · exact selected source' : 'Full prompt';
    const body = el('pre', 'agent-prompt-body', 'Loading full prompt…');
    details.append(summary, body);
    let loaded = false;
    details.addEventListener('toggle', () => {
      if (!details.open || loaded) return;
      loaded = true;
      loadProfile(profileID)
        .then(detail => { body.textContent = String(detail.source || excerpt || 'Prompt source unavailable.'); })
        .catch(error => { body.textContent = 'Full prompt unavailable: ' + String(error?.message || error); });
    });
    section.appendChild(details);
  } else {
    section.appendChild(el('div', 'sub', 'Prompt unavailable: the pinned profile revision is gone.'));
  }
  if (card.profile) section.appendChild(profileIdentitySection(card.profile));
  return section;
}

function deployField(label, input) {
  const wrapper = el('label', 'orchestration-review-field');
  wrapper.append(el('span', 'sub', label), input);
  return wrapper;
}

function sectionTitle(text) { return el('strong', 'agents-card-section-title', text); }

function runHistory(runs) {
  // runs === null means the history READ failed — saying "0 runs" then would
  // be a lie; the card states unavailability instead.
  if (runs === null) {
    return el('div', 'sub banner', 'Run history unavailable: the managed projection read failed. Reload to retry.');
  }
  const details = document.createElement('details');
  const summary = document.createElement('summary');
  summary.textContent = 'Run history (' + runs.length + ')';
  details.appendChild(summary);
  if (!runs.length) {
    details.appendChild(el('div', 'sub', 'No recorded runs. This does not imply the watched sessions were idle.'));
    return details;
  }
  for (const run of runs.slice(0, 25)) {
    const row = el('div', 'sub agents-roster-run');
    row.append(el('span', 'chip', String(run.state || 'unknown').replaceAll('_', ' ')),
      el('span', '', ' ' + String(run.detail?.verdict || run.message || run.action || 'no claim')
        + ' · group ' + String(run.group_id || 'unknown').slice(0, 12)
        + (run.completed_at ? ' · ' + fmtTime(run.completed_at) : '')));
    // Attribution (provider-outage plan, invariant 2): which model actually
    // ran, and why the work moved routes, are never hidden — especially on
    // judgment-shaped output.
    const rerouted = run.detail?.rerouted_from;
    if (rerouted && typeof rerouted === 'object') {
      const outage = run.detail?.provider_outage;
      const attempts = Array.isArray(outage?.attempts) ? outage.attempts : [];
      const executed = attempts.length ? attempts[attempts.length - 1] : null;
      row.appendChild(el('span', 'chip',
        'rerouted: ' + String(rerouted.runtime || '?') + ' → '
        + (executed ? String(executed.runtime || '?') + (executed.model ? '/' + executed.model : '') : 'fallback')
        + ' (' + String(rerouted.class || 'provider failure').replaceAll('_', ' ') + ')'));
    }
    if (run.state === 'parked') {
      row.appendChild(el('span', '', ' · ' + String(run.recovery || 'provider unavailable; waiting to retry or reroute')));
    }
    details.appendChild(row);
  }
  if (runs.length > 25) details.appendChild(el('div', 'sub', 'Showing the 25 most recent of ' + runs.length + ' runs.'));
  return details;
}

/* ---------- managed (follower / helper) card ---------- */

function managedAgentCard(refresh, card, managed, agents, roots, runs, groups = []) {
  const section = el('section', 'runtime-integration agents-roster-card');
  const binding = card.binding;
  const model = card.model;
  section.appendChild(cardHeader(card, binding?.priority ? 'priority ' + binding.priority : ''));
  if (card.reason) section.appendChild(el('div', card.compatible && !card.needsAttention ? 'sub' : 'banner', card.reason));
  section.appendChild(promptSection(card));
  if (!card.compatible && !card.deployed) {
    // Incompatible or collided: the card states why and offers no Enable.
    section.appendChild(runHistory(runs));
    return section;
  }

  // Deployment scope — set-and-forget.
  section.appendChild(sectionTitle('Deployment scope'));
  const scope = el('div', 'orchestration-review-form');
  const project = document.createElement('input');
  project.placeholder = roots.length ? 'choose a repository root' : 'repository root';
  project.value = binding?.project_root || (roots.length === 1 ? roots[0] : '');
  project.setAttribute('aria-label', card.name + ' project root');
  if (roots.length) {
    const listID = 'agents-roots-' + (card.bindingID || card.name).replaceAll(/[^a-zA-Z0-9_-]/g, '');
    const datalist = document.createElement('datalist'); datalist.id = listID;
    roots.forEach(root => { const option = document.createElement('option'); option.value = root; datalist.appendChild(option); });
    project.setAttribute('list', listID);
    section.appendChild(datalist);
  }
  const scopeRuntime = document.createElement('input'); scopeRuntime.placeholder = 'all source runtimes';
  scopeRuntime.value = binding?.scope_runtime || '';
  scopeRuntime.setAttribute('aria-label', card.name + ' source runtime');
  const scopeSession = document.createElement('input'); scopeSession.placeholder = 'all sessions in project';
  scopeSession.value = binding?.scope_session || '';
  scopeSession.setAttribute('aria-label', card.name + ' exact source session');
  scope.append(deployField('Project root', project), deployField('Source runtime · optional', scopeRuntime),
    deployField('Exact source session · optional', scopeSession));
  section.appendChild(scope);

  const delivery = el('p', 'orchestration-muted');
  delivery.textContent = 'Messages to source sessions: ' + (managed?.chat_capabilities || []).map(item =>
    item.display_name + ': ' + (item.message_delivery?.supported
      ? item.message_delivery.boundary + '. ' + item.message_delivery.detail
      : item.message_delivery?.detail || 'Support not declared.')).join(' ');
  section.appendChild(delivery);

  // Runtime route.
  section.appendChild(sectionTitle('Runtime route'));
  const route = el('div', 'orchestration-review-form');
  const runtime = document.createElement('select');
  runtime.setAttribute('aria-label', card.name + ' agent runtime');
  const capabilities = (managed?.chat_capabilities || []).filter(item => item.can_start && item.modes?.some(mode => mode.risk === 'normal'));
  capabilities.forEach(item => { const option = document.createElement('option'); option.value = item.runtime; option.textContent = item.display_name; runtime.appendChild(option); });
  if (capabilities.some(item => item.runtime === binding?.runtime)) runtime.value = binding.runtime;
  const mode = document.createElement('select');
  mode.setAttribute('aria-label', card.name + ' read-only mode');
  const syncModes = () => {
    mode.replaceChildren();
    const capability = capabilities.find(item => item.runtime === runtime.value);
    (capability?.modes || []).filter(item => item.risk === 'normal').forEach(item => {
      const option = document.createElement('option'); option.value = item.id; option.textContent = item.label; mode.appendChild(option);
    });
    if ([...mode.options].some(item => item.value === binding?.mode)) mode.value = binding.mode;
  };
  runtime.onchange = () => { syncModes(); syncModels(); }; syncModes();
  // Model picker (natural-session plan B-GUI, M12): the honest preset list
  // from the selected runtime's published capabilities plus Custom…
  // preserving free text — labeled as presets, never a fake installed-models
  // list (live model enumeration stays the runtime-discovery follow-up).
  const modelSelect = document.createElement('select');
  modelSelect.setAttribute('aria-label', card.name + ' model');
  const customModelInput = document.createElement('input'); customModelInput.placeholder = 'exact provider/model';
  customModelInput.value = binding?.model || '';
  const syncModels = () => {
    modelSelect.replaceChildren();
    const capability = capabilities.find(item => item.runtime === runtime.value);
    const stored = binding?.model || '';
    const inPresets = !stored || (capability?.models || []).some(model => model.id === stored);
    const defaultOption = document.createElement('option');
    defaultOption.value = ''; defaultOption.textContent = 'Runtime default';
    modelSelect.appendChild(defaultOption);
    (capability?.models || []).forEach(model => {
      const option = document.createElement('option');
      option.value = model.id; option.textContent = model.label + (model.custom ? '' : ' · preset');
      modelSelect.appendChild(option);
    });
    const customOption = document.createElement('option');
    customOption.value = 'custom'; customOption.textContent = 'Custom…';
    modelSelect.appendChild(customOption);
    modelSelect.value = inPresets ? stored : 'custom';
    customModelInput.classList.toggle('hidden', modelSelect.value !== 'custom');
  };
  syncModels();
  const modelHost = el('div', 'row');
  modelHost.append(modelSelect, customModelInput);
  modelSelect.onchange = () => customModelInput.classList.toggle('hidden', modelSelect.value !== 'custom');
  const modelValue = () => modelSelect.value === 'custom' ? customModelInput.value.trim() : modelSelect.value;
  route.append(deployField('Agent runtime', runtime), deployField('Read-only mode', mode),
    deployField('Model · optional', modelHost));
  section.appendChild(route);

  // Watches natural sessions (natural-session plan Slice B): the consent
  // flag, default OFF — pre-checked only when the binding already opted in.
  section.appendChild(sectionTitle('Natural sessions'));
  const watchNatural = document.createElement('input'); watchNatural.type = 'checkbox';
  watchNatural.checked = Boolean(binding?.watch_natural);
  watchNatural.setAttribute('aria-label', card.name + ' watches natural sessions');
  const watchHost = el('div', 'row');
  watchHost.append(watchNatural, el('span', 'sub',
    'Watches naturally started sessions in this binding\u2019s scope (terminal/vendor-GUI work), not just console-started tasks.'));
  section.appendChild(watchHost);

  // Fallback routes (provider-outage plan Slice C): the user-authored chain a
  // provider outage may advance along. Ordered, at most two slots in this
  // editor; every entry is validated server-side to the same read-only mode
  // bar as the primary route \u2014 an outage can never widen authority.
  section.appendChild(sectionTitle('Fallback routes'));
  const routesHost = el('div', 'orchestration-review-form');
  const storedRoutes = Array.isArray(binding?.routes) ? binding.routes : [];
  const routeSlots = [];
  for (let slot = 0; slot < 2; slot++) {
    const slotRuntime = document.createElement('select');
    slotRuntime.setAttribute('aria-label', card.name + ' fallback route ' + (slot + 1) + ' runtime');
    const noneOption = document.createElement('option');
    noneOption.value = ''; noneOption.textContent = '\u2014 none \u2014';
    slotRuntime.appendChild(noneOption);
    capabilities.forEach(item => {
      const option = document.createElement('option');
      option.value = item.runtime; option.textContent = item.display_name;
      slotRuntime.appendChild(option);
    });
    const slotModel = document.createElement('input');
    slotModel.placeholder = 'model (optional, exact provider/model)';
    slotModel.setAttribute('aria-label', card.name + ' fallback route ' + (slot + 1) + ' model');
    const stored = storedRoutes[slot];
    if (stored && capabilities.some(item => item.runtime === stored.runtime)) {
      slotRuntime.value = stored.runtime;
      slotModel.value = stored.model || '';
    }
    routesHost.append(deployField('Fallback ' + (slot + 1) + ' runtime', slotRuntime),
      deployField('Fallback ' + (slot + 1) + ' model', slotModel));
    routeSlots.push({ runtime: slotRuntime, model: slotModel });
  }
  section.appendChild(routesHost);
  section.appendChild(el('div', 'sub',
    'When this agent\u2019s provider is down or out of quota, parked work advances along these routes in order \u2014 the model that actually ran is always shown on the run. Empty = park and wait.'));

  // Grants — requested by this profile, never implied by selection.
  section.appendChild(sectionTitle('Grants'));
  const authorityHost = el('div', 'orchestration-managed-authority');
  const auto = document.createElement('input'); auto.type = 'checkbox'; auto.checked = Boolean(binding?.auto_action);
  const requested = Array.isArray(card.requestedAuthority) ? card.requestedAuthority : [];
  authorityHost.appendChild(el('div', 'sub', 'Local grants · requested by this profile, never implied by selection'));
  requested.forEach(name => {
    const check = document.createElement('input'); check.type = 'checkbox'; check.value = name;
    check.checked = binding ? Boolean(binding.granted_authority?.includes(name)) : ['advise', 'draft-reply'].includes(name);
    const label = document.createElement('label'); label.append(check, ' ' + name.replaceAll('-', ' '));
    authorityHost.appendChild(label);
  });
  if (!requested.length) authorityHost.appendChild(el('div', 'sub', 'This profile requests no acting authority. Observe and tag only.'));
  if (card.type === 'helper') {
    const label = document.createElement('label');
    label.append(auto, ' automatically apply granted reply / launch / interrupt');
    authorityHost.appendChild(label);
  }
  // The plain-language powers line (owner direction, plan §2.5): what the
  // CURRENT grants mean, in words — from the binding when deployed, else
  // from the pre-checked defaults an Enable would grant.
  const powersSource = binding || {
    granted_authority: requested.filter(name => ['advise', 'draft-reply'].includes(name)),
    auto_action: false,
  };
  authorityHost.appendChild(el('div', 'sub agents-roster-powers', describePowers(powersSource)));
  section.appendChild(authorityHost);
  const priority = document.createElement('input'); priority.type = 'number'; priority.step = '1';
  priority.value = binding?.priority ? String(binding.priority) : '';
  priority.placeholder = '0'; priority.setAttribute('aria-label', card.name + ' priority');
  const declaredTags = document.createElement('input');
  declaredTags.placeholder = "profile's whole declared vocabulary";
  declaredTags.value = Array.isArray(binding?.declared_tags) ? binding.declared_tags.join(', ') : '';
  declaredTags.setAttribute('aria-label', card.name + ' declared tags');
  const arbitration = el('div', 'orchestration-review-form');
  arbitration.append(deployField('Priority · helpers arbitrate on it', priority),
    deployField('Declared tags · comma separated, optional', declaredTags));
  section.appendChild(arbitration);

  // Budgets — one form, saved with everything else by the one primary action.
  section.appendChild(sectionTitle('Budgets'));
  const budgetsGrid = el('div', 'orchestration-review-form');
  const limitInputs = new Map();
  const defaults = agents?.defaults || managed?.defaults || {};
  for (const [key, label] of BUDGET_FIELDS) {
    const input = document.createElement('input');
    input.type = 'number'; input.min = '0'; input.step = '1';
    const current = binding?.limits?.[key];
    input.value = current ? String(current) : '';
    input.placeholder = defaults[key] != null ? 'default ' + defaults[key] : 'shipped default';
    input.setAttribute('aria-label', card.name + ' ' + label);
    budgetsGrid.appendChild(deployField(label, input));
    limitInputs.set(key, input);
  }
  section.appendChild(budgetsGrid);
  section.appendChild(el('div', 'sub',
    'Empty uses the shipped default configuration. A budget bounds the next admission, not the in-flight turn (telemetry lag stated honestly).'));

  const sessionsLine = helperSessionsLine(helperSessionsSummary(groups, card.bindingID));
  if (sessionsLine) section.appendChild(el('div', 'sub', sessionsLine));
  section.appendChild(runHistory(runs));

  // ONE primary action. RT-11: the PUT always re-enables, so a disabled
  // card's action says so.
  const actions = el('div', 'row');
  const primaryLabel = card.deployed && card.enabled ? 'Save changes' : 'Review and enable';
  const save = el('button', 'btn primary', primaryLabel);
  save.onclick = async () => {
    const option = card.option;
    const digests = option && option.profile_id === (binding?.profile_id || card.profile?.profile_id)
      ? { source: option.source_digest, bundle: option.bundle_digest }
      : { source: binding?.profile_source_digest || card.profile?.source_digest, bundle: binding?.profile_bundle_digest || card.profile?.bundle_digest };
    const grants = [...authorityHost.querySelectorAll('input[type="checkbox"][value]:checked')].map(item => item.value);
    const limitValues = {};
    for (const [key, input] of limitInputs) {
      if (input.value.trim() === '') continue;
      const value = Number(input.value);
      if (!Number.isInteger(value) || value < 0) { section.prepend(el('div', 'banner', 'Budgets are whole non-negative numbers.')); return; }
      limitValues[key] = value;
    }
    const tags = declaredTags.value.split(',').map(tag => tag.trim()).filter(Boolean);
    if (!card.enableToken) {
      section.prepend(el('div', 'banner', 'No valid state token for this card; reload the page.'));
      return;
    }
    save.disabled = true;
    try {
      const response = await saveAgent(card.bindingID, {
        profile_id: binding?.profile_id || card.profile.profile_id,
        profile_source_digest: digests.source, profile_bundle_digest: digests.bundle,
        project_root: project.value.trim(),
        scope_runtime: scopeRuntime.value.trim(), scope_session: scopeSession.value.trim(),
        runtime: runtime.value, mode: mode.value, model: modelValue(),
        watch_natural: watchNatural.checked,
        routes: routeSlots
          .filter(slot => slot.runtime.value)
          .map(slot => ({ runtime: slot.runtime.value, model: slot.model.value.trim() })),
        granted_authority: grants, auto_action: card.type === 'helper' && auto.checked,
        priority: priority.value.trim() === '' ? 0 : Number(priority.value),
        declared_tags: tags.length ? tags : null, limits: limitValues,
      }, card.enableToken);
      await refresh(response.note || 'Agent binding saved.');
    } catch (error) {
      save.disabled = false;
      section.prepend(el('div', 'banner', 'Not saved: ' + String(error?.message || error)
        + (error?.code === 'state_conflict' ? ' — the binding changed elsewhere; reload this page.' : '')));
    }
  };
  actions.appendChild(save);
  if (card.deployed && card.enabled) {
    const disable = el('button', 'btn', 'Disable new triggers');
    disable.onclick = async () => {
      disable.disabled = true;
      try { await refresh((await disableAgent(card.bindingID, card.enableToken)).note || 'Agent disabled.'); }
      catch (error) { disable.disabled = false; section.prepend(el('div', 'banner', String(error?.message || error))); }
    };
    actions.appendChild(disable);
  }
  section.appendChild(actions);
  return section;
}

/* ---------- reviewer card (reviews routes; singleton binding) ---------- */

function reviewerAgentCard(refresh, card, settings) {
  const section = el('section', 'runtime-integration agents-roster-card orchestration-review-binding');
  const binding = card.deployed ? card.binding : null;
  const latest = settings?.recent?.[0]?.invocation;
  let stateLabel = card.state;
  if (binding?.state === 'enabled') {
    stateLabel = latest ? 'enabled · last review ' + String(latest.state || 'unknown').replaceAll('_', ' ')
      : 'enabled · availability unverified';
  }
  const head = el('div', 'row');
  head.append(el('strong', '', '⚖ ' + card.name),
    el('span', 'chip', 'Reviewer · independent tool-call review'),
    el('span', 'chip ' + (STATE_CHIP[card.state] || ''), stateLabel));
  section.appendChild(head);
  if (card.reason) section.appendChild(el('div', 'sub', card.reason));
  section.appendChild(promptSection(card));
  section.appendChild(el('div', 'sub', String(settings?.effect
    || 'It is report only and cannot create a defer, override a denial, or stop an allowed action.')));
  if (!card.compatible) { section.appendChild(reviewHistorySection(settings?.recent || [])); return section; }
  if (binding && settings?.update_available) {
    section.appendChild(el('div', 'banner',
      'Update available: the selected profile changed. New work remains pinned to the current binding until you review and save it.'));
  }

  // Deployment scope (a reviewer sees one event at a time — runtime filter is
  // its only scope field).
  section.appendChild(sectionTitle('Deployment scope'));
  const runtime = document.createElement('input'); runtime.placeholder = 'all connected runtimes';
  runtime.value = binding?.runtime_filter || ''; runtime.setAttribute('aria-label', 'Optional exact runtime');
  const scope = el('div', 'orchestration-review-form');
  scope.appendChild(deployField('Runtime · optional', runtime));
  section.appendChild(scope);

  // Runtime route — every carried element of the review lane stays (g4 plan
  // §2.4): endpoint, model, timeout, effect sync, subdeadline visibility,
  // destination and availability lines.
  section.appendChild(sectionTitle('Runtime route'));
  const form = el('div', 'orchestration-review-form');
  const endpoint = document.createElement('input'); endpoint.type = 'url';
  endpoint.placeholder = 'http://127.0.0.1:11434'; endpoint.value = binding?.endpoint || '';
  endpoint.setAttribute('aria-label', 'Literal-loopback Ollama endpoint');
  const model = document.createElement('input'); model.placeholder = 'exact local model name';
  model.value = binding?.model || ''; model.setAttribute('aria-label', 'Ollama model');
  const timeout = document.createElement('input'); timeout.type = 'number'; timeout.min = '1'; timeout.step = '1';
  timeout.value = binding ? String(Math.max(1, Math.round(binding.timeout_ms / 1000))) : '10';
  timeout.setAttribute('aria-label', 'Review timeout seconds');
  const effect = document.createElement('select'); effect.setAttribute('aria-label', 'Review effect');
  const subdeadline = document.createElement('input'); subdeadline.type = 'number'; subdeadline.min = '0.25';
  subdeadline.step = '0.25'; subdeadline.value = binding?.approval_subdeadline_ms
    ? String(binding.approval_subdeadline_ms / 1000) : '3';
  subdeadline.setAttribute('aria-label', 'Delegated review subdeadline seconds');
  const subdeadlineField = deployField('Reviewer budget inside approval · seconds', subdeadline);
  form.append(deployField('Local Ollama endpoint', endpoint), deployField('Model', model),
    deployField('Timeout · seconds', timeout), deployField('Effect', effect), subdeadlineField);
  const syncEffects = () => {
    const previous = effect.value;
    effect.replaceChildren();
    (card.option?.effects || ['report-only']).forEach(value => {
      const option = document.createElement('option'); option.value = value;
      option.textContent = value === 'delegated-first' ? 'Answer existing asks first' : 'Report only';
      effect.appendChild(option);
    });
    const pinnedEffect = binding?.profile_id === card.option?.profile_id ? binding?.effect : '';
    effect.value = [...effect.options].some(item => item.value === (pinnedEffect || previous))
      ? (pinnedEffect || previous) : effect.options[0]?.value;
    subdeadlineField.classList.toggle('hidden', effect.value !== 'delegated-first');
  };
  syncEffects();
  section.appendChild(form);
  if (binding) {
    section.append(el('div', 'sub', 'Data destination: ' + String(binding.endpoint)),
      el('div', 'sub', 'Binding state: ' + String(binding.state_token)));
  }
  if (settings?.availability) section.appendChild(el('div', 'sub', String(settings.availability)));

  // Grants: report-only has none. A reviewer answering existing asks has exactly
  // one, and it is the operator's to give: may it answer a question the held call
  // is asking, or only permit and refuse it?
  section.appendChild(sectionTitle('Grants'));
  const reportOnlyGrants = el('div', 'sub',
    'Observe and tag only — report-only by construction; no grants exist to give.');
  const answerPrompts = document.createElement('input');
  answerPrompts.type = 'checkbox';
  answerPrompts.checked = binding ? binding.answer_choice_prompts !== false : true;
  answerPrompts.setAttribute('aria-label', card.name + ' may answer choice prompts');
  const answerGrant = el('div', 'row');
  answerGrant.append(answerPrompts, el('span', 'sub',
    'May answer choice prompts. When a held call is asking a multiple-choice question, '
    + 'this reviewer may pick the answer as its allow. Off: it sees the call without the '
    + 'options and can only allow, deny, or abstain. It can never type an answer of its '
    + 'own, and you can always take over.'));
  section.append(reportOnlyGrants, answerGrant);
  const syncGrants = () => {
    const delegated = effect.value === 'delegated-first';
    answerGrant.classList.toggle('hidden', !delegated);
    reportOnlyGrants.classList.toggle('hidden', delegated);
  };

  // Budgets: pinned limits, read-only where bound.
  section.appendChild(sectionTitle('Budgets'));
  section.appendChild(el('div', 'sub', binding
    ? 'Pinned limits: ' + String(binding.max_input_bytes) + ' input bytes · '
      + String(binding.max_output_bytes) + ' output bytes · ' + String(binding.max_tokens)
      + ' tokens · ' + String(binding.max_concurrency) + ' concurrent'
    : 'Limits pin from the profile at enable time.'));

  effect.onchange = () => { syncEffects(); syncGrants(); };
  syncGrants();

  section.appendChild(reviewHistorySection(settings?.recent || []));

  const actions = el('div', 'row orchestration-review-actions');
  const save = el('button', 'btn primary', card.deployed && card.enabled ? 'Review and update binding' : 'Review and enable');
  save.onclick = async () => {
    const option = card.option;
    const timeoutMS = Number(timeout.value) * 1000;
    const subdeadlineMS = effect.value === 'delegated-first' ? Number(subdeadline.value) * 1000 : 0;
    if (!option || !endpoint.value.trim() || !model.value.trim() || !Number.isInteger(timeoutMS) || !Number.isInteger(subdeadlineMS)) {
      section.prepend(el('div', 'banner', 'Complete the endpoint, model, and whole-second timeout before enabling.'));
      return;
    }
    save.disabled = true;
    try {
      const response = await saveReviewBinding({ profile_id: option.profile_id,
        profile_source_digest: option.source_digest, profile_bundle_digest: option.bundle_digest,
        endpoint: endpoint.value.trim(), model: model.value.trim(), timeout_ms: timeoutMS,
        effect: effect.value, approval_subdeadline_ms: subdeadlineMS,
        answer_choice_prompts: answerPrompts.checked,
        runtime_filter: runtime.value.trim(), expected_state_token: card.enableToken });
      await refresh(response.note || 'Review binding saved.');
    } catch (error) {
      save.disabled = false;
      section.prepend(el('div', 'banner', 'Binding not changed: ' + String(error?.message || error)));
      if (error?.code === 'state_conflict') await refresh('Binding changed elsewhere; current state reloaded.');
    }
  };
  actions.appendChild(save);
  if (binding?.state === 'enabled') {
    const disable = el('button', 'btn', 'Disable new reviews');
    disable.onclick = async () => {
      disable.disabled = true; save.disabled = true;
      try { await refresh((await disableReviewBinding(card.enableToken)).note || 'New reviews disabled.'); }
      catch (error) {
        disable.disabled = false; save.disabled = false;
        section.prepend(el('div', 'banner', 'Binding not changed: ' + String(error?.message || error)));
      }
    };
    actions.appendChild(disable);
  }
  section.appendChild(actions);
  return section;
}

function reviewHistorySection(items) {
  const section = el('section', 'orchestration-review-history');
  const details = document.createElement('details');
  const summary = document.createElement('summary');
  summary.textContent = 'Recent tool reviews (' + items.length + ')';
  details.appendChild(summary);
  if (!items.length) {
    details.appendChild(el('div', 'sub', 'No review was admitted yet. This does not imply that no tool actions occurred.'));
  } else {
    items.forEach(item => details.appendChild(reviewCard(item)));
  }
  section.appendChild(details);
  return section;
}
