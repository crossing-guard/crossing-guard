import { $, el, cpHeaders, api, fmtTime, normMemId, escapeHtml, mdInline, mdToHtml, debounce, fillSelect, mkSelectKV, mkSelect, lblWrap, fmtTok, shortWhen, SEV_CHIP, WATER_ORDER, CLASS_CHIP, getDefaults, setDefaults } from "../core.js";
import { applyTheme, mkMark, mkLoader, mkSkeletons, withState, attachBottomPill, toggleHelp } from "../ui.js";
import { loadChatCapabilities, chooseChatCapability, findChatCapability, capabilityPairs, runtimeChatDefaults } from "../chat-capabilities.js";
import { loadRuntimeIntegrations, previewRuntimeIntegration, commitRuntimeIntegration,
  startRuntimeIntegrationWatch, readRuntimeIntegrationWatch, confirmRuntimeIntegrationVisibleBlock } from "../runtime-integrations.js";
import { renderAgentsPage } from "../orchestration/settings-agents.js";
import { loadSpeechCapabilities } from "../speech/speech-api.js";

// Minimal subpage mechanism (IMPL-9: none existed to shrink into). One nav row
// toggles two render functions; the choice is in-view state, hash-free. Other
// views may request a subpage before navigating here via `cg:settings-subpage`.
const SETTINGS_SUBPAGES = ['settings', 'agents'];
let settingsSubpage = 'settings';

if (typeof document !== 'undefined') {
  document.addEventListener('cg:settings-subpage', event => {
    const requested = String(event.detail || '');
    if (SETTINGS_SUBPAGES.includes(requested)) settingsSubpage = requested;
  });
}

async function renderSettings() {
  const main = $('#main');
  main.innerHTML = '';
  const nav = el('div', 'settings-subnav');
  const mkTab = (id, label) => {
    const tab = el('button', 'settings-subnav-tab' + (settingsSubpage === id ? ' active' : ''), label);
    tab.type = 'button';
    tab.setAttribute('aria-pressed', String(settingsSubpage === id));
    tab.onclick = () => { if (settingsSubpage !== id) { settingsSubpage = id; void renderSettings(); } };
    return tab;
  };
  nav.append(mkTab('settings', 'Settings'), mkTab('agents', 'Agents'));
  main.appendChild(nav);
  if (settingsSubpage === 'agents') {
    main.appendChild(el('h2', '', 'Agents'));
    await renderAgentsPage(main);
    return;
  }
  await renderGeneralSettings(main);
}

async function renderGeneralSettings(main) {
  main.appendChild(el('h2', '', 'Settings'));
  main.appendChild(el('div', 'sub', 'Appearance and chat defaults stay in this browser. Runtime connections and reusable profiles are stored by the local Crossing Guard daemon and explain their effects before a change.'));

  // appearance
  main.appendChild(el('h2', '', 'Appearance'));
  const themeRow = el('div', 'row');
  const themeSel = mkSelectKV([['auto', 'Auto (match OS)'], ['dark', 'Dark'], ['light', 'Light']],
    localStorage.getItem('cp_theme') || 'auto');
  themeSel.onchange = () => applyTheme(themeSel.value);
  themeRow.append(lblWrap('theme', themeSel));
  main.appendChild(themeRow);

  await renderRuntimeConnections(main);
  await renderSpeechSettings(main);
  // Agents own their whole surface: profile import, binding create/edit, the
  // roster, and run history all live on the Agents subpage.
  main.appendChild(el('div', 'sub settings-agents-pointer',
    'Agents (top of this page): every deployed agent — prompt, powers, budgets, run history — plus profile import and deployment.'));

  // chat defaults
  main.appendChild(el('h2', '', 'Chat defaults'));
  const d = getDefaults();
  const capabilityLoading = el('div', 'sub', 'Loading registered chat runtimes…');
  main.appendChild(capabilityLoading);
  let capabilities;
  try {
    capabilities = await loadChatCapabilities();
  } catch (error) {
    if (capabilityLoading.isConnected) capabilityLoading.replaceWith(el('div', 'banner', 'Chat defaults unavailable: ' + (error.message || error)));
    return;
  }
  if (!capabilityLoading.isConnected) return;
  capabilityLoading.remove();
  // Interface revision per runtime (natural-session plan, Slice A): names the
  // provider CLI the product's interface was verified against. The installed
  // version is NOT claimed here — that evidence belongs to runtime discovery.
  for (const capability of capabilities) {
    if (capability.interfaceRevision) {
      main.appendChild(el('div', 'sub', capability.displayName
        + ' — interface verified against CLI ' + capability.interfaceRevision
        + '; the installed version may differ.'));
    }
    // Governance lane (Slice C): a version-gated runtime says which lane its
    // installed hook actually got. A collection-only fallback and its reason
    // must be visible here — never a silently implied enforcement.
    if (capability.governanceLane) {
      let line = capability.displayName + ' governance: ' + capability.governanceLane + ' lane installed';
      if (capability.governanceNote) line += ' — ' + capability.governanceNote;
      main.appendChild(el('div', 'sub', line));
    }
  }
  const row = el('div', 'row');
  const selected = chooseChatCapability(capabilities, d.runtime);
  const accounts = Object.fromEntries(capabilities.map(capability => {
    const value = runtimeChatDefaults(d, capability.runtime);
    return [capability.runtime, { base_url: value.base_url || '', auth_token: value.auth_token || '',
      binary: value.binary || '', extra_args: value.extra_args || '' }];
  }));
  let currentRuntime = selected.runtime;
  const currentAccount = accounts[currentRuntime];
  const defRuntime = mkSelectKV(capabilityPairs(capabilities), selected.runtime);
  const defBase = el('input'); defBase.placeholder = 'custom provider base URL'; defBase.style.width = '250px'; defBase.value = currentAccount.base_url;
  const defCwd = el('input'); defCwd.placeholder = 'working directory'; defCwd.style.width = '260px'; defCwd.value = d.cwd || '';
  // Moved here from the composer's per-message drawer (§11): this is
  // machine/account config, entered once. The token was a password field that
  // was never persisted, so it had to be retyped for every single chat.
  const defToken = el('input'); defToken.placeholder = 'custom provider token';
  defToken.type = 'password'; defToken.style.width = '190px'; defToken.value = currentAccount.auth_token;
  const defBinary = el('input'); defBinary.placeholder = 'binary path'; defBinary.style.width = '170px'; defBinary.value = currentAccount.binary;
  const defExtra = el('input'); defExtra.placeholder = 'extra args'; defExtra.style.width = '130px'; defExtra.value = currentAccount.extra_args;
  const baseField = lblWrap('base URL', defBase);
  const tokenField = lblWrap('token', defToken);
  row.append(lblWrap('runtime', defRuntime), baseField,
    tokenField, lblWrap('working dir', defCwd),
    lblWrap('binary', defBinary), lblWrap('extra args', defExtra));
  const syncRuntimeFields = () => {
    const capability = findChatCapability(capabilities, defRuntime.value);
    baseField.classList.toggle('hidden', !capability?.supportsBaseURL);
    tokenField.classList.toggle('hidden', !capability?.supportsAuthToken);
  };
  const captureAccount = () => {
    accounts[currentRuntime] = { base_url: defBase.value, auth_token: defToken.value,
      binary: defBinary.value, extra_args: defExtra.value };
  };
  defRuntime.onchange = () => {
    captureAccount(); currentRuntime = defRuntime.value;
    const account = accounts[currentRuntime];
    defBase.value = account.base_url; defToken.value = account.auth_token;
    defBinary.value = account.binary; defExtra.value = account.extra_args;
    syncRuntimeFields();
  };
  syncRuntimeFields();
  main.appendChild(row);
  // Say where the token goes. It is persisted so it stops being retyped, and
  // localStorage is not a secret store — stating that is the whole point.
  main.appendChild(el('div', 'sub',
    'Saved in this browser\u2019s localStorage, unencrypted, so it survives reloads. ' +
    'Anything with access to this browser profile can read it. Only runtimes that declare custom-provider fields receive these values; leave them empty to use the harness\u2019s own auth.'));
  const save = el('button', 'btn primary', 'Save defaults');
  save.onclick = () => {
    captureAccount();
    const previous = getDefaults();
    setDefaults({ ...previous, runtime: defRuntime.value, cwd: defCwd.value,
      runtimes: { ...(previous.runtimes || {}), ...accounts } });
    save.textContent = 'Saved ✓'; setTimeout(() => save.textContent = 'Save defaults', 1200);
  };
  main.appendChild(save);

  // security status (honesty: state what is and isn't protected)
  main.appendChild(el('h2', '', 'Security'));
  const tok = localStorage.getItem('cg_token') || localStorage.getItem('cp_token');
  main.appendChild(el('div', 'sub',
    (tok ? '✓ API token active (from startup URL). ' : '✖ No API token in this browser — open the URL printed at daemon startup. ')
    + 'Listener is loopback-only with an Origin check. Caller-supplied binary paths are not yet allowlisted (probe limit).'));

  // about
  main.appendChild(el('h2', '', 'About'));
  main.appendChild(el('div', 'sub',
    'consoleprobe — experiment, not product. Design: docs/console-design-contract.md · Target: docs/console-target-features.md · Keyboard: ? for shortcuts · / search · ⌘/Ctrl+O expand chips · Esc stops/clears.'));
}

const CONNECTION_STATE_LABELS = Object.freeze({
  not_detected: 'Not detected', detected: 'Detected · not connected',
  preview_blocked: 'Review required', connected_unverified: 'Connected · no saved verification',
  needs_attention: 'Needs attention', firing_observed: 'Hook firing observed',
  decision_firing_observed: 'Decision hook observed', denial_returned: 'Denial returned',
  enforcement_verified: 'Enforcement verified',
});

// Speech renders what speech.json says and whether it loaded. Nothing here is
// editable: the file is the owner (ADR 0026), and this page points at it.
async function renderSpeechSettings(main) {
  main.appendChild(el('h2', '', 'Speech'));
  const host = el('div', 'settings-speech');
  main.appendChild(host);
  let caps;
  try { caps = await loadSpeechCapabilities(); }
  catch (error) { host.appendChild(el('div', 'banner', 'Speech settings unavailable: ' + (error.message || error))); return; }
  // An open chat tab refreshes its mic state whenever this page reloads capabilities.
  document.dispatchEvent(new CustomEvent('cg:speech-capabilities-changed'));
  const row = (label, node, note, bad) => {
    const line = el('div', 'row');
    const value = el('div');
    value.appendChild(typeof node === 'string' ? el('span', 'field', node) : node);
    if (note) value.appendChild(el('div', bad ? 'problem' : 'ok', note));
    line.append(el('span', 'k', label), value);
    host.appendChild(line);
  };
  if (caps.state === 'unconfigured') {
    row('Dictation', 'not set up', caps.reason || 'speech.json is missing from the data directory', true);
    host.appendChild(el('div', 'sub', 'Start from docs/public/reference/config/speech.example.json, set the model path and checksum, copy it to the data directory as speech.json, and restart the daemon.'));
    return;
  }
  const backendName = caps.backend?.backend || (caps.problems?.length ? 'selected in speech.json' : '');
  row('Dictation backend', backendName || 'unknown', caps.state === 'ready' ? 'ready' : (caps.reason || 'not ready'), caps.state !== 'ready');
  row('Configuration file', caps.config_path || '', (caps.problems || []).join(' · ') || 'loaded', (caps.problems || []).length > 0);
  for (const fact of caps.backend?.facts || []) row(fact.label, fact.value, fact.note || '', false);
  if (caps.disclosure) row('Before audio leaves', caps.disclosure.text, 'disclosure version ' + caps.disclosure.version, false);
  if (caps.hints) {
    const on = [caps.hints.project_name && 'project name', caps.hints.branch_name && 'branch name', (caps.hints.keywords || []).length && 'keyword list'].filter(Boolean);
    row('Hints sent with audio', on.length ? on.join(', ') : 'none', (caps.hints.keywords || []).length ? 'keywords: ' + caps.hints.keywords.join(', ') : '', false);
  }
  if (caps.ui) {
    row('Push-to-talk', caps.ui.push_to_talk, 'hold to talk · click the mic to toggle · Esc cancels', false);
    row('Auto-send', caps.ui.auto_send ? 'on, ' + caps.ui.auto_send_min_words + '+ words' : 'off', '', false);
  }
  if (caps.limits) {
    row('Limits', 'up to ' + caps.limits.max_seconds + ' s · stops after ' + caps.limits.silence_stop_seconds + ' s of silence · ' + caps.limits.max_concurrent + ' at a time', '', false);
  }
}

async function renderRuntimeConnections(main) {
  main.appendChild(el('h2', '', 'Runtime connections'));
  main.appendChild(el('div', 'sub',
    'Detection is read-only. Connecting changes only the previewed provider hook file; Crossing Guard never opens or controls the provider.'));
  const host = el('div', 'runtime-integrations');
  const loading = el('div', 'sub', 'Checking registered runtime connections…');
  host.appendChild(loading); main.appendChild(host);
  try {
    await refreshRuntimeConnections(host);
  } catch (error) {
    loading.replaceWith(el('div', 'banner', 'Runtime connections unavailable: ' + (error.message || error)));
  }
}

async function refreshRuntimeConnections(host, notice = '') {
  const integrations = await loadRuntimeIntegrations(true);
  if (!host.isConnected) return;
  host.replaceChildren();
  if (notice) host.appendChild(el('div', 'banner', notice));
  if (!integrations.length) {
    host.appendChild(el('div', 'sub', 'No runtime currently advertises a guided connection experience.'));
    return;
  }
  integrations.forEach(integration => host.appendChild(runtimeConnectionCard(host, integration)));
}

function runtimeConnectionCard(host, integration) {
  const card = el('section', 'runtime-integration');
  const head = el('div', 'row');
  head.append(el('strong', '', integration.displayName),
    el('span', 'chip ' + connectionStateClass(integration.state),
      CONNECTION_STATE_LABELS[integration.state] || integration.state.replaceAll('_', ' ')));
  card.appendChild(head);
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
    const verify = el('button', 'btn', 'Verify manually');
    verify.onclick = () => showRuntimeVerification(host, card, integration);
    actions.appendChild(verify);
  }
  if (!integration.detected && !canDisconnect) {
    actions.appendChild(el('span', 'sub', 'Install the provider first; Crossing Guard will not install or launch it.'));
  }
  card.appendChild(actions);
  return card;
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

export { renderSettings };
