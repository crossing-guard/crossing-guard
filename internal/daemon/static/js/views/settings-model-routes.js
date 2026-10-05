// Model routes on Settings → Models (team rest-of-release plan §5.5): the one
// place a runtime, a model id or an inference endpoint is typed and shown. A
// route is created, renamed, edited and deleted here; agents pick one by name.
// Every write goes preview → select, so the sheet shows what the daemon will do
// — the places that follow, what refuses it — before anything is written.
import { el } from "../core.js";
import { button, row, spacer, problem, field, textInput, selectBox, keyValue, linkButton } from "../orchestration/agents/agent-ui.js";
import { openDialog } from "../orchestration/agents/agent-dialog.js";
import { modelPicker } from "../orchestration/agents/model-picker.js";
import { loadModelRoutes, previewModelRoute, selectModelRoute, deleteModelRoute } from "../orchestration/model-routes-api.js";
import {
  routeRows, unroutedPlaces, routeDraft, renameDraft, previewView, saveLabel, refusalView, usedBy,
  FAMILY_WORDS, FAMILY_MANAGED, FAMILY_REVIEW,
} from "./settings-routes-model.js";

const PREVIEW_DELAY_MS = 350;

// renderModelRoutes draws the Model routes card. `context` carries the runtime
// capabilities and names, and the roster (for agent names and places still
// waiting for a route); `reload` redraws the whole Models page after a write.
export async function renderModelRoutes(card, context, reload) {
  const create = button('New route', 'primary', () => openRouteSheet(context, reload, null));
  card.appendChild(row(el('h3', '', 'Model routes'), spacer(), create));
  let listed;
  try { listed = await loadModelRoutes(); }
  catch (error) { card.appendChild(problem('Model routes cannot be read: ' + refusalView(error).message)); return; }
  for (const item of listed.problems || []) {
    card.appendChild(problem(String(item?.problem?.message || 'A stored model route failed its check.') + ' ' + String(item?.problem?.recovery || '')));
  }
  for (const place of unroutedPlaces(context.roster)) card.appendChild(unroutedRow(place));
  if (listed.places_unavailable) card.appendChild(el('div', 'sub', 'Which places use each route cannot be read right now.'));
  const rows = routeRows(listed, context);
  if (!rows.length) {
    card.append(el('div', 'sub', 'No model routes yet.'),
      el('div', 'sub', 'A model route names where a model call goes. Agents pick one by name.'));
    return;
  }
  card.appendChild(routeTable(rows, context, reload));
}

// unroutedRow names a place that has no route yet, or whose route is missing,
// with the way to choose one.
function unroutedRow(place) {
  const node = el('div', 'settings-route-wait');
  node.dataset.modelRow = '1';
  const open = linkButton('Choose route…', () => {
    document.dispatchEvent(new CustomEvent('cg:agents-open', { detail: { id: place.agentID, tab: 'places' } }));
    document.dispatchEvent(new CustomEvent('cg:nav', { detail: 'settings' }));
  });
  node.append(el('b', '', place.agent), el('span', 'sub', place.where), el('span', '', place.words), open);
  return node;
}

function routeTable(rows, context, reload) {
  const table = el('table', 'settings-table settings-routes');
  const head = el('tr');
  for (const label of ['Route', 'Runs on', 'Used by', '']) head.appendChild(el('th', '', label));
  table.appendChild(head);
  for (const item of rows) table.appendChild(routeRowNode(item, context, reload));
  return table;
}

function routeRowNode(item, context, reload) {
  const tr = el('tr');
  tr.dataset.modelRow = '1';
  const name = el('td');
  name.append(el('b', '', item.name), el('div', 'sub', item.familyWords));
  if (item.migrated) name.appendChild(el('div', 'sub', item.migrated));
  const runs = el('td');
  runs.append(...item.runsOn.map(fact => el('div', 'settings-route-runs', fact)), el('div', 'sub', item.locality));
  const used = el('td');
  if (!item.usedBy.length) used.appendChild(el('span', 'sub', 'Not used'));
  for (const group of item.usedBy) used.appendChild(usedLine(group));
  const actions = el('td', 'settings-route-actions');
  actions.append(button('Rename', 'ghost', () => openRenameSheet(item.route, reload)),
    button('Edit', 'ghost', () => openRouteSheet(context, reload, item.route)),
    button('Delete', 'ghost', () => openDeleteSheet(item.route, context, reload)));
  tr.append(name, runs, used, actions);
  return tr;
}

function usedLine(group) {
  const line = el('div', 'settings-route-used');
  line.append(el('span', '', group.agent), el('span', 'sub', group.places.join(', ')));
  return line;
}

// factLines draws facts one per line.
function factLines(facts) {
  const node = el('div', 'settings-route-facts');
  for (const fact of facts) node.appendChild(el('div', '', fact));
  return node;
}

function placesList(title, groups, note = '') {
  const node = el('div', 'settings-route-places');
  node.appendChild(el('div', 'agents-field-label', title));
  if (note) node.appendChild(el('div', 'sub', note));
  for (const group of groups) node.appendChild(usedLine(group));
  return node;
}

/* ---------- create and edit ---------- */

// routeForm builds the inputs of a create or an edit: the name, the family (a
// new route only), then the existing runtime-and-model picker with effort, or
// an endpoint on this machine and a model.
function routeForm(context, route, onChange) {
  const name = textInput(route?.name || '', 'Name', 'A name agents pick it by');
  const family = selectBox(Object.entries(FAMILY_WORDS), route?.family || FAMILY_MANAGED, 'For');
  const picker = modelPicker(context.capabilities, route?.family === FAMILY_MANAGED ? route.fields : {}, { withMode: false, onChange });
  const endpoint = textInput(route?.family === FAMILY_REVIEW ? route.fields.endpoint : '', 'Endpoint on this machine', 'http://127.0.0.1:port');
  const model = textInput(route?.family === FAMILY_REVIEW ? route.fields.model : '', 'Model');
  const local = el('div', 'agents-stack');
  local.append(field('Endpoint on this machine', endpoint), field('Model', model));
  const sync = () => {
    picker.node.classList.toggle('hidden', family.value !== FAMILY_MANAGED);
    local.classList.toggle('hidden', family.value !== FAMILY_REVIEW);
  };
  family.onchange = () => { sync(); onChange(); };
  for (const input of [name, endpoint, model]) input.oninput = onChange;
  sync();
  const node = el('div', 'agents-stack');
  node.append(field('Name', name));
  // A route keeps its family: places of the other kind could not follow a change.
  node.append(route ? keyValue([['For', FAMILY_WORDS[route.family] || route.family]]) : field('For', family));
  node.append(picker.node, local);
  const draft = () => routeDraft({ routeID: route?.route_id || '', name: name.value, family: family.value,
    managed: picker.value(), endpoint: endpoint.value, model: model.value });
  return { node, draft, name };
}

// openRouteSheet creates a route (route null) or edits one. The sheet previews
// as the person types; Save writes exactly what the last preview showed.
function openRouteSheet(context, reload, route) {
  const facts = el('div', 'settings-route-preview');
  let ticket = 0;
  let timer = null;
  let form = null;
  let dialog = null;
  const refresh = () => {
    clearTimeout(timer);
    timer = setTimeout(async () => {
      const mine = ++ticket;
      const result = await readPreview(form.draft(), context);
      if (mine === ticket && facts.isConnected) paintPreview(facts, result, dialog);
    }, PREVIEW_DELAY_MS);
  };
  form = routeForm(context, route, refresh);
  dialog = openDialog({ title: route ? 'Edit ' + route.name : 'New model route', wide: true, body: [form.node, facts], actions: [
    { label: 'Cancel', onClick: current => current.close() },
    { label: route ? 'Save' : 'Create', primary: true, onClick: current => saveRoute(current, form.draft(), context, reload, facts) },
  ] });
  form.name.focus();
  if (route) refresh();
}

async function readPreview(draft, context) {
  if (!draft.name) return { empty: true };
  try {
    return { view: previewView(await previewModelRoute(draft), context) };
  } catch (error) {
    return { refusal: refusalView(error) };
  }
}

// paintPreview shows what the route would be: what it runs on, where data goes,
// the rules that apply, the places that follow, and anything that refuses it.
function paintPreview(host, result, dialog) {
  host.replaceChildren();
  const save = dialog?.primaryButton?.();
  if (result.empty) return;
  if (result.refusal) {
    host.appendChild(refusalNode(result.refusal));
    return;
  }
  const view = result.view;
  if (save) save.textContent = saveLabel(view);
  host.appendChild(keyValue([['Runs on', factLines(view.runsOn)], ['Data', view.locality], ['Rules that apply', rulesNode(view.rules)]]));
  if (view.follows.length) host.appendChild(placesList('Used by', view.follows, 'These follow the change.'));
  for (const item of view.problems) host.appendChild(refusalNode({ message: 'Not saved: ' + item.message, places: item.places }));
}

function rulesNode(rules) {
  if (!rules.length) return 'none';
  const node = el('span', 'settings-route-rules');
  for (const rule of rules) {
    node.appendChild(el('span', rule.refuses ? 'settings-route-refuses' : '', rule.rule + ' — ' + rule.action + rule.where));
  }
  return node;
}

function refusalNode(refusal) {
  const node = el('div', 'settings-route-refusal');
  node.appendChild(problem(refusal.message));
  if (refusal.places.length) {
    const list = el('ul', 'settings-route-refused-places');
    for (const place of refusal.places) list.appendChild(el('li', '', place));
    node.appendChild(list);
  }
  return node;
}

async function saveRoute(dialog, draft, context, reload, facts) {
  dialog.showProblem('');
  if (!draft.name) { dialog.showProblem('Give the route a name.'); return; }
  try {
    const preview = await previewModelRoute(draft);
    const view = previewView(preview, context);
    paintPreview(facts, { view }, dialog);
    if (!view.canSave) return;
    await selectModelRoute(draft, preview);
    dialog.close();
    await reload();
  } catch (error) {
    paintPreview(facts, { refusal: refusalView(error) }, dialog);
  }
}

/* ---------- rename ---------- */

function openRenameSheet(route, reload) {
  const name = textInput(route.name, 'Name');
  openDialog({ title: 'Rename ' + route.name, body: [field('Name', name),
    el('div', 'agents-sub', 'Every place that uses this route keeps it.')], actions: [
    { label: 'Cancel', onClick: dialog => dialog.close() },
    { label: 'Rename', primary: true, onClick: dialog => renameRoute(dialog, route, name.value, reload) },
  ] });
  name.select();
}

async function renameRoute(dialog, route, name, reload) {
  dialog.showProblem('');
  const draft = renameDraft(route, name);
  if (!draft.name) { dialog.showProblem('Give the route a name.'); return; }
  try {
    const preview = await previewModelRoute(draft);
    await selectModelRoute(draft, preview);
    dialog.close();
    await reload();
  } catch (error) {
    dialog.showProblem(refusalView(error).message);
  }
}

/* ---------- delete ---------- */

// openDeleteSheet refuses a route that is used, naming the places, and asks
// once before deleting one that is not. The daemon checks again either way.
function openDeleteSheet(route, context, reload) {
  const groups = usedBy(route, context.agentNames);
  const count = (route.places || []).length;
  if (count) {
    openDialog({ title: 'Delete ' + route.name, body: [placesList('In use by', groups),
      problem('Not deleted: ' + count + (count === 1 ? ' place uses' : ' places use') + ' this route. Choose another route for '
        + (count === 1 ? 'it' : 'them') + ' first.')],
    actions: [{ label: 'Close', onClick: dialog => dialog.close() }] });
    return;
  }
  openDialog({ title: 'Delete ' + route.name, body: el('p', 'agents-dialog-text', 'Not used by any agent.'), actions: [
    { label: 'Cancel', onClick: dialog => dialog.close() },
    { label: 'Delete', primary: true, onClick: dialog => removeRoute(dialog, route, reload) },
  ] });
}

async function removeRoute(dialog, route, reload) {
  dialog.showProblem('');
  try {
    await deleteModelRoute(route);
    dialog.close();
    await reload();
  } catch (error) {
    dialog.showProblem(refusalView(error).message);
  }
}
