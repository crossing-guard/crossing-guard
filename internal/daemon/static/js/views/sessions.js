import { $, el, cpHeaders, api, fmtTime, escapeHtml, mdInline, mdToHtml, linkify, debounce, fillSelect, mkSelectKV, mkSelect, lblWrap, fmtTok, shortWhen, SEV_CHIP, WATER_ORDER, CLASS_CHIP, getDefaults, setDefaults } from "../core.js";
import { applyTheme, mkMark, mkLoader, mkSkeletons, withState, attachBottomPill, toggleHelp, authRecovery } from "../ui.js";
import { S } from "../state.js";
import { showPaneHost, beginPaneDeckTransition, endPaneDeckTransition } from "../pane-host.js";
import * as refs from "../refs.js";
import { loadChatCapabilities, findChatCapability } from "../chat-capabilities.js";
import { taskProjectionStore } from "../task/task-projection-store.js";
import { hasRenderableText } from "../task/task-event-semantics.js";
import { approvalProjectionStore } from "../approval/approval-projection-store.js";
import { renderSessionStatus, SessionAttentionStore } from "../task/session-status.js";
import { mountSessionOrchestration } from "../orchestration/session-orchestration.js";
import { sessionActivityStore } from "../session/session-activity-store.js";
import { SessionLiveClient } from "../session/session-live-client.js";
import { SessionEventStore, describeTurnState, humanDuration } from "../session/session-event-store.js";
import { presentTranscriptIndexCoverage } from "../transcript-index-coverage.js";
import { applyTranscriptDecorators } from "../transcript-decorators.js";
import { HIDE, COLLAPSE, followingDisplay, selectProfile, rowPeek, rowLength, sizeLabel, hiddenUnits } from "../session/transcript-view.js";
import { activeQuery } from "../session-organization/organization-state.js";
import { organizedRailUrl, organizedPageUrl, railStateKey, groupsAreRepositories, groupLabel } from "../session-organization/view-group-source.js";
import { appendRowOrganization, repaintRowTags, rowTime } from "../session-organization/tag-chips.js";
import { mountViewList, configureViewList, refreshViewCounts, ensureViewsReady } from "../session-organization/view-list.js";
import { handleBarInput, configureQueryBar, loadViewIntoBar, clearBar } from "../session-organization/query-bar.js";
import { handleRowSelectClick, rowSessionsForTagging, configureRailSelection, clearRailSelection } from "../session-organization/rail-selection.js";
import { openTagPopover } from "../session-organization/tag-popover.js";
import { mountHeaderTags, configureHeaderTags } from "../session-organization/header-tags.js";
import "../session-organization/tag-shortcut.js";

let sessions = [], currentSession = null, sessionOpenGeneration = 0, sessionRailRenderGeneration = 0;

// ONE live-session stream per tab (natural-session plan B-GUI, red-team B1):
// the singleton enforces the budget — subscribing closes any prior stream.
const sessionLiveClient = new SessionLiveClient();
// One store for the open session's conversation. Not the task store: sessions
// are not tasks, and routing them through it would invent owned, stoppable
// work for sessions Crossing Guard never launched.
const sessionEventStore = new SessionEventStore();
// The composer reports each block it drew (by the vendor's record identity)
// and each prompt it sent; the store then skips the harvested copies.
document.addEventListener('cg:drawn-live', event => {
  const d = event.detail || {};
  if (d.anchor) sessionEventStore.markRenderedLive(d.anchor);
  if (d.prompt) sessionEventStore.markPromptRenderedLive(d.prompt);
});
let selectedTaskRefresh = () => {}, selectedStatusRefresh = () => {}, selectedAttentionCleanup = () => {};
let revealRailSelection = () => {};
const SESSION_PAGE_SIZE = 15;
const RAIL_STATE_KEY = 'cg-session-rail-state';
const RAIL_MODES = ['closed', 'open', 'all'];
const sessionAttentionStore = new SessionAttentionStore();

// The daemon decides what a session is doing; the browser draws it. The one
// input is the activity item the daemon published for this session; the one
// client-side fact applied on top is this reader's acknowledgement ledger.
function statusForSession(runtime, catalogID, _nativeID) {
  return renderSessionStatus(sessionActivityStore.activity(runtime, catalogID), sessionAttentionStore);
}

function makeStatusDot(status) {
  if (status.indicator.kind === 'none') return null;
  const dot = el('span', 'session-status-dot ' + status.indicator.kind);
  dot.title = status.indicator.label + (status.indicator.detail ? ' — ' + status.indicator.detail : '');
  dot.setAttribute('aria-hidden', 'true');
  return dot;
}

function paintSessionSelection(row, selected = S.sel) {
  const matches = Boolean(selected)
    && row.dataset.runtime === String(selected.runtime)
    && row.dataset.sessionId === String(selected.id);
  row.classList.toggle('sel', matches);
  if (matches) row.setAttribute('aria-current', 'true');
  else row.removeAttribute('aria-current');
}

function selectSessionRows(selected) {
  for (const row of document.querySelectorAll('#sidebody .sess[data-runtime][data-session-id]')) {
    paintSessionSelection(row, selected);
  }
}

function setPendingSessionRows(target) {
  for (const row of document.querySelectorAll('#sidebody .sess[data-runtime][data-session-id]')) {
    const pending = Boolean(target)
      && row.dataset.runtime === String(target.runtime)
      && row.dataset.sessionId === String(target.id);
    row.classList.toggle('pending', pending);
    if (pending) row.setAttribute('aria-busy', 'true');
    else row.removeAttribute('aria-busy');
    paintSessionStatus(row);
  }
}

function paintSessionStatus(row) {
  const slot = row.querySelector('.session-status-slot');
  if (!slot) return;
  const runtime = row.dataset.runtime, catalogID = row.dataset.sessionId, nativeID = row.dataset.resumeId;
  const status = statusForSession(runtime, catalogID, nativeID);
  const baseLabel = row.dataset.baseAriaLabel || row.getAttribute('aria-label') || 'Session';
  slot.replaceChildren();
  const dot = makeStatusDot(status);
  if (dot) slot.appendChild(dot);
  slot.title = dot?.title || '';
  const statusText = status.indicator.label || (status.execution !== 'idle' ? status.label : '');
  const label = statusText ? baseLabel + ' · ' + statusText : baseLabel;
  row.setAttribute('aria-label', row.classList.contains('pending') ? label + ' · Opening' : label);
}

function beginSessionTransition(target, generation) {
  const main = $('#main');
  main.querySelectorAll(':scope > .session-load-error').forEach(node => node.remove());
  main.classList.remove('session-load-feedback');
  main.classList.add('session-opening');
  main.setAttribute('aria-busy', 'true');
  setPendingSessionRows(target);
  const transition = { generation, panelToken: null, timer: null };
  transition.timer = setTimeout(() => {
    if (!(generation === sessionOpenGeneration) || !main.isConnected) return;
    const overlay = el('div', 'session-load-overlay');
    overlay.dataset.generation = String(generation);
    overlay.setAttribute('role', 'status');
    overlay.appendChild(mkLoader('Opening ' + (target.runtime || '') + ' session…'));
    main.appendChild(overlay);
    transition.panelToken = beginPaneDeckTransition('Opening ' + (target.title || 'session') + '…');
  }, 150);
  return transition;
}

function endSessionTransition(transition) {
  clearTimeout(transition.timer);
  if (transition.generation !== sessionOpenGeneration) return;
  const main = $('#main');
  main.querySelector(':scope > .session-load-overlay')?.remove();
  main.classList.remove('session-opening');
  main.removeAttribute('aria-busy');
  setPendingSessionRows(null);
  if (transition.panelToken != null) endPaneDeckTransition(transition.panelToken);
}

function showSessionOpenFailure(target, error, transition) {
  if (transition.generation !== sessionOpenGeneration) return;
  endSessionTransition(transition);
  const main = $('#main');
  main.classList.add('session-load-feedback');
  const card = el('div', 'session-load-error');
  card.setAttribute('role', 'alert');
  const title = el('strong', '', 'Could not open ' + (target.title || 'this session'));
  const detail = el('div', 'sub', error?.message || String(error));
  const retry = el('button', 'btn', 'Retry');
  retry.onclick = () => openSession(target);
  const dismiss = el('button', 'btn', 'Dismiss');
  dismiss.onclick = () => {
    card.remove();
    main.classList.remove('session-load-feedback');
  };
  const actions = el('div', 'row');
  actions.append(retry, dismiss);
  card.append(title, detail, actions);
  main.appendChild(card);
}

function refreshSessionStatuses() {
  sessionAttentionStore.establishBaseline(taskProjectionStore.all(), taskProjectionStore.cursor());
  for (const row of document.querySelectorAll('#sidebody .sess[data-runtime][data-session-id]')) paintSessionStatus(row);
  selectedTaskRefresh();
  selectedStatusRefresh();
}

function clearSelectedSessionBindings() {
  selectedTaskRefresh = () => {};
  selectedStatusRefresh = () => {};
  selectedAttentionCleanup();
  selectedAttentionCleanup = () => {};
  sessionLiveClient.close();
}

taskProjectionStore.subscribe(refreshSessionStatuses);
approvalProjectionStore.subscribe(refreshSessionStatuses);
sessionAttentionStore.subscribe(refreshSessionStatuses);
sessionActivityStore.subscribe(refreshSessionStatuses);

function readRailState() {
  try {
    const value = JSON.parse(localStorage.getItem(RAIL_STATE_KEY) || '{}');
    const state = Object.create(null);
    if (value && typeof value === 'object' && !Array.isArray(value)) {
      for (const [key, pref] of Object.entries(value)) state[key] = pref;
    }
    return state;
  } catch { return Object.create(null); }
}

function writeRailState(state) {
  try { localStorage.setItem(RAIL_STATE_KEY, JSON.stringify(state)); } catch { /* preference only */ }
}

function sessionRailLabel(key, nameCount) {
  const base = key === '(no project)' ? key : (key.split('/').filter(Boolean).pop() || key);
  if (key === '(no project)' || nameCount[base] === 1) return base;
  const parts = key.split('/').filter(Boolean);
  return base + ' · ' + (parts[parts.length - 2] || '/');
}

function modeLabel(mode) {
  return mode === 'open' ? 'Open' : (mode === 'all' ? 'All' : 'Closed');
}

async function preserveRailHeader(head, action, focusNode, isCurrent = () => true) {
  const side = $('#sidebody');
  if (!isCurrent() || !head.isConnected || !side?.isConnected) return;
  const before = head.getBoundingClientRect().top;
  await action();
  await new Promise(resolve => requestAnimationFrame(resolve));
  if (!isCurrent() || !head.isConnected || !side?.isConnected) return;
  const after = head.getBoundingClientRect().top;
  side.scrollTop += after - before;
  const target = typeof focusNode === 'function' ? focusNode() : focusNode;
  target?.focus({ preventScroll: true });
}

/* ---------- sessions ---------- */
// One session row — shared by the grouped list and flat search results.
// Identity, status, selection, and open behavior have one owner; grouped and
// search content remain separate because their metadata density is different.
function createSessionRowShell(s) {
  const d = el('button', 'sess');
  d.type = 'button';
  d.dataset.runtime = s.runtime;
  d.dataset.sessionId = s.id;
  d.dataset.resumeId = s.resume_id || s.id;
  const untitledIdentity = s.id ? ' · ' + String(s.id).slice(0, 8) : '';
  d.dataset.baseAriaLabel = (s.title || 'Untitled session' + untitledIdentity) + ' · ' + s.runtime;
  d.setAttribute('aria-label', d.dataset.baseAriaLabel);
  const statusSlot = el('span', 'session-status-slot');
  statusSlot.setAttribute('aria-hidden', 'true');
  d.appendChild(statusSlot);
  paintSessionSelection(d);
  paintSessionStatus(d);
  d.onclick = event => { if (!handleRowSelectClick(event, d, s)) openSession(s); };
  d.oncontextmenu = event => {
    event.preventDefault();
    openTagPopover(d, rowSessionsForTagging(s), repaintTaggedRows);
  };
  return d;
}

// repaintTaggedRows patches the rows of the sessions a tag change touched, and
// the view counts, in place. The rail is not re-rendered and the open session
// is not touched: tagging is something the owner does mid-conversation.
function repaintTaggedRows(updated) {
  for (const session of updated || []) {
    const rows = document.querySelectorAll('#sidebody .sess[data-runtime="' + CSS.escape(String(session.runtime))
      + '"][data-session-id="' + CSS.escape(String(session.id)) + '"]');
    rows.forEach(row => {
      repaintRowTags(row, session, { showNote: Boolean(activeQuery()) });
      paintSessionStatus(row); // the accessible name carries status after the tags
    });
  }
  refreshViewCounts();
}

// buildAgentFold renders the ONE compact collapsed "⚖ agents (n)" line a
// parent session may carry (G-5): agent runs never appear as flat rail rows —
// passive/decision runs fold away (the strip and Related sessions own them),
// and this line expands in place to the n agent-session rows, built by the
// same row constructor so selection/status painting stays one-owner. The
// fold starts expanded when the current selection is one of its children
// (opened from the strip), so exactly one selected row renders.
function buildAgentFold(parent, fold) {
  const host = el('div', 'agent-fold');
  const rows = el('div', 'agent-fold-rows');
  const toggle = el('button', 'agent-fold-toggle', '⚖ agents (' + fold.length + ')');
  toggle.type = 'button';
  toggle.title = 'Agent sessions that watched or acted on this session. They are not peer work sessions; full detail lives in the session’s agent strip.';
  const startOpen = Boolean(currentSession)
    && fold.some(child => child.runtime === currentSession.runtime && child.id === currentSession.id);
  const paint = open => {
    toggle.setAttribute('aria-expanded', String(open));
    rows.classList.toggle('hidden', !open);
  };
  toggle.onclick = () => paint(rows.classList.contains('hidden'));
  for (const child of fold) rows.appendChild(buildSessionRow(child, false));
  paint(startOpen);
  host.append(toggle, rows);
  return host;
}

function buildSessionRow(s, showProject) {
  const d = createSessionRowShell(s);
  // INV-22: render from the honesty label. A `prompt` title is NOT a title — it is
  // the first user message, truncated, because neither the user nor the runtime ever
  // named this session. Shown in the same typography as an authored title it reads
  // as one; "Reply with exactly: OK" looked like a deliberate name. custom / ai /
  // thread are all AUTHORED, so distinguishing between them here would be noise.
  const t = el('div', 't', s.title || '(untitled)');
  if (s.title_source === 'prompt') {
    t.classList.add('t-unnamed');
    t.title = 'No title authored by the runtime — showing the first prompt, truncated.';
  }
  d.appendChild(t);
  const m = el('div', 'm');
  m.appendChild(el('span', 'chip ' + s.runtime, s.runtime));
  // agent_role is set only on fold children (G-5): agent sessions never
  // render as flat rail rows, so this label appears inside a parent's
  // expanded "\u2696 agents (n)" fold, naming the child's role.
  if (s.agent_role) m.appendChild(el('span', 'modeltag agent-session-tag', '\u2696 agent \u00b7 ' + s.agent_role));
  if (showProject) m.appendChild(el('span', '', s.project.split('/').slice(-2).join('/')));
  m.appendChild(el('span', '', fmtTime(rowTime(s))));
  if (s.model) {
    const mm = el('span', 'modeltag', s.model.replace(/^claude-/, ''));
    mm.title = s.model + (s.turns ? ' · ' + s.turns + ' turns' : '');
    m.appendChild(mm);
  }
  if (s.activity_status === 'unknown') {
    const activity = el('span', 'modeltag', 'activity unknown');
    activity.title = 'This runtime does not expose exact per-session liveness evidence; unknown does not mean closed.';
    m.appendChild(activity);
  }
  // governor tags (cpmem engine index): branch, commits, PRs, memory writes
  if (s.branch) {
    const bt = el('span', 'modeltag', '⎇ ' + s.branch + (s.commits ? ' +' + s.commits : ''));
    bt.title = 'git branch' + (s.commits ? ' · ' + s.commits + ' commit(s) this session' : '');
    m.appendChild(bt);
  }
  for (const pr of (s.prs || []).slice(0, 2)) {
    const pt = el('span', 'prtag', pr);
    pt.title = 'issue/PR referenced in this session';
    m.appendChild(pt);
  }
  if (s.mem_writes > 0) {
    const mw = el('span', 'modeltag', '☰' + s.mem_writes);
    mw.title = s.mem_writes + ' memory write(s) from this session';
    m.appendChild(mw);
  }
  if (s.context > 0) {
    const cb = el('span', 'ctxbadge', fmtTok(s.context));
    cb.title = 'context at last turn: ' + s.context.toLocaleString() + ' tokens; window not stated by vendor';
    m.appendChild(cb);
  }
  d.appendChild(m);
  appendRowOrganization(d, s, { showNote: Boolean(activeQuery()) });
  paintSessionStatus(d); // the accessible name carries status after the tags
  return d;
}

function buildSearchSessionRow(s) {
  const d = createSessionRowShell(s);
  d.classList.add('sess-search');
  const shortID = String(s.id || '').slice(0, 8);
  d.appendChild(el('div', 't', s.title || ('Untitled · ' + shortID)));
  const meta = el('div', 'm');
  meta.appendChild(el('span', 'chip ' + s.runtime, s.runtime));
  const matchKind = el('span', 'modeltag search-hit-kind', s.kind === 'title' ? 'Title match' : 'Transcript match');
  matchKind.dataset.kind = s.kind || 'transcript';
  meta.appendChild(matchKind);
  if (s.project) meta.appendChild(el('span', '', s.project.split('/').slice(-2).join('/')));
  d.appendChild(meta);
  d.appendChild(el('div', 'search-snippet', String(s.snippet || '').trim()));
  return d;
}

function renderTranscriptIndexCoverage(coverage) {
  const presentation = presentTranscriptIndexCoverage(coverage);
  const box = el('div', 'banner transcript-index-coverage');
  box.dataset.state = presentation.state;
  box.setAttribute('role', 'status');
  box.appendChild(el('strong', 'transcript-index-coverage-state', presentation.label));
  box.appendChild(el('div', 'transcript-index-coverage-detail', presentation.detail));
  box.appendChild(el('div', 'transcript-index-coverage-time', presentation.timestampLabel));
  if (presentation.recovery) box.appendChild(el('div', 'transcript-index-coverage-recovery', presentation.recovery));
  return { box, presentation };
}

// renderSessionList shows the Sessions surface with no session open: the
// landing text in the centre and the rail. renderRail draws the rail alone and
// is what everything that happens WHILE a session is open must call — selecting
// a view, clearing the filter bar — because the landing half closes the open
// session's live stream and replaces the conversation.
async function renderSessionList() {
  renderSessionsLanding();
  return renderRail({ landing: true });
}

function renderSessionsLanding() {
  clearSelectedSessionBindings();
  $('#main').innerHTML = '<h2>Sessions</h2><div class="sub">Harvesting from attached runtime session stores…</div>';
}

async function renderRail(options = {}) {
  const railGeneration = ++sessionRailRenderGeneration;
  revealRailSelection = () => {};
  // Every row is about to be rebuilt; a selection of rows that no longer exist
  // would leave "3 selected" pointing at nothing the owner can see.
  clearRailSelection();
  await ensureViewsReady();
  if (railGeneration !== sessionRailRenderGeneration) return;
  const active = activeQuery();
  const fetchRail = () => options.prefetched ? Promise.resolve(options.prefetched)
    : api(organizedRailUrl(active, 500) || '/api/sessions?view=rail&repository_limit=500');
  const skel = el('div'); skel.appendChild(mkSkeletons(10));
  await withState($('#sidebody'), skel, fetchRail, data => {
    sessions = [];
    const side = $('#sidebody');
    // New chat — start an ad-hoc live conversation in the session surface (Chat is
    // no longer a separate nav item; it's the live mode of this surface).
    side.appendChild(mountViewList());
    const newChatPin = el('button', 'railpin');
    newChatPin.type = 'button';
    newChatPin.innerHTML = '<svg class="ico" viewBox="0 0 16 16" aria-hidden="true"><path d="M8 3.5v9M3.5 8h9"/></svg><span>New chat</span>';
    newChatPin.onclick = () => document.dispatchEvent(new CustomEvent('cg:continue', { detail: { fresh: true } }));
    side.appendChild(newChatPin);
    // Usage folds into Sessions (console-design §4): a pinned rail item that opens
    // the usage dashboard in the CENTER — the session list stays in the rail, since
    // Usage is a detail of this surface, not a surface of its own.
    const usagePin = el('button', 'railpin');
    usagePin.type = 'button';
    usagePin.innerHTML = '<svg class="ico" viewBox="0 0 16 16" aria-hidden="true"><path d="M3 13V8.5M7 13V3.5M11 13V6"/></svg><span>Usage</span>';
    usagePin.onclick = () => document.dispatchEvent(new CustomEvent('cg:center', { detail: { view: 'usage' } }));
    side.appendChild(usagePin);
    const repositories = data.repositories || [];
    if (!repositories.length) {
      side.appendChild(el('div', 'empty', active ? 'No sessions match.'
        : 'No sessions found. Sessions appear here after you use Claude Code or Codex on this machine.'));
      return;
    }
    // Repository identity, worktree folding, and /private normalization now come from
    // the harvested projection so server paging and the browser cannot disagree.
    const base = k => k === '(no project)' ? k : (k.split('/').filter(Boolean).pop() || k);
    const nameCount = {};
    for (const group of repositories) nameCount[base(group.key)] = (nameCount[base(group.key)] || 0) + 1;
    const railState = readRailState();
    const groupControllers = new Map();
    // Boot fills the expanded groups through a small queue so page load cannot
    // occupy the whole browser connection pool at once. Direct user actions
    // (header toggle, mode chip, pager, session click) never enter this queue.
    const backgroundFills = [];
    let activeFills = 0;
    const pumpFills = () => {
      while (activeFills < 2 && backgroundFills.length) {
        activeFills++;
        backgroundFills.shift()().finally(() => { activeFills--; pumpFills(); });
      }
    };
    const legacyCollapsed = (() => {
      try {
        const value = JSON.parse(localStorage.getItem('cp-collapsed') || 'null');
        return Array.isArray(value) ? new Set(value) : null;
      } catch { return null; }
    })();
    for (let index = 0; index < repositories.length; index++) {
      const group = repositories[index], key = group.key;
      const label = groupLabel(active, group) || sessionRailLabel(key, nameCount);
      // A view's groups keep their own open/closed memory, apart from the
      // repositories' and from other views'; a view opens with its groups shown.
      const stateKey = railStateKey(active, key);
      if (!railState[stateKey] || typeof railState[stateKey] !== 'object' || Array.isArray(railState[stateKey])) {
        const legacyOpen = legacyCollapsed && !legacyCollapsed.has(key);
        const firstUseful = index === 0 && (data.activity?.status === 'available' ? group.open_total > 0 : true);
        railState[stateKey] = active ? { mode: 'all', offset: 0 }
          : { mode: legacyOpen || firstUseful ? (group.open_total > 0 ? 'open' : 'all') : 'closed', offset: 0 };
      }
      const pref = railState[stateKey];
      if (!RAIL_MODES.includes(pref.mode)) pref.mode = 'closed';
      if (!Number.isInteger(pref.offset) || pref.offset < 0) pref.offset = 0;
      const head = el('div', 'projhead');
      const name = el('button', 'pname projname', label);
      name.type = 'button';
      const count = el('span', 'pcount', String(group.total));
      const create = el('button', 'projnew', '+');
      create.type = 'button';
      create.title = group.launch_cwd ? 'New chat in ' + group.launch_cwd : 'No project directory is available';
      create.setAttribute('aria-label', 'New chat in ' + label);
      create.disabled = !group.launch_cwd;
      create.hidden = !groupsAreRepositories(active); // a tag group has no folder to start a chat in
      create.onclick = e => {
        e.stopPropagation();
        document.dispatchEvent(new CustomEvent('cg:continue',
          { detail: { fresh: true, cwd: group.launch_cwd } }));
      };
      const disclosure = el('button', 'projtoggle projmode', modeLabel(pref.mode));
      disclosure.type = 'button';
      const bodyID = 'session-project-' + index;
      name.setAttribute('aria-controls', bodyID);
      disclosure.setAttribute('aria-controls', bodyID);
      // The header follows the Claude/Codex reflex: the NAME toggles collapsed ↔
      // the last expanded mode; the mode CHIP alone switches Open ↔ All. Cycling
      // all three states on the name made every glance at a project refetch twice.
      const expandedModeFor = () => (pref.last === 'open' || pref.last === 'all') ? pref.last
        : (group.open_total > 0 ? 'open' : 'all');
      const setA11y = () => {
        const expanded = pref.mode !== 'closed';
        name.setAttribute('aria-expanded', String(expanded));
        name.setAttribute('aria-label', label + (expanded
          ? ' · ' + modeLabel(pref.mode) + ' sessions; activate to collapse'
          : ' · collapsed; activate to expand'));
        disclosure.textContent = modeLabel(pref.mode);
        disclosure.setAttribute('aria-expanded', String(expanded));
        disclosure.setAttribute('aria-label', label + ' mode: ' + modeLabel(pref.mode) + '; activate for '
          + modeLabel(expanded ? (pref.mode === 'open' ? 'all' : 'open') : expandedModeFor()));
      };
      setA11y();
      head.append(name, count, create, disclosure);
      head.title = key;
      side.appendChild(head);
      const body = el('div', 'projbody');
      body.id = bodyID;
      if (pref.mode === 'closed') body.classList.add('hidden');
      side.appendChild(body);
      let loadGeneration = 0;
      const renderPage = (page, persist = true) => {
        pref.offset = page.offset;
        if (persist) writeRailState(railState);
        body.replaceChildren();
        if (pref.mode === 'open' && page.activity?.status !== 'available') {
          const unavailable = el('div', 'empty open-unavailable', page.activity?.detail || 'Open-session observation is unavailable.');
          const showAll = el('button', 'btn', 'Show all sessions');
          showAll.onclick = () => setMode('all', disclosure);
          unavailable.append(document.createElement('br'), showAll);
          body.appendChild(unavailable);
          return;
        }
        for (const s of page.sessions || []) {
          body.appendChild(buildSessionRow(s, false));
          const fold = page.agent_children?.[s.id] || [];
          if (fold.length) body.appendChild(buildAgentFold(s, fold));
        }
        // The current selection may live inside a fold (an agent session
        // opened from the strip) — that is on this page, not outside it.
        const selectionInFold = currentSession && Object.values(page.agent_children || {})
          .some(fold => fold.some(child => child.runtime === currentSession.runtime && child.id === currentSession.id));
        const selectedOutside = !active && currentSession && currentSession.repository_key === key
          && !selectionInFold
          && !(page.sessions || []).some(s => s.runtime === currentSession.runtime && s.id === currentSession.id);
        if (selectedOutside) {
          // A selected AGENT session never renders as a flat rail row (G-5),
          // even when its parent's fold is on another page: the label says
          // where it lives instead (postwork SF-5). orchestration_role is
          // the daemon's own agent-session marker on the session payload.
          if (currentSession.orchestration_role) {
            body.appendChild(el('div', 'selected-outside-label',
              'Selected agent session · folded under its parent on another page'));
          } else {
            body.appendChild(el('div', 'selected-outside-label', 'Selected session · outside this page'));
            body.appendChild(buildSessionRow(currentSession, false));
          }
        }
        if (!page.total) {
          body.appendChild(el('div', 'empty', pref.mode === 'open'
            ? 'No session in this repository is open right now.'
            : (active ? 'No sessions match.' : 'No sessions in this repository.')));
        }
        const pager = el('div', 'session-pager');
        const previous = el('button', 'btn', 'Previous');
        previous.disabled = page.offset === 0;
        previous.onclick = () => changePage(Math.max(0, page.offset - SESSION_PAGE_SIZE),
          () => body.querySelector('.session-pager button:first-child'));
        const end = Math.min(page.total, page.offset + (page.sessions || []).length);
        const range = el('span', 'session-range', page.total ? (page.offset + 1) + '–' + end + ' of ' + page.total : '0 of 0');
        range.setAttribute('aria-live', 'polite');
        const next = el('button', 'btn', 'Next');
        next.disabled = page.offset + SESSION_PAGE_SIZE >= page.total;
        next.onclick = () => changePage(page.offset + SESSION_PAGE_SIZE,
          () => body.querySelector('.session-pager button:last-child'));
        pager.append(previous, range, next);
        body.appendChild(pager);
        sessions = page.sessions || [];
      };
      const loadPage = async (focusNode, selected = null, persist = true) => {
        if (pref.mode === 'closed') return;
        const generation = ++loadGeneration;
        const url = organizedPageUrl(active, { key, mode: pref.mode, offset: pref.offset, limit: SESSION_PAGE_SIZE, selected })
          || '/api/sessions?view=repository&repository=' + encodeURIComponent(key)
          + '&mode=' + pref.mode + '&offset=' + pref.offset + '&limit=' + SESSION_PAGE_SIZE
          + (selected ? '&selected_runtime=' + encodeURIComponent(selected.runtime)
            + '&selected_id=' + encodeURIComponent(selected.id) : '');
        const isCurrent = () => generation === loadGeneration
          && railGeneration === sessionRailRenderGeneration && S.view === 'sessions' && head.isConnected;
        // Non-destructive refresh: rows already on screen stay (dimmed via
        // .loading) until the fresh page arrives; only an empty body shows a
        // loader row. Failure keeps the old rows and appends a retryable notice
        // instead of wiping the list (fit-and-finish plan, Slice 2).
        const load = async () => {
          body.classList.add('loading');
          body.querySelector(':scope > .session-load-issue')?.remove();
          let placeholder = null;
          if (!body.querySelector('.sess')) {
            placeholder = mkLoader('Loading ' + modeLabel(pref.mode).toLowerCase() + ' sessions…');
            body.appendChild(placeholder);
          }
          try {
            const page = await api(url);
            if (!isCurrent()) return;
            renderPage(page, persist);
          } catch (err) {
            if (!isCurrent()) return;
            placeholder?.remove();
            if (err?.status === 401 && !body.querySelector('.sess')) {
              body.replaceChildren(authRecovery());
              return;
            }
            const issue = el('div', 'empty session-load-issue');
            issue.append('✖ ' + (err.message || err), document.createElement('br'));
            const retry = el('button', 'btn', 'Retry');
            retry.style.marginTop = '8px';
            retry.onclick = () => loadPage(retry, selected, persist);
            issue.appendChild(retry);
            body.appendChild(issue);
          } finally {
            if (isCurrent()) body.classList.remove('loading');
          }
        };
        // Restored groups load concurrently. Only a direct user action owns scroll
        // and focus correction; background loads rely on browser scroll anchoring.
        if (focusNode) await preserveRailHeader(head, load, focusNode, isCurrent);
        else await load();
      };
      const setMode = async (mode, focusNode, selected = null, persist = true) => {
        pref.mode = mode;
        pref.offset = 0;
        if (persist) writeRailState(railState);
        setA11y();
        if (mode === 'closed') {
          ++loadGeneration;
          await preserveRailHeader(head, async () => {
            body.classList.add('hidden');
            body.replaceChildren();
          }, focusNode);
          return;
        }
        body.classList.remove('hidden');
        await loadPage(focusNode, selected, persist);
      };
      const changePage = async (offset, focusNode) => {
        pref.offset = offset;
        writeRailState(railState);
        await loadPage(focusNode);
      };
      name.onclick = event => {
        if (pref.mode === 'closed') return setMode(expandedModeFor(), event.currentTarget);
        pref.last = pref.mode;
        return setMode('closed', event.currentTarget);
      };
      disclosure.onclick = event => setMode(pref.mode === 'closed' ? expandedModeFor()
        : (pref.mode === 'open' ? 'all' : 'open'), event.currentTarget);
      groupControllers.set(key, { head, reveal: selected => {
        // Ephemeral reveal (FF-RT-9): navigation may expand or re-page a group to
        // show the selection, but it never persists mode/offset — only direct
        // header/chip/pager actions write the rail preference.
        if (pref.mode === 'closed') {
          pref.mode = 'all';
          body.classList.remove('hidden');
          setA11y();
        }
        loadPage(null, selected, false);
      } });
      if (pref.mode !== 'closed') backgroundFills.push(() => loadPage());
    }
    pumpFills();
    revealRailSelection = selected => {
      if (!selected) return;
      // A click on a visible row must be read-only toward the rail: no refetch,
      // no mode change, no reorder (fit-and-finish plan, Slice 2). Reveal work
      // happens only when the selected row is not currently in the DOM.
      const row = document.querySelector('#sidebody .sess[data-runtime="' + CSS.escape(String(selected.runtime))
        + '"][data-session-id="' + CSS.escape(String(selected.id)) + '"]');
      if (row) return;
      // A view's groups are keyed by what the view groups on, so reveal-by-
      // repository applies to the plain rail only.
      const controller = active ? null : groupControllers.get(selected?.repository_key);
      if (controller?.head.isConnected) controller.reveal(selected);
    };
    writeRailState(railState);
    const totalSessions = repositories.reduce((sum, group) => sum + group.total, 0);
    // What "open" means is the daemon's answer, not a second copy kept here: a
    // lane added server-side would silently falsify a hardcoded sentence.
    const activityNote = ' ' + (data.activity?.detail
      || (data.activity?.status === 'available'
        ? 'Open does not mean a model turn is generating.'
        : 'Open-session observation is unavailable on this machine.'));
    const repositoryNote = repositories.length < data.repository_total
      ? ' Showing ' + repositories.length + ' of ' + data.repository_total + ' projects; search reaches all sessions.'
      : ' Across ' + data.repository_total + ' projects.';
    if (options.landing) $('#main').innerHTML = '<h2>Sessions</h2><div class="sub">' + totalSessions +
      ' sessions.' + repositoryNote + ' From attached local runtime session stores. Select one to review, or search.' + activityNote +
      ' OpenCode session activity is reported as unknown because its sessions share one database.</div>';
  }, () => railGeneration === sessionRailRenderGeneration && S.view === 'sessions');
}

$('#search').oninput = debounce(async e => {
  const q = e.target.value.trim();
  // A term with a colon is a filter and is handled by the filter bar; plain
  // words are the search of every transcript, as always.
  if (await handleBarInput(q)) return;
  if (!q) return S.sel ? renderRail() : renderSessionList();
  const railGeneration = ++sessionRailRenderGeneration;
  revealRailSelection = () => {};
  await withState($('#sidebody'), mkLoader('Searching all sessions…'),
    () => api('/api/search?q=' + encodeURIComponent(q)), res => {
      const side = $('#sidebody');
      const hits = res.hits || [];
      const coverage = renderTranscriptIndexCoverage(res.coverage);
      side.appendChild(coverage.box);
      side.appendChild(el('div', 'empty', hits.length + ' indexed session' + (hits.length === 1 ? '' : 's') + ' · ' + res.source));
      if (!hits.length) {
        side.appendChild(el('div', 'empty', coverage.presentation.zeroAuthoritative
          ? 'No indexed sessions mention "' + q + '" as of ' + coverage.presentation.timestamp + '.'
          : 'No indexed matches yet. This is not a complete “not found” result while transcript coverage is limited.'));
        return;
      }
      for (const h of hits) {
        side.appendChild(buildSearchSessionRow(h));
      }
    }, () => railGeneration === sessionRailRenderGeneration && S.view === 'sessions');
}, 350);

async function openSession(s) {
  const generation = ++sessionOpenGeneration;
  const transition = beginSessionTransition(s, generation);
  try {
    const [d, allNotes, transcriptView] = await Promise.all([
      api(`/api/session?runtime=${s.runtime}&id=${encodeURIComponent(s.id)}`),
      api('/api/notes'),
      loadTranscriptView(),
    ]);
    if (generation !== sessionOpenGeneration) return false;

    // Reference discovery starts against an immutable root but is deliberately
    // not on the essential first-paint path. The previous committed session's
    // resolver remains active until this exact generation commits.
    const referenceScope = refs.forProject(d.cwd || '');
    const referenceLoad = referenceScope.start();

    // One synchronous browser commit: selection, center identity, and panel
    // chrome change in the same task. Provider/reference enhancements hydrate
    // only after their generation-scoped hosts exist.
    S.sel = d;
    selectSessionRows(d);
    referenceScope.activate();
    renderSessionDetail(d, allNotes, referenceScope, referenceLoad, transcriptView);
    void showPaneHost({ surface: 'session', selection: d, api });
    endSessionTransition(transition);

    const runtime = String(d.runtime || s.runtime || '');
    const id = String(d.id || s.id || '');
    if (runtime && id) {
      document.dispatchEvent(new CustomEvent('cg:session-selected', {
        detail: { runtime, id, repository_key: d.repository_key || '' },
      }));
      revealRailSelection({ runtime, id, repository_key: d.repository_key || '' });
    }
    return true;
  } catch (error) {
    showSessionOpenFailure(s, error, transition);
    return false;
  }
}

export function renderUsageStrip(u) {
  const strip = el('div', 'usage');
  const cell = (v, k, title) => {
    const c = el('div', 'u'); if (title) c.title = title;
    c.append(el('div', 'v', v), el('div', 'k', k));
    return c;
  };
  if (u.model) strip.appendChild(cell(u.model.replace(/^claude-/, ''), 'model'));
  strip.appendChild(cell(fmtTok(u.input_tokens + u.cache_read + u.cache_create), 'input', 'cumulative input incl. cache'));
  strip.appendChild(cell(fmtTok(u.output_tokens), 'output'));
  if (u.cache_hit_rate > 0) strip.appendChild(cell(Math.round(u.cache_hit_rate * 100) + '%', 'cache hit', 'cache_read / all input — the efficiency-parity number'));
  strip.appendChild(cell(String(u.turns), u.turns === 1 ? 'turn' : 'turns'));
  // context occupancy — bar only when the vendor states the window
  const ctx = el('div', 'u ctxbar');
  if (u.context_window > 0) {
    const pct = Math.min(100, Math.round(u.context / u.context_window * 100));
    ctx.append(el('div', 'v', fmtTok(u.context) + ' / ' + fmtTok(u.context_window) + ' (' + pct + '%)'), el('div', 'k', 'context window'));
    const track = el('div', 'track');
    const fill = el('div', 'fill');
    fill.style.width = pct + '%';
    track.appendChild(fill); ctx.appendChild(track);
  } else {
    ctx.append(el('div', 'v', fmtTok(u.context)), el('div', 'k', 'context at last turn · window not stated by vendor'));
  }
  strip.appendChild(ctx);
  return strip;
}

function hydrateSessionReferences(host, statusHost, scope, loadPromise, generation) {
  const apply = () => {
    if (generation !== sessionOpenGeneration || !host.isConnected) return;
    const state = scope.status();
    if (state.state === 'ready') {
      for (const pending of host.querySelectorAll('.ref-pending[data-ref-token]')) {
        const wrapper = document.createElement('span');
        wrapper.innerHTML = linkify(pending.dataset.refToken || pending.textContent || '', scope.resolve);
        pending.replaceWith(...wrapper.childNodes);
      }
      statusHost.textContent = '';
      statusHost.dataset.state = 'ready';
      return;
    }
    statusHost.textContent = state.state === 'unavailable'
      ? 'References unavailable — ' + (state.reason || 'the project index could not be read') + '.'
      : 'Indexing project references…';
    statusHost.dataset.state = state.state;
  };
  apply();
  Promise.resolve(loadPromise).then(apply, apply);
}

// loadTranscriptView reads the view profile modules and the console's selection
// on every session open, so a module file dropped in takes effect at the next
// open. Unreadable means no module: every row is shown.
async function loadTranscriptView() {
  try {
    const [config, modules] = await Promise.all([api('/api/console/config'), api('/api/console/view-profiles')]);
    return { selection: config?.config?.transcript || {}, profiles: modules?.profiles || [] };
  } catch {
    return { selection: {}, profiles: [] };
  }
}

// mountTranscriptViewSwitch lists every module; a choice applies to this view
// only and is not saved (transcript-view-profiles plan D-1).
function mountTranscriptViewSwitch(host, view, current, onChange) {
  if (!view.profiles.length) return;
  const select = el('select', 'transcript-view-switch');
  select.setAttribute('aria-label', 'Transcript view');
  for (const profile of view.profiles) {
    const option = el('option', '', profile.name || profile.id);
    option.value = profile.id;
    option.title = profile.description || '';
    select.appendChild(option);
  }
  select.value = current?.id || '';
  select.onchange = () => onChange(view.profiles.find(profile => profile.id === select.value) || null);
  host.appendChild(select);
}

function renderSessionDetail(d, allNotes, referenceScope = refs.forProject(d.cwd || ''), referenceLoad = null,
  transcriptView = { selection: {}, profiles: [] }) {
  clearSelectedSessionBindings();
  const main = $('#main');
  currentSession = d;
  main.innerHTML = '';
  main.classList.add('split'); // header + inner scroller; render() resets this
  const pane = el('div', 'detailscroll'); // everything below the header scrolls in here
  // pinned header: title + actions stay put while the transcript scrolls
  const head = el('div', 'stickyhead');
  const trow = el('div', 'trow');
  const h2 = el('h2', '', d.title || d.id);
  h2.title = d.title || d.id;
  trow.appendChild(h2);
  head.appendChild(trow);
  const statusSummary = el('div', 'session-status-summary');
  statusSummary.setAttribute('role', 'status');
  statusSummary.setAttribute('aria-live', 'polite');
  head.appendChild(statusSummary);
  main.appendChild(head);
  const sub = el('div', 'sub');
  sub.append(el('span', 'chip ' + d.runtime, d.runtime), ' ', d.project + ' · ' + d.events.length + ' events · ' + fmtTime(d.modified), ' ');
  if (d.usage?.model) {
    const mt = el('span', 'modeltag', d.usage.model.replace(/^claude-/, ''));
    mt.title = d.usage.model + ' · ' + d.usage.turns + ' turns · ' + Math.round((d.usage.cache_hit_rate || 0) * 100) + '% cache hit';
    sub.appendChild(mt);
  }
  if (d.branch) {
    const bt = el('span', 'modeltag', '⎇ ' + d.branch + (d.commits ? ' +' + d.commits : ''));
    bt.title = 'git branch' + (d.commits ? ' · ' + d.commits + ' commit(s)' : '');
    sub.appendChild(bt);
  }
  for (const pr of (d.prs || []).slice(0, 3)) sub.appendChild(el('span', 'prtag', pr));
  if (d.mem_writes > 0) {
    const mw = el('span', 'modeltag', '☰ ' + d.mem_writes + ' mem');
    mw.title = d.mem_writes + ' memory write(s) recorded by the governor';
    sub.appendChild(mw);
  }
  // exposure badge — pinned in the always-visible header (loadExposure fills it),
  // so the audit result is seen even above a long transcript
  const expoBadge = el('span', 'modeltag', '⋯ exposure');
  expoBadge.title = 'data exposure (audit water mark for this session)';
  sub.appendChild(expoBadge);
  if (d.usage?.context > 0) {
    const cb = el('span', 'ctxbadge', fmtTok(d.usage.context) + ' ctx');
    cb.title = 'context at last turn: ' + d.usage.context.toLocaleString() + ' tokens';
    sub.appendChild(cb);
  }
  // No Continue button: the session view IS the live view (console-design §6).
  // The composer renders inline under the transcript, so you reply where you read.
  head.appendChild(sub);
  mountHeaderTags(head, d);
  main.appendChild(pane);
  // Exposure card + usage now render in the right reference panel (session.exposure
  // / session.usage providers, below); the header keeps the pinned water-mark badge
  // as an always-visible summary.
  loadExposure(null, expoBadge, d.runtime, d.id);
  if (d.unparsed > 0) {
    pane.appendChild(el('div', 'banner',
      `Honest coverage: ${d.unparsed} lines could not be normalized and are not shown. The canonical-store version must close or declare this gap.`));
  }
  // note composer lives in the STICKY header as a text toggle — reachable from
  // anywhere in a long transcript, collapsed so the header stays lean. Text, not
  // an icon: a lone glyph makes the reader guess (icons carry labels here).
  const notes = allNotes.filter(n => n.target.startsWith(d.runtime + '/' + d.id));
  const noteToggle = el('button', 'btn', notes.length ? 'Notes · ' + notes.length : 'Add note');
  noteToggle.title = notes.length ? notes.length + ' note(s) pinned — click to add another' : 'Pin a note to this session';
  const noteRow = el('div', 'row hidden');
  noteRow.style.marginTop = '8px';
  const noteInput = el('input'); noteInput.placeholder = 'Add a note to this session…'; noteInput.style.flex = '1';
  const noteBtn = el('button', 'btn', 'Pin note');
  const pinIt = async () => {
    if (!noteInput.value.trim()) return;
    await api('/api/notes', { method: 'POST', body: JSON.stringify({ target: d.runtime + '/' + d.id, text: noteInput.value }) });
    noteInput.value = ''; noteBtn.textContent = 'Pinned ✓';
    noteToggle.textContent = 'Notes · ' + (notes.length + 1);
    setTimeout(() => { noteBtn.textContent = 'Pin note'; noteRow.classList.add('hidden'); }, 900);
  };
  noteBtn.onclick = pinIt;
  noteInput.onkeydown = e => { if (e.key === 'Enter') pinIt(); };
  noteRow.append(noteInput, noteBtn);
  noteToggle.onclick = () => {
    noteRow.classList.toggle('hidden');
    if (!noteRow.classList.contains('hidden')) noteInput.focus();
  };
  // Notes is now the only header action — the Continue buttons it used to be
  // inserted before are gone (console-design §6.2 rev. 3).
  trow.appendChild(noteToggle);
  // meta_id / thread_id ride along so the header's projection reads cover
  // every exact identity the runs may be recorded under (g4 plan §3).
  mountSessionOrchestration(trow, String(d.runtime || ''), String(d.id || ''),
    { cwd: String(d.cwd || ''), repository: String(d.repository_key || ''),
      meta_id: String(d.meta_id || ''), thread_id: String(d.thread_id || '') });
  head.appendChild(noteRow);

  // Session facts now render as the compact preface of the one Change Map owner;
  // the transcript center does not maintain a second footprint toggle or list.
  for (const n of notes) {
    const nd = el('div', 'banner', 'Note — ' + n.text + '  · ' + fmtTime(n.createdAt || n.created_at));
    nd.style.borderLeftColor = 'var(--accent2)';
    pane.appendChild(nd);
  }
  if (d.transcript_note) pane.appendChild(el('div', 'banner', d.transcript_note));
  // Agent findings are NOT rendered here. Independent-review output is the
  // reviewer's work, not this conversation's content, and as full cards under
  // the transcript it read as a wall of model prose the reader never asked
  // for. It lives on the surfaces about agents (Settings › Agents), where the
  // helper/follower work already is.
  const reviewGeneration = sessionOpenGeneration;
  const referenceStatus = el('div', 'session-reference-status sub', 'Indexing project references…');
  referenceStatus.setAttribute('role', 'status');
  pane.appendChild(referenceStatus);
  // transcript — SAME renderer family as the chat tab (one conversation
  // language everywhere), and opens at the END like every chat you've ever
  // used: the most recent exchange is why you opened it.
  const log = el('div', 'translog');
  // The events drawn so far, live deltas included, so a view switch redraws
  // the same conversation under another module.
  const transcript = { events: [...(d.events || [])],
    profile: selectProfile(transcriptView.profiles, transcriptView.selection, d.orchestration_role) };
  renderTranscript(log, transcript.events, referenceScope.resolve, referenceScope.root, { profile: transcript.profile });
  mountTranscriptViewSwitch(trow, transcriptView, transcript.profile, profile => {
    transcript.profile = profile;
    renderTranscript(log, transcript.events, referenceScope.resolve, referenceScope.root, { profile });
  });
  pane.appendChild(log);
  hydrateSessionReferences(pane, referenceStatus, referenceScope,
    referenceLoad || referenceScope.start(), reviewGeneration);
  // Natural-session live stream (plan B-GUI): ONE stream for the selected
  // session; deltas append through the SAME transcript renderer so rows are
  // identical to the static view.
  //
  // There is no second block under the transcript any more. A task's output
  // belongs in the conversation at its own moment or on the surfaces about
  // tasks — never pinned beneath the conversation as a parallel record with a
  // caption explaining our bookkeeping.
  sessionEventStore.reset();
  // What the reader actually wants to know: is it working, or is it my turn?
  const liveStatus = el('span', 'chip', '');
  trow.appendChild(liveStatus);
  let liveUnavailable = false;
  const paintTurnState = () => {
    if (!liveStatus.isConnected) return;
    if (liveUnavailable) {
      liveStatus.textContent = 'live updates unavailable · snapshot';
      liveStatus.className = 'chip st-disputed';
      liveStatus.title = 'The console cannot reach the live feed; the transcript below is the last snapshot it received.';
      return;
    }
    const rendered = renderSessionStatus(sessionEventStore.turnState, sessionAttentionStore);
    const shown = describeTurnState(sessionEventStore.turnState, Date.now(), rendered.seen);
    liveStatus.textContent = shown.age ? shown.text + ' · ' + shown.age : shown.text;
    liveStatus.className = 'chip';
    liveStatus.title = 'What this session is doing right now, and when it last did anything.';
  };
  paintTurnState();
  // The age keeps aging while nothing arrives, so re-render it on the cadence
  // the daemon published rather than leaving a frozen number on screen.
  const ageTick = setInterval(() => {
    if (!liveStatus.isConnected) { clearInterval(ageTick); return; }
    paintTurnState();
  }, (sessionEventStore.ageTickSeconds || 15) * 1000);
  sessionLiveClient.subscribe(String(d.runtime || ''), String(d.id || ''), {
    onEvents: events => {
      if (!log.isConnected || currentSession !== d) return;
      const fresh = sessionEventStore.accept(events);
      if (fresh.length) {
        transcript.events.push(...fresh);
        appendTranscriptEvents(log, fresh, referenceScope.resolve, referenceScope.root, transcript.profile);
      }
      const scroller = log.closest('.detailscroll');
      if (scroller) scroller.scrollTop = scroller.scrollHeight;
    },
    onTurnState: state => {
      if (!liveStatus.isConnected || currentSession !== d) return;
      const wasRunning = sessionEventStore.turnState?.execution === 'running';
      sessionEventStore.setTurnState(state);
      paintTurnState();
      selectedStatusRefresh();
      // A turn boundary is the Diff pane's refresh trigger (design §3.4, R14):
      // the session stopped running, so its edits and the checkout may have moved.
      if (wasRunning && state?.execution !== 'running') {
        document.dispatchEvent(new CustomEvent('cg:session-turn-state', { detail: { runtime: d.runtime, id: d.thread_id || d.id, execution: state?.execution } }));
      }
    },
    onIdentity: payload => {
      if (!liveStatus.isConnected || currentSession !== d) return;
      if (payload && payload.following === false) {
        liveStatus.textContent = 'no longer following this session';
        liveStatus.className = 'chip st-disputed';
        liveStatus.title = 'This session is no longer resolvable, so the console stopped following it. That is not a claim that it ended.';
      }
    },
    onGovernance: () => {
      // The strip and action surfaces refresh off the same channel; the
      // projections themselves live in their own stores.
      document.dispatchEvent(new CustomEvent('cg:session-activity-delta'));
    },
    onStatus: (status, detail) => {
      if (!liveStatus.isConnected || currentSession !== d) return;
      liveUnavailable = status !== 'live';
      if (status === 'live' && detail && typeof detail.age_tick_seconds === 'number') {
        sessionEventStore.ageTickSeconds = detail.age_tick_seconds;
      }
      paintTurnState();
    },
  });
  // A task's output belongs in the conversation at its own moment, or on the
  // surfaces about tasks — never pinned beneath the conversation as a second
  // record with a caption explaining why we could not join the two. That block
  // was the last thing visible on every visit, so a session whose newest task
  // died days ago read as a transcript that had stopped updating.
  selectedTaskRefresh = () => {};
  let maybeAcknowledge = () => {};
  selectedStatusRefresh = () => {
    if (!statusSummary.isConnected) return;
    const status = statusForSession(d.runtime, d.id, d.resume_id || d.id);
    statusSummary.replaceChildren();
    const dot = makeStatusDot(status);
    if (dot) statusSummary.appendChild(dot);
    const copy = el('span', 'session-status-copy');
    const heading = status.label;
    copy.appendChild(el('strong', '', heading));
    const explanation = status.indicator.detail || status.detail;
    if (explanation) copy.appendChild(el('span', 'session-status-detail', explanation));
    statusSummary.appendChild(copy);
    statusSummary.dataset.status = status.indicator.kind;
    requestAnimationFrame(() => maybeAcknowledge());
  };
  const latestEdgeVisible = () => pane.isConnected
    && document.visibilityState === 'visible' && document.hasFocus()
    && pane.scrollTop + pane.clientHeight >= pane.scrollHeight - 60;
  maybeAcknowledge = () => {
    if (!latestEdgeVisible()) return;
    const status = statusForSession(d.runtime, d.id, d.resume_id || d.id);
    if (!['new_result', 'new_failure', 'interrupted'].includes(status.attention)) return;
    sessionAttentionStore.acknowledge(d.runtime, d.id, d.resume_id || d.id,
      status.attention_source, status.attention_id);
  };
  const onAttentionOpportunity = () => requestAnimationFrame(maybeAcknowledge);
  pane.addEventListener('scroll', onAttentionOpportunity, { passive: true });
  window.addEventListener('focus', onAttentionOpportunity);
  document.addEventListener('visibilitychange', onAttentionOpportunity);
  selectedAttentionCleanup = () => {
    pane.removeEventListener('scroll', onAttentionOpportunity);
    window.removeEventListener('focus', onAttentionOpportunity);
    document.removeEventListener('visibilitychange', onAttentionOpportunity);
  };
  selectedTaskRefresh();
  selectedStatusRefresh();
  attachBottomPill(pane, 'pill-session'); // item 3 — pill tracks the inner scroller
  // Capability discovery and command dispatch come from the same registered
  // adapter. Session presentation never owns a runtime allowlist or ID parser.
  const composerHost = log.parentNode || main;
  const capabilityPending = el('div', 'sub', 'Checking whether this harness can resume…');
  composerHost.appendChild(capabilityPending);
  const renderReadOnly = reason => {
    if (!log.isConnected) return;
    capabilityPending.remove();
    const dead = el('div', 'composer-dead');
    const ta = el('textarea');
    ta.placeholder = 'Read-only — no resume path for runtime "' + d.runtime + '"';
    ta.disabled = true; ta.rows = 1;
    dead.append(ta, el('div', 'sub', reason));
    composerHost.appendChild(dead);
  };
  loadChatCapabilities().then(capabilities => {
    if (!log.isConnected) return;
    const capability = findChatCapability(capabilities, d.runtime);
    if (!capability?.canResume) {
      renderReadOnly('This session was recorded by "' + d.runtime
        + '", whose registered chat adapter does not expose resume support.');
      return;
    }
    capabilityPending.remove();
    S.chatPreload = {
      runtime: d.runtime, sessionId: d.resume_id || d.id, harvestId: d.id,
      cwd: d.cwd || '', title: d.title, model: d.model || d.usage?.model || '',
    };
    try {
      document.dispatchEvent(new CustomEvent('cg:mount-chat', {
        detail: { container: composerHost, inline: true, log },
      }));
    } finally { S.chatPreload = null; }
  }).catch(error => renderReadOnly('Resume capability could not be loaded: ' + (error.message || error)));
  // open at the END. One anchored settle instead of the old rAF + 80 ms double
  // scroll: the second jump was the visible "bounce" on session open. Fonts are
  // the layout input rAF races, so the single correction waits on fonts.ready and
  // runs only while the reader is still at the latest edge (fit-and-finish plan).
  const toEnd = () => {
    pane.scrollTop = pane.scrollHeight;
    const p = document.getElementById('pill-session');
    if (p) {
      p.lastElementChild.classList.remove('show');
      const showOldest = pane.scrollTop > 300;
      p.firstElementChild.classList.toggle('show', showOldest);
      p.classList.toggle('show', showOldest);
    }
    requestAnimationFrame(maybeAcknowledge);
  };
  requestAnimationFrame(toEnd);
  const stillAtLatest = () => pane.isConnected
    && pane.scrollTop + pane.clientHeight >= pane.scrollHeight - 300;
  (document.fonts?.ready || Promise.resolve()).then(() => requestAnimationFrame(() => {
    if (stillAtLatest()) toEnd();
  }));
}

// toolPeek turns a tool call's raw arguments into the one thing worth showing
// while the chip is collapsed.
//
// Tool arguments arrive as JSON, so the old "first 70 characters" peek rendered
// `{"file_path":"/Users/example/Documents/Sites/example-project` —
// punctuation, then the prefix every file on the machine shares, cut off BEFORE
// the filename. It was unreadable and, because the path was truncated
// mid-string, unresolvable: the transcript showed no links precisely where the
// files are.
//
// Deliberately shape-agnostic rather than keyed to tool names: parse, take the
// most substantial string value, make it project-relative. That works for a
// read, a write or a shell command, and for either runtime (INV — canonical, no
// per-vendor branching).
// PEEK_FIELDS names the argument a tool is ABOUT, best first.
//
// Not a per-tool branch: one table, consulted by name, so an unknown tool falls
// through to the longest-string heuristic instead of breaking. The old code had
// only that heuristic, and for a Write the longest string is the file CONTENT —
// so a list of 91 edits showed 91 opening comment blocks and never once said
// which file. Scanning edits by tool name is useless; scanning them by filename
// is the review.
// RESULT_SEP joins a tool call's arguments to its result inside one chip.
const RESULT_SEP = '\n── result ──\n';

const PEEK_FIELDS = ['file_path', 'path', 'command', 'pattern', 'query', 'url', 'prompt', 'notebook_path'];

function toolPeek(text, projectRoot = refs.projectRoot()) {
  const raw = String(text || '').trim();
  let shown = raw;
  try {
    const obj = JSON.parse(raw);
    if (obj && typeof obj === 'object' && !Array.isArray(obj)) {
      const named = PEEK_FIELDS.map(k => obj[k]).find(v => typeof v === 'string' && v.trim());
      if (named) {
        shown = named;
      } else {
        const strings = Object.values(obj).filter(v => typeof v === 'string' && v.trim());
        if (strings.length) shown = strings.reduce((a, b) => (b.length > a.length ? b : a));
      }
    }
  } catch {
    // Clipped mid-object, so JSON.parse fails — and these are precisely the
    // BIGGEST edits, the ones most worth labelling. Pull the field out of the
    // fragment textually rather than falling back to showing raw JSON.
    const m = new RegExp('"(?:' + PEEK_FIELDS.join('|') + ')"\\s*:\\s*"((?:[^"\\\\]|\\\\.)*)"').exec(raw);
    if (m) {
      try { shown = JSON.parse('"' + m[1] + '"'); } catch { shown = m[1]; }
    }
  }
  if (projectRoot) shown = shown.split(projectRoot + '/').join(''); // relative reads shorter AND resolves
  shown = shown.replace(/\s+/g, ' ').trim();
  return shown.length > 96 ? shown.slice(0, 96) + '…' : shown;
}

// RESULT_PATH matches the paths tool results state in prose. Only used to
// LABEL a chip whose arguments were clipped before the filename appeared.
const RESULT_PATH = /(?:created successfully at|The file|Updated|Wrote)[:\s]+(\/[^\s,)]+)/i;

// relabelPeekFromResult replaces a chip's summary when it is still showing raw
// JSON and the result names the file. Silent no-op otherwise — a peek that
// already says something useful is never overwritten.
function relabelPeekFromResult(det, resultText, projectRoot = refs.projectRoot(), resolveReference = refs.resolve) {
  const peek = det.querySelector('.tpeek');
  if (!peek || !peek.textContent.trim().startsWith('{')) return;
  const m = RESULT_PATH.exec(resultText);
  if (!m) return;
  const shown = projectRoot ? m[1].split(projectRoot + '/').join('') : m[1];
  peek.innerHTML = ' ' + linkify(shown, resolveReference);
}

// decodePayload turns a tool's raw JSON argument blob into something a human
// reads at review speed.
//
// Every shell command in every transcript was rendered as its JSON encoding —
// `2>&1` for `2>&1`, `&&` for `&&`, backslashed quotes
// throughout. Unreadable, and it also manufactured phantom references: the
// linkifier tokenised `>/tmp/ro.php` and dimmed it as a missing file.
//
// Only presentation changes here. Unparseable input (including a payload
// clipped mid-object, which is most of them before expansion) is returned
// untouched — a best-effort prettifier must never eat content.
function decodePayload(text) {
  const raw = String(text || '');
  let obj;
  try { obj = JSON.parse(raw); } catch { return raw; }
  if (!obj || typeof obj !== 'object' || Array.isArray(obj)) return raw;
  const out = [];
  for (const [k, v] of Object.entries(obj)) {
    if (v == null || v === '') continue;
    const val = typeof v === 'string' ? v : JSON.stringify(v, null, 2);
    // A one-line scalar reads as a header; anything multi-line gets its own
    // block, because that is the shape of a command, a file, or a diff side.
    out.push(val.includes('\n') || val.length > 80 ? k + ':\n' + val : k + ': ' + val);
  }
  return out.length ? out.join('\n\n') : raw;
}

// lazyBody holds a tool/thinking payload as TEXT and linkifies it the first time
// the chip is opened.
//
// Why lazy: a session has hundreds of these and a single tool result can be a
// whole file, so linkifying all of them up front would cost seconds of paint for
// content nobody has looked at. <details> is collapsed by default, so the work
// lands exactly when someone asks to read it.
function lazyBody(text, ev) {
  const body = el('div', 'tbody');
  body.dataset.raw = text;
  body.textContent = text; // safe until the chip is opened
  // A clipped payload records where the rest lives, so expanding can go get it.
  if (ev && ev.full_len > 0) {
    body.dataset.seq = ev.seq;
    body.dataset.fullLen = ev.full_len;
  }
  return body;
}

// paintBody linkifies a chip body, once, when it is visible — fetching the
// untruncated payload first if the transcript only carried a fragment.
//
// The transcript clips tool payloads so a 1,300-event session stays openable.
// That clip is fine for recognising an event and useless for reviewing one:
// agent-written code opens with a doc comment, so the surviving bytes were the
// prose and the discarded bytes were the code. Expanding a chip is the moment
// someone has asked to actually read it, so that is when we pay for the rest.
async function paintBody(body) {
  const det = body.closest('details');
  if (!det || !det.open || body.dataset.linked === '1') return;
  body.dataset.linked = '1';
  const render = () => {
    // Split the arguments from the result before decoding: only the first half
    // is a JSON blob, and the result is already plain text.
    const [args, ...rest] = (body.dataset.raw || '').split(RESULT_SEP);
    const shown = decodePayload(args) + (rest.length ? RESULT_SEP + rest.join(RESULT_SEP) : '');
    body.innerHTML = linkify(shown, refs.resolve);
  };
  render(); // show what we have immediately; the fetch only ever adds

  if (!body.dataset.seq || body.dataset.fetched === '1') return;
  body.dataset.fetched = '1';
  const d = currentSession;
  if (!d) return;
  try {
    const full = await api('/api/session/event?runtime=' + encodeURIComponent(d.runtime) +
      '&id=' + encodeURIComponent(d.id) + '&seq=' + encodeURIComponent(body.dataset.seq));
    if (full.available && full.text) {
      // Replace ONLY the arguments half. The fetch returns this one event — the
      // tool_call's arguments — but the paired tool_result was appended
      // client-side at a different seq (RESULT_SEP), and it is already on screen.
      // Overwriting the whole dataset would silently drop the result the reader
      // is looking at.
      const [, ...rest] = (body.dataset.raw || '').split(RESULT_SEP);
      body.dataset.raw = full.text + (rest.length ? RESULT_SEP + rest.join(RESULT_SEP) : '');
      render();
      return;
    }
    // Could not deepen it. Say which, rather than leaving a fragment looking
    // complete — the ellipsis alone reads as "a bit more", not "20KB more".
    body.appendChild(el('div', 'sub', full.note ||
      'showing the first ' + (body.dataset.raw || '').length + ' of ' +
      body.dataset.fullLen + ' characters — the rest could not be read'));
  } catch (e) {
    body.appendChild(el('div', 'sub',
      'clipped at ' + (body.dataset.raw || '').length + ' of ' + body.dataset.fullLen +
      ' characters; could not fetch the rest — ' + (e.message || e)));
  }
}

// One delegated listener rather than one per chip: a session can hold hundreds.
document.addEventListener('toggle', e => {
  const det = e.target;
  if (!(det instanceof HTMLDetailsElement) || !det.open) return;
  const body = det.querySelector(':scope > .tbody');
  if (body) paintBody(body);
}, true);

const TRANSCRIPT_WINDOW_EVENTS = 500;

function transcriptWindowStart(events, target) {
  let start = Math.max(0, target);
  const floor = Math.max(0, start - 120);
  while (start > floor && !['user', 'assistant', 'summary'].includes(events[start]?.kind)) start--;
  return start;
}

// Large histories are one conversation, not one mandatory DOM allocation. The
// latest bounded window uses this same renderer; “Load older” repaints a wider
// prefix and preserves the reader's anchor rather than introducing a second
// transcript implementation or a payload cache.
function renderTranscript(log, events, resolveReference = refs.resolve,
  projectRoot = refs.projectRoot(), options = {}) {
  const source = Array.isArray(events) ? events : [];
  const profile = options.profile || null;
  if (options.windowed === false || source.length <= TRANSCRIPT_WINDOW_EVENTS) {
    log.replaceChildren();
    appendTranscriptEvents(log, source, resolveReference, projectRoot, profile);
    return;
  }
  const paint = (start, preserveAnchor = false) => {
    const scroller = log.closest('.detailscroll');
    const beforeHeight = preserveAnchor ? log.scrollHeight : 0;
    const beforeTop = preserveAnchor && scroller ? scroller.scrollTop : 0;
    log.replaceChildren();
    if (start > 0) {
      const control = el('div', 'transcript-window-control');
      const loadOlder = el('button', 'btn', 'Load older · ' + start + ' events hidden');
      loadOlder.onclick = () => {
        const next = transcriptWindowStart(source, Math.max(0, start - TRANSCRIPT_WINDOW_EVENTS));
        paint(next, true);
      };
      control.appendChild(loadOlder);
      log.appendChild(control);
    }
    appendTranscriptEvents(log, source.slice(start), resolveReference, projectRoot, profile);
    if (preserveAnchor && scroller) {
      const delta = log.scrollHeight - beforeHeight;
      scroller.scrollTop = beforeTop + delta;
      requestAnimationFrame(() => log.querySelector('.transcript-window-control .btn')?.focus({ preventScroll: true }));
    }
  };
  paint(transcriptWindowStart(source, source.length - TRANSCRIPT_WINDOW_EVENTS));
}

// Rows a view profile may fold into one line, and the label that line carries.
// Tool calls and thinking are chips already, so collapsing them changes nothing.
const ROW_CHIP_LABELS = {
  user: ev => 'You' + (ev.ts ? ' · ' + fmtTime(ev.ts) : ''),
  assistant: () => 'Agent',
  summary: () => 'summary',
  system: ev => 'system · ' + (ev.name || ''),
  context: ev => 'context · ' + (ev.name || ''),
  other: ev => ev.name || 'other',
};

// rowChip is a collapsed row: its label, size and first line; the body fills in
// when opened, through the same lazy path as tool chips.
function rowChip(ev) {
  const det = document.createElement('details');
  det.className = 'thinkchip rowchip';
  const sum = el('summary');
  // Size before the peek: the peek is what gets cut when the line runs out.
  sum.append(el('span', 'rowchip-label', ROW_CHIP_LABELS[ev.kind](ev)),
    el('span', 'rowchip-size', ' · ' + sizeLabel(rowLength(ev)) + ' · '),
    el('span', 'tpeek', rowPeek(ev.text)));
  det.append(sum, lazyBody(ev.text || '', ev));
  return det;
}

// A hidden row is never silent: consecutive hidden rows share one count that
// draws them in place when pressed.
const hiddenRuns = new WeakMap();

function appendHiddenRow(log, ev, resolveReference, projectRoot) {
  let control = log.lastElementChild;
  if (!control || !hiddenRuns.has(control)) {
    control = el('button', 'btn transcript-hidden');
    hiddenRuns.set(control, []);
    const run = hiddenRuns.get(control);
    control.onclick = () => {
      const drawn = el('div');
      appendTranscriptEvents(drawn, run, resolveReference, projectRoot, null);
      control.replaceWith(...drawn.childNodes);
    };
    log.appendChild(control);
  }
  const run = hiddenRuns.get(control);
  run.push(ev);
  control.textContent = hiddenUnits(run) + ' hidden';
}

// appendTranscriptEvents maps canonical events to chat-style DOM: agent prose
// is the page's primary text (no box), user turns keep the composer bubble,
// tools/thinking collapse to chips, and system lines stay quiet. A view profile
// may fold or hide rows; with none, every row is drawn.
function appendTranscriptEvents(log, events, resolveReference, projectRoot, profile = null) {
  let lastTool = null, previousTool = '';
  for (const ev of events) {
    const display = followingDisplay(ev, profile, previousTool);
    if (display === HIDE) {
      appendHiddenRow(log, ev, resolveReference, projectRoot);
      if (ev.kind === 'tool_call') { lastTool = null; previousTool = 'hidden'; }
      if (ev.kind === 'tool_result') previousTool = '';
      continue;
    }
    previousTool = ev.kind === 'tool_call' ? 'shown' : '';
    // A thought's duration renders where the thought sat: on the block that
    // followed a signature-only thought, or on the thought itself when the
    // vendor supplied its summary. The daemon omits durations under the
    // configured minimum, so anything present is worth a line. Appended before
    // priorRow is captured so the decorator seam still keys on the row itself.
    if (Number(ev.thought_ms) > 0) {
      log.appendChild(el('div', 'thought-line', 'thought for ' + humanDuration(Math.round(Number(ev.thought_ms) / 1000))));
    }
    const priorRow = log.lastElementChild;
    if (display === COLLAPSE && ROW_CHIP_LABELS[ev.kind]) {
      log.appendChild(rowChip(ev));
    } else switch (ev.kind) {
      case 'user': {
        const m = el('div', 'msg user');
        if (ev.ts) m.appendChild(el('div', 'hd', 'You · ' + fmtTime(ev.ts)));
        // A reference a HUMAN typed is a reference too (§3), so user turns are
        // linkified as well. linkify escapes before it injects anything — this
        // must never become a bare innerHTML of transcript text (impl-plan R1).
        const bubble = el('div', 'bubble');
        bubble.innerHTML = linkify(ev.text || '', resolveReference);
        m.appendChild(bubble);
        log.appendChild(m);
        break;
      }
      case 'assistant': {
        const m = el('div', 'msg agent');
        m.appendChild(mkMark()); // our checkpoint mark — never a vendor glyph (chat.js does the same)
        const md = el('div', 'md');
        md.innerHTML = mdToHtml(ev.text || '', resolveReference);
        m.appendChild(md);
        log.appendChild(m);
        break;
      }
      case 'thinking': {
        if (!hasRenderableText(ev.text)) break;
        const det = document.createElement('details'); det.className = 'thinkchip';
        det.appendChild(el('summary', '', 'thinking'));
        det.appendChild(lazyBody(ev.text || '', ev));
        log.appendChild(det);
        break;
      }
      case 'tool_call': {
        const det = document.createElement('details'); det.className = 'toolchip';
        const sum = el('summary');
        sum.append('⚙ ');
        sum.appendChild(el('span', 'tname', ev.name || 'tool'));
        // The collapsed summary is what you SEE without expanding, and in an
        // agent transcript it is usually the file being read or written — so it
        // has to be linkified too, not just the body.
        const peek = el('span', 'tpeek');
        peek.innerHTML = ' ' + linkify(toolPeek(ev.text, projectRoot), resolveReference);
        sum.appendChild(peek);
        det.appendChild(sum);
        det.appendChild(lazyBody(ev.text || '', ev));
        log.appendChild(det);
        lastTool = det;
        break;
      }
      case 'tool_result': {
        if (lastTool) {
          const body = lastTool.querySelector('.tbody');
          body.dataset.raw = (body.dataset.raw || '') + RESULT_SEP + (ev.text || '');
          // Last resort for the peek: a Write whose `content` precedes
          // `file_path` in the JSON has its path clipped away entirely, so the
          // arguments genuinely do not contain it — but the RESULT names the
          // file it wrote. Recover the label from there rather than leaving the
          // chip showing a JSON brace.
          relabelPeekFromResult(lastTool, ev.text || '', projectRoot, resolveReference);
          paintBody(body); // no-op while collapsed; refreshes if already open
          lastTool = null;
        } else {
          const line = el('div', 'sysline');
          line.innerHTML = '⚙ result: ' + linkify((ev.text || '').slice(0, 200), resolveReference);
          log.appendChild(line);
        }
        break;
      }
      case 'summary':
        log.appendChild(el('div', 'sysline', '§ ' + (ev.text || '')));
        break;
      case 'context': {
        const line = el('div', 'sysline context-line');
        line.append(el('span', 'rowchip-label', 'context · ' + (ev.name || '') + ' '), ev.text || '');
        log.appendChild(line);
        break;
      }
      default: { // system + anything else stays quiet but inspectable
        const det = document.createElement('details'); det.className = 'thinkchip';
        det.appendChild(el('summary', '', 'system · ' + (ev.name || '')));
        det.appendChild(el('div', 'tbody', ev.text || ''));
        log.appendChild(det);
      }
    }
    // Row-decorator seam (GUI design §4): the ONE extension point for per-turn
    // annotations across all three transcript surfaces. Runs once per freshly
    // appended row; a merged tool_result adds no row, so nothing re-fires.
    const rowEl = log.lastElementChild;
    if (rowEl && rowEl !== priorRow) {
      applyTranscriptDecorators(ev, rowEl, { kind: ev.kind, projectRoot, resolveReference });
    }
  }
}

function renderEvent(ev) {
  const wrap = el('div', 'ev ' + ev.kind);
  const label = { user: 'You', assistant: 'Agent', tool_call: '⚙ ' + (ev.name || 'tool'),
    tool_result: '⚙ result', thinking: 'thinking', summary: 'summary', system: 'system · ' + (ev.name || '') }[ev.kind] || ev.kind;
  wrap.appendChild(el('div', 'hd', label + (ev.ts ? ' · ' + fmtTime(ev.ts) : '')));
  if (ev.kind === 'tool_call' || ev.kind === 'tool_result' || ev.kind === 'thinking') {
    const det = document.createElement('details');
    const sum = el('summary', '', (ev.text || '').slice(0, 110));
    det.appendChild(sum);
    det.appendChild(el('div', 'body', ev.text || ''));
    wrap.appendChild(det);
  } else {
    wrap.appendChild(el('div', 'body', ev.text || ''));
  }
  return wrap;
}

/* ---------- handoff composer (the product verb) ----------
   `d` needs only { runtime, id }; pass the HARVEST id rather than a resume handle
   (LoadSession accepts either today, but only the harvest id is guaranteed to
   address the record). `liveTurns` is how many turns the caller
   sent in this console since the session was opened; they are not in the harvest
   the extract is generated from, so they are declared rather than dropped
   silently (INV-21). */
async function renderHandoffComposer(d, liveTurns, target) {
  const main = $('#main');
  await withState(main, mkLoader('Generating handoff from ' + d.runtime + ' session…'),
    () => api(`/api/handoff/generate?runtime=${d.runtime}&id=${encodeURIComponent(d.id)}`),
    draft => {
      main.innerHTML = '';
      main.appendChild(el('h2', '', 'Continue in… — handoff composer'));
      const sub = el('div', 'sub');
      sub.append('Source: ', el('span', 'chip ' + d.runtime, d.runtime), ' ' + draft.session_ref +
        ' — mechanical extract with [runtime/session#seq] anchors. Edit before publishing; the human edit IS the quality gate.');
      main.appendChild(sub);

      // Coverage: state the boundary of what the extract can contain. The
      // generator reads the harvested record, so turns sent in this console
      // since the session was opened are not in it.
      const cov = el('div', 'banner');
      if (liveTurns > 0) {
        cov.style.borderLeftColor = 'var(--warn)';
        cov.textContent = 'Coverage: generated from the harvested session record. '
          + liveTurns + ' turn(s) you sent in this console are not yet harvested and are NOT included below — '
          + 'paste anything you still need before publishing.';
      } else {
        cov.textContent = 'Coverage: generated from the harvested session record.';
      }
      main.appendChild(cov);

      const back = el('button', 'btn', '◂ Back to session');
      back.onclick = () => openSession(d, null);
      main.appendChild(back);

      const ta = el('textarea');
      ta.value = draft.markdown;
      ta.style.cssText = 'width:100%;max-width:900px;height:46vh;margin:12px 0;font-family:var(--mono);font-size:12px;line-height:1.5;';
      main.appendChild(ta);

      const row1 = el('div', 'row');
      const dir = el('input'); dir.value = draft.suggested_dir; dir.style.width = '420px';
      row1.append(lblWrap('publish into directory', dir));
      main.appendChild(row1);

      const row2 = el('div', 'row');
      const mk = (label, checked, title) => {
        const c = el('input'); c.type = 'checkbox'; c.checked = checked; c.title = title || '';
        const l = el('label', '', ' ' + label);
        row2.append(c, l);
        return c;
      };
      const cbIgnore = mk('git-ignore .crossing-guard/', true, 'Governance default: keep the handoff out of git history');
      const cbClaude = mk('CLAUDE.md block', true, 'Claude Code auto-loads CLAUDE.md — this is the proven crosscarry rail');
      const cbAgents = mk('AGENTS.md block', true, 'Codex auto-loads AGENTS.md');
      main.appendChild(row2);
      main.appendChild(el('div', 'banner',
        'Governance: .crossing-guard/handoff.md is git-ignored by default, but CLAUDE.md / AGENTS.md blocks are NOT — a committed handoff is effectively undeletable (git history). Review content before committing.'));

      const pub = el('button', 'btn primary', 'Publish handoff');
      const out = el('div'); out.style.marginTop = '12px';
      pub.onclick = async () => {
        pub.disabled = true; pub.textContent = 'Publishing…';
        out.innerHTML = '';
        try {
          const res = await api('/api/handoff/publish', { method: 'POST', body: JSON.stringify({
            dir: dir.value.trim(), markdown: ta.value,
            gitignore: cbIgnore.checked, write_claude: cbClaude.checked, write_agents: cbAgents.checked,
          })});
          const okBox = el('div', 'banner'); okBox.style.borderLeftColor = 'var(--ok)';
          okBox.append('Published ✓', document.createElement('br'));
          for (const p of res.written) { okBox.append('· ' + p, document.createElement('br')); }
          out.appendChild(okBox);
          for (const n of (res.notes || [])) out.appendChild(el('div', 'banner', '⚠ ' + n));
          const next = el('div', 'banner');
          next.style.borderLeftColor = 'var(--accent2)';
          next.textContent = 'Next session picks it up automatically: cd ' + dir.value.trim() + ' && claude   (or codex) — both auto-load their context file.';
          out.appendChild(next);
          // console-design §6.2: the gate "generates a handoff you review before
          // it is written, and only then starts the target session." Publishing
          // and stopping stranded the user on this screen.
          if (target) {
            const go = el('button', 'btn primary', 'Start the ' + target + ' session →');
            go.style.marginTop = '10px';
            go.onclick = () => {
              setDefaults({ ...getDefaults(), runtime: target, cwd: dir.value.trim() });
              document.dispatchEvent(new CustomEvent('cg:continue', { detail: { fresh: true } }));
            };
            out.appendChild(go);
            out.appendChild(el('div', 'sub',
              'Starts a new ' + target + ' session in ' + dir.value.trim()
              + '. It carries the handoff you just published — not this session\'s history.'));
          }
        } catch (err) {
          out.appendChild(el('div', 'banner', '✖ ' + (err.message || err)));
        }
        pub.disabled = false; pub.textContent = 'Publish handoff';
      };
      main.appendChild(pub);
      main.appendChild(out);
    });
}

// the audit evidence for ONE session, rendered on the session detail (design gap
// named: clicking a finding must land you where the evidence is, not just the transcript).
export async function loadExposure(box, badge, runtime, id) {
  try {
    const e = await api('/api/audit/session?runtime=' + encodeURIComponent(runtime)
      + '&id=' + encodeURIComponent(id));
    if (box) renderExposure(box, e); // box may be null: badge-only fill (card lives in the panel provider)
    if (badge) {
      const n = (e.matched || []).length;
      if (e.watermark) {
        badge.textContent = '⚠ ' + e.watermark + (n ? ' · ' + n + ' rule' + (n > 1 ? 's' : '') : '');
        badge.style.color = 'var(--bad)';
      } else {
        badge.textContent = '○ no data-class (weak neg.)';
      }
      badge.style.cursor = 'default';
    }
  } catch {
    if (box) box.replaceChildren(el('div', 'evidence-state', 'Request failed'), el('div', 'sub', 'Data and secret evidence could not be loaded.'));
    if (badge) badge.textContent = '';
  }
}
export function renderExposure(box, e) {
  box.replaceChildren();
  const values = el('div', 'evidence-value-strip evidence-subview-values');
  const watermark = e.watermark || 'none';
  const matched = e.matched || [];
  const mark = el('span', 'evidence-value ' + (e.watermark ? CLASS_CHIP(e.watermark) : ''), watermark);
  mark.appendChild(el('small', '', e.watermark ? 'watermark' : 'watermark · weak negative'));
  const match = el('span', 'evidence-value', String(matched.length));
  match.appendChild(el('small', '', matched.length === 1 ? 'rule match' : 'rule matches'));
  values.append(mark, match); box.appendChild(values);
  if (e.matched && e.matched.length) {
    e.matched.forEach(m => {
      const line = el('div', 'evidence-exposure-row');
      line.append(el('span', 'chip ' + (SEV_CHIP[m.severity] || 'st-draft'), m.severity || '?'),
        el('code', '', m.rule || '?'), el('span', 'sub', m.message || ''));
      box.appendChild(line);
    });
  } else {
    box.appendChild(el('div', 'sub', 'No declared detector matched · weak negative'));
  }
  if (e.tags && e.tags.length) {
    const tw = el('div'); tw.style.cssText = 'margin-top:6px';
    tw.appendChild(el('span', 'sub', 'tags observed: '));
    e.tags.forEach(t => {
      const src = t.detector.startsWith('srcmap') || t.detector.startsWith('builtin');
      const c = el('span', 'chip ' + (src ? 'cl-observed' : 'st-stale'), t.key + '=' + t.value);
      c.title = t.detector + (t.evidence ? ' · ' + t.evidence : ''); c.style.marginRight = '4px';
      tw.appendChild(c);
    });
    box.appendChild(tw);
  }
}

configureViewList({ onSelect: () => { clearBar(); renderRail(); }, onEdit: loadViewIntoBar });
configureQueryBar({ renderRail });
configureRailSelection({ onTagged: repaintTaggedRows });
configureHeaderTags({ onSessionChanged: repaintTaggedRows });
api('/api/console/config').then(found => {
  const organization = found?.config?.session_organization || {};
  configureViewList({ settings: { viewsVisible: organization.views_visible, countRefreshMs: organization.count_refresh_ms } });
  configureHeaderTags({ recentTagToggles: organization.recent_tag_toggles });
}).catch(() => {});

export { openSession, renderSessionList, renderRail, renderTranscript, renderHandoffComposer };
