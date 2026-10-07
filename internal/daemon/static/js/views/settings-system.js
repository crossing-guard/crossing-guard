// Settings › Dictation, Security and About (settings-restructure plan §3.10,
// §3.11). Dictation shows what speech.json says; nothing on it is editable.
import { el, api, getDefaults, setDefaults } from "../core.js";
import { toggleHelp } from "../ui.js";
import { loadSpeechCapabilities } from "../speech/speech-api.js";
import { forgetProviderTokens, storedProviderTokens } from "./settings-model.js";

// Speech renders what speech.json says and whether it loaded. Nothing here is
// editable: the file is the owner (ADR 0026), and this page points at it.
async function renderSpeechSettings(main) {
  const host = el('div', 'settings-speech');
  main.appendChild(host);
  let caps;
  try { caps = await loadSpeechCapabilities(); }
  catch (error) { host.appendChild(el('div', 'banner', 'Dictation settings unavailable: ' + (error.message || error))); return; }
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

// factRow is one row of a facts page: a status dot, what it is, what is true
// of it, and an action when there is one.
function factRow(state, title, detail, action) {
  const row = el('div', 'settings-fact');
  row.append(el('span', 'settings-dot settings-dot-' + state));
  const text = el('div', 'settings-fact-text');
  text.append(el('strong', '', title), el('div', 'sub', detail));
  row.appendChild(text);
  if (action) row.appendChild(action);
  return row;
}

async function loadDaemonVersion() {
  try { return await api('/api/version'); } catch { return null; }
}

// Security: what protects this console, one fact per row.
async function renderSecurityPage(main) {
  const host = el('div', 'settings-card settings-facts');
  main.appendChild(host);
  const version = await loadDaemonVersion();
  if (!host.isConnected) return;
  const paint = () => {
    host.replaceChildren();
    const address = String(version?.listen_addr || '');
    const loopback = /^(127\.|localhost|\[::1\])/.test(address);
    host.appendChild(factRow(address ? (loopback ? 'ok' : 'bad') : 'off', 'Listener',
      address ? address + (loopback ? ' · reachable from this machine only' : ' · reachable from other machines') : 'address unavailable'));
    host.appendChild(factRow('ok', 'Origin check', 'Requests from other web pages are refused.'));
    const token = localStorage.getItem('cg_token') || localStorage.getItem('cp_token');
    host.appendChild(factRow(token ? 'ok' : 'bad', 'This browser\u2019s API token',
      token ? 'Active, from the startup URL.' : 'None in this browser — open the URL printed at daemon startup.',
      token ? signOutButton() : null));
    const stored = storedProviderTokens(getDefaults());
    host.appendChild(factRow(stored ? 'warn' : 'ok', 'Custom provider tokens in this browser',
      stored ? stored + ' stored, unencrypted, in this browser\u2019s storage.' : 'None stored.',
      stored ? forgetButton(paint) : null));
    host.appendChild(factRow('bad', 'Chat can run any program',
      'A chat request names its own binary and folder, and the API token is the only check.'));
  };
  paint();
}

function signOutButton() {
  const button = el('button', 'btn', 'Sign out this browser');
  button.type = 'button';
  button.onclick = () => {
    if (!confirm('Sign out this browser? Every request from it will fail until you open the startup URL again from a terminal.')) return;
    localStorage.removeItem('cg_token');
    localStorage.removeItem('cp_token');
    location.reload();
  };
  return button;
}

function forgetButton(repaint) {
  const button = el('button', 'btn', 'Forget');
  button.type = 'button';
  button.onclick = () => {
    if (!confirm('Forget the custom provider addresses and tokens stored in this browser? Binaries, arguments and pinned models are kept.')) return;
    setDefaults(forgetProviderTokens(getDefaults()));
    repaint();
  };
  return button;
}

// About: the facts that identify this running daemon.
async function renderAboutPage(main) {
  const host = el('div', 'settings-card');
  main.appendChild(host);
  const version = await loadDaemonVersion();
  if (!host.isConnected) return;
  if (!version) { host.appendChild(el('div', 'banner', 'Version facts unavailable.')); return; }
  const facts = [['Product', 'Crossing Guard'], ['Build', version.build_version], ['API', version.version],
    ['Store schema', String(version.store_schema ?? '')], ['Data directory', version.data_dir],
    ['Log', version.log_path], ['Listening on', version.listen_addr]].filter(([, value]) => value);
  const list = el('dl', 'runtime-facts');
  for (const [label, value] of facts) {
    const cell = el('dd', label === 'Product' ? '' : 'mono', value);
    list.append(el('dt', '', label), cell);
  }
  host.appendChild(list);
  const actions = el('div', 'row');
  const copy = el('button', 'btn', 'Copy diagnostics');
  copy.type = 'button';
  copy.onclick = async () => {
    try {
      await navigator.clipboard.writeText(facts.map(([label, value]) => label + ': ' + value).join('\n'));
      copy.textContent = 'Copied ✓';
    } catch { copy.textContent = 'Copy failed'; }
  };
  const keys = el('button', 'btn', 'Keyboard shortcuts');
  keys.type = 'button';
  keys.onclick = () => toggleHelp();
  actions.append(copy, keys);
  host.appendChild(actions);
}

export { renderSpeechSettings, renderSecurityPage, renderAboutPage };
