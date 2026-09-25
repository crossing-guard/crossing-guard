// Governance — the shared rail/center furniture every tab uses.
//
// The panel contract (console-panel-contract-plan §2, R6) says the left rail is
// THE ACTIVE TAB'S LIST and the center is the selected item, with no blank left.
// Only Capture ever honoured that; the other three blanked the rail. These are the
// three pieces that were missing, factored out so the four tabs cannot drift into
// four different-looking lists:
//
//   railHead  — a LABELED header, so "what is this list?" is answered in the rail
//   railRow   — one selectable list entry, the same shape on every tab
//   crumb     — a breadcrumb in the center, so "what am I looking at?" is answered
//               where you act: Governance › Policy › <rule id>
//   railEmpty — an honest empty: a tab with no list SAYS SO rather than going blank
//
// This module owns no state and imports no tab, so importing it from governance.js
// and from each tab creates no cycle.
import { el } from "../core.js";
import { S } from "../state.js";

// railHead labels the list and states its size. The count is the honest part: a
// rail that just said "Rules" left the reader to count four cards themselves.
export function railHead(label, count) {
  const h = el('div', 'railhead');
  h.appendChild(el('span', 'railhead-l', label));
  if (count !== undefined && count !== null) {
    h.appendChild(el('span', 'railhead-n', String(count)));
  }
  return h;
}

// railRow is one entry in a governance rail: a title line and a meta line of chips.
// meta takes strings or elements, so a tab can mix a chip with a plain word.
// `title` optionally sets the row's tooltip (e.g. the full id behind a truncated one).
export function railRow({ title, titleAttr, meta = [], dim = false, onClick }) {
  const d = el('div', 'sess');
  const t = el('div', 't', title);
  if (dim) t.classList.add('t-unnamed');
  if (titleAttr) t.title = titleAttr;
  d.appendChild(t);
  if (meta.length) {
    const m = el('div', 'm');
    for (const bit of meta) {
      m.appendChild(typeof bit === 'string' ? el('span', '', bit) : bit);
    }
    d.appendChild(m);
  }
  // A clickable row is a control: give it button semantics and keyboard operation,
  // so the rail is not a mouse-only surface. markSelected still owns the .sel class.
  if (onClick) {
    d.setAttribute('role', 'button');
    d.tabIndex = 0;
    d.onclick = () => onClick(d);
    d.onkeydown = e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); onClick(d); } };
  }
  return d;
}

// headRow is the flex header strip a detail pane leads with (runtime chip · title ·
// date). Factored here because Capture, Audit, and the session list had three copies
// of the same inline style string — the drift governance-ui exists to prevent.
export function headRow(marginBottom = 8) {
  const h = el('div');
  h.style.cssText = 'display:flex;gap:8px;align-items:center;flex-wrap:wrap;margin-bottom:' + marginBottom + 'px';
  return h;
}

// crumb renders `Governance › Policy › <selection>` above the center content. It
// always starts at Governance, so the trail states the whole scope rather than
// assuming the reader remembers which tab they clicked.
export function crumb(...parts) {
  const c = el('div', 'crumb');
  const all = ['Governance', ...parts.filter(p => p !== undefined && p !== null && p !== '')];
  all.forEach((p, i) => {
    if (i) c.appendChild(el('span', 'crumb-sep', '›'));
    const s = el('span', i === all.length - 1 ? 'crumb-here' : 'crumb-up', String(p));
    c.appendChild(s);
  });
  return c;
}

// railEmpty is the honest collapse (console-design §5): a tab whose list is empty
// says WHY it is empty instead of leaving a gap that reads as a broken pane.
export function railEmpty(why) {
  return el('div', 'empty', why);
}

// markSelected clears the selection class across the rail and sets it on one row,
// so every tab highlights the same way.
export function markSelected(row) {
  document.querySelectorAll('#sidebody .sess').forEach(x => x.classList.remove('sel'));
  row?.classList.add('sel');
}

// ---- the shared session scope (the "stay on the selected session" feature) ----
//
// One session is selected for the WHOLE surface (S.governSession), and the tabs
// re-lens it. Capture/Audit list sessions and select the shared one; Approvals
// FILTERS its holds to it; Policy HIGHLIGHTS the rules its tags could trip. The
// hard rule (IA plan R4): a filter must never SILENTLY hide. Any tab that narrows
// its list to the scope has to say the scope out loud and show "N hidden", with a
// one-click way out — which is exactly what scopeBanner renders.

// setGovernSession makes one session the surface scope. Pass null to clear it.
// `silent` is for the tab that ORIGINATED the change and has already painted itself
// (Capture/Audit selecting a row) — it updates the shared state without asking the
// active tab to repaint over the render it just did. Other tabs pick the scope up
// when next shown. A non-silent call (the scope banner's Clear) fires the bus so the
// visible tab drops its filter immediately.
export function setGovernSession(sess, { silent = false } = {}) {
  S.governSession = sess || null;
  if (!silent) document.dispatchEvent(new CustomEvent('cg:govern-scope'));
}

// scopeBanner states the active session scope in the centre and offers the way
// out. `hidden` (optional) is the count this tab is holding back BECAUSE of the
// scope — shown loudly so a filtered view never passes for a complete one.
export function scopeBanner({ note, hidden } = {}) {
  const s = S.governSession;
  if (!s) return null;
  const bar = el('div', 'scopebar');
  bar.appendChild(el('span', 'scopebar-l', 'Scoped to session'));
  const name = el('span', 'scopebar-name', s.title || s.id);
  if (!s.title) name.classList.add('t-unnamed');
  name.title = s.id;
  bar.appendChild(name);
  if (s.runtime) bar.appendChild(el('span', 'chip ' + s.runtime, s.runtime));
  if (note) bar.appendChild(el('span', 'sub', note));
  if (hidden > 0) {
    // The whole point of R4: the number that is NOT shown, shown.
    bar.appendChild(el('span', 'scopebar-hidden', hidden + ' hidden by this scope'));
  }
  const clear = el('button', 'btn', 'Clear scope');
  clear.title = 'Stop scoping the tabs to this session';
  clear.onclick = () => setGovernSession(null);
  bar.appendChild(clear);
  return bar;
}
