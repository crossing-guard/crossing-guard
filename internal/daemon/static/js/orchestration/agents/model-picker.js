// The runtime-and-model picker with effort (plan §7 Settings; RT-7). Since model
// routes (team rest-of-release plan §5.5) it is the way a route is made on
// Settings → Models; the agent pages pick a route by name (route-picker.js).
// Runtimes and their models come from what the daemon publishes; the stored
// model is always an option and round-trips unchanged; typed model text is
// offered only where the runtime says it accepts a custom model.
import { createEffortPicker } from '../../task/thinking-effort.js';
import { el, selectBox, textInput, field } from './agent-ui.js';
import { loadChatModels, modelPairs, isCustomModelOption, modelHint } from '../../chat-capabilities.js';

// managedRuntimes are the runtimes a managed agent can run on: they start
// tasks and publish at least one normal-risk (read-only) mode.
export function managedRuntimes(capabilities = []) {
  return capabilities.filter(item => item.canStart && item.modes.some(mode => mode.risk === 'normal'));
}

const CUSTOM = '\u0000custom';

// modelPicker returns { node, value() } for a { runtime, model, mode } choice.
// A route has no mode (a mode is the place's): withMode false leaves it out.
export function modelPicker(capabilities, current = {}, { withMode = true, onChange = () => {} } = {}) {
  const runtimes = managedRuntimes(capabilities);
  const runtime = selectBox(runtimes.map(item => [item.runtime, item.displayName]), current.runtime || runtimes[0]?.runtime, 'Runs on');
  const model = document.createElement('select');
  model.setAttribute('aria-label', 'Model');
  const custom = textInput('', 'Model id');
  const mode = document.createElement('select');
  mode.setAttribute('aria-label', 'Read-only mode');
  const modeField = field('Read-only mode', mode);
  const lists = {};
  const requests = {};
  let effort = current.thinking_effort || null;
  let effortKey = JSON.stringify([runtime.value, current.model || '']);
  const effortDrafts = new Map([[effortKey, effort]]);
  const picker = createEffortPicker({ inline: true, modelControls: [field('Model', model), custom],
    onChange: value => { effort = value; effortDrafts.set(effortKey, value); syncEffort(); onChange(); }, onRefresh: refreshModels });
  function syncEffort() {
    const id = model.value === CUSTOM ? custom.value.trim() : model.value;
    const nextKey = JSON.stringify([runtime.value, id]);
    if (nextKey !== effortKey) { effortKey = nextKey; effort = effortDrafts.get(nextKey) || { kind: 'inherit' }; }
    const list = lists[runtime.value];
    picker.update({ modelId: id, model: model.selectedOptions[0]?.textContent, selection: effort,
      capability: list?.models.find(item => item.id === id)?.effort,
      unavailable: list?.state !== 'fresh' ? 'Model options are not verified. Refresh models to try again.' : '' });
  }
  const fill = () => { fillModels({ runtimes, runtime, model, custom, mode, modeField, lists, current, withMode }); syncEffort(); };
  function refreshModels(refresh = false) {
    const requested = runtime.value;
    const generation = (requests[requested] || 0) + 1;
    requests[requested] = generation;
    return loadChatModels(requested, { refresh }).then(list => {
      if (requests[requested] !== generation) return;
      lists[requested] = list;
      if (runtime.value === requested && node.isConnected) fill();
    });
  }
  runtime.onchange = () => { fill(); void refreshModels(); onChange(); };
  model.onchange = () => { custom.classList.toggle('hidden', model.value !== CUSTOM); syncEffort(); onChange(); };
  custom.oninput = () => { syncEffort(); onChange(); };
  fill();
  const node = el('div', 'agents-model-picker');
  node.append(field('Runs on', runtime), picker.node, modeField);
  void refreshModels();
  return {
    node,
    value: () => ({ runtime: runtime.value, model: model.value === CUSTOM ? custom.value.trim() : model.value,
      ...(withMode ? { mode: mode.value } : {}), ...(effort ? { thinking_effort: effort } : {}) }),
  };
}

function fillModels({ runtimes, runtime, model, custom, mode, modeField, lists, current, withMode }) {
  const capability = runtimes.find(item => item.runtime === runtime.value);
  const sameRuntime = runtime.value === current.runtime;
  const previousMode = mode.options.length ? mode.value : (sameRuntime ? current.mode : '');
  const previous = model.options.length ? model.value : (sameRuntime ? String(current.model || '') : '');
  const pairs = modelPairs(capability, lists[runtime.value]).filter(([id]) => id !== '' && !isCustomModelOption(capability, id));
  const options = [['', 'Default model'], ...pairs.map(([id, label]) => [id, label])];
  if (previous && previous !== CUSTOM && !options.some(([id]) => id === previous)) options.push([previous, previous]);
  if (capability?.acceptsCustomModel) options.push([CUSTOM, 'Another model id…']);
  model.replaceChildren(...options.map(([id, label]) => { const option = document.createElement('option'); option.value = id; option.textContent = label; return option; }));
  model.value = options.some(([id]) => id === previous) ? previous : '';
  custom.placeholder = modelHint(capability);
  custom.classList.toggle('hidden', model.value !== CUSTOM);
  const modes = (capability?.modes || []).filter(item => item.risk === 'normal');
  mode.replaceChildren(...modes.map(item => { const option = document.createElement('option'); option.value = item.id; option.textContent = item.label || 'Read only'; return option; }));
  mode.value = modes.some(item => item.id === previousMode) ? previousMode : (modes[0]?.id || '');
  modeField.classList.toggle('hidden', !withMode || modes.length < 2);
}
