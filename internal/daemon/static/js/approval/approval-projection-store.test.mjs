import assert from 'node:assert/strict';
import { ApprovalProjectionStore, reduceApprovalProjection } from './approval-projection-store.js';

const initial = { pending: new Map(), history: new Map() };
const snapshot = reduceApprovalProjection(initial, { type: 'snapshot', pending: [
  { id: 'ap_2', status: 'pending', deadline: '2026-08-28T12:02:00Z' },
  { id: 'ap_1', status: 'pending', deadline: '2026-08-28T12:01:00Z' },
], history: [] });
assert.equal(snapshot.error, undefined);
assert.equal(snapshot.state.pending.size, 2);

const decided = reduceApprovalProjection(snapshot.state, { type: 'approval', kind: 'decided', approval: {
  id: 'ap_1', status: 'allowed', decided_at: '2026-08-28T12:00:30Z',
  responses: [{ id: 'apr_FIXTURE', responder: { kind: 'interactive', id: 'local-console' },
    decision: 'allow', reason: 'reviewed', submitted_at: '2026-08-28T12:00:29Z',
    accepted_at: '2026-08-28T12:00:30Z', disposition: 'operative' }],
} });
assert.equal(decided.state.pending.has('ap_1'), false);
assert.equal(decided.state.history.get('ap_1').status, 'allowed');
assert.deepEqual(decided.state.history.get('ap_1').responses[0].responder,
  { kind: 'interactive', id: 'local-console' });
assert.equal(decided.state.history.get('ap_1').responses[0].disposition, 'operative');

const store = new ApprovalProjectionStore(() => {});
let healthyListenerCalls = 0;
store.subscribe(() => { throw new Error('isolated listener'); });
store.subscribe(() => { healthyListenerCalls++; });
store.snapshot({ pending: [...snapshot.state.pending.values()], history: [] });
assert.equal(healthyListenerCalls, 2);
assert.deepEqual(store.pending().map(item => item.id), ['ap_1', 'ap_2']);
store.apply({ type: 'approval', kind: 'decided', approval: { id: 'ap_1', status: 'denied' } });
assert.deepEqual(store.pending().map(item => item.id), ['ap_2']);
assert.equal(store.history()[0].id, 'ap_1');

for (let index = 0; index < 101; index++) {
  store.apply({ type: 'approval', kind: 'decided', approval: {
    id: 'ap_history_' + index, status: 'denied', decided_at: new Date(Date.UTC(2026, 7, 28, 13, 0, index)).toISOString(),
  } });
}
assert.equal(store.history().length, 100);
assert.equal(store.history().some(item => item.id === 'ap_history_0'), false);

globalThis.document = { querySelector: () => null, querySelectorAll: () => [] };
globalThis.setInterval = () => 0;
const { acceptedApprovalResponse, approvalResponderLabel } = await import('../views/approvals.js');
const malicious = '<img src=x onerror=alert(1)>';
const maliciousResponse = { responder: { kind: 'service', id: malicious } };
assert.equal(approvalResponderLabel(maliciousResponse), 'Answered by service responder ' + malicious);
assert.equal(approvalResponderLabel(null), 'Responder not recorded (legacy)');
assert.equal(acceptedApprovalResponse({ responses: [{ id: 'first' }, maliciousResponse] }), maliciousResponse);

console.log('approval projection store: PASS');
