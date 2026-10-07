// Shared view behaviors used by both surfaces that manage the owner's views:
// the rail's view list (view-list.js) and the Settings › Views page
// (settings-views.js). Pure where it can be, so the .test.mjs suite can pin
// it under node. No DOM is built here; helpers take and return plain values.

// moveOrder computes a new id order after moving one view by one step.
// Returns null when the move would fall outside the list (no-ops must not
// become 409-generating writes).
export function moveOrder(order, id, step) {
  const from = order.indexOf(id);
  const to = from + step;
  if (from < 0 || to < 0 || to >= order.length) return null;
  const next = [...order];
  next.splice(to, 0, next.splice(from, 1)[0]);
  return next;
}

// The one delete sentence, shared so the rail menu and the settings page
// cannot drift apart in what they promise.
export function confirmDeleteText(name) {
  return 'Delete the view \u201c' + (name || 'unnamed') + '\u201d? No session is changed.';
}

// renameSettle builds the commit/abort decision for an in-place rename field.
// Enter commits a changed, non-empty name; Escape and blur keep the old one.
// Returns { commit: boolean, name: string }.
export function renameSettle(current, typed) {
  const name = String(typed || '').trim();
  return { commit: Boolean(name) && name !== current, name };
}

// recoverySuffix is what follows a refused write's own message: a 400 keeps
// the edit on the card; a conflict (409) reloads the list. Anything else is
// reported without a promise about what survived.
export function recoverySuffix(status) {
  if (status === 400) return ' \u2014 the edit is kept; fix it and save again.';
  if (status === 409) return ' \u2014 the list was reloaded from the file; repeat the change.';
  return '';
}
// viewCommit is what a view write sends: the whole view the surface holds,
// with the name trimmed; a field the view leaves out stays out, so a save
// never adds defaults to the owner's file. The daemon's PUT replaces the stored
// view, so a field left out here is deleted on disk — a board's columns, a
// memory view's record_kind (settings-views-full-view-save plan §2).
export function viewCommit(view) {
  return { ...view, name: String(view.name || '').trim(), query: view.query || '' };
}

const TAG_KEY = 'tag-key:';
const MECHANICAL_GROUPINGS = [['', 'repository'], ['runtime', 'runtime'], ['none', 'none']];

// groupingIdentity folds a group_by value: case never matters, and '' and
// 'repository' are two spellings of the default grouping.
function groupingIdentity(value) {
  const folded = String(value || '').toLowerCase();
  return folded === 'repository' ? '' : folded;
}

// groupByChoices lists the groupings, as [value, label] pairs: the mechanical
// ones, then every tag key present on the owner's sessions (`known` from the
// tag vocabulary) — never a key we thought of. Each of currents (the stored
// and the edited value of a view; undefined is skipped) is always offered in
// its own spelling, so a select showing it is never blank, an untouched Save
// never regroups, and a refused regroup can be switched back.
export function groupByChoices(known, ...currents) {
  const choices = MECHANICAL_GROUPINGS.map(choice => [...choice]);
  for (const tag of known || []) {
    const key = String(tag.key || '').toLowerCase();
    if (key && !choices.some(([value]) => value === TAG_KEY + key)) choices.push([TAG_KEY + key, 'tag key ' + key]);
  }
  for (const current of currents) {
    if (current === undefined) continue;
    const at = choices.findIndex(([value]) => groupingIdentity(value) === groupingIdentity(current));
    if (at >= 0) choices[at][0] = current;
    else choices.push([current, groupingIdentity(current).startsWith(TAG_KEY) ? 'tag key ' + current.slice(TAG_KEY.length) : current]);
  }
  return choices;
}
