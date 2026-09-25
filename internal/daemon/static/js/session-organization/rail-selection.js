// Selecting several rail rows to tag them together. A plain click still opens
// a session, exactly as before; holding Cmd/Ctrl or Shift selects instead. The
// toolbar appears at the foot of the rail only while something is selected.
import { $, el } from '../core.js';
import { openTagPopover } from './tag-popover.js';

const selected = new Map(); // "runtime id" → session
let lastKey = '';
let onTagged = () => {};

export function configureRailSelection(options) {
  if (options.onTagged) onTagged = options.onTagged;
}

const keyOf = session => session.runtime + ' ' + session.id;

// handleRowSelectClick returns true when the click selected rather than opened.
export function handleRowSelectClick(event, row, session) {
  if (!(event.metaKey || event.ctrlKey || event.shiftKey)) {
    if (selected.size) clearRailSelection();
    return false;
  }
  event.preventDefault();
  if (event.shiftKey && lastKey) selectRange(row);
  else toggle(row, session);
  lastKey = keyOf(session);
  paintToolbar();
  return true;
}

function toggle(row, session) {
  const key = keyOf(session);
  if (selected.has(key)) selected.delete(key);
  else selected.set(key, session);
  row.classList.toggle('multi', selected.has(key));
}

function selectRange(row) {
  // Only rows on screen: a collapsed agent fold keeps its rows in the DOM, and
  // a range must never tag sessions the owner cannot see.
  const rows = [...document.querySelectorAll('#sidebody .sess')].filter(each => each.offsetParent !== null);
  const from = rows.findIndex(each => each.dataset.runtime + ' ' + each.dataset.sessionId === lastKey);
  const to = rows.indexOf(row);
  if (from < 0 || to < 0) return;
  for (const each of rows.slice(Math.min(from, to), Math.max(from, to) + 1)) {
    const session = { runtime: each.dataset.runtime, id: each.dataset.sessionId };
    selected.set(keyOf(session), session);
    each.classList.add('multi');
  }
}

// rowSessionsForTagging is what a row's context menu tags: the whole selection
// when the row is part of one, otherwise just that row.
export function rowSessionsForTagging(session) {
  return selected.has(keyOf(session)) ? [...selected.values()] : [session];
}

export function clearRailSelection() {
  selected.clear();
  lastKey = '';
  document.querySelectorAll('#sidebody .sess.multi').forEach(row => row.classList.remove('multi'));
  paintToolbar();
}

function paintToolbar() {
  let bar = $('#railbulk');
  if (!selected.size) { bar?.remove(); return; }
  if (!bar) {
    bar = el('div', 'rail-bulk');
    bar.id = 'railbulk';
    bar.setAttribute('role', 'toolbar');
    bar.setAttribute('aria-label', 'Selected sessions');
    $('#sidebody').after(bar);
  }
  const tag = el('button', 'btn', 'Tag…');
  tag.type = 'button';
  tag.onclick = () => openTagPopover(tag, [...selected.values()], updated => onTagged(updated));
  const clear = el('button', 'btn', 'Clear');
  clear.type = 'button';
  clear.onclick = clearRailSelection;
  const spacer = el('span', 'rail-bulk-spacer');
  bar.replaceChildren(el('span', '', selected.size + ' selected'), spacer, tag, clear);
}
