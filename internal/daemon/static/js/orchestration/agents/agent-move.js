// Moving places to a version (plan §7 "Move dialog"; D-6, RT-15). Used by
// Publish, Use in… and Move to latest. Each place is listed with a checkbox and
// what the move keeps or drops; nothing moves unless ticked, the moves are
// validated before they are written, and the write is one batch.
import { el, checkbox, stateLabel, problem, errorText } from './agent-ui.js';
import { openDialog } from './agent-dialog.js';
import { batch } from './roster-api.js';
import { placeName, movePlan, powerWords } from './roster-model.js';
import { saveReviewBinding } from '../review-api.js';

// openMoveDialog lists the agent's places for a move to `target`
// ({version, source_digest, bundle_digest, normalized}). `prepare` runs first
// on confirm (a publish) and may fail; `preselect(place)` picks the default ticks.
export function openMoveDialog(ctx, { title, target, intro = null, prepare = null, confirmLabel = 'Move', preselect = () => true, nextTab = '' }) {
  const places = (ctx.agent.places || []).filter(place => place.source_digest !== target.source_digest
    || place.bundle_digest !== target.bundle_digest);
  const rows = places.map(place => moveRow(ctx, place, target, preselect(place)));
  const list = el('div', 'agents-move-list');
  list.append(...rows.map(item => item.node));
  if (!rows.length && !prepare) list.appendChild(el('div', 'agents-sub', 'Every place already runs this version.'));
  const body = [intro, rows.length ? el('div', 'agents-field-label', 'Move these places to v' + target.version) : null, list,
    rows.length ? el('div', 'agents-sub', 'Unticked places keep their version. A run already in progress finishes on the old one.') : null];
  openDialog({ title, body, wide: true, actions: [
    { label: 'Cancel', onClick: dialog => dialog.close() },
    { label: confirmLabel, primary: true, onClick: dialog => confirmMove(ctx, dialog, rows, target, prepare, nextTab) },
  ] });
}

function moveRow(ctx, place, target, ticked) {
  // A reviewer moves by saving it, which turns it on; one that is off stays put.
  const reviewerOff = place.lane === 'review' && place.state !== 'enabled';
  const box = checkbox(ticked && !reviewerOff, placeName(place));
  box.input.disabled = reviewerOff;
  const node = el('div', 'agents-move-row');
  const plan = movePlan(place, target.normalized || {});
  const facts = el('div', 'agents-sub');
  const parts = ['now ' + (place.version ? 'v' + place.version : 'an unstored version')];
  if (reviewerOff) parts.push('off: turn it on to move it');
  if (place.lane === 'managed') {
    if (plan.grantsDropped.length) parts.push('stops: ' + plan.grantsDropped.map(powerWords).join(', '));
    if (plan.grantsNew.length) parts.push('newly asks (not granted): ' + plan.grantsNew.map(powerWords).join(', '));
    if (plan.tagsDropped.length) parts.push('drops tags: ' + plan.tagsDropped.join(', '));
    const live = Number(ctx.detail.live_helper_sessions?.[place.place_id] || 0);
    if (live) parts.push(live === 1 ? '1 open conversation continues with this version on its next turn'
      : live + ' open conversations continue with this version on their next turn');
  }
  facts.textContent = parts.join(' · ');
  const problemHost = el('div');
  node.append(box.node, stateLabel(place.state === 'enabled' ? 'on' : 'off', place.state === 'enabled' ? 'On' : 'Off'), facts, problemHost);
  return { node, place, plan, input: box.input, problemHost };
}

function moveChange(item, target) {
  const { place, plan } = item;
  return { binding_id: place.place_id, expected_state_token: place.state_token, op: 'update', set: {
    revision: { source_digest: target.source_digest, bundle_digest: target.bundle_digest },
    permissions: { granted_authority: plan.grantsKept, auto_action: Boolean(place.auto_action) && plan.grantsKept.length > 0 },
    declared_tags: plan.tagsKept,
  } };
}

async function confirmMove(ctx, dialog, rows, target, prepare, nextTab) {
  dialog.showProblem('');
  if (prepare) {
    try {
      await prepare();
    } catch (error) {
      dialog.showProblem(errorText(error, 'Not published'));
      return;
    }
  }
  const ticked = rows.filter(item => item.input.checked);
  const managed = ticked.filter(item => item.place.lane === 'managed');
  const review = ticked.find(item => item.place.lane === 'review');
  try {
    if (managed.length) await validatedBatch(managed, target);
    if (review) await moveReviewer(review.place, target);
    dialog.close();
    await ctx.reload(nextTab || ctx.tab);
  } catch (error) {
    if (!prepare) {
      dialog.showProblem(errorText(error, 'Places did not move'));
      return;
    }
    // The version is published and the draft is gone: this dialog's tokens are
    // stale, so the retry happens from Where it runs with fresh ones.
    dialog.close();
    const why = String(error?.message || error) + (error?.status === 409 ? '' : ' ' + String(error?.recovery || ''));
    await ctx.reload('places', { notice: 'Published v' + target.version + '. The places did not move: ' + why.trim() + ' Move them from here.' });
  }
}

async function validatedBatch(items, target) {
  const changes = items.map(item => moveChange(item, target));
  const check = await batch(changes, { validateOnly: true });
  const invalid = (check.results || []).filter(result => !result.valid);
  for (const item of items) {
    const result = invalid.find(entry => entry.binding_id === item.place.place_id);
    item.problemHost.replaceChildren(result ? problem(result.problem) : '');
  }
  if (invalid.length) throw new Error(invalid.length + ' place(s) cannot run this version as they are set up.');
  await batch(changes);
}

// moveReviewer re-pins the review singleton, keeping every other setting.
async function moveReviewer(place, target) {
  await saveReviewBinding({ profile_id: target.normalized?.id || '', profile_source_digest: target.source_digest,
    profile_bundle_digest: target.bundle_digest, route_id: place.route_id || '',
    timeout_ms: place.timeout_ms, effect: place.effect || 'report-only',
    approval_subdeadline_ms: place.approval_subdeadline_ms || 0, answer_choice_prompts: place.answer_choice_prompts,
    runtime_filter: place.runtime_filter || '', expected_state_token: place.state_token });
}
