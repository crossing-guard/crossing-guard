import { api } from './core.js';

let snapshotPromise;

async function runtimeIntegrationApi(path, options) {
  try {
    return await api(path, options);
  } catch (error) {
    try {
      const payload = JSON.parse(String(error?.message || ''));
      if (typeof payload?.message === 'string' && payload.message.trim()) {
        const normalized = new Error(payload.message.trim());
        normalized.status = error.status;
        normalized.code = String(payload.error || '');
        normalized.result = payload.result;
        throw normalized;
      }
    } catch (parseError) {
      if (parseError instanceof SyntaxError) throw error;
      throw parseError;
    }
    throw error;
  }
}

function loadRuntimeIntegrations(fresh = false) {
  if (fresh) snapshotPromise = null;
  if (!snapshotPromise) {
    snapshotPromise = runtimeIntegrationApi('/api/runtime-integrations').then(normalizeRuntimeIntegrations).catch(error => {
      snapshotPromise = null;
      throw error;
    });
  }
  return snapshotPromise;
}

function normalizeRuntimeIntegrations(raw) {
  if (!Array.isArray(raw?.integrations)) throw new Error('Runtime connections returned an invalid response.');
  const seen = new Set();
  return Object.freeze(raw.integrations.map(item => {
    const descriptor = item?.descriptor;
    const runtime = String(descriptor?.runtime || '');
    if (!runtime || seen.has(runtime) || !descriptor?.display_name || !descriptor?.config_path) {
      throw new Error('Runtime connections contain an incomplete provider descriptor.');
    }
    seen.add(runtime);
    const surfaces = Array.isArray(descriptor.surfaces) ? descriptor.surfaces.map(surface => ({
      id: String(surface?.id || ''), label: String(surface?.label || ''),
      description: String(surface?.description || ''),
    })).filter(surface => surface.id && surface.label) : [];
    return Object.freeze({
      runtime, displayName: String(descriptor.display_name), configPath: String(descriptor.config_path),
      configLabel: String(descriptor.config_label || 'Provider configuration'),
      hookPhases: Object.freeze(Array.isArray(descriptor.hook_phases) ? descriptor.hook_phases.map(String) : []),
      surfaces: Object.freeze(surfaces),
      limitations: Object.freeze(Array.isArray(descriptor.limitations) ? descriptor.limitations.map(String) : []),
      verificationSteps: Object.freeze(Array.isArray(descriptor.verification_steps) ? descriptor.verification_steps.map(String) : []),
      state: String(item.state || 'needs_attention'), detected: item.detected === true,
      consented: item.consented === true, consentOrigin: String(item.consent_origin || ''),
      attached: item.attached === true, current: item.current === true,
      binaryPresent: item.binary_present === true, hookBinary: String(item.hook_binary || ''),
      problem: String(item.problem || ''), revision: String(item.revision || ''),
    });
  }));
}

function previewRuntimeIntegration(runtime, operation) {
  if (operation !== 'connect' && operation !== 'disconnect') throw new Error('Unknown runtime connection operation.');
  return runtimeIntegrationApi('/api/runtime-integrations/' + encodeURIComponent(runtime) + '/preview-' + operation,
    { method: 'POST' });
}

function commitRuntimeIntegration(runtime, operation, previewToken) {
  if (operation !== 'connect' && operation !== 'disconnect') throw new Error('Unknown runtime connection operation.');
  return runtimeIntegrationApi('/api/runtime-integrations/' + encodeURIComponent(runtime) + '/' + operation, {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ preview_token: previewToken, confirmed: true }),
  });
}

function startRuntimeIntegrationWatch(runtime, surface) {
  return runtimeIntegrationApi('/api/runtime-integrations/' + encodeURIComponent(runtime) + '/watch', {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ surface }),
  });
}

function readRuntimeIntegrationWatch(runtime, token) {
  return runtimeIntegrationApi('/api/runtime-integrations/' + encodeURIComponent(runtime) + '/watch/' + encodeURIComponent(token));
}

function confirmRuntimeIntegrationVisibleBlock(runtime, token) {
  return runtimeIntegrationApi('/api/runtime-integrations/' + encodeURIComponent(runtime) + '/watch/' +
    encodeURIComponent(token) + '/confirm-visible', { method: 'POST' });
}

export { loadRuntimeIntegrations, previewRuntimeIntegration, commitRuntimeIntegration,
  startRuntimeIntegrationWatch, readRuntimeIntegrationWatch, confirmRuntimeIntegrationVisibleBlock };
