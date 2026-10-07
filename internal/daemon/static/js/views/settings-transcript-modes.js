// Settings › Transcript modes (session-view-and-console-preferences plan §B5).
// A mode is a view-profile module: an ordered list of rules, the first match
// deciding whether a row is shown, collapsed to one line, or hidden behind a
// count. Editing a built-in saves the owner's copy under the same id; "Revert"
// deletes that copy. Which mode each kind of session opens with is selection.
import { el, api } from '../core.js';
import { ruleFor } from '../session/transcript-view.js';
import { KIND_WORDS, ownerMessage, uniqueID } from '../appearance-api.js';

const KINDS = ['', 'user', 'assistant', 'thinking', 'tool_call', 'tool_result', 'summary', 'system', 'context', 'other'];
// Rows of these kinds already render as one line, so collapse is not offered.
const CHIP_KINDS = new Set(['tool_call', 'tool_result', 'thinking']);
const ROLES = ['helper', 'follower', 'reviewer'];

// The sample the preview runs a mode over when no session supplies rows.
const SAMPLE = [
  { kind: 'context', text: 'Session start: three memories recalled.' },
  { kind: 'user', text: 'Review the header design and fix what is overloaded.' },
  { kind: 'thinking', text: 'Find what each element is before proposing anything.' },
  { kind: 'tool_call', name: 'Bash', text: 'grep -rn "watching" static/js', facts: ['exec:run'] },
  { kind: 'tool_result', text: 'session-orchestration.js:22' },
  { kind: 'tool_call', name: 'Grep', text: 'font-size', facts: ['search:code'] },
  { kind: 'user', text: 'Context pack: ' + 'x'.repeat(2400) },
  { kind: 'assistant', text: 'The header has twelve elements; three of them report activity.' },
];

// displaysFor is what a rule on this kind can do.
export function displaysFor(kind) {
  return CHIP_KINDS.has(kind) ? ['show', 'hide'] : ['show', 'collapse', 'hide'];
}

// previewRows runs a mode over rows the way the transcript does: a result goes
// wherever its call went. Each row carries the index of the rule that decided
// it (-1 when none did and it is shown by default); a result carries its call's.
export function previewRows(rows, mode) {
  const out = [];
  let call = null;
  for (const row of rows) {
    let rule = ruleFor(row, mode);
    let display = rule < 0 ? 'show' : mode.rules[rule].display;
    if (row.kind === 'tool_result' && call) {
      display = call.display === 'hide' ? 'hide' : 'show';
      rule = call.rule;
    }
    call = row.kind === 'tool_call' ? { display, rule } : null;
    out.push({ row, display, rule });
  }
  return out;
}

export async function renderTranscriptModesPage(main) {
  const host = el('div', 'settings-modes');
  main.appendChild(host);
  await paintModes(host, null);
}

async function paintModes(host, selectedID) {
  let modules, config;
  try {
    [modules, config] = await Promise.all([api('/api/console/view-profiles'), api('/api/console/config')]);
  } catch (error) {
    host.replaceChildren(el('div', 'banner', 'Transcript modes could not be loaded: ' + (error.message || error)));
    return;
  }
  const profiles = modules.profiles || [];
  const current = profiles.find(profile => profile.id === selectedID) || profiles[0];
  const refresh = id => paintModes(host, id);
  const list = el('div', 'settings-mode-list');
  for (const profile of profiles) {
    const item = el('button', 'settings-mode' + (profile.id === current?.id ? ' active' : ''));
    item.type = 'button';
    item.append(el('strong', '', profile.name), el('span', 'sub', profile.origin === 'builtin' ? 'built-in' : 'yours'),
      el('div', 'sub', profile.description || ''));
    item.onclick = () => refresh(profile.id);
    list.appendChild(item);
  }
  // Which mode each kind of session opens with comes first: it is the choice
  // made most often, and the editor below is for changing a mode itself.
  const grid = el('div', 'settings-modes-grid');
  host.replaceChildren(selectionFields(profiles, config, refresh), grid);
  grid.append(list, current ? modeEditor(current, refresh, new Set(profiles.map(item => item.id))) : el('div'));
}

function selectionFields(profiles, config, refresh) {
  const box = el('div', 'settings-group settings-opens-with');
  box.appendChild(el('div', 'settings-label', 'Opens with'));
  const selection = config.config?.transcript || {};
  const status = el('div', 'sub');
  const choose = (value, write) => modeSelect(profiles, value, write, refresh, status);
  const token = config.state_token || '';
  box.appendChild(field('Any session', choose(selection.default_profile, id => ({ state_token: token, default_profile: id }))));
  for (const role of ROLES) {
    box.appendChild(field(role + ' agents', choose(selection.role_profiles?.[role] || selection.default_profile,
      id => ({ state_token: token, role_profiles: { [role]: id } }))));
  }
  box.appendChild(status);
  return box;
}

function modeSelect(profiles, value, write, refresh, status) {
  const select = el('select');
  for (const profile of profiles) { const option = el('option', '', profile.name); option.value = profile.id; select.appendChild(option); }
  select.value = value || '';
  select.onchange = () => writeSelection(write(select.value), refresh, status);
  return select;
}

async function writeSelection(body, refresh, status) {
  try { await api('/api/console/config/transcript', { method: 'PUT', body: JSON.stringify(body) }); await refresh(); }
  catch (error) { status.textContent = ownerMessage(error); }
}

// modeDraft is the editable copy of a mode: every rule with every match field
// it stores, so an edit to one field saves the rest unchanged.
export function modeDraft(profile) {
  return structuredClone({ format_version: 2, id: profile.id, name: profile.name,
    description: profile.description || '', rules: profile.rules || [] });
}

function modeEditor(profile, refresh, taken) {
  const draft = modeDraft(profile);
  const box = el('div', 'settings-group settings-mode-editor');
  const name = el('input'); name.value = draft.name;
  name.oninput = () => { draft.name = name.value; };
  const table = el('div', 'settings-rules');
  const preview = el('div', 'settings-mode-preview');
  const repaint = () => { paintRules(table, draft, repaint); paintPreview(preview, draft); };
  repaint();
  const add = el('button', 'btn', '+ rule');
  add.onclick = () => { draft.rules.push({ match: {}, display: 'show' }); repaint(); };
  box.append(field('Name', name), table, add,
    el('div', 'sub', 'The first rule that matches a row decides it; a row no rule matches is shown. A tool result goes wherever its call went.'),
    modeActions(profile, draft, refresh, taken), el('div', 'settings-label', 'Preview'), preview);
  return box;
}

// What each control in a rule row is, in the row's own order.
const RULE_COLUMNS = Object.freeze(['#', 'Row kind', 'Fact', 'Tool', 'Min chars', 'Max chars', 'Show as', '', '', '']);

function paintRules(table, draft, repaint) {
  table.replaceChildren();
  if (draft.rules.length) {
    const header = el('div', 'settings-rule settings-rule-head');
    for (const label of RULE_COLUMNS) header.appendChild(el('span', 'sub', label));
    table.appendChild(header);
  }
  draft.rules.forEach((rule, index) => {
    const row = el('div', 'settings-rule');
    const kind = choice(KINDS, rule.match.kind || '', value => { setMatch(rule, 'kind', value); fixDisplay(rule); repaint(); }, KIND_WORDS);
    const fact = text(rule.match.fact, 'any fact', value => setMatch(rule, 'fact', value), repaint);
    const tool = text(rule.match.tool, 'any tool', value => setMatch(rule, 'tool', value), repaint);
    const min = number(rule.match.min_chars, 'min', value => setMatch(rule, 'min_chars', value), repaint);
    const max = number(rule.match.max_chars, 'max', value => setMatch(rule, 'max_chars', value), repaint);
    const display = choice(displaysFor(rule.match.kind), rule.display, value => { rule.display = value; repaint(); });
    const move = (step, label) => {
      const button = el('button', 'btn ghost', label); button.type = 'button';
      button.setAttribute('aria-label', step < 0 ? 'Move rule up' : 'Move rule down');
      button.onclick = () => { const next = index + step; if (next < 0 || next >= draft.rules.length) return;
        [draft.rules[index], draft.rules[next]] = [draft.rules[next], draft.rules[index]]; repaint(); };
      return button;
    };
    const remove = el('button', 'btn ghost', '✕'); remove.type = 'button';
    remove.setAttribute('aria-label', 'Remove rule');
    remove.onclick = () => { draft.rules.splice(index, 1); repaint(); };
    row.append(el('span', 'sub', String(index + 1)), kind, fact, tool, min, max, display, move(-1, '↑'), move(1, '↓'), remove);
    table.appendChild(row);
  });
}

export function setMatch(rule, key, value) {
  if (value === '' || value === 0 || value === null) delete rule.match[key]; else rule.match[key] = value;
}

function fixDisplay(rule) {
  if (!displaysFor(rule.match.kind).includes(rule.display)) rule.display = 'show';
}

// paintPreview shows every sample row under the draft: hidden rows struck
// through, folded rows clipped, each naming the rule that decided it.
function paintPreview(host, draft) {
  host.replaceChildren();
  const counts = { show: 0, collapse: 0, hide: 0 };
  for (const { row, display, rule } of previewRows(SAMPLE, draft)) {
    counts[display] += 1;
    const label = row.kind === 'tool_call' ? row.name : (KIND_WORDS[row.kind] || row.kind);
    const line = el('div', 'settings-preview-row ' + display);
    line.appendChild(el('span', 'settings-preview-text', label + ' · ' + row.text.slice(0, display === 'collapse' ? 60 : 120) + (display === 'collapse' ? '…' : '')));
    if (rule >= 0) line.appendChild(el('span', 'sub settings-preview-rule', 'rule ' + (rule + 1)));
    host.appendChild(line);
  }
  host.appendChild(el('div', 'sub', counts.show + ' shown · ' + counts.collapse + ' folded · ' + counts.hide + ' hidden'));
}

function modeActions(profile, draft, refresh, taken) {
  const row = el('div', 'row');
  const status = el('span', 'sub');
  const put = (module, token) => putMode(module, token, status, refresh);
  const save = el('button', 'btn primary', 'Save');
  save.onclick = () => put(draft, profile.state_token || '');
  const copy = el('button', 'btn', 'Duplicate as new mode');
  copy.onclick = () => {
    const name = prompt('Name for the new mode', draft.name + ' copy');
    if (!name) return;
    put({ ...draft, id: uniqueID(name, taken, 'mode'), name }, '');
  };
  row.append(save, copy);
  if (profile.state_token) {
    const revert = el('button', 'btn', profile.builtin ? 'Revert to built-in' : 'Delete');
    revert.onclick = () => {
      if (!confirm(profile.builtin ? 'Revert "' + profile.name + '" to the built-in? Your changes to it are removed.'
        : 'Delete "' + profile.name + '"?')) return;
      revertMode(profile, status, refresh);
    };
    row.appendChild(revert);
  }
  row.appendChild(status);
  return row;
}

async function putMode(module, token, status, refresh) {
  try {
    await api('/api/console/view-profiles/' + encodeURIComponent(module.id), { method: 'PUT', body: JSON.stringify({ state_token: token, profile: module }) });
    await refresh(module.id);
  } catch (error) { status.textContent = ownerMessage(error); }
}

async function revertMode(profile, status, refresh) {
  try {
    await api('/api/console/view-profiles/' + encodeURIComponent(profile.id) + '?state_token=' + encodeURIComponent(profile.state_token), { method: 'DELETE' });
    await refresh(profile.builtin ? profile.id : null);
  } catch (error) { status.textContent = ownerMessage(error); }
}

function choice(values, current, onChange, words = {}) {
  const select = el('select');
  for (const value of values) { const option = el('option', '', words[value] || value); option.value = value; select.appendChild(option); }
  select.value = current || '';
  select.onchange = () => onChange(select.value);
  return select;
}

function text(value, placeholder, onChange, repaint) {
  const input = el('input'); input.value = value || ''; input.placeholder = placeholder; input.spellcheck = false;
  input.onchange = () => { onChange(input.value.trim()); repaint(); };
  return input;
}

function number(value, placeholder, onChange, repaint) {
  const input = el('input'); input.type = 'number'; input.min = '0'; input.value = value || ''; input.placeholder = placeholder;
  input.onchange = () => { onChange(Number(input.value) || 0); repaint(); };
  return input;
}

function field(label, control) {
  const wrap = el('label', 'settings-field');
  wrap.append(el('span', 'settings-label', label), control);
  return wrap;
}
