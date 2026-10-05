// Share with team… (team rest-of-release plan §4.3, §14 Q13; criterion 93): the
// lead builds the next revision of a bundle that carries this agent. The daemon
// writes the unsigned file under its own data directory; the sheet then shows
// the two acts that remain — sign it where the organization key is, upload the
// signed file — with the full path and the sign command to copy, and the diff
// against the published revision. The console never holds the key and writes
// nothing into a checkout or a Downloads folder.
import { api } from '../../core.js';
import { loadTeamStatus } from '../../team-link.js';
import { el, button, row, spacer, problem, field, selectBox, checkbox, keyValue } from './agent-ui.js';
import { openDialog } from './agent-dialog.js';
import { shareView } from './route-model.js';
import { repositoryName } from './roster-model.js';

const KEEP = '';
// How long the built revision is valid, as the build route's durations; the
// first choice leaves the team's configured default in force.
const EXPIRY_CHOICES = Object.freeze([[KEEP, 'The team’s default'], ['168h', '7 days'], ['720h', '30 days'], ['2160h', '90 days']]);
const FAILURE_CHOICES = Object.freeze([[KEEP, 'As published'], ['fail-open', 'fail-open'], ['fail-closed', 'fail-closed']]);
const CONTENT_CHOICES = Object.freeze([[KEEP, 'As published'], ['off', 'off'], ['consent', 'consent'], ['mandated', 'mandated']]);

// buildBundle asks the daemon to build; a refusal's own words are the error.
async function buildBundle(request) {
  try {
    return await api('/api/team/bundles/build', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(request) });
  } catch (error) {
    let message = '';
    try { message = String(JSON.parse(String(error?.message || ''))?.error || ''); } catch { message = ''; }
    throw message ? Object.assign(new Error(message), { status: error.status }) : error;
  }
}

// addShareItem puts "Share with team…" in the ⋯ menu, in its place, and shows it
// once this device is known to be linked.
export function addShareItem(ctx, list, item) {
  const node = item('Share with team…', () => openShareSheet(ctx, node.team));
  node.classList.add('hidden');
  list.append(node);
  loadTeamStatus().then(team => {
    if (team?.state !== 'linked') return;
    node.team = team;
    node.classList.remove('hidden');
  }).catch(() => { /* not linked, or the link cannot be read: nothing to share with */ });
}

function scopeChoices(ctx) {
  const roots = [...new Set((ctx.agent.places || []).map(place => String(place.project_root || '')).filter(Boolean))];
  return [['organization', 'Organization — every member'],
    ...roots.map(root => ['repository\u0000' + root, 'Repository — ' + repositoryName(root)])];
}

function openShareSheet(ctx, team) {
  const organization = String(team?.organization?.name || 'your team');
  const inputs = {
    scope: selectBox(scopeChoices(ctx), 'organization', 'Scope'),
    rules: checkbox(false, 'Include this device’s rules'),
    failure: selectBox(FAILURE_CHOICES, KEEP, 'Failure mode'),
    content: selectBox(CONTENT_CHOICES, KEEP, 'Content'),
    expires: selectBox(EXPIRY_CHOICES, KEEP, 'Expires in'),
  };
  const state = { ctx, team, inputs, removed: [], result: el('div', 'agents-share-result') };
  const version = ctx.detail.current?.current?.version;
  const form = el('div', 'agents-stack');
  form.append(field('Scope', inputs.scope),
    keyValue([['Agent', ctx.agent.name || ctx.id], ['Version', version ? 'v' + version : '']]),
    inputs.rules.node, row(field('Failure mode', inputs.failure), field('Content', inputs.content), field('Expires in', inputs.expires)));
  openDialog({ title: 'Share with ' + organization, wide: true, body: [form, state.result], actions: [
    { label: 'Close', onClick: dialog => dialog.close() },
    { label: 'Build', primary: true, onClick: dialog => build(dialog, state) },
  ] });
}

function buildRequest(state) {
  const { inputs, ctx } = state;
  const [scope, cwd] = inputs.scope.value.split('\u0000');
  return { scope, ...(cwd ? { cwd } : {}), agents: [ctx.id], remove_agents: state.removed, rules: inputs.rules.input.checked,
    ...(inputs.failure.value ? { failure_mode: inputs.failure.value } : {}),
    ...(inputs.content.value ? { content_policy: inputs.content.value } : {}),
    ...(inputs.expires.value ? { expires_in: inputs.expires.value } : {}) };
}

async function build(dialog, state) {
  dialog.showProblem('');
  try {
    const built = await buildBundle(buildRequest(state));
    state.result.replaceChildren(resultNode(shareView(built, state.team?.server), state, dialog));
    dialog.primaryButton().textContent = 'Build again';
  } catch (error) {
    state.result.replaceChildren();
    dialog.showProblem('Not built: ' + String(error?.message || error));
  }
}

// resultNode shows the built revision: what is in it against what is published,
// then the two acts that remain.
function resultNode(view, state, dialog) {
  const node = el('div', 'agents-stack');
  node.append(el('h3', 'agents-card-title', view.title),
    keyValue([['Against', view.against], ['Valid until', view.expires]]), documentsNode(view, state, dialog));
  for (const change of view.changes) node.appendChild(keyValue([[change.label, change.from + ' → ' + change.to]]));
  const rules = rulesWords(view.rules);
  if (rules) node.appendChild(keyValue([['Rules', rules]]));
  node.append(step('1 · Sign it, where the organization key is', [copyRow('Built file', view.path), copyRow('Sign command', view.signCommand)]),
    step('2 · Upload the signed file', [copyRow('Signed file', view.signedPath), uploadRow(view.uploadURL)]));
  return node;
}

function rulesWords(rules) {
  return [rules.added.length ? rules.added.length + ' added' : '', rules.removed.length ? rules.removed.length + ' removed' : '',
    rules.changed.length ? rules.changed.length + ' changed' : ''].filter(Boolean).join(', ');
}

// documentsNode lists the bundle's documents with what changed; another agent in
// the bundle can be left out, which builds again.
function documentsNode(view, state, dialog) {
  const node = el('div', 'agents-share-documents');
  node.appendChild(el('div', 'agents-field-label', 'In the bundle'));
  for (const document of view.documents) {
    const line = row(el('span', document.removed ? 'agents-muted' : '', document.name || document.kind), el('span', 'agents-sub', document.change), spacer());
    if (document.profileID && !document.removed && document.profileID !== state.ctx.id) {
      line.appendChild(button('Leave out', 'ghost', () => { state.removed.push(document.profileID); return build(dialog, state); }));
    }
    node.appendChild(line);
    if (document.diff.length) node.appendChild(diffNode(document));
  }
  return node;
}

function diffNode(document) {
  const node = el('div', 'agents-diff');
  const changed = document.diff.filter(line => line.op !== 'keep');
  const head = node.appendChild(el('div', 'agents-diff-head'));
  head.append(el('span', '', document.name), el('span', 'agents-diff-counts', '+' + changed.filter(line => line.op === 'add').length
    + ' −' + changed.filter(line => line.op !== 'add').length));
  for (const line of changed) {
    node.appendChild(el('div', 'agents-diff-line agents-diff-' + (line.op === 'add' ? 'add' : 'del'), (line.op === 'add' ? '+ ' : '- ') + line.text));
  }
  return node;
}

function step(title, rows) {
  const node = el('div', 'agents-share-step');
  node.append(el('div', 'agents-share-step-title', title), ...rows.filter(Boolean));
  return node;
}

// copyRow is one labelled value in full, with Copy.
function copyRow(label, value) {
  const node = el('div', 'agents-copy-row');
  const copy = button('Copy', '', async () => {
    try {
      await navigator.clipboard.writeText(value);
      copy.textContent = 'Copied';
    } catch {
      copy.textContent = 'Select and copy';
    }
  });
  node.append(el('span', 'agents-kv-k', label), el('code', 'agents-copy-value', value), copy);
  return node;
}

function uploadRow(url) {
  if (!url) return problem('The team console’s address is not known on this device.');
  const node = el('div', 'agents-copy-row');
  const link = el('a', 'agents-share-link', url);
  link.href = url;
  link.target = '_blank';
  link.rel = 'noopener noreferrer';
  node.append(el('span', 'agents-kv-k', 'Upload page'), link);
  return node;
}
