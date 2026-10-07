// The Agents index: one row per agent, no controls in rows (plan §7).
import { el, button, kindChip, stateLabel, sparkline, problem, errorText, row, spacer } from './agent-ui.js';
import { loadRoster } from './roster-api.js';
import { indexRows, agentIndexGroups, weekShares, filterCounts, INDEX_FILTERS, KIND_LABEL, relativeTime } from './roster-model.js';
import { importDialog } from './agent-dialog.js';
import { renderOutages } from './agent-outages.js';
import { modelsLink } from './route-picker.js';

export async function renderIndex(page, notice = '') {
  const { host } = page;
  const head = row(el('h2', 'agents-title', 'Agents'), spacer(),
    button('Import PROFILE.md', '', () => importDialog(async id => page.navigate(id ? { view: 'detail', id, tab: 'overview' } : { view: 'index' }))),
    button('New agent', 'primary', () => page.navigate({ view: 'new' })));
  head.classList.add('agents-head');
  host.appendChild(head);
  const body = el('div', 'agents-index-body', 'Loading agents…');
  host.appendChild(body);
  let roster;
  try {
    roster = await loadRoster();
  } catch (error) {
    body.replaceChildren(problem(errorText(error, 'Agents unavailable')), button('Retry', '', () => page.navigate({ view: 'index' })));
    return;
  }
  if (!body.isConnected) return;
  body.replaceChildren();
  if (notice) body.appendChild(el('div', 'agents-notice', notice));
  renderOutages(body, roster.outages, page);
  if (roster.places_unavailable) body.appendChild(problem('Where agents run cannot be read right now; their definitions are listed.'));
  for (const item of roster.problems || []) {
    body.appendChild(problem(String(item?.problem?.message || 'A stored agent failed verification.') + ' ' + String(item?.problem?.recovery || '')));
  }
  const rows = indexRows(roster, page.runtimeNames);
  if (!rows.length) {
    body.appendChild(el('div', 'agents-empty', 'No agents yet.'));
    return;
  }
  renderTable(page, body, rows, roster.stats_days);
}

function renderTable(page, body, rows, statsDays) {
  const state = { filter: 'all', query: '', idleOpen: false };
  const filters = el('div', 'agents-filters');
  const table = el('table', 'agents-table');
  const tbody = document.createElement('tbody');
  const redraw = () => {
    const groups = agentIndexGroups(rows, state.filter, state.query);
    tbody.replaceChildren(...groups.rows.map(item => indexRow(page, item)));
    if (groups.idle.length) {
      tbody.appendChild(idleHeader(groups.idle.length, state, redraw));
      if (state.idleOpen) tbody.append(...groups.idle.map(item => indexRow(page, item)));
    }
    for (const chipNode of filters.querySelectorAll('.agents-filter')) {
      chipNode.classList.toggle('active', chipNode.dataset.filter === state.filter);
      chipNode.setAttribute('aria-pressed', String(chipNode.dataset.filter === state.filter));
    }
  };
  const counts = filterCounts(rows);
  for (const [key, label] of INDEX_FILTERS) {
    const chipNode = button(label, 'agents-filter', () => { state.filter = key; redraw(); });
    chipNode.dataset.filter = key;
    chipNode.appendChild(el('span', 'agents-filter-count', String(counts[key] || 0)));
    filters.appendChild(chipNode);
  }
  const search = document.createElement('input');
  search.type = 'search'; search.placeholder = 'Search agents'; search.setAttribute('aria-label', 'Search agents');
  search.oninput = () => { state.query = search.value; redraw(); };
  filters.appendChild(search);
  const header = document.createElement('thead');
  const headRow = document.createElement('tr');
  for (const label of ['State', 'Agent', 'Where it runs', 'Model route', 'Last activity', 'Last ' + statsDays + ' days']) {
    const cell = el('th', '', label);
    // Rows carry no controls (plan §7); the way to what a route is sits on the column.
    if (label === 'Model route') cell.appendChild(modelsLink());
    headRow.appendChild(cell);
  }
  header.appendChild(headRow);
  table.append(header, tbody);
  const scroller = el('div', 'agents-table-wrap');
  scroller.appendChild(table);
  body.append(filters, scroller);
  redraw();
}

// idleHeader is the fold for agents that run nowhere: its count and a toggle.
function idleHeader(count, state, redraw) {
  const tr = document.createElement('tr');
  tr.className = 'agents-idle-row';
  const cell = el('td');
  cell.colSpan = 6;
  const toggle = button((state.idleOpen ? '▾ ' : '▸ ') + count + ' off or not deployed', 'ghost', () => { state.idleOpen = !state.idleOpen; redraw(); });
  toggle.setAttribute('aria-expanded', String(state.idleOpen));
  cell.appendChild(toggle);
  tr.appendChild(cell);
  return tr;
}

function indexRow(page, item) {
  const tr = document.createElement('tr');
  tr.className = 'agents-index-row';
  tr.tabIndex = 0;
  tr.setAttribute('role', 'link');
  tr.setAttribute('aria-label', 'Open ' + item.name);
  const open = () => page.navigate({ view: 'detail', id: item.id, tab: 'overview' });
  tr.onclick = open;
  tr.onkeydown = event => { if (event.key === 'Enter') open(); };
  const stateCell = el('td');
  stateCell.appendChild(stateLabel(item.state, item.stateLabel));
  if (item.attention) stateCell.appendChild(el('div', 'agents-sub agents-attention', item.attention));
  const agentCell = el('td', 'agents-agent-cell');
  const title = row(el('span', 'agents-name', item.name), kindChip(item.kind, KIND_LABEL[item.kind]));
  if (item.hasDraft && item.state !== 'draft') title.appendChild(el('span', 'agents-sub', 'draft'));
  agentCell.appendChild(title);
  // Where a shared agent came from, and an id a team agent also uses, each on
  // its own line under the name.
  if (item.origin) agentCell.appendChild(el('div', 'agents-sub agents-origin', item.origin));
  if (item.collision) agentCell.appendChild(el('div', 'agents-sub agents-origin', item.collision));
  agentCell.appendChild(el('div', 'agents-desc', item.description));
  tr.append(stateCell, agentCell, twoLine(item.where || '—', item.whereMore), twoLine(item.route || '—', item.routeLocality),
    twoLine(item.lastWords || '—', item.lastAt ? relativeTime(item.lastAt) : ''), weekCell(item));
  return tr;
}

function twoLine(first, second) {
  const cell = el('td', 'agents-cell');
  cell.appendChild(el('div', '', first));
  if (second) cell.appendChild(el('div', 'agents-sub', second));
  return cell;
}

function weekCell(item) {
  const cell = el('td', 'agents-nowrap');
  if (item.statsUnavailable) {
    cell.appendChild(el('div', 'agents-sub', '\u2014 run history unavailable'));
    return cell;
  }
  if (item.spark.some(Boolean)) cell.appendChild(sparkline(item.spark));
  const shares = weekShares(item);
  if (shares) {
    const bar = el('div', 'agents-week-bar');
    bar.setAttribute('role', 'img');
    bar.setAttribute('aria-label', item.acted + ' acted, ' + item.failed + ' failed, of ' + item.runs + ' runs');
    for (const [cls, share] of [['agents-bar-acted', shares.acted], ['agents-bar-other', shares.other], ['agents-bar-failed', shares.failed]]) {
      const piece = el('i', cls);
      piece.style.width = share + '%';
      bar.appendChild(piece);
    }
    cell.appendChild(bar);
  }
  cell.appendChild(el('div', 'agents-sub', item.runs
    ? item.runs + ' runs · ' + item.acted + ' acted' + (item.failed ? ' · ' + item.failed + ' failed' : '') : 'no runs'));
  return cell;
}
