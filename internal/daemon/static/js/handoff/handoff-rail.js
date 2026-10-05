// The Handoffs group of the Sessions rail (team rest-of-release plan §8.2, §14
// Q1, Q3): Received and Sent as rail rows. The count on the group is the only
// sign that one arrived — no badge elsewhere, no interrupt. A device with no
// handoff shows no group.
import { el } from '../core.js';
import { loadHandoffs } from './handoff-api.js';
import { withSide, railGroup, endedRowWords, stateView, itemTitle, railMeta, railWhen } from './handoff-model.js';

// How often the group is read again, at most. The daemon publishes the rail's
// own count cadence (session_organization.count_refresh_ms); nothing here stands
// in for it. readSettings is how it is read, and read again while there is none.
let refreshMs = 0;
let readSettings = null;
let settingsReading = false;
// How many ended handoffs are rows before the rest stand behind one row; the
// daemon publishes it (session_organization.handoffs_ended_visible).
let endedVisible = 0;
// Whether the person asked to see the ended ones; kept while the page is open.
let showEnded = false;
let lastReadAt = 0;
let timer = null;
let selectedId = '';
// The last lists read. The group is read on its cadence whether or not the rail is
// on screen, so a rail mounted again (the person came back to Sessions) is painted
// from this at once and is never a view older than one cadence.
let lastList = null;

export function configureHandoffRail({ countRefreshMs, readSettings: reader } = {}) {
  if (typeof reader === 'function') readSettings = reader;
  adopt({ countRefreshMs });
  void ensureSettings();
}

function adopt({ countRefreshMs, endedVisible: ended } = {}) {
  if (Number(countRefreshMs) > 0) refreshMs = Number(countRefreshMs);
  if (Number(ended) > 0 && Number(ended) !== endedVisible) {
    endedVisible = Number(ended);
    repaint();
  }
  schedule();
}

// ensureSettings asks for the cadence while the group has none. The group once
// took it from a single read made as the page loaded; that read was refused on a
// page opened from its link, nothing said so, and the group was never read again
// until the page was reloaded. Every read of the group (the rail mounted, the
// window focused, a handoff changed here) now asks again until one answers.
async function ensureSettings() {
  if (refreshMs || !readSettings || settingsReading) return;
  settingsReading = true;
  try {
    adopt(await readSettings());
  } catch (error) {
    console.warn('Handoffs: the refresh cadence could not be read; it is asked for again with the next read.', error);
  } finally {
    settingsReading = false;
  }
}

// mountHandoffRail adds the group's host to the rail and fills it.
export function mountHandoffRail(side) {
  const host = el('div', 'handoff-rail');
  side.appendChild(host);
  if (lastList) paint(host, lastList);
  void refreshHandoffRail();
  return host;
}

// refreshHandoffRail reads the lists and repaints every mounted group. With no
// group mounted (another page is shown) it still reads and keeps the cadence going:
// stopping here left the rail dead until the page was loaded again.
export async function refreshHandoffRail() {
  void ensureSettings();
  lastReadAt = Date.now();
  try { lastList = withSide(await loadHandoffs()); } catch { schedule(); return; } // the rail says nothing about a read that failed; the next one repaints
  repaint();
  schedule();
}

function repaint() {
  if (lastList) document.querySelectorAll('.handoff-rail').forEach(host => paint(host, lastList));
}

function paint(host, list) {
  const group = railGroup(list, { endedVisible, showEnded });
  host.replaceChildren();
  if (group.empty) return;
  const head = el('div', 'projhead');
  const name = el('button', 'pname projname', 'Handoffs');
  name.type = 'button';
  const body = el('div', 'projbody');
  name.setAttribute('aria-expanded', 'true');
  name.onclick = () => name.setAttribute('aria-expanded', String(!body.classList.toggle('hidden')));
  head.append(name, el('span', 'pcount', String(group.total)));
  host.append(head, body);
  section(body, 'Received', group.received, group.receivedCount, list.transport);
  section(body, 'Sent', group.sent, group.sentCount, list.transport);
  endedRow(body, group);
}

// A side with no row to show has no heading either; its count is of its list.
function section(body, label, items, total, transport) {
  if (!items.length) return;
  const head = el('div', 'handoff-rail-group');
  head.append(el('span', '', label), el('span', 'handoff-rail-count', String(total)));
  body.appendChild(head);
  for (const item of items) body.appendChild(rowNode(item, transport));
}

// endedRow is the one row the older ended handoffs stand behind (the views
// list's "Show all" pattern): nothing is out of reach, and the repositories
// below are not pushed off the screen by handoffs that are over.
function endedRow(body, group) {
  const words = endedRowWords(group);
  if (!words) return;
  const row = el('button', 'handoff-rail-ended', words);
  row.type = 'button';
  row.setAttribute('aria-expanded', String(group.endedShown));
  row.onclick = () => { showEnded = !showEnded; repaint(); };
  body.appendChild(row);
}

function rowNode(item, transport) {
  const row = el('button', 'sess handoff-rail-row' + (item.id === selectedId ? ' sel' : ''));
  row.type = 'button';
  row.dataset.handoffId = item.id;
  const state = stateView(item, transport);
  row.appendChild(el('span', 'session-status-slot'));
  row.appendChild(el('div', 't', itemTitle(item)));
  const meta = el('div', 'm');
  meta.appendChild(el('span', 'chip ' + state.chip, state.word));
  if (railMeta(item)) meta.appendChild(el('span', '', railMeta(item)));
  meta.appendChild(el('span', '', railWhen(item.state_at || item.created_at)));
  row.appendChild(meta);
  row.onclick = () => document.dispatchEvent(new CustomEvent('cg:center', { detail: { view: 'handoff', id: item.id } }));
  return row;
}

function select(id) {
  selectedId = id;
  document.querySelectorAll('.handoff-rail-row').forEach(row => row.classList.toggle('sel', row.dataset.handoffId === id));
}

// schedule reads the group again no sooner than the configured cadence, and
// only while the page is being looked at.
function schedule() {
  if (timer || !refreshMs || typeof document === 'undefined') return;
  timer = setTimeout(() => {
    timer = null;
    if (document.visibilityState === 'visible') void refreshHandoffRail();
    else schedule();
  }, Math.max(refreshMs, refreshMs - (Date.now() - lastReadAt)));
}

if (typeof document !== 'undefined') {
  document.addEventListener('cg:handoffs-changed', () => { void refreshHandoffRail(); });
  document.addEventListener('cg:handoff-shown', event => select(String(event.detail?.id || '')));
  document.addEventListener('cg:session-selected', () => select(''));
  globalThis.addEventListener?.('focus', () => { void refreshHandoffRail(); });
}
