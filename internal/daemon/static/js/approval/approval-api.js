import { cpHeaders } from '../core.js';

const ROOT = '/api/v1/approvals';

export class ApprovalHTTPError extends Error {
  constructor(operation, status, detail) {
    super(operation + ' failed: ' + (detail || ('HTTP ' + status)));
    this.name = 'ApprovalHTTPError'; this.status = status;
  }
}

async function checkedFetch(operation, path, options = {}) {
  const response = await fetch(ROOT + path, options);
  if (response.ok) return response;
  throw new ApprovalHTTPError(operation, response.status, (await response.text()).trim());
}

export async function fetchApprovalSnapshot(signal) {
  const response = await checkedFetch('Approval snapshot', '', { headers: cpHeaders(), signal });
  return response.json();
}

export function openApprovalStream(signal) {
  return checkedFetch('Approval stream', '/stream', { headers: cpHeaders(), signal });
}

export async function decideApproval(id, decision, reason, selections) {
  // Selections ride along only when the held call was asking a question, so an
  // ordinary allow or deny is byte-identical to what it has always been.
  const body = selections?.length
    ? JSON.stringify({ id, decision, reason, selections })
    : JSON.stringify({ id, decision, reason });
  const response = await checkedFetch('Approval decision', '/decision', {
    method: 'POST', headers: { ...cpHeaders(), 'Content-Type': 'application/json' }, body,
  });
  return response.json();
}

export async function reportApprovalPresence(payload, signal) {
  await checkedFetch('Approval presence', '/presence', {
    method: 'POST', headers: { ...cpHeaders(), 'Content-Type': 'application/json' },
    body: JSON.stringify(payload), signal,
  });
}
