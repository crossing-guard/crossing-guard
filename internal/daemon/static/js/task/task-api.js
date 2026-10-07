import { cpHeaders } from '../core.js';

const ROOT = '/api/v1/runtime-tasks';

export class TaskHTTPError extends Error {
  constructor(operation, status, detail, code = '', field = '') {
    super(operation + ' failed: ' + (detail || ('HTTP ' + status)));
    this.name = 'TaskHTTPError';
    this.status = status;
    this.detail = detail || '';
    this.field = field;
    this.code = code; // the daemon's machine-readable reason, when it gave one
  }
}

async function checkedFetch(operation, path, options = {}) {
  const response = await fetch(ROOT + path, options);
  if (response.ok) return response;
  const raw = (await response.text()).trim();
  let detail = raw, code = '', field = '';
  try {
    const parsed = JSON.parse(raw);
    detail = String(parsed?.message || raw);
    code = String(parsed?.code || '');
    field = typeof parsed?.field === 'string' ? parsed.field : '';
  } catch { /* plain text */ }
  throw new TaskHTTPError(operation, response.status, detail, code, field);
}

export async function fetchTaskSnapshot(signal, filters = {}) {
  const response = await checkedFetch('Task snapshot', '?' + new URLSearchParams({ ...filters, limit: '200' }), {
    headers: cpHeaders(), signal,
  });
  return response.json();
}

export function openTaskStream(afterEventID, signal) {
  return checkedFetch('Task stream', '/stream?after=' + encodeURIComponent(afterEventID), {
    headers: cpHeaders(), signal,
  });
}

export async function createRuntimeTask(request, idempotencyKey) {
  const response = await checkedFetch('Send', '', {
    method: 'POST',
    headers: { ...cpHeaders(), 'Content-Type': 'application/json', 'Idempotency-Key': idempotencyKey },
    body: JSON.stringify(request),
  });
  return response.json();
}

export async function interruptRuntimeTask(taskID) {
  const response = await checkedFetch('Stop', '/' + encodeURIComponent(taskID) + '/interrupt', {
    method: 'POST', headers: cpHeaders(),
  });
  return response.json();
}

// Session effort is durable execution intent, separate from console appearance.
export async function sessionEffort(context, selection, token) {
  const path = '/api/session-turn-settings';
  const options = selection ? { method: 'PUT', headers: { ...cpHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify({ ...context, thinking_effort: selection, token }) } : { headers: cpHeaders() };
  const response = await fetch(path + (selection ? '' : '?' + new URLSearchParams(context)), options);
  const raw = await response.text();
  let body;
  try { body = JSON.parse(raw); } catch { body = { message: raw || 'No response from the server' }; }
  if (!response.ok) throw new TaskHTTPError('Thinking effort', response.status, body.message || 'Unable to save setting', body.code, body.field);
  return body;
}

export async function previewEffort(request) {
  const response = await fetch('/api/chat/effort-preview', { method: 'POST',
    headers: { ...cpHeaders(), 'Content-Type': 'application/json' }, body: JSON.stringify(request) });
  const raw = await response.text();
  let body;
  try { body = JSON.parse(raw); } catch { body = { message: raw || 'No response from the server' }; }
  if (!response.ok) throw new TaskHTTPError('Thinking effort', response.status, body.message || 'Check extra args in Settings', body.code, body.field);
  return body;
}
