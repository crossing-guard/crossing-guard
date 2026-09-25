// The Files pane (console-files-pane-plan §4): browse the session's checkout,
// read a file with line numbers, open the editor at a line, jump to the Diff
// pane for a changed file. Read-only; git is the inventory; every listing and
// read comes from the daemon's workspace-files routes.
import { api, el } from '../core.js';
import { activatePane, closePaneByID, paneContextKey } from '../pane-host.js';
import { sessionQuery } from './session-evidence.js';
import {
  emptyTreeState, fileProblemState, fileState, filterRows, listingState, nextFocus, rememberListing, toggleDir, truncationCopy, visibleRows,
} from './workspace-files-tree.js';

const ICON = {
  files: '<svg viewBox="0 0 24 24"><path d="M4 5h6l2 2h8v12H4z"/></svg>',
  search: '<svg viewBox="0 0 24 24"><circle cx="11" cy="11" r="6"/><path d="M20 20l-4.5-4.5"/></svg>',
  editor: '<svg viewBox="0 0 24 24"><path d="M14 4h6v6M20 4l-9 9M18 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5"/></svg>',
  diff: '<svg viewBox="0 0 24 24"><path d="M12 3v18M5 9l7-6 7 6M5 15l7 6 7-6"/></svg>',
  refresh: '<svg viewBox="0 0 24 24"><path d="M20 12a8 8 0 1 1-2.3-5.7M20 4v5h-5"/></svg>',
  close: '<svg viewBox="0 0 24 24"><path d="M6 6l12 12M18 6L6 18"/></svg>',
};

const contexts = new Map();
function stateFor(ctx) {
  const key = paneContextKey(ctx);
  if (!contexts.has(key)) contexts.set(key, { tree: emptyTreeState(), checkout: null, problem: null, file: null, fileProblem: null, query: '', generation: 0, inflight: new Map(), focusPath: '' });
  return contexts.get(key);
}
document.addEventListener('cg:panel-selection-reset', () => contexts.clear());

const fileName = path => String(path || '').split('/').pop();
const fileDir = path => String(path || '').split('/').slice(0, -1).join('/');

function problemFromError(error) {
  if (error?.status === 404) return { code: 'route-missing' };
  if (!error?.status) return { code: 'unreachable', message: error?.message || '' };
  try { const parsed = JSON.parse(error.message); if (parsed?.error?.code) return { code: parsed.error.code, message: parsed.error.message || '' }; } catch { /* plain text */ }
  return { code: 'git-failed', message: error.message };
}

// ---- data ------------------------------------------------------------------------

// listDir lists one directory; a listing already in flight for the same
// directory is shared, so a toggle-close-toggle-open never races two answers.
function listDir(pane, dir) {
  const { ctx, state } = pane;
  if (state.inflight.has(dir)) return state.inflight.get(dir);
  const request = api('/api/workspace-files?' + sessionQuery(ctx, { dir })).then(response => {
    if (dir === '') { state.checkout = response?.checkout || state.checkout; state.problem = response?.problem || null; }
    if (response?.problem) { if (dir !== '') state.tree.states.set(dir, { state: 'failed', message: response.problem.message }); return; }
    rememberListing(state.tree, response?.listing || { dir });
  }).catch(error => {
    if (dir === '') state.problem = problemFromError(error);
    else state.tree.states.set(dir, { state: 'failed', message: problemFromError(error).message });
  }).finally(() => state.inflight.delete(dir));
  state.inflight.set(dir, request);
  return request;
}

async function readFile(pane, path) {
  const { ctx, state } = pane;
  const generation = ++state.generation;
  state.tree.selected = path; state.file = null; state.fileProblem = null;
  pane.paint();
  try {
    const response = await api('/api/workspace-files/read?' + sessionQuery(ctx, { path }));
    if (generation !== state.generation) return;
    state.file = response?.file || null; state.fileProblem = response?.problem || null;
  } catch (error) {
    if (generation !== state.generation) return;
    state.fileProblem = problemFromError(error);
  }
  pane.paint();
}

// loadChanged reads the working scope once so changed files carry a mark and a
// "Show diff" control; the Diff pane owns the scope, this pane only consumes it.
async function loadChanged(pane) {
  try {
    const response = await api('/api/workspace-diff?' + sessionQuery(pane.ctx, { scope: 'working' }));
    pane.state.tree.changed = new Set((response?.files || []).map(file => file.path));
  } catch { pane.state.tree.changed = new Set(); }
}

async function refresh(pane) {
  const { state } = pane;
  state.problem = null;
  await Promise.all([listDir(pane, ''), loadChanged(pane)]);
  await Promise.all([...state.tree.expanded].map(dir => listDir(pane, dir)));
  if (state.tree.selected) await readFile(pane, state.tree.selected);
  pane.paint();
}

// ---- painting --------------------------------------------------------------------

function iconButton(svg, title, fn) {
  const button = el('button', 'diff-ib'); button.type = 'button'; button.title = title; button.setAttribute('aria-label', title); button.innerHTML = svg;
  button.addEventListener('click', event => { event.stopPropagation(); fn(); });
  return button;
}

function openInEditor(pane, path, line) {
  const root = pane.state.checkout?.root || pane.ctx.selection?.cwd || '';
  if (root) document.dispatchEvent(new CustomEvent('cg:open-editor', { detail: { root, path, line } }));
}

// buildToolbar runs once per controller so the filter box is never rebuilt
// under the reader's caret; paintToolbar only refreshes the crumb.
function buildToolbar(pane) {
  const { toolbar } = pane.parts; const { state } = pane;
  const crumb = el('div', 'diff-crumb');
  toolbar.append(crumb, el('span', 'diff-toolbar-space'));
  const search = document.createElement('input');
  search.type = 'search'; search.className = 'files-search'; search.placeholder = 'Filter loaded files'; search.value = state.query;
  search.setAttribute('aria-label', 'Filter loaded files');
  search.addEventListener('input', () => { state.query = search.value; paintTree(pane); });
  toolbar.appendChild(search);
  toolbar.appendChild(iconButton(ICON.refresh, 'Refresh', () => void refresh(pane)));
  toolbar.appendChild(iconButton(ICON.editor, 'Open folder in editor', () => openInEditor(pane, '.', 0)));
  toolbar.appendChild(iconButton(ICON.close, 'Close pane', () => closePaneByID('workspace.files')));
  pane.parts.crumb = crumb;
}

function paintToolbar(pane) {
  const { crumb } = pane.parts; const { state } = pane;
  crumb.replaceChildren();
  const root = state.checkout?.root || '';
  crumb.appendChild(el('span', 'diff-crumb-base', root ? fileName(root.replace(/\/$/, '')) : 'checkout'));
  if (state.checkout?.branch) crumb.append(el('span', 'diff-crumb-arrow', '·'), el('span', 'diff-crumb-base', state.checkout.branch));
}

// activateRow opens or closes a directory, or reads a file, and remembers the
// row so the repaint hands focus back to it.
function activateRow(pane, entry) {
  pane.state.focusPath = entry.path;
  if (entry.kind !== 'dir') { void readFile(pane, entry.path); return; }
  const open = toggleDir(pane.state.tree, entry.path);
  if (open && !pane.state.tree.children.has(entry.path)) void listDir(pane, entry.path).then(() => pane.paint());
  pane.paint();
}

function onRowKey(pane, rows, index, event) {
  if (event.key === 'Enter' || event.key === ' ') { event.preventDefault(); activateRow(pane, rows[index].entry); return; }
  const move = nextFocus(rows, index, event.key);
  if (!move) return;
  event.preventDefault();
  if (move.toggle) { activateRow(pane, rows[index].entry); return; }
  const target = rows[move.index]?.entry.path;
  if (!target) return;
  pane.state.focusPath = target;
  pane.parts.tree.querySelector(`.files-row[data-path="${CSS.escape(target)}"]`)?.focus();
}

function rowElement(pane, rows, index) {
  const row = rows[index]; const { entry } = row; const { state } = pane;
  const selected = state.tree.selected === entry.path;
  const node = el('div', `files-row files-${entry.kind}${row.changed ? ' changed' : ''}${entry.untracked ? ' untracked' : ''}${selected ? ' on' : ''}`);
  node.style.setProperty('--depth', String(row.depth));
  node.dataset.path = entry.path; node.setAttribute('role', 'treeitem');
  // Roving tabindex: one row is the tree's tab stop; arrows move within it.
  const focusPath = state.focusPath || state.tree.selected || rows[0]?.entry.path;
  node.tabIndex = entry.path === focusPath ? 0 : -1;
  node.setAttribute('aria-level', String(row.depth + 1));
  node.setAttribute('aria-selected', String(selected));
  if (entry.kind === 'dir') node.setAttribute('aria-expanded', String(row.expanded));
  const glyph = entry.kind === 'dir' ? (row.expanded ? '▾' : '▸') : { symlink: '↪', submodule: '⧉', missing: '∅', 'nested-repository': '⧉', special: '?' }[entry.kind] || '·';
  node.append(el('span', 'files-glyph', glyph), el('span', 'files-name', entry.name));
  if (row.changed) node.appendChild(el('span', 'files-mark', 'M'));
  else if (entry.untracked && entry.kind !== 'dir') node.appendChild(el('span', 'files-mark', 'U'));
  node.addEventListener('click', () => activateRow(pane, entry));
  node.addEventListener('focus', () => { state.focusPath = entry.path; });
  node.addEventListener('keydown', event => onRowKey(pane, rows, index, event));
  return node;
}

function paintTree(pane) {
  const { tree } = pane.parts; const { state } = pane;
  const hadFocus = tree.contains(document.activeElement);
  tree.replaceChildren();
  const rows = filterRows(visibleRows(state.tree), state.query);
  rows.forEach((row, index) => {
    tree.appendChild(rowElement(pane, rows, index));
    if (row.entry.kind !== 'dir' || !row.expanded) return;
    const meta = state.tree.states.get(row.entry.path);
    const note = meta?.state === 'failed' ? meta.message : (listingState(meta) || truncationCopy(meta));
    if (note && !(state.tree.children.get(row.entry.path) || []).length) { const line = el('div', 'files-note', note); line.style.setProperty('--depth', String(row.depth + 1)); tree.appendChild(line); }
    else if (truncationCopy(meta)) { const line = el('div', 'files-note', truncationCopy(meta)); line.style.setProperty('--depth', String(row.depth + 1)); tree.appendChild(line); }
  });
  const rootMeta = state.tree.states.get('');
  if (!rows.length) tree.appendChild(el('div', 'files-note', state.query ? 'No loaded file matches' : (listingState(rootMeta) || 'Loading…')));
  else if (truncationCopy(rootMeta)) tree.appendChild(el('div', 'files-note', truncationCopy(rootMeta)));
  // A repaint replaces the row that had focus; hand focus back to its successor.
  if (hadFocus && state.focusPath) tree.querySelector(`.files-row[data-path="${CSS.escape(state.focusPath)}"]`)?.focus();
}

function paintSourceHeader(pane, path) {
  const { state } = pane;
  const header = el('div', 'files-file-header');
  header.append(el('span', 'diff-name', fileName(path)), el('span', 'diff-dir', fileDir(path)));
  const status = fileState(state.file);
  if (status) header.appendChild(el('span', 'files-file-state', status));
  header.appendChild(el('span', 'diff-toolbar-space'));
  if (state.tree.changed.has(path)) header.appendChild(iconButton(ICON.diff, 'Show diff', () => activatePane('workspace.diff', { scope: 'working', path })));
  header.appendChild(iconButton(ICON.editor, 'Open in editor', () => openInEditor(pane, path, 1)));
  return header;
}

function paintSourceLines(pane, path, text) {
  const table = el('table', 'files-lines');
  const body = document.createElement('tbody');
  const lines = text.split('\n');
  if (lines.length && lines[lines.length - 1] === '') lines.pop();
  lines.forEach((line, index) => {
    const row = document.createElement('tr');
    const number = el('td', 'files-ln');
    const button = el('button', 'diff-open', String(index + 1)); button.type = 'button'; button.title = `Open ${path}:${index + 1} in the editor`;
    button.addEventListener('click', () => openInEditor(pane, path, index + 1));
    number.appendChild(button);
    row.append(number, el('td', 'files-code', line));
    body.appendChild(row);
  });
  table.appendChild(body);
  return table;
}

function paintSource(pane) {
  const { source } = pane.parts; const { state } = pane;
  source.replaceChildren();
  const path = state.tree.selected;
  if (!path) { source.appendChild(el('div', 'diff-state-detail', 'Select a file to read it.')); return; }
  source.appendChild(paintSourceHeader(pane, path));
  const problem = fileProblemState(state.fileProblem);
  if (problem) { source.append(el('div', 'diff-state-title', problem.title), el('div', 'diff-state-detail', problem.detail)); return; }
  if (!state.file) { source.appendChild(el('div', 'diff-state-detail', 'Loading…')); return; }
  if (state.file.kind !== 'text') { source.appendChild(el('div', 'diff-state-detail', fileState(state.file))); return; }
  source.appendChild(paintSourceLines(pane, path, state.file.text || ''));
  if (state.file.truncated) source.appendChild(el('div', 'diff-footer', `${fileState(state.file)} · open in the editor for the rest`));
}

function paintPane(pane) {
  if (!pane.alive()) return;
  paintToolbar(pane);
  const problem = fileProblemState(pane.state.problem);
  if (problem) { pane.parts.tree.replaceChildren(el('div', 'diff-state-title', problem.title), el('div', 'diff-state-detail', problem.detail)); pane.parts.source.replaceChildren(); return; }
  paintTree(pane);
  paintSource(pane);
}

function createController(ctx, host) {
  host.classList.add('files');
  const parts = { toolbar: el('div', 'diff-toolbar'), cols: el('div', 'files-cols'), tree: el('div', 'files-tree'), source: el('div', 'files-source') };
  parts.tree.setAttribute('role', 'tree');
  parts.cols.append(parts.tree, parts.source); host.append(parts.toolbar, parts.cols);
  const pane = { ctx, host, parts, state: stateFor(ctx), disposed: false };
  pane.alive = () => !pane.disposed && host.isConnected;
  pane.paint = () => paintPane(pane);
  buildToolbar(pane);
  const onTurnState = event => {
    const detail = event.detail || {};
    if (detail.id && detail.id !== (ctx.selection?.thread_id || ctx.selection?.id)) return;
    if (detail.execution !== 'running') void refresh(pane);
  };
  document.addEventListener('cg:session-turn-state', onTurnState);
  pane.paint();
  if (!pane.state.tree.children.has('')) void refresh(pane); else void loadChanged(pane).then(() => pane.paint());
  return {
    activate: detail => { if (detail?.path) void readFile(pane, detail.path); },
    resume: () => {}, suspend: () => {},
    dispose: () => { pane.disposed = true; document.removeEventListener('cg:session-turn-state', onTurnState); host.replaceChildren(); },
  };
}

export const workspaceFilesDescriptor = {
  id: 'workspace.files',
  title: 'Files',
  icon: ICON.files,
  capability: 'workspace-read',
  catalogOrder: -19,
  catalogMatch: ctx => ctx?.surface === 'session',
  match: ctx => ctx?.surface === 'session' && !!(ctx.selection?.id || ctx.selection?.thread_id),
  unavailable: () => 'Select a session to browse its checkout.',
  create: (ctx, host) => createController(ctx, host),
};
