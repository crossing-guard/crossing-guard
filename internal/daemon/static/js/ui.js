// Shared UI components (loaders, chips-of-state, pill, help overlay, theme).
import { $, el } from "./core.js";

// --- theme: auto (OS) / dark / light, persisted ---
function applyTheme(mode) {
  if (mode === 'dark' || mode === 'light') document.documentElement.dataset.theme = mode;
  else delete document.documentElement.dataset.theme;
  localStorage.setItem('cp_theme', mode || 'auto');
}

// The checkpoint mark (our own; never the asterisk glyph which is vendor IP).
// spin=true rotates it for loading/working states.
function mkMark(spin) {
  const NS = 'http://www.w3.org/2000/svg';
  const svg = document.createElementNS(NS, 'svg');
  svg.setAttribute('viewBox', '0 0 32 32');
  svg.setAttribute('class', 'mark' + (spin ? ' spin' : ''));
  const ring = document.createElementNS(NS, 'path');
  ring.setAttribute('class', 'ring');
  ring.setAttribute('d', 'M21.07 5.12 A12 12 0 1 0 26.88 10.93');
  ring.setAttribute('fill', 'none');
  ring.setAttribute('stroke-width', '2.6');
  ring.setAttribute('stroke-linecap', 'round');
  const dot = document.createElementNS(NS, 'circle');
  dot.setAttribute('class', 'dot');
  dot.setAttribute('cx', '24.49'); dot.setAttribute('cy', '7.51'); dot.setAttribute('r', '3');
  svg.append(ring, dot);
  return svg;
}
// Design contract §3.4/§6: any wait >150ms shows a state; no flash on fast loads.
function mkLoader(label) {
  const l = el('div', 'loader');
  l.append(mkMark(true), el('span', '', label || 'Loading…'));
  return l;
}
function mkSkeletons(n) {
  const frag = document.createDocumentFragment();
  for (let i = 0; i < n; i++) {
    const s = el('div', 'skel');
    const b1 = el('div', 'bar'); b1.style.width = (55 + (i * 17) % 40) + '%';
    s.append(b1, el('div', 'bar short'));
    frag.appendChild(s);
  }
  return frag;
}
function authRecovery() {
  const card = el('section', 'empty auth-recovery');
  card.appendChild(el('h3', 'auth-recovery-title', 'This browser tab is not connected'));
  card.appendChild(el('p', '',
    'Crossing Guard stores its local console key for one browser address. This tab does not have the current key.'));

  card.appendChild(el('p', 'auth-recovery-fallback', 'Open the console from a terminal:'));
  card.appendChild(el('code', 'auth-recovery-command', 'crossing-guard console --open'));
  card.appendChild(el('p', 'auth-recovery-retry', 'After running that command, this tab can try again.'));
  const retry = el('button', 'btn', 'Try again');
  retry.onclick = () => location.reload();
  card.appendChild(retry);

  const why = document.createElement('details');
  why.className = 'auth-recovery-why';
  why.appendChild(el('summary', '', 'Why this happens'));
  why.appendChild(el('p', '',
    'localhost and 127.0.0.1 are separate browser addresses. The console uses one configured address; the command supplies its local key once.'));
  card.appendChild(why);
  return card;
}
// Runs an async loader against a container: shows `pending` only if workFn()
// is still in flight after 150ms, then swaps in the result of render(data).
// workFn is a thunk so Retry re-fetches instead of re-awaiting a dead promise.
async function withState(container, pending, workFn, render, isCurrent = () => true) {
  const t = setTimeout(() => {
    if (!isCurrent()) return;
    container.innerHTML = '';
    container.appendChild(pending);
  }, 150);
  try {
    const data = await workFn();
    clearTimeout(t);
    if (!isCurrent()) return;
    container.innerHTML = '';
    render(data);
  } catch (err) {
    clearTimeout(t);
    if (!isCurrent()) return;
    container.innerHTML = '';
    if (err?.status === 401) {
      container.appendChild(authRecovery());
      return;
    }
    const e = el('div', 'empty');
    e.append('✖ ' + (err.message || err), document.createElement('br'));
    const retry = el('button', 'btn', 'Retry'); retry.style.marginTop = '8px';
    retry.onclick = () => withState(container, pending, workFn, render, isCurrent);
    e.appendChild(retry);
    container.appendChild(e);
  }
}
/* jump pill (item 3, two-ended): fixed pill centered over a scroller.
   "↑ Oldest" shows when >300px from the top, "↓ Latest" when >300px from
   the end; either alone or both together. One pill per id; re-attach replaces. */
function attachBottomPill(scroller, id) {
  document.getElementById(id)?.remove();
  const pill = el('div', 'jumppill jumppill-right');
  pill.id = id;
  const oldest = el('button', '', '↑ Oldest');
  const latest = el('button', '', '↓ Latest');
  pill.append(oldest, latest);
  document.body.appendChild(pill);
  const place = () => {
    const r = scroller.getBoundingClientRect();
    // The inline composer is sticky to the BOTTOM of this same scroller, so a
    // pill placed 14px above the scroller's edge lands on top of its controls —
    // it was covering the model selector. Lift it clear of whatever is pinned.
    const pinned = scroller.querySelector('#chatwrap.inline');
    const lift = pinned ? pinned.getBoundingClientRect().height : 0;
    // Right-aligned, not centred: centred it sat on top of the prose you were
    // trying to read. transform:translateX(-50%) is dropped by .jumppill-right.
    pill.style.left = (r.right - 16) + 'px';
    pill.style.bottom = (window.innerHeight - r.bottom + 14 + lift) + 'px';
  };
  const update = () => {
    if (!document.body.contains(scroller)) { pill.remove(); return; }
    const fromEnd = scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight;
    const fromTop = scroller.scrollTop;
    // 300px is more than a screenful of chat: the control stayed hidden through
    // the exact early scrolling where you most want "back to the top".
    const TRIGGER = 80;
    oldest.classList.toggle('show', fromTop > TRIGGER);
    latest.classList.toggle('show', fromEnd > TRIGGER);
    pill.classList.toggle('show', fromTop > TRIGGER || fromEnd > TRIGGER);
    if (pill.classList.contains('show')) place();
  };
  scroller.addEventListener('scroll', update, { passive: true });
  window.addEventListener('resize', update, { passive: true });
  latest.onclick = () => { scroller.scrollTop = scroller.scrollHeight; update(); };
  oldest.onclick = () => { scroller.scrollTop = 0; update(); };
  update();
  return update; // callers may nudge after appending content
}
/* ? — shortcut help overlay (item 7) */
function toggleHelp() {
  const existing = $('#helpcard');
  if (existing) { existing.remove(); return; }
  const card = el('div'); card.id = 'helpcard';
  card.innerHTML = '<h3>Keyboard</h3><table>' +
    [['/', 'search sessions (chat: command palette)'],
     ['@', 'file mention (in chat composer)'],
     ['Esc', 'stop running turn · clear · close'],
     ['⌘/Ctrl+O', 'expand / collapse all tool & thinking chips'],
     ['Enter', 'send · Shift+Enter newline (chat)'],
     ['?', 'this help']]
      .map(([k, v]) => `<tr><td><code>${k}</code></td><td>${v}</td></tr>`).join('') +
    '</table><div class="sub" style="margin-top:8px">Esc or ? to close</div>';
  document.body.appendChild(card);
}

export { applyTheme, mkMark, mkLoader, mkSkeletons, withState, attachBottomPill, toggleHelp, authRecovery };
