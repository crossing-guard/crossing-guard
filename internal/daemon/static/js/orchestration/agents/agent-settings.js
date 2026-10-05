// Settings tab (plan §7; D-2, D-9): what places run with. Each section has a
// read view and its own Edit; saving patches only that section, on the places
// ticked, in one all-or-nothing batch.
import { el, button, row, spacer, problem, errorText, keyValue, checkbox, textInput, field, selectBox, card, whileBusy } from './agent-ui.js';
import { batch } from './roster-api.js';
import {
  referencePlace, placeName, placeDifferences, powersView, budgetsView, profileOf, deliveryWords,
} from './roster-model.js';
import { routeFacts, fallbackFacts, ROUTE_FAMILY_MANAGED } from './route-model.js';
import { routePicker, routeValue, loadRouteChoices } from './route-picker.js';
import { renderReviewerSettings } from './agent-settings-review.js';

export function renderSettings(pane, ctx) {
  if (ctx.kind === 'reviewer') {
    renderReviewerSettings(pane, ctx);
    pane.appendChild(ceilingsCard(ctx));
    return;
  }
  const places = (ctx.agent.places || []).filter(place => place.lane === 'managed');
  if (!places.length) {
    pane.append(el('div', 'agents-empty', 'Model route, permissions and budgets belong to the places an agent runs in. Add a repository first.'),
      ceilingsCard(ctx));
    return;
  }
  const reference = referencePlace(places);
  const holder = card('');
  holder.append(modelSection(ctx, places, reference), permissionsSection(ctx, places, reference));
  if ((ctx.profile?.may_tag || []).length) holder.appendChild(tagsSection(ctx, places, reference));
  if (ctx.kind === 'helper') holder.appendChild(prioritySection(ctx, places, reference));
  holder.appendChild(budgetsSection(ctx, places, reference));
  pane.append(holder, ceilingsCard(ctx));
}

// section renders one read view with an Edit that swaps in the editor, the
// place ticks, and Save.
function section(ctx, { title, note, read, edit, places, field: fieldName }) {
  const node = el('div', 'agents-setsec');
  const label = el('div', 'agents-setsec-label');
  label.append(el('b', '', title), el('span', 'agents-sub', note || ''));
  const body = el('div', 'agents-setsec-body');
  const differing = places.filter(place => placeDifferences(referencePlace(places), place).some(field => field === fieldName || (fieldName === 'model' && field === 'fallback')));
  body.append(read, differing.length ? el('div', 'agents-sub', 'Differs in ' + differing.map(placeName).join(', ')) : '');
  // An editor may need to read first (the routes); Edit waits for it.
  const open = button('Edit', 'ghost', async () => {
    open.disabled = true;
    openEditor(ctx, body, await edit(), places);
    open.remove();
  });
  node.append(label, body, open);
  return node;
}

// openEditor swaps a section's read view for its editor, the place ticks, and
// a Save that names how many places it writes.
function openEditor(ctx, body, editor, places) {
  const targets = targetPicker(places);
  const status = el('div');
  const saveLabel = () => 'Save to ' + targets.count() + ' place' + (targets.count() === 1 ? '' : 's');
  const cancel = button('Cancel', '', () => ctx.reload('settings'));
  const save = button(saveLabel(), 'primary', () => whileBusy([save, cancel], () => saveSection(ctx, editor, targets.chosen(), status)));
  targets.onChange(() => { save.textContent = saveLabel(); });
  body.replaceChildren(editor.node, targets.node, row(cancel, spacer(), save), status);
}

async function saveSection(ctx, editor, chosen, status) {
  if (!chosen.length) {
    status.replaceChildren(problem('Tick at least one place.'));
    return;
  }
  const set = editor.collect();
  if (!set) {
    status.replaceChildren(problem('Create a model route first.'));
    return;
  }
  try {
    await batch(chosen.map(place => ({ binding_id: place.place_id, expected_state_token: place.state_token, op: 'update', set })));
    await ctx.reload('settings');
  } catch (error) {
    status.replaceChildren(problem(errorText(error, 'Not saved')));
  }
}

// targetPicker ticks the places a save goes to: enabled places whose version
// is stored, by default; a place that cannot be rebuilt says why.
function targetPicker(places) {
  const node = el('div', 'agents-targets');
  node.appendChild(el('span', 'agents-field-label', 'Save to'));
  const boxes = places.map(place => {
    const box = checkbox(place.state === 'enabled' && place.revision_available, placeName(place) + (place.state === 'enabled' ? '' : ' (off)')
      + (place.revision_available ? '' : ' — its version is no longer stored'));
    box.input.disabled = !place.revision_available;
    node.appendChild(box.node);
    return { place, box };
  });
  return {
    node,
    chosen: () => boxes.filter(item => item.box.input.checked).map(item => item.place),
    count: () => boxes.filter(item => item.box.input.checked).length,
    onChange: handler => boxes.forEach(item => item.box.input.addEventListener('change', handler)),
  };
}

// modelSection is the Model route section: the route each place runs on, by
// name, and the route it falls back to. What a route resolves to is on
// Settings → Models, one click away.
function modelSection(ctx, places, reference) {
  const fallback = fallbackFacts(reference)[0];
  const read = el('div', 'agents-stack');
  read.appendChild(routeValue(routeFacts(reference)));
  if (fallback) read.appendChild(el('div', 'agents-sub', 'If it is unavailable: ' + fallback.name));
  if (ctx.profile?.requirements?.destination?.locality === 'local-only') {
    read.appendChild(el('div', 'agents-warnline', 'This agent asks to run local-only. Helpers do not enforce it.'));
  }
  return section(ctx, { title: 'Model route', note: 'What each place runs on', read, places, field: 'model',
    edit: () => routeEditor(ctx, reference, fallback) });
}

// routeEditor picks the route by name, and optionally a second route to run on
// when the first is unavailable, each with its own read-only mode.
async function routeEditor(ctx, reference, fallback) {
  let choices = [];
  try { choices = await loadRouteChoices(ROUTE_FAMILY_MANAGED, ctx.capabilities); } catch { choices = []; }
  const primary = routePicker(choices, reference);
  const useFallback = checkbox(Boolean(fallback), 'If it is unavailable, run on another route');
  const second = routePicker(choices, fallback ? { route_id: fallback.id, mode: fallback.mode } : {}, { label: 'Fallback route' });
  const node = el('div', 'agents-stack');
  const sync = () => second.node.classList.toggle('hidden', !useFallback.input.checked);
  useFallback.input.addEventListener('change', sync);
  sync();
  node.append(primary.node, useFallback.node, second.node);
  const collect = () => (primary.empty ? null
    : { model: primary.value(), fallback: useFallback.input.checked ? [second.value()] : [] });
  return { node, collect };
}

function permissionsSection(ctx, places, reference) {
  const requested = ctx.profile?.authority_requests || [];
  const view = powersView(requested, reference);
  const read = el('div');
  if (!requested.length) read.appendChild(el('div', '', 'It asks for no powers: it observes and tags only.'));
  for (const item of view.rows) read.appendChild(el('div', item.granted ? '' : 'agents-muted', (item.granted ? '\u2713 ' : '\u2014 ') + item.words));
  if (view.actingGranted) read.appendChild(el('div', 'agents-sub', view.automatic ? 'Acts automatically' : 'Waits for you before it acts'));
  if (requested.includes('send-message')) read.appendChild(el('div', 'agents-sub', 'Messages land — ' + deliveryWords(ctx.capabilities)));
  return section(ctx, { title: 'What it may do', note: 'Only what it asked for', read, places, field: 'permissions',
    edit: () => permissionsEditor(view, reference) });
}

function permissionsEditor(view, reference) {
  const boxes = view.rows.map(item => ({ name: item.name, box: checkbox(item.granted, item.words) }));
  const auto = selectBox([['wait', 'Waits for you before it acts'], ['auto', 'Acts automatically']], reference.auto_action ? 'auto' : 'wait', 'When it acts');
  const node = el('div', 'agents-checks');
  node.append(...boxes.map(item => item.box.node), field('When it acts', auto));
  const granted = () => boxes.filter(item => item.box.input.checked).map(item => item.name);
  return { node, collect: () => ({ permissions: { granted_authority: granted(), auto_action: auto.value === 'auto' } }) };
}

function tagsSection(ctx, places, reference) {
  const read = el('div', '', (reference.declared_tags || []).join(', ') || 'none');
  return section(ctx, { title: 'Tags it may apply', note: 'From its definition', read, places, field: 'tags',
    edit: () => tagsEditor(ctx.profile.may_tag || [], reference.declared_tags || []) });
}

function tagsEditor(mayTag, declared) {
  const boxes = mayTag.map(tag => ({ tag, box: checkbox(declared.includes(tag), tag) }));
  const node = el('div', 'agents-checks');
  node.append(...boxes.map(item => item.box.node));
  return { node, collect: () => ({ declared_tags: boxes.filter(item => item.box.input.checked).map(item => item.tag) }) };
}

function prioritySection(ctx, places, reference) {
  const read = el('div', '', String(reference.priority || 0));
  return section(ctx, { title: 'Priority', note: 'When two helpers want the same moment, the higher one acts', read, places,
    field: 'priority', edit: () => priorityEditor(reference) });
}

function priorityEditor(reference) {
  const input = textInput(String(reference.priority || 0), 'Priority');
  input.type = 'number';
  return { node: field('Priority', input), collect: () => ({ priority: Number(input.value) || 0 }) };
}

function budgetsSection(ctx, places, reference) {
  const view = budgetsView(reference.limits, ctx.detail.defaults, ctx.detail.enforced_limits || []);
  const read = el('div', 'agents-limits');
  for (const item of view) read.appendChild(budgetTile(item));
  return section(ctx, { title: 'Budgets', note: 'Stops it from running away', read, places, field: 'budgets',
    edit: () => budgetsEditor(view, ctx.detail.defaults || {}) });
}

function budgetTile(item) {
  const tile = el('div', item.isDefault ? 'agents-limit agents-muted' : 'agents-limit');
  tile.append(el('b', '', item.value == null ? '\u2014' : String(item.value)), el('span', 'agents-sub', item.label + (item.isDefault ? ' \u00b7 default' : '')));
  return tile;
}

function budgetsEditor(view, defaults) {
  const inputs = view.map(item => budgetInput(item, defaults));
  const node = el('div', 'agents-limits');
  node.append(...inputs.map(item => field(item.label, item.input)));
  const valueOf = key => Number(inputs.find(item => item.key === key)?.input.value || 0);
  return { node, collect: () => ({ budgets: { max_total: valueOf('max_total'), loop_budget: valueOf('loop_budget') } }) };
}

function budgetInput(item, defaults) {
  const input = textInput(item.isDefault ? '' : String(item.value), item.label, 'default ' + String(defaults[item.key] ?? ''));
  input.type = 'number';
  input.min = '0';
  return { key: item.key, input, label: item.label };
}

// ceilingsCard shows the definition's own limits and says which ones the
// daemon does not enforce for helpers (published by Go, never a JS list).
function ceilingsCard(ctx) {
  const profile = profileOf(ctx.detail);
  if (!profile) return el('div');
  const unenforced = new Set(ctx.kind === 'reviewer' ? [] : ctx.detail.unenforced_profile_limits || []);
  const mark = key => (unenforced.has(key) ? ' · not enforced for helpers' : '');
  const limits = profile.limits || {};
  const identity = el('details', 'agents-advanced');
  const summary = document.createElement('summary'); summary.textContent = 'Identity and failure behaviour';
  identity.append(summary, keyValue([
    ['Agent id', profile.id], ['Version', profile.version],
    ['Source', ctx.detail.current?.current?.source_digest || ''], ['Compiled', ctx.detail.current?.current?.bundle_digest || ''],
    ['If context is missing', String(profile.failure?.missing_required_context || '') + mark('failure')],
    ['If it times out', String(profile.failure?.timeout || '') + mark('failure')],
    ['If the reply is malformed', String(profile.failure?.malformed_output || '') + mark('failure')],
  ]));
  return card('Ceilings from its definition', keyValue([
    ['Time per run', String(limits.timeout || '') + mark('timeout')],
    ['Reads up to', limits.max_input_bytes ? Math.round(limits.max_input_bytes / 1024) + ' KB' : ''],
    ['Replies up to', limits.max_output_bytes ? Math.round(limits.max_output_bytes / 1024) + ' KB' : ''],
    ['Tokens per reply', limits.max_tokens ? String(limits.max_tokens) + mark('max_tokens') : ''],
    ['Data may go', String(profile.requirements?.destination?.locality || 'no requirement') + mark('locality')],
  ]), identity);
}
