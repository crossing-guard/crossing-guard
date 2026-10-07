// Settings → Team: the one dialog its acts share (Un-adopt, Unlink, Trust new key,
// Share). The body lists what the act does; nothing here is a browser confirm.
import { el, errorText } from '../orchestration/agents/agent-ui.js';
import { openDialog } from '../orchestration/agents/agent-dialog.js';

// actDialog shows `body` with Cancel and one primary act. `ready` may answer a
// sentence that says why the act cannot run yet; a failure is said in the dialog and
// leaves it open; success closes it and repaints the page. `after` may answer a
// sentence about an act that succeeded with something the person must know (an
// unlink the server never heard of): the page is repainted and the dialog stays, with
// that sentence and Close alone.
export function actDialog(view, { title, body, label, failed, act, ready, after }) {
  const run = async dialog => {
    const waiting = ready ? ready() : '';
    if (waiting) { dialog.showProblem(waiting); return; }
    let result;
    try { result = await act(); } catch (error) { dialog.showProblem(errorText(error, failed)); return; }
    const said = after ? after(result) : '';
    if (!said) dialog.close();
    await view.repaint();
    if (said) settled(dialog, said);
  };
  openDialog({ title, body, actions: [{ label: 'Cancel', onClick: dialog => dialog.close() }, { label, primary: true, onClick: run }] });
}

// settled leaves a dialog whose act is done with one sentence and Close.
function settled(dialog, sentence) {
  dialog.body.replaceChildren(el('div', 'banner', sentence));
  const primary = dialog.primaryButton();
  const cancel = primary?.previousElementSibling;
  if (cancel) cancel.textContent = 'Close';
  primary?.remove();
}
