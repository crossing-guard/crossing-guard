// The filter bar is the existing search box with a grammar. Plain words are
// today's search of every transcript, untouched. A term with a colon filters.
// A filter the daemon cannot read is reported under the bar in its own words,
// and the list below is left exactly as it was.
import { $, el, api } from '../core.js';
import { createView, updateView } from './organization-api.js';
import { organization, activeQuery, isStructuredQuery, selectedView, selectView } from './organization-state.js';
import { organizedRailUrl } from './view-group-source.js';

let renderRail = () => {};
let inputGeneration = 0; // a slow answer to an older keystroke must not undo a newer one

export function configureQueryBar(options) {
  renderRail = options.renderRail;
}

const statusHost = () => $('#searchstatus');

// Only a problem is announced. The count line changes on every pause in typing
// and would otherwise be read out again each time.
function showProblem(err) {
  const problem = el('div', 'searchstatus-problem', err.message || String(err));
  problem.setAttribute('role', 'alert');
  statusHost().replaceChildren(problem);
}

// A filter typed over a selected view never overwrites that view: only a view
// opened with "Edit filter" is updated; anything else saves as a new one.
// handleBarInput reports whether the text was a filter (and so handled here).
// Anything else is the caller's: plain words search everything, as always.
export async function handleBarInput(text) {
  const query = String(text || '').trim();
  const generation = ++inputGeneration;
  if (!isStructuredQuery(query)) {
    const had = Boolean(organization.barQuery);
    organization.barQuery = '';
    statusHost().replaceChildren();
    return had && !query ? (renderRail(), true) : false;
  }
  const previous = organization.barQuery;
  organization.barQuery = query;
  try {
    const rail = await api(organizedRailUrl(activeQuery(), 500));
    if (generation !== inputGeneration) return true;
    paintStatus(rail);
    renderRail({ prefetched: rail }); // the rail draws what was just fetched; no second request
  } catch (err) {
    if (generation !== inputGeneration) return true;
    organization.barQuery = previous;
    showProblem(err);
  }
  return true;
}

// loadViewIntoBar puts a view's filter in the bar for editing.
export function loadViewIntoBar(view) {
  selectView(view.id);
  organization.editingID = view.id;
  organization.barGroupBy = view.group_by || '';
  $('#search').value = view.query || '';
  $('#search').focus();
  if (view.query) handleBarInput(view.query);
}

// clearBar empties the bar and returns the rail to the selected view, or to
// Repositories. It reports whether there was anything to clear.
export function clearBar() {
  const had = Boolean(organization.barQuery || $('#search').value);
  organization.barQuery = '';
  organization.within = false;
  organization.editingID = '';
  $('#search').value = '';
  statusHost().replaceChildren();
  return had;
}

function paintStatus(rail) {
  const total = (rail.repositories || []).reduce((sum, group) => sum + group.total, 0);
  const line = el('div', 'searchstatus-line');
  const counted = total + (total === 1 ? ' session' : ' sessions') + (rail.more_text_matches ? ' · best matches only' : '');
  line.appendChild(el('span', 'searchstatus-count', counted));
  const view = selectedView();
  const editing = organization.views.find(each => each.id === organization.editingID) || null;
  if (view && !editing) line.appendChild(withinChip(view));
  line.appendChild(groupButton());
  line.appendChild(saveButton(editing));
  statusHost().replaceChildren(line);
}

function withinChip(view) {
  const chip = el('button', 'tag-toggle' + (organization.within ? ' on' : ''), 'within “' + view.name + '”');
  chip.type = 'button';
  chip.setAttribute('aria-pressed', String(organization.within));
  chip.onclick = () => { organization.within = !organization.within; handleBarInput($('#search').value); };
  return chip;
}

// The grouping choices are the mechanical ones plus every tag key that occurs
// in what the owner has tagged — never a list of keys we thought of.
function groupButton() {
  const button = el('button', 'btn', 'Group: ' + (organization.barGroupBy || 'repository'));
  button.type = 'button';
  button.onclick = async () => {
    const choices = ['repository', 'runtime', 'none'];
    try {
      const known = (await api('/api/session-tags/vocabulary')).known || [];
      for (const tag of known) {
        const choice = tag.key ? 'tag-key:' + String(tag.key).toLowerCase() : '';
        if (choice && !choices.includes(choice)) choices.push(choice);
      }
    } catch { /* the mechanical choices remain */ }
    const at = choices.indexOf(organization.barGroupBy || 'repository');
    organization.barGroupBy = choices[(at + 1) % choices.length];
    if (organization.barGroupBy === 'repository') organization.barGroupBy = '';
    handleBarInput($('#search').value);
  };
  return button;
}

function saveButton(view) {
  const button = el('button', 'btn primary', view ? 'Update “' + view.name + '”' : 'Save view');
  button.type = 'button';
  button.onclick = () => (view ? saveBarAsView(view, view.name) : askViewName()).catch(showProblem);
  return button;
}

// askViewName swaps the status line for a name field. Enter saves, Escape
// returns to the filter; nothing leaves the page for a dialog.
async function askViewName() {
  const line = el('div', 'searchstatus-line');
  const input = el('input', 'searchstatus-name');
  input.placeholder = 'Name this view';
  input.setAttribute('aria-label', 'Name this view');
  input.maxLength = 120;
  const save = el('button', 'btn primary', 'Save');
  save.type = 'button';
  const submit = () => { if (input.value.trim()) saveBarAsView(null, input.value.trim()).catch(showProblem); };
  save.onclick = submit;
  input.addEventListener('keydown', event => {
    if (event.key === 'Enter') { event.preventDefault(); submit(); }
    if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); handleBarInput($('#search').value); }
  });
  line.append(input, save);
  statusHost().replaceChildren(line);
  input.focus();
}

// saveBarAsView writes the bar's filter to the owner's views file: over the
// view being edited, or as a new view with the name he gave.
async function saveBarAsView(view, name) {
  const query = organization.barQuery, groupBy = organization.barGroupBy;
  if (view) {
    await updateView({ ...view, query, group_by: groupBy });
    selectView(view.id);
  } else {
    await createView({ name, query, group_by: groupBy, sort: 'longest' });
    selectView(organization.views[organization.views.length - 1]?.id || '');
  }
  $('#search').value = '';
  statusHost().replaceChildren();
  renderRail();
}
