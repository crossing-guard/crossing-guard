import { orchestrationApi } from './orchestration-api-error.js';

// The one profile-lane error shape: shared unwrap, profile fallback code and
// recovery line (orchestration-api-error.js owns the envelope parsing).
function profileApi(path, options) {
  return orchestrationApi(path, options, { code: 'profile_error', recovery: 'Review the profile and retry.' });
}

function bytesToBase64(bytes) {
  const view = bytes instanceof Uint8Array ? bytes : new Uint8Array(bytes);
  let binary = '';
  const chunkSize = 0x8000;
  for (let offset = 0; offset < view.length; offset += chunkSize) {
    binary += String.fromCharCode(...view.subarray(offset, offset + chunkSize));
  }
  return btoa(binary);
}

function loadProfiles() {
  return profileApi('/api/orchestration/profiles');
}

function loadProfile(profileID) {
  return profileApi('/api/orchestration/profiles/' + encodeURIComponent(profileID));
}

function previewProfile(sourceName, bytes) {
  return profileApi('/api/orchestration/profiles/preview', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ source_name: sourceName, source_base64: bytesToBase64(bytes) }),
  });
}

function selectProfile(sourceName, bytes, preview) {
  return profileApi('/api/orchestration/profiles/select', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ source_name: sourceName, source_base64: bytesToBase64(bytes),
      source_digest: preview.source_digest, bundle_digest: preview.bundle_digest,
      state_token: preview.state_token, confirmed: true }),
  });
}

export { loadProfiles, loadProfile, previewProfile, selectProfile, bytesToBase64 };
