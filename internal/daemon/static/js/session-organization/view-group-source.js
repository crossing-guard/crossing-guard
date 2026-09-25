// URLs and keys for a filtered rail. With no active query every function here
// answers "nothing", and views/sessions.js builds the requests it always built.
const enc = encodeURIComponent;

function queryParams(active) {
  return '&query=' + enc(active.query || '') + '&group_by=' + enc(active.groupBy || 'repository')
    + (active.sort ? '&sort=' + enc(active.sort) : '');
}

// group_by is always sent, so an empty query still selects the filtered rail.
export function organizedRailUrl(active, repositoryLimit) {
  if (!active) return '';
  return '/api/sessions?view=rail&repository_limit=' + repositoryLimit + queryParams(active);
}

export function organizedPageUrl(active, page) {
  if (!active) return '';
  const selected = page.selected
    ? '&selected_runtime=' + enc(page.selected.runtime) + '&selected_id=' + enc(page.selected.id) : '';
  return '/api/sessions?view=group&group=' + enc(page.key) + '&mode=' + page.mode
    + '&offset=' + page.offset + '&limit=' + page.limit + queryParams(active) + selected;
}

// A view's groups remember their collapsed state apart from the repositories',
// and apart from each other, even when two views share a group name.
export function railStateKey(active, key) {
  return active ? [active.scope, active.groupBy || 'repository', key].join(' :: ') : key;
}

export function groupsAreRepositories(active) {
  return !active || !active.groupBy || active.groupBy === 'repository';
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
