// Settings › Runtimes (settings-restructure plan §3.3): one row per registered
// runtime with the facts the daemon stores about it, the guided connection
// actions for a runtime that advertises them, and what this browser uses for
// new chats. The connection preview, commit, watch and recovery functions below
// the row code are moved from settings.js unchanged.
import { el, api, fmtTime, lblWrap, mkSelectKV, getDefaults, setDefaults } from "../core.js";
import { loadRuntimeIntegrations, previewRuntimeIntegration, commitRuntimeIntegration,
  startRuntimeIntegrationWatch, readRuntimeIntegrationWatch, confirmRuntimeIntegrationVisibleBlock } from "../runtime-integrations.js";
import { loadChatCapabilities, chooseChatCapability, findChatCapability, capabilityPairs, runtimeChatDefaults } from "../chat-capabilities.js";
import { loadRecall } from "../handoff/handoff-api.js";
import { recallControl } from "../handoff/memory-recall.js";

const CONNECTION_STATE_LABELS = Object.freeze({
  not_detected: 'Not detected', detected: 'Detected · not connected',
  preview_blocked: 'Review required', connected_unverified: 'Connected · no saved verification',
  needs_attention: 'Needs attention', firing_observed: 'Hook firing observed',
  decision_firing_observed: 'Decision hook observed', denial_returned: 'Denial returned',
  enforcement_verified: 'Enforcement verified',
});

// The guided states still worth a chip of their own; the rest are read off the ladder.
const CHIP_STATES = new Set(['needs_attention', 'preview_blocked', 'denial_returned', 'decision_firing_observed']);

// What the daemon's attention code means, for a runtime with no sentence of its own.
const RUNTIME_ATTENTION_WORDS = Object.freeze({
  hook_binary_missing: 'The hook is configured, but the hook program it names is gone.',
  hook_outdated: 'The hook needs repair: it does not match this Crossing Guard.',
  never_fired: 'Connected. No tool event from it has been seen yet.',
});

// What the owner did to each runtime's row: opened it or closed it. Only their
// own clicks are kept across redraws and visits; a row that opens by itself
// (it was asked for, or it has a problem) is decided again on every draw.
const ownerRows = { opened: new Set(), closed: new Set() };
// The runtime this visit asked for (Overview, find a setting).
let requestedRuntime = '';

// runtimeRowOpen says whether a runtime's row is drawn open: the owner's own
// choice first; otherwise open when this visit asked for it or it has a problem
// to read ("connected, never fired" is a note, not a problem).
// recordRowChoice notes the owner's choice for one row: it is in exactly one of
// the two sets afterwards.
function recordRowChoice(rows, name, open) {
  rows[open ? 'opened' : 'closed'].add(name);
  rows[open ? 'closed' : 'opened'].delete(name);
}

function runtimeRowOpen({ name, target = '', attention = '', opened = ownerRows.opened, closed = ownerRows.closed }) {
  if (opened.has(name)) return true;
  if (closed.has(name)) return false;
  return name === target || Boolean(attention && attention !== 'never_fired');
}

// renderRuntimesPage is the page; renderRuntimeConnections is its first card.
async function renderRuntimesPage(main, ctx = {}) {
  requestedRuntime = String(ctx.target || '');
  // Asking for a runtime by name is the owner's choice to see it: it opens even
  // if they closed that row before.
  if (requestedRuntime) ownerRows.closed.delete(requestedRuntime);
  await renderRuntimeConnections(main);
  await renderChatDefaults(main);
}

async function renderRuntimeConnections(main) {
  const card = el('section', 'settings-card');
  card.appendChild(el('h3', '', 'Connections'));
  card.appendChild(el('div', 'sub',
    'Detection is read-only. Connecting changes only the previewed provider hook file; Crossing Guard never opens or controls the provider.'));
  const host = el('div', 'runtime-integrations');
  const loading = el('div', 'sub', 'Checking registered runtime connections…');
  host.appendChild(loading); card.appendChild(host); main.appendChild(card);
  try {
    await refreshRuntimeConnections(host);
  } catch (error) {
    loading.replaceWith(el('div', 'banner', 'Runtime connections unavailable: ' + (error.message || error)));
  }
}

// refreshRuntimeConnections redraws every runtime's row from the daemon's two
// reads: the stored status of each runtime, and the guided connection of those
// that advertise one.
async function refreshRuntimeConnections(host, notice = '') {
  // The stored status is the page; the guided connections add actions to it. A
  // failed connection read leaves the rows and says so.
  const [statusRead, guidedRead, recallRead] = await Promise.allSettled([api('/api/runtime-status'), loadRuntimeIntegrations(true), loadRecall()]);
  if (statusRead.status === 'rejected') throw statusRead.reason;
  if (!host.isConnected) return;
  const status = statusRead.value;
  const integrations = guidedRead.status === 'fulfilled' ? guidedRead.value : [];
  host.replaceChildren();
  if (notice) host.appendChild(el('div', 'banner', notice));
  if (guidedRead.status === 'rejected') host.appendChild(el('div', 'banner', 'Connection actions unavailable: ' + (guidedRead.reason?.message || guidedRead.reason)));
  const guided = new Map(integrations.map(integration => [integration.runtime, integration]));
  // Memory recall, per runtime that has a memory hook (team rest-of-release plan
  // §6.3, §14 Q25). A failed read leaves the rows without the control.
  const recall = new Map((recallRead.status === 'fulfilled' ? recallRead.value.runtimes || [] : []).map(state => [state.runtime, state]));
  for (const runtime of status.runtimes || []) {
    host.appendChild(runtimeRow(host, runtime, guided.get(runtime.name), { ...status, recall: recall.get(runtime.name) }));
  }
  if (status.observed_at) host.appendChild(el('div', 'sub runtime-observed', 'Hook events read ' + fmtTime(status.observed_at)));
  if (status.live_unavailable) host.appendChild(el('div', 'banner', 'Stored hook events cannot be read right now; only what is configured is shown.'));
}

// ladderSteps is the four stored facts of a runtime, in order. A fact the
// daemon cannot report is neutral: neither met nor missing.
function ladderSteps(runtime) {
  return [
    ['Installed', runtime.presence_reported ? Boolean(runtime.installed) : null],
    ['Hook connected', Boolean(runtime.hook_configured && runtime.hook_binary_present)],
    ['Hook fired', Boolean(runtime.last_live_at)],
    ['Denial returned', Boolean(runtime.canary_at)],
  ];
}

function runtimeLadder(runtime) {
  const ladder = el('div', 'runtime-ladder');
  const steps = ladderSteps(runtime);
  // The next missing step is marked only once an earlier one is met.
  const reached = steps.some(([, met]) => met === true);
  const next = reached ? steps.findIndex(([, met]) => met === false) : -1;
  steps.forEach(([label, met], index) => {
    const state = met === true ? 'met' : (met === null ? 'unreported' : (index === next ? 'next' : 'unmet'));
    const step = el('span', 'runtime-step runtime-step-' + state, label + (met === null ? ' · not reported' : ''));
    ladder.appendChild(step);
  });
  return ladder;
}

function runtimeRow(host, runtime, integration, status) {
  const row = el('section', 'runtime-row');
  row.dataset.runtime = runtime.name;
  const head = el('button', 'runtime-row-head');
  head.type = 'button';
  const title = el('span', 'runtime-row-name', runtime.display_name || runtime.name);
  const when = el('span', 'sub runtime-row-when', [
    runtime.last_live_at ? 'event ' + fmtTime(runtime.last_live_at) : '',
    runtime.canary_at ? 'denial ' + fmtTime(runtime.canary_at) : '',
  ].filter(Boolean).join(' · '));
  head.append(title, runtimeLadder(runtime), when);
  const body = el('div', 'runtime-row-body');
  const setOpen = value => {
    body.classList.toggle('hidden', !value);
    head.setAttribute('aria-expanded', String(value));
  };
  head.onclick = () => {
    const open = body.classList.contains('hidden');
    recordRowChoice(ownerRows, runtime.name, open);
    setOpen(open);
  };
  // Acting inside a row is choosing to have it open: a row that opened for a
  // problem must not close under the owner once their repair clears it.
  body.addEventListener('click', event => {
    if (event.target.closest?.('button')) recordRowChoice(ownerRows, runtime.name, true);
  });
  setOpen(runtimeRowOpen({ name: runtime.name, target: requestedRuntime, attention: runtime.attention }));
  // Memory recall sits directly under the runtime's header line, above its hook facts.
  if (status.recall) {
    body.appendChild(recallControl({ state: status.recall, runtimeLabel: () => runtime.display_name || runtime.name }));
  }
  // A guided connection says its own problem on its card below; the row says
  // it only for a runtime that has none.
  if (runtime.attention && !(integration && integration.problem)) {
    body.appendChild(el('div', runtime.attention === 'never_fired' ? 'sub' : 'sub runtime-integration-problem',
      runtime.problem || RUNTIME_ATTENTION_WORDS[runtime.attention] || runtime.attention));
  }
  body.appendChild(runtimeFacts(runtime, status));
  if (integration) body.appendChild(runtimeConnectionCard(host, integration));
  else if (!runtime.hook_configured && (runtime.installed || !runtime.presence_reported)) {
    body.appendChild(el('div', 'sub', 'Connect it with the installer: crossing-guard init.'));
  }
  row.append(head, body);
  return row;
}

function runtimeFacts(runtime, status) {
  const facts = el('dl', 'runtime-facts');
  const fact = (label, value) => {
    if (!value) return;
    facts.append(el('dt', '', label), el('dd', '', value));
  };
  fact('Hook file', runtime.config_path);
  fact('Hook', !runtime.hook_configured ? '' : (runtime.hook_current ? 'current' : 'does not match this Crossing Guard'));
  fact('Collection only', runtime.collection_only ? 'it records what the runtime does; it cannot stop it' : '');
  const phases = Object.entries(runtime.hook_phases || {});
  fact('Hook phases', phases.length ? phases.map(([name, on]) => name + (on ? '' : ' (missing)')).join(', ') : '');
  fact('Interface checked against', runtime.interface_revision ? 'CLI ' + runtime.interface_revision + ' — the installed version may differ' : '');
  fact('Governance', runtime.governance_lane ? runtime.governance_lane + ' lane installed' + (runtime.governance_note ? ' — ' + runtime.governance_note : '') : '');
  // Said only when the daemon checked the rulebook and it would not deny the canary.
  fact('Denial check', runtime.hook_configured && !runtime.canary_at && status.canary_rule_checked && !status.canary_rule_active
    ? 'the active rulebook does not deny the harmless canary, so a denial cannot be shown' : '');
  return facts;
}

function runtimeConnectionCard(host, integration) {
  const card = el('section', 'runtime-integration');
  if (CHIP_STATES.has(integration.state)) {
    const head = el('div', 'row');
    head.append(el('span', 'chip ' + connectionStateClass(integration.state),
      CONNECTION_STATE_LABELS[integration.state] || integration.state.replaceAll('_', ' ')));
    card.appendChild(head);
  }
  card.appendChild(el('div', 'sub', integration.configLabel + ': ' + integration.configPath));
  if (integration.problem) card.appendChild(el('div', 'sub runtime-integration-problem', integration.problem));
  const limitations = el('ul', 'runtime-integration-limitations');
  integration.limitations.forEach(item => limitations.appendChild(el('li', '', item)));
  if (limitations.childNodes.length) card.appendChild(limitations);

  const actions = el('div', 'row runtime-integration-actions');
  const canDisconnect = integration.attached || integration.consented;
  const canConnect = integration.detected && integration.state !== 'preview_blocked' &&
    (!integration.current || !integration.consented);
  if (canConnect) {
    const connectLabel = integration.attached && !integration.consented ? 'Review existing hook'
      : (integration.attached ? 'Preview repair' : 'Preview connection');
    const connect = el('button', 'btn primary', connectLabel);
    connect.onclick = () => showRuntimeConnectionPreview(host, card, integration, 'connect');
    actions.appendChild(connect);
  } else if (integration.state === 'preview_blocked') {
    const review = el('button', 'btn', 'Review connection');
    review.onclick = () => showRuntimeConnectionPreview(host, card, integration, 'connect');
    actions.appendChild(review);
  }
  if (canDisconnect) {
    const disconnect = el('button', 'btn', 'Preview disconnect');
    disconnect.onclick = () => showRuntimeConnectionPreview(host, card, integration, 'disconnect');
    actions.appendChild(disconnect);
  }
  if (integration.consented && integration.attached && integration.current && integration.binaryPresent && integration.surfaces.length) {
    const verify = el('button', 'btn', 'Watch it block…');
    verify.onclick = () => showRuntimeVerification(host, card, integration);
    actions.appendChild(verify);
  }
  if (!integration.detected && !canDisconnect) {
    actions.appendChild(el('span', 'sub', 'Install the provider first; Crossing Guard will not install or launch it.'));
  }
  card.appendChild(actions);
  return card;
}

// accountFields is one runtime's custom-provider fields; a field the runtime
// does not declare is not shown.
function accountFields(capability, account) {
  const wrap = el('div', 'settings-fields');
  const input = (key, label, placeholder, type) => {
    const node = el('input');
    node.placeholder = placeholder; node.value = account[key] || '';
    if (type) node.type = type;
    node.oninput = () => { account[key] = node.value; };
    wrap.appendChild(lblWrap(label, node));
  };
  if (capability?.supportsBaseURL) input('base_url', 'base URL', 'custom provider base URL');
  if (capability?.supportsAuthToken) input('auth_token', 'token', 'custom provider token', 'password');
  input('binary', 'binary', 'binary path');
  input('extra_args', 'extra args', 'extra args');
  return wrap;
}

// accountTabs is one tab per runtime over that runtime's account fields.
function accountTabs(capabilities, accounts, first) {
  const tabs = el('div', 'settings-tabs');
  const fields = el('div');
  const show = runtime => {
    for (const tab of tabs.children) tab.setAttribute('aria-pressed', String(tab.dataset.runtime === runtime));
    fields.replaceChildren(accountFields(findChatCapability(capabilities, runtime), accounts[runtime]));
  };
  for (const capability of capabilities) {
    const tab = el('button', 'settings-tab', capability.displayName);
    tab.type = 'button'; tab.dataset.runtime = capability.runtime;
    tab.onclick = () => show(capability.runtime);
    tabs.appendChild(tab);
  }
  show(first);
  return [tabs, fields];
}

// renderChatDefaults is what this browser uses for a new chat: the default
// runtime and folder, and each runtime's custom-provider account. It lives in
// this browser's storage and nowhere else.
async function renderChatDefaults(main) {
  const card = el('section', 'settings-card');
  card.append(el('h3', '', 'New chats from this browser'), el('div', 'sub', 'Stored in this browser only.'));
  main.appendChild(card);
  const d = getDefaults();
  const capabilityLoading = el('div', 'sub', 'Loading registered chat runtimes…');
  card.appendChild(capabilityLoading);
  let capabilities;
  try {
    capabilities = await loadChatCapabilities();
  } catch (error) {
    if (capabilityLoading.isConnected) capabilityLoading.replaceWith(el('div', 'banner', 'Chat defaults unavailable: ' + (error.message || error)));
    return;
  }
  if (!capabilityLoading.isConnected) return;
  capabilityLoading.remove();
  const selected = chooseChatCapability(capabilities, d.runtime);
  const accounts = Object.fromEntries(capabilities.map(capability => {
    const value = runtimeChatDefaults(d, capability.runtime);
    return [capability.runtime, { base_url: value.base_url || '', auth_token: value.auth_token || '',
      binary: value.binary || '', extra_args: value.extra_args || '' }];
  }));
  const defRuntime = mkSelectKV(capabilityPairs(capabilities), selected.runtime);
  const defCwd = el('input'); defCwd.placeholder = 'working directory'; defCwd.value = d.cwd || '';
  const row = el('div', 'settings-fields');
  row.append(lblWrap('runtime', defRuntime), lblWrap('working dir', defCwd));
  card.appendChild(row);

  card.appendChild(el('h4', '', 'Custom provider accounts'));
  card.append(...accountTabs(capabilities, accounts, selected.runtime));
  // Say where the token goes. It is persisted so it stops being retyped, and
  // localStorage is not a secret store — stating that is the whole point.
  card.appendChild(el('div', 'sub',
    'Saved in this browser\u2019s localStorage, unencrypted, so it survives reloads. ' +
    'Anything with access to this browser profile can read it. Only runtimes that declare custom-provider fields receive these values; leave them empty to use the harness\u2019s own auth.'));
  const save = el('button', 'btn primary', 'Save defaults');
  save.type = 'button';
  save.onclick = () => {
    const previous = getDefaults();
    // Merge per runtime: a runtime's stored object may hold more than these
    // account fields, and replacing it wholesale would drop them (P-RT2).
    const runtimes = { ...(previous.runtimes || {}) };
    for (const [runtime, account] of Object.entries(accounts)) runtimes[runtime] = { ...(runtimes[runtime] || {}), ...account };
    setDefaults({ ...previous, runtime: defRuntime.value, cwd: defCwd.value, runtimes });
    save.textContent = 'Saved ✓'; setTimeout(() => save.textContent = 'Save defaults', 1200);
  };
  card.appendChild(save);
}

function showRuntimeVerification(host, card, integration) {
  const existing = card.querySelector('.runtime-integration-verification');
  if (existing) { existing.remove(); return; }
  const panel = el('div', 'runtime-integration-preview runtime-integration-verification');
  panel.appendChild(el('strong', '', 'Manual verification'));
  const steps = el('ol', 'runtime-integration-summary');
  integration.verificationSteps.forEach(step => steps.appendChild(el('li', '', step)));
  panel.appendChild(steps);
  const surface = document.createElement('select');
  integration.surfaces.forEach(option => {
    const node = document.createElement('option'); node.value = option.id; node.textContent = option.label; surface.appendChild(node);
  });
  const controls = el('div', 'row');
  const start = el('button', 'btn primary', 'Start watching');
  const close = el('button', 'btn', 'Close'); close.onclick = () => panel.remove();
  controls.append(lblWrap('surface label', surface), start, close); panel.appendChild(controls);
  const output = el('div', 'runtime-integration-watch'); panel.appendChild(output);
  output.setAttribute('aria-live', 'polite'); output.setAttribute('aria-atomic', 'false');
  start.onclick = async () => {
    start.disabled = true; surface.disabled = true;
    output.replaceChildren(el('div', 'sub', 'Starting a local event watch…'));
    try {
      const watch = await startRuntimeIntegrationWatch(integration.runtime, surface.value);
      if (!panel.isConnected) return;
      renderRuntimeWatchInstructions(output, watch);
      pollRuntimeWatch(host, panel, output, integration, watch.watch_token, () => {
        if (!panel.isConnected) return;
        start.disabled = false; surface.disabled = false; start.textContent = 'Start new watch';
      });
    } catch (error) {
      start.disabled = false; surface.disabled = false;
      output.replaceChildren(el('div', 'banner', 'Watch unavailable: ' + (error.message || error)));
    }
  };
  card.appendChild(panel);
}

function renderRuntimeWatchInstructions(output, watch) {
  output.replaceChildren();
  output.appendChild(el('div', 'sub', watch.note || 'Watching only the local Crossing Guard event log.'));
  output.appendChild(el('div', 'sub', 'Current-watch evidence is temporary and is not saved as provider verification. ' +
    (watch.expires_at ? 'This watch expires at ' + fmtTime(watch.expires_at) + '.' : 'It expires with this daemon watch.')));
  output.appendChild(el('div', 'sub', 'Surface: ' + String(watch.surface_label || watch.surface) +
    ' (' + String(watch.surface_evidence || 'user-selected') + ')'));
  if (watch.canary_available && watch.canary_command) {
    output.appendChild(el('div', 'sub', 'Optional harmless denial command (safe even if the hook does not fire):'));
    output.appendChild(el('code', 'runtime-integration-canary', String(watch.canary_command)));
  } else {
    output.appendChild(el('div', 'sub', 'The active rulebook does not hard-deny the shipped harmless canary. This watch can prove decision-hook firing only.'));
  }
  const state = el('div', 'sub runtime-integration-watch-state', 'Waiting for a new provider tool event…');
  state.setAttribute('role', 'status');
  output.appendChild(state);
}

function pollRuntimeWatch(host, panel, output, integration, token, onStopped) {
  const poll = async () => {
    if (!panel.isConnected) return;
    try {
      const result = await readRuntimeIntegrationWatch(integration.runtime, token);
      if (!panel.isConnected) return;
      const state = output.querySelector('.runtime-integration-watch-state');
      if (state) state.textContent = runtimeWatchMessage(result);
      if (result.status === 'denial_returned') {
        let confirm = output.querySelector('.runtime-integration-confirm-visible');
        if (!confirm) {
          confirm = el('button', 'btn primary runtime-integration-confirm-visible',
            'I personally saw ' + integration.displayName + ' block it');
          confirm.onclick = async () => {
            confirm.disabled = true;
            try {
              const confirmed = await confirmRuntimeIntegrationVisibleBlock(integration.runtime, token);
              const current = output.querySelector('.runtime-integration-watch-state');
              if (current) current.textContent = runtimeWatchMessage(confirmed);
              confirm.remove();
            } catch (error) {
              confirm.disabled = false;
              output.appendChild(el('div', 'banner', 'Confirmation failed: ' + (error.message || error)));
            }
          };
          output.appendChild(confirm);
        }
      }
      if (result.status === 'needs_attention') {
        output.querySelector('.runtime-integration-confirm-visible')?.remove();
        try {
          await refreshRuntimeConnections(host, result.note || 'The runtime connection changed; review its current status.');
        } catch (error) {
          if (panel.isConnected) {
            output.appendChild(el('div', 'banner', 'Current connection status could not be refreshed: ' + (error.message || error)));
            onStopped();
          }
        }
      } else if (result.status !== 'enforcement_verified') {
        setTimeout(poll, 1500);
      }
    } catch (error) {
      if (panel.isConnected) {
        output.querySelector('.runtime-integration-confirm-visible')?.remove();
        output.appendChild(el('div', 'banner', 'Watch stopped: ' + (error.message || error)));
        onStopped();
      }
    }
  };
  setTimeout(poll, 700);
}

function runtimeWatchMessage(result) {
  switch (result.status) {
  case 'decision_firing_observed': return 'Decision hook firing observed. This does not yet prove visible provider blocking.';
  case 'denial_returned': return 'Crossing Guard returned and stored the harmless denial. Confirm only if you saw the provider block it.';
  case 'enforcement_verified': return 'Current watch verified for this owner-selected surface: denial recorded and visible blocking owner-confirmed. This evidence is not saved to the connection status.';
  case 'needs_attention': return result.note || 'The connection changed; start a new watch after reviewing its status.';
  default: return result.note || 'Waiting for a new provider tool event…';
  }
}

async function showRuntimeConnectionPreview(host, card, integration, operation) {
  const existing = card.querySelector('.runtime-integration-preview');
  if (existing) existing.remove();
  const panel = el('div', 'runtime-integration-preview');
  panel.appendChild(el('div', 'sub', 'Preparing a read-only preview…'));
  card.appendChild(panel);
  try {
    const response = await previewRuntimeIntegration(integration.runtime, operation);
    if (!panel.isConnected) return;
    const preview = response.preview;
    panel.replaceChildren();
    panel.appendChild(el('strong', '', operation === 'connect' ? 'Connection preview' : 'Disconnect preview'));
    panel.appendChild(el('div', 'sub', 'File: ' + String(preview.config_path || integration.configPath)));
    if (operation === 'connect') panel.appendChild(el('div', 'sub', 'Hook binary: ' + String(preview.hook_binary || 'unavailable')));
    const phases = Array.isArray(preview.hook_phases) ? preview.hook_phases : integration.hookPhases;
    if (phases.length) panel.appendChild(el('div', 'sub', 'Hook phases: ' + phases.map(String).join(', ')));
    const consentEffect = operation === 'connect'
      ? (preview.consent_present
        ? 'Existing durable connection consent remains in place.'
        : 'Connect will record durable connection consent for this provider after the hook change succeeds.')
      : (preview.consent_present
        ? 'Disconnect will remove durable connection consent before removing the owned hooks.'
        : 'No durable connection consent is present; disconnect only removes any owned hooks.');
    panel.appendChild(el('div', 'sub', consentEffect));
    const summary = el('ul', 'runtime-integration-summary');
    (Array.isArray(preview.summary) ? preview.summary : []).forEach(item => summary.appendChild(el('li', '', String(item))));
    panel.appendChild(summary);
    const fileEffect = preview.will_create_file
      ? 'The provider hook file will be created; no backup exists for a new file.'
      : (preview.will_create_backup
        ? 'An immediate-prior private backup will be written before the change.'
        : 'No file creation or backup is needed for this operation.');
    panel.appendChild(el('div', 'sub', fileEffect));
    const confirmRow = el('div', 'row');
    const cancel = el('button', 'btn', 'Cancel'); cancel.onclick = () => panel.remove();
    const confirm = el('button', operation === 'connect' ? 'btn primary' : 'btn danger',
      (operation === 'connect' ? 'Connect ' : 'Disconnect ') + integration.displayName);
    confirm.onclick = async () => {
      confirm.disabled = true; cancel.disabled = true;
      try {
        const result = await commitRuntimeIntegration(integration.runtime, operation, response.preview_token);
        const note = result.detail || (operation === 'connect' ? 'Runtime connected.' : 'Runtime disconnected.');
        await refreshRuntimeConnectionsWithRecovery(host, panel, note);
      } catch (error) {
        await refreshRuntimeConnectionsWithRecovery(host, panel, runtimeMutationFailureMessage(error));
      }
    };
    confirmRow.append(cancel, confirm); panel.appendChild(confirmRow);
  } catch (error) {
    if (!panel.isConnected) return;
    panel.replaceChildren(el('div', 'banner', 'Preview unavailable: ' + (error.message || error)));
  }
}

function runtimeMutationFailureMessage(error) {
  const result = error?.result;
  const effects = [];
  if (result?.config_changed) effects.push('provider hook configuration changed');
  if (result?.consent_changed) effects.push('connection consent changed');
  if (result?.residual_hook) effects.push('an owned hook may remain, with automatic repair disabled');
  const suffix = effects.length ? ' Local outcome: ' + effects.join('; ') + '.' : '';
  return 'Operation needs attention: ' + (error?.message || error) + suffix;
}

async function refreshRuntimeConnectionsWithRecovery(host, panel, notice) {
  try {
    await refreshRuntimeConnections(host, notice);
  } catch (error) {
    if (!panel.isConnected) return;
    panel.replaceChildren(el('div', 'banner', notice),
      el('div', 'banner', 'The operation may have changed local state, but current status could not be refreshed: ' +
        (error.message || error)));
    const retry = el('button', 'btn primary', 'Refresh current status');
    retry.onclick = () => {
      retry.disabled = true;
      refreshRuntimeConnectionsWithRecovery(host, panel, notice);
    };
    panel.appendChild(retry);
  }
}

function connectionStateClass(state) {
  if (state === 'enforcement_verified') return 'st-verified';
  if (state === 'needs_attention' || state === 'preview_blocked') return 'st-disputed';
  if (state === 'connected_unverified' || state === 'denial_returned') return 'st-stale';
  return 'st-draft';
}

export { renderRuntimesPage, ladderSteps, runtimeRowOpen, recordRowChoice };
