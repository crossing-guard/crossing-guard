import test from 'node:test';
import assert from 'node:assert/strict';
import { findingRefModel, unresolvedRefCount, latestRunByBinding, groupCounters, deliveryStatusText, helperSessionForBinding, helperSessionLabel, relatedRowView, relatedRowsTree, relatedSectionModel, newerUnfinishedByBinding, runOutcomeText } from './session-agents-panel.js';

test('resolved file refs become real anchors bound to the resolved path', () => {
  const model = findingRefModel({ kind: 'file', path: 'internal/daemon/main.go', line: 42, resolution: 'resolved', resolved_path: 'internal/daemon/main.go' });
  assert.equal(model.state, 'resolved');
  assert.equal(model.path, 'internal/daemon/main.go');
  assert.equal(model.line, 42);
  assert.equal(model.label, 'internal/daemon/main.go:42');
});

test('stated non-resolved resolutions flag as unverified with the reason', () => {
  for (const resolution of ['missing', 'stale', 'unresolved']) {
    const model = findingRefModel({ kind: 'file', path: 'gone.go', resolution });
    assert.equal(model.state, 'unverified');
    assert.equal(model.reason, resolution);
  }
});

test('unstated resolution consults the injected project-root resolver', () => {
  const resolved = findingRefModel({ kind: 'file', path: 'a/b.go', line: 3 },
    token => ({ state: 'resolved', path: 'src/a/b.go', line: 3, token }));
  assert.equal(resolved.state, 'resolved');
  assert.equal(resolved.path, 'src/a/b.go');
  const missing = findingRefModel({ kind: 'file', path: 'a/b.go' },
    () => ({ state: 'missing', reason: 'no file at this path in the current tree' }));
  assert.equal(missing.state, 'unverified');
  assert.match(missing.reason, /no file/);
  const noResolver = findingRefModel({ kind: 'file', path: 'a/b.go' }, null);
  assert.equal(noResolver.state, 'unverified');
  assert.equal(noResolver.reason, 'resolution not reported');
});

test('session and event refs never become file anchors', () => {
  const sess = findingRefModel({ kind: 'session', session: 'sess-9', resolution: 'resolved' });
  assert.equal(sess.state, 'resolved-session');
  assert.equal(sess.label, 'session sess-9');
  const unresolvedSess = findingRefModel({ kind: 'session', session: 'sess-9' });
  assert.equal(unresolvedSess.state, 'unverified');
  const event = findingRefModel({ kind: 'event', anchor: 'codex:turn:7', session: 'sess-9' });
  assert.equal(event.state, 'unverified');
  assert.match(event.label, /codex:turn:7/);
});

test('unresolvedRefCount counts every unverified ref across findings', () => {
  const findings = [
    { severity: 'high', statement: 'a', refs: [
      { kind: 'file', path: 'x.go', resolution: 'resolved', resolved_path: 'x.go' },
      { kind: 'file', path: 'y.go', resolution: 'missing' },
    ] },
    { severity: 'low', statement: 'b', refs: [{ kind: 'session', session: 's1' }] },
    { severity: 'low', statement: 'c' },
  ];
  assert.equal(unresolvedRefCount(findings), 2);
});

test('latestRunByBinding prefers newest completed claims in newest-first API order', () => {
  const latest = latestRunByBinding([
    { binding_id: 'a', run_id: '1', state: 'completed' },
    { binding_id: 'a', run_id: '2', state: 'deferred' },
    { binding_id: 'a', run_id: '0', state: 'completed' },
    { binding_id: 'b', run_id: '3', state: 'running' },
    { binding_id: 'b', run_id: '4', state: 'completed' },
  ]);
  assert.equal(latest.get('a').run_id, '1');
  assert.equal(latest.get('b').run_id, '4');
});

test('groupCounters reads cycles and replies from relationships for the given groups', () => {
  const relationships = [
    { group_id: 'g1', cycle: 1, reply_task_id: 't1' },
    { group_id: 'g1', cycle: 3, reply_task_id: '' },
    { group_id: 'g2', cycle: 9, reply_task_id: 't2' },
  ];
  const counters = groupCounters(relationships, new Set(['g1']));
  assert.equal(counters.cycles, 3);
  assert.equal(counters.replies, 1);
  assert.deepEqual(groupCounters(relationships, new Set()), { cycles: 0, replies: 0 });
});


test('message claims distinguish proposals, queue receipts, and uncertain effects', () => {
  assert.equal(deliveryStatusText({ action: 'reply' }), '');
  assert.match(deliveryStatusText({ action: 'send_message' }), /proposed/);
  for (const state of ['accepted', 'pending', 'unknown', 'unavailable', 'delivered', 'expired']) {
    const text = deliveryStatusText({ action: 'send_message', detail: { delivery: { state } } });
    assert.ok(text.length > 0);
    assert.doesNotMatch(text, /Message delivered/);
    if (state === 'accepted') assert.match(text, /consumption is not confirmed/);
    if (state === 'delivered') assert.match(text, /boundary .* not separately confirmed/);
    if (state === 'expired') assert.match(text, /not delivered/);
    if (state === 'pending' || state === 'unknown') assert.match(text, /retry automatically/);
  }
  const queued = deliveryStatusText({ action: 'send_message', detail: { delivery: { state: 'accepted', boundary: 'next hook boundary' } } });
  assert.match(queued, /Boundary: next hook boundary/);
  const posted = deliveryStatusText({ action: 'send_message', detail: { delivery: { state: 'accepted', tier: 'socket-post', carrier: 'socket' } } });
  assert.match(posted, /peer message .*hold or drop it/);
  assert.doesNotMatch(posted, /Message queued/);
  const stillQueued = deliveryStatusText({ action: 'send_message', detail: { delivery: { state: 'accepted', tier: 'queued-delivery' } } });
  assert.match(stillQueued, /Message queued/);
});

test('the helper session shown is the binding\'s newest group for THIS session, and only once a turn reported one', () => {
  const groups = [
    { group_id: 'g-old', binding_id: 'b1', root_native_session_id: 'ses-1', helper_native_session_id: 'hs-old', helper_runtime: 'claude', helper_turns: 3, updated_at: 10 },
    { group_id: 'g-new', binding_id: 'b1', root_catalog_session_id: 'ses-1', helper_native_session_id: 'hs-new', helper_runtime: 'claude', helper_turns: 1, pending_event_id: 9, updated_at: 20 },
    { group_id: 'g-other', binding_id: 'b1', root_native_session_id: 'ses-2', helper_native_session_id: 'hs-2', helper_turns: 5, updated_at: 30 },
    { group_id: 'g-none', binding_id: 'b1', root_native_session_id: 'ses-1', helper_native_session_id: '', updated_at: 40 },
    { group_id: 'g-b2', binding_id: 'b2', root_native_session_id: 'ses-1', helper_native_session_id: 'hs-b2', updated_at: 50 },
  ];
  const picked = helperSessionForBinding(groups, 'b1', ['ses-1']);
  assert.equal(picked.group_id, 'g-new');
  assert.equal(helperSessionLabel(picked), 'helper session \u00b7 1 turn \u00b7 1 waiting');
  assert.equal(helperSessionLabel(groups[0]), 'helper session \u00b7 3 turns');
  assert.equal(helperSessionLabel({ helper_turns: 2, pending_dropped: 1 }), 'helper session \u00b7 2 turns \u00b7 1 dropped');
  assert.equal(helperSessionForBinding(groups, 'b3', ['ses-1']), null);
  assert.equal(helperSessionForBinding([groups[3]], 'b1', ['ses-1']), null);
  assert.equal(helperSessionForBinding(groups, 'b1', []).group_id, 'g-other');
});

test('a reply the daemon classed as a draft says it was not sent, with the receipt reason', () => {
  const run = { action: 'reply', attention_class: 'draft',
    detail: { delivery: { state: 'unavailable', reason_class: 'attended_session', detail: 'The session is not daemon-owned.' } } };
  assert.equal(deliveryStatusText(run), 'Reply not sent · The session is not daemon-owned.');
  assert.equal(deliveryStatusText({ action: 'reply', detail: { delivery: { state: 'started' } } }), '');
  assert.equal(deliveryStatusText({ action: 'send_message', detail: { delivery: { state: 'not_requested' } } }),
    'Message proposed · automatic delivery was not requested.');
});

test('a related row links only when the daemon says its id can open; an agent id is text', () => {
  const agent = relatedRowView({ kind: 'spawned', provenance: 'observed', direction: 'child', runtime: 'rt-a',
    session_id: 'a0aa1b7579b8e57c4', role: 'Explore', description: 'map the flow', openable: false });
  assert.equal(agent.link, null);
  assert.equal(agent.idLabel, 'rt-a \u00b7 a0aa1b7579b8e5\u2026');
  assert.equal(agent.description, 'map the flow');
  assert.equal(agent.text, 'spawned \u00b7 child \u00b7 Explore');
  // A row that omits `openable` is not a link: an absent fact never yields a dead link.
  assert.equal(relatedRowView({ kind: 'spawned', runtime: 'rt-a', session_id: 'x' }).link, null);
  const session = relatedRowView({ kind: 'spawned', provenance: 'observed', direction: 'child', runtime: 'rt-b',
    session_id: 'abcdef00-aaaa-7bbb-8ccc-00000000000c', openable: true });
  assert.deepEqual(session.link, { runtime: 'rt-b', id: 'abcdef00-aaaa-7bbb-8ccc-00000000000c', label: 'rt-b \u00b7 abcdef00-aaaa-\u2026' });
  assert.equal(relatedRowView({ kind: 'spawned', runtime: '', session_id: 'x', openable: true }).link, null);
});

test('a collapsed caused row states its runs, replies and newest state', () => {
  const view = relatedRowView({ kind: 'reviewed-by', provenance: 'caused', direction: 'child', role: 'helper',
    state: 'running', runs: 39, replies: 1, runtime: 'rt-b', session_id: 's-1', openable: true });
  assert.equal(view.text, 'reviewed-by \u00b7 child \u00b7 helper \u00b7 running \u00b7 39 runs \u00b7 1 reply');
  assert.equal(relatedRowView({ kind: 'resumed-by', provenance: 'caused', direction: 'self', role: 'helper', runs: 1 }).text,
    'resumed-by \u00b7 self \u00b7 helper');
  const unresolved = relatedRowView({ kind: 'read_context_of', direction: 'peer', label: 'list_threads', unresolved: true });
  assert.equal(unresolved.label, 'list_threads');
  assert.equal(unresolved.link, null);
});

test('a nested agent sits under the agent that launched it, and no row is ever dropped', () => {
  const rows = [
    { session_id: 'c1', direction: 'child' },
    { session_id: 'd1', direction: 'descendant', via: 'c2' },
    { session_id: 'c2', direction: 'child' },
    { session_id: 'd2', direction: 'descendant', via: 'd1' },
    { session_id: 'o1', direction: 'descendant', via: 'gone' },
    { session_id: 'x1', via: 'x2' },
    { session_id: 'x2', via: 'x1' },
  ];
  const tree = relatedRowsTree(rows).map(({ row, depth }) => row.session_id + ':' + depth);
  assert.deepEqual(tree, ['c1:0', 'c2:0', 'd1:1', 'd2:2', 'o1:0', 'x1:0', 'x2:1']);
  assert.equal(relatedRowsTree(rows).length, rows.length);
});

test('a failed or partial related read says so; only a complete empty read renders nothing', () => {
  assert.deepEqual(relatedSectionModel({ status: 'rejected', reason: new Error('HTTP 500') }),
    { unavailable: 'Related sessions unavailable: HTTP 500' });
  assert.equal(relatedSectionModel({ status: 'fulfilled', value: { related: [], coverage: '' } }), null);
  const partial = relatedSectionModel({ status: 'fulfilled', value: { related: [], coverage: 'caused relations unavailable: locked' } });
  assert.deepEqual(partial, { rows: [], coverage: 'caused relations unavailable: locked' });
  const nested = relatedSectionModel({ status: 'fulfilled', value: { related: [
    { kind: 'spawned', direction: 'descendant', runtime: 'rt-a', session_id: 'n1', via: 'c1', openable: false },
    { kind: 'spawned', direction: 'child', runtime: 'rt-a', session_id: 'c1', openable: false },
  ] } });
  assert.deepEqual(nested.rows.map(({ view, depth }) => view.sessionId + ':' + depth), ['c1:0', 'n1:1']);
  assert.ok(nested.rows.every(({ view }) => view.link === null));
});

// managed-turn-profile-limits plan §4.8 (R2-1): the panel shows a binding's
// last completed claim, so a newer refusal or timeout must surface beside it.
test('a newer unfinished run is found beside the completed claim the panel shows', () => {
  const runs = [
    { run_id: 'r4', binding_id: 'recall', state: 'suppressed', error_class: 'destination_locality', recovery: 'Not a local route.' },
    { run_id: 'r3', binding_id: 'recall', state: 'failed', error_class: 'timeout', recovery: 'Stopped.' },
    { run_id: 'r2', binding_id: 'recall', state: 'completed', action: 'send_message' },
    { run_id: 'r1', binding_id: 'recall', state: 'failed', error_class: 'context' },
    { run_id: 'q2', binding_id: 'quiet', state: 'completed' },
    { run_id: 'q1', binding_id: 'quiet', state: 'failed', error_class: 'timeout' },
    { run_id: 'n1', binding_id: 'never', state: 'suppressed', error_class: 'destination_locality' },
  ];
  const later = newerUnfinishedByBinding(runs, latestRunByBinding(runs));
  assert.equal(later.get('recall').run_id, 'r4');
  assert.equal(later.has('quiet'), false, 'an OLDER failure than the shown claim is not news');
  assert.equal(later.has('never'), false, 'when the shown run is itself unfinished the card states it directly');
});

test('an unfinished run reads as its class and recovery; a completed one says nothing', () => {
  assert.equal(runOutcomeText({ state: 'failed', error_class: 'timeout', recovery: 'Stopped after 2m6s.' }), 'timeout: Stopped after 2m6s.');
  assert.equal(runOutcomeText({ state: 'suppressed', error_class: 'destination_locality' }), 'destination locality');
  assert.equal(runOutcomeText({ state: 'completed', recovery: 'x' }), '');
});
