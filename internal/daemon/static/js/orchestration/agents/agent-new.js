// New agent (plan §7): start blank (per kind), from another agent, or from a
// PROFILE.md; name it; the daemon writes an inert draft. Its instructions,
// trigger, context and powers are then edited in the one Instructions editor,
// and publishing it leads to Where it runs.
import { el, button, linkButton, row, spacer, problem, errorText, textInput, field, selectBox, whileBusy } from './agent-ui.js';
import { startDraft, loadRoster } from './roster-api.js';
import { KIND_LABEL, KIND_BLURB } from './roster-model.js';
import { importDialog } from './agent-dialog.js';

const KINDS = Object.freeze(['helper', 'follower', 'reviewer']);

export async function renderNewAgent(page, route = {}) {
  const { host } = page;
  host.appendChild(row(linkButton('‹ Agents', () => page.navigate({ view: 'index' }))));
  host.appendChild(el('h2', 'agents-title', 'New agent'));
  let agents = [];
  try { agents = ((await loadRoster()).agents || []).filter(agent => !agent.draft_only); } catch { agents = []; }
  const state = { start: route.duplicateFrom ? 'duplicate' : 'blank', kind: 'helper', from: route.duplicateFrom || '' };
  const choices = el('div', 'agents-choices');
  const form = purposeForm(page, state, agents);
  const redraw = () => {
    choices.replaceChildren(...startChoices(state, page, agents, redraw));
    form.sync();
  };
  redraw();
  host.append(el('div', 'agents-field-label', 'Start from'), choices, form.node);
}

function startChoices(state, page, agents, redraw) {
  const choice = (selected, title, text, onPick) => {
    const node = button('', 'agents-choice' + (selected ? ' selected' : ''), onPick);
    node.append(el('b', '', title), el('span', 'agents-sub', text));
    node.setAttribute('aria-pressed', String(selected));
    return node;
  };
  const out = KINDS.map(kind => choice(state.start === 'blank' && state.kind === kind, 'Blank ' + KIND_LABEL[kind].toLowerCase(),
    KIND_BLURB[kind], () => { state.start = 'blank'; state.kind = kind; redraw(); }));
  if (agents.length) {
    out.push(choice(state.start === 'duplicate', 'An existing agent', 'Copy one of your agents and change it.',
      () => { state.start = 'duplicate'; redraw(); }));
  }
  out.push(choice(false, 'A PROFILE.md', 'Import an agent someone shared, or one from a repository.',
    () => importDialog(async id => page.navigate(id ? { view: 'detail', id, tab: 'overview' } : { view: 'index' }))));
  return out;
}

function purposeForm(page, state, agents) {
  const name = textInput('', 'Name', 'What should it be called?');
  const description = textInput('', 'What it’s for', 'One sentence, shown in the list');
  const id = textInput('', 'Agent id');
  let idTouched = false;
  id.oninput = () => { idTouched = true; };
  name.oninput = () => { if (!idTouched) id.value = slug(name.value); };
  const from = selectBox(agents.map(agent => [agent.profile_id, agent.name || agent.profile_id]), state.from, 'Copy from');
  const fromField = field('Copy from', from);
  const status = el('div');
  const create = button('Create draft', 'primary', () => whileBusy([create], () => createDraft(page, state, { id, name, description, from }, status)));
  const node = el('div', 'agents-stack');
  node.append(fromField, field('Name', name), field('What it’s for', description), field('Agent id', id), status,
    row(spacer(), button('Cancel', '', () => page.navigate({ view: 'index' })), create));
  return { node, sync: () => fromField.classList.toggle('hidden', state.start !== 'duplicate') };
}

async function createDraft(page, state, inputs, status) {
  status.replaceChildren();
  const id = inputs.id.value.trim();
  const request = { start: state.start, id, name: inputs.name.value.trim(), description: inputs.description.value.trim(),
    type: state.kind, from_profile_id: state.start === 'duplicate' ? inputs.from.value : '' };
  try {
    await startDraft(request);
    page.navigate({ view: 'detail', id, tab: 'instructions', options: { edit: true } });
  } catch (error) {
    status.replaceChildren(problem(errorText(error, 'Not created')));
  }
}

// slug derives an agent id from a name: lowercase letters, digits and single
// hyphens, within the id pattern's 64 characters.
export function slug(name) {
  return String(name || '').toLowerCase().normalize('NFKD').replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 64).replace(/-+$/, '');
}
