// Governance — one surface, tabbed (console-design §4/§6.4). Face B's single
// place: author rules (Policy), run them across real sessions (Audit), review
// held decisions (Approvals). Each tab renders one of the container-parameterized
// view functions into a sub-container, so the tab bar is never wiped. Ledger (the
// dev sandbox) appears only behind a dev flag. Live approvals become a global
// interrupt in Step 3; this tab is the history/queue.
import { $, el } from "../core.js";
import { S } from "../state.js";
import { renderPolicy } from "./policy.js";
import { renderAudit } from "./audit.js";
import { renderApprovals } from "./approvals.js";
import { renderLedger } from "./ledger.js";
import { renderCapture, clearCaptureSelection } from "./governance-capture.js";
import { setGovernSession } from "./governance-ui.js";
import "./governance-model.js"; // registers the governance.model info provider
import "./governance-entity.js"; // registers the governance.entity info providers

const DEV = localStorage.getItem('cg_dev') === '1' || new URLSearchParams(location.search).has('dev');
// Capture lands first: it is the only tab backed by what actually happened on this
// machine. Policy/Audit/Approvals are authoring and review surfaces over rules that
// may never have fired. (Every tab now writes the rail — that used to be Capture's
// alone, which is what made three of the four tabs open onto a blank left panel.)
let activeTab = 'capture';

// The four tabs are ONE loop, not four features, and saying so is the whole
// difference between "understandable" and "a wall of governance jargon". Each
// tab gets a plain-English one-liner: what it IS, then what you DO. No internal
// vocabulary ("policy plane", "honest boundary") — those meant something to us
// and nothing to a first-time reader.
const TAB_DESC = {
  capture: 'The record — every governed tool call Crossing Guard captured on this machine. Pick a session on the left to see what it touched; uncaptured calls remain unknown.',
  policy: 'The active engine rulebook — what to allow, hold, or block. This tab shows configuration state; hook attachment and firing require separate observed evidence.',
  audit: 'The preview — run your rules across every past session to see what they WOULD have caught, before you rely on them. This never blocks anything; it only reports.',
  approvals: 'The record — every action a rule held for a human yes/no, and how it ended. A hold waiting on you right now raises a banner over every surface; this tab is the history, not somewhere you have to go looking.',
  ledger: 'Developer sandbox.',
};

// activeShow points at the current render's tab switcher, so the module-level scope
// listener below always drives the live panel. A new render overwrites it.
let activeShow = null;
// renderSeq counts full renders of this surface, so the open-handler below can tell a
// fresh render (which already painted the new scope) from a keep-alive restore (which
// did not) — and repaint only in the second case.
let renderSeq = 0;
// The shared session scope changed (a tab set it, or someone cleared it): repaint the
// active tab so it re-lenses the new scope. Registered ONCE, here — not per render.
document.addEventListener('cg:govern-scope', () => {
  if (S.view === 'governance' && activeShow) activeShow(activeTab);
});

// Policy mutations ask the governance owner to repaint instead of recursively writing
// the shared main element. If the user navigated away or changed tabs while the request
// was in flight, the stale completion cannot overwrite the newer surface.
document.addEventListener('cg:govern-refresh', e => {
  if (S.view === 'governance' && activeTab === 'policy' && activeShow) {
    activeShow('policy', e.detail || null);
  }
});

// Open bus: another surface (the Session panel) hands us a session and asks for a
// specific tab. The scope is set BEFORE navigating so a fresh render paints it once,
// already correct; a restored keep-alive pane is repainted after, because render()
// short-circuits on the cache and would otherwise still show the previous scope.
document.addEventListener('cg:govern-open', e => {
  const d = e.detail || {};
  if (d.tab) activeTab = d.tab;
  if (d.session) setGovernSession(d.session, { silent: true });
  const before = renderSeq;
  document.dispatchEvent(new CustomEvent('cg:nav', { detail: 'governance' }));
  if (renderSeq === before && activeShow) activeShow(activeTab); // restored, not re-rendered
});

function renderGovernance() {
  renderSeq++;
  const main = $('#main');
  main.innerHTML = '<h2>Governance</h2>'
    + '<div class="sub">crossing-guard watches what your AI agents do and lets you set deterministic rules over it. '
    + 'The four tabs are a loop: <b>Capture</b> what happened → write <b>Policy</b> → <b>Audit</b> it against your real history → clear the <b>Approvals</b> it holds.</div>';
  const tabs = [
    ['capture', 'Capture', renderCapture],
    ['policy', 'Policy', renderPolicy],
    ['audit', 'Audit', renderAudit],
    ['approvals', 'Approvals', renderApprovals],
  ];
  if (DEV) tabs.push(['ledger', 'Ledger · dev', renderLedger]);
  if (!tabs.some(t => t[0] === activeTab)) activeTab = 'capture';

  const bar = el('div', 'tabbar');
  const tabDesc = el('div', 'sub');
  tabDesc.style.cssText = 'margin:6px 0 10px;padding:8px 12px;border-left:2px solid var(--accent);background:var(--panel2)';
  const panel = el('div');
  const show = (key, mutationNotice) => {
    activeTab = key;
    [...bar.children].forEach(b => b.classList.toggle('active', b.dataset.tab === key));
    tabDesc.innerHTML = TAB_DESC[key] || '';
    const renderHost = el('div');
    panel.replaceChildren(renderHost);
    S.governTab = key; // the info panel scopes its model counts to the active tab
    S.governEntity = null; // a clicked-file view belongs to the tab it was opened in
    // The rail belongs to the ACTIVE TAB, and every tab now writes it (panel
    // contract R6 — "no blank left"). Clearing here rather than in each tab keeps
    // the handover in one place: whatever the previous tab left is gone before the
    // next one paints. Leaving Capture still drops ITS capture REPORT (the info
    // panel), but NOT the shared session scope — that survives the tab switch on
    // purpose, so the next tab re-lenses the same session.
    $('#sidebody').replaceChildren();
    if (key !== 'capture') clearCaptureSelection();
    tabs.find(t => t[0] === key)[2](renderHost, mutationNotice); // detached hosts contain late async work after a tab switch
    document.dispatchEvent(new CustomEvent('cg:info'));
  };
  for (const [key, label] of tabs) {
    const b = el('button', 'tab', label); b.dataset.tab = key;
    b.onclick = () => show(key);
    bar.appendChild(b);
  }
  // Publish THIS render's show() so the one module-level scope listener repaints the
  // live panel, never a detached one from a previous render (renderGovernance re-runs
  // on every fresh visit / active-tab re-click; a per-render listener would stack).
  activeShow = show;
  main.append(bar, tabDesc, panel);
  show(activeTab);
}

export { renderGovernance };
