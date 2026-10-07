// Overview tab: the one-page read of an agent (plan §7). No inputs; each row
// links to the tab that changes it.
import { el, card, cardAction, linkButton, stateLabel, row, spacer, problem, errorText } from './agent-ui.js';
import { loadRuns } from './roster-api.js';
import {
  profileOf, labelFor, outputWords, powersView, referencePlace, placeName, sessionScopeWords,
  outcomeWords, relativeTime, dayLabel, instructionsView,
} from './roster-model.js';
import { routeFacts, placeStatus } from './route-model.js';
import { modelsLink } from './route-picker.js';

export async function renderOverview(pane, ctx) {
  const left = card('How it works', howItWorks(ctx));
  const right = el('div', 'agents-stack');
  right.append(statsCard(ctx), recentCard(ctx), placesCard(ctx));
  const grid = el('div', 'agents-cols');
  grid.append(left, right);
  pane.appendChild(grid);
}

function howItWorks(ctx) {
  const profile = profileOf(ctx.detail);
  const flow = el('div', 'agents-flow');
  if (!profile) {
    flow.appendChild(el('div', 'agents-sub', 'Its definition cannot be read.'));
    return flow;
  }
  const signals = ctx.detail.signals || [];
  const staged = instructionsView(profile);
  const when = staged.staged
    ? staged.prompts.map(item => labelFor(signals, item.signal)).join(' · ')
    : (ctx.detail.trigger_label || labelFor(signals, profile.trigger?.event));
  const reads = (profile.context || []).map(item => labelFor(ctx.detail.context_kinds, item.kind)
    + (item.required ? '' : ' (if available)')).join(' · ');
  const reference = referencePlace(ctx.agent.places || []);
  flowRow(flow, 'When', when, 'Instructions', () => ctx.openTab('instructions'));
  flowRow(flow, 'Reads', reads || 'nothing', 'Instructions', () => ctx.openTab('instructions'));
  flowRow(flow, 'Does', outputWords(profile.output?.kind), 'Instructions', () => ctx.openTab('instructions'));
  flowRow(flow, 'May', mayWords(profile, reference, ctx), 'Settings', () => ctx.openTab('settings'));
  flowRow(flow, 'Runs on', runsOnWords(profile, reference, ctx), 'Settings', () => ctx.openTab('settings'));
  flowRow(flow, 'Where', whereWords(ctx), 'Where it runs', () => ctx.openTab('places'));
  return flow;
}

function flowRow(flow, key, value, linkLabel, go) {
  const cell = el('span', 'agents-flow-v');
  cell.append(value);
  flow.append(el('span', 'agents-flow-k', key), cell, linkButton(linkLabel, go));
}

function mayWords(profile, reference, ctx) {
  const requested = profile.authority_requests || [];
  if (!requested.length) return 'Observe and tag only';
  if (!reference && ctx.kind !== 'reviewer') return 'Asks to: ' + requested.join(', ') + ' (granted per place)';
  const view = powersView(requested, reference);
  const granted = view.rows.filter(item => item.granted).map(item => item.words);
  if (!granted.length) return 'Nothing granted yet';
  return granted.join(' · ') + (view.actingGranted ? (view.automatic ? ' · acts automatically' : ' · waits for you') : '');
}

// runsOnWords is the route's name, where it runs, and the way to Settings →
// Models — nothing else of the route (team rest-of-release plan §5.5).
function runsOnWords(profile, reference, ctx) {
  const place = reference || (ctx.agent.places || [])[0];
  const node = el('span', 'agents-route-value');
  if (!place) {
    node.appendChild(el('span', '', 'Chosen per place'));
    return node;
  }
  const route = routeFacts(place);
  node.appendChild(el('span', '', route.name));
  if (route.locality) node.appendChild(el('span', 'agents-sub', route.locality));
  if (profile.requirements?.destination?.locality === 'local-only' && ctx.kind !== 'reviewer') {
    node.appendChild(el('span', 'agents-sub', 'asks for local-only, which helpers do not enforce'));
  }
  node.appendChild(modelsLink());
  return node;
}

function whereWords(ctx) {
  const places = ctx.agent.places || [];
  if (!places.length) return ctx.agent.draft_only ? 'Not published yet' : 'Not deployed';
  return places.map(place => placeName(place) + (place.state === 'enabled' ? '' : ' (off)')).join(' · ');
}

function statsCard(ctx) {
  if (ctx.agent.stats_unavailable) return card('Recent days', el('div', 'agents-sub', 'Run history cannot be read right now.'));
  const stats = ctx.agent.stats || {};
  const totals = stats.totals || {};
  const tiles = el('div', 'agents-stats');
  for (const [value, label, cls] of [[totals.runs, 'runs', ''], [totals.acted, 'acted', 'agents-acted'],
    [totals.quiet, 'stayed quiet', ''], [totals.failed, 'failed', 'agents-failed']]) {
    const tile = el('div', 'agents-stat');
    tile.append(el('b', cls, String(value || 0)), el('span', '', label));
    tiles.appendChild(tile);
  }
  const node = card('Last ' + (ctx.detail.stats_days || (stats.days || []).length) + ' days', tiles, bars(stats.days || []));
  return node;
}

function bars(days) {
  const max = Math.max(1, ...days.map(day => Number(day.runs || 0)));
  const chart = el('div', 'agents-bars');
  chart.setAttribute('role', 'img');
  chart.setAttribute('aria-label', days.map(day => dayLabel(day.day) + ' ' + (day.runs || 0) + ' runs').join(', '));
  const labels = el('div', 'agents-bar-labels');
  for (const day of days) {
    const column = el('div', 'agents-bar');
    const other = Number(day.runs || 0) - Number(day.acted || 0) - Number(day.failed || 0);
    for (const [count, cls] of [[day.acted, 'agents-bar-acted'], [other, 'agents-bar-other'], [day.failed, 'agents-bar-failed']]) {
      const piece = el('i', cls);
      piece.style.height = (Number(count || 0) / max * 100) + '%';
      column.appendChild(piece);
    }
    column.title = (day.runs || 0) + ' runs · ' + (day.acted || 0) + ' acted · ' + (day.failed || 0) + ' failed';
    chart.appendChild(column);
    labels.appendChild(el('span', '', dayLabel(day.day)));
  }
  const wrapper = el('div');
  wrapper.append(chart, labels);
  return wrapper;
}

function recentCard(ctx) {
  const list = el('div', 'agents-recent', 'Loading…');
  const node = card('Recent', list);
  cardAction(node, 'Activity', () => ctx.openTab('activity'));
  loadRuns(ctx.id, { limit: ctx.detail.overview_runs || 5 }).then(page => {
    list.replaceChildren(...(page.runs || []).map(run => recentRow(run)));
    if (!(page.runs || []).length) list.replaceChildren(el('div', 'agents-sub', 'No runs yet.'));
  }).catch(error => list.replaceChildren(problem(errorText(error, 'Runs unavailable'))));
  return node;
}

function recentRow(run) {
  const line = el('div', 'agents-recent-row');
  line.append(el('span', 'agents-sub agents-recent-time', relativeTime(run.admitted_at)),
    stateLabel(run.outcome || 'none', outcomeWords(run)),
    el('span', 'agents-recent-text', run.session?.title || run.repository || ''));
  return line;
}

function placesCard(ctx) {
  const places = ctx.agent.places || [];
  const list = el('div');
  for (const place of places) {
    const item = el('div', 'agents-where-row');
    const status = placeStatus(place, ctx.agent.origin);
    item.append(row(stateLabel(status.state, placeName(place)), spacer(),
      el('span', 'agents-mono', place.version ? 'v' + place.version : 'version unavailable')),
    el('div', 'agents-sub agents-where-scope', status.reason || sessionScopeWords(place, ctx.runtimeNames)));
    list.appendChild(item);
  }
  if (!places.length) list.appendChild(el('div', 'agents-sub', 'Not deployed.'));
  const node = card('Where it runs', list);
  cardAction(node, 'Manage', () => ctx.openTab('places'));
  return node;
}
