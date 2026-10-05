// The send sheet (team rest-of-release plan §6.1, §14 Q2, Q14–Q16; criterion
// 62). The person picks who it goes to, edits the title, the remaining list and
// the text, and reads "what will be sent" — the daemon's own preview, after its
// checks — before Send. Send is offered only while the form still equals the
// form that preview was taken of, and the real send repeats that preview.
import { api, debounce } from '../core.js';
import { el, button, field, textInput, textArea, checkbox, selectBox, row } from '../orchestration/agents/agent-ui.js';
import { openDialog } from '../orchestration/agents/agent-dialog.js';
import { loadDraft, loadMembers, sendHandoff } from './handoff-api.js';
import { recipientChoices, noRecipientWords, draftForm, formRequest, formKey, formProblem, willBeSent, sheetSentences, sendRefusal } from './handoff-model.js';

// How long the form rests before "what will be sent" is asked for again.
const PREVIEW_REST_MS = 450;

// openSendSheet opens the sheet for one session.
//   session  { runtime, id, title } — id is the session's catalog id
//   local    a same-device handoff: no recipient, nothing leaves (§6.6)
//   target   the runtime's name a same-device handoff continues in
//   prefill  the kept text of an earlier handoff ("Send again…")
//   liveTurns turns sent from this console that the extract cannot hold yet
//   onSent(result) runs after a send the daemon accepted
export async function openSendSheet({ session, local = false, target = '', prefill = null, liveTurns = 0, onSent = () => {} }) {
  const body = el('div', 'handoff-sheet');
  body.appendChild(el('div', 'agents-sub', 'Reading the session…'));
  const sheet = { session, local, target, liveTurns, onSent, body, form: null, preview: null, previewKey: '', members: [], turns: 0 };
  sheet.dialog = openDialog({ title: local ? 'Continue in ' + (target || 'another runtime') : 'Hand off', body, wide: true, actions: [
    { label: 'Cancel', onClick: dialog => dialog.close() },
    { label: local ? 'Continue' : 'Send', primary: true, onClick: () => send(sheet) },
  ] });
  syncSend(sheet);
  try {
    await loadSheet(sheet, prefill);
  } catch (error) {
    body.replaceChildren(el('div', 'banner agents-problem', 'The handoff could not be prepared: ' + (error.message || error)));
    return;
  }
  drawSheet(sheet);
}

async function loadSheet(sheet, prefill) {
  const [draft, members, config] = await Promise.all([
    prefill ? Promise.resolve(null) : loadDraft(sheet.session),
    sheet.local ? Promise.resolve({ linked: true, members: [] }) : loadMembers(),
    api('/api/console/config').catch(() => null),
  ]);
  sheet.form = prefill ? { ...draftForm({}), ...prefill } : draftForm(draft);
  sheet.directory = members;
  sheet.members = recipientChoices(members.members || []);
  sheet.turns = Number(config?.config?.handoff?.conversation_turns) || 0;
}

// drawSheet builds the form once; later changes repaint only "what will be sent".
function drawSheet(sheet) {
  const { body, form, local } = sheet;
  body.replaceChildren();
  if (!local && !sheet.members.length) {
    body.appendChild(field('To', el('div', 'agents-empty', noRecipientWords(sheet.directory || {}))));
    // Nothing can be sent: the sheet only closes.
    const primary = sheet.dialog.primaryButton();
    primary.previousElementSibling.textContent = 'Close';
    primary.remove();
    return;
  }
  const changed = () => formChanged(sheet);
  if (!local) body.appendChild(recipientField(sheet, changed));
  const title = textInput(form.title, 'Title');
  title.oninput = () => { form.title = title.value; changed(); };
  body.appendChild(field('Title', title));
  body.appendChild(remainingField(form, changed));
  const text = textArea(form.text, 'Text', 9);
  text.oninput = () => { form.text = text.value; changed(); };
  body.appendChild(field('Text', text));
  if (sheet.liveTurns > 0) {
    body.appendChild(el('div', 'agents-sub', sheet.liveTurns + (sheet.liveTurns === 1 ? ' turn' : ' turns')
      + ' sent from this console since the session was read are not in this text. Add what you still need.'));
  }
  const excerpt = checkbox(form.includeConversation, 'Include the last ' + (sheet.turns ? sheet.turns + ' ' : '') + 'turns');
  excerpt.input.onchange = () => { form.includeConversation = excerpt.input.checked; changed(); };
  body.appendChild(excerpt.node);
  sheet.sentHost = body.appendChild(el('div', 'handoff-sent'));
  for (const sentence of sheetSentences({ local, recipient: recipientName(sheet) })) body.appendChild(el('div', 'agents-sub handoff-sentence', sentence));
  sheet.sentences = [...body.querySelectorAll('.handoff-sentence')];
  sheet.refresh = debounce(() => { void preview(sheet); }, PREVIEW_REST_MS);
  void preview(sheet);
}

function recipientName(sheet) {
  const chosen = (sheet.directory?.members || []).find(member => member.user_id === sheet.form.to);
  return chosen?.self ? '' : String(chosen?.display_name || '');
}

function recipientField(sheet, changed) {
  const select = selectBox([['', 'Choose a teammate…'], ...sheet.members.map(choice => [choice.value, choice.label])], sheet.form.to, 'To');
  select.onchange = () => {
    sheet.form.to = select.value;
    const sentences = sheetSentences({ recipient: recipientName(sheet) });
    sheet.sentences.forEach((node, index) => { node.textContent = sentences[index] || ''; });
    changed();
  };
  return field('To', select);
}

// remainingField is the editable list of what is left: one input per item.
function remainingField(form, changed) {
  const wrap = el('div', 'agents-field');
  wrap.appendChild(el('span', 'agents-field-label', 'What remains'));
  const list = wrap.appendChild(el('div', 'handoff-remaining'));
  const paint = () => {
    list.replaceChildren();
    form.remaining.forEach((item, index) => {
      // A field that wraps and grows with its text: a one-line input cut a long item.
      const input = textArea(item, 'Remaining item ' + (index + 1), 1);
      input.className = 'handoff-remaining-item';
      const fit = () => { input.style.height = 'auto'; input.style.height = input.scrollHeight + 'px'; };
      input.oninput = () => { form.remaining[index] = input.value; fit(); changed(); };
      list.appendChild(row(input, button('Remove', 'ghost', () => { form.remaining.splice(index, 1); paint(); changed(); })));
      requestAnimationFrame(fit);
    });
    list.appendChild(row(button('Add item', '', () => {
      form.remaining.push('');
      paint();
      list.querySelectorAll('.handoff-remaining-item')[form.remaining.length - 1]?.focus();
    })));
  };
  paint();
  return wrap;
}

// formChanged: what was previewed is no longer what would be sent.
function formChanged(sheet) {
  syncSend(sheet);
  sheet.dialog.showProblem('');
  if (sheet.sentHost) sheet.sentHost.classList.add('handoff-sent-stale');
  sheet.refresh?.();
}

const fresh = sheet => Boolean(sheet.preview) && sheet.previewKey === formKey(sheet.form, sheet.local);

// syncSend offers Send only from a fresh preview.
function syncSend(sheet) {
  const primary = sheet.dialog?.primaryButton();
  if (primary) primary.disabled = !fresh(sheet);
}

async function preview(sheet) {
  const problem = formProblem(sheet.form, sheet.local);
  if (problem) {
    sheet.preview = null;
    sheet.sentHost.replaceChildren(el('div', 'agents-sub', problem));
    syncSend(sheet);
    return;
  }
  const key = formKey(sheet.form, sheet.local);
  try {
    const answer = await sendHandoff(formRequest(sheet.form, sheet.session, { local: sheet.local }));
    if (key !== formKey(sheet.form, sheet.local)) return; // the form moved on while this was read
    sheet.preview = answer;
    sheet.previewKey = key;
    sheet.sentHost.classList.remove('handoff-sent-stale');
    sheet.sentHost.replaceChildren(sentNode(willBeSent(answer)));
  } catch (error) {
    sheet.preview = null;
    sheet.sentHost.replaceChildren(el('div', 'banner agents-problem', sendRefusal(error, recipientName(sheet))));
  }
  syncSend(sheet);
}

// sentNode draws "what will be sent" as readable fields, one fact per row.
function sentNode(model) {
  const box = document.createElement('details');
  box.className = 'handoff-sent-box';
  box.open = true;
  box.appendChild(el('summary', '', 'What will be sent'));
  const grid = box.appendChild(el('div', 'handoff-facts'));
  const add = (label, value) => grid.append(el('span', 'handoff-facts-k', label), value instanceof Node ? value : el('span', '', value));
  add('Size', model.bytes);
  add('Title', model.title);
  if (model.remaining.length) {
    const list = el('ol', 'handoff-list');
    model.remaining.forEach(item => list.appendChild(el('li', '', item)));
    add('What remains', list);
  }
  add('Text', el('pre', 'agents-source handoff-text', model.text));
  if (model.agents.length) add('Agents', linesNode(model.agents));
  add('Session tags', model.sessionTags.length ? factsNode(model.sessionTags) : 'None');
  if (model.session.length) add('Session', factsNode(model.session));
  if (model.excerpt) add('Excerpt', excerptNode(model.excerpt));
  for (const check of model.checks) add(check.label, el('span', check.changed ? 'handoff-replaced' : '', check.value));
  return box;
}

// factsNode is a label/value grid inside one row's value: one fact per line.
function factsNode(rows) {
  const facts = el('div', 'handoff-facts handoff-facts-inner handoff-facts-wide');
  for (const fact of rows) facts.append(el('span', 'handoff-facts-k', fact.label), el('span', '', fact.value));
  return facts;
}

// linesNode is a list of plain values, one per line.
function linesNode(values) {
  const list = el('ul', 'handoff-list handoff-list-plain');
  values.forEach(value => list.appendChild(el('li', '', value)));
  return list;
}

// excerptNode is the excerpt folded to its three facts; it opens to every turn
// exactly as it will leave.
function excerptNode(excerpt) {
  const wrap = el('div', 'handoff-excerpt');
  wrap.appendChild(factsNode(excerpt.facts)).classList.remove('handoff-facts-wide');
  const turns = wrap.appendChild(document.createElement('details'));
  turns.appendChild(el('summary', '', 'Read every turn'));
  for (const turn of excerpt.turns) {
    const item = turns.appendChild(el('div', 'handoff-turn'));
    item.append(el('span', 'handoff-turn-role', turn.seq + ' ' + turn.role), el('pre', 'handoff-turn-text', turn.text));
  }
  return wrap;
}

async function send(sheet) {
  // The dialog re-enables its buttons after any click; Send goes back to what
  // the preview allows once this returns.
  setTimeout(() => syncSend(sheet), 0);
  if (!fresh(sheet)) return;
  try {
    const result = await sendHandoff(formRequest(sheet.form, sheet.session, { local: sheet.local, previewed: sheet.preview }));
    sheet.dialog.close();
    document.dispatchEvent(new CustomEvent('cg:handoffs-changed', { detail: { id: result.id } }));
    await sheet.onSent(result);
  } catch (error) {
    sheet.dialog.showProblem(sendRefusal(error, recipientName(sheet)));
    // The session moved since the preview: what is shown is no longer what
    // would leave, so it is read again before Send is offered.
    if (error.code === 'preview_mismatch') { sheet.preview = null; void preview(sheet); }
    setTimeout(() => syncSend(sheet), 0);
  }
}
