// Pure view models for the Agents pages (agents-settings-redesign plan §6–§7).
// Everything here maps daemon facts onto words and shapes: nothing fetches,
// nothing touches the DOM, and no runtime or vendor name appears — runtime
// names, signal labels, context labels and enforced limits arrive as data.

import { REPLY_NOT_SENT } from '../../task/session-status.js';
import { routeFacts, routeKey, fallbackKey, originView, collisionView, stoppedWords } from './route-model.js';

/* ---------- kinds and states ---------- */

export const KIND_LABEL = Object.freeze({
  reviewer: 'Reviewer', follower: 'Follower', helper: 'Helper', unknown: 'Agent',
});

export const KIND_BLURB = Object.freeze({
  reviewer: 'Looks at one action at a time and gives a verdict. Can answer approvals if you allow it.',
  follower: 'Reads the session as it goes and labels it with tags. Never speaks into it.',
  helper: 'Reads the session and can act — reply, or send a message — within what you allow.',
});

const ATTENTION_WORDS = Object.freeze({
  revision_unavailable: 'A place runs a version that is no longer stored',
  profile_problem: 'Its stored definition failed verification',
  provider_outage: 'Its model is not answering',
  recent_failures: 'Its recent runs all failed',
  flow_ceiling_breach: 'A run tried to go past its flow\u2019s limits',
});

export const STATE_LABEL = Object.freeze({
  attn: 'Needs attention', on: 'On', off: 'Off', draft: 'Draft', none: 'Not deployed',
});

// agentState is the one state an index row shows: attention first, then on,
// off (placed but every place off), draft (never published), or not deployed.
export function agentState(agent = {}) {
  if (agent.attention) return 'attn';
  const places = Array.isArray(agent.places) ? agent.places : [];
  if (places.some(place => place.state === 'enabled')) return 'on';
  if (places.length) return 'off';
  if (agent.draft_only) return 'draft';
  return 'none';
}

// attentionWords words the daemon's attention code. A place that is on and
// starts no run is said with its own reason (who shared it, since when) when the
// agent is given.
export function attentionWords(code, agent = null) {
  return (agent && stoppedWords(agent)) || ATTENTION_WORDS[code] || '';
}

export function kindOf(agent = {}) {
  return KIND_LABEL[agent.agent_type] ? agent.agent_type : 'unknown';
}

/* ---------- outcomes ---------- */

// The one display-label map of claim actions (escalation-delivery plan
// P2-10): display words only, allowlisted by name in the vocabulary scan.
// Classification never happens here — the daemon projects it.
const ACTION_WORDS = Object.freeze({
  reply: 'replied', draft_reply: 'drafted a reply', advise: 'gave advice', advise_user: 'flagged a problem',
  ask_owner: 'asked you',
  launch_profile: 'asked to launch a helper', request_interrupt: 'asked to interrupt',
  allow: 'allowed', deny: 'denied', abstain: 'abstained',
});

const DELIVERED = new Set(['accepted', 'delivered']);

// messageWords says what happened to a send_message claim: "sent" only when
// the delivery receipt says it arrived (plan §6, RT-14).
export function messageWords(detail = {}) {
  const state = String(detail?.delivery?.state || '');
  if (DELIVERED.has(state)) return 'sent a message';
  if (state === 'pending') return 'message waiting for the session';
  if (state) return 'message not delivered (' + state.replaceAll('_', ' ') + ')';
  return 'wrote a message';
}

// outcomeWords turns a run's outcome class (plus its action, tags and receipt)
// into the owner's plain words (D-8). The raw state stays in run detail.
export function outcomeWords(run = {}) {
  const detail = run.detail || {};
  const tags = Array.isArray(detail.tags) ? detail.tags.map(String) : [];
  switch (run.outcome) {
    case 'acted':
      if (run.action === 'send_message') return messageWords(detail);
      // The receipt decides, through the daemon's class (RT-15).
      if (run.attention_class === 'draft' && detail.delivery) return REPLY_NOT_SENT;
      if (run.action === 'reply' && (detail.auto_reply_suppressed || detail.auto_action_suppressed)) return 'held a reply';
      if (tags.length && (!run.action || run.action === 'no_action')) return 'tagged ' + tags.join(', ');
      return ACTION_WORDS[run.action] || (run.action ? run.action.replaceAll('_', ' ') : 'reviewed');
    case 'quiet': return run.action === 'abstain' ? 'abstained' : 'stayed quiet';
    case 'failed': return 'failed';
    case 'skipped': return 'skipped';
    case 'deferred': return 'another helper acted';
    case 'waiting': return 'waiting for its model';
    case 'running': return 'running';
    default: return String(run.state || 'unknown').replaceAll('_', ' ');
  }
}

export const OUTCOME_FILTERS = Object.freeze([
  ['', 'All', 'runs'], ['acted', 'Acted', 'acted'], ['quiet', 'Stayed quiet', 'quiet'],
  ['failed', 'Failed', 'failed'], ['skipped', 'Skipped', 'skipped'],
]);

/* ---------- places ---------- */

export function repositoryName(projectRoot) {
  const clean = String(projectRoot || '').replace(/[\\/]+$/, '');
  if (!clean) return '';
  const parts = clean.split(/[\\/]/);
  return parts[parts.length - 1] || clean;
}

export function placeName(place = {}) {
  if (place.lane === 'review') return 'Every repository';
  return place.repository || repositoryName(place.project_root) || place.place_id || 'place';
}

// sessionScopeWords says which sessions a place watches, in the user's terms.
export function sessionScopeWords(place = {}, runtimeNames = {}) {
  const runtimeOnly = runtime => ' · ' + (runtimeNames[runtime] || runtime) + ' only';
  if (place.lane === 'review') {
    return place.runtime_filter ? 'Every repository' + runtimeOnly(place.runtime_filter) : 'Every repository';
  }
  let words;
  if (place.scope_session) words = 'One session ' + String(place.scope_session).slice(0, 10) + '…';
  else if (place.watch_natural) words = 'Every session, including terminal';
  else words = 'Sessions started from the console';
  return place.scope_runtime ? words + runtimeOnly(place.scope_runtime) : words;
}

// referencePlace is the place the agent's settings are read from: the most
// recently updated enabled managed place, else the most recently updated one.
export function referencePlace(places = []) {
  const managed = places.filter(place => place.lane !== 'review');
  const pool = managed.some(place => place.state === 'enabled')
    ? managed.filter(place => place.state === 'enabled') : managed;
  return pool.reduce((best, place) => (!best || Number(place.updated_at || 0) > Number(best.updated_at || 0) ? place : best), null);
}

const COMPARED_FIELDS = Object.freeze([
  ['model', routeKey],
  ['permissions', place => [...(place.granted_authority || [])].sort().join(',') + '|' + Boolean(place.auto_action)],
  ['priority', place => String(place.priority || 0)],
  ['tags', place => [...(place.declared_tags || [])].sort().join(',')],
  ['budgets', place => String(place.limits?.max_total || 0) + '/' + String(place.limits?.loop_budget || 0)],
  ['fallback', fallbackKey],
]);

// placeDifferences names the settings on which a place differs from the
// reference place (D-2: a place may differ; the page marks it).
export function placeDifferences(reference, place) {
  if (!reference || !place || reference === place || place.lane === 'review') return [];
  return COMPARED_FIELDS.filter(([, key]) => key(reference) !== key(place)).map(([name]) => name);
}

/* ---------- index rows ---------- */

export function indexRow(agent = {}, runtimeNames = {}) {
  const places = Array.isArray(agent.places) ? agent.places : [];
  const enabled = places.filter(place => place.state === 'enabled');
  const first = enabled[0] || places[0] || null;
  const state = agentState(agent);
  const extra = [];
  if (first && first.lane !== 'review' && enabled.length > 1) extra.push('+ ' + (enabled.length - 1) + ' more');
  if (first && first.lane === 'review' && first.runtime_filter) extra.push((runtimeNames[first.runtime_filter] || first.runtime_filter) + ' only');
  if (enabled.length && places.length > enabled.length) extra.push('+ ' + (places.length - enabled.length) + ' off');
  const stats = agent.stats || {};
  const totals = stats.totals || {};
  const route = first ? routeFacts(first) : null;
  const origin = originView(agent);
  const collision = collisionView(agent);
  return {
    id: String(agent.profile_id || ''),
    name: String(agent.name || agent.profile_id || 'agent'),
    description: String(agent.description || ''),
    kind: kindOf(agent),
    state,
    stateLabel: STATE_LABEL[state],
    attention: attentionWords(agent.attention, agent),
    origin: origin ? origin.mark : '',
    collision: collision ? collision.mark : '',
    where: first ? placeName(first) : '',
    whereMore: extra.join(' · '),
    route: route ? route.short : '',
    routeLocality: route ? route.locality : '',
    lastAt: Number(stats.last_at || 0),
    lastWords: stats.last_at ? outcomeWords({ outcome: stats.last_outcome, action: stats.last_action }) : '',
    spark: (stats.days || []).map(day => Number(day.runs || 0)),
    runs: Number(totals.runs || 0),
    acted: Number(totals.acted || 0),
    quiet: Number(totals.quiet || 0),
    failed: Number(totals.failed || 0),
    statsUnavailable: Boolean(agent.stats_unavailable),
    hasDraft: Boolean(agent.has_draft),
    search: [agent.name, agent.profile_id, agent.description, origin?.organization, ...places.map(placeName)].join(' ').toLowerCase(),
  };
}

const STATE_ORDER = Object.freeze({ attn: 0, on: 1, draft: 2, off: 3, none: 4 });

export function indexRows(roster = {}, runtimeNames = {}) {
  return (roster.agents || []).map(agent => indexRow(agent, runtimeNames))
    .sort((a, b) => (STATE_ORDER[a.state] - STATE_ORDER[b.state]) || a.name.localeCompare(b.name));
}

export const INDEX_FILTERS = Object.freeze([
  ['all', 'All'], ['on', 'On'], ['attn', 'Needs attention'], ['off', 'Off'], ['none', 'Not deployed'], ['draft', 'Drafts'],
]);

export function filterRows(rows, filter = 'all', query = '') {
  const needle = String(query || '').trim().toLowerCase();
  return rows.filter(row => {
    const inFilter = filter === 'draft' ? (row.state === 'draft' || row.hasDraft) : (filter === 'all' || row.state === filter);
    return inFilter && (!needle || row.search.includes(needle));
  });
}

// agentIndexGroups splits the index for display: with the "All" chip and an
// empty search, agents that run nowhere (off, or not deployed) fold into one
// collapsed group under the rest; any other chip or a search shows flat rows,
// so a match is never hidden inside the fold.
export function agentIndexGroups(rows, filter = 'all', query = '') {
  const shown = filterRows(rows, filter, query);
  if (filter !== 'all' || String(query || '').trim()) return { rows: shown, idle: [] };
  const idle = shown.filter(row => row.state === 'off' || row.state === 'none');
  return { rows: shown.filter(row => !idle.includes(row)), idle };
}

// weekShares is a week's runs split for the stacked bar: acted, failed, and
// everything else (quiet, skipped, deferred), as percentages of all runs.
export function weekShares(row) {
  const runs = Number(row.runs || 0);
  if (!runs) return null;
  const acted = Math.round(Number(row.acted || 0) / runs * 100), failed = Math.round(Number(row.failed || 0) / runs * 100);
  return { acted, failed, other: Math.max(0, 100 - acted - failed) };
}

export function filterCounts(rows) {
  const counts = { all: rows.length, on: 0, attn: 0, off: 0, none: 0, draft: 0 };
  for (const row of rows) {
    counts[row.state] = (counts[row.state] || 0) + 1;
    if (row.hasDraft && row.state !== 'draft') counts.draft += 1;
  }
  return counts;
}

/* ---------- powers and budgets ---------- */

const POWER_WORDS = Object.freeze({
  'draft-reply': 'Draft a reply for you to send',
  'reply': 'Reply as the next turn',
  'advise': 'Record advice on completed work',
  'launch-profile': 'Launch one allowed read-only helper',
  'send-message': 'Send a message into the session',
  'request-interrupt': 'Interrupt the session’s running task',
  'respond-approval': 'Answer an approval that is waiting',
});

// Powers that take effect without the owner when auto-action is on.
const ACTING_POWERS = Object.freeze(['reply', 'send-message', 'launch-profile', 'request-interrupt']);

export function powerWords(name) {
  return POWER_WORDS[name] || String(name).replaceAll('-', ' ');
}

// powersView lists every power the profile requested with whether the place
// grants it, and whether granted acting powers apply by themselves. The store
// keeps ONE auto-action flag per binding, so the view never pretends it is per
// power.
export function powersView(requested = [], place = null) {
  const granted = new Set(place?.granted_authority || []);
  const rows = requested.map(name => ({ name, words: powerWords(name), granted: granted.has(name), acting: ACTING_POWERS.includes(name) }));
  const actingGranted = rows.some(row => row.granted && row.acting);
  return { rows, actingGranted, automatic: Boolean(place?.auto_action) && actingGranted };
}

export function isActingPower(name) {
  return ACTING_POWERS.includes(name);
}

const LIMIT_LABEL = Object.freeze({
  max_total: 'runs per session', loop_budget: 'reply loop cycles', max_active: 'active at once',
  max_hops: 'hops', max_group_tokens: 'tokens per session', max_agent_tokens: 'tokens per agent',
});

// budgetsView shows exactly the budgets the daemon says it enforces, each with
// its own value or the shipped default.
export function budgetsView(limits = {}, defaults = {}, enforced = []) {
  return enforced.map(key => {
    const own = Number(limits?.[key] || 0);
    const fallback = defaults?.[key];
    return { key, label: LIMIT_LABEL[key] || key.replaceAll('_', ' '), value: own || (fallback ?? null), isDefault: !own };
  });
}

/* ---------- instructions ---------- */

// instructionsView shows what runs (RT-13): with stage prompts, each moment's
// prompt IS the instructions and the Markdown body is not sent; without them,
// the body is.
export function instructionsView(profile = {}) {
  const stages = Object.entries(profile?.stages || {}).sort(([a], [b]) => a.localeCompare(b));
  return {
    staged: stages.length > 0,
    prompts: stages.map(([signal, prompt]) => ({ signal, prompt: String(prompt) })),
    body: String(profile?.instructions || ''),
    replyShape: String(profile?.reply_shape || ''),
  };
}

export function labelFor(options = [], kind = '') {
  const found = options.find(option => option.kind === kind);
  return found?.label || kind;
}

/* ---------- versions and moves ---------- */

const COMPARE_GROUPS = Object.freeze([
  ['When it runs', profile => ({ event: profile?.trigger?.event || '', stages: Object.keys(profile?.stages || {}).sort() })],
  ['What it reads', profile => (profile?.context || []).map(item => [item.kind, item.required ? 'required' : '', item.max_bytes || ''].join(' '))],
  ['What it may do', profile => ({ output: profile?.output?.kind || '', authority: [...(profile?.authority_requests || [])].sort(), tags: [...(profile?.may_tag || [])].sort() })],
  ['Where data goes', profile => profile?.requirements?.destination?.locality || ''],
  ['Ceilings', profile => profile?.limits || {}],
]);

// compareGroups lists the plain-language groups two versions differ in, and
// the ones they do not. Instructions are compared as text (text-diff.js).
export function compareGroups(before, after) {
  const changed = [];
  const same = [];
  for (const [name, pick] of COMPARE_GROUPS) {
    const a = pick(before), b = pick(after);
    (JSON.stringify(a) === JSON.stringify(b) ? same : changed).push({ name, before: a, after: b });
  }
  const instructionsBefore = instructionsText(before);
  const instructionsAfter = instructionsText(after);
  const instructionsChanged = instructionsBefore !== instructionsAfter;
  if (!instructionsChanged) same.push({ name: 'Instructions' });
  return { changed, same: same.map(group => group.name), instructionsChanged, instructionsBefore, instructionsAfter };
}

export function instructionsText(profile) {
  if (!profile) return '';
  const view = instructionsView(profile);
  const parts = view.staged ? view.prompts.map(item => '[' + item.signal + ']\n' + item.prompt.trim()) : [view.body.trim()];
  if (view.replyShape) parts.push('[reply shape]\n' + view.replyShape.trim());
  return parts.filter(Boolean).join('\n\n');
}

// movePlan says what moving a place to another version does to its grants and
// tags (RT-15): kept, dropped because the version no longer requests them, and
// newly requested (not granted).
export function movePlan(place = {}, target = {}) {
  const requested = new Set(target?.authority_requests || []);
  const granted = place.granted_authority || [];
  const mayTag = new Set(target?.may_tag || []);
  const tags = place.declared_tags || [];
  return {
    grantsKept: granted.filter(name => requested.has(name)),
    grantsDropped: granted.filter(name => !requested.has(name)),
    grantsNew: [...requested].filter(name => !granted.includes(name)),
    tagsKept: tags.filter(tag => mayTag.has(tag)),
    tagsDropped: tags.filter(tag => !mayTag.has(tag)),
  };
}

/* ---------- time ---------- */

export function relativeTime(epochSeconds, nowSeconds = Date.now() / 1000) {
  const at = Number(epochSeconds || 0);
  if (!at) return '';
  const delta = Math.max(0, nowSeconds - at);
  if (delta < 60) return 'just now';
  if (delta < 3600) return Math.floor(delta / 60) + ' min ago';
  if (delta < 86400) return Math.floor(delta / 3600) + ' h ago';
  return new Date(at * 1000).toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
}

export function dayLabel(dayIndex) {
  return new Date(Number(dayIndex) * 86400000).toLocaleDateString(undefined, { weekday: 'short', timeZone: 'UTC' });
}

/* ---------- outputs ---------- */

const OUTPUT_WORDS = Object.freeze({
  'intervention': 'Sends a message into the session, or stays quiet',
  'draft-reply': 'Drafts a reply for the session',
  'advice': 'Records advice and applies tags',
  'review-recommendation': 'Recommends allow, deny or abstain on the action',
  'approval-response': 'Answers the waiting approval: allow, deny or abstain',
  'stage-classification': 'Classifies where the work stands',
});

export function outputWords(kind) {
  return OUTPUT_WORDS[kind] || String(kind || '').replaceAll('-', ' ');
}

// profileOf is the definition a detail page shows: the current version, else
// the draft of a never-published agent.
export function profileOf(detail = {}) {
  return detail.current?.normalized || detail.draft?.normalized || null;
}

// proseBlocks turns hard-wrapped instruction text into readable blocks: a
// paragraph's single line breaks become spaces; lists, headings, indented and
// fenced lines keep their breaks.
export function proseBlocks(text) {
  const keep = line => /^\s*([-*+]\s|\d+[.)]\s|#|>|```|\s{2,}\S)/.test(line);
  return String(text || '').split(/\n\s*\n/).map(block => block.replace(/^\n+|\s+$/g, '')).filter(Boolean)
    .map(block => (block.split('\n').some(keep) ? { keep: true, text: block } : { keep: false, text: block.split('\n').map(line => line.trim()).join(' ') }));
}

// repositoryChoices labels known repositories for a picker: busiest first;
// a folder name shared by several repositories gets its parent folders.
export function repositoryChoices(repos, taken = new Set()) {
  const parts = root => String(root).split('/').filter(Boolean);
  const counts = new Map();
  for (const repo of repos) counts.set(parts(repo.root).pop(), (counts.get(parts(repo.root).pop()) || 0) + 1);
  const label = repo => {
    const segments = parts(repo.root);
    const name = segments[segments.length - 1] || repo.root;
    const where = counts.get(name) > 1 ? ' \u2014 ' + segments.slice(-3, -1).join('/') : '';
    const sessions = repo.sessions ? ' \u00b7 ' + repo.sessions + (repo.sessions === 1 ? ' session' : ' sessions') : '';
    return name + where + sessions + (taken.has(repo.root) ? ' (already here)' : '');
  };
  return [...repos].sort((a, b) => (b.sessions || 0) - (a.sessions || 0) || a.root.localeCompare(b.root))
    .map(repo => ({ root: repo.root, label: label(repo), taken: taken.has(repo.root) }));
}

const COVERAGE_WORDS = Object.freeze({ supplied: 'read', empty: 'nothing there', unavailable: 'not available', truncated: 'cut to fit' });

// coverageWords names what one run read, in the words the editor uses.
export function coverageWords(coverage, options) {
  return (coverage || []).map(item => labelFor(options, item.kind) + ' (' + (COVERAGE_WORDS[item.state] || String(item.state || '')) + ')').join(' \u00b7 ');
}

// rerouteSummary counts a reroute preview's decisions and says them.
export function rerouteSummary(plan) {
  const counts = { reroute: 0, stale_draft: 0, no_route: 0 };
  for (const decision of plan?.decisions || []) counts[decision.action] = (counts[decision.action] || 0) + 1;
  const one = (n, single, many) => (n === 1 ? '1 ' + single : n + ' ' + many);
  const words = [one(counts.reroute, 'moves to its fallback route', 'move to their fallback route'),
    one(counts.stale_draft, 'stale helper reply is held for you as a draft', 'stale helper replies are held for you as drafts'),
    one(counts.no_route, 'has no fallback and stays parked', 'have no fallback and stay parked')];
  return { counts, words: words.join('; ') + '.' };
}

// deliveryWords says, per runtime, where a helper's message into a session
// lands — the published boundary — or that it is not delivered there.
export function deliveryWords(capabilities = []) {
  return capabilities.filter(item => item.messageDelivery)
    .map(item => item.displayName + ': ' + (item.messageDelivery.supported ? item.messageDelivery.boundary || 'delivered' : 'not delivered'))
    .join(' \u00b7 ');
}
