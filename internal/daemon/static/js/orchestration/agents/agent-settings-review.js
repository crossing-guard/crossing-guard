// A reviewer's settings (plan §7; D-7): its one place's model route (picked by
// name — what the route resolves to is on Settings → Models; team
// rest-of-release plan §5.5), effect, answer window, choice-prompt grant and
// runtime filter.
import { el, button, row, spacer, problem, errorText, keyValue, checkbox, textInput, field, selectBox, card, whileBusy } from './agent-ui.js';
import { saveReviewBinding } from '../review-api.js';
import { routeFacts, refusalWords, ROUTE_FAMILY_REVIEW } from './route-model.js';
import { routePicker, routeValue, loadRouteChoices } from './route-picker.js';

export function renderReviewerSettings(pane, ctx) {
  const place = (ctx.agent.places || []).find(item => item.lane === 'review');
  if (!place) {
    pane.appendChild(el('div', 'agents-empty', 'Its settings belong to its place. Turn it on from Where it runs first.'));
    return;
  }
  const body = el('div');
  body.appendChild(readView(place, ctx));
  // Saving a reviewer turns it on (the review lane has no "edit while off"),
  // so a reviewer that is off is changed only after Turn on.
  if (place.state === 'enabled') {
    body.appendChild(row(spacer(), button('Edit', '', async () => body.replaceChildren(await editView(place, ctx)))));
  } else {
    body.appendChild(row(el('span', 'agents-sub', 'Off.'), spacer(), button('Turn on…', '', () => ctx.openTab('places'))));
  }
  pane.appendChild(card('Reviewer', body));
}

function effectWords(effect) {
  return effect === 'delegated-first' ? 'Answers existing asks first' : 'Reports only';
}

function readView(place, ctx) {
  return keyValue([
    ['Model route', routeValue(routeFacts(place))],
    ['Last review', place.state === 'enabled' ? refusalWords(place.run_refusal, ctx.agent.origin) : ''],
    ['Time per review', Math.round(Number(place.timeout_ms || 0) / 1000) + ' s'],
    ['Effect', effectWords(place.effect)],
    ['Answer within', place.effect === 'delegated-first' ? (Number(place.approval_subdeadline_ms || 0) / 1000) + ' s' : ''],
    ['May answer choice prompts', place.effect === 'delegated-first' ? (place.answer_choice_prompts ? 'Yes' : 'No') : ''],
    ['Reviews sessions from', place.runtime_filter ? (ctx.runtimeNames[place.runtime_filter] || place.runtime_filter) + ' only' : 'Every runtime'],
  ]);
}

async function editView(place, ctx) {
  const effects = ctx.detail.review_option?.effects || ['report-only'];
  let choices = [];
  try { choices = await loadRouteChoices(ROUTE_FAMILY_REVIEW); } catch { choices = []; }
  const inputs = {
    route: routePicker(choices, { route_id: place.route_id }, { withMode: false }),
    timeout: textInput(String(Math.round(Number(place.timeout_ms || 0) / 1000)), 'Time per review (seconds)'),
    effect: selectBox(effects.map(value => [value, effectWords(value)]), place.effect || effects[0], 'Effect'),
    within: textInput(String(Number(place.approval_subdeadline_ms || 0) / 1000 || ''), 'Answer within (seconds)'),
    prompts: checkbox(place.answer_choice_prompts !== false, 'May answer choice prompts'),
    runtime: selectBox([['', 'Every runtime'], ...ctx.capabilities.map(item => [item.runtime, item.displayName + ' only'])], place.runtime_filter || '', 'Reviews sessions from'),
  };
  const delegated = el('div');
  delegated.append(field('Answer within (seconds)', inputs.within), inputs.prompts.node);
  const sync = () => delegated.classList.toggle('hidden', inputs.effect.value !== 'delegated-first');
  inputs.effect.onchange = sync;
  sync();
  const status = el('div');
  const node = el('div', 'agents-stack');
  const cancel = button('Cancel', '', () => ctx.reload('settings'));
  const saveButton = button('Save', 'primary', () => whileBusy([saveButton, cancel], () => save(place, ctx, inputs, status)));
  node.append(inputs.route.node, field('Time per review (seconds)', inputs.timeout),
    field('Effect', inputs.effect), delegated, field('Reviews sessions from', inputs.runtime), status, row(cancel, spacer(), saveButton));
  return node;
}

async function save(place, ctx, inputs, status) {
  const delegated = inputs.effect.value === 'delegated-first';
  const route = inputs.route.value();
  if (!route) {
    status.replaceChildren(problem('Create a model route first: the reviewer has none to run on.'));
    return;
  }
  try {
    await saveReviewBinding({ profile_id: ctx.id, profile_source_digest: place.source_digest, profile_bundle_digest: place.bundle_digest,
      route_id: route.route_id, timeout_ms: Math.round(Number(inputs.timeout.value) * 1000),
      effect: inputs.effect.value, approval_subdeadline_ms: delegated ? Math.round(Number(inputs.within.value) * 1000) : 0,
      answer_choice_prompts: inputs.prompts.input.checked, runtime_filter: inputs.runtime.value, expected_state_token: place.state_token });
    await ctx.reload('settings');
  } catch (error) {
    status.replaceChildren(problem(errorText(error, 'Not saved')));
  }
}
