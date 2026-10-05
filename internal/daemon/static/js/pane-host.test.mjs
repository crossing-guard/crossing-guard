import assert from 'node:assert/strict';
import test from 'node:test';

// The strip's pure state derivation and the host's context identity are pinned
// without a DOM; everything else in the host needs a document.
globalThis.document = { createElement: () => ({ classList: { add() {}, remove() {}, toggle() {} }, setAttribute() {}, appendChild() {}, append() {}, addEventListener() {}, querySelector: () => null, querySelectorAll: () => [], style: { setProperty() {} }, replaceChildren() {}, remove() {} }), addEventListener() {}, removeEventListener() {}, dispatchEvent() {}, body: { classList: { add() {}, remove() {}, toggle() {}, contains: () => false }, appendChild() {} }, querySelector: () => null, querySelectorAll: () => [] };
globalThis.window = { innerWidth: 1200, innerHeight: 800, addEventListener() {} };
globalThis.localStorage = { getItem: () => null, setItem() {} };

const { paneContextKey } = await import('./pane-host.js');
const { paneStripState } = await import('./pane-strip.js');

test('strip state reads closed, open, and focused from the layout and dims while hidden', () => {
  const root = { type: 'split', id: 's1', dir: 'col', ratio: 0.5,
    a: { type: 'region', id: 'r1', tabs: ['workspace.diff', 'evidence'], active: 'workspace.diff' },
    b: { type: 'region', id: 'r2', tabs: ['session.plan'], active: 'session.plan' } };
  const catalog = [
    { id: 'workspace.diff', title: 'Diff', catalogVisible: true, available: true },
    { id: 'evidence', title: 'Evidence', catalogVisible: true, available: true },
    { id: 'session.plan', title: 'Plan', catalogVisible: true, available: true },
    { id: 'workspace.files', title: 'Files', catalogVisible: true, available: true },
    { id: 'memory.only', title: 'Memory', catalogVisible: false, available: true },
    { id: 'session.agents', title: 'Agents', catalogVisible: true, available: false, alwaysOffered: true },
  ];
  const state = paneStripState({ root, catalog, focusedRegion: 'r1', hidden: false });
  assert.deepEqual(state.map(item => [item.id, item.status]), [
    ['workspace.diff', 'focused'], ['evidence', 'open'], ['session.plan', 'open'], ['workspace.files', 'closed'], ['session.agents', 'closed'],
  ]);
  assert.equal(state.find(item => item.id === 'session.agents').available, false, 'an always-offered but unavailable pane shows disabled, not missing');
  assert.equal(state.some(item => item.id === 'memory.only'), false, 'panes of another surface never appear');
  const hidden = paneStripState({ root, catalog, focusedRegion: 'r1', hidden: true });
  assert.ok(hidden.every(item => item.dimmed), 'a hidden workspace dims every button but keeps the way back');
  assert.deepEqual(paneStripState(null), []);
});

test('pane context identity keeps tab-specific surfaces and distinct catalog sessions apart', () => {
  const selection = { id: 'governance-selection' };
  assert.notEqual(paneContextKey({ surface: 'governance', tab: 'models', selection }), paneContextKey({ surface: 'governance', tab: 'captures', selection }));
  const common = { runtime: 'codex', thread_id: 'native-thread' };
  assert.notEqual(
    paneContextKey({ surface: 'session', selection: { ...common, id: 'catalog-a', meta_id: 'rollout-a' } }),
    paneContextKey({ surface: 'session', selection: { ...common, id: 'catalog-b', meta_id: 'rollout-b' } }),
  );
});

test('reveal refuses, rather than doing nothing, when there is no workspace', async () => {
  const { revealModule, revealPane, canRevealPane, isModuleEnabled, takeModuleIntent, hidePaneHost } = await import('./pane-host.js');
  assert.equal(isModuleEnabled('session.agents'), false, 'no host: a link that reveals the module must not render');
  assert.equal(canRevealPane('session.change'), false);
  assert.equal(revealPane('session.change'), false, 'revealPane never un-hides a workspace it cannot fill');
  await assert.rejects(revealModule('session.agents', { intent: 'expand-first' }), /no workspace/);
  assert.equal(takeModuleIntent('session.agents'), null, 'a refused reveal leaves no intent behind');
  hidePaneHost();
  assert.equal(takeModuleIntent('session.agents'), null, 'a host change clears pending intents');
});
