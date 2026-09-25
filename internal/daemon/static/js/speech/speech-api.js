// Speech HTTP client: capabilities, disclosure confirmation, and the dictation
// create / frames / stream / finish / cancel calls. Transport only; the
// dictation state machine lives in task/dictation.js. Every path is the
// canonical /api/v1 surface; the daemon rewrites it before its mux.
import { cpHeaders } from '../core.js';
import { readSSE } from './../task/task-stream.js';

const ROOT = '/api/v1/speech';

export class SpeechHTTPError extends Error {
  constructor(operation, status, code, detail) {
    super(operation + ' failed: ' + (detail || ('HTTP ' + status)));
    this.name = 'SpeechHTTPError';
    this.status = status;
    this.code = code || 'http_error';
  }
}

async function checkedFetch(operation, path, options = {}, fetchImpl = fetch) {
  const response = await fetchImpl(path, { ...options, headers: { ...cpHeaders(), ...(options.headers || {}) } });
  if (response.ok) return response;
  const raw = (await response.text()).trim();
  let detail = raw, code = '';
  try {
    const body = JSON.parse(raw);
    detail = String(body?.message || raw); code = String(body?.code || '');
  } catch { /* plain text remains the fallback detail */ }
  throw new SpeechHTTPError(operation, response.status, code, detail);
}

export async function loadSpeechCapabilities(fetchImpl = fetch) {
  const response = await checkedFetch('speech capabilities', ROOT + '/capabilities', {}, fetchImpl);
  return response.json();
}

export async function confirmDisclosure(backendId, version, fetchImpl = fetch) {
  const response = await checkedFetch('disclosure', '/api/v1/speech-disclosures', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ backend_id: backendId, operation: 'transcription', disclosure_version: version }),
  }, fetchImpl);
  return response.json();
}

export async function createDictation(request, fetchImpl = fetch) {
  const response = await checkedFetch('dictation', ROOT + '/dictations', {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(request || {}),
  }, fetchImpl);
  return response.json();
}

// sendFrame posts one sequenced PCM frame. seq and offset are the browser's
// bookkeeping; the daemon refuses gaps and duplicates.
export async function sendFrame(id, seq, offset, bytes, fetchImpl = fetch) {
  const response = await checkedFetch('dictation frame', ROOT + '/dictations/' + encodeURIComponent(id) + '/frames?seq=' + seq + '&offset=' + offset, {
    method: 'POST', headers: { 'Content-Type': 'application/octet-stream' }, body: bytes,
  }, fetchImpl);
  if (typeof response.arrayBuffer === 'function') await response.arrayBuffer().catch(() => {});
}

// openDictationStream reads named events until the stream ends. Each event is
// delivered as the parsed JSON payload; the promise resolves when the daemon
// closes the stream after the terminal event.
export async function openDictationStream(id, onEvent, signal, fetchImpl = fetch) {
  const response = await checkedFetch('dictation stream', ROOT + '/dictations/' + encodeURIComponent(id) + '/stream',
    { signal, headers: { Accept: 'text/event-stream' } }, fetchImpl);
  await readSSE(response.body, frame => {
    const payload = parsePayload(frame);
    if (payload) onEvent(payload);
  });
}

// readSSE hands over the already-parsed JSON payload; a string is tolerated
// for tests that feed raw frames.
function parsePayload(frame) {
  if (frame && typeof frame === 'object') return frame;
  try { return JSON.parse(String(frame)); } catch { return null; }
}

export async function finishDictation(id, reason, fetchImpl = fetch) {
  const response = await checkedFetch('dictation finish', ROOT + '/dictations/' + encodeURIComponent(id) + '/finish', {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ reason: reason || 'release' }),
  }, fetchImpl);
  return response.json();
}

export async function cancelDictation(id, fetchImpl = fetch) {
  await checkedFetch('dictation cancel', ROOT + '/dictations/' + encodeURIComponent(id), { method: 'DELETE' }, fetchImpl);
}
