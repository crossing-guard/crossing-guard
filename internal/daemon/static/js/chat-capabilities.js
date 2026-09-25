import { api } from './core.js';

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

function modelPairs(capability) {
  return capability.models.map(model => [model.id, model.label, model.description]);
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
};
