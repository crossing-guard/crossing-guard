// The one Model route control on the agent pages (team rest-of-release plan
// §5.5, §5.8): a list of routes by name. It shows a route's name and whether it
// is on this machine or leaves it, with a link to Settings → Models — never what
// the route resolves to. A device with no route of the needed family says so and
// offers Create route.
import { el, selectBox, field, linkButton, button } from './agent-ui.js';
import { managedRuntimes } from './model-picker.js';
import { loadModelRoutes } from '../model-routes-api.js';
import { routeChoices, ROUTE_FAMILY_MANAGED, NO_ROUTE_FOR_AGENT } from './route-model.js';

// openModels shows Settings → Models: the Settings shell hears the page request,
// then the nav bus lands it.
export function openModels() {
  document.dispatchEvent(new CustomEvent('cg:settings-subpage', { detail: 'models' }));
  document.dispatchEvent(new CustomEvent('cg:nav', { detail: 'settings' }));
}

export function modelsLink(label = 'Models ›') {
  return linkButton(label, openModels);
}

// loadRouteChoices reads the routes of one family as choices. A managed
// choice carries the read-only modes its place may pick.
export async function loadRouteChoices(family, capabilities = []) {
  const listed = await loadModelRoutes();
  const runtimes = managedRuntimes(capabilities);
  const modesFor = route => (runtimes.find(item => item.runtime === route.fields?.runtime)?.modes || []).filter(mode => mode.risk === 'normal');
  return routeChoices(listed.routes || [], family, family === ROUTE_FAMILY_MANAGED ? modesFor : () => []);
}

// routePicker returns { node, value(), empty } for a { route_id, mode } choice.
// `current` is the place's route and mode; a route that is no longer listed is
// not offered. With no choices the control says so and value() is null.
export function routePicker(choices, current = {}, { label = 'Model route', withMode = true } = {}) {
  const node = el('div', 'agents-route-picker');
  if (!choices.length) {
    const empty = el('div', 'agents-route-empty');
    empty.append(el('span', '', NO_ROUTE_FOR_AGENT), button('Create route', '', openModels));
    node.append(field(label, empty));
    return { node, empty: true, value: () => null };
  }
  const start = choices.some(choice => choice.id === current.route_id) ? current.route_id : choices[0].id;
  const route = selectBox(choices.map(choice => [choice.id, choice.name]), start, label);
  const locality = el('span', 'agents-sub');
  const facts = el('div', 'agents-route-facts');
  facts.append(locality, modelsLink());
  const mode = document.createElement('select');
  mode.setAttribute('aria-label', 'Read-only mode');
  const modeField = field('Read-only mode', mode);
  const sync = () => {
    const choice = choices.find(item => item.id === route.value);
    locality.textContent = choice.locality;
    const previous = mode.options.length ? mode.value : String(current.mode || '');
    mode.replaceChildren(...choice.modes.map(item => { const option = document.createElement('option'); option.value = item.id; option.textContent = item.label; return option; }));
    mode.value = choice.modes.some(item => item.id === previous) ? previous : (choice.modes[0]?.id || '');
    modeField.classList.toggle('hidden', !withMode || choice.modes.length < 2);
  };
  route.onchange = sync;
  sync();
  node.append(field(label, route), facts, modeField);
  return { node, empty: false, value: () => ({ route_id: route.value, mode: mode.value }) };
}

// routeValue renders a place's route as a value: its name, where it runs, and
// the link to Settings → Models.
export function routeValue(facts, extra = '') {
  const node = el('span', 'agents-route-value');
  node.appendChild(el('span', facts.problem ? 'agents-route-problem' : '', facts.name));
  if (facts.locality) node.appendChild(el('span', 'agents-sub', facts.locality));
  if (extra) node.appendChild(el('span', 'agents-sub', extra));
  node.appendChild(modelsLink());
  return node;
}
