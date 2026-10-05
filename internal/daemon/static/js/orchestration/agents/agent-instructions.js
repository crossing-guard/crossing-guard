// Instructions tab (plan §5, §7): a read view of what runs, and an edit mode
// that changes a draft. Nothing live changes until Publish, and Publish asks
// which places move.
import { el, card, button, row, spacer, problem, errorText, textArea, textInput, field, checkbox, selectBox, linkButton, prose, whileBusy } from './agent-ui.js';
import { saveDraftEdit, saveDraftSource, discardDraft, publishDraft } from './roster-api.js';
import { profileOf, instructionsView, labelFor, outputWords, relativeTime, powerWords } from './roster-model.js';
import { sourceDialog, confirmDialog } from './agent-dialog.js';
import { openMoveDialog } from './agent-move.js';

// Powers a managed agent may ask for (profilefs validates the request).
const MANAGED_POWERS = Object.freeze(['draft-reply', 'reply', 'send-message', 'advise', 'request-interrupt', 'launch-profile']);

export function renderInstructions(pane, ctx, options = {}) {
  // A shared agent's definition is read-only on this device (OD-21).
  const readOnly = Boolean(ctx.origin?.readOnly);
  const editing = !readOnly && (Boolean(options.edit) || (Boolean(ctx.draft) && !ctx.detail.current));
  if (editing) renderEditor(pane, ctx);
  else renderReader(pane, ctx);
}

function renderReader(pane, ctx) {
  const profile = profileOf(ctx.detail);
  const version = ctx.detail.current?.current?.version || profile?.version || '';
  const bar = row(el('span', 'agents-mono', version ? 'v' + version : ''), el('span', 'agents-sub', versionFacts(ctx)), spacer(),
    ctx.draft && !ctx.origin?.readOnly ? button('Open draft', '', () => ctx.openTab('instructions', { edit: true })) : null,
    button('View PROFILE.md', 'ghost', () => sourceDialog('PROFILE.md', ctx.detail.current?.source || ctx.draft?.source || '')),
    ctx.origin?.readOnly ? button('Duplicate\u2026', '', ctx.duplicate) : button('Edit', '', () => ctx.openTab('instructions', { edit: true })));
  pane.appendChild(bar);
  if (!profile) {
    pane.appendChild(problem('Its definition cannot be read.'));
    return;
  }
  const grid = el('div', 'agents-cols');
  grid.append(readInstructions(profile, ctx), readFacts(profile, ctx));
  pane.appendChild(grid);
}

function versionFacts(ctx) {
  const selected = ctx.detail.current?.current?.selected_at;
  const live = (ctx.agent.places || []).filter(place => place.is_current && place.state === 'enabled').map(place => place.repository || 'every repository');
  return [selected ? (ctx.origin ? 'adopted ' : 'published ') + relativeTime(Date.parse(selected) / 1000) : 'not published',
    live.length ? 'on in ' + live.join(', ') : ''].filter(Boolean).join(' · ');
}

function readInstructions(profile, ctx) {
  const view = instructionsView(profile);
  const column = el('div', 'agents-stack');
  if (view.staged) {
    for (const item of view.prompts) column.appendChild(card('When: ' + labelFor(ctx.detail.signals, item.signal), prose(item.prompt)));
    column.appendChild(card('Markdown body', el('div', 'agents-sub', 'Not sent — each moment above has its own prompt.'),
      prose(view.body, 'agents-muted')));
  } else {
    column.appendChild(card('Instructions', prose(view.body)));
  }
  if (view.replyShape) column.appendChild(card('A good reply looks like', prose(view.replyShape)));
  return column;
}

function readFacts(profile, ctx) {
  const column = el('div', 'agents-stack');
  column.appendChild(card('When it runs', el('div', '', ctx.detail.trigger_label || labelFor(ctx.detail.signals, profile.trigger?.event))));
  const reads = el('div', 'agents-kv');
  for (const item of profile.context || []) {
    reads.append(el('span', '', labelFor(ctx.detail.context_kinds, item.kind)),
      el('span', 'agents-sub', (item.required ? 'required' : 'if available') + (item.max_bytes ? ' · up to ' + Math.round(item.max_bytes / 1024) + ' KB' : '')));
  }
  column.appendChild(card('What it reads', reads));
  const returns = el('div');
  returns.appendChild(el('div', '', outputWords(profile.output?.kind)));
  for (const name of profile.authority_requests || []) returns.appendChild(el('div', 'agents-sub', 'Asks permission to: ' + powerWords(name)));
  if ((profile.may_tag || []).length) returns.appendChild(el('div', 'agents-sub', 'Tags it may apply: ' + profile.may_tag.join(', ')));
  column.appendChild(card('What it can do', returns));
  return column;
}

/* ---------- edit mode ---------- */

function renderEditor(pane, ctx) {
  const draft = ctx.draft;
  if (draft?.problem) {
    const form = sourceForm(ctx);
    pane.append(draftBar(ctx, form), form.node);
    return;
  }
  const profile = draft?.normalized || profileOf(ctx.detail) || {};
  const reviewer = ctx.kind === 'reviewer';
  const form = editorForm(profile, ctx, reviewer);
  form.save = () => saveEdits(ctx, form);
  pane.append(draftBar(ctx, form), form.node);
}

// sourceForm edits a draft that does not parse as its PROFILE.md text: the
// fields cannot be read back from it, so the text is what gets fixed.
function sourceForm(ctx) {
  const area = textArea(ctx.draft.source || '', 'PROFILE.md', 24);
  area.classList.add('agents-source');
  const status = el('div', 'agents-sub', '');
  const node = el('div', 'agents-editor');
  node.append(field('PROFILE.md', area));
  const save = async () => {
    try {
      await saveDraftSource(ctx.id, ctx.detail.draft_token, ctx.detail.selection_token, area.value);
      await ctx.reload('instructions', { edit: true });
    } catch (error) {
      status.replaceChildren(problem(errorText(error, 'Not saved')));
    }
  };
  return { node, status, save };
}

function draftBar(ctx, form) {
  const draft = ctx.draft;
  const words = draft
    ? (draft.normalized?.version ? 'Draft v' + draft.normalized.version : 'Draft') + ' · saved ' + relativeTime(Date.parse(draft.updated_at) / 1000)
      + (draft.problem ? ' · needs a fix before it can publish' : ' · not running anywhere')
    : (ctx.profile?.version ? 'Editing v' + ctx.profile.version : 'New draft');
  const saveButton = button('Save draft', '', () => whileBusy([saveButton], () => form.save()));
  const bar = row(el('b', '', words), spacer(), saveButton,
    draft ? button('Discard', 'ghost', () => confirmDialog('Discard draft', 'The draft is deleted; published versions stay.', 'Discard',
      async () => { await discardDraft(ctx.id, ctx.detail.draft_token); await ctx.reload('instructions'); })) : null,
    draft && !draft.problem ? button('Publish…', 'primary', () => publishFlow(ctx)) : null,
    button('Close', 'ghost', () => ctx.openTab('instructions')));
  bar.classList.add('agents-draftbar');
  const node = el('div');
  node.appendChild(bar);
  if (draft?.problem) node.appendChild(problem((draft.problem.field ? draft.problem.field + ': ' : '') + draft.problem.message + ' ' + (draft.problem.recovery || '')));
  node.appendChild(form.status);
  return node;
}

function editorForm(profile, ctx, reviewer) {
  const view = instructionsView(profile);
  const inputs = {
    name: textInput(profile.name, 'Name'), description: textInput(profile.description, 'Description'),
    version: textInput(profile.version, 'Version'), body: textArea(view.body, 'Instructions', 14),
    replyShape: textArea(view.replyShape, 'A good reply looks like', 3),
  };
  const node = el('div', 'agents-editor');
  node.append(row(field('Name', inputs.name), field('Version', inputs.version)), field('What it’s for', inputs.description));
  const stages = reviewer ? null : stageEditor(view, ctx);
  if (stages) node.appendChild(stages.node);
  node.appendChild(field(view.staged ? 'Markdown body (not sent while moment prompts exist)' : 'Instructions', inputs.body));
  const extra = reviewer ? null : managedFields(profile, ctx, inputs);
  if (extra) node.appendChild(extra);
  return { node, inputs, stages, profile, reviewer, status: el('div', 'agents-sub', '') };
}

function stageEditor(view, ctx) {
  const node = el('div', 'agents-stages');
  const entries = view.prompts.map(item => ({ signal: item.signal, area: textArea(item.prompt, 'Prompt for ' + item.signal, 6) }));
  const redraw = () => drawStages(node, entries, ctx, redraw);
  redraw();
  return { node, entries };
}

// drawStages renders one editor per moment prompt, a remove link each, and a
// picker for moments the agent has no prompt for yet.
function drawStages(node, entries, ctx, redraw) {
  node.replaceChildren(el('div', 'agents-field-label', 'Prompts for specific moments'));
  for (const entry of entries) {
    node.append(field(labelFor(ctx.detail.signals, entry.signal), entry.area),
      linkButton('Remove this moment', () => { entries.splice(entries.indexOf(entry), 1); redraw(); }));
  }
  const unused = (ctx.detail.signals || []).filter(signal => signal.served && !entries.some(entry => entry.signal === signal.kind));
  if (!unused.length) return;
  const picker = selectBox([['', 'Add a prompt for a moment…'], ...unused.map(signal => [signal.kind, signal.label || signal.kind])], '', 'Add a moment');
  picker.onchange = () => addStage(entries, picker.value, redraw);
  node.appendChild(picker);
}

function addStage(entries, signal, redraw) {
  if (!signal) return;
  entries.push({ signal, area: textArea('', 'Prompt', 6) });
  redraw();
}

function managedFields(profile, ctx, inputs) {
  const node = el('div', 'agents-stack');
  node.appendChild(field('A good reply looks like', inputs.replyShape));
  const served = (ctx.detail.signals || []).filter(signal => signal.served);
  inputs.trigger = selectBox(served.map(signal => [signal.kind, signal.label || signal.kind]), profile.trigger?.event, 'When it runs');
  node.appendChild(field('When it runs', inputs.trigger));
  inputs.context = (ctx.detail.context_kinds || []).map(option => {
    const current = (profile.context || []).find(item => item.kind === option.kind);
    const box = checkbox(Boolean(current), option.label);
    const required = checkbox(Boolean(current?.required), 'required');
    return { kind: option.kind, box, required, maxBytes: current?.max_bytes || 0 };
  });
  const reads = el('div', 'agents-checks');
  reads.append(...inputs.context.map(item => row(item.box.node, item.required.node)));
  node.appendChild(field('What it reads', reads));
  inputs.powers = MANAGED_POWERS.map(name => ({ name, box: checkbox((profile.authority_requests || []).includes(name), powerWords(name)) }));
  const powers = el('div', 'agents-checks');
  powers.append(...inputs.powers.map(item => item.box.node));
  node.appendChild(field('Asks permission to', powers));
  inputs.tags = textInput((profile.may_tag || []).join(', '), 'Tags it may apply', 'comma separated');
  node.appendChild(field('Tags it may apply', inputs.tags));
  return node;
}

// collectEdit sends only the fields that changed from the definition shown.
function collectEdit(form) {
  const { inputs, profile, stages, reviewer } = form;
  const edit = {};
  const setIf = (key, value, before) => { if (value !== before) edit[key] = value; };
  setIf('name', inputs.name.value.trim(), profile.name || '');
  setIf('description', inputs.description.value.trim(), profile.description || '');
  setIf('version', inputs.version.value.trim(), profile.version || '');
  setIf('instructions', inputs.body.value, String(profile.instructions || ''));
  if (reviewer) return edit;
  setIf('reply_shape', inputs.replyShape.value.trim(), String(profile.reply_shape || ''));
  setIf('trigger_event', inputs.trigger.value, profile.trigger?.event || '');
  const stagesNow = Object.fromEntries(stages.entries.map(entry => [entry.signal, entry.area.value]).filter(([, prompt]) => prompt.trim()));
  if (JSON.stringify(stagesNow) !== JSON.stringify(Object.fromEntries(Object.entries(profile.stages || {}).map(([key, value]) => [key, String(value)])))) edit.stages = stagesNow;
  const context = inputs.context.filter(item => item.box.input.checked)
    .map(item => ({ kind: item.kind, required: item.required.input.checked, max_bytes: item.maxBytes }));
  const contextBefore = (profile.context || []).map(item => ({ kind: item.kind, required: Boolean(item.required), max_bytes: item.max_bytes || 0 }));
  if (JSON.stringify(context) !== JSON.stringify(contextBefore)) edit.context = context;
  const powers = inputs.powers.filter(item => item.box.input.checked).map(item => item.name);
  if (powers.join(',') !== [...(profile.authority_requests || [])].join(',')) edit.authority_requests = powers;
  const tags = inputs.tags.value.split(',').map(tag => tag.trim()).filter(Boolean);
  if (tags.join(',') !== (profile.may_tag || []).join(',')) edit.may_tag = tags;
  return edit;
}

async function saveEdits(ctx, form) {
  const edit = collectEdit(form);
  if (!Object.keys(edit).length) {
    form.status.textContent = 'No changes.';
    return;
  }
  try {
    await saveDraftEdit(ctx.id, ctx.detail.draft_token, ctx.detail.selection_token, edit);
    await ctx.reload('instructions', { edit: true });
  } catch (error) {
    form.status.replaceChildren(problem(errorText(error, 'Not saved')));
  }
}

function publishFlow(ctx) {
  const draft = ctx.draft;
  const target = { version: draft.normalized?.version || '', source_digest: draft.source_digest,
    bundle_digest: draft.bundle_digest, normalized: draft.normalized };
  const current = ctx.detail.current?.current;
  openMoveDialog(ctx, {
    title: 'Publish version ' + target.version, target, confirmLabel: 'Publish',
    intro: el('div', 'agents-sub', current ? 'Replaces v' + current.version + ' as the current version.' : 'The first version of this agent.'),
    preselect: place => place.state === 'enabled' && place.is_current,
    prepare: () => publishDraft(ctx.id, ctx.detail.draft_token, ctx.detail.selection_token),
    nextTab: (ctx.agent.places || []).length ? 'instructions' : 'places',
  });
}
