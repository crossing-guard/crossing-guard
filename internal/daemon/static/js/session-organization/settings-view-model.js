// The Session views page's model (session-views-rebuild plan §3.4): what a
// view's row and read page say, and the draft an edit works on. Pure: no DOM,
// no request. The page never parses a filter and never names a view, a column
// or a key; every one is the owner's.
import { copyView, boardKey, setBoard } from './settings-view-board.js';
import { foldGroupKey } from './view-group-source.js';

const TAG_KEY = 'tag-key:';
// The key the one unfinished new view's draft is kept under.
export const NEW_DRAFT = 'new';
export const SORTS = [['longest', 'longest here first'], ['newest', 'newest first'], ['oldest', 'oldest first']];
const isTagGroup = value => String(value || '').toLowerCase().startsWith(TAG_KEY);

// shownAs says how a view is drawn, from what it stores.
export function shownAs(view) {
  const group = view.group_by || 'repository';
  return [view.board ? 'Board' : 'List', 'by ' + group.replace(/^tag-key:/, 'tag '), (view.sort || 'newest') + ' first']
    .concat(view.board?.columns?.length ? [view.board.columns.join(', ')] : []).join(' · ');
}

export function sortLabel(sort) {
  const found = SORTS.find(([value]) => value === (sort || 'newest'));
  return found ? found[1] : String(sort);
}

// groupLabel names a grouping as the index and the read page show it.
export function groupLabel(groupBy) {
  const value = String(groupBy || '');
  if (isTagGroup(value)) return 'tag ' + value.slice(TAG_KEY.length);
  return value || 'repository';
}

export function isMemoryView(view) {
  return view?.record_kind === 'memory';
}

// indexRow is one view's line on the index. The count is blank where the
// daemon gives none, and for a memory view, whose records are not sessions.
export function indexRow(view, counts, dirty) {
  const memory = isMemoryView(view);
  return {
    id: view.id, name: view.name || '(unnamed)', query: view.query || '',
    kind: memory ? 'Memory' : view.board ? 'Board' : 'List',
    columns: view.board ? (view.board.columns || []).length : 0,
    groupedBy: groupLabel(view.group_by),
    count: !memory && counts && view.id in counts ? String(counts[view.id]) : '',
    dirty: Boolean(dirty),
  };
}

// draftOf makes the draft an edit works on. `view` is the page's own copy;
// only commitOf's projection of it is ever sent. The list's grouping and the
// board's key are held apart, so switching how the view is shown loses neither.
export function draftOf(stored) {
  const view = stored ? copyView(stored) : { name: '', query: '', sort: 'longest' };
  const groupBy = String(view.group_by || '');
  return {
    base: stored ? JSON.stringify(stored) : '',
    view,
    shownAs: view.board ? 'board' : 'list',
    listGroupBy: groupBy,
    boardKey: boardKey(view),
    storedGroupBy: groupBy,
  };
}

// showAs switches a draft between list and board. A board left for a list
// stays in the draft until Save, so switching back finds it as it was.
export function showAs(draft, kind) {
  draft.shownAs = kind;
  if (kind === 'board' && !draft.view.board) setBoard(draft.view, true);
}

// A grouping the owner did not change is committed as it was stored, its
// prefix's letter case included, so an untouched view is never rewritten.
function boardGroupBy(draft) {
  const key = String(draft.boardKey || '').trim();
  const stored = draft.storedGroupBy;
  if (isTagGroup(stored) && stored.slice(TAG_KEY.length) === key) return stored;
  return TAG_KEY + key;
}

// commitOf is the view a draft would save. It never changes the draft: a
// refused save has lost nothing.
export function commitOf(draft) {
  const copy = copyView(draft.view);
  let groupBy = draft.listGroupBy;
  if (draft.shownAs === 'board') {
    if (!copy.board) setBoard(copy, true);
    groupBy = boardGroupBy(draft);
  } else {
    setBoard(copy, false);
  }
  if (groupBy) copy.group_by = groupBy;
  else delete copy.group_by;
  return copy;
}

// stable spells a value with its keys in order, so two views that hold the
// same fields compare equal however they were built.
function stable(value) {
  if (Array.isArray(value)) return '[' + value.map(stable).join(',') + ']';
  if (value && typeof value === 'object') {
    return '{' + Object.keys(value).sort().map(key => JSON.stringify(key) + ':' + stable(value[key])).join(',') + '}';
  }
  return JSON.stringify(value);
}

// isDirty compares what the draft would save with what a fresh draft of the
// stored view would save: never the draft with the stored JSON, since a copy
// spells an empty rule list the file leaves out.
export function isDirty(draft, stored) {
  return stable(commitOf(draft)) !== stable(commitOf(draftOf(stored)));
}

// baseView is the stored view a draft was made from (undefined for a new one).
export function baseView(draft) {
  return draft.base ? JSON.parse(draft.base) : undefined;
}

// draftChanged reports an edit the owner made: the draft against its own base.
export function draftChanged(draft) {
  return isDirty(draft, baseView(draft));
}

// saveState says whether Save may be pressed, and what to say when not.
export function saveState(draft, stored, blocked) {
  if (blocked) return { ready: false, words: 'Saving is off until the unreadable entry is fixed' };
  const named = Boolean(String(draft.view.name || '').trim()) && Boolean(draft.view.query);
  if (!named) return { ready: false, words: 'A name and a filter are needed' };
  if (draft.shownAs === 'board' && !String(draft.boardKey || '').trim()) return { ready: false, words: 'A board needs a tag key for your moves' };
  if (!isDirty(draft, stored)) return { ready: false, words: 'No changes' };
  return { ready: true, words: stored ? 'Unsaved changes' : 'Ready to create' };
}

// baseState says what happened to the stored view since the draft was made:
// '' (nothing), 'changed' (edited elsewhere) or 'gone' (deleted elsewhere).
export function baseState(draft, stored) {
  if (!draft.base) return '';
  if (!stored) return 'gone';
  return JSON.stringify(stored) === draft.base ? '' : 'changed';
}

// rebase keeps the owner's edit over a view that changed elsewhere: the
// form's fields from the draft, the fields the form does not show from the
// stored view.
export function rebase(draft, stored) {
  const next = draftOf(stored);
  for (const field of ['name', 'query', 'sort']) {
    if (field in draft.view) next.view[field] = draft.view[field];
    else delete next.view[field];
  }
  const edited = copyView(draft.view).board;
  if (edited) next.view.board = edited;
  else delete next.view.board;
  next.shownAs = draft.shownAs;
  next.listGroupBy = draft.listGroupBy;
  next.boardKey = draft.boardKey;
  return next;
}

// asNewDraft turns the draft of a view deleted elsewhere into a new view's.
export function asNewDraft(draft) {
  const view = copyView(draft.view);
  delete view.id;
  return { ...draft, base: '', view, storedGroupBy: '' };
}

// duplicateOf is a whole stored view under another name and no id: every
// stored field goes with it.
export function duplicateOf(stored, name) {
  const copy = JSON.parse(JSON.stringify(stored));
  delete copy.id;
  copy.name = name;
  return copy;
}

// groupChoiceSets splits groupByChoices' answer for the select: the
// mechanical groupings, the tag keys the owner applies, and every other key.
export function groupChoiceSets(choices, ownerTags) {
  const mine = new Set((ownerTags || []).map(tag => String(tag.key || '').toLowerCase()).filter(Boolean));
  const sets = { basic: [], yours: [], other: [] };
  for (const choice of choices) {
    const value = String(choice[0]);
    if (!isTagGroup(value)) sets.basic.push(choice);
    else if (mine.has(value.slice(TAG_KEY.length).toLowerCase())) sets.yours.push(choice);
    else sets.other.push(choice);
  }
  return sets;
}

// repositoryName shows a repository group by its folder's own name.
function repositoryName(key) {
  const parts = String(key || '').split('/').filter(Boolean);
  return parts.length ? parts[parts.length - 1] : String(key || '');
}

// lineLabel names one group of a read. A tag group with no value holds the
// sessions with no tag under that key.
function lineLabel(view, group) {
  if (group.label) return group.label;
  if (isTagGroup(view.group_by)) return group.key || 'no ' + String(view.group_by).slice(TAG_KEY.length);
  if (!view.group_by || view.group_by === 'repository') return repositoryName(group.key);
  return group.key || 'all';
}

// sessionsNow reads a view's rail answer for the "Sessions now" card. A
// board's declared columns come first, in the owner's order; a column the
// answer does not hold is 0 only when no group was cut, else unknown (null).
export function sessionsNow(view, read) {
  const groups = Array.isArray(read?.repositories) ? read.repositories : [];
  const more = Math.max(0, Number(read?.repository_total || 0) - groups.length);
  const total = Number.isInteger(read?.match_total) ? read.match_total : null;
  if (!view.board) return { total, more, lines: groups.map(group => ({ label: lineLabel(view, group), count: group.total })) };
  const byKey = new Map(groups.map(group => [group.key, group]));
  const lines = (view.board.columns || []).map(column => {
    const group = byKey.get(foldGroupKey(String(column).trim()));
    byKey.delete(foldGroupKey(String(column).trim()));
    return { label: column, count: group ? group.total : more ? null : 0 };
  });
  for (const group of byKey.values()) lines.push({ label: lineLabel(view, group), count: group.total });
  return { total, more, lines };
}
