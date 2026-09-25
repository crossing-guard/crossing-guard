// The one place tags are edited. Type anything and press Enter: that is how a
// tag comes to exist. Suggestions are the owner's own tags, most recently used
// first — nothing here is offered that he did not write.
import { el } from '../core.js';
import { changeSessionTags, loadTagVocabulary } from './organization-api.js';
import { isOwnerTag, parseTagText, sameTag, tagChip, tagLabel } from './tag-chips.js';
import { openTagManager } from './tag-manage.js';

let active = null;

export function closeTagPopover(restoreFocus = false) {
  if (!active) return;
  const { root, anchor, outside } = active;
  document.removeEventListener('pointerdown', outside, true);
  root.remove();
  active = null;
  if (restoreFocus && anchor?.isConnected) anchor.focus();
}

// openTagPopover edits the tags of one session, or applies tags to several.
// onChanged receives the sessions as the daemon now describes them.
export function openTagPopover(anchor, sessions, onChanged) {
  closeTagPopover();
  const state = { sessions, onChanged, vocabulary: [], cursor: 0, busy: false };
  const root = el('div', 'pane-menu tag-pop');
  root.setAttribute('role', 'dialog');
  root.setAttribute('aria-label', sessions.length > 1 ? 'Tag ' + sessions.length + ' sessions' : 'Tags');
  state.current = el('div', 'tag-pop-current');
  state.input = el('input', 'tag-pop-input');
  state.input.placeholder = sessions.length > 1 ? 'Tag ' + sessions.length + ' sessions…' : 'Add a tag…';
  state.input.setAttribute('aria-label', state.input.placeholder);
  state.input.autocomplete = 'off';
  state.input.spellcheck = false;
  state.list = el('div', 'tag-pop-list');
  state.list.id = 'tag-pop-list';
  state.list.setAttribute('role', 'listbox');
  state.input.setAttribute('role', 'combobox');
  state.input.setAttribute('aria-controls', 'tag-pop-list');
  state.input.setAttribute('aria-expanded', 'true');
  state.problem = el('div', 'tag-pop-problem');
  state.problem.setAttribute('role', 'status');
  const manage = el('button', 'pane-menu-item tag-pop-manage', 'Manage tags…');
  manage.type = 'button';
  manage.onclick = () => openTagManager(anchor, () => notify(state, []));
  root.append(state.current, state.input, state.list, state.problem, manage);
  place(root, anchor);
  const outside = event => { if (!root.contains(event.target) && event.target !== anchor) closeTagPopover(true); };
  document.addEventListener('pointerdown', outside, true);
  active = { root, anchor, outside };
  // Escape closes this popover and nothing else: left to bubble, it reaches the
  // handler that interrupts a running agent turn.
  root.addEventListener('keydown', event => onKey(event, state));
  state.input.addEventListener('input', () => { state.cursor = 0; paint(state); });
  paint(state);
  state.input.focus();
  loadTagVocabulary().then(found => { state.vocabulary = found.tags || []; paint(state); }).catch(() => {});
}

// place opens the popover below its anchor, or above it when the anchor sits
// in the lower half of the window. Above, it is pinned by its bottom edge, so
// it grows upward as suggestions arrive instead of running off the screen.
export function place(root, anchor) {
  document.body.appendChild(root);
  const rect = anchor.getBoundingClientRect();
  root.style.right = 'auto';
  root.style.left = Math.max(8, Math.min(window.innerWidth - root.offsetWidth - 8, rect.left)) + 'px';
  if (rect.top > window.innerHeight / 2) {
    root.style.top = 'auto';
    root.style.bottom = (window.innerHeight - rect.top + 6) + 'px';
  } else {
    root.style.bottom = 'auto';
    root.style.top = (rect.bottom + 6) + 'px';
  }
}

function onKey(event, state) {
  if (event.key === 'Escape') {
    event.preventDefault();
    event.stopPropagation();
    closeTagPopover(true);
  } else if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
    event.preventDefault();
    const count = state.list.querySelectorAll('button').length;
    if (count) state.cursor = (state.cursor + (event.key === 'ArrowDown' ? 1 : count - 1)) % count;
    paintCursor(state);
  } else if (event.key === 'Enter' && event.target === state.input) {
    event.preventDefault();
    const chosen = state.list.querySelectorAll('button')[state.cursor];
    if (chosen) chosen.click();
  }
}

function ownerTagsOf(state) {
  return state.sessions.length === 1 ? (state.sessions[0].tags || []).filter(isOwnerTag) : [];
}

// choices is what the list offers for the typed text: the owner's matching
// tags, and the typed text itself when it is not one of them yet.
export function tagChoices(text, vocabulary) {
  const typed = parseTagText(text);
  const needle = String(text || '').trim().toLowerCase();
  const known = vocabulary.filter(tag => tagLabel(tag).toLowerCase().includes(needle));
  const isNew = Boolean(typed.value) && !vocabulary.some(tag => sameTag(tag, typed));
  return { typed, known, isNew };
}

function paint(state) {
  state.current.replaceChildren();
  for (const tag of ownerTagsOf(state)) {
    const chip = tagChip(tag);
    const remove = el('button', 'tag-remove', '×');
    remove.type = 'button';
    remove.setAttribute('aria-label', 'Remove ' + tagLabel(tag));
    remove.onclick = () => commit(state, tag);
    chip.appendChild(remove);
    state.current.appendChild(chip);
  }
  const { typed, known, isNew } = tagChoices(state.input.value, state.vocabulary);
  state.list.replaceChildren();
  if (isNew) state.list.appendChild(choice('Create “' + tagLabel(typed) + '”', '', () => commit(state, typed)));
  for (const tag of known.slice(0, 8)) {
    state.list.appendChild(choice(tagLabel(tag), String(tag.sessions || ''), () => commit(state, tag)));
  }
  paintCursor(state);
}

function choice(label, hint, act) {
  const button = el('button', 'pane-menu-item');
  button.type = 'button';
  button.setAttribute('role', 'option');
  button.appendChild(el('span', '', label));
  if (hint) button.appendChild(el('span', 'pane-menu-hint', hint));
  button.onclick = act;
  return button;
}

// The cursor moves while focus stays in the input, so it is announced through
// aria-activedescendant rather than by moving focus.
function paintCursor(state) {
  state.input.removeAttribute('aria-activedescendant');
  state.list.querySelectorAll('button').forEach((button, index) => {
    const current = index === state.cursor;
    button.id = 'tag-pop-option-' + index;
    button.classList.toggle('sel', current);
    button.setAttribute('aria-selected', String(current));
    if (current) state.input.setAttribute('aria-activedescendant', button.id);
  });
}

// commit applies the tag, or takes it off when the one session already has it.
async function commit(state, tag) {
  if (state.busy || !tag.value) return;
  const has = ownerTagsOf(state).some(existing => sameTag(existing, tag));
  const body = { key: tag.key || undefined, value: tag.value };
  state.busy = true;
  state.problem.textContent = '';
  try {
    const result = await changeSessionTags(state.sessions, has ? { retract: [body] } : { apply: [body] });
    state.input.value = '';
    notify(state, result.sessions || []);
    loadTagVocabulary().then(found => { state.vocabulary = found.tags || []; paint(state); }).catch(() => {});
  } catch (err) {
    state.problem.textContent = err.message || String(err);
  } finally {
    state.busy = false;
    paint(state);
    state.input.focus();
  }
}

function notify(state, updated) {
  if (updated.length === state.sessions.length) state.sessions = updated;
  state.onChanged?.(updated);
}
