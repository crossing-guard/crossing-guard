// Pure pane-layout grammar (version 4) and browser-presentation preferences.
// No DOM, provider, API, runtime, or workspace knowledge belongs here.
//
// node := { type:'region', id, tabs:[paneID…], active:paneID|null }
//       | { type:'split', id, dir:'row'|'col', ratio, a:node, b:node }   // col = stacked, a on top
// surface := { root:node, hidden:boolean, preset?:name }
// preferences := { version:4, surfaces:{…}, module_disabled:[…], recently_closed:{ <surface>:[paneID…] } }
//
// Every number comes from `defaults` (the daemon's published console configuration); nothing is compiled in.

export const PANEL_PREFS_KEY = 'cg_panel_prefs';
export const PANEL_PREFS_VERSION = 4;

const isObject = value => !!value && typeof value === 'object' && !Array.isArray(value);
const ids = values => new Set((Array.isArray(values) ? values : []).map(value => typeof value === 'string' ? value : value?.id).filter(Boolean));

// ---- defaults --------------------------------------------------------------

// layoutDefaults shapes what the daemon published into what the grammar reads.
// Absent values become inert (no clamp beyond (0,1), one preset that is empty).
export function layoutDefaults(config = {}) {
  const clamp = Array.isArray(config.split_clamp) && config.split_clamp.length === 2 ? config.split_clamp : [0.05, 0.95];
  return {
    clamp: [Number(clamp[0]), Number(clamp[1])],
    presets: isObject(config.presets) ? config.presets : {},
    defaultPreset: typeof config.default_preset === 'string' ? config.default_preset : '',
    recentlyClosed: Number.isInteger(config.recently_closed) && config.recently_closed > 0 ? config.recently_closed : 8,
    minRegion: isObject(config.min_region_px) ? config.min_region_px : { width: 0, height: 0 },
    keymap: isObject(config.keymap) ? config.keymap : {},
  };
}

export function clampRatio(value, defaults) {
  const number = Number(value);
  const [min, max] = defaults.clamp;
  if (!Number.isFinite(number)) return 0.5;
  return Math.min(max, Math.max(min, number));
}

// ---- tree helpers -----------------------------------------------------------

function nextID(root, prefix) {
  let max = 0;
  walk(root, node => { const match = /^(?:r|s)(\d+)$/.exec(node.id || ''); if (match) max = Math.max(max, Number(match[1])); });
  return `${prefix}${max + 1}`;
}

export function walk(node, visit) {
  if (!node) return;
  visit(node);
  if (node.type === 'split') { walk(node.a, visit); walk(node.b, visit); }
}

export function regions(root) {
  const out = [];
  walk(root, node => { if (node.type === 'region') out.push(node); });
  return out;
}

export function findNode(root, id) {
  let found = null;
  walk(root, node => { if (node.id === id) found = node; });
  return found;
}

export function findParent(root, id, parent = null) {
  if (!root) return null;
  if (root.id === id) return parent;
  if (root.type !== 'split') return null;
  return findParent(root.a, id, root) || findParent(root.b, id, root);
}

export function firstRegion(root) {
  return root?.type === 'region' ? root : firstRegion(root?.a);
}

export function openPanes(root) {
  return regions(root).flatMap(region => region.tabs);
}

export function regionOf(root, paneID) {
  return regions(root).find(region => region.tabs.includes(paneID)) || null;
}

const clone = node => JSON.parse(JSON.stringify(node));

function replaceNode(root, id, replacement) {
  if (root.id === id) return replacement;
  if (root.type !== 'split') return root;
  return { ...root, a: replaceNode(root.a, id, replacement), b: replaceNode(root.b, id, replacement) };
}

export function emptyRoot() { return { type: 'region', id: 'r1', tabs: [], active: null }; }

// ---- operations (each returns a new tree) -----------------------------------

export function addPane(root, regionID, paneID) {
  const next = clone(root);
  const region = findNode(next, regionID) || firstRegion(next);
  if (!region || region.type !== 'region' || region.tabs.includes(paneID)) return next;
  region.tabs.push(paneID); region.active = paneID;
  return next;
}

export function activateTab(root, paneID) {
  const next = clone(root);
  const region = regionOf(next, paneID);
  if (region) region.active = paneID;
  return next;
}

// closePane removes the pane; an emptied region collapses into its sibling. The
// last region never collapses — it stays as the empty host.
export function closePane(root, paneID) {
  let next = clone(root);
  const region = regionOf(next, paneID);
  if (!region) return next;
  region.tabs = region.tabs.filter(id => id !== paneID);
  if (region.active === paneID) region.active = region.tabs[0] || null;
  if (region.tabs.length) return next;
  const parent = findParent(next, region.id);
  if (!parent) return next;
  const sibling = parent.a.id === region.id ? parent.b : parent.a;
  next = replaceNode(next, parent.id, sibling);
  return next;
}

export function movePane(root, paneID, toRegionID) {
  const from = regionOf(root, paneID);
  if (!from || from.id === toRegionID) return clone(root);
  const without = closePane(root, paneID);
  return addPane(without, toRegionID, paneID);
}

export function split(root, regionID, dir, defaults) {
  const next = clone(root);
  const region = findNode(next, regionID);
  if (!region || region.type !== 'region') return next;
  const fresh = { type: 'region', id: nextID(next, 'r'), tabs: [], active: null };
  const node = { type: 'split', id: nextID(next, 's'), dir: dir === 'row' ? 'row' : 'col', ratio: clampRatio(0.5, defaults), a: region, b: fresh };
  return replaceNode(next, regionID, node);
}

export function setRatio(root, splitID, ratio, defaults) {
  const next = clone(root);
  const node = findNode(next, splitID);
  if (node?.type === 'split') node.ratio = clampRatio(ratio, defaults);
  return next;
}

// ---- normalization ----------------------------------------------------------

// normalize drops pane ids the catalog does not know, collapses splits that lost a
// side, guarantees one region survives, and re-clamps ratios. Malformed input
// yields the empty host, never a blank panel.
export function normalize(root, catalog, defaults) {
  const known = ids(catalog);
  const seen = new Set();
  const fix = node => {
    if (!isObject(node)) return null;
    if (node.type === 'region') {
      const tabs = (Array.isArray(node.tabs) ? node.tabs : []).filter(id => typeof id === 'string' && known.has(id) && !seen.has(id) && seen.add(id));
      return { type: 'region', id: typeof node.id === 'string' ? node.id : '', tabs, active: tabs.includes(node.active) ? node.active : (tabs[0] || null) };
    }
    if (node.type !== 'split') return null;
    const a = fix(node.a), b = fix(node.b);
    if (!a && !b) return null;
    if (!a) return b;
    if (!b) return a;
    if (a.type === 'region' && !a.tabs.length && (b.type !== 'region' || b.tabs.length)) return b;
    if (b.type === 'region' && !b.tabs.length && (a.type !== 'region' || a.tabs.length)) return a;
    return { type: 'split', id: typeof node.id === 'string' ? node.id : '', dir: node.dir === 'row' ? 'row' : 'col', ratio: clampRatio(node.ratio, defaults), a, b };
  };
  const fixed = fix(root) || emptyRoot();
  return assignIDs(fixed);
}

// assignIDs gives every node a unique id, keeping valid existing ones.
function assignIDs(root) {
  const used = new Set();
  let counter = 0;
  walk(root, node => {
    const valid = /^(?:r|s)\d+$/.test(node.id || '') && !used.has(node.id);
    if (!valid) { do { counter++; node.id = `${node.type === 'region' ? 'r' : 's'}${counter}`; } while (used.has(node.id)); }
    used.add(node.id);
  });
  return root;
}

export function applyPreset(name, catalog, defaults) {
  const preset = defaults.presets[name] ?? defaults.presets[defaults.defaultPreset];
  const root = normalize(preset ? clone(preset) : null, catalog, defaults);
  if (openPanes(root).length) return root;
  // A catalog with none of the preset's panes still gets its first available pane.
  const first = (Array.isArray(catalog) ? catalog : []).find(item => item && item.available !== false);
  return first ? addPane(emptyRoot(), 'r1', first.id) : emptyRoot();
}

export function defaultSurface(catalog, defaults) {
  return { root: applyPreset(defaults.defaultPreset, catalog, defaults), hidden: false, preset: defaults.defaultPreset || undefined };
}

export function normalizeSurface(value, catalog, defaults) {
  if (!isObject(value) || !isObject(value.root)) return defaultSurface(catalog, defaults);
  const root = normalize(value.root, catalog, defaults);
  if (!openPanes(root).length && !value.explicit_empty) return defaultSurface(catalog, defaults);
  return { root, hidden: value.hidden === true, preset: typeof value.preset === 'string' ? value.preset : undefined, explicit_empty: value.explicit_empty === true || undefined };
}

// ---- preferences ------------------------------------------------------------

export function emptyPanelPreferences() {
  return { version: PANEL_PREFS_VERSION, surfaces: {}, module_disabled: [], recently_closed: {} };
}

// migratePanelPreferences accepts v4, v3, the pre-v3 {disabled,pick} shape, or
// garbage. It is catalog-free: every stored surface migrates, and normalization
// against a catalog happens lazily when that surface is shown.
export function migratePanelPreferences(value, defaults) {
  const result = emptyPanelPreferences();
  if (!isObject(value)) return result;
  const disabledSource = Array.isArray(value.module_disabled) ? value.module_disabled : (Array.isArray(value.disabled) ? value.disabled : []);
  result.module_disabled = [...new Set(disabledSource.filter(id => typeof id === 'string'))];
  if (value.version === PANEL_PREFS_VERSION) return migrateV4(value, result);
  if (value.version === 3 && isObject(value.surfaces)) return migrateV3(value, result, defaults);
  return migrateLegacyPicks(value, result);
}

function migrateV4(value, result) {
  if (isObject(value.surfaces)) {
    for (const [surface, stored] of Object.entries(value.surfaces)) if (isObject(stored) && isObject(stored.root)) result.surfaces[surface] = stored;
  }
  if (isObject(value.recently_closed)) {
    for (const [surface, list] of Object.entries(value.recently_closed)) if (Array.isArray(list)) result.recently_closed[surface] = list.filter(id => typeof id === 'string');
  }
  return result;
}

function migrateV3(value, result, defaults) {
  for (const [surface, stored] of Object.entries(value.surfaces)) {
    const migrated = migrateV3Surface(stored, defaults);
    if (migrated) result.surfaces[surface] = migrated;
  }
  return result;
}

function migrateLegacyPicks(value, result) {
  if (!isObject(value.pick)) return result;
  for (const [surface, paneID] of Object.entries(value.pick)) {
    if (typeof paneID !== 'string' || result.module_disabled.includes(paneID)) continue;
    result.surfaces[surface] = { root: { type: 'region', id: 'r1', tabs: [paneID], active: paneID }, hidden: false };
  }
  return result;
}

// migrateV3Surface: slots:[a] → one region; slots:[a,b] + ratio → a stacked split
// with a on top (v3's first slot was the top pane); slots:[] → explicit empty host.
function migrateV3Surface(stored, defaults) {
  if (!isObject(stored) || !Array.isArray(stored.slots)) return null;
  const slots = stored.slots.filter(id => typeof id === 'string');
  if (slots.length === 0) return { root: emptyRoot(), hidden: false, explicit_empty: true };
  if (slots.length === 1) return { root: { type: 'region', id: 'r1', tabs: [slots[0]], active: slots[0] }, hidden: false };
  return {
    root: { type: 'split', id: 's1', dir: 'col', ratio: clampRatio(stored.ratio, defaults),
      a: { type: 'region', id: 'r1', tabs: [slots[0]], active: slots[0] },
      b: { type: 'region', id: 'r2', tabs: [slots[1]], active: slots[1] } },
    hidden: false,
  };
}

export function readPanelPreferences(storage, defaults) {
  let raw = null;
  let warning = false;
  try { raw = storage?.getItem?.(PANEL_PREFS_KEY); } catch { warning = true; }
  let parsed = null;
  if (raw) {
    try { parsed = JSON.parse(raw); } catch { warning = true; }
  }
  const migrated = migratePanelPreferences(parsed, defaults);
  if (!parsed || JSON.stringify(parsed) !== JSON.stringify(migrated)) {
    try { storage?.setItem?.(PANEL_PREFS_KEY, JSON.stringify(migrated)); } catch { warning = true; }
  }
  return { preferences: migrated, warning };
}

export function writePanelPreferences(storage, preferences) {
  try {
    storage?.setItem?.(PANEL_PREFS_KEY, JSON.stringify(preferences));
    return true;
  } catch {
    return false;
  }
}

export function withSurfacePreference(preferences, surface, value) {
  return { ...preferences, version: PANEL_PREFS_VERSION, surfaces: { ...(preferences?.surfaces || {}), [surface]: clone(value) } };
}

export function withModuleEnabled(preferences, id, enabled) {
  const disabled = new Set(Array.isArray(preferences?.module_disabled) ? preferences.module_disabled : []);
  enabled ? disabled.delete(id) : disabled.add(id);
  return { ...preferences, version: PANEL_PREFS_VERSION, module_disabled: [...disabled] };
}

export function withRecentlyClosed(preferences, surface, paneID, defaults) {
  const current = (preferences?.recently_closed?.[surface] || []).filter(id => id !== paneID);
  const list = [paneID, ...current].slice(0, defaults.recentlyClosed);
  return { ...preferences, version: PANEL_PREFS_VERSION, recently_closed: { ...(preferences?.recently_closed || {}), [surface]: list } };
}

export function recentlyClosed(preferences, surface) {
  return [...(preferences?.recently_closed?.[surface] || [])];
}
