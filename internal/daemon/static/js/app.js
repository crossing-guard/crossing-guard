// Entry point: bootstrap, nav dispatch, keyboard, rail, init. Imports every view.
import { S } from './state.js';
import { $, el, cpHeaders, api, fmtTime, normMemId, escapeHtml, mdInline, mdToHtml, debounce, fillSelect, mkSelectKV, mkSelect, lblWrap, fmtTok, shortWhen, SEV_CHIP, WATER_ORDER, CLASS_CHIP, getDefaults, setDefaults } from './core.js';
import { applyTheme, mkMark, mkLoader, mkSkeletons, withState, attachBottomPill, toggleHelp } from './ui.js';
import { openSession, renderSessionList, renderRail, renderTranscript, renderHandoffComposer } from './views/sessions.js';
import { clearBar } from './session-organization/query-bar.js';
import { renderUsage } from './views/usage.js';
import { renderMemory } from './views/memory.js';
import { renderChat } from './views/chat.js';
import { renderSkills } from './views/skills.js';
import { renderSettings } from './views/settings.js';
import { renderGovernance } from './views/governance.js';
import { refreshApprovalsBadge, renderInterrupt } from './views/approvals.js';
import { approvalProjectionStore } from './approval/approval-projection-store.js';
import { ApprovalAttentionClient } from './approval/approval-attention-client.js';
import { taskProjectionStore } from './task/task-projection-store.js';
import { sessionActivityStore } from './session/session-activity-store.js';
import { EventStreamClient } from './session/event-stream-client.js';
import { attachLiveTransport } from './session/session-live-client.js';
import { fetchTaskSnapshot } from './task/task-api.js';
import { fetchSessionActivitySnapshot } from './session/session-activity-api.js';
import { evidencePaneSource } from './infopanel.js';
import { activatePane, configurePaneHost, hidePaneHost, registerPaneSource, showPaneHost, toggleWorkspaceHidden } from './pane-host.js';
import { configureWorkspaceDiff, workspacePaneSource } from './views/workspace-diff.js';
import './orchestration/session-agents-panel.js'; // registers the session.agents right-column strip + panel
import * as refs from './refs.js';
import './views/refpanel.js'; // registers the ref.* info-panel providers
import './views/cartography.js'; // registers file.cartography (renders any descriptor)
import './views/session-governance.js'; // Governance-page bridge reused by the session frame
import './views/session-change.js'; // registers Changes (C6 P/T/Δ/C evidence)
import './views/session-impact.js'; // registers Effects (C7 bounded typed facts)
import './views/session-plan.js'; // registers Plan over the existing P/Δ/C projection
import './views/session-evidence.js'; // registers C9 persistent header + evidence boundary
import './views/session-trace.js'; // reusable exact action/resource Activity subview
import './views/session-verification.js'; // reusable verification subview
import './views/session-reach.js'; // reusable lazy capture diagnostics

registerPaneSource(evidencePaneSource);
registerPaneSource(workspacePaneSource);

let chipsExpanded = false;
let activeTask = null;

document.addEventListener('cg:task-active', e => {
  const d = e.detail || {};
  if (!(d.root instanceof Element) || typeof d.interrupt !== 'function') return;
  activeTask = d;
});

// --- auth token: arrives once via the printed URL fragment, then localStorage ---
// The fragment can also carry a DESTINATION, so a CLI that just recorded something
// can hand you the record rather than directions to it: a link is the difference
// between "here is what was blocked" and "go to Governance, then Capture, then find
// the session named…". Read before the fragment is stripped; applied after the
// first render, because the destination is a view that does not exist yet.
function parseGoto(hash) {
  const g = hash.match(/[#&]session=([A-Za-z0-9_.:-]+)/);
  if (!g) return null;
  return { tab: (hash.match(/[#&]tab=([a-z]+)/) || [, 'capture'])[1], session: g[1] };
}
function storeTokenFromHash(hash) {
  const m = hash.match(/[#&]t=([A-Za-z0-9_-]+)/);
  if (!m) return false;
  localStorage.setItem('cg_token', m[1]);
  return true;
}
// Primary session identity is browser navigation state, not cached evidence.
// Keep it in the query so refresh/Back/Forward can re-run the one authoritative
// openSession path while the auth key remains fragment-only.
function parseSessionLocation(search = location.search) {
  const query = new URLSearchParams(search);
  const runtime = query.get('runtime');
  const id = query.get('session');
  return runtime && id ? { runtime, id } : null;
}
function sessionLocation(selection) {
  const url = new URL(location.href);
  url.hash = '';
  if (selection?.runtime && selection?.id) {
    url.searchParams.set('runtime', selection.runtime);
    url.searchParams.set('session', selection.id);
  } else {
    url.searchParams.delete('runtime');
    url.searchParams.delete('session');
  }
  const query = url.searchParams.toString();
  return url.pathname + (query ? '?' + query : '');
}
let bootGoto = null;
let bootSession = parseSessionLocation();
(() => {
  const storedToken = storeTokenFromHash(location.hash);
  bootGoto = parseGoto(location.hash);
  // A normal auth bootstrap must not erase a session query. Governance handoff
  // is a different destination and intentionally clears ordinary session focus.
  if (storedToken || bootGoto) history.replaceState(null, '',
    location.pathname + (bootGoto ? '' : location.search));
})();
// A link pasted into an ALREADY-OPEN console changes only the fragment, which does
// not reload the page — so the bootstrap above never runs and the link silently
// does nothing. That is the normal way someone follows a link they were handed:
// the console is already open in a tab. Handle it as navigation, not as startup.
window.addEventListener('hashchange', () => {
  const storedToken = storeTokenFromHash(location.hash);
  const g = parseGoto(location.hash);
  if (!storedToken && !g) return;
  if (storedToken) {
    // A newly supplied key must re-run the failed API journey. Preserve a validated
    // destination across that one reload; startup consumes it on the next pass.
    const destination = g ? `#tab=${g.tab}&session=${g.session}` : '';
    history.replaceState(null, '', location.pathname + (g ? '' : location.search) + destination);
    location.reload();
    return;
  }
  history.replaceState(null, '', location.pathname);
  document.dispatchEvent(new CustomEvent('cg:govern-open',
    { detail: { tab: g.tab, session: { id: g.session } } }));
});

applyTheme(localStorage.getItem('cp_theme') || 'auto');

// ONE long-lived connection per tab (workspace-panes plan, Slice T). Every feed
// keeps its own projection store; only the transport is shared, so a second tab
// no longer starves the first behind the browser's six-connection cap.
const feedStatus = el('div', 'taskfeedstatus hidden');
feedStatus.setAttribute('role', 'status');
feedStatus.setAttribute('aria-live', 'polite');
document.body.appendChild(feedStatus);
const approvalAttentionClient = new ApprovalAttentionClient();
const eventStreamClient = new EventStreamClient({
  taskStore: taskProjectionStore, activityStore: sessionActivityStore, approvalStore: approvalProjectionStore,
  snapshots: { tasks: fetchTaskSnapshot, activity: fetchSessionActivitySnapshot },
  clientID: approvalAttentionClient.clientID,
  notify: detail => {
    const visible = detail.state !== 'connected';
    feedStatus.textContent = visible
      ? (detail.state === 'reconnecting' ? 'Live updates reconnecting…' : (detail.detail || 'Live updates degraded.'))
      : '';
    feedStatus.classList.toggle('hidden', !visible);
    document.dispatchEvent(new CustomEvent('cg:task-stream-state', { detail }));
  },
});
attachLiveTransport(eventStreamClient);
eventStreamClient.start(); approvalAttentionClient.start();
window.addEventListener('beforeunload', () => { eventStreamClient.stop(); approvalAttentionClient.stop(); });

/* ---------- navigation & keep-alive view lifecycle (ADR 0021 Phase 1) ----------
   Views survive navigation: on leaving a view its DOM is detached into a cache
   and reattached on return, so scroll position, form input, and in-flight work
   (a running chat stream) are preserved. The honest way to satisfy INV-22 is to
   keep UI state and revalidate DATA — not to destroy everything on every switch
   (see gui-client-architecture.md §4). This generalizes the old chat-only
   `chatDom` hack to every view.
   Freshness policy: 'live' = never cached, always re-render (approvals — SSE);
   'pin' = survive verbatim, never rebuilt even on re-click (chat's stream);
   default 'keep' = survive, but re-clicking the ACTIVE tab rebuilds it fresh
   (the refresh gesture, since kept views don't auto-refetch yet). */
const FRESH = { sessions: 'pin' }; // Session surface holds live chats in-place → survive verbatim; re-click never rebuilds
const paneCache = new Map(); // view -> { mf, sf, split, ms, ss }

function stashPane(view) {
  const mf = document.createDocumentFragment(); mf.append(...[...$('#main').childNodes]);
  const sf = document.createDocumentFragment(); sf.append(...[...$('#sidebody').childNodes]);
  paneCache.set(view, { mf, sf, split: $('#main').classList.contains('split'),
    ms: $('#main').scrollTop, ss: $('#sidebody').scrollTop });
}
function restorePane(view) {
  const p = paneCache.get(view);
  $('#main').append(p.mf); $('#sidebody').append(p.sf);
  $('#main').classList.toggle('split', p.split);
  requestAnimationFrame(() => { $('#main').scrollTop = p.ms; $('#sidebody').scrollTop = p.ss; });
  if (view === 'chat') $('#main').querySelector('textarea')?.focus();
  // A restored (keep-alive) governance pane re-appends a FROZEN liveness strip whose
  // "last event Ns ago" was computed when you left. Tell it to refresh NOW, not up to
  // 30s later on the next poll tick — a stale LIVE badge over a dead pipeline is the
  // exact freshness-lie the strip exists to kill.
  if (view === 'governance') document.dispatchEvent(new CustomEvent('cg:refresh-liveness'));
}

// One dispatch for both the nav list and the account menu (Settings/Skills live
// in the account menu, not the nav — console-design §4).
// Right reference panel follows the current surface + selection. Called after
// every render (fresh OR keep-alive restore), so the panel reappears when you
// return to a session, and clears on surfaces that have no providers.
function updateInfo() {
  // A clicked reference outranks the session it was clicked in: you asked about
  // THAT, and the panel keeps showing it until you clear it or change surface.
  if (S.selRef) showPaneHost({ surface: 'ref', selection: S.selRef, api });
  else if (S.view === 'sessions' && S.sel) showPaneHost({ surface: 'session', selection: S.sel, api });
  else if (S.view === 'memory' && S.selMemory) showPaneHost({ surface: 'memory', selection: S.selMemory, api });
  else if (S.view === 'skills' && S.selSkill) showPaneHost({ surface: 'skills', selection: S.selSkill, api });
  // A file clicked inside a Capture session outranks the session, the same way a
  // clicked reference outranks its transcript: you asked about THAT file.
  else if (S.view === 'governance' && S.governEntity) showPaneHost({ surface: 'governance-entity', selection: S.governEntity, tab: S.governTab, api });
  // Governance shows the panel with or WITHOUT a selection: the data-model module
  // (governance.model) explains the two axes and their counts, which is exactly what
  // a reader needs on the tab where they have not selected anything yet. Providers
  // that need a selection still declare `!!ctx.selection` themselves.
  else if (S.view === 'governance') showPaneHost({ surface: 'governance', selection: S.selGovern, tab: S.governTab, api });
  else hidePaneHost();
}
// Info bus: Governance's tabs change the selection WITHOUT a nav event, so they
// cannot rely on go()'s updateInfo. Routed over a bus so governance.js need not
// import app.js (which imports it — that edge would be a cycle).
document.addEventListener('cg:info', updateInfo);

function go(v) {
  const same = v === S.view;
  if (!same && FRESH[S.view] !== 'live') stashPane(S.view); // preserve the view we're leaving
  if (!same) { S.selRef = null; S.governEntity = null; } // a reference/file belongs to the surface it was clicked in
  S.view = v;
  document.querySelectorAll('nav button').forEach(x => x.classList.toggle('active', x.dataset.view === v));
  document.querySelectorAll('.jumppill').forEach(p => p.remove()); // pills are view-owned
  render(same).finally(updateInfo);
}
document.querySelectorAll('nav button[data-view]').forEach(b => b.onclick = () => go(b.dataset.view));
document.addEventListener('cg:nav', e => go(e.detail)); // nav bus: views navigate (e.g. Sessions→Usage) without importing each other

// sessions.js owns loading and rendering; app.js owns browser navigation. The
// success event is the narrow seam between them, avoiding a second router or a
// URL-aware session loader.
async function restoreLocatedSession(target) {
  if (!target) return;
  // openSession owns the pending/committed transaction. Keep the previous
  // committed selection coherent until the requested identity succeeds; a
  // failed Back/Forward target is reported against that target without erasing
  // the usable center and panel.
  S.selRef = null;
  await openSession(target);
}
document.addEventListener('cg:session-selected', e => {
  const target = e.detail;
  if (!target?.runtime || !target?.id) return;
  const current = parseSessionLocation();
  if (current?.runtime === target.runtime && current.id === target.id) return;
  history.pushState(null, '', sessionLocation(target));
});
window.addEventListener('popstate', () => {
  const target = parseSessionLocation();
  if (target) {
    restoreLocatedSession(target);
    return;
  }
  S.sel = null;
  S.selRef = null;
  renderSessionList().finally(updateInfo);
});
// Continue bus: Chat is the live mode of the Session surface — render the
// conversation into the session center IN PLACE, so the session list rail and the
// right context panel stay put. `fresh` = an ad-hoc new chat (no session context).
document.addEventListener('cg:continue', e => {
  const d = e.detail || {};
  if (d.fresh) { S.chatPreload = null; S.chatCwd = typeof d.cwd === 'string' ? d.cwd : null; S.sel = null; }
  S.view = 'sessions';
  document.querySelectorAll('nav button').forEach(x => x.classList.toggle('active', x.dataset.view === 'sessions'));
  renderChat();   // into #main; consumes S.chatPreload when continuing a session
  updateInfo();   // context panel persists on Continue (S.sel set); hidden for a fresh chat
});

// Sessions owns loading/transcript selection; Chat owns the live composer; app owns
// their composition. This one-way seam removes the sessions.js <-> chat.js ESM cycle
// without teaching either view how to construct the other.
document.addEventListener('cg:mount-chat', e => {
  const d = e.detail || {};
  if (!(d.container instanceof Element) || !(d.log instanceof Element) || !d.log.isConnected) return;
  renderChat(d.container, d.inline === true, d.log);
});

// Handoff bus: the composer's cross-runtime gate asks for the handoff draft.
// Routed over the bus so chat.js need not import sessions.js — that pair is
// already a cycle resolving only via hoisting, and this would be a third edge.
// The center swaps; rail and reference panel persist (panel contract §5).
document.addEventListener('cg:handoff', e => {
  const d = e.detail || {};
  if (!d.id) return;
  renderHandoffComposer({ runtime: d.runtime, id: d.id }, d.liveTurns || 0, d.target);
});

// Reference clicks — the "open" half of the reference model (console-and-info-panel
// §8). Delegated at the document so it covers every transcript, past and future,
// without each renderer wiring handlers. Three honest destinations:
//   a doc      -> the in-console doc viewer, anchored
//   code/file  -> the editor, at the line (vscode://), because reading code in
//                 full is the editor's job, not the panel's (INV lightweight)
//   any ref    -> the info panel, so "understand" happens without leaving the
//                 session you are reading
// Dim/stale references are spans, not anchors, so they never reach this handler.
document.addEventListener('click', e => {
  const a = e.target.closest?.('a.ref-ok');
  if (!a) return;
  e.preventDefault();
  const target = {
    kind: a.dataset.refKind || '', path: a.dataset.refPath || '',
    line: parseInt(a.dataset.refLine || '0', 10) || 0,
    token: a.dataset.refToken || a.textContent, root: refs.projectRoot(),
  };
  S.selRef = target;
  // Clicking a reference SHOWS it. It does not also launch your editor: doing
  // both meant one click detoured you into VS Code while the panel filled in
  // behind it, and there was no way back. Opening is an explicit second action
  // (the panel header's "Open in editor"); a doc still opens its in-console
  // viewer, which stays inside the console.
  updateInfo();
  if (target.kind === 'doc' || target.kind === 'id') openDocViewer(target);
});

// EDITOR_SCHEME is the one place the editor choice lives, so switching to
// cursor:// or jetbrains:// is a one-line change (design §14 open question —
// vscode is the assumed default, not a detected one).
let EDITOR_SCHEME = ''; // published by GET /api/console/config; never compiled in

// openDocViewer renders a repo markdown file in the center pane, anchored to the
// heading the reference names. The rail and the info panel persist — only the
// center swaps (console-design §5 panel contract), so you can read a doc without
// losing the session you were reading it from.
async function openDocViewer(target) {
  const main = $('#main');
  const path = target.path;
  // STASH the session pane rather than destroying it. The session surface is
  // 'pin' (never rebuilt — it can hold a live chat stream), so replaceChildren
  // here would drop the transcript on the floor and `go('sessions')` would not
  // bring it back. Detaching into a fragment keeps the DOM, the scroll position,
  // and any running stream alive while a doc is read over the top.
  const stashed = document.createDocumentFragment();
  stashed.append(...main.childNodes);
  const wasSplit = main.classList.contains('split');
  const scrollTop = main.scrollTop;
  main.classList.remove('split');
  const restore = () => {
    S.selRef = null;
    main.replaceChildren();
    main.append(stashed);
    main.classList.toggle('split', wasSplit);
    requestAnimationFrame(() => { main.scrollTop = scrollTop; });
    updateInfo();
  };
  const head = el('div', 'dochead');
  const back = el('button', 'btn', '◂ Back to session');
  back.onclick = restore;
  head.append(back, el('span', 'docpath', path));
  const openEd = el('button', 'btn', 'Open in editor');
  openEd.onclick = () => openInEditor(target);
  head.appendChild(openEd);
  main.appendChild(head);
  const body = el('div', 'md docview');
  main.appendChild(body);
  try {
    const d = await refs.doc(path);
    // A doc's own links are relative to ITS directory, not the repo root.
    body.innerHTML = mdToHtml(d.text || '', refs.boundTo(refs.dirOf(path)));
    if (d.truncated) main.appendChild(el('div', 'banner', 'This document was truncated for display.'));
    // Anchor to the item, not just the file — "click D5 and it opens the md" is
    // only useful if it lands ON D5. Headings carry slug ids, but our D-items are
    // defined in TABLE ROWS, so fall back to the rendered definition text. If
    // neither is found we stay at the top rather than pretending we jumped.
    if (target.kind === 'id') scrollToDefinition(body, target.token || '');
  } catch (e) {
    body.appendChild(el('div', 'banner', 'Could not open ' + path + ' — ' + (e.message || e)));
  }
}

// scrollToDefinition lands the viewer on the line that DEFINES an id. It tries
// the heading anchor first, then the bold declaration a tracker table uses
// (`**D18**` renders as <strong>D18</strong>) — the same definition-vs-mention
// rule the index applies, so the console jumps where the index pointed.
function scrollToDefinition(body, token) {
  const id = token.replace(/\s+/g, '');
  const byAnchor = body.querySelector('#' + CSS.escape(id));
  const hit = byAnchor || [...body.querySelectorAll('strong, b, h1, h2, h3')]
    .find(e => e.textContent.trim().replace(/\s+/g, '') === id);
  if (!hit) return;
  hit.scrollIntoView({ block: 'center' });
  // A brief highlight, because scrolling alone leaves the reader hunting for
  // which of thirty table rows they were sent to.
  const row = hit.closest('tr, li, p, h1, h2, h3') || hit;
  row.classList.add('refhit');
  setTimeout(() => row.classList.remove('refhit'), 2400);
}

// openInEditor hands a code reference to the reader's real editor. The console
// deliberately does not render code in full (INV lightweight). Invoked only by
// an explicit control, never as a side effect of clicking a reference.
document.addEventListener('cg:open-editor', e => openInEditor(e.detail || {}));
document.addEventListener('cg:close-ref', () => { S.selRef = null; updateInfo(); });
function openInEditor(target) {
  const root = target.root || '';
  if (!root || !target.path) return;
  // Encode per segment: the separators are structure, everything else is data.
  // A space, a '#' or a non-ASCII name would otherwise truncate or corrupt the
  // URL before the editor ever sees it.
  const abs = (root.replace(/\/$/, '') + '/' + target.path).split('/').map(encodeURIComponent).join('/');
  window.location.href = `${EDITOR_SCHEME}${abs}${target.line ? ':' + target.line : ''}`;
}

// Center bus: render a Sessions-surface center view IN PLACE (Usage today). Usage
// is a detail OF the session surface, not a surface of its own — so the rail keeps
// the session list and only the center swaps (console-design §5 panel contract).
// Using a bus rather than a direct import avoids a sessions.js ⇄ usage.js cycle.
document.addEventListener('cg:center', e => {
  const what = (e.detail || {}).view;
  S.view = 'sessions';
  document.querySelectorAll('nav button').forEach(x => x.classList.toggle('active', x.dataset.view === 'sessions'));
  S.sel = null; hidePaneHost(); // a dashboard, not a session selection
  document.querySelectorAll('#sidebody .sess').forEach(x => x.classList.remove('sel'));
  $('#main').replaceChildren(); $('#main').classList.remove('split');
  if (what === 'usage') renderUsage($('#main'));
});

// account / settings menu (pinned bottom of the left panel)
const acctBtn = $('#acctbtn'), acctMenu = $('#acctmenu');
const closeAcct = () => { acctMenu.classList.add('hidden'); acctBtn.setAttribute('aria-expanded', 'false'); };
acctBtn.onclick = e => { e.stopPropagation();
  const open = !acctMenu.classList.toggle('hidden'); acctBtn.setAttribute('aria-expanded', String(open)); };
document.querySelectorAll('#acctmenu button[data-view]').forEach(b => b.onclick = () => { go(b.dataset.view); closeAcct(); });
document.addEventListener('click', e => { if (!$('#account').contains(e.target)) closeAcct(); });
const themeBtn = $('#themebtn');
const THEMES = ['auto', 'dark', 'light'];
const setThemeLabel = () => { themeBtn.querySelector('span').textContent = 'Theme: ' + (localStorage.getItem('cp_theme') || 'auto'); };
setThemeLabel();
themeBtn.onclick = e => { e.stopPropagation();
  const cur = localStorage.getItem('cp_theme') || 'auto';
  applyTheme(THEMES[(THEMES.indexOf(cur) + 1) % THEMES.length]); setThemeLabel(); };

async function render(reclick) {
  $('#search').classList.toggle('hidden', S.view !== 'sessions');
  $('#searchstatus')?.classList.toggle('hidden', S.view !== 'sessions');
  const policy = FRESH[S.view] || 'keep';
  // 'pin' (Session surface): re-click never rebuilds (would discard an in-place
  // live chat); switching in restores the kept DOM.
  if (reclick && policy === 'pin') { $('#main').querySelector('textarea')?.focus(); return; }
  if (!reclick && policy !== 'live' && paneCache.has(S.view)) { // returning to a kept view
    $('#main').replaceChildren(); $('#sidebody').replaceChildren(); $('#main').classList.remove('split');
    restorePane(S.view); return;
  }
  // fresh render (first visit, a 'live' view, or an active-tab re-click)
  $('#main').replaceChildren(); $('#sidebody').replaceChildren();
  $('#main').classList.remove('split');
  paneCache.delete(S.view);
  if (S.view === 'sessions') await renderSessionList();
  if (S.view === 'memory') await renderMemory();
  if (S.view === 'governance') await renderGovernance();
  if (S.view === 'usage') await renderUsage();
  if (S.view === 'skills') await renderSkills();
  if (S.view === 'settings') renderSettings();
}

/* ---------- resizable rail (parity doc §5) ---------- */
(() => {
  const side = $('#side'), divider = $('#divider');
  const saved = localStorage.getItem('cp-rail-w');
  if (saved) document.documentElement.style.setProperty('--rail-w', saved + 'px');
  // guard: transiently-zero innerWidth (hidden pane, mid-layout) must never
  // invert the clamp — the ceiling is always at least 320px above the floor
  const clamp = w => Math.min(Math.max(w, 220), Math.max(window.innerWidth * 0.6, 540));
  divider.addEventListener('mousedown', e => {
    e.preventDefault();
    divider.classList.add('dragging');
    document.body.classList.add('dragging-rail');
    const move = ev => {
      const w = clamp(ev.clientX);
      document.documentElement.style.setProperty('--rail-w', w + 'px');
    };
    const up = () => {
      document.removeEventListener('mousemove', move);
      document.removeEventListener('mouseup', up);
      divider.classList.remove('dragging');
      document.body.classList.remove('dragging-rail');
      localStorage.setItem('cp-rail-w', String(Math.round(side.getBoundingClientRect().width)));
    };
    document.addEventListener('mousemove', move);
    document.addEventListener('mouseup', up);
  });
  divider.addEventListener('dblclick', () => {
    document.documentElement.style.removeProperty('--rail-w');
    localStorage.removeItem('cp-rail-w');
  });
})();

/* ---------- resizable right reference panel (mirrors the rail) ---------- */
(() => {
  const panel = $('#refpanel'), divider = $('#refdivider');
  const savedR = localStorage.getItem('cp-ref-w');
  if (savedR) document.documentElement.style.setProperty('--ref-w', savedR + 'px');
  // width grows from the RIGHT edge, so measure inward from window's right side
  const clamp = w => Math.min(Math.max(w, 240), Math.max(window.innerWidth * 0.6, 540));
  divider.addEventListener('mousedown', e => {
    e.preventDefault();
    divider.classList.add('dragging');
    document.body.classList.add('dragging-rail');
    // width grows from the panel's right edge (flush to the window) — measuring
    // from it rather than window.innerWidth is exact and avoids a 0-innerWidth trap
    const right = panel.getBoundingClientRect().right;
    const move = ev => {
      const w = clamp(right - ev.clientX);
      document.documentElement.style.setProperty('--ref-w', w + 'px');
    };
    const up = () => {
      document.removeEventListener('mousemove', move);
      document.removeEventListener('mouseup', up);
      divider.classList.remove('dragging');
      document.body.classList.remove('dragging-rail');
      localStorage.setItem('cp-ref-w', String(Math.round(panel.getBoundingClientRect().width)));
    };
    document.addEventListener('mousemove', move);
    document.addEventListener('mouseup', up);
  });
  divider.addEventListener('dblclick', () => {
    document.documentElement.style.removeProperty('--ref-w');
    localStorage.removeItem('cp-ref-w');
  });
})();

document.addEventListener('keydown', e => {
  const inField = /INPUT|TEXTAREA|SELECT/.test(document.activeElement?.tagName || '');
  // Cmd/Ctrl+O — expand/collapse all chips (native reflex: Claude Ctrl+O / Codex Ctrl+T)
  if ((e.metaKey || e.ctrlKey) && e.key === 'o') {
    e.preventDefault();
    chipsExpanded = !chipsExpanded;
    document.querySelectorAll('#main details.toolchip, #main details.thinkchip, #chatlog details')
      .forEach(d => { d.open = chipsExpanded; });
    return;
  }
  if (e.key === '/' && !inField) {
    e.preventDefault();
    if (S.view !== 'sessions') { document.querySelector('nav button[data-view="sessions"]').click(); }
    setTimeout(() => $('#search')?.focus(), 50);
  } else if (e.key === '?' && !inField) {
    e.preventDefault();
    toggleHelp();
  } else if (e.key === 'Escape') {
    if ($('#helpcard')) { toggleHelp(); return; }        // close help first
    if (activeTask?.root?.isConnected && activeTask.interrupt()) return;
    if (document.activeElement === $('#search') && $('#search').value) {
      // With a session open only the rail is redrawn: the landing half of
      // renderSessionList would close the session's stream and replace it.
      clearBar(); if (S.sel) renderRail(); else renderSessionList();
    } else if (inField) document.activeElement.blur();
  }
});

refreshApprovalsBadge();
renderInterrupt();

// Console presentation policy (presets, clamps, keymap, editor scheme) is the
// daemon's to publish and the browser's to read once; nothing here is compiled in.
let consoleKeymap = {};
const consoleConfigReady = api('/api/console/config').then(body => {
  const config = body?.config || {};
  configurePaneHost(config);
  configureWorkspaceDiff(config);
  consoleKeymap = config.keymap || {};
  EDITOR_SCHEME = config.editor_scheme || '';
  // The workspace column's first width is policy too; a width the reader chose wins.
  if (config.workspace_width_px > 0 && !localStorage.getItem('cp-ref-w')) {
    document.documentElement.style.setProperty('--ref-w', config.workspace_width_px + 'px');
  }
}).catch(() => configurePaneHost({}));
// keyMatches reads a binding like "Meta+Shift+Y" against a KeyboardEvent.
function keyMatches(binding, event) {
  if (!binding) return false;
  const parts = String(binding).split('+');
  const key = parts.pop();
  const want = new Set(parts.map(part => part.toLowerCase()));
  if (want.has('meta') !== event.metaKey || want.has('ctrl') !== event.ctrlKey || want.has('shift') !== event.shiftKey || want.has('alt') !== event.altKey) return false;
  return event.key.toLowerCase() === key.toLowerCase();
}
document.addEventListener('keydown', event => {
  if (keyMatches(consoleKeymap.hide_workspace, event)) { event.preventDefault(); toggleWorkspaceHidden(); }
  if (keyMatches(consoleKeymap.show_files, event)) { event.preventDefault(); document.dispatchEvent(new CustomEvent('cg:diff-show-files')); }
});
void activatePane; // re-exported through the host for provider drill-downs
consoleConfigReady.then(render).finally(async () => {
  updateInfo();
  // Deliver a deep link once the first view exists. Reuses the bus the session
  // panel already uses to hand a session to Governance, so there is one path into
  // that surface rather than a second one that can drift from it. Only the id is
  // known here; the tab resolves the rest.
  if (bootGoto) {
    document.dispatchEvent(new CustomEvent('cg:govern-open',
      { detail: { tab: bootGoto.tab, session: { id: bootGoto.session } } }));
    bootGoto = null;
    bootSession = null;
    return;
  }
  if (bootSession) {
    const target = bootSession;
    bootSession = null;
    await restoreLocatedSession(target);
  }
});
