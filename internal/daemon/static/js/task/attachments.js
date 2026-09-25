import { el, formatBytes } from '../core.js';
import * as inputAPI from './task-input-api.js';

const ACCEPT = '.png,.jpg,.jpeg,.gif,.webp,.txt,.md,.go,.js,.mjs,.css,.html,.json,.yaml,.yml,.sh,.sql';

export function reordered(items, from, to) {
  const result = [...items];
  if (from < 0 || from >= result.length || to < 0 || to >= result.length || from === to) return result;
  const [item] = result.splice(from, 1);
  result.splice(to, 0, item);
  return result;
}

export function inputCapabilityIssue(capability, inputs) {
  const declared = new Map((capability?.inputs || []).map(input => [input.kind, input]));
  for (const input of inputs || []) {
    const supported = declared.get(input.kind);
    if (!supported) return (capability?.displayName || 'Selected runtime') + ' does not accept ' + input.kind + ' attachments.';
    if (supported.modelConditional || supported.modeConditional) return supported.note || (input.kind + ' support depends on the selected model or mode.');
  }
  return '';
}

export function createAttachments({ root, controls, storageKey, capability, api = inputAPI } = {}) {
  const tray = el('div', 'attachment-tray hidden');
  tray.setAttribute('aria-live', 'polite');
  const error = el('div', 'attachment-error hidden');
  const warning = el('div', 'attachment-warning hidden');
  const picker = document.createElement('input');
  picker.type = 'file'; picker.multiple = true; picker.accept = ACCEPT; picker.className = 'hidden';
  const add = el('button', 'iconbtn attachment-add', '+');
  add.type = 'button'; add.title = 'Attach images or files'; add.setAttribute('aria-label', 'Attach images or files');
  controls.insertBefore(add, controls.firstChild);
  root.insertBefore(tray, controls);
  root.insertBefore(error, controls);
  root.insertBefore(warning, controls);
  root.appendChild(picker);

  const controller = new AbortController();
  let scopeID = '';
  let inputs = [];
  let busy = 0;
  let selectedCapability = capability || null;
  const previews = new Map();
  const key = String(storageKey || 'cg_task_inputs:new');

  const saveScope = () => {
    if (scopeID) sessionStorage.setItem(key, scopeID);
    else sessionStorage.removeItem(key);
  };
  const setError = message => {
    error.textContent = String(message || ''); error.classList.toggle('hidden', !message);
  };
  const paint = () => {
    tray.replaceChildren();
    tray.classList.toggle('hidden', inputs.length === 0 && busy === 0);
    inputs.forEach((input, index) => {
      const row = el('div', 'attachment-item');
      if (previews.has(input.id)) {
        const image = document.createElement('img'); image.alt = ''; image.src = previews.get(input.id);
        row.appendChild(image);
      } else row.appendChild(el('span', 'attachment-kind', input.kind === 'image' ? '▧' : '≡'));
      const detail = el('span', 'attachment-detail');
      detail.append(el('span', 'attachment-name', input.name),
        el('span', 'attachment-meta', input.kind + ' · ' + formatBytes(input.prepared_bytes)));
      row.appendChild(detail);
      const up = attachmentButton('↑', 'Move attachment earlier', index === 0, () => move(index, index - 1));
      const down = attachmentButton('↓', 'Move attachment later', index === inputs.length - 1, () => move(index, index + 1));
      const remove = attachmentButton('×', 'Remove attachment', false, () => void removeOne(input.id));
      row.append(up, down, remove); tray.appendChild(row);
    });
    if (busy > 0) tray.appendChild(el('div', 'attachment-pending', 'Inspecting ' + busy + ' attachment' + (busy === 1 ? '…' : 's…')));
    const issue = inputCapabilityIssue(selectedCapability, inputs);
    warning.textContent = issue; warning.classList.toggle('hidden', !issue);
  };
  const loadPreview = async input => {
    if (input.kind !== 'image' || !input.preview_available || previews.has(input.id)) return;
    try {
      const blob = await api.fetchTaskInputContent(scopeID, input.id, controller.signal);
      const url = URL.createObjectURL(blob); previews.set(input.id, url); paint();
    } catch { /* metadata remains useful when preview retrieval fails */ }
  };
  const stageFiles = async (files, source) => {
    const selected = Array.from(files || []);
    if (!selected.length) return;
    setError(''); busy += selected.length; paint();
    for (const file of selected) {
      try {
        const result = await api.stageTaskInput(file, source, scopeID, controller.signal);
        scopeID = String(result.scope?.id || '');
        inputs = Array.isArray(result.scope?.inputs) ? result.scope.inputs : [];
        saveScope();
        const added = inputs.find(input => input.id === result.input?.id);
        if (added) void loadPreview(added);
      } catch (cause) { setError(cause?.message || cause); }
      finally { busy--; paint(); }
    }
  };
  const removeOne = async inputID => {
    setError('');
    try {
      const scope = await api.removeTaskInput(scopeID, inputID, controller.signal);
      const url = previews.get(inputID); if (url) URL.revokeObjectURL(url); previews.delete(inputID);
      inputs = Array.isArray(scope?.inputs) ? scope.inputs : [];
      if (!inputs.length) { scopeID = ''; saveScope(); }
      paint();
    } catch (cause) { setError(cause?.message || cause); }
  };
  const move = (from, to) => { inputs = reordered(inputs, from, to); paint(); };

  add.addEventListener('click', () => picker.click());
  picker.addEventListener('change', () => { void stageFiles(picker.files, 'picker'); picker.value = ''; });
  root.addEventListener('dragover', event => { if (event.dataTransfer?.types?.includes('Files')) { event.preventDefault(); root.classList.add('attachment-drop'); } });
  root.addEventListener('dragleave', () => root.classList.remove('attachment-drop'));
  root.addEventListener('drop', event => {
    root.classList.remove('attachment-drop');
    if (event.dataTransfer?.files?.length) { event.preventDefault(); void stageFiles(event.dataTransfer.files, 'drop'); }
  });
  root.addEventListener('paste', event => {
    const files = Array.from(event.clipboardData?.files || []);
    if (files.length) { event.preventDefault(); void stageFiles(files, 'paste'); }
  });

  const restore = async () => {
    const stored = sessionStorage.getItem(key);
    if (!stored) return;
    try {
      const scope = await api.listTaskInputs(stored, controller.signal);
      scopeID = String(scope?.id || ''); inputs = Array.isArray(scope?.inputs) ? scope.inputs : [];
      saveScope(); paint(); inputs.forEach(input => void loadPreview(input));
    } catch { scopeID = ''; inputs = []; saveScope(); paint(); }
  };
  void restore();
  paint();

  return {
    tray,
    references() { return inputs.length ? { input_scope_id: scopeID, input_ids: inputs.map(input => input.id) } : {}; },
    hasPending() { return busy > 0; },
    snapshot() { return inputs.map(input => ({ ...input })); },
    setCapability(next) { selectedCapability = next; paint(); },
    claimed() {
      for (const url of previews.values()) URL.revokeObjectURL(url);
      previews.clear(); scopeID = ''; inputs = []; saveScope(); paint(); setError('');
    },
    dispose() {
      controller.abort(); for (const url of previews.values()) URL.revokeObjectURL(url); previews.clear();
    },
  };
}

function attachmentButton(label, title, disabled, click) {
  const button = el('button', 'attachment-action', label);
  button.type = 'button'; button.title = title; button.setAttribute('aria-label', title); button.disabled = disabled;
  button.addEventListener('click', click); return button;
}

