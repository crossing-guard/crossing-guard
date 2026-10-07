// Settings → Team (team rest-of-release plan §4.2): the header, a needs-you block only
// when something needs the person, From the team, From this device, and Details,
// collapsed. The page owns one element and repaints into it after every change. What
// each part says is settings-team-model.js; this file only draws it and runs the acts.
import { el } from "../core.js";
import { button, linkButton, problem, keyValue, errorText } from "../orchestration/agents/agent-ui.js";
import { loadRoster } from "../orchestration/agents/roster-api.js";
import { placeName } from "../orchestration/agents/roster-model.js";
import { loadTeamStatus, startTeamLink, unlinkTeam, loadTeamLayers, adoptLayer, unadoptLayer, repinOrgKey } from "../team-link.js";
import { teamPage, unadoptWords, unlinkWords, unlinkOutcome, approvalHref } from "./settings-team-model.js";
import { shareMemoryDialog } from "./settings-team-memory.js";
import { actDialog } from "./settings-team-dialog.js";
import { teamDetailsPart } from "./settings-team-details.js";

const CHIP_CLASS = Object.freeze({ good: 'st-verified', neutral: 'st-draft', warn: 'st-stale', bad: 'st-disputed' });
// How often a device waiting for approval asks the daemon whether it was approved.
const PENDING_RECHECK_MS = 5000;

async function renderTeamPage(main, ctx = {}) {
  const page = el('div', 'team-page');
  main.appendChild(page);
  await paintTeam(page, ctx);
}

// paintTeam redraws the whole page from the daemon's current answers. Every act
// (link, cancel, unlink, adopt, un-adopt, trust, share) and the pending recheck end
// here, and the sidebar's count is read again with it.
async function paintTeam(page, ctx) {
  if (!page.isConnected) return;
  let status, layers = null;
  try { status = await loadTeamStatus(); }
  catch (error) { page.replaceChildren(el('div', 'banner', 'Team could not be read: ' + errorText(error))); return; }
  try { layers = await loadTeamLayers(); } catch { /* said on the page below */ }
  if (!page.isConnected) return;
  const view = { page, ctx, status, layers: layers || {}, model: teamPage(status, layers),
    repaint: async () => { await paintTeam(page, ctx); void ctx.changed?.(); } };
  const parts = [headPart(view), needsPart(view), layers ? null : el('div', 'banner', 'What the team shares could not be read.'),
    linkPart(view), pendingPart(view), teamPart(view), ...view.model.leftovers.map(group => leftoverPart(view, group)),
    devicePart(view), teamDetailsPart(view)];
  page.replaceChildren(...parts.filter(Boolean));
  if (view.model.mode === 'pending') watchPending(view);
}

function headPart(view) {
  const { model } = view;
  const head = el('div', 'team-head');
  head.append(el('h2', '', model.heading), el('span', 'chip ' + CHIP_CLASS[model.chip.tone], model.chip.words));
  if (model.mode !== 'linked') return head;
  const menu = el('div', 'agents-menu');
  const items = el('div', 'agents-menu-list');
  const toggle = button('⋯', 'ghost', () => items.classList.toggle('open'));
  toggle.setAttribute('aria-label', 'Team actions');
  toggle.setAttribute('aria-haspopup', 'true');
  items.appendChild(button('Unlink…', 'agents-menu-item', () => { items.classList.remove('open'); unlinkDialog(view); }));
  menu.append(toggle, items);
  head.append(el('span', 'agents-spacer'), menu);
  return head;
}

// needsPart is absent when nothing needs the person.
function needsPart(view) {
  if (!view.model.needs.length) return null;
  const section = el('section', 'settings-card settings-attention-list');
  section.setAttribute('aria-label', 'Needs you');
  for (const item of view.model.needs) section.appendChild(needsItem(view, item));
  return section;
}

function needsItem(view, item) {
  const row = el('div', 'settings-attention settings-attention-' + item.severity);
  const mark = el('span', 'settings-attention-mark', item.severity === 'info' ? 'i' : '!');
  mark.setAttribute('aria-hidden', 'true');
  const body = el('div', 'settings-fact-text team-attention-body');
  body.append(el('strong', '', item.title));
  if (item.sub) body.appendChild(el('div', 'sub', item.sub));
  if (item.prints) body.appendChild(printsGrid(item.prints));
  if ((item.facts || []).length) body.appendChild(keyValue(item.facts));
  const changes = changesNode(item);
  if (changes) body.appendChild(changes);
  if (item.technical) body.appendChild(disclosure('Reason', el('div', 'agents-mono', item.technical)));
  if ((item.actions || []).length) {
    const actions = el('div', 'agents-row');
    for (const action of item.actions) actions.appendChild(actionNode(view, action, body));
    body.appendChild(actions);
  }
  row.append(mark, body);
  return row;
}

// printsGrid shows fingerprints large enough to compare by eye.
function printsGrid(prints) {
  return keyValue(prints.map(([label, print]) => [label, el('code', 'team-print', print)]));
}

function disclosure(summary, content, open = false) {
  const node = el('details', 'settings-disclosure');
  node.open = open;
  const inner = el('div', 'team-disclosed');
  inner.append(...[].concat(content));
  node.append(el('summary', '', summary), inner);
  return node;
}

function diffBlock(diff) {
  const block = el('div', 'agents-diff');
  block.appendChild(el('div', 'agents-diff-head', diff.head));
  for (const line of diff.lines || []) {
    block.appendChild(el('div', 'agents-diff-line ' + (line.op === 'add' ? 'agents-diff-add' : 'agents-diff-del'), (line.op === 'add' ? '+ ' : '- ') + line.text));
  }
  if (diff.text) block.appendChild(el('pre', 'agents-source team-source', diff.text));
  return block;
}

function documentsTable(rows, heads = ['In it', 'On this device']) {
  const table = el('table', 'settings-table');
  const head = el('tr');
  head.append(...heads.map(text => el('th', '', text)));
  table.appendChild(head);
  for (const [name, state] of rows) {
    const line = el('tr');
    line.append(el('td', '', name), el('td', '', state));
    table.appendChild(line);
  }
  return table;
}

// changesNode is what an offer changes: drawn in the card itself for a changed
// bundle, and in an open disclosure for a first offer, so Adopt is pressed beside it.
function changesNode(item) {
  const blocks = (item.diffs || []).map(diffBlock);
  if ((item.documents || []).length) blocks.push(documentsTable(item.documents, ['In it', 'When you adopt']));
  if (!blocks.length) return null;
  if (item.inline) {
    const inline = el('div', 'team-disclosed');
    inline.append(...blocks);
    return inline;
  }
  return disclosure('What changes', blocks, true);
}

// factsGrid is the label | value | action grid: one fact per row, labels in one column.
function factsGrid(view, rows) {
  const grid = el('div', 'team-facts');
  for (const row of rows) {
    const value = el('div');
    value.appendChild(el('div', '', row.value));
    for (const sub of row.subs || []) value.appendChild(el('div', 'sub', sub));
    const action = el('div');
    if (row.action) action.appendChild(actionNode(view, row.action, value));
    grid.append(el('span', 'team-facts-k', row.label), value, action);
  }
  return grid;
}

function card(title, ...children) {
  const section = el('section', 'settings-card');
  section.append(el('h3', '', title), ...children.filter(Boolean));
  return section;
}

function teamPart(view) {
  const { model } = view;
  if (model.mode !== 'linked') return null;
  return card('From the team', factsGrid(view, model.team), ...model.bundles.map(bundle => bundleDisclosure(view, bundle)));
}

// bundleDisclosure holds one adopted bundle's facts, what is in it, and Un-adopt.
function bundleDisclosure(view, bundle) {
  const actions = el('div', 'agents-row');
  actions.appendChild(actionNode(view, { kind: 'unadopt', label: 'Un-adopt…', danger: true, bundle }, actions));
  return disclosure(bundle.title + ', revision ' + bundle.revision, [keyValue(bundle.facts), documentsTable(bundle.documents), actions]);
}

function leftoverPart(view, group) {
  return card('Still on this device from ' + group.organization, factsGrid(view, group.rows));
}

function devicePart(view) {
  return card('From this device', factsGrid(view, view.model.device));
}

// actionNode draws one act a row or a needs-you item offers. `host` is where a
// failure is said, beside what was pressed.
function actionNode(view, action, host) {
  const go = {
    adopt: node => adopt(view, action, node, host),
    unadopt: () => unadoptDialog(view, action.bundle),
    trust: () => trustDialog(view, action),
    share: () => shareMemoryDialog(view),
    agent: () => view.ctx.go?.('agents', action.agent),
    settings: () => view.ctx.go?.(action.page, ''),
    view: () => document.dispatchEvent(new CustomEvent('cg:nav', { detail: action.view })),
  }[action.kind];
  if (['agent', 'settings', 'view'].includes(action.kind)) return linkButton(action.label, go);
  const node = button(action.label, action.primary ? 'primary' : action.danger ? 'danger' : '');
  node.onclick = () => go(node);
  return node;
}

async function adopt(view, action, node, host) {
  node.disabled = true;
  host.querySelector('.agents-problem')?.remove();
  try {
    await adoptLayer(action.scope, action.bundle, action.token);
    await view.repaint();
  } catch (error) {
    host.appendChild(problem(errorText(error, 'Not adopted')));
    node.disabled = false;
  }
}

// placesOn names, per shared agent, the places it is on in; empty when the roster
// cannot be read, and the dialog then says "wherever it is on".
async function placesOn(bundle) {
  let roster;
  try { roster = await loadRoster(); } catch { return {}; }
  const out = {};
  for (const agent of bundle.agents) {
    const found = (roster.agents || []).find(item => item.profile_id === agent.id);
    if (found) out[agent.id] = (found.places || []).filter(place => place.state === 'enabled').map(placeName);
  }
  return out;
}

async function unadoptDialog(view, bundle) {
  const places = await placesOn(bundle);
  actDialog(view, { title: 'Un-adopt ' + bundle.title.charAt(0).toLowerCase() + bundle.title.slice(1), body: keyValue(unadoptWords(bundle, places)),
    label: 'Un-adopt', failed: 'Not un-adopted', act: () => unadoptLayer(bundle.scope, bundle.organizationId) });
}

// trustDialog re-pins only after the person says the presented fingerprint matches
// the Policy page; the request carries that fingerprint and names this surface.
function trustDialog(view, action) {
  const check = el('input');
  check.type = 'checkbox';
  const label = el('label', 'agents-check');
  label.append(check, el('span', '', 'The new fingerprint matches the Policy page'));
  const prints = keyValue([['Trusted now', el('code', 'team-print', action.pinned)], ['New', el('code', 'team-print', action.presented)],
    ['Compare with', 'The Policy page of the team console']]);
  actDialog(view, { title: 'Trust new key', body: [prints, label], label: 'Trust new key', failed: 'Not trusted',
    ready: () => check.checked ? '' : 'Compare the new fingerprint with the Policy page first.', act: () => repinOrgKey(action.presented) });
}

function unlinkDialog(view) {
  actDialog(view, { title: 'Unlink this device', body: keyValue(unlinkWords(view.model)), label: 'Unlink', failed: 'Not unlinked', act: () => unlinkTeam(), after: unlinkOutcome });
}

function labelled(text, input) {
  const field = el('label', 'agents-field');
  input.type = 'text';
  input.setAttribute('aria-label', text);
  field.append(el('span', 'agents-field-label', text), input);
  return field;
}

// linkPart is the unlinked state's card: two labelled fields and Link.
function linkPart(view) {
  if (view.model.mode !== 'unlinked') return null;
  const server = el('input'), name = el('input');
  server.placeholder = 'https://team.example.com';
  name.placeholder = 'This computer’s name';
  const fields = el('div', 'agents-model-picker');
  fields.append(labelled('Team server', server), labelled('Device name', name));
  const actions = el('div', 'agents-row');
  const section = card('Link this device', fields, el('div', 'sub', 'Linking shows a code to approve in the team console.'), actions);
  if (view.model.problem) section.appendChild(problem(view.model.problem));
  const link = button('Link', 'primary', async () => {
    if (!server.value.trim()) { server.focus(); return; }
    link.disabled = true;
    section.querySelector('.agents-problem')?.remove();
    try { await startTeamLink(server.value.trim(), name.value.trim()); await view.repaint(); }
    catch (error) { section.appendChild(problem(errorText(error, 'Not linked'))); link.disabled = false; }
  });
  actions.appendChild(link);
  return section;
}

// approvalLink opens the team console's approval page in a new tab. The address is the
// server's: one that is not http or https is shown as text and is not a link.
function approvalLink(address) {
  const href = approvalHref(address);
  if (!href) return el('span', '', address);
  const link = el('a', '', address);
  link.href = href;
  link.target = '_blank';
  link.rel = 'noopener';
  return link;
}

// pendingPart is the linking state's card: one fact per row, and Cancel.
function pendingPart(view) {
  const pending = view.model.pending;
  if (!pending) return null;
  const mono = new Set(['Code', 'This device’s key']);
  const rows = pending.rows.map(([label, value]) => [label, mono.has(label) ? el('code', 'team-print', value) : label === 'Open' ? approvalLink(value) : value]);
  const actions = el('div', 'agents-row');
  const section = card(pending.title, keyValue(rows), actions);
  actions.appendChild(button('Cancel', '', async () => {
    try { await unlinkTeam(); await view.repaint(); }
    catch (error) { section.appendChild(problem(errorText(error, 'Not cancelled'))); }
  }));
  return section;
}

// watchPending asks again until the enrollment resolves, so an approval shows without
// a reload; leaving the page (or a repaint) stops it.
function watchPending(view) {
  const marker = view.page.firstChild;
  const recheck = async () => {
    if (!marker?.isConnected) return;
    try {
      const now = await loadTeamStatus();
      if (now.state !== 'pending') { await view.repaint(); return; }
    } catch { /* the next check will say */ }
    setTimeout(recheck, PENDING_RECHECK_MS);
  };
  setTimeout(recheck, PENDING_RECHECK_MS);
}

export { renderTeamPage };
