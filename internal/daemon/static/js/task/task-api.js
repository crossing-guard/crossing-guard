import { cpHeaders } from '../core.js';

const ROOT = '/api/v1/runtime-tasks';

export class TaskHTTPError extends Error {
  constructor(operation, status, detail, code = '') {
    super(operation + ' failed: ' + (detail || ('HTTP ' + status)));
    this.name = 'TaskHTTPError';
    this.status = status;
    this.detail = detail || '';
    this.code = code; // the daemon's machine-readable reason, when it gave one
  }
}

async function checkedFetch(operation, path, options = {}) {
  const response = await fetch(ROOT + path, options);
  if (response.ok) return response;
  const raw = (await response.text()).trim();
  let detail = raw, code = '';
  try {
    const parsed = JSON.parse(raw);
    detail = String(parsed?.message || raw);
    code = String(parsed?.code || '');
  } catch { /* plain text */ }
  throw new TaskHTTPError(operation, response.status, detail, code);
}

export async function fetchTaskSnapshot(signal) {
  const response = await checkedFetch('Task snapshot', '?limit=200', {
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
