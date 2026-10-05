import { $, el, cpHeaders, api, fmtTime, escapeHtml, mdInline, mdToHtml, linkify, debounce, fillSelect, mkSelectKV, mkSelect, lblWrap, fmtTok, shortWhen, SEV_CHIP, WATER_ORDER, CLASS_CHIP, getDefaults, setDefaults } from "../core.js";
import { applyTheme, mkMark, mkLoader, mkSkeletons, withState, attachBottomPill, toggleHelp, authRecovery,
  captureTranscriptPosition, restoreTranscriptPosition } from "../ui.js";
import { S } from "../state.js";
import { showPaneHost, beginPaneDeckTransition, endPaneDeckTransition, revealPane, canRevealPane } from "../pane-host.js";
import * as refs from "../refs.js";
import { loadChatCapabilities, findChatCapability } from "../chat-capabilities.js";
import { openTurnSettings } from "../task/thinking-effort-history.js";
import { taskProjectionStore } from "../task/task-projection-store.js";
import { hasRenderableText } from "../task/task-event-semantics.js";
import { approvalProjectionStore } from "../approval/approval-projection-store.js";
import { renderSessionStatus, SessionAttentionStore, agentNote } from "../task/session-status.js";
import { mountSessionOrchestration } from "../orchestration/session-orchestration.js";
import { sessionActivityStore } from "../session/session-activity-store.js";
import { SessionLiveClient } from "../session/session-live-client.js";
import { SessionEventStore, humanDuration } from "../session/session-event-store.js";
import { createActivityLine } from "../session/activity-line.js";
import { changeEvidenceNote } from "../session/change-evidence-note.js";
import { nativeOpenControl, nativeOpenModel, openNative } from "../session/native-open.js";
import { ownerMessage } from "../appearance-api.js";
import { presentTranscriptIndexCoverage } from "../transcript-index-coverage.js";
import { applyTranscriptDecorators } from "../transcript-decorators.js";
import { HIDE, COLLAPSE, followingDisplay, selectProfile, rowPeek, rowLength, sizeLabel,
  hiddenActivityKind, hiddenActivityLabel, createCallPairing } from "../session/transcript-view.js";
import { activeQuery, selectedView, selectView } from "../session-organization/organization-state.js";
import { organizedRailUrl, organizedPageUrl, railStateKey, groupsAreRepositories, groupLabel, FILTER_TYPING_PAUSE_MS } from "../session-organization/view-group-source.js";
import { appendRowOrganization, repaintRowTags, rowTime } from "../session-organization/tag-chips.js";
import { mountViewList, configureViewList, refreshViewCounts, ensureViewsReady } from "../session-organization/view-list.js";
import { boardConfig, renderBoardNodes, paintBoardAttention, refreshBoard, boardBusy, boardNeedsControl } from "../session-organization/board.js";
// The board's card click opens the session through the session surface's
// one opener: sessions.js is the only openSession owner; board.js must not
// import it (cycle) — the event is the narrow seam (placement rule PO-13).
document.addEventListener('cg:board-open', e => {
  const t = e.detail;
  if (t && t.runtime && t.id) openSession(t);
});
// A board move is a tag write: other views' counts may have changed. The
// board repaints its own columns; the view list beside it is this surface's
// (board-move-refresh plan §2), as after a rail-side tag write.
document.addEventListener('cg:board-moved', () => { refreshViewCounts(); });
// A board is placed by what was observed, and nothing pushes a new
// observation to it: it reads again when the owner comes back to the page
// (board-observed-columns plan §2.3). No timer.
document.addEventListener('visibilitychange', () => {
  if (document.hidden) return;
  for (const board of document.querySelectorAll('#main .board')) refreshBoard(board);
});
// boardFits reports a centre wide enough for a board: two columns side by
// side. The column's width is the style's (--board-column-min), never a
// number here. Below it a board view is its grouped list in the rail. The
// room is measured from the centre's left edge to the window's right: a
// board takes the whole centre, and the panel a just-closed session left
// open beside it is about to go.
function boardFits() {
  const column = parseFloat(getComputedStyle(document.documentElement).getPropertyValue('--board-column-min'));
  const room = document.documentElement.clientWidth - $('#main').getBoundingClientRect().left;
  return !(column > 0) || room >= 2 * column;
}
let boardFitted = null;
window.addEventListener('resize', () => {
  const fits = boardFits();
  const crossed = boardFitted !== null && fits !== boardFitted;
  boardFitted = fits;
  // Only a board view with no session open changes with the width, and a
  // card in hand is never redrawn from under the pointer.
  if (!crossed || S.view !== 'sessions' || S.sel || boardBusy() || !boardConfig(selectedView())) return;
  renderSessionList();
});
import { handleBarInput, configureQueryBar, loadViewIntoBar, clearBar, queryNotesNode } from "../session-organization/query-bar.js";
import { handleRowSelectClick, rowSessionsForTagging, configureRailSelection, clearRailSelection } from "../session-organization/rail-selection.js";
import { openTagPopover } from "../session-organization/tag-popover.js";
import { mountHeaderTags, configureHeaderTags, openHeaderNote } from "../session-organization/header-tags.js";
import "../session-organization/tag-shortcut.js";
import { handOffSession } from "../handoff/handoff-view.js";
import { liveTurnsOf } from "../handoff/live-turns.js";
import { mountHandoffRail, configureHandoffRail } from "../handoff/handoff-rail.js";
import { mountContinues } from "../handoff/handoff-session.js";
import { contextRowName } from "../handoff/handoff-model.js";

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
  if (d.row) sessionEventStore.markLiveRow(d.row);
});
// The selected session installs these DOM integrations. Store state remains
// generation-scoped, so late events from a detached composer are inert.
let placeOwnedTurnBoundary = () => {}, applySessionEventEffect = () => {};
document.addEventListener('cg:owned-turn', event => {
  const d = event.detail || {};
  if (d.state === 'start') placeOwnedTurnBoundary(d.id, sessionEventStore.beginOwnedTurn(d.id));
  else if (d.state === 'end') applySessionEventEffect(sessionEventStore.endOwnedTurn(d.id));
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

// draftWords is the rail's phrasing of the quiet agent note.
function draftWords(ask) {
  const note = agentNote(ask);
  return note.charAt(0).toUpperCase() + note.slice(1);
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
  // A reply an agent proposed but did not send is a quiet fact: no dot, but
  // the row says so to a pointer and to a screen reader.
  const draft = !dot ? draftWords(status.ask) : '';
  slot.title = dot?.title || draft;
  const statusText = status.indicator.label || (status.execution !== 'idle' ? status.label : '');
  const label = [baseLabel, statusText, draft].filter(Boolean).join(' · ');
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
  for (const board of document.querySelectorAll('#main .board')) paintBoardAttention(board);
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

// buildNativeFold renders the compact collapsed "⧉ n native subagents" line a
// parent session may carry for vendor-observed subagent children (guardian,
// thread_spawn). Provenance is separate from buildAgentFold (caused); the child
// rows reuse buildSessionRow so selection/status painting stays one-owner.
function buildNativeFold(parent, fold) {
  const host = el('div', 'agent-fold native-fold');
  const rows = el('div', 'agent-fold-rows');
  const toggle = el('button', 'agent-fold-toggle', '⧉ ' + fold.length + ' native subagent' + (fold.length === 1 ? '' : 's'));
  toggle.type = 'button';
  toggle.title = 'Native subagent sessions this runtime reported under this parent. They are not peer work sessions; full detail lives in the session’s related-sessions strip.';
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
  const native = nativeOpenControl(s);
  if (native) m.appendChild(native);
  // agent_role is set only on fold children (G-5): agent sessions never
  // render as flat rail rows, so this label appears inside a parent's
  // expanded "\u2696 agents (n)" fold, naming the child's role.
  if (s.agent_role) m.appendChild(el('span', 'modeltag agent-session-tag', '\u2696 agent \u00b7 ' + s.agent_role));
  // native_role/native_kind mark native subagent children; distinct glyph so
  // the observed class is not mistaken for a caused agent.
  if (s.native_role) m.appendChild(el('span', 'modeltag agent-session-tag native-session-tag', '\u29c9 subagent \u00b7 ' + s.native_role));
  if (showProject) m.appendChild(el('span', '', projectLabel(s)));
  m.appendChild(el('span', '', fmtTime(rowTime(s))));
  if (s.model) {
    const mm = el('span', 'modeltag', s.model);
    mm.title = s.model + (s.turns ? ' · ' + s.turns + ' calls' : '');
    m.appendChild(mm);
  }
  if (s.activity_status === 'unknown') {
    const activity = el('span', 'modeltag', 'activity unknown');
    activity.title = 'This runtime does not expose exact per-session liveness evidence; unknown does not mean closed.';
    m.appendChild(activity);
  }
  // branch, commits, PRs: legacy summary fields with no writer since 2026-07-20
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
  const native = nativeOpenControl(s);
  if (native) meta.appendChild(native);
  const matchKind = el('span', 'modeltag search-hit-kind', s.kind === 'title' ? 'Title match' : 'Transcript match');
  matchKind.dataset.kind = s.kind || 'transcript';
  meta.appendChild(matchKind);
  if (s.project || s.cwd) meta.appendChild(el('span', '', projectLabel(s)));
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
  // board_view only rides the fetch when the ACTIVE query is the selected
  // view's own (not a bar filter): the board's placement is the board's
  // rendering, not a rail-wide behavior (the filter-bar interaction fix).
  const viewScoped = active && active.scope === 'view:' + (selectedView()?.id || '');
  const boardView = viewScoped && boardConfig(selectedView()) ? selectedView().id : '';
  const fetchRail = () => options.prefetched ? Promise.resolve(options.prefetched)
    : api(organizedRailUrl(active, 500, boardView) || '/api/sessions?view=rail&repository_limit=500');
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
    // Handoffs: Received and Sent as rail rows (team rest-of-release plan §8.2).
    mountHandoffRail(side);
    const repositories = data.repositories || [];
    // The board (sessions-board plan; placement rule PO-13): a board view
    // with NO session open renders the board IN #main — the wide center
    // surface, full width (no split, no right panel for now). With a session
    // open, the session keeps the center and the rail renders its grouped
    // list. A bar filter takes you to the rail (scope rule PO-11): the board
    // is the view's rendering, only when the active query IS that view.
    const view = selectedView();
    const board = boardConfig(view);
    boardFitted = boardFits();
    if (board && active && active.scope === 'view:' + view.id && !S.sel && boardFitted) {
      renderBoardCenter(view, board, data);
      // The board holds the centre only while no session does: opening one
      // hands the centre over, and the rail then lists the view's groups.
      revealRailSelection = () => { renderRail(); };
      return;
    }
    if (!repositories.length) {
      side.appendChild(railEmptyState(active, viewScoped ? data.query_notes : null));
      // An empty list is not the whole story when the store could not be read.
      const evidenceNote = changeEvidenceNote(data.change_evidence_problem);
      if (evidenceNote) side.appendChild(el('div', 'empty', evidenceNote));
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
        // Plan D3: a repository containing presence-unknown sessions defaults
        // to All on first use, because Open would hide them silently. The first
        // useful group opens in All; other such groups remain collapsed but use
        // All when expanded. Repositories whose sessions all participate in
        // exact presence keep the existing default.
        const hasUnknown = group.presence_unknown_total > 0;
        const firstUseful = index === 0 && (data.activity?.status === 'available' ? group.open_total > 0 || hasUnknown : true);
        railState[stateKey] = active ? { mode: 'all', offset: 0 }
          : { mode: legacyOpen || firstUseful ? (hasUnknown ? 'all' : (group.open_total > 0 ? 'open' : 'all')) : 'closed', offset: 0 };
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
          const nativeFold = page.native_children?.[s.id] || [];
          if (nativeFold.length) body.appendChild(buildNativeFold(s, nativeFold));
        }
        // The current selection may live inside a fold (an agent or native
        // subagent session opened from the strip) — that is on this page.
        const selectionInFold = currentSession && (
          Object.values(page.agent_children || {}).concat(Object.values(page.native_children || {}))
            .some(fold => fold.some(child => child.runtime === currentSession.runtime && child.id === currentSession.id))
        );
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
          } else if (currentSession.lineage_kind) {
            body.appendChild(el('div', 'selected-outside-label',
              'Selected native subagent · folded under its parent on another page'));
          } else {
            body.appendChild(el('div', 'selected-outside-label', 'Selected session · outside this page'));
            body.appendChild(buildSessionRow(currentSession, false));
          }
        }
        if (!page.total) {
          if (pref.mode === 'open' && group.presence_unknown_total > 0) {
            const notice = el('div', 'empty open-unavailable',
              group.presence_unknown_total + ' session' + (group.presence_unknown_total === 1 ? '' : 's') +
              ' in this repository may be active but cannot be proved open (shared runtime store).');
            const showAll = el('button', 'btn', 'Show all sessions');
            showAll.onclick = () => setMode('all', disclosure);
            notice.append(document.createElement('br'), showAll);
            body.appendChild(notice);
          } else {
            body.appendChild(el('div', 'empty', pref.mode === 'open'
              ? 'No session in this repository is open right now.'
              : (active ? 'No sessions match.' : 'No sessions in this repository.')));
          }
        } else if (pref.mode === 'open' && group.presence_unknown_total > 0) {
          // Even with open results, show the exclusion count and Show All.
          const notice = el('div', 'rail-exclusion-note',
            '+ ' + group.presence_unknown_total + ' may be active (not shown in Open)');
          const showAll = el('button', 'btn btn-small', 'Show all');
          showAll.onclick = () => setMode('all', disclosure);
          notice.append(' ', showAll);
          body.appendChild(notice);
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
        const url = organizedPageUrl(active, { key, mode: pref.mode, offset: pref.offset, limit: SESSION_PAGE_SIZE, selected, boardView })
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
    // The reason names a store path: text, never markup.
    const evidenceNote = changeEvidenceNote(data.change_evidence_problem);
    if (options.landing && evidenceNote) $('#main').appendChild(el('div', 'sub', evidenceNote));
  }, () => railGeneration === sessionRailRenderGeneration && S.view === 'sessions');
}

// renderBoardCenter draws a board view in the centre: its name, the notes on
// its query (board-view-query-correctness plan), and the board itself.
function renderBoardCenter(view, board, data) {
  const center = $('#main');
  center.replaceChildren();
  center.classList.remove('split');
  const head = center.appendChild(el('div', 'board-headrow'));
  head.appendChild(el('h2', 'board-heading', view.name || 'Board'));
  const notes = queryNotesNode(data.query_notes);
  if (notes) center.appendChild(notes);
  const host = center.appendChild(renderBoardNodes(view, board, data.repositories || [], { status: statusForSession, statusDot: makeStatusDot }));
  head.appendChild(boardNeedsControl(host));
  const refresh = head.appendChild(el('button', 'board-refresh', 'Refresh'));
  refresh.type = 'button';
  refresh.title = 'Read the board again';
  refresh.onclick = () => refreshBoard(host);
  const boardNote = changeEvidenceNote(data.change_evidence_problem);
  if (boardNote) center.appendChild(el('div', 'sub', boardNote));
}

// railEmptyState is the rail with nothing to list. A view's query that can
// select nothing says why, under the line.
function railEmptyState(active, notes) {
  const empty = el('div', 'empty', active ? 'No sessions match.'
    : 'No sessions found. Sessions appear here after you use Claude Code or Codex on this machine.');
  const noted = queryNotesNode(notes);
  if (noted) empty.appendChild(noted);
  return empty;
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
}, FILTER_TYPING_PAUSE_MS);

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
    return { selection: config?.config?.transcript || {}, profiles: modules?.profiles || [], stateToken: config?.state_token || '' };
  } catch {
    return { selection: {}, profiles: [] };
  }
}

// projectLabel is a row's short place: the last two folders of the working
// directory, or the unresolved project label as-is (never split as a path).
function projectLabel(s) {
  return s.cwd ? String(s.cwd).split('/').slice(-2).join('/') : String(s.project || '');
}

// displayPath shows a working directory with the home folder as ~. An
// unresolved project (no cwd) is a label, shown as-is and never used as a path.
function displayPath(d) {
  if (d.cwd) return String(d.cwd).replace(/^\/(Users|home)\/[^/]+(?=\/|$)/, '~');
  return String(d.project || '');
}

// mountSessionMenu is the header's ⋯ menu: actions that are one click away but
// never take header space.
function mountSessionMenu(host, items) {
  const wrap = el('div', 'session-menu');
  const button = el('button', 'btn ghost session-menu-button', '⋯');
  button.type = 'button';
  button.setAttribute('aria-label', 'Session actions');
  button.setAttribute('aria-haspopup', 'menu');
  const menu = el('div', 'session-menu-list hidden');
  menu.setAttribute('role', 'menu');
  const close = () => { menu.classList.add('hidden'); document.removeEventListener('click', outside, true); };
  const outside = event => { if (!wrap.contains(event.target)) close(); };
  // Items are built when the menu opens, so one that cannot act right now
  // (no workspace to reveal, no session record to note) is simply not offered.
  const fill = () => {
    menu.replaceChildren();
    for (const item of items.filter(candidate => !candidate.available || candidate.available())) {
      const entry = el('button', 'session-menu-item', item.label);
      entry.type = 'button';
      entry.setAttribute('role', 'menuitem');
      entry.onclick = () => { close(); item.run(); };
      menu.appendChild(entry);
    }
  };
  button.onclick = () => {
    const opening = menu.classList.contains('hidden');
    if (opening) fill();
    menu.classList.toggle('hidden', !opening);
    if (opening) document.addEventListener('click', outside, true);
  };
  wrap.append(button, menu);
  host.appendChild(wrap);
}

// mountPinnedNotes keeps the pinned notes (/api/notes) one menu click away:
// never as banners in the conversation (owner decision O-3a).
function mountPinnedNotes(head, d, notes) {
  const box = el('div', 'pinned-notes hidden');
  const paint = () => paintPinnedNotes(box, d, notes, paint);
  paint();
  head.appendChild(box);
  return () => { box.classList.toggle('hidden'); box.querySelector('input')?.focus(); };
}

function paintPinnedNotes(box, d, notes, repaint) {
  box.replaceChildren();
  for (const note of notes) box.appendChild(el('div', 'pinned-note', note.text + ' · ' + fmtTime(note.created_at)));
  const row = el('div', 'row');
  const input = el('input'); input.placeholder = 'Pin a note to this session…'; input.style.flex = '1';
  const pin = el('button', 'btn', 'Pin');
  const save = () => pinNote(d, input.value, notes).then(saved => { if (saved) repaint(); });
  pin.onclick = save;
  input.onkeydown = event => { if (event.key === 'Enter') save(); };
  row.append(input, pin);
  box.appendChild(row);
}

async function pinNote(d, text, notes) {
  if (!text.trim()) return false;
  await api('/api/notes', { method: 'POST', body: JSON.stringify({ target: d.runtime + '/' + d.id, text }) });
  notes.push({ text, created_at: new Date().toISOString() });
  return true;
}

// mountTranscriptToolbar holds the view switch, its "Make default" action and
// the count of rows the view hides. A choice applies to this view only
// (transcript-view-profiles plan D-1); "Make default" is the explicit save.
function mountTranscriptToolbar(host, view, d, current, onChange) {
  const bar = el('div', 'transcript-toolbar');
  const hiddenTotal = el('span', 'transcript-hidden-total');
  bar.appendChild(hiddenTotal);
  host.appendChild(bar);
  if (!view.profiles.length) return { setHidden() {} };
  const select = el('select', 'transcript-view-switch');
  select.setAttribute('aria-label', 'Transcript view');
  for (const profile of view.profiles) {
    const option = el('option', '', profile.name || profile.id);
    option.value = profile.id;
    option.title = profile.description || '';
    select.appendChild(option);
  }
  select.value = current?.id || '';
  const role = d.orchestration_role || '';
  const opensWith = () => (role && view.selection.role_profiles?.[role]) || view.selection.default_profile || '';
  const makeDefault = el('button', 'btn ghost transcript-make-default', 'Make default');
  makeDefault.type = 'button';
  makeDefault.title = role ? 'Open ' + role + ' sessions with this view' : 'Open sessions with this view';
  const syncDefault = () => { makeDefault.hidden = !select.value || select.value === opensWith(); };
  makeDefault.onclick = async () => {
    const body = { state_token: view.stateToken || '' };
    if (role) body.role_profiles = { [role]: select.value }; else body.default_profile = select.value;
    makeDefault.disabled = true;
    try {
      const saved = await api('/api/console/config/transcript', { method: 'PUT', body: JSON.stringify(body) });
      view.selection = saved?.config?.transcript || view.selection;
      view.stateToken = saved?.state_token || '';
    } catch (error) {
      hiddenTotal.textContent = ownerMessage(error);
    } finally { makeDefault.disabled = false; syncDefault(); }
  };
  select.onchange = () => { syncDefault(); onChange(view.profiles.find(profile => profile.id === select.value) || null); };
  syncDefault();
  bar.append(select, makeDefault);
  return {
    setHidden(count) { hiddenTotal.textContent = count ? count + ' hidden' : ''; },
  };
}

function countHiddenRows(log) {
  return (transcriptRenderStates.get(log)?.groups || []).reduce((total, group) =>
    total + (group.details.open ? 0 : group.items.length), 0);
}

function renderSessionDetail(d, allNotes, referenceScope = refs.forProject(d.cwd || ''), referenceLoad = null,
  transcriptView = { selection: {}, profiles: [] }) {
  clearSelectedSessionBindings();
  const main = $('#main');
  currentSession = d;
  main.innerHTML = '';
  main.classList.add('split'); // header + inner scroller; render() resets this
  const pane = el('div', 'detailscroll'); // everything below the header scrolls in here
  // The header is two lines (session-view plan §A1): who this session is, and
  // where it is. Everything else is one click away, in the ⋯ menu or the
  // workspace panel; what the session is doing lives above the reply box.
  const head = el('div', 'stickyhead');
  const line1 = el('div', 'session-line1');
  const h2 = el('h2', '', d.title || d.id);
  h2.title = d.title || d.id;
  const facts = el('div', 'session-facts');
  facts.appendChild(el('span', 'chip ' + d.runtime, d.runtime));
  if (d.usage?.model) {
    const model = el('span', 'modeltag', d.usage.model);
    model.title = d.usage.model + ' · ' + d.usage.turns + ' calls · ' + Math.round((d.usage.cache_hit_rate || 0) * 100) + '% cache hit';
    facts.appendChild(model);
  }
  if (d.branch) facts.appendChild(el('span', 'modeltag', '⎇ ' + d.branch));
  for (const pr of (d.prs || []).slice(0, 3)) facts.appendChild(el('span', 'prtag', pr));
  if (d.usage?.context > 0) {
    const context = el('span', 'ctxbadge', fmtTok(d.usage.context) + ' ctx');
    context.title = d.usage.context.toLocaleString() + ' tokens at the last turn';
    facts.appendChild(context);
  }
  // A data-exposure finding earns one chip; a weak negative shows nothing here.
  const exposure = el('button', 'chip st-disputed session-exposure');
  exposure.type = 'button';
  exposure.hidden = true;
  exposure.onclick = () => { if (!revealPane('session.impact')) exposure.hidden = true; };
  facts.appendChild(exposure);
  line1.append(h2, facts, el('span', 'session-line-spacer'));
  head.appendChild(line1);
  const line2 = el('div', 'session-line2');
  const place = el('span', 'session-path', displayPath(d));
  place.title = d.cwd || d.project || '';
  line2.appendChild(place);
  const tagHost = mountHeaderTags(head, d);
  line2.appendChild(tagHost);
  head.appendChild(line2);
  const details = el('button', 'btn ghost session-details', 'details ›');
  details.type = 'button';
  details.onclick = () => revealPane('session.change');
  // "details ›" shows only while the workspace can show the session's evidence.
  const syncDetails = () => {
    if (!details.isConnected && details.dataset.mounted) { document.removeEventListener('cg:module-state', syncDetails); return; }
    details.hidden = !canRevealPane('session.change');
  };
  details.dataset.mounted = '1';
  document.addEventListener('cg:module-state', syncDetails);
  syncDetails();
  line2.appendChild(details);
  // A session opened for a handoff says what it continues (plan §6.2, §14 Q22).
  void mountContinues(line2, d, tagHost);
  const notes = allNotes.filter(n => n.target.startsWith(d.runtime + '/' + d.id));
  const togglePinned = mountPinnedNotes(head, d, notes);
  mountSessionMenu(line1, [
    { label: 'Note…', run: openHeaderNote, available: () => tagHost.dataset.placeable !== 'false' },
    { label: notes.length ? 'Pinned notes · ' + notes.length : 'Pin a note…', run: togglePinned },
    { label: 'Session details', run: () => revealPane('session.change'), available: () => canRevealPane('session.change') },
    { label: 'Turn settings', run: () => openTurnSettings(d) },
    { label: 'Hand off…', run: () => handOffSession(d, liveTurnsOf(d)) },
    { label: 'Copy session id', run: () => navigator.clipboard?.writeText(String(d.id || '')) },
    { label: nativeOpenModel(d)?.label || '', run: () => openNative(d), available: () => nativeOpenModel(d) !== null },
  ]);
  main.appendChild(head);
  main.appendChild(pane);
  loadExposure(null, exposure, d.runtime, d.id);
  const reviewGeneration = sessionOpenGeneration;
  const referenceStatus = el('div', 'session-reference-status sub', 'Indexing project references…');
  referenceStatus.setAttribute('role', 'status');
  const log = el('div', 'translog');
  log.effortSession = { runtime: d.runtime, id: d.id, resume_id: d.resume_id || d.id };
  const transcriptKeyPrefix = 'session:' + sessionOpenGeneration + ':';
  sessionEventStore.reset(d.events || []);
  // The events drawn so far, live deltas included, so a view switch redraws
  // the same conversation under another module.
  const transcript = { events: [...sessionEventStore.events],
    profile: selectProfile(transcriptView.profiles, transcriptView.selection, d.orchestration_role) };
  // The view switch rides header line 2, so it is reachable at any scroll depth.
  const toolbar = mountTranscriptToolbar(line2, transcriptView, d, transcript.profile, profile => {
    transcript.profile = profile;
    renderTranscript(log, transcript.events, referenceScope.resolve, referenceScope.root,
      { profile, keyPrefix: transcriptKeyPrefix });
    toolbar.setHidden(countHiddenRows(log));
  });
  line2.appendChild(details);
  pane.appendChild(referenceStatus);
  // transcript — SAME renderer family as the chat tab (one conversation
  // language everywhere), and opens at the END like every chat you've ever
  // used: the most recent exchange is why you opened it.
  renderTranscript(log, transcript.events, referenceScope.resolve, referenceScope.root,
    { profile: transcript.profile, keyPrefix: transcriptKeyPrefix });
  toolbar.setHidden(countHiddenRows(log));
  const onHiddenChanged = () => { if (log.isConnected) toolbar.setHidden(countHiddenRows(log)); else document.removeEventListener('cg:transcript-hidden-changed', onHiddenChanged); };
  document.addEventListener('cg:transcript-hidden-changed', onHiddenChanged);
  pane.appendChild(log);
  hydrateSessionReferences(pane, referenceStatus, referenceScope,
    referenceLoad || referenceScope.start(), reviewGeneration);
  // The foot is pinned to the bottom of the scroller in every state — resume
  // pending, composer mounted, or read-only — and the activity line is always
  // its first row (session-view plan §A2).
  const foot = el('div', 'session-foot');
  const activity = createActivityLine({ ledger: sessionAttentionStore, ageTickSeconds: sessionEventStore.ageTickSeconds || 15 });
  foot.appendChild(activity.root);
  pane.appendChild(foot);
  mountSessionOrchestration(activity.linkHost, String(d.runtime || ''), String(d.id || ''),
    { cwd: String(d.cwd || ''), cwd_key: String(d.cwd_key || ''), repository: String(d.repository_key || ''),
      meta_id: String(d.meta_id || ''), thread_id: String(d.thread_id || ''), identities: d.identities });
  // Natural-session live stream (plan B-GUI): ONE stream for the selected
  // session; deltas append through the SAME transcript renderer so rows are
  // identical to the static view.
  let tailBoundary = null, updateTranscriptPill = () => {}, recoveryPending = false;
  placeOwnedTurnBoundary = (_id, effect) => {
    if (!log.isConnected || currentSession !== d || effect?.type !== 'boundary' || tailBoundary) return;
    tailBoundary = document.createComment('owned-turn-tail');
    log.appendChild(tailBoundary);
  };
  const recoverCanonicalTail = async () => {
    if (recoveryPending || !log.isConnected || currentSession !== d) return;
    recoveryPending = true;
    try {
      const snapshot = await api(`/api/session?runtime=${d.runtime}&id=${encodeURIComponent(d.id)}`);
      if (log.isConnected && currentSession === d) {
        const effect = sessionEventStore.acceptSnapshot(snapshot.events || []);
        if (effect.type === 'pending') activity.setLiveUnavailable(true);
        applySessionEventEffect(effect);
      }
    } catch {
      activity.setLiveUnavailable(true);
    } finally { recoveryPending = false; }
  };
  applySessionEventEffect = effect => {
    if (!log.isConnected || currentSession !== d || !effect || ['noop', 'pending', 'boundary'].includes(effect.type)) return;
    if (effect.type === 'refresh') { void recoverCanonicalTail(); return; }
    const scroller = log.closest('.detailscroll');
    const position = captureTranscriptPosition(scroller, log);
    if (effect.type === 'append') {
      transcript.events.push(...effect.events);
      appendTranscriptEvents(log, effect.events, referenceScope.resolve, referenceScope.root,
        transcript.profile, transcriptKeyPrefix, true);
    } else if (effect.type === 'replace-tail') {
      transcript.events.splice(effect.boundary, transcript.events.length - effect.boundary, ...effect.events);
      if (tailBoundary?.parentNode === log) {
        let node = tailBoundary.nextSibling;
        while (node) { const next = node.nextSibling; node.remove(); node = next; }
        tailBoundary.remove();
      }
      tailBoundary = null;
      pruneTranscriptRenderState(log);
      appendTranscriptEvents(log, effect.events, referenceScope.resolve, referenceScope.root,
        transcript.profile, transcriptKeyPrefix, true);
    }
    toolbar.setHidden(countHiddenRows(log));
    restoreTranscriptPosition(scroller, log, position);
    updateTranscriptPill();
  };
  sessionLiveClient.subscribe(String(d.runtime || ''), String(d.id || ''), {
    onEvents: events => {
      if (!log.isConnected || currentSession !== d) return;
      applySessionEventEffect(sessionEventStore.accept(events));
    },
    onTurnState: state => {
      if (!log.isConnected || currentSession !== d) return;
      const wasRunning = sessionEventStore.turnState?.execution === 'running';
      sessionEventStore.setTurnState(state);
      activity.setLive(state);
      selectedStatusRefresh();
      // A turn boundary is the Diff pane's refresh trigger (design §3.4, R14):
      // the session stopped running, so its edits and the checkout may have moved.
      if (wasRunning && state?.execution !== 'running') {
        document.dispatchEvent(new CustomEvent('cg:session-turn-state', { detail: { runtime: d.runtime, id: d.thread_id || d.id, execution: state?.execution } }));
      }
    },
    onIdentity: payload => {
      if (!log.isConnected || currentSession !== d) return;
      if (payload && payload.following === false) activity.setFollowing(false);
    },
    onGovernance: () => {
      // The strip and action surfaces refresh off the same channel; the
      // projections themselves live in their own stores.
      document.dispatchEvent(new CustomEvent('cg:session-activity-delta'));
    },
    onStatus: (status, detail) => {
      if (!log.isConnected || currentSession !== d) return;
      activity.setLiveUnavailable(status !== 'live');
      if (status === 'live' && detail && typeof detail.age_tick_seconds === 'number') {
        sessionEventStore.ageTickSeconds = detail.age_tick_seconds;
        activity.setAgeTick(detail.age_tick_seconds);
      }
    },
  });
  // A task's output belongs in the conversation at its own moment, or on the
  // surfaces about tasks — never pinned beneath the conversation as a second
  // record with a caption explaining why we could not join the two.
  selectedTaskRefresh = () => {};
  let maybeAcknowledge = () => {};
  // The rail item feeds the line before the first live frame and while live is
  // unavailable, and is its only source of presence and freshness.
  selectedStatusRefresh = () => {
    if (!foot.isConnected) return;
    activity.setRail(sessionActivityStore.activity(d.runtime, d.id));
    requestAnimationFrame(() => maybeAcknowledge());
  };
  const latestEdgeVisible = () => pane.isConnected
    && document.visibilityState === 'visible' && document.hasFocus()
    && pane.scrollTop + pane.clientHeight >= pane.scrollHeight - 60;
  // The ask the line is showing (id, since when) and the reader's last
  // pointer or key press in the console.
  let askShown = { id: 0, at: 0 };
  let lastInteraction = 0;
  // Acknowledge exactly what the line is showing: its attention id and source.
  maybeAcknowledge = () => {
    if (!latestEdgeVisible()) return;
    const current = activity.current();
    const status = current.source;
    if (!status) return;
    // The line showed an agent's ask: acknowledging it acknowledges every
    // ask up to that id, and the marker beneath shows next (plan §6.2). An
    // ask is acknowledged only after the reader acted in the console while
    // it was showing — a pointer or key press — never by merely rendering
    // in a focused window (independent red-team J5).
    if (current.kind === 'ask') {
      if (askShown.id !== status.ask.id) askShown = { id: status.ask.id, at: Date.now() };
      if (lastInteraction <= askShown.at) return;
      sessionAttentionStore.acknowledge(d.runtime, d.id, d.resume_id || d.id, 'agent', status.ask.id);
      return;
    }
    if (!['new_result', 'new_failure', 'interrupted'].includes(status.attention)) return;
    sessionAttentionStore.acknowledge(d.runtime, d.id, d.resume_id || d.id,
      status.attention_source, status.attention_id);
  };
  const onAttentionOpportunity = () => requestAnimationFrame(maybeAcknowledge);
  const onInteraction = () => { lastInteraction = Date.now(); onAttentionOpportunity(); };
  document.addEventListener('pointerdown', onInteraction, true);
  document.addEventListener('keydown', onInteraction, true);
  const stopAttention = sessionAttentionStore.subscribe?.(() => activity.setRail(sessionActivityStore.activity(d.runtime, d.id)));
  pane.addEventListener('scroll', onAttentionOpportunity, { passive: true });
  window.addEventListener('focus', onAttentionOpportunity);
  document.addEventListener('visibilitychange', onAttentionOpportunity);
  selectedAttentionCleanup = () => {
    pane.removeEventListener('scroll', onAttentionOpportunity);
    window.removeEventListener('focus', onAttentionOpportunity);
    document.removeEventListener('visibilitychange', onAttentionOpportunity);
    document.removeEventListener('pointerdown', onInteraction, true);
    document.removeEventListener('keydown', onInteraction, true);
    if (typeof stopAttention === 'function') stopAttention();
    activity.dispose();
  };
  selectedTaskRefresh();
  selectedStatusRefresh();
  updateTranscriptPill = attachBottomPill(pane, 'pill-session'); // item 3 — pill tracks the inner scroller
  // Capability discovery and command dispatch come from the same registered
  // adapter. Session presentation never owns a runtime allowlist or ID parser.
  const capabilityPending = el('div', 'sub', 'Checking whether this harness can resume…');
  foot.appendChild(capabilityPending);
  const renderReadOnly = reason => {
    if (!log.isConnected) return;
    capabilityPending.remove();
    const dead = el('div', 'composer-dead');
    const ta = el('textarea');
    ta.placeholder = 'Read-only — no resume path for runtime "' + d.runtime + '"';
    ta.disabled = true; ta.rows = 1;
    dead.append(ta, el('div', 'sub', reason));
    foot.appendChild(dead);
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
        detail: { container: foot, inline: true, log, activity },
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
    const full = await fullTranscriptEvent(d, body.dataset.seq);
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

function fullTranscriptEvent(session, seq) {
  return api('/api/session/event?runtime=' + encodeURIComponent(session.runtime) +
    '&id=' + encodeURIComponent(session.id) + '&seq=' + encodeURIComponent(seq));
}

// One delegated listener rather than one per chip: a session can hold hundreds.
document.addEventListener('toggle', e => {
  const det = e.target;
  if (!(det instanceof HTMLDetailsElement) || !det.open) return;
  const body = det.querySelector(':scope > .tbody');
  if (body) paintBody(body);
}, true);

const TRANSCRIPT_WINDOW_EVENTS = 500;
const transcriptRenderStates = new WeakMap();

function resetTranscriptRenderState(log) {
  const state = { groups: [], currentGroup: null, calls: createCallPairing() };
  transcriptRenderStates.set(log, state);
  return state;
}

// A replaced tail removes rows from the log; the state must stop counting their
// groups, appending into them, or pairing a re-delivered result with their calls.
function pruneTranscriptRenderState(log) {
  const state = transcriptRenderStates.get(log);
  if (!state) return;
  state.groups = state.groups.filter(group => group.details.parentNode === log);
  if (!state.groups.includes(state.currentGroup)) state.currentGroup = null;
  state.calls.close();
}

// A spoken turn ends the wait for a result: an interrupted call is not running.
function closeWaitingCalls(state) {
  for (const call of state.calls.close()) {
    if (!call.item?.awaiting) continue;
    call.item.awaiting = false;
    labelHiddenGroup(call.item.group);
  }
}

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
    resetTranscriptRenderState(log);
    appendTranscriptEvents(log, source, resolveReference, projectRoot, profile, options.keyPrefix || '');
    return;
  }
  const paint = (start, preserveAnchor = false) => {
    const scroller = log.closest('.detailscroll');
    const beforeHeight = preserveAnchor ? log.scrollHeight : 0;
    const beforeTop = preserveAnchor && scroller ? scroller.scrollTop : 0;
    log.replaceChildren();
    resetTranscriptRenderState(log);
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
    appendTranscriptEvents(log, source.slice(start), resolveReference, projectRoot, profile, options.keyPrefix || '');
    document.dispatchEvent(new CustomEvent('cg:transcript-hidden-changed'));
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
  context: ev => 'context · ' + contextRowName(ev),
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

// The first disclosure is a lightweight list of activity; payloads stay lazy
// until a reader opens an individual item. It never evaluates profile rules.
function labelHiddenGroup(group) {
  const pending = group.items.some(item => item.awaiting);
  group.summary.textContent = hiddenActivityLabel(group.kind, group.items.length, pending);
}

function appendHiddenPart(item, event, label, resolveReference) {
  const part = el('div', 'transcript-activity-part');
  part.appendChild(el('div', 'transcript-activity-part-label', label));
  const body = lazyBody(event.text || '', event);
  part.appendChild(body);
  item.parts.appendChild(part);
  paintHiddenPart(body, event, label.toLowerCase(), resolveReference, item.session);
}

async function paintHiddenPart(body, event, label, resolveReference, session) {
  if (body.dataset.fetched === '1') return;
  body.dataset.fetched = '1';
  const render = value => { body.innerHTML = linkify(event.kind === 'tool_call' ? decodePayload(value) : value, resolveReference); };
  render(event.text || '');
  if (!event.full_len || event.full_len <= (event.text || '').length) return;
  try {
    if (!session) throw new Error('session is no longer open');
    const full = await fullTranscriptEvent(session, event.seq);
    if (full.available) { render(full.text || ''); return; }
    body.appendChild(el('div', 'sub', full.note || 'Full ' + label + ' text is unavailable'));
  } catch (error) {
    body.appendChild(el('div', 'sub', 'Showing clipped ' + label + ': ' + (error.message || error)));
  }
}

function mountHiddenItem(group, item, resolveReference, projectRoot) {
  const details = document.createElement('details');
  details.className = 'transcript-activity-item';
  const summary = el('summary');
  const ev = item.event;
  if (ev.kind === 'tool_call') {
    summary.appendChild(el('span', 'tname', ev.name || 'tool'));
    const peek = el('span', 'tpeek');
    peek.innerHTML = linkify(toolPeek(ev.text, projectRoot), resolveReference);
    summary.appendChild(peek);
  } else {
    // A context row is named by what it is (the handoff, or the hook event that added it).
    summary.appendChild(el('span', 'tname', ev.kind === 'tool_result' ? 'Result' : (ev.kind === 'context' ? 'context · ' + contextRowName(ev) : ev.name || ev.kind)));
    summary.appendChild(el('span', 'tpeek', rowPeek(ev.text)));
  }
  item.parts = el('div', 'transcript-activity-parts');
  details.append(summary, item.parts);
  item.node = details;
  item.session = group.session;
  const identity = ev.turn_anchor ? 'anchor:' + ev.turn_anchor
    : (Number(ev.seq) ? group.keyPrefix + 'seq:' + Number(ev.seq) : '');
  if (identity) details.dataset.transcriptKey = identity;
  applyTranscriptDecorators(ev, details, { kind: ev.kind, projectRoot, resolveReference, session: group.session });
  details.addEventListener('toggle', () => paintHiddenItem(item, resolveReference));
  group.list.appendChild(details);
}

// A result may arrive after this item was opened and closed. Mount each part
// independently so reopening catches up and a queued toggle never duplicates it.
function paintHiddenItem(item, resolveReference) {
  if (!item.node?.open) return;
  if (!item.drawn) {
    item.drawn = true;
    appendThoughtDuration(item.parts, item.event);
    appendHiddenPart(item, item.event, item.event.kind === 'tool_call' ? 'Call' : 'Transcript', resolveReference);
  }
  if (item.result && !item.resultDrawn) {
    item.resultDrawn = true;
    appendHiddenPart(item, item.result, 'Result', resolveReference);
  }
}

// call is the hidden call this result answers, when there is one. A call is
// labelled as running only when it arrived live and its result has not.
function appendHiddenRow(log, ev, state, call, live, resolveReference, projectRoot, keyPrefix) {
  if (call?.item) {
    const item = call.item;
    item.result = ev;
    item.awaiting = false;
    labelHiddenGroup(item.group);
    paintHiddenItem(item, resolveReference);
    return;
  }
  const kind = hiddenActivityKind(ev);
  let group = state.currentGroup;
  if (!group || group.kind !== kind) {
    const details = document.createElement('details');
    details.className = 'transcript-activity-group';
    const summary = el('summary');
    const list = el('div', 'transcript-activity-list');
    details.append(summary, list);
    group = { kind, details, summary, list, items: [], mounted: false, keyPrefix,
      session: log.effortSession || currentSession };
    details.addEventListener('toggle', () => {
      if (details.open && !group.mounted) {
        group.mounted = true;
        group.items.forEach(item => mountHiddenItem(group, item, resolveReference, projectRoot));
      }
      document.dispatchEvent(new CustomEvent('cg:transcript-hidden-changed'));
    });
    log.appendChild(details);
    state.groups.push(group);
    state.currentGroup = group;
  }
  const item = { event: ev, group, result: null, awaiting: false, node: null, drawn: false, resultDrawn: false, parts: null };
  if (ev.kind === 'tool_call') {
    item.awaiting = live;
    state.calls.call({ display: 'hidden', item });
  }
  group.items.push(item);
  labelHiddenGroup(group);
  if (group.mounted) mountHiddenItem(group, item, resolveReference, projectRoot);
}

function appendConversationMessage(log, ev, resolveReference) {
  if (ev.kind === 'user') {
    const m = el('div', 'msg user');
    if (ev.ts) m.appendChild(el('div', 'hd', 'You · ' + fmtTime(ev.ts)));
    // linkify escapes human text before inserting markup (impl-plan R1).
    const bubble = el('div', 'bubble');
    bubble.innerHTML = linkify(ev.text || '', resolveReference);
    m.appendChild(bubble);
    log.appendChild(m);
    return;
  }
  const m = el('div', 'msg agent');
  m.appendChild(mkMark());
  const md = el('div', 'md');
  md.innerHTML = mdToHtml(ev.text || '', resolveReference);
  m.appendChild(md);
  log.appendChild(m);
}

function appendToolTranscriptRow(log, ev, state, call, resolveReference, projectRoot) {
  if (ev.kind === 'tool_call') {
    const det = document.createElement('details'); det.className = 'toolchip';
    const sum = el('summary');
    sum.append('⚙ ');
    sum.appendChild(el('span', 'tname', ev.name || 'tool'));
    const peek = el('span', 'tpeek');
    peek.innerHTML = ' ' + linkify(toolPeek(ev.text, projectRoot), resolveReference);
    sum.appendChild(peek);
    det.append(sum, lazyBody(ev.text || '', ev));
    log.appendChild(det);
    state.calls.call({ display: 'shown', chip: det });
    return;
  }
  if (call?.chip) {
    const body = call.chip.querySelector('.tbody');
    body.dataset.raw = (body.dataset.raw || '') + RESULT_SEP + (ev.text || '');
    // A clipped Write argument may lose its path; recover the peek from the result.
    relabelPeekFromResult(call.chip, ev.text || '', projectRoot, resolveReference);
    paintBody(body);
    return;
  }
  const line = el('div', 'sysline');
  line.innerHTML = '⚙ result: ' + linkify((ev.text || '').slice(0, 200), resolveReference);
  log.appendChild(line);
}

function appendThoughtDuration(log, ev) {
  // Duration sits immediately before the row it describes.
  if (Number(ev.thought_ms) > 0) {
    log.appendChild(el('div', 'thought-line', 'thought for ' + humanDuration(Math.round(Number(ev.thought_ms) / 1000))));
  }
}

// appendTranscriptEvents maps canonical events to chat-style DOM: agent prose
// is the page's primary text (no box), user turns keep the composer bubble,
// tools/thinking collapse to chips, and system lines stay quiet. A view profile
// may fold or hide rows; with none, every row is drawn.
// live marks events that arrived while this view was open.
function appendTranscriptEvents(log, events, resolveReference, projectRoot, profile = null, keyPrefix = '', live = false) {
  const state = transcriptRenderStates.get(log) || resetTranscriptRenderState(log);
  for (const ev of events) {
    if (ev.kind === 'user' || ev.kind === 'assistant') closeWaitingCalls(state);
    // Results answer calls in arrival order, so calls fired together keep their own.
    const answers = ev.kind === 'tool_result';
    const display = followingDisplay(ev, profile, answers ? state.calls.peek()?.display || '' : '');
    const call = answers ? state.calls.result() : null;
    if (display === HIDE) {
      appendHiddenRow(log, ev, state, call, live, resolveReference, projectRoot, keyPrefix);
      continue;
    }
    state.currentGroup = null;
    appendThoughtDuration(log, ev);
    const priorRow = log.lastElementChild;
    if (display === COLLAPSE && ROW_CHIP_LABELS[ev.kind]) {
      log.appendChild(rowChip(ev));
    } else switch (ev.kind) {
      case 'user':
      case 'assistant':
        appendConversationMessage(log, ev, resolveReference);
        break;
      case 'thinking': {
        if (!hasRenderableText(ev.text)) break;
        const det = document.createElement('details'); det.className = 'thinkchip';
        det.appendChild(el('summary', '', 'thinking'));
        det.appendChild(lazyBody(ev.text || '', ev));
        log.appendChild(det);
        break;
      }
      case 'tool_call':
      case 'tool_result':
        appendToolTranscriptRow(log, ev, state, call, resolveReference, projectRoot);
        break;
      case 'summary':
        log.appendChild(el('div', 'sysline', '§ ' + (ev.text || '')));
        break;
      case 'context': {
        const line = el('div', 'sysline context-line');
        line.append(el('span', 'rowchip-label', 'context · ' + contextRowName(ev) + ' '), ev.text || '');
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
      const identity = ev.turn_anchor ? 'anchor:' + ev.turn_anchor
        : (Number(ev.seq) ? keyPrefix + 'seq:' + Number(ev.seq) : '');
      if (identity) rowEl.dataset.transcriptKey = identity;
      applyTranscriptDecorators(ev, rowEl, { kind: ev.kind, projectRoot, resolveReference, session: log.effortSession });
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

// the audit evidence for ONE session, rendered on the session detail (design gap
// named: clicking a finding must land you where the evidence is, not just the transcript).
export async function loadExposure(box, badge, runtime, id) {
  try {
    const e = await api('/api/audit/session?runtime=' + encodeURIComponent(runtime)
      + '&id=' + encodeURIComponent(id));
    if (box) renderExposure(box, e); // box may be null: badge-only fill (card lives in the panel provider)
    // The header chip shows a finding only. "No detector matched" is a weak
    // negative, and saying it in prime space reads as a clean bill; it lives
    // with the evidence instead (session.impact, Data and secrets).
    if (badge) {
      const n = (e.matched || []).length;
      badge.hidden = !e.watermark;
      if (e.watermark) badge.textContent = '⚠ ' + e.watermark + (n ? ' · ' + n + ' rule' + (n > 1 ? 's' : '') : '');
    }
  } catch {
    if (box) box.replaceChildren(el('div', 'evidence-state', 'Request failed'), el('div', 'sub', 'Data and secret evidence could not be loaded.'));
    if (badge) badge.hidden = true;
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
      if (m.absent && m.absent.length) line.appendChild(el('span', 'sub', ' · absent: ' + m.absent.join(', ')));
      box.appendChild(line);
    });
  } else {
    box.appendChild(el('div', 'sub', 'No declared detector matched · weak negative'));
  }
  if (e.agent_state === 'unavailable') {
    box.appendChild(el('div', 'sub', 'Agent tags could not be read · rules on agent: tags were not evaluated'));
  }
  if (e.rules_not_fully_audited > 0) {
    box.appendChild(el('div', 'sub', e.rules_not_fully_audited + ' armed rule(s) read the command, tool or target of one call and are only partly audited here'));
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
// readOrganization reads the rail's published settings and applies them. A read
// that fails is forgotten, so the next caller reads again: the Handoffs group asks
// through this until it has its cadence.
let organizationRead = null;
function readOrganization() {
  organizationRead ||= api('/api/console/config').then(found => {
    const organization = found?.config?.session_organization || {};
    configureViewList({ settings: { viewsVisible: organization.views_visible, countRefreshMs: organization.count_refresh_ms } });
    configureHeaderTags({ recentTagToggles: organization.recent_tag_toggles });
    return { countRefreshMs: organization.count_refresh_ms, endedVisible: organization.handoffs_ended_visible };
  }).catch(error => { organizationRead = null; throw error; });
  return organizationRead;
}
configureHandoffRail({ readSettings: readOrganization });

// Settings › Views sends cg:view-select before cg:nav. Navigating alone would
// restore this pinned pane from cache without a repaint (app.js FRESH
// policy), so the view would never show; selectView's state change would sit
// under stale DOM and a stale filter bar. The listener mirrors what a click
// on the view's own rail row does. clearBar first: an un-cleared bar would
// override the view (query-bar's activeQuery precedence).
document.addEventListener('cg:view-select', event => {
  clearBar();
  selectView(String(event.detail || ''));
  renderRail();
});

export { openSession, renderSessionList, renderRail, renderTranscript };
