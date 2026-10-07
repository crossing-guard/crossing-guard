import test from 'node:test';
import assert from 'node:assert/strict';
import { ruleFor, rowDisplay } from '../session/transcript-view.js';
import { previewRows, displaysFor, modeDraft, setMatch } from './settings-transcript-modes.js';
import { themeAccents, accentSet, setAccent, clearAccent, setSchemeMode, previewProperties } from './settings-appearance.js';
import { ladderSteps } from './settings-runtimes.js';
import { modelColumns } from './settings-models.js';
import { shownAs } from '../session-organization/settings-view-model.js';
import { agentIndexGroups, weekShares } from '../orchestration/agents/roster-model.js';

const mode = { rules: [
  { match: { kind: 'tool_call', fact: 'exec:run' }, display: 'hide' },
  { match: { kind: 'user', min_chars: 10, max_chars: 20 }, display: 'collapse' },
  { match: { kind: 'thinking' }, display: 'hide' },
] };

test('one matcher: ruleFor names the deciding rule and rowDisplay agrees with it', () => {
  const call = { kind: 'tool_call', name: 'Bash', facts: ['exec:run'], text: 'ls' };
  assert.equal(ruleFor(call, mode), 0);
  assert.equal(rowDisplay(call, mode), 'hide');
  // A fact rule does not match a call without the fact; a length rule needs both bounds.
  assert.equal(ruleFor({ kind: 'tool_call', name: 'Grep', facts: ['search:code'], text: 'x' }, mode), -1);
  assert.equal(ruleFor({ kind: 'user', text: 'twelve chars' }, mode), 1);
  assert.equal(ruleFor({ kind: 'user', text: 'a much longer message than twenty characters' }, mode), -1);
  assert.equal(ruleFor({ kind: 'user', text: 'short' }, mode), -1);
  assert.equal(rowDisplay({ kind: 'assistant', text: 'hello' }, mode), 'show');
  assert.equal(ruleFor({ kind: 'assistant' }, null), -1);
});

test('the preview tags each row with its rule; a result carries its call’s, an undecided row none', () => {
  const rows = [
    { kind: 'tool_call', name: 'Bash', facts: ['exec:run'], text: 'ls' },
    { kind: 'tool_result', text: 'a b c' },
    { kind: 'tool_call', name: 'Grep', facts: ['search:code'], text: 'x' },
    { kind: 'tool_result', text: 'hit' },
    { kind: 'assistant', text: 'done' },
    { kind: 'thinking', text: 'hmm' },
  ];
  assert.deepEqual(previewRows(rows, mode).map(item => [item.display, item.rule]),
    [['hide', 0], ['hide', 0], ['show', -1], ['show', -1], ['show', -1], ['hide', 2]]);
  assert.deepEqual(displaysFor('thinking'), ['show', 'hide']);
  assert.deepEqual(displaysFor('user'), ['show', 'collapse', 'hide']);
});

test('accent swatches are the installed themes’ own accents for that scheme', () => {
  const catalog = { themes: [
    { id: 'dark', scheme: 'dark', tokens: { accent: '#AABBCC' } }, { id: 'hc', scheme: 'dark', tokens: { accent: '#aabbcc' } },
    { id: 'mine', scheme: 'dark', tokens: { accent: '#112233' } }, { id: 'light', scheme: 'light', tokens: { accent: '#445566' } },
    { id: 'bare', scheme: 'light', tokens: {} },
  ] };
  assert.deepEqual(themeAccents(catalog, 'dark'), ['#aabbcc', '#112233']);
  assert.deepEqual(themeAccents(catalog, 'light'), ['#445566']);
  assert.deepEqual(themeAccents({}, 'dark'), []);
});

test('the page’s edits to an appearance change their own field and no other', () => {
  const stored = { follow_os: true, pinned_scheme: '', theme_dark: 'hc', theme_light: 'paper', ui_font: 'A, sans-serif', mono_font: 'B, monospace',
    text_size: 14, transcript_width: 880, chat_width: 860, accent: { dark: accentSet('dark', '#112233'), light: accentSet('light', '#445566') } };
  const changedKeys = draft => Object.keys(stored).filter(key => JSON.stringify(stored[key]) !== JSON.stringify(draft[key]));
  // An accent for one scheme leaves the other scheme's accent and both themes.
  const accented = setAccent(structuredClone(stored), 'light', '#778899');
  assert.deepEqual(changedKeys(accented), ['accent']);
  assert.deepEqual(accented.accent.dark, stored.accent.dark);
  assert.equal(accented.accent.light.accent, '#778899');
  // Pinning a scheme keeps the hidden scheme's theme and accent in the draft.
  const pinned = setSchemeMode(structuredClone(stored), 'dark');
  assert.deepEqual(changedKeys(pinned), ['follow_os', 'pinned_scheme']);
  assert.deepEqual([pinned.theme_light, pinned.accent.light], [stored.theme_light, stored.accent.light]);
  assert.deepEqual(changedKeys(setSchemeMode(pinned, 'auto')), []);
  // "The theme's own" removes that scheme's accent only.
  const cleared = clearAccent(structuredClone(stored), 'dark');
  assert.deepEqual(Object.keys(cleared.accent), ['light']);
  assert.deepEqual(clearAccent({ theme_dark: 'x' }, 'dark'), { theme_dark: 'x' });
  // The live preview uses the accent of the scheme it is told is on screen.
  assert.equal(previewProperties(accented, [14], 'dark')['--accent'], '#112233');
  assert.equal(previewProperties(accented, [14], 'light')['--accent'], '#778899');
});

test('a mode’s editable copy holds every match field, and editing one keeps the rest', () => {
  const profile = { id: 'm', name: 'Mode', description: 'd', rules: [
    { match: { kind: 'tool_call', fact: 'exec:run', tool: 'Bash', min_chars: 10, max_chars: 500 }, display: 'hide' }] };
  const draft = modeDraft(profile);
  assert.deepEqual(draft.rules, profile.rules);
  assert.notEqual(draft.rules[0], profile.rules[0], 'a copy, not the stored object');
  setMatch(draft.rules[0], 'tool', 'Grep');
  assert.deepEqual(draft.rules[0].match, { kind: 'tool_call', fact: 'exec:run', tool: 'Grep', min_chars: 10, max_chars: 500 });
  setMatch(draft.rules[0], 'min_chars', 0);
  assert.deepEqual(draft.rules[0].match, { kind: 'tool_call', fact: 'exec:run', tool: 'Grep', max_chars: 500 });
  assert.equal(profile.rules[0].match.tool, 'Bash');
});

test('the runtime ladder shows stored facts, and a fact nobody reports is neither met nor missing', () => {
  assert.deepEqual(ladderSteps({ presence_reported: true, installed: true, hook_configured: true, hook_binary_present: true, last_live_at: 't' })
    .map(([, met]) => met), [true, true, true, false]);
  assert.deepEqual(ladderSteps({ presence_reported: false, installed: true, hook_configured: true, hook_binary_present: false }).map(([, met]) => met),
    [null, false, false, false]);
  assert.deepEqual(ladderSteps({ presence_reported: true, installed: false }).map(([, met]) => met), [false, false, false, false]);
});

test('a model list shows only the columns some model has a value for; a zero price is a price', () => {
  const bare = [{ label: 'A', id: 'a', groupLabel: '', contextTokens: 0, price: null, inputs: [] }];
  assert.deepEqual(modelColumns(bare).map(([label]) => label), ['Model']);
  const zero = { per_tokens: 1000000, unit: 'USD', rates: [{ class: 'input', amount: 0 }, { class: 'output', amount: 0 }] };
  const full = [{ label: 'B', id: 'b', groupLabel: 'G', contextTokens: 200000, price: zero, pricePartial: false, inputs: ['text'] }, ...bare];
  const columns = modelColumns(full);
  assert.deepEqual(columns.map(([label]) => label), ['Model', 'Group', 'Context', 'Price (runtime-stated)', 'Inputs']);
  const price = columns[3][1];
  assert.notEqual(price(full[0]), 'unknown');
  assert.match(price(full[0]), /0/);
  assert.equal(price(full[1]), 'unknown');
});

test('a view’s row says how it is shown from what it stores, and nothing parses its filter', () => {
  assert.equal(shownAs({ group_by: '', sort: 'longest' }), 'List · by repository · longest first');
  assert.equal(shownAs({ group_by: 'tag-key:flow', sort: 'oldest', board: { columns: ['building', 'done'] } }),
    'Board · by tag flow · oldest first · building, done');
  assert.equal(shownAs({}), 'List · by repository · newest first');
});

const row = (name, state) => ({ name, state, hasDraft: false, search: name.toLowerCase() });

test('agents that run nowhere fold away only under All with no search', () => {
  const rows = [row('Failing', 'attn'), row('Live', 'on'), row('Draft', 'draft'), row('Stopped', 'off'), row('Unplaced', 'none')];
  const folded = agentIndexGroups(rows, 'all', '');
  assert.deepEqual(folded.rows.map(item => item.name), ['Failing', 'Live', 'Draft']);
  assert.deepEqual(folded.idle.map(item => item.name), ['Stopped', 'Unplaced']);
  // A search hit inside the fold shows; so does the Off chip's own list.
  assert.deepEqual(agentIndexGroups(rows, 'all', 'stop'), { rows: [rows[3]], idle: [] });
  assert.deepEqual(agentIndexGroups(rows, 'off', '').rows.map(item => item.name), ['Stopped']);
  assert.deepEqual(agentIndexGroups(rows, 'off', '').idle, []);
  assert.deepEqual(agentIndexGroups([rows[1]], 'all', ''), { rows: [rows[1]], idle: [] });
});

test('the week bar splits runs into acted, failed and the rest', () => {
  assert.deepEqual(weekShares({ runs: 73, acted: 0, failed: 73 }), { acted: 0, failed: 100, other: 0 });
  assert.deepEqual(weekShares({ runs: 36, acted: 19, failed: 12 }), { acted: 53, failed: 33, other: 14 });
  assert.equal(weekShares({ runs: 0 }), null);
});

test('a runtime row stays as the owner left it; a problem or a request opens it for that draw only', async () => {
  const { runtimeRowOpen } = await import('./settings-runtimes.js');
  const none = new Set();
  const open = options => runtimeRowOpen({ name: 'r', opened: none, closed: none, ...options });
  assert.equal(open({}), false);
  assert.equal(open({ target: 'r' }), true);
  assert.equal(open({ target: 'other' }), false);
  assert.equal(open({ attention: 'hook_outdated' }), true);
  assert.equal(open({ attention: 'never_fired' }), false, 'a note is not a problem');
  // The owner's own click wins both ways, whatever the row would do by itself.
  assert.equal(open({ opened: new Set(['r']) }), true);
  assert.equal(open({ closed: new Set(['r']), target: 'r', attention: 'hook_outdated' }), false);
  // The owner's choice is one or the other, never both, and the latest wins.
  const { recordRowChoice } = await import('./settings-runtimes.js');
  const rows = { opened: new Set(), closed: new Set() };
  recordRowChoice(rows, 'r', true);
  assert.deepEqual([[...rows.opened], [...rows.closed]], [['r'], []]);
  recordRowChoice(rows, 'r', false);
  assert.deepEqual([[...rows.opened], [...rows.closed]], [[], ['r']]);
  // A row opened for a problem, then acted in (recorded open), stays open once the problem clears.
  recordRowChoice(rows, 'r', true);
  assert.equal(runtimeRowOpen({ name: 'r', attention: '', ...rows }), true);
});

test('what Settings shows on entry and on restore: only a request tells Agents anything', async () => {
  const { settleDecision } = await import('./settings-model.js');
  const agent = { page: 'agents', target: 'helper-a' };
  // First visit: the overview is drawn; nothing is said to the Agents page.
  assert.deepEqual(settleDecision({}), { page: 'overview', target: '', show: true, tellAgents: false });
  // Re-clicking Settings while reading an agent (a fresh render, nothing asked): the agent stays.
  assert.deepEqual(settleDecision({ mounted: agent, drawn: false }), { page: 'agents', target: 'helper-a', show: true, tellAgents: false });
  // Coming back with nothing asked: nothing is drawn or said.
  assert.deepEqual(settleDecision({ mounted: agent, drawn: true, restored: true }), { page: 'agents', target: 'helper-a', show: false, tellAgents: false });
  // A request for the Agents page alone means the list, even with Agents mounted.
  assert.deepEqual(settleDecision({ requested: { page: 'agents', target: '' }, mounted: agent, drawn: true, restored: true }),
    { page: 'agents', target: '', show: true, tellAgents: true });
  assert.equal(settleDecision({ requested: { page: 'agents' }, mounted: { page: 'agents', target: '' }, drawn: true }).show, true);
  // A request that names an agent opens it and says so.
  assert.deepEqual(settleDecision({ requested: { page: 'agents', target: 'helper-b' }, mounted: agent, drawn: true }),
    { page: 'agents', target: 'helper-b', show: true, tellAgents: true });
  // The Agents page was already told by the event itself: show it, say nothing more.
  assert.deepEqual(settleDecision({ requested: { page: 'agents', target: 'helper-b', external: true }, mounted: agent, drawn: true }),
    { page: 'agents', target: 'helper-b', show: true, tellAgents: false });
  // A re-read on restore with nothing asked drops the old target …
  const runtimes = { page: 'runtimes', target: 'r' };
  assert.deepEqual(settleDecision({ mounted: runtimes, drawn: true, restored: true, reread: true }), { page: 'runtimes', target: '', show: true, tellAgents: false });
  // … but a request that arrives with the restore keeps its own target.
  assert.deepEqual(settleDecision({ requested: { page: 'runtimes', target: 'q' }, mounted: runtimes, drawn: true, restored: true, reread: true }),
    { page: 'runtimes', target: 'q', show: true, tellAgents: true });
  // A page typed into is not read again (reread false): nothing is drawn.
  assert.equal(settleDecision({ mounted: runtimes, drawn: true, restored: true, reread: false }).show, false);
  // A fresh render is never a re-read: the mounted target is kept.
  assert.equal(settleDecision({ mounted: runtimes, drawn: false, reread: true }).target, 'r');
});
