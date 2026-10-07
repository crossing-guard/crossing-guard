import { el } from '../core.js';
import { previewProfile, selectProfile } from './profile-api.js';

const MAX_PROFILE_BYTES = 262144;

// renderProfileImporter is the one PROFILE.md import flow (preview → select),
// hosted by the Agents page's Import dialog. The browser sends the file's
// exact bytes, never its path; onImported receives the select result.
function renderProfileImporter(host, onImported = async () => {}) {
  host.appendChild(buildImporter(onImported));
}

function buildImporter(onImported) {
  const card = el('section', 'orchestration-profile-import');
  const picker = document.createElement('input');
  picker.type = 'file'; picker.accept = '.md,text/markdown,text/plain';
  picker.className = 'orchestration-profile-file';
  picker.setAttribute('aria-label', 'Choose PROFILE.md');
  const status = el('div', 'sub orchestration-profile-status');
  status.setAttribute('role', 'status'); status.setAttribute('aria-live', 'polite');
  const previewHost = el('div', 'orchestration-profile-preview');
  card.append(picker, status, previewHost);
  picker.onchange = async () => {
    previewHost.replaceChildren();
    const file = picker.files?.[0];
    status.textContent = fileProblem(file);
    if (!file || status.textContent) return;
    status.textContent = 'Reading PROFILE.md…';
    try {
      const bytes = new Uint8Array(await file.arrayBuffer());
      const response = await previewProfile(file.name, bytes);
      if (!card.isConnected) return;
      status.textContent = '';
      renderPreview(previewHost, picker, file.name, bytes, response.preview, onImported);
    } catch (error) {
      if (card.isConnected) status.textContent = profileErrorMessage('This file cannot be imported', error);
    }
  };
  return card;
}

function fileProblem(file) {
  if (!file) return '';
  if (file.name !== 'PROFILE.md') return 'Choose a file named exactly PROFILE.md.';
  if (file.size > MAX_PROFILE_BYTES) return 'PROFILE.md must be 262144 bytes or smaller.';
  return '';
}

function renderPreview(previewHost, picker, sourceName, bytes, preview, onSelected) {
  previewHost.replaceChildren();
  const panel = el('section', 'orchestration-profile-preview-card');
  panel.appendChild(el('strong', '', preview.normalized?.name || preview.profile_id));
  panel.appendChild(el('div', 'sub', preview.profile_id + ' · version ' + String(preview.normalized?.version || '')));
  const facts = el('div', 'orchestration-profile-facts');
  facts.append(fact('When it runs', preview.when), factList('What it reads', preview.sees), fact('What it returns', preview.does),
    fact('Where data goes', preview.destination), factList('Ceilings', preview.limits),
    factList('Asks permission to', preview.requested_authority));
  panel.appendChild(facts);
  const identity = document.createElement('details');
  const summary = document.createElement('summary'); summary.textContent = 'Identity';
  identity.append(summary, digestLine('Source', preview.source_digest), digestLine('Compiled', preview.bundle_digest),
    jsonDetails('Normalized typed value', preview.normalized));
  panel.appendChild(identity);
  const actions = el('div', 'row orchestration-profile-actions');
  const select = el('button', 'btn primary', 'Import');
  const cancel = el('button', 'btn', 'Cancel');
  cancel.onclick = () => { previewHost.replaceChildren(); picker.value = ''; };
  select.onclick = () => selectPreviewed({ panel, select, cancel, previewHost, picker, sourceName, bytes, preview, onSelected });
  actions.append(cancel, select); panel.appendChild(actions); previewHost.appendChild(panel);
}

async function selectPreviewed({ panel, select, cancel, previewHost, picker, sourceName, bytes, preview, onSelected }) {
  select.disabled = true; cancel.disabled = true;
  try {
    const result = await selectProfile(sourceName, bytes, preview);
    if (!panel.isConnected) return;
    picker.value = '';
    await onSelected(result);
  } catch (error) {
    if (!panel.isConnected) return;
    select.disabled = false; cancel.disabled = false;
    const failure = el('div', 'banner orchestration-profile-failure', profileErrorMessage('Not imported', error));
    if (error.code === 'state_conflict') {
      const retry = el('button', 'btn', 'Preview current bytes again');
      retry.onclick = async () => {
        retry.disabled = true;
        try {
          const response = await previewProfile(sourceName, bytes);
          renderPreview(previewHost, picker, sourceName, bytes, response.preview, onSelected);
        } catch (retryError) {
          failure.textContent = profileErrorMessage('This file cannot be imported', retryError);
          retry.disabled = false;
        }
      };
      failure.appendChild(retry);
    }
    panel.prepend(failure);
  }
}

function fact(label, value) {
  const item = el('div', 'orchestration-profile-fact');
  item.append(el('strong', '', label), el('div', 'sub', String(value || 'none')));
  return item;
}

function factList(label, values) {
  return fact(label, Array.isArray(values) && values.length ? values.map(String).join(' · ') : 'none');
}

function digestLine(label, digest) {
  const line = el('div', 'sub orchestration-profile-digest');
  line.append(el('strong', '', label + ': '), el('code', '', String(digest || 'unavailable')));
  return line;
}

function jsonDetails(label, value) {
  const details = document.createElement('details');
  const summary = document.createElement('summary'); summary.textContent = label;
  const content = el('pre', 'orchestration-profile-source');
  content.textContent = JSON.stringify(value ?? null, null, 2);
  details.append(summary, content);
  return details;
}

function profileErrorMessage(prefix, error) {
  const message = String(error?.message || error || 'Unknown error');
  const field = error?.field ? ' Field: ' + String(error.field) + '.' : '';
  const recovery = error?.recovery ? ' ' + String(error.recovery) : '';
  return prefix + ': ' + message + field + recovery;
}

export { renderProfileImporter };
