// The owner's saved views, at the top of the rail. "Repositories" is always
// first and is the rail as it always was. Every other line is a view he saved;
// a fresh installation has none. The number beside a view is how many sessions
// are in it, in every repository — and there is no number where one would not
// be true (a view on live status, or on search words).
import { el } from '../core.js';
import { deleteView, loadViewCounts, loadViews, reorderViews, updateView } from './organization-api.js';
import { organization, recallSelectedView, selectView } from './organization-state.js';
import { moveOrder, confirmDeleteText, renameSettle } from './view-actions.js';
import { sessionActivityStore } from '../session/session-activity-store.js';

let settings = { viewsVisible: 12, countRefreshMs: 4000 };
let onSelect = () => {};
let onEdit = () => {};
let showAll = false;
let recalled = false;
let countTimer = null;
let lastCountAt = 0;
let openMenu = null;
let problem = '';
let ready = null;

export function configureViewList(options) {
  for (const [name, value] of Object.entries(options.settings || {})) {
    if (Number.isInteger(value) && value > 0) settings[name] = value;
  }
  if (options.onSelect) onSelect = options.onSelect;
  if (options.onEdit) onEdit = options.onEdit;
}

// ensureViewsReady loads the views once and reselects the view last used in
// this browser. The rail awaits it before its first draw, so a remembered view
// is what is drawn first — not Repositories followed by a second render.
export function ensureViewsReady() {
  if (!ready) {
    ready = loadViews().catch(() => {}).then(() => {
      const remembered = recallSelectedView();
      if (!recalled && remembered && organization.views.some(view => view.id === remembered)) selectView(remembered);
      recalled = true;
    });
  }
  return ready;
}

// mountViewList returns the list's host. The views are re-read on every rail
// draw, because the owner may have edited his file by hand.
export function mountViewList() {
  const host = el('div', 'viewlist');
  host.setAttribute('role', 'navigation');
  host.setAttribute('aria-label', 'Saved views');
  paintViewList(host);
  loadViews().then(() => {
    if (host.isConnected) paintViewList(host);
    refreshViewCounts();
  }).catch(() => {});
  return host;
}

export function paintViewList(host) {
  host.replaceChildren(viewButton(host, { id: '', name: 'Repositories' }));
  const views = organization.views;
  const visible = showAll ? views : views.slice(0, settings.viewsVisible);
  for (const view of visible) host.appendChild(viewButton(host, view));
  if (views.length > visible.length) {
    const more = el('button', 'view view-more', 'Show all (' + views.length + ')');
    more.type = 'button';
    more.onclick = () => { showAll = true; paintViewList(host); };
    host.appendChild(more);
  }
  for (const rejected of organization.rejected) host.appendChild(rejectedLine(rejected));
  if (problem) {
    const line = el('div', 'view-problem viewlist-problem', problem);
    line.setAttribute('role', 'alert');
    host.appendChild(line);
  }
  if (!views.length && !organization.rejected.length) {
    host.appendChild(el('div', 'empty viewlist-empty', 'No views yet. Type a filter above, then Save view.'));
  }
}

function rejectedLine(rejected) {
  const line = el('div', 'view view-rejected');
  line.appendChild(el('span', 'vn', rejected.name || 'session-views.json'));
  line.appendChild(el('span', 'view-problem', rejected.problem));
  return line;
}

function viewButton(host, view) {
  const button = el('button', 'view');
  button.type = 'button';
  button.dataset.viewId = view.id;
  button.appendChild(el('span', 'vn', view.name));
  const count = el('span', 'vc');
  button.appendChild(count);
  if (view.id === organization.viewID && !organization.barQuery) {
    button.classList.add('sel');
    button.setAttribute('aria-current', 'true');
  }
  button.onclick = () => { selectView(view.id); onSelect(); };
  if (view.id) button.oncontextmenu = event => { event.preventDefault(); openViewMenu(host, button, view); };
  return button;
}

// refreshViewCounts patches the numbers in place. It never redraws the rail.
export async function refreshViewCounts() {
  if (!organization.views.length) return;
  lastCountAt = Date.now();
  let counts = {}, notes = {};
  try {
    const read = await loadViewCounts();
    counts = read.view_counts || {};
    notes = read.view_notes || {};
  } catch { return; }
  document.querySelectorAll('.viewlist .view[data-view-id]').forEach(button => {
    const id = button.dataset.viewId;
    const count = button.querySelector('.vc');
    if (!id || !count) return;
    const known = Object.prototype.hasOwnProperty.call(counts, id);
    count.textContent = known ? String(counts[id]) : '';
    count.classList.toggle('hot', known && counts[id] > 0);
    const name = button.querySelector('.vn')?.textContent || '';
    const label = known ? name + ', ' + counts[id] + (counts[id] === 1 ? ' session' : ' sessions') : name;
    const problems = paintViewNote(button, notes[id]);
    button.setAttribute('aria-label', problems ? label + '. ' + problems : label);
  });
}

// paintViewNote shows a view's query notes as one visible short line under
// its name (the rejected-entry precedent); the full sentences go in the
// entry's accessible name, which this returns. A view with no notes loses
// any stale line.
function paintViewNote(button, notes) {
  button.querySelector('.view-problem')?.remove();
  const noted = Array.isArray(notes) && notes.length > 0;
  button.classList.toggle('view-noted', noted);
  if (!noted) return '';
  button.appendChild(el('span', 'view-problem', notes.map(note => note.term + ' matches no tag — try ' + note.suggest).join('; ')));
  return notes.map(note => note.problem).join(' ');
}

// Session activity changes often; counts follow it no faster than configured.
function scheduleCountRefresh() {
  if (countTimer || !document.querySelector('.viewlist')) return;
  const wait = Math.max(0, settings.countRefreshMs - (Date.now() - lastCountAt));
  countTimer = setTimeout(() => { countTimer = null; refreshViewCounts(); }, wait);
}

sessionActivityStore.subscribe(scheduleCountRefresh);
window.addEventListener('focus', scheduleCountRefresh);

// Another surface (Settings › Views, or this one after a rail-side write)
// adopted a new views document; repaint every connected list from the adopted
// state. The pinned Sessions pane is NOT re-rendered on navigation, so this is
// the only way a rename/reorder/delete made in Settings shows here. Never
// loadViews() inside the handler: writes already adopt, and a load would
// self-trigger this listener.
if (typeof document !== 'undefined') {
  document.addEventListener('cg:views-changed', () => {
    document.querySelectorAll('.viewlist').forEach(host => paintViewList(host));
    refreshViewCounts();
  });
}

function openViewMenu(host, anchor, view) {
  closeViewMenu();
  const menu = el('div', 'pane-menu view-menu');
  menu.setAttribute('role', 'menu');
  const item = (label, act) => {
    const button = el('button', 'pane-menu-item', label);
    button.type = 'button';
    button.setAttribute('role', 'menuitem');
    button.onclick = async () => { closeViewMenu(); await act(); if (host.isConnected) paintViewList(host); refreshViewCounts(); };
    menu.appendChild(button);
  };
  item('Rename', () => renameView(anchor, view));
  item('Edit filter', () => onEdit(view));
  for (const [sort, label] of [['longest', 'Sort: longest here first'], ['newest', 'Sort: newest first'], ['oldest', 'Sort: oldest first']]) {
    if ((view.sort || 'newest') !== sort) item(label, () => updateView({ ...view, sort }).then(onSelect));
  }
  item('Move up', () => moveView(view, -1));
  item('Move down', () => moveView(view, 1));
  item('Delete', () => removeView(view));
  document.body.appendChild(menu);
  const rect = anchor.getBoundingClientRect();
  menu.style.top = rect.bottom + 4 + 'px';
  menu.style.left = rect.left + 12 + 'px';
  menu.style.right = 'auto';
  const outside = event => { if (!menu.contains(event.target)) closeViewMenu(); };
  document.addEventListener('pointerdown', outside, true);
  menu.addEventListener('keydown', event => {
    if (event.key !== 'Escape') return;
    event.preventDefault();
    event.stopPropagation();
    closeViewMenu();
    anchor.focus();
  });
  openMenu = { menu, outside };
  menu.querySelector('button')?.focus();
}

function closeViewMenu() {
  if (!openMenu) return;
  document.removeEventListener('pointerdown', openMenu.outside, true);
  openMenu.menu.remove();
  openMenu = null;
}

// renameView edits the name where it stands: Enter saves, Escape or leaving
// the field keeps the old name. The commit rule lives in view-actions.
function renameView(button, view) {
  return new Promise(resolve => {
    const input = el('input', 'view-rename');
    input.value = view.name;
    input.maxLength = 120;
    input.setAttribute('aria-label', 'Rename ' + view.name);
    let settled = false;
    const finish = async save => {
      if (settled) return;
      settled = true;
      const { commit, name } = renameSettle(view.name, save ? input.value : '');
      if (commit) await guarded(() => updateView({ ...view, name }));
      resolve();
    };
    input.addEventListener('keydown', event => {
      if (event.key === 'Enter') { event.preventDefault(); finish(true); }
      if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); finish(false); }
    });
    input.addEventListener('blur', () => finish(false));
    button.replaceChildren(input);
    input.focus();
    input.select();
  });
}

async function moveView(view, step) {
  const order = moveOrder(organization.views.map(each => each.id), view.id, step);
  if (!order) return;
  await guarded(() => reorderViews(order));
}

async function removeView(view) {
  if (!window.confirm(confirmDeleteText(view.name))) return;
  await guarded(() => deleteView(view.id));
  if (organization.viewID === view.id) { selectView(''); onSelect(); }
}

// A refused write (another browser or a hand edit got there first) reloads the
// views, so the owner sees what is actually saved and can try again.
async function guarded(write) {
  problem = '';
  try { await write(); } catch (err) {
    await loadViews().catch(() => {});
    problem = err.message || String(err);
  }
}
