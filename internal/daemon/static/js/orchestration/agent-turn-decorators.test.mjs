import test from 'node:test';
import assert from 'node:assert/strict';
import { buildAgentTurnIndex, reviewChipText, authorshipBadgeText } from './agent-turn-decorators.js';

const run = (overrides = {}) => ({
  run_id: 'run-1', binding_id: 'agent-a', group_id: 'group-1', state: 'completed',
  profile_name: 'Design follower', profile_id: 'design-follower', source_task_id: 'task-src',
  detail: { verdict: 'PLAN NOT READY' }, ...overrides,
});

test('anchored relationship facts key by turn anchor', () => {
  const index = buildAgentTurnIndex({
    runs: [run()],
    relationships: [{ relationship_id: 'r1', group_id: 'group-1', run_id: 'run-1',
      parent_task_id: 'task-src', reply_task_id: 'task-reply',
      source_turn_anchor: 'codex:turn:41', reply_turn_anchor: 'codex:turn:42', cycle: 2, role: 'follower', state: 'recorded' }],
  });
  const source = index.byAnchor.get('codex:turn:41');
  assert.equal(source.reviews.length, 1);
  assert.equal(source.reviews[0].agent, 'Design follower');
  assert.equal(source.reviews[0].verdict, 'PLAN NOT READY');
  const reply = index.byAnchor.get('codex:turn:42');
  assert.equal(reply.authorship.agent, 'Design follower');
  assert.equal(index.boundary.length, 0);
});

test('missing anchors fall back to the task boundary — never a row', () => {
  const index = buildAgentTurnIndex({
    runs: [run()],
    relationships: [{ relationship_id: 'r1', group_id: 'group-1', run_id: 'run-1',
      parent_task_id: 'task-src', reply_task_id: 'task-reply',
      source_turn_anchor: '', reply_turn_anchor: '', cycle: 1, role: 'follower', state: 'recorded' }],
  });
  assert.equal(index.byAnchor.size, 0, 'no anchor means no row-level fact');
  assert.equal(index.boundary.length, 2);
  const review = index.boundary.find(entry => entry.kind === 'review');
  assert.equal(review.taskId, 'task-src');
  const authorship = index.boundary.find(entry => entry.kind === 'authorship');
  assert.equal(authorship.taskId, 'task-reply');
});

test('a completed run without any relationship becomes a boundary review fact', () => {
  const index = buildAgentTurnIndex({ runs: [run()], relationships: [] });
  assert.equal(index.byAnchor.size, 0);
  assert.equal(index.boundary.length, 1);
  assert.equal(index.boundary[0].kind, 'review');
  assert.equal(index.boundary[0].taskId, 'task-src');
});

test('an in-flight run without a relationship stays silent', () => {
  const index = buildAgentTurnIndex({ runs: [run({ state: 'running', detail: null })], relationships: [] });
  assert.equal(index.boundary.length, 0);
});

test('relationship without a reply task records no authorship fact', () => {
  const index = buildAgentTurnIndex({
    runs: [run()],
    relationships: [{ relationship_id: 'r1', group_id: 'group-1', run_id: 'run-1',
      parent_task_id: 'task-src', reply_task_id: '', source_turn_anchor: 'a1', reply_turn_anchor: '', cycle: 0 }],
  });
  assert.equal(index.byAnchor.get('a1').authorship, null);
  assert.equal(index.boundary.length, 0);
});

test('verdict falls back through detail → action, and agent label through profile fields', () => {
  const index = buildAgentTurnIndex({
    runs: [run({ profile_name: '', detail: null, action: 'draft_reply' })],
    relationships: [{ run_id: 'run-1', group_id: 'group-1', source_turn_anchor: 'a2' }],
  });
  const fact = index.byAnchor.get('a2').reviews[0];
  assert.equal(fact.agent, 'design-follower');
  assert.equal(fact.verdict, 'draft reply');
});

test('review chip wording', () => {
  assert.equal(reviewChipText({ agent: 'Design follower', verdict: 'ready' }), '⚖ reviewed by Design follower → ready');
  assert.equal(reviewChipText({ agent: 'Design follower' }), '⚖ reviewed by Design follower');
});

test('authorship badge wording follows plan §13 Q4', () => {
  assert.equal(authorshipBadgeText({ agent: 'helper', operatorEdited: false }), 'sent by ⚖ helper');
  assert.equal(authorshipBadgeText({ agent: 'helper', operatorEdited: true }), 'you · via ⚖ helper');
  // Unknown edit state defaults to the operator-edited wording — the client
  // join cannot prove an unedited auto-send.
  assert.equal(authorshipBadgeText({ agent: 'helper' }), 'you · via ⚖ helper');
});
