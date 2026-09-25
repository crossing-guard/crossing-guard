// What the rail is currently showing: the repositories, one of the owner's
// saved views, or whatever the filter bar says. Views themselves are the
// owner's configuration and live on the daemon (session-views.json); the only
// thing kept in this browser is which view was last selected.
const LAST_VIEW_KEY = 'cg-session-organization';

export const organization = {
  views: [],        // saved views as the daemon returned them
  rejected: [],     // entries of the owner's file that could not be read
  stateToken: '',   // names the file bytes the views came from; every write presents it
  viewID: '',       // selected view, '' for Repositories
  barQuery: '',     // a structured filter typed into the bar
  barGroupBy: '',   // grouping chosen for the bar's filter
  within: false,    // bar filter applies inside the selected view
  editingID: '',    // the view whose filter is in the bar for editing, if any
};

// A term is field:value. Plain words alone are today's global search.
export function isStructuredQuery(text) {
  return /(^|\s)-?[A-Za-z]+:\S/.test(String(text || ''));
}

export function selectedView() {
  return organization.views.find(view => view.id === organization.viewID) || null;
}

// activeQuery is what the rail asks the daemon for, or null for the plain
// repository rail, which is requested exactly as it always has been.
export function activeQuery() {
  const view = selectedView();
  if (organization.barQuery) {
    const inside = organization.within && view;
    return {
      query: inside ? (view.query + ' ' + organization.barQuery).trim() : organization.barQuery,
      groupBy: organization.barGroupBy, sort: inside ? (view.sort || '') : '', scope: 'bar',
    };
  }
  if (view) return { query: view.query || '', groupBy: view.group_by || '', sort: view.sort || '', scope: 'view:' + view.id };
  return null;
}

export function selectView(id) {
  organization.viewID = id || '';
  organization.barQuery = '';
  organization.within = false;
  organization.editingID = '';
  try { localStorage.setItem(LAST_VIEW_KEY, JSON.stringify({ view: organization.viewID })); } catch { /* preference only */ }
}

export function recallSelectedView() {
  try {
    const saved = JSON.parse(localStorage.getItem(LAST_VIEW_KEY) || 'null');
    return typeof saved?.view === 'string' ? saved.view : '';
  } catch { return ''; }
}

export function adoptViews(document) {
  organization.views = Array.isArray(document?.views) ? document.views : [];
  organization.rejected = Array.isArray(document?.rejected) ? document.rejected : [];
  organization.stateToken = String(document?.state_token || '');
  if (organization.viewID && !selectedView()) organization.viewID = '';
}
