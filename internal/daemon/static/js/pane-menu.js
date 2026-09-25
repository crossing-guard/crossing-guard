// Menus of the pane host: the "+" menu of a region and the workspace ⋮ menu.
// Every menu closes on Escape, on an outside pointer, or on its own ×, and
// returns focus to what opened it.
import * as L from './pane-layout.js';
import { activeHostState, applyPresetLayout, enabledModuleIDs, openPane, paneHostDefaults, paneHostPreferenceWarning, resetLayout, setModuleEnabled, toggleWorkspaceHidden } from './pane-host.js';

let activeMenu = null;

export function closeMenus(restoreFocus = false) {
  if (!activeMenu) return;
  const { menu, anchor, close } = activeMenu;
  document.removeEventListener('pointerdown', close, true);
  menu.remove();
  anchor?.setAttribute('aria-expanded', 'false');
  activeMenu = null;
  if (restoreFocus && anchor?.isConnected) anchor.focus();
}

function openMenu(anchor, label, build) {
  closeMenus();
  const menu = document.createElement('div');
  menu.className = 'pane-menu'; menu.setAttribute('role', 'dialog'); menu.setAttribute('aria-label', label);
  const head = document.createElement('div'); head.className = 'pane-menu-head';
  const title = document.createElement('strong'); title.textContent = label;
  const close = document.createElement('button'); close.type = 'button'; close.className = 'pane-menu-close'; close.title = 'Close'; close.setAttribute('aria-label', 'Close'); close.textContent = '×';
  close.addEventListener('click', () => closeMenus(true));
  head.append(title, close); menu.appendChild(head);
  build(menu);
  const rect = anchor.getBoundingClientRect();
  menu.style.top = `${Math.min(window.innerHeight - 20, rect.bottom + 6)}px`;
  menu.style.right = `${Math.max(8, window.innerWidth - rect.right)}px`;
  document.body.appendChild(menu);
  anchor.setAttribute('aria-expanded', 'true');
  const outside = event => { if (!menu.contains(event.target) && event.target !== anchor) closeMenus(true); };
  menu.addEventListener('keydown', event => { if (event.key === 'Escape') { event.preventDefault(); closeMenus(true); } });
  activeMenu = { menu, anchor, close: outside };
  document.addEventListener('pointerdown', outside, true);
  menu.querySelector('button:not(.pane-menu-close):not([disabled]), input')?.focus();
}

function item(label, action, options = {}) {
  const button = document.createElement('button');
  button.type = 'button'; button.className = 'pane-menu-item' + (options.checked ? ' checked' : '');
  const text = document.createElement('span'); text.textContent = label; button.appendChild(text);
  if (options.hint) { const hint = document.createElement('span'); hint.className = 'pane-menu-hint'; hint.textContent = options.hint; button.appendChild(hint); }
  if (options.disabled) button.disabled = true;
  button.addEventListener('click', () => { closeMenus(true); action(); });
  return button;
}

function section(menu, label) {
  const heading = document.createElement('div'); heading.className = 'pane-menu-section'; heading.textContent = label; menu.appendChild(heading);
}

// openAddMenu lists every pane the surface offers; an open pane focuses instead.
export function openAddMenu(host, region, anchor) {
  openMenu(anchor, 'Add a pane', menu => {
    const state = activeHostState();
    const open = new Set(L.openPanes(state.root));
    for (const pane of state.catalog.filter(item => item.catalogVisible && (item.available || item.alwaysOffered))) {
      menu.appendChild(item(pane.title, () => openPane(pane.id, region.id), {
        hint: open.has(pane.id) ? 'open' : '', disabled: !pane.available,
      }));
    }
  });
}

function presetItems(menu, state) {
  const defaults = paneHostDefaults();
  const names = Object.keys(defaults.presets);
  if (!names.length) return;
  section(menu, 'Layout');
  menu.appendChild(item('Reset layout', () => resetLayout(), { hint: defaults.defaultPreset }));
  for (const name of names) menu.appendChild(item(`Preset · ${name}`, () => applyPresetLayout(name), { checked: state.preset === name }));
}

function recentItems(menu, state) {
  section(menu, 'Recently closed');
  if (!state.recentlyClosed.length) { menu.appendChild(item('Nothing closed yet', () => {}, { disabled: true })); return; }
  for (const paneID of state.recentlyClosed) {
    const pane = state.catalog.find(entry => entry.id === paneID);
    menu.appendChild(item(pane?.title || paneID, () => openPane(paneID), { disabled: !pane?.available }));
  }
}

function moduleItems(menu, state) {
  const optional = state.modules.filter(module => !module.required && module.available);
  if (!optional.length) return;
  section(menu, 'Modules');
  const enabled = enabledModuleIDs(state.modules);
  for (const module of optional) {
    const label = document.createElement('label'); label.className = 'pane-menu-toggle';
    const input = document.createElement('input'); input.type = 'checkbox'; input.checked = enabled.has(module.id);
    input.addEventListener('change', () => { closeMenus(true); setModuleEnabled(module.id, input.checked); });
    label.append(input, document.createTextNode(module.title)); menu.appendChild(label);
  }
}

// openHostMenu is the workspace ⋮ menu: hide/show, layout, recently closed,
// modules, and the preference-storage warning as its footer.
export function openHostMenu(host, anchor) {
  openMenu(anchor, 'Workspace', menu => {
    const state = activeHostState(); if (!state) return;
    menu.appendChild(item(state.hidden ? 'Show workspace' : 'Hide workspace', () => toggleWorkspaceHidden(), { hint: paneHostDefaults().keymap.hide_workspace || '' }));
    presetItems(menu, state);
    recentItems(menu, state);
    moduleItems(menu, state);
    if (paneHostPreferenceWarning()) {
      const warning = document.createElement('div'); warning.className = 'pane-menu-warning'; warning.setAttribute('role', 'status');
      warning.textContent = 'View preferences are unavailable for this page.'; menu.appendChild(warning);
    }
  });
}
