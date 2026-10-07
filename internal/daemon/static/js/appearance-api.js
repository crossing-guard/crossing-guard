// Console appearance, browser side (session-view-and-console-preferences plan
// §C). The daemon owns the modules and renders the selected one as
// /appearance.css; this module reads the catalog, writes through the guarded
// routes, previews unsaved edits, and reloads the sheet after a save. It keeps
// nothing in browser storage.
import { api } from './core.js';

// The fields a module is written with; everything else in a catalog entry is
// what the daemon adds when it resolves one (origin, tokens, inheritance).
const APPEARANCE_FIELDS = ['format_version', 'id', 'name', 'theme_dark', 'theme_light', 'follow_os', 'pinned_scheme',
  'accent', 'ui_font', 'mono_font', 'text_size', 'transcript_width', 'chat_width'];
const THEME_FIELDS = ['format_version', 'id', 'name', 'scheme', 'tokens', 'labels'];

function pick(source, fields) {
  const out = {};
  for (const field of fields) if (source[field] !== undefined && source[field] !== '') out[field] = source[field];
  return out;
}

export const appearanceModule = entry => pick(entry, APPEARANCE_FIELDS);

// Row kinds and theme tokens in the owner's words, for the editors' labels.
export const KIND_WORDS = { '': 'any row', user: 'your messages', assistant: 'replies', thinking: 'thinking',
  tool_call: 'tool calls', tool_result: 'tool results', summary: 'summaries', system: 'system notes',
  context: 'added context', other: 'other rows' };
const TOKEN_WORDS = { bg: 'Background', panel: 'Panels', panel2: 'Raised panels', surface: 'Surfaces', border: 'Borders',
  text: 'Text', dim: 'Quiet text', accent: 'Accent', 'accent-ink': 'Text on accent', 'accent-fg': 'Accent text',
  accent2: 'Links', ok: 'Success', warn: 'Warning', bad: 'Error', purple: 'Highlight',
  'user-bubble-bg': 'Your messages', 'user-bubble-border': 'Your messages, border' };
export function tokenWords(name) {
  if (TOKEN_WORDS[name]) return TOKEN_WORDS[name];
  const chip = /^chip-(\w+)-(bg|fg)$/.exec(name);
  if (chip) return chip[1].charAt(0).toUpperCase() + chip[1].slice(1) + ' chips, ' + (chip[2] === 'bg' ? 'fill' : 'text');
  return name.replace(/-/g, ' ');
}

// ownerMessage turns a refused write into words the owner acts on. A conflict
// means someone else changed the same thing; a refusal says what to change.
export function ownerMessage(error) {
  if (error?.status === 409) {
    return /selected|uses|appearance /.test(String(error.message))
      ? 'In use: ' + String(error.message).replace(/^the module is selected by /, 'chosen by ').replace(/; choose another.*$/, '') + '. Choose another there first.'
      : 'This changed somewhere else. Reload the page and try again.';
  }
  let text = String(error?.message || error || 'Not saved.');
  text = text.replace(/^the (module is invalid|console configuration would be invalid): /, '');
  text = text.replace(/rules\[(\d+)\]\.(match\.)?/g, (_, index) => 'rule ' + (Number(index) + 1) + ': ');
  for (const [kind, words] of Object.entries(KIND_WORDS)) if (kind) text = text.replace(new RegExp('\\b' + kind + '\\b', 'g'), words);
  return 'Not saved: ' + text;
}

// uniqueID is the id a new module gets from its name: never one an existing
// module already has, so "Save as" can never silently replace a built-in.
export function uniqueID(name, taken, fallback) {
  const base = String(name).toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 60) || fallback;
  let id = base;
  for (let n = 2; taken.has(id); n++) id = base + '-' + n;
  return id;
}
export const themeModule = entry => pick(entry, THEME_FIELDS);

export const loadAppearanceCatalog = () => api('/api/console/appearance');

export function saveAppearance(entry, stateToken) {
  const module = appearanceModule(entry);
  return api('/api/console/appearance/' + encodeURIComponent(module.id),
    { method: 'PUT', body: JSON.stringify({ state_token: stateToken || '', appearance: module }) });
}

export function deleteAppearance(id, stateToken) {
  return api('/api/console/appearance/' + encodeURIComponent(id) + '?state_token=' + encodeURIComponent(stateToken || ''), { method: 'DELETE' });
}

export function saveTheme(entry, stateToken) {
  const module = themeModule(entry);
  return api('/api/console/themes/' + encodeURIComponent(module.id),
    { method: 'PUT', body: JSON.stringify({ state_token: stateToken || '', theme: module }) });
}

export function deleteTheme(id, stateToken) {
  return api('/api/console/themes/' + encodeURIComponent(id) + '?state_token=' + encodeURIComponent(stateToken || ''), { method: 'DELETE' });
}

export async function selectAppearance(id) {
  const config = await api('/api/console/config');
  return api('/api/console/config/appearance', { method: 'PUT', body: JSON.stringify({ state_token: config.state_token || '', module: id }) });
}

// reloadAppearanceSheet refetches /appearance.css after a save.
export function reloadAppearanceSheet() {
  const link = document.getElementById('appearance-sheet');
  if (link) link.href = '/appearance.css?v=' + Date.now();
}

// previewTokens applies unsaved values as inline custom properties on the root,
// which beat the sheet; null clears the preview. It never writes storage and
// never sets data-theme.
let previewed = [];
export function previewTokens(properties) {
  const style = document.documentElement.style;
  for (const name of previewed) style.removeProperty(name);
  previewed = [];
  if (!properties) return;
  for (const [name, value] of Object.entries(properties)) {
    style.setProperty(name, value);
    previewed.push(name);
  }
}

export const schemeMode = module => (module?.follow_os ? 'auto' : (module?.pinned_scheme || 'auto'));

// setSchemeMode switches the selected appearance between following the OS and
// pinning one scheme. A built-in is saved as the owner's copy under its id.
export async function setSchemeMode(mode) {
  const catalog = await loadAppearanceCatalog();
  const selected = catalog.appearances.find(item => item.id === catalog.selected);
  if (!selected) throw new Error('the selected appearance is not loaded');
  const next = { ...appearanceModule(selected), follow_os: mode === 'auto', pinned_scheme: mode === 'auto' ? '' : mode };
  await saveAppearance(next, selected.state_token);
  reloadAppearanceSheet();
  return mode;
}

// migrateLegacyTheme turns the old per-browser theme choice (localStorage
// cp_theme) into the owner's saved appearance once, then forgets it. Auto is
// what the built-in already does, so it only needs forgetting.
export async function migrateLegacyTheme(storage = globalThis.localStorage) {
  let legacy = null;
  try { legacy = storage?.getItem?.('cp_theme'); } catch { return; }
  if (!legacy) return;
  const forget = () => { try { storage.removeItem('cp_theme'); } catch { /* nothing to forget */ } };
  if (legacy === 'dark' || legacy === 'light') {
    try {
      // Only an untouched built-in takes the old choice: the saved appearance is
      // installation-wide, and a second browser must not overwrite a newer one.
      const catalog = await loadAppearanceCatalog();
      const selected = catalog.appearances.find(item => item.id === catalog.selected);
      if (selected?.origin === 'builtin') await setSchemeMode(legacy);
    } catch (error) {
      if (!(error?.status >= 400 && error.status < 500)) return; // try again on the next load
    }
  }
  forget();
  delete document.documentElement.dataset.theme;
}
