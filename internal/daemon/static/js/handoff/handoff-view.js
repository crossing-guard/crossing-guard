// A handoff, opened in the centre as a document (team rest-of-release plan
// §8.2, §6.2, §6.3, §6.5; §14 Q17, Q21, Q31): who sent it, the title, the
// repository, what remains, the text, the excerpt if it carries one, and the
// agents it names with whether each is adopted here — one fact per row. Its
// actions are the ones the daemon offers on the item, and nothing else.
import { $, el, fmtTime } from '../core.js';
import { button, chip, card, linkButton } from '../orchestration/agents/agent-ui.js';
import { openDialog } from '../orchestration/agents/agent-dialog.js';
import { loadChatCapabilities, findChatCapability } from '../chat-capabilities.js';
import { loadHandoff, loadHandoffs, loadOpenOptions, loadAdoptions, declineHandoff, closeHandoff, withdrawHandoff, deliverAgain } from './handoff-api.js';
import { withSide, stateView, stateLine, receiptLine, itemTitle, itemActions, openView, documentFacts, agentRows, adoptedProfileIds, neverArrived, peerName } from './handoff-model.js';
import { openOpenSheet, openRuntimesSettings } from './handoff-open-sheet.js';
import { openSendSheet } from './handoff-send-sheet.js';

// Each open of a document is numbered, so a slow read for an earlier one is
// dropped instead of painted over the one being looked at.
let shown = 0;

// renderHandoffDocument draws the handoff `id` into the centre.
export async function renderHandoffDocument(id) {
  const turn = ++shown;
  const main = $('#main');
  // The session that was in the centre left its jump pill ("↑ Oldest") on the page:
  // the pill belongs to that session's scroller and would float over this document.
  document.querySelectorAll('.jumppill').forEach(pill => pill.remove());
  main.replaceChildren(el('div', 'sub', 'Reading the handoff…'));
  document.dispatchEvent(new CustomEvent('cg:handoff-shown', { detail: { id } }));
  let view;
  try {
    view = await readView(id);
  } catch (error) {
    if (turn === shown) main.replaceChildren(el('div', 'banner', 'This handoff could not be read: ' + (error.message || error)));
    return;
  }
  if (turn !== shown) return;
  view.repaint = () => renderHandoffDocument(id);
  main.replaceChildren(documentNode(view));
}

// readView reads the item, its document, and the three facts the page words:
// the runtimes' names, which agents are adopted here, where its repository is.
async function readView(id) {
  const [detail, list, capabilities] = await Promise.all([loadHandoff(id), loadHandoffs(), loadChatCapabilities().catch(() => [])]);
  const sided = withSide(list);
  const item = [...sided.received, ...sided.sent].find(row => row.id === id) || { ...detail.handoff, side: detail.handoff.sent_here ? 'sent' : 'received' };
  const record = detail.document || null;
  const [layers, options] = await Promise.all([
    record?.agents?.length ? loadAdoptions().catch(() => null) : null,
    item.open_offered ? loadOpenOptions(id).catch(() => null) : null,
  ]);
  return { item: { ...item, opens: detail.opens || item.opens || [] }, record, transport: list.transport || null,
    runtimeLabel: runtime => findChatCapability(capabilities, runtime)?.displayName || runtime,
    adopted: adoptedProfileIds(layers), folder: options?.checkout_roots?.[0] || '', folderKnown: Boolean(options) };
}

function documentNode(view) {
  const { item, record } = view;
  const page = el('div', 'handoff-doc');
  const crumb = el('div', 'crumb');
  crumb.append(el('span', '', 'Handoffs'), el('span', 'crumb-sep', '›'), el('span', 'crumb-here', item.side === 'sent' ? 'Sent' : 'Received'));
  page.append(crumb, el('h2', '', itemTitle(item)));
  const state = stateView(item, view.transport);
  const line = el('div', 'handoff-state');
  line.appendChild(chip(state.word, state.chip));
  const sentence = stateLine(item, { transport: view.transport, runtimeLabel: view.runtimeLabel });
  if (sentence) line.appendChild(el('span', 'sub', sentence));
  page.appendChild(line);
  if (receiptLine(item)) page.appendChild(el('div', 'sub', receiptLine(item)));
  page.appendChild(factsNode(view));
  for (const open of item.opens) {
    const node = openNode(view, open);
    if (node) page.appendChild(node);
  }
  const actions = actionsNode(view);
  if (actions) page.appendChild(actions);
  if (neverArrived(item) || !record) return page;
  page.append(...recordCards(view));
  return page;
}

function factsNode(view) {
  const grid = el('div', 'handoff-facts');
  const rows = documentFacts(view.item, view.record, { when: fmtTime, runtimeLabel: view.runtimeLabel, folder: view.folder, folderKnown: view.folderKnown });
  for (const fact of rows) grid.append(el('span', 'handoff-facts-k', fact.label), el('span', '', fact.value));
  const session = view.item.sent_here ? view.item.session : null;
  if (session?.runtime && (session.catalog_id || session.native_id)) {
    grid.append(el('span', 'handoff-facts-k', 'From session'), sessionLink(session.runtime, session.catalog_id || session.native_id, 'Open the session'));
  }
  return grid;
}

// sessionLink is a link the console's one session opener follows.
function sessionLink(runtime, id, label) {
  const link = el('a', 'ref session-link', label);
  link.href = '/?runtime=' + encodeURIComponent(runtime) + '&session=' + encodeURIComponent(id);
  link.dataset.sessionRuntime = runtime;
  link.dataset.sessionId = id;
  return link;
}

// openNode is one Open of this handoff on this device: where it stands, and
// what that Open offers.
function openNode(view, open) {
  const shownOpen = openView(open, view.runtimeLabel, { ended: stateView(view.item, view.transport).terminal });
  if (!shownOpen) return null;
  const node = el('div', 'handoff-open' + (shownOpen.tone ? ' handoff-open-' + shownOpen.tone : ''));
  node.appendChild(el('div', 'handoff-open-words', shownOpen.words));
  if (shownOpen.note) node.appendChild(el('div', 'sub', shownOpen.note));
  const said = el('span', 'sub');
  const actions = el('div', 'row');
  for (const action of shownOpen.actions) actions.appendChild(openAction(view, open, action, said));
  if (shownOpen.actions.length) node.appendChild(actions).appendChild(said);
  return node;
}

function openAction(view, open, action, said) {
  if (action.id === 'go') return sessionLink(open.session.runtime, open.session.native_id, action.label);
  if (action.id === 'runtimes') return linkButton(action.label, openRuntimesSettings);
  if (action.id === 'retry') {
    return button(action.label, 'primary', () => openOpenSheet({ item: view.item, prefer: { runtime: open.runtime, folder: open.checkout_root } }));
  }
  const node = button(action.label, '', async () => {
    node.disabled = true;
    try {
      const answer = await deliverAgain(view.item.id, open.ticket_id);
      said.textContent = answer.armed ? 'Its next prompt carries it.' : 'It is delivered once the session has room for it.';
    } catch (error) {
      said.textContent = 'Not delivered again: ' + (error.message || error);
    }
    node.disabled = false;
  });
  return node;
}

function actionsNode(view) {
  const actions = itemActions(view.item);
  if (!actions.length) return null;
  const node = el('div', 'row handoff-actions');
  const run = { open: () => openOpenSheet({ item: view.item }), decline: () => declineDialog(view), close: () => closeDialog(view),
    withdraw: () => withdrawDialog(view), 'send-again': () => sendAgain(view) };
  for (const action of actions) node.appendChild(button(action.label, action.primary ? 'primary' : (action.danger ? 'danger' : ''), run[action.id]));
  return node;
}

function recordCards(view) {
  const { record } = view;
  const cards = [];
  if (record.remaining?.length) {
    const list = el('ol', 'handoff-list');
    record.remaining.forEach(entry => list.appendChild(el('li', '', entry)));
    cards.push(card('What remains', list));
  }
  cards.push(card('Text', el('pre', 'agents-source handoff-text', record.body_markdown || '')));
  const turns = record.conversation?.turns || [];
  if (turns.length) cards.push(card('Their last ' + turns.length + (turns.length === 1 ? ' turn' : ' turns'), ...turns.map(turnNode)));
  const agents = agentRows(record.agents || [], view.adopted);
  if (agents.length) cards.push(card('Agents', ...agents.map(agentNode)));
  return cards;
}

function turnNode(turn) {
  const node = el('div', 'handoff-turn');
  node.append(el('span', 'handoff-turn-role', turn.seq + ' ' + turn.role), el('pre', 'handoff-turn-text', turn.text));
  return node;
}

function agentNode(agent) {
  const node = el('div', 'agents-row');
  node.append(el('span', '', agent.name), chip(agent.adopted ? 'adopted here' : 'not adopted here', agent.adopted ? 'st-verified' : 'st-stale'));
  if (!agent.adopted) {
    node.appendChild(linkButton('Settings → Team ›', () => {
      document.dispatchEvent(new CustomEvent('cg:settings-subpage', { detail: 'team' }));
      document.dispatchEvent(new CustomEvent('cg:nav', { detail: 'settings' }));
    }));
  }
  return node;
}

// actDialog states what an act does, one fact per row, and runs it on the
// primary button. A refusal is said in the dialog, which stays open.
function actDialog(view, { title, rows, label, act }) {
  const grid = el('div', 'handoff-facts');
  for (const [key, value] of rows) grid.append(el('span', 'handoff-facts-k', key), el('span', '', value));
  openDialog({ title, body: grid, actions: [
    { label: 'Cancel', onClick: dialog => dialog.close() },
    { label, primary: true, onClick: async dialog => {
      try { await act(view.item.id); } catch (error) { dialog.showProblem(label + ' did not happen: ' + (error.message || error)); return; }
      dialog.close();
      document.dispatchEvent(new CustomEvent('cg:handoffs-changed', { detail: { id: view.item.id } }));
      await view.repaint();
    } },
  ] });
}

function declineDialog(view) {
  const rows = view.item.local ? [['Here', 'Stays under Received; Open is no longer offered']]
    : [[peerName(view.item), 'Sees that you declined it'], ['Here', 'Stays under Received; Open is no longer offered']];
  actDialog(view, { title: 'Decline this handoff', label: 'Decline', act: declineHandoff, rows });
}

function closeDialog(view) {
  const rows = [['Sessions already opened with it', 'Keep what they read'], ['Here', 'Open and Deliver again are no longer offered']];
  if (!view.item.local) rows.unshift([peerName(view.item), 'Sees that you closed it']);
  actDialog(view, { title: 'Close this handoff', label: 'Close handoff', act: closeHandoff, rows });
}

function withdrawDialog(view) {
  actDialog(view, { title: 'Withdraw this handoff', label: 'Withdraw', act: withdrawHandoff, rows: [
    ['Team server', 'Text erased'],
    [peerName(view.item) + '’s devices', 'Removed at their next sync'],
    ['A session already opened with it', 'Keeps what it read'],
  ] });
}

// sendAgain reopens the send sheet with the kept text (§14 Q17).
function sendAgain(view) {
  const { item, record } = view;
  const session = { runtime: item.session?.runtime || '', id: item.session?.catalog_id || item.session?.native_id || '' };
  void openSendSheet({ session, prefill: { to: item.peer?.user_id || '', title: record?.title || item.title || '',
    remaining: [...(record?.remaining || [])], text: record?.body_markdown || '', includeConversation: false },
    onSent: result => renderHandoffDocument(result.id) });
}

// handOffSession is "Hand off…" in a session's ⋯ menu (§14 Q2).
export function handOffSession(session, liveTurns = 0) {
  void openSendSheet({ session: { runtime: session.runtime, id: session.id }, liveTurns });
}

// continueElsewhere is the composer's cross-runtime gate (§6.6 "Local
// handoff"): a runtime cannot resume another's session, so the session is
// handed off on this device — nothing leaves it — and opened by the same Open.
export async function continueElsewhere({ runtime, id, liveTurns = 0, target = '', cwd = '' }) {
  const capabilities = await loadChatCapabilities().catch(() => []);
  const label = findChatCapability(capabilities, target)?.displayName || target;
  await openSendSheet({ session: { runtime, id }, local: true, target: label, liveTurns,
    onSent: result => openOpenSheet({ item: { id: result.id, title: result.title, local: true, side: 'received' }, prefer: { runtime: target, folder: cwd } }) });
}
