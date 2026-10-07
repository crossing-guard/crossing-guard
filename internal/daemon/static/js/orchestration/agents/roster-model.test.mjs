import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  agentState, outcomeWords, sessionScopeWords, placeName, referencePlace, placeDifferences,
  indexRows, filterRows, filterCounts, powersView, budgetsView, compareGroups, repositoryName,
  instructionsView, movePlan, messageWords, labelFor, proseBlocks, repositoryChoices, coverageWords,
  rerouteSummary, deliveryWords,
} from './roster-model.js';

const names = { 'rt-a': 'Runtime A', 'rt-b': 'Runtime B' };

const place = (overrides = {}) => ({
  place_id: 'agent-x', lane: 'managed', state: 'enabled', project_root: '/work/repo-one', repository: 'repo-one',
  runtime: 'rt-a', model: '', mode: 'read-only', granted_authority: ['send-message'], auto_action: true,
  priority: 0, declared_tags: [], limits: {}, routes: [], updated_at: 10, ...overrides,
});

test('agent state: attention first, then on, off, draft, none', () => {
  assert.equal(agentState({ attention: 'recent_failures', places: [place()] }), 'attn');
  assert.equal(agentState({ places: [place({ state: 'disabled' }), place()] }), 'on');
  assert.equal(agentState({ places: [place({ state: 'disabled' })] }), 'off');
  assert.equal(agentState({ places: [], draft_only: true }), 'draft');
  assert.equal(agentState({ places: [] }), 'none');
});

test('outcome words follow the owner vocabulary and delivery receipts', () => {
  assert.equal(outcomeWords({ outcome: 'acted', action: 'send_message', detail: { delivery: { state: 'accepted' } } }), 'sent a message');
  assert.equal(outcomeWords({ outcome: 'acted', action: 'reply', attention_class: 'draft', detail: { delivery: { state: 'unavailable' } } }), 'reply not sent');
  assert.equal(outcomeWords({ outcome: 'acted', action: 'reply', detail: { delivery: { state: 'started' } } }), 'replied');
  assert.equal(outcomeWords({ outcome: 'acted', action: 'ask_owner', attention_class: 'ask', detail: {} }), 'asked you');
  assert.equal(outcomeWords({ outcome: 'acted', action: 'send_message', detail: { delivery: { state: 'pending' } } }), 'message waiting for the session');
  assert.match(outcomeWords({ outcome: 'acted', action: 'send_message', detail: { delivery: { state: 'binding_changed' } } }), /not delivered \(binding changed\)/);
  assert.equal(messageWords({}), 'wrote a message');
  assert.equal(outcomeWords({ outcome: 'acted', action: 'no_action', detail: { tags: ['plan', 'question'] } }), 'tagged plan, question');
  assert.equal(outcomeWords({ outcome: 'acted', action: 'reply', detail: { auto_reply_suppressed: 'attended_session' } }), 'held a reply');
  assert.equal(outcomeWords({ outcome: 'quiet', action: 'no_action' }), 'stayed quiet');
  assert.equal(outcomeWords({ outcome: 'quiet', action: 'abstain' }), 'abstained');
  assert.equal(outcomeWords({ outcome: 'deferred' }), 'another helper acted');
  assert.equal(outcomeWords({ outcome: '', state: 'timed_out' }), 'timed out');
});

test('scope words name sessions in user terms', () => {
  assert.equal(sessionScopeWords(place({ watch_natural: true })), 'Every session, including terminal');
  assert.equal(sessionScopeWords(place()), 'Sessions started from the console');
  assert.equal(sessionScopeWords(place({ scope_session: '01a096fe-5aaa', scope_runtime: 'rt-b' }), names),
    'One session 01a096fe-5… · Runtime B only');
  assert.equal(sessionScopeWords({ lane: 'review', runtime_filter: '' }), 'Every repository');
});

test('place and model names never need a vendor literal', () => {
  assert.equal(placeName(place()), 'repo-one');
  assert.equal(placeName({ lane: 'review' }), 'Every repository');
  assert.equal(repositoryName('/a/b/c/'), 'c');
});

test('reference place is the newest enabled managed place', () => {
  const older = place({ place_id: 'a', updated_at: 5 });
  const newer = place({ place_id: 'b', updated_at: 9 });
  const off = place({ place_id: 'c', updated_at: 99, state: 'disabled' });
  assert.equal(referencePlace([older, newer, off]).place_id, 'b');
  assert.equal(referencePlace([off]).place_id, 'c');
  assert.equal(referencePlace([{ lane: 'review' }]), null);
});

test('place differences name only the settings that differ', () => {
  const reference = place();
  assert.deepEqual(placeDifferences(reference, place({ place_id: 'y' })), []);
  assert.deepEqual(placeDifferences(reference, place({ place_id: 'y', route_id: 'rte_other', limits: { max_total: 2 } })), ['model', 'budgets']);
  assert.deepEqual(placeDifferences(reference, place({ place_id: 'y', fallback_routes: [{ route_id: 'rte_b', mode: 'plan' }] })), ['fallback']);
});

test('index rows sort attention first and summarise places and stats', () => {
  const rows = indexRows({ agents: [
    { profile_id: 'quiet-one', name: 'Quiet', agent_type: 'follower', places: [] },
    { profile_id: 'busy', name: 'Busy', agent_type: 'helper', places: [place(), place({ place_id: 'z', repository: 'repo-two', state: 'disabled' })],
      stats: { totals: { runs: 7, acted: 3 }, days: [{ runs: 1 }, { runs: 6 }], last_at: 100, last_outcome: 'acted', last_action: 'draft_reply' } },
    { profile_id: 'broken', name: 'Broken', agent_type: 'reviewer', attention: 'provider_outage', places: [{ lane: 'review', state: 'enabled' }] },
  ] }, names);
  assert.deepEqual(rows.map(row => row.id), ['broken', 'busy', 'quiet-one']);
  const busy = rows[1];
  assert.equal(busy.where, 'repo-one');
  assert.equal(busy.whereMore, '+ 1 off');
  assert.equal(busy.lastWords, 'drafted a reply');
  assert.deepEqual(busy.spark, [1, 6]);
  assert.equal(busy.runs, 7);
  assert.equal(rows[0].attention, 'Its model is not answering');
  assert.equal(rows[0].where, 'Every repository');
  assert.equal(indexRows({ agents: [{ profile_id: 'x', stats_unavailable: true, places: [] }] })[0].statsUnavailable, true);
  assert.equal(rows[0].whereMore, '');
});

test('filters and counts include drafts of published agents', () => {
  const rows = indexRows({ agents: [
    { profile_id: 'a', name: 'A', places: [place()], has_draft: true },
    { profile_id: 'b', name: 'B', places: [], draft_only: true, has_draft: true },
    { profile_id: 'c', name: 'C', places: [] },
  ] });
  assert.deepEqual(filterCounts(rows), { all: 3, on: 1, attn: 0, off: 0, none: 1, draft: 2 });
  assert.deepEqual(filterRows(rows, 'draft').map(row => row.id), ['a', 'b']);
  assert.deepEqual(filterRows(rows, 'all', 'repo-one').map(row => row.id), ['a']);
});

test('powers view keeps auto-action binding-wide and only for acting powers', () => {
  const view = powersView(['send-message', 'draft-reply'], place({ granted_authority: ['draft-reply'], auto_action: true }));
  assert.equal(view.actingGranted, false);
  assert.equal(view.automatic, false);
  assert.deepEqual(view.rows.map(row => [row.name, row.granted]), [['send-message', false], ['draft-reply', true]]);
  assert.equal(powersView(['send-message'], place()).automatic, true);
});

test('budgets view shows exactly the budgets the daemon enforces', () => {
  const view = budgetsView({ max_total: 3, max_active: 9 }, { max_total: 48, loop_budget: 5 }, ['max_total', 'loop_budget']);
  assert.deepEqual(view.map(item => [item.key, item.value, item.isDefault]), [['max_total', 3, false], ['loop_budget', 5, true]]);
  assert.deepEqual(budgetsView({}, {}, []), []);
});

test('instructions view shows the stage prompts when they are what runs', () => {
  const staged = instructionsView({ instructions: 'body', stages: { 'b.kind': 'second', 'a.kind': 'first' } });
  assert.equal(staged.staged, true);
  assert.deepEqual(staged.prompts.map(item => item.signal), ['a.kind', 'b.kind']);
  assert.equal(instructionsView({ instructions: 'body' }).staged, false);
  assert.equal(labelFor([{ kind: 'k', label: 'Plain' }], 'k'), 'Plain');
  assert.equal(labelFor([], 'k'), 'k');
});

test('compare groups name what changed and what did not', () => {
  const a = { trigger: { event: 'signal.one' }, context: [], output: { kind: 'intervention' }, authority_requests: ['send-message'], instructions: 'one', limits: { timeout: '2m' } };
  const b = { ...a, trigger: { event: 'signal.two' } };
  const result = compareGroups(a, b);
  assert.deepEqual(result.changed.map(group => group.name), ['When it runs']);
  assert.ok(result.same.includes('Instructions'));
  assert.equal(result.instructionsChanged, false);
  assert.equal(compareGroups(a, { ...a, instructions: 'two' }).instructionsChanged, true);
});

test('move plan names grants and tags a version keeps, drops and newly offers', () => {
  const plan = movePlan(place({ granted_authority: ['reply', 'send-message'], declared_tags: ['plan', 'old'] }),
    { authority_requests: ['send-message', 'draft-reply'], may_tag: ['plan'] });
  assert.deepEqual(plan.grantsKept, ['send-message']);
  assert.deepEqual(plan.grantsDropped, ['reply']);
  assert.deepEqual(plan.grantsNew, ['draft-reply']);
  assert.deepEqual(plan.tagsKept, ['plan']);
  assert.deepEqual(plan.tagsDropped, ['old']);
});

test('prose blocks unwrap paragraphs and keep lists and fences', () => {
  const blocks = proseBlocks('Follow the discussion and\nrecall earlier decisions.\n\n- one\n- two\n\n```\ncode\n```\n');
  assert.deepEqual(blocks, [
    { keep: false, text: 'Follow the discussion and recall earlier decisions.' },
    { keep: true, text: '- one\n- two' },
    { keep: true, text: '```\ncode\n```' },
  ]);
  assert.deepEqual(proseBlocks(''), []);
});

test('repository choices put busy repositories first and tell same-named folders apart', () => {
  const choices = repositoryChoices([
    { root: '/tmp/a/proj', sessions: 1 }, { root: '/work/big', sessions: 40 }, { root: '/tmp/b/proj', sessions: 2 },
  ], new Set(['/work/big']));
  assert.deepEqual(choices.map(item => item.label), [
    'big \u00b7 40 sessions (already here)', 'proj \u2014 tmp/b \u00b7 2 sessions', 'proj \u2014 tmp/a \u00b7 1 session',
  ]);
  assert.equal(choices.find(item => !item.taken).root, '/tmp/b/proj');
});

test('coverage words name context and its state plainly', () => {
  const options = [{ kind: 'session.messages', label: 'Recent messages' }, { kind: 'operator.group_notes', label: 'Your notes' }];
  assert.equal(coverageWords([{ kind: 'operator.group_notes', state: 'empty' }, { kind: 'session.messages', state: 'supplied' }], options),
    'Your notes (nothing there) \u00b7 Recent messages (read)');
});

test('the reroute summary says what a reroute does', () => {
  const summary = rerouteSummary({ decisions: [{ action: 'reroute' }, { action: 'reroute' }, { action: 'no_route' }] });
  assert.deepEqual(summary.counts, { reroute: 2, stale_draft: 0, no_route: 1 });
  assert.equal(summary.words, '2 move to their fallback route; 0 stale helper replies are held for you as drafts; 1 has no fallback and stays parked.');
});

test('delivery words name where a message lands per runtime', () => {
  assert.equal(deliveryWords([
    { displayName: 'Runtime A', messageDelivery: { supported: true, boundary: 'next turn boundary' } },
    { displayName: 'Runtime B', messageDelivery: { supported: false, boundary: '' } },
  ]), 'Runtime A: next turn boundary \u00b7 Runtime B: not delivered');
});
