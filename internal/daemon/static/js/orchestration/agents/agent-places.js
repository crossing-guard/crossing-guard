// Where it runs (plan §7): the agent's places. Each place has its own on/off
// and version; its settings come from the agent unless it differs, which is
// marked. Turning a place off is state-only and always works. A place runs on a
// model route picked by name (team rest-of-release plan §5.5); the page shows
// the route's name and whether it leaves this machine, and a place that is on
// but starts no run — held, or its route missing — says why.
import { el, button, stateLabel, row, spacer, problem, errorText, keyValue, selectBox, textInput, field, checkbox, whileBusy } from './agent-ui.js';
import { batch, newPlaceID, knownRepositories } from './roster-api.js';
import {
  placeName, sessionScopeWords, referencePlace, placeDifferences, relativeTime, powersView, powerWords, repositoryChoices,
  deliveryWords,
} from './roster-model.js';
import { routeFacts, fallbackFacts, placeStatus, refusalWords, needsRoute, routeFamilyFor } from './route-model.js';
import { routePicker, routeValue, loadRouteChoices } from './route-picker.js';
import { openDialog } from './agent-dialog.js';
import { openMoveDialog } from './agent-move.js';
import { saveReviewBinding, disableReviewBinding } from '../review-api.js';

export function renderPlaces(pane, ctx) {
  const places = ctx.agent.places || [];
  const canDeploy = Boolean(ctx.detail.current) && ctx.agent.compatible;
  const head = row(spacer(), ctx.kind === 'reviewer'
    ? (canDeploy && !places.length ? button('Turn on for every repository', 'primary', () => reviewerSetup(ctx)) : null)
    : (canDeploy ? button('Add repository', 'primary', () => addPlaceDialog(ctx)) : null));
  pane.appendChild(head);
  if (!ctx.detail.current) pane.appendChild(el('div', 'agents-empty', 'Publish the draft to deploy this agent.'));
  else if (!ctx.agent.compatible) pane.appendChild(problem(ctx.agent.incompatible_reason || 'This agent cannot be deployed as defined.'));
  if (!places.length) return;
  const reference = referencePlace(places);
  const table = el('div', 'agents-places');
  for (const place of places) table.appendChild(placeBlock(ctx, place, reference));
  pane.appendChild(table);
}

function placeBlock(ctx, place, reference) {
  const block = el('details', 'agents-place');
  const summary = document.createElement('summary');
  const status = placeStatus(place, ctx.agent.origin);
  const route = routeFacts(place);
  summary.append(stateLabel(status.state, placeName(place)),
    el('span', status.reason ? 'agents-sub agents-place-reason' : 'agents-sub', status.reason || sessionScopeWords(place, ctx.runtimeNames)), spacer(),
    el('span', 'agents-mono', place.version ? 'v' + place.version + (place.is_current ? '' : ' · older') : 'version unavailable'),
    el('span', 'agents-sub', route.short));
  block.appendChild(summary);
  const differs = placeDifferences(reference, place);
  const powers = powersView(ctx.profile?.authority_requests || [], place);
  const granted = powers.rows.filter(item => item.granted).map(item => item.words).join(', ') || 'nothing granted';
  block.appendChild(keyValue([
    ['Held', place.state === 'enabled' && place.held_reason ? holdDetail(status.reason) : ''],
    ['Last run', place.state === 'enabled' && !place.held_reason ? refusalWords(place.run_refusal, ctx.agent.origin) : ''],
    ['Sessions', sessionScopeWords(place, ctx.runtimeNames)],
    ['Repository', place.project_root || ''],
    ['Version', place.version ? 'v' + place.version + (place.is_current ? ' (current)' : '') : 'not stored any more'],
    ['Model route', routeValue(route, differs.includes('model') ? 'differs from other places' : '')],
    ['If it is unavailable', fallbackWords(place, differs)],
    ['May', granted + (powers.actingGranted ? (powers.automatic ? ' · automatically' : ' · waits for you') : '')
      + (differs.includes('permissions') ? ' · differs' : '')],
    ['Live conversations', String(ctx.detail.live_helper_sessions?.[place.place_id] || '')],
    ['Last changed', relativeTime(place.updated_at)],
  ]));
  block.appendChild(placeActions(ctx, place));
  return block;
}

// holdDetail is a hold reason as the value of the Held row: the row's label
// already says "Held".
function holdDetail(reason) {
  const words = reason.replace(/^held \u2014 /, '');
  return words.charAt(0).toUpperCase() + words.slice(1);
}

// fallbackWords names the fallback chain by route, in order; that it differs from
// the agent's other places is its own line.
function fallbackWords(place, differs) {
  const chain = fallbackFacts(place).map(entry => entry.name);
  if (!chain.length) return '';
  const node = el('span', 'agents-route-value');
  node.appendChild(el('span', '', chain.join(', then ')));
  if (differs.includes('fallback')) node.appendChild(el('span', 'agents-sub', 'differs from other places'));
  return node;
}

function placeActions(ctx, place) {
  const actions = row();
  const status = el('div');
  const run = work => whileBusy([...actions.querySelectorAll('button')], async () => {
    status.replaceChildren();
    try { await work(); await ctx.reload('places'); } catch (error) { status.replaceChildren(problem(errorText(error, 'Not changed'))); }
  });
  if (place.state === 'enabled') {
    actions.appendChild(button('Turn off here', 'danger', () => run(() => turnOff(place))));
  } else if (place.revision_available) {
    actions.appendChild(button('Turn on', 'primary', () => run(() => turnOn(ctx, place))));
  }
  const current = ctx.detail.current?.current;
  if (current && !place.is_current) {
    actions.appendChild(button('Move to v' + current.version + '…', '', () => openMoveDialog(ctx, {
      title: 'Move to v' + current.version, target: { version: current.version, source_digest: current.source_digest,
        bundle_digest: current.bundle_digest, normalized: ctx.detail.current.normalized },
      preselect: candidate => candidate.place_id === place.place_id,
    })));
  }
  if (needsRoute(place)) actions.appendChild(button('Choose route\u2026', '', () => chooseRouteDialog(ctx, place)));
  const node = el('div');
  node.append(actions, status);
  return node;
}

// chooseRouteDialog gives one place a model route: a place whose route is
// missing, or one the daemon could not move onto a route.
async function chooseRouteDialog(ctx, place) {
  const picker = routePicker(await routeChoicesFor(ctx, place.lane), { route_id: place.route_id, mode: place.mode });
  openDialog({ title: 'Model route for ' + placeName(place), body: [picker.node], actions: [
    { label: 'Cancel', onClick: dialog => dialog.close() },
    { label: 'Save', primary: true, onClick: dialog => saveRoute(ctx, dialog, place, picker) },
  ] });
}

async function routeChoicesFor(ctx, lane) {
  try {
    return await loadRouteChoices(routeFamilyFor(lane), ctx.capabilities);
  } catch {
    return [];
  }
}

function writeRoute(ctx, place, chosen) {
  if (place.lane === 'review') {
    return saveReviewBinding(reviewPayload({ ...place, route_id: chosen.route_id }, place.source_digest, place.bundle_digest, place.state_token, ctx.id));
  }
  return batch([{ binding_id: place.place_id, expected_state_token: place.state_token, op: 'update', set: { model: chosen } }]);
}

async function saveRoute(ctx, dialog, place, picker) {
  const chosen = picker.value();
  if (!chosen) { dialog.showProblem('Create a model route first.'); return; }
  try {
    await writeRoute(ctx, place, chosen);
    dialog.close();
    await ctx.reload('places');
  } catch (error) {
    dialog.showProblem(errorText(error, 'Not saved'));
  }
}

async function turnOff(place) {
  if (place.lane === 'review') return disableReviewBinding(place.state_token);
  return batch([{ binding_id: place.place_id, expected_state_token: place.state_token, op: 'disable' }]);
}

async function turnOn(ctx, place) {
  if (place.lane === 'review') {
    return saveReviewBinding(reviewPayload(place, place.source_digest, place.bundle_digest, place.state_token, ctx.id));
  }
  return batch([{ binding_id: place.place_id, expected_state_token: place.state_token, op: 'enable' }]);
}

function reviewPayload(place, sourceDigest, bundleDigest, token, profileID) {
  return { profile_id: profileID, profile_source_digest: sourceDigest, profile_bundle_digest: bundleDigest,
    route_id: place.route_id || '', timeout_ms: place.timeout_ms, effect: place.effect || 'report-only',
    approval_subdeadline_ms: place.approval_subdeadline_ms || 0, answer_choice_prompts: place.answer_choice_prompts !== false,
    runtime_filter: place.runtime_filter || '', expected_state_token: token };
}

/* ---------- add a repository ---------- */

async function addPlaceDialog(ctx) {
  const repos = await knownRepositories();
  const taken = new Set((ctx.agent.places || []).map(place => place.project_root));
  const choices = repositoryChoices(repos, taken);
  const repo = selectBox(choices.map(item => [item.root, item.label]), choices.find(item => !item.taken)?.root, 'Repository');
  const sessions = selectBox([['natural', 'Every session, including terminal'], ['console', 'Only sessions started from the console'],
    ['one', 'One session']], 'natural', 'Which sessions');
  const sessionID = textInput('', 'Session id', 'session id');
  sessionID.classList.add('hidden');
  sessions.onchange = () => sessionID.classList.toggle('hidden', sessions.value !== 'one');
  const runtimes = selectBox([['', 'Every runtime'], ...ctx.capabilities.map(item => [item.runtime, item.displayName + ' only'])], '', 'Watch sessions from');
  const start = checkbox(true, 'Turn on now');
  const settings = placeSettings(ctx, await routeChoicesFor(ctx, 'managed'));
  const dialog = openDialog({ title: 'Add ' + (ctx.agent.name || ctx.id) + ' to a repository', wide: true, body: [
    repos.length ? field('Repository', repo) : problem('No repositories are known yet: start a session in one first.'),
    field('Which sessions', sessions), sessionID, field('Watch sessions from', runtimes), settings.node, start.node,
  ], actions: [
    { label: 'Cancel', onClick: dialog => dialog.close() },
    { label: 'Add', primary: true, onClick: dialog => createPlace(ctx, dialog, {
      root: repo.value, scope: sessions.value, session: sessionID.value.trim(), runtime: runtimes.value,
      enabled: start.input.checked, settings: settings.value() }) },
  ] });
  // With no model route for this agent there is nothing to add yet (plan §5.8).
  if (settings.noRoute) dialog.primaryButton().disabled = true;
}

// placeSettings copies the reference place's settings, editable before saving;
// a first place starts from the first model route and the grants you tick.
function placeSettings(ctx, choices) {
  const reference = referencePlace(ctx.agent.places || []);
  const picker = routePicker(choices, reference || {});
  const requested = ctx.profile?.authority_requests || [];
  const grants = requested.map(name => ({ name, box: checkbox(reference ? (reference.granted_authority || []).includes(name) : name === 'draft-reply' || name === 'advise', powerWords(name)) }));
  const auto = checkbox(Boolean(reference?.auto_action), 'Act automatically (otherwise it waits for you)');
  const node = el('div', 'agents-stack');
  node.append(picker.node);
  if (grants.length) node.append(field('May', el('div', 'agents-checks')), auto.node);
  if (requested.includes('send-message')) node.append(el('div', 'agents-sub', 'Messages land — ' + deliveryWords(ctx.capabilities)));
  node.querySelector('.agents-checks')?.append(...grants.map(item => item.box.node));
  return { node, noRoute: picker.empty, value: () => ({ route: picker.value(), grants: grants.filter(item => item.box.input.checked).map(item => item.name),
    auto: auto.input.checked, reference }) };
}

async function createPlace(ctx, dialog, input) {
  if (!input.settings.route) { dialog.showProblem('Create a model route first: this agent has none to run on.'); return; }
  try {
    const id = await newPlaceID(ctx.id, input.root);
    const change = { binding_id: id.binding_id, expected_state_token: id.expected_state_token, op: 'create', place: newPlace(ctx, input) };
    await batch([change]);
    dialog.close();
    await ctx.reload('places');
  } catch (error) {
    dialog.showProblem(errorText(error, 'Not added'));
  }
}

// newPlace is the full binding a created place starts with: the current
// version, the chosen scope and model route, and the reference place's other
// settings. Its fallback chain is the reference place's, by route.
function newPlace(ctx, input) {
  const reference = input.settings.reference;
  const current = ctx.detail.current.current;
  const route = input.settings.route;
  return {
    profile_id: ctx.id, profile_source_digest: current.source_digest, profile_bundle_digest: current.bundle_digest,
    project_root: input.root, scope_runtime: input.runtime, scope_session: input.scope === 'one' ? input.session : '',
    watch_natural: input.scope === 'natural', route_id: route.route_id, mode: route.mode,
    granted_authority: input.settings.grants, auto_action: input.settings.auto, priority: reference?.priority || 0,
    declared_tags: reference ? reference.declared_tags : null, limits: reference?.limits || {},
    routes: fallbackFacts(reference || {}).filter(entry => entry.id).map(entry => ({ route_id: entry.id, mode: entry.mode })),
    state: input.enabled ? 'enabled' : 'disabled',
  };
}

/* ---------- the reviewer's one place ---------- */

async function reviewerSetup(ctx) {
  const slot = ctx.detail.review_slot || {};
  const effects = ctx.detail.review_option?.effects || ['report-only'];
  const inputs = {
    route: routePicker(await routeChoicesFor(ctx, 'review'), {}, { withMode: false }),
    effect: selectBox(effects.map(value => [value, value === 'delegated-first' ? 'Answer existing asks first' : 'Report only']), effects[0], 'Effect'),
    within: textInput('', 'Answer within (seconds)', 'seconds'),
  };
  const withinField = field('Answer within (seconds)', inputs.within);
  const syncWithin = () => withinField.classList.toggle('hidden', inputs.effect.value !== 'delegated-first');
  inputs.effect.onchange = syncWithin;
  syncWithin();
  const replaces = slot.profile_id && slot.profile_id !== ctx.id ? el('div', 'agents-sub', 'Replaces ' + slot.profile_id + ' as the reviewer.') : null;
  openDialog({ title: 'Turn on ' + (ctx.agent.name || ctx.id), body: [replaces, inputs.route.node, field('Effect', inputs.effect), withinField],
    actions: [{ label: 'Cancel', onClick: dialog => dialog.close() },
      { label: 'Turn on', primary: true, onClick: dialog => turnOnReviewer(ctx, dialog, inputs, slot) }] });
}

async function turnOnReviewer(ctx, dialog, inputs, slot) {
  const current = ctx.detail.current.current;
  const delegated = inputs.effect.value === 'delegated-first';
  const route = inputs.route.value();
  if (!route) { dialog.showProblem('Create a model route first: the reviewer has none to run on.'); return; }
  try {
    await saveReviewBinding({ profile_id: ctx.id, profile_source_digest: current.source_digest, profile_bundle_digest: current.bundle_digest,
      route_id: route.route_id, timeout_ms: Number(ctx.detail.review_option?.timeout_ceiling_ms || 0),
      effect: inputs.effect.value, approval_subdeadline_ms: delegated ? Math.round(Number(inputs.within.value) * 1000) : 0,
      answer_choice_prompts: true, runtime_filter: '', expected_state_token: slot.state_token });
    dialog.close();
    await ctx.reload('places');
  } catch (error) {
    dialog.showProblem(errorText(error, 'Not turned on'));
  }
}
