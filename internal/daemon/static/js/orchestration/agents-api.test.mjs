import test from 'node:test';
import assert from 'node:assert/strict';
import { agentWatchesSession, sessionIdentities } from './agents-api.js';

const agent = (overrides = {}) => ({
  binding_id: 'agent-a', state: 'enabled', scope_runtime: '', scope_session: '', project_root: '', ...overrides,
});
const session = { runtime: 'codex', sessionId: 'sess-1', cwd: '/repo/app' };

test('enabled agent with empty scopes watches everything', () => {
  assert.equal(agentWatchesSession(agent(), session), true);
});

test('disabled or missing agents never watch', () => {
  assert.equal(agentWatchesSession(agent({ state: 'disabled' }), session), false);
  assert.equal(agentWatchesSession(null, session), false);
});

test('runtime and session scopes are exact matches', () => {
  assert.equal(agentWatchesSession(agent({ scope_runtime: 'codex' }), session), true);
  assert.equal(agentWatchesSession(agent({ scope_runtime: 'claude' }), session), false);
  assert.equal(agentWatchesSession(agent({ scope_session: 'sess-1' }), session), true);
  assert.equal(agentWatchesSession(agent({ scope_session: 'sess-2' }), session), false);
});

test('session scope matches any exact identity alternate (stem, meta, thread)', () => {
  const scoped = agent({ scope_session: 'thread-9' });
  assert.equal(agentWatchesSession(scoped, { runtime: 'codex', sessionIds: ['stem-1', 'meta-4', 'thread-9'], cwd: '' }), true);
  assert.equal(agentWatchesSession(scoped, { runtime: 'codex', sessionIds: ['stem-1', 'meta-4'], cwd: '' }), false);
  assert.equal(agentWatchesSession(scoped, { runtime: 'codex', sessionId: 'thread-9', cwd: '' }), true, 'single-id callers keep working');
});

// Deliberate semantic change (g4 plan §5.9, red-team RT-5): the host triggers
// only on tasks whose working directory EQUALS the binding root, so prefix
// containment here overstated watching. One semantic, mirrored.
test('project root matches the host: exact cleaned equality, never containment', () => {
  assert.equal(agentWatchesSession(agent({ project_root: '/repo/app' }), session), true);
  assert.equal(agentWatchesSession(agent({ project_root: '/repo/app/' }), session), true, 'trailing slash is not a different root');
  assert.equal(agentWatchesSession(agent({ project_root: '/repo' }), session), false, 'a subdirectory session never triggers, so it is not "watched"');
  assert.equal(agentWatchesSession(agent({ project_root: '/other' }), session), false);
});

test('a project-rooted agent does not watch a session with no cwd', () => {
  assert.equal(agentWatchesSession(agent({ project_root: '/repo' }), { runtime: 'codex', sessionId: 's', cwd: '' }), false);
});

test('sessionIdentities dedupes and drops empty alternates', () => {
  assert.deepEqual(sessionIdentities({ id: 'a', meta_id: 'a', thread_id: 'b' }), ['a', 'b']);
  assert.deepEqual(sessionIdentities({ id: 'only' }), ['only'], 'claude sessions collapse to one identity');
  assert.deepEqual(sessionIdentities({}), []);
  assert.deepEqual(sessionIdentities({ alternates: [' x ', '', 'x', 'y'] }), ['x', 'y']);
});
