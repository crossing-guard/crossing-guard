// One region of the pane host: a tab bar over its panes and the body of the
// active one. Tabs drag between regions; × closes; + adds; the two split
// buttons open a new empty region beside or below. No feature knowledge.
import * as L from './pane-layout.js';
import { activateTab, closePaneByID, entryFor, focusAdjacentRegion, focusRegion, movePaneTo, openPane, splitRegion } from './pane-host.js';
import { openAddMenu } from './pane-menu.js';

const DRAG_TYPE = 'application/x-cg-pane';

function iconButton(className, title, svg, action) {
  const button = document.createElement('button');
  button.type = 'button'; button.className = className; button.title = title; button.setAttribute('aria-label', title);
  button.innerHTML = svg;
  button.addEventListener('click', event => { event.stopPropagation(); action(event); });
  return button;
}

export const ICONS = {
  add: '<svg viewBox="0 0 24 24"><path d="M12 5v14M5 12h14"/></svg>',
  splitRight: '<svg viewBox="0 0 24 24"><rect x="3" y="4" width="18" height="16" rx="2"/><path d="M12 4v16"/></svg>',
  splitDown: '<svg viewBox="0 0 24 24"><rect x="3" y="4" width="18" height="16" rx="2"/><path d="M3 12h18"/></svg>',
  close: '<svg viewBox="0 0 24 24"><path d="M6 6l12 12M18 6L6 18"/></svg>',
  more: '<svg viewBox="0 0 24 24"><circle cx="12" cy="5" r="1.3"/><circle cx="12" cy="12" r="1.3"/><circle cx="12" cy="19" r="1.3"/></svg>',
  eye: '<svg viewBox="0 0 24 24"><path d="M2 12s4-7 10-7 10 7 10 7-4 7-10 7S2 12 2 12z"/><circle cx="12" cy="12" r="3"/></svg>',
  eyeOff: '<svg viewBox="0 0 24 24"><path d="M3 3l18 18M10.6 10.6a2 2 0 0 0 2.8 2.8M9.9 5.2A10 10 0 0 1 12 5c6 0 10 7 10 7a17 17 0 0 1-3.2 3.9M6.6 6.6A16 16 0 0 0 2 12s4 7 10 7a9.7 9.7 0 0 0 4-.8"/></svg>',
};

function makeTab(host, region, paneID) {
  const descriptor = host.catalog.find(item => item.id === paneID);
  const tab = document.createElement('button');
  tab.type = 'button'; tab.className = 'pane-tab' + (region.active === paneID ? ' active' : '');
  tab.dataset.paneId = paneID; tab.draggable = true;
  tab.setAttribute('role', 'tab'); tab.setAttribute('aria-selected', region.active === paneID ? 'true' : 'false');
  const label = document.createElement('span'); label.className = 'pane-tab-label'; label.textContent = descriptor?.title || paneID;
  const close = document.createElement('span'); close.className = 'pane-tab-close'; close.title = `Close ${descriptor?.title || paneID}`;
  close.setAttribute('aria-label', close.title); close.innerHTML = ICONS.close;
  close.addEventListener('click', event => { event.stopPropagation(); closePaneByID(paneID); });
  tab.append(label, close);
  tab.addEventListener('click', () => activateTab(paneID));
  tab.addEventListener('dragstart', event => { event.dataTransfer.setData(DRAG_TYPE, paneID); event.dataTransfer.effectAllowed = 'move'; });
  tab.addEventListener('keydown', event => regionKeys(event, region));
  return tab;
}

function regionKeys(event, region) {
  if (!event.ctrlKey) return;
  const step = { ArrowRight: 1, ArrowDown: 1, ArrowLeft: -1, ArrowUp: -1 }[event.key];
  if (!step) return;
  event.preventDefault(); focusAdjacentRegion(region.id, step);
}

function acceptDrops(host, region, tabs) {
  tabs.addEventListener('dragover', event => {
    if (!event.dataTransfer.types.includes(DRAG_TYPE)) return;
    event.preventDefault(); tabs.classList.add('dragover');
  });
  tabs.addEventListener('dragleave', () => tabs.classList.remove('dragover'));
  tabs.addEventListener('drop', event => {
    tabs.classList.remove('dragover');
    const paneID = event.dataTransfer.getData(DRAG_TYPE);
    if (!paneID) return;
    event.preventDefault(); movePaneTo(paneID, region.id);
  });
}

function makeTabBar(host, region) {
  const tabs = document.createElement('div'); tabs.className = 'pane-tabs'; tabs.setAttribute('role', 'tablist');
  for (const paneID of region.tabs) tabs.appendChild(makeTab(host, region, paneID));
  const spacer = document.createElement('span'); spacer.className = 'pane-tabs-spacer'; tabs.appendChild(spacer);
  const actions = document.createElement('span'); actions.className = 'pane-region-actions';
  actions.appendChild(iconButton('pane-icon-button', 'Add a pane here', ICONS.add, event => openAddMenu(host, region, event.currentTarget)));
  actions.appendChild(iconButton('pane-icon-button', 'Split right', ICONS.splitRight, () => splitRegion(region.id, 'row')));
  actions.appendChild(iconButton('pane-icon-button', 'Split down', ICONS.splitDown, () => splitRegion(region.id, 'col')));
  tabs.appendChild(actions);
  acceptDrops(host, region, tabs);
  return tabs;
}

function emptyBody(host, region) {
  const empty = document.createElement('div'); empty.className = 'pane-region-empty';
  for (const pane of host.catalog.filter(item => item.catalogVisible && item.available && !L.openPanes(host.surfaceState.root).includes(item.id))) {
    const button = document.createElement('button'); button.type = 'button'; button.className = 'pane-region-empty-choice';
    button.textContent = pane.title; button.setAttribute('aria-label', `Open ${pane.title} here`);
    button.addEventListener('click', () => openPane(pane.id, region.id));
    empty.appendChild(button);
  }
  return empty;
}

export function renderRegion(host, region) {
  const element = document.createElement('section');
  element.className = 'pane-region'; element.dataset.regionId = region.id;
  element.setAttribute('aria-label', `${region.active ? (host.catalog.find(item => item.id === region.active)?.title || region.active) : 'Empty'} pane`);
  element.addEventListener('pointerdown', () => focusRegion(region.id), true);
  element.addEventListener('focusin', () => focusRegion(region.id));
  element.appendChild(makeTabBar(host, region));
  const body = document.createElement('div'); body.className = 'pane-region-body';
  if (region.active) {
    const entry = entryFor(host, region.active);
    if (entry) body.appendChild(entry.host);
  } else body.appendChild(emptyBody(host, region));
  element.appendChild(body);
  return element;
}
