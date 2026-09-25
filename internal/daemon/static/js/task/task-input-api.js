import { cpHeaders } from '../core.js';

const ROOT = '/api/v1/task-inputs';

export class TaskInputHTTPError extends Error {
  constructor(operation, status, code, detail) {
    super(operation + ' failed: ' + (detail || ('HTTP ' + status)));
    this.name = 'TaskInputHTTPError';
    this.status = status;
    this.code = code || 'http_error';
  }
}

async function checkedFetch(operation, path, options = {}) {
  const response = await fetch(ROOT + path, options);
  if (response.ok) return response;
  const raw = (await response.text()).trim();
  let detail = raw, code = '';
  try {
    const body = JSON.parse(raw);
    detail = String(body?.message || raw); code = String(body?.code || '');
  } catch { /* plain text remains the fallback detail */ }
  throw new TaskInputHTTPError(operation, response.status, code, detail);
}

function scopeHeaders(scopeID) {
  const headers = cpHeaders();
  if (scopeID) headers['X-CG-Input-Scope'] = scopeID;
  return headers;
}

export async function stageTaskInput(file, source, scopeID, signal) {
  const body = new FormData();
  body.append('source', source);
  body.append('file', file, file.name);
  const response = await checkedFetch('Attach', '', {
    method: 'POST', headers: scopeHeaders(scopeID), body, signal,
  });
  return response.json();
}

export async function listTaskInputs(scopeID, signal) {
  const response = await checkedFetch('Restore attachments', '', {
    headers: scopeHeaders(scopeID), signal,
  });
  return response.json();
}

export async function fetchTaskInputContent(scopeID, inputID, signal) {
  const response = await checkedFetch('Load attachment preview', '/' + encodeURIComponent(inputID) + '/content', {
    headers: scopeHeaders(scopeID), signal,
  });
  return response.blob();
}

export async function removeTaskInput(scopeID, inputID, signal) {
  const response = await checkedFetch('Remove attachment', '/' + encodeURIComponent(inputID), {
    method: 'DELETE', headers: scopeHeaders(scopeID), signal,
  });
  return response.json();
}
