import { el, fmtTime } from '../core.js';
import { fetchTaskSnapshot } from './task-api.js';
import { taskProjectionStore } from './task-projection-store.js';
import { registerTranscriptDecorator } from '../transcript-decorators.js';

export function effortCaption(settings) {
  if (!settings) return 'Effort not recorded';
  return settings.thinking_effort_label || (settings.thinking_effort?.kind === 'inherit' ? 'Default' : settings.thinking_effort?.value || 'Unknown');
}

export function effortDetail(settings) {
  return 'Requested: ' + effortCaption(settings) + '. Source: ' + settings.source
    + '. Applied effort: not reported. Runtime or account limits may change it.';
}

// Exact native anchors only. No proximity/time/model-based transcript join.
export function taskForEffortAnchor(tasks, anchor, session) {
  if (!anchor || !session?.runtime || !session.id) return null;
  const native = session.resume_id || session.id;
  const matches = tasks.filter(task => task.session_runtime === session.runtime
    && (task.catalog_session_id === session.id || (!task.catalog_session_id && session.id === native && task.native_session_id === native))
    && task.requested_settings && task.events?.some(event => event.payload?.anchor === anchor));
  return matches.length === 1 ? matches[0] : null;
}

function paintEffortRow(row, session) {
  const task = taskForEffortAnchor(taskProjectionStore.all(), row.dataset.effortAnchor, session);
  row.querySelector(':scope > .turn-effort')?.remove();
  if (!task) return;
  const chip = el('span', 'turn-effort', task.requested_settings.model + ' · ' + effortCaption(task.requested_settings));
  chip.title = effortDetail(task.requested_settings);
  row.appendChild(chip);
}

registerTranscriptDecorator((event, row, ctx) => {
  if (!event.turn_anchor || !ctx.session) return;
  row.dataset.effortAnchor = event.turn_anchor;
  row.effortSession = ctx.session;
  paintEffortRow(row, ctx.session);
}, 30);

taskProjectionStore.subscribe(() => {
  if (typeof document === 'undefined') return;
  for (const row of document.querySelectorAll('[data-effort-anchor]')) paintEffortRow(row, row.effortSession);
});

// Retained task snapshots remain inspectable even after event-window pruning.
// This is session metadata, never a second copy of conversation output.
export async function openTurnSettings(session) {
  const dialog = document.createElement('dialog'); dialog.className = 'effort-history';
  const close = el('button', 'btn', 'Close'); close.onclick = () => dialog.close();
  const body = el('div', '', 'Loading recent turn settings…');
  dialog.append(el('h3', '', 'Recent turn settings'), close, body);
  dialog.addEventListener('keydown', event => { if (event.key === 'Escape') event.stopPropagation(); });
  dialog.addEventListener('close', () => dialog.remove(), { once: true });
  document.body.appendChild(dialog); dialog.showModal(); close.focus();
  try {
    const native = session.resume_id || session.id;
    const snapshot = await fetchTaskSnapshot(undefined, { session_runtime: session.runtime, native_session_id: native });
    if (!dialog.isConnected) return;
    const tasks = snapshot.tasks.filter(task => task.catalog_session_id === session.id
      || (!task.catalog_session_id && session.id === native));
    body.replaceChildren();
    if (!tasks.length) body.appendChild(el('p', '', 'No console turn settings in this session’s recent task window.'));
    for (const task of tasks) {
      const row = el('details'); row.appendChild(el('summary', '', fmtTime(task.created_at) + ' · ' + (task.requested_settings?.model || 'Runtime model') + ' · ' + effortCaption(task.requested_settings)));
      row.appendChild(el('p', '', task.requested_settings ? effortDetail(task.requested_settings) : 'This older turn did not record an effort selection.'));
      row.appendChild(el('code', '', task.id)); body.appendChild(row);
    }
  } catch (error) { if (dialog.isConnected) body.textContent = error.message; }
}
