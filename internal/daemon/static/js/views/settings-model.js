// Settings shell model (settings-restructure plan §3.1, §3.2): the pages, what
// needs the owner, where a request lands, and what a search finds. Pure: no
// DOM, no fetch — the pages render from it and the tests pin it.
import { attentionWords } from '../orchestration/agents/roster-model.js';
import { outageWords, outageDetail } from '../orchestration/agents/route-model.js';
import { teamAttention } from './settings-team-model.js';

// [group, page id, label]. Ids used by other views (agents, appearance,
// transcript-modes, views) are unchanged; the old general page id maps to overview.
export const SETTINGS_PAGES = Object.freeze([
  ['', 'overview', 'Overview'],
  ['Connections', 'runtimes', 'Runtimes'],
  ['Connections', 'models', 'Models'],
  ['Connections', 'team', 'Team'],
  ['Automation', 'agents', 'Agents'],
  ['Console', 'appearance', 'Appearance'],
  ['Console', 'transcript-modes', 'Transcript modes'],
  ['Console', 'views', 'Session views'],
  ['Console', 'dictation', 'Dictation'],
  ['System', 'security', 'Security'],
  ['System', 'about', 'About'],
]);

const PAGE_IDS = new Set(SETTINGS_PAGES.map(([, id]) => id));

export function normalizePage(id) {
  const page = String(id || '');
  if (page === 'settings') return 'overview';
  return PAGE_IDS.has(page) ? page : '';
}

export function pageLabel(id) {
  return (SETTINGS_PAGES.find(([, page]) => page === id) || [])[2] || '';
}

// pageForRequest says what to show when Settings is entered or restored: the
// requested page and target when one was asked for, else what is mounted; and
// whether that differs from what is on screen (so it must be rendered).
export function pageForRequest(requested, mounted) {
  const page = normalizePage(requested?.page) || mounted?.page || 'overview';
  const target = requested ? String(requested.target || '') : String(mounted?.target || '');
  const changed = !mounted || mounted.page !== page || (Boolean(requested) && String(mounted.target || '') !== target);
  return { page, target, render: changed };
}

// settleDecision says what the shell does when Settings is entered (a fresh
// render) or restored from the stash: which page and target, whether to draw
// it, and whether to tell the Agents page what to show.
//   requested: { page, target, external } another view asked for, or null
//   mounted:   { page, target } last shown, or null
//   drawn:     the mounted page is drawn in the current shell
//   restored:  the pane came back from the stash (no render happened)
//   reread:    this page is read again on restore (and was not typed into)
// Only a request tells the Agents page anything: with nothing asked it keeps
// the agent the owner was reading. A request for Agents that names no agent
// means the list. A re-read with nothing asked names no target, so it cannot
// reopen what the owner has since closed.
export function settleDecision({ requested = null, mounted = null, drawn = false, restored = false, reread = false } = {}) {
  const asked = Boolean(requested);
  const external = Boolean(requested?.external);
  const next = pageForRequest(requested, mounted);
  const agentsList = asked && next.page === 'agents' && !external && !next.target;
  const rereadNow = restored && reread;
  return {
    page: next.page,
    target: !asked && rereadNow ? '' : next.target,
    show: next.render || agentsList || !drawn || rereadNow,
    tellAgents: asked && !external,
  };
}

const RUNTIME_ATTENTION = Object.freeze({
  hook_binary_missing: ['bad', 'Its hook is configured, but the hook program is gone'],
  hook_outdated: ['bad', 'Its hook needs repair'],
  needs_attention: ['bad', ''],
  preview_blocked: ['bad', ''],
  never_fired: ['info', 'Connected, but its hook has never fired'],
});

// settingsAttention lists what needs the owner, worst first. Every item is a
// fact a daemon read asserts; nothing here weighs a threshold. An item is
// { severity: 'bad'|'warn'|'info', page, target, title, facts }. Team's items are
// its page's needs-you block and nothing else (settings-team-model.js), so the
// Team count is absent exactly when that block is.
export function settingsAttention(reads = {}, runtimeNames = {}) {
  const items = [...teamAttention(reads.team, reads.teamLayers), ...agentAttention(reads.roster, runtimeNames),
    ...runtimeAttention(reads.runtimeStatus), ...speechAttention(reads.speech)];
  const order = { bad: 0, warn: 1, info: 2 };
  return items.sort((a, b) => order[a.severity] - order[b.severity]);
}

function agentAttention(roster, runtimeNames) {
  if (!roster) return [];
  const items = [];
  for (const outage of roster.outages || []) {
    items.push({ severity: 'bad', page: 'agents', target: '', title: outageWords(outage), facts: [outageDetail(outage)].filter(Boolean) });
  }
  for (const agent of roster.agents || []) {
    if (!agent.attention) continue;
    items.push({ severity: 'bad', page: 'agents', target: String(agent.profile_id || ''),
      title: String(agent.name || agent.profile_id || 'An agent'), facts: [attentionWords(agent.attention, agent) || String(agent.attention)] });
  }
  for (const stored of roster.problems || []) {
    items.push({ severity: 'bad', page: 'agents', target: '',
      title: String(stored?.problem?.message || 'A stored agent failed verification'), facts: [stored?.problem?.recovery].filter(Boolean) });
  }
  if (roster.places_unavailable) {
    items.push({ severity: 'bad', page: 'agents', target: '', title: 'Where agents run cannot be read', facts: [] });
  }
  return items;
}

function runtimeAttention(status) {
  const items = [];
  for (const runtime of status?.runtimes || []) {
    if (!runtime.attention) continue;
    const [severity, words] = RUNTIME_ATTENTION[runtime.attention] || ['bad', ''];
    items.push({ severity, page: 'runtimes', target: String(runtime.name || ''),
      title: String(runtime.display_name || runtime.name || ''), facts: [runtime.problem || words || String(runtime.attention)] });
  }
  return items;
}

function speechAttention(speech) {
  if (!speech) return [];
  const problems = Array.isArray(speech.problems) ? speech.problems.filter(Boolean) : [];
  if (speech.state !== 'unavailable' && !problems.length) return [];
  return [{ severity: 'warn', page: 'dictation', target: '', title: 'Dictation is not working',
    facts: problems.length ? problems : [speech.reason].filter(Boolean) }];
}

// attentionCounts: what the badges show. Info items are listed, never counted.
export function attentionCounts(items = []) {
  const counts = { total: 0, worst: '', pages: {} };
  for (const item of items) {
    if (item.severity === 'info') continue;
    counts.total += 1;
    const page = counts.pages[item.page] || (counts.pages[item.page] = { count: 0, worst: 'warn' });
    page.count += 1;
    if (item.severity === 'bad') page.worst = 'bad';
    if (item.severity === 'bad' || !counts.worst) counts.worst = item.severity;
  }
  return counts;
}

const ACCOUNT_SECRETS = Object.freeze(['base_url', 'auth_token']);

// forgetProviderTokens returns the stored chat defaults without any custom
// provider address or token — per runtime, and the legacy flat pair — keeping
// every other key (default runtime, folder, binary, arguments, pinned models).
export function forgetProviderTokens(defaults = {}) {
  const out = { ...defaults };
  for (const key of ACCOUNT_SECRETS) delete out[key];
  if (defaults.runtimes && typeof defaults.runtimes === 'object') {
    out.runtimes = {};
    for (const [runtime, stored] of Object.entries(defaults.runtimes)) {
      const kept = { ...(stored || {}) };
      for (const key of ACCOUNT_SECRETS) delete kept[key];
      out.runtimes[runtime] = kept;
    }
  }
  return out;
}

// storedProviderTokens counts the places a custom provider token is stored.
export function storedProviderTokens(defaults = {}) {
  let count = defaults.auth_token ? 1 : 0;
  for (const stored of Object.values(defaults.runtimes || {})) if (stored?.auth_token) count += 1;
  return count;
}

// The find index names pages and controls only. Runtime, model and agent names
// are matched from reads the caller already holds (the `named` argument).
const FIND_INDEX = Object.freeze([
  ['overview', 'What needs you', 'attention problems health status'],
  ['runtimes', 'Runtime connections', 'hook connect disconnect repair verify watch canary installed'],
  ['runtimes', 'New chats from this browser', 'chat default runtime folder working directory'],
  ['runtimes', 'Custom provider account', 'base url token binary extra arguments key'],
  ['models', 'Model lists', 'models context price refresh'],
  ['team', 'Team link', 'team organization link unlink device fingerprint server'],
  ['team', 'Waiting to send', 'waiting send outbox push pending high-water sync report'],
  ['team', 'What this device sends', 'disclosure privacy events sessions content memory'],
  ['team', 'Shared rules and agents', 'adopt un-adopt bundle organization key trust rules agents shared'],
  ['agents', 'Agents', 'agents helper follower reviewer outage reroute profile'],
  ['appearance', 'Colour scheme and themes', 'theme dark light system contrast'],
  ['appearance', 'Accent colour', 'accent colour color'],
  ['appearance', 'Fonts and text size', 'font size width transcript chat'],
  ['transcript-modes', 'Transcript mode rules', 'transcript mode hide fold collapse thinking tool'],
  ['transcript-modes', 'Mode each session opens with', 'opens with default helper follower reviewer'],
  ['views', 'Saved session views', 'views filter board group sort rail'],
  ['dictation', 'Dictation', 'speech dictation voice microphone push-to-talk'],
  ['security', 'Security', 'security token origin listener binary sign out forget'],
  ['about', 'Version and diagnostics', 'version build schema data directory log diagnostics'],
]);

export function findSettings(query, named = []) {
  const needle = String(query || '').trim().toLowerCase();
  if (!needle) return [];
  const hits = [];
  for (const [page, label, words] of FIND_INDEX) {
    if ((label + ' ' + words).toLowerCase().includes(needle)) hits.push({ page, target: '', label });
  }
  for (const entry of named) {
    if (String(entry.label || '').toLowerCase().includes(needle)) hits.push({ page: entry.page, target: String(entry.target || ''), label: String(entry.label) });
  }
  return hits.slice(0, 8);
}
