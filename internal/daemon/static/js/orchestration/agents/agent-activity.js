// Activity tab (plan §7): the agent's decisions, newest first, filterable by
// outcome and place, paged by keyset. A reviewer's runs render through the one
// review card.
import { el, button, stateLabel, problem, errorText, keyValue, selectBox, row, spacer, sequence } from './agent-ui.js';
import { loadRuns } from './roster-api.js';
import { OUTCOME_FILTERS, outcomeWords, placeName, relativeTime, labelFor, coverageWords } from './roster-model.js';
import { reviewCard } from '../session-reviews.js';
import { openSession } from '../../views/sessions.js';

export function renderActivity(pane, ctx) {
  const state = { outcome: '', place: '' };
  const totals = ctx.detail.totals || {};
  const chips = el('div', 'agents-filters');
  const places = ctx.agent.places || [];
  const placeFilter = selectBox([['', 'All places'], ...places.map(place => [place.place_id, placeName(place)])], '', 'Place');
  const table = el('div', 'agents-runs');
  const more = el('div', 'agents-more');
  const ticket = sequence();
  const reload = () => loadPage(ctx, state, table, more, '', ticket());
  for (const [key, label, total] of OUTCOME_FILTERS) {
    const chipNode = button(label, 'agents-filter' + (key === state.outcome ? ' active' : ''), () => {
      state.outcome = key;
      chips.querySelectorAll('.agents-filter').forEach(node => node.classList.toggle('active', node === chipNode));
      reload();
    });
    if (!ctx.detail.totals_unavailable) chipNode.appendChild(el('span', 'agents-filter-count', String(totals[total] || 0)));
    chips.appendChild(chipNode);
  }
  placeFilter.onchange = () => { state.place = placeFilter.value; reload(); };
  if (places.length > 1) chips.appendChild(placeFilter);
  pane.append(chips, table, more);
  reload();
}

async function loadPage(ctx, state, table, more, before, current) {
  if (!before) table.replaceChildren(el('div', 'agents-sub', 'Loading…'));
  more.replaceChildren();
  let page;
  try {
    page = await loadRuns(ctx.id, { place: state.place, outcome: state.outcome, before });
  } catch (error) {
    if (current()) table.replaceChildren(problem(errorText(error, 'Runs unavailable')));
    return;
  }
  if (!current()) return;
  if (!before) table.replaceChildren();
  for (const run of page.runs || []) table.appendChild(runRow(ctx, run));
  if (!table.children.length) table.appendChild(el('div', 'agents-empty', 'No runs.'));
  if (page.next) more.appendChild(button('Older runs', '', () => loadPage(ctx, state, table, more, page.next, current)));
}

function runRow(ctx, run) {
  const block = el('details', 'agents-run');
  const summary = document.createElement('summary');
  summary.append(el('span', 'agents-run-time', relativeTime(run.admitted_at)), stateLabel(run.outcome || 'none', outcomeWords(run)),
    el('span', 'agents-run-session', run.session?.title || run.repository || run.session?.catalog_id || ''),
    el('span', 'agents-run-said', firstLine(run.message)));
  block.appendChild(summary);
  block.addEventListener('toggle', () => {
    if (block.open && block.children.length === 1) block.appendChild(runDetail(ctx, run));
  }, { once: false });
  return block;
}

// deliveryReason is the receipt's structural cause, falling back to the
// older free-text reason on receipts written before reason_class existed.
function deliveryReason(delivery) {
  const reason = delivery.reason_class || delivery.reason || '';
  return reason ? ' · ' + String(reason) : '';
}

function runDetail(ctx, run) {
  if (run.review) return reviewCard(run.review);
  const detail = run.detail || {};
  const coverage = Array.isArray(detail.context_coverage) ? detail.context_coverage : [];
  const node = el('div', 'agents-run-detail');
  node.appendChild(keyValue([
    ['Moment', labelFor(ctx.detail.signals, run.signal) + (run.repository ? ' · ' + run.repository : '')],
    ['Said', run.message ? el('div', 'agents-prose', run.message) : ''],
    ['Cited', (run.citations || []).join(' · ')],
    ['Tags', Array.isArray(detail.tags) ? detail.tags.join(', ') : ''],
    ['Delivery', detail.delivery ? String(detail.delivery.state || '') + deliveryReason(detail.delivery) : ''],
    ['It read', coverageWords(coverage, [...(ctx.detail.context_kinds || []), ...(ctx.detail.context_always || [])])],
    ['Version', run.version ? 'v' + run.version : 'not stored any more'],
    ['State', String(run.state || '') + (run.error_class ? ' · ' + run.error_class : '')],
    ['Recovery', run.recovery || ''],
  ]));
  const sessionID = run.session?.catalog_id || run.session?.native_id;
  if (sessionID && run.session?.runtime) {
    node.appendChild(row(spacer(), button('Open session', '', () => openRunSession(run.session.runtime, sessionID))));
  }
  return node;
}

function openRunSession(runtime, id) {
  document.querySelector('nav button[data-view="sessions"]')?.click();
  setTimeout(() => openSession({ runtime, id }), 250);
}

function firstLine(text) {
  const line = String(text || '').split('\n').find(part => part.trim()) || '';
  return line.length > 160 ? line.slice(0, 159) + '…' : line;
}
