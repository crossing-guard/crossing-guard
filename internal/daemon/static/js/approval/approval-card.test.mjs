import assert from 'node:assert/strict';
import test from 'node:test';

import { approvalSubtitle, approvalTitle, countdownText } from './approval-card.js';

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
  assert.equal(countdownText('2026-08-31T12:00:01.001Z', now), '2s until fail-closed');
  assert.equal(countdownText('2026-08-31T12:00:00Z', now), 'deadline passed—request denied');
  assert.equal(countdownText('invalid', now), 'deadline passed—request denied');
});
