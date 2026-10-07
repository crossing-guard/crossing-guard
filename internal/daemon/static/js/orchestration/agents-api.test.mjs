import test from 'node:test';
import assert from 'node:assert/strict';
import { agentWatchesSession, sessionIdentities, watchScopeIds, attachRequestBody } from './agents-api.js';

// The default agent is rooted at the session's own folder, so the scope
// tests below exercise scopes alone (an agent with no root watches nothing).
const agent = (overrides = {}) => ({
  binding_id: 'agent-a', state: 'enabled', scope_runtime: '', scope_session: '', project_root: '/repo/app', ...overrides,
});
const session = { runtime: 'codex', sessionId: 'sess-1', cwd: '/repo/app' };

test('enabled agent rooted at the session folder with empty scopes watches it', () => {
  assert.equal(agentWatchesSession(agent(), session), true);
});

// Deliberate semantic change (place-root-folder-identity plan §3.2, red-team
// PR-11): the host never triggers a binding without a root, so showing it as
// watching every session was a lie.
test('an agent with no project root watches nothing', () => {
  assert.equal(agentWatchesSession(agent({ project_root: '' }), session), false);
  assert.equal(agentWatchesSession(agent({ project_root: '', project_root_key: '/k' }), { ...session, cwdKey: '/k' }), false);
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
  assert.equal(agentWatchesSession(scoped, { runtime: 'codex', sessionIds: ['stem-1', 'meta-4', 'thread-9'], cwd: '/repo/app' }), true);
  assert.equal(agentWatchesSession(scoped, { runtime: 'codex', sessionIds: ['stem-1', 'meta-4'], cwd: '/repo/app' }), false);
  assert.equal(agentWatchesSession(scoped, { runtime: 'codex', sessionId: 'thread-9', cwd: '/repo/app' }), true, 'single-id callers keep working');
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
  assert.equal(agentWatchesSession(agent({ project_root: '/repo', project_root_key: '' }), { ...session, cwd: '', cwdKey: '' }), false,
    'two empty keys are not a folder match');
  assert.equal(agentWatchesSession(agent({ project_root: 'repo/app' }), { ...session, cwd: 'repo/app' }), false,
    'a relative directory never matches, as on the host');
});

// One rule with the host (place-root-folder-identity plan §3.2): the same
// spelling, or the same folder key the daemon published. /tmp is /private/tmp
// on macOS; the rail offers the first, the vendor records the second.
test('project root matches the same folder under another spelling through the daemon keys', () => {
  const tmpSession = { ...session, cwd: '/private/tmp/x', cwdKey: '/private/tmp/x' };
  assert.equal(agentWatchesSession(agent({ project_root: '/tmp/x', project_root_key: '/private/tmp/x' }), tmpSession), true);
  assert.equal(agentWatchesSession(agent({ project_root: '/tmp/x', project_root_key: '/private/tmp/y' }), tmpSession), false,
    'a different folder key is a different folder');
  assert.equal(agentWatchesSession(agent({ project_root: '/tmp/x' }), tmpSession), false,
    'without the agent key only the spelling can match');
  assert.equal(agentWatchesSession(agent({ project_root: '/tmp/x', project_root_key: '/private/tmp/x' }), { ...tmpSession, cwdKey: '' }), false,
    'without the session key only the spelling can match');
  assert.equal(agentWatchesSession(agent({ project_root: '/private/tmp/x', project_root_key: '/elsewhere' }), tmpSession), true,
    'an equal spelling matches whatever the keys say, as on the host');
});

test('sessionIdentities dedupes and drops empty alternates', () => {
  assert.deepEqual(sessionIdentities({ id: 'only' }), ['only'], 'a session with one identity has one');
  assert.deepEqual(sessionIdentities({}), []);
  assert.deepEqual(sessionIdentities({ alternates: [' x ', '', 'x', 'y'] }), ['x', 'y']);
});

// A subagent's thread id names its parent (child-thread-identity plan): runs
// and tags are read under the ids the daemon published, never derived here.
test('sessionIdentities takes the published identities and never derives one', () => {
  const child = { id: 'stem-child', meta_id: 'own-child', thread_id: 'thread-parent', identities: ['stem-child', 'own-child'] };
  assert.deepEqual(sessionIdentities(child), ['stem-child', 'own-child']);
  const primary = { id: 'stem-p', meta_id: 'thread-p', thread_id: 'thread-p', identities: ['stem-p', 'thread-p'] };
  assert.deepEqual(sessionIdentities(primary), ['stem-p', 'thread-p'], 'a primary keeps its thread');
  assert.deepEqual(sessionIdentities({ id: 'stem-child', meta_id: 'own-child', thread_id: 'thread-parent' }), ['stem-child'],
    'without published identities the selection is its own id alone');
  assert.deepEqual(sessionIdentities({ id: 'x', identities: [' x ', '', 'x', 'y'] }), ['x', 'y']);
});

// Watching mirrors the trigger (plan D-1): a subagent's activity is recorded
// under its parent's thread, so a binding scoped to that thread watches it.
test('watchScopeIds keeps every recorded form, the thread included', () => {
  const child = { id: 'stem-child', meta_id: 'own-child', thread_id: 'thread-parent', identities: ['stem-child', 'own-child'] };
  assert.deepEqual(watchScopeIds(child), ['stem-child', 'own-child', 'thread-parent']);
  assert.deepEqual(watchScopeIds({ id: 'a', meta_id: 'a', thread_id: 'b' }), ['a', 'b']);
  const scoped = agent({ scope_session: 'thread-parent' });
  // The session sits in the agent's folder: a cwd-less session never matches (place plan §3.2).
  assert.equal(agentWatchesSession(scoped, { runtime: 'rt', sessionIds: watchScopeIds(child), cwd: '/repo/app' }), true);
  assert.equal(agentWatchesSession(scoped, { runtime: 'rt', sessionIds: sessionIdentities(child), cwd: '/repo/app' }), false,
    'the published identities alone would hide the watcher that fires');
});

// The binding PUT decodes strictly: a body that repeats the path's binding id is
// refused ("unknown field binding_id"), which is how Attach in a session failed.
test('the attach request names the binding in the path only', () => {
  const body = attachRequestBody({ binding_id: 'agent-a', profile_id: 'a', route_id: 'rte_1', mode: 'plan', watch_natural: true }, 'token-1');
  assert.deepEqual(body, { profile_id: 'a', route_id: 'rte_1', mode: 'plan', watch_natural: true, expected_state_token: 'token-1', confirmed: true });
  assert.equal('binding_id' in body, false);
});
