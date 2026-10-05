// URLs and keys for a filtered rail. With no active query every function here
// answers "nothing", and views/sessions.js builds the requests it always built.
const enc = encodeURIComponent;

// How long typing in a filter rests before it is read: the rail's bar and the
// Session views page's match line wait the same time.
export const FILTER_TYPING_PAUSE_MS = 350;

function queryParams(active) {
  return '&query=' + enc(active.query || '') + '&group_by=' + enc(active.groupBy || 'repository')
    + (active.sort ? '&sort=' + enc(active.sort) : '');
}

// group_by is always sent, so an empty query still selects the filtered rail.
// boardView is the id of the board view the read is for, or empty. The daemon
// then places rows as that board does: by the owner's tags, then by the
// view's placement rules, which it reads from the views file itself
// (board-observed-columns plan §2.2). The caller decides whether the active
// query is that view's own.
export function organizedRailUrl(active, repositoryLimit, boardView) {
  if (!active) return '';
  return '/api/sessions?view=rail&repository_limit=' + repositoryLimit + queryParams(active)
    + (boardView ? '&board_view=' + enc(boardView) : '');
}

// boardView places a page's rows as the rail read that counted the group did:
// a board's columns and a board view's rail pages both send it. A page with
// no limit takes the daemon's: 15, or with boardView the configured
// board_column_cards_max, which the browser never needs to know
// (board-column-overflow plan §2.2).
export function organizedPageUrl(active, page) {
  if (!active) return '';
  const selected = page.selected
    ? '&selected_runtime=' + enc(page.selected.runtime) + '&selected_id=' + enc(page.selected.id) : '';
  return '/api/sessions?view=group&group=' + enc(page.key) + '&mode=' + page.mode
    + '&offset=' + page.offset + (page.limit ? '&limit=' + page.limit : '') + queryParams(active) + selected
    + (page.boardView ? '&board_view=' + enc(page.boardView) : '');
}

// A view's groups remember their collapsed state apart from the repositories',
// and apart from each other, even when two views share a group name.
export function railStateKey(active, key) {
  return active ? [active.scope, active.groupBy || 'repository', key].join(' :: ') : key;
}

export function groupsAreRepositories(active) {
  return !active || !active.groupBy || active.groupBy === 'repository';
}

// foldGroupKey spells a name as the daemon spells a tag group's key, so a
// board column declared "Review" finds the group "review". It lowers one code
// point at a time and keeps the first code point of the result: that is Go's
// strings.ToLower, which toLowerCase() is not for "İ" or a word-final "Σ"
// (board-column-letter-case plan §2). Matching only; the daemon owns the key.
export function foldGroupKey(name) {
  return Array.from(String(name), point => Array.from(point.toLowerCase())[0]).join('');
}

// groupLabel names a group that is not a repository. A tag group with no value
// holds the sessions that carry no tag under that key.
export function groupLabel(active, group) {
  if (groupsAreRepositories(active)) return '';
  if (group.label) return group.label;
  if (group.key) return group.key;
  const key = String(active.groupBy || '').startsWith('tag-key:') ? active.groupBy.slice('tag-key:'.length) : '';
  return key ? 'no ' + key : 'all';
}
