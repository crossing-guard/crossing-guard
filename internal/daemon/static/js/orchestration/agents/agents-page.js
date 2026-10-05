// Settings → Agents (agents-settings-redesign plan §7): an index of agents,
// one detail page per agent with tabs, and the New agent flow. The route is
// in-view state (like the Settings subpages), so the rail and hash stay
// untouched; other views may ask for an agent with `cg:agents-open`.
import { el } from './agent-ui.js';
import { loadChatCapabilities } from '../../chat-capabilities.js';
import { renderIndex } from './agents-index.js';
import { renderDetail } from './agent-detail.js';
import { renderNewAgent } from './agent-new.js';

let route = { view: 'index' };

if (typeof document !== 'undefined') {
  document.addEventListener('cg:agents-open', event => {
    const id = String(event.detail?.id || '');
    route = id ? { view: 'detail', id, tab: String(event.detail?.tab || 'overview') } : { view: 'index' };
  });
}

// renderAgentsPage mounts the Agents subpage into main and keeps the route
// across re-renders of the Settings view.
export async function renderAgentsPage(main) {
  const host = el('div', 'agents-page');
  main.appendChild(host);
  let capabilities = [];
  try {
    capabilities = await loadChatCapabilities();
  } catch {
    capabilities = [];
  }
  const runtimeNames = Object.fromEntries(capabilities.map(item => [item.runtime, item.displayName]));
  const page = { host, capabilities, runtimeNames, navigate: next => navigate(page, next) };
  await show(page);
}

async function navigate(page, next) {
  route = next;
  await show(page);
  page.host.scrollIntoView?.({ block: 'start' });
}

async function show(page) {
  if (!page.host.isConnected) return;
  page.host.replaceChildren();
  if (route.view === 'detail') {
    await renderDetail(page, route.id, route.tab || 'overview', route.options || {});
  } else if (route.view === 'new') {
    await renderNewAgent(page, route);
  } else {
    await renderIndex(page, route.notice || '');
  }
}
