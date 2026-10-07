// Modal dialogs for the Agents pages: one host with a focus trap, Escape and
// scrim to close, and focus returned to what opened it. Content is typed DOM.
import { el, button, problem } from './agent-ui.js';
import { renderProfileImporter } from '../settings-profiles.js';

const FOCUSABLE = 'button:not([disabled]), [href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

// openDialog shows title + body + actions. Each action is
// { label, primary, onClick(dialog) }; onClick may return a promise, during
// which the buttons are disabled. The returned dialog can close() itself and
// show a problem.
export function openDialog({ title, body, actions = [], wide = false }) {
  const opener = document.activeElement;
  const scrim = el('div', 'agents-scrim');
  const box = el('div', 'agents-dialog' + (wide ? ' agents-dialog-wide' : ''));
  box.setAttribute('role', 'dialog');
  box.setAttribute('aria-modal', 'true');
  const heading = el('h2', 'agents-dialog-title', title);
  heading.id = 'agents-dialog-' + Math.random().toString(36).slice(2);
  box.setAttribute('aria-labelledby', heading.id);
  const problemHost = el('div', 'agents-dialog-problem');
  const footer = el('div', 'agents-dialog-actions');
  box.append(heading, el('div', 'agents-dialog-body'), problemHost, footer);
  box.querySelector('.agents-dialog-body').append(...[].concat(body).filter(Boolean));
  let onKey = null;
  let onNav = null;
  const dialog = {
    close() {
      scrim.remove();
      document.removeEventListener('keydown', onKey, true);
      document.removeEventListener('cg:nav', onNav);
      if (opener?.focus) opener.focus();
    },
    showProblem(text) { problemHost.replaceChildren(text ? problem(text) : ''); },
    body: box.querySelector('.agents-dialog-body'),
    // primaryButton is the dialog's primary action, so a sheet can word it from
    // what a preview showed ("Save for 3 places").
    primaryButton: () => footer.querySelector('.btn.primary'),
  };
  footer.append(...actions.map(action => actionButton(action, dialog, footer)));
  onKey = event => trapKeys(event, box, dialog);
  document.addEventListener('keydown', onKey, true);
  // A link inside a dialog that leaves the page (Settings → Models) closes it.
  onNav = () => dialog.close();
  document.addEventListener('cg:nav', onNav);
  scrim.addEventListener('mousedown', event => { if (event.target === scrim) dialog.close(); });
  scrim.appendChild(box);
  document.body.appendChild(scrim);
  (box.querySelector('[autofocus]') || box.querySelector(FOCUSABLE))?.focus();
  return dialog;
}

function actionButton(action, dialog, footer) {
  const node = button(action.label, action.primary ? 'primary' : action.danger ? 'danger' : '');
  node.onclick = async () => {
    const buttons = [...footer.querySelectorAll('button')];
    buttons.forEach(item => { item.disabled = true; });
    try {
      await action.onClick?.(dialog);
    } finally {
      buttons.forEach(item => { item.disabled = false; });
    }
  };
  return node;
}

function trapKeys(event, box, dialog) {
  if (event.key === 'Escape') {
    // The dialog's Escape is its own: the console's global Escape handler
    // must not also act on it.
    event.preventDefault();
    event.stopPropagation();
    dialog.close();
    return;
  }
  if (event.key !== 'Tab') return;
  const items = [...box.querySelectorAll(FOCUSABLE)];
  if (!items.length) return;
  const first = items[0], last = items[items.length - 1];
  if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last.focus(); }
  else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus(); }
}

// sourceDialog shows exact PROFILE.md bytes.
export function sourceDialog(title, text) {
  const pre = el('pre', 'agents-source');
  pre.textContent = String(text || '');
  openDialog({ title, body: pre, wide: true, actions: [{ label: 'Close', onClick: dialog => dialog.close() }] });
}

// importDialog wraps the one profile importer (preview → select); on a
// selected revision it closes and hands the new profile id to onImported.
export function importDialog(onImported) {
  const host = el('div');
  const dialog = openDialog({ title: 'Import PROFILE.md', body: host, wide: true,
    actions: [{ label: 'Close', onClick: current => current.close() }] });
  renderProfileImporter(host, async result => {
    dialog.close();
    await onImported(result?.profile?.profile_id || '');
  });
}

// confirmDialog asks one question with a primary action, or a danger one
// when confirming removes something.
export function confirmDialog(title, text, label, onConfirm, danger = false) {
  openDialog({ title, body: el('p', 'agents-dialog-text', text), actions: [
    { label: 'Cancel', onClick: dialog => dialog.close() },
    { label, primary: !danger, danger, onClick: dialog => runConfirmed(dialog, onConfirm) },
  ] });
}

async function runConfirmed(dialog, onConfirm) {
  try {
    await onConfirm();
    dialog.close();
  } catch (error) {
    dialog.showProblem(String(error?.message || error));
  }
}
