// The memory-recall control (team rest-of-release plan §6.3 OD-22, OD-24; §14
// Q25; criteria 87, 92). One control, used under a runtime's header line on
// Settings → Runtimes and in the handoff open sheet: the state, the one action
// this daemon may take, and the consent that names the settings file it edits.
// Nothing is written without that second click.
import { el, button, chip } from '../orchestration/agents/agent-ui.js';
import { setRecall } from './handoff-api.js';
import { recallView, recallConsent, recallOutcome } from './handoff-model.js';

function factGrid(rows) {
  const grid = el('div', 'handoff-facts');
  for (const item of rows) grid.append(el('span', 'handoff-facts-k', item.label), el('span', '', item.value));
  return grid;
}

// recallControl returns the control's node for one runtime's state.
//   state        GET /api/memory/attach's entry for the runtime
//   runtimeLabel runtime id → the name the daemon publishes
//   onChanged(state) runs after the settings file was changed, or left alone
//   heading      the control's own heading; the open sheet passes none
export function recallControl({ state, runtimeLabel = String, onChanged = () => {}, heading = 'Memory recall' }) {
  const host = el('div', 'recall-control');
  const control = { host, state, runtimeLabel, onChanged, heading, message: '' };
  paint(control);
  return host;
}

function paint(control) {
  const view = recallView(control.state, control.runtimeLabel);
  const head = el('div', 'recall-head');
  if (control.heading) head.appendChild(el('strong', '', control.heading));
  head.appendChild(chip(view.word, view.chip));
  control.host.replaceChildren(head);
  if (view.note) control.host.appendChild(el('div', 'sub', view.note));
  if (control.message) control.host.appendChild(el('div', 'sub', control.message));
  if (!view.action) return;
  const act = button(view.action.id === 'attach' ? view.action.label + ' for ' + view.label + '…' : view.action.label + '…', '',
    () => paintConsent(control, view, view.action.id));
  control.host.appendChild(el('div', 'row recall-actions')).appendChild(act);
}

// paintConsent replaces the action with what it will do to which file, and the
// confirming button. Cancel leaves the file as it is.
function paintConsent(control, view, action) {
  const consent = recallConsent(view, action);
  const box = el('div', 'recall-consent');
  box.append(el('strong', '', consent.title), factGrid(consent.rows));
  const problem = el('div', 'sub runtime-integration-problem');
  const cancel = button('Cancel', '', () => { control.message = ''; paint(control); });
  const confirm = button(consent.label, 'primary', async () => {
    confirm.disabled = cancel.disabled = true;
    try {
      const result = await setRecall(view.runtime, action);
      control.state = result.state || control.state;
      control.message = recallOutcome(result);
      paint(control);
      control.onChanged(control.state);
    } catch (error) {
      confirm.disabled = cancel.disabled = false;
      problem.textContent = (action === 'attach' ? 'Not turned on: ' : 'Not turned off: ') + (error.message || error);
    }
  });
  const actions = el('div', 'row recall-actions');
  actions.append(cancel, confirm);
  box.append(actions, problem);
  control.host.replaceChildren(box);
}
