// A view's read page (session-views-rebuild plan §3.3): what the view is, in
// plain rows, and what it holds now. Nothing here is an input.
import { el, api } from '../core.js';
import { organizedRailUrl } from './view-group-source.js';
import { confirmDeleteText } from './view-actions.js';
import { shownAs, sortLabel, groupLabel, isMemoryView, sessionsNow } from './settings-view-model.js';
import { boardFacts, loadPlacementReads } from './settings-view-board.js';
import { button, linkButton, chip, card, cardAction, keyValue, problem, row, spacer, field, textInput } from '../orchestration/agents/agent-ui.js';
import { openDialog, confirmDialog } from '../orchestration/agents/agent-dialog.js';

// A list's card shows its largest groups; a board is read whole, because the
// groups a board does not declare can push a declared column past a short cut.
const LIST_GROUPS = 12;
const BOARD_GROUPS = 500;

export function renderDetail(page, body, view) {
  const memory = isMemoryView(view);
  body.appendChild(linkButton('‹ Session views', () => page.navigate({ screen: 'index' }, 'view-' + view.id)));
  body.appendChild(header(page, view, memory));
  body.appendChild(el('div', 'agents-sub', memory ? 'Lists memory records' : shownAs(view)));
  const what = el('div');
  what.appendChild(whatCard(page, view, memory));
  const cols = el('div', 'views-cols');
  cols.appendChild(what);
  if (!memory) cols.appendChild(nowCard(page, view));
  body.appendChild(cols);
  // What each placement rule holds now arrives after the page: the card is
  // drawn again with the counts, never held back for them.
  if (view.board && (view.board.placement || []).length) {
    loadPlacementReads([view]).then(() => { if (what.isConnected) what.replaceChildren(whatCard(page, view, memory)); }).catch(() => {});
  }
}

function header(page, view, memory) {
  const blocked = page.blocked();
  const open = button('Open in Sessions', '', () => page.openInSessions(view.id));
  const edit = button('Edit', '', () => page.navigate({ screen: 'edit', id: view.id }, 'name'));
  const copy = button('Duplicate', 'ghost', () => duplicateDialog(page, view));
  const del = button('Delete', 'ghost danger', () => confirmDialog('Delete view', confirmDeleteText(view.name), 'Delete view', () => page.remove(view), true));
  for (const node of [edit, copy, del]) node.disabled = blocked;
  const kind = memory ? chip('Memory') : chip(view.board ? 'Board' : 'List', view.board ? 'views-chip-board' : '');
  const head = row(el('h2', 'agents-title', view.name || '(unnamed)'), kind, spacer(), open, edit, copy, del);
  return head;
}

function duplicateDialog(page, view) {
  const name = textInput((view.name || 'View') + ' copy', 'Name for the copy');
  name.maxLength = 120;
  name.setAttribute('autofocus', '');
  const dialog = openDialog({ title: 'Duplicate view', body: field('Name', name), actions: [
    { label: 'Cancel', onClick: current => current.close() },
    { label: 'Create copy', primary: true, onClick: current => createCopy(page, view, name.value.trim(), current) },
  ] });
  name.addEventListener('keydown', event => { if (event.key === 'Enter') createCopy(page, view, name.value.trim(), dialog); });
}

// A refused copy is said in the dialog, in the daemon's words.
async function createCopy(page, view, name, dialog) {
  try {
    await page.duplicate(view, name);
    dialog.close();
  } catch (error) {
    dialog.showProblem(String(error?.message || error));
  }
}

function whatCard(page, view, memory) {
  const pairs = [['Filter', el('code', 'views-filter-text', view.query || '')]];
  if (memory) pairs.push(['Lists', 'memory records'], ['Sorted', sortLabel(view.sort)]);
  else if (view.board) pairs.push(['Shown as', 'Board · sorted ' + sortLabel(view.sort)], ...boardFacts(view));
  else pairs.push(['Shown as', 'List'], ['Grouped by', groupLabel(view.group_by)], ['Sorted', sortLabel(view.sort)]);
  const node = card('What it shows', keyValue(pairs));
  if (!page.blocked()) cardAction(node, 'Edit', () => page.navigate({ screen: 'edit', id: view.id }, 'name'));
  return node;
}

// The "Sessions now" card: the total and its groups, from one read of the
// saved view. A number the daemon did not send is not shown, and no reason is
// given for its absence: the page is not told one.
function nowCard(page, view) {
  const content = el('div', 'views-now', 'Reading…');
  const node = cardAction(card('Sessions now', content), 'Open in Sessions', () => page.openInSessions(view.id));
  const active = { query: view.query || '', groupBy: view.group_by || '', sort: view.sort || '' };
  api(organizedRailUrl(active, view.board ? BOARD_GROUPS : LIST_GROUPS, view.board ? view.id : ''))
    .then(read => { if (content.isConnected) fillNow(content, sessionsNow(view, read)); })
    .catch(error => { if (content.isConnected) content.replaceChildren(problem(String(error?.message || error))); });
  return node;
}

function fillNow(content, now) {
  const total = now.total === null ? el('div', 'agents-sub', 'Not counted') : el('div', 'views-now-total', String(now.total));
  const top = Math.max(1, ...now.lines.map(line => line.count || 0));
  const lines = el('div', 'views-now-lines');
  for (const line of now.lines) {
    const label = el('div', 'views-now-label', line.label);
    const bar = el('i');
    bar.style.width = Math.round((line.count || 0) / top * 100) + '%';
    label.appendChild(bar);
    lines.append(label, el('span', 'views-num', line.count === null ? '' : String(line.count)));
  }
  content.replaceChildren(total, lines);
  if (now.more) content.appendChild(el('div', 'agents-sub', now.more + (now.more === 1 ? ' more group' : ' more groups')));
}
