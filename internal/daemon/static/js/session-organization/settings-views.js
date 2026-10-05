// The Settings › Session views page (session-views-rebuild plan): an index of
// the owner's saved views, a read page for each, and an edit mode with one
// Save. This module is the page's entry: it holds the route, the drafts, the
// load-failure state and every write. Each write goes through
// organization-api.js, and a save sends the whole view the draft holds
// (viewCommit), because the daemon's PUT replaces the stored view. The page
// never parses a filter: the daemon is the one authority, and a refusal is
// shown in its own words.
import { el } from '../core.js';
import { createView, deleteView, loadTagVocabulary, loadViewCounts, loadViews, reorderViews, updateView } from './organization-api.js';
import { organization, selectView } from './organization-state.js';
import { moveOrder, recoverySuffix, viewCommit } from './view-actions.js';
import { NEW_DRAFT, draftOf, commitOf, draftChanged, baseState, duplicateOf } from './settings-view-model.js';
import { button, problem } from '../orchestration/agents/agent-ui.js';
import { renderIndex } from './settings-view-index.js';
import { renderDetail } from './settings-view-detail.js';
import { renderEdit } from './settings-view-edit.js';

let host = null;                        // the page's root while it is connected
let live = null;                        // a polite live region: where a moved view went
let route = { screen: 'index', id: '' };
const drafts = new Map();               // view id (or NEW_DRAFT) -> an unsaved edit; outlives the mount
let counts = {};                        // view id -> sessions now, only where the daemon counts the view
let vocabulary = { tags: [], known: [] };
let notice = '';                        // a refused reorder, shown on the index
let saving = false;                     // a save's own adoption is not a change made elsewhere
let moving = false;                     // one reorder at a time: each answer changes the state token
let onAdopt = null;                     // the edit screen's check, while it is shown
let onVocabulary = null;                // the edit screen's group list, while it is shown
let shown = '';                         // the views a screen was last drawn from

const viewIds = () => organization.views.map(view => view.id);
const viewsText = () => JSON.stringify([organization.views, organization.rejected, organization.origin]);

const page = {
  drafts,
  get counts() { return counts; },
  get vocabulary() { return vocabulary; },
  blocked: () => organization.rejected.length > 0,
  stored: id => organization.views.find(view => view.id === id),
  draft(key) {
    if (!drafts.has(key)) drafts.set(key, draftOf(key === NEW_DRAFT ? undefined : page.stored(key)));
    return drafts.get(key);
  },
  watchAdopt(check) { onAdopt = check; },
  watchVocabulary(fill) { onVocabulary = fill; },
  navigate, save, reorder, remove, duplicate, openInSessions,
};

export async function renderViewsPage(main) {
  host = el('div', 'views-page');
  live = el('div', 'views-live');
  live.setAttribute('role', 'status');
  live.setAttribute('aria-live', 'polite');
  main.append(host, live);
  route = { screen: 'index', id: '' };
  notice = '';
  onAdopt = null;
  onVocabulary = null;
  host.appendChild(el('div', 'sub', 'Reading…'));
  const mine = host;
  try {
    await loadViews();
  } catch (error) {
    if (mine !== host) return;
    host.replaceChildren(problem('Views unavailable: ' + (error.message || error)),
      button('Retry', '', () => { host.remove(); live.remove(); renderViewsPage(main); }));
    return;
  }
  if (mine !== host) return;
  dropCleanDrafts();
  show('');
  refreshCounts();
  // The tag keys arrive after the views, so a slow vocabulary never holds the
  // page back; until then the group list offers the mechanical choices.
  loadTagVocabulary().then(found => {
    vocabulary = { tags: found.tags || [], known: found.known || [] };
    if (onVocabulary) onVocabulary();
  }).catch(() => {});
}

// An edit the owner opened and did not change leaves nothing behind.
function dropCleanDrafts() {
  for (const [key, draft] of drafts) {
    if (!draftChanged(draft)) drafts.delete(key);
  }
}

// show draws the screen the route names, and keeps focus on the row or
// field that had it.
function show(focus) {
  if (!host?.isConnected) return;
  const held = host.contains(document.activeElement) ? document.activeElement.closest('[data-focus]')?.dataset.focus : '';
  onAdopt = null;
  onVocabulary = null;
  shown = viewsText();
  const body = el('div', 'views-screen');
  const stored = page.stored(route.id);
  if (route.screen === 'new') renderEdit(page, body, NEW_DRAFT);
  else if (route.screen === 'edit' && (stored || drafts.has(route.id))) renderEdit(page, body, route.id);
  else if (route.screen === 'view' && stored) renderDetail(page, body, stored);
  else {
    route = { screen: 'index', id: '' };
    renderIndex(page, body, notice);
  }
  host.replaceChildren(body);
  const target = focus || held;
  if (target) body.querySelector('[data-focus="' + target + '"]')?.focus();
}

function navigate(next, focus = '') {
  dropCleanDrafts();
  route = { screen: next.screen, id: next.id || '' };
  notice = '';
  show(focus);
}

function refreshCounts() {
  loadViewCounts().then(read => {
    counts = read.view_counts || {};
    if (route.screen === 'index') show('');
  }).catch(() => {});
}

// guarded runs a write that happens at once (reorder, delete, duplicate) and
// answers the sentence to show when it was refused: the daemon's own words,
// and what survived. A conflict or a lost request reloads the views.
async function guarded(write) {
  try {
    await write();
    refreshCounts();
    return '';
  } catch (err) {
    const status = err?.status || 0;
    if (status === 409 || status === 0) await loadViews().catch(() => {});
    return (err.message || String(err)) + recoverySuffix(status);
  }
}

// save writes a draft. Before sending, the draft's base is compared with the
// stored view now: the state token cannot catch a view changed elsewhere,
// because this page's own re-reads refresh it. The answer says what happened:
// { ok, id }, { stale } (changed or gone elsewhere) or { message, status }.
async function save(key) {
  const draft = drafts.get(key);
  const before = new Set(viewIds());
  const refusal = { last: null };
  saving = true;
  try {
    for (let attempt = 0; attempt < 2; attempt++) {
      const stale = baseState(draft, page.stored(key));
      if (stale) return { stale };
      const result = await sendDraft(key, draft, before, refusal);
      if (result) return result;
    }
    // Refused twice with this view untouched: the daemon's sentence stands.
    const stale = baseState(draft, page.stored(key));
    return stale ? { stale } : refusal.last;
  } finally {
    saving = false;
  }
}

// sendDraft sends one save. It answers null when another writer moved the
// state token under it (409) and the views were read again: save then checks
// the draft's base and, if this view is untouched, sends once more.
async function sendDraft(key, draft, before, refusal) {
  try {
    if (draft.view.id) await updateView(viewCommit(commitOf(draft)));
    else await createView(viewCommit(commitOf(draft)));
    drafts.delete(key);
    refreshCounts();
    return { ok: true, id: draft.view.id || viewIds().find(id => !before.has(id)) || '' };
  } catch (err) {
    const status = err?.status || 0;
    const answer = { message: err.message || String(err), status };
    if (status !== 409 && status !== 404) return answer;
    await loadViews().catch(() => {});
    // A conflict is also how the daemon refuses every write while an entry
    // cannot be read: that is said in its words, not retried.
    if (page.blocked()) return answer;
    refusal.last = answer;
    return status === 404 && !baseState(draft, page.stored(key)) ? { stale: 'gone' } : null;
  }
}

// reorder moves one view by `step` places. A move past either end sends
// nothing, and a second move waits for the first.
async function reorder(id, step) {
  if (moving || page.blocked()) return;
  const order = moveOrder(viewIds(), id, step);
  if (!order) return;
  moving = true;
  notice = await guarded(() => reorderViews(order));
  moving = false;
  const at = viewIds().indexOf(id);
  const moved = page.stored(id);
  live.textContent = notice || !moved ? '' : (moved.name || 'View') + ' moved to position ' + (at + 1) + ' of ' + organization.views.length;
  show('view-' + id);
}

async function remove(view) {
  const refused = await guarded(() => deleteView(view.id));
  if (refused) throw new Error(refused);
  drafts.delete(view.id);
  navigate({ screen: 'index' }, 'new');
}

// duplicate creates the whole stored view again under another name.
async function duplicate(view, name) {
  const before = new Set(viewIds());
  const refused = await guarded(() => createView(viewCommit(duplicateOf(view, name))));
  if (refused) throw new Error(refused);
  navigate({ screen: 'view', id: viewIds().find(id => !before.has(id)) || '' });
}

// openInSessions shows the view in the session rail. The select event goes
// before the navigation: navigating alone restores the kept Sessions pane
// without a repaint (see the listener in views/sessions.js).
function openInSessions(id) {
  selectView(id);
  document.dispatchEvent(new CustomEvent('cg:view-select', { detail: id }));
  document.dispatchEvent(new CustomEvent('cg:nav', { detail: 'sessions' }));
}

// adopted runs when the views were adopted: a write from this page, one from
// the rail, or a re-read. The edit screen is never redrawn over what is being
// typed; it only checks its draft's base.
function adopted() {
  if (!host?.isConnected || saving) return;
  if (onAdopt) onAdopt();
  else if (viewsText() !== shown) show('');
}

// reread catches a change made while the page was not looked at: a hand edit
// of session-views.json, another tab, tagging done in Sessions.
function reread() {
  if (!host?.isConnected) return;
  const before = shown;
  // A read page's "Sessions now" comes from its own read, so it is drawn
  // again even when no view changed; the index only re-reads its counts.
  loadViews().catch(() => {}).then(() => { if (route.screen === 'view' && shown === before && !onAdopt) show(''); });
  if (!onAdopt) refreshCounts();
}

// Module-scope listeners: the page renders many times, and a per-render
// listener would accumulate.
if (typeof document !== 'undefined') {
  document.addEventListener('cg:views-changed', adopted);
  document.addEventListener('cg:view-restored', reread);
  window.addEventListener('focus', reread);
  document.addEventListener('visibilitychange', () => { if (document.visibilityState === 'visible') reread(); });
}
