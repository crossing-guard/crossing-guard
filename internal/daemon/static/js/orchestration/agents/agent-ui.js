// Small DOM builders shared by the Agents pages. Typed nodes only — never raw
// HTML — so authored text (names, prompts, messages) can never become markup.
import { el } from '../../core.js';

export { el };

import { proseBlocks } from './roster-model.js';

export function button(label, className, onClick) {
  const node = el('button', 'btn' + (className ? ' ' + className : ''), label);
  node.type = 'button';
  if (onClick) node.onclick = onClick;
  return node;
}

export function linkButton(label, onClick) {
  const node = el('button', 'agents-link', label);
  node.type = 'button';
  node.onclick = onClick;
  return node;
}

export function chip(text, className = '') {
  return el('span', 'chip' + (className ? ' ' + className : ''), text);
}

const KIND_CHIP = Object.freeze({ reviewer: 'agents-kind-reviewer', follower: 'agents-kind-follower', helper: 'agents-kind-helper' });

export function kindChip(kind, label) {
  return chip(label, KIND_CHIP[kind] || '');
}

// stateLabel renders a dot plus words; the dot's class carries the state.
export function stateLabel(state, words) {
  return el('span', 'agents-state agents-state-' + state, words);
}

export function card(title, ...children) {
  const node = el('section', 'agents-card');
  if (title) node.appendChild(el('h3', 'agents-card-title', title));
  node.append(...children.filter(Boolean));
  return node;
}

// sequence hands out tickets; only the newest ticket is current, so a slow
// response for an older selection is dropped instead of painted.
export function sequence() {
  let latest = 0;
  return () => {
    const mine = ++latest;
    return () => mine === latest;
  };
}

// whileBusy disables the buttons for the length of one write, so a second
// click cannot send a second write with the token the first one used.
export async function whileBusy(nodes, work) {
  const buttons = nodes.filter(Boolean);
  for (const node of buttons) node.disabled = true;
  try {
    return await work();
  } finally {
    for (const node of buttons) node.disabled = false;
  }
}

// cardAction puts a link on the right of a card's title.
export function cardAction(node, label, onClick) {
  const title = node.querySelector(':scope > .agents-card-title');
  const head = el('div', 'agents-card-head');
  title.replaceWith(head);
  head.append(title, linkButton(label, onClick));
  return node;
}

// prose renders instruction text as paragraphs (see proseBlocks).
export function prose(text, className) {
  const node = el('div', 'agents-prose' + (className ? ' ' + className : ''));
  for (const block of proseBlocks(text)) node.appendChild(el(block.keep ? 'pre' : 'p', block.keep ? 'agents-prose-keep' : '', block.text));
  return node;
}

export function row(...children) {
  const node = el('div', 'agents-row');
  node.append(...children.filter(Boolean));
  return node;
}

export function spacer() {
  return el('span', 'agents-spacer');
}

export function problem(text) {
  const node = el('div', 'banner agents-problem', text);
  node.setAttribute('role', 'alert');
  return node;
}

export function errorText(error, prefix = '') {
  const message = String(error?.message || error || 'Unknown error');
  const recovery = error?.recovery ? ' ' + String(error.recovery) : '';
  return (prefix ? prefix + ': ' : '') + message + recovery;
}

// keyValue builds a two-column facts grid from [label, value|Node] pairs.
export function keyValue(pairs) {
  const grid = el('div', 'agents-kv');
  for (const [label, value] of pairs) {
    if (value == null || value === '') continue;
    grid.append(el('span', 'agents-kv-k', label), typeof value === 'string' ? el('span', '', value) : value);
  }
  return grid;
}

export function sparkline(values = []) {
  const max = Math.max(1, ...values);
  const node = el('span', 'agents-spark');
  node.setAttribute('aria-hidden', 'true');
  for (const value of values) {
    const bar = el('i', value ? '' : 'agents-spark-zero');
    bar.style.height = (value ? Math.max(2, Math.round(value / max * 18)) : 1) + 'px';
    node.appendChild(bar);
  }
  return node;
}

export function selectBox(options, value, label) {
  const select = document.createElement('select');
  if (label) select.setAttribute('aria-label', label);
  for (const [optionValue, optionLabel] of options) {
    const option = document.createElement('option');
    option.value = optionValue;
    option.textContent = optionLabel;
    select.appendChild(option);
  }
  if (value != null) select.value = value;
  return select;
}

export function checkbox(checked, label) {
  const input = document.createElement('input');
  input.type = 'checkbox';
  input.checked = Boolean(checked);
  const wrapper = el('label', 'agents-check');
  wrapper.append(input, el('span', '', label));
  return { input, node: wrapper };
}

export function textInput(value, label, placeholder = '') {
  const input = document.createElement('input');
  input.type = 'text';
  input.value = value || '';
  input.placeholder = placeholder;
  if (label) input.setAttribute('aria-label', label);
  return input;
}

export function textArea(value, label, rows = 8) {
  const area = document.createElement('textarea');
  area.value = value || '';
  area.rows = rows;
  if (label) area.setAttribute('aria-label', label);
  return area;
}

export function field(label, control) {
  const wrapper = el('label', 'agents-field');
  wrapper.append(el('span', 'agents-field-label', label), control);
  return wrapper;
}
