import assert from 'node:assert/strict';
import test from 'node:test';
import {
  PANEL_PREFS_KEY, PANEL_PREFS_VERSION, activateTab, addPane, applyPreset, clampRatio, closePane, defaultSurface,
  emptyRoot, layoutDefaults, migratePanelPreferences, movePane, normalize, normalizeSurface, openPanes,
  readPanelPreferences, recentlyClosed, regionOf, regions, setRatio, split, withModuleEnabled,
  withRecentlyClosed, withSurfacePreference, writePanelPreferences,
} from './pane-layout.js';

const catalog = [
  { id: 'workspace.diff', catalogOrder: 1 },
  { id: 'evidence', catalogOrder: 5 },
  { id: 'session.plan', catalogOrder: 7 },
  { id: 'session.agents', catalogOrder: 9, available: false },
];
const defaults = layoutDefaults({
  split_clamp: [0.15, 0.85], recently_closed: 3, default_preset: 'review', min_region_px: { width: 240, height: 160 },
  presets: {
    review: { type: 'split', dir: 'col', ratio: 0.62, a: { type: 'region', tabs: ['workspace.diff', 'workspace.files'] }, b: { type: 'region', tabs: ['workspace.terminal', 'session.plan'] } },
    focus: { type: 'region', tabs: ['workspace.diff', 'evidence'] },
  },
});

test('presets normalize against the catalog: unknown panes drop and emptied regions collapse', () => {
  const root = applyPreset('review', catalog, defaults);
  assert.equal(root.type, 'split');
  assert.deepEqual(regions(root).map(r => r.tabs), [['workspace.diff'], ['session.plan']]);
  assert.equal(root.ratio, 0.62);
  const focus = applyPreset('focus', catalog, defaults);
  assert.deepEqual(openPanes(focus), ['workspace.diff', 'evidence']);
  const nothing = applyPreset('review', [{ id: 'evidence' }], defaults);
  assert.deepEqual(openPanes(nothing), ['evidence'], 'a catalog with none of the preset panes still opens its first pane');
});

test('add, activate, close, move, split, ratio are pure and keep one region alive', () => {
  let root = emptyRoot();
  root = addPane(root, 'r1', 'workspace.diff');
  root = addPane(root, 'r1', 'evidence');
  assert.deepEqual(regions(root)[0].tabs, ['workspace.diff', 'evidence']);
  assert.equal(regions(root)[0].active, 'evidence');
  root = activateTab(root, 'workspace.diff');
  assert.equal(regions(root)[0].active, 'workspace.diff');
  root = split(root, 'r1', 'col', defaults);
  assert.equal(root.type, 'split');
  assert.equal(root.dir, 'col');
  const fresh = regions(root)[1];
  root = movePane(root, 'evidence', fresh.id);
  assert.deepEqual(regions(root).map(r => r.tabs), [['workspace.diff'], ['evidence']]);
  root = setRatio(root, root.id, 0.99, defaults);
  assert.equal(root.ratio, 0.85, 'ratios clamp to the published bounds');
  root = closePane(root, 'evidence');
  assert.equal(root.type, 'region', 'an emptied region collapses into its sibling');
  root = closePane(root, 'workspace.diff');
  assert.deepEqual(root, { type: 'region', id: root.id, tabs: [], active: null }, 'the last region stays as the empty host');
  assert.equal(regionOf(root, 'workspace.diff'), null);
});

test('normalize repairs garbage, duplicates, and unknown ids without a blank panel', () => {
  for (const value of [null, 'x', {}, { type: 'split' }, { type: 'region', tabs: ['missing'] }]) {
    const root = normalize(value, catalog, defaults);
    assert.equal(root.type, 'region');
    assert.deepEqual(root.tabs, []);
  }
  const root = normalize({ type: 'split', dir: 'sideways', ratio: 5,
    a: { type: 'region', tabs: ['evidence', 'evidence', 42] }, b: { type: 'region', tabs: ['missing'] } }, catalog, defaults);
  assert.equal(root.type, 'region', 'the side with only unknown panes collapses');
  assert.deepEqual(root.tabs, ['evidence']);
  const kept = normalize({ type: 'split', dir: 'row', ratio: 0.5, a: { type: 'region', tabs: ['session.agents'] }, b: { type: 'region', tabs: ['evidence'] } }, catalog, defaults);
  assert.deepEqual(openPanes(kept), ['session.agents', 'evidence'], 'a known but unavailable pane is retained, not dropped');
});

test('v3 preferences migrate catalog-free with orientation preserved', () => {
  const migrated = migratePanelPreferences({
    version: 3,
    surfaces: { session: { slots: ['session.change', 'session.impact'], ratio: 0.7 }, memory: { slots: ['memory.provenance'], ratio: 0.5 }, skills: { slots: [], ratio: 0.5 } },
    module_disabled: ['session.agents'],
  }, defaults);
  assert.equal(migrated.version, PANEL_PREFS_VERSION);
  const session = migrated.surfaces.session.root;
  assert.equal(session.type, 'split');
  assert.equal(session.dir, 'col');
  assert.equal(session.ratio, 0.7);
  assert.deepEqual([session.a.tabs, session.b.tabs], [['session.change'], ['session.impact']], 'the v3 top slot stays on top');
  assert.deepEqual(migrated.surfaces.memory.root.tabs, ['memory.provenance'], 'other surfaces migrate without a catalog');
  assert.equal(migrated.surfaces.skills.explicit_empty, true, 'an explicit v3 close stays closed');
  assert.deepEqual(migrated.module_disabled, ['session.agents']);
  const legacy = migratePanelPreferences({ disabled: ['session.plan'], pick: { session: 'session.impact', closed: 'session.plan' } }, defaults);
  assert.deepEqual(openPanes(legacy.surfaces.session.root), ['session.impact']);
  assert.equal(legacy.surfaces.closed, undefined);
});

test('normalizeSurface applies the default preset to an empty or malformed surface unless closed on purpose', () => {
  assert.deepEqual(openPanes(normalizeSurface(undefined, catalog, defaults).root), ['workspace.diff', 'session.plan']);
  assert.deepEqual(openPanes(normalizeSurface({ root: { type: 'region', tabs: ['missing'] } }, catalog, defaults).root), ['workspace.diff', 'session.plan']);
  const closed = normalizeSurface({ root: emptyRoot(), explicit_empty: true, hidden: true }, catalog, defaults);
  assert.deepEqual(openPanes(closed.root), []);
  assert.equal(closed.hidden, true, 'the hidden state is durable');
  assert.equal(defaultSurface(catalog, defaults).preset, 'review');
});

test('storage read migrates in place, warns on failure, and writes preserve every dimension', () => {
  let written = '';
  const storage = {
    getItem: key => key === PANEL_PREFS_KEY ? JSON.stringify({ version: 3, surfaces: { session: { slots: ['evidence'], ratio: 0.5 } }, module_disabled: [] }) : null,
    setItem: (_key, value) => { written = value; },
  };
  const loaded = readPanelPreferences(storage, defaults);
  assert.equal(loaded.warning, false);
  assert.equal(JSON.parse(written).version, PANEL_PREFS_VERSION);
  const failed = readPanelPreferences({ getItem() { throw new Error('denied'); }, setItem() { throw new Error('denied'); } }, defaults);
  assert.equal(failed.warning, true);
  assert.deepEqual(failed.preferences.surfaces, {});
  assert.equal(writePanelPreferences({ setItem() { throw new Error('quota'); } }, loaded.preferences), false);
  let preferences = withSurfacePreference(loaded.preferences, 'session', { root: emptyRoot(), hidden: true });
  preferences = withModuleEnabled(preferences, 'session.agents', false);
  preferences = withRecentlyClosed(preferences, 'session', 'a', defaults);
  preferences = withRecentlyClosed(preferences, 'session', 'b', defaults);
  preferences = withRecentlyClosed(preferences, 'session', 'c', defaults);
  preferences = withRecentlyClosed(preferences, 'session', 'a', defaults);
  assert.equal(preferences.surfaces.session.hidden, true);
  assert.deepEqual(preferences.module_disabled, ['session.agents']);
  assert.deepEqual(recentlyClosed(preferences, 'session'), ['a', 'c', 'b'], 'most recent first, deduplicated, bounded by the published length');
  assert.equal(clampRatio('bad', defaults), 0.5);
});
