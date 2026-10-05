// Handoff between members, as the console words it (team rest-of-release plan
// §6.1–§6.3, §6.5, §8.2, §14 Q14–Q23, Q31). Pure functions over the daemon's
// own response shapes (GET /api/team/handoffs, the send preview, open-options):
// which state word and sentence an item shows, which actions it offers, what an
// Open says about a runtime, and the "what will be sent" model of the send
// sheet. Nothing here decides: every offer is the daemon's, read from the item.

// The sender's and the recipient's state words (§14 Q17). A refused send reads
// "not sent"; a queued one reads "held" while the team server carries none.
const STATE_WORDS = Object.freeze({
  queued: 'queued', sent: 'sent', received: 'received', started: 'started', opened: 'opened',
  closed: 'closed', declined: 'declined', withdrawn: 'withdrawn', expired: 'expired', refused: 'not sent',
});
const HELD = 'held';

// One chip class per state, from the console's existing chip vocabulary.
const STATE_CHIPS = Object.freeze({
  queued: 'st-stale', held: 'st-stale', sent: 'st-draft', received: 'st-draft', started: 'cl-observed',
  opened: 'st-verified', closed: 'st-draft', declined: 'st-disputed', withdrawn: 'st-disputed',
  expired: 'st-expired', refused: 'st-disputed',
});

const TERMINAL = new Set(['closed', 'declined', 'withdrawn', 'expired', 'refused']);

// Why the team server refused a send, in words (§6.6 push answers).
const REFUSAL_WORDS = Object.freeze({
  unknown_recipient: who => who + ' is not a member of this team',
  recipient_inactive: who => who + ' is no longer a member of this team',
  recipient_inbox_full: who => who + ' has too many unopened handoffs',
  rate_limited: () => 'too many handoffs were sent this hour',
  over_cap: () => 'it is larger than the team server accepts',
  conflict: () => 'the team server already holds a different handoff under the same name',
});

// Why the team server set aside this device's last answer: the handoff had
// already ended another way, or a session had already started for it.
const RECEIPT_WORDS = Object.freeze({
  handoff_withdrawn: 'it had already been withdrawn',
  handoff_declined: 'it had already been declined',
  handoff_closed: 'it had already been closed',
  handoff_expired: 'it had already expired',
  handoff_started: 'a session had already started for it',
});

// What an Open is missing in a runtime, as the open sheet says it (§6.3).
const MISSING_WORDS = Object.freeze({
  session_entry_not_observed: window => 'No session start seen' + within(window),
  prompt_event_not_observed: window => 'No prompt seen' + within(window),
  prompt_kind_not_a_carrier_kind: () => 'Its prompt event cannot carry a handoff here',
  hook_did_not_run_in_launched_session: () => 'Its hook did not run in the session Crossing Guard started',
});

const text = value => String(value ?? '').trim();
const within = window => (window ? ' in ' + window : '');
const sentenceCase = word => word.charAt(0).toUpperCase() + word.slice(1);

// peerName is the other member as this device knows them; a device knows a
// display name and nothing else (OD-10).
export function peerName(item) {
  return text(item?.peer?.display_name) || 'A teammate';
}

// withSide marks each row with the list the daemon put it under, so a row
// carries its side wherever it is drawn.
export function withSide(list = {}) {
  const tag = side => item => ({ ...item, side });
  return { ...list, received: (list.received || []).map(tag('received')), sent: (list.sent || []).map(tag('sent')) };
}

// isHeld: queued while the team server answered that it carries no handoffs.
function isHeld(item, transport) {
  return item?.state === 'queued' && transport && transport.server_carries_handoffs === false;
}

// stateView is the item's chip: the word and its class.
export function stateView(item, transport = null) {
  const state = isHeld(item, transport) ? HELD : text(item?.state);
  return { word: state === HELD ? HELD : (STATE_WORDS[state] || state), chip: STATE_CHIPS[state] || 'st-draft', terminal: TERMINAL.has(state) };
}

// neverArrived: a handoff this device never held the text of. Either it ended
// before it reached this device, or another of the person's devices opened it
// and the team server no longer carries its text. It shows the sender and the
// time: no title, no text, no Open (§6.7, criterion 78).
export function neverArrived(item) {
  return item?.side === 'received' && item?.to_me === true && item?.ever_held === false && !item?.local;
}

// itemTitle is the rail and document heading. A row this device holds no
// title for is named by who it is from, or to.
export function itemTitle(item) {
  if (text(item?.title)) return text(item.title);
  if (item?.from_another_device) return 'Sent from another of your devices';
  return (item?.side === 'sent' ? 'To ' : 'From ') + peerName(item);
}

function openedRuntime(item, runtimeLabel) {
  const runtime = text(item?.opened_by?.session?.runtime);
  return runtime ? runtimeLabel(runtime) : '';
}

function sentLine(item, transport, runtimeLabel) {
  const who = peerName(item), runtime = openedRuntime(item, runtimeLabel);
  const lines = {
    queued: isHeld(item, transport) ? 'this team server does not carry handoffs' : 'waiting to leave this device',
    sent: 'waiting for ' + who + '’s devices',
    received: 'on ' + who + '’s devices',
    started: who + ' started a ' + (runtime ? runtime + ' ' : '') + 'session',
    opened: 'continued by ' + who + (runtime ? ' in ' + runtime : ''),
    closed: 'closed by ' + who,
    declined: 'declined by ' + who,
    withdrawn: 'you withdrew it',
    expired: 'not opened before it expired',
    refused: (REFUSAL_WORDS[item?.refusal_code] || (() => 'the team server refused it'))(who),
  };
  return lines[item?.state] || '';
}

// What a row this device never held the text of says: the two ways a handoff
// ends with nobody acting on it here, and the one way it is still in use.
const NEVER_ARRIVED_WORDS = Object.freeze({
  withdrawn: 'withdrawn before it reached this device',
  expired: 'expired before it reached this device',
});
const OPENED_ELSEWHERE = 'opened on another of your devices; the text is no longer on the team server';

function receivedLine(item) {
  const who = peerName(item);
  if (neverArrived(item) && NEVER_ARRIVED_WORDS[item.state]) return NEVER_ARRIVED_WORDS[item.state];
  // Still in use, and no text came with it: a session started for it elsewhere.
  if (neverArrived(item) && !TERMINAL.has(item.state)) return OPENED_ELSEWHERE;
  // Who it is from is its own row; a handoff that is waiting or in use says no more here.
  const lines = {
    closed: 'you closed it', declined: 'you declined it',
    withdrawn: item?.local ? 'you withdrew it' : who + ' withdrew this handoff',
    expired: 'not opened before it expired',
  };
  return lines[item?.state] || '';
}

// stateLine is the one sentence beside the chip. runtimeLabel turns a runtime
// id into the name the daemon publishes for it.
export function stateLine(item, { transport = null, runtimeLabel = String } = {}) {
  return item?.side === 'sent' ? sentLine(item, transport, runtimeLabel) : receivedLine(item);
}

// receiptLine says the team server set aside what this device last asked for.
export function receiptLine(item) {
  const words = RECEIPT_WORDS[item?.receipt_code];
  return words ? 'The team server did not take the last change: ' + words + '.' : '';
}

// railMeta is the rail row's second line: who, never a chain of facts.
export function railMeta(item) {
  if (item?.local) return 'this device';
  if (!text(item?.title) && !item?.from_another_device) return ''; // the row's title already says who
  if (item?.from_another_device) return 'to ' + peerName(item);
  return (item?.side === 'sent' ? 'to ' : 'from ') + peerName(item);
}

// railWhen is a rail row's time: the day and the minute, short enough to fit.
export function railWhen(iso, { locale, timeZone } = {}) {
  const date = new Date(iso);
  if (isNaN(date)) return '';
  return date.toLocaleString(locale, { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit', timeZone });
}

// The states in which an item is waiting on the person at this device, by side,
// and how early each is listed. Received: one not yet opened, then one a session
// started for or opened (still theirs to finish or close). Sent: one that has not
// left this device, or that the team server refused.
const NEEDS_PERSON = Object.freeze({
  received: Object.freeze({ received: 0, started: 1, opened: 1 }),
  sent: Object.freeze({ queued: 0, refused: 0 }),
});

const needRank = item => NEEDS_PERSON[item?.side]?.[item?.state];
const changedAt = item => Date.parse(item?.state_at || item?.created_at) || 0;

// inRailOrder lists one side: what needs the person first, then the rest, each
// by the most recent change of state. The daemon's own order breaks a tie.
function inRailOrder(items = []) {
  const late = Object.keys(STATE_WORDS).length;
  return [...items].sort((a, b) => ((needRank(a) ?? late) - (needRank(b) ?? late)) || (changedAt(b) - changedAt(a)));
}

// isEnded: nothing more will happen to it, and it asks nothing of the person.
const isEnded = item => TERMINAL.has(item?.state) && needRank(item) === undefined;

// railGroup is the Handoffs group of the Sessions rail (§14 Q1, Q3): the two
// lists, in the order the rail shows them, and the one count that says something
// arrived. Every item that has not ended is a row. Of the ended ones the
// endedVisible most recently ended are rows (the daemon publishes how many); the
// rest stand behind one row, `endedBehind` of them, until showEnded. The counts
// are of the lists, not of the rows.
export function railGroup(list = {}, { endedVisible = 0, showEnded = false } = {}) {
  const received = inRailOrder(list.received), sent = inRailOrder(list.sent);
  const all = [...received, ...sent];
  const ended = all.filter(isEnded).sort((a, b) => changedAt(b) - changedAt(a));
  const recent = new Set(ended.slice(0, Math.max(0, Number(endedVisible) || 0)));
  const rows = items => items.filter(item => !isEnded(item) || showEnded || recent.has(item));
  return { total: all.length, empty: all.length === 0, received: rows(received), sent: rows(sent),
    receivedCount: received.length, sentCount: sent.length, endedBehind: ended.length - recent.size, endedShown: showEnded };
}

// endedRowWords is the one row the older ended handoffs stand behind.
export function endedRowWords(group) {
  if (!group?.endedBehind) return '';
  return (group.endedShown ? 'Hide ended (' : 'Show ended (') + group.endedBehind + ')';
}

const hasOffer = (item, name) => (item?.offers || []).includes(name);

// claimedOpens are the sessions that started for this handoff on this device.
export function claimedOpens(item) {
  return (item?.opens || []).filter(open => open.state === 'claimed' && open.session);
}

// itemActions lists what the item offers now, in the order the document shows
// them (§6.2 "When the console offers each", §14 Q17, Q21, Q31). Every entry is
// the daemon's own offer; nothing is offered that the item does not carry.
export function itemActions(item) {
  const actions = [];
  if (item?.side === 'sent') {
    if (hasOffer(item, 'withdrawn')) actions.push({ id: 'withdraw', label: 'Withdraw', danger: true });
    if (['declined', 'expired', 'refused'].includes(item?.state) && item?.sent_here && item?.has_text) {
      actions.push({ id: 'send-again', label: 'Send again…' });
    }
    return actions;
  }
  if (item?.open_offered) {
    const again = claimedOpens(item).length > 0;
    actions.push({ id: 'open', label: again ? 'Open again…' : 'Open…', primary: !again });
  }
  if (hasOffer(item, 'declined')) actions.push({ id: 'decline', label: 'Decline' });
  if (hasOffer(item, 'closed')) actions.push({ id: 'close', label: 'Close handoff' });
  return actions;
}

// shortSession names a session by the ends of its native id.
export function shortSession(id) {
  const value = text(id);
  return value.length > 12 ? value.slice(0, 6) + '…' + value.slice(-2) : value;
}

const BRIEF_WORDS = Object.freeze({
  waiting_for_next_prompt: 'waiting for its next prompt',
  not_delivered: 'brief not delivered',
});

function claimedOpenView(open, runtime) {
  const session = runtime + ' session ' + shortSession(open.session.native_id);
  const actions = [{ id: 'go', label: 'Go to session' }];
  if ((open.offers || []).includes('deliver-again')) actions.push({ id: 'deliver-again', label: 'Deliver again' });
  const tail = BRIEF_WORDS[open.brief];
  return { words: tail ? 'Started in ' + session + '; ' + tail : 'Opened in ' + session,
    note: open.brief_due_again ? 'Delivers again with its next prompt.' : '',
    tone: open.brief === 'not_delivered' ? 'problem' : '', actions };
}

// openView is one Open of the item on this device, as its row reads (§6.3,
// §6.5, criterion 94). An Open whose composer is still open, or was closed
// without sending, is not a fact about the handoff and has no row. `ended`
// says the handoff itself has ended.
export function openView(open, runtimeLabel = String, { ended = false } = {}) {
  const runtime = runtimeLabel(text(open?.runtime));
  // A session that started for it stays a fact of the item after the handoff ends.
  if (open?.session && (open.state === 'claimed' || open.state === 'cancelled')) return claimedOpenView(open, runtime);
  // An Open that started no session is worth a row only while it can be acted on.
  if (open?.state !== 'cancelled' || ended) return null;
  if (open.ended_code === 'runtime_not_started') {
    return { words: 'Could not start ' + runtime, note: text(open.ended_detail), tone: 'problem',
      actions: (open.offers || []).includes('retry') ? [{ id: 'retry', label: 'Retry', primary: true }] : [] };
  }
  if (open.ended_code === 'hook_did_not_run') {
    return { words: 'The ' + runtime + ' hook did not run in the session Crossing Guard started',
      note: 'Trust or re-attach the ' + runtime + ' hook, then open again.', tone: 'problem',
      actions: [{ id: 'runtimes', label: 'Settings → Runtimes ›', link: true }] };
  }
  if (open.ended_code === 'link_ended') {
    return { words: 'Cancelled when this device left the team', note: '', tone: '', actions: [] };
  }
  return null;
}

// readinessView is one runtime of the open sheet: "Ready", or exactly what is
// missing (§6.3 "When Open is offered"; criterion 84). recallOff says shared
// memory is not observed in it, which never withholds Open.
export function readinessView(option, runtimeLabel = String, firingWindow = '') {
  const label = runtimeLabel(text(option?.runtime));
  const missing = (option?.missing || []).map(code => (MISSING_WORDS[code] || (() => code.replaceAll('_', ' ')))(firingWindow));
  const notes = [];
  const recall = option?.memory_recall;
  if (option?.ready && recall && !recall.observed_injecting) notes.push(recall.entry_present ? 'Memory recall on, not yet observed' : 'Memory recall off');
  if (option?.ready && option?.recall_tools && option.recall_tools !== 'current') notes.push('The handoff tool is not registered: the session gets the summary only');
  return { runtime: text(option?.runtime), label, ready: option?.ready === true, steps: option?.ready ? ['Ready'] : missing, notes };
}

// firstReady picks the runtime the sheet opens on: the first that is ready.
export function firstReady(options = []) {
  return options.find(option => option.ready)?.runtime || '';
}

// windowWords turns a Go duration ("720h0m0s") into "30 days" for a sentence.
export function windowWords(duration) {
  const hours = /^(\d+)h/.exec(text(duration));
  if (!hours) return '';
  const count = Number(hours[1]);
  if (count >= 48 && count % 24 === 0) return (count / 24) + ' days';
  return count + (count === 1 ? ' hour' : ' hours');
}

// continuesLine is the opened session's header fact (§6.2, §14 Q22).
export function continuesLine(facts) {
  if (!facts?.opened) return '';
  const from = facts.local ? 'an earlier session on this device' : (text(facts.from?.display_name) || 'a teammate');
  return 'continues “' + (text(facts.title) || 'a handoff') + '” from ' + from;
}

// BRIEF_OPENING is how the hook's brief begins; a context row that begins with
// it is the handoff, and reads "context · handoff" (§14 Q22). The row's words
// stay the hook's own.
const BRIEF_OPENING = '[Crossing Guard handoff from ';
export function contextRowName(event) {
  return text(event?.text).startsWith(BRIEF_OPENING) ? 'handoff' : text(event?.name);
}

// ---------- the send sheet ----------

// recipientChoices is the directory as the sheet lists it: names only, the
// person's own entry last and worded as their other devices (§14 Q16).
export function recipientChoices(members = []) {
  const others = members.filter(member => !member.self)
    .sort((a, b) => text(a.display_name).localeCompare(text(b.display_name)));
  const mine = members.filter(member => member.self);
  return [...others.map(member => ({ value: member.user_id, label: text(member.display_name) || member.user_id })),
    ...mine.map(member => ({ value: member.user_id, label: (text(member.display_name) || 'You') + ' — your other devices' }))];
}

// noRecipientWords says why the sheet has nobody to send to. A directory that could
// not be read is not an empty team: the daemon's own problem is said.
export function noRecipientWords(directory = {}) {
  if (!directory.linked) return 'This device is not linked to a team.';
  const problem = text(directory.problem);
  if (problem) return 'The team’s member list could not be read: ' + problem;
  return 'No teammates on this team yet.';
}

// draftForm is the sheet's starting text, from the mechanical extract.
export function draftForm(draft = {}) {
  return { to: '', title: text(draft.title), remaining: (draft.remaining || []).map(text).filter(Boolean),
    text: String(draft.markdown || ''), includeConversation: false };
}

// formRequest is the body of POST /api/team/handoffs/send for this form.
// `previewed` is the preview a real send repeats; without it this is a preview.
export function formRequest(form, session, { local = false, previewed = null } = {}) {
  const body = { runtime: session.runtime, session_id: session.id, to: local ? '' : form.to, local,
    title: text(form.title), body_markdown: String(form.text || ''),
    remaining: (form.remaining || []).map(text).filter(Boolean), include_conversation: form.includeConversation === true };
  if (!previewed) return { ...body, preview: true };
  return { ...body, preview: false, id: previewed.id, created_at: previewed.created_at, wire_hash: previewed.wire_hash };
}

// formKey identifies what a preview was taken of: Send is offered only while
// the form still equals the form that was previewed.
export function formKey(form, local = false) {
  return JSON.stringify(formRequest(form, { runtime: '', id: '' }, { local }));
}

// formProblem: a handoff needs someone to go to, a title and a text.
export function formProblem(form, local = false) {
  if (!local && !text(form.to)) return 'Choose who it goes to.';
  if (!text(form.title)) return 'Give it a title.';
  if (!text(form.text)) return 'The text is empty.';
  return '';
}

const count = (value, one, many) => value.toLocaleString('en-US') + ' ' + (value === 1 ? one : many);
export const bytesWords = value => count(Number(value) || 0, 'byte', 'bytes');

const MARKER = /\[redacted:[^\]]*\]|\[absolute path\]/g;
const markersIn = turns => turns.reduce((total, turn) => total + (String(turn.text || '').match(MARKER) || []).length, 0);

function checkRows(checks = {}) {
  const rows = [];
  const secrets = Number(checks.redaction_count) || 0, paths = Number(checks.absolute_paths) || 0;
  if (!secrets && !paths) return [{ label: 'Checked', value: 'No secrets and no paths outside the repository were found.' }];
  if (secrets) {
    const names = Object.keys(checks.redactions || {}).sort().join(', ');
    rows.push({ label: 'Secrets replaced', value: count(secrets, 'match', 'matches') + (names ? ' (' + names + ')' : ''), changed: true });
  }
  if (paths) rows.push({ label: 'Paths replaced', value: count(paths, 'path', 'paths') + ' outside the repository', changed: true });
  return rows;
}

// willBeSent is "what will be sent" as readable fields (§6.1, §14 Q14): the
// title, the remaining items and the text in full, exactly as the preview
// returned them, with the size of what one push carries. A ticked excerpt is
// folded to three facts and opens to every turn as it will leave. The document
// also carries the session's declared tags and its identities; they leave too,
// so they are rows here: nothing leaves that the sheet does not show.
export function willBeSent(preview) {
  if (!preview) return null;
  const model = { bytes: bytesWords(preview.bytes), title: text(preview.title), remaining: preview.remaining || [],
    text: String(preview.body_markdown || ''), agents: (preview.agents || []).map(agent => text(agent.name) || agent.profile_id),
    sessionTags: sentTagRows(preview.governance_state), session: sentSessionRows(preview.session),
    checks: checkRows(preview.checks), excerpt: null };
  const conversation = preview.conversation;
  if (conversation) {
    const turns = conversation.turns || [];
    model.excerpt = { turns, facts: [
      { label: 'Turns', value: count(Number(conversation.turn_count) || turns.length, 'turn', 'turns') + (conversation.truncated ? ' (older ones did not fit)' : '') },
      { label: 'Size', value: bytesWords(conversation.bytes) },
      { label: 'Replaced', value: count(markersIn(turns), 'secret or path', 'secrets or paths') },
    ] };
  }
  return model;
}

// sentTagRows is the "Session tags" row: the highest data class the session's
// tags record (the water mark), then every tag, one per line. Empty when the
// session declares nothing.
function sentTagRows(governance) {
  const rows = [];
  const mark = text(governance?.water_mark);
  if (mark) rows.push({ label: 'Highest data class', value: mark });
  for (const tag of governance?.tags || []) rows.push({ label: text(tag.key), value: text(tag.value) });
  return rows;
}

const SESSION_ID_ROWS = [['runtime', 'Runtime'], ['native_id', 'Session id'], ['catalog_id', 'Catalog id'], ['resume_id', 'Resume id'], ['id', 'Id on the team server']];

// sentSessionRows is the "Session" row: each identity the document carries, as
// its own line, never joined.
function sentSessionRows(session) {
  return SESSION_ID_ROWS.filter(([key]) => text(session?.[key])).map(([key, label]) => ({ label, value: text(session[key]) }));
}

// sheetSentences are the sheet's two sentences (§14 Q15), short. A same-device
// handoff leaves nothing and says only that.
export function sheetSentences({ local = false, recipient = '' } = {}) {
  if (local) return ['Stays on this device.'];
  const who = text(recipient) || 'The recipient';
  return [who + ' chooses the runtime; this text may be sent to whichever model provider their device uses.',
    'Whoever runs the team server can read this text.'];
}

// sendRefusal words a refused send. The text is kept either way.
export function sendRefusal(error, recipient = '') {
  const who = text(recipient) || 'The recipient';
  const known = REFUSAL_WORDS[error?.code];
  if (known) return 'Not sent: ' + known(who) + '. Your text is kept.';
  if (error?.code === 'preview_mismatch') return 'The session moved on since this was checked. Look at what will be sent again, then send.';
  if (error?.code === 'not_linked') return 'Not sent: this device is not linked to a team.';
  return 'Not sent: ' + (text(error?.message) || 'the daemon refused it') + '. Your text is kept.';
}

// ---------- the document ----------

// agentRows says, for each agent a handoff names, whether it is adopted here.
// adoptedIds are the profile ids this device's adoption records list.
export function agentRows(agents = [], adoptedIds = new Set()) {
  return agents.map(agent => ({ name: text(agent.name) || agent.profile_id, adopted: adoptedIds.has(agent.profile_id) }));
}

// adoptedProfileIds reads GET /api/team/layers: every agent an adoption lists.
export function adoptedProfileIds(layers) {
  const ids = new Set();
  for (const record of layers?.adopted_bundles || []) {
    for (const agent of record.agents || []) if (agent.profile_id) ids.add(agent.profile_id);
  }
  return ids;
}

// excerptWords says whether the document carries turns of the sender's session.
export function excerptWords(document) {
  const turns = document?.conversation?.turns?.length || 0;
  return turns ? 'Their last ' + count(turns, 'turn', 'turns') : 'Not included';
}

// folderName is the last part of a folder's path: what a person calls the checkout.
const folderName = folder => text(folder).split('/').filter(Boolean).pop() || '';

// documentFacts is the label/value list of a handoff, one fact per row (§8.2).
// `when` formats a timestamp. A handoff names its repository by an id derived
// from the remote, which is not a name; the repository is said as the checkout
// this device resolved it to (`folder`), or as not being here (`folderKnown`
// says the device looked).
export function documentFacts(item, document, { when = String, runtimeLabel = String, folder = '', folderKnown = false } = {}) {
  const rows = [];
  const add = (label, value) => { if (text(value)) rows.push({ label, value: text(value) }); };
  if (item?.local) add('From', 'A session on this device');
  else if (item?.from_another_device) add('To', peerName(item));
  else add(item?.side === 'sent' ? 'To' : 'From', peerName(item));
  if (item?.from_another_device) add('Text', 'On the device that sent it');
  if (folder) add('Repository', folderName(folder));
  else if (folderKnown && item?.repository_id) add('Repository', 'No checkout of it on this device');
  add('On this device', folder);
  if (item?.side !== 'sent') add('Written in', item?.session?.runtime ? runtimeLabel(item.session.runtime) : '');
  if (document) add('Excerpt', excerptWords(document));
  // A handoff that has not left this device was written, not sent.
  add(['queued', 'refused'].includes(item?.state) ? 'Written' : 'Sent', item?.created_at ? when(item.created_at) : '');
  const moved = item?.state_at && item.state_at !== item.created_at && !['sent', 'queued', 'refused'].includes(item.state);
  if (moved) add(sentenceCase(stateView(item).word), when(item.state_at));
  // other_opened_count counts the recipient's OTHER SESSIONS that opened it — on this
  // device or another. It says nothing about devices, so neither do the words.
  if (item?.other_opened_count > 0) add('Also opened', 'in ' + count(item.other_opened_count, 'other session', 'other sessions'));
  if (item?.link_ended) add('Team', 'This device is no longer linked to it');
  return rows;
}


// ---------- memory recall ----------

// recallView is one runtime's memory recall as Settings → Runtimes and the open
// sheet state it (§6.3 OD-22, OD-24; criteria 87, 92): the state word, the one
// action this daemon may take, and the words when it may take none.
export function recallView(state, runtimeLabel = String) {
  const label = runtimeLabel(text(state?.runtime));
  const offers = state?.offers || [];
  const on = state?.entry_present === true;
  const view = { runtime: text(state?.runtime), label, on, file: text(state?.config_path), action: null, note: '' };
  // What was observed wins over what the settings file holds: sessions seen
  // receiving the memory index are receiving it, entry here or not (another hook
  // can deliver it).
  const observed = state?.observed_injecting === true;
  view.word = observed ? 'on — observed injecting' : (on ? 'on — not yet observed' : 'off');
  view.chip = observed ? 'st-verified' : (on ? 'st-draft' : 'st-stale');
  if (offers.includes('attach')) view.action = { id: 'attach', label: 'Turn on memory recall' };
  else if (offers.includes('detach')) view.action = { id: 'detach', label: 'Turn off memory recall' };
  if (state?.withheld_code === 'no_lifecycle_hook') view.note = label + ' is not connected on this device. Connect it first.';
  else if (state?.withheld_code) view.note = text(state.withheld) ? sentenceCase(text(state.withheld)) + '.' : 'This daemon may not change this runtime’s settings.';
  else if (on && !state.added_by_action) view.note = 'Added by hand — left alone.';
  else if (!on && observed) view.note = 'Recent ' + label + ' sessions received the memory index, from an entry this settings file does not hold.';
  else if (!on) view.note = label + ' sessions get no memory index.';
  return view;
}

// recallConsent is what the person agrees to before the settings file changes.
export function recallConsent(view, action) {
  const file = view.file || 'that runtime’s settings file';
  if (action === 'detach') {
    return { title: 'Turn off memory recall for ' + view.label, label: 'Turn off memory recall',
      rows: [{ label: 'File', value: file }, { label: 'Removes', value: 'The one memory entry Crossing Guard added' }, { label: 'Leaves', value: 'Every other entry as it is' }] };
  }
  return { title: 'Turn on memory recall for ' + view.label, label: 'Turn on memory recall for ' + view.label,
    rows: [{ label: 'File', value: file }, { label: 'Adds', value: 'One entry that gives each new session the memory index' }, { label: 'Leaves', value: 'Every other entry as it is' }] };
}

// recallOutcome words what the action did when the file was left as it was.
export function recallOutcome(result) {
  if (result?.changed) return '';
  const words = { already_present: 'An entry was already there. It was added by hand, so it is left alone.',
    not_added_by_action: 'The entry was added by hand, so it is left alone.',
    entry_changed: 'The entry no longer matches what was added, so it is left alone.',
    not_attached: 'Memory recall was not on.' };
  return words[result?.code] || '';
}

// ---------- a session's own handoff facts ----------

// sameSession says a wire session names this console session. A session has
// three ids (native, catalog, resume); it is matched on the runtime and on any
// one of them being the same value in the same role, never on one id alone.
function sameSession(wire, session) {
  if (!wire || text(wire.runtime) !== text(session?.runtime)) return false;
  const natives = new Set([session.resume_id, session.thread_id, ...(session.identities || [])].map(text).filter(Boolean));
  return (text(wire.catalog_id) && text(wire.catalog_id) === text(session.id))
    || (text(wire.resume_id) && text(wire.resume_id) === text(session.resume_id))
    || (text(wire.native_id) && (natives.has(text(wire.native_id)) || text(wire.native_id) === text(session.id)));
}

// relatedHandoffRows are the Related-sessions rows a handoff adds (§14 Q22,
// Q23). The session that continues one names where it came from; the session a
// handoff was sent from names who continued it. Neither is a link: the other
// session is on another person's device.
export function relatedHandoffRows({ session, facts = null, list = null, runtimeLabel = String }) {
  const rows = [];
  if (facts?.opened) {
    rows.push({ label: facts.local ? 'An earlier session on this device' : (text(facts.from?.display_name) || 'A teammate') + '’s session',
      description: 'handed off “' + (text(facts.title) || 'a handoff') + '”' });
  }
  for (const item of list?.sent || []) {
    if (!item.sent_here || !item.opened_by || !sameSession(item.session, session)) continue;
    const runtime = text(item.opened_by.session?.runtime);
    rows.push({ label: peerName(item) + '’s session', description: 'continued this' + (runtime ? ' in ' + runtimeLabel(runtime) : '') });
  }
  return rows;
}

// nativeIdsOf lists the ids a session may be known to its hook by, most likely
// first: the resume id, then the catalog id.
export function nativeIdsOf(session) {
  return [...new Set([session?.resume_id, session?.thread_id, session?.id].map(text).filter(Boolean))];
}
