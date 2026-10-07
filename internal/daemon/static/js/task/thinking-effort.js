// Shared presentation for the composer and managed model settings. Native IDs
// remain opaque; adapters supply labels and model-scoped capabilities.
// `inline` is the picker inside a form (a model route's sheet): the model and effort
// controls are the form's own fields, always shown — no trigger and no panel that
// opens over the form or closes when focus leaves it.
export function createEffortPicker({ modelControls = [], onChange, onReload = () => {}, onRetry, onRefresh, inline = false }) {
  const node = document.createElement('div'); node.className = 'effort-picker' + (inline ? ' effort-picker-inline' : '');
  const trigger = document.createElement('button'); trigger.type = 'button'; trigger.className = 'ctl btn ghost effort-trigger';
  trigger.setAttribute('aria-expanded', 'false');
  const modelName = document.createElement('span');
  const label = document.createElement('span'); label.className = 'effort-label';
  const ring = document.createElement('span'); ring.className = 'effort-ring'; ring.setAttribute('aria-hidden', 'true');
  trigger.append(modelName, label, ring);
  const panel = document.createElement('div'); panel.className = 'effort-panel'; panel.hidden = true;
  const heading = document.createElement('strong'); heading.textContent = 'Thinking effort';
  const select = document.createElement('select'); select.setAttribute('aria-label', 'Thinking effort');
  const note = document.createElement('p'); note.className = 'sub';
  const problem = document.createElement('div'); problem.className = 'effort-problem'; problem.setAttribute('role', 'status');
  const reload = document.createElement('button'); reload.type = 'button'; reload.className = 'btn'; reload.textContent = 'Reload saved setting'; reload.onclick = onReload;
  const retry = document.createElement('button'); retry.type = 'button'; retry.className = 'btn'; retry.textContent = 'Retry saving'; retry.onclick = onRetry;
  const refresh = document.createElement('button'); refresh.type = 'button'; refresh.className = 'btn'; refresh.textContent = 'Refresh models';
  let refreshGeneration = 0, refreshing = false;
  const catalogStatus = document.createElement('p'); catalogStatus.className = 'sub'; catalogStatus.setAttribute('role', 'status');
  async function refreshModels(force) {
    if (!onRefresh) return;
    const generation = ++refreshGeneration;
    refreshing = true; refresh.setAttribute('aria-disabled', 'true'); catalogStatus.textContent = 'Checking models…';
    try { await onRefresh(force); }
    catch { if (generation === refreshGeneration) catalogStatus.textContent = 'Could not refresh models. Try again.'; }
    finally {
      if (generation === refreshGeneration) {
        refreshing = false; refresh.setAttribute('aria-disabled', 'false');
        if (catalogStatus.textContent === 'Checking models…') catalogStatus.textContent = '';
      }
    }
  }
  refresh.hidden = !onRefresh;
  refresh.onclick = () => { if (!refreshing) void refreshModels(true); };
  const focusModel = () => (modelControls.map(control => control.tagName === 'SELECT' ? control : control.querySelector('select')).find(Boolean) || select).focus();
  const choose = document.createElement('button'); choose.type = 'button'; choose.className = 'btn'; choose.textContent = 'Choose a model'; choose.onclick = focusModel;
  panel.append(...modelControls, heading, select, note, choose, retry, reload, refresh, catalogStatus);
  node.append(trigger, panel, problem);
  function toggle(open) {
    if (inline) return; // always open: there is nothing to toggle
    panel.hidden = !open; trigger.setAttribute('aria-expanded', String(open)); if (open) void refreshModels(false);
  }
  if (inline) { trigger.hidden = true; panel.hidden = false; }
  trigger.onclick = () => toggle(panel.hidden);
  node.addEventListener('keydown', event => {
    if (event.key === 'Escape' && !panel.hidden) { event.preventDefault(); event.stopPropagation(); toggle(false); trigger.focus(); }
  });
  node.addEventListener('focusout', event => { if (!node.contains(event.relatedTarget)) toggle(false); });
  select.onchange = () => onChange(select.value === '' ? { kind: 'inherit' } : { kind: 'level', value: select.value });
  return {
    node, open() { toggle(true); focusModel(); },
    update({ model = 'Session / configured default', modelId = '', capability, selection, busy = false, error = '', unavailable = '', legacy = '', pendingIdentity = false }) {
      modelName.textContent = model;
      const choices = capability?.state === 'supported' ? capability.choices || [] : [];
      const selected = selection?.kind === 'level' ? selection.value : '';
      const option = choices.find(choice => choice.id === selected);
      label.textContent = busy ? 'Saving…' : legacy || (selected ? option?.label || 'Unavailable choice' : 'Default');
      trigger.setAttribute('aria-label', model + ', thinking effort ' + label.textContent);
      const options = [{ id: '', label: 'Use runtime setting' }, ...choices];
      if (selected && !option) options.push({ id: selected, label: 'Unavailable saved choice' });
      select.replaceChildren(...options.map(choice => {
        const item = document.createElement('option'); item.value = choice.id; item.textContent = choice.label; return item;
      }));
      select.value = selected;
      // Even without evidence the inheritance option remains a recovery path.
      select.disabled = busy || (!modelId && !selected);
      choose.hidden = Boolean(modelId);
      const unsupported = capability?.state === 'unsupported' && !selected;
      select.hidden = unsupported; heading.hidden = unsupported; ring.hidden = unsupported; label.hidden = unsupported;
      for (const item of select.options) if (item.value) item.disabled = Boolean(unavailable) || !choices.some(choice => choice.id === item.value);
      note.textContent = (pendingIdentity ? 'For this chat; not saved yet. ' : '') + [!modelId ? 'Choose a model above to set thinking effort.' : '', unavailable || (modelId && capability?.state === 'unsupported' ? 'This model does not offer thinking effort.' : choices.length ? 'Applies to future messages until you change it. Default uses the runtime setting.' : modelId ? 'This model has no verified effort options. Default uses the runtime setting.' : '')].filter(Boolean).join(' ');
      problem.textContent = error || (busy ? 'Saving thinking effort…' : '');
      reload.hidden = !error; retry.hidden = !error || !onRetry; retry.disabled = busy;
      ring.classList.toggle('effort-ring-active', Boolean(selected));
    },
  };
}
