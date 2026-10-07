// A view's board on the Settings › Session views page (board-observed-columns
// plan §2.6, session-views-rebuild plan §3.3-3.4): what the read page says
// about it, and the section of the edit form that names its key, its columns
// and which sessions go where when the owner has not placed them. The form
// edits the draft's own copy of the view; Save writes it, and the daemon alone
// decides whether it is valid. Nothing here names a column, a key or a rule:
// every one is the owner's.
import { el, api } from '../core.js';
import { button, linkButton, chip, field, checkbox, selectBox, textInput } from '../orchestration/agents/agent-ui.js';
import { organizedRailUrl, foldGroupKey } from './view-group-source.js';
import { queryNotesNode } from './query-bar.js';

const TAG_KEY = 'tag-key:';

// copyView is a view with a board of its own: a card edits columns and rules
// in place, and must never reach the view the rail and the board read.
export function copyView(view) {
  const copy = { ...view };
  if (view.board) {
    const columns = [...(view.board.columns || [])];
    // A rule may name its column in another letter case; on the card it
    // holds the column exactly as declared, so the two are tied by one text.
    const declared = new Map(columns.map(column => [foldGroupKey(String(column).trim()), column]));
    copy.board = { ...view.board, columns,
      placement: (view.board.placement || []).map(rule => ({ ...rule, column: declared.get(foldGroupKey(String(rule.column || '').trim())) ?? rule.column })) };
  }
  return copy;
}

// boardKey is the tag key a board view's moves are written under.
export function boardKey(view) {
  const groupBy = String(view.group_by || '');
  return groupBy.toLowerCase().startsWith(TAG_KEY) ? groupBy.slice(TAG_KEY.length) : '';
}

// setBoard turns the board on or off. Off removes the board alone: the
// grouping stays, since a list may be grouped by the same key.
export function setBoard(view, on) {
  if (on) view.board = view.board || { columns: [], empty_columns: true, placement: [] };
  else delete view.board;
}

// renameColumn renames a column and every rule that named it, so a rename
// never leaves a rule pointing at a column the board no longer declares.
// A rule is tied to its column by the column's exact text, an empty name
// included, so typing a name letter by letter carries the rules along. While
// two columns have the same text the tie is ambiguous and no rule is moved.
export function renameColumn(board, index, name) {
  const before = board.columns[index];
  const shared = board.columns.filter(column => column === before).length > 1;
  board.columns[index] = name;
  if (shared) return;
  for (const rule of board.placement || []) {
    if (rule.column === before) rule.column = name;
  }
}

// columnInUse reports a column a rule still names: it cannot be removed
// until the rule is moved to another column or deleted.
export function columnInUse(board, index) {
  return (board.placement || []).some(rule => rule.column === board.columns[index]);
}

// moveItem moves one entry of a list one place up or down, in place.
export function moveItem(list, index, delta) {
  const to = index + delta;
  if (to < 0 || to >= list.length) return;
  list.splice(to, 0, ...list.splice(index, 1));
}

// What the last board read said about each saved board view's rules: how
// many of the view's sessions each rule placed, and the notes on its query.
// Kept with the rules it was read for, so an edited rule shows no number.
const placementReads = new Map();

// loadPlacementReads reads each saved board view that has rules, once. The
// page calls it when it paints and after a save, not on every focus.
export async function loadPlacementReads(views) {
  const reads = views.filter(view => view.board && (view.board.placement || []).length).map(async view => {
    const active = { query: view.query || '', groupBy: view.group_by || '', sort: view.sort || '' };
    try {
      const data = await api(organizedRailUrl(active, 1, view.id));
      placementReads.set(view.id, { rules: rulesText(view), counts: data.placement_counts || [], notes: data.placement_notes || [] });
    } catch { placementReads.delete(view.id); }
  });
  await Promise.all(reads);
}

// rulesText spells a view's rules as a draft holds them (each rule's column in
// its declared spelling), so a read made for the saved view is still the
// draft's until a rule is edited.
function rulesText(view) {
  return JSON.stringify(copyView(view).board.placement);
}

// ruleRead is what the last read said about one rule of a view, or null when
// the rules were edited since or the view was never read.
function ruleRead(view, index) {
  const read = placementReads.get(view.id);
  if (!read || read.rules !== rulesText(view)) return null;
  return { count: read.counts[index], notes: (read.notes.find(entry => entry.rule === index) || {}).notes || [] };
}

// Who places a board's card, first to last. Built here once, so the read page
// and the edit form cannot disagree, and a later step is added in one place.
const PLACEMENT_ORDER = [['Your placement', 'wins'], ['the first rule that matches', 'then'], ['unplaced', 'then']];

function placementOrderNode() {
  const line = el('div', 'views-order');
  PLACEMENT_ORDER.forEach(([who, word], index) => {
    if (index) line.append(el('span', 'views-order-step', '›'), document.createTextNode(word + ' '));
    line.appendChild(el('strong', '', who));
    if (!index) line.appendChild(document.createTextNode(' ' + word));
  });
  return line;
}

let dragged = null; // { group, index } while a grip is being dragged

// reorderRow makes one row of a list movable: its grip dragged onto another
// row of the same list, or Alt+ArrowUp / Alt+ArrowDown while focus is in the
// row. `move(from, to)` is told only about a move inside the list.
export function reorderRow(row, grip, group, index, count, move) {
  grip.draggable = true;
  grip.title = 'Drag to reorder, or press Alt+Up / Alt+Down in this row';
  grip.addEventListener('dragstart', event => {
    dragged = { group, index };
    if (event.dataTransfer) event.dataTransfer.setData('text/plain', String(index));
  });
  grip.addEventListener('dragend', () => { dragged = null; });
  row.addEventListener('dragover', event => { if (dragged?.group === group) event.preventDefault(); });
  row.addEventListener('drop', event => {
    if (dragged?.group !== group) return;
    event.preventDefault();
    const from = dragged.index;
    dragged = null;
    if (from !== index) move(from, index);
  });
  row.addEventListener('keydown', event => {
    // Alt+Arrow in a select opens it; the row leaves that key alone.
    if (!event.altKey || event.target.tagName === 'SELECT') return;
    const step = event.key === 'ArrowUp' ? -1 : event.key === 'ArrowDown' ? 1 : 0;
    if (!step) return;
    event.preventDefault();
    if (index + step >= 0 && index + step < count) move(index, index + step);
  });
}

function removeMark(title, disabled, run) {
  const node = button('✕', 'ghost views-remove', run);
  node.title = title;
  node.setAttribute('aria-label', title);
  node.disabled = disabled;
  return node;
}

function grip() {
  const node = el('span', 'views-grip', '⠿');
  node.setAttribute('aria-hidden', 'true');
  return node;
}

// One column of the board: its name, and its remove mark, off while a rule
// still places sessions in it.
function columnRow(draft, ctx, index) {
  const board = draft.view.board;
  const row = el('div', 'views-item views-item-col');
  row.dataset.focus = 'col-' + index;
  const handle = grip();
  const name = textInput(board.columns[index], 'Column ' + (index + 1) + ' name');
  name.disabled = ctx.blocked;
  name.oninput = () => { renameColumn(board, index, name.value); ctx.changed(); };
  // Once the name is settled the rules' column lists show it. They are
  // refilled where they stand: a redraw here would replace the button the
  // owner is in the middle of pressing.
  name.onchange = () => refillRuleColumns(row.closest('.agents-card'), board);
  const used = columnInUse(board, index);
  row.append(handle, name, el('span', 'views-item-note', used ? 'a rule places sessions here' : ''),
    removeMark(used ? 'A rule places sessions in this column; change or remove the rule first' : 'Remove column', ctx.blocked || used,
      () => { board.columns.splice(index, 1); ctx.redraw(''); }));
  reorderRow(row, handle, 'columns', index, board.columns.length, (from, to) => { moveItem(board.columns, from, to - from); ctx.redraw('col-' + to); });
  return row;
}

// One placement rule: the column, the filter, and how many of the view's
// sessions it places now (shown only while the rules are as last read).
function ruleRow(draft, ctx, index) {
  const board = draft.view.board;
  const rule = board.placement[index];
  const row = el('div', 'views-item views-item-rule');
  row.dataset.focus = 'rule-' + index;
  const handle = grip();
  const column = selectBox(board.columns.map(name => [name, name]), rule.column, 'Column for rule ' + (index + 1));
  column.disabled = ctx.blocked;
  column.onchange = () => { rule.column = column.value; ctx.changed(); };
  const query = textInput(rule.query, 'Filter for rule ' + (index + 1));
  query.className = 'views-mono';
  query.disabled = ctx.blocked;
  query.oninput = () => { rule.query = query.value; ctx.changed(); };
  const read = ruleRead(draft.view, index);
  row.append(handle, column, query, el('span', 'views-item-count', read && Number.isInteger(read.count) ? read.count + ' now' : ''),
    removeMark('Remove rule', ctx.blocked, () => { board.placement.splice(index, 1); ctx.redraw(''); }));
  reorderRow(row, handle, 'rules', index, board.placement.length, (from, to) => { moveItem(board.placement, from, to - from); ctx.redraw('rule-' + to); });
  const notes = read && queryNotesNode(read.notes);
  return notes ? [row, notes] : [row];
}

// refillRuleColumns rewrites each rule's column list from the board's columns.
function refillRuleColumns(root, board) {
  if (!root) return;
  root.querySelectorAll('.views-item-rule select').forEach((select, index) => {
    select.replaceChildren(...board.columns.map(name => {
      const option = el('option', '', name);
      option.value = name;
      return option;
    }));
    select.value = board.placement[index]?.column ?? '';
  });
}

// showRuleCounts writes what the last read said beside each rule of a form
// already drawn, without redrawing it.
export function showRuleCounts(root, view) {
  root.querySelectorAll('.views-item-rule .views-item-count').forEach((node, index) => {
    const read = ruleRead(view, index);
    node.textContent = read && Number.isInteger(read.count) ? read.count + ' now' : '';
  });
}

function addLink(label, disabled, run) {
  const node = linkButton(label, run);
  node.disabled = disabled;
  return node;
}

function columnsField(draft, ctx) {
  const board = draft.view.board;
  const list = el('div', 'views-list');
  board.columns.forEach((_, index) => list.appendChild(columnRow(draft, ctx, index)));
  const empty = checkbox(Boolean(board.empty_columns), 'Show columns that hold no sessions');
  empty.input.disabled = ctx.blocked;
  empty.input.onchange = () => { board.empty_columns = empty.input.checked; ctx.changed(); };
  const foot = el('div', 'views-list-foot');
  foot.append(addLink('+ Add column', ctx.blocked, () => { board.columns.push(''); ctx.redraw('col-' + (board.columns.length - 1)); }), empty.node);
  const wrap = el('div', 'views-field');
  wrap.append(el('span', 'agents-field-label', 'Columns, left to right'), list, foot);
  return wrap;
}

function rulesField(draft, ctx) {
  const board = draft.view.board;
  board.placement = board.placement || [];
  const list = el('div', 'views-list');
  board.placement.forEach((_, index) => list.append(...ruleRow(draft, ctx, index)));
  const foot = el('div', 'views-list-foot');
  const hint = el('span', 'agents-sub');
  hint.append('A rule writes nothing. It cannot use ', el('code', '', 'status:'), ', ', el('code', '', 'open:'), ' or plain words.');
  foot.append(addLink('+ Add rule', ctx.blocked || !board.columns.length,
    () => { board.placement.push({ column: board.columns[0], query: '' }); ctx.redraw('rule-' + (board.placement.length - 1)); }), hint);
  const wrap = el('div', 'views-field');
  wrap.append(el('span', 'agents-field-label', 'Placement rules'), placementOrderNode(), list, foot);
  return wrap;
}

// boardEditor is the Board section of the edit form. `ctx.changed` tells the
// form its draft was edited; `ctx.redraw(focus)` redraws the form after a
// change to the section's structure and puts focus in the named row.
export function boardEditor(draft, ctx) {
  const key = textInput(draft.boardKey, 'Tag key the board writes moves under');
  key.className = 'views-mono';
  key.disabled = ctx.blocked;
  const moves = el('div', 'agents-sub');
  const say = () => {
    const name = key.value.trim();
    moves.replaceChildren(name ? 'Moving a card writes your tag ' : 'A board needs a tag key: moving a card writes your tag under it.');
    if (name) moves.appendChild(el('code', '', name + '=<column>'));
  };
  key.oninput = () => { draft.boardKey = key.value; say(); ctx.changed(); };
  say();
  const keyField = field('Tag key for your moves', key);
  keyField.classList.add('views-key-field');
  keyField.appendChild(moves);
  const how = el('div', 'agents-sub', 'To reorder columns or rules, drag a row by its grip, or press Alt+Up or Alt+Down in the row.');
  return [keyField, how, columnsField(draft, ctx), rulesField(draft, ctx)];
}

// The board's rows on a view's read page: its columns in order, the tag a
// move writes, and who places a card.
export function boardFacts(view) {
  const board = view.board;
  const columns = el('div', 'views-chips');
  (board.columns || []).forEach((name, index) => {
    if (index) columns.appendChild(el('span', 'views-order-step', '›'));
    columns.appendChild(chip(name));
  });
  if (!(board.columns || []).length) columns.appendChild(el('span', 'agents-sub', 'None declared; a column appears for each value in use.'));
  const columnsCell = el('div', 'views-stack');
  columnsCell.append(columns, el('div', 'agents-sub', board.empty_columns ? 'Empty columns are shown.' : 'Empty columns are hidden.'));
  const moves = el('span', '', 'Moving a card writes your tag ');
  moves.appendChild(el('code', '', boardKey(view) + '=<column>'));
  return [['Columns', columnsCell], ['Moves', moves], ['Placement', placementFacts(view)]];
}

function placementFacts(view) {
  const rules = view.board.placement || [];
  const cell = el('div', 'views-stack');
  if (!rules.length) {
    cell.appendChild(el('span', '', 'No rules. A card is in the column you put it in; a session you have not placed is in a last column of its own.'));
    return cell;
  }
  cell.appendChild(placementOrderNode());
  const list = el('ol', 'views-rules');
  rules.forEach((rule, index) => {
    const item = el('li');
    const read = ruleRead(view, index);
    item.append(chip(rule.column), el('code', '', rule.query), el('span', 'agents-sub', read && Number.isInteger(read.count) ? read.count + ' now' : ''));
    list.appendChild(item);
    const notes = read && queryNotesNode(read.notes);
    if (notes) list.appendChild(notes);
  });
  cell.appendChild(list);
  return cell;
}
