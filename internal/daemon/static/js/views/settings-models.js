// Settings › Models (settings-restructure plan §3.4; team rest-of-release plan
// §5.5): the named model routes agents run on, which of them are in use, and
// what each runtime reports it can run. The runtime lists are read-only: a
// runtime's own configuration owns which models exist (decision D-3).
import { el, fmtTime, fmtLimit, fmtPrice } from "../core.js";
import { loadChatCapabilities, loadChatModels } from "../chat-capabilities.js";
import { loadRoster } from "../orchestration/agents/roster-api.js";
import { renderModelRoutes } from "./settings-model-routes.js";
import { routeRows, routesInUse } from "./settings-routes-model.js";
import { loadModelRoutes } from "../orchestration/model-routes-api.js";

// Reasons a model list could not be read; codes come from the daemon's fixed
// set and never carry vendor output.
const MODEL_REASON_LABELS = Object.freeze({
  'not-installed': 'the runtime binary was not found',
  'exited-with-error': 'the runtime exited with an error',
  'timed-out': 'the runtime did not answer in time',
  'output-too-large': 'the list exceeded its size bound',
  unparseable: 'the list was not in the expected form',
  'too-many-entries': 'the list has more entries than models.max_entries',
  'work-dir-in-repository': 'the daemon data directory is inside a repository',
  'request-failed': 'the daemon did not answer',
});

async function renderModelsPage(main) {
  const search = el('input', 'settings-search');
  search.type = 'search'; search.placeholder = 'Search models'; search.setAttribute('aria-label', 'Search models');
  main.appendChild(search);
  const routes = el('section', 'settings-card');
  const inUse = el('section', 'settings-card');
  const catalog = el('div');
  main.append(routes, inUse, catalog);
  let capabilities = [];
  try { capabilities = await loadChatCapabilities(); }
  catch (error) { catalog.appendChild(el('div', 'banner', 'Models unavailable: ' + (error.message || error))); return; }
  const runtimeNames = Object.fromEntries(capabilities.map(capability => [capability.runtime, capability.displayName]));
  // The roster names the agents that use a route and the places still waiting
  // for one; without it the routes are still listed, by agent id.
  let roster = null;
  try { roster = await loadRoster(); } catch { roster = null; }
  const context = { capabilities, runtimeNames, roster: roster || {},
    agentNames: Object.fromEntries((roster?.agents || []).map(agent => [agent.profile_id, agent.name || agent.profile_id])) };
  const reload = async () => { main.replaceChildren(); await renderModelsPage(main); };
  void renderModelRoutes(routes, context, reload);
  void renderRoutesInUse(inUse, context);
  // Not awaited: a runtime's first discovery can take seconds (P-RT7).
  void renderModelLists(catalog, capabilities);
  search.oninput = () => {
    const needle = search.value.trim().toLowerCase();
    for (const row of main.querySelectorAll('[data-model-row]')) row.classList.toggle('hidden', Boolean(needle) && !row.textContent.toLowerCase().includes(needle));
    if (needle) for (const list of main.querySelectorAll('details.settings-model-list')) list.open = true;
  };
}

// renderRoutesInUse lists the routes agents run on: which agents, and where.
async function renderRoutesInUse(card, context) {
  card.appendChild(el('h3', '', 'In use'));
  let listed;
  try { listed = await loadModelRoutes(); }
  catch (error) { card.appendChild(el('div', 'sub', 'Model routes cannot be read: ' + (error.message || error))); return; }
  const used = routesInUse(routeRows(listed, context));
  if (!used.length) { card.appendChild(el('div', 'sub', 'No agent is placed anywhere.')); return; }
  const table = el('table', 'settings-table');
  for (const item of used) {
    const tr = el('tr'); tr.dataset.modelRow = '1';
    const route = el('td', '', item.name);
    for (const fact of item.runsOn) route.appendChild(el('div', 'sub', fact));
    tr.append(route, el('td', '', item.agents.join(', ')), el('td', 'sub', item.places));
    table.appendChild(tr);
  }
  card.appendChild(table);
}

// renderModelLists shows each runtime's own model list, read-only: the
// runtime's configuration owns which models exist (design §6, decision D-3).
async function renderModelLists(main, capabilities) {
  const section = el('div');
  main.appendChild(section);
  const lists = await Promise.all(capabilities.map(capability => loadChatModels(capability.runtime)));
  const shown = capabilities.map((capability, index) => [capability, lists[index]])
    .filter(([, list]) => list.state !== 'unsupported');
  if (!shown.length) return;
  section.appendChild(el('div', 'sub', 'What each runtime reports it can run. Which models exist, and their limits and prices, come from the runtime\u2019s own configuration; change them there.'));
  shown.forEach(([capability, list], index) => section.appendChild(modelListCard(capability, list, index === 0)));
}

// modelColumns is the table's columns for one list: a column no model in the
// list has a value for is left out.
function modelColumns(models) {
  const columns = [['Model', model => model.label]];
  if (models.some(model => model.groupLabel)) columns.push(['Group', model => model.groupLabel || '—']);
  if (models.some(model => model.contextTokens)) columns.push(['Context', model => fmtLimit(model.contextTokens)]);
  if (models.some(model => model.price)) columns.push(['Price (runtime-stated)',
    model => (model.price ? fmtPrice(model.price) + (model.pricePartial ? ' (partial)' : '') : 'unknown')]);
  if (models.some(model => model.inputs.length)) columns.push(['Inputs', model => model.inputs.join(', ') || '—']);
  return columns;
}

function modelListCard(capability, list, open = false) {
  const card = el('details', 'chartcard settings-model-list');
  card.open = open;
  const head = el('summary', 'row');
  head.append(el('h3', '', capability.displayName), el('span', 'sub', list.models.length + (list.models.length === 1 ? ' model' : ' models')),
    el('span', 'chip ' + (list.state === 'fresh' ? 'st-verified' : 'st-stale'), list.state));
  // Refresh sits under the summary, not inside it: a button in a summary
  // would also toggle the list.
  const refresh = el('button', 'btn', 'Refresh');
  refresh.type = 'button';
  refresh.onclick = async () => {
    refresh.disabled = true; refresh.textContent = 'Refreshing…';
    card.replaceWith(modelListCard(capability, await loadChatModels(capability.runtime, { refresh: true }), true));
  };
  card.append(head, refresh);
  const facts = [
    list.observedAt ? 'observed ' + fmtTime(list.observedAt) : '',
    list.reasonCode ? 'last attempt: ' + (MODEL_REASON_LABELS[list.reasonCode] || list.reasonCode) : '',
    list.scope, list.binary ? 'binary ' + list.binary : '',
    list.interfaceRevision ? 'interface verified against CLI ' + list.interfaceRevision : '',
    list.rejected ? list.rejected + ' entries were not listed (unreadable, duplicate, or colliding with a built-in choice)' : '',
  ].filter(Boolean);
  card.appendChild(el('div', 'sub', facts.join(' · ')));
  if (!list.models.length) return card;
  const columns = modelColumns(list.models);
  const table = el('table');
  const header = el('tr');
  for (const [label] of columns) header.appendChild(el('th', '', label));
  table.appendChild(header);
  for (const model of list.models) {
    const tr = el('tr'); tr.dataset.modelRow = '1';
    columns.forEach(([, value], index) => {
      const cell = el('td', '', value(model));
      if (index === 0) cell.title = model.id;
      tr.appendChild(cell);
    });
    table.appendChild(tr);
  }
  card.appendChild(table);
  return card;
}

export { renderModelsPage, modelColumns };
