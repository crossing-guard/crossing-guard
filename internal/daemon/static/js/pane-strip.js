// The pane strip: one icon per pane kind the surface offers, with three states
// (closed, open, open-and-focused), dimmed while the workspace is hidden. It is
// the guaranteed way back to any pane, so it never disappears: it lives in the
// host chrome while the workspace is shown and in #pane-strip while hidden.
import { $ } from './core.js';
import * as L from './pane-layout.js';
import { activeHostState, closePaneByID, focusRegion, openPane, toggleWorkspaceHidden } from './pane-host.js';
import { ICONS } from './pane-region.js';
import { openHostMenu } from './pane-menu.js';

// paneStripState is the pure part: what each button shows. Exported so it can be
// pinned without a DOM.
export function paneStripState(state) {
  const out = [];
  if (!state) return out;
  const focused = L.findNode(state.root, state.focusedRegion);
  for (const pane of state.catalog) {
    if (!pane.catalogVisible || (!pane.available && !pane.alwaysOffered)) continue;
    const region = L.regionOf(state.root, pane.id);
    const status = !region ? 'closed' : (focused?.id === region.id && region.active === pane.id ? 'focused' : 'open');
    out.push({ id: pane.id, title: pane.title, status, dimmed: state.hidden, available: pane.available, icon: pane.icon || '' });
  }
  return out;
}

function stripButton(item) {
  const button = document.createElement('button');
  button.type = 'button'; button.className = `pane-strip-button ${item.status}` + (item.dimmed ? ' dimmed' : '');
  button.dataset.paneId = item.id; button.title = item.title; button.setAttribute('aria-label', item.title);
  button.setAttribute('aria-pressed', item.status === 'closed' ? 'false' : 'true');
  button.disabled = !item.available;
  const glyph = document.createElement('span'); glyph.className = 'pane-strip-glyph'; glyph.innerHTML = item.icon || `<span class="pane-strip-initial">${(item.title || '?')[0]}</span>`;
  const dot = document.createElement('span'); dot.className = 'pane-strip-dot';
  button.append(glyph, dot);
  button.addEventListener('click', () => {
    if (item.status === 'closed') { openPane(item.id); return; }
    if (item.status === 'open') { const state = activeHostState(); const region = L.regionOf(state.root, item.id); if (region) { focusRegion(region.id, true); openPane(item.id); } return; }
    closePaneByID(item.id);
  });
  return button;
}

function controlButton(className, title, svg, action) {
  const button = document.createElement('button');
  button.type = 'button'; button.className = className; button.title = title; button.setAttribute('aria-label', title);
  button.innerHTML = svg; button.addEventListener('click', event => { event.stopPropagation(); action(event); });
  return button;
}

// renderStrip (re)draws the strip into its current home: the host chrome when
// the workspace is shown, #pane-strip when it is hidden. One node, one owner.
export function renderStrip(host) {
  const state = activeHostState();
  const strip = document.createElement('div'); strip.className = 'pane-strip'; strip.setAttribute('role', 'toolbar'); strip.setAttribute('aria-label', 'Panes');
  for (const item of paneStripState(state)) strip.appendChild(stripButton(item));
  const spacer = document.createElement('span'); spacer.className = 'pane-strip-spacer'; strip.appendChild(spacer);
  strip.appendChild(controlButton('pane-icon-button', state?.hidden ? 'Show workspace' : 'Hide workspace', state?.hidden ? ICONS.eye : ICONS.eyeOff, () => toggleWorkspaceHidden()));
  strip.appendChild(controlButton('pane-icon-button pane-host-menu-button', 'Workspace menu', ICONS.more, event => openHostMenu(host, event.currentTarget)));
  const closed = $('#pane-strip');
  if (state?.hidden || !host.root) {
    if (closed) { closed.classList.remove('hidden'); closed.replaceChildren(strip); }
    return strip;
  }
  if (closed) { closed.classList.add('hidden'); closed.replaceChildren(); }
  const chrome = host.root.querySelector(':scope > .pane-host-chrome');
  if (chrome) chrome.replaceChildren(strip);
  return strip;
}
