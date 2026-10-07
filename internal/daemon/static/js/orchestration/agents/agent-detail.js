// One agent's detail page: header facts, tabs, and the ⋯ menu (plan §7).
import { el, button, linkButton, kindChip, chip, stateLabel, problem, errorText, row, spacer, keyValue } from './agent-ui.js';
import { loadAgent, batch } from './roster-api.js';
import { KIND_LABEL, kindOf, placeName, referencePlace, relativeTime, attentionWords, outcomeWords, agentState, STATE_LABEL } from './roster-model.js';
import { routeFacts, originView, collisionView, placeStatus } from './route-model.js';
import { modelsLink } from './route-picker.js';
import { addShareItem } from './agent-share.js';
import { confirmDialog } from './agent-dialog.js';
import { disableReviewBinding } from '../review-api.js';
import { renderOverview } from './agent-overview.js';
import { renderInstructions } from './agent-instructions.js';
import { renderPlaces } from './agent-places.js';
import { renderActivity } from './agent-activity.js';
import { renderSettings } from './agent-settings.js';
import { renderVersions } from './agent-versions.js';

const TABS = Object.freeze([
  ['overview', 'Overview', renderOverview], ['instructions', 'Instructions', renderInstructions],
  ['places', 'Where it runs', renderPlaces], ['activity', 'Activity', renderActivity],
  ['settings', 'Settings', renderSettings], ['versions', 'Versions', renderVersions],
]);

export async function renderDetail(page, id, tab, options = {}) {
  const { host } = page;
  host.appendChild(row(linkButton('‹ Agents', () => page.navigate({ view: 'index' }))));
  const body = el('div', 'agents-detail', 'Loading agent…');
  host.appendChild(body);
  let detail;
  try {
    detail = await loadAgent(id);
  } catch (error) {
    body.replaceChildren(problem(errorText(error, 'Agent unavailable')));
    return;
  }
  if (!body.isConnected) return;
  const ctx = detailContext(page, id, detail);
  body.replaceChildren(header(ctx));
  const tabBar = el('div', 'agents-tabs');
  tabBar.setAttribute('role', 'tablist');
  const pane = el('div', 'agents-tabpane');
  pane.setAttribute('role', 'tabpanel');
  pane.id = 'agents-tabpane';
  body.append(tabBar, pane);
  const select = async (key, tabOptions = {}) => {
    ctx.tab = key;
    for (const node of tabBar.children) {
      const active = node.dataset.tab === key;
      node.classList.toggle('active', active);
      node.setAttribute('aria-selected', String(active));
      if (active) pane.setAttribute('aria-labelledby', node.id);
    }
    pane.replaceChildren();
    if (tabOptions.notice) pane.appendChild(problem(tabOptions.notice));
    const entry = TABS.find(([name]) => name === key) || TABS[0];
    await entry[2](pane, ctx, tabOptions);
  };
  ctx.openTab = select;
  for (const [key, label] of TABS) tabBar.appendChild(tabButton(key, label, ctx, select));
  await select(tab, options);
}

function detailContext(page, id, detail) {
  const agent = detail.agent || {};
  return {
    page, id, detail, agent,
    kind: kindOf(agent),
    // A shared agent's definition is read-only here (OD-21): Edit offers Duplicate.
    origin: originView(agent),
    duplicate: () => page.navigate({ view: 'new', duplicateFrom: id }),
    profile: detail.current?.normalized || null,
    draft: detail.draft || null,
    runtimeNames: page.runtimeNames,
    capabilities: page.capabilities,
    reload: (tab, options = {}) => page.navigate({ view: 'detail', id, tab: tab || 'overview', options }),
  };
}

function tabButton(key, label, ctx, select) {
  const node = button(label, 'agents-tab', () => select(key));
  node.dataset.tab = key;
  node.id = 'agents-tab-' + key;
  node.setAttribute('role', 'tab');
  node.setAttribute('aria-controls', 'agents-tabpane');
  const count = key === 'places' ? ctx.agent.places?.length : key === 'versions' ? (ctx.detail.history?.length || 0) + (ctx.draft ? 1 : 0) : 0;
  if (count) node.appendChild(el('span', 'agents-tab-count', String(count)));
  return node;
}

function header(ctx) {
  const { agent, detail } = ctx;
  const node = el('div', 'agents-detail-head');
  const edit = ctx.origin?.readOnly
    ? button('Duplicate…', '', ctx.duplicate)
    : button('Edit instructions', '', () => ctx.openTab('instructions', { edit: true }));
  const title = row(el('h2', 'agents-title', agent.name || ctx.id), kindChip(ctx.kind, KIND_LABEL[ctx.kind]),
    ctx.origin ? chip(ctx.origin.mark, 'agents-origin-chip') : null, spacer(), edit, moreMenu(ctx));
  node.append(title, el('div', 'agents-desc', agent.description || ''));
  const collision = collisionView(agent);
  if (collision) node.appendChild(collisionBanner(ctx, collision));
  if (agent.attention) node.appendChild(problem(attentionWords(agent.attention, agent)));
  node.appendChild(facts(ctx));
  if (ctx.origin) node.appendChild(keyValue(ctx.origin.facts));
  if (detail.places_unavailable) node.appendChild(problem('Where this agent runs cannot be read right now.'));
  return node;
}

// collisionBanner says, on the member's own agent, that a team agent with the
// same id will not be adopted (OD-6, Q5), and offers the way out.
function collisionBanner(ctx, collision) {
  const node = el('div', 'agents-warnline agents-collision');
  node.append(el('span', '', collision.banner), button('Duplicate under a new id…', '', ctx.duplicate));
  return node;
}

function facts(ctx) {
  const { agent, detail } = ctx;
  const line = el('div', 'agents-facts');
  const places = agent.places || [];
  if (!places.length) {
    const state = agentState(agent);
    line.appendChild(stateLabel(state, STATE_LABEL[state]));
  }
  for (const place of places.slice(0, 3)) {
    const status = placeStatus(place, agent.origin);
    line.appendChild(stateLabel(status.state, status.label + ' in ' + placeName(place)));
  }
  if (places.length > 3) line.appendChild(el('span', 'agents-sub', '+ ' + (places.length - 3) + ' more'));
  const version = detail.current?.current?.version || agent.version;
  if (version) line.appendChild(el('span', 'agents-mono', 'v' + version));
  const reference = referencePlace(places) || places[0];
  if (reference) line.append(...routeSpans(reference));
  const stats = agent.stats || {};
  if (stats.last_at) {
    line.appendChild(el('span', 'agents-sub', outcomeWords({ outcome: stats.last_outcome, action: stats.last_action })
      + ' · ' + relativeTime(stats.last_at)));
  }
  return line;
}

// routeSpans are the facts line's words for the route: its name, where it runs,
// and the link to Settings → Models. Nothing else of a route is shown here.
function routeSpans(place) {
  const route = routeFacts(place);
  const out = [el('span', '', route.short)];
  if (route.locality) out.push(el('span', 'agents-sub', route.locality));
  out.push(modelsLink());
  return out;
}

function moreMenu(ctx) {
  const wrapper = el('div', 'agents-menu');
  const list = el('div', 'agents-menu-list');
  const toggle = button('⋯', 'ghost', () => list.classList.toggle('open'));
  toggle.setAttribute('aria-label', 'More actions');
  toggle.setAttribute('aria-haspopup', 'true');
  const item = (label, action) => { const node = button(label, 'agents-menu-item', () => { list.classList.remove('open'); action(); }); return node; };
  list.append(item('Duplicate…', ctx.duplicate));
  if (ctx.detail.current?.source) list.append(item('Export PROFILE.md', () => exportSource(ctx.detail.current.source)));
  // Share with team… is the lead's act on their own published agent, on a linked
  // device (plan §4.3, Q13a); the item appears once the link is known.
  if (!ctx.origin && ctx.detail.current) addShareItem(ctx, list, item);
  if ((ctx.agent.places || []).some(place => place.state === 'enabled')) list.append(item('Turn off everywhere…', () => turnOffEverywhere(ctx)));
  wrapper.append(toggle, list);
  return wrapper;
}

function exportSource(text) {
  const link = document.createElement('a');
  link.href = URL.createObjectURL(new Blob([String(text)], { type: 'text/markdown' }));
  link.download = 'PROFILE.md';
  link.click();
  setTimeout(() => URL.revokeObjectURL(link.href), 1000);
}

function turnOffEverywhere(ctx) {
  const enabled = (ctx.agent.places || []).filter(place => place.state === 'enabled');
  confirmDialog('Turn off everywhere', enabled.map(placeName).join(', '), 'Turn off ' + enabled.length, async () => {
    const managed = enabled.filter(place => place.lane === 'managed')
      .map(place => ({ binding_id: place.place_id, expected_state_token: place.state_token, op: 'disable' }));
    if (managed.length) await batch(managed);
    const review = enabled.find(place => place.lane === 'review');
    if (review) await disableReviewBinding(review.state_token);
    await ctx.reload(ctx.tab);
  });
}
