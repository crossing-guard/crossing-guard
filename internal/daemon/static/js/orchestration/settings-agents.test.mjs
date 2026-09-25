import test from 'node:test';
import assert from 'node:assert/strict';
import { agentRosterModel, describePowers, BUDGET_FIELDS, agentPageModel, helperSessionsSummary, helperSessionsLine } from './settings-agents.js';

const payload = {
  defaults: { max_total: 8, max_active: 2, max_hops: 1, loop_budget: 5, max_group_tokens: 200000, max_agent_tokens: 50000 },
  agents: [{
    binding_id: 'agent-design', state: 'enabled', role: 'helper', priority: 10,
    declared_tags: ['design-aligned', 'regex-hole'],
    limits: { max_total: 4, max_active: null, max_hops: 1, loop_budget: null, max_group_tokens: null, max_agent_tokens: null },
    scope_runtime: 'codex', scope_session: '', project_root: '/repo',
    profile_id: 'design-follower', runtime: 'claude', mode: 'read-only',
    granted_authority: ['draft-reply', 'advise'], auto_action: false,
    state_token: 'tok-1', profile_name: 'Design follower',
    profile_description: 'Keeps work aligned with design docs.', prompt_excerpt: 'Review the coding agent…',
  }],
  signals: [{ kind: 'message.completed', source: 'task', terminal: false, description: 'a turn finished' }],
};

test('roster model maps every backend field the card renders', () => {
  const [model] = agentRosterModel(payload);
  assert.equal(model.bindingID, 'agent-design');
  assert.equal(model.name, 'Design follower');
  assert.equal(model.role, 'helper');
  assert.equal(model.state, 'enabled');
  assert.equal(model.priority, 10);
  assert.deepEqual(model.declaredTags, ['design-aligned', 'regex-hole']);
  assert.equal(model.deployment.projectRoot, '/repo');
  assert.equal(model.deployment.scopeRuntime, 'codex');
  assert.equal(model.deployment.runtime, 'claude');
  assert.equal(model.deployment.mode, 'read-only');
  assert.equal(model.profileID, 'design-follower');
  assert.equal(model.stateToken, 'tok-1');
});

test('budgets resolve against shipped defaults without hiding which is which', () => {
  const [model] = agentRosterModel(payload);
  assert.equal(model.budgets.length, BUDGET_FIELDS.length);
  const total = model.budgets.find(item => item.key === 'max_total');
  assert.equal(total.value, 4);
  assert.equal(total.resolvedDefault, 8);
  assert.equal(total.effective, 4);
  const loop = model.budgets.find(item => item.key === 'loop_budget');
  assert.equal(loop.value, null);
  assert.equal(loop.resolvedDefault, 5);
  assert.equal(loop.effective, 5);
});

test('powers render as plain language from grants plus auto_action', () => {
  const manual = describePowers({ granted_authority: ['draft-reply', 'advise'], auto_action: false });
  assert.match(manual, /draft a reply for you to review and send/);
  assert.match(manual, /record advice/);
  assert.match(manual, /waits for your review/);
  const auto = describePowers({ granted_authority: ['launch-profile'], auto_action: true });
  assert.match(auto, /allowlisted read-only child profile/);
  assert.match(auto, /apply automatically/);
  assert.equal(describePowers({ granted_authority: [] }), 'Observe and tag only — no granted actions.');
  assert.match(describePowers({ granted_authority: ['new-grant-kind'] }), /new grant kind/, 'unknown grants humanize, never vanish');
});

test('empty payload yields an empty roster, no throw', () => {
  assert.deepEqual(agentRosterModel({}), []);
  assert.deepEqual(agentRosterModel(), []);
});

/* ---------- agentPageModel: the agent-first card join (g4 plan §2) ---------- */

const profilesPayload = {
  profiles: [
    { profile_id: 'design-follower', name: 'Design follower', description: 'desc', role: 'follower',
      version: '3', source_digest: 'sd-1', bundle_digest: 'bd-1', integrity: 'verified', selected_at: 1 },
    { profile_id: 'fresh-helper', name: 'Fresh helper', description: 'undeployed', role: 'helper',
      version: '1', source_digest: 'sd-2', bundle_digest: 'bd-2', integrity: 'verified', selected_at: 2 },
    { profile_id: 'tool-reviewer', name: 'Tool reviewer', description: 'review lane', role: 'reviewer',
      version: '2', source_digest: 'sd-3', bundle_digest: 'bd-3', integrity: 'verified', selected_at: 3 },
    { profile_id: 'broken-profile', name: 'Broken', description: '', role: 'follower',
      version: '1', source_digest: 'sd-4', bundle_digest: 'bd-4', integrity: 'verified', selected_at: 4 },
  ],
};
const managedPayload = {
  bindings: [],
  profiles: [
    { profile_id: 'design-follower', agent_type: 'follower', compatible: true, requested_authority: ['draft-reply'], source_digest: 'sd-1', bundle_digest: 'bd-1' },
    { profile_id: 'fresh-helper', agent_type: 'helper', compatible: true, requested_authority: ['reply'], source_digest: 'sd-2', bundle_digest: 'bd-2' },
    { profile_id: 'broken-profile', agent_type: 'follower', compatible: false, reason: 'context selector unknown', source_digest: 'sd-4', bundle_digest: 'bd-4' },
  ],
  absent_state_tokens: { 'agent-fresh-helper': 'absent-tok-fresh', 'agent-broken-profile': 'absent-tok-broken' },
  defaults: { max_total: 8 },
};
const reviewPayload = {
  binding: null, state_token: 'review-settings-tok',
  profiles: [{ profile_id: 'tool-reviewer', name: 'Tool reviewer', compatible: true, effects: ['report-only'], source_digest: 'sd-3', bundle_digest: 'bd-3' }],
  recent: [],
};
const agentsPayload = {
  agents: [{ binding_id: 'managed-follower', state: 'enabled', role: 'follower', priority: 0,
    profile_id: 'design-follower', profile_name: 'Design follower', prompt_excerpt: 'Review…',
    granted_authority: ['draft-reply'], state_token: 'live-tok-1', limits: {} }],
  defaults: { max_total: 8 },
};

test('deployed bindings, the review singleton, and undeployed profiles each get one card', () => {
  const cards = agentPageModel(profilesPayload, managedPayload, reviewPayload, agentsPayload);
  const byName = Object.fromEntries(cards.map(card => [card.name, card]));
  assert.equal(cards.length, 4);
  assert.equal(byName['Design follower'].deployed, true, 'legacy-id binding renders as its own deployed card');
  assert.equal(byName['Design follower'].bindingID, 'managed-follower', 'legacy binding ids keep working unchanged');
  assert.equal(byName['Design follower'].tokenSource, 'live');
  assert.equal(byName['Fresh helper'].deployed, false);
  assert.equal(byName['Fresh helper'].bindingID, 'agent-fresh-helper');
  assert.equal(byName['Fresh helper'].enableToken, 'absent-tok-fresh', 'creation uses the published ABSENT token');
  assert.equal(byName['Fresh helper'].tokenSource, 'absent');
  assert.equal(byName['Tool reviewer'].kind, 'reviewer');
  assert.equal(byName['Tool reviewer'].enableToken, 'review-settings-tok');
  assert.equal(byName['Broken'].compatible, false, 'incompatible profiles still get a card');
  assert.match(byName['Broken'].reason, /context selector unknown/);
});

test('RT-2: an existing binding under agent-<pid> blocks the undeployed card instead of silently rebinding', () => {
  const taken = {
    ...agentsPayload,
    agents: [...agentsPayload.agents, { binding_id: 'agent-fresh-helper', state: 'enabled', role: 'helper',
      profile_id: 'design-follower', profile_name: 'Design follower', state_token: 'live-tok-2', limits: {} }],
  };
  const cards = agentPageModel(profilesPayload, managedPayload, reviewPayload, taken);
  const fresh = cards.find(card => card.name === 'Fresh helper');
  assert.equal(fresh.compatible, false);
  assert.equal(fresh.enableToken, '', 'a collided card never carries a live token');
  assert.match(fresh.reason, /already taken/);
});

test('RT-14a: a binding whose pinned revision is gone renders needs-attention, never vanishes', () => {
  const orphan = {
    agents: [{ binding_id: 'managed-helper', state: 'enabled', role: 'helper',
      profile_id: 'gone-profile', profile_name: '', state_token: 'tok', limits: {} }],
    defaults: {},
  };
  const cards = agentPageModel({ profiles: [] }, { profiles: [], bindings: [] }, { profiles: [] }, orphan);
  assert.equal(cards.length, 1);
  assert.equal(cards[0].state, 'needs attention');
  assert.equal(cards[0].needsAttention, true);
  assert.match(cards[0].reason, /revision is unavailable/);
});

test('reviewer replacement is stated when another reviewer is bound', () => {
  const bound = {
    ...reviewPayload,
    binding: { profile_id: 'other-reviewer', state: 'enabled', endpoint: 'http://127.0.0.1:11434',
      timeout_ms: 10000, state_token: 'rb-tok' },
    profiles: [...reviewPayload.profiles,
      { profile_id: 'other-reviewer', name: 'Other reviewer', compatible: true, effects: ['report-only'] }],
  };
  const cards = agentPageModel(profilesPayload, managedPayload, bound, agentsPayload);
  const undeployed = cards.find(card => card.name === 'Tool reviewer');
  assert.equal(undeployed.replacesReviewer, true);
  assert.match(undeployed.reason, /replaces the currently enabled reviewer/);
});

test('missing absent token degrades to an honest no-Enable reason, not a guess', () => {
  const noTokens = { ...managedPayload, absent_state_tokens: {} };
  const cards = agentPageModel(profilesPayload, noTokens, reviewPayload, agentsPayload);
  const fresh = cards.find(card => card.name === 'Fresh helper');
  assert.equal(fresh.tokenSource, null);
  assert.match(fresh.reason, /did not publish a creation token/);
});

test('empty payloads yield an empty card list, no throw', () => {
  assert.deepEqual(agentPageModel(null, null, null, null), []);
});

test('helper sessions are counted per binding from groups that adopted a vendor session', () => {
  const groups = [
    { binding_id: 'b1', helper_native_session_id: 'hs-1', helper_turns: 4 },
    { binding_id: 'b1', helper_native_session_id: 'hs-2', helper_turns: 1, pending_event_id: 7 },
    { binding_id: 'b1', helper_native_session_id: '', helper_turns: 0 },
    { binding_id: 'b2', helper_native_session_id: 'hs-3', helper_turns: 9 },
  ];
  assert.deepEqual(helperSessionsSummary(groups, 'b1'), { sessions: 2, turns: 5, waiting: 1 });
  assert.equal(helperSessionsLine(helperSessionsSummary(groups, 'b1')), '2 helper sessions \u00b7 5 turns \u00b7 1 waiting');
  assert.equal(helperSessionsLine(helperSessionsSummary(groups, 'b2')), '1 helper session \u00b7 9 turns');
  assert.equal(helperSessionsLine(helperSessionsSummary(groups, 'b9')), '');
});
