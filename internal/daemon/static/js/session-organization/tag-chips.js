import { el } from '../core.js';

// A tag reads key:value, or just its value. Who said it is shown by colour and
// by who-and-when on hover — never by a word on the chip.
export function tagLabel(tag) {
  return tag.key ? tag.key + ':' + tag.value : String(tag.value || '');
}

// parseTagText is the inverse: what the owner types becomes a tag. Only the
// first colon separates; a tag without one has no key.
export function parseTagText(text) {
  const trimmed = String(text || '').trim();
  const at = trimmed.indexOf(':');
  if (at <= 0) return { value: trimmed };
  return { key: trimmed.slice(0, at).trim(), value: trimmed.slice(at + 1).trim() };
}

export function sameTag(a, b) {
  return tagLabel(a).toLowerCase() === tagLabel(b).toLowerCase();
}

const CLASS_BY_PROVENANCE = { 'user-asserted': 'cl-user-asserted', 'model-claimed': 'cl-model-claimed' };

export function tagClass(tag) {
  return 'chip ' + (CLASS_BY_PROVENANCE[tag.provenance] || 'cl-observed');
}

export function isOwnerTag(tag) { return tag.provenance === 'user-asserted'; }

function shortDate(seconds) {
  if (!seconds) return '';
  return new Date(seconds * 1000).toLocaleDateString(undefined, { day: 'numeric', month: 'short' });
}

export function tagTitle(tag) {
  const when = shortDate(tag.at);
  if (isOwnerTag(tag)) return when ? 'you · ' + when : 'you';
  if (tag.provenance === 'model-claimed') return [tag.by || '', when].filter(Boolean).join(' · ');
  return when;
}

export function tagChip(tag) {
  const chip = el('span', tagClass(tag), tagLabel(tag));
  const title = tagTitle(tag);
  if (title) chip.title = title;
  return chip;
}

// rowTime is the time a rail row shows: how long the session has been in the
// selected view when there is one, otherwise its last activity.
export function rowTime(session) {
  return session.in_view_since ? new Date(session.in_view_since * 1000).toISOString() : session.modified;
}

// appendRowOrganization adds what the owner and his agents said about a
// session to its rail row: a tag line only when there are tags, a note line
// only inside a view. A row with neither is left exactly as it was.
export function appendRowOrganization(row, session, options = {}) {
  const tags = session.tags || [];
  const base = row.dataset.untaggedAriaLabel || row.dataset.baseAriaLabel || '';
  row.dataset.untaggedAriaLabel = base;
  if (tags.length) {
    const line = el('div', 'g');
    for (const tag of tags) line.appendChild(tagChip(tag));
    row.appendChild(line);
  }
  row.dataset.baseAriaLabel = tags.length ? base + ' · tagged ' + tags.map(tagLabel).join(', ') : base;
  row.setAttribute('aria-label', row.dataset.baseAriaLabel);
  if (session.note && options.showNote) row.appendChild(el('div', 'n', session.note));
  row.classList.toggle('missing', Boolean(session.transcript_missing));
  return row;
}

// repaintRowTags replaces a row's tag and note lines after a change, in place.
export function repaintRowTags(row, session, options = {}) {
  row.querySelectorAll(':scope > .g, :scope > .n').forEach(node => node.remove());
  appendRowOrganization(row, session, options);
}
