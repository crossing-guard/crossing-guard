// Rename a tag on every session at once, or remove it everywhere. Rare and, for
// removal, final — so it sits behind its own menu and asks once.
import { el } from '../core.js';
import { loadTagVocabulary, purgeSessionTag, renameSessionTag } from './organization-api.js';
import { parseTagText, tagLabel } from './tag-chips.js';
import { closeTagPopover, place } from './tag-popover.js';

let manager = null;

function closeTagManager(restoreFocus = false) {
  if (!manager) return;
  document.removeEventListener('pointerdown', manager.outside, true);
  manager.root.remove();
  const anchor = manager.anchor;
  manager = null;
  if (restoreFocus && anchor?.isConnected) anchor.focus();
}

export async function openTagManager(anchor, onChanged) {
  closeTagPopover();
  closeTagManager();
  const root = el('div', 'pane-menu tag-pop tag-manage');
  root.setAttribute('role', 'dialog');
  root.setAttribute('aria-label', 'Manage tags');
  const problem = el('div', 'tag-pop-problem');
  problem.setAttribute('role', 'status');
  const list = el('div', 'tag-pop-list');
  root.append(el('div', 'pane-menu-section', 'Your tags'), list, problem);
  place(root, anchor);
  const outside = event => { if (!root.contains(event.target)) closeTagManager(true); };
  document.addEventListener('pointerdown', outside, true);
  manager = { root, anchor, outside };
  root.addEventListener('keydown', event => {
    if (event.key !== 'Escape') return;
    event.preventDefault();
    event.stopPropagation();
    closeTagManager(true);
  });
  const state = { list, problem, onChanged };
  await repaint(state);
  list.querySelector('button')?.focus();
}

async function repaint(state) {
  let tags = [];
  try { tags = (await loadTagVocabulary()).tags || []; } catch (err) { state.problem.textContent = err.message || String(err); }
  state.list.replaceChildren();
  if (!tags.length) state.list.appendChild(el('div', 'empty', 'You have not tagged anything yet.'));
  for (const tag of tags) state.list.appendChild(manageRow(state, tag));
}

function manageRow(state, tag) {
  const row = el('div', 'tag-manage-row');
  row.appendChild(el('span', 'tag-manage-name', tagLabel(tag)));
  row.appendChild(el('span', 'pane-menu-hint', String(tag.sessions)));
  const rename = el('button', 'btn', 'Rename');
  rename.type = 'button';
  rename.onclick = () => beginRename(state, row, tag);
  const remove = el('button', 'btn', 'Remove everywhere');
  remove.type = 'button';
  remove.onclick = () => confirmRemove(state, row, tag);
  row.append(rename, remove);
  return row;
}

function beginRename(state, row, tag) {
  const input = el('input', 'tag-pop-input');
  input.value = tagLabel(tag);
  input.setAttribute('aria-label', 'New name for ' + tagLabel(tag));
  const save = el('button', 'btn primary', 'Rename');
  save.type = 'button';
  const apply = () => act(state, () => renameSessionTag(plain(tag), plain(parseTagText(input.value))));
  save.onclick = apply;
  input.addEventListener('keydown', event => { if (event.key === 'Enter') { event.preventDefault(); apply(); } });
  row.replaceChildren(input, save);
  input.focus();
  input.select();
}

function confirmRemove(state, row, tag) {
  const question = el('span', 'tag-manage-name', 'Remove “' + tagLabel(tag) + '” from ' + tag.sessions + ' session' + (tag.sessions === 1 ? '' : 's') + '?');
  const yes = el('button', 'btn', 'Remove');
  yes.type = 'button';
  yes.onclick = () => act(state, () => purgeSessionTag(plain(tag)));
  const no = el('button', 'btn', 'Keep');
  no.type = 'button';
  no.onclick = () => repaint(state);
  row.replaceChildren(question, yes, no);
  no.focus();
}

const plain = tag => ({ key: tag.key || undefined, value: tag.value });

async function act(state, change) {
  state.problem.textContent = '';
  try {
    await change();
    state.onChanged?.();
  } catch (err) {
    state.problem.textContent = err.message || String(err);
  }
  await repaint(state);
}
