import { el } from '../core.js';

// A held call can be asking its approver a question. When it is, the card offers the
// options themselves instead of a wall of JSON: the approver picks, and the pick
// travels back as part of the allow. Nothing here knows which runtime asked.

export function promptsOf(approval) {
  return Array.isArray(approval.prompts) ? approval.prompts : [];
}

export function promptsWereDropped(approval) {
  return approval.prompts_completeness === 'truncated';
}

// groupName keeps one surface's radio group from capturing another's. The same
// approval can be on screen twice (the interrupt bar and the approvals view), and
// two groups sharing a name would move together.
function groupName(surface, approvalID, promptID) {
  return 'ap-choice-' + surface + '-' + approvalID + '-' + promptID;
}

function optionRow(name, multi, label, description, checked, onPick) {
  const row = el('label', 'ap-option');
  const input = el('input');
  input.type = multi ? 'checkbox' : 'radio';
  input.name = name;
  input.value = label;
  input.checked = checked;
  input.addEventListener('change', onPick);
  const text = el('span');
  text.appendChild(el('span', 'ap-option-label', label));
  if (description) text.appendChild(el('div', 'sub', description));
  row.append(input, text);
  return row;
}

function freeTextRow(name, multi, value, onPick) {
  const row = el('label', 'ap-option');
  const input = el('input');
  input.type = multi ? 'checkbox' : 'radio';
  input.name = name;
  input.dataset.freeText = 'true';
  input.checked = Boolean(value);
  const typed = el('input', 'ap-free-text');
  typed.type = 'text';
  typed.placeholder = 'Other — type your own answer';
  typed.setAttribute('aria-label', 'Other answer');
  typed.value = value || '';
  input.addEventListener('change', onPick);
  typed.addEventListener('input', () => { input.checked = Boolean(typed.value.trim()); onPick(); });
  row.append(input, typed);
  return row;
}

function promptFieldset(approval, prompt, surface, chosen, onPick) {
  const box = el('fieldset', 'ap-prompt');
  box.dataset.promptId = prompt.id;
  box.dataset.multi = prompt.multi ? 'true' : 'false';
  const legend = el('legend');
  if (prompt.header) legend.appendChild(el('span', 'chip st-draft', prompt.header));
  legend.appendChild(el('span', 'ap-prompt-text', prompt.text));
  box.appendChild(legend);
  const name = groupName(surface, approval.id, prompt.id);
  const offered = new Set((prompt.options || []).map(option => option.label));
  for (const option of prompt.options || []) {
    box.appendChild(optionRow(name, prompt.multi, option.label, option.description,
      chosen.includes(option.label), onPick));
  }
  if (prompt.free_text) {
    const typed = chosen.find(value => !offered.has(value)) || '';
    box.appendChild(freeTextRow(name, prompt.multi, typed, onPick));
  }
  box.appendChild(el('div', 'sub', prompt.multi ? 'Choose one or more' : 'Choose one'));
  return box;
}

// readSelections reports what is on screen right now, in the shape the approval
// owner validates. An unanswered prompt is simply absent, which is what makes the
// answer incomplete and keeps Allow disabled.
export function readSelections(host) {
  const selections = [];
  for (const box of host.querySelectorAll('.ap-prompt')) {
    const values = [];
    for (const input of box.querySelectorAll('input[type=radio], input[type=checkbox]')) {
      if (!input.checked) continue;
      if (input.dataset.freeText === 'true') {
        const typed = input.parentElement.querySelector('.ap-free-text');
        if (typed && typed.value.trim()) values.push(typed.value.trim());
        continue;
      }
      values.push(input.value);
    }
    if (values.length) selections.push({ prompt_id: box.dataset.promptId, values });
  }
  return selections;
}

export function selectionsAreComplete(approval, selections) {
  const prompts = promptsOf(approval);
  if (!prompts.length) return true;
  return prompts.every(prompt => selections.some(
    selection => selection.prompt_id === prompt.id && selection.values.length));
}

// appendPromptFieldsets renders the questions and returns a reader for them, or null
// when this approval is not asking anything. A caller that gets null keeps today's
// plain allow-or-deny card exactly as it was.
export function appendPromptFieldsets(card, approval, surface, onPick) {
  const prompts = promptsOf(approval);
  if (!prompts.length) return null;
  const host = el('div', 'ap-prompts');
  const chosenFor = promptID => {
    const existing = (onPick.initial || []).find(selection => selection.prompt_id === promptID);
    return existing ? existing.values : [];
  };
  for (const prompt of prompts) {
    host.appendChild(promptFieldset(approval, prompt, surface, chosenFor(prompt.id), onPick.changed));
  }
  card.appendChild(host);
  return () => readSelections(host);
}
