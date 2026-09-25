// The open session's header: the owner's tags (removable), agents' tags, the
// owner's most recently used tags as one-click toggles, what detectors saw, and
// one line of note. It lives in its own host inside the header and repaints
// only that host — tagging never redraws the conversation below it.
import { el } from '../core.js';
import { changeSessionTags, loadSessionTags, loadTagVocabulary, saveSessionNote } from './organization-api.js';
import { isOwnerTag, sameTag, tagChip, tagLabel } from './tag-chips.js';
import { openTagPopover } from './tag-popover.js';

let recentToggleCount = 4;
let onSessionChanged = () => {};

export function configureHeaderTags(options) {
  if (Number.isInteger(options.recentTagToggles) && options.recentTagToggles >= 0) recentToggleCount = options.recentTagToggles;
  if (options.onSessionChanged) onSessionChanged = options.onSessionChanged;
}

// mountHeaderTags adds the host and fills it. A session the daemon cannot place
// simply gets no tag line; the header is otherwise untouched.
export function mountHeaderTags(head, session) {
  const host = el('div', 'session-tags');
  host.dataset.runtime = session.runtime;
  host.dataset.sessionId = session.id;
  head.appendChild(host);
  refreshHeaderTags(host, { runtime: session.runtime, id: session.id });
  return host;
}

export async function refreshHeaderTags(host, session) {
  try {
    const [found, vocabulary] = await Promise.all([loadSessionTags(session), loadTagVocabulary()]);
    if (!host.isConnected) return;
    paintHeaderTags(host, found.sessions?.[0] || session, vocabulary.tags || []);
  } catch (err) {
    // A session the daemon cannot place (a chat not yet saved, say) simply has
    // no tag line. Anything else is said, in the daemon's words.
    if (!host.isConnected) return;
    host.replaceChildren(...(err?.status === 404 ? [] : [el('div', 'tag-pop-problem', err.message || String(err))]));
  }
}

// repaintAfterChange uses the session the write returned; only the owner's
// vocabulary (its order and counts changed) is fetched again.
async function repaintAfterChange(host, session) {
  let vocabulary = [];
  try { vocabulary = (await loadTagVocabulary()).tags || []; } catch { /* toggles are a convenience */ }
  if (host.isConnected) paintHeaderTags(host, session, vocabulary);
}

// openHeaderTagPopover is what the tag shortcut calls for the open session.
export function openHeaderTagPopover() {
  document.querySelector('.session-tags .tag-add')?.click();
}

function paintHeaderTags(host, session, vocabulary) {
  const changed = updated => {
    onSessionChanged(updated);
    repaintAfterChange(host, updated[0] || session);
  };
  const line = el('div', 'tagline');
  const tags = session.tags || [];
  for (const tag of tags) line.appendChild(isOwnerTag(tag) ? removableChip(session, tag, changed) : tagChip(tag));
  for (const tag of recentToggles(vocabulary, tags)) line.appendChild(toggle(session, tag, changed));
  const add = el('button', 'tag-add', '+ tag');
  add.type = 'button';
  add.onclick = () => openTagPopover(add, [session], changed);
  line.appendChild(add);
  const parts = [line];
  if ((session.facts || []).length) {
    const facts = el('div', 'tagline tagline-facts');
    for (const fact of session.facts) facts.appendChild(tagChip(fact));
    parts.push(facts);
  }
  parts.push(noteField(session, changed));
  host.replaceChildren(...parts);
}

// recentToggles are the owner's most recently used tags this session lacks.
export function recentToggles(vocabulary, tags, count = recentToggleCount) {
  return vocabulary.filter(candidate => !tags.some(tag => isOwnerTag(tag) && sameTag(tag, candidate))).slice(0, count);
}

function removableChip(session, tag, changed) {
  const chip = tagChip(tag);
  const remove = el('button', 'tag-remove', '×');
  remove.type = 'button';
  remove.setAttribute('aria-label', 'Remove ' + tagLabel(tag));
  remove.onclick = () => write(session, { retract: [{ key: tag.key || undefined, value: tag.value }] }, changed);
  chip.appendChild(remove);
  return chip;
}

function toggle(session, tag, changed) {
  const button = el('button', 'tag-toggle', tagLabel(tag));
  button.type = 'button';
  button.setAttribute('aria-label', 'Tag this session ' + tagLabel(tag));
  button.onclick = () => write(session, { apply: [{ key: tag.key || undefined, value: tag.value }] }, changed);
  return button;
}

async function write(session, change, changed) {
  try {
    const result = await changeSessionTags([session], change);
    changed(result.sessions || []);
  } catch (err) {
    const host = document.querySelector('.session-tags');
    host?.querySelector(':scope > .tag-pop-problem')?.remove();
    const problem = el('div', 'tag-pop-problem', err.message || String(err));
    problem.setAttribute('role', 'alert');
    host?.appendChild(problem);
  }
}

function noteField(session, changed) {
  const row = el('div', 'session-note');
  const input = el('input');
  input.placeholder = 'Note to self';
  input.setAttribute('aria-label', 'Note to self');
  input.maxLength = 500;
  input.value = session.note || '';
  const save = async () => {
    if (input.value.trim() === (session.note || '')) return;
    try {
      const result = await saveSessionNote(session, input.value);
      changed(result.sessions || []);
    } catch (err) {
      input.setCustomValidity(err.message || String(err));
      input.reportValidity();
    }
  };
  input.addEventListener('change', save);
  input.addEventListener('keydown', event => { if (event.key === 'Enter') { event.preventDefault(); input.blur(); } });
  row.appendChild(input);
  return row;
}
