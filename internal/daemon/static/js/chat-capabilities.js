import { api, fmtLimit, fmtPrice } from './core.js';

let snapshotPromise = null;

function loadChatCapabilities() {
  if (!snapshotPromise) {
    snapshotPromise = api('/api/chat/capabilities').then(normalizeCapabilities).catch(error => {
      snapshotPromise = null;
      throw error;
    });
  }
  return snapshotPromise;
}

function normalizeCapabilities(value) {
  if (!Array.isArray(value)) throw new Error('Chat capabilities returned an invalid response.');
  const seen = new Set();
  const capabilities = value.map(raw => {
    const runtime = String(raw?.runtime || '');
    if (!runtime || seen.has(runtime)) throw new Error('Chat capabilities contain an invalid runtime.');
    seen.add(runtime);
    const modes = Array.isArray(raw.modes) ? raw.modes.map(mode => ({
      id: String(mode?.id || ''), label: String(mode?.label || ''),
      description: String(mode?.description || ''), risk: String(mode?.risk || 'normal'),
    })) : [];
    const models = Array.isArray(raw.models) ? raw.models.map(model => ({
      id: String(model?.id || ''), label: String(model?.label || ''),
      description: String(model?.description || ''), custom: model?.custom === true,
      vendorAuthRequired: typeof model?.vendor_auth_required === 'boolean' ? model.vendor_auth_required : null,
    })) : [];
    const inputs = Array.isArray(raw.inputs) ? raw.inputs.map(input => ({
      kind: String(input?.kind || ''),
      mediaTypes: Object.freeze(Array.isArray(input?.media_types) ? input.media_types.map(String) : []),
      canStart: input?.can_start === true, canResume: input?.can_resume === true,
      modelConditional: input?.model_conditional === true,
      modeConditional: input?.mode_conditional === true,
      note: String(input?.note || ''),
    })) : [];
    if (!raw.display_name || !modes.some(mode => mode.id === '')) {
      throw new Error('Chat capability for ' + runtime + ' is incomplete.');
    }
    return Object.freeze({
      runtime, displayName: String(raw.display_name),
      // Interface revision (Slice A) and governance lane (Slice C) are
      // presentation facts; dropping them here silently blanked the Settings
      // compatibility lines.
      interfaceRevision: String(raw.interface_revision || ''),
      governanceLane: String(raw.governance_lane || ''),
      governanceNote: String(raw.governance_note || ''),
      canStart: raw.can_start === true,
      canResume: raw.can_resume === true, canSignIn: raw.can_sign_in === true,
      vendorAuthDefault: raw.vendor_auth_default === true,
      supportsBaseURL: raw.supports_base_url === true,
      supportsAuthToken: raw.supports_auth_token === true,
      acceptsCustomModel: raw.accepts_custom_model === true,
      modelHint: String(raw.model_hint || 'model id'),
      // Where a helper's message into a source session lands (or that it cannot).
      messageDelivery: Object.freeze({ supported: raw.message_delivery?.supported === true,
        boundary: String(raw.message_delivery?.boundary || ''), detail: String(raw.message_delivery?.detail || '') }),
      modes: Object.freeze(modes), models: Object.freeze(models), inputs: Object.freeze(inputs),
    });
  });
  if (!capabilities.length) throw new Error('No chat runtimes are registered.');
  return Object.freeze(capabilities);
}

function findChatCapability(capabilities, runtime) {
  return capabilities.find(item => item.runtime === runtime) || null;
}

function chooseChatCapability(capabilities, preferred) {
  return findChatCapability(capabilities, preferred)
    || capabilities.find(item => item.canStart)
    || capabilities[0];
}

function capabilityPairs(capabilities) {
  return capabilities.filter(item => item.canStart).map(item => [item.runtime, item.displayName]);
}

function modePairs(capability) {
  return capability.modes.map(mode => [mode.id, mode.label, mode.description]);
}

/* Runtime-reported model lists (runtime-model-catalog-and-usage design §5.1).
   Ids are opaque: never split, prefixed or built here. A list that is not
   fresh is labelled by its state; its reason belongs to Settings. */
const MODEL_STATES = new Set(['fresh', 'stale', 'unavailable', 'unsupported']);
const modelListRequests = new Map();

function positiveCount(value) {
  const number = Number(value);
  return Number.isFinite(number) && number > 0 ? number : null;
}

function normalizeChatModels(raw, runtime) {
  const state = MODEL_STATES.has(raw?.state) ? raw.state : 'unavailable';
  const models = Array.isArray(raw?.models) ? raw.models.filter(model => typeof model?.id === 'string' && model.id !== '')
    .map(model => Object.freeze({
      id: model.id, label: String(model.label || model.id), description: String(model.description || ''),
      group: String(model.group || ''), groupLabel: String(model.group_label || model.group || ''),
      contextTokens: positiveCount(model.limits?.context_tokens),
      outputTokens: positiveCount(model.limits?.output_tokens),
      inputs: Object.freeze(Array.isArray(model.inputs) ? model.inputs.map(String) : []),
      price: model.price && typeof model.price === 'object' ? model.price : null,
      pricePartial: model.price_partial === true,
      effort: model.thinking_effort || null,
    })) : [];
  return Object.freeze({
    runtime: String(raw?.runtime || runtime || ''), state,
    observedAt: String(raw?.observed_at || ''), digest: String(raw?.digest || ''),
    scope: String(raw?.scope || ''), binary: String(raw?.binary || ''),
    interfaceRevision: String(raw?.interface_revision || ''), reasonCode: String(raw?.reason_code || ''),
    rejected: Number(raw?.rejected) || 0, models: Object.freeze(models),
  });
}

// The daemon owns freshness. Only active requests are shared by consumers;
// a stale background refresh is joined once so its result reaches this caller.
function loadChatModels(runtime, { refresh = false } = {}) {
  const active = modelListRequests.get(runtime);
  if (active && (!refresh || active.refresh)) return active.request;
  const path = refresh ? '/api/chat/models/refresh?runtime=' : '/api/chat/models?runtime=';
  const entry = { refresh };
  entry.request = api(path + encodeURIComponent(runtime), refresh ? { method: 'POST' } : {})
    .then(async raw => {
      if (!refresh && raw?.state === 'stale' && !raw.reason_code) {
        // A newer explicit refresh owns recovery if it overtook this GET.
        const newer = modelListRequests.get(runtime);
        if (newer && newer !== entry) return newer.request;
        entry.refresh = true;
        raw = await api('/api/chat/models/refresh?runtime=' + encodeURIComponent(runtime), { method: 'POST' });
      }
      return normalizeChatModels(raw, runtime);
    })
    .catch(() => normalizeChatModels({ runtime, state: 'unavailable', reason_code: 'request-failed' }, runtime));
  modelListRequests.set(runtime, entry);
  entry.request.then(() => { if (modelListRequests.get(runtime) === entry) modelListRequests.delete(runtime); });
  return entry.request;
}

function findDiscoveredModel(list, id) {
  return list?.models?.find(model => model.id === id) || null;
}

// modelPairs merges the adapter's declared entries with the runtime's list:
// declared (custom last), pinned, then the rest grouped by the runtime's own
// group labels. Works on normalized and raw capability objects.
function modelPairs(capability, discovered = null, pins = []) {
  const declared = (capability?.models || []).map(model => ({ id: String(model.id ?? ''),
    label: String(model.label || ''), description: String(model.description || ''), custom: model.custom === true }));
  const listed = discovered?.models || [];
  const pinned = new Set(pins.filter(id => listed.some(model => model.id === id)));
  const describe = model => [model.groupLabel, model.contextTokens ? fmtLimit(model.contextTokens) : '',
    model.price ? fmtPrice(model.price) : ''].filter(Boolean).join(' · ');
  const rest = listed.filter(model => !pinned.has(model.id)).sort((a, b) =>
    a.groupLabel.localeCompare(b.groupLabel) || a.label.localeCompare(b.label));
  const named = model => model.label + (declared.some(item => item.label === model.label) ? ' · ' + model.id : '')
    + (model.groupLabel ? ' — ' + model.groupLabel : '');
  return [
    ...declared.filter(model => !model.custom).map(model => [model.id, model.label, model.description]),
    ...listed.filter(model => pinned.has(model.id)).map(model => [model.id, '★ ' + named(model), describe(model)]),
    ...rest.map(model => [model.id, named(model), describe(model)]),
    ...declared.filter(model => model.custom).map(model => [model.id, model.label, model.description]),
  ];
}

// isCustomModelOption answers from the declared entry's own flag, never from
// a sentinel id (A-RT12).
function isCustomModelOption(capability, id) {
  return (capability?.models || []).some(model => model.custom === true && String(model.id) === id);
}

// modelHint is the placeholder a runtime publishes for a typed model id.
function modelHint(capability) {
  return String(capability?.modelHint || capability?.model_hint || 'model id');
}

function runtimeChatDefaults(defaults, runtime) {
  const scoped = defaults?.runtimes?.[runtime];
  if (scoped && typeof scoped === 'object') return { ...defaults, ...scoped };
  // Legacy flat account fields belonged to the saved default runtime. Do not
  // copy a provider token or binary override into every newly discovered runtime.
  if (defaults?.runtime === runtime) return { ...defaults };
  return { cwd: defaults?.cwd || '' };
}

export {
  loadChatCapabilities, normalizeCapabilities, findChatCapability, chooseChatCapability,
  capabilityPairs, modePairs, modelPairs, runtimeChatDefaults,
  loadChatModels, normalizeChatModels, findDiscoveredModel, isCustomModelOption, modelHint,
};
