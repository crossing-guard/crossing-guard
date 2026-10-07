// The board (sessions-board plan rev 3, slice 1): a saved view rendered as
// kanban. Columns are the values of the view's group tag (group_by
// tag-key:<key>); cards are the same organized rows the rail renders; drag
// (and its keyboard pair) moves a card by WRITING TAGS through the existing
// session-tags API — apply destination + retract origin in one call. The
// board renders; it never stores: journal → signals → flow membership is
// the only event path, and it already exists.
//
// Placement is the daemon's (board-observed-columns plan §2): every read
// names the board's view, and the daemon files a session under the owner's
// tag first, else under the first of the view's placement rules its facts
// match, else under no column. A rule writes nothing; only a move does. A
// detector fact sharing the group key never places a card in a column the
// drag would write a tag into (sessions-board plan §3.3).

import { el, api } from '../core.js';
import { relativeTime } from '../orchestration/agents/roster-model.js';
import { changeSessionTags, loadSessionTags } from './organization-api.js';
import { activeQuery } from './organization-state.js';
import { organizedPageUrl, organizedRailUrl, foldGroupKey, groupLabel } from './view-group-source.js';
import { askLine, agentNote, needsReader } from '../task/session-status.js';
import { rememberCardLink, cardOpenItem, mountCardOpen, hideCardOpen, settleCardOpen } from './board-card-open.js';

// The session surface's status reader for each rendered board: (runtime,
// catalogID, nativeID) → the rail's rendered status. Cards read attention
// through it so the board and the rail say the same words.
const boardStatus = new WeakMap();

// What each rendered board was drawn from — the view, its board config and
// the query — and its refresh in flight (refreshBoard).
const boardSources = new WeakMap();

// The query each rendered column was drawn from. A column re-reads under
// that query, never under whatever is active later: a filter typed in the bar
// leaves the board on screen while the active query becomes the bar's.
const columnQuery = new WeakMap();

// The card being dragged, for the dragover test only. Where a move goes from
// and which key it writes are read from the rendered board when it happens
// (board-view-query-correctness plan invariant 6), never kept here.
const dragState = { card: null };

// One record per rendered column (board-move-refresh plan §3): the read in
// flight, whether another was asked for meanwhile, and whether a page is
// waiting for a drag to end. Keyed by the column element, so a replaced board
// leaves nothing behind.
const columnLoads = new WeakMap();

// The views whose board shows only the cards that need the reader. Kept by
// view id for the life of the page: opening a card replaces the board, and
// the reader comes back to the list he was working through.
const needsOnly = new Set();

const cardKey = (runtime, id) => runtime + '\u0000' + id;
const boardColumns = board => board ? board.querySelectorAll('.board-column') : [];

// boardConfig reads one view's board configuration. The board exists only
// for views whose group_by is tag-key:<key> (validated at save); the config
// carries order and emptiness only — never the grouping.
export function boardConfig(view) {
  if (!view || !view.board) return null;
  const groupBy = view.group_by || '';
  if (!groupBy.startsWith('tag-key:')) return null;
  return {
    groupKey: groupBy.slice('tag-key:'.length),
    columns: Array.isArray(view.board.columns) ? view.board.columns.map(column => String(column).trim()) : [],
    emptyColumns: !!view.board.empty_columns,
    hasRules: Array.isArray(view.board.placement) && view.board.placement.length > 0,
  };
}

// boardColumnList is the board's columns for one rail read: declared columns
// first, observed undeclared values after, and last — only on a board with
// placement rules — the sessions nothing placed (plan §2.3). A declared
// column keeps the owner's spelling; only the match to the daemon's folded
// group key ignores letter case.
function boardColumnList(board, groups) {
  const byKey = new Map();
  for (const group of groups) byKey.set(group.key, group);
  const columns = [];
  const seen = new Set();
  for (const column of board.columns) {
    const groupKey = foldGroupKey(column);
    columns.push({ key: column, declared: true, group: byKey.get(groupKey) || null });
    seen.add(groupKey);
  }
  for (const group of groups) {
    if (!seen.has(group.key) && group.key) columns.push({ key: group.key, declared: false, group });
  }
  const unplaced = byKey.get('');
  if (board.hasRules && unplaced && unplaced.total) columns.push({ key: '', declared: false, group: unplaced, unplaced: true });
  return columns;
}

// renderBoardNodes builds the board synchronously from the organized read
// the rail already fetched (plan slice 1). Cards per column load through the
// group-page read the rail's expanded groups already use. `status` is the
// session surface's status reader and `statusDot` its dot builder, so a card
// and a rail row say the same thing about a session.
export function renderBoardNodes(view, board, groups, { status = null, statusDot = null } = {}) {
  const host = el('div', 'board');
  if (status) boardStatus.set(host, { status, statusDot });
  boardSources.set(host, { view, board, query: activeQuery(), refresh: null, again: false });
  host.dataset.groupKey = board.groupKey;
  host.dataset.emptyColumns = view.board && view.board.empty_columns ? '1' : '';
  host.dataset.rules = board.hasRules ? '1' : '';
  host.dataset.needsOnly = needsOnly.has(view.id) ? '1' : '';
  host.setAttribute('role', 'region');
  host.setAttribute('aria-label', 'Board: ' + (view.name || 'untitled view'));
  for (const column of boardColumnList(board, groups)) host.appendChild(renderColumn(column, view, board, activeQuery()));
  paintBoardA11y(host);
  mountCardOpen(host);
  return host;
}

// renderColumn draws one column and starts its read under `query`: the query
// the board was drawn from, never whatever is active when a refresh adds it.
function renderColumn(column, view, board, query) {
  const columnHost = el('div', 'board-column');
  const name = column.unplaced ? groupLabel({ groupBy: 'tag-key:' + board.groupKey }, { key: '' }) : column.key;
  columnHost.dataset.column = column.key;
  columnHost.dataset.declared = column.declared ? '1' : '';
  columnHost.dataset.unplaced = column.unplaced ? '1' : '';
  columnHost.dataset.boardView = view.id || '';
  columnQuery.set(columnHost, query);
  columnHost.setAttribute('aria-label', 'Column ' + name);
  const head = el('div', 'board-column-head');
  head.appendChild(el('span', 'board-column-name', name));
  columnHost.appendChild(head);
  const cards = el('div', 'board-cards');
  cards.setAttribute('role', 'list');
  columnHost.appendChild(cards);
  const group = column.group;
  if (group) {
    head.appendChild(el('span', 'board-column-count', String(group.total || 0)));
    loadColumn(columnHost);
  } else if (view.board && view.board.empty_columns) {
    cards.appendChild(el('div', 'board-empty', 'No sessions'));
  }
  // The sessions nothing placed are not a place to put one: a card is taken
  // out of a column by "Follow observed", never by a drop (plan §2.3).
  if (!column.unplaced) makeDropTarget(columnHost, column.key);
  return columnHost;
}

// Drop targets: dragover must preventDefault for the drop event to fire.
function makeDropTarget(columnHost, key) {
  columnHost.addEventListener('dragover', event => {
    if (!dragState.card) return;
    event.preventDefault();
    columnHost.classList.add('board-drop');
  });
  columnHost.addEventListener('dragleave', () => columnHost.classList.remove('board-drop'));
  columnHost.addEventListener('drop', event => {
    event.preventDefault();
    columnHost.classList.remove('board-drop');
    moveCard(dragState.card, key);
  });
}

// loadColumn reads one column, one read at a time. A load asked for while one
// is in flight queues exactly one more, and the promise resolves after the
// read that covers the request — so a burst of moves costs one read in flight
// plus one queued per column (board-move-refresh plan invariant 3).
function loadColumn(columnHost) {
  let load = columnLoads.get(columnHost);
  if (!load) columnLoads.set(columnHost, load = { running: null, again: false, deferred: false });
  if (load.running) { load.again = true; return load.running; }
  load.running = (async () => {
    try {
      do { load.again = false; await readColumn(columnHost, load); } while (load.again);
    } finally { load.running = null; }
  })();
  return load.running;
}

async function readColumn(columnHost, load) {
  // One read per column through the existing group-page endpoint. Named the
  // board's view, the daemon places rows as the header's count did, and
  // bounds the page by daemon.json's board_column_cards_max; the board sends
  // no limit (board-column-overflow plan §2). Mode is 'all',
  // as the rail's view groups start: a column is a workflow stage, not
  // liveness, and the daemon refuses any mode but all/open
  // (board-column-mode-fix plan §2).
  const url = organizedPageUrl(columnQuery.get(columnHost), { key: columnHost.dataset.column, mode: 'all', offset: 0, boardView: columnHost.dataset.boardView });
  if (!url) return;
  let data = null, failure = '';
  try {
    data = await api(url);
  } catch (err) {
    failure = 'Could not load this column: ' + (err && err.message ? err.message : 'the request failed');
  }
  // A newer read is queued: it paints, this answer is already old.
  if (load.again) return;
  // A drag holds a card in this column: its page waits until the drag ends,
  // so a repaint never detaches the card in hand (RT-13).
  if (dragState.card && columnHost.contains(dragState.card)) { load.deferred = true; return; }
  clearColumnLines(columnHost);
  // The column says it could not load, instead of looking empty (J8); the
  // cards it already shows stay.
  if (failure) { columnHost.querySelector('.board-cards').appendChild(el('div', 'board-empty board-failed', failure)); settleBoard(columnHost.closest('.board')); }
  else renderColumnPage(columnHost, data || {});
}

function clearColumnLines(columnHost) {
  for (const line of columnHost.querySelectorAll('.board-empty')) line.remove();
  for (const line of columnHost.querySelectorAll('.board-overflow')) line.remove();
}

// renderColumnPage makes a column show its page — count, cards, overflow —
// in place: the first load and every refresh after a move come through here.
function renderColumnPage(columnHost, data) {
  const cards = columnHost.querySelector('.board-cards');
  const sessions = data.sessions || [];
  const total = Number.isInteger(data.total) ? data.total : null;
  reconcileCards(cards, sessions);
  // An observed (undeclared) column exists only while it holds sessions.
  if (total === 0 && !columnHost.dataset.declared) { removeColumn(columnHost); return; }
  paintColumnCount(columnHost, total);
  if (!sessions.length && columnHost.dataset.declared && columnHost.closest('.board')?.dataset.emptyColumns) {
    cards.appendChild(el('div', 'board-empty', 'No sessions'));
  }
  // Past the bound the column says how many it leaves out, never truncating
  // silently (board plan §3 item 5). The line sits after the card list, so
  // drag, the column menu and attention paint never take it for a card.
  const hidden = (total || 0) - sessions.length;
  if (hidden > 0) {
    columnHost.appendChild(el('div', 'board-overflow', hidden + ' more ' + (hidden === 1 ? 'session' : 'sessions') + ' not shown'));
  }
  settleBoard(columnHost.closest('.board'));
}

// removeColumn takes an emptied observed column off the board, with any open
// column menu: a menu may still offer the column that is going.
function removeColumn(columnHost) {
  const board = columnHost.closest('.board');
  for (const menu of board?.querySelectorAll('.board-menu') || []) menu.closeMenu();
  columnHost.remove();
  settleBoard(board);
}

// The header count is the page's total, which the daemon counts with the
// placer the rail's group total uses. A page with no total claims nothing.
function paintColumnCount(columnHost, total) {
  if (total == null) return;
  const head = columnHost.querySelector('.board-column-head');
  let count = head.querySelector('.board-column-count');
  if (total === 0) { count?.remove(); return; }
  if (!count) count = head.appendChild(el('span', 'board-column-count'));
  count.textContent = String(total);
}

// reconcileCards makes the list hold the page's sessions in the page's order,
// keyed by runtime + catalog id. A card that stays and is in order is never
// taken out of the tree; only cards that left, arrived or changed place are
// touched.
function reconcileCards(cards, sessions) {
  const status = boardStatus.get(cards.closest('.board'));
  const focused = document.activeElement;
  const kept = new Map();
  for (const card of cards.querySelectorAll('.board-card')) kept.set(cardKey(card.dataset.runtime, card.dataset.sessionId), card);
  const wanted = sessions.map(session => {
    const key = cardKey(session.runtime, session.id);
    const card = kept.get(key);
    // A card that stays is repainted from the fresh row: where it was placed
    // from and since when may have changed under it (plan §2.3).
    if (card) {
      kept.delete(key);
      paintCardCells(card, session);
      if (status) paintCardAttention(card, status.status, status.statusDot);
      return card;
    }
    const made = boardCard(session);
    if (status) paintCardAttention(made, status.status, status.statusDot);
    return made;
  });
  for (const gone of kept.values()) dropCard(gone);
  wanted.forEach((card, index) => {
    if (cards.children[index] !== card) cards.insertBefore(card, cards.children[index] || null);
  });
  if (focused && focused !== document.activeElement && cards.contains(focused) && isShown(focused)) focused.focus();
  settleCardOpen(cards.closest('.board'), isShown);
}

// dropCard takes a card off the board, with the column menu opened for it
// and, if it was somehow the card in hand, the drag that held it.
function dropCard(card) {
  for (const menu of card.closest('.board')?.querySelectorAll('.board-menu') || []) {
    if (menu.forCard === card) menu.closeMenu();
  }
  const board = card.closest('.board');
  if (board?.querySelector('.board-card-open')?.forCard === card) hideCardOpen(board);
  if (dragState.card === card) dragState.card = null;
  card.remove();
}

// paintBoardAttention repaints every card's agent line in place, by card key
// (runtime + catalog id), with no refetch — so a drag in progress and the
// cards' order survive (RT-13). The session surface calls it from the rail's
// activity-store listener.
export function paintBoardAttention(host) {
  const status = boardStatus.get(host);
  if (!status) return;
  for (const card of host.querySelectorAll('.board-card')) paintCardAttention(card, status.status, status.statusDot);
  settleBoard(host);
}

// isShown says whether a card is drawn: every card, unless the board shows
// only the cards that need the reader. It reads the card's mark and the
// board's state, never layout.
function isShown(card) {
  return !card.closest('.board')?.dataset.needsOnly || Boolean(card.dataset.needs);
}
const shownCards = root => [...root.querySelectorAll('.board-card')].filter(isShown);

// focusShown puts focus on a card if it is drawn; else on the next drawn
// card of the column it was in, else the first on the board, else the
// board's "Needs you" control. Every place that gives a card focus comes here.
function focusShown(board, card, from = card?.closest('.board-column')) {
  if (!board) return;
  const drawn = card && isShown(card) && card.isConnected !== false ? card : null;
  // The next drawn card below the one that cannot take focus, else the
  // column's first.
  const inColumn = from ? [...from.querySelectorAll('.board-card')] : [];
  const below = inColumn.slice(inColumn.indexOf(card) + 1).find(isShown);
  const target = drawn || below || inColumn.find(isShown) || shownCards(board)[0] || boardSources.get(board)?.needs;
  target?.focus();
}

// settleBoard brings a board to rest after its cards or their marks changed:
// the "Needs you" number, one tab stop on a drawn card, no menu left open for
// a card that is no longer drawn, and focus moved off a card about to hide.
function settleBoard(board) {
  if (!board) return;
  paintNeedsControl(board);
  for (const menu of board.querySelectorAll('.board-menu')) {
    if (!menu.forCard || isShown(menu.forCard)) continue;
    // A menu holding focus takes it away when it closes: hand it on.
    const held = menu.contains(document.activeElement);
    menu.closeMenu();
    if (held) focusShown(board, menu.forCard);
  }
  const focused = document.activeElement;
  if (focused && focused.closest?.('.board') === board && focused.dataset?.sessionId && !isShown(focused)) focusShown(board, focused);
  settleCardOpen(board, isShown);
  keepOneTabStop(board);
}

// boardNeedsControl is the board's "Needs you" control, for the heading row
// the session surface owns. It says how many cards on the board are waiting
// on the reader and, pressed, leaves only those drawn, each in its column.
// Released with nothing waiting it is off; pressed it can always be released.
export function boardNeedsControl(host) {
  const source = boardSources.get(host);
  const control = el('button', 'board-needs');
  control.type = 'button';
  control.appendChild(el('span', '', 'Needs you'));
  control.appendChild(el('span', 'board-needs-count'));
  control.onclick = () => {
    const viewId = source?.view.id;
    if (host.dataset.needsOnly) { host.dataset.needsOnly = ''; needsOnly.delete(viewId); }
    else { host.dataset.needsOnly = '1'; needsOnly.add(viewId); }
    settleBoard(host);
  };
  if (source) source.needs = control;
  paintNeedsControl(host);
  return control;
}

function paintNeedsControl(board) {
  const control = boardSources.get(board)?.needs;
  if (!control) return;
  const waiting = [...board.querySelectorAll('.board-card')].filter(card => card.dataset.needs).length;
  const pressed = Boolean(board.dataset.needsOnly);
  control.querySelector('.board-needs-count').textContent = waiting ? String(waiting) : '';
  control.setAttribute('aria-pressed', pressed ? 'true' : 'false');
  // Never switched off while pressed (it must be releasable) or while it
  // holds focus (focus would be left on a dead control).
  control.disabled = !pressed && waiting === 0 && document.activeElement !== control;
}

// paintCardAttention draws one card's line for an agent: an unseen ask
// ("asks you: …", with the dot), else an unsent proposed reply ("reply not
// sent: …", quiet). The helper's words are set as text, never as markup.
export function paintCardAttention(card, status, statusDot = null) {
  const state = status(card.dataset.runtime, card.dataset.sessionId, card.dataset.resumeId);
  const ask = state?.ask;
  card.dataset.needs = needsReader(state) ? '1' : '';
  // The dot is the rail row's dot: what the session is doing now.
  const slot = card.querySelector('.board-card-dot');
  if (slot && statusDot) {
    const dot = state ? statusDot(state) : null;
    slot.replaceChildren(...(dot ? [dot] : []));
  }
  card.querySelector('.board-card-agent')?.remove();
  let words = '';
  let line = null;
  if (ask?.unseen) {
    words = askLine(ask);
    line = el('span', 'board-card-agent board-card-ask');
    const dot = el('span', 'session-status-dot ask');
    dot.setAttribute('aria-hidden', 'true');
    line.appendChild(dot);
    line.appendChild(el('span', '', words));
  } else if (agentNote(ask)) {
    words = agentNote(ask);
    line = el('span', 'board-card-agent board-card-draft', words);
  }
  if (line) card.appendChild(line);
  const base = card.dataset.baseLabel || card.getAttribute('aria-label') || '';
  card.dataset.baseLabel = base;
  card.setAttribute('aria-label', words ? base + ' · ' + words : base);
}

function boardCard(session) {
  const card = el('button', 'board-card');
  card.type = 'button';
  card.draggable = true;
  card.tabIndex = -1;
  card.dataset.runtime = session.runtime;
  card.dataset.sessionId = session.id;
  card.dataset.resumeId = session.resume_id || session.id;
  const title = el('span', 'board-card-title');
  title.appendChild(el('span', 'board-card-dot'));
  title.appendChild(el('span', '', session.title || 'Untitled session'));
  card.appendChild(title);
  card.appendChild(el('span', 'board-card-cells'));
  paintCardCells(card, session);
  // Opening from the board goes through the session surface's one opener via
  // the cg:board-open event — sessions.js owns openSession and listens for
  // it; no circular import (placement rule PO-13).
  card.onclick = () => {
    document.dispatchEvent(new CustomEvent('cg:board-open', {
      detail: { runtime: session.runtime, id: session.id, resumeId: session.resume_id || session.id } }));
  };
  card.addEventListener('dragstart', event => {
    dragState.card = card;
    event.dataTransfer.effectAllowed = 'move';
    try { event.dataTransfer.setData('text/plain', session.id); } catch { /* some engines */ }
    card.classList.add('board-card-dragging');
  });
  card.addEventListener('dragend', () => endDrag(card));
  return card;
}

// endDrag lets everything that waited for the drag go ahead: the columns
// that held their page back, and a board refresh asked for meanwhile.
function endDrag(card) {
  card.classList.remove('board-card-dragging');
  dragState.card = null;
  const board = card.closest('.board');
  for (const column of boardColumns(board)) {
    const load = columnLoads.get(column);
    if (load?.deferred) { load.deferred = false; loadColumn(column); }
  }
  const source = boardSources.get(board);
  if (source?.deferred) { source.deferred = false; refreshBoard(board); }
}

// paintCardCells draws what the row says about the session under its title:
// runtime, repository and how long it has been in this column; the facts its
// tags carry; and, on a card the owner placed while its facts place it
// elsewhere, that other column. Facts ride as tag rows with a detector
// provenance; a card shows them whichever provenance wrote them.
function paintCardCells(card, session) {
  const cells = card.querySelector('.board-card-cells');
  if (!cells) return;
  card.dataset.placedBy = session.placed_by || '';
  rememberCardLink(card, session);
  const repository = String(session.repository_key || '').split('/').filter(Boolean).pop() || '';
  const since = relativeTime(session.in_column_since);
  const meta = [session.runtime, repository, since].filter(Boolean);
  const nodes = [el('span', 'board-card-meta', meta.join(' \u00b7 '))];
  for (const tag of (session.tags || [])) {
    if (tag.key === 'work' && tag.value === 'uncommitted') nodes.push(el('span', 'board-card-fact', 'uncommitted edits'));
  }
  if (session.observed_group) nodes.push(el('span', 'board-card-observed', 'matches ' + session.observed_group));
  cells.replaceChildren(...nodes);
  const label = [session.title || 'Untitled session', ...meta, session.observed_group ? 'matches ' + session.observed_group : ''].filter(Boolean).join(' \u00b7 ');
  card.dataset.baseLabel = label;
  card.setAttribute('aria-label', label);
}

// boardMoveTags is the tag change one move writes: apply the destination
// value under the board's group key, retract the origin value. It is null
// when there is nothing to write — no key, no destination, or no move — so
// a board that cannot say which key it groups by writes nothing at all.
export function boardMoveTags(groupKey, fromColumn, toColumn) {
  if (!groupKey || !toColumn || fromColumn === toColumn) return null;
  return {
    apply: [{ key: groupKey, value: toColumn }],
    retract: fromColumn ? [{ key: groupKey, value: fromColumn }] : [],
  };
}

// cardMoveTags reads a move's coordinates from the rendered board at the
// moment it happens: the key from the card's own board, the origin from the
// card's own column. Pointer and keyboard moves both come through here.
export function cardMoveTags(card, toColumn) {
  const { groupKey, fromColumn } = cardCoordinates(card);
  return boardMoveTags(groupKey, fromColumn, toColumn);
}

function cardCoordinates(card) {
  return {
    groupKey: card.closest('.board')?.dataset.groupKey || '',
    fromColumn: card.closest('.board-column')?.dataset.column || '',
  };
}

// moveCard is THE event: one POST with the compound write the API already
// supports — retract the origin value, apply the destination value. The
// journal records the pair; the flow reacts within one sweep. A failed or
// refused write says why and rolls nothing back partially (AC 4); a drop on
// the card's own column is no move and says nothing, and neither does a card
// a refresh already took off the board. `write` is the tag write, a seam for
// the node tests. The promise resolves when the touched columns have
// answered (or failed, or are waiting on a drag).
export async function moveCard(card, toColumn, write = changeSessionTags) {
  const board = card?.closest('.board');
  if (!board) return;
  const { groupKey, fromColumn } = cardCoordinates(card);
  if (toColumn && fromColumn === toColumn) return;
  const change = boardMoveTags(groupKey, fromColumn, toColumn);
  if (!change) {
    announce(groupKey ? 'Move refused: there is no column to move to. Nothing was written.'
      : 'Move refused: this board does not say which tag key it groups by. Nothing was written.');
    return;
  }
  try {
    await write([{ runtime: card.dataset.runtime, id: card.dataset.sessionId }], change);
  } catch (err) {
    announce('Move failed: ' + (err && err.message ? err.message : 'the tag write failed'));
    return;
  }
  announce('Moved to ' + toColumn);
  // The session surface owns the view list beside the board; it refreshes
  // the view counts on this event (the cg:board-open seam, the other way).
  document.dispatchEvent(new CustomEvent('cg:board-moved'));
  await showMove(board, card, toColumn);
}

// showMove makes the board show a written move without rebuilding it
// (board-move-refresh plan): the moved card leaves its origin column at once —
// the retract makes that certain, and a stale card left behind could be moved
// again into two columns — then only the columns holding the session and the
// destination re-read their page. Columns are matched on their dataset, never
// a selector: a tag value may hold any character.
async function showMove(board, card, toColumn) {
  const movedKey = cardKey(card.dataset.runtime, card.dataset.sessionId);
  const holdsMoved = column => [...column.querySelectorAll('.board-card')]
    .some(other => cardKey(other.dataset.runtime, other.dataset.sessionId) === movedKey);
  const touched = [...boardColumns(board)].filter(column => column.dataset.column === toColumn || holdsMoved(column));
  dropCard(card);
  settleBoard(board);
  await Promise.all(touched.map(loadColumn));
  // Focus follows the moved card unless the owner has put it elsewhere.
  if (document.activeElement && document.activeElement !== document.body) return;
  for (const column of touched) {
    if (column.dataset.column !== toColumn) continue;
    for (const moved of column.querySelectorAll('.board-card')) {
      if (cardKey(moved.dataset.runtime, moved.dataset.sessionId) === movedKey) focusShown(board, moved, column);
    }
  }
}

function announce(text) {
  let region = document.querySelector('.board-live');
  if (!region) {
    region = el('div', 'board-live');
    region.setAttribute('aria-live', 'polite');
    region.style.position = 'absolute';
    region.style.left = '-9999px';
    document.body.appendChild(region);
  }
  region.textContent = text;
}

// Keyboard parity (plan §3.2): the board is one tab stop; arrows move
// between cards and columns, Enter opens the card, m opens its menu, whose
// choices write what a drag writes.
function paintBoardA11y(host) {
  host.addEventListener('keydown', event => {
    const card = event.target.closest('.board-card');
    if (!card) return;
    if (event.key === 'm' || event.key === 'M') {
      event.preventDefault();
      openColumnMenu(card);
      return;
    }
    const next = arrowTarget(host, card, event.key);
    if (next) { event.preventDefault(); next.focus(); }
  });
  // The pointer's way to the same menu; a touch long-press raises it too.
  host.addEventListener('contextmenu', event => {
    const card = event.target.closest('.board-card');
    // A card with nothing to offer keeps the browser's own menu.
    if (!card || !cardMenuItems(card, host, moveCard).length) return;
    event.preventDefault();
    openColumnMenu(card);
  });
  host.addEventListener('focusin', event => {
    const card = event.target.closest('.board-card');
    if (!card) return;
    for (const other of host.querySelectorAll('.board-card')) other.tabIndex = other === card ? 0 : -1;
  });
}

// arrowTarget is the card an arrow key moves to: the next or previous card in
// the column, or the card at the same height in the nearest column to that
// side that holds any.
function arrowTarget(host, card, key) {
  const columns = [...boardColumns(host)].map(shownCards);
  const at = columns.findIndex(cards => cards.includes(card));
  if (at < 0) return null;
  const row = columns[at].indexOf(card);
  if (key === 'ArrowDown') return columns[at][row + 1] || null;
  if (key === 'ArrowUp') return columns[at][row - 1] || null;
  const step = key === 'ArrowRight' ? 1 : key === 'ArrowLeft' ? -1 : 0;
  for (let index = at + step; step && index >= 0 && index < columns.length; index += step) {
    if (columns[index].length) return columns[index][Math.min(row, columns[index].length - 1)];
  }
  return null;
}

// keepOneTabStop leaves exactly one card reachable by Tab: the one that had
// it, else the first on the board.
function keepOneTabStop(board) {
  if (!board) return;
  const shown = shownCards(board);
  if (!shown.length || shown.some(card => card.tabIndex === 0)) return;
  // The holder of the tab stop must itself be drawn.
  for (const card of board.querySelectorAll('.board-card')) card.tabIndex = -1;
  shown[0].tabIndex = 0;
}

// cardMenuItems are the choices the menu offers for a card: opening its
// session in the desktop app, when the session has one; every other
// column a card can be put in; on a board with placement rules, "Pin here"
// to keep the card where it is whatever its facts later say; and, on a card
// the owner placed, "Follow observed" to hand it back to the rules.
function cardMenuItems(card, board, move) {
  const current = card.closest('.board-column')?.dataset.column;
  const open = cardOpenItem(card);
  const items = (open ? [open] : []).concat([...boardColumns(board)].map(column => column.dataset.column)
    .filter(column => column && column !== current)
    .map(column => ({ label: column, run: () => move(card, column) })));
  if (!board.dataset.rules) return items;
  if (current) items.push({ label: 'Pin here', run: () => pinCard(card) });
  if (card.dataset.placedBy === 'owner') items.push({ label: 'Follow observed', run: () => releaseCard(card) });
  return items;
}

// pinCard writes the card's own column as the owner's tag and retracts
// nothing, so it is the same write whether a rule or the owner had put the
// card there (plan §2.3). `write` is a seam for the node tests.
export async function pinCard(card, write = changeSessionTags) {
  const { groupKey, fromColumn } = cardCoordinates(card);
  const columnHost = card.closest('.board-column');
  if (!groupKey || !fromColumn || !columnHost) return;
  try {
    await write([{ runtime: card.dataset.runtime, id: card.dataset.sessionId }], { apply: [{ key: groupKey, value: fromColumn }], retract: [] });
  } catch (err) {
    announce('Pin failed: ' + (err && err.message ? err.message : 'the tag write failed'));
    return;
  }
  announce('Pinned in ' + fromColumn);
  document.dispatchEvent(new CustomEvent('cg:board-moved'));
  await loadColumn(columnHost);
  // The menu that held focus is gone; it returns to the card it was for.
  if (!document.activeElement || document.activeElement === document.body) focusShown(columnHost.closest('.board'), card, columnHost);
}

// ownerValuesUnder are the values the owner's tags give a session under one
// key, from the session's full tag list: a row's own tags are capped for
// display and may leave some out.
function ownerValuesUnder(answer, groupKey) {
  const key = foldGroupKey(groupKey);
  const values = [];
  for (const session of (answer && answer.sessions) || []) {
    for (const tag of session.tags || []) {
      if (tag.provenance === 'user-asserted' && foldGroupKey(tag.key || '') === key && tag.value) values.push(tag.value);
    }
  }
  return values;
}

// releaseCard hands a card the owner placed back to the board's rules: it
// retracts every value the owner's tags give the session under the board's
// key, in one write, and the board is read again, since only the daemon
// knows where the rules put it. `read` and `write` are seams for the tests.
export async function releaseCard(card, { read = loadSessionTags, write = changeSessionTags } = {}) {
  const board = card.closest('.board');
  const { groupKey } = cardCoordinates(card);
  if (!board || !groupKey) return;
  const session = { runtime: card.dataset.runtime, id: card.dataset.sessionId };
  let left = [];
  try {
    const retract = ownerValuesUnder(await read(session), groupKey).map(value => ({ key: groupKey, value }));
    if (retract.length) left = ownerValuesUnder(await write([session], { apply: [], retract }), groupKey);
  } catch (err) {
    announce('Release failed: ' + (err && err.message ? err.message : 'the tag write failed'));
    return;
  }
  announce(left.length ? 'This card could not be released: it is still placed by your tag.'
    : 'Released: the card follows what was observed.');
  document.dispatchEvent(new CustomEvent('cg:board-moved'));
  const from = card.closest('.board-column');
  const key = cardKey(card.dataset.runtime, card.dataset.sessionId);
  await refreshBoard(board);
  focusAfterRelease(board, key, from);
}

// Focus follows the released card; if it left the board, it goes to the
// first card of the column it was in. The owner's own focus is never taken.
function focusAfterRelease(board, key, from) {
  if (document.activeElement && document.activeElement !== document.body && document.activeElement.isConnected) return;
  const moved = [...board.querySelectorAll('.board-card')].find(card => cardKey(card.dataset.runtime, card.dataset.sessionId) === key);
  focusShown(board, moved || null, from);
}

// refreshBoard reads the board again in place (plan §2.3): one rail read
// under the query the board was drawn from, then the columns are brought to
// what it says — new ones added, each one's page read and reconciled — with
// no card rebuilt that is still there. One refresh runs at a time and a
// second request is folded into one more; while a card is being dragged the
// refresh waits for the drag to end, as a column's page does. `read` is a
// seam for the node tests.
export function refreshBoard(host, read = api) {
  const source = boardSources.get(host);
  if (!source) return Promise.resolve();
  if (dragState.card && host.contains(dragState.card)) { source.deferred = true; return Promise.resolve(); }
  if (source.refresh) { source.again = true; return source.refresh; }
  source.refresh = (async () => {
    try {
      do { source.again = false; await readBoard(host, source, read); } while (source.again);
    } finally { source.refresh = null; }
  })();
  return source.refresh;
}

async function readBoard(host, source, read) {
  const url = organizedRailUrl(source.query, 500, source.view.id);
  if (!url) return;
  // Where focus was, if on a card: a refresh that moves that card must not
  // leave the keyboard nowhere.
  const focused = document.activeElement?.closest?.('.board-card');
  const held = focused && host.contains(focused)
    ? { key: cardKey(focused.dataset.runtime, focused.dataset.sessionId), from: focused.closest('.board-column') } : null;
  let data = null;
  try { data = await read(url); } catch { /* each column's own read says what failed */ }
  const present = new Map([...boardColumns(host)].map(column => [column.dataset.column, column]));
  const loads = [];
  for (const column of data ? boardColumnList(source.board, data.repositories || []) : []) {
    const existing = present.get(column.key);
    if (existing) { loads.push(loadColumn(existing)); present.delete(column.key); continue; }
    // A new column goes after the others and before the unplaced one. It
    // reads under the board's query and its first read is waited for too.
    const made = renderColumn(column, source.view, source.board, source.query);
    const unplaced = [...boardColumns(host)].find(other => other.dataset.unplaced);
    host.insertBefore(made, column.unplaced ? null : unplaced || host.querySelector('.board-menu') || null);
    loads.push(columnLoads.get(made)?.running);
  }
  // Columns the read did not name read their own page: an emptied one that
  // is not declared takes itself off the board, and when the board's read
  // failed every column says so itself.
  for (const gone of present.values()) loads.push(loadColumn(gone));
  await Promise.all(loads);
  settleBoard(host);
  if (held) focusAfterRelease(host, held.key, held.from);
}

// boardBusy reports a drag in progress on the board, for a caller about to
// redraw the surface the board is on.
export function boardBusy() { return Boolean(dragState.card); }

// openColumnMenu lists the other columns for the card. The menu is rendered
// on the board, placed under the card, never inside it: the card is a
// draggable button whose click opens the session, and a menu inside it would
// be a button in a button whose every click also opened the session (PO-17).
// One menu at a time; Escape, a click or key press elsewhere, or a choice
// closes it and removes every listener it added. `move` is a seam for tests.
export function openColumnMenu(card, move = moveCard) {
  const board = card.closest('.board');
  if (!board) return;
  for (const open of board.querySelectorAll('.board-menu')) open.closeMenu?.();
  hideCardOpen(board);
  const menu = el('div', 'board-menu');
  menu.setAttribute('role', 'menu');
  menu.setAttribute('aria-label', 'Card actions');
  for (const choice of cardMenuItems(card, board, move)) {
    const item = el('button', 'board-menu-item', choice.label);
    item.type = 'button';
    item.setAttribute('role', 'menuitem');
    item.onclick = () => { close(Boolean(choice.refocus)); choice.run(); };
    menu.appendChild(item);
  }
  const cardRect = card.getBoundingClientRect();
  const boardRect = board.getBoundingClientRect();
  menu.style.top = (cardRect.bottom - boardRect.top + board.scrollTop) + 'px';
  menu.style.left = (cardRect.left - boardRect.left + board.scrollLeft) + 'px';
  menu.closeMenu = () => close(false);
  menu.forCard = card;
  board.appendChild(menu);
  document.addEventListener('keydown', onKey, true);
  document.addEventListener('pointerdown', onPointer, true);
  menu.querySelector('button')?.focus();

  function close(refocus) {
    menu.remove();
    document.removeEventListener('keydown', onKey, true);
    document.removeEventListener('pointerdown', onPointer, true);
    if (refocus) focusShown(board, card);
  }
  function onPointer(event) {
    if (!menu.contains(event.target)) close(false);
  }
  function onKey(event) {
    if (event.key === 'Escape') {
      event.preventDefault();
      event.stopPropagation();
      close(true);
      return;
    }
    if (!menu.contains(event.target)) { close(false); return; }
    const buttons = [...menu.querySelectorAll('button')];
    const index = buttons.indexOf(document.activeElement);
    if (event.key === 'ArrowDown') { event.preventDefault(); buttons[(index + 1) % buttons.length]?.focus(); }
    if (event.key === 'ArrowUp') { event.preventDefault(); buttons[(index - 1 + buttons.length) % buttons.length]?.focus(); }
  }
}
