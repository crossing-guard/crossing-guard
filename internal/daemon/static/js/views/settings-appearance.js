// Settings › Appearance (session-view-and-console-preferences plan §C4). Every
// value here is part of an appearance or theme module the daemon stores; an edit
// previews live on this page and Save writes it. Nothing is kept in the browser.
import { el } from '../core.js';
import { loadAppearanceCatalog, saveAppearance, deleteAppearance, saveTheme, selectAppearance,
  reloadAppearanceSheet, previewTokens, appearanceModule, ownerMessage, uniqueID, tokenWords } from '../appearance-api.js';

const REFERENCE_SIZE = 14;

// contrastRatio is the WCAG ratio of two #rrggbb / #rgb colours.
export function contrastRatio(a, b) {
  const luminance = hex => {
    let value = String(hex || '').replace('#', '');
    if (value.length === 3) value = value.split('').map(c => c + c).join('');
    const channel = shift => {
      const v = ((parseInt(value, 16) >> shift) & 255) / 255;
      return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4;
    };
    return 0.2126 * channel(16) + 0.7152 * channel(8) + 0.0722 * channel(0);
  };
  const [high, low] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (high + 0.05) / (low + 0.05);
}

// previewProperties is what the sheet would emit for this draft: fonts, type
// steps, widths and the accent of the scheme being previewed.
export function previewProperties(draft, steps, scheme) {
  const out = { '--ui-font': draft.ui_font, '--mono': draft.mono_font,
    '--measure': draft.transcript_width + 'px', '--chat-measure': draft.chat_width + 'px' };
  steps.forEach((reference, index) => {
    out['--fs-' + (index + 1)] = Math.round(reference * draft.text_size / REFERENCE_SIZE * 100) / 100 + 'px';
  });
  const accent = draft.accent?.[scheme] || {};
  if (accent.accent) out['--accent'] = accent.accent;
  if (accent.accent_ink) out['--accent-ink'] = accent.accent_ink;
  if (accent.accent_fg) out['--accent-fg'] = accent.accent_fg;
  return out;
}

export async function renderAppearancePage(main) {
  const host = el('div', 'settings-appearance');
  main.appendChild(host);
  let catalog;
  try { catalog = await loadAppearanceCatalog(); } catch (error) {
    host.appendChild(el('div', 'banner', 'Appearance could not be loaded: ' + (error.message || error)));
    return;
  }
  paintAppearance(host, catalog);
}

function paintAppearance(host, catalog) {
  host.replaceChildren();
  previewTokens(null); // a repaint starts from what is saved
  const selected = catalog.appearances.find(item => item.id === catalog.selected) || catalog.appearances[0];
  const steps = catalog.type_steps || [];
  const draft = structuredClone(appearanceModule(selected));
  const themes = scheme => catalog.themes.filter(theme => theme.scheme === scheme);
  const refresh = async () => paintAppearance(host, await loadAppearanceCatalog());
  // Two schemes are in play: the one the console is drawn in now (what a live
  // preview may touch), and the one the sample pane is showing.
  const osScheme = () => (globalThis.matchMedia?.('(prefers-color-scheme: light)')?.matches ? 'light' : 'dark');
  const liveScheme = () => (draft.follow_os ? osScheme() : draft.pinned_scheme);
  let scheme = liveScheme();
  const sample = sampleTranscript();
  const editor = themeEditor(catalog, refresh);
  const preview = () => {
    previewTokens(previewProperties(draft, steps, liveScheme()));
    paintSample(sample, catalog, draft, scheme);
  };
  const setScheme = value => { scheme = value; repaintSlots(); preview(); };
  const schemes = schemeFields(draft, themes, preview, setScheme, editor);
  const accents = accentFields(draft, catalog, () => scheme, preview, setScheme);
  // A pinned scheme hides the other scheme's slots; their stored values stay in the draft.
  const repaintSlots = () => {
    for (const node of [...schemes.querySelectorAll('[data-scheme]'), ...accents.querySelectorAll('[data-scheme]')]) {
      node.classList.toggle('hidden', !draft.follow_os && node.dataset.scheme !== draft.pinned_scheme);
    }
    sample.switcher.classList.toggle('hidden', !draft.follow_os);
    for (const button of sample.switcher.children) button.setAttribute('aria-pressed', String(button.dataset.scheme === scheme));
  };
  for (const button of sample.switcher.children) button.onclick = () => setScheme(button.dataset.scheme);
  const controls = el('div', 'settings-appearance-controls');
  controls.append(appearanceChooser(catalog, selected, refresh), schemes, accents,
    typeFields(draft, catalog.limits, preview), appearanceActions(draft, selected, catalog, refresh), editor);
  const layout = el('div', 'settings-appearance-layout');
  layout.append(controls, sample.node);
  host.appendChild(layout);
  repaintSlots();
  paintSample(sample, catalog, draft, scheme);
}

// sampleTranscript is a few transcript rows drawn with one theme's own colours,
// so a theme, an accent or a text size can be judged without leaving the page.
function sampleTranscript() {
  const node = el('div', 'settings-sample-wrap');
  const head = el('div', 'row');
  const switcher = el('div', 'settings-tabs');
  for (const scheme of ['dark', 'light']) {
    const button = el('button', 'settings-tab', scheme === 'dark' ? 'Dark' : 'Light');
    button.type = 'button'; button.dataset.scheme = scheme;
    switcher.appendChild(button);
  }
  head.append(el('span', 'sub', 'Preview'), switcher);
  const box = el('div', 'settings-sample');
  box.append(el('div', 'settings-sample-user', 'Review the header design and fix what is overloaded.'),
    el('div', '', 'The header has twelve elements; three of them report activity.'),
    el('div', 'settings-sample-tool', 'grep -rn "watching" static/js'),
    el('span', 'settings-sample-action', 'Send'));
  const ratios = el('div', 'settings-contrast');
  node.append(head, box, ratios);
  return { node, box, ratios, switcher };
}

function paintSample(sample, catalog, draft, scheme) {
  const chosen = catalog.themes.find(theme => theme.id === draft['theme_' + scheme]);
  const base = catalog.themes.find(theme => theme.id === scheme && theme.builtin) || { tokens: {} };
  const tokens = { ...base.tokens, ...(chosen?.tokens || {}) };
  const accent = draft.accent?.[scheme] || {};
  if (accent.accent) tokens.accent = accent.accent;
  if (accent.accent_ink) tokens['accent-ink'] = accent.accent_ink;
  sample.box.style.cssText = '';
  sample.box.style.setProperty('color-scheme', scheme);
  for (const [name, value] of Object.entries(tokens)) sample.box.style.setProperty('--' + name, value);
  sample.ratios.replaceChildren();
  for (const [fg, bg] of [['text', 'bg'], ['dim', 'bg'], ['accent-ink', 'accent']]) {
    if (!tokens[fg] || !tokens[bg]) continue;
    const ratio = contrastRatio(tokens[fg], tokens[bg]);
    sample.ratios.appendChild(el('span', 'settings-ratio' + (ratio < 4.5 ? ' low' : ''), tokenWords(fg) + ' on ' + tokenWords(bg).toLowerCase() + ' ' + ratio.toFixed(1) + ':1'));
  }
}

function appearanceChooser(catalog, selected, refresh) {
  const row = el('div', 'row settings-row');
  const select = el('select');
  for (const item of catalog.appearances) {
    const option = el('option', '', item.name + (item.builtin && item.origin === 'builtin' ? '' : ' · yours'));
    option.value = item.id;
    select.appendChild(option);
  }
  select.value = selected.id;
  const status = el('span', 'sub');
  select.onchange = async () => {
    previewTokens(null);
    try { await selectAppearance(select.value); reloadAppearanceSheet(); await refresh(); }
    catch (error) { status.textContent = ownerMessage(error); }
  };
  row.append(el('span', 'settings-label', 'Appearance'), select, status);
  return row;
}

function schemeFields(draft, themes, preview, onScheme, editor) {
  const box = el('div', 'settings-group');
  // themeSlot is one scheme's theme, chosen from cards that show the theme's own colours.
  const themeSlot = (scheme, key, label) => {
    const slot = el('div', 'settings-theme-slot');
    slot.dataset.scheme = scheme;
    const cards = el('div', 'settings-theme-cards');
    const paint = () => { for (const card of cards.children) card.setAttribute('aria-pressed', String(card.dataset.theme === draft[key])); };
    for (const theme of themes(scheme)) {
      const card = el('button', 'settings-theme-card');
      card.type = 'button'; card.dataset.theme = theme.id;
      const swatch = el('span', 'settings-theme-swatch');
      for (const token of ['bg', 'panel', 'text', 'accent']) {
        const chip = el('i');
        if (theme.tokens?.[token]) chip.style.background = theme.tokens[token];
        swatch.appendChild(chip);
      }
      card.append(swatch, el('span', '', theme.name));
      card.onclick = () => { draft[key] = theme.id; paint(); onScheme(scheme); };
      cards.appendChild(card);
    }
    paint();
    const edit = el('button', 'btn ghost', 'Edit this theme\u2019s colours…');
    edit.type = 'button';
    edit.onclick = () => { editor.open = true; editor.showTheme(draft[key]); editor.scrollIntoView?.({ block: 'nearest' }); };
    slot.append(el('span', 'settings-label', label), cards, edit);
    return slot;
  };
  const mode = el('select');
  for (const [value, label] of [['auto', 'Follow the system'], ['dark', 'Always dark'], ['light', 'Always light']]) {
    const option = el('option', '', label); option.value = value; mode.appendChild(option);
  }
  mode.value = draft.follow_os ? 'auto' : draft.pinned_scheme;
  mode.onchange = () => {
    setSchemeMode(draft, mode.value);
    onScheme(draft.follow_os ? (globalThis.matchMedia?.('(prefers-color-scheme: light)')?.matches ? 'light' : 'dark') : mode.value);
  };
  box.append(field('Light or dark', mode), themeSlot('dark', 'theme_dark', 'Dark theme'), themeSlot('light', 'theme_light', 'Light theme'));
  return box;
}

function typeFields(draft, limits, preview) {
  const box = el('div', 'settings-group');
  const text = (key, placeholder) => {
    const input = el('input'); input.value = draft[key]; input.placeholder = placeholder; input.spellcheck = false;
    input.oninput = () => { draft[key] = input.value; preview(); };
    return input;
  };
  const range = (key, min, max, step, unit) => {
    const input = el('input'); input.type = 'range'; input.min = min; input.max = max; input.step = step; input.value = draft[key];
    const out = el('span', 'settings-value', draft[key] + unit);
    input.oninput = () => { draft[key] = Number(input.value); out.textContent = input.value + unit; preview(); };
    const wrap = el('span', 'settings-range'); wrap.append(input, out);
    return wrap;
  };
  box.append(field('Interface font', text('ui_font', '-apple-system, sans-serif')),
    field('Code font', text('mono_font', 'ui-monospace, monospace')),
    field('Text size', range('text_size', limits.text_size_min, limits.text_size_max, 0.5, 'px')),
    field('Transcript width', range('transcript_width', limits.width_min, limits.width_max, 20, 'px')),
    field('Chat width', range('chat_width', limits.width_min, limits.width_max, 20, 'px')));
  return box;
}

function accentFields(draft, catalog, currentScheme, preview, onScheme) {
  const box = el('div', 'settings-group');
  for (const scheme of ['dark', 'light']) box.appendChild(accentField(draft, catalog, scheme, currentScheme, preview, onScheme));
  return box;
}

// setSchemeMode, setAccent and clearAccent are the page's edits to a draft.
// Each changes its own field and leaves every other stored field as it was.
export function setSchemeMode(draft, value) {
  draft.follow_os = value === 'auto';
  draft.pinned_scheme = draft.follow_os ? '' : value;
  return draft;
}

export function setAccent(draft, scheme, colour) {
  draft.accent = { ...(draft.accent || {}), [scheme]: accentSet(scheme, colour) };
  return draft;
}

export function clearAccent(draft, scheme) {
  if (draft.accent) delete draft.accent[scheme];
  return draft;
}

// accentSet is the accent triple for one scheme: dark text-on-dark uses the
// accent itself; light keeps its theme's darker text colour.
export function accentSet(scheme, colour) {
  const set = { accent: colour, accent_ink: inkFor(colour) };
  if (scheme === 'dark') set.accent_fg = colour;
  return set;
}

// themeAccents is the accents the installed themes of one scheme use: the
// swatches offered, so no colour is written into this file.
export function themeAccents(catalog, scheme) {
  const seen = new Set();
  for (const theme of catalog.themes || []) {
    const accent = theme.scheme === scheme ? theme.tokens?.accent : '';
    if (accent) seen.add(String(accent).toLowerCase());
  }
  return [...seen];
}

function accentField(draft, catalog, scheme, currentScheme, preview, onScheme) {
  const offered = themeAccents(catalog, scheme);
  const input = el('input'); input.type = 'color';
  input.value = draft.accent?.[scheme]?.accent || offered[0] || input.value;
  const choose = colour => {
    setAccent(draft, scheme, colour);
    input.value = colour;
    if (currentScheme() === scheme) preview(); else onScheme(scheme);
  };
  const wrap = el('span', 'settings-range');
  for (const colour of offered) {
    const swatch = el('button', 'settings-accent-swatch');
    swatch.type = 'button'; swatch.style.background = colour;
    swatch.title = colour; swatch.setAttribute('aria-label', 'Accent ' + colour);
    swatch.onclick = () => choose(colour);
    wrap.appendChild(swatch);
  }
  const clear = el('button', 'btn ghost', 'Use the theme\u2019s');
  clear.type = 'button';
  input.oninput = () => choose(input.value);
  clear.onclick = () => { clearAccent(draft, scheme); if (currentScheme() === scheme) preview(); else onScheme(scheme); };
  wrap.append(input, clear);
  // A plain row, not a <label>: a label would hand a click on its text to the
  // first swatch and pick a colour nobody chose.
  const row = el('div', 'settings-field');
  row.append(el('span', 'settings-label', 'Accent (' + scheme + ')'), wrap);
  row.dataset.scheme = scheme;
  return row;
}

// inkFor is the text colour on an accent fill: whichever of near-black and
// white reads better on it.
function inkFor(hex) {
  return contrastRatio(hex, '#141604') >= contrastRatio(hex, '#ffffff') ? '#141604' : '#ffffff';
}

function appearanceActions(draft, selected, catalog, refresh) {
  const row = el('div', 'row settings-row');
  const status = el('span', 'sub');
  const save = el('button', 'btn primary', 'Save');
  save.onclick = async () => {
    try { await saveAppearance(draft, selected.state_token); reloadAppearanceSheet(); previewTokens(null); await refresh(); }
    catch (error) { status.textContent = ownerMessage(error); }
  };
  row.appendChild(save);
  if (selected.state_token) {
    const reset = el('button', 'btn', selected.builtin ? 'Reset to built-in' : 'Delete');
    reset.onclick = async () => {
      try { await deleteAppearance(selected.id, selected.state_token); reloadAppearanceSheet(); previewTokens(null); await refresh(); }
      catch (error) { status.textContent = ownerMessage(error); }
    };
    row.appendChild(reset);
  }
  const discard = el('button', 'btn ghost', 'Discard changes');
  discard.onclick = async () => { previewTokens(null); await refresh(); };
  row.append(discard, status);
  if (catalog.rejected?.length) row.appendChild(el('div', 'sub', catalog.rejected.length === 1
    ? 'One saved setting could not be read, so its default is used.'
    : catalog.rejected.length + ' saved settings could not be read, so their defaults are used.'));
  return row;
}

function themeEditor(catalog, refresh) {
  const box = el('details', 'settings-group settings-themes');
  box.appendChild(el('summary', '', 'Theme colours'));
  const select = el('select');
  for (const theme of catalog.themes) { const option = el('option', '', theme.name + ' (' + theme.scheme + ')'); option.value = theme.id; select.appendChild(option); }
  const body = el('div', 'settings-theme-body');
  const taken = new Set(catalog.themes.map(theme => theme.id));
  const show = () => {
    previewTokens(null);
    const theme = structuredClone(catalog.themes.find(item => item.id === select.value));
    // An installed theme inherits what it omits from the built-in of its scheme.
    const base = catalog.themes.find(item => item.id === theme.scheme && item.builtin) || { tokens: {} };
    theme.tokens = { ...base.tokens, ...theme.tokens };
    paintTheme(body, theme, taken, refresh);
  };
  select.onchange = show;
  box.append(field('Theme', select), body);
  show();
  box.showTheme = id => { if (catalog.themes.some(theme => theme.id === id)) { select.value = id; show(); } };
  return box;
}

function paintTheme(body, theme, taken, refresh) {
  body.replaceChildren();
  const ratios = el('div', 'settings-contrast');
  const paintRatios = () => {
    ratios.replaceChildren();
    for (const [fg, bg] of [['text', 'bg'], ['dim', 'bg'], ['accent-ink', 'accent']]) {
      const ratio = contrastRatio(theme.tokens[fg], theme.tokens[bg]);
      ratios.appendChild(el('span', 'settings-ratio' + (ratio < 4.5 ? ' low' : ''), tokenWords(fg) + ' on ' + tokenWords(bg).toLowerCase() + ' ' + ratio.toFixed(1) + ':1'));
    }
  };
  const grid = el('div', 'settings-token-grid');
  const preview = () => {
    const properties = { 'color-scheme': theme.scheme };
    for (const [name, value] of Object.entries(theme.tokens)) properties['--' + name] = value;
    previewTokens(properties);
  };
  for (const name of Object.keys(theme.tokens)) {
    const input = el('input'); input.type = 'color'; input.value = theme.tokens[name];
    input.oninput = () => { theme.tokens[name] = input.value; paintRatios(); preview(); };
    grid.appendChild(field(tokenWords(name), input));
  }
  paintRatios();
  const status = el('span', 'sub');
  const save = el('button', 'btn', 'Save theme');
  save.onclick = () => writeTheme(theme, theme.state_token, status, refresh);
  const saveAs = el('button', 'btn ghost', 'Save theme as…');
  saveAs.onclick = () => {
    const name = prompt('Name for the new theme');
    if (!name) return;
    writeTheme({ ...theme, id: uniqueID(name, taken, 'theme'), name }, '', status, refresh);
  };
  const actions = el('div', 'row'); actions.append(save, saveAs, status);
  body.append(ratios, grid, actions);
}

async function writeTheme(theme, token, status, refresh) {
  try { await saveTheme(theme, token); reloadAppearanceSheet(); previewTokens(null); await refresh(); }
  catch (error) { status.textContent = ownerMessage(error); }
}

function field(label, control) {
  const wrap = el('label', 'settings-field');
  wrap.append(el('span', 'settings-label', label), control);
  return wrap;
}
