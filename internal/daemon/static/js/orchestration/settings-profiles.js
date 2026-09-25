import { el, fmtTime } from '../core.js';
import { loadProfile, previewProfile, selectProfile } from './profile-api.js';

const MAX_PROFILE_BYTES = 262144;

// The importer is the Agents page's entry point (G-4: importing a profile
// creates the agent's card — the roster is the page, so there is no separate
// profile list here). onImported lets the page rebuild its roster after a new
// exact revision is selected.
function renderProfileImporter(main, onImported = async () => {}) {
  main.appendChild(el('h2', '', 'Reusable profiles'));
  main.appendChild(el('div', 'sub orchestration-profile-intro',
    'Profiles are reusable instructions only. Selecting one does not run it. Compatible active use requires a separate, visible agent binding below.'));
  main.appendChild(buildImporter(onImported));
}

function buildImporter(onImported) {
  const card = el('section', 'runtime-integration orchestration-profile-import');
  card.appendChild(el('strong', '', 'Import reusable profile'));
  card.appendChild(el('div', 'sub',
    'Choose one local file named PROFILE.md. The browser sends its exact bytes, not its local path. Preview is read-only. Importing creates the agent’s card in the roster below.'));
  const controls = el('div', 'row orchestration-profile-actions');
  const picker = document.createElement('input');
  picker.type = 'file'; picker.accept = '.md,text/markdown,text/plain';
  picker.className = 'orchestration-profile-file';
  picker.setAttribute('aria-label', 'Import PROFILE.md');
  controls.appendChild(picker); card.appendChild(controls);
  const status = el('div', 'sub orchestration-profile-status');
  status.setAttribute('role', 'status'); status.setAttribute('aria-live', 'polite');
  const previewHost = el('div', 'orchestration-profile-preview');
  card.append(status, previewHost);
  picker.onchange = async () => {
    previewHost.replaceChildren();
    const file = picker.files?.[0];
    if (!file) { status.textContent = ''; return; }
    if (file.name !== 'PROFILE.md') {
      status.textContent = 'Import unavailable: choose a file named exactly PROFILE.md.';
      return;
    }
    if (file.size > MAX_PROFILE_BYTES) {
      status.textContent = 'Import unavailable: PROFILE.md must be 262144 bytes or smaller.';
      return;
    }
    status.textContent = 'Preparing a read-only preview…';
    try {
      const bytes = new Uint8Array(await file.arrayBuffer());
      const response = await previewProfile(file.name, bytes);
      if (!card.isConnected) return;
      status.textContent = response.note || 'Preview ready. No profile ran.';
      renderPreview(previewHost, picker, file.name, bytes, response.preview, onImported);
    } catch (error) {
      if (!card.isConnected) return;
      status.textContent = profileErrorMessage('Preview unavailable', error);
    }
  };
  return card;
}

function renderPreview(previewHost, picker, sourceName, bytes, preview, onSelected) {
  previewHost.replaceChildren();
  const panel = el('section', 'runtime-integration-preview orchestration-profile-preview-card');
  panel.appendChild(el('strong', '', 'Synthetic preview · no profile ran'));
  panel.appendChild(el('div', 'sub', preview.profile_id + ' · ' + preview.selection_state.replaceAll('_', ' ')));
  const facts = el('div', 'orchestration-profile-facts');
  facts.append(fact('When', preview.when), factList('Sees', preview.sees), fact('Does', preview.does),
    fact('Appears', preview.appears), fact('Destination', preview.destination), factList('Limits', preview.limits));
  panel.appendChild(facts);
  panel.appendChild(factList('Requested authority · not granted', preview.requested_authority?.length
    ? preview.requested_authority : ['none']));
  panel.appendChild(digestLine('Source identity', preview.source_digest));
  panel.appendChild(digestLine('Compiled identity', preview.bundle_digest));
  panel.appendChild(jsonDetails('Normalized typed value', preview.normalized));
  const actions = el('div', 'row orchestration-profile-actions');
  const select = el('button', 'btn primary', 'Select exact revision');
  const cancel = el('button', 'btn', 'Cancel');
  cancel.onclick = () => { previewHost.replaceChildren(); picker.value = ''; };
  select.onclick = async () => {
    select.disabled = true; cancel.disabled = true;
    try {
      const result = await selectProfile(sourceName, bytes, preview);
      if (!panel.isConnected) return;
      previewHost.replaceChildren(el('div', 'banner', result.note ||
        'Exact profile revision selected as inert reusable configuration.'));
      picker.value = '';
      await onSelected();
    } catch (error) {
      if (!panel.isConnected) return;
      select.disabled = false; cancel.disabled = false;
      const failure = el('div', 'banner orchestration-profile-failure', profileErrorMessage('Selection unavailable', error));
      if (error.code === 'state_conflict') {
        const retry = el('button', 'btn', 'Preview current bytes again');
        retry.onclick = async () => {
          retry.disabled = true;
          try {
            const response = await previewProfile(sourceName, bytes);
            renderPreview(previewHost, picker, sourceName, bytes, response.preview, onSelected);
          } catch (retryError) {
            failure.textContent = profileErrorMessage('Preview unavailable', retryError);
            retry.disabled = false;
          }
        };
        failure.appendChild(retry);
      }
      panel.prepend(failure);
    }
  };
  actions.append(cancel, select); panel.appendChild(actions); previewHost.appendChild(panel);
}

// profileIdentitySection is the set-and-forget identity block on an agent's
// card: pinned digests, integrity, selection history, and the exact source —
// the one owner of profile presentation, now composed under the roster.
function profileIdentitySection(profile) {
  const details = document.createElement('details');
  details.className = 'orchestration-profile-card';
  const summary = document.createElement('summary');
  summary.textContent = 'Profile identity · ' + String(profile.profile_id) + ' · version ' + String(profile.version)
    + (profile.integrity === 'verified' ? ' · Selected · inert' : ' · Needs attention');
  details.appendChild(summary);
  details.append(digestLine('Source', profile.source_digest), digestLine('Compiled', profile.bundle_digest),
    el('div', 'sub', 'Selected ' + fmtTime(profile.selected_at) + '. Selection alone is inert; this card’s binding references this exact revision.'));
  if (profile.problem) {
    details.appendChild(el('div', 'banner orchestration-profile-failure',
      String(profile.problem.message || 'Stored profile needs attention.') + ' ' + String(profile.problem.recovery || '')));
  }
  const detailsHost = el('div', 'orchestration-profile-detail');
  let loaded = false;
  details.addEventListener('toggle', async () => {
    if (!details.open || loaded) return;
    loaded = true;
    detailsHost.replaceChildren(el('div', 'sub', 'Loading exact selected source…'));
    try {
      const detail = await loadProfile(profile.profile_id);
      renderProfileDetail(detailsHost, detail);
    } catch (error) {
      detailsHost.replaceChildren(el('div', 'banner', profileErrorMessage('Profile detail unavailable', error)));
    }
  });
  details.appendChild(detailsHost);
  return details;
}

function renderProfileDetail(host, detail) {
  host.replaceChildren();
  if (detail.problem) {
    host.appendChild(el('div', 'banner orchestration-profile-failure',
      String(detail.problem.message || 'Stored profile needs attention.') + ' ' + String(detail.problem.recovery || '')));
    return;
  }
  host.appendChild(el('div', 'sub', 'Integrity verified · exact immutable selected source'));
  const source = document.createElement('details');
  const sourceSummary = document.createElement('summary'); sourceSummary.textContent = 'Exact PROFILE.md source';
  const sourceText = el('pre', 'orchestration-profile-source'); sourceText.textContent = String(detail.source || '');
  source.append(sourceSummary, sourceText); host.appendChild(source);
  host.appendChild(jsonDetails('Normalized typed value', detail.normalized));
  const history = Array.isArray(detail.history) ? detail.history : [];
  const historyDetails = document.createElement('details');
  const historySummary = document.createElement('summary'); historySummary.textContent = 'Prior selected revisions (' + history.length + ')';
  historyDetails.appendChild(historySummary);
  if (!history.length) historyDetails.appendChild(el('div', 'sub', 'No prior selected revision.'));
  history.forEach(revision => historyDetails.appendChild(el('div', 'sub',
    String(revision.version) + ' · ' + fmtTime(revision.selected_at) + ' · ' + String(revision.source_digest))));
  host.appendChild(historyDetails);
}

// profileStorageDiagnostics renders the stored-selection problems and the
// local storage root — page-level honesty that used to ride the profile list.
function profileStorageDiagnostics(response) {
  const host = el('div', 'orchestration-profile-diagnostics');
  if (Array.isArray(response.problems)) {
    response.problems.forEach(item => host.appendChild(el('div', 'banner orchestration-profile-failure',
      'Stored selection needs attention. ' + String(item?.problem?.recovery || 'Review local profile storage diagnostics.'))));
  }
  const diagnostics = document.createElement('details');
  const summary = document.createElement('summary'); summary.textContent = 'Local profile storage';
  diagnostics.append(summary, el('code', 'orchestration-profile-path', String(response.storage_root || 'unavailable')),
    el('div', 'sub', 'This path is local diagnostics only and is not part of profile identity or shared source.'));
  host.appendChild(diagnostics);
  return host;
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

export { renderProfileImporter, profileIdentitySection, profileStorageDiagnostics };
