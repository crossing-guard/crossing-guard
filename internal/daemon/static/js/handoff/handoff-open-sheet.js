// The Open sheet (team rest-of-release plan §6.3, §14 Q18–Q20; criteria 84,
// 87). It lists the runtimes a handoff can open in, each "Ready" or exactly
// what this device has not seen from it; the folder, from the checkouts this
// device knows or the system's folder dialog — never a typed path; and whether
// memory recall is on for the chosen runtime, with the action to turn it on.
// Open starts nothing: it opens a new chat that carries the handoff, and the
// person types the first prompt.
import { el, field, linkButton } from '../orchestration/agents/agent-ui.js';
import { openDialog } from '../orchestration/agents/agent-dialog.js';
import { loadChatCapabilities, findChatCapability } from '../chat-capabilities.js';
import { loadOpenOptions, openHandoff, chooseFolder } from './handoff-api.js';
import { readinessView, firstReady, windowWords, itemTitle, peerName } from './handoff-model.js';
import { recallControl } from './memory-recall.js';
import { startOpenComposer } from './handoff-composer.js';

// Why the folder dialog did not answer, by the daemon's code.
const FOLDER_DIALOG_WORDS = {
  picker_unavailable: 'This system has no folder dialog Crossing Guard can open. Pick one of the folders listed.',
  picker_open: 'A folder dialog is already open. Choose a folder there, or close it.',
};

const CHOOSE = '\u0000choose';

// openRuntimes shows Settings → Runtimes, where a hook is connected or repaired.
export function openRuntimesSettings() {
  document.dispatchEvent(new CustomEvent('cg:settings-subpage', { detail: 'runtimes' }));
  document.dispatchEvent(new CustomEvent('cg:nav', { detail: 'settings' }));
}

// openOpenSheet opens the sheet for one received handoff.
//   prefer { runtime, folder } — what an earlier Open chose (Retry, a
//   same-device handoff's target runtime)
export async function openOpenSheet({ item, prefer = {} }) {
  const body = el('div', 'handoff-sheet');
  body.appendChild(el('div', 'agents-sub', 'Reading what this device has seen…'));
  const sheet = { item, body, runtime: '', folder: '', options: null, labels: String };
  sheet.dialog = openDialog({ title: 'Open as a new session', body, wide: true, actions: [
    { label: 'Cancel', onClick: dialog => dialog.close() },
    { label: 'Open', primary: true, onClick: () => open(sheet) },
  ] });
  sync(sheet);
  try {
    const [options, capabilities] = await Promise.all([loadOpenOptions(item.id), loadChatCapabilities().catch(() => [])]);
    sheet.options = options;
    sheet.labels = runtime => findChatCapability(capabilities, runtime)?.displayName || runtime;
  } catch (error) {
    body.replaceChildren(el('div', 'banner agents-problem', 'The handoff cannot be opened: ' + (error.message || error)));
    return;
  }
  const views = runtimeViews(sheet);
  sheet.runtime = views.some(view => view.ready && view.runtime === prefer.runtime) ? prefer.runtime : firstReady(views);
  const folders = sheet.options.checkout_roots || [];
  // A folder an earlier Open chose is kept; else the newest checkout of the repository.
  sheet.folder = prefer.folder || folders[0] || '';
  draw(sheet);
}

const runtimeViews = sheet => (sheet.options.runtimes || [])
  .map(option => readinessView(option, sheet.labels, windowWords(sheet.options.firing_window)));

function draw(sheet) {
  const views = runtimeViews(sheet);
  sheet.body.replaceChildren();
  if (!views.length) {
    sheet.body.appendChild(el('div', 'agents-empty', 'No runtime on this device can open a handoff.'));
    sync(sheet);
    return;
  }
  sheet.body.appendChild(runtimeField(sheet, views));
  sheet.body.appendChild(folderField(sheet));
  const recall = recallRow(sheet);
  if (recall) sheet.body.appendChild(recall);
  if (!views.some(view => view.ready)) {
    const help = el('div', 'agents-row');
    help.append(el('span', 'agents-sub', 'Start a session in the runtime once, with its hook connected, and it can open a handoff.'),
      linkButton('Settings → Runtimes ›', openRuntimesSettings));
    sheet.body.appendChild(help);
  }
  sync(sheet);
}

// runtimeField is one choice per runtime: its name, and "Ready" or each thing
// that is missing, one per line.
function runtimeField(sheet, views) {
  const wrap = el('div', 'agents-field');
  wrap.appendChild(el('span', 'agents-field-label', 'Runtime'));
  const choices = wrap.appendChild(el('div', 'agents-choices'));
  for (const view of views) {
    const choice = el('button', 'btn agents-choice' + (view.runtime === sheet.runtime ? ' selected' : ''));
    choice.type = 'button';
    choice.disabled = !view.ready;
    choice.setAttribute('aria-pressed', String(view.runtime === sheet.runtime));
    choice.appendChild(el('b', '', view.label));
    for (const step of view.steps) choice.appendChild(el('span', 'runtime-step runtime-step-' + (view.ready ? 'met' : 'next'), step));
    for (const note of view.notes) choice.appendChild(el('span', 'runtime-step runtime-step-next', note));
    choice.onclick = () => { sheet.runtime = view.runtime; draw(sheet); };
    choices.appendChild(choice);
  }
  return wrap;
}

// folderField lists the checkouts this device has resolved to the handoff's
// repository and offers the system's folder dialog. There is no typed path.
function folderField(sheet) {
  const known = [...new Set([...(sheet.options.checkout_roots || []), sheet.folder].filter(Boolean))];
  const select = document.createElement('select');
  select.setAttribute('aria-label', 'Folder');
  const add = (value, label) => { const option = document.createElement('option'); option.value = value; option.textContent = label; select.appendChild(option); };
  if (!known.length) add('', 'No checkout of this repository on this device');
  known.forEach(path => add(path, path));
  add(CHOOSE, 'Choose a folder…');
  select.value = sheet.folder;
  const problem = el('span', 'agents-sub');
  select.onchange = async () => {
    if (select.value !== CHOOSE) { sheet.folder = select.value; sync(sheet); return; }
    select.disabled = true;
    try {
      const answer = await chooseFolder(sheet.folder);
      if (answer.chosen) sheet.folder = answer.path;
      draw(sheet);
    } catch (error) {
      select.disabled = false;
      select.value = sheet.folder;
      problem.textContent = FOLDER_DIALOG_WORDS[error.code] || 'The folder dialog could not be shown.';
    }
  };
  const wrap = field('Folder', select);
  wrap.appendChild(problem);
  return wrap;
}

// recallRow says whether shared memory reaches the chosen runtime, and offers
// to turn it on where it does not (OD-22). Nothing is written without consent.
function recallRow(sheet) {
  const option = (sheet.options.runtimes || []).find(item => item.runtime === sheet.runtime);
  const state = option?.memory_recall;
  if (!state || state.observed_injecting) return null;
  const wrap = el('div', 'agents-field');
  wrap.appendChild(el('span', 'agents-field-label', 'Shared memory'));
  wrap.appendChild(recallControl({ state, runtimeLabel: sheet.labels, heading: '', onChanged: next => { option.memory_recall = next; draw(sheet); } }));
  return wrap;
}

const ready = sheet => Boolean(sheet.options) && runtimeViews(sheet).some(view => view.ready && view.runtime === sheet.runtime) && Boolean(sheet.folder);

function sync(sheet) {
  const primary = sheet.dialog?.primaryButton();
  if (!primary) return;
  primary.disabled = !ready(sheet);
  primary.textContent = sheet.runtime ? 'Open in ' + sheet.labels(sheet.runtime) : 'Open';
}

async function open(sheet) {
  if (!ready(sheet)) { setTimeout(() => sync(sheet), 0); return; }
  try {
    const answer = await openHandoff(sheet.item.id, sheet.runtime, sheet.folder);
    sheet.dialog.close();
    document.dispatchEvent(new CustomEvent('cg:handoffs-changed', { detail: { id: sheet.item.id } }));
    startOpenComposer({ handoffId: sheet.item.id, ticketId: answer.ticket.ticket_id, runtime: sheet.runtime,
      cwd: answer.ticket.checkout_root || sheet.folder, title: itemTitle(sheet.item), from: peerName(sheet.item), local: sheet.item.local === true });
  } catch (error) {
    sheet.dialog.showProblem('Not opened: ' + (error.message || error));
    setTimeout(() => sync(sheet), 0);
  }
}
