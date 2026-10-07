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

export async function decideApproval(id, decision, reason, selections, grantID = '') {
  // The request-bound choice stays byte-identical to the legacy decision body.
  // Only the explicitly broader choice adds grant_id.
  const selectedGrant = decision === 'allow' && grantID !== 'request' ? grantID : '';
  let body = selections?.length
    ? JSON.stringify({ id, decision, reason, selections })
    : JSON.stringify({ id, decision, reason });
  if (selectedGrant) {
    body = selections?.length
      ? JSON.stringify({ id, decision, reason, selections, grant_id: selectedGrant })
      : JSON.stringify({ id, decision, reason, grant_id: selectedGrant });
  }
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
