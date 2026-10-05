// An Open of a handoff, while its composer is open (team rest-of-release plan
// §6.3, §14 Q20). Open writes the Open and starts nothing: the new chat shows
// the handoff above the reply box and the person types the first prompt, which
// carries it. Closing that composer without sending cancels the Open. This
// module holds the one Open a composer is waiting on, so the chat view needs
// to know nothing about handoffs beyond "this send carries one".
import { el, button, chip } from '../orchestration/agents/agent-ui.js';
import { cancelOpen, cancelOpenOnLeave } from './handoff-api.js';
import { continuesLine } from './handoff-model.js';

// waiting is the Open the next new chat takes; held is the one a composer has.
let waiting = null;
let held = null;
// lastChoice is the mode and model the person sent an Open's first prompt with, per
// runtime, for as long as this page lives: the next Open in that runtime starts
// from it instead of from the first mode and the default model again.
const lastChoice = new Map();

// startOpenComposer hands an Open to the new chat and opens that chat.
//   open { handoffId, ticketId, runtime, runtimeLabel, cwd, title, from, local }
export function startOpenComposer(open) {
  waiting = open;
  document.dispatchEvent(new CustomEvent('cg:continue', { detail: { fresh: true, cwd: open.cwd } }));
}

// release cancels an Open whose first prompt was never sent.
function release(open) {
  if (!open || open.sent) return;
  open.sent = true; // one cancel per Open
  cancelOpen(open.handoffId, open.ticketId).catch(() => {})
    .finally(() => document.dispatchEvent(new CustomEvent('cg:handoffs-changed', { detail: { id: open.handoffId } })));
}

function headingOf(open) {
  return { title: String(open.title || 'Handoff'),
    continues: continuesLine({ opened: true, local: open.local || !open.from, title: open.title, from: { display_name: open.from } }) };
}

// takeOpenComposer is called by a new chat as it mounts. It returns the Open
// this composer carries, or null for an ordinary chat. An Open an earlier
// composer still held unsent is cancelled: that composer is gone.
export function takeOpenComposer() {
  release(held);
  held = waiting;
  waiting = null;
  if (!held) return null;
  const open = held;
  return {
    runtime: open.runtime, cwd: open.cwd,
    // ticket is what the first send carries; empty once it has been sent.
    ticket: () => (open.sent ? '' : open.ticketId),
    strip: () => stripNode(open),
    // heading is what the chat is called once its first prompt is sent: the session
    // exists then and it continues this handoff, so "New session" no longer says it.
    heading: () => headingOf(open),
    // choice is what the last Open in this runtime was sent with, or null.
    choice: () => lastChoice.get(open.runtime) || null,
    // sent marks the first prompt sent; `choice` is the mode and model it went with.
    sent(choice) {
      if (choice) lastChoice.set(open.runtime, { mode: String(choice.mode || ''), model: String(choice.model || '') });
      open.sent = true; open.node?.remove();
    },
    // refusedWords is the daemon's sentence for a first send it refused
    // ("the sender withdrew this handoff"); nothing was started.
    refusedWords(error) {
      if (open.sent || !error?.code) return '';
      const sentence = String(error.detail || error.message || '').trim();
      const local = open.local || !open.from;
      const words = error.code === 'handoff_withdrawn' && !local ? open.from + ' withdrew this handoff' : sentence.charAt(0).toUpperCase() + sentence.slice(1);
      return words.replace(/\.$/, '') + '. Nothing was started.';
    },
  };
}

// stripNode is the handoff as the composer shows it above the reply box.
function stripNode(open) {
  const node = el('div', 'handoff-strip');
  const words = el('div', 'handoff-strip-words');
  words.append(chip('handoff', 'cl-observed'), el('span', 'handoff-strip-title', open.title));
  const from = open.local ? 'from a session on this device' : 'from ' + open.from;
  node.append(words, el('span', 'agents-sub', from), el('span', 'agents-sub', 'Arrives with your first prompt'),
    button('Cancel', 'ghost', () => { release(open); document.dispatchEvent(new CustomEvent('cg:center', { detail: { view: 'handoff', id: open.handoffId } })); }));
  open.node = node;
  return node;
}

// The composer is closed when the centre is given to something else. A plain
// navigation keeps the chat (it is a pinned view) and so keeps the Open.
function releaseIfClosed() {
  setTimeout(() => {
    if (held && !held.sent && held.node && !held.node.isConnected) { release(held); held = null; }
  }, 0);
}

if (typeof document !== 'undefined') {
  document.addEventListener('cg:session-selected', releaseIfClosed);
  document.addEventListener('cg:center', releaseIfClosed);
  globalThis.addEventListener?.('pagehide', () => {
    if (held && !held.sent) cancelOpenOnLeave(held.handoffId, held.ticketId);
  });
}
