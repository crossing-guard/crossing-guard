// Versions tab (plan §7): every stored version with the places pinned to it,
// a compare of any two grouped by what changed, and Use in… (moving places to
// a version, which is also how a place rolls back). History is never rewritten.
import { el, button, row, spacer, problem, errorText, selectBox, card, stateLabel, sequence } from './agent-ui.js';
import { loadRevision } from './roster-api.js';
import { compareGroups, relativeTime, placeName, labelFor } from './roster-model.js';
import { diffLines, diffStats } from './text-diff.js';
import { openMoveDialog } from './agent-move.js';
import { sourceDialog } from './agent-dialog.js';

export function renderVersions(pane, ctx) {
  const versions = versionList(ctx);
  const grid = el('div', 'agents-cols');
  const list = card('Versions');
  for (const version of versions) list.appendChild(versionRow(ctx, version));
  if (!versions.length) list.appendChild(el('div', 'agents-sub', 'No stored version.'));
  grid.append(list, compareCard(ctx, versions));
  pane.appendChild(grid);
}

// versionList puts the draft (if any) first, then current and history.
function versionList(ctx) {
  const out = [];
  if (ctx.draft && ctx.draft.normalized) {
    out.push({ key: 'draft', version: ctx.draft.normalized.version, draft: true, normalized: ctx.draft.normalized, places: [],
      source: ctx.draft.source, at: Date.parse(ctx.draft.updated_at) / 1000 });
  }
  for (const revision of ctx.detail.history || []) {
    out.push({ key: revision.source_digest + '\u0000' + revision.bundle_digest, version: revision.version, current: revision.current,
      source_digest: revision.source_digest, bundle_digest: revision.bundle_digest, places: revision.places || [],
      at: Date.parse(revision.selected_at) / 1000 });
  }
  return out;
}

function versionRow(ctx, version) {
  const places = (ctx.agent.places || []).filter(place => version.places.includes(place.place_id));
  const tag = version.draft ? stateLabel('draft', 'draft') : version.current ? el('span', 'chip agents-chip-current', 'current') : null;
  const facts = [(ctx.origin && !version.draft ? 'adopted ' : '') + relativeTime(version.at), places.length ? places.map(place => placeName(place) + (place.state === 'enabled' ? '' : ' (off)')).join(', ')
    : (version.draft ? 'not running anywhere' : 'no place runs it')].filter(Boolean).join(' · ');
  const actions = [];
  if (version.draft && !ctx.origin?.readOnly) actions.push(button('Open draft', '', () => ctx.openTab('instructions', { edit: true })));
  if (!version.draft) actions.push(button('View', 'ghost', () => viewSource(ctx, version)));
  if (!version.draft && mayUseIn(ctx, version) && (ctx.agent.places || []).some(place => !version.places.includes(place.place_id))) {
    actions.push(button('Use in…', '', () => useIn(ctx, version)));
  }
  const node = el('div', 'agents-version');
  node.append(row(el('span', 'agents-mono agents-version-name', 'v' + version.version), tag, el('span', 'agents-sub', facts), spacer(), ...actions));
  return node;
}

// mayUseIn: a shared agent's places move only to a version its team still
// shares — the adopted one, which is the current version here (criterion 74).
// The daemon refuses any other move; the page does not offer it.
function mayUseIn(ctx, version) {
  if (!ctx.origin) return true;
  return Boolean(version.current) && !ctx.origin.released;
}

async function viewSource(ctx, version) {
  try {
    const detail = await loadRevision(ctx.id, version.source_digest, version.bundle_digest);
    sourceDialog('v' + version.version + ' · PROFILE.md', detail.source);
  } catch (error) {
    sourceDialog('v' + version.version, errorText(error, 'This version cannot be read'));
  }
}

async function useIn(ctx, version) {
  let normalized = null;
  try {
    normalized = (await loadRevision(ctx.id, version.source_digest, version.bundle_digest)).normalized;
  } catch (error) {
    sourceDialog('v' + version.version, errorText(error, 'This version cannot be read'));
    return;
  }
  openMoveDialog(ctx, { title: 'Use v' + version.version + ' in…', confirmLabel: 'Move',
    target: { version: version.version, source_digest: version.source_digest, bundle_digest: version.bundle_digest, normalized },
    preselect: () => false });
}

/* ---------- compare ---------- */

function compareCard(ctx, versions) {
  const node = card('Compare');
  if (versions.length < 2) {
    node.appendChild(el('div', 'agents-sub', 'One version so far.'));
    return node;
  }
  const options = versions.map(version => [version.key, (version.draft ? 'draft ' : 'v') + version.version]);
  const before = selectBox(options, versions[1].key, 'Compare from');
  const after = selectBox(options, versions[0].key, 'Compare to');
  const result = el('div');
  const ticket = sequence();
  const redraw = () => showCompare(ctx, versions, before.value, after.value, result, ticket());
  before.onchange = redraw;
  after.onchange = redraw;
  node.append(row(before, el('span', 'agents-sub', '→'), after), result);
  redraw();
  return node;
}

async function profileFor(ctx, version) {
  if (version.normalized) return version.normalized;
  const detail = await loadRevision(ctx.id, version.source_digest, version.bundle_digest);
  return detail.normalized;
}

async function showCompare(ctx, versions, beforeKey, afterKey, result, current) {
  const pick = key => versions.find(version => version.key === key);
  result.replaceChildren(el('div', 'agents-sub', 'Comparing…'));
  try {
    const [a, b] = await Promise.all([profileFor(ctx, pick(beforeKey)), profileFor(ctx, pick(afterKey))]);
    if (!current()) return;
    const groups = compareGroups(a, b);
    const blocks = groups.changed.map(group => groupBlock(ctx, group.name, group.before, group.after));
    if (groups.instructionsChanged) blocks.push(instructionsBlock(groups.instructionsBefore, groups.instructionsAfter));
    if (!blocks.length) blocks.push(el('div', 'agents-sub', 'These versions define the same agent.'));
    result.replaceChildren(...blocks, el('div', 'agents-sub', groups.same.length ? 'Unchanged: ' + groups.same.join(' · ') : ''));
  } catch (error) {
    if (current()) result.replaceChildren(problem(errorText(error, 'Cannot compare')));
  }
}

function groupBlock(ctx, name, before, after) {
  const node = el('div', 'agents-diff');
  node.append(el('div', 'agents-diff-head', name),
    el('div', 'agents-diff-line agents-diff-del', '- ' + plain(ctx, before)),
    el('div', 'agents-diff-line agents-diff-add', '+ ' + plain(ctx, after)));
  return node;
}

// plain writes a compared value as words: signal kinds by their label,
// lists joined, objects as "key: value" pairs.
function plain(ctx, value) {
  if (value == null || value === '') return 'none';
  if (typeof value === 'string') return labelFor(ctx.detail.signals, value);
  if (Array.isArray(value)) return value.length ? value.map(item => plain(ctx, item)).join(', ') : 'none';
  if (typeof value === 'object') {
    const pairs = Object.entries(value).filter(([, item]) => item !== '' && item != null && !(Array.isArray(item) && !item.length));
    return pairs.length ? pairs.map(([key, item]) => key.replaceAll('_', ' ') + ': ' + plain(ctx, item)).join(' · ') : 'none';
  }
  return String(value);
}

function instructionsBlock(before, after) {
  const diff = diffLines(before, after);
  const stats = diffStats(diff);
  const node = el('div', 'agents-diff');
  node.appendChild(el('div', 'agents-diff-head', 'Instructions · +' + stats.added + ' −' + stats.removed
    + (diff.truncated ? ' · too large to compare line by line' : '')));
  for (const line of diff.lines) {
    if (line.op === 'same') continue;
    node.appendChild(el('div', 'agents-diff-line agents-diff-' + line.op, (line.op === 'add' ? '+ ' : '- ') + line.text));
  }
  return node;
}
