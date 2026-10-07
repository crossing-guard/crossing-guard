// Settings shell (settings-restructure plan §3.1): a grouped sidebar, one page
// at a time, and requests from other views that always land. The route is
// in-view state, hash-free; other views ask for a page with
// `cg:settings-subpage` (a page id, or { page, target }) before navigating here.
import { $, el } from "../core.js";
import { previewTokens } from "../appearance-api.js";
import { renderAgentsPage } from "../orchestration/agents/agents-page.js";
import { renderAppearancePage } from "./settings-appearance.js";
import { renderTranscriptModesPage } from "./settings-transcript-modes.js";
import { renderViewsPage } from "../session-organization/settings-views.js";
import { renderRuntimesPage } from "./settings-runtimes.js";
import { renderModelsPage } from "./settings-models.js";
import { renderSpeechSettings, renderSecurityPage, renderAboutPage } from "./settings-system.js";
import { renderTeamPage } from "./settings-team.js";
import { loadAttention, renderOverviewPage } from "./settings-overview.js";
import { SETTINGS_PAGES, normalizePage, pageLabel, settleDecision, findSettings } from "./settings-model.js";

// A page renders into its host and may answer a function to run when it is left.
const PAGES = Object.freeze({
  overview: (host, ctx) => renderOverviewPage(host, ctx),
  runtimes: (host, ctx) => renderRuntimesPage(host, ctx),
  models: host => renderModelsPage(host),
  team: (host, ctx) => renderTeamPage(host, { ...ctx, changed: refreshAttention }),
  agents: host => renderAgentsPage(host),
  appearance: async host => { await renderAppearancePage(host); return () => previewTokens(null); },
  'transcript-modes': host => renderTranscriptModesPage(host),
  views: host => renderViewsPage(host),
  dictation: host => renderSpeechSettings(host),
  security: host => renderSecurityPage(host),
  about: host => renderAboutPage(host),
});

// Pages whose content can go stale or hold a wait while Settings is hidden:
// they are read again when the pane comes back, so nothing shows a frozen wait.
const REREAD_ON_RESTORE = new Set(['overview', 'runtimes', 'team', 'appearance']);
// Pages that put their own heading on the page.
const OWN_HEADING = new Set(['agents', 'views', 'team']);

let shell = null;      // { root, nav, pageHost } while mounted
let mounted = null;    // { page, target } on screen
let requested = null;  // { page, target, external } asked for by another view
let drawn = false;     // the mounted page is drawn in the current shell
let leavePage = null;  // the mounted page's leave function
let attention = { reads: {}, unread: [], items: [], counts: { total: 0, worst: '', pages: {} } };
let renderTicket = 0;
let attentionTicket = 0;
let attentionPending = null;

if (typeof document !== 'undefined') {
  document.addEventListener('cg:settings-subpage', event => {
    const detail = event.detail;
    const page = normalizePage(typeof detail === 'string' ? detail : detail?.page);
    if (page) requested = { page, target: typeof detail === 'string' ? '' : String(detail?.target || '') };
  });
  // The Agents page keeps its own route and hears this event itself (agent id
  // and tab). Here it is a request for that page with a target, so it is shown
  // even when Agents is already mounted; `external` says the Agents page has
  // already been told which agent, so the shell does not tell it again.
  document.addEventListener('cg:agents-open', event => {
    if (dispatching) return;
    requested = { page: 'agents', target: String(event.detail?.id || ''), external: true };
  });
  // The pane was restored from the stash without a render: land the request
  // that arrived meanwhile, and read again what may have gone stale.
  document.addEventListener('cg:view-restored', () => {
    if (shell?.root.isConnected) void settle(true);
  });
}

let dispatching = false; // the shell's own cg:agents-open is not a new request

async function renderSettings() {
  const main = $('#main');
  main.innerHTML = '';
  const root = el('div', 'settings-shell');
  const nav = el('nav', 'settings-nav');
  nav.setAttribute('aria-label', 'Settings pages');
  const pageHost = el('div', 'settings-page');
  // An edit typed into a page and not yet saved: a restore must not read the
  // page again over it.
  pageHost.addEventListener('input', () => { pageHost.dataset.edited = '1'; });
  root.append(nav, pageHost);
  main.appendChild(root);
  shell = { root, nav, pageHost };
  drawn = false;
  buildNav();
  await settle(false);
}

// settle shows what should be on screen now: the requested page, else the
// mounted one — rendered when it changed, was never drawn into this shell, or
// is a page read again on restore (unless the owner has typed into it).
// Attention is read here: when Settings opens and when it is restored.
async function settle(restored) {
  const mountedPage = normalizePage(requested?.page) || mounted?.page || 'overview';
  const reread = REREAD_ON_RESTORE.has(mountedPage) && !(shell.pageHost.dataset.edited && mountedPage !== 'appearance');
  const next = settleDecision({ requested, mounted, drawn, restored, reread });
  requested = null;
  if (next.show) await showPage(next.page, next.target, { tellAgents: next.tellAgents, fresh: true });
  else void refreshAttention();
}

// showPage draws one page. `fresh` asks for a new attention read (entering or
// returning to Settings); a click in the sidebar reuses the last one, except
// the overview, which is that list and always reads.
async function showPage(page, target = '', { tellAgents = true, fresh = false } = {}) {
  if (!shell) return;
  const ticket = ++renderTicket;
  if (leavePage) { try { leavePage(); } catch { /* a page's cleanup never blocks navigation */ } }
  leavePage = null;
  mounted = { page, target };
  drawn = true;
  markActive();
  const host = el('div', 'settings-page-body');
  shell.pageHost.replaceChildren();
  delete shell.pageHost.dataset.edited;
  if (!OWN_HEADING.has(page)) shell.pageHost.appendChild(el('h2', '', pageLabel(page)));
  shell.pageHost.appendChild(host);
  // Arriving at Agents tells its page what to show: the named agent, or — with
  // no agent named (the sidebar, a request for the page alone) — the list. An
  // external cg:agents-open has already told it, tab included.
  if (page === 'agents' && tellAgents) {
    dispatching = true;
    try { document.dispatchEvent(new CustomEvent('cg:agents-open', { detail: target ? { id: target } : {} })); } finally { dispatching = false; }
  }
  if (page === 'overview') {
    host.appendChild(el('div', 'sub', 'Reading…'));
    await refreshAttention();
    if (ticket !== renderTicket) return;
    host.replaceChildren();
  } else if (fresh) {
    void refreshAttention();
  }
  try {
    const leave = await PAGES[page](host, { target, attention, go: showPage });
    if (ticket === renderTicket && typeof leave === 'function') leavePage = leave;
  } catch (error) {
    if (ticket === renderTicket) host.replaceChildren(el('div', 'banner', pageLabel(page) + ' could not be shown: ' + (error?.message || error)));
  }
}

// buildNav draws the sidebar once per shell; showing a page only moves the
// active mark, so the sidebar keeps keyboard focus where the owner left it.
function buildNav() {
  const { nav } = shell;
  nav.replaceChildren();
  const find = el('input', 'settings-find');
  find.type = 'search'; find.placeholder = 'Find a setting'; find.setAttribute('aria-label', 'Find a setting');
  const hits = el('div', 'settings-find-hits hidden');
  find.oninput = () => paintHits(find, hits);
  nav.append(find, hits);
  let group = null;
  for (const [groupName, id, label] of SETTINGS_PAGES) {
    if (groupName !== group) {
      group = groupName;
      if (groupName) nav.appendChild(el('div', 'settings-nav-group', groupName));
    }
    const item = el('button', 'settings-nav-item');
    item.type = 'button'; item.dataset.page = id;
    item.appendChild(el('span', '', label));
    item.onclick = () => { void showPage(id, ''); };
    nav.appendChild(item);
  }
  markActive();
  paintBadges();
}

function markActive() {
  if (!shell) return;
  for (const item of shell.nav.querySelectorAll('.settings-nav-item')) {
    const active = item.dataset.page === mounted?.page;
    item.classList.toggle('active', active);
    if (active) item.setAttribute('aria-current', 'page'); else item.removeAttribute('aria-current');
  }
}

// namedEntries is what "find a setting" may match by name: the runtimes and
// agents the reads already hold. No name lives in the index itself.
function namedEntries() {
  const runtimes = (attention.reads.runtimeStatus?.runtimes || []).map(item => ({ page: 'runtimes', target: item.name, label: item.display_name || item.name }));
  const agents = (attention.reads.roster?.agents || []).map(item => ({ page: 'agents', target: item.profile_id, label: item.name || item.profile_id }));
  return [...runtimes, ...agents];
}

function paintHits(find, hits) {
  const found = findSettings(find.value, namedEntries());
  hits.replaceChildren();
  hits.classList.toggle('hidden', !find.value.trim());
  if (find.value.trim() && !found.length) hits.appendChild(el('div', 'sub', 'No setting matches.'));
  for (const hit of found) {
    const button = el('button', 'settings-find-hit');
    button.type = 'button';
    button.append(el('span', '', hit.label), el('span', 'sub', pageLabel(hit.page)));
    button.onclick = () => { find.value = ''; hits.classList.add('hidden'); void showPage(hit.page, hit.target); };
    hits.appendChild(button);
  }
}

// paintBadges puts the one attention list on the sidebar and on the account
// button: the same counts everywhere, info items never counted.
function paintBadges() {
  const { counts } = attention;
  if (shell) {
    for (const item of shell.nav.querySelectorAll('.settings-nav-item')) {
      item.querySelector('.settings-nav-count')?.remove();
      const page = item.dataset.page === 'overview' ? (counts.total ? { count: counts.total, worst: counts.worst } : null) : counts.pages[item.dataset.page];
      if (page) item.appendChild(el('span', 'settings-nav-count settings-count-' + page.worst, String(page.count)));
    }
  }
  const account = $('#acctbtn');
  if (!account) return;
  account.querySelector('.acct-attention')?.remove();
  const row = document.querySelector('#acctmenu [data-view="settings"]');
  row?.querySelector('.settings-nav-count')?.remove();
  if (!counts.total) return;
  const mark = el('span', 'acct-attention settings-count-' + counts.worst);
  mark.setAttribute('role', 'img');
  mark.setAttribute('aria-label', counts.total + (counts.total === 1 ? ' setting needs' : ' settings need') + ' attention');
  account.appendChild(mark);
  row?.appendChild(el('span', 'settings-nav-count settings-count-' + counts.worst, String(counts.total)));
}

// refreshAttention reads what needs the owner and repaints the marks. It runs
// once after the console loads, when Settings opens, when it is restored, and
// when the overview is shown; nothing polls while Settings is closed. The
// newest read wins: an older one that answers late is dropped.
async function refreshAttention() {
  const ticket = ++attentionTicket;
  const run = (async () => {
    let read;
    try { read = await loadAttention(); } catch { return; }
    if (ticket !== attentionTicket) return;
    attention = read;
    paintBadges();
  })();
  attentionPending = run;
  await run;
  // A newer read started meanwhile: a caller waiting for "fresh" waits for that one.
  if (attentionPending !== run) await attentionPending;
}

export { renderSettings, refreshAttention };
