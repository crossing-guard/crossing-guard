// The Session views index (session-views-rebuild plan §3.2): one row per
// saved view, no controls in rows. A row opens the view; its grip, or
// Alt+ArrowUp / Alt+ArrowDown, moves it in the rail's order.
import { el } from '../core.js';
import { organization } from './organization-state.js';
import { NEW_DRAFT, indexRow, draftChanged } from './settings-view-model.js';
import { reorderRow } from './settings-view-board.js';
import { button, linkButton, chip, problem, row, spacer } from '../orchestration/agents/agent-ui.js';

const COLUMNS = ['', 'View', 'Shows', 'Filter', 'Grouped by', 'Sessions now'];

export function renderIndex(page, body, notice) {
  const blocked = page.blocked();
  const add = button(page.drafts.has(NEW_DRAFT) ? 'Continue new view' : 'New view', 'primary', () => page.navigate({ screen: 'new' }));
  add.disabled = blocked;
  add.dataset.focus = 'new';
  body.appendChild(row(el('h2', 'agents-title', 'Session views'), spacer(), add));
  if (blocked) {
    body.appendChild(problem('session-views.json has an entry that cannot be read; fix or remove it before saving from the console. ' +
      'Saving stays off until then, so the console can never overwrite your half-made edit.'));
  }
  if (notice) body.appendChild(problem(notice));
  const orphans = [...page.drafts.keys()].filter(key => key !== NEW_DRAFT && !page.stored(key));
  if (!organization.views.length && !organization.rejected.length && !orphans.length) {
    body.appendChild(emptyState());
  } else {
    body.appendChild(table(page, orphans));
  }
  body.appendChild(footer());
}

function emptyState() {
  const node = el('div', 'views-empty');
  node.append(el('div', '', 'No views yet.'),
    el('div', 'agents-sub', 'Add one here, or type a filter above the session rail and press Save view there.'));
  return node;
}

function table(page, orphans) {
  const node = el('table', 'agents-table views-table');
  const head = el('tr');
  COLUMNS.forEach((label, index) => head.appendChild(el('th', index === COLUMNS.length - 1 ? 'views-num' : '', label)));
  const thead = el('thead');
  thead.appendChild(head);
  const tbody = el('tbody');
  organization.views.forEach((view, index) => tbody.appendChild(viewRow(page, view, index)));
  for (const key of orphans) tbody.appendChild(orphanRow(page, key));
  for (const rejected of organization.rejected) tbody.appendChild(rejectedRow(rejected));
  node.append(thead, tbody);
  const wrap = el('div', 'agents-table-wrap');
  wrap.appendChild(node);
  return wrap;
}

function cell(className, ...children) {
  const node = el('td', className);
  node.append(...children.filter(Boolean));
  return node;
}

// opens makes a row behave as a link: a click or Enter opens it, a click on
// its grip does not.
function opens(tr, label, open) {
  tr.tabIndex = 0;
  tr.setAttribute('role', 'link');
  tr.setAttribute('aria-label', label);
  tr.onclick = event => { if (!event.target.closest('.views-grip')) open(); };
  tr.addEventListener('keydown', event => { if (event.key === 'Enter' && event.target === tr) open(); });
}

function viewRow(page, view, index) {
  const draft = page.drafts.get(view.id);
  const facts = indexRow(view, page.counts, draft && draftChanged(draft));
  const tr = el('tr', 'agents-index-row');
  tr.dataset.focus = 'view-' + view.id;
  const grip = el('span', 'views-grip', '⠿');
  grip.setAttribute('aria-hidden', 'true');
  const name = cell('views-name', el('span', '', facts.name), facts.dirty ? chip('Unsaved edit', 'views-chip-warn') : null);
  const shows = cell('', chip(facts.kind, facts.kind === 'Board' ? 'views-chip-board' : ''),
    facts.kind === 'Board' ? el('span', 'agents-sub', ' ' + facts.columns + (facts.columns === 1 ? ' column' : ' columns')) : null);
  tr.append(cell('views-grip-cell', grip), name, shows, cell('views-filter', el('code', '', facts.query)),
    cell('', facts.groupedBy), cell('views-num', facts.count));
  opens(tr, 'Open ' + facts.name + '. Press Alt+Up or Alt+Down to move it.',
    () => page.navigate({ screen: facts.dirty ? 'edit' : 'view', id: view.id }));
  if (!page.blocked()) reorderRow(tr, grip, 'views', index, organization.views.length, 
    // A drop lands on the row dropped on, so the view that moves is named by
    // where the drag began, never by this row.
    (from, to) => page.reorder(organization.views[from].id, to - from));
  return tr;
}

// An unsaved edit whose view was deleted elsewhere: it has no place in the
// order, so it has no grip, and it opens to its edit.
function orphanRow(page, key) {
  const draft = page.drafts.get(key);
  const tr = el('tr', 'agents-index-row');
  tr.dataset.focus = 'view-' + key;
  const name = draft.view.name || '(unnamed)';
  const words = cell('', chip('Unsaved edit — the view was deleted', 'views-chip-warn'));
  words.colSpan = 4;
  tr.append(cell(''), cell('views-name', el('span', '', name)), words);
  opens(tr, 'Open the unsaved edit of ' + name, () => page.navigate({ screen: 'edit', id: key }));
  return tr;
}

function rejectedRow(rejected) {
  const tr = el('tr', 'views-row-bad');
  const words = cell('', rejected.problem);
  words.colSpan = 4;
  tr.append(cell(''), cell('views-name', el('span', '', rejected.name || 'session-views.json')), words);
  return tr;
}

// The file the views live in: its path, or where it will be written.
function footer() {
  const node = el('div', 'views-foot');
  const path = organization.origin;
  if (!path || path === 'none') {
    node.textContent = 'Views you save are written to session-views.json in the data directory.';
    return node;
  }
  const copy = linkButton('Copy path', () => {
    navigator.clipboard?.writeText(path).then(() => { copy.textContent = 'Copied'; }).catch(() => {});
  });
  const shown = el('code', 'views-path', path);
  shown.title = path;
  node.append('Stored in ', shown, copy);
  return node;
}
