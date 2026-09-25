// The Diff pane (workspace-panes design §3; implementation plan §4.3). One
// stacked column of files behind a toolbar: file tree, `base → scope ▾`
// breadcrumb, changed-files picker, ⋮ menu, open in editor, close.
//
// Scopes: "This session" reads the session's recorded edits from the governor
// (section=edits, bodies one at a time) and shows them unanchored (§3.3); the
// git scopes read the workspace routes and show line-anchored hunks. Both feed
// the one diff model. Presentation defaults come from the console configuration
// (`diff.*`) through configureWorkspaceDiff; nothing here compiles one in.
import { api, el } from '../core.js';
import { closePaneByID, paneContextKey } from '../pane-host.js';
import { sessionQuery } from './session-evidence.js';
import { renderDiff } from '../diff/diff-viewer.js';
import { parseUnifiedDiff } from '../diff/unified-diff.js';
import {
  checkoutLabel, dirtyLabel, filterFiles, normalizeDiffPrefs, problemState, renderPicker, renderTree,
  scopeById, scopeLabel, scopeMenuItems,
} from './workspace-diff-tree.js';
import { workspaceFilesDescriptor } from './workspace-files.js';

const PREFS_KEY = 'cg_diff_prefs';
const ICON = {
  diff: '<svg viewBox="0 0 24 24"><path d="M12 3v18M5 9l7-6 7 6M5 15l7 6 7-6"/></svg>',
  tree: '<svg viewBox="0 0 24 24"><path d="M4 6h16M4 12h16M4 18h16"/></svg>',
  list: '<svg viewBox="0 0 24 24"><path d="M9 6h11M9 12h11M9 18h11"/><circle cx="4.5" cy="6" r="1"/><circle cx="4.5" cy="12" r="1"/><circle cx="4.5" cy="18" r="1"/></svg>',
  more: '<svg viewBox="0 0 24 24"><circle cx="12" cy="5" r="1.2"/><circle cx="12" cy="12" r="1.2"/><circle cx="12" cy="19" r="1.2"/></svg>',
  editor: '<svg viewBox="0 0 24 24"><path d="M14 4h6v6M20 4l-9 9M18 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5"/></svg>',
  close: '<svg viewBox="0 0 24 24"><path d="M6 6l12 12M18 6L6 18"/></svg>',
  chevron: '<svg viewBox="0 0 24 24"><path d="M6 9l6 6 6-6"/></svg>',
};

let diffDefaults = {};
let keymap = {};
export function configureWorkspaceDiff(config) {
  diffDefaults = config?.diff || {};
  keymap = config?.keymap || {};
}

function readPrefs() {
  try { return normalizeDiffPrefs(JSON.parse(localStorage.getItem(PREFS_KEY) || '{}'), diffDefaults); }
  catch { return normalizeDiffPrefs({}, diffDefaults); }
}
function writePrefs(prefs) {
  try { localStorage.setItem(PREFS_KEY, JSON.stringify(prefs)); } catch { /* per-viewer convenience only */ }
}

// ---- per-selection state ------------------------------------------------------

const contexts = new Map();
function stateFor(ctx) {
  const key = paneContextKey(ctx);
  if (!contexts.has(key)) {
    contexts.set(key, { scope: 'session', base: '', collapsed: new Set(), model: null, checkout: null, problem: null,
      gitProblem: null, probed: false, loadedAt: 0, bodies: new Map(), generation: 0, open: null, query: '', pendingPath: '' });
  }
  return contexts.get(key);
}
document.addEventListener('cg:panel-selection-reset', () => contexts.clear());

const selectionRoot = ctx => ctx?.selection?.cwd || ctx?.selection?.project || '';
const shortTime = seconds => seconds ? new Date(seconds * 1000).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : '';

// ---- "This session": recorded edits, bodies fetched one at a time -----------------

function whyLine(row) {
  const parts = [row.tool || row.operation || 'edit'];
  parts.push({ replacement: 'replacement', content: 'write', diff: 'patch' }[row.kind] || row.kind);
  if (row.replace_all) parts.push('replace all');
  const when = shortTime(row.completed_at);
  if (when) parts.push(when);
  return parts.join(' · ');
}

function editFromRow(row) {
  const afterBytes = row.kind === 'content' ? row.content_bytes : row.kind === 'diff' ? row.diff_bytes : row.after_bytes;
  return { id: `${row.result_id}:${row.ordinal}`, kind: row.kind, state: 'pending', why: whyLine(row), replaceAll: row.replace_all === true,
    beforeBytes: row.kind === 'replacement' ? row.before_bytes : 0, afterBytes: afterBytes || 0, hunks: [],
    tool: row.tool || '', operation: row.operation || '', resultId: row.result_id, ordinal: row.ordinal };
}

function sessionModel(response) {
  const files = (response?.edits?.files || []).map(file => ({
    id: file.path, oldPath: file.display_path, newPath: file.display_path, displayPath: file.display_path || file.path,
    kind: 'text', hunks: [], edits: (file.edits || []).map(editFromRow),
  }));
  return { source: { kind: 'session', label: 'This session' }, files, page: response?.edits || {} };
}

async function fetchBody(ctx, state, edit, kind) {
  const key = `${edit.resultId}:${edit.ordinal}:${kind}`;
  if (state.bodies.has(key)) return state.bodies.get(key);
  const request = api('/api/govern/session?' + sessionQuery(ctx, { section: 'body', body_kind: kind, result_id: edit.resultId, ordinal: edit.ordinal }))
    .then(response => response?.body?.text ?? null).catch(() => null);
  state.bodies.set(key, request);
  return request;
}

async function fillReplacement(ctx, state, edit) {
  const before = edit.beforeBytes ? await fetchBody(ctx, state, edit, 'effect_before') : '';
  const after = edit.afterBytes ? await fetchBody(ctx, state, edit, 'effect_after') : '';
  if (before === null || after === null) { edit.state = 'unavailable'; return; }
  edit.before = before; edit.after = after; edit.state = 'loaded';
}

async function fillContent(ctx, state, edit) {
  const after = edit.afterBytes ? await fetchBody(ctx, state, edit, 'effect_content') : '';
  if (after === null) { edit.state = 'unavailable'; return; }
  edit.before = null; edit.after = after; edit.state = 'loaded';
}

async function fillPatch(ctx, state, file, edit) {
  const patch = edit.afterBytes ? await fetchBody(ctx, state, edit, 'effect_diff') : '';
  if (patch === null) { edit.state = 'unavailable'; return; }
  const parsed = parseUnifiedDiff(patch, {}, { path: file.displayPath });
  edit.hunks = parsed.files.flatMap(parsedFile => parsedFile.hunks);
  edit.before = null; edit.after = null; edit.state = 'loaded';
}

const fetchWidth = () => diffDefaults.fetch_concurrency > 0 ? diffDefaults.fetch_concurrency : 1;

function fillEdit(ctx, state, file, edit) {
  if (edit.kind === 'content') return fillContent(ctx, state, edit);
  if (edit.kind === 'diff') return fillPatch(ctx, state, file, edit);
  return fillReplacement(ctx, state, edit);
}

// pool runs the body fetches a few at a time; each completion repaints once per
// frame so a session with hundreds of edits fills in without thrashing.
async function pool(tasks, width, onProgress) {
  let next = 0;
  const worker = async () => {
    while (next < tasks.length) { const task = tasks[next++]; await task(); onProgress(); }
  };
  await Promise.all(Array.from({ length: Math.min(width, tasks.length) }, worker));
}

// loadSession reads one page of edits; with an offset the page is appended to
// the files already shown (the "Show more" control), else it replaces them.
async function loadSession(ctx, state, repaint, offset = 0) {
  const response = await api('/api/govern/session?' + sessionQuery(ctx, { section: 'edits', offset }));
  const page = sessionModel(response);
  if (offset && state.model?.source?.kind === 'session') {
    for (const file of page.files) {
      const existing = state.model.files.find(candidate => candidate.id === file.id);
      if (existing) existing.edits.push(...file.edits); else state.model.files.push(file);
    }
    state.model.page = page.page;
  } else {
    state.model = page;
  }
  state.problem = null; state.loadedAt = Date.now();
  repaint();
  const tasks = [];
  for (const file of page.files) for (const edit of file.edits) tasks.push(() => fillEdit(ctx, state, file, edit));
  await pool(tasks, fetchWidth(), repaint);
}

// ---- git scopes: the workspace routes ----------------------------------------------

function problemFromError(error) {
  if (error?.status === 404) return { code: 'route-missing' };
  if (!error?.status) return { code: 'unreachable', message: error?.message || '' };
  try {
    const parsed = JSON.parse(error.message);
    if (parsed?.problem) return parsed.problem;
    if (parsed?.error?.code) return { code: parsed.error.code, message: parsed.error.message || '' };
    if (parsed?.code) return parsed;
  } catch { /* plain text error */ }
  return { code: 'git-failed', message: error.message };
}

function gitFile(row) {
  const kindSummary = { binary: 'Binary file', mode: 'Mode change', typechange: 'File type changed', rename: `Renamed from ${row.old_path || '?'}`,
    conflict: 'Merge conflict; not rendered as a diff', submodule: 'Submodule pointer', special: 'Not a regular file' };
  return { id: row.path, oldPath: row.old_path || row.path, newPath: row.status === 'D' ? '/dev/null' : row.path, displayPath: row.path,
    kind: row.kind || 'text', status: row.status || '', added: row.added, removed: row.removed, freshness: row.freshness || '',
    summary: kindSummary[row.kind] || '', hunks: [], edits: [], patched: false };
}

async function fetchPatch(ctx, state, file) {
  if (file.kind !== 'text' || file.patched) return;
  file.patched = true;
  try {
    const response = await api('/api/workspace-diff/file?' + sessionQuery(ctx, { scope: requestScope(state.scope), base: state.base, path: file.displayPath }));
    if (response?.problem) { file.summary = problemState(response.problem)?.title || 'Patch unavailable'; return; }
    const patch = response?.file?.patch || '';
    const parsed = parseUnifiedDiff(patch, {}, { path: file.displayPath, newPath: file.newPath });
    for (const parsedFile of parsed.files) file.hunks.push(...parsedFile.hunks);
    if (response?.file?.truncated) file.truncated = true;
    if (response?.file?.freshness && file.freshness && response.file.freshness !== file.freshness) file.summary = 'This file changed after the list was read; Refresh to reload it.';
    if (!parsed.files.length && patch) file.summary = 'The patch could not be read as a diff.';
  } catch (error) {
    file.summary = problemState(problemFromError(error))?.title || 'Patch unavailable';
  }
}

// The pane's "compare" scope is the daemon's since-base scope with a chosen ref.
const requestScope = scope => scope === 'compare' ? 'since-base' : scope;

async function loadGit(ctx, state, repaint) {
  let response;
  try {
    response = await api('/api/workspace-diff?' + sessionQuery(ctx, { scope: requestScope(state.scope), base: state.base }));
  } catch (error) {
    state.problem = problemFromError(error);
    if (problemState(state.problem)?.disablesGit) state.gitProblem = state.problem;
    repaint();
    return;
  }
  state.problem = response?.problem || null;
  if (problemState(state.problem)?.disablesGit) state.gitProblem = state.problem;
  state.checkout = response?.checkout || state.checkout;
  state.loadedAt = Date.now();
  const files = (response?.files || []).map(gitFile);
  state.model = { source: { kind: 'live', label: scopeLabel(state.scope, state.base), observedAt: shortTime(response?.checkout?.observed_at) },
    files, truncated: !!response?.truncated, truncatedCounts: response?.truncated || null };
  repaint();
  await pool(files.map(file => () => fetchPatch(ctx, state, file)), fetchWidth(), repaint);
}

// probeCheckout resolves the session's checkout without running a diff, so the
// breadcrumb can name it and the scope menu can disable git scopes with a reason
// before the reader asks for one.
async function probeCheckout(ctx, state, repaint) {
  if (state.probed) return;
  state.probed = true;
  try {
    const response = await api('/api/workspace-diff/checkout?' + sessionQuery(ctx));
    state.checkout = response?.checkout || null;
    state.gitProblem = response?.problem || null;
  } catch (error) {
    state.gitProblem = problemFromError(error);
  }
  repaint();
}

async function loadRefs(ctx, state) {
  const response = await api('/api/workspace-diff/refs?' + sessionQuery(ctx));
  if (response?.problem) throw Object.assign(new Error(JSON.stringify({ problem: response.problem })), { status: 200 });
  const branches = (response?.branches || []).map(branch => ({ label: branch.name, detail: branch.current ? 'current branch' : 'branch', value: branch.name }));
  const commits = (response?.commits || []).map(commit => ({ label: commit.short || String(commit.sha || '').slice(0, 7), detail: commit.subject || '', value: commit.sha }));
  return [...branches, ...commits];
}

// ---- controller ------------------------------------------------------------------

function iconButton(svg, title, on, fn) {
  const button = el('button', 'diff-ib' + (on ? ' on' : ''));
  button.type = 'button'; button.title = title; button.setAttribute('aria-label', title); button.innerHTML = svg;
  if (on) button.setAttribute('aria-pressed', 'true');
  button.addEventListener('click', event => { event.stopPropagation(); fn(); });
  return button;
}

function menuItem(menu, label, { on = false, disabled = false, hint = '', shortcut = '' }, fn) {
  const item = el('div', 'diff-menu-item' + (on ? ' on' : '') + (disabled ? ' disabled' : ''));
  item.setAttribute('role', 'menuitem');
  item.appendChild(el('span', 'diff-menu-label', label));
  if (shortcut) item.appendChild(el('kbd', '', shortcut));
  if (hint) item.appendChild(el('span', 'diff-menu-hint', hint));
  if (!disabled) item.addEventListener('click', event => { event.stopPropagation(); fn(); });
  menu.appendChild(item);
}

const shortcutLabel = binding => String(binding || '').replace('Meta', '⌘').replace('Shift', '⇧').replace('Ctrl', '⌃').replace('Alt', '⌥').replaceAll('+', ' ');

function scrollToFile(view, path) {
  for (const section of view.querySelectorAll('.diff-file')) {
    if (section.dataset.path !== path) continue;
    section.querySelector('.diff-file-body')?.removeAttribute('hidden');
    section.scrollIntoView({ block: 'start' });
    return;
  }
}

// ---- the pane: one object, module-level painters ---------------------------------

const files = pane => pane.state.model?.files || [];
// The editor root follows the scope: session-scope paths are relative to the
// session's recorded folder, git-scope paths to the resolved checkout.
const rootOf = pane => pane.state.scope === 'session' ? selectionRoot(pane.ctx) : (pane.state.checkout?.root || '');

function openInEditor(pane, target) {
  const root = rootOf(pane);
  if (root) document.dispatchEvent(new CustomEvent('cg:open-editor', { detail: { ...target, root } }));
}

function load(pane, offset = 0) {
  const { ctx, state } = pane;
  const generation = ++state.generation;
  const guarded = () => { if (generation === state.generation) pane.repaint(); };
  const run = state.scope === 'session' ? loadSession(ctx, state, guarded, offset) : loadGit(ctx, state, guarded);
  run.catch(error => { if (generation !== state.generation) return; state.problem = problemFromError(error); guarded(); })
    // A jump that its load could not satisfy is forgotten, never fired later.
    .finally(() => { if (generation === state.generation) { pane.paint(); state.pendingPath = ''; } });
}

function setScope(pane, scope, base = '') {
  Object.assign(pane.state, { scope, base, open: null, model: null, problem: null });
  pane.paint(); load(pane);
}

function setPrefs(pane, patch) { pane.prefs = { ...pane.prefs, ...patch }; writePrefs(pane.prefs); pane.paint(); }
function openMenu(pane, name) { pane.state.open = pane.state.open === name ? null : name; pane.paint(); }
function closeMenus(pane) { if (pane.state.open) { pane.state.open = null; pane.paint(); } }

function paintCrumb(pane) {
  const { state, ctx } = pane;
  const crumb = el('div', 'diff-crumb');
  const baseLabel = state.scope === 'session' ? (ctx.selection?.runtime || 'session') : (checkoutLabel(state.checkout) || 'checkout');
  crumb.append(el('span', 'diff-crumb-base', baseLabel), el('span', 'diff-crumb-arrow', '→'));
  const head = el('button', 'diff-crumb-head'); head.type = 'button';
  head.append(el('span', '', scopeLabel(state.scope, state.base))); head.insertAdjacentHTML('beforeend', ICON.chevron);
  head.setAttribute('aria-haspopup', 'menu'); head.setAttribute('aria-expanded', String(state.open === 'scope'));
  head.addEventListener('click', event => { event.stopPropagation(); openMenu(pane, 'scope'); });
  crumb.appendChild(head);
  return crumb;
}

function paintToolbar(pane) {
  const { toolbar } = pane.parts; const { state, prefs } = pane;
  toolbar.replaceChildren();
  toolbar.appendChild(iconButton(ICON.tree, 'Show files', prefs.tree, () => setPrefs(pane, { tree: !prefs.tree })));
  toolbar.append(paintCrumb(pane), el('span', 'diff-toolbar-space'));
  toolbar.appendChild(iconButton(ICON.list, 'Search changed files', state.open === 'files', () => openMenu(pane, 'files')));
  toolbar.appendChild(iconButton(ICON.more, 'More', state.open === 'more', () => openMenu(pane, 'more')));
  toolbar.appendChild(iconButton(ICON.editor, 'Open in editor', false, () => {
    const first = files(pane)[0];
    if (first) openInEditor(pane, { path: first.displayPath, line: 1 });
  }));
  toolbar.appendChild(iconButton(ICON.close, 'Close pane', false, () => closePaneByID('workspace.diff')));
}

function paintScopeMenu(pane) {
  const { state } = pane;
  const menu = el('div', 'diff-menu'); menu.setAttribute('role', 'menu');
  const reason = problemState(state.gitProblem)?.title || '';
  for (const item of scopeMenuItems(state.scope, reason)) {
    const pick = () => { if (scopeById(item.id).picker) { state.open = 'refs'; pane.paint(); return; } setScope(pane, item.id); };
    menuItem(menu, item.label, { on: item.on, disabled: item.disabled, hint: item.reason }, pick);
  }
  if (state.gitProblem?.code === 'ambiguous-folder' && Array.isArray(state.gitProblem.roots)) {
    menu.appendChild(document.createElement('hr'));
    for (const candidate of state.gitProblem.roots) menuItem(menu, candidate, { disabled: true, hint: 'bind a workspace to choose' }, () => {});
  }
  pane.parts.overlay.appendChild(menu);
}

function paintMoreMenu(pane) {
  const { state, prefs, host } = pane;
  const menu = el('div', 'diff-menu diff-menu-wide'); menu.setAttribute('role', 'menu');
  const wideEnough = host.clientWidth >= (diffDefaults.side_by_side_min_width || Infinity);
  // Picking an item closes the menu, like the vendor menus the design follows.
  const toggle = key => () => { state.open = null; setPrefs(pane, { [key]: !prefs[key] }); };
  menuItem(menu, 'Show files', { on: prefs.tree, shortcut: shortcutLabel(keymap.show_files) }, toggle('tree'));
  menuItem(menu, 'Group files by folder', { on: prefs.group }, toggle('group'));
  menu.appendChild(document.createElement('hr'));
  menuItem(menu, 'Collapse all files', {}, () => { for (const file of files(pane)) state.collapsed.add(file.id); state.open = null; pane.paint(); });
  menuItem(menu, 'Expand all files', {}, () => { state.collapsed.clear(); state.open = null; pane.paint(); });
  menu.appendChild(document.createElement('hr'));
  menuItem(menu, 'Side by side', { on: prefs.side, disabled: !wideEnough && !prefs.side, hint: wideEnough || prefs.side ? '' : 'Needs a wider pane' }, toggle('side'));
  menuItem(menu, 'Word wrap', { on: prefs.wrap }, toggle('wrap'));
  menuItem(menu, 'Highlight changed words', { on: prefs.words }, toggle('words'));
  menuItem(menu, 'Hide whitespace changes', { on: prefs.whitespace }, toggle('whitespace'));
  menu.appendChild(document.createElement('hr'));
  menuItem(menu, 'Refresh', {}, () => { state.open = null; state.bodies.clear(); state.probed = false; void probeCheckout(pane.ctx, state, pane.repaint); load(pane); });
  pane.parts.overlay.appendChild(menu);
}

function paintFilePicker(pane) {
  const { state } = pane;
  renderPicker(pane.parts.overlay, { placeholder: 'Search changed files',
    rows: query => filterFiles(files(pane), query).map(file => ({ label: file.displayPath.split('/').pop(), detail: file.displayPath, value: file })),
    onPick: file => { state.open = null; state.collapsed.delete(file.id); pane.paint(); scrollToFile(pane.parts.view, file.displayPath); },
    onClose: () => closeMenus(pane) });
}

function paintRefPicker(pane) {
  let refs = null;
  const picker = renderPicker(pane.parts.overlay, { placeholder: 'Compare against a branch or commit',
    rows: query => refs === null ? [{ label: 'Loading refs…', detail: '', value: null }] : refs.filter(ref => (ref.label + ' ' + ref.detail).toLowerCase().includes(query.toLowerCase())),
    onPick: ref => { if (ref) setScope(pane, 'compare', ref); },
    onClose: () => closeMenus(pane) });
  loadRefs(pane.ctx, pane.state).then(list => { refs = list; })
    .catch(error => { refs = [{ label: problemState(problemFromError(error))?.title || 'Refs unavailable', detail: '', value: null }]; })
    .finally(() => { if (picker.isConnected) picker.querySelector('input').dispatchEvent(new Event('input')); });
}

function paintOverlay(pane) {
  const { overlay } = pane.parts; const { open } = pane.state;
  overlay.replaceChildren();
  overlay.hidden = !open;
  const painters = { scope: paintScopeMenu, more: paintMoreMenu, files: paintFilePicker, refs: paintRefPicker };
  if (painters[open]) painters[open](pane);
}

// shownEdits counts the edits already on the page across every file.
const shownEdits = pane => files(pane).reduce((total, file) => total + file.edits.length, 0);

function footerText(pane) {
  const { state } = pane; const list = files(pane);
  if (state.scope === 'session') {
    const page = state.model?.page || {};
    if (!list.length) return '';
    const text = `${list.length} file${list.length === 1 ? '' : 's'} · ${page.total ?? list.length} edit${page.total === 1 ? '' : 's'}`;
    return text + (page.total > shownEdits(pane) ? ` · ${shownEdits(pane)} shown` : '');
  }
  const parts = [checkoutLabel(state.checkout), dirtyLabel(state.checkout?.dirty)].filter(Boolean);
  if (state.model?.truncatedCounts) parts.push(`truncated · ${state.model.truncatedCounts.files || 0} files not shown`);
  return parts.join(' · ');
}

function paintView(pane) {
  const { state, prefs } = pane; const { view } = pane.parts;
  if (!state.model && !state.problem) { view.replaceChildren(el('div', 'diff-state-detail', 'Loading…')); return; }
  const problem = problemState(state.problem);
  if (problem) { view.replaceChildren(el('div', 'diff-state-title', problem.title), el('div', 'diff-state-detail', problem.detail)); return; }
  const scrollTop = view.scrollTop;
  const root = rootOf(pane);
  renderDiff(view, state.model, { showSource: false, collapsed: state.collapsed, openFile: root ? target => openInEditor(pane, target) : null,
    onToggle: file => { if (state.collapsed.has(file.id)) state.collapsed.delete(file.id); else state.collapsed.add(file.id); pane.paint(); },
    highlightWords: prefs.words, sideBySide: prefs.side, hideWhitespace: prefs.whitespace, wordWrap: prefs.wrap, showHunkHeaders: false });
  if (!files(pane).length) view.replaceChildren(el('div', 'diff-state-title', state.scope === 'session' ? 'No edits in this session' : 'No changes'));
  // A jump from another pane arrives before the scope's model does; scroll once it has.
  if (state.pendingPath && files(pane).some(file => file.displayPath === state.pendingPath)) {
    const target = state.pendingPath; state.pendingPath = '';
    requestAnimationFrame(() => scrollToFile(view, target));
  }
  const footer = footerText(pane);
  if (footer) view.appendChild(el('div', 'diff-footer', footer));
  const page = state.model?.page || {};
  if (state.scope === 'session' && page.total > shownEdits(pane)) {
    const more = el('button', 'btn diff-more', `Show ${Math.min(page.limit || page.total, page.total - shownEdits(pane))} more`);
    more.type = 'button';
    more.addEventListener('click', () => load(pane, shownEdits(pane)));
    view.appendChild(more);
  }
  view.scrollTop = scrollTop;
}

function paintPane(pane) {
  if (!pane.alive()) return;
  paintToolbar(pane);
  pane.host.classList.toggle('diff-nowrap', !pane.prefs.wrap);
  pane.parts.tree.hidden = !pane.prefs.tree;
  if (pane.prefs.tree) renderTree(pane.parts.tree, files(pane), pane.prefs.group, file => scrollToFile(pane.parts.view, file.displayPath));
  paintView(pane);
  paintOverlay(pane);
}

function installListeners(pane) {
  const onClick = event => { if (pane.state.open && !pane.parts.overlay.contains(event.target)) closeMenus(pane); };
  const onTurnState = event => {
    const detail = event.detail || {};
    if (detail.id && detail.id !== (pane.ctx.selection?.thread_id || pane.ctx.selection?.id)) return;
    if (detail.execution === 'running') return;
    load(pane);
  };
  const onShowFiles = () => { if (pane.alive()) setPrefs(pane, { tree: !pane.prefs.tree }); };
  document.addEventListener('click', onClick);
  document.addEventListener('cg:session-turn-state', onTurnState);
  document.addEventListener('cg:diff-show-files', onShowFiles);
  return () => {
    document.removeEventListener('click', onClick);
    document.removeEventListener('cg:session-turn-state', onTurnState);
    document.removeEventListener('cg:diff-show-files', onShowFiles);
  };
}

function createController(ctx, host) {
  host.classList.add('diff');
  const parts = { toolbar: el('div', 'diff-toolbar'), cols: el('div', 'diff-cols'), tree: el('div', 'diff-tree'), view: el('div', 'diff-view'), overlay: el('div', 'diff-overlay') };
  parts.cols.append(parts.tree, parts.view, parts.overlay); host.append(parts.toolbar, parts.cols);
  const pane = { ctx, host, parts, state: stateFor(ctx), prefs: readPrefs(), disposed: false, frame: 0 };
  pane.alive = () => !pane.disposed && host.isConnected;
  pane.paint = () => paintPane(pane);
  pane.repaint = () => {
    if (!pane.alive() || pane.frame) return;
    pane.frame = requestAnimationFrame(() => { pane.frame = 0; pane.paint(); });
  };
  const removeListeners = installListeners(pane);
  pane.paint();
  void probeCheckout(ctx, pane.state, pane.repaint);
  if (!pane.state.model) load(pane);
  return {
    activate: detail => {
      if (detail?.path) pane.state.collapsed.delete(detail.path);
      // A jump from another pane reloads the scope so the file it names is
      // current, then scrolls once the model has arrived.
      if (detail?.scope) {
        pane.state.pendingPath = detail?.path || '';
        if (detail.scope !== pane.state.scope) setScope(pane, detail.scope, detail.base || ''); else load(pane);
        return;
      }
      if (detail?.path) { pane.paint(); scrollToFile(parts.view, detail.path); }
    },
    resume: () => {
      const stale = (diffDefaults.stale_after_seconds > 0 ? diffDefaults.stale_after_seconds : 0) * 1000;
      if (stale && pane.state.loadedAt && Date.now() - pane.state.loadedAt > stale && pane.state.scope !== 'session') load(pane);
    },
    suspend: () => {},
    dispose: () => {
      pane.disposed = true;
      pane.state.pendingPath = '';
      if (pane.frame) cancelAnimationFrame(pane.frame);
      removeListeners();
      host.replaceChildren();
    },
  };
}

export const workspaceDiffDescriptor = {
  id: 'workspace.diff',
  title: 'Diff',
  icon: ICON.diff,
  capability: 'workspace-read',
  catalogOrder: -20,
  catalogMatch: ctx => ctx?.surface === 'session',
  match: ctx => ctx?.surface === 'session' && !!(ctx.selection?.id || ctx.selection?.thread_id),
  unavailable: () => 'Select a session to see its changes.',
  create: (ctx, host) => createController(ctx, host),
};

export const workspacePaneSource = {
  id: 'workspace',
  panes: () => [workspaceDiffDescriptor, workspaceFilesDescriptor],
  modules: () => [],
};
