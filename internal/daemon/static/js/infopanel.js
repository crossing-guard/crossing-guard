// Read-only evidence-provider registry and pane-source adapter.
// Generic layout, focus, lifecycle orchestration, and preferences live in
// pane-host.js / pane-layout.js; providers continue to own factual rendering.
//
// Primary providers that declare `pane.group` share one pane: its header and
// pinned modules render once, and a tab bar swaps the primary host between the
// grouped providers, each retaining its own box and state. Every other primary
// provider is its own pane, as before.

const ZONES = ['header', 'primary', 'pinned'];
const SECTION_FRESH_MS = 30000;
const registry = new Map();

export function provider(candidate) {
  if (!candidate?.id) throw new Error('info provider needs an id');
  if (candidate.zone && !ZONES.includes(candidate.zone)) {
    throw new Error(`info provider ${candidate.id}: unknown zone "${candidate.zone}" (expected ${ZONES.join(' | ')})`);
  }
  registry.set(candidate.id, { zone: 'primary', order: 0, required: false, ...candidate });
}

export function providers() { return [...registry.values()]; }

const matches = (provider, ctx) => {
  try { return !!provider.match(ctx); } catch { return false; }
};
const titleOf = (provider, ctx) => typeof provider.title === 'function' ? provider.title(ctx) : provider.title;

async function renderSection(host, provider, ctx, alive, expandable = false) {
  if (!alive()) return null;
  const section = document.createElement('div'); section.className = 'infosec';
  const heading = document.createElement('div'); heading.className = 'infosec-h'; heading.textContent = titleOf(provider, ctx);
  if (expandable) heading.classList.add('infosec-h-primary');
  const box = document.createElement('div'); section.append(heading, box); host.appendChild(section);
  const renderFailure = () => {
    if (!alive()) return;
    box.replaceChildren();
    const state = document.createElement('div'); state.className = 'evidence-state'; state.textContent = 'Request failed';
    const detail = document.createElement('div'); detail.className = 'sub'; detail.textContent = 'This evidence section could not be loaded.';
    const retry = document.createElement('button'); retry.type = 'button'; retry.className = 'btn'; retry.textContent = 'Retry';
    retry.addEventListener('click', async () => {
      box.replaceChildren();
      const loading = document.createElement('div'); loading.className = 'sub'; loading.textContent = 'Loading evidence…'; box.appendChild(loading);
      try { await provider.render(ctx, box); loading.remove(); } catch { renderFailure(); }
    });
    box.append(state, detail, retry);
  };
  try { await provider.render(ctx, box); } catch { renderFailure(); }
  if (!alive()) return null;
  return { section, box };
}

async function refreshItem(item, provider, ctx, alive) {
  if (!alive() || !item.controller || item.refreshing) return;
  item.refreshing = true;
  const old = item.controller.box;
  const scrollTop = item.host.scrollTop;
  const staging = document.createElement('div'); staging.hidden = true; staging.cgState = old.cgState;
  item.controller.section.appendChild(staging);
  const marker = document.createElement('div'); marker.className = 'sub evidence-refreshing'; marker.textContent = 'Refreshing captured facts…'; old.appendChild(marker);
  try {
    await provider.render(ctx, staging);
    if (!alive() || !staging.isConnected) { staging.remove(); marker.remove(); return; }
    staging.hidden = false; old.replaceWith(staging); item.controller.box = staging; item.loadedAt = Date.now(); item.host.scrollTop = scrollTop;
  } catch {
    staging.remove(); marker.className = 'sub evidence-refresh-failed'; marker.textContent = 'Refresh failed · showing the last loaded facts';
  } finally { item.refreshing = false; }
}

function enabledPaneModules(ctx, placement, enabledModules) {
  return providers()
    .filter(provider => provider.zone !== 'primary')
    .filter(provider => (provider.paneModule?.placement || (provider.zone === 'pinned' ? 'pane-pinned' : 'pane-header')) === placement)
    .filter(provider => matches(provider, ctx) && (provider.required || enabledModules.has(provider.id)))
    .sort((a, b) => (a.order || 0) - (b.order || 0));
}

function renderModuleZone(host, ctx, alive, placement, enabledModules, className, stableItems) {
  const list = enabledPaneModules(ctx, placement, enabledModules);
  if (!list.length) return;
  const zone = document.createElement('div'); zone.className = className; host.appendChild(zone);
  for (const provider of list) {
    const item = { host: zone, provider, loadedAt: 0, controller: null, refreshing: false }; stableItems.push(item);
    void renderSection(zone, provider, ctx, alive).then(controller => { if (!controller) return; item.controller = controller; item.loadedAt = Date.now(); });
  }
}

function makeTabBar(primaries, ctx, activeID, onSelect) {
  const bar = document.createElement('div'); bar.className = 'panel-tabbar';
  const tabs = document.createElement('div'); tabs.className = 'panel-tabs'; tabs.setAttribute('role', 'tablist');
  for (const primary of primaries) {
    const tab = document.createElement('button'); tab.type = 'button'; tab.className = 'panel-tab' + (primary.id === activeID ? ' active' : '');
    tab.dataset.providerId = primary.id; tab.setAttribute('role', 'tab'); tab.setAttribute('aria-selected', primary.id === activeID ? 'true' : 'false');
    tab.textContent = titleOf(primary, ctx);
    tab.addEventListener('click', () => onSelect(primary.id));
    tabs.appendChild(tab);
  }
  bar.appendChild(tabs);
  return bar;
}

// createEvidenceController hosts one or more primary providers behind one
// header and one pinned zone. Each primary keeps its own box (and cgState) in
// a retained panel-primary-host; switching tabs swaps hosts without re-render.
function createEvidenceController(primaries, ctx, host, options = {}) {
  let generation = 1;
  let disposed = false;
  const alive = () => !disposed && generation === 1 && host.isConnected;
  const enabledModules = options.enabledModules || new Set();
  host.classList.add('evidence-pane');
  const stableItems = [];
  renderModuleZone(host, ctx, alive, 'pane-header', enabledModules, 'panel-head', stableItems);
  const body = document.createElement('div'); body.className = 'panel-body';
  const items = new Map();
  let activeID = primaries[0]?.id || null;
  const primaryItem = () => items.get(activeID) || null;
  const mount = id => {
    let item = items.get(id);
    if (!item) {
      const primaryHost = document.createElement('div'); primaryHost.className = 'panel-primary-host';
      const primary = primaries.find(candidate => candidate.id === id);
      item = { host: primaryHost, provider: primary, loadedAt: 0, controller: null, refreshing: false, ready: null };
      item.ready = renderSection(primaryHost, primary, ctx, alive, true).then(controller => {
        if (!controller) return null;
        item.controller = controller; item.loadedAt = Date.now(); return controller;
      });
      items.set(id, item);
    }
    return item;
  };
  let bar = null;
  const select = id => {
    if (!primaries.some(primary => primary.id === id)) return;
    activeID = id;
    const item = mount(id);
    body.replaceChildren(item.host);
    if (bar) { const next = makeTabBar(primaries, ctx, activeID, select); bar.replaceWith(next); bar = next; }
  };
  if (primaries.length > 1) { bar = makeTabBar(primaries, ctx, activeID, select); host.appendChild(bar); }
  host.appendChild(body);
  if (activeID) select(activeID);
  renderModuleZone(host, ctx, alive, 'pane-pinned', enabledModules, 'panel-pinned', stableItems);

  return {
    activate: detail => {
      const targetID = detail?.tab && primaries.some(primary => primary.id === detail.tab) ? detail.tab : activeID;
      const payload = detail?.tab ? detail.detail : detail;
      if (targetID !== activeID) select(targetID);
      const item = primaryItem();
      const primary = item?.provider;
      void Promise.resolve(item?.ready).then(controller => {
        if (!alive() || !controller || typeof primary?.activate !== 'function') return;
        primary.activate(ctx, controller.box, payload);
      });
    },
    resume: () => {
      const item = primaryItem();
      if (item?.controller && Date.now() - item.loadedAt >= SECTION_FRESH_MS) void refreshItem(item, item.provider, ctx, alive);
      for (const stable of stableItems) if (stable.controller && Date.now() - stable.loadedAt >= SECTION_FRESH_MS) void refreshItem(stable, stable.provider, ctx, alive);
    },
    suspend: () => {},
    dispose: () => { disposed = true; generation++; host.replaceChildren(); },
  };
}

function createModuleController(provider, ctx, host) {
  let disposed = false;
  const alive = () => !disposed && host.isConnected;
  const ready = renderSection(host, provider, ctx, alive);
  return {
    resume: () => {}, suspend: () => {},
    dispose: () => { disposed = true; host.replaceChildren(); },
    activate: detail => void ready.then(controller => {
      if (alive() && controller && typeof provider.activate === 'function') provider.activate(ctx, controller.box, detail);
    }),
  };
}

function paneDescriptor(primaries, ctx, group = null) {
  const lead = primaries[0];
  const metadata = lead.pane || {};
  const any = fn => primaries.some(primary => { try { return !!fn(primary); } catch { return false; } });
  return {
    id: group ? group.id : lead.id,
    title: group ? group.title : titleOf(lead, ctx),
    icon: group ? group.icon : metadata.icon,
    capability: metadata.capability || 'evidence',
    catalogOrder: metadata.catalogOrder ?? lead.order ?? 0,
    alwaysOffered: metadata.alwaysOffered === true,
    tabs: group ? primaries.map(primary => primary.id) : undefined,
    // The complete catalog is needed for lossless preference migration, while
    // catalogMatch prevents a provider from leaking into another surface's Views.
    catalogMatch: context => any(primary => primary.match(context)),
    match: context => any(primary => primary.match(context)),
    unavailable: metadata.unavailable,
    create: (context, host, options) => createEvidenceController(primaries.filter(primary => matches(primary, context)), context, host, options),
  };
}

function moduleDescriptor(provider, ctx) {
  return {
    id: provider.id,
    title: titleOf(provider, ctx),
    required: provider.required === true,
    catalogOrder: provider.order || 0,
    placement: provider.paneModule?.placement || (provider.zone === 'pinned' ? 'pane-pinned' : 'pane-header'),
    match: provider.match,
    create: (ctx, host) => createModuleController(provider, ctx, host),
  };
}

// GROUPS are the composite panes: a group id, its title, and the icon. A
// provider joins a group by declaring pane.group.
const GROUPS = {
  evidence: { id: 'evidence', title: 'Evidence', icon: '<svg viewBox="0 0 24 24"><path d="M4 6h16M4 12h10M4 18h7"/></svg>' },
};

export const evidencePaneSource = {
  id: 'evidence',
  // Return the complete registered catalog; the host evaluates match(ctx). This
  // preserves known ids across temporary unavailability and makes migration
  // complete before replacing the old preference bytes.
  panes: ctx => {
    const primaries = providers().filter(provider => provider.zone === 'primary');
    const grouped = new Map();
    const out = [];
    for (const primary of primaries) {
      const group = primary.pane?.group ? GROUPS[primary.pane.group] : null;
      if (!group) { out.push(paneDescriptor([primary], ctx)); continue; }
      if (!grouped.has(group.id)) grouped.set(group.id, []);
      grouped.get(group.id).push(primary);
    }
    for (const [groupID, members] of grouped) out.push(paneDescriptor(members, ctx, GROUPS[groupID]));
    return out;
  },
  modules: ctx => providers().filter(provider => provider.zone !== 'primary').map(provider => moduleDescriptor(provider, ctx)),
};
