import test from 'node:test';
import assert from 'node:assert/strict';
import { findingRefModel, unresolvedRefCount, latestRunByBinding, groupCounters, deliveryStatusText, helperSessionForBinding, helperSessionLabel } from './session-agents-panel.js';

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
