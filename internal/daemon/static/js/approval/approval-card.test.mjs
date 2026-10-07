import assert from 'node:assert/strict';
import test from 'node:test';

import { approvalFacts, approvalSubtitle, approvalTitle, countdownText } from './approval-card.js';

test('approval labels remain provider-neutral and origin-specific', () => {
  assert.equal(approvalTitle({ origin: 'runtime_tool', tool_name: 'Read file' }), 'Read file');
  assert.equal(approvalTitle({ origin: 'runtime_tool' }), 'Runtime tool');
  assert.equal(approvalTitle({ origin: 'governance', rule: 'Protect secrets' }), 'Protect secrets');
  assert.equal(approvalTitle({}), 'Policy approval');
  assert.equal(approvalSubtitle({ origin: 'runtime_tool' }), 'Runtime tool request');
  assert.equal(approvalSubtitle({ origin: 'workspace_mutation' }), 'Governance policy hold');
});

test('countdown is deterministic and fails closed for invalid or elapsed deadlines', () => {
  const now = Date.parse('2026-08-31T12:00:00Z');
  assert.equal(countdownText('2026-08-31T12:00:01.001Z', now), '2s remaining; request is denied on expiry');
  assert.equal(countdownText('2026-08-31T12:00:00Z', now), 'deadline passed—request denied');
  assert.equal(countdownText('invalid', now), 'deadline passed—request denied');
});

test('approval facts state request scope and duration without widening legacy requests', () => {
  assert.deepEqual(approvalFacts({
    origin: 'runtime_tool', tool_name: 'external_directory',
    action: 'Access paths outside the session working folder',
    targets: ['/worktree/**'], approval_reason: 'The runtime requires approval.',
    grant_scope: 'This request only',
    grant_duration: 'Until this request finishes; it is not remembered',
    allow_label: 'Allow once',
  }), {
    action: 'Access paths outside the session working folder', targets: ['/worktree/**'],
    reason: 'The runtime requires approval.', grantID: 'request', scope: 'This request only',
    duration: 'Until this request finishes; it is not remembered', allowLabel: 'Allow once',
  });
  assert.deepEqual(approvalFacts({ origin: 'runtime_tool', tool_name: 'Bash' }), {
    action: 'Bash', targets: [],
    reason: 'Approval is required before this action can continue.', grantID: 'request',
    scope: 'This request only',
    duration: 'Until this request finishes; it is not remembered', allowLabel: 'Allow once',
  });
});

test('approval facts use only a request-bound exact-run option', () => {
  const approval = {
    origin: 'runtime_tool', tool_name: 'external_directory',
    grant_options: [
      { id: 'request', label: 'Allow once', scope: 'This request only', duration: 'One request' },
      { id: 'run_exact', label: 'Allow exact matches for this run',
        scope: 'Same permission and exact targets', duration: 'This run' },
    ],
  };
  assert.deepEqual(approvalFacts(approval, 'run_exact'), {
    action: 'external_directory', targets: [],
    reason: 'Approval is required before this action can continue.',
    grantID: 'run_exact', scope: 'Same permission and exact targets',
    duration: 'This run', allowLabel: 'Allow exact matches for this run',
  });
  assert.equal(approvalFacts(approval, 'forged').grantID, 'request');
  assert.equal(approvalFacts({ tool_name: 'Bash', grant_options: [{ id: 7 }] }).grantID,
    'request');
});
